#requires -Version 7
<#
.SYNOPSIS
    Go 本机启动器 stop 命令的优雅停回归:先投递 Ctrl+C、按服务时限等待、超时强杀进程树。
.DESCRIPTION
    只加载 go_services.ps1 的 AST 中的函数,服务目录取脚本里的真实 $ServiceCatalogue(纯字面量)。
    投递 Ctrl+C、等待退出、强杀进程树、单调时钟、进程查询全部换成桩;Stop-Process 一调用就抛错。
    PID 记录用测试临时目录里的真实 JSON 文件,不读写真实 run/pids,不启动或停止任何真实进程。
.EXAMPLE
    pwsh -File tools/scripts/tests/go_services_stop.tests.ps1
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

# 真实服务目录:时限用例要钉住脚本里 db = 35、其它服务缺省 25,不能用测试自造的目录。
# 目录是纯字面量哈希表,只对它的右值求值,不执行脚本其它部分。
$catalogueAssignment = $ast.Find({
        param($node)
        $node -is [System.Management.Automation.Language.AssignmentStatementAst] -and
        $node.Left.Extent.Text -eq '$ServiceCatalogue'
    }, $false)
if (-not $catalogueAssignment) { throw '在 go_services.ps1 中找不到 $ServiceCatalogue 赋值' }
$ServiceCatalogue = & ([scriptblock]::Create($catalogueAssignment.Right.Extent.Text))

$testRoot = Join-Path ([System.IO.Path]::GetTempPath()) ('mmorpg-go-stop-tests-' + [guid]::NewGuid().ToString('N'))
$PidFile = Join-Path $testRoot 'run/pids/go_services.pid.json'
$LegacyPidFile = Join-Path $testRoot 'absent-legacy-pids.json'
$Services = @()
$ForceStop = $false
$StopTimeoutSeconds = 0

# ── 桩 ────────────────────────────────────────────────────────────────
# $script:Processes:PID → 'running' | 'exited';不在表里 = 查不到。
function Get-Process {
    [CmdletBinding()] param([int]$Id)
    if (-not $script:Processes.ContainsKey($Id)) { throw "进程桩: PID $Id 不存在" }
    return [pscustomobject]@{ Id = $Id; HasExited = ($script:Processes[$Id] -eq 'exited') }
}
function Send-ConsoleCtrlC {
    param([int]$ProcessId)
    $script:Calls.Add("send:$ProcessId")
    if ($script:SendFails -contains $ProcessId) { return $false }
    return $true
}
function Wait-ProcessExit {
    param([int]$ProcessId, [double]$TimeoutSeconds)
    $script:Calls.Add("wait:$ProcessId")
    $script:WaitTimeouts[$ProcessId] = $TimeoutSeconds
    return -not ($script:NeverExits -contains $ProcessId)
}
function Stop-ProcessTree {
    param([int]$ProcessId)
    $script:Calls.Add("kill:$ProcessId")
}
# 依次返回 $script:Clock 里的值,用完后一直停在最后一个值上。
function Get-StopClockSeconds {
    $index = [Math]::Min($script:ClockIndex, $script:Clock.Count - 1)
    $script:ClockIndex++
    return [double]$script:Clock[$index]
}
function Stop-Process { throw '测试禁止 Stop-Process:优雅停只能经 Stop-ProcessTree 强杀' }

function Reset-TestState {
    $script:Processes = @{}
    $script:Calls = [System.Collections.Generic.List[string]]::new()
    $script:WaitTimeouts = @{}
    $script:SendFails = @()
    $script:NeverExits = @()
    $script:Clock = @(0.0)
    $script:ClockIndex = 0
    if (Test-Path -LiteralPath $PidFile) { Remove-Item -LiteralPath $PidFile -Force }
}
function Set-TrackedInstances([hashtable]$Entries) {
    $map = [pscustomobject]@{}
    foreach ($key in ($Entries.Keys | Sort-Object)) {
        $value = $Entries[$key]
        $map | Add-Member -NotePropertyName $key -NotePropertyValue ([pscustomobject]@{ Pid = $value.Pid; Port = $value.Port; Service = $value.Service })
    }
    Write-PidFile $map
}
function Invoke-StopCaptured {
    return (Invoke-Stop 6>&1 | Out-String)
}
function Get-TrackedKeys {
    # 逐个取属性名:对空对象直接 .PSObject.Properties.Name 会得到 $null,包成数组后 Count 是 1。
    $tracked = Get-Content -LiteralPath $PidFile -Raw | ConvertFrom-Json
    return @($tracked.PSObject.Properties | ForEach-Object { $_.Name })
}

try {
    New-Item -ItemType Directory -Path $testRoot -Force | Out-Null

    Test-Case '私有配置路径含空格、引号或尾反斜线时保持单个启动参数' {
        Assert-Equal '"C:\private boot\config.yaml"' (ConvertTo-ServiceProcessArgument 'C:\private boot\config.yaml') '路径不能拆成两个argv'
        Assert-Equal '"C:\private boot\\"' (ConvertTo-ServiceProcessArgument 'C:\private boot\') '结尾反斜线不能吞掉闭合引号'
        Assert-Equal '"a\"b"' (ConvertTo-ServiceProcessArgument 'a"b') '字面引号必须留在同一个argv'
        Assert-Equal '""' (ConvertTo-ServiceProcessArgument '') '空参数不丢失'
    }

    Test-Case '优雅停:Ctrl+C 后在时限内退出,不强杀,移除 PID 记录' {
        Reset-TestState
        $script:Processes[101] = 'running'
        Set-TrackedInstances @{ chat = @{ Pid = 101; Port = 50700; Service = 'chat' } }
        $out = Invoke-StopCaptured
        Assert-Equal 'send:101,wait:101' ($script:Calls -join ',') '先投递 Ctrl+C 再等待,不出现 kill'
        Assert-Match $out '\[stop\]\s+chat \(PID 101, exited in [0-9.]+s\)' '输出 [stop] 与退出耗时'
        Assert-NotMatch $out '\[kill\]' '优雅退出不输出 [kill]'
        Assert-Equal 0 (Get-TrackedKeys).Count 'PID 记录被移除'
    }

    Test-Case '超时:Ctrl+C 已投递但不退出时强杀进程树一次' {
        Reset-TestState
        $script:Processes[101] = 'running'
        $script:NeverExits = @(101)
        Set-TrackedInstances @{ chat = @{ Pid = 101; Port = 50700; Service = 'chat' } }
        $out = Invoke-StopCaptured
        Assert-Equal 'send:101,wait:101,kill:101' ($script:Calls -join ',') '等待超时后强杀恰好一次'
        Assert-Match $out '\[kill\]\s+chat \(PID 101\): no exit within 25s after Ctrl\+C; force-killed process tree' '超时文案带时限'
        Assert-Equal 0 (Get-TrackedKeys).Count '强杀后同样移除 PID 记录'
    }

    Test-Case 'Ctrl+C 投递不出去时立即强杀,不等待' {
        Reset-TestState
        $script:Processes[101] = 'running'
        $script:SendFails = @(101)
        Set-TrackedInstances @{ chat = @{ Pid = 101; Port = 50700; Service = 'chat' } }
        $out = Invoke-StopCaptured
        Assert-Equal 'send:101,kill:101' ($script:Calls -join ',') '投递失败不调用 Wait-ProcessExit'
        Assert-Match $out '\[kill\]\s+chat \(PID 101\): console interrupt not deliverable; force-killed process tree' '投递失败文案'
    }

    Test-Case '-ForceStop 跳过 Ctrl+C 直接强杀进程树' {
        Reset-TestState
        $ForceStop = $true
        $script:Processes[101] = 'running'
        Set-TrackedInstances @{ db = @{ Pid = 101; Port = 6000; Service = 'db' } }
        $out = Invoke-StopCaptured
        Assert-Equal 'kill:101' ($script:Calls -join ',') '-ForceStop 不投递 Ctrl+C、不等待'
        Assert-Match $out '\[kill\]\s+db \(PID 101\): -ForceStop' '-ForceStop 输出 [kill]'
        Assert-Equal 0 (Get-TrackedKeys).Count 'PID 记录被移除'
    }

    Test-Case '等待时限:db 用 35,chat 用 25;-StopTimeoutSeconds 10 时全部用 10' {
        Reset-TestState
        Assert-Equal 35 (Get-GracefulStopSeconds -Info $ServiceCatalogue['db']) '真实目录里 db 的时限'
        Assert-Equal 25 (Get-GracefulStopSeconds -Info $ServiceCatalogue['chat']) '未写时限的服务取默认值'
        Assert-True ($ServiceCatalogue.Contains('mail')) '真实目录里有 mail 条目(邮件 M1d 启用)'
        Assert-Equal 25 (Get-GracefulStopSeconds -Info $ServiceCatalogue['mail']) 'mail 不写时限,默认 25s 盖住它的 24s 硬截止'
        Assert-True ($ServiceCatalogue.Contains('rank')) '真实目录里有 rank 条目(排行榜 R2b 启用)'
        Assert-Equal 25 (Get-GracefulStopSeconds -Info $ServiceCatalogue['rank']) 'rank 不写时限,默认 25s 盖住它的 24s 硬截止'
        Assert-Equal 25 (Get-GracefulStopSeconds -Info $null) '目录里找不到服务时取默认值'
        $script:Processes[101] = 'running'
        $script:Processes[102] = 'running'
        Set-TrackedInstances @{
            db   = @{ Pid = 101; Port = 6000; Service = 'db' }
            chat = @{ Pid = 102; Port = 50700; Service = 'chat' }
        }
        Invoke-StopCaptured | Out-Null
        Assert-Equal 35 $script:WaitTimeouts[101] 'db 的等待参数'
        Assert-Equal 25 $script:WaitTimeouts[102] 'chat 的等待参数'

        Reset-TestState
        $StopTimeoutSeconds = 10
        Assert-Equal 10 (Get-GracefulStopSeconds -Info $ServiceCatalogue['db']) '-StopTimeoutSeconds 覆盖 db'
        Assert-Equal 10 (Get-GracefulStopSeconds -Info $ServiceCatalogue['chat']) '-StopTimeoutSeconds 覆盖 chat'
        $script:Processes[101] = 'running'
        $script:Processes[102] = 'running'
        Set-TrackedInstances @{
            db   = @{ Pid = 101; Port = 6000; Service = 'db' }
            chat = @{ Pid = 102; Port = 50700; Service = 'chat' }
        }
        Invoke-StopCaptured | Out-Null
        Assert-Equal 10 $script:WaitTimeouts[101] '覆盖后 db 等待 10'
        Assert-Equal 10 $script:WaitTimeouts[102] '覆盖后 chat 等待 10'
    }

    Test-Case '旧格式 PID 记录(只有 PID)按实例键推出服务,多 zone 的 db 仍用 35' {
        Reset-TestState
        $script:Processes[101] = 'running'
        Write-PidFile ([pscustomobject]@{ z2_db_1 = 101 })
        Invoke-StopCaptured | Out-Null
        Assert-Equal 35 $script:WaitTimeouts[101] 'z2_db_1 去掉前后缀后是 db'
    }

    Test-Case '并行:先对全部实例投递 Ctrl+C,再逐个等待' {
        Reset-TestState
        $script:Processes[101] = 'running'
        $script:Processes[102] = 'running'
        Write-PidFile ([pscustomobject]@{
                chat_1 = [pscustomobject]@{ Pid = 101; Port = 50700; Service = 'chat' }
                chat_2 = [pscustomobject]@{ Pid = 102; Port = 50701; Service = 'chat' }
            })
        Invoke-StopCaptured | Out-Null
        Assert-Equal 'send:101,send:102,wait:101,wait:102' ($script:Calls -join ',') '调用顺序为 Send A、Send B、Wait A、Wait B'
    }

    Test-Case '剩余时间扣减:后一个实例只等截止时间的剩余量' {
        Reset-TestState
        # 时钟依次:投递 A=0、投递 B=0、A 剩余=0、A 退出后=12,之后停在 12。
        $script:Clock = @(0.0, 0.0, 0.0, 12.0)
        $script:Processes[101] = 'running'
        $script:Processes[102] = 'running'
        Write-PidFile ([pscustomobject]@{
                chat_1 = [pscustomobject]@{ Pid = 101; Port = 50700; Service = 'chat' }
                chat_2 = [pscustomobject]@{ Pid = 102; Port = 50701; Service = 'chat' }
            })
        $out = Invoke-StopCaptured
        Assert-Equal 25 $script:WaitTimeouts[101] 'A 等满 25'
        Assert-Equal 13 $script:WaitTimeouts[102] 'B 只等 25 − 12'
        Assert-Match $out '\[stop\]\s+chat_1 \(PID 101, exited in 12s\)' 'A 的退出耗时按投递时刻算'
    }

    Test-Case '剩余时间耗尽时以 0 等待,超时照常强杀' {
        Reset-TestState
        $script:Clock = @(0.0, 0.0, 0.0, 40.0)
        $script:Processes[101] = 'running'
        $script:Processes[102] = 'running'
        $script:NeverExits = @(102)
        Write-PidFile ([pscustomobject]@{
                chat_1 = [pscustomobject]@{ Pid = 101; Port = 50700; Service = 'chat' }
                chat_2 = [pscustomobject]@{ Pid = 102; Port = 50701; Service = 'chat' }
            })
        Invoke-StopCaptured | Out-Null
        Assert-Equal 0 $script:WaitTimeouts[102] '不传负的等待时长'
        Assert-Equal 'send:101,send:102,wait:101,wait:102,kill:102' ($script:Calls -join ',') '耗尽后不再等、直接强杀'
    }

    Test-Case '已退出或查不到的实例输出 [gone],不投递 Ctrl+C' {
        Reset-TestState
        $script:Processes[101] = 'exited'
        Write-PidFile ([pscustomobject]@{
                chat  = [pscustomobject]@{ Pid = 101; Port = 50700; Service = 'chat' }
                trade = [pscustomobject]@{ Pid = 909; Port = 50800; Service = 'trade' }
            })
        $out = Invoke-StopCaptured
        Assert-Equal 0 $script:Calls.Count '不投递、不等待、不强杀'
        Assert-Match $out '\[gone\]\s+chat \(PID 101 already exited\)' '已退出'
        Assert-Match $out '\[gone\]\s+trade \(PID 909 not found\)' '查不到'
        Assert-Equal 0 (Get-TrackedKeys).Count '两条记录都被移除'
    }

    Test-Case '-Services 只停指定服务,其余 PID 记录保留' {
        Reset-TestState
        $Services = @('db')
        $script:Processes[101] = 'running'
        $script:Processes[102] = 'running'
        Set-TrackedInstances @{
            db   = @{ Pid = 101; Port = 6000; Service = 'db' }
            chat = @{ Pid = 102; Port = 50700; Service = 'chat' }
        }
        Invoke-StopCaptured | Out-Null
        Assert-Equal 'send:101,wait:101' ($script:Calls -join ',') '只处理 db'
        Assert-Equal 'chat' ((Get-TrackedKeys) -join ',') 'chat 的记录不动'
    }

    Test-Case '辅助脚本文本:附着目标控制台、忽略自身 Ctrl+C、向进程组 0 投递,且能被解析' {
        $text = Get-ConsoleCtrlCHelperScript -ProcessId 1234
        Assert-Match $text ([regex]::Escape('FreeConsole()')) '先脱离辅助进程自己的控制台'
        Assert-Match $text ([regex]::Escape('AttachConsole(1234)')) 'PID 以整数嵌入'
        Assert-Match $text 'SetConsoleCtrlHandler\(\[IntPtr\]::Zero, \$true\)' '辅助进程自己忽略 Ctrl+C'
        Assert-Match $text ([regex]::Escape('GenerateConsoleCtrlEvent(0, 0)')) 'CTRL_C_EVENT 投给进程组 0'
        Assert-Match $text.TrimEnd() 'exit 0$' '成功路径以 exit 0 结束'
        $helperErrors = $null
        $null = [System.Management.Automation.Language.Parser]::ParseInput($text, [ref]$null, [ref]$helperErrors)
        Assert-Equal 0 $helperErrors.Count '生成的辅助脚本没有语法错误'
    }

    Test-Case '契约:Start-ServiceInstance 的 Start-Process 都是 -WindowStyle Hidden 且不带 -NoNewWindow' {
        $startFn = $ast.Find({
                param($node)
                $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Start-ServiceInstance'
            }, $false)
        Assert-True ($null -ne $startFn) '找到 Start-ServiceInstance'
        $calls = @($startFn.FindAll({
                    param($node)
                    $node -is [System.Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Start-Process'
                }, $true))
        Assert-Equal 2 $calls.Count 'exe 与 go run 两处启动'
        foreach ($call in $calls) {
            $elements = @($call.CommandElements)
            $windowStyle = $null
            for ($i = 0; $i -lt $elements.Count; $i++) {
                $element = $elements[$i]
                if ($element -isnot [System.Management.Automation.Language.CommandParameterAst]) { continue }
                Assert-True ($element.ParameterName -ne 'NoNewWindow') "共用控制台会让 Ctrl+C 波及其它服务: $($call.Extent.Text)"
                if ($element.ParameterName -eq 'WindowStyle') {
                    $windowStyle = if ($element.Argument) { $element.Argument.Extent.Text } elseif ($i + 1 -lt $elements.Count) { $elements[$i + 1].Extent.Text } else { $null }
                }
            }
            Assert-Equal 'Hidden' $windowStyle "每个实例必须有自己的隐藏控制台: $($call.Extent.Text)"
        }
    }
}
finally {
    $resolvedRoot = [System.IO.Path]::GetFullPath($testRoot)
    $tempParent = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath()).TrimEnd('\', '/') + [System.IO.Path]::DirectorySeparatorChar
    if (-not $resolvedRoot.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolvedRoot) -notlike 'mmorpg-go-stop-tests-*') {
        throw "拒绝清理测试临时目录范围之外的路径: $resolvedRoot"
    }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
Test-Case '严格StopAndReport拒ForceStop与缺原件，不复用普通强杀成功结论' {
    $savedForce=$ForceStop;$savedPlan=$MaintenancePlanPath;$savedReport=$StopReportPath
    try {
        $ForceStop=$true;$MaintenancePlanPath='fixture-plan';$StopReportPath='fixture-output'
        $message='';try{Invoke-StopAndReport}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '禁止ForceStop' -Because '不让普通stop强杀签成永久排空'
        $ForceStop=$false;$MaintenancePlanPath='';$StopReportPath=''
        $message='';try{Invoke-StopAndReport}catch{$message=$_.Exception.Message}
        Assert-Match -Text $message -Pattern '固定维护计划' -Because '无固定issuer/应用frontier不能根据PID消失声称成功'
    } finally {$ForceStop=$savedForce;$MaintenancePlanPath=$savedPlan;$StopReportPath=$savedReport}
}

exit (Complete-TestRun -SuiteName 'go_services 优雅停')
