#!/bin/sh
# Build the SC Box II SSH server (scbox2-sshd) from the shared panel source (agent/cmd/scbox2-sshd).
#   Usage: tools/build-sshd-box2.sh AGENT_SRC_DIR OUT_BINARY
# A program of its own, so SSH keeps working when the panel binary is damaged or crashing: it links only
# agent/internal/sshcore and agent/internal/authfile (shared with the panel) and the vendored SSH/SFTP/pty
# packages, never the panel. Built like the panel (tools/build-agent-box2.sh): the module renamed scbox2-agent in a
# scratch copy (so no embedded path names the SC Lite), -trimpath, no VCS stamp, vendored modules, the local Go
# toolchain only. The source tree itself is not modified.
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
# it must not link the panel: only these packages of this module
DEPS=$(GOFLAGS=-mod=vendor GOTOOLCHAIN=local go list -deps ./cmd/scbox2-sshd | grep '^scbox2-agent' | sort | tr '\n' ' ')
[ "$DEPS" = "scbox2-agent/cmd/scbox2-sshd scbox2-agent/internal/authfile scbox2-agent/internal/sshcore " ] ||
	{ echo "scbox2-sshd links more of the panel than it may: $DEPS"; exit 1; }
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 GOFLAGS=-mod=vendor GOTOOLCHAIN=local \
	go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$T/scbox2-sshd" ./cmd/scbox2-sshd
cp "$T/scbox2-sshd" "$OUT"
echo "built $OUT ($(wc -c < "$OUT") bytes)  sha256 $(sha256sum < "$OUT" | cut -c1-16)  ($(go version | cut -d' ' -f3))"
