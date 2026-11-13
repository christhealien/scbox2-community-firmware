#!/usr/bin/env python3
"""Patch SC Box II xminerd (stock 2.2.1 rootfs /usr/bin/xminerd, md5 0ebc19f06cc9d91322109c9a462f5d54):
the fan check's "learned RPM" record (p1) and the net check (127.0.0.2:80 first, no outside hosts).

usage: patch_xminerd_box2.py <in> <out>

Same changes as the SC Lite kit's patch_xminerd.py (p1 + NETCHECK + NETCHECK_HOSTS), re-derived for this binary.
The SC Box II xminerd is an older build of the same source (21024 bytes vs 23864): the fan-check function
(0xa02c here, 0xa05c on the SC Lite), the boot sweep (0xa260 / 0xa290) and the net-check connect function
(0xa95c / 0xa98c) are the same instructions, 0x30 lower; their bytes differ only in the blx displacements to the
(unmoved) PLT. The one logic difference in the main loop: the SC Lite build also stops/refuses the miner on
miner_state bit 0x8000; this build only on bit 0x80. Nothing here touches that.

Fan check (function 0xa02c, duty >= 30 branch at 0xa176; stock): if table[fan][duty] is unset (-1) it compares
the RPM with 60 % of the boot-sweep value at floor(duty/10)*10 AND stores the current RPM as table[fan][duty],
flagged or not. p1 (10 bytes at 0xa1a4): store only when the fan passed. Never more lenient than stock (see
REPORT.md: proof plus an exhaustive qemu run of the real function, stock vs patched).

Net check (main loop, every 3 s; stock order: the pools' host:port if the pool file exists, then
www.baidu.com:80, then www.google.com:80). Both fallbacks become 127.0.0.2:80 and the order becomes
127.0.0.2:80 first, then the pools exactly as stock, then 127.0.0.2:80 once more.
"""
import hashlib, sys

STOCK_MD5 = "0ebc19f06cc9d91322109c9a462f5d54"
BASE = 0x8000  # first PT_LOAD: vaddr 0x8000 at file offset 0

# p1: at 0xa1a4 "str.w r3,[r9,r10,lsl#2]; ite ge; movge r0,#0; movlt r0,#1"
#  -> "itte ge; strge.w r3,[r9,r10,lsl#2]; movge r0,#0; movlt r0,#1"
# (flags come from "cmp r3, r0" at 0xa1a2: r3 = this reading, r0 = idiv(6*sweep[floor(duty/10)*10], 10))
FAN = (0xa1a4, "49f82a30" "acbf" "0020" "0120", "a6bf" "49f82a30" "0020" "0120")

# The whole stock "unset entry" block 0xa176..0xa1cc (86 bytes) must be byte-identical to the SC Lite's 0xa1a6
# block (the kit's ORIG_A1A6): checked so the proof made for the SC Lite carries over.
ORIG_A176 = bytes.fromhex(
    "59f82a00" "431c" "19d1" "a358" "0a21" "5046" "0392" "0293" "01f0a4fb" "0a21" "4843" "0621"
    "59f82000" "4843" "0a21" "01f09bfb" "029b" "039a" "8342" "49f82a30" "acbf" "0020" "0120" "3851"
    "0ce0" "0623" "0a21" "5843" "0392" "01f08bfb" "039a" "a158" "8142" "acbf" "0020" "0120" "3851")
assert len(ORIG_A176) == 86

NETCHECK = (
    (0x9ae8, "8fb9", "00bf"),  # cbnz r7,0x9b0e (pool file exists: pools first) -> nop: falls into "b 0x9afc" (www.baidu.com)
    (0x9b0c, "54e0", "ffe7"),  # b 0x9bb8 (baidu failed: www.google.com)        -> b 0x9b0e: the pools
    (0x9b20, "ecdd", "4add"),  # ble 0x9afc (no pools in the file: baidu)       -> ble 0x9bb8: www.google.com
    (0x9b3e, "ddd1", "3bd1"),  # bne 0x9afc (pool entry unreadable: baidu)      -> bne 0x9bb8: www.google.com
    (0x9bb6, "a1e7", "ffe7"),  # b 0x9afc (no pool reachable: baidu)            -> b 0x9bb8: www.google.com
)

# Referenced only from the literal pool words at 0x9bfc and 0x9c04 (ldr r0 at 0x9afc and 0x9bb8).
NETCHECK_HOSTS = (
    (0xc0fb, b"www.baidu.com:80\0", b"127.0.0.2:80\0\0\0\0\0"),
    (0xc11b, b"www.google.com:80\0", b"127.0.0.2:80\0\0\0\0\0\0"),
)


def _put(d, va, orig, new, what):
    off = va - BASE
    assert len(orig) == len(new)
    if bytes(d[off:off + len(orig)]) != orig:
        raise SystemExit("xminerd: original bytes don't match at 0x%x (%s)" % (va, what))
    d[off:off + len(new)] = new


def patch(data):
    d = bytearray(data)
    if hashlib.md5(d).hexdigest() != STOCK_MD5:
        raise SystemExit("xminerd: not the stock SC Box II 2.2.1 xminerd: not patched")
    if bytes(d[0xa176 - BASE:0xa176 - BASE + 86]) != ORIG_A176:
        raise SystemExit("xminerd: fan-check block at 0xa176 differs from the SC Lite one")
    va, orig, new = FAN
    _put(d, va, bytes.fromhex(orig), bytes.fromhex(new), "fan check")
    for va, orig, new in NETCHECK_HOSTS:
        _put(d, va, orig, new, "net check host")
    for va, orig, new in NETCHECK:
        _put(d, va, bytes.fromhex(orig), bytes.fromhex(new), "net check")
    return bytes(d)


def main():
    src, dst = sys.argv[1], sys.argv[2]
    d = patch(open(src, "rb").read())
    open(dst, "wb").write(d)
    print("%s: p1 + net check applied, md5 %s" % (dst, hashlib.md5(d).hexdigest()))


if __name__ == "__main__":
    main()
