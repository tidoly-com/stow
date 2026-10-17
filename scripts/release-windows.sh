#!/usr/bin/env bash
#
# release-windows.sh — Windows 배포물을 만든다 (stow issue #18). 올리지는 않는다. 이 맥에서 바로 빌드한다(cgo 없음).
#
#   scripts/release-windows.sh
#
# dist/release/ 에:
#   tidoly-stow-windows.zip    tidoly-stow\stow.exe(트레이 · 창) · stow-uninstall.exe(지우기) + bin\tidoly-light.exe(서버) + bin\ffmpeg.exe(LGPL)
#                              + bin\stow-faces.exe · onnxruntime.dll(얼굴 도우미)
#                              + LICENSE · TRADEMARKS.md + LICENSES\(ffmpeg 고지 · 소스 주소 · Go 모듈 고지)
#   stow-install.exe      설치 파일 — 위의 zip 을 품고, 받아서 더블클릭한다. 서명이 없어 SmartScreen 이 한 번
#                              경고한다: "추가 정보 → 실행" (운영자 2026-10-15). 관리자 권한은 필요 없다
#   stow-windows.ps1           설치 스크립트의 사본 — 시험용. 고객은 저장소의 원문 주소에서 읽는다(파일 머리 참고)
#   SHA256SUMS                 맥 · 윈도 배포물 모두의 체크섬
#
# ffmpeg 는 scripts/build-ffmpeg.sh windows 가 구운 LGPL 빌드를 넣는다 — 없으면 먼저 굽는다(mingw-w64 필요).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

OUT=dist/release
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "▸ 트레이 · 서버 (windows/amd64)"
mkdir -p "$WORK/tidoly-stow/bin"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags=-H=windowsgui -o "$WORK/tidoly-stow/stow.exe" ./cmd/stow
VERSION="${STOW_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$VERSION" -o "$WORK/tidoly-stow/bin/tidoly-light.exe" ./cmd/tidoly-light
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags=-H=windowsgui -o "$WORK/tidoly-stow/stow-uninstall.exe" ./cmd/stow-uninstall
echo "▸ 얼굴 도우미"
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc go build -trimpath -ldflags='-extldflags=-static' -o "$WORK/tidoly-stow/bin/stow-faces.exe" ./cmd/stow-faces
[ -f .cache/onnxruntime/windows/onnxruntime.dll ] || scripts/fetch-onnxruntime.sh windows
cp .cache/onnxruntime/windows/onnxruntime.dll "$WORK/tidoly-stow/bin/"
echo "▸ ffmpeg (LGPL)"
[ -f dist/ffmpeg-windows/ffmpeg.exe ] || scripts/build-ffmpeg.sh windows
cp dist/ffmpeg-windows/ffmpeg.exe "$WORK/tidoly-stow/bin/"
mkdir -p "$WORK/tidoly-stow/LICENSES"
cp dist/ffmpeg-windows/LICENSES/* "$WORK/tidoly-stow/LICENSES/"
cp .cache/onnxruntime/windows/LICENSE "$WORK/tidoly-stow/LICENSES/onnxruntime-LICENSE"
cp .cache/onnxruntime/windows/ThirdPartyNotices.txt "$WORK/tidoly-stow/LICENSES/onnxruntime-ThirdPartyNotices.txt"
cp assets/licenses/* "$WORK/tidoly-stow/LICENSES/"
cp LICENSE TRADEMARKS.md "$WORK/tidoly-stow/"
scripts/collect-go-licenses.sh windows "$WORK/tidoly-stow/LICENSES" ./cmd/stow ./cmd/tidoly-light ./cmd/stow-uninstall ./cmd/stow-faces ./cmd/stow-install

mkdir -p "$OUT"
printf '{"version":"%s"}\n' "$VERSION" >"$OUT/latest.json" # what the running apps compare themselves with
rm -f "$OUT/tidoly-stow-windows.zip"
(cd "$WORK" && zip -qr - tidoly-stow) >"$OUT/tidoly-stow-windows.zip"
cp scripts/stow-windows.ps1 "$OUT/stow-windows.ps1"
echo "▸ 설치 파일"
cp "$OUT/tidoly-stow-windows.zip" cmd/stow-install/payload.zip
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags=-H=windowsgui -o "$OUT/stow-install.exe" ./cmd/stow-install
rm -f cmd/stow-install/payload.zip
(cd "$OUT" && shasum -a 256 tidoly-stow-*.tar.gz tidoly-stow-*.zip stow-*.exe 2>/dev/null >SHA256SUMS || true)

echo "✓ $OUT/tidoly-stow-windows.zip ($(du -h "$OUT/tidoly-stow-windows.zip" | cut -f1))"
