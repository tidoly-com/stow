#!/usr/bin/env bash
#
# release-linux.sh — 리눅스(x86-64) 배포물을 만든다 (stow issue #18). 올리지는 않는다. 이 맥에서 빌드한다.
#
#   scripts/release-linux.sh
#
# dist/release/ 에:
#   tidoly-stow-linux-amd64.tar.gz   tidoly-stow/bin/ tidoly-light(서버) · stow-faces + libonnxruntime.so(얼굴 도우미) · ffmpeg(LGPL)
#                                    + LICENSES/
#   stow-linux.sh                    설치 스크립트의 사본 — 시험용. 고객은 저장소의 원문 주소에서 읽는다
#   SHA256SUMS                       맥 · 윈도 · 리눅스 배포물 모두의 체크섬
#
# 얼굴 도우미(cgo)와 ffmpeg 는 이 맥에서 zig(brew install zig)로 리눅스용을 굽는다 — 도우미는 glibc 2.28 이상,
# ffmpeg 는 musl 정적.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

OUT=dist/release
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "▸ 서버 (linux/amd64)"
mkdir -p "$WORK/tidoly-stow/bin" "$WORK/tidoly-stow/LICENSES"
VERSION="${STOW_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$VERSION" -o "$WORK/tidoly-stow/bin/tidoly-light" ./cmd/tidoly-light
cp LICENSE TRADEMARKS.md "$WORK/tidoly-stow/"
echo "▸ 얼굴 도우미"
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 CC="zig cc -target x86_64-linux-gnu.2.28" go build -trimpath -ldflags=-s -o "$WORK/tidoly-stow/bin/stow-faces" ./cmd/stow-faces
[ -f .cache/onnxruntime/linux/libonnxruntime.so ] || scripts/fetch-onnxruntime.sh linux
cp .cache/onnxruntime/linux/libonnxruntime.so "$WORK/tidoly-stow/bin/"
cp .cache/onnxruntime/linux/LICENSE "$WORK/tidoly-stow/LICENSES/onnxruntime-LICENSE"
cp .cache/onnxruntime/linux/ThirdPartyNotices.txt "$WORK/tidoly-stow/LICENSES/onnxruntime-ThirdPartyNotices.txt"
cp assets/licenses/* "$WORK/tidoly-stow/LICENSES/"
scripts/collect-go-licenses.sh linux "$WORK/tidoly-stow/LICENSES" ./cmd/tidoly-light ./cmd/stow-faces
echo "▸ ffmpeg (LGPL)"
[ -f dist/ffmpeg-linux/ffmpeg ] || scripts/build-ffmpeg.sh linux
cp dist/ffmpeg-linux/ffmpeg "$WORK/tidoly-stow/bin/"
cp dist/ffmpeg-linux/LICENSES/* "$WORK/tidoly-stow/LICENSES/"

mkdir -p "$OUT"
printf '{"version":"%s"}\n' "$VERSION" >"$OUT/latest.json" # what the running apps compare themselves with
rm -f "$OUT/tidoly-stow-linux-amd64.tar.gz"
# no macOS extended attributes in it: GNU tar on Linux warns about each
COPYFILE_DISABLE=1 tar --no-xattrs --no-mac-metadata -czf "$OUT/tidoly-stow-linux-amd64.tar.gz" -C "$WORK" tidoly-stow
cp scripts/stow-linux.sh "$OUT/stow-linux.sh"
(cd "$OUT" && sha256sum tidoly-stow-*.tar.gz tidoly-stow-*.zip stow-*.exe 2>/dev/null >SHA256SUMS || shasum -a 256 tidoly-stow-*.tar.gz tidoly-stow-*.zip stow-*.exe 2>/dev/null >SHA256SUMS || true)

echo "✓ $OUT/tidoly-stow-linux-amd64.tar.gz ($(du -h "$OUT/tidoly-stow-linux-amd64.tar.gz" | cut -f1))"
