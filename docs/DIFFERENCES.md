# Differences from the stock firmware

This firmware is built from the OEM's stock SC Box II firmware 2.2.1. It replaces **only the system
filesystem**: the bootloader, kernel and every other part of the firmware stay byte-for-byte as stock.
The OEM's own programs still run the miner: `minerd` (web backend), `intminer` (the miner, BFGMiner 5.4.2
with the OEM's SC Box II driver), `xminerd` (watchdog) and `fanctrl` (fan control).

## What changes

| | Stock firmware | This firmware |
|---|---|---|
| Control panel | OEM web UI on port 80, reachable from anywhere | `scbox2-agent` on **8080** (HTTP and HTTPS; the port can be moved), also on **80** and **443** (HTTPS; self-signed certificate made per unit). Ports 80 and 443 can be switched off; an optional *HTTPS only* switch. |
| Who can reach it | Anyone who can reach the miner | **Local network only** by default (IPv4 and IPv6), with a switch to allow access from outside. One account; the factory password must be changed at first sign-in; 5 wrong passwords block an address for 10 minutes. See [SECURITY.md](SECURITY.md). |
| OEM web API (`minerd`) | Reachable on the LAN, port 80 | **Loopback only** (127.0.0.1:80): only the panel reaches it |
| Miner API (`intminer`) | Reachable on the LAN, port 4028; allow-list `W:127.0.0.1,A:0/0` | **Loopback only** (127.0.0.1:4028); allow-list `127.0.0.1` only |
| OEM cloud | MQTT client (remote pools, power, firmware, factory reset) and reports to the OEM's discovery host | **Removed**; the cloud host names are not left in what the miner runs |
| UDP 1413 responder | An unauthenticated LAN broadcast sets the IP and DNS and reboots the miner | **Not started** |
| Clock | Two modes: 690 MHz (Hashrate) and 525 MHz (Low-power) | **525–690 MHz**; the chips run in 12.5 MHz steps (690 runs at 687.5). Never above 690 MHz. |
| Voltage | Fixed by the mode | **20 steps from 7.67 V (stock) down to 7.03 V**, shown in volts. Never above stock. |
| Per-chip clocks | Every chip at the board clock | Optional **per-chip trims** and auto-trim, through a hooked copy of the miner and a small library |
| Stop hashing | No idle mode in the stock plans | **Idle** from the panel, a schedule or a safety guard, with checks that the board stopped and cooled |
| Power figures | None | **Watts and J/TH**: your wall-meter readings per setting, else the OEM's published figures for the two stock modes |
| Fans | `fanctrl` | `fanctrl` stays. Optional **auto fan** with your own fan curves, and a **thermal guard** below the cut-off. |
| Thermal cut-off | 86 °C | **86 °C, unchanged.** Never raised or weakened. |
| Starting the miner | `xminerd` (on this board it does not start the miner) | The firmware's own launcher starts it once a pool exists, at every boot and if it exits. Only one miner can run at a time. |
| Pools | Whatever the config says | Starts with `--no-pool-redirect`; the **pool guard** removes any pool that isn't in your Pools tab |
| Share timestamps | The miner moves the first job's timestamp into the wrong bytes, and the pool rejects the first shares (`time-too-new`) | The firmware's copy of the miner keeps the pool's job time. The OEM's unmodified miner stays as a fallback. |
| Internet check (`xminerd`) | Pools, then `www.baidu.com` and `www.google.com` every 3 s | A local address the panel answers, then your pools. No outside host: mining doesn't depend on the internet. |
| Fan check (`xminerd`) | The first reading at a fan level becomes its reference, even from a failing fan | A reading is kept as the reference only when the fan passed |
| SSH | Stock OpenSSH with host keys shared by every unit | Its own small program on **22** (`scbox2-sshd`, started before the panel, so it keeps working if the panel can't start): your panel account, a host key made per unit, shell and SFTP. Stock OpenSSH and the `scp`, `sftp` and `telnet` clients removed. |
| Serial console | Root shell without a password | Login required: `root` / `scbox2-recover`, a documented recovery login, the same on every unit, serial pins only. The `default` account is locked. |
| Wi-Fi | Wi-Fi daemon, hostapd, wpa_supplicant, dnsmasq (the board has no Wi-Fi chip) | **Removed.** Ethernet only, DHCP on `eth0`. |
| mDNS | avahi | **Removed** |
| Time | The OEM's China time zone (UTC+8); four NTP clients on hardcoded servers (two of them IP addresses in China) | China time (UTC+8) until a time zone is chosen in the panel (at first sign-in it takes your browser's); NTP from your servers and the router's (DHCP) together, `pool.ntp.org` last (can be switched off). Works offline: the clock can be set by hand in the panel. |
| DNS | `minerd` writes `114.114.114.114` (a public DNS in China) when a static IP has none | Never written. Your DNS servers, or the router's, or the default gateway. |
| Random seed | `/etc/random-seed`, the same file on every unit | Each miner keeps its own seed on its config partition |
| Leftovers | An NFS script for a factory server, a lighttpd config, a factory Litecoin pool file with someone else's pools | **Removed** |
| Hostname | Fixed | Set in the panel, applied at boot before the network starts |
| MAC address | Set by the OEM's tool | Kept steady across boots, or random each boot, or a fixed one you choose |
| Other coins | Other-coin miners shipped | Removed (this is a Blake2b / Siacoin miner) |
| Firmware install | Web upload, RSA-signed images only, or SD card | **SD card** for the first install. Then **from the panel**: a release's `.ius` from GitHub or a file, checked against the release's `SHA256SUMS`, four confirmations and the panel password, written to the system partition only, read back, and the old system written back on error. |
| Settings | The OEM config | Panel settings on the 3 MB config partition (`/mnt/config/scbox2`), with crash-safe writes. **Settings backup**: export and import as JSON (network, fan curves, presets, schedule, notifications; not pools, hostname, time settings or secrets) |

## What stays the same

- **Bootloader and kernel** (Linux 3.10, InfoTM vendor build) and the C library (uClibc 0.9.33.2). The OEM's
  closed-source programs are built against them.
- **The OEM's SD burn procedure.** The burn container keeps the OEM's format byte for byte except inside the
  system filesystem, so a stock SD card always returns the miner to stock.
- **The OEM fan control** (`fanctrl`) and its **86 °C cut-off**.
- **`minerd`:** the panel uses its API to set clocks, voltages, fans and pools.

## Not in this release

The clock ceiling probe, overclocking above 690 MHz and the auto-tuner are planned for a later release.
