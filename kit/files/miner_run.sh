#!/bin/sh
# Deterministic SC miner launcher for the SC Box II firmware.
#
# Why this exists: on this firmware xminerd's own launch never starts the miner. xminerd picks the miner by
# matching the unit's (Model, Algo) from /root/product.json against a built-in table; on the Box II that
# match falls through to the table's catch-all ("bfgminer" -> /usr/bin/bfgminer) and the exec never reaches
# the miner wrapper (confirmed on hardware: a debug-logging wrapper at both /usr/bin/intminer and
# /usr/bin/bfgminer is never invoked at boot, and no miner process appears). The miner itself works perfectly
# when started directly: /usr/bin/bfgminer -c <config> (the wrapper rewrites -c to --config and runs the
# guarded miner). So we start it here, deterministically, instead of relying on xminerd's lookup.
#
# This cooperates with the stock stack rather than replacing it: fanctrl still controls the fans, and minerd
# still manages pools, talks to the miner API (:4028) and kills the miner on a pool change -- when it does,
# this loop relaunches with the updated config, exactly as xminerd would on stock. It never starts a second
# miner if one is already running.
#
# One miner only: Goldshell's xminerd also starts the miner once its network check passes (~40 s after boot).
# The wrapper allows a single running miner (an flock the miner holds for its life); whichever start comes
# second exits at once with status 75. This loop then simply waits and checks again.
#
# Start tries: the wrapper runs the guarded miner copy (/usr/libexec/scbox2/hook/intminer: Goldshell's miner
# plus this firmware's fixes) at most 3 starts in a row (markers /tmp/scbox2-guard.N, cleared by the panel once
# mining has run 2 minutes) and then falls back to the unmodified stock miner. Those tries are there to catch
# the guarded copy CRASHING (killed by a signal). A miner that exits by itself within a minute -- "No servers
# were found" because the network or the pool isn't up yet -- is not that, so its try is given back here.
# Otherwise two or three early boot starts (DHCP still running) would use up every try and the boot would end
# on the stock miner, without the fixes. For the same reason the first start waits for a default route.
#
# Run from inittab as  ::respawn:/usr/libexec/scbox2/miner-run  -- the internal loop paces relaunches so a
# miner that keeps exiting (e.g. no pool yet, or chips not ready) can't tight-loop.

CFG=/mnt/config/bfgminer/bfgminer.json
QUICK=60	# seconds: a clean exit sooner than this gives its start try back

miner_running() {
	for p in /proc/[0-9]*; do
		c=$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)
		case "$c" in
		*bfgminer*|*intminer*) return 0 ;;
		esac
	done
	return 1
}

have_route() {	# a default route (destination 00000000) exists: DHCP (or a static setup) is done
	awk '$2 == "00000000" { f = 1 } END { exit !f }' /proc/net/route 2>/dev/null
}

uptime_s() { awk '{ print int($1) }' /proc/uptime; }	# monotonic: unaffected by the clock being set

give_back_try() {	# remove the newest start-try marker (the one this start took)
	m=$(ls -t /tmp/scbox2-guard.? /tmp/scbox2-hook.? 2>/dev/null | head -n 1)
	[ -n "$m" ] && rm -f "$m"
}

# the first start waits for the network (at most 2 minutes; a static or odd setup still gets its miner)
i=0
while ! have_route && [ $i -lt 24 ]; do sleep 5; i=$((i + 1)); done

while true; do
	# Start only when a pool is configured and no miner is already up (hand start, or a lingering one).
	if [ -s "$CFG" ] && grep -q '"url"' "$CFG" && ! miner_running; then
		t0=$(uptime_s)
		/usr/bin/bfgminer -c "$CFG"   # blocks until the miner exits, then we loop and relaunch
		rc=$?
		ran=$(($(uptime_s) - t0))
		# rc >= 128: killed by a signal (crash, or minerd/the panel stopping it) -- the try stays used.
		# rc 75: the wrapper found a miner already running (xminerd started one) and took no try.
		if [ $rc -lt 128 ] && [ $rc -ne 75 ] && [ $ran -lt $QUICK ]; then
			give_back_try
		fi
	fi
	sleep 10
done
