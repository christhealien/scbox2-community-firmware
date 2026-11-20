# SC Box II hardware notes

What this firmware knows about the SC Box II, from the stock firmware and from measurements on one unit.
Readings from one unit are a guide, not a spec: your unit may read a little different.

## The miner

| Part | Details |
|---|---|
| Algorithm | Blake2b (Siacoin) |
| Hash board | **1 board, 36 ICT580 chips**, in 18 pairs on a series power rail |
| Fans | **2**, on one fan-control channel. The OEM fan control is calibrated for the stock fans (about 4500 RPM). |
| Control board | **MCB_V6_3_5**, InfoTM **iMAPx910** SoC (ARMv7) |
| Network | **Ethernet only.** There is no Wi-Fi chip on the board, only an empty footprint. |
| Operating system | Linux 3.10 (InfoTM vendor build), uClibc 0.9.33.2, BusyBox 1.21 |
| Storage | Read-only squashfs system on its own flash partition (18,874,368 bytes) and a small **3 MB** FAT config partition on raw NOR (`/dev/spiblock2`; the flash rewrites a 64 KiB erase block for any change) |
| Thermal cut-off | **86 °C**, the OEM's own. This firmware never raises it. |
| Stock firmware | 2.2.1 |

## Stock modes

| In the panel | OEM name | Clock | Voltage (one unit) | Hashrate (one unit) | Wall power (one unit) | OEM power figure |
|---|---|---|---|---|---|---|
| **Stock High** (the factory default) | Hashrate | 690 MHz | 7.67 V | 1.94–2.00 TH/s | 392 W | 400 W |
| **Stock Low** | Low-power | 525 MHz | 7.03 V | 1.48–1.55 TH/s | 251.8 W | 260 W |

The OEM publishes 1.9 TH/s ±5 % at 400 W ±5 % for the Hashrate mode, and 1.45 TH/s at 260 W for Low-power.
The miner has no power sensor: until you calibrate the power estimate with your own wall-meter readings, the
panel shows an estimate (≈) from the readings on one unit below.

## Measured power and overclock

Measured on one unit (wall meter: the whole miner, fans and power supply included). Another unit can differ.

| Clock (requested → the chips run) | Voltage | Wall power | Hashrate | Result |
|---|---|---|---|---|
| 525 MHz → 525 | 7.03 V | 251.8 W | 1.48–1.55 TH/s | Stock Low |
| 690 MHz → 687.5 | 7.67 V | 392 W | 1.94–2.00 TH/s | Stock High |
| 700 MHz → 700 | 7.67 V | 399.4 W | about 2.01 TH/s | clean, 15 min |
| 720 MHz → 712.5 | 7.67 V | 406.9 W | about 2.04 TH/s | clean, 15 min; 2 h soak: 405 W, 2.01 TH/s, 70 °C, 140 HW errors, 0 rejects |
| 725 MHz → 725 | 7.67 V | 413–414 W | about 1.98 TH/s | probe: failed (HW errors 23.77 %, limit 1 %); 15 min by hand clean of the stop rules |

- **Clock ceiling probe (C6):** clean up to 720 MHz (712.5 MHz on the chips) at 7.67 V; 725 MHz failed on HW
  errors. The guard stopped it and the miner was back on 690 MHz within a minute.
- **Overclock (C7):** 700, 720 and 725 MHz each ran 15 minutes without tripping a stop rule; 720 MHz then ran
  2 hours (above); re-locking put the 690 MHz limit back.
- **Conclusion:** above 690 MHz the hashrate gains 0–3 % for 2–5 % more power, and efficiency never improves.
  700 MHz gave about 2 % more hashrate for 2 % more power; above that, errors rose and efficiency got worse.
  Lowering the clock or the voltage is what improves efficiency.
- **The estimate:** the five readings fit one curve, power = k × clock × voltage² (the clock the chips run; k from
  Stock High), within 0.7 W, Stock Low included. The panel uses the readings at their own settings and this curve
  between and past them, from 525 to 750 MHz at every voltage step (750 MHz at 7.67 V: about 428 W).

How the panel uses this: the **clock ceiling probe** steps the clock past 690 MHz on the chips'
12.5 MHz grid (700, 712.5, 725, 737.5, 750 MHz) at 7.67 V, holds each step for 3 minutes while mining, stops on
heat (82 °C), HW errors over 1 % (or 3× the board's own rate at the start, whichever is higher; fewer than 10 new
errors in a step are ignored), a chip or board drop-out, or an estimated wall power over 460 W, and always goes
back to 690 MHz. Its result is saved on the unit. The **overclock unlock** opens only after a clean result, and
only up to it, never above 750 MHz; without a result, or if 700 MHz failed, the clock stays at 690 MHz.

## Tuner results (C8)

Measured on one unit with the panel's auto-tuner: wall meter (the whole miner), the owner's room, each test a
10-minute window (hashrate read over a 10-minute window moves by about ±5 % on its own). All at the stock
690 MHz / 7.67 V unless the table says otherwise.

**Clock only, at 7.67 V:**

| Clock | Wall power | Hashrate (average) | Hottest | |
|---|---|---|---|---|
| 650 MHz | 372.6 W | 1.86 TH/s | 72 °C | |
| 675 MHz | 384.5 W | 2.03 TH/s | 71 °C | within the ±5 % noise of a 10-minute window |
| 690 MHz (Stock High) | 392 W | 1.93 TH/s | 73 °C | |

At a fixed voltage the efficiency (J/TH) is about the same at every clock, as the overclock measurements above
found too: a lower clock saves power only in step with the hashrate it gives up.

**Voltage:**

- At **675 MHz** the lowest voltage that passed was **7.55 V**: it passed one 10-minute test at 100 % of its
  error limit, then failed the 20-minute confirm.
- At **690 MHz** nothing below the stock 7.67 V passed: 7.61, 7.58 and 7.55 V all failed.
- At **7.29 V and below** the chips fail within seconds (hashrate under 1 TH/s).

**Conclusion:** on this unit the stock **690 MHz / 7.67 V** is already the right setting. Units vary: the tuner is
there to check yours.

**Guards, tested on the same unit:**

- **Stop:** the miner was back on Stock High within a minute.
- **Heat abort:** with the abort set to 75 °C, the run stopped itself at 76 °C.
- **Power cut in the middle of a run:** the settings partition came back clean, the run was marked as cut short,
  and the settings were intact.
- **Fan hold** at a 72 °C target: 70–73 °C with the fans at 50–61 %, no swings to full fan. The hold aims at
  74 °C at most on this model.

## Clock

- The panel allows **525–690 MHz**; the chips run in 12.5 MHz steps (690 runs at 687.5). Above 690 MHz (the
  Stock High clock) only once unlocked, up to this unit's clean probe result, 750 MHz at most
  ([above](#measured-power-and-overclock)).
- The ICT580's clock grid is 12.5 MHz steps from 500 MHz up. The miner's driver rounds a request down to that
  grid: its log shows a 690 MHz request running at 687.5 MHz.
- Each time the chips start (boot, mining restart, or a stopped board started again), the driver starts them at
  500 MHz and ramps straight to the set clock while they are still cold. A few dozen HW errors in the first
  minute are normal; the panel sums them up as warm-up errors.

## Voltage

The SC Box II has no voltage sensor and no millivolt setting. The OEM firmware sets the hash board's voltage
in **20 fixed steps**; Stock High (the OEM's Hashrate mode) uses the highest one and Stock Low (Low-power) the
lowest. The panel shows and sets these steps **in volts only**, and never goes above the stock 7.67 V. The step
numbers below are for this table; the panel doesn't show them.

The volts below were measured with a multimeter at the **hash board's power lugs** (DC, + and − lugs where the
supply leads enter the board), under load, on one unit. The clock did not change the reading: the stock step read
7.67 V at both 525 and 690 MHz. Steps without a measurement are estimated at about **0.033 V per step** and shown
with "≈" in the panel.

| Step | Volts | | Step | Volts | |
|---:|---:|---|---:|---:|---|
| 1 (Stock High) | **7.67 V** | measured | 11 | ≈ 7.33 V | estimate |
| 2 | **7.61 V** | measured | 12 | ≈ 7.29 V | estimate |
| 3 | **7.58 V** | measured | 13 | ≈ 7.26 V | estimate |
| 4 | **7.55 V** | measured | 14 | ≈ 7.22 V | estimate |
| 5 | **7.52 V** | measured | 15 | ≈ 7.19 V | estimate |
| 6 | ≈ 7.49 V | estimate | 16 | ≈ 7.16 V | estimate |
| 7 | ≈ 7.46 V | estimate | 17 | ≈ 7.12 V | estimate |
| 8 | ≈ 7.43 V | estimate | 18 | ≈ 7.09 V | estimate |
| 9 | ≈ 7.39 V | estimate | 19 | ≈ 7.06 V | estimate |
| 10 | ≈ 7.36 V | estimate | 20 (Stock Low) | **7.03 V** | measured |

On the measured unit at 690 MHz, the HW-error rate stayed low down to 7.58 V; at 7.55 V it was 0.15 % and at
7.52 V 0.21 %. Every unit is different: lower the voltage one step at a time and watch the HW errors.

## Fans

Keep the **stock fans** fitted. The OEM fan control regulates toward a target fan speed calibrated for them.
With slower replacement fans (for example about 2200 RPM) it never reaches that speed and keeps hunting between
slow and full speed, and the chips run hot: 90 °C and above was seen on one unit. Quieter fans also move far less
air than this board needs.

The panel's auto fan and thermal guard work below the OEM's 86 °C cut-off: the guard forces the fans to 100 %
at 82 °C by default and can stop hashing.

The OEM's backend saves every plan, fan levels included, to its settings before it applies it, and gives up when
the save fails. Up to v0.5.1 that meant the config partition: every fan step was a flash write, and on a partition
the kernel had made read-only after a power cut the fans stopped following the panel. From v0.5.2 the partition
is mounted **read-only** at `/mnt/cfgflash`, and every program works on a copy in RAM at `/mnt/config`. Fan
changes never touch the flash. A setting is written to the flash only when you save it, in a short write window:
the partition is remounted read-write, the file is written with a backup copy, then synced and remounted
read-only. The OEM's backend is also patched to go on with a plan when a save fails. The boot-time check
(`scbox2-fatfix`) still repairs the usual power-cut damage before the partition is mounted.

## Where things are on the miner

| What | Where |
|---|---|
| Panel settings | `/mnt/config/scbox2` (the RAM copy; the partition itself is read-only at `/mnt/cfgflash`) |
| Pool config the miner runs with | `/mnt/config/bfgminer/bfgminer.json` |
| Settings mount at boot / save a file of `/mnt/config` to the partition | `/usr/libexec/scbox2/cfgmount` / `/usr/libexec/scbox2/cfgwrite REL...` (`/tmp/scbox2-cfg-direct`: the RAM copy couldn't be made, the partition is used directly) |
| Panel program / boot script | `/usr/bin/scbox2-agent` / `/etc/init.d/S48scbox2` |
| Config partition check (run by `/etc/init.d/S38initdir`) | `/usr/bin/scbox2-fatfix`; this boot's report in `/tmp/scbox2-fatfix.log`, damage it left alone in `/tmp/scbox2-cfg-repair` |
| Miner launcher | `/usr/libexec/scbox2/miner-run` |
| The OEM miner, unmodified | `/usr/libexec/scbox2/intminer` |
| Firmware version and build details | `/etc/scbox2-release` |
| Result of the last panel update | `/mnt/config/scbox2/update/last.json` |
