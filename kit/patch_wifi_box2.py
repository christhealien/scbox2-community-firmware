#!/usr/bin/env python3
"""Keep SC Box II wifi daemon (stock fw 2.2.1 /usr/bin/wifi, md5 e65f44416dc88bc23aefaaf63333c05c)
from routing its soft-AP clients onto the owner's LAN.

usage: patch_wifi_box2.py <in> <out>

Re-derived by research lane R06. At 0xcd9e the daemon does "movs r0,#1; bl 0xc8a4", writing "1" to
/proc/sys/net/ipv4/ip_forward when it brings up an access point (the SoC kernel has no netfilter, so AP
clients would be bridged straight onto the LAN). "movs r0,#1" -> "movs r0,#0" writes "0": forwarding stays
off. Wi-Fi client mode is untouched. Original bytes + stock md5 are checked. Exposes patch(data) -> data.
"""
import hashlib
import sys

STOCK_MD5 = "e65f44416dc88bc23aefaaf63333c05c"  # SC Box II 2.2.1 /usr/bin/wifi
BASE = 0x8000

SITES = (
    (0xcd9e, "0120", "0020", "AP start: ip_forward stays 0 (no routing of AP clients to the LAN)"),
)


def patch(data):
    d = bytearray(data)
    if hashlib.md5(d).hexdigest() != STOCK_MD5:
        raise SystemExit("wifi: not the stock SC Box II 2.2.1 wifi: not patched")
    for va, orig, new, why in SITES:
        o = va - BASE
        ob, nb = bytes.fromhex(orig), bytes.fromhex(new)
        assert len(ob) == len(nb)
        if bytes(d[o:o + len(ob)]) != ob:
            raise SystemExit("wifi: original bytes don't match at 0x%x (%s): %s" % (va, why, bytes(d[o:o + len(ob)]).hex()))
        d[o:o + len(nb)] = nb
    return bytes(d)


def main():
    src, dst = sys.argv[1], sys.argv[2]
    d = patch(open(src, "rb").read())
    open(dst, "wb").write(d)
    print("%s: ip_forward write neutralised, md5 %s" % (dst, hashlib.md5(d).hexdigest()))


if __name__ == "__main__":
    main()
