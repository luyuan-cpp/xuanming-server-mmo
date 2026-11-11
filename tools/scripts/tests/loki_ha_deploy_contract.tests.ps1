#requires -Version 7
<#
.SYNOPSIS
    集群内 Loki 两种形态的部署契约测试(docs/design/no-single-node-horizontal-scaling-20261001.md §10,批 B)。

.DESCRIPTION
    钉住:
      1. 纯函数:副本数只认 1 / 3,形态变更只放行 1 → 3,对象存储参数的校验(地址、桶名、档位、夹具只在 dev);
      2. DryRun:dev 缺省单副本;3 副本渲染 StatefulSet、PDB、memberlist、S3 配置,访问密钥只进 Secret、不进输出;
         dev 夹具渲染 SeaweedFS 并让 Loki 指向它;
      3. 参数不生效时拒绝(不部署集群内 Loki 却给了 -LokiReplicas / -LokiS3*)。
    子进程一律 -DryRun(不连集群);纯函数按 AST 从 k8s_deploy.ps1 抽出,在本进程里直接调用。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/loki_ha_deploy_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'

. "$PSScriptRoot/lib/test_harness.ps1"
. "$PSScriptRoot/lib/deploy_capture.ps1"

Write-Host ''
Write-Host '=== in-cluster Loki deployment contract tests ==='

$parseErrors = $null
$deployAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'), [ref]$null, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "部署脚本语法错误: $($parseErrors[0].Message)" }
$deployFunctions = @('Get-LokiTopologyFor', 'Get-LokiTopologyChangeVerdict', 'Get-LokiS3Settings')
foreach ($node in $deployAst.FindAll({ param($a) $a -is [Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    if ($deployFunctions -contains $node.Name) { Set-Item -Path "Function:script:$($node.Name)" -Value $node.Body.GetScriptBlock() }
}
foreach ($name in $deployFunctions) {
    if (-not (Test-Path "Function:$name")) { throw "k8s_deploy.ps1 缺少函数 $name" }
}

function Get-ThrownMessage([scriptblock]$Action) {
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

$Infra = 'loki-infra'
$InfraArgs = @('-Command', 'infra-up', '-DryRun', '-SkipPreflight', '-SkipGoSvc', '-SkipJavaSvc', '-InfraNamespace', $Infra,
    '-NodeImage', 'registry.invalid/test/mmorpg-node:0123456789ab')
$InfraEnv = @{ MMORPG_BATTLE_TOKEN_SECRET = 'contract-battle-ticket-0123456789abcdef012345'; MMORPG_GATE_TOKEN_SECRET = 'contract-gate-ticket-0123456789abcdef012345' }
$S3Env = $InfraEnv + @{ MMORPG_LOKI_S3_ACCESS_KEY_ID = 'contract-loki-key-id-0001'; MMORPG_LOKI_S3_SECRET_ACCESS_KEY = 'contract-loki-secret-value-0123456789' }

function Invoke-LokiDryRun {
    param([Parameter(Mandatory = $true)][string[]]$Arguments, [hashtable]$Env = @{})
    if ($Arguments -notcontains '-DryRun') { throw '本套夹具只允许显式 -DryRun,禁止连接或写集群。' }
    return Invoke-CapturedPwsh -ScriptPath (Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1') -Arguments $Arguments -Env $Env
}

function Get-KindBlock {
    param([Parameter(Mandatory = $true)][string]$Output, [Parameter(Mandatory = $true)][string]$Kind, [Parameter(Mandatory = $true)][string]$Name)
    $found = @(Get-ManifestBlocks -Output $Output | ForEach-Object { ($_ -replace "`r`n", "`n") -split '(?m)^---[ \t]*$' } |
        Where-Object { $_ -cmatch ('(?m)^kind: ' + $Kind + '\s*$') -and $_ -cmatch ('(?m)^  name: ' + [regex]::Escape($Name) + '\s*$') })
    if ($found.Count -ne 1) { throw "DryRun 输出里 $Kind $Name 应当恰好 1 份,实际 $($found.Count) 份" }
    return $found[0]
}

# ──────────────────────────────────────────────
# 1. 纯函数
# ──────────────────────────────────────────────
Test-Case '副本数只认 1 和 3,1 是 loki.yaml,3 是 loki-ha.yaml' {
    Assert-Equal -Expected 'loki.yaml' -Actual (Get-LokiTopologyFor -Replicas 1).Manifest -Because '单副本加本地盘'
    $ha = Get-LokiTopologyFor -Replicas 3
    Assert-Equal -Expected 'loki-ha.yaml' -Actual $ha.Manifest -Because '单体模式 3 副本'
    Assert-True -Condition $ha.HighAvailability -Because '3 副本是高可用形态'
    foreach ($bad in @(0, 2, 4, 5)) {
        Assert-Match -Text (Get-ThrownMessage { Get-LokiTopologyFor -Replicas $bad }) -Pattern 'must be 1 .* or 3' -Because "$bad 个副本要拒绝"
    }
}

Test-Case '形态变更只放行 1 → 3;3 → 1 和本脚本从不产生的形态一律拒绝' {
    foreach ($case in @(
            @{ Deployment = $null; StatefulSet = $null; Desired = 1; Verdict = 'absent' }
            @{ Deployment = $null; StatefulSet = $null; Desired = 3; Verdict = 'absent' }
            @{ Deployment = 1; StatefulSet = $null; Desired = 1; Verdict = 'unchanged' }
            @{ Deployment = 1; StatefulSet = $null; Desired = 3; Verdict = 'upgrade' }
            @{ Deployment = $null; StatefulSet = 3; Desired = 3; Verdict = 'unchanged' }
            @{ Deployment = $null; StatefulSet = 3; Desired = 1; Verdict = 'refuse' }
            @{ Deployment = 2; StatefulSet = $null; Desired = 3; Verdict = 'refuse' }
            @{ Deployment = $null; StatefulSet = 2; Desired = 3; Verdict = 'refuse' }
            @{ Deployment = 1; StatefulSet = 3; Desired = 3; Verdict = 'refuse' }
        )) {
        Assert-Equal -Expected $case.Verdict -Actual (Get-LokiTopologyChangeVerdict -LiveDeploymentReplicas $case.Deployment -LiveStatefulSetReplicas $case.StatefulSet -DesiredReplicas $case.Desired) `
            -Because "deployment=$($case.Deployment) statefulset=$($case.StatefulSet) → $($case.Desired)"
    }
}

function Reset-LokiS3Params {
    $script:LokiS3Endpoint = ''
    $script:LokiS3Bucket = ''
    $script:LokiS3Region = ''
    $script:LokiS3ForcePathStyle = $false
    $script:LokiS3Insecure = $false
    $script:LokiS3DevFixture = $false
    $script:ReleaseProfile = 'dev'
    $script:InfraNamespace = $Infra
}

Test-Case '对象存储参数:3 副本要 host:port 与桶名;http 只在 dev;夹具只在 dev 且不能再手写地址;单副本给了任何 -LokiS3* 都拒绝' {
    $single = Get-LokiTopologyFor -Replicas 1
    $ha = Get-LokiTopologyFor -Replicas 3

    Reset-LokiS3Params
    Assert-True -Condition ($null -eq (Get-LokiS3Settings -Topology $single)) -Because '单副本不用对象存储'
    $script:LokiS3Bucket = 'logs'
    Assert-Match -Text (Get-ThrownMessage { Get-LokiS3Settings -Topology $single }) -Pattern 'only applies to -LokiReplicas 3' -Because '参数不生效要拒绝'

    Reset-LokiS3Params
    $script:LokiS3Endpoint = 's3.example.internal:443'
    $script:LokiS3Bucket = 'mmorpg-logs'
    $script:LokiS3ForcePathStyle = $true
    $ok = Get-LokiS3Settings -Topology $ha
    Assert-Equal -Expected 's3.example.internal:443' -Actual $ok.Endpoint -Because '地址原样'
    Assert-Equal -Expected 'us-east-1' -Actual $ok.Region -Because '缺省区域'
    Assert-True -Condition ($ok.ForcePathStyle -and -not $ok.Insecure -and -not $ok.DevFixture) -Because '开关原样'

    foreach ($case in @(
            @{ Endpoint = 'https://s3.example.internal:443'; Bucket = 'mmorpg-logs'; Pattern = 'needs -LokiS3Endpoint as host:port' }
            @{ Endpoint = 'key:secret@s3.example.internal:443'; Bucket = 'mmorpg-logs'; Pattern = 'needs -LokiS3Endpoint as host:port' }
            @{ Endpoint = 's3.example.internal'; Bucket = 'mmorpg-logs'; Pattern = 'needs -LokiS3Endpoint as host:port' }
            @{ Endpoint = 's3.example.internal:443'; Bucket = ''; Pattern = 'needs -LokiS3Bucket' }
            @{ Endpoint = 's3.example.internal:443'; Bucket = 'Bad_Bucket'; Pattern = 'needs -LokiS3Bucket' }
        )) {
        Reset-LokiS3Params
        $script:LokiS3Endpoint = $case.Endpoint
        $script:LokiS3Bucket = $case.Bucket
        Assert-Match -Text (Get-ThrownMessage { Get-LokiS3Settings -Topology $ha }) -Pattern $case.Pattern -Because "$($case.Endpoint) / $($case.Bucket)"
    }

    Reset-LokiS3Params
    $script:LokiS3Endpoint = 's3.example.internal:443'
    $script:LokiS3Bucket = 'mmorpg-logs'
    $script:LokiS3Insecure = $true
    $script:ReleaseProfile = 'prod'
    Assert-Match -Text (Get-ThrownMessage { Get-LokiS3Settings -Topology $ha }) -Pattern 'must use https' -Because 'prod 不许走 http'

    Reset-LokiS3Params
    $script:LokiS3DevFixture = $true
    $fixture = Get-LokiS3Settings -Topology $ha
    Assert-Equal -Expected "seaweedfs-s3.${Infra}:8333" -Actual $fixture.Endpoint -Because '夹具地址'
    Assert-True -Condition ($fixture.DevFixture -and $fixture.ForcePathStyle -and $fixture.Insecure) -Because '夹具按路径寻址、走 http'
    $script:ReleaseProfile = 'staging'
    Assert-Match -Text (Get-ThrownMessage { Get-LokiS3Settings -Topology $ha }) -Pattern 'for dev and kind only' -Because '夹具不进 staging、prod'
    $script:ReleaseProfile = 'dev'
    $script:LokiS3Bucket = 'other'
    Assert-Match -Text (Get-ThrownMessage { Get-LokiS3Settings -Topology $ha }) -Pattern 'drop the other -LokiS3\* parameters' -Because '夹具和手写参数不能混用'
}

# ──────────────────────────────────────────────
# 2. DryRun 渲染
# ──────────────────────────────────────────────
Test-Case 'dev 缺省单副本:Deployment loki 加 PVC,不渲染 StatefulSet 与夹具' {
    $run = Invoke-LokiDryRun -Arguments $InfraArgs -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'Loki: replicas=1 manifest=loki\.yaml object_store=local-disk' -Because '入口打印本次形态'
    $null = Get-KindBlock -Output $run.Output -Kind 'Deployment' -Name 'loki'
    Assert-NotMatch -Text $run.Output -Pattern '(?m)^kind: StatefulSet\s*\r?\n(?:.*\r?\n)*?  name: loki\s*$' -Because '单副本不是 StatefulSet'
    Assert-NotMatch -Text $run.Output -Pattern 'seaweedfs' -Because '没开夹具'
}

Test-Case '3 副本:StatefulSet、PDB、memberlist、S3 配置都对;访问密钥只进 Secret,不进任何输出' {
    $args3 = $InfraArgs + @('-LokiReplicas', '3', '-LokiS3Endpoint', 's3.example.internal:443', '-LokiS3Bucket', 'mmorpg-logs', '-LokiS3ForcePathStyle')
    $run = Invoke-LokiDryRun -Arguments $args3 -Env $S3Env
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    Assert-Match -Text $run.Output -Pattern 'Loki: replicas=3 manifest=loki-ha\.yaml object_store=s3://mmorpg-logs@s3\.example\.internal:443' -Because '入口打印本次形态'

    $sts = Get-KindBlock -Output $run.Output -Kind 'StatefulSet' -Name 'loki'
    Assert-Match -Text $sts -Pattern '(?m)^  replicas: 3\s*$' -Because '3 副本'
    Assert-Match -Text $sts -Pattern 'podManagementPolicy: Parallel' -Because '三个 Pod 同时起,环才凑得齐'
    Assert-Match -Text $sts -Pattern '-target=all' -Because '单体模式,每个 Pod 跑全部组件'
    Assert-Match -Text $sts -Pattern '-config\.expand-env=true' -Because '访问密钥从环境变量展开'
    Assert-Match -Text $sts -Pattern 'name: loki-s3\s+key: access-key-id' -Because '访问密钥来自 Secret'
    Assert-Match -Text $sts -Pattern 'name: loki-s3-secret\s+key: secret-access-key' -Because '密钥来自 Secret'
    Assert-Match -Text $sts -Pattern 'mmorpg\.io/loki-s3-credentials-hash: "[0-9a-f]{12}"' -Because '换密钥 Pod 自动滚动'
    Assert-Match -Text $sts -Pattern 'podAntiAffinity' -Because '尽量分到不同节点'

    $pdb = Get-KindBlock -Output $run.Output -Kind 'PodDisruptionBudget' -Name 'loki-pdb'
    Assert-Match -Text $pdb -Pattern 'minAvailable: 2' -Because 'replication_factor 3 的写入要 2 份确认'
    $memberlist = Get-KindBlock -Output $run.Output -Kind 'Service' -Name 'loki-memberlist'
    Assert-Match -Text $memberlist -Pattern 'clusterIP: None' -Because 'headless'
    Assert-Match -Text $memberlist -Pattern 'publishNotReadyAddresses: true' -Because 'Ready 之前也要互相发现'
    $service = Get-KindBlock -Output $run.Output -Kind 'Service' -Name 'loki'
    Assert-Match -Text $service -Pattern 'port: 3100' -Because 'sidecar 与 Grafana 的地址不变'

    $config = Get-KindBlock -Output $run.Output -Kind 'ConfigMap' -Name 'loki-config'
    foreach ($pattern in @('replication_factor: 3', 'store: memberlist', "loki-memberlist\.${Infra}\.svc\.cluster\.local:7946",
            'endpoint: s3\.example\.internal:443', 'bucketnames: mmorpg-logs', 'region: us-east-1', 's3forcepathstyle: true', 'insecure: false',
            'object_store: s3', 'delete_request_store: s3', 'access_key_id: \$\{LOKI_S3_ACCESS_KEY_ID\}')) {
        Assert-Match -Text $config -Pattern $pattern -Because "Loki 配置要有 $pattern"
    }
    Assert-NotMatch -Text $run.Output -Pattern '__[A-Z][A-Z0-9_]*__' -Because '占位全部替换'
    foreach ($secret in @($S3Env.MMORPG_LOKI_S3_ACCESS_KEY_ID, $S3Env.MMORPG_LOKI_S3_SECRET_ACCESS_KEY)) {
        Assert-NotMatch -Text $run.Output -Pattern ([regex]::Escape($secret)) -Because '访问密钥不进 ConfigMap,也不进 DryRun 输出'
    }
    Assert-Match -Text $run.Output -Pattern 'Secret loki-s3 \(key access-key-id; value hidden\)' -Because 'DryRun 只打一行隐去值的意图'
    Assert-Match -Text $run.Output -Pattern 'Secret loki-s3-secret \(key secret-access-key; value hidden\)' -Because '同上'
}

Test-Case 'dev 夹具:SeaweedFS 先于 Loki 渲染,Loki 指向它、按路径寻址、走 http;夹具的身份配置只进 Secret' {
    $run = Invoke-LokiDryRun -Arguments ($InfraArgs + @('-LokiReplicas', '3', '-LokiS3DevFixture')) -Env $InfraEnv
    Assert-Equal -Expected 0 -Actual $run.ExitCode -Because "输出: $($run.Output)"
    $deployment = Get-KindBlock -Output $run.Output -Kind 'Deployment' -Name 'seaweedfs'
    Assert-Match -Text $deployment -Pattern 'image: chrislusf/seaweedfs:\d+\.\d+' -Because '钉具体版本'
    Assert-Match -Text $deployment -Pattern 's3\.bucket\.create -name loki' -Because '建桶'
    $config = Get-KindBlock -Output $run.Output -Kind 'ConfigMap' -Name 'loki-config'
    Assert-Match -Text $config -Pattern "endpoint: seaweedfs-s3\.${Infra}:8333" -Because 'Loki 指向夹具'
    Assert-Match -Text $config -Pattern 's3forcepathstyle: true' -Because '夹具按路径寻址'
    Assert-Match -Text $config -Pattern 'insecure: true' -Because '夹具走 http'
    Assert-True -Condition ($run.Output.IndexOf('name: seaweedfs') -lt $run.Output.IndexOf('kind: StatefulSet')) -Because '桶要先于 Loki 能写'
    Assert-Match -Text $run.Output -Pattern 'Secret seaweedfs-dev-s3-config \(key s3\.json; value hidden\)' -Because '身份配置里有密钥,只进 Secret'
    Assert-NotMatch -Text $run.Output -Pattern 'mmorpg-loki-dev-secret' -Because '夹具的开发密钥同样不进输出'
}

# ──────────────────────────────────────────────
# 3. 参数不生效时拒绝
# ──────────────────────────────────────────────
Test-Case '-LokiReplicas 2、3 副本缺地址或缺密钥、不部署集群内 Loki 却给了参数:都在任何清单之前拒绝' {
    foreach ($case in @(
            @{ Label = '2 个副本'; Args = @('-LokiReplicas', '2'); Env = $InfraEnv; Pattern = 'must be 1 .* or 3' }
            @{ Label = '3 副本缺地址'; Args = @('-LokiReplicas', '3'); Env = $S3Env; Pattern = 'needs -LokiS3Endpoint' }
            @{ Label = '3 副本缺密钥'; Args = @('-LokiReplicas', '3', '-LokiS3Endpoint', 's3.example.internal:443', '-LokiS3Bucket', 'mmorpg-logs'); Env = $InfraEnv; Pattern = 'needs MMORPG_LOKI_S3_ACCESS_KEY_ID' }
            @{ Label = '关了 sidecar'; Args = @('-LokiReplicas', '3', '-NoCppLogSidecar'); Env = $InfraEnv; Pattern = 'only apply when this command deploys the in-cluster Loki' }
            @{ Label = '指向外部 Loki'; Args = @('-LokiS3DevFixture', '-LokiPushUrl', 'http://loki.observability:3100/loki/api/v1/push'); Env = $InfraEnv; Pattern = 'only apply when this command deploys the in-cluster Loki' }
        )) {
        $run = Invoke-LokiDryRun -Arguments ($InfraArgs + $case.Args) -Env $case.Env
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "$($case.Label) 要拒绝。输出: $($run.Output)"
        Assert-Match -Text $run.Output -Pattern $case.Pattern -Because "$($case.Label):点名原因"
        Assert-NotMatch -Text $run.Output -Pattern '--- BEGIN MANIFEST ---' -Because "$($case.Label):在写路径入口拒绝"
    }
}

Test-Case '两份清单的对外名字一致:Service loki:3100 与 ConfigMap loki-config,同一时刻只 apply 一份' {
    $root = Get-RepoRoot
    foreach ($file in @('loki.yaml', 'loki-ha.yaml')) {
        $text = [IO.File]::ReadAllText((Join-Path $root "deploy/k8s/manifests/infra/$file"))
        Assert-Match -Text $text -Pattern '(?m)^  name: loki\s*$' -Because "$file 的 Service 名是 loki"
        Assert-Match -Text $text -Pattern 'port: 3100' -Because "$file 的端口是 3100"
        Assert-Match -Text $text -Pattern '(?m)^  name: loki-config\s*$' -Because "$file 的 ConfigMap 名是 loki-config"
    }
    $deploy = [IO.File]::ReadAllText((Join-Path (Get-ToolsScriptsDir) 'k8s_deploy.ps1'))
    Assert-Match -Text $deploy -Pattern '\$infraManifests \+= \$lokiTopology\.Manifest' -Because 'Apply-Infra 按形态只选一份'
    Assert-NotMatch -Text $deploy -Pattern '\$infraManifests \+= "loki\.yaml"' -Because '不再无条件追加单副本那份'
}

exit (Complete-TestRun -SuiteName 'in-cluster Loki deployment contract')
