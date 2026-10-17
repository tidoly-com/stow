#!/usr/bin/env bash
#
# release-mac.sh — 맥 배포물을 만든다 (stow issue #18, 4단계). 올리지는 않는다.
#
#   scripts/release-mac.sh <받을 곳 주소>
#   scripts/release-mac.sh https://github.com/tidoly-com/stow/releases/latest/download
#
# dist/release/ 에:
#   tidoly-stow-macos.tar.gz   앱(universal)
#   SHA256SUMS                 설치 스크립트가 확인하는 체크섬
#   stow-macos.sh              고객이 받아 `sh` 로 돌리는 설치 스크립트 (scripts/stow-macos.sh.in)
#
# 세 파일을 그 주소에 함께 올린다. 설치 스크립트는 주소를 안에 박아 두고 거기서 앱과 체크섬을 받는다.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

BASE="${1:?받을 곳 주소를 주세요 — 예: https://github.com/tidoly-com/stow/releases/latest/download}"
OUT=dist/release

scripts/build-mac-app.sh
mkdir -p "$OUT" && rm -f "$OUT/tidoly-stow-macos.tar.gz" "$OUT/stow-macos.sh"
# COPYFILE_DISABLE: 맥의 확장 속성(._ 파일)을 묶지 않는다
COPYFILE_DISABLE=1 tar -czf "$OUT/tidoly-stow-macos.tar.gz" -C dist tidoly-stow.app
(cd "$OUT" && shasum -a 256 tidoly-stow-*.tar.gz tidoly-stow-*.zip stow-*.exe 2>/dev/null >SHA256SUMS || true) # the Windows one too, when there
sed "s|@@BASE@@|$BASE|" scripts/stow-macos.sh.in >"$OUT/stow-macos.sh"
# what the running apps compare themselves with (internal/update)
VERSION="${STOW_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
printf '{"version":"%s"}\n' "$VERSION" >"$OUT/latest.json"

echo "✓ $OUT ($(du -h "$OUT/tidoly-stow-macos.tar.gz" | cut -f1)) → $BASE"
ls -1 "$OUT" | sed 's/^/  /'
