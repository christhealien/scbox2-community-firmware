# Security and privacy

What the SC Box II Community Firmware opens on your network, what it contacts and when, which accounts exist,
and how updates are checked. Everything here describes the **public build** (v0.5.0).

**In short:** the panel and SSH answer only on your local network, one password protects both, nothing contacts
the internet except the time servers (and DNS to find them) unless you set something up, and the miner runs fully
offline with a pool on your LAN.

An independent security review of this firmware found **no critical or high-severity issues**. Its medium
findings are fixed in v0.5.0: installing firmware now asks for the panel password again, and the panel and SSH
answer only on the local network by default.

## Ports

| Port | What answers | Notes |
|---|---|---|
| **8080** (TCP) | The panel, HTTP **and** HTTPS on the same port | The main port. It can't be switched off, but it can be moved: **Settings → Network & SSH → Web UI port** (1024–65535). |
| **80** (TCP) | The panel over HTTP, on each LAN address | So typing just `http://<miner-ip>/` works. Can be switched off. |
| **443** (TCP) | The panel over HTTPS only | Can be switched off. |
| **22** (TCP) | SSH and SFTP: `scbox2-sshd`, a small program of its own (works even when the panel can't start) | On by default. Signs in with the panel account. Can be switched off or moved in Settings. |

Switches in **Settings → Network & SSH**:

| Switch | Default | What it does |
|---|---|---|
| **Allow access from outside the local network** | off | Off: the panel ports and SSH answer only the local network (below). Others get a one-line refusal. |
| **HTTPS only** | off | Plain-HTTP requests are redirected to HTTPS (port 80 to 443, 8080 to HTTPS on 8080). |
| Port 80 / Port 443 | on | Turn either extra port off. Port 8080 always stays. |

**"Local network"** means, for IPv4 and IPv6: loopback, private ranges (10/8, 172.16/12, 192.168/16, IPv6 ULA
`fc00::/7`), carrier-grade NAT (100.64/10, used by some VPNs such as Tailscale), link-local, the miner's own IPv4
subnet, and global IPv6 addresses in the miner's own /64.

The HTTPS certificate is made on the miner at first start, unique to that unit and self-signed, so your browser
warns once. The SSH host key is also made per unit.

### The OEM programs: loopback only

The OEM's own programs still run the hash board. Their network listeners are held to the miner itself:

| Program | Port | Reachable from |
|---|---|---|
| `minerd` (the OEM web backend) | 80 | 127.0.0.1 only. Only the panel talks to it. |
| `intminer` (the miner's API) | 4028 | 127.0.0.1 only, and its API allow-list is `127.0.0.1` only. |

The miner always starts with `--no-pool-redirect`: a pool can't move it to another server.

## What was removed from the stock firmware

| Removed | Why it matters |
|---|---|
| OEM cloud client (MQTT to the OEM's cloud host) | It could change pools and power, download firmware and factory-reset the miner remotely. |
| Reports to the OEM's discovery host | Model, serial number, MAC, IP and miner name sent to the OEM at start-up. |
| UDP 1413 responder | An unauthenticated LAN broadcast could set the miner's IP and DNS and reboot it. |
| Internet checks to `www.baidu.com` / `www.google.com` | Every 3 s. Replaced by a local check, so mining doesn't depend on the internet. |
| Hardcoded Chinese DNS (`114.114.114.114`) and NTP servers (two IP addresses in China, `asia.pool.ntp.org`) | No DNS or time server you didn't choose. |
| Stock OpenSSH | Its host keys were the same on every unit. Replaced by `scbox2-sshd` (modern encryption, the panel's account, a key per unit). |
| avahi (mDNS) | Not needed. |
| The Wi-Fi access-point stack (hostapd, wpa_supplicant, dnsmasq) | The board has no Wi-Fi chip. |
| `telnet`, `scp` and `sftp` clients | Unused. |
| The OEM's development leftovers | An NFS mount script for a factory server, the config of a web server that isn't in the image, and a factory Litecoin pool file with someone else's pools. |
| The shared `/etc/random-seed` | The same file on every unit. Each miner now keeps its own random seed on its config partition. |

Kept: the OEM's "send IP" broadcast inside your LAN (UDP 1314 to 255.255.255.255), without its call to
the OEM's discovery host. It belongs to a key handler in the OEM backend (`minerd`) and likely never fires on this board; it
never leaves your LAN.

The build checks the finished image: no OEM cloud host and no hardcoded DNS or NTP host is left in what the
miner runs.

## What the miner contacts, and who decides

**By default**, with nothing set up, the miner contacts only:

| What | When |
|---|---|
| **Your router's DHCP** | To get an address. |
| **Time servers** | The servers you set in **Settings → Time** and the ones your router hands out by DHCP, asked together; only when those give no time, the public `pool.ntp.org`. Untick *"Use the public time servers"* in **Settings → Time** and it never asks `pool.ntp.org`. |
| **DNS** (your router's, or yours) | To look up the time servers' names. |
| **The OEM's "send IP" broadcast** | Kept, LAN only (UDP 1314, see above); likely never fires. |

**Only what you set up:**

| What | When |
|---|---|
| **Your pools** | While mining. |
| **Your DNS servers** | Set in **Settings → Network & SSH**; otherwise your router's (DHCP), or the default gateway for a static IP. |
| **Notifications** (Telegram, Discord, ntfy, Gotify, a webhook) | Only those you switch on, when an event fires. |
| **GitHub** (`api.github.com`; downloads also from `github.com` and `release-assets.githubusercontent.com`) | Only when you click **Check for updates**, download a release, upload a file (its release is looked up online to confirm it), or if you switch on the *"New firmware available"* notification (one check a day; off by default). |

The panel page loads nothing from the internet: no fonts, scripts or trackers.

## Running offline

The miner runs fully offline with a pool on your LAN. Nothing in mining depends on the internet.

The clock: without a time server the miner can't know the time. Set it in **Settings → Time → Set the clock by
hand**: type a date and time, or click **Use this browser's time**. If a time server is configured, it corrects a
hand-set clock at its next sync.

## Accounts and passwords

| Account | Where | Notes |
|---|---|---|
| **Panel account** (`admin`) | Panel and SSH/SFTP | One account for both: changing the password changes both. The factory password `admin` / `admin` **must be changed at first sign-in**, on the panel or over SSH, before anything else works. |
| **Serial console** `root` / `scbox2-recover` | The UART pins inside the case only | A documented recovery login, **the same on every unit**. Not accepted over the network. It needs the case open and a serial adapter: physical access. The stock passwordless `default` account is locked. |

Protection on the panel and SSH:

- **5 wrong passwords** from one address (IPv6: one /64) **block it for 10 minutes**, on the panel and on SSH (each keeps its own count).
- **Signing out** ends the session on the miner too: a copied session cookie stops working. Changing the password
  ends every session.
- **Changes must come from the panel's own page.** A request from another website, or a script that doesn't name
  the panel, is refused.
- **Installing firmware** and **changing the update source** ask for the panel password again.

## Update integrity

Before **Settings → Firmware update** writes anything:

1. The file must be a complete SC Box II container (header CRC and the checksum of every part), every part except
   the system filesystem must be identical to the stock boot parts, and the system filesystem must fit
   its partition.
2. It must be **genuine**: its SHA-256 must match the line in the release's `SHA256SUMS` on GitHub. This is
   looked up online for downloads and for uploaded files (the file name `scbox2-firmware-vX.Y.Z.ius` tells the
   miner which release to look up). If the miner can't reach GitHub, or the file has another name, you paste
   the line from `SHA256SUMS` yourself.
3. You tick **four confirmations** and enter your **panel password**.

The checksum is fetched over HTTPS from the update source (default `christhealien/scbox2-community-firmware`).
**Releases are not signed.** The check proves the file is the one the release published; it does not protect
against someone who controls that GitHub repository. If you build your own firmware or use a fork, change the
update source under **Update source** (asks for the password).

There is no automatic roll back on this model: the recovery is an SD card. See
[INSTALL.md](INSTALL.md#7-recovery-with-an-sd-card).

## Reporting a security issue

Please open an issue at
[github.com/christhealien/scbox2-community-firmware/issues](https://github.com/christhealien/scbox2-community-firmware/issues).
For something that shouldn't be public before it's fixed, write a short issue asking for a private contact,
without the details.

<!-- OWNER TODO: add a private contact for security reports on the next line (e-mail or a GitHub private advisory link). -->
Private contact: *to be added.*
