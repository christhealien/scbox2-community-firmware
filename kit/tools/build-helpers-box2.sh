#!/bin/sh
# Rebuild the SC Box II ARM helpers the same way build_box2.py expects them, and compare them with (or write
# them over) a given output dir.
#   Usage: tools/build-helpers-box2.sh STOCK_LIB_DIR OUT_DIR [--write]
#   STOCK_LIB_DIR holds libuClibc-0.9.33.2.so and libpthread-0.9.33.2.so from the stock SC Box II /lib (only
#   linked against: they set NEEDED and resolve symbols; nothing from them is copied in). OUT_DIR receives
#   scbox2_bind.so.bin, scbox2_wrap.bin, scbox2_chip.so.bin.
#
# Helpers:
#   scbox2_bind.so   -- files/scbox2_bind.c (raw syscalls, arch-neutral; R07/R12 proved it on the Box II
#                       minerd:80 & intminer:4028). Loopback-binds the stock daemons' TCP listeners.
#   scbox2_wrap.bin  -- files/scbox2_wrap.c, installed as /usr/bin/intminer and /usr/bin/bfgminer: config
#                       store /mnt/config/scbox2, -c -> --config normalization, native --no-pool-redirect.
#   scbox2_chip.so   -- files/scbox2_chip.c (Box II offsets + two-phase hook(dev,frame,phase)).
#
# Byte-identical output needs Ubuntu clang/lld/llvm-strip 18.1.3; any other version is refused.
set -eu
KIT=$(cd "$(dirname "$0")/.." && pwd); F=$KIT/files; LIB=$1; OUT=$2; WRITE=${3:-}
clang --version | head -1 | grep -q 'clang version 18\.1\.3' || { echo "need clang 18.1.3 (got: $(clang --version | head -1))"; exit 1; }
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
cp "$LIB/libuClibc-0.9.33.2.so" "$T/libc.so.0"; cp "$LIB/libpthread-0.9.33.2.so" "$T/libpthread.so.0"
C="--target=armv7a-linux-gnueabihf -mcpu=cortex-a7 -mfloat-abi=hard -marm -O2 -ffreestanding -fno-builtin -fno-stack-protector -nostdlib -fuse-ld=lld -Wl,--build-id=none"
SO="-fPIC -shared -Wl,--hash-style=sysv -Wl,-z,norelro"
clang $C $SO -Wl,-soname,scbox2_bind.so -o "$T/scbox2_bind.so.bin" "$F/scbox2_bind.c" "$T/libc.so.0"
clang $C -fno-pic -static -Wl,-e,_start -o "$T/scbox2_wrap.bin" "$F/scbox2_wrap.c"
clang $C $SO -Wl,-soname,scbox2_chip.so -o "$T/scbox2_chip.so.bin" "$F/scbox2_chip.c" "$T/libc.so.0" "$T/libpthread.so.0"
llvm-strip --strip-unneeded "$T/scbox2_bind.so.bin" "$T/scbox2_wrap.bin"   # strip these two, not scbox2_chip.so
mkdir -p "$OUT"
rc=0
for b in scbox2_bind.so.bin scbox2_wrap.bin scbox2_chip.so.bin; do
  if [ -f "$OUT/$b" ] && cmp -s "$T/$b" "$OUT/$b"; then echo "same     $b $(sha256sum < "$OUT/$b" | cut -c1-16)"
  elif [ "$WRITE" = --write ] || [ ! -f "$OUT/$b" ]; then cp "$T/$b" "$OUT/$b"; echo "written  $b $(sha256sum < "$OUT/$b" | cut -c1-16)"
  else echo "STALE    $b ($OUT/$b doesn't match its source; run with --write)"; rc=1; fi
done
exit $rc
