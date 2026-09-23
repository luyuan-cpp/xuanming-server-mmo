#requires -Version 7
<#
.SYNOPSIS
    一键启动的 Kafka 命令契约复用回归,覆盖好友和帮会异步推送生产者。
.DESCRIPTION
    仅从 AST 加载契约函数,进程全用桩,PID 记录只写临时目录。不启动服务或访问真实运行环境。
.EXAMPLE
    pwsh -File tools/scripts/tests/start_game_command_contract.tests.ps1
#>
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot '../start_game.ps1'), [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
$functionNames = @('Test-UsesKafkaCommandContract','Assert-RunningCommandContract','Save-LocalCommandContract')
foreach ($definition in $ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -in $functionNames }, $false)) {
    . ([scriptblock]::Create($definition.Extent.Text))
}
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('mmorpg-command-contract-tests-' + [guid]::NewGuid().ToString('N'))
$serverRoot = $testRoot
$recordPath = Join-Path $serverRoot 'run/pids/kafka_command_contract.json'
$kafkaContract = @{ Generation = 2; Partitions = 256 }
$script:ProcessStart = [datetime]::SpecifyKind([datetime]'2026-09-22T12:00:00', [DateTimeKind]::Utc)
$script:ProcessQueries = 0
function Get-Process {
    [CmdletBinding()] param([int]$Id)
    $script:ProcessQueries++
    return [pscustomobject]@{ Id = $Id; StartTime = $script:ProcessStart }
}
function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}
function Write-TestRecords($Records) { $Records | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $recordPath -Encoding utf8 }
try {
    New-Item -ItemType Directory -Path (Split-Path -Parent $recordPath) -Force | Out-Null
    foreach ($serviceName in @('friend','guild')) {
        Test-Case "$serviceName 缺少可信启动记录时拒绝复用" {
            Write-TestRecords @{}
            $process = [pscustomobject]@{ Name = "$serviceName.exe"; ProcessId = 77 }
            Assert-Match (Get-ThrownMessage { Assert-RunningCommandContract $process }) '不能确认它使用本次 Kafka 契约' '不能只凭进程存活就给异步推送生产者盖当前代契约'
        }
        Test-Case "$serviceName 保存契约后可按 PID/启动时间/generation/分区复用" {
            Write-TestRecords @{}
            Save-LocalCommandContract $serviceName 77
            $saved = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json -AsHashtable
            Assert-Equal 2 $saved[$serviceName].Generation '保存当前命令主题代数'
            Assert-Equal 256 $saved[$serviceName].Partitions '保存当前分区数'
            Assert-Equal '' (Get-ThrownMessage { Assert-RunningCommandContract ([pscustomobject]@{ Name = "$serviceName.exe"; ProcessId = 77 }) }) '可信的同一进程可复用'
        }
        foreach ($field in @('Generation','Partitions','ProcessId','StartTimeUtcTicks')) {
            Test-Case "$serviceName 拒绝不匹配的 $field" {
                Save-LocalCommandContract $serviceName 77
                $saved = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json -AsHashtable
                $saved[$serviceName][$field] = if ($field -eq 'StartTimeUtcTicks') { '0' } else { 1 }
                Write-TestRecords $saved
                Assert-Match (Get-ThrownMessage { Assert-RunningCommandContract ([pscustomobject]@{ Name = "$serviceName.exe"; ProcessId = 77 }) }) '不能确认它使用本次 Kafka 契约' '旧代、旧分区及复用PID不能通过检查'
            }
        }
    }
    Test-Case '现有控制面消费者和生产者继续要求契约' {
        foreach ($serviceName in @('gate','scene','battle','login','scene_manager','player_locator','match')) {
            Write-TestRecords @{}
            Assert-Match (Get-ThrownMessage { Assert-RunningCommandContract ([pscustomobject]@{ Name = "$serviceName.exe"; ProcessId = 77 }) }) '不能确认它使用本次 Kafka 契约' "$serviceName 既有保护仍生效"
        }
    }
    Test-Case '没有命令主题生产链的服务不检查或写契约' {
        Write-TestRecords @{}
        $script:ProcessQueries = 0
        foreach ($serviceName in @('db','data_service','client_rpc_router','chat','trade')) {
            Assert-RunningCommandContract ([pscustomobject]@{ Name = "$serviceName.exe"; ProcessId = 77 })
            Save-LocalCommandContract $serviceName 77
        }
        Assert-Equal 0 $script:ProcessQueries '无命令生产链的服务不依赖进程契约记录'
        $saved = Get-Content -LiteralPath $recordPath -Raw | ConvertFrom-Json -AsHashtable
        Assert-Equal 0 $saved.Count '不为非命令生产者创建无意义的契约记录'
    }
}
finally {
    $resolvedRoot = [IO.Path]::GetFullPath($testRoot)
    $tempParent = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar
    if (-not $resolvedRoot.StartsWith($tempParent, [StringComparison]::OrdinalIgnoreCase) -or (Split-Path -Leaf $resolvedRoot) -notlike 'mmorpg-command-contract-tests-*') { throw "拒绝清理测试目录以外的路径: $resolvedRoot" }
    if (Test-Path -LiteralPath $resolvedRoot) { Remove-Item -LiteralPath $resolvedRoot -Recurse -Force }
}
exit (Complete-TestRun -SuiteName 'start_game Kafka 命令契约')
