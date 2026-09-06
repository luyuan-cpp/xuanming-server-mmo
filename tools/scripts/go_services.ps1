<#
.SYNOPSIS
    Unified Go micro-service launcher for local Windows development.

.DESCRIPTION
    Starts one, several, or all implemented go-zero services in the go/ directory.
    Each service runs as a separate background process; a stop command terminates them.

.PARAMETER Command
    start   – build & launch selected services (default: all)
    stop    – gracefully stop running services launched by this script
    status  – show which services are currently running
    list    – print the available service catalogue
    build   – compile native binaries for selected services -> bin\go_services\

.PARAMETER Services
    Comma-separated service names to start (e.g. "login,db").
    Defaults to all implemented services.

.EXAMPLE
    # Start every implemented Go service
    pwsh -File tools/scripts/go_services.ps1 -Command start

    # Start only login and db
    pwsh -File tools/scripts/go_services.ps1 -Command start -Services login,db

    # Check what's running
    pwsh -File tools/scripts/go_services.ps1 -Command status

    # Stop all services launched by this script
    pwsh -File tools/scripts/go_services.ps1 -Command stop
#>
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("start", "start-exe", "stop", "status", "list", "build")]
    [string]$Command,

    [string[]]$Services = @(),

    # Per-service instance counts for local multi-open. Accepts either:
    #   * hashtable (in-session use):   -Counts @{ login = 2; scene_manager = 2 }
    #   * string list (pwsh -File):     -Counts login=2,scene_manager=2
    # Service name keys must match the catalogue. Count <= 1 means single instance
    # (uses the original etc/<svc>.yaml). Count > 1 generates derived yaml files
    # under run/etc/go_services/<svc>_<i>.yaml with ListenOn port = base + (i-1).
    # 'db' is intentionally restricted to a single instance (Kafka consumer + DB writer).
    [object]$Counts = @{},

    # Base port offset between instances. The N-th instance (1-based) listens on
    # base + (i-1) * PortStride. Default 1 keeps ports densely packed.
    [int]$PortStride = 1,

    # Disable tier-staged startup (parallel launch like before). Pass when you
    # know dependencies are already satisfied or you want maximum parallelism.
    [switch]$NoTier,

    # Per-tier readiness budget (seconds) when staged startup is enabled. Soft
    # gate: probe TCP LISTEN on each instance port; warn-and-continue on timeout.
    [int]$TierReadySeconds = 10,

    # Local multi-zone launch. When > 0:
    #   * Instance keys are prefixed with 'z<Zone>_' so a second zone can coexist
    #     in the same PID file (e.g. z1_login_1, z2_login_1).
    #   * A derived yaml is materialized at run/etc/go_services/z<Zone>_<svc>[_<i>].yaml
    #     with `ZoneId: N` rewritten and `ListenOn` port shifted by (Zone-1)*ZonePortShift
    #     to avoid TCP port collisions.
    #   * `db` (AllowMultiInstance=false) IS still launched once per zone since each
    #     zone owns its own Kafka topic + MySQL DB; the per-zone single-instance rule
    #     is preserved.
    # Zone=0 (default) keeps legacy single-zone behaviour.
    [int]$Zone = 0,

    # Port offset between zones (Zone N port = base + (Zone-1) * ZonePortShift).
    # Default 1000 keeps zones cleanly separated even with PortStride=1.
    [int]$ZonePortShift = 1000
)

$ErrorActionPreference = "Stop"

$ScriptDir  = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot   = Resolve-Path (Join-Path $ScriptDir "..\..")
$GoRoot     = Join-Path $RepoRoot "go"
# Runtime artifacts (logs, pid files) live under run/, kept separate from
# build outputs in bin/. See docs/ops/log-management.md.
$RunDir     = Join-Path $RepoRoot "run"
$PidFile    = Join-Path $RunDir   "pids\go_services.pid.json"
$LogDir     = Join-Path $RunDir   "logs\go_services"
# Legacy fallback (older pid files written under bin/ before the run/ split).
$LegacyPidFile = Join-Path $RepoRoot "bin\go_services.pid.json"
# Built Go binaries still ship under bin/go_services\ alongside the C++ exes
# because that directory is the runtime working dir for local launches.
$GoBinDir   = Join-Path $RepoRoot "bin\go_services"

# ── Service catalogue ────────────────────────────────────────────────
# Order matters: infrastructure-layer services first, then domain services.
# Each entry:
#   Dir         relative path under go/
#   Entry       main .go file (used by `go run` and `go build`)
#   Port        base ListenOn port (instance #i listens on Port + (i-1)*PortStride)
#   Desc        human-readable description
#   ConfigFlag  command-line flag the entry uses to override the config path (e.g. -f, -loginService)
#   ConfigFile  default config file path relative to the service Dir
#   AllowMultiInstance  $false to forbid -Counts > 1 (e.g. db: single Kafka consumer)
#   Tier        startup ordering bucket. Lower tiers are launched and (softly)
#               wait for TCP LISTEN before the next tier. ONLY used by the local
#               dev launcher to avoid first-boot dial races. Runtime services do
#               not see this; they still discover endpoints via etcd (so rolling
#               upgrades / hot updates / canary releases are unaffected).
#               Tier 0 = infra-adjacent (db, data_service)
#               Tier 1 = scene_manager (depended by player_locator), client_rpc_router (gate 的唯一 gRPC 目标)
#               Tier 2 = player_locator (depended by login)
#               Tier 3 = login (top of the dial chain)
$ServiceCatalogue = [ordered]@{
    db              = @{ Dir = "db";              Entry = "db.go";                    Port = 6000;  Desc = "DB (Kafka consumer + MySQL)";       ConfigFlag = "-f";            ConfigFile = "etc/db.yaml";                    AllowMultiInstance = $false; Tier = 0 }
    data_service    = @{ Dir = "data_service";    Entry = "data_service.go";          Port = 9000;  Desc = "Data Service (multi-zone Redis)"; ConfigFlag = "-f";            ConfigFile = "etc/data_service.yaml";          AllowMultiInstance = $true;  Tier = 0 }
    # Base ports for the zRPC services were originally 50000/50200 but Windows
    # reserves dynamic-port ranges that frequently land on 50000-50171 and a
    # few apps (Manager.exe in dev environments) reuse 50200. Zone 1 (= base
    # + 0 shift) would otherwise fail with "An attempt was made to access a
    # socket in a way forbidden by its access permissions" or silent bind
    # failures. 53000/53200 sit safely above all observed Windows excluded
    # ranges (49728-49927, 50000-50171, 51573-51872) and below 60300 used by
    # scene_manager, so zones 1-4 each get a clean 1000-port slot.
    player_locator  = @{ Dir = "player_locator";  Entry = "player_locator.go";        Port = 53200; Desc = "Player Locator (Redis cache)";    ConfigFlag = "-f";            ConfigFile = "etc/player_locator.yaml";        AllowMultiInstance = $true;  Tier = 2 }
    login           = @{ Dir = "login";           Entry = "login.go";                 Port = 53000; Desc = "Login (gRPC + etcd)";            ConfigFlag = "-loginService"; ConfigFile = "etc/login.yaml";                 AllowMultiInstance = $true;  Tier = 3 }
    scene_manager   = @{ Dir = "scene_manager";   Entry = "scene_manager_service.go"; Port = 60300; Desc = "Scene Manager (Kafka + Redis)";    ConfigFlag = "-f";            ConfigFile = "etc/scene_manager_service.yaml"; AllowMultiInstance = $true;  Tier = 1 }
    # 回合制战斗匹配/切磋编排(docs/design/turn-based-battle-server.md §5.4)。
    # 50500 位于已观测的 Windows 保留区间(50000-50171)与 51573-51872 之间,安全;与 etc yaml ListenOn 保持一致。
    match           = @{ Dir = "match";           Entry = "match_service.go";         Port = 50500; Desc = "Match (turn-battle matchmaking)";  ConfigFlag = "-f";            ConfigFile = "etc/match_service.yaml";         AllowMultiInstance = $true;  Tier = 3 }
    # 客户端 RPC 路由服(docs/design/client-rpc-router.md D29-D34):gate 在 GATE_CLIENT_RPC_ROUTER=1 模式下
    # 唯一的 gRPC 目标,无状态、全局池不分 zone、可多开。Tier 1:gate 一起来就要连到它,与 scene_manager
    # 同批先起;它自身不依赖别的 Go 服务(只按 etcd 发现转发)。
    # 端口 50600 与 match 的 50500 同一安全区间(已观测的 Windows 保留区 50000-50171 与 51573-51872 之间),
    # 与 go/client_rpc_router/etc/client_rpc_router.yaml 的 ListenOn 保持一致。
    client_rpc_router = @{ Dir = "client_rpc_router"; Entry = "client_rpc_router_service.go"; Port = 50600; Desc = "Client RPC Router (gate 唯一 gRPC 目标)"; ConfigFlag = "-f"; ConfigFile = "etc/client_rpc_router.yaml"; AllowMultiInstance = $true; Tier = 1 }
}

# Derived per-instance config files live here so the source tree stays clean.
$DerivedEtcDir = Join-Path $RunDir "etc\go_services"

# Normalize $Counts (which may arrive as a hashtable, string, or string[]) to a
# plain hashtable<string,int>. pwsh -File serializes hashtable args to a string,
# so we accept the string form 'name=N,name=N' as a fallback.
function ConvertTo-CountsHashtable {
    param($Raw)
    $result = @{}
    if ($null -eq $Raw) { return $result }
    if ($Raw -is [hashtable]) {
        foreach ($k in $Raw.Keys) { $result[[string]$k] = [int]$Raw[$k] }
        return $result
    }
    $parts = @()
    if ($Raw -is [string]) {
        $parts = $Raw -split '[,;]' | Where-Object { $_ -ne '' }
    } elseif ($Raw -is [System.Collections.IEnumerable]) {
        foreach ($item in $Raw) {
            if ($item -is [string]) { $parts += ($item -split '[,;]' | Where-Object { $_ -ne '' }) }
        }
    } else {
        throw "Unsupported -Counts value type: $($Raw.GetType().FullName)"
    }
    foreach ($p in $parts) {
        $kv = $p -split '=', 2
        if ($kv.Count -ne 2) { throw "Invalid -Counts entry '$p'. Expected 'name=N'." }
        $result[$kv[0].Trim()] = [int]$kv[1].Trim()
    }
    return $result
}

$Counts = ConvertTo-CountsHashtable -Raw $Counts

# ── Helpers ──────────────────────────────────────────────────────────
function Resolve-ServiceList {
    param([string[]]$Requested)
    if ($Requested.Count -eq 0) {
        return @($ServiceCatalogue.Keys)
    }
    # Allow `-Services "a,b,c"` (single comma-joined string) in addition to
    # `-Services a,b,c` so pwsh -File invocations from .bat work the same way.
    $expanded = @()
    foreach ($s in $Requested) {
        $expanded += ($s -split '[,;]') | Where-Object { $_ -ne '' }
    }
    foreach ($s in $expanded) {
        if (-not $ServiceCatalogue.Contains($s)) {
            throw "Unknown service '$s'. Run with -Command list to see available services."
        }
    }
    return $expanded
}

# Returns the requested instance count for a service. Defaults to 1.
# Validates AllowMultiInstance and rejects negative / huge counts.
function Get-ServiceInstanceCount {
    param([string]$Name)
    if (-not $Counts.ContainsKey($Name)) { return 1 }
    $n = [int]$Counts[$Name]
    if ($n -lt 1) { return 1 }
    if ($n -gt 32) { throw "Refusing to launch $n instances of '$Name' (cap=32)." }
    if ($n -gt 1 -and -not $ServiceCatalogue[$Name].AllowMultiInstance) {
        throw "Service '$Name' does not support multi-instance (AllowMultiInstance=false). Counts entry: $Name=$n."
    }
    return $n
}

# Zone prefix used in instance keys / derived config filenames. Empty when
# Zone <= 0 to preserve legacy single-zone behaviour.
function Get-ZonePrefix {
    if ($Zone -le 0) { return "" }
    return "z${Zone}_"
}

# Builds the InstanceKey used everywhere (PID file, log file, derived yaml).
# Single instance preserves the legacy bare service name for backward compatibility,
# unless multi-zone is active (then keys are always zone-prefixed so multiple
# zones can coexist in the shared PID file).
function Get-InstanceKey {
    param([string]$Name, [int]$Index, [int]$Total)
    $zp = Get-ZonePrefix
    if ($Total -le 1) {
        if ($zp -eq "") { return $Name }
        return "${zp}${Name}"
    }
    return "${zp}${Name}_${Index}"
}

# For instance #1 of a single-instance launch with Zone=0, return the original
# etc path so behavior is unchanged. Otherwise materialize a derived yaml in
# run/etc/go_services with ListenOn (and ZoneId, when -Zone is set) rewritten.
# Windows(Hyper-V / WinNAT)每次开机会随机保留若干 TCP 端口区间(`netsh int ipv4 show excludedportrange protocol=tcp`),
# 固定端口落进去时 bind 报 "An attempt was made to access a socket in a way forbidden by its access permissions",
# go-zero 服务直接 panic(2026-08-22 player_locator 51200、2026-09-05 scene_manager 60300 都是这个坑)。
# 起服务前探一次:落在保留区间就往上挪到第一个不在任何区间里的端口,并强制走派生 yaml 写入;
# 对等方经 etcd 发现实际端口,不受影响。只规避保留区间,不规避"已被占用"(占用要么是陈旧实例要么是配置撞车,应当报错而不是悄悄换)。
$script:ExcludedPortRanges = $null
function Get-ExcludedPortRanges {
    if ($null -ne $script:ExcludedPortRanges) { return $script:ExcludedPortRanges }
    $ranges = @()
    try {
        $out = netsh interface ipv4 show excludedportrange protocol=tcp 2>$null
        foreach ($line in $out) {
            if ($line -match '^\s*(\d+)\s+(\d+)\s*\*?\s*$') { $ranges += ,@([int]$Matches[1], [int]$Matches[2]) }
        }
    } catch { }
    $script:ExcludedPortRanges = $ranges
    return $ranges
}
function Test-PortExcluded([int]$Port) {
    foreach ($r in (Get-ExcludedPortRanges)) { if ($Port -ge $r[0] -and $Port -le $r[1]) { return $true } }
    return $false
}
function Resolve-BindablePort([int]$Port, [string]$Label) {
    $p = $Port
    for ($i = 0; $i -lt 2000 -and (Test-PortExcluded $p); $i++) { $p++ }
    if ($p -ne $Port) {
        Write-Warning "[port] $Label : $Port 落在 Windows 保留端口区间,改用 $p(派生 yaml;对等方经 etcd 发现,不受影响)"
    }
    return $p
}

function Resolve-InstanceConfig {
    param(
        [string]$Name,
        [hashtable]$Info,
        [int]$Index,
        [int]$Total
    )
    $svcDir    = Join-Path $GoRoot $Info.Dir
    $baseYaml  = Join-Path $svcDir $Info.ConfigFile
    $zoneShift = if ($Zone -gt 0) { ($Zone - 1) * $ZonePortShift } else { 0 }
    $port      = [int]$Info.Port + $zoneShift + ($Index - 1) * $PortStride
    $port      = Resolve-BindablePort $port "$Name #$Index"   # 保留区间规避,见上

    # Legacy fast path: single-zone, single-instance -> original yaml as-is.
    if ($Total -le 1 -and $Zone -le 0 -and $port -eq [int]$Info.Port) {
        return [pscustomobject]@{
            Path     = $Info.ConfigFile  # relative path; resolved by working dir
            FullPath = $baseYaml
            Port     = [int]$Info.Port
            Derived  = $false
        }
    }

    if (-not (Test-Path $baseYaml)) {
        throw "Base config not found for '$Name' instance #$Index : $baseYaml"
    }
    if (-not (Test-Path $DerivedEtcDir)) {
        New-Item -ItemType Directory -Path $DerivedEtcDir -Force | Out-Null
    }

    $zp = Get-ZonePrefix
    $derivedName = if ($Total -le 1) { "${zp}${Name}.yaml" } else { "${zp}${Name}_${Index}.yaml" }
    $derivedPath = Join-Path $DerivedEtcDir $derivedName

    # Rewrite top-level ListenOn line. Keep the original host (e.g. 0.0.0.0 vs 127.0.0.1).
    # Use an instance Regex so the count-limited overload is available.
    $content = Get-Content $baseYaml -Raw
    $regex   = [regex]::new('(?m)^(ListenOn:\s*)([^:\s]+):(\d+)\s*$')
    if (-not $regex.IsMatch($content)) {
        Write-Warning "Did not find a top-level 'ListenOn:' line in $baseYaml; instance #$Index may collide on port $($Info.Port)."
    }
    $content = $regex.Replace(
        $content,
        { param($m) "$($m.Groups[1].Value)$($m.Groups[2].Value):$port" },
        1
    )

    # Rewrite top-level MetricsListenAddr (e.g. `MetricsListenAddr: ":9170"`) with the
    # SAME offset rule as ListenOn: base + (Zone-1)*ZonePortShift + (Index-1)*PortStride.
    # Without this every instance of scene_manager / db / match binds the same fixed
    # Prometheus port and the second instance (or second zone) dies on bind. Keeping
    # the rule identical to ListenOn means one mental model: zone 2 match #1 listens
    # on 51500 and exposes metrics on 10170. Services without the key are untouched.
    $metricsRegex = [regex]::new('(?m)^(?<lead>MetricsListenAddr:\s*"?)(?<host>[^:"\s]*):(?<port>\d+)(?<tail>"?\s*(#.*)?)$')
    if ($metricsRegex.IsMatch($content)) {
        $content = $metricsRegex.Replace(
            $content,
            { param($m)
                $metricsPort = [int]$m.Groups['port'].Value + $zoneShift + ($Index - 1) * $PortStride
                "$($m.Groups['lead'].Value)$($m.Groups['host'].Value):$metricsPort$($m.Groups['tail'].Value)"
            },
            1
        )
    }

    # Rewrite first ZoneId entry (handles both top-level 'ZoneId: N' and the
    # nested 'Node:\n  ZoneId: N' shape used by login/guild/friend/player_locator).
    if ($Zone -gt 0) {
        $zoneRegex = [regex]::new('(?m)^(?<lead>\s*)ZoneId:\s*\d+\s*$')
        if ($zoneRegex.IsMatch($content)) {
            $content = $zoneRegex.Replace(
                $content,
                { param($m) "$($m.Groups['lead'].Value)ZoneId: $Zone" },
                1
            )
        } else {
            Write-Warning "No 'ZoneId:' found in $baseYaml; '$Name' will not honour -Zone $Zone."
        }

        # Suffix every etcd RPC discovery key with .z<Zone> so each zone's
        # zrpc clients/servers form their own isolated load-balancer set.
        # Matches both the server's own Etcd.Key (e.g. 'Key: scenemanagerservice.rpc')
        # and every RpcClient.Etcd.Key (e.g. 'SceneManagerRpc.Etcd.Key', 'LoginAdminRpc.Etcd.Key').
        # Deliberately scoped to identifiers ending in '.rpc' to avoid touching
        # Redis cache prefixes (e.g. scene_manager.yaml has 'Redis.Key: scenemanagerservice').
        $keyRegex = [regex]::new('(?m)^(?<lead>\s*Key:\s*)(?<id>[\w.-]+\.rpc)\s*$')
        $content = $keyRegex.Replace(
            $content,
            { param($m) "$($m.Groups['lead'].Value)$($m.Groups['id'].Value).z$Zone" }
        )

        # Suffix Kafka consumer group IDs so two zones do not race for the
        # same partitions when they happen to subscribe to overlapping topics
        # (defence in depth -- per-zone topics are already used in practice).
        $groupRegex = [regex]::new('(?m)^(?<lead>\s*GroupID:\s*")(?<id>[^"\r\n]+)(?<tail>"\s*(#.*)?)$')
        $content = $groupRegex.Replace(
            $content,
            { param($m) "$($m.Groups['lead'].Value)$($m.Groups['id'].Value)_z$Zone$($m.Groups['tail'].Value)" }
        )
    }

    Set-Content -Path $derivedPath -Value $content -Encoding UTF8 -NoNewline

    return [pscustomobject]@{
        Path     = $derivedPath
        FullPath = $derivedPath
        Port     = $port
        Derived  = $true
    }
}

# Normalize a PID-file entry to a hashtable { Pid; Port; Service }.
# Accepts legacy int values (pre multi-instance) for backward compatibility.
function Read-InstanceEntry {
    param($RawValue, [string]$FallbackService, [int]$FallbackPort)
    if ($null -eq $RawValue) { return $null }
    if ($RawValue -is [int] -or $RawValue -is [long]) {
        return @{ Pid = [int]$RawValue; Port = $FallbackPort; Service = $FallbackService }
    }
    $procId = if ($RawValue.PSObject.Properties.Name -contains 'Pid') { [int]$RawValue.Pid } else { 0 }
    $port   = if ($RawValue.PSObject.Properties.Name -contains 'Port') { [int]$RawValue.Port } else { $FallbackPort }
    $svc    = if ($RawValue.PSObject.Properties.Name -contains 'Service') { [string]$RawValue.Service } else { $FallbackService }
    return @{ Pid = $procId; Port = $port; Service = $svc }
}

# Soft TCP LISTEN probe used between tiers. Returns $true once any process is
# accepting on 127.0.0.1:$Port, or $false on timeout. We deliberately do NOT
# fail the launch on timeout: this is just a "give earlier tiers a head start"
# heuristic to avoid first-boot dial races. Runtime services keep using etcd
# discovery + gRPC reconnect, so rolling upgrades / canary releases are unaffected.
function Wait-TcpListenReady {
    param(
        [int]$Port,
        [int]$TimeoutSeconds = 10
    )
    if ($Port -le 0) { return $true }
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $client = $null
        try {
            $client = [System.Net.Sockets.TcpClient]::new()
            $iar = $client.BeginConnect('127.0.0.1', $Port, $null, $null)
            if ($iar.AsyncWaitHandle.WaitOne(250) -and $client.Connected) {
                $client.EndConnect($iar) | Out-Null
                return $true
            }
        } catch {
            # connection refused / not listening yet
        } finally {
            if ($client) { $client.Close() }
        }
        Start-Sleep -Milliseconds 200
    }
    return $false
}

function Read-PidFile {
    if (Test-Path $PidFile) {
        $raw = Get-Content $PidFile -Raw | ConvertFrom-Json
        if ($null -eq $raw) { return [pscustomobject]@{} }
        return $raw
    }
    # One-time migration: pick up legacy pid file if present, then rewrite
    # under the new run/ location on next Write-PidFile.
    if (Test-Path $LegacyPidFile) {
        $raw = Get-Content $LegacyPidFile -Raw | ConvertFrom-Json
        if ($null -eq $raw) { return [pscustomobject]@{} }
        return $raw
    }
    return [pscustomobject]@{}
}

function Write-PidFile {
    param($Map)
    $dir = Split-Path -Parent $PidFile
    if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
    $Map | ConvertTo-Json | Set-Content $PidFile -Encoding UTF8
}

# ── Helpers (startup monitoring) ──────────────────────────────────────
function Wait-ForStartupBanner {
    param(
        [string]$LogFile,
        [string]$ServiceName,
        [int]$TimeoutSeconds = 30
    )
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            if (Test-Path $LogFile) {
                $content = Get-Content $LogFile -Raw -ErrorAction SilentlyContinue
                if ($content -and ($content -match "STARTED SUCCESSFULLY")) {
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
    Write-Host "        [timeout] $ServiceName did not report startup within ${TimeoutSeconds}s - check logs" -ForegroundColor Yellow
}

# ── Commands ─────────────────────────────────────────────────────────
function Resolve-GoExecutablePath {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ServiceName,
        [Parameter(Mandatory = $true)]
        [string]$ServiceDir
    )

    $preferred = Join-Path $GoBinDir "$ServiceName.exe"
    if (Test-Path $preferred) {
        return $preferred
    }

    $fallback = Join-Path $ServiceDir "$ServiceName.exe"
    if (Test-Path $fallback) {
        return $fallback
    }

    return $null
}

function Start-ServiceInstance {
    param(
        [string]$Name,
        [hashtable]$Info,
        [int]$Index,
        [int]$Total,
        [pscustomobject]$Pids,
        [switch]$UseExe
    )

    $svcDir = Join-Path $GoRoot $Info.Dir
    $instanceKey = Get-InstanceKey -Name $Name -Index $Index -Total $Total
    $cfg = Resolve-InstanceConfig -Name $Name -Info $Info -Index $Index -Total $Total

    # Skip if already running
    if ($Pids.PSObject.Properties.Name -contains $instanceKey) {
        $existing = Read-InstanceEntry -RawValue $Pids.$instanceKey -FallbackService $Name -FallbackPort $cfg.Port
        if ($existing -and $existing.Pid -gt 0) {
            try {
                $proc = Get-Process -Id $existing.Pid -ErrorAction Stop
                if (-not $proc.HasExited) {
                    Write-Host "[skip]  $instanceKey (PID $($existing.Pid) already running on :$($existing.Port))" -ForegroundColor Yellow
                    return $null
                }
            } catch {
                # process gone; will restart
            }
        }
    }

    $logOut  = Join-Path $LogDir "$instanceKey.stdout.log"
    $logErr  = Join-Path $LogDir "$instanceKey.stderr.log"

    $configFlag = $Info.ConfigFlag
    $configArg  = $cfg.Path

    if ($UseExe) {
        $exePath = Resolve-GoExecutablePath -ServiceName $Name -ServiceDir $svcDir
        if (-not $exePath) {
            Write-Warning "Skipping '$instanceKey': executable not found. Run -Command build first or place $Name.exe in bin\go_services\ or $svcDir."
            return $null
        }

        Write-Host "[start-exe] $instanceKey  :$($cfg.Port)  ($($Info.Desc))" -ForegroundColor Cyan

        $proc = Start-Process -FilePath $exePath `
            -ArgumentList @($configFlag, $configArg) `
            -WorkingDirectory $svcDir `
            -RedirectStandardOutput $logOut `
            -RedirectStandardError  $logErr `
            -PassThru `
            -WindowStyle Hidden
    }
    else {
        Write-Host "[start] $instanceKey  :$($cfg.Port)  ($($Info.Desc))" -ForegroundColor Cyan

        $proc = Start-Process -FilePath "go" `
            -ArgumentList @("run", $Info.Entry, $configFlag, $configArg) `
            -WorkingDirectory $svcDir `
            -RedirectStandardOutput $logOut `
            -RedirectStandardError  $logErr `
            -PassThru `
            -WindowStyle Hidden
    }

    $entry = [pscustomobject]@{
        Pid     = $proc.Id
        Port    = $cfg.Port
        Service = $Name
    }
    $Pids | Add-Member -NotePropertyName $instanceKey -NotePropertyValue $entry -Force
    if ($cfg.Derived) {
        Write-Host "        PID $($proc.Id)  cfg -> $($cfg.FullPath)" -ForegroundColor DarkGray
    }
    Write-Host "        logs -> run\logs\go_services\$instanceKey.*.log" -ForegroundColor DarkGray

    return [pscustomobject]@{
        InstanceKey = $instanceKey
        LogFile     = $logOut
        Port        = $cfg.Port
        Tier        = if ($Info.ContainsKey('Tier')) { [int]$Info.Tier } else { 99 }
    }
}

function Invoke-Start {
    param(
        [switch]$UseExe
    )

    $names = Resolve-ServiceList -Requested $Services
    $pids  = Read-PidFile
    $launchedServices = @()

    if (-not (Test-Path $LogDir)) { New-Item -ItemType Directory -Path $LogDir -Force | Out-Null }

    # Build a flat plan of (name, index, total, info, tier) entries. Validation
    # (multi-instance allow, missing dirs) happens here so we can still group
    # by tier afterwards without re-scanning.
    $plan = @()
    foreach ($name in $names) {
        $info = $ServiceCatalogue[$name]
        $svcDir = Join-Path $GoRoot $info.Dir
        if (-not (Test-Path $svcDir)) {
            Write-Warning "Skipping '$name': directory $svcDir does not exist."
            continue
        }
        $count = Get-ServiceInstanceCount -Name $name
        $tier  = if ($info.ContainsKey('Tier')) { [int]$info.Tier } else { 99 }
        for ($i = 1; $i -le $count; $i++) {
            $plan += [pscustomobject]@{
                Name = $name; Info = $info; Index = $i; Total = $count; Tier = $tier
            }
        }
    }

    if ($plan.Count -eq 0) {
        Write-Host "No services to launch." -ForegroundColor Yellow
        return
    }

    if ($NoTier) {
        # Legacy parallel path: launch every instance back-to-back, then wait
        # for startup banners at the end. Useful when caller knows there is no
        # cross-service dial dependency to honor.
        foreach ($p in $plan) {
            $launched = Start-ServiceInstance -Name $p.Name -Info $p.Info -Index $p.Index -Total $p.Total -Pids $pids -UseExe:$UseExe
            if ($launched) { $launchedServices += $launched }
        }
        Write-PidFile $pids
        if ($launchedServices.Count -gt 0) {
            Write-Host "`nWaiting for startup confirmation..." -ForegroundColor DarkGray
            foreach ($svc in $launchedServices) {
                Wait-ForStartupBanner -LogFile $svc.LogFile -ServiceName $svc.InstanceKey
            }
        }
        Write-Host "`nAll requested services launched. Use -Command status to check health." -ForegroundColor Green
        return
    }

    # Tier-staged path. Group plan entries by tier ascending. Within a tier we
    # launch all instances in parallel, then issue a TCP LISTEN probe for each
    # before moving on. The probe is a soft gate: timeout warns and continues
    # so a stuck service never blocks the whole batch.
    $tiers = $plan | Group-Object -Property Tier | Sort-Object { [int]$_.Name }
    foreach ($tierGroup in $tiers) {
        $tierName = $tierGroup.Name
        $entries  = $tierGroup.Group
        Write-Host "`n--- Tier $tierName ($($entries.Count) instance(s)) ---" -ForegroundColor Cyan

        $launchedThisTier = @()
        foreach ($p in $entries) {
            $launched = Start-ServiceInstance -Name $p.Name -Info $p.Info -Index $p.Index -Total $p.Total -Pids $pids -UseExe:$UseExe
            if ($launched) {
                $launchedServices += $launched
                $launchedThisTier += $launched
            }
        }

        # Persist PIDs after each tier so a Ctrl+C between tiers does not lose state.
        Write-PidFile $pids

        if ($launchedThisTier.Count -gt 0) {
            Write-Host "  waiting up to ${TierReadySeconds}s for tier $tierName to LISTEN..." -ForegroundColor DarkGray
            foreach ($svc in $launchedThisTier) {
                $ok = Wait-TcpListenReady -Port $svc.Port -TimeoutSeconds $TierReadySeconds
                if ($ok) {
                    Write-Host "  [ready]   $($svc.InstanceKey) :$($svc.Port)" -ForegroundColor Green
                } else {
                    Write-Host "  [warn]    $($svc.InstanceKey) :$($svc.Port) not LISTEN within ${TierReadySeconds}s; continuing anyway" -ForegroundColor Yellow
                }
            }
        }
    }

    if ($launchedServices.Count -gt 0) {
        Write-Host "`nWaiting for startup banners..." -ForegroundColor DarkGray
        foreach ($svc in $launchedServices) {
            Wait-ForStartupBanner -LogFile $svc.LogFile -ServiceName $svc.InstanceKey
        }
    }

    Write-Host "`nAll requested services launched. Use -Command status to check health." -ForegroundColor Green
}

function Invoke-Stop {
    $pids = Read-PidFile
    if (@($pids.PSObject.Properties).Count -eq 0) {
        Write-Host "No tracked services to stop." -ForegroundColor Yellow
        return
    }

    # Build the target instance key list. -Services filters by base service name
    # and expands to every running instance of that service (e.g. login_1, login_2).
    if ($Services.Count -gt 0) {
        $targetServices = Resolve-ServiceList -Requested $Services
        $instanceKeys = @()
        foreach ($prop in $pids.PSObject.Properties) {
            $info = if ($ServiceCatalogue.Contains($prop.Name)) { $ServiceCatalogue[$prop.Name] } else { $null }
            $port = if ($info) { $info.Port } else { 0 }
            $entry = Read-InstanceEntry -RawValue $prop.Value -FallbackService $prop.Name -FallbackPort $port
            if ($entry -and ($targetServices -contains $entry.Service)) {
                $instanceKeys += $prop.Name
            }
        }
    } else {
        $instanceKeys = @($pids.PSObject.Properties.Name)
    }

    foreach ($key in $instanceKeys) {
        if ($pids.PSObject.Properties.Name -notcontains $key) { continue }
        $entry = Read-InstanceEntry -RawValue $pids.$key -FallbackService $key -FallbackPort 0
        $procId = $entry.Pid
        try {
            $proc = Get-Process -Id $procId -ErrorAction Stop
            if (-not $proc.HasExited) {
                Stop-Process -Id $procId -Force
                Write-Host "[stop]  $key (PID $procId)" -ForegroundColor Magenta
            } else {
                Write-Host "[gone]  $key (PID $procId already exited)" -ForegroundColor DarkGray
            }
        } catch {
            Write-Host "[gone]  $key (PID $procId not found)" -ForegroundColor DarkGray
        }
        $pids.PSObject.Properties.Remove($key)
    }

    Write-PidFile $pids
}

function Invoke-Status {
    $pids = Read-PidFile
    if (@($pids.PSObject.Properties).Count -eq 0) {
        Write-Host "No tracked services." -ForegroundColor Yellow
        return
    }
    Write-Host ("{0,-22} {1,-8} {2,-8} {3}" -f "INSTANCE", "PID", "PORT", "STATUS") -ForegroundColor White
    Write-Host ("{0,-22} {1,-8} {2,-8} {3}" -f "--------", "---", "----", "------")
    foreach ($prop in $pids.PSObject.Properties) {
        $key = $prop.Name
        # Derive base service name for catalogue lookup. Strips optional zone
        # prefix (z<N>_) and optional instance suffix (_<i>):
        #   login        -> login
        #   login_2      -> login
        #   z1_login     -> login
        #   z2_login_3   -> login
        $baseName = if ($ServiceCatalogue.Contains($key)) { $key }
                    elseif ($key -match '^(?:z\d+_)?(?<svc>.+?)(?:_\d+)?$' -and $ServiceCatalogue.Contains($Matches.svc)) { $Matches.svc }
                    else { $key }
        $basePort = if ($ServiceCatalogue.Contains($baseName)) { $ServiceCatalogue[$baseName].Port } else { 0 }
        $entry    = Read-InstanceEntry -RawValue $prop.Value -FallbackService $baseName -FallbackPort $basePort
        $procId   = $entry.Pid
        $port     = if ($entry.Port -gt 0) { $entry.Port } else { $basePort }
        $status = "UNKNOWN"
        try {
            $proc = Get-Process -Id $procId -ErrorAction Stop
            $status = if ($proc.HasExited) { "EXITED" } else { "RUNNING" }
        } catch {
            $status = "GONE"
        }
        $color = switch ($status) { "RUNNING" { "Green" } "EXITED" { "Red" } default { "DarkGray" } }
        Write-Host ("{0,-22} {1,-8} {2,-8} {3}" -f $key, $procId, $port, $status) -ForegroundColor $color
    }
}

function Invoke-List {
    Write-Host "`nAvailable Go services:`n" -ForegroundColor Cyan
    Write-Host ("{0,-18} {1,-8} {2}" -f "NAME", "PORT", "DESCRIPTION") -ForegroundColor White
    Write-Host ("{0,-18} {1,-8} {2}" -f "----", "----", "-----------")
    foreach ($kv in $ServiceCatalogue.GetEnumerator()) {
        Write-Host ("{0,-18} {1,-8} {2}" -f $kv.Key, $kv.Value.Port, $kv.Value.Desc)
    }
    Write-Host ""
}

function Invoke-Build {
    $names = Resolve-ServiceList -Requested $Services
    $outDir = Join-Path $RepoRoot "bin\go_services"
    if (-not (Test-Path $outDir)) { New-Item -ItemType Directory -Path $outDir -Force | Out-Null }

    $failed = @()
    foreach ($name in $names) {
        $info   = $ServiceCatalogue[$name]
        $svcDir = Join-Path $GoRoot $info.Dir

        if (-not (Test-Path $svcDir)) {
            Write-Warning "Skipping '$name': directory $svcDir does not exist."
            $failed += $name
            continue
        }

        $outExe = Join-Path $outDir "$name.exe"
        Write-Host "[build] $name -> bin\go_services\$name.exe" -ForegroundColor Cyan

        Push-Location $svcDir
        try {
            go build -o $outExe "./$($info.Entry)"
            if ($LASTEXITCODE -ne 0) {
                Write-Host "[FAIL]  $name" -ForegroundColor Red
                $failed += $name
            } else {
                Write-Host "[ok]    $name" -ForegroundColor Green
            }
        } finally {
            Pop-Location
        }
    }

    if ($failed.Count -gt 0) {
        throw "Build failed for: $($failed -join ', ')"
    }

    Write-Host "`nAll requested services built -> bin\go_services\" -ForegroundColor Green
}

# ── Dispatch ─────────────────────────────────────────────────────────
switch ($Command) {
    "start"      { Invoke-Start }
    "start-exe"  { Invoke-Start -UseExe }
    "stop"   { Invoke-Stop }
    "status" { Invoke-Status }
    "list"   { Invoke-List }
    "build"  { Invoke-Build }
}
