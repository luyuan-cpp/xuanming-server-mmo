#requires -Version 7
[CmdletBinding()]
param()
$ErrorActionPreference='Stop'
. (Join-Path $PSScriptRoot '../lib/sql_boot_catalog_local.ps1')

function Assert([bool]$condition,[string]$case) { if (-not $condition) { throw "Assertion failed: $case" } }
$pwsh=[Environment]::ProcessPath
$trace=New-LocalJavaFixtureDiagnostic
# Deliberately put a sentinel in all three data channels. Public diagnostics must only contain metadata.
$sentinel='UNIT_PRIVATE_SENTINEL'
$childCommand="[Console]::Out.Write('$sentinel');[Console]::Error.Write('$sentinel');exit 19"
$r=Invoke-LocalSqlBootProcess -FilePath $pwsh -Arguments @('-NoProfile','-NonInteractive','-Command',$childCommand) -StandardInput $sentinel -Environment @{UNIT_TEST=$sentinel} -TimeoutSeconds 15 -RequireCompleteExit -FixtureDiagnostic $trace -FixtureOperation 'cf3-read-file'
$event=$trace.nativeCalls[0]
Assert ($r.Code -eq 19 -and $r.Out -ceq $sentinel -and $r.Err -ceq $sentinel) 'original process output and exit'
Assert ($event.classification -ceq 'nonzero-exit' -and $event.exitConfirmed -and $event.exitCode -eq 19 -and $event.elapsedMs -gt 0 -and $event.sourceLine -gt 0) 'confirmed nonzero'
Assert (($trace | ConvertTo-Json -Depth 10) -cnotmatch $sentinel) 'no input, environment, stdout or stderr in diagnostics'

$trace=New-LocalJavaFixtureDiagnostic
$failed=$false
try {
    $null=Invoke-LocalSqlBootProcess -FilePath $pwsh -Arguments @('-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 30') -TimeoutSeconds 1 -RequireCompleteExit -FixtureDiagnostic $trace -FixtureOperation 'cf3-read-file'
} catch { $failed=$true }
$event=$trace.nativeCalls[0]
Assert ($failed -and $trace.nativeCalls.Count -eq 1 -and $event.classification -ceq 'timeout' -and $event.exitConfirmed -and $null -ne $event.exitCode -and $event.elapsedMs -ge 900) 'timeout retains confirmed process exit, with no retry'

$trace=New-LocalJavaFixtureDiagnostic
$failed=$false
try {
    $null=Invoke-LocalSqlBootProcess -FilePath (Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString('N') + '.exe')) -FixtureDiagnostic $trace -FixtureOperation 'sql-surface'
} catch { $failed=$true }
$event=$trace.nativeCalls[0]
Assert ($failed -and $event.classification -ceq 'start-unconfirmed' -and -not $event.exitConfirmed -and $null -eq $event.exitCode) 'failed launch differs from process exit'

function Test-FirstProbeFailure {
    param([bool]$AfterAttempt)
    $trace=New-LocalJavaFixtureDiagnostic
    $trace.stage='schema-identity'
    $root=Join-Path ([IO.Path]::GetTempPath()) ('java-fixture-diag-test-'+[Guid]::NewGuid().ToString('N'))
    $null=[IO.Directory]::CreateDirectory($root)
    $settings=[pscustomobject]@{FixtureRoot=$root;Clock=[Diagnostics.Stopwatch]::StartNew();Diagnostic=$trace
        PrivateHelper=$pwsh;SchemaDsnFile=$sentinel;RootPrincipal=$sentinel;SqlFileSurface=$sentinel}
    # Stub the transport, never CF3-write credentials or launch any database process.
    function Invoke-LocalSqlBootProcess {
        param($FilePath,$Arguments,$TimeoutSeconds,$StandardInput,[switch]$RequireCompleteExit,$FixtureDiagnostic,$FixtureOperation)
        $code=0
        if (-not $AfterAttempt -and $FixtureOperation -ceq 'cf3-read-file') { $code=19 }
        if ($AfterAttempt -and $FixtureOperation -ceq 'sql-surface') {
            $output=$Arguments[[array]::IndexOf($Arguments,'--output-file')+1]
            [IO.File]::WriteAllText("$output.started",'test')
            $code=5
        }
        if ($AfterAttempt -and $FixtureOperation -ceq 'cf3-read-file' -and $Arguments[3].EndsWith('.result.json')) { $code=3 }
        $FixtureDiagnostic.nativeCalls.Add(@{operation=$FixtureOperation;classification=if ($code) {'nonzero-exit'} else {'confirmed'}
            exitConfirmed=$true;exitCode=$code;elapsedMs=1;sourceLine=$MyInvocation.ScriptLineNumber})
        return @{Code=$code;Out=$sentinel;Err=$sentinel}
    }
    try {
        $failed=$false
        try { $null=Invoke-LocalJavaFixtureSql $settings $sentinel 'query' } catch {
            $failed=$true
            Set-LocalJavaFixtureFailure $trace $_ 'producer'
        }
        $f=$trace.failure
        Assert ($failed -and $f.stage -ceq 'schema-identity' -and $f.sourceLine -gt 0 -and $f.classification -ceq 'nonzero-exit') 'first inner failure survives outer hold'
        Assert ($f.attemptMarkerPresent -eq $AfterAttempt -and -not $f.resultPresent) 'actual marker and result presence'
        $native=@($trace.nativeCalls | Where-Object operation -CEQ 'sql-surface')
        if ($AfterAttempt) {
            Assert ($f.step -ceq 'sql-read-result' -and $native.Count -eq 1 -and $f.nativeOperation -ceq 'sql-surface') 'failed native marker without terminal output, no replay'
        } else {
            Assert ($f.step -ceq 'sql-input' -and $native.Count -eq 0 -and $f.nativeOperation -ceq 'cf3-read-file') 'CF3 readback failure before SQL launch'
        }
        Assert (($trace | ConvertTo-Json -Depth 10) -cnotmatch $sentinel) 'failed operation has no secret body'
    } finally {
        # Only this test's fresh directory and the sole synthetic marker may be removed.
        foreach ($marker in [IO.Directory]::GetFiles($root,'sql-*.result.json.started')) { [IO.File]::Delete($marker) }
        [IO.Directory]::Delete($root,$false)
    }
}
Test-FirstProbeFailure $false
Test-FirstProbeFailure $true

$legacy=[pscustomobject]@{Clock=[pscustomobject]@{ElapsedMilliseconds=109000}}
Assert ((Get-LocalJavaFixtureOperationSeconds $legacy) -eq 1) 'legacy direct callers retain 110 seconds'
$legacy.Clock.ElapsedMilliseconds=110000
$rejected=$false
try { $null=Get-LocalJavaFixtureOperationSeconds $legacy } catch { $rejected=$true }
Assert $rejected 'legacy deadline remains closed'
$owned=[pscustomobject]@{ActionBudgetSeconds=480;Clock=[pscustomobject]@{ElapsedMilliseconds=112684}}
Assert ((Get-LocalJavaFixtureOperationSeconds $owned) -eq 60 -and (Get-LocalJavaFixtureOperationSeconds $owned 30) -eq 30) 'owned action still caps individual native SQL and register operations'
$owned.Clock.ElapsedMilliseconds=479000
Assert ((Get-LocalJavaFixtureOperationSeconds $owned) -eq 1) 'owned action clamps near its deadline'
$owned.Clock.ElapsedMilliseconds=480000
$rejected=$false
try { $null=Get-LocalJavaFixtureOperationSeconds $owned } catch { $rejected=$true }
Assert $rejected 'owned deadline remains closed'
foreach ($invalid in @(0,601)) {
    $rejected=$false
    try { Invoke-LocalJavaFixtureAction -ActionBudgetSeconds $invalid }
    catch [Management.Automation.ParameterBindingException] { $rejected=$true }
    Assert $rejected 'invalid formal action budget rejected before I/O'
}
[pscustomobject]@{passed=11;nativeProcesses='local-pwsh-only';sqlExecuted=$false;dockerInvoked=$false} | ConvertTo-Json -Compress
