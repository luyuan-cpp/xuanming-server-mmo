# stress_snap.ps1 — periodic multi-endpoint Prometheus snapshot for stress runs.
#
# Per AGENTS.md §6.2, every stress round must capture prom snapshots at
# ramp-end / steady-mid / steady-end and store them under the run dir's
# prom-snapshots/ folder for stress_summarize.ps1 to consume.
#
# Round 16+ scrapes three endpoints in parallel:
#   :9101  login            (entergame_*, dataloader_preload_*)
#   :9150  scene_manager    (scene_manager_enter_scene_stage_seconds, …)
#   :9160  db               (db_task_stage_seconds, db_task_result_total)
#
# Each snapshot is written as `t{N}m_<service>.txt` so stress_summarize.ps1
# can pick up the login snapshots while the SM/db files are available for
# bespoke deep-dive grep.
#
# Usage:
#   pwsh tools/scripts/stress_snap.ps1 `
#       -RunDir robot/logs/stress-45k-z1-r16-20260601-120000 `
#       -StartTime '2026-06-01 12:00:00' `
#       -Stages '2,5,10,15,18'
#
# StartTime is the wall-clock moment when the robot ramp begins (use the
# value printed by the stress launcher). Stages is a comma-separated list
# of minute offsets from StartTime to capture.

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$RunDir,

    [Parameter(Mandatory = $true)]
    [string]$StartTime,

    [string]$Stages = '2,5,10,15,18',

    [hashtable]$Endpoints = @{
        'login' = 'http://127.0.0.1:9101/metrics'
        'sm'    = 'http://127.0.0.1:9150/metrics'
        'db'    = 'http://127.0.0.1:9160/metrics'
    }
)

$ErrorActionPreference = 'Stop'

$snapDir = Join-Path $RunDir 'prom-snapshots'
if (-not (Test-Path $snapDir)) {
    New-Item -ItemType Directory -Path $snapDir | Out-Null
}

$logFile = Join-Path $snapDir 'job.log'
$start   = [datetime]$StartTime

function Write-JobLog($msg) {
    $ts = (Get-Date -Format 'yyyy-MM-dd HH:mm:ss')
    "$ts $msg" | Out-File -Append -FilePath $logFile -Encoding UTF8
}

Write-JobLog "stress_snap started: runDir=$RunDir start=$StartTime stages=$Stages"
Write-JobLog "endpoints: $($Endpoints.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" } | Join-String -Separator ' ')"

$stageList = $Stages.Split(',') | ForEach-Object { [int]$_.Trim() }

foreach ($mins in $stageList) {
    $tag = "t${mins}m"
    $tgt = $start.AddMinutes($mins)
    $wait = ($tgt - (Get-Date)).TotalSeconds
    if ($wait -gt 0) {
        Write-JobLog "$tag waiting $([int]$wait)s until $($tgt.ToString('HH:mm:ss'))"
        Start-Sleep -Seconds ([int]$wait)
    }

    foreach ($svc in $Endpoints.Keys) {
        $url = $Endpoints[$svc]
        $outFile = Join-Path $snapDir "${tag}_${svc}.txt"
        try {
            $c = (Invoke-WebRequest -Uri $url -TimeoutSec 10 -UseBasicParsing).Content
            Set-Content -Path $outFile -Value $c -Encoding UTF8
            Write-JobLog "$tag $svc saved size=$($c.Length) ($url)"
        } catch {
            Write-JobLog "$tag $svc FAIL ($url): $($_.Exception.Message)"
        }
    }
}

Write-JobLog "stress_snap finished: all stages captured"
