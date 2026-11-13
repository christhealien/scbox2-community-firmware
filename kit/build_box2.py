#!/usr/bin/env python3
"""
Build a flashable Goldshell SC Box II firmware image with the scbox2-agent panel + hardening.

    SOURCE_DATE_EPOCH=<release date> python3 build_box2.py --firmware firmware.ius --agent scbox2-agent.arm \
        --sshd scbox2-sshd.arm --version 0.5.0 --out rel/scbox2-firmware-v0.5.0.ius --img rel/scbox2-sd-v0.5.0.img \
        --xz --sums --require-startup-check

    python3 build_box2.py --check rel/scbox2-sd-v0.5.0.img   # leak/naming gate only (no build)

See README-box2.md. Every gate below stops the build with a non-zero exit, and nothing is written to the
output paths unless every gate passed (outputs are built in the work dir and moved into place at the end;
stale outputs at those paths are removed first).

Unlike the SC Lite build (an in-place ext2 edit with debugfs: see build.py), the SC Box II root is a
**read-only squashfs** (gzip, 128 KiB blocks, in an 18 MB partition). So the pipeline unpacks the system
squashfs to a tree, transforms the tree, rebuilds the squashfs with the exact stock recipe (R03), and
repacks it into a new +ius container byte-for-byte except the one system item (R01/R02 ius tools).

Pipeline (each step is a named function so the review lanes can audit it):
  1. open firmware.ius, find the system item (type 5, squashfs), extract it.
  2. byte-safe sanity: repack the UNMODIFIED system item and assert the result == the stock .ius (R01 trap:
     never trust a burner's "BURN SUCCESS"; prove the container round-trips here instead).
  3. unsquashfs the system item to a fresh tree as root (modes/owners/devnodes intact; R03: do NOT build from
     base/box2/root, it was made read-only).
  4. transform the tree, in order:
        place_panel_and_helpers()   -- WHERE new files land (panel, S48, SSH server + S47, scbox2_bind.so, ntp,
                                       model files)
        apply_binary_patches()      -- WHERE A3's patch modules are called (stub+TODO when absent)
        edit_init_scripts()         -- init/host edits (B8 policy; done inline as in SC Lite build.py)
        apply_removals()            -- R03 B-rec layout drops (owned here)
  5. mksquashfs with the R03 recipe (gzip, -b 131072, -no-xattrs).
  6. guard the image: REFUSE if > 18,874,368 bytes or not gzip or has xattrs; print free space.
  7. repack with ius_set_system (only the system item changes) -> OUT.ius and the SD OUT.img (16 MiB offset);
     brick-safety: nothing but the system item changed vs stock.
  8. read-back gate: the final .ius's system item == the squashfs built, and unsquashfs of it matches the
     packed tree file by file (sha256, mode, owner, symlink target, device numbers); the SD .img carries the
     .ius byte for byte at 16 MiB.
  9. verify with ius.py the stock `upgrade`/uboot0 checks pass (oversize trap included).
 10. leak/naming gate (check_image) on the .ius AND the SD .img: any hit fails the build.
 11. startup check: the SSH server and the panel FROM the read-back image tree, under qemu-arm, 5 runs, each in
     its own mount/network/PID namespaces, chrooted in the image tree with tmpfs config/tmp dirs. scbox2-sshd
     first: port 22 must answer with its SSH banner and key exchange. Then the panel (-ssh=false -lan80=false
     -https443=false, on a free high port) must serve the page and open nothing more on 22/80/443. Then the panel
     binary is replaced by a crashing stub and the SSH server started afresh: port 22 must still answer. Skipped
     loudly when qemu/unshare/root are missing; --require-startup-check (releases) turns that skip into a failure.
 12. optional --xz (fixed settings, byte-reproducible, read back) and --sums (SHA256SUMS), then publish.

Needs: python3, a squashfs-tools mksquashfs/unsquashfs that does gzip -b 131072 (R03: /tmp/sqt or
$MKSQUASHFS/$UNSQUASHFS), and root (for unsquashfs/mksquashfs to keep owners and device nodes). The startup
check also needs unshare and a static qemu-arm (qemu-arm-static, or QEMU_ARM=path); --xz needs xz.
"""
import argparse, hashlib, importlib.util, os, re, shutil, stat, struct, subprocess, sys, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))
FILES = os.path.join(HERE, "files")
TOOLS = os.path.join(HERE, "tools")
if HERE not in sys.path:                # the kit's own modules (edit_box2_init, removals_box2), also under -I
    sys.path.insert(0, HERE)

# The system partition (items.st: system.18432.fs). The squashfs must be <= this, and in practice must leave a
# safe margin (R03 B-rec leaves ~6.9 MB free). The container's burner writes whole 512-byte sectors.
SYS_PART = 18432 * 1024                 # 18,874,368 bytes (hard ceiling: refuse above this)
SYS_FLOOR_MARGIN = 1024 * 1024          # R03: keep >= 1 MiB free as a hard floor
SQUASHFS_MAGIC = b"hsqs"
COMP_GZIP = 1
SQ_FLAG_NO_XATTR = 0x0200               # superblock NO_XATTR flag (stock flags 0x2c0 = DUP|EXPORT|NO_XATTR)

# Reproducible builds: files this build adds get this mtime, it is the squashfs mkfs time, and it is the
# "built:" line of /etc/scbox2-release, so the same inputs always give the same bytes. Releases set
# SOURCE_DATE_EPOCH to the release date (e.g. SOURCE_DATE_EPOCH=$(date -u -d 2026-11-12 +%s)); without it the
# pinned default below is used (still reproducible, but the image then says "built: 2025-11-01T00:00Z").
BUILD_EPOCH_DEFAULT = 1761955200                                     # 2025-11-01T00:00Z
_sde = os.environ.get("SOURCE_DATE_EPOCH", "").strip()
if _sde and not re.fullmatch(r"[0-9]+", _sde):
    sys.exit("build_box2: SOURCE_DATE_EPOCH must be a whole number of seconds since 1970 (got %r)" % _sde)
BUILD_EPOCH = int(_sde) if _sde else BUILD_EPOCH_DEFAULT
BUILD_EPOCH_SOURCE = "SOURCE_DATE_EPOCH" if _sde else "pinned default; releases set SOURCE_DATE_EPOCH"

# ---- the stock firmware this build was checked against (the binary patches are tied to these bytes) -------
# Any other system item stops the build unless --allow-unknown is given (testing only).
KNOWN_FIRMWARE = {"063494ba70fe9e923b96ea107070324f": "SC Box II 2.2.1 (system item md5)"}

# ---- release version: the panel's in-place update (updimage.go iusAssetRe) only accepts an image whose asset
# name is <brand>-firmware-v?<ver>.ius with <ver> = [0-9][0-9A-Za-z.-]*, so --version must give such a name.
VERSION_RE = re.compile(r"^v?[0-9][0-9A-Za-z.\-]*$")
ASSET_RE = re.compile(r"^scbox2-firmware-v?([0-9][0-9A-Za-z.\-]*)\.ius$")     # = iusAssetRe() with brand scbox2

# ---- --xz: fixed settings so the .img.xz is byte-reproducible. With an explicit --block-size the output does
# not depend on the thread count (xz -T splits the input at block boundaries only); 24 MiB = what xz -6 -T>1
# picks by default (3 x the 8 MiB dictionary), which reproduces the v0.4.4 release's .img.xz. Tested with
# XZ Utils 5.4.5; another xz version may encode differently (the .img itself is unaffected).
XZ_ARGS = ["-6", "--check=crc64", "--block-size=25165824", "-T2"]


def die(msg):
    sys.exit("build_box2: " + msg)


def run(cmd, **kw):
    r = subprocess.run(cmd, capture_output=True, text=True, **kw)
    if r.returncode != 0:
        die("%s:\n%s%s" % (" ".join(map(str, cmd)), r.stdout, r.stderr))
    return r.stdout


def load_tool(name):
    """Import one of the vendored tools (tools/<name>.py) as a module."""
    path = os.path.join(TOOLS, name + ".py")
    if not os.path.exists(path):
        die("vendored tool tools/%s.py is missing" % name)
    spec = importlib.util.spec_from_file_location("box2_" + name, path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def find_squashfs_tool(kind):
    """kind = 'mksquashfs' | 'unsquashfs'. Must be a build that does gzip with -b 131072 (R03)."""
    env = os.environ.get(kind.upper())
    for c in (env, shutil.which(kind), os.path.join("/tmp/sqt/squashfs-tools", kind),
              os.path.join(HERE, "tools", kind)):
        if c and os.path.exists(c) and os.access(c, os.X_OK):
            return c
    die("no %s found (set %s=path; R03 built one at /tmp/sqt/squashfs-tools)" % (kind, kind.upper()))


def set_times(root):
    """Give every file AND directory BUILD_EPOCH so the squashfs is byte-reproducible run-to-run. mksquashfs
    stores inode mtimes for directories too, and directories the build wrote into otherwise carry the wall
    clock, which diverged the image between builds (RV3/RV12). Walk bottom-up so each directory's mtime is set
    after its contents; the superblock time is set separately with -mkfs-time."""
    for d, dns, fns in os.walk(root, topdown=False):
        for n in fns + dns:
            p = os.path.join(d, n)
            if not os.path.islink(p):
                os.utime(p, (BUILD_EPOCH, BUILD_EPOCH))
    os.utime(root, (BUILD_EPOCH, BUILD_EPOCH))


# ================================================================ container / squashfs primitives
def extract_system(ius, firmware_path, work, allow_unknown=False):
    """Return (stock_sqsh_path, system_item_descriptor). Refuses anything but a type-5 squashfs item, and a
    system item that isn't in KNOWN_FIRMWARE unless allow_unknown."""
    blob, base = ius.load_container(firmware_path)
    c = ius.Container(blob)
    sysit = c.find_type(5)
    if sysit is None:
        die("firmware has no system item (type 5)")
    data = c.data(sysit)
    if data[:4] != SQUASHFS_MAGIC:
        die("system item is not a squashfs (magic %r)" % data[:4])
    md5 = hashlib.md5(data).hexdigest()
    print("  firmware: container at %#x, system item %d bytes, md5 %s %s"
          % (base, len(data), md5, KNOWN_FIRMWARE.get(md5, "(not a firmware this was checked against)")))
    if md5 not in KNOWN_FIRMWARE:
        if not allow_unknown:
            die("unknown stock firmware (system item md5 %s): the binary patches and init edits were checked "
                "against %s only. Use the stock SC Box II 2.2.1 firmware.ius, or --allow-unknown (testing only)"
                % (md5, ", ".join("%s = %s" % kv for kv in KNOWN_FIRMWARE.items())))
        print("  WARNING: --allow-unknown: building from a firmware this kit was NOT checked against")
    out =os.path.join(work, "stock.sqsh")
    open(out, "wb").write(data)
    return out, sysit, md5


def roundtrip_sanity(firmware_path, stock_sqsh, work):
    """R01 trap: prove the repack is byte-safe before trusting it. Repack the UNMODIFIED system item and
    assert the whole container comes back byte-for-byte identical to the stock firmware."""
    rt = os.path.join(work, "roundtrip.ius")
    run([sys.executable, "-I", os.path.join(TOOLS, "ius_set_system.py"), firmware_path, stock_sqsh, rt])
    a = open(firmware_path, "rb").read()
    b = open(rt, "rb").read()
    if a != b:
        die("ius repack of the unmodified system item is NOT byte-identical: the container tools are unsafe here")
    os.unlink(rt)
    print("  byte-safe: repacking the unmodified system item reproduces the stock .ius exactly")


def unpack(unsq, stock_sqsh, tree):
    if os.path.exists(tree):
        shutil.rmtree(tree)
    run([unsq, "-n", "-d", tree, stock_sqsh])
    # R03: must not unpack from base/box2/root; we unpack the real system item, so modes/owners are intact.
    if not os.path.isdir(os.path.join(tree, "usr", "bin")):
        die("unsquashfs produced an unexpected tree")


def rebuild(mksq, tree, out_sqsh):
    """R03 recipe: gzip, 128 KiB blocks, no xattrs, fixed mkfs time. Deterministic: -processors 1 pins the
    fragment-packing order (multi-threaded mksquashfs assembles fragment blocks in thread-completion order,
    which diverged two builds byte-for-byte on this host, RV12 follow-up), and -all-time pins every inode
    time to BUILD_EPOCH as a belt-and-suspenders backstop to set_times()."""
    if os.path.exists(out_sqsh):
        os.unlink(out_sqsh)
    run([mksq, tree, out_sqsh, "-comp", "gzip", "-b", "131072", "-no-xattrs", "-noappend",
         "-mkfs-time", str(BUILD_EPOCH), "-all-time", str(BUILD_EPOCH), "-no-tailends",
         "-processors", "1", "-quiet", "-no-progress"],
        # squashfs-tools 4.6+ read SOURCE_DATE_EPOCH themselves and refuse it together with -mkfs-time/-all-time;
        # the explicit options carry the same value (BUILD_EPOCH), so hide the variable from mksquashfs
        env={k: v for k, v in os.environ.items() if k != "SOURCE_DATE_EPOCH"})
    return out_sqsh


def guard_image(path):
    """REFUSE the build unless the rebuilt squashfs is gzip, xattr-free and fits with the floor margin.
    Prints the free space (R03 target: >= ~6.9 MB for B-rec)."""
    d = open(path, "rb").read()
    if d[:4] != SQUASHFS_MAGIC:
        die("rebuilt image is not a squashfs")
    comp, = struct.unpack_from("<H", d, 20)
    flags, = struct.unpack_from("<H", d, 24)
    bytes_used, = struct.unpack_from("<Q", d, 40)
    size = len(d)
    if comp != COMP_GZIP:
        die("rebuilt squashfs compressor is %d, not gzip (%d): the SC Box II kernel needs gzip/lzo/xz, we ship gzip"
            % (comp, COMP_GZIP))
    if not (flags & SQ_FLAG_NO_XATTR):
        die("rebuilt squashfs has xattrs (flags %#x): build with -no-xattrs" % flags)
    if bytes_used > size:
        die("squashfs bytes_used %d > file size %d (corrupt)" % (bytes_used, size))
    if size > SYS_PART:
        die("REFUSE: system image %d bytes > partition %d (items.st system.18432.fs). It would be truncated and "
            "the miner would not boot, even though the burner prints BURN SUCCESS (R01)." % (size, SYS_PART))
    free = SYS_PART - size
    if free < SYS_FLOOR_MARGIN:
        die("REFUSE: only %d bytes free (< %d MiB floor): remove more (R03 groups) before shipping"
            % (free, SYS_FLOOR_MARGIN // (1024 * 1024)))
    print("  system image %d bytes of %d (%.1f%%), %d bytes free (%.1f MiB), gzip, no xattrs"
          % (size, SYS_PART, 100.0 * size / SYS_PART, free, free / (1024.0 * 1024.0)))
    return size, free


# ================================================================ tree helpers
class Tree:
    """A small helper over the unpacked rootfs tree: existence, text read/write with stock-byte asserts,
    file placement, removal, and a record of everything that changed (for the report + read-back)."""

    def __init__(self, root):
        self.root = root
        self.added, self.changed, self.removed = [], [], []

    def _p(self, path):
        return os.path.join(self.root, path.lstrip("/"))

    def exists(self, path):
        p = self._p(path)
        return os.path.exists(p) or os.path.islink(p)

    def read(self, path):
        p = self._p(path)
        if not os.path.exists(p):
            die("%s is missing from the stock tree" % path)
        return open(p, "rb").read()

    def text(self, path):
        return self.read(path).decode("latin-1")

    def put(self, path, data, mode=0o644, uid=0, gid=0):
        p = self._p(path)
        existed = os.path.exists(p) or os.path.islink(p)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        if os.path.islink(p):
            os.unlink(p)
        with open(p, "wb") as f:
            f.write(data)
        os.chmod(p, mode)
        os.chown(p, uid, gid)
        os.utime(p, (BUILD_EPOCH, BUILD_EPOCH))
        (self.changed if existed else self.added).append(path)

    def put_file(self, src, path, mode=0o755):
        if not os.path.exists(src):
            print("  TODO: source %s missing; %s not placed (a later lane provides it)" % (src, path))
            return False
        self.put(path, open(src, "rb").read(), mode)
        return True

    def mkdir(self, path, mode=0o755):
        p = self._p(path)
        if not os.path.isdir(p):
            os.makedirs(p, exist_ok=True)
            os.chmod(p, mode)
            os.chown(p, 0, 0)

    def move(self, src, dst):
        s, d = self._p(src), self._p(dst)
        os.makedirs(os.path.dirname(d), exist_ok=True)
        shutil.move(s, d)

    def symlink(self, target, path):
        p = self._p(path)
        if os.path.exists(p) or os.path.islink(p):
            os.unlink(p)
        os.makedirs(os.path.dirname(p), exist_ok=True)
        os.symlink(target, p)
        self.added.append(path)

    def remove(self, path):
        p = self._p(path)
        if os.path.islink(p) or os.path.isfile(p):
            os.unlink(p)
            self.removed.append(path)
        elif os.path.isdir(p):
            shutil.rmtree(p)
            self.removed.append(path + "/")

    def chmod(self, path, mode):
        os.chmod(self._p(path), mode)


def replace_once(text, old, new, what):
    if text.count(old) != 1:
        die("%s: expected exactly one %r, found %d" % (what, old, text.count(old)))
    return text.replace(old, new)


def sh_ok(data):
    return subprocess.run(["sh", "-n"], input=data if isinstance(data, bytes) else data.encode()).returncode == 0


# ================================================================ step 4a: file placement (WHERE files land)
# All paths and the layout follow R03's recommended B-rec layout + the SC Lite kit's file set. These are the
# hook points B8 (init/security files) and the panel build feed; a missing source file is a TODO, not a stop,
# so the pipeline runs end-to-end before every lane has landed.
# The SSH server (scbox2-sshd, tools/build-sshd-box2.sh): a program of its own, so SSH survives a damaged panel.
# Its size budget (stripped ARM): ~4.8 MiB today (x/crypto/ssh, sftp, pty, the Go runtime); more than this means
# it linked something it shouldn't (the panel's code, net/http, ...).
SSHD_MAX_BYTES = 6 * 1024 * 1024


def place_panel_and_helpers(t, agent_path, sshd_path):
    print("step 4a: place panel + SSH server + kit helpers")
    agent = open(agent_path, "rb").read()
    if agent[:4] != b"\x7fELF" or agent[18] != 40:            # EM_ARM
        die("--agent is not an ARM ELF binary")
    sshd = open(sshd_path, "rb").read()
    if sshd[:4] != b"\x7fELF" or sshd[18] != 40:
        die("--sshd is not an ARM ELF binary")
    if len(sshd) > SSHD_MAX_BYTES:
        die("--sshd is %d bytes, over its %d-byte budget: it must link only internal/sshcore + internal/authfile, "
            "never the panel (tools/build-sshd-box2.sh checks that)" % (len(sshd), SSHD_MAX_BYTES))
    if sshd.find(b"SSH-2.0-scbox2") < 0:
        die("--sshd is not scbox2-sshd (no SSH-2.0-scbox2 version string)")
    # the panel (R03: /usr/bin fits the budget). 11 MB; with B-rec removals it leaves ~6.9 MB free (less the SSH
    # server below; guard_image keeps the hard floor).
    t.put("/usr/bin/scbox2-agent", agent, 0o755)
    print("  /usr/bin/scbox2-agent %d bytes, /usr/bin/scbox2-sshd %d bytes (budget %d)"
          % (len(agent), len(sshd), SSHD_MAX_BYTES))

    # the boot script (B8 owns its contents: files/S48scbox2)
    t.put_file(os.path.join(FILES, "S48scbox2"), "/etc/init.d/S48scbox2", 0o755)

    # SSH: its own server and boot script, started before the panel and needing nothing from it (the stock OpenSSH
    # is removed, G5_openssh)
    t.put("/usr/bin/scbox2-sshd", sshd, 0o755)
    if not t.put_file(os.path.join(FILES, "S47scbox2-sshd"), "/etc/init.d/S47scbox2-sshd", 0o755):
        die("files/S47scbox2-sshd is missing: the image would have no SSH")
    for p in ("/etc/init.d/S47scbox2-sshd", "/etc/init.d/S48scbox2"):
        if not sh_ok(t.read(p)):
            die("%s: shell syntax error" % p)

    # loopback-bind shim for minerd:80 / intminer:4028 (files/scbox2_bind.c; DESIGN)
    t.put_file(os.path.join(FILES, "scbox2_bind.so.bin"), "/lib/scbox2_bind.so", 0o755)

    # steady MAC, one NTP loop, sysctl, dhcpcd NTP hook, stub web page (so the old UI assets can go)
    t.put_file(os.path.join(FILES, "scbox2-mac"), "/etc/scbox2-mac", 0o755)
    t.put_file(os.path.join(FILES, "scbox2-ntp"), "/usr/libexec/scbox2/ntp", 0o755)
    t.put_file(os.path.join(FILES, "scbox2-sysctl"), "/usr/libexec/scbox2/sysctl", 0o755)
    # this unit's own random seed on the config partition (S38initdir loads it, S20urandom/S48 save it)
    t.put_file(os.path.join(FILES, "scbox2-seed"), "/usr/libexec/scbox2/seed", 0o755)
    t.put_file(os.path.join(FILES, "40-scbox2-ntp"), "/libexec/dhcpcd-hooks/40-scbox2-ntp", 0o444)
    t.put_file(os.path.join(FILES, "index-box2.html"), "/root/yottamc/index.html", 0o644)

    # model files (A1's model profile loads stock /root/product.json + /root/mcbversion, already in the tree).
    # Any extra kit model file would go here:
    mdir = os.path.join(FILES, "model")
    if os.path.isdir(mdir):
        for n in sorted(os.listdir(mdir)):
            t.put_file(os.path.join(mdir, n), "/root/scbox2-model/" + n, 0o644)
    else:
        print("  (no files/model/*: the profile comes from stock /root/product.json + /root/mcbversion)")

    # /etc/localtime becomes settable from the panel (keep the stock copy as .stock); config lives off the
    # read-only squashfs, so the live file is a symlink to the config partition.
    if t.exists("/etc/localtime") and not t.exists("/etc/localtime.stock"):
        shutil.copy2(t._p("/etc/localtime"), t._p("/etc/localtime.stock"))
        t.added.append("/etc/localtime.stock")

    # Config redirect (RV5/RV7 F1): on the Box II /usr/config is a read-only squashfs dir (only a readme); the
    # one writable store is the FAT at /mnt/config, mounted by S38initdir before S48 starts the panel. Replace
    # /usr/config with a symlink to /mnt/config so the panel's compiled-in /usr/config/scbox2 paths — and the
    # kit scripts that use them (scbox2-mac) — resolve to the FAT, exactly as the SC Lite relies on /usr/config
    # being its own writable mount. Without this every settings write (password, fan curve, SSH host key, TLS
    # cert, ...) hits the read-only root, the first-boot password change returns 500, and nothing persists.
    t.remove("/usr/config")
    t.symlink("/mnt/config", "/usr/config")


def place_intminer(t, patchers):
    """The intminer wrapper + the guarded/hooked copy (mirrors SC Lite build.py step 5/5b). The stock miner
    stays pristine at /usr/libexec/scbox2/intminer; the wrapper (files/scbox2_wrap.bin, 'scbox2 shim') runs
    either it or the hooked copy. Chip trims ship OFF (DESIGN): the hook is built but dormant."""
    print("step 4a': intminer wrapper + guarded/hooked copy")
    if not t.exists("/usr/bin/intminer"):
        die("/usr/bin/intminer missing from the stock tree")
    stock = t.read("/usr/bin/intminer")

    t.mkdir("/usr/libexec", 0o755)
    t.mkdir("/usr/libexec/scbox2", 0o755)
    t.move("/usr/bin/intminer", "/usr/libexec/scbox2/intminer")      # pristine stock miner
    t.changed.append("/usr/libexec/scbox2/intminer")

    wrap_src = os.path.join(FILES, "scbox2_wrap.bin")
    if os.path.exists(wrap_src):
        wrap = open(wrap_src, "rb").read()
        if b"scbox2 shim" not in wrap:
            die("files/scbox2_wrap.bin lacks the 'scbox2 shim' marker the agent checks for")
        if b"wraplog" in wrap or b"minerout" in wrap:
            die("files/scbox2_wrap.bin is a debug build (launch logging): rebuild with tools/build-helpers-box2.sh")
        t.put("/usr/bin/intminer", wrap, 0o755)
        # xminerd launches the SC miner by the NAME "bfgminer" and execs /usr/bin/bfgminer, which the stock
        # tree does not contain -> the boot launch fails silently and the /usr/bin/intminer wrapper is never
        # reached (confirmed on hardware: no wrapper log at boot, no miner process). Install the same wrapper
        # at /usr/bin/bfgminer so xminerd's launch reaches it. The wrapper preserves argv[0] ("bfgminer"),
        # rewrites xminerd's -c to --config, and execs the guarded/hooked miner -- exactly as /usr/bin/intminer
        # does when invoked by hand. (bfgminer-rpc is minerd's API client, left as stock; not the launcher.)
        t.put("/usr/bin/bfgminer", wrap, 0o755)
        t.changed.append("/usr/bin/bfgminer")
    else:
        print("  TODO: files/scbox2_wrap.bin missing; leaving stock miner at /usr/bin/intminer (no wrapper)")
        t.put("/usr/bin/intminer", stock, 0o755)

    # guarded + chip-hook copy: patch_divzero_box2 . patch_poolinput_box2 . patch_intminer_box2 (all A3)
    hooked = stock
    # patch_intminer_box2.patch() consolidates chip-hook + divzero + pool-guards (A3), so one key.
    for key in ("intminer_hook",):
        fn = patchers.get(key)
        if fn:
            hooked = fn(hooked)
    if hooked is stock:
        print("  TODO: no A3 intminer patch module present; hook copy is the UNPATCHED stock miner")
    t.mkdir("/usr/libexec/scbox2/hook", 0o755)
    t.put("/usr/libexec/scbox2/hook/intminer", hooked, 0o755)
    t.put_file(os.path.join(FILES, "scbox2_chip.so.bin"), "/lib/scbox2_chip.so", 0o755)

    # Deterministic miner launcher (inittab runs it; see edit_box2_init.edit_inittab). xminerd's own launch
    # does not reach the miner on this firmware, so this starts /usr/bin/bfgminer -c <cfg> once a pool exists
    # and keeps it running, cooperating with fanctrl/minerd. Required for boot-to-hashing with no SSH.
    run_src = os.path.join(FILES, "miner_run.sh")
    if os.path.exists(run_src):
        t.put_file(run_src, "/usr/libexec/scbox2/miner-run", 0o755)
        t.changed.append("/usr/libexec/scbox2/miner-run")
    else:
        print("  TODO: files/miner_run.sh missing; miner will not auto-start (xminerd launch is broken)")


def write_release(t, version, agent_path, sys_md5):
    """/etc/scbox2-release: the panel shows its "version:" line (Settings -> About). The "built:" time is
    BUILD_EPOCH (SOURCE_DATE_EPOCH when set, else the pinned default), so the same inputs give the same bytes."""
    agent_sha = hashlib.sha256(open(agent_path, "rb").read()).hexdigest()
    rel = ("scbox2-agent firmware build\nversion: %s\nbase: OEM %s firmware (system item md5 %s)\nagent sha256: %s\n"
           "built: %s\n" % (version, KNOWN_FIRMWARE.get(sys_md5, "unknown").split(" (")[0], sys_md5, agent_sha,
                            time.strftime("%Y-%m-%dT%H:%MZ", time.gmtime(BUILD_EPOCH))))
    t.put("/etc/scbox2-release", rel.encode(), 0o644)
    print("  /etc/scbox2-release: version %s, built %s (%s)"
          % (version, time.strftime("%Y-%m-%dT%H:%MZ", time.gmtime(BUILD_EPOCH)), BUILD_EPOCH_SOURCE))


# ================================================================ step 4b: binary patches (A3 hook points)
# A3 delivers kit modules patch_<name>_box2.py, each exposing `patch(data: bytes) -> bytes` (and, for the
# ones tied to a specific binary, a STOCK_MD5 string this pipeline checks first). This loader imports them by
# name from --patches-dir (default: the kit dir). A missing module is a clearly-marked TODO, never a stop.
PATCH_MODULES = {
    # key              module name              target path in the tree       md5 attr (optional)
    "minerd":         ("patch_minerd_box2",     "/usr/bin/minerd",            "MINERD_MD5"),
    "xminerd":        ("patch_xminerd_box2",    "/usr/bin/xminerd",           "STOCK_MD5"),
    "wifi":           ("patch_wifi_box2",       "/usr/bin/wifi",              "WIFI_MD5"),
    # intminer copies are composed in place_intminer(); these only need a .patch:
    "intminer_hook":  ("patch_intminer_box2",   None,                         None),  # hook+divzero+poolguards
}


def load_patchers(patches_dir):
    """Return {key: patch_fn} for every A3 module that is present. Prints a TODO for each absent one."""
    out = {}
    for key, (modname, _tgt, _md5) in PATCH_MODULES.items():
        path = os.path.join(patches_dir, modname + ".py")
        if not os.path.exists(path):
            print("  TODO: A3 module %s.py not found (%s left unpatched)" % (modname, key))
            continue
        spec = importlib.util.spec_from_file_location("box2patch_" + key, path)
        mod = importlib.util.module_from_spec(spec)
        try:
            spec.loader.exec_module(mod)
        except Exception as e:
            print("  TODO: %s.py failed to import (%s); %s left unpatched" % (modname, e, key))
            continue
        fn = getattr(mod, "patch", None)
        if not callable(fn):
            print("  TODO: %s.py has no patch(data)->data; %s left unpatched" % (modname, key))
            continue
        out[key] = fn
        out[key + "__md5"] = getattr(mod, PATCH_MODULES[key][2], None) if PATCH_MODULES[key][2] else None
        print("  loaded A3 patcher %s (%s)" % (modname, key))
    return out


def apply_binary_patches(t, patchers):
    """Apply the simple single-binary patches (minerd, xminerd, wifi) in place. The intminer copies are
    done in place_intminer(). Each binary's md5 is checked first when the module advertises one."""
    print("step 4b: binary patches (A3)")
    for key in ("minerd", "xminerd", "wifi"):
        fn = patchers.get(key)
        tgt = PATCH_MODULES[key][1]
        if not fn:
            continue
        if not t.exists(tgt):
            print("  TODO: %s absent; %s patch skipped" % (tgt, key))
            continue
        data = t.read(tgt)
        want = patchers.get(key + "__md5")
        got = hashlib.md5(data).hexdigest()
        if want and got != want:
            die("%s md5 %s != expected %s for %s: not the firmware the patch was built for" % (tgt, got, want, key))
        t.put(tgt, fn(data), 0o755)
        print("  patched %s" % tgt)


# ================================================================ step 4c: init/host edits (B8 policy)
# These mutate stock init scripts. The SC Lite build does the same edits inline (build.py), so the pipeline
# is their natural home; B8 owns the policy and may refine them. Each asserts the stock text it expects and
# is skipped with a warning (not a stop) if B8 has already restructured the file, so the pipeline keeps
# running. The host-leak grep (--check) is the gate that proves the result is clean.
def edit_init_scripts(t, netcheck_name="scbox2"):
    print("step 4c: init/host edits + security (B8: edit_box2_init.apply)")
    import edit_box2_init
    bind = os.path.join(FILES, "scbox2_bind.so.bin")
    # root_hash: an MD5 ($1$) recovery hash. uClibc 0.9.33.2 on this miner reliably supports DES+MD5 crypt but
    # may lack $6$ SHA-512 (stock root is DES), so /bin/login would reject a $6$. SD reflash is the primary
    # recovery; this serial-console password is documented in the hardware notes.
    edit_box2_init.apply(t, bind_path=bind, root_hash=edit_box2_init.RECOVERY_ROOT_HASH,  # $1$scbox2re$Q1YId9c5RLjHvWSUOalzX1
                         netcheck_name=netcheck_name)
    for p in ("/etc/init.d/S20urandom", "/etc/init.d/S38initdir", "/etc/init.d/S40network", "/usr/libexec/scbox2/seed"):
        if not sh_ok(t.read(p)):
            die("%s: shell syntax error after the edits" % p)
# ================================================================ step 4d: removals (R03 B-rec; owned here)
# R03 layout B-rec + DESIGN: drop the kernel copy, the other-coin miners, the cloud/MQTT tools, the stock web
# UI (keep the stub index.html), avahi and OpenSSH. G7 unreachable libs stay off by default (optional in R03).
REMOVE_GROUPS = {
    "G1_boot_uImage": ["/boot/uImage"],
    # (R03 groups.txt omitted xvgminer-rpc, the companion RPC helper of the removed xvgminer: added here)
    "G2_other_coin_miners": ["/usr/bin/xvgminer", "/usr/bin/xvgminer-rpc", "/usr/bin/kdaminer", "/usr/bin/kdaminer-rpc"],
    "G3_cloud_mqtt": ["/usr/bin/cloudbox", "/usr/bin/MQTTVersion",
                      "/usr/lib/libpaho-mqtt3a.so", "/usr/lib/libpaho-mqtt3a.so.1", "/usr/lib/libpaho-mqtt3a.so.1.3.9",
                      "/usr/lib/libpaho-mqtt3c.so", "/usr/lib/libpaho-mqtt3c.so.1", "/usr/lib/libpaho-mqtt3c.so.1.3.9",
                      "/usr/lib/libpaho-mqtt3as.so", "/usr/lib/libpaho-mqtt3as.so.1", "/usr/lib/libpaho-mqtt3as.so.1.3.9"],
    # G4 stock web UI assets: keep the stub index.html (placed in step 4a); everything else under yottamc goes
    "G4_stock_webui": ["__yottamc_except_index__"],
    "G6_avahi": ["/etc/init.d/S05avahi-setup.sh", "/etc/init.d/S50avahi-daemon", "/etc/avahi",
                 "/usr/sbin/avahi-autoipd", "/usr/sbin/avahi-daemon", "/usr/sbin/avahi-dnsconfd",
                 "/usr/share/avahi", "/usr/lib/avahi", "/var/lib/avahi-autoipd",
                 "/usr/lib/libavahi-core.so", "/usr/lib/libavahi-core.so.7", "/usr/lib/libavahi-core.so.7.0.2",
                 "/usr/lib/libavahi-common.so", "/usr/lib/libavahi-common.so.3", "/usr/lib/libavahi-common.so.3.5.3"],
    # (/usr/bin/slogin is stock OpenSSH's "slogin -> ./ssh" symlink: it would dangle once ssh is gone)
    "G5_openssh": ["/usr/sbin/sshd", "/usr/bin/ssh", "/usr/bin/slogin", "/usr/bin/ssh-keygen",
                   "/usr/bin/ssh-keyscan",
                   "/usr/bin/ssh-agent", "/usr/bin/ssh-add", "/usr/libexec/ssh-pkcs11-helper",
                   "/usr/libexec/ssh-keysign", "/etc/init.d/S50sshd", "/etc/sshd_config", "/etc/ssh_config",
                   "/etc/ssh_host_dsa_key", "/etc/ssh_host_dsa_key.pub", "/etc/ssh_host_ecdsa_key",
                   "/etc/ssh_host_ecdsa_key.pub", "/etc/ssh_host_rsa_key", "/etc/ssh_host_rsa_key.pub",
                   "/etc/ssh_host_key", "/etc/ssh_host_key.pub", "/etc/moduli"],
    # Residual OpenSSH/telnet CLIENTS (RV3/RV6): not listeners (scp/sftp exec the now-removed ssh; telnet is a
    # plain client) so no open-port risk, but dead weight and part of "stock OpenSSH" the design drops.
    "G8_extra_clients": ["/usr/bin/scp", "/usr/bin/sftp", "/usr/bin/telnet"],
    # Goldshell's development leftovers (security audit L10): an NFS mount of a factory server (donfs), a
    # screen-based miner starter, the config of a web server that isn't in the image, a factory Litecoin
    # config with someone else's pools. Nothing kept runs or reads them. /etc/random-seed: the same file on
    # every unit; each miner keeps its own seed on the config partition instead (L12, files/scbox2-seed).
    # Kept on purpose: /root/settings/key.pem and login.json (minerd's own login/JWT on 127.0.0.1:80).
    "G9_goldshell_leftovers": ["/usr/bin/donfs", "/usr/bin/start-bfgminer.sh", "/etc/lighttpd",
                               "/root/settings/qomo.json", "/etc/random-seed"],
}
# must never be removed (R03): executables that are kept link all of these
NEVER_REMOVE = ("libpaho-mqtt3cs", "libeventhub", "libwifi", "libgnutls", "libnettle", "libhogweed",
                "libgmp", "libmicrohttpd", "libulfius", "libcurl", "libssl", "libcrypto", "libiconv")


def apply_removals(t, extra_groups=()):
    print("step 4d: removals (R03 B-rec + B8 Wi-Fi/OpenSSH)")
    import removals_box2
    removals_box2.merge_into(REMOVE_GROUPS)
    for gname, paths in REMOVE_GROUPS.items():
        if gname.startswith("G5") or gname.startswith("G6"):
            pass  # avahi + openssh are in the DESIGN removal list; included by default
        for p in paths:
            if p == "__yottamc_except_index__":
                base = "/root/yottamc"
                if t.exists(base):
                    for name in sorted(os.listdir(t._p(base))):
                        if name != "index.html":
                            t.remove(base + "/" + name)
                continue
            if any(n in p for n in NEVER_REMOVE):
                die("refusing to remove %s: a kept executable links it (R03)" % p)
            t.remove(p)
    removed = len([r for r in t.removed])
    print("  removed %d paths (%s)" % (removed, ", ".join(REMOVE_GROUPS.keys())))


# ================================================================ step 8: read-back gate
def snapshot(root):
    """{path: (kind, mode, uid, gid, detail)} for every entry under root (the root itself is "/"): detail is the
    sha256 of a regular file, the target of a symlink, the device number of a device node, else None."""
    out = {}

    def one(full, rel):
        st = os.lstat(full)
        m = st.st_mode
        if stat.S_ISLNK(m):
            kind, detail = "link", os.readlink(full)
        elif stat.S_ISREG(m):
            h = hashlib.sha256()
            with open(full, "rb") as f:
                for chunk in iter(lambda: f.read(1 << 20), b""):
                    h.update(chunk)
            kind, detail = "file", h.hexdigest()
        elif stat.S_ISDIR(m):
            kind, detail = "dir", None
        elif stat.S_ISCHR(m) or stat.S_ISBLK(m):
            kind, detail = ("chr" if stat.S_ISCHR(m) else "blk"), (os.major(st.st_rdev), os.minor(st.st_rdev))
        else:
            kind, detail = "other:%o" % stat.S_IFMT(m), None
        out[rel] = (kind, stat.S_IMODE(m), st.st_uid, st.st_gid, detail)

    one(root, "/")
    for d, dns, fns in os.walk(root):
        for n in dns + fns:
            full = os.path.join(d, n)
            one(full, "/" + os.path.relpath(full, root))
    return out


def readback_gate(ius, unsq, out_ius, out_img, new_sqsh, tree, work):
    """Prove the image holds exactly the tree that was packed: (a) the final .ius's system item is the squashfs
    mksquashfs wrote, (b) unsquashfs of that item matches the packed tree entry by entry (type, sha256, mode,
    owner, symlink target, device numbers; nothing missing, nothing extra), (c) the SD .img carries the .ius
    byte for byte at the card offset. Returns the read-back tree (the startup check runs the panel from it)."""
    print("step 8: read-back gate (final .ius system item vs the packed tree)")
    ius_bytes = open(out_ius, "rb").read()
    c = ius.Container(ius_bytes)
    sysit = c.find_type(5)
    if sysit is None:
        die("read-back: the built .ius has no system item")
    item = c.data(sysit)
    if item != open(new_sqsh, "rb").read():
        die("read-back: the built .ius's system item is not the squashfs that was built")
    rb = os.path.join(work, "readback")
    if os.path.exists(rb):
        shutil.rmtree(rb)
    sq = os.path.join(work, "readback.sqsh")
    open(sq, "wb").write(item)
    run([unsq, "-n", "-d", rb, sq])
    os.unlink(sq)
    want, got = snapshot(tree), snapshot(rb)
    diffs = []
    for p in sorted(set(want) | set(got)):
        a, b = want.get(p), got.get(p)
        if a == b:
            continue
        if a is None:
            diffs.append("%s: in the image but not in the packed tree %r" % (p, b))
        elif b is None:
            diffs.append("%s: in the packed tree but missing from the image" % p)
        else:
            what = [n for n, x, y in zip(("type", "mode", "uid", "gid", "content/target"), a, b) if x != y]
            diffs.append("%s: %s differ (tree %r, image %r)" % (p, "/".join(what), a, b))
    if diffs:
        die("read-back FAILED: %d difference(s) between the packed tree and the final .ius:\n  %s"
            % (len(diffs), "\n  ".join(diffs[:40]) + ("\n  ..." if len(diffs) > 40 else "")))
    files = sum(1 for v in want.values() if v[0] == "file")
    print("  read-back OK: %d entries (%d files) identical in sha256, mode, owner and link target" % (len(want), files))
    if out_img:
        img = open(out_img, "rb").read()
        off = ius.CARD_OFFSET
        if img[off:off + len(ius_bytes)] != ius_bytes or img[:off].count(0) != off:
            die("read-back FAILED: the SD .img does not carry the .ius byte for byte at %#x" % off)
        print("  read-back OK: the SD .img carries the .ius byte for byte at %#x" % off)
    return rb


# ================================================================ step 11: panel startup check
STARTUP_RUNS = 5


def find_qemu():
    """A static qemu-arm (it runs inside the image's chroot, which has no host libraries)."""
    for c in (os.environ.get("QEMU_ARM"), shutil.which("qemu-arm-static"), shutil.which("qemu-arm"),
              os.path.join(TOOLS, "qemu-arm-static")):
        if c and os.path.exists(c) and os.access(c, os.X_OK) and b"ld-linux" not in open(c, "rb").read(4096):
            return c
    return None


def startup_check(root, skip, require, runs=STARTUP_RUNS):
    """Start the SSH server and the panel FROM the image (root = the read-back tree of the final .ius, so its own
    /usr/bin/scbox2-sshd and /usr/bin/scbox2-agent) under qemu-arm, `runs` times. Each run: unshare -m -n -p -f
    (own mount, network and PID namespaces: nothing it binds is reachable from or on the build host, and it ends
    with the run), chroot in the image tree, tmpfs on /tmp, /mnt (the /usr/config -> /mnt/config target) and /dev
    (null/zero/random/urandom only), settings under a temp dir. Each run: scbox2-sshd must answer on port 22 with
    its SSH banner and key exchange; the panel (-ssh=false -lan80=false -https443=false, a free high port) must
    serve its page, stay up 2 s more, and open nothing on 22/80/443 (beyond the SSH server's and the by-design
    127.0.0.2:80 net-check responder, STARTUP_ALLOWED_LISTEN; all of it inside the run's namespace); then, with
    the panel binary replaced by a crashing stub, a fresh scbox2-sshd must still answer on 22.
    Returns True if it ran and passed, False if skipped (dies on failure)."""
    if skip:
        if require:
            die("--skip-startup-check and --require-startup-check together")
        print("  WARNING: STARTUP CHECK SKIPPED (--skip-startup-check): the panel in this image was NOT started")
        return False
    qemu = find_qemu()
    missing = [n for n, ok in (("a static qemu-arm (qemu-user-static, or QEMU_ARM=path)", qemu),
                               ("unshare", shutil.which("unshare")), ("chroot", shutil.which("chroot")),
                               ("root", os.geteuid() == 0)) if not ok]
    if missing:
        msg = "the startup check needs " + ", ".join(missing)
        if require:
            die(msg + " (--require-startup-check)")
        print("  " + "!" * 100)
        print("  WARNING: STARTUP CHECK SKIPPED: %s.\n  The panel in this image was NOT started. Do not release "
              "this build (releases use --require-startup-check)." % msg)
        print("  " + "!" * 100)
        return False
    if not os.path.exists(os.path.join(root, "usr", "bin", "scbox2-agent")):
        die("startup check: the image has no /usr/bin/scbox2-agent")
    if not os.path.exists(os.path.join(root, "usr", "bin", "scbox2-sshd")):
        die("startup check: the image has no /usr/bin/scbox2-sshd")
    shutil.copy(qemu, os.path.join(root, ".scbox2-qemu-arm"))
    env = {k: v for k, v in os.environ.items() if k not in ("TZ", "http_proxy", "https_proxy", "HTTP_PROXY",
                                                            "HTTPS_PROXY", "no_proxy", "NO_PROXY")}
    for i in range(runs):
        # -f: the run's helper is PID 1 of the new PID namespace, so the panel (and anything it starts) dies with it
        try:
            r = subprocess.run(["unshare", "-m", "-n", "-p", "-f", "--propagation", "private",
                                sys.executable, "-I", os.path.abspath(__file__), "--startup-run", root],
                               env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=180)
            out, rc = r.stdout.decode("latin-1", "replace"), r.returncode
        except subprocess.TimeoutExpired as e:
            out, rc = (e.stdout or b"").decode("latin-1", "replace") + "\n(timed out)", -1
        if rc != 0 or "fatal error" in out or "panic:" in out:
            die("startup check FAILED: the SSH server or the panel from the image did not start (run %d of %d):\n%s"
                % (i + 1, runs, out[-3000:]))
        print("  run %d/%d: %s" % (i + 1, runs, out.strip().splitlines()[-1] if out.strip() else "ok"))
    os.unlink(os.path.join(root, ".scbox2-qemu-arm"))
    print("  the SSH server and the panel from the image started %d of %d times under qemu-arm (isolated): port 22 "
          "answered SSH (also with the panel replaced by a crashing stub) and the panel served its page" % (runs, runs))
    return True


def _listening():
    """{(ip, port)} of the TCP sockets listening in this network namespace (IPv4 as dotted quad)."""
    out = set()
    for f in ("/proc/net/tcp", "/proc/net/tcp6"):
        try:
            lines = open(f).read().splitlines()[1:]
        except OSError:
            continue
        for line in lines:
            parts = line.split()
            if parts[3] != "0A":                                           # TCP_LISTEN
                continue
            hexip, hexport = parts[1].rsplit(":", 1)
            ip = ".".join(str(b) for b in bytes.fromhex(hexip)[::-1]) if len(hexip) == 8 else hexip
            out.add((ip, int(hexport, 16)))
    return out


# The one :80 socket the panel always opens, by design and with no flag: xminerd's net-check responder on
# 127.0.0.2:80 (netlisten.go netCheckAddr; loopback only, accept-and-close). Anything else on 22/80/443 fails.
STARTUP_ALLOWED_LISTEN = {("127.0.0.2", 80)}


def ssh_answers(port=22, timeout=60):
    """Does 127.0.0.1:port speak SSH as scbox2-sshd: its version line (SSH-2.0-scbox2), then, after ours, its key
    exchange (an SSH_MSG_KEXINIT packet)? Retried until timeout (the host key is made at first start). Returns
    (ok, what was seen)."""
    import socket
    seen, end = "nothing", time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=5) as c:
                c.settimeout(10)
                f = c.makefile("rb")
                line = f.readline(256).strip()
                if line != b"SSH-2.0-scbox2":
                    seen = "version line %r, not SSH-2.0-scbox2" % line
                else:
                    c.sendall(b"SSH-2.0-box2check\r\n")
                    hdr = f.read(6)                                 # uint32 length, padding length, message type
                    if len(hdr) == 6 and hdr[5] == 20:              # SSH_MSG_KEXINIT
                        return True, "banner %s, key exchange offered" % line.decode()
                    seen = "banner %s, then %r instead of a key exchange" % (line.decode(), hdr)
        except OSError as e:
            seen = "port %d: %s" % (port, e)
        time.sleep(0.5)
    return False, seen


def start_sshd(root, cfg):
    """scbox2-sshd from the image, in the chroot (its settings next to the panel's under cfg: SSH on, port 22)."""
    return subprocess.Popen(["chroot", root, "/.scbox2-qemu-arm", "/usr/bin/scbox2-sshd",
                             "-authfile", cfg + "/auth.json", "-netcfg", cfg + "/net.json"],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)


def startup_run(root):
    """One startup-check run, inside unshare -m -n -p -f (see startup_check). Exit 0 = the SSH server answered on
    22, the panel served the page, was still running 2 s later and opened nothing more on 22/80/443, and port 22
    still answered from a fresh SSH server with the panel binary replaced by a crashing stub."""
    import fcntl, socket, urllib.request
    s = socket.socket()
    flags = struct.unpack("16sH", fcntl.ioctl(s, 0x8913, struct.pack("16sH", b"lo", 0))[:18])[1]   # SIOCGIFFLAGS
    fcntl.ioctl(s, 0x8914, struct.pack("16sH", b"lo", flags | 1))                                  # IFF_UP
    s.close()
    # private mounts only (this mount namespace): the image's own files, fresh tmpfs for everything it may write
    for d in ("proc", "dev", "tmp", "mnt"):
        os.makedirs(os.path.join(root, d), exist_ok=True)
    subprocess.run(["mount", "-t", "proc", "proc", os.path.join(root, "proc")], check=True)
    for d in ("dev", "tmp", "mnt"):
        subprocess.run(["mount", "-t", "tmpfs", "-o", "mode=1777", "box2check", os.path.join(root, d)], check=True)
    for n in ("null", "zero", "random", "urandom"):
        dst = os.path.join(root, "dev", n)
        open(dst, "w").close()
        subprocess.run(["mount", "--bind", "/dev/" + n, dst], check=True)
    os.makedirs(os.path.join(root, "mnt", "config"))
    cfg = "/tmp/box2check-cfg"                                 # inside the chroot: on the tmpfs
    os.makedirs(os.path.join(root, cfg.lstrip("/"), "tuner"))
    os.makedirs(os.path.join(root, cfg.lstrip("/"), "backup"))
    with socket.socket() as ps:                                # a free high port (in this network namespace)
        ps.bind(("127.0.0.1", 0))
        port = ps.getsockname()[1]
    # 1. the SSH server first (as S47scbox2-sshd before S48scbox2): port 22 must speak SSH
    sshd = start_sshd(root, cfg)
    ssh_ok, ssh_seen = ssh_answers()
    sshd_listen = {ip_port for ip_port in _listening() if ip_port[1] == 22}
    if not ssh_ok or sshd.poll() is not None:
        sshd.kill()
        sys.stdout.write(sshd.communicate()[0].decode("latin-1", "replace"))
        print("\nFAILED: scbox2-sshd from the image didn't answer SSH on port 22: %s" % ssh_seen)
        sys.exit(1)
    # 2. the panel
    args = ["chroot", root, "/.scbox2-qemu-arm", "/usr/bin/scbox2-agent",
            "-listen", "127.0.0.1:%d" % port, "-ssh=false", "-lan80=false", "-https443=false",
            "-log", "/dev/null", "-authfile", cfg + "/auth.json", "-fan", cfg + "/fan.json",
            "-presets", cfg + "/presets.json", "-hashrate", cfg + "/hashrate.json", "-netcfg", cfg + "/net.json",
            "-tunerdir", cfg + "/tuner", "-bfg", cfg + "/bfgminer.json", "-backup", cfg + "/backup",
            "-product", "/root/product.json", "-mcbversion", "/root/mcbversion"]
    p = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    ok, deadline, t0 = False, time.time() + 90, time.time()
    while time.time() < deadline and p.poll() is None:
        try:
            body = urllib.request.build_opener(urllib.request.ProxyHandler({})).open(
                "http://127.0.0.1:%d/" % port, timeout=3).read()
            ok = b"<html" in body.lower() or b"<!doctype" in body.lower()
            break
        except Exception:
            time.sleep(0.5)
    took = time.time() - t0
    time.sleep(2 if ok else 0)              # a crash in a goroutine started after the first request
    alive = p.poll() is None
    bad = sorted("%s:%d" % ip_port for ip_port in _listening()
                 if ip_port[1] in (22, 80, 443) and ip_port not in STARTUP_ALLOWED_LISTEN and ip_port not in sshd_listen)
    p.kill()
    sys.stdout.write(p.communicate()[0].decode("latin-1", "replace"))
    if bad:
        print("\nthe panel listened on %s despite -ssh=false -lan80=false -https443=false" % ", ".join(bad))
    ssh_with_panel, _ = ssh_answers(timeout=10)
    # 3. the panel binary damaged (a crashing stub in its place) and the SSH server started afresh, as at the next
    # boot: SSH must not need the panel
    sshd.kill()
    sys.stdout.write(sshd.communicate()[0].decode("latin-1", "replace"))
    stub = os.path.join(root, "tmp", "crashing-panel")
    open(stub, "wb").write(b"\x7fELF\x01\x01\x01" + b"\0" * 9 + b"damaged panel binary")
    os.chmod(stub, 0o755)
    subprocess.run(["mount", "--bind", stub, os.path.join(root, "usr", "bin", "scbox2-agent")], check=True)
    crash = subprocess.run(["chroot", root, "/.scbox2-qemu-arm", "/usr/bin/scbox2-agent"],
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
    for i in range(30):                                        # the old server's port is free again
        if not any(ip_port[1] == 22 for ip_port in _listening()):
            break
        time.sleep(0.2)
    sshd2 = start_sshd(root, cfg)
    ssh_stub, ssh_stub_seen = ssh_answers()
    sshd2.kill()
    sys.stdout.write(sshd2.communicate()[0].decode("latin-1", "replace"))
    good = ok and alive and not bad and ssh_with_panel and crash.returncode != 0 and ssh_stub
    print("\n%s: SSH on 22: %s; page served on 127.0.0.1:%d: %s (%.1f s), still running 2 s later: %s, listening on "
          "22/80/443: %s; SSH still on 22 with the panel running: %s; with the panel binary crashing (exit %d): %s"
          % ("ok" if good else "FAILED", ssh_seen, port, "yes" if ok else "NO", took, "yes" if alive else "NO",
             ", ".join(bad) or "none (beyond the SSH server and the 127.0.0.2:80 net-check responder)",
             "yes" if ssh_with_panel else "NO", crash.returncode, ssh_stub_seen if ssh_stub else "NO: " + ssh_stub_seen))
    sys.exit(0 if good else 1)


# ================================================================ step 10 / --check: leak + naming gate
LEAK_HOSTS = [b"114.114.114.114", b"www.baidu.com", b"www.google.com", b"202.120.2.101",
              b"202.112.31.197", b"asia.pool.ntp.org", b"hub.goldshell.com", b"find.goldshell.com"]

# (path, host) pairs that are expected and safe: our own panel binary embeds 114.114.114.114 only in its DNS
# policy code, which REFUSES that resolver (B5 netdns.go cleanDNSList / ensureSaneDNS) and never contacts it.
# The leak gate is for Goldshell's binaries reaching out; the panel is our audited code (RV6 verifies it never
# connects there). Only the decompressed-tree grep sees it (the panel is inside the gzip squashfs, so the raw
# grep can't) — so the raw grep stays strict and only this one file/host pair is exempt.
LEAK_EXEMPT = {("/usr/bin/scbox2-agent", b"114.114.114.114")}

# Naming gate: this is the SC Box II firmware, so no file or directory in the rootfs is named after the SC Lite
# and no script or helper mentions it, in any spelling: "sclite", "sc lite", "sc-lite", "sc_lite", any case.
# Allowed, explicitly, with the reason:
#   /etc/init.d/S48scbox2   at most 1 mention: the one-time settings move from /mnt/config/sclite (test builds
#                           before v0.3.0)
#   /usr/bin/scbox2-agent   any count: the panel's model table recognizes both miners (Goldshell-SCLITE, its
#                           init tag), and keeps the SC Lite firmware's update channel, switched off on the SC Box II
BRAND_OLD_RE = re.compile(rb"sc[ _\-]?lite", re.I)
BRAND_OLD_NAME_RE = re.compile(r"sc[ _\-]?lite", re.I)
BRAND_ALLOW = {"/etc/init.d/S48scbox2": 1, "/usr/bin/scbox2-agent": None}   # None = any count


# Files a build of this kit must ship, checked by the gate in the build (not by --check, which also checks older
# releases): the SSH server and its boot script are scanned like everything else, and must be there to be scanned.
REQUIRED_SCANNED = ("/usr/bin/scbox2-sshd", "/etc/init.d/S47scbox2-sshd")


def check_image(path, require=()):
    """The release gate DESIGN requires: no Chinese-host / Goldshell-cloud / leaking-NTP string anywhere in
    what the device would actually run, and nothing named after the SC Lite. Works on a .ius or an SD .img.
    The system squashfs is gzip-compressed, so a raw byte grep of the image is a FALSE NEGATIVE (it can't see
    strings inside the compressed binaries). So this:
       (a) raw-greps the whole file    -> catches plaintext (the FAT, any uncompressed item), and
       (b) extracts the system item and unsquashfs-es it, then greps every file in the tree
           -> catches strings inside minerd/xminerd/wifi/configs as they appear on the running miner.
    Returns 0 only when (a) and (b) both ran and found nothing; a system item that can't be decompressed is a
    FAILURE, not a silent fall-back to the raw grep."""
    data = open(path, "rb").read()
    hits = []
    for needle in LEAK_HOSTS:
        n = data.count(needle)
        if n:
            hits.append(("<raw image>", needle.decode(), n, "%#x" % data.find(needle)))

    # (b) decompress the system squashfs and grep the files
    files = 0
    try:
        ius = load_tool("ius")
        unsq = find_squashfs_tool("unsquashfs")
        blob, _ = ius.load_container(path)
        c = ius.Container(blob)
        sysit = c.find_type(5)
    except SystemExit as e:
        print("CHECK FAILED: could not open the system item of %s (%s)" % (path, e))
        return 1
    if sysit is None or c.data(sysit)[:4] != SQUASHFS_MAGIC:
        print("CHECK FAILED: no system squashfs in %s to decompress" % path)
        return 1
    tmp = tempfile.mkdtemp(prefix="box2-check-", dir=os.environ.get("BOX2_WORK"))
    try:
        sq = os.path.join(tmp, "s.sqsh")
        open(sq, "wb").write(c.data(sysit))
        tree = os.path.join(tmp, "t")
        r = subprocess.run([unsq, "-n", "-d", tree, sq], capture_output=True, text=True)
        if r.returncode != 0 or not os.path.isdir(os.path.join(tree, "usr", "bin")):
            print("CHECK FAILED: unsquashfs of the system item failed:\n%s%s" % (r.stdout, r.stderr))
            return 1
        for d, dns, fns in os.walk(tree):
            for n in dns + fns:
                if BRAND_OLD_NAME_RE.search(n):
                    hits.append(("/" + os.path.relpath(os.path.join(d, n), tree), "path named 'sclite'", 1, "-"))
        scanned = set()
        for d, _, fns in os.walk(tree):
            for fn in fns:
                fp = os.path.join(d, fn)
                # regular files only: never open a device node or FIFO (/dev/console etc. would block)
                if os.path.islink(fp) or not os.path.isfile(fp):
                    continue
                fd = open(fp, "rb").read()
                files += 1
                rel = "/" + os.path.relpath(fp, tree)
                scanned.add(rel)
                found = BRAND_OLD_RE.findall(fd)
                nb = len(found)
                if nb and rel in BRAND_ALLOW and (BRAND_ALLOW[rel] is None or nb <= BRAND_ALLOW[rel]):
                    pass
                elif nb:
                    hits.append((rel, "name %r" % found[0].decode("latin-1"), nb,
                                 "%#x" % BRAND_OLD_RE.search(fd).start()))
                for needle in LEAK_HOSTS:
                    if (rel, needle) in LEAK_EXEMPT:
                        continue
                    n = fd.count(needle)
                    if n:
                        hits.append((rel, needle.decode(), n, "%#x" % fd.find(needle)))
        for r in require:
            if r not in scanned:
                hits.append((r, "missing (must ship)", 1, "-"))
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    if files == 0:
        print("CHECK FAILED: the decompressed system item has no files")
        return 1

    if hits:
        print("CHECK FAILED: %d leak/naming hit(s) in %s:" % (len(hits), path))
        for where, name, n, off in sorted(hits):
            print("  %-45s %-20s %d hit(s) at %s" % (where, name, n, off))
        return 1
    print("CHECK OK: none of the %d leak hosts are present (raw + %d decompressed rootfs files), and nothing in "
          "the rootfs is named after or mentions the SC Lite (beyond the 2 documented exceptions), in %s"
          % (len(LEAK_HOSTS), files, path))
    return 0


# ================================================================ step 12: --xz, --sums
def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def make_xz(img, out_xz):
    """xz the SD image with XZ_ARGS (byte-reproducible), then read it back: it must decompress to the .img."""
    xz = shutil.which("xz")
    if not xz:
        die("--xz needs xz (XZ Utils)")
    ver = subprocess.run([xz, "--version"], capture_output=True, text=True).stdout.split("\n")[0]
    with open(out_xz, "wb") as f:
        r = subprocess.run([xz] + XZ_ARGS + ["-c", img], stdout=f, stderr=subprocess.PIPE)
    if r.returncode != 0:
        die("xz failed:\n" + r.stderr.decode("latin-1", "replace"))
    d = subprocess.run([xz, "-dc", out_xz], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if d.returncode != 0 or hashlib.sha256(d.stdout).hexdigest() != sha256_file(img):
        die("--xz read-back FAILED: %s does not decompress to the SD image" % out_xz)
    print("  xz %s (%s): %d bytes; read back: decompresses to the SD image exactly"
          % (" ".join(XZ_ARGS), ver, os.path.getsize(out_xz)))


# ================================================================ main
def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--check", metavar="IMAGE", help="only run the leak/naming gate on a built .ius or .img, then exit")
    ap.add_argument("--firmware", help="stock SC Box II firmware.ius (2.2.1)")
    ap.add_argument("--agent", help="the panel ARM binary (tools/build-agent-box2.sh)")
    ap.add_argument("--sshd", help="the SSH server ARM binary (tools/build-sshd-box2.sh)")
    ap.add_argument("--out", help="output .ius (for a release: scbox2-firmware-v<version>.ius)")
    ap.add_argument("--img", help="output SD .img (container at 16 MiB)")
    ap.add_argument("--version", help="release version, e.g. 0.5.0 or 0.5.0-owner (required; must match "
                    "[v]<digit>[0-9A-Za-z.-]*, the panel's update asset pattern)")
    ap.add_argument("--patches-dir", default=HERE, help="where A3's patch_*_box2.py modules live (default: kit dir)")
    ap.add_argument("--card-size", type=int, default=0, help="SD .img size in MiB (0 = minimal 1 MiB-aligned)")
    ap.add_argument("--xz", action="store_true", help="also write IMG.xz (fixed, byte-reproducible xz settings)")
    ap.add_argument("--sums", action="store_true", help="write SHA256SUMS (the .ius and, with --xz, the .img.xz) "
                    "next to --out")
    ap.add_argument("--allow-unknown", action="store_true",
                    help="build from a stock firmware this kit was not checked against (testing only)")
    ap.add_argument("--skip-startup-check", action="store_true", help="do not start the panel under qemu (not "
                    "for releases)")
    ap.add_argument("--require-startup-check", action="store_true",
                    help="fail instead of skipping when the startup check can't run (use for releases)")
    ap.add_argument("--keep-work", action="store_true", help="keep the work tree (for debugging)")
    a = ap.parse_args()

    if a.check:
        sys.exit(check_image(a.check))

    for req in ("firmware", "agent", "sshd", "out", "version"):
        if not getattr(a, req):
            die("--%s is required (or use --check IMAGE)" % req)
    if not VERSION_RE.match(a.version):
        die("--version %r is not a version the panel accepts: it must match %s (e.g. 0.5.0, 0.5.0-owner)"
            % (a.version, VERSION_RE.pattern))
    version = a.version[1:] if a.version.startswith("v") else a.version
    asset = "scbox2-firmware-v%s.ius" % version
    if not ASSET_RE.match(asset):
        die("internal: asset name %r does not match the panel's pattern" % asset)
    if a.xz and not a.img:
        die("--xz needs --img")
    out_dir = os.path.dirname(os.path.abspath(a.out))
    xz_path = (a.img + ".xz") if a.xz else None
    sums_path = os.path.join(out_dir, "SHA256SUMS") if a.sums else None
    if a.sums:
        if os.path.basename(a.out) != asset:
            die("--sums: --out must be named %s (the release asset name the panel looks up), not %s"
                % (asset, os.path.basename(a.out)))
        if xz_path and os.path.dirname(os.path.abspath(xz_path)) != out_dir:
            die("--sums: the .img.xz must be in the same folder as the .ius (%s)" % out_dir)
    if a.skip_startup_check and a.require_startup_check:
        die("--skip-startup-check and --require-startup-check together")

    # outputs: built in the work dir, moved into place only when every gate has passed. Remove stale ones first,
    # so a failed build never leaves an earlier (or partial) image at these paths.
    finals = [(a.out, "ius")] + ([(a.img, "img")] if a.img else []) + ([(xz_path, "xz")] if xz_path else []) \
        + ([(sums_path, "sums")] if sums_path else [])
    for p, _ in finals:
        if os.path.lexists(p):
            print("removing stale output %s" % p)
            os.unlink(p)

    ius = load_tool("ius")
    mksq = find_squashfs_tool("mksquashfs")
    unsq = find_squashfs_tool("unsquashfs")
    print("tools: mksquashfs=%s" % mksq)
    print("       unsquashfs=%s" % unsq)
    print("build time: %s (%s)" % (time.strftime("%Y-%m-%dT%H:%MZ", time.gmtime(BUILD_EPOCH)), BUILD_EPOCH_SOURCE))

    work = tempfile.mkdtemp(prefix="box2-build-", dir=os.environ.get("BOX2_WORK"))
    ok = False
    try:
        # 1-2. open + byte-safe round-trip sanity (R01)
        stock_sqsh, sysit, sys_md5 = extract_system(ius, a.firmware, work, a.allow_unknown)
        roundtrip_sanity(a.firmware, stock_sqsh, work)

        # 3. unpack
        tree = os.path.join(work, "tree")
        print("step 3: unsquashfs -> tree")
        unpack(unsq, stock_sqsh, tree)
        t = Tree(tree)

        # 4. transform
        patchers = load_patchers(a.patches_dir)
        place_panel_and_helpers(t, a.agent, a.sshd)
        place_intminer(t, patchers)
        write_release(t, version, a.agent, sys_md5)
        apply_binary_patches(t, patchers)
        edit_init_scripts(t)
        apply_removals(t)
        set_times(tree)

        # 5-6. rebuild + guards
        print("step 5: mksquashfs (R03 recipe)")
        new_sqsh = rebuild(mksq, tree, os.path.join(work, "system.sqsh"))
        print("step 6: guards")
        size, free = guard_image(new_sqsh)

        # 7. repack (only the system item changes) + SD .img, into the work dir
        print("step 7: repack into .ius + SD .img")
        stage = os.path.join(work, "out")
        os.makedirs(stage)
        w_ius = os.path.join(stage, os.path.basename(a.out))
        w_img = os.path.join(stage, os.path.basename(a.img)) if a.img else None
        w_xz = (w_img + ".xz") if a.xz else None
        cmd = [sys.executable, "-I", os.path.join(TOOLS, "ius_set_system.py"), a.firmware, new_sqsh, w_ius]
        if w_img:
            cmd += ["--img", w_img]
        print(run(cmd).rstrip())
        # prove nothing but the system item changed vs the stock container
        verify_only_system_changed(a.firmware, w_ius, sysit)

        # 8. read-back gate
        readback = readback_gate(ius, unsq, w_ius, w_img, new_sqsh, tree, work)

        # 9. stock upgrade/uboot0 checks (oversize trap included: system <= partition)
        print("step 9: ius verify (stock upgrade / uboot0 checks)")
        res = ius.verify(open(w_ius, "rb").read(), {"system": SYS_PART})
        fails = [m for l, m in res if l == "FAIL"]
        for l, m in res:
            if l in ("FAIL", "WARN"):
                print("  %-4s %s" % (l, m))
        if fails:
            die("the result fails the stock checks:\n  " + "\n  ".join(fails))
        print("  stock checks pass; system %d bytes fits partition %d" % (size, SYS_PART))

        # 10. leak + naming gate, on everything that ships
        print("step 10: leak/naming gate")
        bad = [p for p in [w_ius] + ([w_img] if w_img else []) if check_image(p, REQUIRED_SCANNED) != 0]
        if bad:
            die("leak/naming gate FAILED on %s: not writing any output" % ", ".join(map(os.path.basename, bad)))

        # 11. panel startup check, from the image's own tree
        print("step 11: panel startup check (the panel from the image, isolated)")
        started = startup_check(readback, a.skip_startup_check, a.require_startup_check)

        # 12. xz + sums, then publish
        if w_xz:
            print("step 12: xz")
            make_xz(w_img, w_xz)
        moves = [(w_ius, a.out)] + ([(w_img, a.img)] if w_img else []) + ([(w_xz, xz_path)] if w_xz else [])
        for src, dst in moves:
            os.makedirs(os.path.dirname(os.path.abspath(dst)), exist_ok=True)
            shutil.move(src, dst)
        h_ius = sha256_file(a.out)
        print("\nwrote %s  sha256 %s" % (a.out, h_ius))
        if a.img:
            print("wrote %s  sha256 %s" % (a.img, sha256_file(a.img)))
        if xz_path:
            print("wrote %s  sha256 %s" % (xz_path, sha256_file(xz_path)))
        if sums_path:
            lines = "%s  %s\n" % (h_ius, os.path.basename(a.out))
            if xz_path:
                lines += "%s  %s\n" % (sha256_file(xz_path), os.path.basename(xz_path))
            tmp_sums = sums_path + ".tmp"
            open(tmp_sums, "w").write(lines)
            os.replace(tmp_sums, sums_path)
            print("wrote %s:\n  %s" % (sums_path, lines.rstrip().replace("\n", "\n  ")))
        else:
            print("release asset: publish %s as %s; SHA256SUMS line:\n  %s  %s" % (a.out, asset, h_ius, asset))
        print("\nchanged %d files, added %d, removed %d paths"
              % (len(t.changed), len(t.added), len(t.removed)))
        print("gates: brick-safety OK, read-back OK, stock checks OK, leak/naming OK (.ius%s), startup %s"
              % (" + .img" if a.img else "", "OK (%d runs)" % STARTUP_RUNS if started else "SKIPPED (!)"))
        if not started:
            print("WARNING: the startup check did not run: not a release build")
        ok = True
    finally:
        if not ok:
            # nothing was moved into place before every gate passed; make sure no partial output remains
            for p, _ in finals:
                if os.path.lexists(p):
                    os.unlink(p)
            print("build_box2: BUILD FAILED: no output written", file=sys.stderr)
        if a.keep_work:
            print("work tree kept at", work)
        else:
            shutil.rmtree(work, ignore_errors=True)


def verify_only_system_changed(firmware, out, sysit):
    """Assert the new container equals the stock one everywhere except the system item's data, its trailer
    and the two header words (size + CRC). A strong brick-safety guard (RV2)."""
    a = open(firmware, "rb").read()
    b = open(out, "rb").read()
    off = sysit["offset"]
    # everything before the system item must match except header 0x18..0x1c (CRC) and the system size word
    for i in range(off):
        if a[i] != b[i]:
            # allowed: header CRC and the system descriptor's length word
            if 0x18 <= i < 0x1c:
                continue
            desc_len_off = 0x20 + 16 * sysit["index"] + 12
            if desc_len_off <= i < desc_len_off + 4:
                continue
            die("brick-safety: a byte at %#x before the system item changed (index %d)" % (i, sysit["index"]))
    print("  brick-safety: only the system item (+ its size word and the header CRC) changed before it")


if __name__ == "__main__":
    if sys.argv[1:2] == ["--startup-run"]:       # one isolated startup-check run (see startup_check)
        startup_run(sys.argv[2])
    else:
        main()
