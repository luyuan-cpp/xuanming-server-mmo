#requires -Version 7
[CmdletBinding()]
param([string]$ProbeResultFile = (Join-Path $PSScriptRoot 'fixtures/docker_java_fixture_capability.public.json'))
$ErrorActionPreference = 'Stop'
$source = Join-Path $PSScriptRoot '../docker_java_fixture.ps1'
$tokens = $null; $errors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile($source, [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw 'Formal fixture runner parse failed' }
$function = $ast.Find({ param($node)
    $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -ceq 'Assert-FixtureCapabilityRow'
}, $true)
if ($null -eq $function) { throw 'Formal capability validator missing' }
# Import only the pure row validator, never the producer or its action entrypoint.
. ([ScriptBlock]::Create($function.Extent.Text))
$observed = Get-Content -LiteralPath $ProbeResultFile -Raw | ConvertFrom-Json
if ($observed.format -cne 'JAVA_FIXTURE_CAPABILITY_OBSERVED_V1' -or $observed.target_outcome -cne 'confirmed' -or
    $observed.credentials_written -ne $false -or @($observed.rows).Count -ne 1) { throw 'Expected the public capability-only evidence' }
$row = [object[]]@($observed.rows[0])
$script:passed = 0
function Assert-Rejected([object[]]$Value, [string]$Case) {
    try { Assert-FixtureCapabilityRow -Row $Value }
    catch { $script:passed++; return }
    throw "Capability validator accepted invalid case: $Case"
}
Assert-FixtureCapabilityRow -Row $row
$script:passed++
$crlf = [object[]]$row.Clone(); $crlf[0] = $crlf[0].Replace("`n", "`r`n")
Assert-FixtureCapabilityRow -Row $crlf
$script:passed++
foreach ($release in @('v8.5.80-20261021-849a85c','v8.5.8','v8.5.8-20261022-849a85c','v8.5.8-20261021-0000000')) {
    $changed = [object[]]$row.Clone(); $changed[0] = $changed[0].Replace('v8.5.8-20261021-849a85c', $release)
    Assert-Rejected $changed 'different release'
}
$changed = [object[]]$row.Clone(); $changed[0] = $changed[0].Replace('849a85c3795c3472b46f59a8f2917b880d7ee923', ('0' * 40))
Assert-Rejected $changed 'different commit'
$changed = [object[]]$row.Clone(); $changed[0] += "`nRelease Version: v8.5.8-20261021-849a85c"
Assert-Rejected $changed 'duplicate release'
$changed = [object[]]$row.Clone(); $changed[0] += "`nGit Commit Hash: 849a85c3795c3472b46f59a8f2917b880d7ee923"
Assert-Rejected $changed 'duplicate commit'
foreach ($index in 1..7) {
    $changed = [object[]]$row.Clone(); $changed[$index] = @('unused','snapshot','1','weak','follower','1','not-a-number','-1')[$index]
    Assert-Rejected $changed "session column $index"
}
Assert-Rejected ([object[]]$row[0..6]) 'missing column'
Assert-Rejected ([object[]]($row + @('extra'))) 'extra column'
[pscustomobject]@{passed=$script:passed;publicProbeOnly=$true;producerInvoked=$false;sqlExecuted=$false} | ConvertTo-Json -Compress
