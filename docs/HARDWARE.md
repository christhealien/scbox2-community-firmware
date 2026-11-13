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
| Storage | Read-only squashfs system on its own flash partition (18,874,368 bytes) and a small **3 MB** FAT config partition (`/mnt/config`) |
| Thermal cut-off | **86 °C**, the OEM's own. This firmware never raises it. |
| Stock firmware | 2.2.1 |

## Stock modes

| In the panel | OEM name | Clock | Voltage (one unit) | Hashrate | OEM power figure |
|---|---|---|---|---|---|
| **Stock High** (the factory default) | Hashrate | 690 MHz | 7.67 V | about 1.9 TH/s on one unit's dashboard | 400 W |
| **Stock Low** | Low-power | 525 MHz | 7.03 V | | 260 W |

The OEM publishes 1.9 TH/s ±5 % at 400 W ±5 % for the Hashrate mode, and 1.45 TH/s at 260 W for Low-power.
The miner has no power sensor: the panel shows these figures for the two stock modes until you calibrate the
power estimate with your own wall-meter readings.

## Clock

- The panel allows **525–690 MHz**; the chips run in 12.5 MHz steps (690 runs at 687.5). Nothing above 690 MHz,
  the Stock High clock.
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

## Where things are on the miner

| What | Where |
|---|---|
| Panel settings | `/mnt/config/scbox2` |
| Pool config the miner runs with | `/mnt/config/bfgminer/bfgminer.json` |
| Panel program / boot script | `/usr/bin/scbox2-agent` / `/etc/init.d/S48scbox2` |
| Miner launcher | `/usr/libexec/scbox2/miner-run` |
| The OEM miner, unmodified | `/usr/libexec/scbox2/intminer` |
| Firmware version and build details | `/etc/scbox2-release` |
| Result of the last panel update | `/mnt/config/scbox2/update/last.json` |
