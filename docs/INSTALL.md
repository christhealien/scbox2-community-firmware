# Installing the SC Box II Community Firmware

The first install is by **microSD card**. The stock web upload checks an RSA signature and rejects this
firmware. The SD burn checks no signature, only the container's format (the bootloader checks the `+ius` magic
and the descriptor layout; the header CRC is checked by the OEM's `upgrade` tool), so the OEM's normal SD
burn procedure works. After the first install, update from the panel.

> [!WARNING]
> Custom firmware may void your warranty. Flashing carries risks, including hardware damage. The hardware
> controls in this release were tested on one SC Box II. Use it at your own risk.

## Before you start

- A **microSD card** of any size: the image is about 30 MiB. The card is only needed for the flash.
- **balenaEtcher** to write the image.
- From the [latest release](https://github.com/christhealien/scbox2-community-firmware/releases/latest):
  `scbox2-sd-v0.5.1.img.xz` and `SHA256SUMS`.
- An **Ethernet** connection for the miner. The board has no Wi-Fi chip.
- A copy of **the stock SC Box II image**, kept safe, so you can go back to stock later.
- The **stock fans** fitted. The OEM fan control is calibrated for them.

> [!IMPORTANT]
> **A flash wipes the config partition.** Your password, pools, hostname, schedule and preferences are erased.
> If the miner already runs this firmware, export your settings first: **Settings → Firmware update → Settings
> backup** (see [Settings backup](#settings-backup)).

## 1. Check the download

Check the image against `SHA256SUMS`:

| System | Command |
|---|---|
| Linux | `sha256sum -c SHA256SUMS --ignore-missing` (must say `OK`) |
| macOS | `shasum -a 256 scbox2-sd-v0.5.1.img.xz`, then compare with its line in `SHA256SUMS` |
| Windows | `certutil -hashfile scbox2-sd-v0.5.1.img.xz SHA256`, then compare |

If it doesn't match, download it again. Never flash an image that fails the check.

## 2. Write the card

Open **balenaEtcher**, select `scbox2-sd-v0.5.1.img.xz` (Etcher unpacks it; other tools need `xz -dk` or 7-Zip
first), select the microSD card, and click **Flash**.

## 3. Burn it onto the miner

1. Power the miner **off**.
2. Insert the microSD card.
3. Power the miner **on**. No button is needed: the bootloader finds the card and writes the firmware to
   internal storage.
4. While it writes, the **SYS LED flashes blue and red**. This takes about **4 minutes**.
5. When only the **blue** SYS LED flashes, the write is done. **Remove the card.** The miner starts the new
   firmware.

> [!CAUTION]
> Don't remove the card or cut the power while the red LED is still flashing. The power LED stays green throughout.

## 4. First boot and sign-in

1. Give the miner a minute or two. It gets an address from your router by DHCP on Ethernet.
2. Find the miner in your router's list of devices and open `http://<miner-ip>/` (port 80). The panel also
   answers on port **8080** (HTTP and HTTPS) and on **443** (HTTPS). The miner makes its own self-signed
   certificate on first start, so your browser warns once.
3. Sign in with **`admin` / `admin`**. You **must** choose your own password (at least 8 characters) and read the
   risk notice before the panel does anything else.
4. The same username and password work for SSH: `ssh admin@<miner-ip>`. Over SSH, a miner still on `admin` /
   `admin` also asks for a new password before it gives you a shell.

**SSH** gives a **root shell** (`/bin/sh`) and runs commands (`ssh admin@<miner-ip> 'cmd'`). Files are copied over
**SFTP** only: `sftp` works, and so does `scp` from OpenSSH 9 or newer (it uses SFTP); the legacy scp protocol
(`scp -O`, older clients) isn't supported. No port or agent forwarding.

> [!NOTE]
> **Local network only.** The panel and SSH answer only devices on your local network (private addresses,
> link-local, VPN ranges such as 100.64/10, and the miner's own subnet; the same for IPv6). To reach the miner
> from elsewhere, switch on **Settings → Network & SSH → "Allow access from outside the local network"** from a
> device on your LAN. Five wrong passwords from one address block it for 10 minutes. More in
> [SECURITY.md](SECURITY.md).

## 5. Set up the miner

After a flash, set these again:

| What | Where |
|---|---|
| Password | Asked at first sign-in. Later: **Settings → Account** |
| Pool | **Pools** tab. Mining starts by itself within about a minute after you save a pool, and again at every boot. |
| Hostname | **Settings → Network & SSH**. Applied at boot, before the network starts, so your router sees it. |
| Panel port | **Settings → Network & SSH → Web UI port** (1024–65535; 8080 by default) |
| Time zone, time servers, clock | **Settings → Time**. The miner starts on the OEM's China time (UTC+8); at first sign-in the panel sets your browser's time zone. Without internet, set the clock by hand there (**Use this browser's time**). |
| Notifications | **Settings → Notifications** |
| Or most of it at once | **Settings → Firmware update → Settings backup → Import settings** (see [Settings backup](#settings-backup)); then reboot. |

On a fresh install there is no pool, so the miner does not hash until you add one. The hashrate builds up over
the first few minutes while the chips start.

## 6. Updating from the panel

Later releases install from the panel, without an SD card, and keep your settings. **Settings → Firmware
update** has three steps:

1. **Get the update file.** Click **Check for updates** (the miner looks up the latest release on GitHub), or
   **upload** a release's `scbox2-firmware-vX.Y.Z.ius`.
2. **Check the file.** This runs by itself and writes nothing. The panel checks that the file is complete and
   made for the SC Box II, that it is **genuine** (its SHA-256 matches the release's `SHA256SUMS`, looked up on
   GitHub, also for an uploaded file), and that the miner is ready for it. If the miner can't reach GitHub, it
   asks you to paste the file's line from the release's `SHA256SUMS`.
3. **Install.** Tick the four confirmations:
   - I saved my settings
   - I have an SD card with the SC Box II firmware ready
   - The power is stable
   - I understand that if the install fails, the miner only comes back with the firmware on an SD card

   then enter your **panel password** and click **Install … and restart**. Mining stops for about 4 minutes.
   Keep the power on and the page open.

When **Check for updates** finds a newer release, it also offers **Download and install** in one go, with the
same four confirmations and the password. It stops before anything is written if a step fails.

The panel writes only the system partition, reads it back to compare, and reboots. If a write or compare fails,
it writes the old system back. The bootloader and the settings storage are never written.

There is **no automatic roll back** on this model after a successful install. If something goes wrong, the SD
card is the way back (next section). Prepare the card **before** you update.

The update source is this repository by default. To use your own build or a fork, change it under **Update
source** (asks for the panel password).

## 7. Recovery with an SD card

The same steps as **How to recover with an SD card** on the panel's update screen:

1. On a computer, download `scbox2-sd-vX.Y.Z.img.xz` and `SHA256SUMS` from the release page.
2. Write it to a microSD card with **balenaEtcher** (select the `.img.xz`, select the card, Flash).
3. Power the miner **off**, insert the card, power it **on**.
4. While it writes, the **SYS LED flashes blue and red** (about 4 minutes). Don't remove the card or cut the
   power while red is flashing.
5. When only the **blue** SYS LED flashes, the write is done: **remove the card**. The miner starts the new
   firmware.
6. Find the miner in your router's list of devices, open `http://<miner-ip>/`, sign in with `admin` / `admin`
   and set your password.
7. Set your pool, hostname and other settings again, or import the settings file you exported (Settings →
   Firmware update → Settings backup). Either way, **enter your pools again**: they aren't in the file.

## Settings backup

**Settings → Firmware update → Settings backup** has **Export settings** and **Import settings**.

| In the file (restored by an import) | Not in the file |
|---|---|
| Network: panel port, SSH on/off and port, DNS, the access switches (outside access, HTTPS only, ports 80/443), display units | **Pools** (they live in the OEM miner config): enter them again |
| Fan curves | Hostname, MAC address mode |
| Presets | Time zone and time servers |
| Schedule | Password (and the miner's keys and certificate) |
| Notifications, **without** tokens and webhook URLs | Hashrate history, power calibration |

An import keeps notification secrets already stored on the miner when the file has none. Reboot after an import
so every setting takes effect.

## Recovery: serial console

If the network or the panel can't be reached, there is a physical way in: a UART (serial) header inside the
miner. Open the case, connect a 3.3 V USB-to-serial adapter, open a terminal at **115200 8N1**, and log in as
**`root`** / **`scbox2-recover`**.

This is a documented recovery login, **the same on every unit**. It works only on the serial pins, not over SSH
or the web, and it is separate from your panel password. It needs physical access to the board.

## SSH host key warning after a re-flash

Each flash makes a new SSH host key. If your computer refuses to connect with *"REMOTE HOST IDENTIFICATION HAS
CHANGED"*, remove the old key and connect again:

```sh
ssh-keygen -R <miner-ip>
```

## Going back to stock

1. Export your settings if you want to keep them (the flash wipes them).
2. Get **the stock SC Box II firmware** from the OEM's support or download page for the SC Box II.
3. If you have it as an SD image, write it with balenaEtcher. If you have the `.ius` file, turn it into an SD
   image first (the container goes at 16 MiB of a 64 MiB image):

   ```sh
   python3 kit/tools/ius.py card stock-scbox2.ius stock-scbox2-sd.img
   ```

   and write `stock-scbox2-sd.img` with balenaEtcher.
4. Power off, insert the card, power on, wait for the burn to finish (blue only), remove the card.

An SD flash rewrites the whole firmware from the card. This firmware leaves the OEM's bootloader and kernel
unchanged and replaces only the system part, so a stock card always puts the miner back as it came.

## Troubleshooting

| Problem | What to do |
|---|---|
| The SYS LED never goes to blue only (still blue and red after 10 minutes, or no flashing at all) | Power off, take the card out and write it again; check the image against `SHA256SUMS`. Try another microSD card. Leave the card in from power-on until blue only. |
| The miner doesn't show up on your router | Check the Ethernet cable and the port lights (there is no Wi-Fi). Give it 2–3 minutes after the burn. Look for a new device (by MAC address) in the router's DHCP list. If it still doesn't appear, use the [serial console](#recovery-serial-console) or flash again. |
| The browser can't open the panel | Use a device on the same network ([local network only](SECURITY.md)). Try `http://<miner-ip>:8080/` too, or your own panel port. |
| "Too many wrong passwords" | Wait 10 minutes; the block is per address. |
| No time / wrong times without internet | **Settings → Time → Set the clock by hand**. |
