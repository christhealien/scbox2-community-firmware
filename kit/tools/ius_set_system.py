#!/usr/bin/env python3
"""Replace ONLY the system (squashfs) item of an SC Box II firmware.ius, keeping every other byte.

usage: ius_set_system.py STOCK.ius NEW.sqsh OUT.ius [--img OUT.img]
  --img also writes an SD burn image: 16 MiB of zeros, then the container (same layout as the OEM's
        SC Lite burn-*.img, minus its optional FAT partition), padded to a 1 MiB multiple.

Rules enforced (from the R02 analysis of the ramdisk 'upgrade' burner and the SPL):
  * container header CRC32 = zlib.crc32(header[0:0x200] with bytes 0x18..0x1b zeroed)
  * the system entry is the LAST item (type 0x82, part index 5); only its size word (hdr 0x7c), the
    header CRC (hdr 0x18), the item bytes and the 1 KiB md5/name trailer after it change
  * new system size <= partition size (items.st 'system.18432.fs' -> 18432 KiB) and a multiple of 512
    (the burner rounds each write up to 512 and reads that much from the file; in file mode it would
    loop at EOF otherwise)
  * squashfs magic 'hsqs' required
"""
import sys, struct, zlib, hashlib

STOCK_MD5 = {"ius 2.2.1": None}
HDR = 0x200
SYS_PART_KIB = 18432

def crc_hdr(h):
    return zlib.crc32(h[:0x18] + b"\0" * 4 + h[0x1c:HDR]) & 0xffffffff

def die(m): sys.exit("ius_set_system: " + m)

def main():
    a = sys.argv[1:]
    img = None
    if "--img" in a:
        i = a.index("--img"); img = a[i + 1]; del a[i:i + 2]
    stock, new, out = a
    b = bytearray(open(stock, "rb").read())
    if b[:4] != b"+ius": die("not an ius container")
    if crc_hdr(b) != struct.unpack_from("<I", b, 0x18)[0]: die("stock header CRC mismatch")
    n = struct.unpack_from("<I", b, 0x1c)[0]
    ents = [struct.unpack_from("<IIII", b, 0x20 + 16 * i) for i in range(n)]
    last = max(range(n), key=lambda i: (ents[i][0] & 0xffffff))
    w0, idx, w2, size = ents[last]
    if (w0 >> 24) != 0x82 or idx != 5: die("last item is not part index 5 (system)")
    off = (w0 & 0xffffff) * 512
    if b[off:off + 4] != b"hsqs": die("stock system item is not squashfs")
    for i, (x0, _, _, sz) in enumerate(ents):      # system must really be last, nothing after it but the trailer
        if i != last and (x0 & 0xffffff) * 512 + sz > off: die("an item lies after the system item")
    items_st = None
    for x0, ix, _, sz in ents:
        if ix == 1 and (x0 >> 24) == 0x82:
            o = (x0 & 0xffffff) * 512; items_st = bytes(b[o:o + sz]).decode()
    if items_st is None or "system.%d.fs" % SYS_PART_KIB not in items_st:
        die("items.st does not declare system.%d.fs" % SYS_PART_KIB)
    trailer_old = bytes(b[off + size:off + size + 0x40])
    if trailer_old[:16] != hashlib.md5(b[off:off + size]).digest():
        die("stock system trailer md5 does not match (unexpected layout)")
    d = open(new, "rb").read()
    if d[:4] != b"hsqs": die("new system image is not squashfs")
    if len(d) % 4096: d += b"\0" * (4096 - len(d) % 4096)
    if len(d) > SYS_PART_KIB * 1024: die("new system %d > partition %d" % (len(d), SYS_PART_KIB * 1024))
    tail_len = len(b) - (off + size)                 # stock: 1024 bytes (md5 + name + 4 bytes + zeros)
    trailer = bytearray(trailer_old.ljust(tail_len, b"\0")[:tail_len])
    trailer[:16] = hashlib.md5(d).digest()
    nb = bytearray(b[:off]) + d + trailer
    struct.pack_into("<I", nb, 0x20 + 16 * last + 12, len(d))
    struct.pack_into("<I", nb, 0x18, crc_hdr(nb))
    # self-check: everything before the system item except the size word and CRC is unchanged
    diff = [i for i in range(off) if nb[i] != b[i]]
    allowed = set(range(0x18, 0x1c)) | set(range(0x20 + 16 * last + 12, 0x20 + 16 * last + 16))
    if not set(diff) <= allowed: die("unexpected byte changes before the system item: %r" % diff[:8])
    open(out, "wb").write(nb)
    print("wrote %s: system %d bytes (part %d), crc 0x%08x, changed header bytes %s" % (
        out, len(d), SYS_PART_KIB * 1024, struct.unpack_from("<I", nb, 0x18)[0], sorted(diff)))
    if img:
        body = bytes(0x1000000) + bytes(nb)
        body += b"\0" * ((-len(body)) % 0x100000)
        open(img, "wb").write(body)
        print("wrote %s (%d bytes, container at 16 MiB)" % (img, len(body)))

if __name__ == "__main__":
    main()
