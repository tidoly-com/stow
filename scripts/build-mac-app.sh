#!/usr/bin/env bash
#
# build-mac-app.sh — tidoly-stow.app 을 굽는다 (stow issue #18). Apple Silicon 과 Intel 둘 다 도는 universal.
#
#   scripts/build-mac-app.sh            # → dist/tidoly-stow.app
#   scripts/build-mac-app.sh --open     # 굽고 바로 연다
#
# 서명 · 공증은 하지 않는다 — 배포는 tidoly 설치기처럼 curl 로 받는 설치 스크립트로 한다(#18).
# ffmpeg 는 scripts/build-ffmpeg.sh 가 구운 LGPL 빌드를 넣는다 — 없으면 먼저 굽는다.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

OPEN=false
[ "${1:-}" = "--open" ] && OPEN=true
VERSION="${STOW_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}" # STOW_VERSION: to try a release
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

both() { # <name> <go package> <cgo>: builds for both kinds of Mac and joins them
  local name=$1 pkg=$2 cgo=$3
  CGO_ENABLED=$cgo GOOS=darwin GOARCH=arm64 CC="clang -arch arm64" go build -trimpath -ldflags "-X main.version=$VERSION" -o "$WORK/$name-arm64" "$pkg"
  CGO_ENABLED=$cgo GOOS=darwin GOARCH=amd64 CC="clang -arch x86_64" go build -trimpath -ldflags "-X main.version=$VERSION" -o "$WORK/$name-amd64" "$pkg"
  lipo -create -output "$WORK/$name" "$WORK/$name-arm64" "$WORK/$name-amd64"
}

echo "▸ 서버 · 껍데기"
both tidoly-light ./cmd/tidoly-light 0
both stow ./cmd/stow 1
echo "▸ 쿠폰 도우미"
swiftc -O -target arm64-apple-macos13 helpers/coupons-mac/main.swift -o "$WORK/stow-coupons-arm64"
swiftc -O -target x86_64-apple-macos13 helpers/coupons-mac/main.swift -o "$WORK/stow-coupons-amd64" 2>/dev/null
lipo -create -output "$WORK/stow-coupons" "$WORK/stow-coupons-arm64" "$WORK/stow-coupons-amd64"

echo "▸ 얼굴 도우미 (Apple Silicon — 인텔 맥용 ONNX Runtime 은 발행되지 않는다)"
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 CC="clang -arch arm64" go build -trimpath -o "$WORK/stow-faces" ./cmd/stow-faces
[ -f .cache/onnxruntime/mac/libonnxruntime.dylib ] || scripts/fetch-onnxruntime.sh mac

echo "▸ ffmpeg (LGPL)"
[ -x dist/ffmpeg/ffmpeg ] || scripts/build-ffmpeg.sh

APP=dist/tidoly-stow.app
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/bin" "$APP/Contents/Resources/LICENSES"
cp "$WORK/stow" "$APP/Contents/MacOS/stow"
cp "$WORK/tidoly-light" "$WORK/stow-coupons" dist/ffmpeg/ffmpeg "$APP/Contents/Resources/bin/"
cp dist/ffmpeg/LICENSES/* "$APP/Contents/Resources/LICENSES/"   # ffmpeg 의 LGPL 고지 · 소스 주소
cp "$WORK/stow-faces" .cache/onnxruntime/mac/libonnxruntime.dylib "$APP/Contents/Resources/bin/"
cp .cache/onnxruntime/mac/LICENSE "$APP/Contents/Resources/LICENSES/onnxruntime-LICENSE"
cp .cache/onnxruntime/mac/ThirdPartyNotices.txt "$APP/Contents/Resources/LICENSES/onnxruntime-ThirdPartyNotices.txt"
cp assets/licenses/* "$APP/Contents/Resources/LICENSES/"   # 얼굴 모델(YuNet MIT · SFace Apache-2.0) · Immich 응답 틀
cp LICENSE TRADEMARKS.md "$APP/Contents/Resources/"
scripts/collect-go-licenses.sh darwin "$APP/Contents/Resources/LICENSES" ./cmd/stow ./cmd/tidoly-light ./cmd/stow-faces
cp assets/icon/AppIcon.icns "$APP/Contents/Resources/"   # made by scripts/build-icons.sh

# LSUIElement: 창이 없고 화면은 브라우저에 있다 — Dock 대신 메뉴 막대에만 둔다.
cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key>                  <string>tidoly stow</string>
  <key>CFBundleDisplayName</key>           <string>tidoly stow</string>
  <key>CFBundleIdentifier</key>            <string>com.tidoly.stow</string>
  <key>CFBundleExecutable</key>            <string>stow</string>
  <key>CFBundleIconFile</key>              <string>AppIcon</string>
  <key>CFBundlePackageType</key>           <string>APPL</string>
  <key>CFBundleShortVersionString</key>    <string>${VERSION}</string>
  <key>CFBundleInfoDictionaryVersion</key> <string>6.0</string>
  <key>LSMinimumSystemVersion</key>        <string>13.0</string>
  <key>LSUIElement</key>                   <true/>
  <key>NSHighResolutionCapable</key>       <true/>
  <key>NSLocalNetworkUsageDescription</key>
  <string>집 안의 폰이 이 Mac 에 사진을 올리고 보려면 필요합니다.</string>
</dict>
</plist>
PLIST

echo "✓ $APP ($VERSION)"
for f in "$APP/Contents/MacOS/stow" "$APP"/Contents/Resources/bin/*; do echo "  $(basename "$f"): $(lipo -archs "$f")"; done
$OPEN && open "$APP"
true
