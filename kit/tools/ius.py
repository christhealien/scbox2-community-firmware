#!/usr/bin/env python3
"""
ius.py - InfoTM "+ius" burn container tool (SC Box II firmware.ius, also SC Lite burn images).

    ius.py info    FILE                       list header + items (FILE may be a .ius or a raw SD image with +ius at 16 MiB)
    ius.py extract FILE OUTDIR                write every item + manifest.json (enough to rebuild the file byte for byte)
    ius.py pack    OUTDIR OUT [--replace TYPE=PATH ...] [--version S]   rebuild from an extract dir
    ius.py repack  FILE OUT   [--replace TYPE=PATH ...] [--version S]   rebuild from a container (+ replacements)
    ius.py verify  FILE [--part-size TYPE=BYTES ...]   apply exactly the checks the stock `upgrade` and uboot0 apply
    ius.py card    FILE OUT.img [--size MiB]   raw SD-card image: zeros, the container at 16 MiB (no MBR; see REPORT.md)

TYPE is a number or one of the names `upgrade` uses: uboot item ramdisk kernel kernel1 system ramdisk1 ...
Replacing anything but `system` (or config-like non-boot types) needs --unsafe-boot-items (brick risk).

Format (proven against firmware.ius and the `upgrade` binary, see REPORT.md):
  0x000 "+ius"            magic                     (checked by upgrade + uboot0)
  0x004 u32 2             format version            (not checked)
  0x008 8 x 00            reserved
  0x010 char[8]           software version "2.2.1"  (not checked)
  0x018 u32 hcrc          zlib.crc32 of the 512-byte header with this field = 0 (checked by upgrade only)
  0x01c u32 count         number of 16-byte descriptors that follow
  0x020 desc[count]       {u32 w0, u32 w1, u32 w2, u32 length}
        boot descriptor : w0 = 0x01000000|sector, w1 = load addr, w2 = exec addr (BootROM's ISI copy)
        item descriptor : w0 = 0x82000000|sector, w1 = type (0..18), w2 = 0x40000
        data offset = (w0 & 0xffffff) * 512 from the container start (+16 MiB on a card)
  each item: data, then a 64-byte tag {md5(data)[16], name[44], 4 opaque bytes}, zero padding to its sector.
"""
import argparse, hashlib, json, os, struct, sys, zlib

MAGIC = b"+ius"
CARD_OFFSET = 0x1000000
SECTOR = 512
TYPE_NAMES = ["uboot", "item", "ramdisk", "kernel", "kernel1", "system", "ramdisk1", "none", "none", "none",
              "none", "none", "none", "none", "none", "none", "none", "uboot1", "reserved"]
BOOT_TYPES = {0, 1, 2, 3, 4, 6, 17}          # bootloader / items / ramdisk / kernel: never replace casually
DEFAULT_TAG_NAME = {5: b"rootfs.squashfs"}


def die(msg):
    sys.exit("ius.py: " + msg)


def ceil_div(a, b):
    return -(-a // b)


def hcrc(hdr):
    h = bytearray(hdr[:SECTOR])
    h[0x18:0x1C] = b"\0\0\0\0"
    return zlib.crc32(bytes(h)) & 0xFFFFFFFF


def type_num(s):
    if s.isdigit():
        return int(s)
    if s in TYPE_NAMES:
        return TYPE_NAMES.index(s)
    die("unknown item type %r" % s)


def load_container(path):
    data = open(path, "rb").read()
    base = 0
    if data[:4] != MAGIC and data[CARD_OFFSET:CARD_OFFSET + 4] == MAGIC:
        base = CARD_OFFSET                      # raw SD image: container at 16 MiB
    if data[base:base + 4] != MAGIC:
        die("no +ius magic at 0 or at 16 MiB")
    if base:
        # a card image goes on past the container (zeros, a FAT partition...): cut after the last item's
        # tag + one sector, the stock layout
        cnt, = struct.unpack_from("<I", data, base + 0x1C)
        end = SECTOR
        for i in range(min(cnt, 30)):
            w0, _, _, ln = struct.unpack_from("<IIII", data, base + 0x20 + 16 * i)
            end = max(end, (ceil_div((w0 & 0xFFFFFF) * SECTOR + ln + 64, SECTOR) + 1) * SECTOR)
        return data[base:base + end], base
    return data[base:], base


class Container:
    """Parsed container. Everything not understood is kept as raw bytes so pack() is exact."""

    def __init__(self, blob):
        self.blob = blob
        h = blob[:SECTOR]
        self.header = h
        self.version_word, = struct.unpack_from("<I", h, 4)
        self.sw_version = h[0x10:0x18].split(b"\0")[0].decode("latin-1")
        self.crc, self.count = struct.unpack_from("<II", h, 0x18)
        if 0x20 + 16 * self.count > SECTOR:
            die("descriptor count %d does not fit the header" % self.count)
        self.descs = []
        for i in range(self.count):
            w0, w1, w2, ln = struct.unpack_from("<IIII", h, 0x20 + 16 * i)
            d = {"index": i, "w0": w0, "w1": w1, "w2": w2, "length": ln,
                 "flags": w0 >> 24, "sector": w0 & 0xFFFFFF, "offset": (w0 & 0xFFFFFF) * SECTOR}
            d["kind"] = "boot" if d["flags"] == 0x01 else "item"
            d["type"] = w1 if d["kind"] == "item" else None
            d["name"] = (TYPE_NAMES[w1] if d["kind"] == "item" and w1 < len(TYPE_NAMES) else
                         "bootrom-isi" if d["kind"] == "boot" else "type%d" % w1)
            o, n = d["offset"], ln
            tag = blob[o + n:o + n + 64]
            d["md5"] = hashlib.md5(blob[o:o + n]).hexdigest()
            d["tag_ok"] = len(tag) == 64 and tag[:16] == hashlib.md5(blob[o:o + n]).digest()
            d["tag_name"] = tag[16:60].split(b"\0")[0].decode("latin-1") if d["tag_ok"] else None
            self.descs.append(d)

    def items(self):
        return [d for d in self.descs if d["kind"] == "item"]

    def data(self, d):
        return self.blob[d["offset"]:d["offset"] + d["length"]]

    def find_type(self, t):
        # same rule as upgrade's ius_get_image: first descriptor of that type with a non-zero length
        for d in self.descs:
            if d["kind"] == "item" and d["type"] == t and d["length"]:
                return d
        return None


# ---------------------------------------------------------------- layout model used by pack
def model_from_container(c):
    """A model that reproduces c.blob exactly: per-region raw bytes + the original sector slack."""
    regs = sorted(c.descs, key=lambda d: d["offset"])
    model = {"header": c.header.hex(), "regions": [], "tail_slack_sectors": 0}
    end_prev = SECTOR
    for k, d in enumerate(regs):
        o, n = d["offset"], d["length"]
        nxt = regs[k + 1]["offset"] if k + 1 < len(regs) else len(c.blob)
        used = n + (64 if d["tag_ok"] else 0)
        slack = (nxt - o) // SECTOR - ceil_div(used, SECTOR) if nxt % SECTOR == 0 else None
        gap_before = c.blob[end_prev:o]
        model["regions"].append({
            "index": d["index"], "kind": d["kind"], "type": d["type"], "name": d["name"],
            "length": n, "tag": c.blob[o + n:o + n + 64].hex() if d["tag_ok"] else None,
            "pad": c.blob[o + used:nxt].hex() if any(c.blob[o + used:nxt]) else len(c.blob[o + used:nxt]),
            "slack_sectors": slack, "gap_before": gap_before.hex() if any(gap_before) else len(gap_before),
        })
        end_prev = nxt
    return model


def build(model, payloads, version=None):
    """payloads: index -> bytes. Regions keep their order; a region whose length changed shifts the
    following ones (each keeps its original slack). Returns the container bytes."""
    hdr = bytearray(bytes.fromhex(model["header"]))
    out = bytearray(hdr)
    for r in model["regions"]:
        g = r["gap_before"]
        out += bytes.fromhex(g) if isinstance(g, str) else b"\0" * g
        if len(out) % SECTOR:
            die("internal: region %d not sector aligned" % r["index"])
        start = len(out)
        data = payloads[r["index"]]
        out += data
        if r["tag"] is not None:
            tag = bytearray(bytes.fromhex(r["tag"]))
            if tag[:16] != hashlib.md5(data).digest():          # new data: new tag
                name = tag[16:60]
                if r["type"] in DEFAULT_TAG_NAME and not any(name):
                    name = DEFAULT_TAG_NAME[r["type"]].ljust(44, b"\0")
                tag = bytearray(hashlib.md5(data).digest() + bytes(name) + b"\0\0\0\0")
            out += tag
        used = len(out) - start
        if len(data) == r["length"] and isinstance(r["pad"], str):
            out += bytes.fromhex(r["pad"])
        elif len(data) == r["length"]:
            out += b"\0" * r["pad"]
        else:
            slack = r["slack_sectors"] if r["slack_sectors"] is not None else 1
            slack = max(slack, 1)          # always leave the stock +1 sector (upgrade reads whole sectors)
            out += b"\0" * ((ceil_div(used, SECTOR) + slack) * SECTOR - used)
        # descriptor
        i = r["index"]
        w0, w1, w2, _ = struct.unpack_from("<IIII", hdr, 0x20 + 16 * i)
        sec = start // SECTOR
        if sec > 0xFFFFFF:
            die("container too large")
        struct.pack_into("<IIII", hdr, 0x20 + 16 * i, (w0 & 0xFF000000) | sec, w1, w2, len(data))
    if version is not None:
        v = version.encode()
        if len(v) > 8:
            die("version string longer than 8 bytes")
        hdr[0x10:0x18] = v.ljust(8, b"\0")
    hdr[0x18:0x1C] = struct.pack("<I", hcrc(hdr))
    out[:SECTOR] = hdr
    return bytes(out)


def apply_replacements(c, model, payloads, reps, unsafe):
    for spec in reps or []:
        if "=" not in spec:
            die("--replace wants TYPE=PATH")
        t, path = spec.split("=", 1)
        t = type_num(t)
        if t in BOOT_TYPES and not unsafe:
            die("refusing to replace %s (type %d): bootloader/items/ramdisk/kernel are brick-critical; "
                "pass --unsafe-boot-items only with a proven reason" % (TYPE_NAMES[t], t))
        d = c.find_type(t)
        if d is None:
            die("container has no item of type %d" % t)
        payloads[d["index"]] = open(path, "rb").read()
        # the boot descriptor aliases the uboot item's bytes (BootROM copy at sector 1): keep them in step
        if t == 0:
            for b in c.descs:
                if b["kind"] == "boot":
                    payloads[b["index"]] = payloads[d["index"]]


# ---------------------------------------------------------------- verify = the stock checks
def parse_items_st(text):
    parts = {}
    for line in text.splitlines():
        line = line.split("#")[0].split()
        if len(line) >= 2 and line[0].startswith("part"):
            f = line[1].split(".")
            parts[f[0]] = {"kib": int(f[1]), "kind": f[2] if len(f) > 2 else "", "fs": f[3] if len(f) > 3 else ""}
    return parts


def verify(blob, part_sizes, file_mode=True, quiet=False):
    """Returns a list of (level, message). level 'FAIL' means stock upgrade would reject/hang/fail."""
    res = []
    say = lambda lvl, msg: res.append((lvl, msg))
    if blob[:4] != MAGIC:
        say("FAIL", "magic != +ius  (upgrade: 'Check magic of ius failed', uboot0: no card boot)")
        return res
    stored, = struct.unpack_from("<I", blob, 0x18)
    calc = hcrc(blob)
    say("OK" if stored == calc else "FAIL", "header CRC32 stored %08x calc %08x (upgrade ius_check_header)" % (stored, calc))
    c = Container(blob)
    # uboot0 (card boot): magic only, then type<=0x11 && length!=0 -> load address table
    for d in c.descs:
        if d["kind"] == "item" and d["length"] and d["w1"] > 0x11:
            say("WARN", "desc %d type %d > 17 is ignored by uboot0" % (d["index"], d["w1"]))
    items_st = c.find_type(1)
    parts = parse_items_st(c.data(items_st).decode("latin-1")) if items_st else {}
    for t in range(19):                     # upgrade loops type 0..18
        d = c.find_type(t)
        if d is None:
            continue
        name = TYPE_NAMES[t]
        o, n = d["offset"], d["length"]
        need = o + ceil_div(n, SECTOR) * SECTOR      # upgrade reads in whole 512-byte sectors
        if file_mode and need > len(blob):
            say("FAIL", "%s: data+sector padding ends at %#x beyond file end %#x: upgrade's read loop would spin at EOF" % (name, need, len(blob)))
        elif o + n > len(blob):
            say("FAIL", "%s: data beyond end of container" % name)
        p = parts.get(name)
        if name in part_sizes:
            psize = part_sizes[name]
            src = "given"
        elif p is None:
            psize, src = 0, "no partN entry"
        elif p["kind"] == "boot":
            psize, src = p["kib"] * 1024, "items.st %s.%d.boot" % (name, p["kib"])
        else:
            psize, src = p["kib"] * 1024, "ASSUMED = items.st %d KiB (real check uses BLKGETSIZE64 of /dev/spiblockN)" % p["kib"]
        if psize == 0:
            say("INFO", "%s: %d bytes, partition size 0 (%s): upgrade skips it without error (card-boot only)" % (name, n, src))
            continue
        rounded = ceil_div(n, SECTOR) * SECTOR
        if n > psize:
            say("FAIL", "%s: %d bytes > partition %d (%s): 'not enough size', upgrade fails" % (name, n, psize, src))
        elif rounded > psize:
            say("FAIL", "%s: sector-rounded write %d > partition %d: short write, upgrade fails" % (name, rounded, psize))
        else:
            say("OK", "%s: %d bytes at %#x fits partition %d (%s); written and read back+memcmp'd" % (name, n, o, psize, src))
        if not d["tag_ok"]:
            say("WARN", "%s: md5 tag missing or not matching the data: possible corruption (upgrade/uboot0 never check data, they burn it as is)" % name)
        if t == 5:
            sq = blob[o:o + 4]
            if sq != b"hsqs":
                say("WARN", "system item is not a squashfs (magic %r); the SC Box II mounts system as squashfs" % sq)
            else:
                bytes_used, = struct.unpack_from("<Q", blob, o + 40)
                say("OK" if bytes_used <= n else "FAIL", "system: squashfs bytes_used %d <= item %d" % (bytes_used, n))
    return res


# ---------------------------------------------------------------- commands
def cmd_info(a):
    blob, base = load_container(a.file)
    c = Container(blob)
    print("container at %#x, %d bytes, format %d, version %r, hcrc %08x (%s), %d descriptors" % (
        base, len(blob), c.version_word, c.sw_version, c.crc, "ok" if hcrc(blob) == c.crc else "BAD", c.count))
    print("%-2s %-5s %-11s %-6s %-10s %-10s %-10s %-32s %s" % ("#", "kind", "name", "type", "offset", "length", "w2", "md5", "tag"))
    for d in c.descs:
        extra = "load=%#x exec=%#x" % (d["w1"], d["w2"]) if d["kind"] == "boot" else ""
        print("%-2d %-5s %-11s %-6s %#-10x %#-10x %#-10x %s %s %s" % (
            d["index"], d["kind"], d["name"], d["type"] if d["type"] is not None else "-", d["offset"], d["length"],
            d["w2"], d["md5"], ("ok " + d["tag_name"]) if d["tag_ok"] else "none", extra))


def cmd_extract(a):
    blob, base = load_container(a.file)
    c = Container(blob)
    os.makedirs(a.outdir, exist_ok=True)
    model = model_from_container(c)
    files = {}
    for d in c.descs:
        fn = "%02d-%s.bin" % (d["index"], d["name"])
        open(os.path.join(a.outdir, fn), "wb").write(c.data(d))
        files[str(d["index"])] = fn
    model["files"] = files
    model["source_sha256"] = hashlib.sha256(blob).hexdigest()
    json.dump(model, open(os.path.join(a.outdir, "manifest.json"), "w"), indent=1)
    print("extracted %d regions to %s" % (len(files), a.outdir))


def _pack(c, model, payloads, a):
    apply_replacements(c, model, payloads, a.replace, a.unsafe_boot_items)
    # the boot descriptor must stay a byte copy of the uboot item
    out = build(model, payloads, a.version)
    open(a.out, "wb").write(out)
    print("wrote %s (%d bytes, sha256 %s)" % (a.out, len(out), hashlib.sha256(out).hexdigest()))
    bad = [m for l, m in verify(out, {}) if l == "FAIL"]
    if bad:
        die("result fails the stock checks:\n  " + "\n  ".join(bad))


def cmd_repack(a):
    blob, base = load_container(a.file)
    c = Container(blob)
    model = model_from_container(c)
    payloads = {d["index"]: c.data(d) for d in c.descs}
    _pack(c, model, payloads, a)


def cmd_pack(a):
    model = json.load(open(os.path.join(a.outdir, "manifest.json")))
    payloads = {int(k): open(os.path.join(a.outdir, v), "rb").read() for k, v in model["files"].items()}
    # rebuild a Container view of the original header for type lookups
    hdr = bytes.fromhex(model["header"])
    fake = bytearray(hdr)
    c = Container.__new__(Container)
    c.blob = bytes(fake)
    c.descs = []
    for r in model["regions"]:
        c.descs.append({"index": r["index"], "kind": r["kind"], "type": r["type"],
                        "length": r["length"] or 1})
    _pack(c, model, payloads, a)


def cmd_verify(a):
    blob, base = load_container(a.file)
    ps = {}
    for s in a.part_size or []:
        t, v = s.split("=")
        ps[TYPE_NAMES[type_num(t)]] = int(v, 0)
    res = verify(blob, ps)
    for lvl, msg in res:
        print("%-4s %s" % (lvl, msg))
    if base:
        print("INFO container found at %#x (SD-card layout): uboot0 and upgrade both read it at 16 MiB" % base)
    sys.exit(1 if any(l == "FAIL" for l, _ in res) else 0)


def cmd_card(a):
    blob, base = load_container(a.file)
    size = a.size * 1024 * 1024
    if CARD_OFFSET + len(blob) > size:
        die("card image too small")
    with open(a.out, "wb") as f:
        f.truncate(size)
        f.seek(CARD_OFFSET)
        f.write(blob)
    print("wrote %s: %d MiB, +ius at 16 MiB" % (a.out, a.size))


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    s = p.add_subparsers(dest="cmd", required=True)
    x = s.add_parser("info"); x.add_argument("file"); x.set_defaults(fn=cmd_info)
    x = s.add_parser("extract"); x.add_argument("file"); x.add_argument("outdir"); x.set_defaults(fn=cmd_extract)
    for name, fn, first in (("repack", cmd_repack, "file"), ("pack", cmd_pack, "outdir")):
        x = s.add_parser(name); x.add_argument(first); x.add_argument("out")
        x.add_argument("--replace", action="append", metavar="TYPE=PATH")
        x.add_argument("--version"); x.add_argument("--unsafe-boot-items", action="store_true")
        x.set_defaults(fn=fn)
    x = s.add_parser("verify"); x.add_argument("file"); x.add_argument("--part-size", action="append", metavar="TYPE=BYTES")
    x.set_defaults(fn=cmd_verify)
    x = s.add_parser("card"); x.add_argument("file"); x.add_argument("out"); x.add_argument("--size", type=int, default=64)
    x.set_defaults(fn=cmd_card)
    a = p.parse_args()
    a.fn(a)


if __name__ == "__main__":
    main()
