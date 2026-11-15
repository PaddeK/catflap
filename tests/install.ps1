# Test install.ps1 against a release built by goreleaser (Windows):
#   goreleaser release --snapshot --clean
#   powershell -File tests\install.ps1 dist      # Windows PowerShell 5.1
#   pwsh -File tests\install.ps1 dist            # PowerShell 7
#
# Serves the release files on localhost and checks that install.ps1 installs and
# upgrades catflap (also while catflap.exe is running), adds it to the user PATH, and
# refuses tampered or unlisted downloads. Your user PATH is restored at the end.
param([Parameter(Mandatory = $true)][string]$Dist)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$script:pass = 0; $script:fail = 0
function Check([string]$name, [scriptblock]$test) {
    $r = $false
    $script:out = ''
    $err = $null
    try { $r = [bool](& $test) } catch { $r = $false; $err = $_ }
    if ($r) { Write-Host "  PASS  $name"; $script:pass++; return }
    Write-Host "  FAIL  $name"; $script:fail++
    # Show what happened: the installer's output and the error, if any.
    if ($script:out) { ($script:out.TrimEnd() -split "`n") | ForEach-Object { Write-Host "        $_" } }
    if ($err) { Write-Host "        error: $($err.Exception.Message)" }
}

$arch = if ([string][Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } else { 'amd64' }
$asset = "catflap_windows_$arch.exe"
$bin = (Get-Content (Join-Path $Dist 'artifacts.json') -Raw | ConvertFrom-Json) |
    Where-Object { $_.name -eq $asset } | Select-Object -First 1 -ExpandProperty path
$sums = Join-Path $Dist 'checksums.txt'
$want = foreach ($l in Get-Content $sums) { $p = $l -split '\s+'; if ($p[1] -eq $asset) { $p[0] } }

$T = Join-Path ([IO.Path]::GetTempPath()) ('cfinst-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
foreach ($d in 'good', 'tampered', 'unlisted') {
    New-Item -ItemType Directory -Path "$T\www\$d" | Out-Null
    Copy-Item $bin "$T\www\$d\$asset"
}
Copy-Item $sums "$T\www\good\checksums.txt"
Copy-Item $sums "$T\www\tampered\checksums.txt"
[IO.File]::AppendAllText("$T\www\tampered\$asset", 'x')
Get-Content $sums | Where-Object { $_ -notmatch " $([regex]::Escape($asset))$" } | Set-Content "$T\www\unlisted\checksums.txt"

# A tiny static file server (HttpListener on localhost needs no admin rights).
$port = Get-Random -Minimum 20000 -Maximum 40000
$server = Start-Job -ArgumentList "$T\www", $port -ScriptBlock {
    param($www, $port)
    $l = New-Object Net.HttpListener
    $l.Prefixes.Add("http://localhost:$port/")
    $l.Start()
    while ($true) {
        $c = $l.GetContext()
        $f = Join-Path $www ($c.Request.Url.AbsolutePath.TrimStart('/') -replace '/', '\')
        if (Test-Path -LiteralPath $f -PathType Leaf) {
            $b = [IO.File]::ReadAllBytes($f)
            $c.Response.ContentLength64 = $b.Length
            $c.Response.OutputStream.Write($b, 0, $b.Length)
        } else {
            $c.Response.StatusCode = 404
        }
        $c.Response.Close()
    }
}

$key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
$savedPath = $key.GetValue('Path', $null, 'DoNotExpandEnvironmentNames')
$savedKind = if ($null -ne $savedPath) { $key.GetValueKind('Path') } else { $null }
$saved = @{ Base = $env:CATFLAP_BASE_URL; Dir = $env:CATFLAP_INSTALL_DIR; Repo = $env:CATFLAP_REPO; Ver = $env:CATFLAP_VERSION; Cfg = $env:CATFLAP_DIR }
$agent = $null
try {
    $up = $false
    for ($i = 0; $i -lt 50 -and -not $up; $i++) {
        try { Invoke-WebRequest -UseBasicParsing "http://localhost:$port/good/checksums.txt" | Out-Null; $up = $true } catch { Start-Sleep -Milliseconds 100 }
    }
    if (-not $up) { throw "file server did not start: $(Receive-Job $server 2>&1)" }

    $installer = Get-Content -Raw (Join-Path $root 'install.ps1')
    $dir = "$T\bin"
    $dst = "$dir\catflap.exe"
    $env:CATFLAP_INSTALL_DIR = $dir
    Remove-Item Env:CATFLAP_REPO, Env:CATFLAP_VERSION -ErrorAction SilentlyContinue
    function Inst([string]$name) {
        $env:CATFLAP_BASE_URL = "http://localhost:$port/$name"
        $script:out = ''
        $script:out = Invoke-Expression $installer *>&1 | Out-String
        $script:out
    }

    Write-Host '== install.ps1'
    Check 'installs the release binary' { (Inst good) -match 'checksum ok' }
    Check 'installed binary runs' { $v = @(& $dst version); $LASTEXITCODE -eq 0 -and $v[0] -like 'catflap *' }
    Check 'installed file matches checksums.txt' { (Get-FileHash $dst -Algorithm SHA256).Hash.ToLower() -eq $want }
    Check 'install dir added to the user PATH' {
        (($key.GetValue('Path', '', 'DoNotExpandEnvironmentNames') -split ';') -contains $dir)
    }
    Check 'PATH keeps its registry type' { $null -eq $savedKind -or $key.GetValueKind('Path') -eq $savedKind }
    Check 'idle upgrade leaves no old file' { Inst good | Out-Null; @(Get-ChildItem $dir -Name).Count -eq 1 }

    # Upgrade while catflap.exe runs (Windows locks a running exe).
    $env:CATFLAP_DIR = "$T\cfg"
    $agent = Start-Process -FilePath $dst -ArgumentList 'agent' -PassThru -WindowStyle Hidden
    Start-Sleep -Milliseconds 800
    Check 'upgrade while catflap.exe runs' {
        if ($agent.HasExited) { throw "the test agent exited early (code $($agent.ExitCode))" }
        (Inst good) -match 'installed'
    }
    Check 'running exe moved aside' {
        $l = @(Get-ChildItem $dir -Name)
        if (@($l | Where-Object { $_ -like 'catflap.exe.old-*' }).Count -ne 1) { throw "install dir has: $($l -join ', ')" }
        $true
    }
    Stop-Process -Id $agent.Id -Force; $agent.WaitForExit(); $agent = $null
    Check 'next upgrade removes the old file' { Inst good | Out-Null; @(Get-ChildItem $dir -Name).Count -eq 1 }

    $h = (Get-FileHash $dst).Hash
    Check 'tampered binary is refused' { try { Inst tampered | Out-Null; $false } catch { $_.Exception.Message -match 'checksum mismatch' } }
    Check 'tampered: existing file untouched' { (Get-FileHash $dst).Hash -eq $h }
    Check 'binary without checksum is refused' { try { Inst unlisted | Out-Null; $false } catch { $_.Exception.Message -match 'no entry' } }
    Check 'unlisted: existing file untouched' { (Get-FileHash $dst).Hash -eq $h }
    Check 'missing release fails' { try { Inst missing | Out-Null; $false } catch { $true } }
} finally {
    if ($agent) { Stop-Process -Id $agent.Id -Force -ErrorAction SilentlyContinue }
    if ($null -ne $savedPath) { $key.SetValue('Path', $savedPath, $savedKind) } else { $key.DeleteValue('Path', $false) }
    $key.Close()
    $env:CATFLAP_BASE_URL = $saved.Base; $env:CATFLAP_INSTALL_DIR = $saved.Dir; $env:CATFLAP_REPO = $saved.Repo
    $env:CATFLAP_VERSION = $saved.Ver; $env:CATFLAP_DIR = $saved.Cfg
    Stop-Job $server; Remove-Job $server -Force
    Start-Sleep -Milliseconds 300
    Remove-Item -LiteralPath $T -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Host ''
Write-Host "$($script:pass) passed, $($script:fail) failed"
if ($script:fail -ne 0) { exit 1 }
