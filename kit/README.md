# SC Box II firmware with the scbox2 panel

Builds a flashable SC Box II firmware (`.ius` for the in-panel update and an SD-card `.img`) from
the stock SC Box II 2.2.1 `firmware.ius`, with the scbox2 panel built in and the OEM's extras removed.
(`README.md` / `build.py` in this folder are the **SC Lite** build: a different miner, a different pipeline.)

## 1. Build the panel

```
TAGS=scbox2,public tools/build-agent-box2.sh ../agent out/scbox2-agent-public.arm   # public release
tools/build-agent-box2.sh ../agent out/scbox2-agent-owner.arm                       # owner's build
```

- `TAGS` must include `scbox2` (it selects `brand_scbox2.go`: the Box II's paths and names).
  - `scbox2,public`: the public release (`variant_public.go`): no Commissioning, features that passed their
    hardware tests always on, no overclock ceiling.
  - `scbox2` (default): the owner's commissioning build (`variant_owner.go`): features are switched on per unit
    in Settings -> Commissioning; 725 MHz clock ceiling for the C6/C7 tests.
- The script builds in a scratch copy (module renamed `scbox2-agent`), never modifying the source tree.
  Reproducible: `-trimpath`, `-buildvcs=false`, vendored modules, `GOTOOLCHAIN=local`. Built and checked
  with **go1.24.7**: two builds from the same source give the same binary.

### 1b. Build the SSH server

```
tools/build-sshd-box2.sh ../agent out/scbox2-sshd.arm
```

SSH is its own program (`agent/cmd/scbox2-sshd`, started by `/etc/init.d/S47scbox2-sshd` before the panel), so a
damaged or crashing panel binary can't take SSH with it. It signs in with the panel's account and follows the
panel's SSH settings. The script builds it like the panel and refuses it if it links anything of the panel beyond
`internal/sshcore` and `internal/authfile`. The same binary goes with either panel build.

## 2. Build the firmware

```
export SOURCE_DATE_EPOCH=$(date -u -d 2026-11-12 +%s)     # the release date (see Reproducibility)
python3 build_box2.py --firmware firmware.ius --agent out/scbox2-agent-public.arm --sshd out/scbox2-sshd.arm --version 0.5.0 \
    --out rel/scbox2-firmware-v0.5.0.ius --img rel/scbox2-sd-v0.5.0.img --xz --sums --require-startup-check
```

Run as root (unsquashfs/mksquashfs must keep owners and device nodes; the startup check uses namespaces).
Needs python3, squashfs-tools that do gzip with `-b 131072` (`$MKSQUASHFS`/`$UNSQUASHFS`, else `PATH`, else
`/tmp/sqt/squashfs-tools`), a static qemu-arm (`qemu-arm-static` or `QEMU_ARM=path`), `unshare`, `chroot`, and
`xz` for `--xz`.

| option | |
|---|---|
| `--firmware` | stock SC Box II `firmware.ius`. Only the known 2.2.1 system item (md5 `063494ba…324f`) is accepted |
| `--allow-unknown` | build from another stock firmware anyway (testing only: the patches were checked against 2.2.1) |
| `--agent` | the panel binary from step 1 (ARM ELF) |
| `--sshd` | the SSH server binary from step 1b (ARM ELF, at most 6 MiB) |
| `--version` | **required**. Must match `^v?[0-9][0-9A-Za-z.-]*$` (the panel's update-asset pattern, `updimage.go` `iusAssetRe`), e.g. `0.5.0`, `0.5.0-owner`. A leading `v` is dropped. Written to `/etc/scbox2-release` |
| `--out` | the `.ius`. For a release name it `scbox2-firmware-v<version>.ius` (the asset name the panel's update looks for; `--sums` insists) |
| `--img` | the SD burn image (the container at 16 MiB) |
| `--xz` | also write `<img>.xz` with fixed, byte-reproducible settings (needs `--img`) |
| `--sums` | write `SHA256SUMS` next to `--out`: `<sha256>  <name>` lines for the `.ius` and (with `--xz`) the `.img.xz` |
| `--require-startup-check` | fail instead of skipping when the startup check can't run. **Use it for releases** |
| `--skip-startup-check` | don't start the panel (not for releases; refused together with `--require-startup-check`) |
| `--patches-dir` | where the `patch_*_box2.py` modules are (default: this folder) |
| `--keep-work` | keep the work dir (`$BOX2_WORK` or the system temp dir) |
| `--check IMAGE` | run only the leak/naming gate on a built `.ius` or `.img`; exit 1 on any hit |

## Pipeline and gates

The system partition is a read-only gzip squashfs: the build unpacks it, transforms the tree (panel and
helpers, intminer wrapper and hook copy, binary patches, init edits, removals), rebuilds it with the stock
recipe (gzip, 128 KiB blocks, no xattrs, one processor, fixed times) and puts it back into the container;
nothing else in the container changes.

Every gate stops the build with a non-zero exit. The outputs are built in the work dir and moved to `--out`,
`--img`, `<img>.xz` and `SHA256SUMS` only after every gate has passed; outputs left at those paths by an earlier
build are deleted when a build starts, so a failed build leaves nothing behind (it ends with
`BUILD FAILED: no output written`). Argument errors (e.g. a bad `--version`) stop before anything is touched.

1. **Known firmware:** the system item's md5 must be the checked 2.2.1 one (unless `--allow-unknown`).
2. **Byte-safe container:** repacking the *unmodified* system item must reproduce the stock `.ius` exactly.
3. **Image guards:** gzip, no xattrs, fits the 18,874,368-byte partition with >= 1 MiB to spare.
4. **Brick safety:** the new container equals stock everywhere before the system item except its size word
   and the header CRC.
5. **Read-back:** the final `.ius`'s system item must be the squashfs that was built; it is unsquashfs-ed and
   compared with the packed tree entry by entry: type, sha256 of every file, mode, owner (uid/gid), symlink
   target, device numbers; nothing missing, nothing extra. The `.img` must carry the `.ius` byte for byte at
   16 MiB.
6. **Stock checks:** the OEM's own `upgrade`/uboot0 container checks (`tools/ius.py verify`).
7. **Leak/naming gate** (`check_image`), on the `.ius` **and** the `.img`: a raw grep plus a grep of every file
   in the decompressed system item for the leak hosts (114.114.114.114, www.baidu.com, www.google.com,
   202.120.2.101, 202.112.31.197, asia.pool.ntp.org, and the OEM's cloud and discovery hosts), and for any file
   name or content mentioning the SC Lite (`sclite`, `sc lite`, `sc-lite`, `sc_lite`, any case). Explicit
   exemptions: `/usr/bin/scbox2-agent` may contain 114.114.114.114 (its DNS policy refuses that resolver) and
   SC Lite names (its model table and the SC Lite's update channel, off on the Box II);
   `/etc/init.d/S48scbox2` may mention `sclite` once (the one-time move from `/mnt/config/sclite`). A system
   item that can't be decompressed is a failure, never a silent raw-grep-only pass.
8. **Startup check:** see below.
9. **xz read-back** (with `--xz`): the `.img.xz` must decompress to the `.img`.

## Startup check (isolation)

The panel **from the image** (the read-back tree of the final `.ius`, so its own `/usr/bin/scbox2-agent`) is
started 5 times under qemu-arm. Each run:

- runs in its own mount, network and PID namespaces (`unshare -m -n -p -f`): only a private loopback exists,
  nothing it opens is reachable from or bound on the build host, and it and its children end with the run;
- is chrooted in the image tree, with fresh tmpfs on `/tmp`, `/mnt` (the target of `/usr/config ->
  /mnt/config`) and `/dev` (only null, zero, random, urandom bound in), so nothing is written to the host's
  `/usr/config` or anywhere else on the host;
- gets its settings in a temp dir (`-authfile`, `-fan`, `-presets`, `-hashrate`, `-netcfg`, `-tunerdir`,
  `-bfg`, `-backup` under `/tmp/box2check-cfg`), `-ssh=false -lan80=false -https443=false`, `-log /dev/null`,
  and listens on a free high port on 127.0.0.1;
- starts the SSH server first (`/usr/bin/scbox2-sshd`, settings in the same temp dir): port 22 must answer with
  `SSH-2.0-scbox2` and offer a key exchange;
- must serve the panel page, still be running 2 s later, print no Go `fatal error`/`panic:`, and have nothing
  listening on 22/80/443, except the SSH server's port 22 and the panel's built-in net-check responder on
  127.0.0.2:80 (no flag for it; loopback-only, and inside the run's own network namespace);
- then replaces the panel binary with a crashing stub (a bind mount, inside the run) and starts the SSH server
  afresh: port 22 must still answer SSH.

Without root, `unshare`, `chroot` or a static qemu-arm the check is skipped with a banner warning and the build
summary says `startup SKIPPED (!)`; `--require-startup-check` turns that into a failure.

## Reproducibility

The same stock firmware, panel binary, kit, `--version` and `SOURCE_DATE_EPOCH` give the same `.ius`, `.img`
and `.img.xz` byte for byte (checked: two public 0.5.0 builds, all three identical). Pinned:

- **Panel:** go1.24.7 via `tools/build-agent-box2.sh` (see step 1).
- **squashfs-tools 4.7.5** (`mksquashfs -comp gzip -b 131072 -no-xattrs -noappend -no-tailends -processors 1
  -mkfs-time T -all-time T`). Another version may pack differently.
- **Time:** `T` = `SOURCE_DATE_EPOCH` when set, else the pinned default 1761955200 (2025-11-01T00:00Z). It is the
  squashfs time, every file's time and the `built:` line of `/etc/scbox2-release` (shown in Settings ->
  About). **Releases set `SOURCE_DATE_EPOCH` to the release date** (`date -u -d YYYY-MM-DD +%s`); without it
  the image says it was built on 2025-11-01. (The variable is hidden from mksquashfs itself, which refuses it
  together with the explicit time options.)
- **xz:** `xz -6 --check=crc64 --block-size=25165824 -T2` (XZ Utils 5.4.5). With an explicit block size the
  multi-threaded encoder gives the same bytes for any `-T` >= 2 (and `-T0` on a multi-core host; this is
  what reproduces the v0.4.4 `.img.xz`). `-T1` selects the single-threaded encoder, whose block headers differ:
  don't use it. Another XZ Utils version may encode differently; the `.img` (and its sha256) is unaffected.

## Release checklist

1. `TAGS=scbox2,public tools/build-agent-box2.sh <agent> <out>` and `tools/build-sshd-box2.sh <agent> <out>`
   (go1.24.7).
2. `SOURCE_DATE_EPOCH=<release date> python3 build_box2.py ... --version X.Y.Z --out
   rel/scbox2-firmware-vX.Y.Z.ius --img rel/scbox2-sd-vX.Y.Z.img --xz --sums --require-startup-check`;
   the last line must read `gates: ... startup OK (5 runs)`.
3. Build it a second time in another folder and `cmp` the `.ius`, `.img`, `.img.xz`.
4. Publish `scbox2-firmware-vX.Y.Z.ius`, `scbox2-sd-vX.Y.Z.img.xz` and `SHA256SUMS` (the panel's update
   downloads `SHA256SUMS` and matches the `.ius` by name).
