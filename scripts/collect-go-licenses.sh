#!/usr/bin/env bash
#
# collect-go-licenses.sh — 우리 실행 파일에 링크되는 Go 모듈들의 라이선스 전문과 NOTICE 를 모은다. MIT · BSD ·
# Apache 는 바이너리로 나눠 줄 때도 고지를 함께 주라고 한다. 릴리스 스크립트가 부른다.
#
#   scripts/collect-go-licenses.sh <GOOS> <dest> <package>...
#
# <dest>/go/<모듈 경로>/ 에 LICENSE · NOTICE 등을, <dest>/go/MODULES.txt 에 모듈 · 버전 목록을 둔다.
# 라이선스 파일이 없는 모듈이 있으면 멈춘다 — 고지 없이 내보내지 않는다.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

goos=$1 dest=$2
shift 2
out="$dest/go"
mkdir -p "$out"
: >"$out/MODULES.txt"
missing=0
# CGO on so the list covers the cgo builds too (the face helper); listing compiles nothing. -e: the installer
# embeds its payload, which is made only after this runs — a missing embedded file does not change the imports.
mods=$(GOOS=$goos GOARCH=amd64 CGO_ENABLED=1 go list -e -deps -f '{{if not .Standard}}{{with .Module}}{{.Path}}@{{.Version}}{{end}}{{end}}' "$@" | sort -u)
[ -n "$mods" ] || { echo "no modules listed for $*" >&2; exit 1; }
for mod in $mods; do
  path=${mod%@*}
  [ "$path" = github.com/tidoly-com/stow ] && continue
  dir=$(go list -m -f '{{.Dir}}' "$path")
  files=$(ls "$dir" | grep -iE '^(licen[cs]e|copying|notice|patents)' || true)
  if [ -z "$files" ]; then
    echo "no license file in $mod" >&2
    missing=1
    continue
  fi
  mkdir -p "$out/$path"
  for f in $files; do cp "$dir/$f" "$out/$path/"; done
  echo "$mod" >>"$out/MODULES.txt"
done
[ $missing = 0 ]
echo "  $(wc -l <"$out/MODULES.txt" | tr -d ' ') Go modules"
