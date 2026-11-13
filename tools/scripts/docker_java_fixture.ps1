#requires -Version 7
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateSet('check-helper','create','close')][string]$Action,
    [string]$StateFile = '',
    [ValidateRange(1,600)][int]$ActionBudgetSeconds=480,
    [Parameter(Mandatory)][string]$PrivateHelper,
    [Parameter(Mandatory)][string]$PrivateHelperSha256
)
$ErrorActionPreference = 'Stop'
$repo = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../..'))
. (Join-Path $PSScriptRoot 'lib/sql_boot_catalog_local.ps1')

function Write-FixtureReceipt([string]$Path, $Value) {
    $stream = [IO.File]::Open($Path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
    try {
        $bytes = [Text.UTF8Encoding]::new($false).GetBytes(($Value | ConvertTo-Json -Depth 20))
        $stream.Write($bytes, 0, $bytes.Length)
        $stream.Flush($true)
    } finally { $stream.Dispose() }
}

function Assert-FixtureHelper {
    if (-not [IO.Path]::IsPathFullyQualified($PrivateHelper) -or $PrivateHelperSha256 -cnotmatch '^[a-fA-F0-9]{64}$') { throw 'Invalid helper pin' }
    $full = [IO.Path]::GetFullPath($PrivateHelper)
    if (-not [IO.File]::Exists($full)) { throw 'Helper unavailable' }
    $trusted = @([Security.Principal.WindowsIdentity]::GetCurrent().User.Value, 'S-1-5-18', 'S-1-5-32-544',
        ([Security.Principal.NTAccount]::new('NT SERVICE\TrustedInstaller').Translate([Security.Principal.SecurityIdentifier]).Value))
    # Same distinction as WriterAuthority: creating siblings is not replacing an
    # existing helper ancestor. Do not alter any ACL, install, or copy a helper.
    $mutation = [Security.AccessControl.FileSystemRights]::WriteExtendedAttributes -bor
        [Security.AccessControl.FileSystemRights]::DeleteSubdirectoriesAndFiles -bor
        [Security.AccessControl.FileSystemRights]::WriteAttributes -bor
        [Security.AccessControl.FileSystemRights]::Delete -bor
        [Security.AccessControl.FileSystemRights]::ChangePermissions -bor
        [Security.AccessControl.FileSystemRights]::TakeOwnership
    for ($part = $full; $part; $part = [IO.Path]::GetDirectoryName($part)) {
        $item = Get-Item -LiteralPath $part -Force
        if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw 'Reparse helper path' }
        $acl = Get-Acl -LiteralPath $part
        if ($acl.GetOwner([Security.Principal.SecurityIdentifier]).Value -cnotin $trusted) { throw 'Untrusted helper owner' }
        $mask = $mutation
        if ($part -ceq $full) { $mask = $mask -bor [Security.AccessControl.FileSystemRights]::WriteData -bor [Security.AccessControl.FileSystemRights]::AppendData }
        foreach ($rule in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
            if ($rule.AccessControlType -eq 'Allow' -and -not ($rule.PropagationFlags -band [Security.AccessControl.PropagationFlags]::InheritOnly) -and
                $rule.IdentityReference.Value -cnotin $trusted -and ($rule.FileSystemRights -band $mask)) { throw 'Replaceable helper path' }
        }
    }
    if ((Get-FileHash -LiteralPath $full -Algorithm SHA256).Hash -ine $PrivateHelperSha256) { throw 'Helper pin changed' }
}

function Assert-FixtureCapabilityRow {
    param([object[]]$Row)
    if ($Row.Count -ne 8) { throw 'Capability column count mismatch' }
    # The pinned image is the approved patched 8.5.8 build, not an arbitrary
    # version sharing its prefix. Both public build identities must match once.
    $release = @([regex]::Matches([string]$Row[0], '(?m)^Release Version: ([^\r\n]+)\r?$'))
    $commit = @([regex]::Matches([string]$Row[0], '(?m)^Git Commit Hash: ([a-f0-9]{40})\r?$'))
    if ($release.Count -ne 1 -or $commit.Count -ne 1 -or
        $release[0].Groups[1].Value -cne 'v8.5.8-20261021-849a85c' -or
        $commit[0].Groups[1].Value -cne '849a85c3795c3472b46f59a8f2917b880d7ee923') { throw 'Capability build identity mismatch' }
    if ($Row[1] -cne '' -or $Row[2] -cne '0' -or $Row[3] -cne 'strict' -or $Row[4] -cne 'leader' -or
        $Row[5] -cne '0' -or $Row[6] -cnotmatch '^[0-9]+$' -or $Row[7] -cnotmatch '^[0-9]+$') { throw 'Capability session mismatch' }
}

$phase = 'helper-qualification'
$producerStarted = $false
$diagnostic = New-LocalJavaFixtureDiagnostic
try {
    Assert-FixtureHelper
    if ($Action -ceq 'check-helper') { exit 0 }
    $phase = 'input-validation'
    if (-not [IO.Path]::IsPathFullyQualified($StateFile)) { throw 'Absolute state required' }
    $state = Get-Content -LiteralPath $StateFile -Raw | ConvertFrom-Json -AsHashtable
    $evidence = [IO.Path]::GetDirectoryName([IO.Path]::GetFullPath($StateFile))
    $expectedBase = [IO.Path]::GetFullPath((Join-Path $repo 'build/verification')).TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar
    if (-not $evidence.StartsWith($expectedBase, [StringComparison]::OrdinalIgnoreCase) -or $state.mode -cne 'owned' -or
        $state.nonce -cnotmatch '^[a-f0-9]{32}$' -or $state.project -cne "xuanming-java-fixture-$($state.nonce)" -or
        $state.directory -ine $evidence -or [IO.Path]::GetFileName($StateFile) -cne 'state.json') { throw 'State owner mismatch' }
    $nonce = $state.nonce
    $privateRoot = Join-Path $evidence 'private'
    $fixtureRoot = Join-Path $privateRoot "java-fixture-$nonce"
    if ($state.privateRoot -ine $privateRoot -or $state.fixtureRoot -ine $fixtureRoot -or $state.contextFile -ine (Join-Path $fixtureRoot 'context.json')) { throw 'State path mismatch' }
    foreach ($name in @('writer_authority','sql_file_surface')) {
        $tool = $state.tools[$name]
        if ($tool.path -ine (Join-Path $evidence "tools/$name.exe") -or $tool.sha256 -cnotmatch '^[a-f0-9]{64}$' -or
            (Get-FileHash -LiteralPath $tool.path -Algorithm SHA256).Hash -ine $tool.sha256 -or
            ((Get-Item -LiteralPath $tool.path).Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Tool pin mismatch' }
    }
    if (-not [IO.File]::Exists((Join-Path $evidence 'infra.completed.json'))) { throw 'Infrastructure confirmation required' }
    # Control capabilities exist only in this producer process, never its Java caller.
    foreach ($name in @((Get-ChildItem Env:COMMERCE_IT_* | Select-Object -ExpandProperty Name)) +
        @('MMORPG_SQL_SCHEMA_DSN','MMORPG_SQL_REGISTRAR_DSN','MMORPG_SQL_MAINTENANCE_DSN','MMORPG_SQL_WRITER_GRANT_FILE',
          'MMORPG_SQL_WRITER_GRANT_SHA256','MMORPG_SQL_WRITER_CONSUME_DIR')) { [Environment]::SetEnvironmentVariable($name, $null) }
    $env:MMORPG_SQL_PRIVATE_FILE_HELPER = $PrivateHelper
    $env:MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256 = $PrivateHelperSha256.ToLowerInvariant()
    $env:COMMERCE_IT_REPO_ROOT = $repo
    $env:COMMERCE_IT_PRIVATE_ROOT = $privateRoot
    $env:COMMERCE_IT_SCHEMA_PRINCIPAL = 'root@%'
    $env:COMMERCE_IT_SCHEMA_DSN_FILE = Join-Path $privateRoot 'schema.dsn'
    $env:COMMERCE_IT_PHYSICAL_AUTHORITY_FILE = Join-Path $privateRoot 'physical-authority.original'
    $env:COMMERCE_IT_LOCK_QUALIFICATION_FILE = Join-Path $privateRoot 'lock-qualification.tidb.SELECT_ONLY.json'
    $env:COMMERCE_IT_WRITER_TOOL = $state.tools.writer_authority.path
    $env:COMMERCE_IT_WRITER_TOOL_SHA256 = $state.tools.writer_authority.sha256
    $env:COMMERCE_IT_SQL_FILE_SURFACE = $state.tools.sql_file_surface.path
    $env:COMMERCE_IT_SQL_FILE_SURFACE_SHA256 = $state.tools.sql_file_surface.sha256
    Write-FixtureReceipt (Join-Path $evidence "producer-$Action.started.json") @{status='started';actionBudgetSeconds=$ActionBudgetSeconds;automaticRetry=$false}
    $producerStarted = $true
    $phase = 'prepare-private-inputs'
    if ($Action -ceq 'create') {
        if (Test-Path -LiteralPath $privateRoot) { throw 'Private root must be new' }
        Initialize-LocalSqlBootPrivateDirectory -Path $privateRoot
        $created = Invoke-LocalSqlBootProcess -FilePath $PrivateHelper -Arguments @('--command','create-file','--path',$env:COMMERCE_IT_SCHEMA_DSN_FILE,'--max-bytes','65536') -StandardInput 'root@tcp(127.0.0.1:24000)/' -TimeoutSeconds 10 -RequireCompleteExit -FixtureDiagnostic $diagnostic -FixtureOperation 'cf3-create-file'
        if ($created.Code -ne 0) { throw 'Schema private original unconfirmed' }
        $probeRoot = Join-Path $privateRoot 'probe'
        $null = [IO.Directory]::CreateDirectory($probeRoot)
        $probeIo = [pscustomobject]@{FixtureRoot=$probeRoot;PrivateHelper=$PrivateHelper;Clock=[Diagnostics.Stopwatch]::StartNew();SchemaDsnFile=$env:COMMERCE_IT_SCHEMA_DSN_FILE;RootPrincipal='root@%';SqlFileSurface=$env:COMMERCE_IT_SQL_FILE_SURFACE;Diagnostic=$diagnostic}
        $sql = 'SELECT TIDB_VERSION(),@@SESSION.tidb_snapshot,@@SESSION.tidb_read_staleness,@@SESSION.tidb_read_consistency,@@SESSION.tidb_replica_read,@@SESSION.tidb_low_resolution_tso,@@SESSION.max_execution_time,@@SESSION.tidb_dml_max_execution_time'
        # One actual SQL capability probe after bounded greeting readiness; no SQL retries.
        $phase = 'capability-probe'
        $diagnostic.stage = $phase
        $probe = Invoke-LocalJavaFixtureSql $probeIo $sql 'query'
        $phase = 'capability-validation'
        if (@($probe.rows).Count -ne 1) { throw 'Capability row count mismatch' }
        $row = @($probe.rows[0])
        Assert-FixtureCapabilityRow -Row $row
        $phase = 'record-capability'
        Write-FixtureReceipt (Join-Path $evidence 'probe.completed.json') @{status='confirmed';backend='tidb';version='8.5.8';release='v8.5.8-20261021-849a85c';gitCommit='849a85c3795c3472b46f59a8f2917b880d7ee923';strongRead=$true;executionBudgetVariables=$true;productionI3Qualification=$false}
        $phase = 'prepare-local-qualification'
        $settings = [pscustomobject]@{Endpoint='127.0.0.1:24000';Backend='tidb';LockPrimitive='SELECT_ONLY'}
        $null = Get-LocalSqlBootPhysicalAuthority -Settings $settings -PrivateRoot $privateRoot
        $null = Get-LocalSqlBootLockQualification -Settings $settings -PrivateRoot $privateRoot
        $null = [IO.Directory]::CreateDirectory($fixtureRoot)
    } else {
        Assert-LocalSqlBootPrivateDirectory -Path $privateRoot
        if (-not [IO.File]::Exists((Join-Path $evidence 'create.completed.json'))) { throw 'Confirmed creation required' }
    }
    $phase = 'prepare-formal-' + $Action
    foreach ($pair in @(@('COMMERCE_IT_PHYSICAL_AUTHORITY_FILE','COMMERCE_IT_PHYSICAL_AUTHORITY_SHA256'),
                         @('COMMERCE_IT_LOCK_QUALIFICATION_FILE','COMMERCE_IT_LOCK_QUALIFICATION_SHA256'))) {
        [Environment]::SetEnvironmentVariable($pair[1], (Get-FileHash -LiteralPath ([Environment]::GetEnvironmentVariable($pair[0])) -Algorithm SHA256).Hash.ToLowerInvariant())
    }
    $io = [pscustomobject]@{FixtureRoot=$fixtureRoot;PrivateHelper=$PrivateHelper;Diagnostic=$diagnostic}
    if ($Action -ceq 'create') {
        $requestFile = Join-Path $fixtureRoot 'action-1.request.json'
        $request = @{format='JAVA_OWNED_TIDB_ACTION_V1';nonce=$nonce;root=$fixtureRoot;outputFile=$state.contextFile;endpoint='jdbc:mysql://127.0.0.1:24000/';schema="mall_it_$nonce"}
    } else {
        $confirmed = Get-Content -LiteralPath (Join-Path $evidence 'producer-create.completed.json') -Raw | ConvertFrom-Json
        $context = Read-LocalJavaFixtureJson $io $state.contextFile $confirmed.contextSha256
        $maximum = 1
        foreach ($file in @(Get-ChildItem -LiteralPath $fixtureRoot -File -Filter 'action-*.request.json')) {
            if ($file.Name -cmatch '^action-([1-9][0-9]*)[.]request[.]json$') { $maximum = [Math]::Max($maximum, [int]$Matches[1]) }
        }
        $requestFile = Join-Path $fixtureRoot "action-$($maximum+1).request.json"
        $request = @{format='JAVA_OWNED_TIDB_ACTION_V1';nonce=$nonce;root=$fixtureRoot;outputFile=(Join-Path $fixtureRoot 'closed.json');ownerFile=$context.ownerFile;ownerSha256=$context.ownerSha256}
    }
    $sha = Write-LocalJavaFixtureJson $io $requestFile $request
    $phase = 'formal-' + $Action
    Invoke-LocalJavaFixtureAction -Action $Action -RequestFile $requestFile -RequestSha256 $sha -Diagnostic $diagnostic -ActionBudgetSeconds $ActionBudgetSeconds
    $phase = 'confirm-formal-' + $Action
    $record = Read-LocalJavaFixtureFile $io $request.outputFile
    Write-FixtureReceipt (Join-Path $evidence "producer-$Action.completed.json") @{status='confirmed';contextSha256=$record.Sha256;localQualificationOnly=$true;nativeCalls=$diagnostic.nativeCalls.ToArray()}
    exit 0
} catch {
    # Never print a caught exception, SQL, context, account, password, or DSN.
    $failureType = $_.Exception.GetType().FullName
    if ($producerStarted) {
        Set-LocalJavaFixtureFailure $diagnostic $_ 'runner'
        try { Write-FixtureReceipt (Join-Path $evidence "producer-$Action.failure.json") @{phase=$phase;type=$failureType;failure=$diagnostic.failure;nativeCalls=$diagnostic.nativeCalls.ToArray()} } catch { }
    }
    [Console]::Error.WriteLine("Java fixture producer phase '$phase' failed or outcome is unknown; retain original evidence; do not retry or clean up.")
    exit 1
}
