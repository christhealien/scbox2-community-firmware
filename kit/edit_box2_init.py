#!/usr/bin/env python3
"""
B8: SC Box II init-script and security hardening edits.

These are the init/host/security transforms for the Box II image.  They are the B8-owned policy that
``build_box2.py``'s ``edit_init_scripts(t)`` must apply (see REPORT.md for the integration spec).  Every
edit asserts the exact stock bytes it expects (an ``md5`` anchor where a whole file matters, a
``replace_once`` where a line matters) and raises ``Fail`` if they are not there, so a future stock-firmware
revision stops the build instead of silently shipping an un-hardened image -- the same convention as the SC
Lite ``build.py``.

``apply(t, bind_path=..., root_hash=...)`` runs them all against A2's ``Tree`` object ``t`` (it uses only
``t.text``, ``t.read``, ``t.exists``, ``t.put``, ``t.remove``).  Each function is also exposed on its own so
the test harness can drive it against a plain rootfs copy.

Nothing here removes the Wi-Fi / OpenSSH binaries themselves: those are reclaimed by the removal groups in
``removals_box2.py`` (so the squashfs actually shrinks).  This module only rewrites the *scripts and account
files* that reference them.

NOTE on /etc/hosts: the SC Lite kit adds ``127.0.0.1 hub.goldshell.com`` / ``find.goldshell.com`` redirect
lines.  On the Box II those two hostnames are scrubbed *out of the binaries* by A3's string overwrites (per
A2's --check gate, which greps the final image for those literals and would FAIL if we also wrote them into
/etc/hosts).  So B8 adds ONLY the ``127.0.0.2 <name>-netcheck`` reverse name xminerd's on-box check needs.
"""
import hashlib
import re

# md5 of each stock file this module rewrites whole-cloth (SC Box II 2.2.1 system squashfs).
STOCK_MD5 = {
    "/etc/init.d/S60wdt":    "0c6a347ef4e17497aafb3317980c634a",
    "/etc/inittab":          "3fc7171a18cbfff82f835be7e277db43",
    "/etc/init.d/S38initdir": "014c82923155d9e00afc6c7ba5efab5a",
    "/etc/fstab":            "3ca14841641e479c2edf053b2021a3ed",
    "/etc/mdev/sd_card_insert": "337a03c91c188f51eb8128cc6b2096b8",
    "/etc/mdev/udisk_insert":   "c358964a7d9e341fb553f4d5eddb948d",
    "/etc/init.d/S20urandom":   "87002c6061a544fe7a439c3d5d936822",
    "/etc/init.d/S40network":   "50fb24c56170fb6c9c442c7ea8464a2e",
}

# Documented recovery root password for the serial console (/bin/login reads /etc/shadow).  The stock image
# ships root with an *unknown* DES hash ("kRmQ3yGkGZThM" / "VqHlh2uTs0W4Y" in shadow/shadow-); we overwrite
# both with a FRESH MD5-crypt ($1$) hash of a documented password so the owner has a known way back in at the
# UART (a person at the UART can reflash the SD card anyway, so this is recovery convenience, not the trust
# boundary).  MD5-crypt, not SHA-512 ($6$): the miner's uClibc 0.9.33.2 crypt() reliably does DES and MD5 but
# may lack SHA-512, so /bin/login could reject a $6$ hash and lock the console.  Reproducible: fixed salt, so
# the same build inputs give the same hash.  build_box2.py passes this same hash (root_hash=...) explicitly.
#
#   recovery password (plaintext): scbox2-recover
#   salt:                          scbox2re   (MD5-crypt uses at most 8 salt characters)
#   $1$ hash (openssl passwd -1 -salt scbox2re scbox2-recover):
RECOVERY_ROOT_PW = "scbox2-recover"
RECOVERY_ROOT_SALT = "scbox2re"
RECOVERY_ROOT_HASH = "$1$scbox2re$Q1YId9c5RLjHvWSUOalzX1"

# the stock root password fields we expect to find and overwrite (so a stock revision that changed them
# stops the build rather than leaving an unknown hash in place).
STOCK_ROOT_FIELDS = {"/etc/shadow": "kRmQ3yGkGZThM", "/etc/shadow-": "VqHlh2uTs0W4Y"}


class Fail(Exception):
    pass


def _replace_once(text, old, new, what):
    n = text.count(old)
    if n != 1:
        raise Fail("%s: expected exactly one %r, found %d" % (what, old, n))
    return text.replace(old, new)


def _assert_md5(t, path):
    got = hashlib.md5(t.read(path)).hexdigest()
    want = STOCK_MD5.get(path)
    if want and got != want:
        raise Fail("%s md5 %s != expected %s (not the firmware B8 was built against)" % (path, got, want))


# ---------------------------------------------------------------- /etc/inittab
def edit_inittab(t, bind_path):
    """Serial getty -> /bin/login (drop -n); remove the ttyAMA3 Wi-Fi line; run minerd through the
    scbox2_bind.so LD_PRELOAD shim guarded by a sha256 self-check (as SC Lite build.py does).  fanctrl and
    xminerd are left untouched."""
    _assert_md5(t, "/etc/inittab")
    s = t.text("/etc/inittab")

    # getty: drop -n and swap /bin/sh for /bin/login (so the serial console authenticates via /etc/shadow,
    # and without -n getty waits at its login: prompt instead of respawning login every 60 s).
    s = _replace_once(s,
                      "/sbin/getty -L -n -l /bin/sh ttyAMA3 115200 vt100",
                      "/sbin/getty -L -l /bin/login ttyAMA3 115200 vt100",
                      "/etc/inittab getty")

    # remove the stock open-AP Wi-Fi daemon line (no Wi-Fi chip; the daemon is removed from the image)
    s = _replace_once(s, "ttyAMA3::respawn:wifi\n", "", "/etc/inittab wifi line")

    # minerd via the loopback-bind shim, with a sha256 self-check so a tampered/old shim can't silently load
    sha = hashlib.sha256(open(bind_path, "rb").read()).hexdigest()
    shim = ("ttyAMA3::respawn:/bin/sh -c 'echo \"%s  /lib/scbox2_bind.so\" | sha256sum -c -s && "
            "LD_PRELOAD=/lib/scbox2_bind.so exec minerd; sleep 60'\n" % sha)
    s = _replace_once(s, "ttyAMA3::respawn:minerd\n", shim, "/etc/inittab minerd line")

    # Deterministic miner launcher: xminerd's own (Model,Algo) lookup falls through on this firmware and
    # never starts the miner (confirmed on hardware), so start it here. Added right after the minerd line so
    # the miner comes up with the panel/pools. The script waits for a pool, runs /usr/bin/bfgminer -c <cfg>
    # (the wrapper), and relaunches when it exits; it no-ops while another miner is already running.
    if "/usr/libexec/scbox2/miner-run" not in s:
        s = _replace_once(s, shim, shim + "ttyAMA3::respawn:/usr/libexec/scbox2/miner-run\n",
                          "/etc/inittab add miner-run launcher")

    t.put("/etc/inittab", s.encode(), 0o644)
    return s


# ---------------------------------------------------------------- /etc/init.d/S60wdt
def edit_s60wdt(t):
    """Assert md5; delete the two ``modprobe imapx_wdt.ko`` lines (no watchdog driver on the Box II, so they
    only print errors); replace the four leaking ntpd lines (two Chinese IPs, 0.pool.ntp.org,
    asia.pool.ntp.org -- plus the commented aliyun server) with the SC Lite NTP loop."""
    _assert_md5(t, "/etc/init.d/S60wdt")
    s = t.text("/etc/init.d/S60wdt")

    s = _replace_once(s, "    modprobe imapx_wdt.ko tmr_atboot=0 tmr_margin=10000\n", "",
                      "/etc/init.d/S60wdt modprobe (start)")
    s = _replace_once(s, "    modprobe -r imapx_wdt.ko\n", "",
                      "/etc/init.d/S60wdt modprobe (stop)")

    ntp_block = ("    #ntpd -dn -p ntp5.aliyun.com &\n"
                 "    busybox ntpd -nN -p 202.120.2.101 &\n"
                 "    busybox ntpd -nN -p 202.112.31.197 &\n"
                 "    busybox ntpd -nN -p 0.pool.ntp.org &\n"
                 "    busybox ntpd -nN -p asia.pool.ntp.org &\n")
    loop = ("    # scbox2: one NTP loop (panel servers, then DHCP-advertised, then pool.ntp.org; "
            "switchable in Settings -> Time)\n"
            "    /usr/libexec/scbox2/ntp >/dev/null 2>&1 &\n")
    s = _replace_once(s, ntp_block, loop, "/etc/init.d/S60wdt ntp block")

    for bad in ("202.120.2.101", "202.112.31.197", "asia.pool.ntp.org", "aliyun", "imapx_wdt"):
        if bad in s:
            raise Fail("/etc/init.d/S60wdt still contains %r after the edit" % bad)
    t.put("/etc/init.d/S60wdt", s.encode(), 0o755)
    return s


# ---------------------------------------------------------------- /etc/init.d/S38initdir (config mount)
# The hostname set in the panel. /etc/hostname is on the read-only squashfs (it says "qsdk", which init applies
# at sysinit), so the panel keeps the name on the config partition (/mnt/config/scbox2/hostname) and it is put
# back here: right after the config partition is mounted and BEFORE S40network brings eth0 up, so the DHCP
# request already carries the owner's name. Only a name the panel itself would accept is applied (1-63
# letters, digits or hyphens, not starting/ending with a hyphen, not "localhost"); anything else is ignored
# and the stock name stays.
HOSTNAME_RESTORE = (
    "        # the hostname set in the panel (kept on the config partition: /etc is read-only); before the network\n"
    "        h=$(head -n 1 /mnt/config/scbox2/hostname 2>/dev/null | tr -d ' \\t\\r')\n"
    "        if [ -n \"$h\" ] && [ \"$h\" != localhost ] &&\n"
    "            echo \"$h\" | grep -qE '^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$'; then\n"
    "            hostname \"$h\" && echo \"S38initdir: hostname $h\"\n"
    "        fi\n")


def edit_s38initdir(t):
    """Mount /mnt/config with umask=077,noatime,errors=remount-ro (the FAT holds panel secrets; 077 keeps a
    stolen card's files owner-only, errors=remount-ro stops writes to a failing flash), and verify the mount
    succeeded -- on failure raise /tmp/scbox2-cfg-broken (S48 reads it) and keep booting."""
    _assert_md5(t, "/etc/init.d/S38initdir")
    s = t.text("/etc/init.d/S38initdir")
    old = "        mount -t vfat  /dev/spiblock2 /mnt/config\n"
    new = ("        if mount -t vfat -o umask=077,noatime,errors=remount-ro /dev/spiblock2 /mnt/config; then\n"
           "            rm -f /tmp/scbox2-cfg-broken\n"
           "            # this miner's own random seed (kept here, not the image's shared /etc/random-seed):\n"
           "            # into the kernel's pool before anything makes a key (SSH, https, sessions), then renewed\n"
           "            [ -x /usr/libexec/scbox2/seed ] && /usr/libexec/scbox2/seed load\n"
           "        else\n"
           "            echo \"S38initdir: /mnt/config mount failed\" > /tmp/scbox2-cfg-broken\n"
           "        fi\n"
           + HOSTNAME_RESTORE)
    s = _replace_once(s, old, new, "/etc/init.d/S38initdir mount")
    t.put("/etc/init.d/S38initdir", s.encode(), 0o755)
    return s


# ---------------------------------------------------------------- /etc/init.d/S20urandom (random seed)
S20URANDOM = """#! /bin/sh
#
# urandom: the random seed (scbox2). Each miner keeps its own seed on the config partition
# (/mnt/config/random-seed, see /usr/libexec/scbox2/seed): the read-only system image's /etc/random-seed is the
# same on every unit and isn't used. This script runs before /mnt/config is mounted, so S38initdir loads the
# seed right after mounting it; this script saves a fresh one at shutdown (S48scbox2 also saves one every 6 h).

[ -c /dev/urandom ] || exit 0

case "$1" in
	start|"")
		;;
	stop)
		[ -x /usr/libexec/scbox2/seed ] && /usr/libexec/scbox2/seed save
		;;
	*)
		echo "Usage: urandom {start|stop}" >&2
		exit 1
		;;
esac
exit 0
"""


def edit_s20urandom(t):
    """The stock script loads and saves /etc/random-seed: on the read-only squashfs that is one fixed file,
    the same on every unit, and never renewed.  Replace it (md5-checked) with the per-unit seed on the config
    partition (files/scbox2-seed, placed by build_box2 at /usr/libexec/scbox2/seed)."""
    _assert_md5(t, "/etc/init.d/S20urandom")
    s = t.text("/etc/init.d/S20urandom")
    if "/etc/random-seed" not in s:
        raise Fail("/etc/init.d/S20urandom: no /etc/random-seed use found (not the stock script)")
    t.put("/etc/init.d/S20urandom", S20URANDOM.encode(), 0o755)
    return S20URANDOM


# ---------------------------------------------------------------- /etc/init.d/S40network (MAC address)
def edit_s40network(t):
    """Run /etc/scbox2-mac (the panel's MAC setting: keep / random / fixed) right after Goldshell's maccfg and
    before eth0 comes up, so DHCP sees the right address from the first packet (as the SC Lite's build.py does
    for /etc/sclite-mac).  The config partition is mounted by S38initdir before this."""
    _assert_md5(t, "/etc/init.d/S40network")
    s = t.text("/etc/init.d/S40network")
    s = _replace_once(s, "\tmaccfg\n\t/sbin/ifup lo\n",
                      "\tmaccfg\n\t[ -x /etc/scbox2-mac ] && /etc/scbox2-mac\t# scbox2: the panel's MAC setting\n"
                      "\t/sbin/ifup lo\n", "/etc/init.d/S40network maccfg")
    t.put("/etc/init.d/S40network", s.encode(), 0o755)
    return s


# ---------------------------------------------------------------- /root/bfgminer.json (miner API)
BFGMINER_JSON_MD5 = "79079908d9bbd10fd3da7d3d450ba4c4"


def edit_bfgminer_json(t):
    """The miner's API (cgminer :4028) allowed every address ("A:0/0", write from 127.0.0.1).  It is bound to
    loopback by scbox2_bind.so anyway; this makes the config say the same: write access from 127.0.0.1 only,
    nothing else.  minerd copies this file to /mnt/config/bfgminer/bfgminer.json on a fresh config partition
    (S48scbox2 fixes a copy made by an older build)."""
    data = t.read("/root/bfgminer.json")
    got = hashlib.md5(data).hexdigest()
    if got != BFGMINER_JSON_MD5:
        raise Fail("/root/bfgminer.json md5 %s != expected %s" % (got, BFGMINER_JSON_MD5))
    s = _replace_once(data.decode(), '"api-allow" : "W:127.0.0.1,A:0/0"', '"api-allow" : "W:127.0.0.1"',
                      "/root/bfgminer.json api-allow")
    t.put("/root/bfgminer.json", s.encode(), 0o664)
    return s


# ---------------------------------------------------------------- /etc/fstab (defense in depth)
def edit_fstab(t):
    """Carry the same secure /mnt/config options in fstab, in case anything triggers a `mount -a` or a
    remount (S38initdir is the live mount; this is belt-and-braces)."""
    _assert_md5(t, "/etc/fstab")
    s = t.text("/etc/fstab")
    s = _replace_once(s,
                      "/dev/spiblock2   /mnt/config        vfat     rw,discard        0       2",
                      "/dev/spiblock2   /mnt/config        vfat     rw,umask=077,noatime,errors=remount-ro,discard  0  2",
                      "/etc/fstab spiblock2")
    t.put("/etc/fstab", s.encode(), 0o644)
    return s


# ---------------------------------------------------------------- /etc/mdev/*insert (removable media)
def edit_mdev_mounts(t):
    """Removable SD/USB media mounted nosuid,nodev: a card cannot carry a setuid-root binary or a device
    node onto the running miner."""
    for path, mnt in (("/etc/mdev/sd_card_insert", "/mnt/sd$var"),
                      ("/etc/mdev/udisk_insert", "/mnt/udisk$var")):
        _assert_md5(t, path)
        s = t.text(path)
        s = _replace_once(s, "\tmount  $point  %s\n" % mnt,
                          "\tmount -o nosuid,nodev $point  %s\n" % mnt, path)
        t.put(path, s.encode(), 0o755)


# ---------------------------------------------------------------- /etc/shadow + /etc/shadow-
def edit_shadow(t, root_hash=None):
    """In BOTH /etc/shadow and its backup /etc/shadow-: disable the empty-password ``default`` account
    (``default::`` -> ``default:*:``) and replace root's unknown stock DES hash with the known $1$ (MD5-crypt)
    recovery hash.  There is no ``goldshell`` user on the Box II, so (unlike SC Lite) it is NOT in the table; a
    ``goldshell`` line, if a future stock revision adds one, is disabled too."""
    rh = root_hash or RECOVERY_ROOT_HASH
    for sh in ("/etc/shadow", "/etc/shadow-"):
        if not t.exists(sh):
            continue
        s = t.text(sh)

        # 1. empty-password 'default' -> locked
        if "\ndefault::" in s or s.startswith("default::"):
            s = s.replace("default::", "default:*:", 1)
        elif not re.search(r"(?m)^default:(\*|!)", s):
            raise Fail("%s: the 'default' account is neither empty-password nor already disabled" % sh)

        # 1b. a 'goldshell' account should not exist on the Box II; disable it if a revision adds one
        m = re.search(r"(?m)^goldshell:([^:]*):", s)
        if m and m.group(1) not in ("*", "") and not m.group(1).startswith("!"):
            s = s[:m.start(1)] + "*" + s[m.end(1):]

        # 2. root: the known $1$ (MD5-crypt) recovery hash in place of the unknown stock DES field
        stock = STOCK_ROOT_FIELDS.get(sh)
        m = re.search(r"(?m)^root:([^:]*):", s)
        if not m:
            raise Fail("%s: no root line" % sh)
        cur = m.group(1)
        if stock and cur == stock:
            s = s[:m.start(1)] + rh + s[m.end(1):]
        elif cur == rh:
            pass  # already applied
        elif cur in ("*", "!") or cur.startswith("!"):
            # root already locked by some earlier step: install the recovery hash anyway (documented way in)
            s = s[:m.start(1)] + rh + s[m.end(1):]
        else:
            raise Fail("%s: root password field %r is not the expected stock DES hash %r" % (sh, cur, stock))

        t.put(sh, s.encode(), 0o600)


# ---------------------------------------------------------------- /etc/hosts
def edit_hosts(t, netcheck_name="scbox2"):
    """Add ONLY the 127.0.0.2 reverse name xminerd's on-box internet check resolves to (the panel).  NO
    hub/find.goldshell.com lines (those are scrubbed from the binaries by A3; writing them here would fail
    build_box2 --check)."""
    s = t.text("/etc/hosts").rstrip("\n")
    for bad in ("hub.goldshell.com", "find.goldshell.com"):
        if bad in s:
            raise Fail("/etc/hosts unexpectedly already contains %r (A3 scrubs these from binaries, not hosts)" % bad)
    line = "127.0.0.2\t%s-netcheck" % netcheck_name
    if line not in s:
        s += ("\n# scbox2: xminerd's on-box internet check resolves here (127.0.0.2:80, the panel). "
              "No outside hosts.\n" + line + "\n")
    t.put("/etc/hosts", s.encode(), 0o644)
    return s


# ---------------------------------------------------------------- driver
def apply(t, bind_path, root_hash=None, netcheck_name="scbox2"):
    """Run every B8 init/security edit against A2's Tree ``t``.  ``bind_path`` is files/scbox2_bind.so.bin
    (for the inittab sha256 self-check)."""
    edit_inittab(t, bind_path)
    edit_s60wdt(t)
    edit_s38initdir(t)
    edit_s20urandom(t)
    edit_s40network(t)
    edit_bfgminer_json(t)
    edit_fstab(t)
    edit_mdev_mounts(t)
    edit_shadow(t, root_hash)
    edit_hosts(t, netcheck_name)
