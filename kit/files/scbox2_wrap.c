/*
 * scbox2 shim -- the SC Box II miner wrapper, installed as /usr/bin/intminer and /usr/bin/bfgminer.
 *
 * The stock miner (Goldshell's intminer: BFGMiner 5.4.2 + the gsscbox2 driver) lives at
 * /usr/libexec/scbox2/intminer (same file name, so the running process is still called "intminer"). The
 * config store is the 3 MB FAT at /mnt/config (the root is a read-only squashfs). This wrapper:
 *   - runs at most ONE miner: two launchers start it on this firmware (the boot launcher
 *     /usr/libexec/scbox2/miner-run, and Goldshell's xminerd once its network check passes, ~40 s after boot).
 *     Two miners on the same chip chain fight over it (seen on hardware: "Read Nonce Faild 10 Times, Reinit
 *     Device", "Auto addressing failed", dropped nonces, HW errors up to 100 %). So the wrapper first takes an
 *     exclusive flock on /tmp/scbox2-miner.lock and keeps that descriptor open across execve: the miner holds
 *     the lock for its whole life and the kernel releases it when the miner exits, however it exits. A start
 *     that finds the lock held exits at once with status 75, before touching the chips or a start try,
 *   - keeps argv[0] exactly as the launcher gave it (minerd matches miners by /proc/<pid>/cmdline),
 *   - always adds --no-pool-redirect: a pool can't move the miner to another server (stratum
 *     client.reconnect / redirects); it only ever mines where the Pools tab says,
 *   - adds -D -P while /mnt/config/scbox2/verbose exists, so verbose survives restarts and reboots. -P
 *     (protocol dump) and the per-nonce "OK" lines are debug-level messages this miner prints only with -D,
 *   - normalizes the config flag: this intminer accepts the config ONLY as the long form --config <path>.
 *     The short -c <path> (what the boot launcher /usr/libexec/scbox2/miner-run and xminerd pass) and
 *     --config=<path> are rewritten to --config <path>; with no config at all, the panel's live pool config
 *     /mnt/config/bfgminer/bfgminer.json is used. Without this the miner rejects the arguments
 *     ("Unexpected extra commandline arguments") and exits pool-less,
 *   - loads /lib/scbox2_bind.so so the cgminer API (:4028) listens on 127.0.0.1 only,
 *   - while /mnt/config/scbox2/chiphook exists (Controls -> Chip trims; OFF by default on the SC Box II), runs
 *     the chip-hook copy (/usr/libexec/scbox2/hook/intminer, same file name) with /lib/scbox2_chip.so as well,
 *     which applies per-chip clock trims. Without the flag the same copy runs without the trim library: it's
 *     Goldshell's miner plus the guards (patch_intminer_box2: divide-by-zero + pool-input guards).
 *     Either way at most 3 starts in a row (markers in /tmp, cleared once mining runs); then, or if the copy
 *     is missing, the stock miner runs.
 * Static, no libc: raw ARM EABI syscalls only.
 */
#define NR_exit 1
#define NR_write 4
#define NR_execve 11
#define NR_access 33
#define NR_open 5
#define NR_close 6
#define NR_flock 143
#define O_RDWR_CREAT 0102 /* O_RDWR|O_CREAT (no O_CLOEXEC: the lock must survive execve into the miner) */
#define LOCK_EX_NB 6      /* LOCK_EX|LOCK_NB */
#define EXIT_ALREADY_RUNNING 75
#define O_WRONLY_CREAT_EXCL 0301 /* O_WRONLY|O_CREAT|O_EXCL */
#define F_OK 0

static const char LOCKFILE[] = "/tmp/scbox2-miner.lock"; /* /tmp is RAM: no stale lock after a reboot */
static const char REAL[] = "/usr/libexec/scbox2/intminer";
static const char REAL_HOOK[] = "/usr/libexec/scbox2/hook/intminer";
static const char HOOK_FLAG[] = "/mnt/config/scbox2/chiphook";
static const char HOOK_LIB[] = "/lib/scbox2_chip.so";
static char PRELOAD_HOOK[] = "LD_PRELOAD=/lib/scbox2_bind.so /lib/scbox2_chip.so";
static char PRELOAD_CHIP[] = "LD_PRELOAD=/lib/scbox2_chip.so"; /* the hook copy when scbox2_bind.so is missing */
static const char FLAG[] = "/mnt/config/scbox2/verbose";
static const char LIB[] = "/lib/scbox2_bind.so";
static char PRELOAD[] = "LD_PRELOAD=/lib/scbox2_bind.so";
static char DASHD[] = "-D";
static char DASHP[] = "-P";
static char NOREDIR[] = "--no-pool-redirect";
static char CFGFLAG[] = "--config";
static char POOLCFG[] = "/mnt/config/bfgminer/bfgminer.json"; /* the live pool config the panel maintains */
/* the hook copy is started at most 3 times in a row (/tmp is RAM): scbox2_chip.so removes these markers once the
 * hooked miner has run 60 s with chips finding nonces, so only starts that never got that far count */
static const char TRY1[] = "/tmp/scbox2-hook.1", TRY2[] = "/tmp/scbox2-hook.2", TRY3[] = "/tmp/scbox2-hook.3";
/* the same for the guarded copy without trims (the panel removes these once mining has run 2 minutes) */
static const char GTRY1[] = "/tmp/scbox2-guard.1", GTRY2[] = "/tmp/scbox2-guard.2", GTRY3[] = "/tmp/scbox2-guard.3";

/* the panel checks for this marker to know /usr/bin/intminer is this wrapper (verbose persists via -D -P) */
const char scbox2_marker[] __attribute__((used)) = "scbox2 shim";

static long sc3(long n, long a, long b, long c)
{
	register long r0 __asm__("r0") = a;
	register long r1 __asm__("r1") = b;
	register long r2 __asm__("r2") = c;
	register long r7 __asm__("r7") = n;
	__asm__ volatile("svc 0" : "+r"(r0) : "r"(r1), "r"(r2), "r"(r7) : "memory");
	return r0;
}

static int starts(const char *s, const char *p)
{
	while (*p)
		if (*s++ != *p++)
			return 0;
	return 1;
}

static int streq(const char *a, const char *b)
{
	while (*a && *a == *b)
		a++, b++;
	return *a == *b;
}

/* one of three start tries: 1 = this start may use the copy, 0 = three starts in a row already used it */
static long take_try(const char *a, const char *b, const char *c)
{
	long fd = sc3(NR_open, (long)a, O_WRONLY_CREAT_EXCL, 0600);
	if (fd < 0)
		fd = sc3(NR_open, (long)b, O_WRONLY_CREAT_EXCL, 0600);
	if (fd < 0)
		fd = sc3(NR_open, (long)c, O_WRONLY_CREAT_EXCL, 0600);
	if (fd < 0)
		return 0;
	sc3(NR_close, fd, 0, 0);
	return 1;
}

#define MAXA 256
#define MAXE 256

__attribute__((used, noreturn)) void wrap_main(long *sp)
{
	long argc = sp[0];
	char **argv = (char **)(sp + 1);
	char **envp = argv + argc + 1;
	char *na[MAXA + 4], *ne[MAXE + 2];
	long i, n = 0, m = 0, hook, guard, have_cfg = 0;

	/* one miner at a time: hold the lock (fd left open, inherited by the miner through execve) or leave */
	{
		long lfd = sc3(NR_open, (long)LOCKFILE, O_RDWR_CREAT, 0600);
		if (lfd >= 0 && sc3(NR_flock, lfd, LOCK_EX_NB, 0) != 0) {
			static const char busy[] = "intminer wrapper: a miner is already running; not starting another\n";
			sc3(NR_write, 2, (long)busy, sizeof busy - 1);
			sc3(NR_exit, EXIT_ALREADY_RUNNING, 0, 0);
		}
		/* lfd < 0 (no /tmp?): start anyway, as before -- never block mining on the lock itself */
	}
	if (argc > MAXA)
		argc = MAXA;
	na[n++] = argc > 0 ? argv[0] : (char *)"/usr/bin/intminer";
	na[n++] = NOREDIR;
	if (sc3(NR_access, (long)FLAG, F_OK, 0) == 0) {
		na[n++] = DASHD;
		na[n++] = DASHP;
	}
	/* forward the launcher's arguments, rewriting -c <path> and --config=<path> into --config <path> */
	for (i = 1; i < argc && n < MAXA; i++) {
		char *a = argv[i];
		if (streq(a, "-c") && i + 1 < argc) {
			na[n++] = CFGFLAG;
			if (n < MAXA)
				na[n++] = argv[++i];
			have_cfg = 1;
		} else if (starts(a, "--config=")) {
			na[n++] = CFGFLAG;
			if (n < MAXA)
				na[n++] = a + 9; /* past "--config=" */
			have_cfg = 1;
		} else {
			if (streq(a, CFGFLAG))
				have_cfg = 1;
			na[n++] = a;
		}
	}
	if (!have_cfg && n + 2 <= MAXA) { /* no config given: the panel's live pool config */
		na[n++] = CFGFLAG;
		na[n++] = POOLCFG;
	}
	na[n] = 0;

	hook = sc3(NR_access, (long)HOOK_FLAG, F_OK, 0) == 0 && sc3(NR_access, (long)REAL_HOOK, F_OK, 0) == 0 &&
	       sc3(NR_access, (long)HOOK_LIB, F_OK, 0) == 0;
	if (hook)
		hook = take_try(TRY1, TRY2, TRY3); /* 3 starts in a row never got going: no trims */
	/* without trims the same copy runs without the trim library: Goldshell's miner plus the guards */
	guard = !hook && sc3(NR_access, (long)REAL_HOOK, F_OK, 0) == 0 && take_try(GTRY1, GTRY2, GTRY3);
	for (i = 0; envp[i] && m < MAXE; i++)
		if (!starts(envp[i], "LD_PRELOAD="))
			ne[m++] = envp[i];
	if (sc3(NR_access, (long)LIB, F_OK, 0) == 0)
		ne[m++] = hook ? PRELOAD_HOOK : PRELOAD;
	else if (hook)
		ne[m++] = PRELOAD_CHIP; /* without the trim library the hook copy would run untrimmed and use up its tries */
	ne[m] = 0;

	if (hook || guard)
		sc3(NR_execve, (long)REAL_HOOK, (long)na, (long)ne); /* the guarded copy (with trims if hook) */
	sc3(NR_execve, (long)REAL, (long)na, (long)ne);          /* stock (or the copy failed to start) */
	{
		static const char msg[] = "intminer wrapper: cannot exec /usr/libexec/scbox2/intminer\n";
		sc3(NR_write, 2, (long)msg, sizeof msg - 1);
	}
	sc3(NR_exit, 127, 0, 0);
	for (;;)
		;
}

__attribute__((naked, noreturn)) void _start(void)
{
	__asm__ volatile(
		"mov fp, #0\n"
		"mov r0, sp\n"
		"bic sp, sp, #7\n"
		"bl wrap_main\n");
}
