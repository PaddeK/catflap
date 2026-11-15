# catflap installer for Windows (PowerShell 5.1 or later).
#
#   irm https://github.com/PaddeK/catflap/releases/latest/download/install.ps1 | iex
#
# Downloads the release binary for this machine, checks it against the release's
# checksums.txt and installs it to %LOCALAPPDATA%\Programs\catflap (added to your
# user PATH). Running it again upgrades catflap in place.
#
# Options (environment variables, e.g. $env:CATFLAP_VERSION = 'v1.0.0'):
#   CATFLAP_VERSION      release to install (default: the latest release)
#   CATFLAP_INSTALL_DIR  where to put catflap.exe
#   CATFLAP_REPO         GitHub repository to download from (default: PaddeK/catflap)
#   CATFLAP_BASE_URL     download from this URL instead of GitHub (mirrors, tests)
#
# Everything is inside one script block, so a download cut short runs nothing.
# Errors are thrown, never `exit`, so `| iex` does not close your window.

& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'   # Invoke-WebRequest is very slow with the progress bar
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = if ($env:CATFLAP_REPO) { $env:CATFLAP_REPO } else { 'PaddeK/catflap' }
    $version = if ($env:CATFLAP_VERSION) { $env:CATFLAP_VERSION } else { 'latest' }

    $arch = $null
    try { $arch = [string][Runtime.InteropServices.RuntimeInformation]::OSArchitecture } catch {}
    if (-not $arch) {
        $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    }
    switch ($arch) {
        { $_ -in 'X64', 'AMD64' } { $arch = 'amd64'; break }
        'ARM64' { $arch = 'arm64'; break }
        default { throw "catflap install: no build for $arch (catflap has amd64 and arm64 builds)" }
    }
    $asset = "catflap_windows_$arch.exe"

    if ($env:CATFLAP_BASE_URL) {
        $base = $env:CATFLAP_BASE_URL.TrimEnd('/')
    } else {
        if ($version -eq 'latest') {
            $base = "https://github.com/$repo/releases/latest/download"
        } else {
            $base = "https://github.com/$repo/releases/download/$version"
        }
    }

    $dir = if ($env:CATFLAP_INSTALL_DIR) { $env:CATFLAP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\catflap' }
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('catflap-' + [guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        Write-Host "downloading $asset ($version)"
        $sums = Join-Path $tmp 'checksums.txt'
        $file = Join-Path $tmp $asset
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $sums
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $file

        $want = $null
        foreach ($line in Get-Content $sums) {
            $p = $line -split '\s+'
            if ($p.Count -ge 2 -and ($p[1] -eq $asset -or $p[1] -eq "*$asset")) { $want = $p[0].ToLower(); break }
        }
        if (-not $want) { throw "catflap install: checksums.txt has no entry for $asset" }
        $got = (Get-FileHash -Algorithm SHA256 -Path $file).Hash.ToLower()
        if ($got -ne $want) {
            throw "catflap install: checksum mismatch for $asset (expected $want, got $got); nothing was installed"
        }
        Write-Host 'checksum ok'

        # Read all output before looking at it: `| Select-Object -First 1` would stop
        # catflap mid-run and leave a random exit code.
        $out = @(& $file version)
        $code = $LASTEXITCODE
        $new = $out | Select-Object -First 1
        if ($code -ne 0 -or -not $new) {
            throw "catflap install: the downloaded binary does not run on this machine (exit code $code)"
        }

        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $dst = Join-Path $dir 'catflap.exe'
        # A running catflap.exe (e.g. the client agent) can't be deleted, but it can be
        # renamed: move it aside then. Leftovers from earlier upgrades go when unused.
        # A file that isn't running may still be held for a moment (virus scanner), so
        # deleting it is retried briefly before falling back to the rename.
        Get-ChildItem -Path $dir -Filter 'catflap.exe.old-*' -ErrorAction SilentlyContinue |
            ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
        if (Test-Path -LiteralPath $dst) {
            $running = @(Get-Process -Name catflap -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $dst }).Count -gt 0
            $tries = if ($running) { 0 } else { 10 }
            for ($i = 0; $i -lt $tries -and (Test-Path -LiteralPath $dst); $i++) {
                try { Remove-Item -LiteralPath $dst -Force } catch { Start-Sleep -Milliseconds 200 }
            }
            if (Test-Path -LiteralPath $dst) {
                Move-Item -LiteralPath $dst -Destination "$dst.old-$([guid]::NewGuid().ToString('N').Substring(0, 8))"
            }
        }
        Move-Item -LiteralPath $file -Destination $dst
        Write-Host "installed $new to $dst"

        # Edit the registry value directly: [Environment]::SetEnvironmentVariable would store
        # PATH as REG_SZ and expand entries like %USERPROFILE%\... for good.
        $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
        try {
            $raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
            $parts = @($raw -split ';' | Where-Object { $_ })
            $expanded = @($parts | ForEach-Object { [Environment]::ExpandEnvironmentVariables($_).TrimEnd('\') })
            if ($expanded -notcontains $dir.TrimEnd('\')) {
                $kind = if ($raw -and $key.GetValueKind('Path') -eq 'String') { 'String' } else { 'ExpandString' }
                $key.SetValue('Path', (($parts + $dir) -join ';'), [Microsoft.Win32.RegistryValueKind]$kind)
                # Tell Explorer that the environment changed (this call broadcasts WM_SETTINGCHANGE).
                [Environment]::SetEnvironmentVariable('CATFLAP_INSTALL_NOTIFY', $null, 'User')
                Write-Host "added $dir to your user PATH (new terminals pick it up)"
            }
        } finally {
            $key.Close()
        }
        if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }

        # Upgrades: a running homelab keeps the old version until it restarts. (cmd /c keeps
        # schtasks' error output away from PowerShell, which would stop on it.)
        cmd.exe /c 'schtasks /Query /TN catflap-homelab >nul 2>&1'
        if ($LASTEXITCODE -eq 0) {
            Write-Host 'this machine is a homelab: restart it with "catflap restart" to run the new version'
        }
    } finally {
        Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
    }
}
