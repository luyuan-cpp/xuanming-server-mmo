<#
.SYNOPSIS
    Report monthly git commit statistics (lines added / deleted / net).

.PARAMETER Year
    Four-digit year to report on.  Defaults to the current year.

.PARAMETER Month
    Month number (1-12) to report on.  Defaults to the current month.

.PARAMETER Author
    Optional author filter (name or email substring).

.EXAMPLE
    # Current month
    .\git_stats.ps1

.EXAMPLE
    # Specific month
    .\git_stats.ps1 -Year 2026 -Month 3

.EXAMPLE
    # Filter by author
    .\git_stats.ps1 -Author "alice"
#>
param(
    [int]$Year  = (Get-Date).Year,
    [int]$Month = (Get-Date).Month,
    [string]$Author = ""
)

$ErrorActionPreference = "Stop"

# Build the date window for the requested month
$Since = "{0:D4}-{1:D2}-01" -f $Year, $Month
$Until = (New-Object DateTime($Year, $Month, 1)).AddMonths(1).ToString("yyyy-MM-dd")

$Label = "{0:D4}-{1:D2}" -f $Year, $Month
Write-Host "Git commit statistics for $Label" -ForegroundColor Cyan
Write-Host ("=" * 45) -ForegroundColor Cyan

# Build git log arguments
$gitArgs = @(
    "log",
    "--since=$Since",
    "--until=$Until",
    "--pretty=tformat:",
    "--numstat"
)
if (-not [string]::IsNullOrWhiteSpace($Author)) {
    $gitArgs += "--author=$Author"
}

$numstatOutput = & git @gitArgs 2>&1
if ($LASTEXITCODE -ne 0) {
    throw "git log failed: $numstatOutput"
}

$linesAdded   = 0
$linesDeleted = 0
$filesChanged = 0

foreach ($line in $numstatOutput) {
    # git --numstat 固定用 TAB 分隔三列;路径本身可以包含空格。
    if ($line -match '^(\d+)\t(\d+)\t.+$') {
        $linesAdded   += [int]$Matches[1]
        $linesDeleted += [int]$Matches[2]
        $filesChanged++
    }
}

# Count commits separately
$commitArgs = @(
    "log",
    "--since=$Since",
    "--until=$Until",
    "--oneline"
)
if (-not [string]::IsNullOrWhiteSpace($Author)) {
    $commitArgs += "--author=$Author"
}

$commitLines = @(& git @commitArgs 2>&1 | Where-Object { $_ -ne "" })
$commitCount = $commitLines.Count

$netLines = $linesAdded - $linesDeleted

Write-Host ("Period        : {0} — {1}" -f $Since, ([DateTime]::Parse($Until).AddDays(-1).ToString("yyyy-MM-dd")))
if (-not [string]::IsNullOrWhiteSpace($Author)) {
    Write-Host ("Author filter : $Author")
}
Write-Host ("Commits       : {0}" -f $commitCount)
Write-Host ("Files changed : {0}" -f $filesChanged)
Write-Host ("Lines added   : {0}" -f $linesAdded)   -ForegroundColor Green
Write-Host ("Lines deleted : {0}" -f $linesDeleted)  -ForegroundColor Red
Write-Host ("Net lines     : {0}" -f $netLines)      -ForegroundColor Yellow
