#requires -Version 7
<#
.SYNOPSIS
    极简 pwsh 测试骨架(不依赖 Pester —— 运维机器上装不了模块)。

.DESCRIPTION
    用法:
        . "$PSScriptRoot/lib/test_harness.ps1"
        Test-Case "描述" { Assert-Equal -Expected 1 -Actual 1 -Because "..." }
        exit (Complete-TestRun)

    断言失败抛异常,Test-Case 捕获后记一条 FAIL 继续跑下一条 —— 一次跑完
    能看到全部失败项,而不是修一条跑一次。
#>

Set-StrictMode -Off

$script:TestResults = New-Object System.Collections.Generic.List[object]

function Test-Case {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][scriptblock]$Body
    )

    try {
        & $Body
        $script:TestResults.Add([pscustomobject]@{ Name = $Name; Ok = $true; Error = "" })
        Write-Host "  [PASS] $Name" -ForegroundColor Green
    }
    catch {
        $script:TestResults.Add([pscustomobject]@{ Name = $Name; Ok = $false; Error = $_.Exception.Message })
        Write-Host "  [FAIL] $Name" -ForegroundColor Red
        Write-Host "         $($_.Exception.Message)" -ForegroundColor Red
    }
}

function Assert-True {
    param(
        [Parameter(Mandatory = $true)][bool]$Condition,
        [Parameter(Mandatory = $true)][string]$Because
    )
    if (-not $Condition) { throw "断言失败: $Because" }
}

function Assert-Equal {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()]$Expected,
        [Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()]$Actual,
        [Parameter(Mandatory = $true)][string]$Because
    )
    $e = if ($null -eq $Expected) { '<null>' } else { [string]$Expected }
    $a = if ($null -eq $Actual) { '<null>' } else { [string]$Actual }
    if ($e -ne $a) { throw "断言失败: $Because (期望 '$e',实际 '$a')" }
}

function Assert-Match {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Pattern,
        [Parameter(Mandatory = $true)][string]$Because
    )
    if ($null -eq $Text -or $Text -notmatch $Pattern) {
        $preview = if ($null -eq $Text) { '<null>' } else { $Text.Substring(0, [Math]::Min(600, $Text.Length)) }
        throw "断言失败: $Because (未匹配 /$Pattern/。文本前 600 字符: $preview)"
    }
}

function Assert-NotMatch {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][AllowNull()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Pattern,
        [Parameter(Mandatory = $true)][string]$Because
    )
    if ($null -ne $Text -and $Text -match $Pattern) {
        throw "断言失败: $Because (不该匹配 /$Pattern/,但匹配到了 '$($Matches[0])')"
    }
}

<#
.SYNOPSIS
    汇总并返回退出码(0 = 全绿)。
#>
function Complete-TestRun {
    param([string]$SuiteName = "")

    $failed = @($script:TestResults | Where-Object { -not $_.Ok })
    Write-Host ""
    Write-Host "------------------------------------------------------------"
    if ($SuiteName) { Write-Host " suite: $SuiteName" }
    Write-Host (" total={0} pass={1} fail={2}" -f $script:TestResults.Count, ($script:TestResults.Count - $failed.Count), $failed.Count)
    Write-Host "------------------------------------------------------------"
    if ($failed.Count -gt 0) {
        foreach ($f in $failed) { Write-Host "  FAILED: $($f.Name)" -ForegroundColor Red }
        return 1
    }
    return 0
}
