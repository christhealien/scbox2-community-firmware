# Changelog

All notable changes to the SC Box II Community Firmware. Newest first.

Versions before 0.5.0 were test builds, run on one SC Box II. In those builds every hardware control shipped off
and was switched on per unit after a hardware test (C1–C9). Those test codes appear in the history below.

## v0.5.0: first public release

The first public release. The hardware controls that passed their tests on real hardware now ship **on**, and
the per-unit commissioning switches and the dashboard's commissioning banner are gone from this build.

**On in this release**
- **Stop hashing / Idle** from the panel, with safety guards and scheduled Idle.
- **Power estimate** in watts and J/TH, calibrated with your own wall-meter readings per setting; the OEM's
  published figures for the two stock presets otherwise.
- **Clock control:** 525–690 MHz in 12.5 MHz steps.
- **Voltage control in volts:** 20 steps from 7.67 V (stock) down to 7.03 V, never above stock.
- **Per-chip clock trims and auto-trim.**
- **Firmware update from the panel** (below).

**Planned for a later release:** clock ceiling probe, overclock above 690 MHz, auto-tuner.

**Panel**
- Presets renamed **Stock High** (690 MHz) and **Stock Low** (525 MHz); no separate "Stock" card.
- Voltage is shown and chosen **in volts everywhere** (measured at the power lugs on one unit; "≈" marks an
  estimate). The OEM's step numbers never appear.
- Fan pair offset and the fan-check fitting are hidden on the SC Box II.

**Install safety, SSH and first start**
- **SSH is its own small program** (`scbox2-sshd`), started before the panel: if the panel ever can't start, SSH
  still works. Same account, local-network-only rule and wrong-password lockout; settings stay in the panel.
- **The install checks the chip itself:** after writing, every block is read back from the flash through a fresh
  handle with the caches dropped; mismatched blocks are rewritten once, and if they still differ the old system
  is written back and the miner doesn't reboot into a bad copy.
- **Progress bar** in Settings → Firmware update and on the dashboard while an install runs (each stage, elapsed
  time), and the result once the miner is back.
- **First start** (no settings yet): mines on **Stock High** with **auto fans on the balanced curve**.

**Firmware update** (Settings → Firmware update)
- Three steps: ① get the file (Check for updates, or upload) · ② automatic check, writes nothing: complete, made
  for the SC Box II, **genuine** (matches the release's `SHA256SUMS`, looked up on GitHub, also for an uploaded
  file; paste the line by hand when offline) · ③ install.
- Install needs **four confirmations** (settings saved · SD card ready · power stable · "if the install fails,
  the miner only comes back with the firmware on an SD card") and **the panel password**.
- **Download and install** in one go from the latest release, with the same confirmations and password.
- **How to recover with an SD card** shown under the confirmations. No automatic roll back on this model.
- **Settings backup** drop-down: **export and import** the panel's settings (network, fan curves, presets,
  schedule, notifications; secrets stripped). Pools, hostname and time settings aren't in the file.
- Update source: `christhealien/scbox2-community-firmware` by default; changing it needs the panel password.

**Security** ([docs/SECURITY.md](docs/SECURITY.md))
- **Local network only** by default for the panel and SSH, IPv4 and IPv6; switch: *Allow access from outside the
  local network* (off).
- New switches: **HTTPS only** (off), port **80** and port **443** on/off. Port 8080 serves HTTP and HTTPS.
- Changes are accepted only from the panel's own page. **Signing out** ends the session on the miner too.
- 5 wrong passwords block an address for 10 minutes, on the panel and on SSH (each keeps its own count).
- Keys made at first start wait for the kernel's random pool; each unit keeps its own random seed.
- Removed: the OEM's development leftovers (an NFS script, a lighttpd config, a factory Litecoin pool file with
  someone else's pools) and the shared `/etc/random-seed`.
- **Settings → Time:** the public time servers (`pool.ntp.org`) can be switched off; **set the clock by hand**
  (date and time, or *Use this browser's time*) for offline miners.
- An independent security review found no critical or high-severity issues; its medium findings (password for
  installs, local-network-only default) are fixed here.

**Fixes found in review**
- The thermal guard's Idle can no longer be undone by a setting change that was already on its way; the board is
  enabled again only after the cool-down, and not during a pool or fan hold.
- Raising the board clock (after a throttle step or a scheduled change) is refused when a chip's "+" trim
  couldn't run at the new clock.
- A running plan with an unreadable or fractional voltage is replaced by Stock High at start-up.
- Auto fan **Save** no longer fails on an out-of-range thermal-guard value while the throttle is off.
- Switching *Allow access from outside* on needs the panel password; a settings import never changes the access
  switches; refused outside connections are read with a small size cap.
- A settings import with an unreadable presets or schedule section is refused (nothing changed); schedule
  changes that point at a missing preset are dropped, with a note.
- A preset saved under v0.4.x as "Stock High" or "Stock Low" is renamed "… (mine)" once, and the schedule follows.
- Uploading the SD image by mistake gives a clear message instead of a dropped connection.

**Also in this release:** everything from the v0.3 and v0.4 test builds below: monitoring, pools and pool guard,
fans and thermal guard, the HW-error warm-up summary, notifications, log and settings export, schedule, NTP and
SSH.

**Release files:** `scbox2-firmware-v0.5.0.ius` (update from the panel), `scbox2-sd-v0.5.0.img.xz` (SD card),
`scbox2-firmware-v0.5.0-source.tar.gz` (source), `SHA256SUMS`.

## v0.4.4

- Voltage shown in volts everywhere on the Box II, from the C4 lug readings on one unit (7.67, 7.61, 7.58,
  7.55, 7.52 and 7.03 V; other steps estimated and marked "≈").
- Controls → Clock & voltage: the SC Lite board-voltage (PV) controls removed; the voltage box shows volts.
- No watts or J/TH while the power estimate (C2) is off.
- Auto-tuner (C8) uses Box II data: 525–690 MHz, Box II voltage steps, results compared by hashrate and HW errors.
- Ceiling probe (C6) and overclock (C7) refuse anything above 690 MHz.
- Leftover SC Lite wording removed; exports carry a Box II voltage column.
- Fixed: saved chip data checked against this board (one chain, chips 1–36); the strong-chip test and the
  ceiling probe's restore check fitted to the Box II; an SC Lite settings migration no longer runs.

## v0.4.3

- Fixed: the Stock preset always means the factory default (690 MHz, stock voltage) again.
- Voltage shown in volts on the board card, Presets page and stock presets.
- Fixed: switching chip trims (C5) off clears every per-chip trim.

## v0.4.2

- Fixed: the HW-error warm-up summary now works at boot, not only when a stopped board is started again.
- HW errors tile: "warming up, N min left", then the HW % since warm-up and the count at start-up.
- A stopped board shows its hashrate as "0 (stopped)".
- Turning verbose logging on or off is written to the activity feed.

## v0.4.1

- Fixed: the fan curve, auto fan and the thermal guard work again on the Stock plan.
- Fixed: the thermal guard reports "fans forced 100 %" only when the write landed, and notifies when it fails.
- Fixed: a stopped board shows STOPPED; while stopped, the panel judges cooling on the live board sensor.
- Warm-up HW errors summed up in one line in the activity feed instead of listed one by one.
- The dashboard banner shows which features are on for the unit.

## v0.4.0

- Fixed: the panel no longer moves a voltage trim when it sets the fans (the plan's 4th field keeps the stock
  value). Saved plans are corrected at start-up.
- New: every hardware control from the SC Lite firmware ported, shipped off, switched on per unit after its
  hardware test: C1 stop hashing, C2 power estimate, C3 clock control, C4 voltage control, C5 per-chip trims,
  C6 ceiling probe, C7 overclock, C8 auto-tuner, C9 firmware update from the panel.
- A feature can't be switched off in the middle of a tuning run, probe, chip test or idle.
- At start-up, a running clock that's no longer allowed is replaced with the stock setting.
- A firmware image install is refused while the clock-ceiling probe runs.

## v0.3.4

- Fixed for good: no rejected shares (`time-too-new`) right after the miner starts. The Blake2b header builder
  keeps the pool's job time too.
- The reason for a rejected share is shown in the panel's events and in the log export.

## v0.3.3

- Fixed: only one miner runs at a time. Two copies driving the same chips caused re-inits and very high HW errors.
- The board card shows the board's voltage setting instead of "0 V".

## v0.3.2

- Fixed: the first shares after a start were rejected as `time-too-new`; the firmware's copy of the miner keeps
  the pool's job time.
- The miner launcher waits for the network before its first start, so a slow network start doesn't make it fall
  back to the OEM's unmodified miner.

## v0.3.1

- Fixed: the hostname set in the panel survives a restart. It is applied at boot, before the network starts.

## v0.3.0

- Mining starts by itself: the firmware's own launcher starts the miner once a pool exists, at every boot and if
  it exits.
- Miner config passed the way the Box II miner accepts it (`--config <file>`).
- Every file, program and path named for the SC Box II (`scbox2-agent`, `/mnt/config/scbox2`, `S48scbox2`).
- Firmware version shown in the panel (`/etc/scbox2-release`).
- Debug launch logging removed. The image is reproducible.
