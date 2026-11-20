# Troubleshooting

## Sending the system log to your computer

If the miner freezes or restarts by itself, the panel can send every system-log line to your computer as it
happens, so the last lines before a freeze are kept even when the miner can't save them. Leave it off otherwise.

The setting is in **Settings → Troubleshooting**, the last panel of the Settings tab, on every SC Box II build.
It is off by default.

> [!WARNING]
> The log is sent **unencrypted** over the local network (UDP syslog). Point it only at your own computer on the
> same network. It takes an IPv4 address only (no host names); the port is 514 by default.

### What it sends

Every line of the system log: the kernel's and the programs' messages, and while a tuner run or the clock-ceiling
probe runs, a breadcrumb for each call the panel makes to the mining program, its backend and the settings
partition. Each line goes over UDP to that computer as it is logged, so a freeze can't take the last lines with it.
The setting is kept across restarts. Nothing is sent anywhere else.

### How to receive it on your computer

1. **Start a receiver.** It needs Python 3 and nothing else. Leave its window open. Every line is printed and also
   kept in `scbox2-syslog.txt`.

   Linux or Mac, in a terminal:

   ```sh
   sudo python3 -u -c "import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('0.0.0.0',514));[print(s.recv(4096).decode(errors='replace').strip(),flush=True) for _ in iter(int,1)]" | tee scbox2-syslog.txt
   ```

   Windows, in PowerShell (no `sudo`; allow Python through the firewall when Windows asks):

   ```powershell
   python -u -c "import socket;s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('0.0.0.0',514));[print(s.recv(4096).decode(errors='replace').strip(),flush=True) for _ in iter(int,1)]" | tee scbox2-syslog.txt
   ```

   On Linux with a firewall on, let the log in first:

   ```sh
   sudo ufw allow 514/udp
   ```

2. **Find your computer's IPv4 address:** `ip -4 addr` (Linux), `ipconfig` (Windows) or System Settings → Network
   (Mac). It looks like `192.168.1.20`.
3. **Enter it in Settings → Troubleshooting** as `address:514` (for example `192.168.1.20:514`) and press
   **Save**. The panel shows "On: every line is also sent to …". If it shows a warning instead, the miner couldn't
   restart its system log for the setting: press Save again (the next boot applies it too).
4. **Check that lines appear** in the receiver's window. Logging in prints some; or, signed in over SSH, run
   `logger hello` on the miner.
5. **Reproduce the problem** and leave the receiver running.

To turn it off, empty the field and press **Save**.

### What to attach to a bug report

- The **last ~100 lines before the freeze** from `scbox2-syslog.txt`.
- The panel's **private log export**: **Log → For sending, private details out (.zip)**, taken after the miner is
  back. It has the pools' addresses and accounts, the miner's name, IP and MAC addresses and notification secrets
  taken out. If **Settings → Recovery → Keep the system log across restarts** was on, it also holds the system log
  from before the last restart.

The receiver's file is not filtered: look through those lines before you post them and take out anything you
don't want to share (for example your network's addresses).
