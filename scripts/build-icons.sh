#!/usr/bin/env bash
#
# build-icons.sh — assets/icon/*.svg 에서 tidoly stow 의 아이콘을 굽는다 (stow issue #18).
#   → assets/icon/AppIcon.icns          앱 아이콘 (build-mac-app.sh 가 넣는다)
#   → cmd/stow-mac/menubar.png           메뉴 막대 아이콘 (macOS 가 단색으로 칠한다, 18pt@2x)
#   → internal/web/static/favicon.png    웹 화면의 아이콘 · 홈 화면 아이콘
# 구운 결과는 저장소에 넣는다 — 앱을 구울 때 Chrome 이 없어도 되게. SVG 를 고쳤을 때만 다시 돌린다.
# SVG 를 그림으로 바꾸는 데 Chrome(headless)을 쓴다 — 이 맥에 다른 도구가 없다.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

shot() { # <svg> <pixels> <out.png>
  sed "s/width=\"1024\" height=\"1024\"/width=\"$2\" height=\"$2\"/" "$1" > "$WORK/in.svg"
  "$CHROME" --headless=new --disable-gpu --hide-scrollbars --default-background-color=00000000 \
    --window-size="$2,$2" --force-device-scale-factor=1 --screenshot="$3" "file://$WORK/in.svg" >/dev/null 2>&1
}

SET="$WORK/AppIcon.iconset"
mkdir -p "$SET"
for n in 16 32 128 256 512; do
  shot assets/icon/icon.svg $n "$SET/icon_${n}x${n}.png"
  shot assets/icon/icon.svg $((n * 2)) "$SET/icon_${n}x${n}@2x.png"
done
iconutil -c icns -o assets/icon/AppIcon.icns "$SET"
shot assets/icon/menubar.svg 36 cmd/stow-mac/menubar.png
shot assets/icon/icon.svg 180 internal/web/static/favicon.png
ls -la assets/icon/AppIcon.icns cmd/stow-mac/menubar.png internal/web/static/favicon.png
