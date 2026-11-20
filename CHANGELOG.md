# Changelog

All notable changes to the SC Box II Community Firmware. Newest first.

Versions before 0.5.0 were test builds, run on one SC Box II. In those builds every hardware control shipped off
and was switched on per unit after a hardware test (C1–C9). Those test codes appear in the history below.

## v0.5.6: custom clocks survive a reboot, calmer fan hold, a reboot that doesn't hang

Fixes from testing v0.5.5 on hardware (the C8b tuner test).

- **A custom clock now survives a reboot.** The OEM's miner program keeps a hidden second copy of its settings
  files (`.cpbsetting.json`, `.bfgminer.json`, `.temp_bfgminer.json`) and starts mining from those at boot. Since
  v0.5.2, with the settings kept in RAM, the panel saved only the visible files, so every boot started on the clock
  from before v0.5.2 (on the test unit, 690 MHz instead of the saved 688 MHz). The hidden copies are now saved with
  their files, by the same rules (your own changes only; fan steps, the heat throttle and schedules never).
  - **Once, at the first start of v0.5.6:** if the miner started on that old copy, your saved clock is put back
    and saved properly ("⚙ start-up: your saved clock setting … put back"). Not while a tuning run waits to be put
    back, not on Idle, and only if the saved setting is still allowed.
- **The tuner's fan hold writes far less often.** Each fan write hands the miner its whole setting again; in a
  17-hour run one such write landed in the same second as a new job from the pool and the OEM's mining program
  restarted. The hold now writes a change once it adds up to 3 % (or after 2 minutes), never twice within 10 s,
  and re-sends the same value every 5 minutes instead of every minute: about 19 writes in 70 minutes instead of 194.
  A board 1 °C or more over the target still gets more air at once; full fan and the heat abort are unchanged.
- **Reboots don't hang.** An update's reboot once stalled for over 4 minutes until the power was cycled: the OEM's
  shutdown scripts can stall. With everything already on the read-only settings partition, the reboot now skips
  them; otherwise a reboot that hasn't happened 90 s later is forced.
- **Tuner:**
  - the chip map shows the Dashboard's hover box: a chip's new errors, its limit and how much of it was used, its
    chip trim, and a red or green verdict;
  - a preset with the same clock and voltage as one already built isn't saved a second time (a narrow clock range
    made Low and Lowest power the same);
  - `results.csv` shows the fan the hold held on failed tests too (it showed 0 %), and nothing when the fans
    weren't held;
  - "1 chip" / "1 error" in the trim notes.
- **Firmware update, step ② in one go:** the line from the release's `SHA256SUMS` can be pasted with the file in
  step ①, and the check then needs nothing more; pasting it in step ② now runs the miner's check straight away
  (before: Check, then paste, then Check again).
- **Presets:** after Apply, the list keeps refreshing until the miner runs the preset (up to a minute), so it is
  marked "on the miner" without pressing it again (a change takes 30–40 s to settle on the SC Box II).
- **Security:** a connection whose address can't be read is refused (the panel and SSH).

## v0.5.5 (2026-11-15): the auto-tuner, with chip trims and presets

- **Auto-tuner in the public release:** the **Tuner** tab maps clock and voltage on this miner, confirms the best
  setting and can trim weak chips and build presets (below). Its clock and voltage search was tested on hardware
  on one unit (2026-11-15); its chip trims and preset building are being tested on this release's public image
  (see the release notes). Run a **dry run** first: it reads the chips and writes nothing.
- **Tuner → “Trim weak chips”** now works on the SC Box II, as on the SC Lite. When a test fails because of one or
  a few chips, the tuner slows those chips 12.5 MHz at a time (through the chip trims) and runs the same test again,
  before it adds voltage for the whole board. A trim stays only if it made the failing test pass.
  - At most **4 chips per board** (the most this miner allows), each slowed by **25–75 MHz** (50 MHz by
    default), one chip per pair. A chip is never run faster than the board.
  - On this miner most failed tests are on the board's total, with a few chips making most of the errors (the C8
    run: b0.c26, c23, c25 …). Such a test counts too when one chip made at least a third of those errors. When
    the errors are spread over the whole board, no trim is tried: that needs more voltage, and trying would cost
    hours of extra tests.
  - The order is the SC Lite's: at each voltage, from the lowest up, the tuner first tries trims, and only if they
    don't make the test pass does it undo them and go one voltage step up. So a lower voltage with a few slowed
    chips is always tried before a higher one for the whole board.
  - At the end a **trim check** lifts each trim one step at a time at the best setting and keeps only the ones
    still needed. The trims the run keeps are listed and stay in **Controls → Chip trims**; auto-trim takes them
    over.
  - **Stop, a safety stop, a run that confirms nothing, and a power cut** put every trim back as it was before the
    run. While the run tries trims they stay in memory only; the trims file is saved once, at the end.
  - The hashrate check allows for the trims (a slowed chip does less work), and the results table shows the power
    estimate with the trims and J/TH. A slowed chip uses less power in the same proportion, so trims don't change
    J/TH; what they buy is a lower voltage, and J/TH follows the voltage squared.
  - The chip map marks the slowed chips (▾) in each test.
  - Re-applying a trim sends no command to the miner while it is still taking a new clock (its confirmation and
    quiet time), nor for 30 s after a failed clock change is put back, an Idle or a whole setting put back, nor
    while the miner is on Idle or held: the miner's API sees no extra traffic while its boards ramp.
- **Tuner → “Build presets”** on the SC Box II: **High, Middle, Low and Lowest power**, each a clock and a voltage
  from the clean curve and its own fan curve, tested by the tuner. Also **“Rebuild the presets from the last
  search”**. (The Presets tab's older **Build from tuner** shortcut, which tested nothing, is not offered on the
  SC Box II: use Tuner → Rebuild presets.)
  - Building presets needs **Hold a steady temperature** (each fan curve is made from the fan speed that held
    the preset's temperature). The presets are held from your target up to 74 °C at most, and the warmest at least
    8 °C below the abort.
  - Every preset clock is inside the clock window and inside the range the curve ran clean. Chip trims are not
    part of a preset (as on the SC Lite): the run's kept trims apply to every clock.
  - If the run confirmed nothing, its trims come off before the presets are tested, so each preset is tested as
    the miner will run it.
- **Presets panel** on the SC Box II: the four tuned presets next to Stock High, Stock Low and Idle, each with
  Apply / Edit / Apply again, its fan curve, "Clean at … °C · ≈ … TH/s · tested …" and its power (your wall-meter
  reading there, or the estimate from the readings on one unit). **Apply** uses the same checked writer as
  Controls, switches the fans to the preset's curve and keeps the setting for the next boot. A schedule can use
  the presets; a scheduled step is not saved as your setting. **Edit** checks this miner's ranges: 7.03–7.67 V,
  a clock inside the window and on the chips' 12.5 MHz steps (enter 588 for 587.5). An edit that changes the
  clock or the voltage drops the preset's "tested" tag (the test was of the old setting). A preset saved above a
  clock window that was later locked lower is shown as "outside the current clock window", so you can delete it;
  a schedule step using it fails saying so. Your own wall-meter reading shows on a preset even with the power
  estimate off.
- The public release shows these too (its tuner is on): the preset slots and the pointer to "Trim weak chips" in
  Chip trims are no longer hidden.
- **Safety fixes in the tuner's trims and presets** (found in review before release):
  - The end of a run sets the trims file back to exactly the trims before the run (or those plus the trims it
    kept, with a confirmed setting), whatever a failed write left. It writes the file only and puts the safe clock
    back at once (a heat abort no longer waits on the trims' re-send); the clock change applies the trims.
  - The run's restart marker is removed from the flash too when a run ends well, so a power cut afterwards no
    longer makes the next start undo a finished run; the watchdog marks a stuck run as ended before it clears it.
  - A plan you set (Apply, Controls) reaches the flash only while the miner still runs it and the OEM's files show
    it, so a throttle, schedule or fan-hold step a few seconds later can't be saved as your setting.
  - While a run tries trims, no save (yours, or the one before a reboot) puts them on the flash. Auto-trim's record
    of the trims it owns (and the tuner's it took over) now reaches the flash, so they don't come back as hand
    trims after a power cut. The tuner's saved curve reaches the flash, so a rebuild works after a reboot.
  - "Rebuild presets" checks its clock and voltage steps and voltages like a map run; the tuner only counts chips
    as fixable by trims when it could really trim them (not hand-trimmed, not at the deepest trim, one per pair,
    a free slot).


- **Settings → Troubleshooting:** a new panel, the last one in Settings, for diagnosing freezes and unexpected
  restarts. It holds "Send the system log to this computer" (a UDP syslog relay), now in **every SC Box II build**,
  the public release included. Off by default; an IPv4 address only.
- The panel explains it in plain words: what it does and when to use it, that the log goes unencrypted over the
  local network (point it only at your own computer), a one-line receiver to copy for Linux, Mac and Windows, the
  steps to set it up, and what to attach to a bug report. The same in
  [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).

## v0.5.2: settings storage after a power cut; clock ceiling probe and overclock per unit; measured power

- **Clock ceiling probe and overclock, in the public release:** both are now in **Controls**, each behind its own
  confirmation (there is still no Commissioning section; the auto-tuner stays out).
  - The probe steps the clock past 690 MHz on the chips' 12.5 MHz grid (700, 712.5, 725, 737.5, 750 MHz) at the
    stock 7.67 V, holds each step 3 minutes while mining, and stops on heat (82 °C), HW errors over 1 % (or 3× the
    board's own rate at the start, whichever is higher; fewer than 10 new errors in a step are ignored), a chip or
    board drop-out, or an estimated wall power over 460 W. It always goes back to 690 MHz. Its result is saved on
    the unit, like a setting.
  - The clock limit can be unlocked only after this unit's probe has a clean result, and only up to it, never
    above 750 MHz. No result, or a failed 700 MHz step: the clock stays at 690 MHz.
  - Measured on the first unit: 700 MHz gave about 2 % more hashrate for 2 % more power; above that, errors rose
    and efficiency got worse. Overclocking never improves efficiency; lowering clock/voltage does
    ([HARDWARE.md](docs/HARDWARE.md#measured-power-and-overclock)).
- **Power estimate from wall-meter readings:** without your own reading, the panel now shows (≈) what one unit drew
  at the wall: 392 W at Stock High and 251.8 W at Stock Low (the OEM states 400 W and 260 W), and 399.4 / 406.9 /
  413.5 W at 700 / 720 / 725 MHz. Between and past those, from 525 to 750 MHz at every voltage step, it follows
  the curve through them (power = k × clock × voltage², within 0.7 W of every reading). Controls shows the
  estimate for the setting you are about to apply. Your own readings still replace it exactly.

The settings-storage changes below change no clocks or voltages.

- **Start-up check of the settings partition:** after a power cut the 3 MiB config partition can come up "dirty",
  with clusters no file uses any more and an invalid FSInfo sector. The kernel then made it read-only the first
  time it tripped over that, and nothing could be saved until a reboot. A new small program,
  `/usr/bin/scbox2-fatfix`, now checks the partition at every boot before it is mounted (`S38initdir`, log in
  `/tmp/scbox2-fatfix.log`). It repairs only what is safe: frees the lost clusters, rewrites the FSInfo sector,
  clears the dirty flag and keeps the backup boot sector in step, then reads its writes back. On anything riskier
  (files sharing clusters, broken or looping chains, sizes that don't match, damaged directory entries) it changes
  nothing and the Dashboard says what it found. The volume label is left as it is.
- **Read-only settings storage is shown:** a red Dashboard banner and one notification (new event: "Settings
  can't be saved") when the settings storage is read-only, with what to do (a reboot runs the repair).
- **Fans and the thermal guard don't depend on saving:** the fan curve, the abort tier's 100 %, the throttle and
  the overheat Idle run from the settings in memory whatever the storage does; a failed write is logged once.
  Fan settings you save while the storage is read-only are used until the next restart (the panel says they
  weren't saved).
- **Found while testing:** the OEM's backend saves every plan, fan levels included, to the settings storage
  *before* it applies it, and gives up when it can't. So while the storage is read-only, fan changes from the
  panel (the curve and the guard's 100 %) don't reach the fans: they stay at the last level that landed. The
  banner and the fan status say so; the thermal guard can still stop the boards, and the OEM's 86 °C cut-off is
  unchanged.
- **Every failed save is shown:** each setting the panel couldn't save answers with the reason, and the page shows
  it (several buttons were silent before: clearing the power calibration, time sync, display units, dismissing
  the restart warning, resuming the pool guard, stopping the tuner).
- **Build:** `kit/tools/build-fatfix-box2.sh` and `--fatfix`; the build's start-up check runs the image's own
  `scbox2-fatfix` on a test image with power-cut damage (must be repaired) and on a cross-linked one (must be
  left untouched).
- **No flash writes while the miner runs:** the settings partition is now mounted **read-only** (at
  `/mnt/cfgflash`), and every program works on a copy of it in RAM at `/mnt/config`. That includes the OEM's
  backend, which rewrote its settings file on the flash for every fan step and at every start, and the panel's
  history and counters. A fan change never touches the flash. Nothing that keeps the miner
  cool waits on the flash, and normal running can no longer damage the partition in a power cut. The fan curve is
  exactly as smooth as before: every step still goes to the fans.
- **Saving writes in a short window:** a setting is copied to the partition only when you save it. The partition
  is made writable, the file is written with a backup copy (`.bak`), everything is synced, and the partition is
  read-only again. Only one save runs at a time, across the panel, the SSH server and the boot scripts. If the
  partition can't be made writable, the save says so ("read-only") and the setting is used until the next restart.
- **What is saved when:**
  - Panel settings: when you save them.
  - The OEM's settings (pools, the network, the clock setting and mode you choose): when your change reaches
    the OEM's backend. A change of the fan levels alone is never saved.
  - Hashrate history, events, best shares and tuning logs: when you reboot from the panel, at a shutdown, and at
    the end of a tuning run.
  - The safety holds (the overheat Idle, the pool guard's hold, the fan lock): when a guard sets or lifts them, so
    they survive a power cut as before.
  - The random seed: at boot and at shutdown, no longer every 6 hours.
  - **Settings → Recovery → Keep the system log** (off by default) still writes its log tail and "last seen" note
    every few minutes, because that is what it is for.
- **The OEM's backend no longer drops a plan when its save fails:** two patched branches in `minerd`'s
  `PUT /mcb/setting` (0xf5aa, 0xf5e2). A failed `cpbsetting.json` or `bfgminer.json` save is logged, and the plan
  still goes to the miner. Before, the fans froze on a read-only partition. This only matters if the RAM copy
  couldn't be made.
- **Fallbacks:** if the RAM copy can't be made at boot, the partition is used directly, as before. If it can't be
  mounted at all, the miner starts from defaults, and the Dashboard says settings can't be saved.
- **Build:** the start-up check now also boots the settings partition the way the miner does, using the image's
  own boot step, `minerd` and panel under qemu (in a user namespace with its own `binfmt_misc`). It proves that:
  - the partition is read-only after boot;
  - five fan changes through the real backend all answer and land in RAM, while the partition stays the same
    byte for byte and no write window opens;
  - the patched backend still takes a plan with its settings folder read-only;
  - a panel save opens a window, lands, and leaves the partition read-only.
  `scbox2-fatfix` 2 (it also carries the write window, `scbox2-fatfix commit`), and two new scripts:
  `/usr/libexec/scbox2/cfgmount` and `/usr/libexec/scbox2/cfgwrite`.
- **Installing:** nothing to migrate. The partition keeps the same files in the same places, so the stock
  firmware can still be restored from an SD card. After a reboot you made from the panel, nothing is lost. After
  a power cut, the hashrate history and events since the last panel reboot are lost, but no setting you saved.

## v0.5.1: neutral naming and new look

No change to mining, clocks, voltages or fans.

- **Vendor-neutral wording everywhere:** the panel, its messages and activity log, the files the firmware writes
  (`/etc/scbox2-release`), the log export, the build kit and every document now call the miner's manufacturer
  "the OEM". Log lines streamed from the stock programs show the same neutral naming. The only literal vendor
  strings left are the ones the firmware must match on the device (the model the stock miner reports, the cloud
  hosts it blocks, a stock login it disables), never shown.
- **New logo and banner:** the panel's gold ₿ block is now the project mark, in the README banners (dark and light).
- **Docs:** the SSH server's build step (`kit/tools/build-sshd-box2.sh`, `--sshd`) added to the README and
  [docs/BUILDING.md](docs/BUILDING.md).
- **Tests:** a test file no longer contains a token-shaped string or a real wallet address (GitHub's secret
  scanning flagged the made-up token used to test log redaction).

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
