/*
 * scbox2_bind.so — loaded with LD_PRELOAD into the stock miner daemons (minerd: web API on :80,
 * intminer: cgminer API on :4028) so their TCP listening sockets bind to loopback only. Only the
 * scbox2-agent panel on the same box can reach them; the LAN sees nothing on those ports. UDP and
 * outgoing connections are untouched.
 *
 * The kernel has no netfilter, so this replaces a firewall rule. Freestanding: no libc calls except
 * __errno_location (resolved from the process's own uClibc when the library loads), raw EABI syscalls.
 */
typedef unsigned int u32;
typedef unsigned short u16;

#define AF_INET 2
#define AF_INET6 10
#define SOL_SOCKET 1
#define SO_TYPE 3
#define SOCK_STREAM 1
#define NR_bind 282
#define NR_getsockopt 295

extern int *__errno_location(void);

static long sc5(long n, long a, long b, long c, long d, long e)
{
	register long r0 __asm__("r0") = a;
	register long r1 __asm__("r1") = b;
	register long r2 __asm__("r2") = c;
	register long r3 __asm__("r3") = d;
	register long r4 __asm__("r4") = e;
	register long r7 __asm__("r7") = n;
	__asm__ volatile("svc 0" : "+r"(r0) : "r"(r1), "r"(r2), "r"(r3), "r"(r4), "r"(r7) : "memory");
	return r0;
}

int bind(int fd, const void *addr, u32 len)
{
	unsigned char buf[128];
	const void *use = addr;
	int type = 0;
	u32 tl = sizeof(type);

	if (addr && len >= 2 && len <= sizeof(buf) &&
	    sc5(NR_getsockopt, fd, SOL_SOCKET, SO_TYPE, (long)&type, (long)&tl) == 0 && type == SOCK_STREAM) {
		const unsigned char *s = addr;
		u32 i;
		u16 fam;
		for (i = 0; i < len; i++)
			buf[i] = s[i];
		fam = (u16)(buf[0] | (buf[1] << 8));
		if (fam == AF_INET && len >= 8) {
			/* sin_addr at offset 4 → 127.0.0.1 */
			buf[4] = 127; buf[5] = 0; buf[6] = 0; buf[7] = 1;
			use = buf;
		} else if (fam == AF_INET6 && len >= 24) {
			/* sin6_addr at offset 8 → ::1 */
			for (i = 8; i < 23; i++)
				buf[i] = 0;
			buf[23] = 1;
			use = buf;
		}
	}
	{
		long r = sc5(NR_bind, fd, (long)use, len, 0, 0);
		if (r < 0 && r > -4096) {
			*__errno_location() = (int)-r;
			return -1;
		}
		return (int)r;
	}
}
