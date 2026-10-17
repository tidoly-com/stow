# tidoly stow 설치 (Windows) — stow issue #18.
#
# 사용: PowerShell 을 열고 붙여 넣는다
#   irm https://raw.githubusercontent.com/tidoly-com/stow/main/scripts/stow-windows.ps1 | iex
#
# 이 파일은 저장소의 원문 주소에서 읽는다 — 거기는 charset=utf-8 로 내보내 한글이 깨지지 않는다. GitHub
# 릴리스는 application/octet-stream 으로 내보내, Windows PowerShell 5.1 의 irm 이 라틴 문자로 읽는다. BOM 을
# 붙이면 irm | iex 에서 첫 명령 이름에 붙어 오류가 나므로 붙이지 않는다(둘 다 2026-10-15 에 쟀다).
# 앱 묶음과 체크섬은 릴리스에서 받는다. 시험할 때는 $env:STOW_BASE_URL 로 받을 곳을 바꾼다.
#
# 받은 .ps1 파일은 Windows 의 실행 정책이 막지만, 이렇게 읽어 들인 것은 막지 않는다. 앱은 이 스크립트가
# 받는다 — 브라우저로 받은 것이 아니라 SmartScreen 의 "인터넷에서 받은 파일" 표시가 붙지 않는다. 서명이
# 없으므로 무결성은 체크섬이 책임진다. 안 맞으면 설치하지 않고, 건너뛰는 옵션도 두지 않는다.
#
# 앱은 %LOCALAPPDATA%\Programs\tidoly-stow 에 둔다 — 관리자 권한이 필요 없다. 시작 메뉴에 바로가기를 둔다.
# 사진 · 설정은 건드리지 않는다 — 사진은 고객이 고른 폴더, 설정은 %APPDATA%\tidoly-stow 에 있다.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # the progress bar slows Invoke-WebRequest down many times over
$Base = if ($env:STOW_BASE_URL) { $env:STOW_BASE_URL } else { 'https://github.com/tidoly-com/stow/releases/latest/download' }
$Asset = 'tidoly-stow-windows.zip'
$Dir = Join-Path $env:LOCALAPPDATA 'Programs\tidoly-stow'
$Exe = Join-Path $Dir 'stow.exe'

function Ours { Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.Path.StartsWith($Dir, [StringComparison]::OrdinalIgnoreCase) } }

$tmp = Join-Path $env:TEMP ('tidoly-stow-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host 'tidoly stow 설치'
    Invoke-WebRequest -UseBasicParsing "$Base/$Asset" -OutFile "$tmp\$Asset"
    Invoke-WebRequest -UseBasicParsing "$Base/SHA256SUMS" -OutFile "$tmp\SHA256SUMS"
    $line = Get-Content "$tmp\SHA256SUMS" | Where-Object { $_ -match (' ' + [regex]::Escape($Asset) + '$') } | Select-Object -First 1
    if (-not $line) { throw "체크섬 목록에 $Asset 이 없습니다" }
    $want = ($line -split '\s+')[0]
    $sum = (Get-FileHash "$tmp\$Asset" -Algorithm SHA256).Hash.ToLower()
    if ($sum -ne $want) { throw "체크섬이 맞지 않습니다 — 받은 파일을 버립니다`n  기대: $want`n  실제: $sum" }
    Write-Host '  ✓ 받기 · 체크섬 확인'
    Expand-Archive "$tmp\$Asset" -DestinationPath "$tmp\app"
    if (-not (Test-Path "$tmp\app\tidoly-stow\stow.exe")) { throw '받은 파일 안에 앱이 없습니다' }

    # 떠 있으면 끈다 — 메뉴의 끝내기와 같이, 서버가 DB 를 닫고 내려간다. 다 내려갈 때까지 기다린다.
    if (Ours) {
        Write-Host '  · 떠 있는 tidoly stow 를 끕니다'
        if (Test-Path $Exe) { & $Exe --quit }
        $deadline = (Get-Date).AddSeconds(20)
        while ((Ours) -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 500 }
        if (Ours) { throw 'tidoly stow 가 꺼지지 않습니다 — 트레이 아이콘에서 끝내기를 누른 뒤 다시 실행해 주세요' }
    }

    if (Test-Path $Dir) { Remove-Item -Recurse -Force $Dir }
    New-Item -ItemType Directory -Force -Path (Split-Path $Dir) | Out-Null
    Move-Item "$tmp\app\tidoly-stow" $Dir
    $link = Join-Path ([Environment]::GetFolderPath('Programs')) 'tidoly stow.lnk'
    $shortcut = (New-Object -ComObject WScript.Shell).CreateShortcut($link)
    $shortcut.TargetPath = $Exe
    $shortcut.WorkingDirectory = $Dir
    $shortcut.Save()
    # Settings → Apps, for this user: removed there by stow-uninstall.exe in the app's folder
    $apps = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\tidoly-stow'
    New-Item -Path $apps -Force | Out-Null
    Set-ItemProperty $apps DisplayName 'tidoly stow'
    Set-ItemProperty $apps Publisher 'tidoly'
    Set-ItemProperty $apps DisplayIcon $Exe
    Set-ItemProperty $apps InstallLocation $Dir
    Set-ItemProperty $apps UninstallString ('"' + (Join-Path $Dir 'stow-uninstall.exe') + '"')
    Set-ItemProperty $apps NoModify 1 -Type DWord
    Set-ItemProperty $apps NoRepair 1 -Type DWord
    Write-Host "  ✓ 설치: $Dir"

    # 처음이면 설치 화면이, 이미 쓰던 PC 면 사진 화면이 뜬다.
    Start-Process $Exe
    Write-Host ''
    Write-Host 'tidoly stow 를 엽니다 — 작업 표시줄 오른쪽의 트레이 아이콘으로 다시 열 수 있습니다.'
}
catch {
    Write-Host "오류: $($_.Exception.Message)" -ForegroundColor Red
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
