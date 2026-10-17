#!/usr/bin/env bash
#
# build-ffmpeg.sh — tidoly stow 에 넣을 ffmpeg 를 소스에서 굽는다 (stow issue #18, 2단계).
#
#   scripts/build-ffmpeg.sh            # 맥 → dist/ffmpeg/ffmpeg (arm64 + x86_64) · dist/ffmpeg/LICENSES/
#   scripts/build-ffmpeg.sh windows    # 윈도 → dist/ffmpeg-windows/ffmpeg.exe (x86_64) · LICENSES/
#   scripts/build-ffmpeg.sh linux      # 리눅스 → dist/ffmpeg-linux/ffmpeg (x86_64, musl 정적 — 어느 배포판에서나) · LICENSES/
#
# LGPL 빌드다 — --enable-gpl · --enable-nonfree 를 쓰지 않고, 바깥 라이브러리도 넣지 않는다.
# 서버가 ffmpeg 에 시키는 일은 하나다: HEIC 사진 · 영상(MP4 · MOV)에서 한 장을 꺼내 JPEG 로 넘기기
# (internal/thumbs). 그 일에 드는 부품만 켠다. 다 되면 빌드 설정에 GPL 이 없는지 스스로 확인한다.
#
# x86_64 의 어셈블리 최적화에는 nasm 이 있어야 한다. 없으면 끄고 굽는다(느려질 뿐 동작은 같다).
# 윈도용은 이 맥에서 mingw-w64(brew install mingw-w64)로 굽는다 — 정적으로 묶어 Windows 의 DLL 만 쓴다.
# 리눅스용은 zig(brew install zig)의 musl 로 굽는다 — 정적이라 배포판의 라이브러리를 찾지 않는다.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

TARGET="${1:-mac}"
VERSION=9.0.2
SHA256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
URL="https://ffmpeg.org/releases/ffmpeg-$VERSION.tar.xz"
CACHE=.cache/ffmpeg
OUT=dist/ffmpeg
[ "$TARGET" = windows ] && OUT=dist/ffmpeg-windows
[ "$TARGET" = linux ] && OUT=dist/ffmpeg-linux

mkdir -p "$CACHE"
TARBALL="$CACHE/ffmpeg-$VERSION.tar.xz"
if [ ! -f "$TARBALL" ]; then
  echo "▸ 소스 받기 $URL"
  curl -fsSL -o "$TARBALL.part" "$URL"
  mv "$TARBALL.part" "$TARBALL"
fi
echo "$SHA256  $TARBALL" | shasum -a 256 -c - >/dev/null || { echo "체크섬이 다르다: $TARBALL" >&2; exit 1; }

# What the server asks of ffmpeg, and what the ffmpeg program itself puts in between: turning the
# picture upright (transpose · hflip · vflip · rotate), cropping and joining an iPhone HEIC's tiles
# (crop · xstack), converting for the JPEG encoder (scale · format).
COMMON=(
  --disable-everything --disable-autodetect --disable-network --disable-doc --disable-debug
  --disable-ffplay --disable-ffprobe --disable-avdevice --disable-swresample
  --enable-static --disable-shared
  --enable-protocol=file,pipe
  --enable-demuxer=mov
  --enable-decoder=hevc,h264,mpeg4,h263,mjpeg,prores,vp9
  --enable-parser=hevc,h264,mpeg4video,h263,mjpeg,vp9
  --enable-encoder=mjpeg
  --enable-muxer=image2pipe
  --enable-filter=buffer,buffersink,null,format,scale,crop,xstack,transpose,hflip,vflip,rotate,trim,setpts
)

build() { # <mac arch | windows>
  local arch=$1
  local src="$CACHE/src-$arch" how=()
  rm -rf "$src" && mkdir -p "$src"
  tar -xf "$TARBALL" -C "$src" --strip-components 1
  case "$arch" in
  windows)
    command -v x86_64-w64-mingw32-gcc >/dev/null || { echo "mingw-w64 가 없다 — brew install mingw-w64" >&2; exit 1; }
    # Windows' own threads; no zlib to link against there
    how=(--target-os=mingw32 --arch=x86_64 --enable-cross-compile --cross-prefix=x86_64-w64-mingw32- --extra-ldflags=-static) ;;
  linux)
    command -v zig >/dev/null || { echo "zig 가 없다 — brew install zig" >&2; exit 1; }
    how=(--target-os=linux --arch=x86_64 --enable-cross-compile "--cc=zig cc -target x86_64-linux-musl"
      "--ar=zig ar" "--ranlib=zig ranlib" --nm=nm --host-cc=clang "--extra-ldflags=-static -s") ;; # -s: no symbols
  *)
    how=(--target-os=darwin "--cc=clang -arch $arch" --enable-pthreads --enable-zlib
      --extra-cflags=-mmacosx-version-min=13.0 --extra-ldflags=-mmacosx-version-min=13.0)
    if [ "$arch" = x86_64 ]; then how+=(--enable-cross-compile --arch=x86_64); fi ;;
  esac
  if [ "$arch" != arm64 ] && ! command -v nasm >/dev/null; then
    echo "  (nasm 이 없어 x86_64 어셈블리 최적화를 끈다)"
    how+=(--disable-x86asm)
  fi
  echo "▸ ffmpeg $VERSION $arch"
  (
    cd "$src"
    ./configure "${how[@]}" "${COMMON[@]}" >configure.log 2>&1 || { tail -20 configure.log >&2; exit 1; }
    make -j"$(sysctl -n hw.ncpu)" "ffmpeg$([ "$arch" = windows ] && echo .exe)" >make.log 2>&1 || { tail -20 make.log >&2; exit 1; }
  )
  # The license must be LGPL, as configure settled it — not only as we called it.
  if ! { grep -q '^#define CONFIG_GPL 0$' "$src/config.h" && grep -q '^#define CONFIG_NONFREE 0$' "$src/config.h" &&
    grep -q '^#define CONFIG_VERSION3 0$' "$src/config.h" && grep -q 'FFMPEG_LICENSE "LGPL version 2.1 or later"' "$src/config.h"; }; then
    echo "LGPL 이 아니다($arch) — 넣을 수 없다" >&2
    exit 1
  fi
}

rm -rf "$OUT" && mkdir -p "$OUT/LICENSES"
if [ "$TARGET" = linux ]; then
  build linux
  cp "$CACHE/src-linux/ffmpeg" "$OUT/ffmpeg"
  file "$OUT/ffmpeg" | grep -q "statically linked" || { echo "정적으로 묶이지 않았다" >&2; exit 1; }
  config="$(sed -n 's/^#define FFMPEG_CONFIGURATION "\(.*\)"$/\1/p' "$CACHE/src-linux/config.h" | tr ' ' '\n' | sed 's/^/    /')"
  src=linux
  bin=bin/ffmpeg
  built="$OUT/ffmpeg (x86_64 linux, static, $(du -h "$OUT/ffmpeg" | cut -f1))"
elif [ "$TARGET" = windows ]; then
  build windows
  cp "$CACHE/src-windows/ffmpeg.exe" "$OUT/ffmpeg.exe"
  # linked statically: only Windows' own DLLs may be asked for
  dlls="$(x86_64-w64-mingw32-objdump -p "$OUT/ffmpeg.exe" | sed -n 's/.*DLL Name: //p' | tr 'A-Z' 'a-z' | sort -u | tr '\n' ' ')"
  for dll in $dlls; do
    case "$dll" in
    kernel32.dll | user32.dll | advapi32.dll | bcrypt.dll | ole32.dll | shell32.dll | shlwapi.dll | msvcrt.dll | ws2_32.dll | api-ms-win-*) ;;
    *) echo "Windows 에 없는 DLL 을 찾는다: $dll" >&2; exit 1 ;;
    esac
  done
  config="$(sed -n 's/^#define FFMPEG_CONFIGURATION "\(.*\)"$/\1/p' "$CACHE/src-windows/config.h" | tr ' ' '\n' | sed 's/^/    /')"
  src=windows
  bin='bin\ffmpeg.exe'
  built="$OUT/ffmpeg.exe (x86_64, $(du -h "$OUT/ffmpeg.exe" | cut -f1); DLL: $dlls)"
else
  build arm64
  build x86_64
  lipo -create -output "$OUT/ffmpeg" "$CACHE/src-arm64/ffmpeg" "$CACHE/src-x86_64/ffmpeg"
  # and as the program itself says it
  license="$("$OUT/ffmpeg" -hide_banner -L | tr '\n' ' ' | tr -s ' ')"
  config="$("$OUT/ffmpeg" -hide_banner -buildconf | grep -v "^Exiting")"
  case "$config" in *--enable-gpl* | *--enable-nonfree* | *--enable-version3*)
    echo "GPL · nonfree 부품이 들어갔다 — 넣을 수 없다" >&2
    exit 1 ;;
  esac
  case "$license" in *"GNU Lesser General Public License as published"*"version 2.1"*) ;; *)
    echo "LGPL 이 아니다: $license" >&2
    exit 1 ;;
  esac
  src=arm64
  bin=bin/ffmpeg
  built="$OUT/ffmpeg ($(lipo -archs "$OUT/ffmpeg"), $(du -h "$OUT/ffmpeg" | cut -f1))"
fi

cp "$CACHE/src-$src/COPYING.LGPLv2.1" "$OUT/LICENSES/ffmpeg-COPYING.LGPLv2.1"
cat >"$OUT/LICENSES/ffmpeg.txt" <<NOTICE
tidoly stow 에는 FFmpeg $VERSION 이 따로 된 실행 파일($bin)로 들어 있습니다.
사진 · 영상의 작은 그림을 만드는 데 씁니다.

FFmpeg 는 GNU Lesser General Public License 2.1 이상(LGPL v2.1+)으로 배포됩니다 —
전문은 ffmpeg-COPYING.LGPLv2.1. 이 빌드에는 GPL · nonfree 부품이 없습니다.

FFmpeg is licensed under the GNU Lesser General Public License version 2.1 or later.
This build contains no GPL or nonfree parts.

소스 (이 빌드와 같은 버전) / Source of this exact version:
  $URL
  SHA-256 $SHA256
빌드 방법 / How it was built: scripts/build-ffmpeg.sh (https://github.com/tidoly-com/stow)

빌드 설정 / configuration:
$config
NOTICE

echo "✓ $built"
echo "  LGPL v2.1+ · GPL · nonfree 없음"
