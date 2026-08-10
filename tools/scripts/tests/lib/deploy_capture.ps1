#requires -Version 7
<#
.SYNOPSIS
    契约测试用的采集工具:跑 k8s_deploy.ps1 -DryRun,把它打印的 manifest 抓回来解析。

.DESCRIPTION
    k8s_deploy.ps1 的产物不入库(每次 apply 现生成),所以契约测试只能通过
    -DryRun 这一条缝去看它到底生成了什么。Invoke-KubectlWithInputFile 在
    DryRun 下会把完整 YAML 夹在 `--- BEGIN MANIFEST ---` / `--- END MANIFEST ---`
    之间打出来,这里就靠这对标记切块。
#>

Set-StrictMode -Off

# $PSScriptRoot 在被 dot-source 的文件里指向**它自己**所在目录(tools/scripts/tests/lib),
# 所以往上两级才是 tools/scripts。
$script:ToolsScriptsDir = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$script:RepoRootPath = (Resolve-Path (Join-Path $script:ToolsScriptsDir "..\..")).Path

. (Join-Path $script:ToolsScriptsDir "lib\release_common.ps1")

function Get-RepoRoot { return $script:RepoRootPath }
function Get-ToolsScriptsDir { return $script:ToolsScriptsDir }

<#
.SYNOPSIS
    以子进程跑 k8s_deploy.ps1,返回 @{ ExitCode; Output }。

.PARAMETER Env
    要在子进程里设置的环境变量(hashtable)。跑完自动还原,避免污染同一轮里的其它用例。
#>
function Invoke-DeployDryRun {
    param(
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [hashtable]$Env = @{}
    )

    $script = Join-Path $script:ToolsScriptsDir "k8s_deploy.ps1"
    $saved = @{}
    foreach ($k in $Env.Keys) {
        $saved[$k] = [System.Environment]::GetEnvironmentVariable($k)
        [System.Environment]::SetEnvironmentVariable($k, $Env[$k])
    }
    try {
        $output = & pwsh -NoProfile -File $script @Arguments 2>&1 | Out-String
        return @{ ExitCode = $LASTEXITCODE; Output = $output }
    }
    finally {
        foreach ($k in $Env.Keys) {
            [System.Environment]::SetEnvironmentVariable($k, $saved[$k])
        }
    }
}

<#
.SYNOPSIS
    以子进程跑 tools/scripts 下任意脚本,返回 @{ ExitCode; Output }。
#>
function Invoke-ToolScript {
    param(
        [Parameter(Mandatory = $true)][string]$ScriptName,
        [Parameter(Mandatory = $true)][string[]]$Arguments,
        [hashtable]$Env = @{}
    )

    $script = Join-Path $script:ToolsScriptsDir $ScriptName
    $saved = @{}
    foreach ($k in $Env.Keys) {
        $saved[$k] = [System.Environment]::GetEnvironmentVariable($k)
        [System.Environment]::SetEnvironmentVariable($k, $Env[$k])
    }
    try {
        $output = & pwsh -NoProfile -File $script @Arguments 2>&1 | Out-String
        return @{ ExitCode = $LASTEXITCODE; Output = $output }
    }
    finally {
        foreach ($k in $Env.Keys) {
            [System.Environment]::SetEnvironmentVariable($k, $saved[$k])
        }
    }
}

<#
.SYNOPSIS
    从 DryRun 输出里切出所有 manifest 文本块。
#>
function Get-ManifestBlocks {
    param([Parameter(Mandatory = $true)][string]$Output)

    $blocks = New-Object System.Collections.Generic.List[string]
    $lines = $Output -split "`r?`n"
    $current = $null
    foreach ($line in $lines) {
        if ($line.Trim() -eq '--- BEGIN MANIFEST ---') {
            $current = New-Object System.Collections.Generic.List[string]
            continue
        }
        if ($line.Trim() -eq '--- END MANIFEST ---') {
            if ($null -ne $current) { $blocks.Add(($current -join "`n")) }
            $current = $null
            continue
        }
        if ($null -ne $current) { $current.Add($line) }
    }
    return $blocks
}

<#
.SYNOPSIS
    找出 metadata.name 等于指定值的那个 manifest 块。找不到返回 $null。
#>
function Select-ManifestByName {
    param(
        [Parameter(Mandatory = $true)][string]$Output,
        [Parameter(Mandatory = $true)][string]$Name
    )

    foreach ($block in (Get-ManifestBlocks -Output $Output)) {
        if ($block -match "(?m)^\s*name:\s*$([regex]::Escape($Name))\s*$") {
            return $block
        }
    }
    return $null
}

<#
.SYNOPSIS
    把 manifest 块拍平成 "点分路径 -> 值"。ConfigMap 里内嵌的配置文件
    (`data: <file.yaml>: |`)会以 `data.<file.yaml>.<原路径>` 的形式出现。
#>
function ConvertTo-FlatManifest {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Block)

    # fail-closed:块拿不到就直接报清楚,不能让后续断言在空字符串上"顺利通过"
    if ([string]::IsNullOrWhiteSpace($Block)) {
        throw "manifest 块为空 —— DryRun 输出里没找到目标 ConfigMap/Deployment(视为契约破坏)"
    }
    return (ConvertFrom-YamlToFlatMap -Text $Block)
}

<#
.SYNOPSIS
    从拍平后的 manifest 里取值,取不到抛异常(fail-closed:测试不能因为"没找到"而静默通过)。
#>
function Get-FlatValue {
    param(
        [Parameter(Mandatory = $true)]$Flat,
        [Parameter(Mandatory = $true)][string]$KeyPath
    )

    if (-not $Flat.Scalars.Contains($KeyPath)) {
        throw "生成的 manifest 里查不到 '$KeyPath'(fail-closed:视为契约破坏,不是跳过)"
    }
    return [string]$Flat.Scalars[$KeyPath]
}

<#
.SYNOPSIS
    读服务侧 etc/*.yaml 的权威值,取不到抛异常。
#>
function Get-EtcValue {
    param(
        [Parameter(Mandatory = $true)][string]$RelativePath,
        [Parameter(Mandatory = $true)][string]$KeyPath
    )

    $full = Join-Path $script:RepoRootPath ($RelativePath -replace '/', [System.IO.Path]::DirectorySeparatorChar)
    $r = Get-YamlScalar -Path $full -KeyPath $KeyPath
    if (-not $r.Found) { throw "读不到权威值 ${RelativePath}:${KeyPath} —— $($r.Reason)" }
    return $r.Value
}
