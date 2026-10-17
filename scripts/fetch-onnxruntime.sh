#!/usr/bin/env bash
#
# fetch-onnxruntime.sh — 얼굴 도우미(cmd/stow-faces)가 쓰는 ONNX Runtime 을 받는다 (stow issue #10).
#
#   scripts/fetch-onnxruntime.sh mac       # → .cache/onnxruntime/mac/     libonnxruntime.dylib (arm64) · LICENSE · ThirdPartyNotices.txt
#   scripts/fetch-onnxruntime.sh windows   # → .cache/onnxruntime/windows/ onnxruntime.dll (x64) · LICENSE · ThirdPartyNotices.txt
#   scripts/fetch-onnxruntime.sh linux     # → .cache/onnxruntime/linux/   libonnxruntime.so (x86-64) · LICENSE · ThirdPartyNotices.txt
#
# Microsoft 가 발행한 것을 버전과 체크섬으로 고정해 받는다(MIT). 1.29 대 — cmd/stow-faces 가 쓰는 onnxruntime_go
# v1.36.0 이 1.29 의 C API 에 맞춰져 있다. 인텔 맥용은 발행되지 않는다: 인텔 맥에는 얼굴 도우미를 넣지 않는다.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

VERSION=1.29.1
case "${1:-}" in
mac) asset=onnxruntime-osx-arm64-$VERSION.tgz sum=c845ad2f4340669dc454c0ae092ccef7270d5bf858281890aef3c9bbf7da41e9 lib=libonnxruntime.$VERSION.dylib name=libonnxruntime.dylib ;;
linux) asset=onnxruntime-linux-x64-$VERSION.tgz sum=a28d7d65acafc06fb0f416cb409998773f5314c7eebf77caf907812831cdfc67 lib=libonnxruntime.so.$VERSION name=libonnxruntime.so ;;
windows) asset=onnxruntime-win-x64-$VERSION.zip sum=999a6ae6f70cd773b62a407d9a16f41bc2090bfda84f5fcee3a9268938b52a7a lib=onnxruntime.dll name=onnxruntime.dll ;;
*) echo "mac · windows · linux" >&2; exit 1 ;;
esac

CACHE=.cache/onnxruntime
OUT="$CACHE/$1"
mkdir -p "$CACHE"
if [ ! -f "$CACHE/$asset" ]; then
  echo "▸ ONNX Runtime $VERSION 받기 ($1)"
  curl -fsSL -o "$CACHE/$asset.part" "https://github.com/microsoft/onnxruntime/releases/download/v$VERSION/$asset"
  mv "$CACHE/$asset.part" "$CACHE/$asset"
fi
echo "$sum  $CACHE/$asset" | shasum -a 256 -c - >/dev/null || { echo "체크섬이 다르다: $CACHE/$asset" >&2; exit 1; }

dir="${asset%.tgz}"
dir="${dir%.zip}"
rm -rf "$CACHE/$dir" "$OUT" && mkdir -p "$OUT"
case "$asset" in
*.tgz) tar -xzf "$CACHE/$asset" -C "$CACHE" ;;
*.zip) unzip -qo "$CACHE/$asset" -d "$CACHE" ;;
esac
cp "$CACHE/$dir/lib/$lib" "$OUT/$name"
cp "$CACHE/$dir/LICENSE" "$OUT/LICENSE"
cp "$CACHE/$dir/ThirdPartyNotices.txt" "$OUT/ThirdPartyNotices.txt"
echo "✓ $OUT/$name ($(du -h "$OUT/$name" | cut -f1))"
