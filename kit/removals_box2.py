#!/usr/bin/env python3
"""
B8: SC Box II Wi-Fi + OpenSSH removal groups for build_box2.py.

The DESIGN removes the whole Wi-Fi stack (the board has no Wi-Fi chip; a bare footprint only) and stock
OpenSSH (the panel ships its own SSH; stock sshd shares baked-in host keys).  build_box2.py's
``apply_removals()`` already has an OpenSSH group (``G5_openssh``) but NO Wi-Fi group, so the Wi-Fi binaries
would otherwise stay in the squashfs and waste space.  This module supplies:

  * ``WIFI_GROUP``   -- every Wi-Fi file to drop (minus ``libwifi.so``, which minerd links -- KEEP it).
  * ``OPENSSH_EXTRA`` -- files the stock Box II has that A2's G5_openssh list misses (the sftp subsystem).
  * ``merge_into(REMOVE_GROUPS)`` -- fold both into build_box2.py's REMOVE_GROUPS dict in place.

Reachability was proven (see REPORT.md / tests): with the Wi-Fi + OpenSSH files removed, NO kept executable
has a DT_NEEDED on any removed library, and the only consumer of ``libwifi.so`` is ``/usr/bin/minerd``
(kept).  ``libnl-*`` is used only by the removed hostapd / wpa_supplicant, so the whole libnl family is
reclaimed too.
"""

# The Wi-Fi stack.  libwifi.so is deliberately NOT here (minerd links it; it is in build_box2's
# NEVER_REMOVE guard).  libnl-* go because only hostapd/wpa_supplicant referenced them.
WIFI_GROUP = [
    # daemons / tools
    "/usr/bin/wifi",                # the OEM open-AP "wifi" daemon (inittab line removed by B8)
    "/usr/bin/ap_state",
    "/usr/bin/hostapd_cli",
    "/usr/sbin/hostapd",
    "/usr/sbin/wpa_supplicant",
    "/usr/sbin/dnsmasq",            # only started for the Wi-Fi soft-AP's DHCP
    # configs / hook / blobs
    "/etc/wpa_supplicant.conf",
    "/root/wifi",                   # dnsmasq.conf / hostapd.conf / wpa_supplicant.conf templates
    "/wifi",                        # empty Wi-Fi firmware dir (/wifi/firmware/Readme)
    "/usr/share/dhcpcd/hooks/10-wpa_supplicant",
    # libnl family (hostapd/wpa_supplicant only; nothing kept links it)
    "/etc/libnl",
    "/usr/lib/libnl-3.so", "/usr/lib/libnl-3.so.200", "/usr/lib/libnl-3.so.200.18.0",
    "/usr/lib/libnl-genl-3.so", "/usr/lib/libnl-genl-3.so.200", "/usr/lib/libnl-genl-3.so.200.18.0",
    "/usr/lib/libnl-route-3.so", "/usr/lib/libnl-route-3.so.200", "/usr/lib/libnl-route-3.so.200.18.0",
    "/usr/lib/libnl-nf-3.so", "/usr/lib/libnl-nf-3.so.200", "/usr/lib/libnl-nf-3.so.200.18.0",
    "/usr/lib/libnl-idiag-3.so", "/usr/lib/libnl-idiag-3.so.200", "/usr/lib/libnl-idiag-3.so.200.18.0",
]

# OpenSSH files the stock Box II ships that A2's G5_openssh list omits.
OPENSSH_EXTRA = [
    "/usr/libexec/sftp-server",     # the sftp subsystem binary (sshd_config Subsystem sftp)
]


def merge_into(remove_groups):
    """Add the Wi-Fi group and the OpenSSH extras into build_box2.py's REMOVE_GROUPS dict, in place."""
    remove_groups["G7_wifi_stack"] = list(WIFI_GROUP)
    g5 = remove_groups.get("G5_openssh")
    if g5 is not None:
        for p in OPENSSH_EXTRA:
            if p not in g5:
                g5.append(p)
    else:
        remove_groups["G5_openssh"] = list(OPENSSH_EXTRA)
    return remove_groups
