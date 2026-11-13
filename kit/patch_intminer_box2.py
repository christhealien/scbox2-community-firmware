#!/usr/bin/env python3
"""All binary edits for the Goldshell SC Box II intminer (stock fw 2.2.1 /usr/bin/intminer, BFGMiner 5.4.2 +
Goldshell "gsscbox2" driver, ICT580; md5 be17560d5fc8caca6691e66b596ce8cb /
sha256 8badec162b9a55721479a0c9cb7f305e1be34fffd4da64b213d5c15b8a29eb2f). This is the Box II equivalent of the
SC Lite kit's patch_intminer.py + patch_divzero.py + patch_poolinput.py, consolidated into one module. The
three groups share the binary's dead code without overlapping; every original byte is checked and the stock
md5 is gated before anything is written. Exposes patch(data) -> data (applies all three groups).

Re-derived by research lane R09 (pool guards + divzero) and R10 (chip hook). All addresses are virtual;
file offset = va - 0x8000 (first PT_LOAD at va 0x8000, offset 0; .text VA 0xc328 at file 0x4328).

GROUPS
  (1) CHIP HOOK  -- two call sites in the board-clock function 0x1bdf64 and an 80-byte cave at 0x1bd5d4.
      The library (sclite_chip.so) fills a slot word in .bss (SLOT 0x230ff8); the cave calls
      hook(dev, frame, phase) through it.  PRE  0x1be21a ("mov.w r3,#1"): BEFORE the miner overwrites its
      per-chip clock table with the board clock (the table still shows DTFS slow-downs / trims).
      POST 0x1be2a4 ("mov.w r3,#0"): right before the epilogue, table already reset.  phase = 0 (PRE) / 1
      (POST).  Chip trims ship OFF on Box II (HARD RULE 1): this hook may be built but trims stay disabled by
      the model profile.  The cave 0x1bd5d4 is a DIFFERENT dead function from the SC Lite one -- the SC Lite
      cave site (the DTFS counter) is live here (called from the nonce path), and on Box II the DTFS code
      uses that old cave; 0x1bd5d4 is the "plugin GPIO" reader (dev+0x800d8), reached by no bl/b/blx/pointer
      in the whole file (checked: find_cave.py + a raw word scan over the LOAD segment).

  (2) DIVZERO  -- the driver divides by a board's chip count (dev+0x800ac) in the DTFS average (0x1be6dc,
      0x1be71c) and the chip-temperature average (0x1becba); each is a signed `bl __divsi3` (0x1cbf1c) whose
      __aeabi_idiv0 path raises SIGFPE, so a board reporting 0 chips kills the whole miner. A 10-byte stub at
      0x48cfc returns 0 for divisor 0 (DTFS pass needs avg>=5 so it skips; the temperature average reads 0),
      and the three calls retarget to it. NEEDS A HARDWARE TEST (same caveat as SC Lite).

  (3) POOL GUARDS -- (a) difficulty floor: the stock refuse-0 test at 0x4a9c2 accepts tiny positives and
      negatives; a tiny positive difficulty SIGSEGVs the SC Lite miner (whether Box II's 36xICT580 crashes
      the same way is a HARDWARE question, but the floor is a safe strict replacement). FLOOR = 2^-32 refuses
      0, negatives and NaN through the stock refuse path and accepts anything strictly greater.
      (b) receive-line cap: recalloc_sock (0x477d8) grows pool->sockbuf (pool+0x380, size at pool+0x384)
      without limit while a line has no newline; the size test at 0x47850 branches to a cave that, over CAP
      (1 MiB), empties the buffer and returns instead of growing it. Both use caves inside the same dead
      function as the divzero stub.

  (4) NTIME  -- TWO sites. (a) BLAKE2B, the one this miner actually uses: gen_stratum_work2 hands header
      building to the coin's algorithm descriptor when it has one. blake2b's descriptor (data va 0x227f10,
      name "blake2b") has a builder at slot +0x18 = 0x1ba439 (Thumb 0x1ba438). It computes the timestamp the
      same way: 0x1ba5ae `ldr r5,[r3,#0x5c]` (swork->ntime), 0x1ba5bc `bl 0x1b8f40` (its own copy of
      timer_elapsed(&swork->tv_received, NULL)), 0x1ba5c2 `adds r3,r5,r3`, then htobe32 into the header. Seen
      on hardware after the generic fix below: submit ntime "0000000a00000000" (+10 s) on the first job ->
      time-too-new. Same edit: the call becomes `movs r0,#0 ; nop`.
      (b) GENERIC (other algorithms; kept for completeness): the stratum work generator (gen_stratum_work2, 0x2fff0) writes the header timestamp as
      htobe32(job ntime + timer_elapsed(job received)): 0x30296 `bl timer_elapsed(&swork->tv_received, NULL)`
      (0xc9c0, whole seconds), 0x3029c `adds r3, r5, r3` (r5 = swork->ntime, swork+0x5c), then a byte swap
      into work->data[68]. On the SC Box II's blake2b stratum the job ntime is an 8-byte field the pool sends
      as 0 and expects back unchanged; the rolled 4-byte value lands in the wrong bytes (seen on hardware:
      submit ntime "0000001300000000" after 19 s), which the pool rejects as time-too-new (DATUM: more than
      curtime + 7200). It only shows when the miner builds extra work from a job some seconds after receiving
      it, i.e. the first job while the chips start up: 2-7 rejected shares after every miner start. The call
      becomes `movs r0, #0 ; nop`: elapsed = 0, the header carries the pool's own job time, exactly what every
      accepted share sends. Fresh work still comes from extranonce2, as on every other job. No cave needed.

SHARED CAVE LAYOUT (none overlapping; the three groups apply in any order)
  dead function 0x48cfc..0x4905e (866 bytes, unreferenced):
      0x48cfc .. 0x48d06   divzero stub   (10 bytes)      group 2
      0x48d06 .. 0x48d24   difficulty cave                group 3a
      0x48d24 .. 0x48d40   receive cave                   group 3b
  dead function 0x1bd5d4..0x1bd624 (80 bytes, unreferenced):
      0x1bd5d4 .. +len(PRE)   PRE  hook entry             group 1
      +len(PRE) .. +len(CAVE) POST hook entry             group 1
"""
import hashlib
import struct
import sys

STOCK_MD5 = "be17560d5fc8caca6691e66b596ce8cb"
STOCK_SHA256 = "8badec162b9a55721479a0c9cb7f305e1be34fffd4da64b213d5c15b8a29eb2f"
BASE = 0x8000


def off(va):
    return va - BASE


# ---------------------------------------------------------------------------
# shared Thumb-2 encoders
# ---------------------------------------------------------------------------
def t4(frm, to, link=False):
    """Thumb-2 b.w (link=False) / bl (link=True) from frm to to."""
    o = to - (frm + 4)
    assert -(1 << 24) <= o < (1 << 24) and o % 2 == 0
    s = 1 if o < 0 else 0
    imm = o & ((1 << 25) - 1)
    i1, i2 = (imm >> 23) & 1, (imm >> 22) & 1
    j1, j2 = (1 - i1) ^ s, (1 - i2) ^ s
    return struct.pack("<HH", 0xF000 | (s << 10) | ((imm >> 12) & 0x3FF),
                       (0xD000 if link else 0x9000) | (j1 << 13) | (j2 << 11) | ((imm >> 1) & 0x7FF))


def bcond_n(frm, to, cond):
    o = to - (frm + 4)
    assert -256 <= o < 256 and o % 2 == 0
    return struct.pack("<H", 0xD000 | (cond << 8) | ((o >> 1) & 0xFF))


def movw(rd, imm):
    i, imm4, imm3, imm8 = (imm >> 11) & 1, (imm >> 12) & 0xF, (imm >> 8) & 7, imm & 0xFF
    return struct.pack("<HH", 0xF240 | (i << 10) | imm4, (imm3 << 12) | (rd << 8) | imm8)


def movt(rd, imm):
    i, imm4, imm3, imm8 = (imm >> 11) & 1, (imm >> 12) & 0xF, (imm >> 8) & 7, imm & 0xFF
    return struct.pack("<HH", 0xF2C0 | (i << 10) | imm4, (imm3 << 12) | (rd << 8) | imm8)


GT, HI = 0xC, 0x8


def _check(b, va, want, what):
    o = off(va)
    if bytes(b[o:o + len(want)]) != want:
        raise SystemExit("intminer: unexpected bytes at %#x (%s): %s, expected %s"
                         % (va, what, bytes(b[o:o + len(want)]).hex(), want.hex()))


# ---------------------------------------------------------------------------
# (1) CHIP HOOK  (research lane R10)
# ---------------------------------------------------------------------------
PRE_AT = 0x1be21a
POST_AT = 0x1be2a4
CAVE_AT = 0x1bd5d4
SLOT = 0x230ff8
PRE_ORIG = bytes.fromhex("4ff00103")   # mov.w r3, #1
CALL_ORIG = bytes.fromhex("4ff00003")  # mov.w r3, #0
CAVE_ORIG = bytes.fromhex(             # the whole dead function 0x1bd5d4..0x1bd624 (80 bytes)
    "80b584b000af78607b6803f5002303f1d8031b6818464ef64ae80346002b02d04ff00103"
    "0fe07b6803f5002303f1d8031b6818464ef628ecf860fb68002bccbf00230123dbb2184607f11007bd4680bd")
assert len(CAVE_ORIG) == 80


def _hook_entry(phase, orig):
    return (bytes.fromhex("2de93f50")      # push.w {r0-r5, r12, lr}
            + bytes.fromhex("f868")        # ldr r0, [r7, #12]  (dev)
            + bytes.fromhex("3946")        # mov r1, r7         (frame)
            + bytes([phase, 0x22])         # movs r2, #phase
            + movw(3, SLOT & 0xFFFF) + movt(3, SLOT >> 16)
            + bytes.fromhex("1b68")        # ldr r3, [r3]       (hook slot)
            + bytes.fromhex("03b1")        # cbz r3, +4         (skip the blx when unset)
            + bytes.fromhex("9847")        # blx r3
            + bytes.fromhex("bde83f50")    # pop.w {r0-r5, r12, lr}
            + orig                         # the instruction the call replaced
            + bytes.fromhex("7047"))       # bx lr


PRE_CAVE = _hook_entry(0, PRE_ORIG)
POST_CAVE = _hook_entry(1, CALL_ORIG)
CHIP_CAVE = PRE_CAVE + POST_CAVE


def patch_chiphook(data):
    b = bytearray(data)
    _check(b, PRE_AT, PRE_ORIG, "clock-table reset (PRE)")
    _check(b, POST_AT, CALL_ORIG, "clock function end (POST)")
    _check(b, CAVE_AT, CAVE_ORIG, "dead cave function (chip hook)")
    assert len(CHIP_CAVE) <= 80 and len(PRE_CAVE) % 2 == 0
    b[off(PRE_AT):off(PRE_AT) + 4] = t4(PRE_AT, CAVE_AT, link=True)
    b[off(POST_AT):off(POST_AT) + 4] = t4(POST_AT, CAVE_AT + len(PRE_CAVE), link=True)
    b[off(CAVE_AT):off(CAVE_AT) + len(CHIP_CAVE)] = CHIP_CAVE
    return bytes(b)


# ---------------------------------------------------------------------------
# (2) DIVZERO  (research lane R09)
# ---------------------------------------------------------------------------
DIVSI3 = 0x1cbf1c
STUB_AT = 0x48cfc
STUB_ORIG = bytes.fromhex("80b500af44f69410c0f2")  # head of the dead function
DIV_SITES = {  # call site: original `bl 0x1cbf1c` bytes
    0x1be6dc: "0df01efc",
    0x1be71c: "0df0fefb",
    0x1becba: "0df02ff9",
}


def _divstub():
    cbz = struct.pack("<H", 0xB100 | (1 << 3) | 1)  # cbz r1, +6 (to movs)
    return cbz + t4(STUB_AT + 2, DIVSI3, link=False) + bytes.fromhex("0020" "7047")  # movs r0,#0 ; bx lr


def patch_divzero(data):
    b = bytearray(data)
    _check(b, STUB_AT, STUB_ORIG, "divzero stub site")
    for va, orig in DIV_SITES.items():
        o = off(va)
        if bytes(b[o:o + 4]) != bytes.fromhex(orig) or t4(va, DIVSI3, link=True) != bytes.fromhex(orig):
            raise SystemExit("intminer: unexpected call at %#x: %s" % (va, bytes(b[o:o + 4]).hex()))
    st = _divstub()
    b[off(STUB_AT):off(STUB_AT) + len(st)] = st
    for va in DIV_SITES:
        b[off(va):off(va) + 4] = t4(va, STUB_AT, link=True)
    return bytes(b)


# ---------------------------------------------------------------------------
# (3) POOL GUARDS  (research lane R09)
# ---------------------------------------------------------------------------
FLOOR_HI = 0x3DF00000  # FLOOR = 2^-32 as a double (low word 0)
CAP = 0x100000         # 1 MiB for one stratum line

DIFF_SITE = 0x4a9c2
DIFF_SITE_ORIG = bytes.fromhex("f5ee400b" "f1ee10fa" "02d1")  # vcmp.f64 d16,#0 ; vmrs ; bne accept
DIFF_REFUSE = 0x4a9cc   # mov.w r3,#0 ; b ...   (return false)
DIFF_OK = 0x4a9d2       # stock "accepted" path
DIFF_CTX = {0x4a9cc: "4ff00003" "ebe0", 0x4a9d2: "07f10803"}
DIFF_CAVE = 0x48d06

RECV_SITE = 0x47850
RECV_SITE_ORIG = bytes.fromhex("9a42" "00f28180")  # cmp r2,r3 ; bhi.w 0x47958
RECV_GROW = 0x47856     # stock grow path
RECV_RET = 0x47958      # nop before the epilogue
RECV_CTX = {0x477d8: "80b5", 0x477e0: "02af",
            0x477e2: "07f11003" "a3f10c03" "1860",  # pool at [r7,#4]
            0x47856: "07f58051", 0x47958: "00bf" "07f11007"}
RECV_CAVE = 0x48d24

CAVE_END = 0x48d40
DIFF_CAVE_ORIG = bytes.fromhex("1d0044f6a411c0f21d01fff7d8fe44f69410c0f21d0044f6c011c0f21d01")
RECV_CAVE_ORIG = bytes.fromhex("fff7cefe44f69410c0f21d0044f6e411c0f21d01fff7c4fe44f69410")


def _diff_cave():
    a = DIFF_CAVE
    c = bytes.fromhex("0022" "0023")                      # movs r2,#0 ; movs r3,#0
    c += movt(3, FLOOR_HI >> 16)                          # r3:r2 = FLOOR
    c += bytes.fromhex("43ec312b" "f4ee610b" "f1ee10fa")  # vmov d17,r2,r3 ; vcmp.f64 d16,d17 ; vmrs
    ok = a + len(c) + 2 + 4
    c += bcond_n(a + len(c), ok, GT)                      # bgt ok   (NaN: unordered, not taken)
    c += t4(a + len(c), DIFF_REFUSE)                      # b.w refuse
    c += t4(a + len(c), DIFF_OK)                          # ok: b.w accepted path
    return c


def _recv_cave():
    a = RECV_CAVE
    c = bytes.fromhex("9a42")                             # cmp r2,r3 (size vs needed, as stock)
    ret = a + 28 - 4
    c += bcond_n(a + len(c), ret, HI)                     # bhi ret
    c += bytes.fromhex("b3f5801f")                        # cmp.w r3,#CAP
    assert CAP == 0x100000
    cap = a + len(c) + 2 + 4
    c += bcond_n(a + len(c), cap, HI)                     # bhi cap
    c += t4(a + len(c), RECV_GROW)                        # b.w grow (stock path)
    assert a + len(c) == cap
    c += bytes.fromhex("7b68" "d3f88033" "0022" "1a70")   # cap: ldr r3,[r7,#4]; ldr.w r3,[r3,#0x380]; movs r2,#0; strb r2,[r3]
    assert a + len(c) == ret
    c += t4(a + len(c), RECV_RET)                         # ret: b.w 0x47958
    return c


def patch_poolguards(data):
    b = bytearray(data)
    _check(b, DIFF_SITE, DIFF_SITE_ORIG, "difficulty check")
    _check(b, RECV_SITE, RECV_SITE_ORIG, "recalloc_sock size test")
    for va, h in list(DIFF_CTX.items()) + list(RECV_CTX.items()):
        _check(b, va, bytes.fromhex(h), "context")
    _check(b, DIFF_CAVE, DIFF_CAVE_ORIG, "dead function (diff cave)")
    _check(b, RECV_CAVE, RECV_CAVE_ORIG, "dead function (recv cave)")
    dc, rc = _diff_cave(), _recv_cave()
    assert DIFF_CAVE + len(dc) <= RECV_CAVE and RECV_CAVE + len(rc) <= CAVE_END
    b[off(DIFF_CAVE):off(DIFF_CAVE) + len(dc)] = dc
    b[off(RECV_CAVE):off(RECV_CAVE) + len(rc)] = rc
    b[off(DIFF_SITE):off(DIFF_SITE) + 10] = t4(DIFF_SITE, DIFF_CAVE) + bytes.fromhex("00bf00bf00bf")
    b[off(RECV_SITE):off(RECV_SITE) + 6] = t4(RECV_SITE, RECV_CAVE) + bytes.fromhex("00bf")
    return bytes(b)


# ---------------------------------------------------------------------------
# (4) NTIME  (hardware log 2026-11-10: time-too-new on the first job after start)
# ---------------------------------------------------------------------------
TIMER_ELAPSED = 0xc9c0
NTIME_CALL = 0x30296
NTIME_CALL_ORIG = bytes.fromhex("dcf793fb")  # bl 0xc9c0 <timer_elapsed>
NTIME_CTX = {
    0x30280: "dd6d",          # ldr r5, [r3, #0x5c]      r5 = swork->ntime
    0x3028c: "03f16403",      # add.w r3, r3, #0x64      &swork->tv_received
    0x30292: "4ff00001",      # mov.w r1, #0             (now = NULL)
    0x3029a: "0346",          # mov r3, r0               elapsed
    0x3029c: "eb18",          # adds r3, r5, r3          ntime + elapsed
}
NTIME_NEW = bytes.fromhex("0020" "00bf")      # movs r0, #0 ; nop

# (a) the blake2b builder (the path this miner takes)
B2_ELAPSED = 0x1b8f40
B2_CALL = 0x1ba5bc
B2_CALL_ORIG = bytes.fromhex("fef7c0fc")     # bl 0x1b8f40 <timer_elapsed copy>
B2_CTX = {
    0x1ba5ae: "dd6d",          # ldr r5, [r3, #0x5c]      r5 = swork->ntime
    0x1ba5b2: "03f16403",      # add.w r3, r3, #0x64      &swork->tv_received
    0x1ba5b8: "4ff00001",      # mov.w r1, #0             (now = NULL)
    0x1ba5c0: "0346",          # mov r3, r0               elapsed
    0x1ba5c2: "eb18",          # adds r3, r5, r3          ntime + elapsed
}
B2_DESC_SLOT_FILE = 0x217f28   # blake2b descriptor (va 0x227f10, data segment file 0x217f10) + 0x18
B2_DESC_NAME_FILE = 0x217f10   # its name pointer -> "blake2b"


def patch_ntime(data):
    b = bytearray(data)
    # (a) blake2b builder: prove the descriptor really points at this function, then patch its elapsed call
    if struct.unpack_from("<I", b, B2_DESC_SLOT_FILE)[0] != (0x1ba438 | 1):
        raise SystemExit("intminer: blake2b descriptor slot +0x18 doesn't point at the builder 0x1ba438")
    name_va = struct.unpack_from("<I", b, B2_DESC_NAME_FILE)[0]
    name_off = name_va - 0x8000
    if bytes(b[name_off:name_off + 8]) != b"blake2b\0":
        raise SystemExit("intminer: the descriptor at 0x227f10 is not blake2b's")
    _check(b, B2_CALL, B2_CALL_ORIG, "timer_elapsed call in the blake2b header builder")
    assert t4(B2_CALL, B2_ELAPSED, link=True) == B2_CALL_ORIG
    for va, h in B2_CTX.items():
        _check(b, va, bytes.fromhex(h), "blake2b ntime context")
    b[off(B2_CALL):off(B2_CALL) + 4] = NTIME_NEW
    # (b) the generic builder
    _check(b, NTIME_CALL, NTIME_CALL_ORIG, "timer_elapsed call in the stratum header builder")
    assert t4(NTIME_CALL, TIMER_ELAPSED, link=True) == NTIME_CALL_ORIG
    for va, h in NTIME_CTX.items():
        _check(b, va, bytes.fromhex(h), "ntime context")
    b[off(NTIME_CALL):off(NTIME_CALL) + 4] = NTIME_NEW
    return bytes(b)


# ---------------------------------------------------------------------------
# cross-group non-overlap guard + top-level patch
# ---------------------------------------------------------------------------
def _assert_disjoint():
    # group 2 + group 3 share the 0x48cfc dead function; prove the three spans don't overlap.
    spans = [(STUB_AT, STUB_AT + len(_divstub())),
             (DIFF_CAVE, DIFF_CAVE + len(_diff_cave())),
             (RECV_CAVE, RECV_CAVE + len(_recv_cave()))]
    spans.sort()
    for (s0, e0), (s1, e1) in zip(spans, spans[1:]):
        assert e0 <= s1, "cave overlap: %#x..%#x vs %#x..%#x" % (s0, e0, s1, e1)
    assert spans[-1][1] <= CAVE_END
    # group 1's cave is a different dead function; prove it doesn't touch 0x48cfc..0x4905e.
    assert not (0x48cfc <= CAVE_AT < 0x4905e)
    # group 4 is a 4-byte in-place edit in live code, away from every cave and call site above.
    for a, e in [(STUB_AT, CAVE_END), (CAVE_AT, CAVE_AT + 80), (DIFF_SITE, DIFF_SITE + 10),
                 (RECV_SITE, RECV_SITE + 6)] + [(v, v + 4) for v in DIV_SITES] + [(PRE_AT, PRE_AT + 4),
                                                                                    (POST_AT, POST_AT + 4)]:
        assert not (a < NTIME_CALL + 4 and NTIME_CALL < e)
        assert not (a < B2_CALL + 4 and B2_CALL < e)


_assert_disjoint()


def patch(data):
    """Apply all four groups. md5-gated; each group checks its own original bytes."""
    if hashlib.md5(data).hexdigest() != STOCK_MD5:
        raise SystemExit("intminer: not the stock SC Box II 2.2.1 intminer: not patched")
    out = patch_ntime(patch_poolguards(patch_divzero(patch_chiphook(data))))
    return out


def main():
    src, dst = sys.argv[1], sys.argv[2]
    out = patch(open(src, "rb").read())
    open(dst, "wb").write(out)
    print("patched %s  md5 %s  sha256 %s" % (dst, hashlib.md5(out).hexdigest(), hashlib.sha256(out).hexdigest()))


if __name__ == "__main__":
    main()
