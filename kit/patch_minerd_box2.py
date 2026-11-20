#!/usr/bin/env python3
"""Network/robustness cuts for SC Box II minerd (stock fw 2.2.1 /usr/bin/minerd,
md5 b49e8ca23e4931666ccff0bcfebc37e8): the web backend that serves the miner's :80 HTTP API.

usage: patch_minerd_box2.py <in> <out>

Re-derived for this build by research lanes R06/R07 (see $P/R06, $P/R07 reports); these are the Box II
equivalents of the SC Lite build.py "step 3a" minerd edits, retargeted to this binary. Every original byte
is checked and the stock md5 is gated before anything is written. Exposes patch(data) -> data like the SC
Lite patch scripts. All addresses are virtual; file offset = va - 0x8000 (first PT_LOAD at va 0x8000, off 0).

Sites (HARD RULE 6: no OEM cloud / Chinese hosts / internet unless the owner enabled it):
  0xb7be  cloud client thread start (entry 0x1d920): POST to the OEM cloud hub /sign/post, then MQTT to the
          returned broker (topic /server/<id>/#), remote pools/power/firmware download via curl + factory
          reset.  bl 0x1def0 -> nop.w.
  0xb7ca  UDP-1413 responder thread start (entry 0x172a4, socket bound INADDR_ANY:1413): an unauthenticated
          LAN broadcast naming the MAC sets the box's IP/DNS and reboots it.  bl 0x178fc -> nop.w.
  0x18c7e monitor thread (entry 0x18b4e, KEPT: it still does the overheat/chip-error shutdowns) HTTPS POST of
          model/SN/MAC/IP/miner-name to the OEM miner finder (:443/api/miners) while uptime < 59 s; result unused.
          bl 0x1a310 -> nop.w.
  0x17b6e eventhub "sendip" key (0x17a60): after the LAN broadcast to 255.255.255.255:1314 it calls the same
          miner-finder POST; result unused (the LAN broadcast itself is kept).  bl 0x1a310 -> nop.w.
  0xb6c0  signal(SIGTTIN=0x15, SIG_IGN) -> signal(SIGPIPE=0x0d, SIG_IGN): the miner no longer dies on a broken
          pipe (a closed API/web client).  movs r0,#0x15 -> movs r0,#0x0d.
  0x1de9c PUT /mcb/cmossetting told the (now absent) cloud client its settings changed through a struct that
          only the client's start allocates (ldr r3,[r4,#0x18]; ldr r0,[r3,#0x3c] -> NULL+0x3c deref). Same
          fix as SC Lite 0x1db94: "mov r5,r7; b 0x1deb2" keeps the handler's result and skips the call.
  0x1ff0d DNS fallback string (only user 0x12390, at start-up 0x14cdc and after a static-IP set without DNS):
          if /tmp/resolv.conf was missing/empty it ran
          system('echo "nameserver 114.114.114.114" > /etc/resolv.conf') -- a public DNS server in China.
          The command becomes "true" (NUL-padded, same length): no DNS server is invented.

A failed save never blocks a plan (v0.5.2). PUT /mcb/setting's handler (0xf4cc; "Set Settings: " at 0xf4ea) builds
the plan (0xf334), saves the request as cpbsetting.json (0xf5a0: bl 0x13538, the OEM's JSON-to-file writer, which
logs its own "Open File"/"Write File %s Error"), sets the temperature target (0xf5be: bl 0xf430), rewrites
bfgminer.json with the new plan line (0xf5d4: bl 0x143c4), answers 200 (0xf5f8) and only then hands the plan to the
miner (0xf63c..0xf646). Each failed step jumped to the shared error tail (0xf572: log, r5 = 3, skip everything after
it): with the settings partition read-only every fan step and the thermal guard's 100 % were dropped (seen on a
miner at 83 C). Two sites:
  0xf5aa  after the cpbsetting.json save: "bge 0xf5be" -> "b 0xf5be": a failed save goes on with the plan (the writer
          has logged it). (cmp r0,#0 at 0xf5a8 is left; its flags are unused now.)
  0xf5e2  the bfgminer.json step's failure branch (mov r0,r4; blx lock; ldr r1,=msg; add r0,sp,#0x48 -- 10 bytes,
          0xf5ec onward is shared with two other error paths and stays) becomes: ldr r1,=0x1f698 ("Dump To
          bfgminer.json Error", the literal at 0xf688 the original loaded); movs r0,#0; bl 0x117f8 (the log call the
          tail makes, level 0 as there); b 0xf5f2 (the 200 answer and the hand-over to the miner).
With the settings in RAM (v0.5.2's cfgmount) neither save normally fails; this keeps the fans independent of the
storage even when the RAM copy couldn't be made (a full RAM folder, or the partition used directly).

NOTE (left to the build/init lanes, not done here per the design's EXACT BINARY PATCH SITES list): research
lane R06 also found minerd's only wifi_ap_enable caller at 0xb3f70eea / va 0x1ab3a (brings up hostapd
"GSMINER" WPA2 "123456789" + dnsmasq + ip_forward=1). The design kills the stock open-AP via the wifi binary
(patch_wifi_box2.py, ip_forward=0) and the init/Wi-Fi lanes (wifi.json default, removed AP daemons), not a
minerd patch, so this module leaves 0x1ab3a alone; see REPORT.md.
"""
import hashlib
import sys

STOCK_MD5 = "b49e8ca23e4931666ccff0bcfebc37e8"  # SC Box II 2.2.1 /usr/bin/minerd
BASE = 0x8000

# (va, orig_hex, new_hex, why)
SITES = (
    (0xb6c0, "1520", "0d20", "signal SIGTTIN->SIGPIPE ignored (no death on broken pipe)"),
    (0xb7be, "12f097fb", "aff30080", "cloud client thread (the OEM cloud hub, MQTT) not started"),
    (0xb7ca, "0cf097f8", "aff30080", "UDP 1413 set-IP-and-reboot responder not started"),
    (0x17b6e, "02f0cffb", "aff30080", "sendip key: OEM miner-finder POST not called (LAN broadcast kept)"),
    (0x18c7e, "01f047fb", "aff30080", "monitor thread: OEM miner-finder POST not called"),
    (0x1de9c, "a3693d46", "3d4608e0", "PUT /mcb/cmossetting no longer derefs the absent cloud client"),
    (0x1ab3a, "f0f70eea", "aff30080", "wifi_ap_enable starter not called (defense-in-depth: the Wi-Fi stack is removed and product.json forces wifi:0, so the open AP can never come up anyway)"),
    # v0.5.2: PUT /mcb/setting goes on with the plan when a save fails (see the docstring)
    (0xf5aa, "08da", "08e0", "PUT /mcb/setting: a failed cpbsetting.json save no longer drops the plan (bge -> b)"),
    (0xf5e2, "2046fbf77aec274912a8", "2949002002f007f902e0",
     "PUT /mcb/setting: a failed bfgminer.json update is logged and the plan still goes to the miner"),
)

# (va, orig_bytes, replacement_bytes) -- replacement is NUL-padded to the original length.
STRINGS = (
    (0x1ff0d, b'echo "nameserver 114.114.114.114" > /etc/resolv.conf\0', b"true"),
)

# Dead host strings left in minerd after the code that used them is NOP'd (the cloud/MQTT thread, the
# miner-finder POSTs) or the resolv.conf command is rewritten (above): a second "Set Default DNS"
# 114.114.114.114 display string (VA 0x1ef5f) and the OEM cloud's two host names (below: the binary's own
# strings, kept verbatim so the scrub finds them). They are never
# reached now, but the leak-check gate (build_box2 --check) is zero-tolerance, so overwrite every remaining
# occurrence with a harmless loopback address, length-preserved. At least one of each must be present (a
# changed binary is caught). A2 flagged these for scrubbing.
SCRUB_HOSTS = (
    b"114.114.114.114",
    b"find.goldshell.com",
    b"hub.goldshell.com",
)


def patch(data):
    d = bytearray(data)
    if hashlib.md5(d).hexdigest() != STOCK_MD5:
        raise SystemExit("minerd: not the stock SC Box II 2.2.1 minerd: not patched")
    for va, orig, new, why in SITES:
        o = va - BASE
        ob, nb = bytes.fromhex(orig), bytes.fromhex(new)
        assert len(ob) == len(nb)
        if bytes(d[o:o + len(ob)]) != ob:
            raise SystemExit("minerd: original bytes don't match at 0x%x (%s): %s" % (va, why, bytes(d[o:o + len(ob)]).hex()))
        d[o:o + len(nb)] = nb
    for va, orig, rep in STRINGS:
        o = va - BASE
        if bytes(d[o:o + len(orig)]) != orig:
            raise SystemExit("minerd: original string doesn't match at 0x%x" % va)
        d[o:o + len(orig)] = rep + b"\0" * (len(orig) - len(rep))
    # scrub every remaining dead host string (loopback, length-preserved)
    loop = b"127.0.0.1"
    for host in SCRUB_HOSTS:
        n = 0
        start = 0
        while True:
            i = d.find(host, start)
            if i < 0:
                break
            d[i:i + len(host)] = loop + b"\0" * (len(host) - len(loop))
            start = i + len(host)
            n += 1
        if n == 0:
            raise SystemExit("minerd: expected host string %r to scrub but none found" % host)
    return bytes(d)


def main():
    src, dst = sys.argv[1], sys.argv[2]
    d = patch(open(src, "rb").read())
    open(dst, "wb").write(d)
    print("%s: minerd network cuts applied, md5 %s" % (dst, hashlib.md5(d).hexdigest()))


if __name__ == "__main__":
    main()
