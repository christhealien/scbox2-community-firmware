# SC Lite Firmware — Readiness, Security & Flashing Runbook

Prepared overnight on 2026-11-01. This covers the **sclite-agent** security audit (done and
verified here) plus the **firmware-side** steps to finish, harden, and package the image for an
end user who has no SSH access.

> **Scope note — read this first.** This cloud workspace contains only the **agent source**
> (`sclite-agent/`), the demo mock, and tuner logs. The **OEM firmware tree is not here** (it lives
> on your machine from the earlier Phase‑1/Phase‑2 work, and I can't reach your computer or the
> miner from this container). So: the agent audit below is **done and verified**; the firmware-side
> sections are a **runbook you execute against the real tree**, written from what the agent encodes
> plus our checklist. Anything I couldn't verify is marked **[verify on tree]**.

---

## 1. Status at a glance

| Area | State |
|---|---|
| Agent: cloud / telemetry / phone-home | ✅ none (localhost + opt-in Telegram/Discord only) |
| Agent: auth, secrets, crypto | ✅ hardened (fixes applied) |
| Agent: ports / listeners | ✅ one port, documented |
| Agent: shell exec / injection | ✅ clean |
| Agent: factory-default lockout | ✅ added server-side (admin/admin can't act until changed) |
| Firmware: cloud daemons removed | **[verify on tree]** — method below |
| Firmware: old OEM UI removed | **[verify on tree]** — method below |
| Firmware: unneeded ports closed | **[verify on tree]** — list below |
| Firmware: runs from internal flash, SD for upgrade only | **[verify on tree]** — section 5 |
| End-user flashable image | **[build on tree]** — section 6 |

**Latest audited agent binary:** `sclite-agent-arm` (md5 in the chat message that delivered it).
Rebuild from source with the command in section 7.

---

## 2. Agent security audit — findings & fixes (all applied + verified tonight)

**No cloud / no phone-home.** The only network egress in the agent is:
- `127.0.0.1:4028` — the miner cgminer/bfgminer API (local).
- `127.0.0.1:80` — the stock miner web backend, for `/mcb` + `/dbg` (local).
- `api.telegram.org` and a Discord webhook — **only** if the owner configures them (opt-in notifications).
No analytics, update-checks, or vendor callbacks. The local-IP helper no longer references a public
address (changed to use the interface list first).

**Fixes applied this pass:**

1. **`notify.json` was world-readable (0644)** though it holds the Telegram bot token + Discord
   webhook. → now **0600** in a **0700** dir, matching `auth.json`.
2. **No slow-request / body limits on the HTTP server.** → added `ReadHeaderTimeout: 10s`,
   `IdleTimeout: 120s`, `MaxHeaderBytes: 64 KiB`, and a **1 MiB request-body cap** (prevents
   Slowloris and OOM on a low-RAM miner). `WriteTimeout` stays 0 so the live log/SSE stream isn't cut.
3. **Factory default (admin/admin) could act before the owner changed it.** The forced change was
   UI-only. → now **enforced server-side**: while `must_change` is set, a valid session may hit only
   `/api/passwd` and `/api/acknowledge`; every other API returns **403**. Verified end-to-end
   (login → `/api/control` 403 → change password → `/api/control` 200).

**Already-good, confirmed:**
- Login password: **PBKDF2-HMAC-SHA256**, 60k iterations, random salt, **constant-time** compare;
  600 ms penalty on bad login (brute-force brake).
- Session: **HMAC-SHA256 signed** cookie, `HttpOnly`, `SameSite=Strict`, `Secure` over https, 24 h
  expiry (also bounded by uptime, so a clock set forward or back can't stretch it). Signing out revokes
  the session on the miner (kept across a panel restart).
- CSRF: `SameSite` alone isn't relied on (another host on the same site, e.g. `evil.fritz.box`, is
  same-site). On the SC Box II every state-changing request must carry a same-origin `Origin`, or
  `Sec-Fetch-Site: same-origin`, or the page's own `X-Requested-With: panel` header (which a page
  elsewhere can't add without a CORS preflight the panel never answers); anything else is refused.
- SSH/root password: hashed in Go as **SHA-512 crypt** (`$6$`, 16-char salt) and written straight to
  `/etc/shadow`; plaintext never touches a shell. Applied live, never stored.
- Secrets at rest: `auth.json` (cookie secret + stock admin pw) is **0600 / 0700**.
- Shell use: only `mount` remounts, `sync`, `reboot`, `killall`, `passwd` (password via **stdin**),
  and `hostname` (regex-validated **and** single-quoted). No command injection surface.
- HTTP headers: `X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: no-referrer`, and a tight
  **CSP** (`default-src 'self'`, no external connect/img/frame). No `pprof`/debug endpoints.
- Export endpoints: strict `kind` **whitelist** — no path traversal.
- Rootfs writes (SSH pw, hostname, boot script) always **remount ro again** afterward, fail-safe.

**HTTPS (SC Box II):** every panel port, the main one (8080) included, accepts both http and https
(the first byte of each connection decides), and port 443 serves https only. The certificate is the
miner's own, made on first start (self-signed: the browser warns once). Settings -> Network ->
**HTTPS only** (off by default) redirects plain http to https (port 80 to 443); loopback stays plain
for the miner's own tools. With it off, plain http on the LAN remains possible, and the session cookie
then travels in cleartext on the local network. The panel and SSH answer only the local network unless
"Allow access from outside the local network" is switched on, which needs the panel password again
(and a settings file can't switch it, or HTTPS only or the ports, on import).

---

## 3. Firmware cloud-removal — verify on the real tree

The agent itself has no cloud code, so "remove the cloud stuff" means the **OEM daemons** stay
removed by the Phase‑1 script. On the built image, confirm:

```sh
# 1) no vendor cloud/telemetry process starts at boot — inspect init
ls -la /etc/init.d/ /etc/rc.d/ 2>/dev/null
# look for and disable anything like: cloud, mqtt, report, agent (vendor), ntp-to-vendor, stat*, phone*
# 2) nothing dials out on boot — grep the init scripts and /usr/bin for hardcoded hosts
grep -rIE 'https?://|[0-9]{1,3}(\.[0-9]{1,3}){3}' /etc/init.d /usr/bin 2>/dev/null \
  | grep -vE '127\.0\.0\.1|localhost|0\.0\.0\.0'
# 3) runtime check on a booted unit: nothing established to the WAN except what you expect
netstat -tunp 2>/dev/null | grep -vE '127\.0\.0\.1|:0 '   # or: ss -tunp
```
Disable (don't just kill) each vendor cloud unit by removing/renaming its `S##` init symlink so it
can't come back after reboot. Keep the Phase‑1 removal **reversible** (you already have the backup).

---

## 4. Ports — what to keep, what to close  **[verify on tree]**

| Port | What | Ship as |
|---|---|---|
| agent UI (`:8080`, or `:80` if you move it) | the new control panel | **LAN-exposed**, behind login |
| cgminer/bfgminer API `:4028` | unauthenticated control of the miner | **bind 127.0.0.1 only** |
| stock web backend `:80` | `/mcb` + `/dbg` the agent proxies | **bind 127.0.0.1 only** (LAN sees only the agent) |
| SSH / dropbear `:22` | root shell | **off by default** for end users, or on behind the SHA-512 password |
| telnet `:23` | legacy shell | **off** |
| any vendor cloud/mqtt port | telemetry | **off** (removed in §3) |

How:
- **Miner API to localhost** — in the miner config (the agent manages `bfgminer.json`, but the
  **API bind** is the OEM's launch args/config): ensure `api-listen` is on and `api-allow` is
  `W:127.0.0.1` (or the API address is `127.0.0.1:4028`). The agent already talks to `127.0.0.1:4028`,
  so localhost-only is compatible. This is the single most important port change — the cgminer API
  has **no auth**; anyone on the LAN who can reach it can restart/stop/reconfigure the miner.
- **Stock web backend to localhost** — if the OEM web server can bind `127.0.0.1:80`, do that and run
  the agent as the only LAN-facing port. If it can't, firewall it:
  ```sh
  iptables -A INPUT -p tcp --dport 80 ! -s 127.0.0.1 -j DROP   # only localhost reaches :80
  ```
- **SSH** — for the end-user build, ship dropbear disabled (remove its init `S##` symlink) or leave it
  on behind the strong password. "Someone who doesn't have SSH unlocked" should still be able to flash
  and run everything through the web UI; SSH is not required by the agent at runtime.

Verify on a booted unit: `ss -tlnp` should show the agent's port on `0.0.0.0`, and `:80`/`:4028`
on `127.0.0.1` only (or firewalled).

---

## 5. Old UI removal + SD-card policy  **[verify on tree]**

**Old OEM UI cleanly gone.** The agent is the LAN UI. Make sure the stock *frontend* (its static
HTML/JS) isn't still served to the LAN:
- If the agent runs on its own port and `:80` is localhost-only/firewalled (§4), the LAN never sees
  the old UI even though the backend stays for `/mcb` + `/dbg`.
- If you want the old static files physically removed, delete the OEM web root's HTML/JS bundle but
  **keep the backend handlers** (`/mcb`, `/dbg`, `/user/login`) the agent depends on. Confirm those
  three still answer on `127.0.0.1:80` after removal.
- Boot the image and load the miner's IP in a browser: you should get **only** the SC Lite agent,
  never the OEM UI.

**SD card: upgrades/flashing only — never the running rootfs.** Confirm the built firmware runs
entirely from **internal eMMC/flash**, and that the SD card is used only as an upgrade/recovery
medium:
- The agent's own data lives on `/usr/config` (`/dev/mmcblk0p2`, internal) and the rootfs is internal
  `/dev/root` — **not** the SD. Good, but verify the image doesn't mount the SD as a working volume:
  ```sh
  mount | grep -iE 'mmcblk1|sd[a-z]|/media|/mnt'      # SD should NOT be a rootfs/overlay/config mount
  cat /proc/cmdline                                   # root= should be internal (mmcblk0* / ubi*), not the SD
  grep -i mmcblk1 /etc/fstab /etc/init.d/* 2>/dev/null # no auto-mount of the SD as a data volume
  ```
- If anything mounts the SD at boot for config/overlay, move it to internal flash. The SD should only
  be read by the **upgrade/flash** path (the recovery/`.ius`/image-writer flow), so a user can pull the
  card after flashing and the miner runs fine without it.

---

## 6. End-user flashable image (no SSH needed)  **[build on tree]**

Goal: a user with zero SSH can take a board to your firmware and back. Two delivery paths; pick what
the SC Lite's bootrom supports (from the Phase‑2 work):

**A. SD recovery image (simplest for end users).**
1. Assemble the Phase‑2 rootfs with: cloud daemons removed (§3), old UI handled (§5), ports set (§4),
   the **audited agent binary** placed at its path, and `S48sclite` installed so it auto-starts
   (the agent writes this itself via `-install-service`; see §7).
2. Pack it into the SD recovery image the InfoTM/OEM bootrom expects (the format your Phase‑2
   produced — `.img` written with `dd`/Balena Etcher, or the vendor's SD-update layout).
3. End user: write the image to an SD, insert, power-on to flash internal eMMC, **remove the SD**,
   reboot. First boot: browse to the miner IP → forced password change (admin/admin is locked until
   then) → disclaimer → done. No SSH at any point.

**B. `.ius` / web-upload update (if the stock recovery web accepts it).**
- Package the same rootfs as the vendor `.ius`/update blob and let the user upload it through the
  stock recovery page. This keeps SD out of it entirely but depends on the signed-update constraints
  you investigated in task #7.

**First-boot UX to verify on the image:** fresh unit → `admin` / `admin` → **must-change screen**
(server now blocks everything else) → set password (optionally "match SSH") → acknowledge disclaimer
→ dashboard. The default credential is safe because §2.3 locks it server-side until changed.

**Ship a one-page quick-start** with: how to find the miner IP, the default `admin`/`admin` + "you'll
be forced to change it," and "SD card is only for flashing — remove it after."

---

## 7. Build & install the agent (reproducible)

```sh
# static ARMv7 binary, no libc dependency — drops onto the miner as-is
cd sclite-agent
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags="-s -w" -o sclite-agent-arm .

# sanity (run on a dev box)
go vet ./... && go test ./...

# on the miner (or baked into the image): install the boot service, optionally seed the stock pw
./sclite-agent-arm -install-service                 # writes /etc/init.d/S48sclite (remounts ro after)
./sclite-agent-arm -install-service -miner-pass XXX # also stores the stock admin pw for /dbg access
```
- Boot order is correct: **`S48sclite`** starts the agent before **`S901eventhub`** brings the miner
  up, so the UI is reachable while mining initialises.
- Runtime paths: config/data under `/usr/config/sclite/` (internal flash), logs streamed from
  `/var/log/messages`. Nothing is written to the SD.
- Default listen is `:8080`; the owner can change the port in Settings (persisted to
  `/usr/config/sclite/net.json`, which overrides the boot flag).

---

## 8. Pre-publish checklist

- [ ] Flash the latest **audited** agent binary into the image (not an older build).
- [ ] §3 cloud daemons confirmed gone; `netstat` shows no WAN callouts at boot.
- [ ] §4 miner API `:4028` + stock web `:80` on **127.0.0.1**; SSH off (or behind strong pw); telnet off.
- [ ] §5 old OEM UI not reachable on the LAN; SD **not** mounted as rootfs/config/overlay.
- [ ] §6 image flashes from SD, runs from internal flash, works with the **SD removed**.
- [ ] First boot: admin/admin → forced change (verified blocked server-side) → disclaimer → UI.
- [ ] Quick-start doc shipped.
- [ ] You've done a real-hardware pass: ceiling probe, a short tune, a reboot, and a cold boot.

---

### What I verified vs. what still needs the tree
**Verified here (code I have):** every item in §2, including the new `must_change` lockout
(live-tested), the 0600 perms, and the server limits — all compile, vet, `go test -race`, and the
page JS passes `node --check`.
**Needs the real firmware tree / a booted unit (I can't reach it from this container):** §3–§6 —
the OEM daemon/UI removal, the port binds, the SD-mount check, and the image build. The commands
above are ready to run against it.
