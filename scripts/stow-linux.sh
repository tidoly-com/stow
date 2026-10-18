#!/bin/sh
#
# tidoly stow 설치 (Linux) — stow issue #18.
#
# 사용:  curl -fsSL https://raw.githubusercontent.com/tidoly-com/stow/main/scripts/stow-linux.sh | sudo sh
#
# 화면이 없는 서버에 맞춘다: 프로그램을 /opt/tidoly-stow 에 두고 systemd 서비스(tidoly-stow)로 띄운다. 설치를
# 시작한 사람의 계정으로 돈다. 설치 화면은 집 안의 다른 기기 브라우저로 http://<이 PC 주소>:30891 을 연다.
# 설치 화면이 사진 둘 곳을 정하면 서버가 스스로 끝나고, systemd 가 다시 띄워 사진 서버가 된다.
#
# 앱 묶음과 체크섬은 GitHub 릴리스에서 받는다(STOW_BASE_URL 로 바꿀 수 있다). 서명이 없으므로 무결성은
# 체크섬이 책임진다. 안 맞으면 설치하지 않는다. 사진 · 설정은 건드리지 않는다.
#
# 관리 화면의 업데이트 단추를 위해 root 로 도는 tidoly-stow-update 서비스도 깐다 — 단추를 누르면 이 스크립트를
# 릴리스에서 다시 받아 그대로 돌린다(stow issue #2).
set -eu

BASE="${STOW_BASE_URL:-https://github.com/tidoly-com/stow/releases/latest/download}"
DIR=/opt/tidoly-stow
UNIT=/etc/systemd/system/tidoly-stow.service
UNIT_UPDATE=/etc/systemd/system/tidoly-stow-update # .service and .path
UPD=/var/lib/tidoly-stow                           # the update service's files (internal/update/install_linux.go)

say() { printf '%s\n' "$*"; }
die() { printf '오류: %s\n' "$*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || die "리눅스용 설치 스크립트입니다"
case "$(uname -m)" in
x86_64 | amd64) ASSET=tidoly-stow-linux-amd64.tar.gz ;;
*) die "아직 x86-64 리눅스만 됩니다 (지금 $(uname -m))" ;;
esac
[ "$(id -u)" = 0 ] || die "관리자 권한이 필요합니다 — sudo sh 로 실행해 주세요"
command -v systemctl >/dev/null 2>&1 || die "systemd 가 필요합니다"
command -v curl >/dev/null 2>&1 || die "curl 이 필요합니다"

# who it runs as: whoever ran sudo — their home holds the settings and, by default, the photos. The update
# service runs this as root without sudo, and says who it was.
RUN_AS="${STOW_RUN_AS:-${SUDO_USER:-root}}"
HOME_OF="$(getent passwd "$RUN_AS" | cut -d: -f6)"
[ -n "$HOME_OF" ] || die "계정 $RUN_AS 의 홈을 찾지 못했습니다"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

say "tidoly stow 설치"
curl -fsSL -o "$tmp/$ASSET" "$BASE/$ASSET" || die "받지 못했습니다: $BASE/$ASSET"
curl -fsSL -o "$tmp/SHA256SUMS" "$BASE/SHA256SUMS" || die "체크섬 파일을 받지 못했습니다 — 확인 없이는 설치하지 않습니다"
want="$(grep " $ASSET\$" "$tmp/SHA256SUMS" | cut -d' ' -f1 | head -1)"
[ -n "$want" ] || die "체크섬 목록에 $ASSET 이 없습니다"
sum="$(sha256sum "$tmp/$ASSET" | cut -d' ' -f1)"
[ "$sum" = "$want" ] || die "체크섬이 맞지 않습니다 — 받은 파일을 버립니다
  기대: $want
  실제: $sum"
say "  ✓ 받기 · 체크섬 확인"

mkdir "$tmp/app"
tar -xzf "$tmp/$ASSET" -C "$tmp/app" || die "풀지 못했습니다"
[ -x "$tmp/app/tidoly-stow/bin/tidoly-light" ] || die "받은 파일 안에 서버가 없습니다"

if systemctl is-active --quiet tidoly-stow; then
  say "  · 떠 있는 tidoly stow 를 끕니다"
  systemctl stop tidoly-stow # SIGTERM: the server closes its database first
fi
rm -rf "$DIR"
mv "$tmp/app/tidoly-stow" "$DIR"
chmod -R a+rX "$DIR"
ln -sf "$DIR/bin/tidoly-light" /usr/local/bin/tidoly-stow # "sudo tidoly-stow password" sets the owner's password again

opts="-config $HOME_OF/.config/tidoly-stow/config.json -listen 0.0.0.0:30891"
[ -x "$DIR/bin/stow-faces" ] && opts="$opts -face-helper $DIR/bin/stow-faces"
cat >"$UNIT" <<UNIT
[Unit]
Description=tidoly stow — photos kept on this PC
After=network-online.target
Wants=network-online.target

[Service]
User=$RUN_AS
ExecStart=$DIR/bin/tidoly-light $opts
# the setup ends the program once the folder for the photos is chosen; started again, it is the photo server
Restart=always
RestartSec=1
StandardOutput=append:$HOME_OF/.local/state/tidoly-stow/server.log
StandardError=append:$HOME_OF/.local/state/tidoly-stow/server.log

[Install]
WantedBy=multi-user.target
UNIT
for d in "$HOME_OF/.config/tidoly-stow" "$HOME_OF/.local/state/tidoly-stow"; do
  mkdir -p "$d" && chown "$RUN_AS" "$d"
done

# The admin page's update button (stow issue #2): the photo server, not root, leaves a file in $UPD/ask;
# systemd sees it and runs this same script as root, apart from the photo server — stopping that server does
# not stop the update. Where it downloads from is fixed here, never read from the request. What it did goes
# in $UPD/state (running · done · failed), where the server waiting on it reads it.
mkdir -p "$UPD/ask"
chown "$RUN_AS" "$UPD/ask"
cat >"$UPD/update.sh.new" <<RUNNER
#!/bin/sh
# run by tidoly-stow-update.service — written by the install, which replaces it
rm -f "$UPD/ask/update"
echo running >"$UPD/state"
if curl -fsSL -o "$UPD/stow-linux.sh" "$BASE/stow-linux.sh" &&
  STOW_BASE_URL="$BASE" STOW_RUN_AS="$RUN_AS" sh "$UPD/stow-linux.sh"; then
  echo done >"$UPD/state"
else
  echo failed >"$UPD/state"
  exit 1
fi
RUNNER
chmod 755 "$UPD/update.sh.new"
mv "$UPD/update.sh.new" "$UPD/update.sh" # a new file: the one running now, when this is the update, reads on
cat >"$UNIT_UPDATE.service" <<UNIT
[Unit]
Description=tidoly stow — update, asked from the admin page

[Service]
Type=oneshot
ExecStart=/bin/sh $UPD/update.sh
TimeoutStartSec=30min
UNIT
cat >"$UNIT_UPDATE.path" <<UNIT
[Unit]
Description=tidoly stow — wait for the admin page to ask for an update

[Path]
PathExists=$UPD/ask/update
Unit=tidoly-stow-update.service

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now tidoly-stow >/dev/null 2>&1 || die "서비스를 띄우지 못했습니다 — journalctl -u tidoly-stow"
systemctl enable --now tidoly-stow-update.path >/dev/null 2>&1 || say "  · 업데이트 단추를 켜지 못했습니다 — 새 버전은 이 명령을 다시 실행해 받습니다"
say "  ✓ 설치: $DIR · 서비스 tidoly-stow ($RUN_AS 계정으로, 부팅하면 저절로)"

addr="$(ip -4 route get 192.0.2.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')"
say ""
say "집 안의 다른 기기 브라우저에서 여세요:  http://${addr:-<이 PC 주소>}:30891"
say "기록: $HOME_OF/.local/state/tidoly-stow/server.log · 끄기: sudo systemctl stop tidoly-stow"
