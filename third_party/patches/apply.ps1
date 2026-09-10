<#
.SYNOPSIS
    把 third_party 子模块的本地编译修复补丁应用回去。

.DESCRIPTION
    子模块指向上游仓库,主仓只存 commit 指针,存不下对子模块内容的改动。
    `git submodule update` / 重新 clone / 换机器都会把这些改动冲掉,
    表现是编译错误。本脚本把它们重新贴回去。

    幂等:已经应用过的补丁会被跳过(用 --reverse --check 探测),不会重复贴。
    详见同目录 README.md。

.PARAMETER DryRun
    只报告每个补丁的状态,不改动任何文件。

.EXAMPLE
    pwsh -File third_party/patches/apply.ps1
    pwsh -File third_party/patches/apply.ps1 -DryRun
#>
[CmdletBinding()]
param(
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

# 仓库根 = 本脚本所在目录往上两级(third_party/patches → 仓库根)
$patchDir = $PSScriptRoot
$repoRoot = Split-Path -Parent (Split-Path -Parent $patchDir)

# 补丁文件名约定:<子模块名>-<用途>.patch,子模块目录 = third_party/<子模块名>
$patches = Get-ChildItem -Path $patchDir -Filter '*.patch' | Sort-Object Name

if ($patches.Count -eq 0) {
    Write-Host "没有找到任何 .patch 文件" -ForegroundColor Yellow
    exit 0
}

$applied = 0
$skipped = 0
$failed  = 0

foreach ($patch in $patches) {
    # 取第一个 '-' 之前的部分作为子模块名
    $submodule = ($patch.BaseName -split '-')[0]
    $target = Join-Path $repoRoot "third_party/$submodule"

    if (-not (Test-Path $target)) {
        Write-Host "[跳过] $($patch.Name): 子模块目录不存在 ($target)" -ForegroundColor Yellow
        $skipped++
        continue
    }

    # 子模块工作树为空 = 没检出,先 git submodule update --init
    if (-not (Get-ChildItem -Path $target -Force | Where-Object { $_.Name -ne '.git' })) {
        Write-Host "[跳过] $($patch.Name): $submodule 工作树是空的,先跑 git submodule update --init third_party/$submodule" -ForegroundColor Yellow
        $skipped++
        continue
    }

    Push-Location $target
    try {
        # 已应用? 反向 check 成功 = 补丁内容已在工作树里
        & git apply --check --reverse -- $patch.FullName 2>$null
        if ($LASTEXITCODE -eq 0) {
            Write-Host "[已应用] $($patch.Name)" -ForegroundColor DarkGray
            $skipped++
            continue
        }

        # 能否干净应用
        & git apply --check -- $patch.FullName 2>$null
        if ($LASTEXITCODE -ne 0) {
            Write-Host "[失败] $($patch.Name): 无法干净应用 —— 子模块版本可能已变,需要重新导出补丁(见 README)" -ForegroundColor Red
            $failed++
            continue
        }

        if ($DryRun) {
            Write-Host "[将应用] $($patch.Name) → third_party/$submodule" -ForegroundColor Cyan
            $applied++
            continue
        }

        & git apply -- $patch.FullName
        if ($LASTEXITCODE -ne 0) {
            Write-Host "[失败] $($patch.Name): git apply 返回 $LASTEXITCODE" -ForegroundColor Red
            $failed++
            continue
        }

        Write-Host "[已应用] $($patch.Name) → third_party/$submodule" -ForegroundColor Green
        $applied++
    }
    finally {
        Pop-Location
    }
}

Write-Host ""
$verb = if ($DryRun) { "将应用" } else { "应用" }
Write-Host "$verb=$applied  跳过=$skipped  失败=$failed"

# 失败要让调用方(CI / 构建脚本)看得见
if ($failed -gt 0) { exit 1 }
exit 0
