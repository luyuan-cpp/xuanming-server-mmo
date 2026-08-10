<#
.SYNOPSIS
    C++ node launcher for local Windows development.

.DESCRIPTION
    Starts, stops, or queries C++ game-server nodes (gate, scene) from the bin/ directory.
    Each node runs as a separate background process with PID tracking and per-node logging.

.PARAMETER Command
    start   – launch selected nodes (default: all)
    stop    – stop running nodes launched by this script
    status  – show which nodes are currently running
    list    – print the available node catalogue

.PARAMETER Nodes
    Comma-separated node names to start (e.g. "gate,scene").
    Defaults to all nodes.

.PARAMETER GateCount
    Number of gate.exe instances to launch (default: 1).

.PARAMETER SceneCount
    Number of scene.exe instances to launch (default: 1).

.PARAMETER NodeIp
    Address these nodes advertise into etcd (NODE_IP). Defaults to this
    machine's detected physical-NIC IPv4, so no configuration is needed for
    either same-box or off-box clients. Accepts an explicit address, or
    'loopback' / 'engine' to opt out of detection. See the parameter block.

.EXAMPLE
    # Start all nodes with default counts
    pwsh -File tools/scripts/cpp_nodes.ps1 -Command start

    # Start 2 gates and 4 scenes
    pwsh -File tools/scripts/cpp_nodes.ps1 -Command start -GateCount 2 -SceneCount 4

    # Start only scene nodes
    pwsh -File tools/scripts/cpp_nodes.ps1 -Command start -Nodes scene

    # Pin the advertised address instead of detecting it
    pwsh -File tools/scripts/cpp_nodes.ps1 -Command start -NodeIp 192.168.2.28

    # Strictly single-box stack; never goes stale when the network changes
    pwsh -File tools/scripts/cpp_nodes.ps1 -Command start -NodeIp loopback

    # Check what's running
    pwsh -File tools/scripts/cpp_nodes.ps1 -Command status

    # Stop all nodes
    pwsh -File tools/scripts/cpp_nodes.ps1 -Command stop
#>
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("start", "stop", "status", "list")]
    [string]$Command,

    [string[]]$Nodes = @(),

    [int]$GateCount  = 1,
    [int]$SceneCount = 1,

    # Local multi-zone launch. When > 0:
    #   * Instance keys are prefixed with 'z<Zone>_' so a second zone can
    #     coexist in the same PID file (e.g. z1_gate_1, z2_gate_1).
    #   * Each child process inherits ZONE_ID=<Zone>, which the C++ engine
    #     honours via libs/engine/config/config.cpp readGameConfig() override.
    # Zone=0 (default) keeps legacy single-zone behaviour.
    [int]$Zone = 0,

    # Address these nodes publish into etcd for other nodes and clients to dial,
    # passed to the child process as NODE_IP (see Node::ResolveNodeIp).
    #
    # This is the *advertise* address, not the bind address: nodes always listen
    # on 0.0.0.0 (override with BIND_IP). The two are separate on purpose, the
    # same way etcd splits --listen-client-urls from --advertise-client-urls and
    # Kafka splits listeners from advertised.listeners. Publishing an address
    # that is local-but-wrong is the classic failure mode here — every process
    # reports healthy and nothing can connect.
    #
    #   (empty)    default — detect this machine's physical-NIC IPv4, falling
    #              back to 127.0.0.1. Requires no configuration from anyone and
    #              works whether the client runs on this box or another device.
    #   auto       same detection, but ignore any inherited NODE_IP
    #   <ip>       advertise exactly this address
    #   loopback   force 127.0.0.1 — most robust for a strictly single-box stack,
    #              since it never goes stale when the network changes
    #   engine     leave NODE_IP unset and let the C++ side pick
    #              (libs/engine/core/network/process_info.cpp localip())
    #
    # Detection asks Windows for physical adapters instead of matching names, so
    # Hyper-V / WSL / Docker 'vEthernet' switches, VMware, VirtualBox and VPN
    # tunnels are all excluded — see Find-PhysicalNicIPv4 for why that matters.
    # Deliberately not a hard-coded LAN address: that would be wrong on every
    # other machine. An already-exported NODE_IP beats detection but loses to an
    # explicit -NodeIp.
    [string]$NodeIp = ""
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot  = Resolve-Path (Join-Path $ScriptDir "..\..")
$BinDir    = Join-Path $RepoRoot "bin"
# Runtime artifacts (logs, pid files) live under run/, kept separate from
# build outputs and the C++ runtime working dir in bin/.
# See docs/ops/log-management.md.
$RunDir    = Join-Path $RepoRoot "run"
$PidFile   = Join-Path $RunDir   "pids\cpp_nodes.pid.json"
$LogDir    = Join-Path $RunDir   "logs\cpp_nodes"
# Legacy fallback for pid file written under bin/ before the run/ split.
$LegacyPidFile = Join-Path $RepoRoot "bin\cpp_nodes.pid.json"

# ── Node catalogue ───────────────────────────────────────────────
$NodeCatalogue = [ordered]@{
    gate  = @{ Exe = "gate.exe";  Desc = "Gate Node (client TCP bridge + Kafka routing)" }
    scene = @{ Exe = "scene.exe"; Desc = "Scene Node (gameplay + ECS systems)" }
}

# ── Helpers ──────────────────────────────────────────────────────
function Resolve-NodeList {
    param([string[]]$Requested)
    if ($Requested.Count -eq 0) {
        return @($NodeCatalogue.Keys)
    }
    foreach ($n in $Requested) {
        if (-not $NodeCatalogue.Contains($n)) {
            throw "Unknown node '$n'. Run with -Command list to see available nodes."
        }
    }
    return $Requested
}

function Get-InstanceCount {
    param([string]$NodeName)
    switch ($NodeName) {
        "gate"  { return $GateCount }
        "scene" { return $SceneCount }
        default { return 1 }
    }
}

function Find-PhysicalNicIPv4 {
    # Pick this machine's real LAN IPv4 by asking Windows which adapters are
    # physical, rather than by pattern-matching adapter names.
    #
    # Get-NetAdapter -Physical is what makes this reliable: it excludes Hyper-V /
    # WSL / Docker Desktop 'vEthernet' switches, VMware and VirtualBox adapters,
    # VPN tunnels, and loopback in one shot. Those are exactly the addresses that
    # make naive detection pick a wrong-but-local IP -- gethostbyname(hostname)
    # returns them in an order set by interface metric, and probing the routing
    # table follows the default route straight into an active VPN tunnel.
    #
    # Remaining filters: link-local (169.254.x, adapter up but unconfigured) and
    # PrefixOrigin WellKnown are not usable addresses. When a box has several
    # physical NICs up (laptop docked over Ethernet with WiFi still on), the
    # lowest InterfaceMetric is the one Windows itself prefers.
    #
    # Returns $null when nothing qualifies or the cmdlets are unavailable (non-
    # Windows pwsh); the caller falls back to loopback.
    try {
        $physicalIndexes = @(
            Get-NetAdapter -Physical -ErrorAction Stop |
                Where-Object { $_.Status -eq 'Up' } |
                Select-Object -ExpandProperty InterfaceIndex
        )
        if ($physicalIndexes.Count -eq 0) { return $null }

        $candidates = @(
            Get-NetIPAddress -AddressFamily IPv4 -ErrorAction Stop |
                Where-Object {
                    $_.InterfaceIndex -in $physicalIndexes -and
                    $_.PrefixOrigin -in 'Dhcp', 'Manual' -and
                    $_.IPAddress -ne '127.0.0.1' -and
                    $_.IPAddress -notlike '169.254.*' -and
                    -not $_.SkipAsSource
                }
        )
        if ($candidates.Count -eq 0) { return $null }

        $best = $candidates |
            Sort-Object { (Get-NetIPInterface -InterfaceIndex $_.InterfaceIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue).InterfaceMetric } |
            Select-Object -First 1

        if ($candidates.Count -gt 1) {
            $others = ($candidates | Where-Object { $_.IPAddress -ne $best.IPAddress } | ForEach-Object { "$($_.IPAddress) ($($_.InterfaceAlias))" }) -join ', '
            Write-Host "[addr]  multiple physical NICs up; chose lowest-metric one, ignoring: $others" -ForegroundColor DarkGray
        }
        return $best
    } catch {
        return $null
    }
}

function Resolve-AdvertiseIp {
    # Precedence: explicit -NodeIp > already-exported NODE_IP > detected LAN IP
    # > loopback. Returns $null only for 'engine', meaning "leave NODE_IP unset
    # and let the C++ side decide".
    #
    # Detecting a LAN address rather than defaulting to loopback is what keeps
    # this zero-configuration for the off-box case too: a client running on a
    # phone or a second PC can reach the gate without anyone passing a flag.
    # It is safe to guess here because nodes bind 0.0.0.0 -- if detection picks
    # some other local address, same-box traffic still connects; only an off-box
    # client would notice, and that is the case where you would pass -NodeIp
    # explicitly anyway.
    switch ($NodeIp) {
        "engine"   { return $null }
        "loopback" { return "127.0.0.1" }
    }
    if (-not [string]::IsNullOrWhiteSpace($NodeIp) -and $NodeIp -ne "auto") {
        return $NodeIp.Trim()
    }
    if ($NodeIp -ne "auto" -and -not [string]::IsNullOrWhiteSpace($env:NODE_IP)) {
        return $env:NODE_IP.Trim()
    }

    $detected = Find-PhysicalNicIPv4
    if ($null -ne $detected) {
        Write-Host "[addr]  detected LAN address on '$($detected.InterfaceAlias)'" -ForegroundColor DarkGray
        return $detected.IPAddress
    }

    Write-Host "[addr]  no usable physical NIC found; falling back to loopback (off-box clients will not connect)" -ForegroundColor Yellow
    return "127.0.0.1"
}

function Wait-ForStartupBanner {
    param(
        [string]$LogFile,
        [string]$InstanceKey,
        [int]$TimeoutSeconds = 30
    )
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            if (Test-Path $LogFile) {
                $content = Get-Content $LogFile -Raw -ErrorAction SilentlyContinue
                if ($content -and ($content -match "STARTED SUCCESSFULLY")) {
                    # Extract lines between the first and last ===== separator
                    $lines = $content -split "`n"
                    $inBanner = $false
                    foreach ($line in $lines) {
                        $trimmed = $line.Trim()
                        if ($trimmed -match '^={5,}') {
                            if ($inBanner) {
                                Write-Host "        $trimmed" -ForegroundColor Green
                                break
                            }
                            $inBanner = $true
                        }
                        if ($inBanner -and $trimmed.Length -gt 0) {
                            Write-Host "        $trimmed" -ForegroundColor Green
                        }
                    }
                    return
                }
            }
        } catch {
            # File might be locked briefly; retry on next iteration
        }
        Start-Sleep -Milliseconds 500
    }
    Write-Host "        [timeout] $InstanceKey did not report startup within ${TimeoutSeconds}s - check logs" -ForegroundColor Yellow
}

function Read-PidFile {
    if (Test-Path $PidFile) {
        return Get-Content $PidFile -Raw | ConvertFrom-Json
    }
    if (Test-Path $LegacyPidFile) {
        return Get-Content $LegacyPidFile -Raw | ConvertFrom-Json
    }
    return [pscustomobject]@{}
}

function Write-PidFile {
    param($Map)
    $dir = Split-Path -Parent $PidFile
    if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
    $Map | ConvertTo-Json | Set-Content $PidFile -Encoding UTF8
}

# ── Commands ─────────────────────────────────────────────────────
function Invoke-Start {
    $names = Resolve-NodeList -Requested $Nodes
    $pids  = Read-PidFile
    $launchedInstances = @()

    # Resolved once so every instance in this batch advertises the same address.
    $advertiseIp = Resolve-AdvertiseIp
    if ($null -eq $advertiseIp) {
        Write-Host "[addr]  NODE_IP unset - nodes will detect their own address" -ForegroundColor DarkGray
    } else {
        Write-Host "[addr]  advertising NODE_IP=$advertiseIp (listen stays 0.0.0.0)" -ForegroundColor DarkGray
    }

    if (-not (Test-Path $LogDir)) { New-Item -ItemType Directory -Path $LogDir -Force | Out-Null }

    # muduo AsyncLogging fopen("a") does NOT create intermediate directories on
    # Windows; a missing folder triggers assert(fp_) in FileUtil::AppendFile and
    # aborts gate.exe / scene.exe before any log line reaches disk. Node uses
    # cwd-relative "logs/cpp_nodes/<short>" as the rolled-log basename, so make
    # sure that folder exists under the working directory (bin\) before launch.
    $MuduoLogDir = Join-Path $BinDir "logs\cpp_nodes"
    if (-not (Test-Path $MuduoLogDir)) { New-Item -ItemType Directory -Path $MuduoLogDir -Force | Out-Null }

    foreach ($name in $names) {
        $info = $NodeCatalogue[$name]
        $exePath = Join-Path $BinDir $info.Exe

        if (-not (Test-Path $exePath)) {
            Write-Warning "Skipping '$name': $exePath not found. Build the solution first (msbuild game.sln)."
            continue
        }

        $count = Get-InstanceCount -NodeName $name
        $zonePrefix = if ($Zone -gt 0) { "z${Zone}_" } else { "" }

        for ($i = 1; $i -le $count; $i++) {
            $bareKey = if ($count -eq 1) { $name } else { "${name}_${i}" }
            $instanceKey = "${zonePrefix}${bareKey}"

            # Skip if already running
            if ($pids.PSObject.Properties.Name -contains $instanceKey) {
                $existingPid = $pids.$instanceKey
                try {
                    $proc = Get-Process -Id $existingPid -ErrorAction Stop
                    if (-not $proc.HasExited) {
                        Write-Host "[skip]  $instanceKey (PID $existingPid already running)" -ForegroundColor Yellow
                        continue
                    }
                } catch {
                    # process gone; will restart
                }
            }

            $logOut = Join-Path $LogDir "$instanceKey.stdout.log"
            $logErr = Join-Path $LogDir "$instanceKey.stderr.log"

            Write-Host "[start] $instanceKey  ($($info.Desc))" -ForegroundColor Cyan

            # ZONE_ID and NODE_IP are read by the C++ engine at startup and
            # override the YAML ZoneId / detected address respectively. Temporarily
            # mutate the parent shell env so the child process inherits them;
            # Start-Process has no native per-process env override on Windows
            # PowerShell. Both are restored in the finally block, including the
            # "was not set before" case.
            $prevZoneEnv   = $env:ZONE_ID
            $prevNodeIpEnv = $env:NODE_IP
            if ($Zone -gt 0) { $env:ZONE_ID = "$Zone" }
            if ($null -ne $advertiseIp) { $env:NODE_IP = $advertiseIp }
            try {
                $proc = Start-Process -FilePath $exePath `
                    -WorkingDirectory $BinDir `
                    -RedirectStandardOutput $logOut `
                    -RedirectStandardError  $logErr `
                    -PassThru `
                    -WindowStyle Hidden
            } finally {
                if ($null -eq $prevZoneEnv) { Remove-Item Env:ZONE_ID -ErrorAction SilentlyContinue }
                else { $env:ZONE_ID = $prevZoneEnv }
                if ($null -eq $prevNodeIpEnv) { Remove-Item Env:NODE_IP -ErrorAction SilentlyContinue }
                else { $env:NODE_IP = $prevNodeIpEnv }
            }

            $pids | Add-Member -NotePropertyName $instanceKey -NotePropertyValue $proc.Id -Force
            Write-Host "        PID $($proc.Id)  logs -> run\logs\cpp_nodes\$instanceKey.*.log" -ForegroundColor DarkGray
            $launchedInstances += @{ Key = $instanceKey; LogFile = $logOut }

            # Small delay between launches (same pattern as original start_server.bat)
            if ($i -lt $count) {
                Start-Sleep -Milliseconds 500
            }
        }

        # Delay between different node types
        Start-Sleep -Milliseconds 500
    }

    Write-PidFile $pids

    if ($launchedInstances.Count -gt 0) {
        Write-Host "`nWaiting for startup confirmation..." -ForegroundColor DarkGray
        foreach ($inst in $launchedInstances) {
            Wait-ForStartupBanner -LogFile $inst.LogFile -InstanceKey $inst.Key
        }
    }

    Write-Host "`nAll requested nodes launched. Use -Command status to check." -ForegroundColor Green
}

function Invoke-Stop {
    $pids = Read-PidFile
    if (@($pids.PSObject.Properties).Count -eq 0) {
        Write-Host "No tracked nodes to stop." -ForegroundColor Yellow
        return
    }

    $requestedNames = if ($Nodes.Count -gt 0) { Resolve-NodeList -Requested $Nodes } else { $null }

    foreach ($prop in @($pids.PSObject.Properties)) {
        $instanceKey = $prop.Name
        $procId = $prop.Value

        # Filter by node name if specified
        if ($null -ne $requestedNames) {
            # Strip optional zone prefix (z<N>_) and instance suffix (_<i>) to
            # recover the catalogue base name (e.g. z2_gate_3 -> gate).
            $baseName = $instanceKey
            if ($baseName -match '^z\d+_(?<rest>.+)$') { $baseName = $Matches.rest }
            $baseName = ($baseName -split '_')[0]
            if ($baseName -notin $requestedNames) { continue }
        }

        try {
            $proc = Get-Process -Id $procId -ErrorAction Stop
            if (-not $proc.HasExited) {
                Stop-Process -Id $procId -Force
                Write-Host "[stop]  $instanceKey (PID $procId)" -ForegroundColor Magenta
            } else {
                Write-Host "[gone]  $instanceKey (PID $procId already exited)" -ForegroundColor DarkGray
            }
        } catch {
            Write-Host "[gone]  $instanceKey (PID $procId not found)" -ForegroundColor DarkGray
        }
        $pids.PSObject.Properties.Remove($instanceKey)
    }

    Write-PidFile $pids
}

function Invoke-Status {
    $pids = Read-PidFile
    if (@($pids.PSObject.Properties).Count -eq 0) {
        Write-Host "No tracked nodes." -ForegroundColor Yellow
        return
    }
    Write-Host ("{0,-18} {1,-8} {2}" -f "NODE", "PID", "STATUS") -ForegroundColor White
    Write-Host ("{0,-18} {1,-8} {2}" -f "----", "---", "------")
    foreach ($prop in $pids.PSObject.Properties) {
        $instanceKey = $prop.Name
        $procId = $prop.Value
        $status = "UNKNOWN"
        try {
            $proc = Get-Process -Id $procId -ErrorAction Stop
            $status = if ($proc.HasExited) { "EXITED" } else { "RUNNING" }
        } catch {
            $status = "GONE"
        }
        $color = switch ($status) { "RUNNING" { "Green" } "EXITED" { "Red" } default { "DarkGray" } }
        Write-Host ("{0,-18} {1,-8} {2}" -f $instanceKey, $procId, $status) -ForegroundColor $color
    }
}

function Invoke-List {
    Write-Host "`nAvailable C++ nodes:`n" -ForegroundColor Cyan
    Write-Host ("{0,-10} {1,-15} {2}" -f "NAME", "EXECUTABLE", "DESCRIPTION") -ForegroundColor White
    Write-Host ("{0,-10} {1,-15} {2}" -f "----", "----------", "-----------")
    foreach ($kv in $NodeCatalogue.GetEnumerator()) {
        Write-Host ("{0,-10} {1,-15} {2}" -f $kv.Key, $kv.Value.Exe, $kv.Value.Desc)
    }
    Write-Host "`nInstance counts (adjustable): -GateCount $GateCount  -SceneCount $SceneCount" -ForegroundColor DarkGray
    Write-Host ""
}

# ── Dispatch ─────────────────────────────────────────────────────
switch ($Command) {
    "start"  { Invoke-Start }
    "stop"   { Invoke-Stop }
    "status" { Invoke-Status }
    "list"   { Invoke-List }
}
