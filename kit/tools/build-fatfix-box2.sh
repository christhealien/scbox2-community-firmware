#!/bin/sh
# Build the SC Box II's start-up check of its settings partition (scbox2-fatfix) from the shared panel source
# (agent/cmd/scbox2-fatfix).
#   Usage: tools/build-fatfix-box2.sh AGENT_SRC_DIR OUT_BINARY
# A program of its own, run by /etc/init.d/S38initdir before /mnt/config is mounted: it links no package of this
# module but its own and internal/cfgwin (the settings write window, standard library only), never the panel. Built like the panel and the SSH server
# (tools/build-agent-box2.sh, tools/build-sshd-box2.sh): the module renamed scbox2-agent in a scratch copy (so no
# embedded path names the SC Lite), -trimpath, no VCS stamp, vendored modules, the local Go toolchain only, static
# ARMv7. The source tree itself is not modified.
set -eu
SRC=$(cd "$1" && pwd); OUT=$2
case $OUT in /*) ;; *) OUT=$PWD/$OUT ;; esac
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
cp -a "$SRC"/. "$T"/
sed -i '1s/^module sclite-agent$/module scbox2-agent/' "$T/go.mod"
head -1 "$T/go.mod" | grep -qx 'module scbox2-agent' || { echo "go.mod: unexpected module line: $(head -1 "$T/go.mod")"; exit 1; }
find "$T" -path "$T/vendor" -prune -o -name '*.go' -type f -print | xargs grep -l '"sclite-agent/' 2>/dev/null |
	xargs -r sed -i 's#"sclite-agent/#"scbox2-agent/#g'
! grep -rq --include='*.go' --exclude-dir=vendor '"sclite-agent/' "$T" || { echo "an import of sclite-agent/ is left after the rename"; exit 1; }
cd "$T"
# it must not link the panel, nor anything outside the standard library (a package path with a dot is a module's)
MOD=$(GOFLAGS=-mod=vendor GOTOOLCHAIN=local go list -deps ./cmd/scbox2-fatfix | grep -e '^scbox2-agent' -e '^[^/]*\.' | sort | tr '\n' ' ')
[ "$MOD" = "scbox2-agent/cmd/scbox2-fatfix scbox2-agent/internal/cfgwin " ] || { echo "scbox2-fatfix links more than the standard library and internal/cfgwin: $MOD"; exit 1; }
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 GOFLAGS=-mod=vendor GOTOOLCHAIN=local \
	go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$T/scbox2-fatfix" ./cmd/scbox2-fatfix
cp "$T/scbox2-fatfix" "$OUT"
echo "built $OUT ($(wc -c < "$OUT") bytes)  sha256 $(sha256sum < "$OUT" | cut -c1-16)  ($(go version | cut -d' ' -f3))"
