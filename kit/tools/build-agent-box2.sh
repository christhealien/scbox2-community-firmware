#!/bin/sh
# Build the SC Box II panel (scbox2-agent) from the shared panel source.
#   Usage: [TAGS=scbox2,public] tools/build-agent-box2.sh AGENT_SRC_DIR OUT_BINARY
#   TAGS: the build tags (default scbox2, the owner's commissioning build; scbox2,public for the public release).
# The same source builds the SC Lite panel by default; -tags scbox2 selects brand_scbox2.go (the Box II's
# paths and names). The Go module is renamed scbox2-agent in a scratch copy so the binary's embedded source
# paths carry the Box II name too; the source tree itself is not modified. Reproducible: -trimpath, no VCS
# stamp, vendored modules, the local Go toolchain only.
set -eu
SRC=$(cd "$1" && pwd); OUT=$2; TAGS=${TAGS:-scbox2}
case $OUT in /*) ;; *) OUT=$PWD/$OUT ;; esac # a relative output path is taken from where the script was run
case ",$TAGS," in *,scbox2,*) ;; *) echo "TAGS must include scbox2 (got: $TAGS)"; exit 1 ;; esac
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
cp -a "$SRC"/. "$T"/
sed -i '1s/^module sclite-agent$/module scbox2-agent/' "$T/go.mod"
head -1 "$T/go.mod" | grep -qx 'module scbox2-agent' || { echo "go.mod: unexpected module line: $(head -1 "$T/go.mod")"; exit 1; }
# the panel's own packages (internal/sshcore, internal/authfile) are imported by the module's name: follow the rename
find "$T" -path "$T/vendor" -prune -o -name '*.go' -type f -print | xargs grep -l '"sclite-agent/' 2>/dev/null |
	xargs -r sed -i 's#"sclite-agent/#"scbox2-agent/#g'
! grep -rq --include='*.go' --exclude-dir=vendor '"sclite-agent/' "$T" || { echo "an import of sclite-agent/ is left after the rename"; exit 1; }
cd "$T"
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 GOFLAGS=-mod=vendor GOTOOLCHAIN=local \
	go build -tags "$TAGS" -trimpath -buildvcs=false -ldflags="-s -w" -o "$T/scbox2-agent" .
cp "$T/scbox2-agent" "$OUT"
echo "built $OUT (tags $TAGS)  sha256 $(sha256sum < "$OUT" | cut -c1-16)  ($(go version | cut -d' ' -f3))"
