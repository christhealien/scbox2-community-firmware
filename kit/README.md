# SC Box II firmware with the scbox2 panel

Builds a flashable SC Box II firmware (`.ius` for the in-panel update and an SD-card `.img`) from
the OEM's stock SC Box II 2.2.1 `firmware.ius`, with the scbox2 panel built in and the OEM's extras removed.

## 1. Build the panel

```
tools/build-agent-box2.sh ../agent out/scbox2-agent-public.arm   # the public release (TAGS defaults to scbox2,public)
```

- `TAGS` must include `scbox2` (it selects `brand_scbox2.go`: the Box II's paths and names). The default,
  `scbox2,public`, is the public release (`variant_public.go`): the features that passed their hardware tests
  always on, no per-unit switches; the clock limit unlocks per unit only up to that unit's saved clean probe result
  (750 MHz at most). Other tag sets are not published.
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

### 1c. Build the settings-partition check

```
tools/build-fatfix-box2.sh ../agent out/scbox2-fatfix.arm
```

`agent/cmd/scbox2-fatfix`, run by `/etc/init.d/S38initdir` on `/dev/spiblock2` **before** `/mnt/config` is mounted
(output in `/tmp/scbox2-fatfix.log`). The image has no `fsck.fat`, and after a power cut the 3 MiB FAT32 can come up
dirty with lost clusters; the kernel then remounts it read-only the first time it trips over them, and no setting
can be saved until a reboot. The check reads the boot sector (only a FAT32 layout it fully understands; anything
else is left alone), every FAT copy and every directory, then:

- **repairs**, only when the volume is dirty or its FSInfo sector is invalid and nothing worse is found: frees lost
  cluster chains (allocated but reached by no entry) in every FAT copy, writes a valid FSInfo (signatures, true free
  count, next-free hint), clears the dirty flag (last) and keeps the backup boot sector equal to the boot sector.
  Changed sectors only, a few writes, read back and compared. The volume label is left alone;
- **refuses** (changes nothing, exit 1, reason in `/tmp/scbox2-cfg-repair` for the panel's banner) on cross-linked
  chains, chains that leave the cluster range, loops, chains running into a free or bad cluster, FAT copies that
  disagree on a file, sizes that don't match their chains, malformed directory entries, or a lost cluster that
  leads into a file. The mount goes ahead either way (`errors=remount-ro` still protects the partition).

Standard library and `internal/cfgwin` only (the script refuses anything else), static ARMv7, about 1.8 MiB;
reproducible like the others. It is also the boot scripts' way to save a setting (`scbox2-fatfix commit REL...`,
wrapped as `/usr/libexec/scbox2/cfgwrite`, see below).

## Settings partition: RAM copy, read-only flash, write window (v0.5.2)

The settings live on a 3 MiB FAT on raw NOR (`/dev/spiblock2`). No journal, a rename-over that isn't atomic
against a power cut, and the flash rewrites a whole 64 KiB erase block for any change. The OEM's backend
(`minerd`) handles `PUT /mcb/setting` this way: it builds the fan/clock command, saves `cpbsetting.json` and then
`bfgminer.json`, and only after that hands the command to the miner. If a save fails it gives up. So every
fan-curve step the panel sends rewrote a file on that flash, and a power cut during normal running could damage
the FAT. On a read-only FAT the fans froze (seen at 83 C).

**Mechanism.** `/usr/libexec/scbox2/cfgmount` (run by `S38initdir`) checks the partition with `scbox2-fatfix`,
mounts it **read-only** at `/mnt/cfgflash`, mounts a tmpfs (8 MiB cap) at `/mnt/config`, copies the partition
into it and compares the copy (`diff -r`). Every program keeps using `/mnt/config` (`/usr/config` links there),
so all of them work on RAM without any path change: the OEM's binaries, the panel, the SSH server and the
scripts. Why not patch the OEM's path strings, or bind-mount tmpfs over single folders? A path patch would cover
only the paths we found, in the binaries we patched. Anything missed (`miner_history/history`, `login.json`, a
dot-temp file) would still write the flash. Over-mounting the whole folder covers every writer, including ones we
don't know about. Nothing on the flash is held open, so remounting it read-only never fails with "busy". And the
partition's layout stays exactly the OEM's, so going back to the stock firmware from an SD card still works.

**Write window.** What must survive a reboot is copied from RAM to the partition by `internal/cfgwin`: take
`flock /tmp/scbox2-cfgwin.lock`, so only one window is open at a time across the panel, `scbox2-sshd` and the
scripts. Then remount `/mnt/cfgflash` read-write and, for each file: write `name.tmp` with fsync, copy the current
file to `name.bak` with fsync, rename, fsync the folder. A file gone from RAM is removed together with its `.bak`.
Then `sync` and remount read-only (retried for 2 s; if that still fails it is logged loudly). Files whose bytes
are already the same are not written, and no window opens at all when nothing differs. If the read-write remount
fails, the save fails as "read-only" and the Dashboard's settings-storage banner says so. With no RAM copy (the
fallback below), the window does nothing, because the files were written in place.

**Fallbacks.** If the RAM copy can't be made or doesn't match, the partition is mounted read-write at
`/mnt/config` as before v0.5.2, and `/tmp/scbox2-cfg-direct` says why. If the partition can't be mounted at all,
`/mnt/config` is an empty tmpfs: the programs start from their defaults, nothing is saved, and
`/tmp/scbox2-cfg-broken` says so.

**Who saves what.**
- **Panel settings** are committed when written (`writeAtomic`/`cfgWrite`/`saveAuth`, then `cfgCommitWritten`).
  Every change the owner makes in the panel (an authenticated request that isn't a GET) runs inside
  `cfgUserSave`. It commits whatever that request wrote or removed in the panel's folder, and the OEM's files if
  the request reached the OEM's backend (a PUT answered 200) or edited them (the verbose switch). The OEM's
  plan files (`cpbsetting.json`, `bfgminer.json`) are compared with the fan levels taken out
  (`N RPM` / `vfff=v:f:FAN:FAN`). A change of the fans alone is never saved. `cpbsetting.json` is saved only
  when the request wrote the plan (`PUT /mcb/setting`), so a throttle step made at the same time isn't kept as
  the owner's clock.
- **Hot panel files** (hashrate history, events, best shares, the throttle's saved clock, chip-error counters,
  update notices, the schedule's "plan before", the tuner's run folder, incidents) stay in RAM. They are saved
  when an owner's change touches them, when a tuning run ends, and at every reboot the panel makes. That
  checkpoint is in `markCleanReboot`, and it also runs on SIGTERM (`S48scbox2 stop` at shutdown).
- **Safety holds** (the overheat Idle, the pool guard's hold, the fan lock, the raised fan floor, the soft-Idle
  marker) are written when a guard trips and removed when it is lifted. They are saved both times, so they
  survive a power cut as before. This only happens when a guard trips, never in normal running.
- **Keep the system log** (Settings -> Recovery, off by default) is the one feature that writes while the miner
  runs: its "last seen" note every 5 minutes and the log tail every 10. Writing those is what the feature is for.

### What writes under /mnt/config at run time (inventory)

From the image's scripts, the OEM's binaries (strings, disassembly, and `minerd` traced under qemu) and the panel
source. "Before" is v0.5.1 (every write went to the flash). "Now" is v0.5.2.

| Writer | File(s) under `/mnt/config` | When | Now |
|---|---|---|---|
| OEM backend `minerd` | `minerd/settings/cpbsetting.json` (via `.cpbsetting.json`) | at **every start**, and on every `PUT /mcb/setting`: each fan-curve step, the abort tier's 100 %, the throttle, plan/Idle changes | RAM; flash only when the owner's change wrote a plan whose clock/V/mode differ (not the fans) |
| `minerd` | `bfgminer/bfgminer.json`, `bfgminer/temp_bfgminer.json` | at every start (regenerated from the plan), every `PUT /mcb/setting` (the plan line carries the fans), pool changes | RAM; flash on the owner's pool/plan change (fans ignored) |
| `minerd` | `minerd/settings/pools.json`, `miner_history/history` (appended: pool add/delete/priority, algorithm) | pool changes (`/mcb/pools`, `newpool`, `delpool`) | RAM; flash right after the owner's change |
| `minerd` | `minerd/settings/ip.json`, `dns.json`, `interfaces`, `resolv.conf`, `etc/network/interfaces` | the OEM's network settings (`/mcb/ipsettings`) | RAM; flash after an owner's change that reached the backend |
| `minerd` | `minerd/settings/algosetting.json`, `login.json`, `cmos.json`, `rgbcontrol.json`, `cpbcontrol.json`, `tutorial.json`, `wifi.json` | their own `PUT`s (algorithm, the backend's password, board on/off, ...) | RAM; flash after the owner's change |
| `minerd` | `minerd/settings/*` from `/root/settings`, `bfgminer/bfgminer.json` from `/root` | first start on an empty partition | RAM (made again at each boot until an owner's save; same content) |
| stock `S38initdir` step (now in `cfgmount`) | `etc/network/interfaces` from `/root/interfaces` | first boot on an empty partition | window, once |
| `fanctrl`, `xminerd`, `eventhub` | none (they read `cpbsetting.json`, `algosetting.json`, `bfgminer.json`; logs in `/dev/shm`, pid in `/var/run`) | | |
| the miner (`intminer`/`bfgminer`) | none (`-c bfgminer/bfgminer.json` read; its API's `save` is never called, and write access is 127.0.0.1 only) | | |
| `wifi` | `wifi/*` | never: not started (its inittab line is removed) | |
| `dhcpcd` | none: leases would go to `/var/db` and the DUID to `/etc`, both on the read-only root | | |
| `scbox2-seed` | `random-seed` | boot (`load` saves a fresh one at once) and shutdown (`S20urandom stop`); v0.5.1 also every 6 hours | window, at boot and at shutdown only |
| `scbox2-mac` | `scbox2/mac` | first boot in "keep" mode | window, once |
| `S48scbox2` | `bfgminer/bfgminer.json` (API access fix, once), `minerd/settings/qomo.json` removed (once), `sclite` -> `scbox2` move (once), `scbox2/localtime` (default copy) | boot | window for the one-time fixes; `localtime` stays in RAM until a zone is chosen |
| `scbox2-ntp`, `40-scbox2-ntp`, `miner-run` | none (`/tmp` only) | | |
| `scbox2-sshd` | `scbox2/auth.json` (factory login if the panel never started; the first password change over SSH), `scbox2/ssh_host_ed25519_key` (first start) | | window |
| panel | settings (`auth.json`, `fan.json`, `net.json`, `presets.json`, `notify.json`, `schedule.json`, power, MAC, NTP, time zone, TLS key/cert, ...) | the owner's saves; first start (login, TLS key) | window |
| panel | safety holds (`heat-idle.json`, `pool-hold.json`, `fan-lock.json`, `fan-guard.json`, `softidle.json`), chip trims and the runs that change them | a guard trips / is lifted; an owner's run | window |
| panel | hot files (see above) | while running | RAM; saved with an owner's change that touches them, at the end of a tuning run, and at a reboot the panel makes |
| panel | `last-alive.json`, `syslog-kept.txt` | every 5 / 10 min, **only** with Settings -> Recovery -> Keep the system log | window (that feature's purpose) |

First boot of v0.5.2 needs no migration: the partition keeps its files and layout. The first boot copies them
to RAM, and anything saved after that is written in a window.

## 2. Build the firmware

```
export SOURCE_DATE_EPOCH=$(date -u -d 2026-11-12 +%s)     # the release date (see Reproducibility)
python3 build_box2.py --firmware firmware.ius --agent out/scbox2-agent-public.arm --sshd out/scbox2-sshd.arm \
    --fatfix out/scbox2-fatfix.arm --version 0.5.0 \
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
| `--fatfix` | the settings-partition check from step 1c (ARM ELF, at most 3 MiB, must carry its version string) |
| `--version` | **required**. Must match `^v?[0-9][0-9A-Za-z.-]*$` (the panel's update-asset pattern, `updimage.go` `iusAssetRe`), e.g. `0.5.0`, `0.5.0-rc1`. A leading `v` is dropped. Written to `/etc/scbox2-release` |
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
   202.120.2.101, 202.112.31.197, asia.pool.ntp.org, and the OEM cloud's two hosts: its MQTT hub and its miner finder), and for any file
   name or content mentioning the SC Lite (`sclite`, `sc lite`, `sc-lite`, `sc_lite`, any case). Explicit
   exemptions: `/usr/bin/scbox2-agent` may contain 114.114.114.114 (its DNS policy refuses that resolver) and
   SC Lite names (its model table and the SC Lite's update channel, off on the Box II);
   `/etc/init.d/S48scbox2` may mention `sclite` once (the one-time move from `/mnt/config/sclite`). A system
   item that can't be decompressed is a failure, never a silent raw-grep-only pass. `/usr/bin/scbox2-sshd`,
   `/etc/init.d/S47scbox2-sshd`, `/usr/bin/scbox2-fatfix`, `/usr/libexec/scbox2/cfgmount` and
   `/usr/libexec/scbox2/cfgwrite` must be in the image (and so are scanned).
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
- runs the settings-partition check first (`/usr/bin/scbox2-fatfix`, as S38initdir does) on two small FAT32 test
  images made by the build: a power-cut one (dirty, FSInfo unwritten, a lost chain) must be repaired and read back
  clean (and a second run must find nothing to do), a cross-linked one must be refused with exit 1, nothing
  changed and the marker written;
- starts the SSH server next (`/usr/bin/scbox2-sshd`, settings in the same temp dir): port 22 must answer with
  `SSH-2.0-scbox2` and offer a key exchange;
- must serve the panel page, still be running 2 s later, print no Go `fatal error`/`panic:`, and have nothing
  listening on 22/80/443, except the SSH server's port 22 and the panel's built-in net-check responder on
  127.0.0.2:80 (no flag for it; loopback-only, and inside the run's own network namespace);
- then replaces the panel binary with a crashing stub (a bind mount, inside the run) and starts the SSH server
  afresh: port 22 must still answer SSH.

Then, once, **the settings in RAM** (`shadow_check`): the image's own boot step, the OEM's backend and the panel
run together under qemu-arm. They run in a user namespace with its own `binfmt_misc` (nothing is registered on
the build host), so the shell scripts and the OEM's programs can start other ARM programs as they do on the
miner. The interpreter is registered by the bare name `eventhub`, because `minerd` won't start unless a process
with that argv[0] runs. The partition is a tmpfs standing in for `/dev/spiblock2`, holding a unit's files: the
build host has no FAT driver, and a user namespace can't mount a block device. It must hold that:

- after `/usr/libexec/scbox2/cfgmount` (as `S38initdir` runs it) the partition is mounted read-only, `/mnt/config`
  is a tmpfs holding the partition's files, and the first-boot save (`etc/network/interfaces`) landed;
- the panel's first login file reaches the partition (the panel's settings go through `/usr/config`);
- five fan changes sent to the image's own `minerd` (`PUT /mcb/setting`, as the fan curve sends them) all answer
  200 and land in the RAM copy of `cpbsetting.json`. The partition stays the same byte for byte (sha256 over
  every file), no write window is logged, and it is still read-only;
- with its settings folder made read-only, the patched `minerd` still answers 200 to a plan (the patch that keeps
  a failed save from dropping the plan);
- a panel save (the first password change, through `/api/login` and `/api/passwd`) logs a window
  (`config window: rw, ... synced, ro`), the new `auth.json` is on the partition, and the partition is read-only
  again.

This needs Linux 6.7+ (binfmt_misc in a user namespace) and `openssl` on the host. Without them this part is
skipped with a warning, and `--require-startup-check` turns that into a failure.

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

1. `TAGS=scbox2,public tools/build-agent-box2.sh <agent> <out>`, `tools/build-sshd-box2.sh <agent> <out>` and
   `tools/build-fatfix-box2.sh <agent> <out>` (go1.24.7).
2. `SOURCE_DATE_EPOCH=<release date> python3 build_box2.py ... --version X.Y.Z --out
   rel/scbox2-firmware-vX.Y.Z.ius --img rel/scbox2-sd-vX.Y.Z.img --xz --sums --require-startup-check`;
   the last line must read `gates: ... startup OK (5 runs)`.
3. Build it a second time in another folder and `cmp` the `.ius`, `.img`, `.img.xz`.
4. Publish `scbox2-firmware-vX.Y.Z.ius`, `scbox2-sd-vX.Y.Z.img.xz` and `SHA256SUMS` (the panel's update
   downloads `SHA256SUMS` and matches the `.ius` by name).
