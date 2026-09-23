#requires -Version 7
<#
.SYNOPSIS
    Go 本机启动器的端口分配、已有实例复用和监听归属回归。
.DESCRIPTION
    只加载脚本 AST 中的函数;网络、进程查询和启动均使用桩,配置仅写入测试临时目录。
    不启动或停止真实服务,不连接端口,不修改真实 PID 文件与派生配置。
.EXAMPLE
    pwsh -File tools/scripts/tests/go_services_ports.tests.ps1
#>
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$launcherPath = Join-Path $PSScriptRoot '../go_services.ps1'
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile($launcherPath, [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
foreach ($definition in $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    . ([scriptblock]::Create($definition.Extent.Text))
}

$testRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('mmorpg-go-ports-tests-' + [guid]::NewGuid().ToString('N'))
$RepoRoot = $testRoot
$GoRoot = Join-Path $testRoot 'go'
$GoBinDir = Join-Path $testRoot 'bin'
$DerivedEtcDir = Join-Path $testRoot 'run/etc/go_services'
$LogDir = Join-Path $testRoot 'run/logs'
$PidFile = Join-Path $testRoot 'run/pids.json'
$LegacyPidFile = Join-Path $testRoot 'absent-legacy-pids.json'
$Zone = 0
$ZonePortShift = 1000
$PortStride = 1
$Counts = @{}
$NoTier = $true
$ServiceCatalogue = [ordered]@{
    chat = @{ Dir = 'chat'; Entry = 'chat.go'; Port = 50700; Desc = '聊天'; ConfigFlag = '-f'; ConfigFile = 'etc/chat.yaml'; AllowMultiInstance = $true; Tier = 1 }
    trade = @{ Dir = 'trade'; Entry = 'trade.go'; Port = 50800; Desc = '交易'; ConfigFlag = '-f'; ConfigFile = 'etc/trade.yaml'; AllowMultiInstance = $true; Tier = 1 }
}

function Get-NetTCPConnection {
    [CmdletBinding()] param([string]$State)
    if ($script:NetworkQueryFailed) { throw '网络查询失败桩' }
    if ($State -ne 'Listen') { throw '网络桩只接受监听查询' }
    return $script:Listeners
}
function Get-CimInstance {
    [CmdletBinding()] param([string]$ClassName, [string]$Filter)
    if ($script:ProcessQueryFailed) { throw '进程查询失败桩' }
    if ($ClassName -ne 'Win32_Process') { throw '进程桩只接受 Win32_Process' }
    if ($Filter -match '^ProcessId = (\d+)$') { return $script:Processes[[int]$Matches[1]] }
    if ($Filter -match '^ParentProcessId = (\d+)$') {
        $parentId = [int]$Matches[1]
        return @($script:Processes.Values | Where-Object ParentProcessId -eq $parentId)
    }
    throw "未预期的进程查询: $Filter"
}
function Get-Process {
    [CmdletBinding()] param([int]$Id)
    if ($script:Processes.ContainsKey($Id)) { return [pscustomobject]@{ Id = $Id; HasExited = $false } }
    return $null
}
function Start-Process {
    [CmdletBinding()] param($FilePath, $ArgumentList, $WorkingDirectory, $RedirectStandardOutput, $RedirectStandardError, [switch]$PassThru, $WindowStyle)
    $script:Starts += [pscustomobject]@{ FilePath = $FilePath; Arguments = $ArgumentList }
    return [pscustomobject]@{ Id = 1000 + $script:Starts.Count }
}
function Stop-Process { throw '测试禁止终止进程' }
function netsh { return @('开始端口 结束端口', ' 50700 50848 ') }
function Get-Command { param([string]$Name) if ($Name -eq 'go') { return [pscustomobject]@{ Source = 'C:\test-go\go.exe' } }; throw "未预期的工具查询: $Name" }
function Wait-ForStartupBanner { param($LogFile, $ServiceName) }
function Reset-TestState {
    $script:ExcludedPortRanges = @(,@(50700, 50848))
    $script:Listeners = @()
    $script:Processes = @{}
    $script:Starts = @()
    $script:NetworkQueryFailed = $false
    $script:ProcessQueryFailed = $false
}
function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}
function New-TestProcess([int]$ProcessId, [string]$Name, [string]$ConfigPath, [int]$ParentProcessId = 0) {
    return [pscustomobject]@{
        ProcessId = $ProcessId; ParentProcessId = $ParentProcessId
        ExecutablePath = Join-Path $GoBinDir "$Name.exe"
        CommandLine = "`"$(Join-Path $GoBinDir "$Name.exe")`" -f `"$ConfigPath`""
    }
}

try {
    foreach ($name in $ServiceCatalogue.Keys) {
        $configDir = Join-Path $GoRoot "$name/etc"
        New-Item -ItemType Directory -Path $configDir -Force | Out-Null
        Set-Content -LiteralPath (Join-Path $configDir "$name.yaml") -Value "ListenOn: 0.0.0.0:$($ServiceCatalogue[$name].Port)`nZoneId: 1"
    }
    New-Item -ItemType Directory -Path $GoBinDir, $DerivedEtcDir -Force | Out-Null
    foreach ($name in $ServiceCatalogue.Keys) { Set-Content -LiteralPath (Join-Path $GoBinDir "$name.exe") -Value '进程桩占位文件' }

    Test-Case '单个保留区间在首次查询及缓存读取时都保持数组形状' {
        Reset-TestState
        $script:ExcludedPortRanges = $null
        Assert-Equal $true (Test-PortExcluded 50700) '首次 netsh 输出可识别单区间'
        Assert-Equal $true (Test-PortExcluded 50848) '缓存单区间仍可识别末端'
        Assert-Equal $false (Test-PortExcluded 50849) '保留区间之外不误判'
    }
    Test-Case '同批 chat/trade 共享保留区间时分配不同端口并写入各自配置' {
        Reset-TestState
        $Services = @('chat', 'trade')
        Invoke-Start -UseExe
        $tracked = Get-Content -LiteralPath $PidFile -Raw | ConvertFrom-Json
        Assert-Equal 50849 $tracked.chat.Port 'chat 使用首个可用端口'
        Assert-Equal 50850 $tracked.trade.Port 'trade 避开 chat 尚未开始监听的已计划端口'
        Assert-Match (Get-Content -LiteralPath (Join-Path $DerivedEtcDir 'trade.yaml') -Raw) 'ListenOn: 0.0.0.0:50850' '派生配置与记录一致'
        Assert-Equal 2 $script:Starts.Count '恰好启动两次进程桩'
    }
    Test-Case '分批启动避开前一批实际监听端口' {
        Reset-TestState
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50849; OwningProcess = 77 })
        Assert-Equal 50850 (Resolve-BindablePort 50800 'trade') '不能把已运行 chat 的监听端口再分配给 trade'
    }
    Test-Case '同批稍后处理的现存 trade 尚未 LISTEN 时,chat 仍避开其实际端口' {
        Reset-TestState
        $configPath = Join-Path $DerivedEtcDir 'trade.yaml'
        Set-Content -LiteralPath $configPath -Value 'trade 仍在初始化,配置不可覆盖'
        $script:Processes[77] = New-TestProcess 77 'trade' $configPath
        $pids = [pscustomobject]@{ trade = [pscustomobject]@{ Pid = 77; Port = 50849; Service = 'trade' } }
        Write-PidFile $pids
        $Services = @('chat', 'trade')
        Invoke-Start -UseExe
        $tracked = Read-PidFile
        Assert-Equal 50850 $tracked.chat.Port '先启动的 chat 不能占用后续现存 trade 的端口'
        Assert-Equal 50849 $tracked.trade.Port 'trade 保留原有挪移端口'
        Assert-Equal 1 $script:Starts.Count '只启动新 chat'
        Assert-Match (Get-Content -LiteralPath $configPath -Raw) 'trade 仍在初始化,配置不可覆盖' '复用未就绪实例也不改写配置'
    }
    Test-Case '未请求的现存实例尚未 LISTEN 时,分批启动也预留其端口' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'trade' (Join-Path $DerivedEtcDir 'trade.yaml')
        Write-PidFile ([pscustomobject]@{ trade = [pscustomobject]@{ Pid = 77; Port = 50849; Service = 'trade' } })
        $Services = @('chat')
        Invoke-Start -UseExe
        Assert-Equal 50850 (Read-PidFile).chat.Port '不在请求列表里的初始化实例也不可被抢占端口'
    }
    Test-Case '预留其他 zone 的实例时按实例键确认配置身份' {
        Reset-TestState
        $Zone = 2
        $script:Processes[77] = New-TestProcess 77 'trade' 'etc/trade.yaml'
        $script:Processes[88] = New-TestProcess 88 'chat' (Join-Path $DerivedEtcDir 'z1_chat.yaml')
        $pids = [pscustomobject]@{
            trade = [pscustomobject]@{ Pid = 77; Port = 50850; Service = 'trade' }
            z1_chat = [pscustomobject]@{ Pid = 88; Port = 50849; Service = 'chat' }
        }
        $reserved = @{}
        Reserve-RunningInstancePorts -Pids $pids -ReservedPorts $reserved
        Assert-Equal 'trade' $reserved[50850] '当前请求 zone 不应使默认区的原始配置被误拒'
        Assert-Equal 'z1_chat' $reserved[50849] '其他区的派生配置按实例键核对'
    }
    Test-Case '保留区间出口避开尚未启动实例的原始端口' {
        Reset-TestState
        $reserved = @{ 50849 = 'later' }
        Assert-Equal 50850 (Resolve-BindablePort 50700 'chat' $reserved) '后续实例的原始端口已预留'
    }
    Test-Case '原始端口遭无关进程占用时报错且保留 PID 诊断' {
        Reset-TestState
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50600; OwningProcess = 987 })
        $message = Get-ThrownMessage { Resolve-BindablePort 50600 'router' }
        Assert-Match $message '原始端口 50600 已占用.*PID=987' '显式端口冲突不可静默换端口'
        Assert-Equal 0 $script:Starts.Count '诊断过程不启动任何进程'
    }
    Test-Case '网络或进程查询失败时中止,不能当作空闲端口或消失实例' {
        Reset-TestState
        $script:NetworkQueryFailed = $true
        Assert-Match (Get-ThrownMessage { Resolve-BindablePort 50600 'router' }) '网络查询失败桩' '查询失败不可变成空闲端口'
        $script:ProcessQueryFailed = $true
        $pids = [pscustomobject]@{ chat = 77 }
        Assert-Match (Get-ThrownMessage { Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids $pids -UseExe }) '进程查询失败桩' 'CIM 失败不可吞掉并启动重复实例'
        Assert-Equal 0 $script:Starts.Count '失败没有启动副作用'
    }
    Test-Case '搜索到端口上界仍没有候选时报错' {
        Reset-TestState
        $script:ExcludedPortRanges = @(,@(65534, 65535))
        Assert-Match (Get-ThrownMessage { Resolve-BindablePort 65534 'chat' }) '没有可用候选' '不可返回越界或仍被保留的端口'
    }
    Test-Case '已运行实例先核对身份再复用,派生配置逐字保持' {
        Reset-TestState
        $configPath = Join-Path $DerivedEtcDir 'chat.yaml'
        Set-Content -LiteralPath $configPath -Value '运行中的专属配置,不能覆盖'
        $before = [System.IO.File]::ReadAllBytes($configPath)
        $script:Processes[77] = New-TestProcess 77 'chat' $configPath
        $pids = [pscustomobject]@{ chat = [pscustomobject]@{ Pid = 77; Port = 50849; Service = 'chat' } }
        $reserved = @{}
        Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids $pids -UseExe -ReservedPorts $reserved
        Assert-Equal ([Convert]::ToBase64String($before)) ([Convert]::ToBase64String([System.IO.File]::ReadAllBytes($configPath))) '重复启动不能改写运行配置'
        Assert-Equal 0 $script:Starts.Count '正确实例被复用'
        Assert-Equal 'chat' $reserved[50849] '复用端口加入本批预留'
    }
    Test-Case '复用同一可执行文件也必须匹配配置实例' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'chat' (Join-Path $DerivedEtcDir 'z2_chat.yaml')
        $pids = [pscustomobject]@{ chat = [pscustomobject]@{ Pid = 77; Port = 50849; Service = 'chat' } }
        Assert-Match (Get-ThrownMessage { Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids $pids -UseExe }) '配置不匹配' '别的区服实例不能被误认'
        Assert-Equal 0 $script:Starts.Count '身份冲突不做进程操作'
    }
    Test-Case 'PID 被另一个可执行文件复用时拒绝跳过启动' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'trade' (Join-Path $DerivedEtcDir 'chat.yaml')
        $pids = [pscustomobject]@{ chat = 77 }
        Assert-Match (Get-ThrownMessage { Start-ServiceInstance -Name chat -Info $ServiceCatalogue.chat -Index 1 -Total 1 -Pids $pids -UseExe }) '可执行文件或配置不匹配' '旧格式 PID 也必须验证身份'
    }
    Test-Case 'go run 复用必须匹配本仓库的绝对源码路径' {
        Reset-TestState
        $process = [pscustomobject]@{ ExecutablePath = 'C:\test-go\go.exe'; CommandLine = "go.exe run `"$(Join-Path $GoRoot 'chat/chat.go')`" -f etc/chat.yaml" }
        Assert-Equal $true (Test-InstanceProcessIdentity -Name chat -Info $ServiceCatalogue.chat -InstanceKey chat -Total 1 -Process $process) '绝对源码路径与配置共同确认身份'
        $process.CommandLine = 'go.exe run chat.go -f etc/chat.yaml'
        Assert-Equal $false (Test-InstanceProcessIdentity -Name chat -Info $ServiceCatalogue.chat -InstanceKey chat -Total 1 -Process $process) '相对路径不能证明属于本仓库'
    }
    Test-Case '无关 PID 监听不能让目标进程被判就绪' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'trade' 'etc/trade.yaml'
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50849; OwningProcess = 88 })
        Assert-Equal $false (Wait-TcpListenReady -Port 50849 -ProcessId 77 -TimeoutSeconds 0) 'chat 的端口不能充当 trade 的就绪证据'
    }
    Test-Case '目标 PID 实际监听才判就绪' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'trade' 'etc/trade.yaml'
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50850; OwningProcess = 77 })
        Assert-Equal $true (Wait-TcpListenReady -Port 50850 -ProcessId 77 -TimeoutSeconds 0) '监听归属正确'
    }
    Test-Case 'go run 允许自己的子进程监听,直接 exe 不接受子进程代听' {
        Reset-TestState
        $script:Processes[77] = New-TestProcess 77 'go' 'etc/trade.yaml'
        $script:Processes[88] = New-TestProcess 88 'trade' 'etc/trade.yaml' 77
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50850; OwningProcess = 88 })
        Assert-Equal $true (Wait-TcpListenReady -Port 50850 -ProcessId 77 -TimeoutSeconds 0 -AllowChildProcess) 'go run 包装进程的实际服务子进程可就绪'
        Assert-Equal $false (Wait-TcpListenReady -Port 50850 -ProcessId 77 -TimeoutSeconds 0) 'exe 模式严格要求目标 PID'
    }
    Test-Case '目标已经退出时不能被残留监听判就绪' {
        Reset-TestState
        $script:Listeners = @([pscustomobject]@{ LocalPort = 50850; OwningProcess = 77 })
        Assert-Equal $false (Wait-TcpListenReady -Port 50850 -ProcessId 77 -TimeoutSeconds 0) '先核对目标存活'
    }
}
finally {
    $resolvedRoot = [System.IO.Path]::GetFullPath($testRoot)
    $tempParent = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath()).TrimEnd('\', '/') + [System.IO.Path]::DirectorySeparatorChar
    if (-not $resolvedRoot.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolvedRoot) -notlike 'mmorpg-go-ports-tests-*') {
        throw "拒绝清理测试临时目录范围之外的路径: $resolvedRoot"
    }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'go_services 端口与实例归属')
