# Building from source

The firmware is built in two parts:

1. **The panel** (`scbox2-agent`): a single static Go binary for ARMv7, from `agent/`.
2. **The firmware image**: `kit/build_box2.py` takes the stock SC Box II firmware, puts the panel and the
   community changes into its system filesystem, and writes the panel's update file (`.ius`) and an SD-card image.

## You need your own stock firmware

This repository does **not** contain the OEM firmware. You supply your own copy of the **stock SC Box II
firmware `.ius`**. The build was checked against stock **2.2.1**; it recognises that version by the MD5 of its
system item (`063494ba70fe9e923b96ea107070324f`). Its binary patches are tied to those bytes: on a version it
doesn't recognise, the build says so, and every patch checks the bytes it replaces before it writes.

The build replaces **only the system item** inside the container. The bootloader, kernel and other items stay
byte-for-byte as in your stock file, and the build checks this before it finishes.

## Requirements

| What | Version / notes |
|---|---|
| Go | 1.24 (`go.mod`: `go 1.24.0`). Releases are built with **go1.24.7**. Modules are vendored: no downloads. |
| Python | 3 |
| squashfs-tools | `mksquashfs` and `unsquashfs` with gzip and `-b 131072`. Releases are built with **4.7.5**. Looked for in `$MKSQUASHFS` / `$UNSQUASHFS`, then on `PATH`, then `/tmp/sqt/squashfs-tools/`, then `kit/tools/`. |
| root | `unsquashfs` / `mksquashfs` must keep owners, modes and device nodes |
| qemu-arm-static | Optional: for the panel start-up check (`QEMU_ARM=path` to choose one) |
| clang / ld.lld / llvm-strip 18.1.3 | Optional: only to rebuild the small ARM helpers (see below) |

## 1. Build the panel

From the repository root:

```sh
mkdir -p out
kit/tools/build-agent-box2.sh agent out/scbox2-agent
```

This is the build the releases use: with no `TAGS` set the script builds the public release (`TAGS=scbox2,public`,
the default), so the command above and `TAGS=scbox2,public kit/tools/build-agent-box2.sh agent out/scbox2-agent`
give the same binary. The script copies `agent/` to a scratch directory, names the Go module
`scbox2-agent` there (the source tree isn't changed), builds, and prints the first 16 hex digits of the binary's SHA-256 and the Go version.
It runs:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 GOFLAGS=-mod=vendor GOTOOLCHAIN=local \
  go build -tags "scbox2,public" -trimpath -buildvcs=false -ldflags="-s -w" -o scbox2-agent .
```

| Tag | What it selects |
|---|---|
| `scbox2` | The SC Box II names and paths (`brand_scbox2.go`). Required: the script refuses tags without it. |
| `public` | The public release: the features described in the [README](../README.md) always on, the rest off, no per-unit switches. |

> [!IMPORTANT]
> `TAGS` defaults to `scbox2,public`, the public release build: leave it unset to reproduce a release. Any other tag
> set is not published and not described in these docs.

`-trimpath`, `-buildvcs=false`, vendored modules and `GOTOOLCHAIN=local` keep the binary reproducible.

`agent/go.mod` says `module sclite-agent` on purpose: the panel source is shared with the SC Lite firmware, and the
script renames the module to `scbox2-agent` in its scratch copy. Running `go build` by hand in `agent/` also works,
but it **needs `-tags scbox2,public`** (without `scbox2` it builds the SC Lite panel), and the binary won't be
byte-identical to a release's, because the module name is part of it.

### The SSH server

SSH is its own small program (`agent/cmd/scbox2-sshd`), so SSH keeps working if the panel can't start. Build it
from the repository root the same way:

```sh
kit/tools/build-sshd-box2.sh agent out/scbox2-sshd
```

The script refuses a binary that links anything of the panel beyond `internal/sshcore`, `internal/authfile` and
`internal/cfgwin` (the settings write window).
The same SSH server goes with either panel build.

### The settings-partition check

`agent/cmd/scbox2-fatfix` checks the config partition at every boot, before it is mounted, and repairs what a
power cut can safely leave behind (lost clusters, an invalid FSInfo sector, the dirty flag). On anything riskier it
changes nothing and the panel shows what it found. It is also how the boot scripts save a setting onto the
partition, which is read-only while the miner runs (`scbox2-fatfix commit`, via `/usr/libexec/scbox2/cfgwrite`).
It uses the Go standard library and `internal/cfgwin` only:

```sh
kit/tools/build-fatfix-box2.sh agent out/scbox2-fatfix
```

## 2. Build the firmware

From `kit/`, as root:

```sh
SOURCE_DATE_EPOCH=<the release's> python3 build_box2.py \
  --firmware /path/to/your/stock-scbox2-2.2.1.ius \
  --agent    /path/to/scbox2-agent \
  --sshd     /path/to/scbox2-sshd \
  --fatfix   /path/to/scbox2-fatfix \
  --out      out/scbox2-firmware-v0.5.6.ius \
  --img      out/scbox2-sd-v0.5.6.img \
  --version  0.5.6 --xz --sums --require-startup-check
```

| Option | Meaning |
|---|---|
| `--firmware` | Your stock SC Box II `.ius` (required) |
| `--agent` | The ARM panel binary from step 1 (required) |
| `--sshd` | The SSH server binary from step 1 (required) |
| `--fatfix` | The settings-partition check from step 1 (required) |
| `--out` | The update file for the panel (required). Publish it as `scbox2-firmware-vX.Y.Z.ius`: the panel looks for that name on GitHub and takes the version from it. |
| `--img` | The SD-card image (the container placed at 16 MiB) |
| `--version` | Written into `/etc/scbox2-release` on the miner |
| `--card-size` | SD image size in MiB (default: the smallest 1 MiB-aligned size) |
| `--xz` | Also write `IMG.xz` with fixed xz settings (`-6 --check=crc64 --block-size=25165824 -T2`), so it is byte-reproducible too, and check it decompresses to the image |
| `--sums` | Write `SHA256SUMS` (the `.ius` and, with `--xz`, the `.img.xz`) next to `--out` |
| `--require-startup-check` | Fail instead of skipping when the start-up check can't run (releases use it) |
| `--skip-startup-check` | Skip the qemu start-up check (not for releases) |
| `--keep-work` | Keep the work tree for debugging |
| `--check IMAGE` | Only run the leak check on a built image |

With `--xz --sums` the build writes the compressed SD image and `SHA256SUMS` itself. (A plain `xz -k` of the
image works for flashing, but its bytes depend on the xz version and settings, so it won't match a release's
`.img.xz`.)

## What the build does

1. Opens the stock `.ius` and extracts the system item (a squashfs).
2. **Round-trip check:** repacks the unmodified system item and checks the result is byte-for-byte the stock
   `.ius`.
3. Unpacks the system squashfs as root.
4. Adds the panel and helpers, installs the miner wrapper and patched miner copy, writes `/etc/scbox2-release`,
   applies the binary patches, edits the init scripts, and removes what this firmware drops (OEM cloud,
   Wi-Fi stack, stock web UI and others; see [DIFFERENCES.md](DIFFERENCES.md)).
5. Rebuilds the squashfs with the stock recipe: `-comp gzip -b 131072 -no-xattrs -noappend`, plus
   `-mkfs-time` / `-all-time` (the build time below), `-no-tailends` and `-processors 1` so the bytes don't depend
   on the build host.
6. **Guards:** refuses an image larger than the 18,874,368-byte system partition, not gzip, or with xattrs, and
   keeps at least 1 MiB free.
7. Repacks the container with only the system item changed, and writes the SD image.
8. **Brick-safety check:** every byte before the system item matches stock, apart from its size word and the
   header CRC; the OEM's own update checks pass.
9. **Start-up check** (optional): under qemu-arm, runs the settings-partition check on two test images (a
   power-cut one must be repaired, a cross-linked one left untouched), starts the SSH server and the panel, and
   checks the panel serves its page. Then, once, it boots the settings the way the miner does. The image's own
   boot step, the OEM's backend and the panel run together, and the check proves four things: the partition is
   read-only, fan changes stay in RAM and leave the partition byte for byte the same, the patched backend still
   takes a plan when its settings can't be saved, and a panel save writes in a short window and leaves the
   partition read-only. That part needs Linux 6.7+ (`binfmt_misc` in a user namespace; nothing is registered on
   the host) and `openssl`. Without qemu, the check is skipped with a one-line note.
10. **Leak check** on the SD image: no OEM cloud host, hardcoded DNS or NTP host is left in what the miner
    runs (it unpacks the squashfs to search inside compressed binaries).

> [!WARNING]
> **The start-up check runs the ARM panel on your build host, as root** (the build runs as root). It uses the
> panel's default paths: it writes `/usr/config/scbox2/*` (a login file, a certificate, an SSH host key) and
> `/tmp/scbox2-*`, and may try to open ports 80, 443 and 22. Run the build in a container or VM, or pass
> `--skip-startup-check`.

## Reproducible builds

The public build reproduces: the same source, the same stock firmware and the same tool versions give the same
panel binary and the same `.ius`, `.img` and `.img.xz`, byte for byte.

- **The panel and its helpers:** the default `TAGS` (the public build), `-trimpath`, `-buildvcs=false`, vendored
  modules and `GOTOOLCHAIN=local`, with **go1.24.7**. The build scripts print each binary's SHA-256 prefix and the
  Go version.
- **The image:** file times and the squashfs creation time are pinned to `SOURCE_DATE_EPOCH` (without it, the
  pinned default `1761955200`, 2025-11-01 00:00 UTC; releases set it to their release date, e.g.
  `SOURCE_DATE_EPOCH=$(date -u -d 2026-11-13 +%s)`), with **squashfs-tools 4.7.5**; `--xz` uses fixed xz settings.
  `/etc/scbox2-release` on the miner shows the build time, so you can read off the release's `SOURCE_DATE_EPOCH`.

To check a release, build all three binaries and the image with the same inputs and compare against its
`SHA256SUMS`.

## The ARM helpers

Three small ARM helpers come prebuilt in `kit/files/*.bin`:

| Helper | What it does |
|---|---|
| `scbox2_bind.so` | Turns the stock daemons' TCP listeners into loopback-only ones (the kernel has no firewall) |
| `scbox2_wrap.bin` | The miner wrapper: one miner at a time, config handling, `--no-pool-redirect` |
| `scbox2_chip.so` | The per-chip clock trim library |

To rebuild them from `kit/files/*.c`:

```sh
kit/tools/build-helpers-box2.sh STOCK_LIB_DIR OUT_DIR [--write]
```

`STOCK_LIB_DIR` holds `libuClibc-0.9.33.2.so` and `libpthread-0.9.33.2.so` from your stock firmware's `/lib`
(linked against only, nothing is copied in). Byte-identical output needs clang, lld and llvm-strip **18.1.3**;
the script refuses other versions.
