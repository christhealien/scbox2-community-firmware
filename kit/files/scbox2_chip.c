/*
 * scbox2_chip.so: per-chip clock trims for the SC Box II miner (intminer), and per-chip counters for
 * the panel. Port of the SC Lite chip-trim library (offsets re-derived for the SC Box II miner, research lane R10).
 *
 * Loaded with LD_PRELOAD into the patched copy of intminer only (/usr/libexec/scbox2/hook/intminer, chosen
 * by the wrapper while /mnt/config/scbox2/chiphook exists). In that copy the board-clock function
 * (0x1bdf64) is patched at two points to call scbox2_chip_hook(dev, frame, phase) through a slot this
 * library fills in:
 *     phase 0 (PRE):  after the broadcast ramp, BEFORE the miner overwrites its per-chip clock table with
 *                     the board clock -- the table still shows each chip's clock (DTFS slow-downs, trims).
 *                     This library snapshots that table.
 *     phase 1 (POST): right before the function's epilogue, the per-chip table already reset to the board
 *                     clock -- this library applies the per-chip trims and ramps the chips in 10 MHz steps.
 * Both phases run on the miner's own thread. The two-phase cave (PRE snapshot, POST apply/ramp) replaces the
 * SC Lite single-call hook: the PRE snapshot gives the chips' real pre-reset clocks directly, so none of the
 * SC Lite re-init/re-apply ambiguity bookkeeping is needed.
 *
 * SAFETY (HARD RULE 1 -- clock never above stock): the chip-trim feature ships OFF on the SC Box II (the
 * model profile disables it; the panel writes the chiphook flag only once ChipTrims is commissioned on the
 * unit, and removes a leftover flag at start-up when it isn't). When it IS used:
 *   - trims honour the panel's exact limits (-100..+50 MHz in 12.5 steps; any other line is skipped, never
 *     clamped), never run a chip below CLOCK_FLOOR, and a "+" trim that would put a chip above CHIP_CEIL
 *     (690 MHz, the Box II's top stock clock) is skipped -- so no chip ever runs above stock, whatever the
 *     trims file says;
 *   - a chip is ramped in 10 MHz steps from where it physically is: the board clock after a real clock change
 *     (the miner's broadcast ramp moved every chip there), or -- when the clock didn't change (a re-apply, and
 *     every vfff write minerd makes for the fans runs this function at the same clock) -- the PRE snapshot of
 *     the miner's table, i.e. where this library (or DTFS) left it. A trimmed chip is never jerked back toward
 *     the board clock and down again, and a trim taken off is really undone (the chip ramps back);
 *   - a chip the miner itself slowed (DTFS: the snapshot shows it below where this library left it) is never
 *     raised by a re-apply; it may only be slowed further;
 *   - chips with no trim, that this library never moved, are not touched at all (not even the miner's table):
 *     with no trims file the library only writes its counters.
 *
 * Trims: /mnt/config/scbox2/chiptrim, one "chain chip mhz" per line (mhz = offset from the board clock).
 * Counters: /tmp/scbox2-chipstats.json, every 10 s: each chain's board clock, chip count, the miner's
 * per-chip clock table and per-chip good-nonce counts (read only). Clocks are written as whole tenths of a
 * MHz (6250 = 625.0 MHz): this firmware's uClibc mis-reads doubles passed by clang-built code to printf, so
 * nothing here formats a floating-point number.
 *
 * The addresses below belong to intminer from SC Box II firmware 2.2.1 (non-PIE, fixed
 * addresses); the kit's patch_intminer_box2.py checks the bytes it patches. No trims -> nothing is set, the
 * miner behaves like stock.
 */
typedef unsigned int u32;
typedef unsigned long size_t;

/* from the miner's own uClibc and libpthread */
extern int open(const char *, int, ...);
extern int close(int);
extern long read(int, void *, size_t);
extern long write(int, const void *, size_t);
extern int rename(const char *, const char *);
extern long readlink(const char *, char *, size_t);
extern int usleep(u32);
extern int snprintf(char *, size_t, const char *, ...);
extern long time(long *);
extern int getpid(void);
extern int unlink(const char *);
extern long lseek(int, long, int);
extern int pthread_create(void *, const void *, void *(*)(void *), void *);
extern int pthread_mutex_lock(void *);
extern int pthread_mutex_unlock(void *);

#define O_RDONLY 0
#define O_WRONLY 1
#define O_CREAT 0100
#define O_TRUNC 01000
#define O_APPEND 02000

/* ---- SC Box II intminer 2.2.1 layout (research lane R10) ---- */
#define SLOT_ADDR 0x230ff8u              /* spare word in the last .bss page, read by the cave */
#define DEV_BASE_CLK 0x80348             /* double: board clock (MHz) */
#define DEV_CLK_TABLE 0x80350            /* float[chip]: the miner's per-chip clock table */
#define DEV_NCHIPS 0x800ac               /* int: chips on the chain */
#define DEV_CHAIN 0x800b4                /* int: chain id (the "C%d" in the log) */
#define DEV_NONCES 0x805bc               /* int[idx]: good nonces per chip */
typedef int (*set_clock_fn)(void *dev, int chip, double mhz);
static set_clock_fn set_clock = (set_clock_fn)(0x1c4178u | 1); /* Thumb */

#define FRAME_REQ_CLK 0x0                /* double: the clock the function was asked for */
#define FRAME_OLD_CLK 0x1010             /* double: the board clock before this change */

/* file names (variables only so the qemu harness can point them into its own directory: scbox2_chip_test_paths) */
static const char *TRIM_FILE = "/mnt/config/scbox2/chiptrim";
static const char *STATS_FILE = "/tmp/scbox2-chipstats.json";
static const char *STATS_TMP = "/tmp/scbox2-chipstats.json.tmp";
static const char *LOG_FILE = "/tmp/scbox2-chiphook.log";
#define LOG_MAX 65536 /* the log is in RAM (/tmp): started again once it is this long */
#define HOOK_EXE "/usr/libexec/scbox2/hook/intminer"
#define MAX_CHAINS 4
#define MAX_CHIPS 64
/* the gap from DEV_CLK_TABLE to DEV_NONCES is (0x805bc-0x80350)/4 = 155 floats; 59 keeps index 0..n+1 well
 * inside it for this board's 36 chips (18 pairs), exactly as the SC Lite float[60] cap did. */
#define TABLE_CHIPS 59
#define TRIM_MIN (-100.0)
#define TRIM_MAX (50.0)  /* a hand trim may run a strong chip up to 50 MHz over its board (the panel's limit) */
#define CLOCK_FLOOR 200.0
#define CHIP_CEIL 690.0  /* the SC Box II's top stock clock (Hashrate plan): a "+" trim never takes a chip past it */

static char *devs[MAX_CHAINS];
static int ndevs;
static int stats_started;
static int stats_lock[16]; /* pthread_mutex_t storage, word-aligned (zero = default mutex in uClibc) */
static int trim_lock[16];  /* the trim parser's static buffers (the hook may run on several chain threads) */
static long started_at;    /* when the hook was registered (0 = not in the hooked miner) */
static int vouched;
static int test_mode;      /* set by the test harness */

/* per-chain PRE snapshot of the miner's clock table (what each chip was at before the reset) */
static float snap[MAX_CHAINS][MAX_CHIPS];
static int snap_valid[MAX_CHAINS];
/* what this library last left each chip at (MHz; 0 = at the board clock / never moved by this library) */
static double applied[MAX_CHAINS][MAX_CHIPS];
static int posts[MAX_CHAINS]; /* POST calls seen per chain (the first one is always logged) */

static int tenths(double v) { return (int)(v * 10 + (v < 0 ? -0.5 : 0.5)); }

static void logline(const char *s, int n)
{
	int fd = open(LOG_FILE, O_WRONLY | O_CREAT | O_APPEND, 0644);
	if (fd >= 0 && lseek(fd, 0, 2 /* SEEK_END */) > LOG_MAX) { /* bounded: /tmp is RAM */
		close(fd);
		fd = open(LOG_FILE, O_WRONLY | O_CREAT | O_TRUNC, 0644);
	}
	if (fd >= 0) {
		write(fd, s, n);
		close(fd);
	}
}

/* parse "chain chip mhz" lines; mhz may have a fraction (12.5) and a sign */
struct trim { int chain, chip; double mhz; };

static int parse_num(const char **p, double *out)
{
	const char *s = *p;
	double sign = 1, v = 0, frac = 0.1;
	int any = 0;
	while (*s == ' ' || *s == '\t') s++;
	if (*s == '-') { sign = -1; s++; } else if (*s == '+') s++;
	while (*s >= '0' && *s <= '9') { v = v * 10 + (*s - '0'); s++; any = 1; }
	if (*s == '.') { s++; while (*s >= '0' && *s <= '9') { v += (*s - '0') * frac; frac /= 10; s++; any = 1; } }
	*p = s;
	*out = sign * v;
	return any;
}

/* a field ends at a blank or the end of the line ("0 5+50" and "0.9 5 -50" are not trims: the panel drops them) */
static int field_end(const char *p, const char *end) { return p == end || *p == ' ' || *p == '\t' || *p == '\r' || *p == '\n'; }
static int whole(double v) { return v >= 0 && v <= 99 && v == (double)(int)v; }

static int read_trims(struct trim *t, int max)
{
	static char buf[16384]; /* the panel reads the whole file: so does this (a hand-written comment block can be long) */
	int fd = open(TRIM_FILE, O_RDONLY), n = 0;
	long len;
	const char *p, *end;
	if (fd < 0)
		return 0;
	len = read(fd, buf, sizeof buf - 1);
	close(fd);
	if (len <= 0)
		return 0;
	buf[len] = 0;
	p = buf;
	end = buf + len;
	while (p < end && n < max) {
		double a, b, c;
		if (*p == '#') { while (p < end && *p != '\n') p++; }
		else if (parse_num(&p, &a) && field_end(p, end) && whole(a) && parse_num(&p, &b) && field_end(p, end) && whole(b) &&
		         parse_num(&p, &c) && field_end(p, end)) { /* "-1e2", "12.5x": skipped, as the panel drops them */
			t[n].chain = (int)a; t[n].chip = (int)b; t[n].mhz = c; n++;
		}
		while (p < end && *p != '\n') p++;
		if (p < end) p++;
	}
	return n;
}

static void write_stats(void)
{
	static char out[16384];
	int len = 0, i, c;
	long now = time(0), nonces = 0;
	pthread_mutex_lock(stats_lock); /* the buffer and the .tmp file are shared by the stats thread and the hook */
	len += snprintf(out + len, sizeof out - len, "{\"t\":%ld,\"chains\":[", now);
	for (c = 0; c < ndevs; c++) {
		char *dev = devs[c];
		int n = *(int *)(dev + DEV_NCHIPS);
		if (n < 0) n = 0;
		if (n > TABLE_CHIPS - 1) n = TABLE_CHIPS - 1; /* read index 0..n+1 must stay in the clock/nonce tables */
		len += snprintf(out + len, sizeof out - len, "%s{\"chain\":%d,\"chips\":%d,\"base10\":%d,\"clock10\":[",
		                c ? "," : "", *(int *)(dev + DEV_CHAIN), n, tenths(*(double *)(dev + DEV_BASE_CLK)));
		/* index 0..n+1: the panel works out the numbering (chips are addressed 1..n) */
		for (i = 0; i <= n + 1; i++)
			len += snprintf(out + len, sizeof out - len, "%s%d", i ? "," : "", tenths(*(float *)(dev + DEV_CLK_TABLE + 4 * i)));
		len += snprintf(out + len, sizeof out - len, "],\"nonces\":[");
		for (i = 0; i <= n + 1; i++) {
			len += snprintf(out + len, sizeof out - len, "%s%d", i ? "," : "", *(int *)(dev + DEV_NONCES + 4 * i));
			nonces += *(int *)(dev + DEV_NONCES + 4 * i) > 0;
		}
		len += snprintf(out + len, sizeof out - len, "]}");
	}
	len += snprintf(out + len, sizeof out - len, "]}\n");
	{
		int fd = open(STATS_TMP, O_WRONLY | O_CREAT | O_TRUNC, 0644);
		if (fd >= 0) {
			write(fd, out, len);
			close(fd);
			rename(STATS_TMP, STATS_FILE);
		}
	}
	/* the hooked miner has run 60 s and its chips are finding nonces: clear the wrapper's start markers, so only
	 * starts that never got this far count towards its limit of 3 */
	if (!vouched && started_at && now - started_at >= 60 && nonces > 0) {
		vouched = 1;
		unlink("/tmp/scbox2-hook.1");
		unlink("/tmp/scbox2-hook.2");
		unlink("/tmp/scbox2-hook.3");
	}
	pthread_mutex_unlock(stats_lock);
}

static void *stats_thread(void *arg)
{
	(void)arg;
	for (;;) {
		write_stats();
		usleep(10 * 1000 * 1000);
	}
	return 0;
}

/* Ramp every chip in moving[] from from[] to want[] together: each chip goes in 10 MHz steps, as the miner
 * ramps, and the 40 ms wait is shared, so the whole set takes as long as its longest ramp (at most ~15
 * steps, 0.6 s), not the sum of them -- otherwise many trimmed chips would overrun the panel's 4 s API
 * timeout for a pgaset. */
static void ramp_all(char *dev, int n, const int *moving, double *from, const double *want)
{
	int k, left = 1;
	while (left) {
		int stepped = 0;
		left = 0;
		for (k = 0; k < n; k++) {
			int i = moving[k];
			double f = from[i], to = want[i];
			if (f != f) /* done */
				continue;
			if ((to < f && f - 10 > to) || (to > f && f + 10 < to)) {
				from[i] = f + (to < f ? -10 : 10);
				set_clock(dev, i, from[i]);
				stepped = left = 1;
			} else {
				set_clock(dev, i, to);
				from[i] = 0.0 / 0.0; /* NaN: done */
			}
		}
		if (stepped)
			usleep(40000);
	}
}

static int find_chain(char *dev)
{
	int c;
	pthread_mutex_lock(stats_lock);
	for (c = 0; c < ndevs && devs[c] != dev; c++)
		;
	if (c == ndevs && ndevs < MAX_CHAINS)
		devs[ndevs++] = dev;
	if (c < MAX_CHAINS && !stats_started && !test_mode) {
		unsigned long th;
		stats_started = 1;
		pthread_create(&th, 0, stats_thread, 0);
	}
	pthread_mutex_unlock(stats_lock);
	return c;
}

static int same10(double a, double b) { return tenths(a) == tenths(b); }

/* Called by the cave at the PRE and POST points of every board-clock change, on the thread that changes the
 * clock (the miner's own at start-up, the API thread for a pgaset/vfff). frame = the board-clock function's
 * frame (r7): +0 the requested clock, +0x1010 the board clock before the call (both doubles). */
__attribute__((used)) void scbox2_chip_hook(char *dev, char *frame, int phase)
{
	static struct trim t[256];
	double want[MAX_CHIPS], from[MAX_CHIPS], req, old;
	int moving[MAX_CHIPS];
	char msg[200];
	int nt, i, c, chain, nchips, set = 0, trimmed = 0, skipped = 0, held = 0, moved_clock, have_snap;
	double base;

	if (!dev)
		return;
	c = find_chain(dev);
	if (c >= MAX_CHAINS)
		return;

	nchips = *(int *)(dev + DEV_NCHIPS);
	if (nchips > TABLE_CHIPS)
		nchips = TABLE_CHIPS;
	if (nchips < 0)
		nchips = 0;

	if (phase == 0) {
		/* PRE: the per-chip table still holds each chip's clock as the miner last knew it (this library's trims,
		 * DTFS slow-downs) -- snapshot it. */
		for (i = 0; i <= nchips && i < MAX_CHIPS; i++)
			snap[c][i] = *(float *)(dev + DEV_CLK_TABLE + 4 * i);
		snap_valid[c] = 1;
		return;
	}
	if (phase != 1)
		return;
	have_snap = snap_valid[c];
	snap_valid[c] = 0;

	/* POST: the miner just reset every chip's table entry to the board clock. */
	chain = *(int *)(dev + DEV_CHAIN);
	base = *(double *)(dev + DEV_BASE_CLK);
	if (!(base >= CLOCK_FLOOR && base <= 1500)) /* not a real board clock (also NaN): touch nothing */
		return;
	/* did the function broadcast a clock to every chip? It ramps (chip address 0) exactly when the requested
	 * clock differs from the old board clock (0x1be080 / 0x1be15a); a NaN compares false there, so no ramp. */
	req = frame ? *(double *)(frame + FRAME_REQ_CLK) : base;
	old = frame ? *(double *)(frame + FRAME_OLD_CLK) : base;
	moved_clock = req > old || req < old;
	if (moved_clock)
		for (i = 0; i < MAX_CHIPS; i++)
			applied[c][i] = 0; /* the broadcast put every chip on the board clock */

	for (i = 0; i <= nchips; i++)
		want[i] = base;
	pthread_mutex_lock(trim_lock);
	nt = read_trims(t, 256);
	for (i = 0; i < nt; i++) {
		double voff;
		if (t[i].chain != chain || t[i].chip < 1 || t[i].chip > nchips)
			continue;
		voff = t[i].mhz;
		/* the panel's limits exactly (readChipTrims drops any other line): -100 .. +50 in 12.5 steps. Clamping
		 * a hand-edited +62.5 to +50 would run the chip 50 MHz over the board while the panel shows no trim. */
		if (!(voff >= TRIM_MIN && voff <= TRIM_MAX) || voff * 2 / 25 != (double)(int)(voff * 2 / 25))
			continue;
		if (voff > 0 && base + voff > CHIP_CEIL) { /* never above the top stock clock: the chip stays on its board */
			want[t[i].chip] = base;
			skipped++;
			continue;
		}
		want[t[i].chip] = base + voff >= CLOCK_FLOOR ? base + voff : base; /* (the last line for a chip counts) */
	}
	pthread_mutex_unlock(trim_lock);

	for (i = 1; i <= nchips; i++) {
		double phys, target = want[i];
		if (target == base && applied[c][i] == 0)
			continue; /* untrimmed and never moved by this library: the miner's own, untouched */
		if (moved_clock)
			phys = base; /* the broadcast ramp left it on the board clock */
		else if (have_snap && snap[c][i] >= CLOCK_FLOOR && snap[c][i] <= base + TRIM_MAX)
			phys = snap[c][i]; /* the clock unchanged: the chip is where the table said before the reset */
		else
			phys = applied[c][i] > 0 ? applied[c][i] : base;
		if (!moved_clock && !same10(phys, base) && !(applied[c][i] > 0 && same10(phys, applied[c][i])) && target > phys) {
			target = phys; /* the miner slowed it since (DTFS): a re-apply never raises it, only slows further */
			held++;
		}
		if (!same10(target, phys)) {
			from[i] = phys;
			want[i] = target;
			moving[set++] = i;
		}
		if (!same10(target, base))
			trimmed++;
		applied[c][i] = same10(target, base) ? 0 : target;
		*(float *)(dev + DEV_CLK_TABLE + 4 * i) = (float)target; /* keep the miner's table in step with the chip */
	}
	ramp_all(dev, set, moving, from, want);
	if (set || skipped || held || moved_clock || posts[c] == 0) {
		int n = snprintf(msg, sizeof msg,
		                 "%ld chain %d: board %d.%d MHz%s, %d chip(s) trimmed, %d changed now%s%s\n", time(0), chain,
		                 tenths(base) / 10, tenths(base) % 10, moved_clock ? " (clock changed)" : "", trimmed, set,
		                 skipped ? ", a + trim over 690 MHz skipped" : "", held ? ", a chip the miner slowed kept as is" : "");
		logline(msg, n);
	}
	posts[c]++;
	if (!test_mode)
		write_stats();
}

/* Register the hook, only inside the patched miner (the library is loaded nowhere else, but check). */
__attribute__((constructor)) static void scbox2_chip_init(void)
{
	char exe[128];
	long n = readlink("/proc/self/exe", exe, sizeof exe - 1);
	static const char want[] = HOOK_EXE;
	int i;
	if (n != (long)(sizeof want - 1))
		return;
	for (i = 0; i < n; i++)
		if (exe[i] != want[i])
			return;
	started_at = time(0);
	*(void (**)(char *, char *, int))SLOT_ADDR = scbox2_chip_hook;
	{
		char msg[96];
		int k = snprintf(msg, sizeof msg, "%ld hook registered (pid %d)\n", time(0), getpid());
		logline(msg, k);
	}
}

/* test harness hooks (not used by the miner) */
__attribute__((used)) void scbox2_chip_test_setup(void *fn) { test_mode = 1; set_clock = (set_clock_fn)fn; }
__attribute__((used)) void scbox2_chip_test_started(long t) { started_at = t; }
__attribute__((used)) void scbox2_chip_test_stats(void) { write_stats(); }
__attribute__((used)) void scbox2_chip_test_paths(const char *trim, const char *stats, const char *stats_tmp, const char *log)
{
	TRIM_FILE = trim;
	STATS_FILE = stats;
	STATS_TMP = stats_tmp;
	LOG_FILE = log;
}
