# Weave CLI installer for Windows (PowerShell 5.1+ / 7).
#
#   irm https://github.com/crueber/weave/releases/download/<tag>/install.ps1 | iex
#   .\install.ps1 [-Version vX.Y.Z] [-InstallDir DIR] [-Skill] [-NoSkill] [-BaseUrl URL]
#
# Downloads the release zip for this architecture, verifies SHA-256 against SHA256SUMS.txt and the
# ed25519 signature of SHA256SUMS.txt, installs weave.exe (default %LOCALAPPDATA%\Programs\weave)
# and adds it to the user PATH.
#
# Signature: PowerShell has no ed25519, so the signature is checked with OpenSSL >= 3 when it is on
# PATH (e.g. Git for Windows), otherwise by running the freshly downloaded weave.exe
# `verify-release` from the temp directory BEFORE it is installed. The second path only proves the
# release is internally consistent (a malicious download host could ship a binary that says "ok");
# use OpenSSL, or verify with an already-trusted weave, when that matters.
#
# Environment: WEAVE_REQUIRE_SIGNATURE=1 refuses releases without a valid signature;
# WEAVE_INSECURE=1 allows http:// URLs (testing only); WEAVE_RELEASE_PUBKEY = PEM file of a fork's key.
param(
  [string]$Version = $env:WEAVE_VERSION,
  [string]$InstallDir = $env:WEAVE_INSTALL_DIR,
  [string]$BaseUrl = $env:WEAVE_RELEASE_BASE,
  [string]$Server = $(if ($env:WEAVE_SERVER) { $env:WEAVE_SERVER } else { 'https://github.com' }),
  [string]$Repo = $(if ($env:WEAVE_REPO) { $env:WEAVE_REPO } else { 'crueber/weave' }),
  [switch]$Skill,
  [switch]$NoSkill
)
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
# TLS 1.2 and (where the runtime knows it) 1.3 only
$proto = [Net.SecurityProtocolType]::Tls12
if ([Enum]::GetNames([Net.SecurityProtocolType]) -contains 'Tls13') { $proto = $proto -bor [Net.SecurityProtocolType]::Tls13 }
[Net.ServicePointManager]::SecurityProtocol = $proto

# base64 of the DER SubjectPublicKeyInfo of the release signing key (see internal/relsign)
$ReleasePubKeyB64 = 'MCowBQYDK2VwAyEAX2HjqHRYLLIO9QVyhCHsgJ8UqLT115QJqCMxxGhKkXk='
$RequireSig = $env:WEAVE_REQUIRE_SIGNATURE -eq '1'
$Insecure = $env:WEAVE_INSECURE -eq '1'
if (-not $Insecure) {
  foreach ($u in @($Server, $BaseUrl)) {
    if ($u -and -not $u.StartsWith('https://', [StringComparison]::OrdinalIgnoreCase)) { throw "refusing non-https URL $u (set WEAVE_INSECURE=1 for testing)" }
  }
}

switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { $arch = 'amd64' }
  'ARM64' { $arch = 'arm64' }
  default { throw "unsupported architecture $($env:PROCESSOR_ARCHITECTURE)" }
}

if (-not $BaseUrl) {
  if (-not $Version) {
    Write-Host 'looking up the latest release...'
    $rel = Invoke-RestMethod "$Server/api/v1/repos/$Repo/releases/latest"
    $Version = $rel.tag_name
    if (-not $Version) { throw 'cannot determine the latest release; pass -Version vX.Y.Z' }
  }
  $BaseUrl = "$Server/$Repo/releases/download/$Version"
}
$BaseUrl = $BaseUrl.TrimEnd('/')

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("weave-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "downloading checksums from $BaseUrl"
  $sums = Join-Path $tmp 'SHA256SUMS.txt'
  Invoke-WebRequest "$BaseUrl/SHA256SUMS.txt" -OutFile $sums
  $sig = Join-Path $tmp 'SHA256SUMS.txt.sig'
  $haveSig = $false
  try { Invoke-WebRequest "$BaseUrl/SHA256SUMS.txt.sig" -OutFile $sig; $haveSig = $true } catch { }
  if (-not $haveSig -and $RequireSig) { throw "WEAVE_REQUIRE_SIGNATURE=1 but $BaseUrl/SHA256SUMS.txt.sig is not available" }
  if (-not $haveSig) { Write-Warning 'this release has no signature file (SHA256SUMS.txt.sig); continuing with the checksum only' }
  $suffix = "_windows_$arch.zip"
  $line = Get-Content $sums | Where-Object { $p = ($_ -split '\s+'); $p.Count -ge 2 -and $p[1].StartsWith('weave_') -and $p[1].EndsWith($suffix) } | Select-Object -First 1
  if (-not $line) { throw "no release archive for windows/$arch in $BaseUrl/SHA256SUMS.txt" }
  $parts = $line -split '\s+'
  $want = $parts[0].ToLower(); $archive = $parts[1]

  Write-Host "downloading $archive"
  $zip = Join-Path $tmp $archive
  Invoke-WebRequest "$BaseUrl/$archive" -OutFile $zip
  $got = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
  if ($got -ne $want) { throw "checksum mismatch for $archive (expected $want, got $got)" }
  Write-Host 'checksum ok'

  $x = Join-Path $tmp 'x'
  Expand-Archive $zip -DestinationPath $x
  if (-not (Test-Path (Join-Path $x 'weave.exe'))) { throw 'archive does not contain weave.exe' }

  if ($haveSig) {
    $ossl = Get-Command openssl -ErrorAction SilentlyContinue
    if ($ossl -and ((& $ossl.Source version) -match '^OpenSSL [3-9]')) {
      $pem = Join-Path $tmp 'pub.pem'
      if ($env:WEAVE_RELEASE_PUBKEY) { Copy-Item $env:WEAVE_RELEASE_PUBKEY $pem }
      else { Set-Content -Path $pem -Encoding ascii -Value "-----BEGIN PUBLIC KEY-----`n$ReleasePubKeyB64`n-----END PUBLIC KEY-----" }
      $bin = Join-Path $tmp 'sig.bin'
      [IO.File]::WriteAllBytes($bin, [Convert]::FromBase64String((Get-Content $sig -Raw).Trim()))
      & $ossl.Source pkeyutl -verify -rawin -pubin -inkey $pem -sigfile $bin -in $sums *> $null
      if ($LASTEXITCODE -ne 0) { throw 'SIGNATURE VERIFICATION FAILED for SHA256SUMS.txt: do not install this release' }
      Write-Host 'signature ok (ed25519, OpenSSL)'
    } else {
      # run the downloaded binary only to verify, from the temp dir; it is not installed yet
      $vargs = @('verify-release', $sums, $sig)
      if ($env:WEAVE_RELEASE_PUBKEY) { $vargs += @('--pubkey', $env:WEAVE_RELEASE_PUBKEY) }
      & (Join-Path $x 'weave.exe') @vargs *> $null
      if ($LASTEXITCODE -ne 0) { throw 'SIGNATURE VERIFICATION FAILED for SHA256SUMS.txt: do not install this release' }
      Write-Host 'signature ok (weave verify-release; install OpenSSL >= 3 for an independent check)'
    }
  }

  if (-not $InstallDir) { $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\weave' }
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  Copy-Item (Join-Path $x 'weave.exe') (Join-Path $InstallDir 'weave.exe') -Force
  Write-Host "installed $(Join-Path $InstallDir 'weave.exe')"

  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (($userPath -split ';') -notcontains $InstallDir) {
    [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $InstallDir), 'User')
    Write-Host "added $InstallDir to your user PATH (open a new terminal)"
  }
  & (Join-Path $InstallDir 'weave.exe') version

  $doSkill = $Skill.IsPresent
  if (-not $Skill -and -not $NoSkill -and [Environment]::UserInteractive -and -not [Console]::IsInputRedirected) {
    $ans = Read-Host 'Install the Weave skill for every coding agent found here (Claude Code, Codex, OpenCode, pi, Gemini, Cursor, ...)? [y/N]'
    $doSkill = $ans -match '^(y|yes)$'
  }
  if ($doSkill) { & (Join-Path $InstallDir 'weave.exe') skill install }
  Write-Host ''
  Write-Host 'next: weave login --url https://your-weave-server --token wk_...   (then: weave doctor)'
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
