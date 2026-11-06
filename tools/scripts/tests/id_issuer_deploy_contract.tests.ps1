<#
.SYNOPSIS
    发号器部署面的静态契约:第二套 etcd 的清单、部署脚本的接线,以及 biz_tag 清单三处一致。

.DESCRIPTION
    设计:docs/design/player-data-tidb-scaling-plan.md §6.3;运维:docs/ops/id-issuer-runbook.md。

    这里钉的都是「错了不会报错、出事才发现」的东西:
      - 第二套 etcd 的标签如果写成 app: etcd,会混进主 etcd 的 Service,等于脑裂;
      - ConfigMap 里 SecondaryEtcd 如果漏了,生产发号器装配失败;
      - biz_tag 清单漏了一处,新种类在那个环境发号被拒。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/id_issuer_deploy_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

# 路径一律正斜杠 + Join-Path:CI 在 ubuntu 上跑。
$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:SecondaryPath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/infra/etcd-id-secondary.yaml'
$script:RegistryPath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/infra/etcd-id-registry.yaml'
$script:DeployPath = Join-Path $script:RepoRoot 'tools/scripts/k8s_deploy.ps1'
$script:ServiceYamlPath = Join-Path $script:RepoRoot 'go/data_service/etc/data_service.yaml'
$script:ConfigGoPath = Join-Path $script:RepoRoot 'go/data_service/internal/config/config.go'
$script:MatchYamlPath = Join-Path $script:RepoRoot 'go/match/etc/match_service.yaml'
$script:MatchManifestPath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/go-svc/match.yaml'

function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$Path)
    return ([IO.File]::ReadAllText($Path)) -replace "`r`n", "`n"
}

# 去掉注释行再断言:注释里提到 app: etcd 是在解释为什么不能这么写。
function Get-NonCommentText {
    param([Parameter(Mandatory = $true)][string]$Text)
    return (($Text -split "`n") | Where-Object { $_ -notmatch '^\s*#' }) -join "`n"
}

function Get-TagSet {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Pattern)
    $match = [regex]::Match($Text, $Pattern)
    Assert-True -Condition $match.Success -Because "找不到 biz_tag 清单: $Pattern"
    return (@($match.Groups[1].Value -split ',' | ForEach-Object { $_.Trim(' ', '"') } | Where-Object { $_ } | Sort-Object) -join ',')
}

Test-Case '第二套 etcd 的名字和标签与主 etcd 完全分开' {
    $text = Get-NonCommentText (Read-RepoText $script:SecondaryPath)
    $plainLabels = [regex]::Matches($text, '(?m)^\s*app:\s*etcd\s*$').Count
    Assert-Equal -Expected 1 -Actual $plainLabels -Because '只有反亲和那一处可以写 app: etcd;写在别处会混进主 etcd 的 Service 和 PDB'
    Assert-Match -Text $text -Pattern '(?s)podAntiAffinity:.*matchLabels:\s*\n\s*app:\s*etcd\s*\n' -Because '那一处必须在反亲和里'
    $names = @([regex]::Matches($text, '(?m)^  name:\s*(\S+)\s*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'etcd-id-secondary,etcd-id-secondary-headless' -Actual ($names -join ',') -Because '对象名不能和主 etcd 重名'
}

Test-Case '第二套 etcd 是独立引导的 3 成员集群' {
    $text = Get-NonCommentText (Read-RepoText $script:SecondaryPath)
    Assert-Match -Text $text -Pattern '(?m)^  replicas:\s*3\s*$' -Because '3 成员才能容忍单节点故障'
    Assert-Match -Text $text -Pattern 'value:\s*"mmorpg-etcd-id-secondary-__INFRA_NAMESPACE__"' -Because '引导令牌要和主 etcd 不同,防止误把对方当成员'
    Assert-Match -Text $text -Pattern 'value:\s*"etcd-id-secondary-0=http://etcd-id-secondary-0\.etcd-id-secondary-headless\.' -Because '成员表必须指向自己的 headless Service'
    Assert-Match -Text $text -Pattern '(?m)^\s*publishNotReadyAddresses:\s*true\s*$' -Because '不发布未就绪地址,成员永远互相找不到'
}

Test-Case '部署脚本只在非 dev 档起第二套 etcd,并把两套地址写进 data-service ConfigMap' {
    $text = Read-RepoText $script:DeployPath
    Assert-Match -Text $text -Pattern "(?s)if \(\`$ReleaseProfile -ne 'dev'\) \{\s*\`$infraManifests \+= `"etcd-id-secondary\.yaml`"" -Because 'dev 档两份记录放同一套 etcd,不多起 3 个 Pod'
    Assert-Match -Text $text -Pattern "(?s)if \(\`$ReleaseProfile -ne 'dev'\) \{\s*Wait-ForStatefulSetReady -Namespace \`$InfraNamespace -StatefulSetName `"etcd-id-secondary`"" -Because '-WaitReady 要等它成 quorum'
    Assert-Match -Text $text -Pattern "\`$dataServiceModeLine = if \(\`$SvcName -eq 'data-service' -and \`$ReleaseProfile -eq 'dev'\) \{ 'Mode: dev' \}" -Because '只有 dev 档能把两份记录放同一套 etcd,staging、prod 不写 Mode'
    # 脚本里引号带 PowerShell 的反引号转义。
    Assert-Match -Text $text -Pattern 'PrimaryEtcd: \[`?"etcd\.\$\{InfraNamespace\}:2379`?"\]' -Because '主记录放现有 etcd'
    Assert-Match -Text $text -Pattern 'SecondaryEtcd: \[`?"etcd-id-secondary\.\$\{InfraNamespace\}:2379`?"\]' -Because '副记录放独立 etcd,漏了生产发号器装配失败'
}

Test-Case '登记处 etcd 的名字和标签与两套发号 etcd 完全分开,反亲和同时避开它们' {
    $text = Get-NonCommentText (Read-RepoText $script:RegistryPath)
    $names = @([regex]::Matches($text, '(?m)^  name:\s*(\S+)\s*$') | ForEach-Object { $_.Groups[1].Value } | Sort-Object -Unique)
    Assert-Equal -Expected 'etcd-id-registry,etcd-id-registry-headless' -Actual ($names -join ',') -Because '对象名不能和 etcd、etcd-id-secondary 重名'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^\s*app:\s*etcd\s*$').Count) -Because 'app: etcd 只能出现在反亲和里,否则混进主 etcd 的 Service 和 PDB'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($text, '(?m)^\s*app:\s*etcd-id-secondary\s*$').Count) -Because 'app: etcd-id-secondary 只能出现在反亲和里'
    Assert-Match -Text $text -Pattern '(?s)podAntiAffinity:.*app:\s*etcd\s*\n.*app:\s*etcd-id-secondary\s*\n' -Because '反亲和避开本集群两套发号 etcd'
    Assert-Match -Text $text -Pattern '(?m)^  replicas:\s*3\s*$' -Because '3 成员才能容忍单节点故障'
    Assert-Match -Text $text -Pattern 'value:\s*"mmorpg-etcd-id-registry-__INFRA_NAMESPACE__"' -Because '引导令牌和两套发号 etcd 都不同'
    Assert-Match -Text $text -Pattern 'value:\s*"etcd-id-registry-0=http://etcd-id-registry-0\.etcd-id-registry-headless\.' -Because '成员表指向自己的 headless Service'
    Assert-Match -Text $text -Pattern '(?m)^\s*publishNotReadyAddresses:\s*true\s*$' -Because '不发布未就绪地址,成员永远互相找不到'
    Assert-Equal -Expected (Get-NonCommentText (Read-RepoText $script:SecondaryPath) | Select-String -Pattern '(?m)^\s*image:\s*(\S+)' | ForEach-Object { $_.Matches[0].Groups[1].Value }) `
        -Actual ($text | Select-String -Pattern '(?m)^\s*image:\s*(\S+)' | ForEach-Object { $_.Matches[0].Groups[1].Value }) -Because '和另外两套 etcd 同一镜像,钉版本一起改'
}

Test-Case '部署脚本:登记处只在 -HostIdRegistry 的集群起,地址写进 data-service ConfigMap,入口先解析' {
    $text = Read-RepoText $script:DeployPath
    Assert-Match -Text $text -Pattern '(?s)if \(\$HostIdRegistry\) \{\s*if \(\$ReleaseProfile -eq ''dev''\) \{\s*throw[^\n]*\n\s*\}\s*\$infraManifests \+= "etcd-id-registry\.yaml"' -Because '全世界只有一个集群托管登记处,dev 不起'
    Assert-Match -Text $text -Pattern '(?s)if \(\$HostIdRegistry\) \{\s*Wait-ForStatefulSetReady -Namespace \$InfraNamespace -StatefulSetName "etcd-id-registry"' -Because '-WaitReady 要等它成 quorum'
    Assert-Match -Text $text -Pattern 'RegistryEtcd: \[\$registryList\]' -Because '登记处地址漏了,生产发号器装配失败'
    Assert-Match -Text $text -Pattern '(?s)if \(\$deploysDataService\) \{\s*#[^\n]*\n\s*\$registry = @\(Resolve-IdRegistryEtcd\)' -Because '入口在任何写操作之前解析登记处地址'
    $devTools = Read-RepoText (Join-Path $script:RepoRoot 'tools/scripts/dev_tools.ps1')
    Assert-Match -Text $devTools -Pattern '\$args\.IdRegistryEtcd = \$IdRegistryEtcd' -Because 'dev_tools 转发 -IdRegistryEtcd'
    Assert-Match -Text $devTools -Pattern 'if \(\$HostIdRegistry\) \{ \$args\.HostIdRegistry = \$true \}' -Because 'dev_tools 转发 -HostIdRegistry'
}

Test-Case '运维手册:初始化、恢复、退役、开启登记处是分开的入口,恢复不靠手算余量' {
    $runbook = Read-RepoText (Join-Path $script:RepoRoot 'docs/ops/id-issuer-runbook.md')
    foreach ($flag in @('-id-registry-open', '-id-issuer-init', '-id-issuer-recover', '-id-issuer-rebind', '-id-issuer-retire')) {
        Assert-Match -Text $runbook -Pattern ([regex]::Escape($flag)) -Because "手册要写 $flag 什么时候用"
    }
    Assert-NotMatch -Text $runbook -Pattern '30 \* 86400' -Because '恢复取幸存记录的上界,不再手算时间余量'
    Assert-NotMatch -Text $runbook -Pattern 'etcdctl put /id_issuer' -Because '不再手工改记录'
}

Test-Case '-Region 注入 data-service 主容器,初始化命令用私有目录里生效的配置' {
    $deploy = Read-RepoText $script:DeployPath
    Assert-Match -Text $deploy -Pattern 'MMORPG_ID_ISSUER_EXPECTED_REGION = \[string\]\(Resolve-IdIssuerRegion\)' -Because '值来自 -Region,所有档位都写'
    Assert-Match -Text $deploy -Pattern "\`$SvcName -eq 'data-service'\) \{\s+if \(\`$Stage -ne 'Prepare'\) \{ Assert-DataServiceRosterSchemas -Namespace \`$Namespace \}\s+\`$manifestContent = Add-IdIssuerExpectedRegionEnv" -Because '只给 data-service'
    Assert-NotMatch -Text (Read-RepoText (Join-Path $script:RepoRoot 'deploy/k8s/manifests/go-svc/data-service.yaml')) -Pattern 'MMORPG_ID_ISSUER_EXPECTED_REGION' -Because '由脚本注入,清单手写就是第二份真相'
    foreach ($runbook in @('docs/ops/id-issuer-runbook.md', 'docs/ops/open-new-region-runbook.md', 'docs/ops/k8s-open-server-runbook.md')) {
        $text = Read-RepoText (Join-Path $script:RepoRoot $runbook)
        Assert-Match -Text $text -Pattern '/app/etc/private/data_service\.yaml -id-issuer-init' -Because "$runbook 的命令要读写者启动目录选中的那份配置"
        Assert-Match -Text $text -Pattern '就绪之后、放流量之前' -Because "$runbook 写明什么时候做"
    }
}

Test-Case 'biz_tag 清单在代码默认值、服务 yaml、K8s ConfigMap 三处一致' {
    $code = Get-TagSet -Text (Read-RepoText $script:ConfigGoPath) -Pattern 'DefaultIdSegmentTags = \[\]string\{([^}]*)\}'
    $yaml = Get-TagSet -Text (Read-RepoText $script:ServiceYamlPath) -Pattern '(?m)^  Tags: \[([^\]]*)\]'
    $deploy = Get-TagSet -Text (Read-RepoText $script:DeployPath) -Pattern '(?m)^  Tags: \[([^\]]*)\]'
    Assert-Equal -Expected $code -Actual $yaml -Because 'etc/data_service.yaml 的 IdSegment.Tags 要与 DefaultIdSegmentTags 相同'
    Assert-Equal -Expected $code -Actual $deploy -Because 'k8s_deploy.ps1 的 IdSegment.Tags 要与 DefaultIdSegmentTags 相同'
    foreach ($tag in @('battle', 'snapshot_row', 'rollback_audit')) {
        Assert-True -Condition (@($code -split '\s*,\s*') -contains $tag) -Because "$tag 不在清单里,发号器会拒绝它"
    }
}

Test-Case 'match 走号段发 battle_id,ConfigMap 取自服务 yaml,不再申领 snowflake 槽位' {
    $deploy = Read-RepoText $script:DeployPath
    Assert-Match -Text (Read-RepoText $script:MatchYamlPath) -Pattern '(?m)^IdSegment:\s*\n  Enabled: true\s*$' -Because '没写 Enabled: true,match 起服即拒绝'
    Assert-Match -Text $deploy -Pattern "Get-AuthoritativeYamlBlock -RelativePath \`$matchYaml -Key 'IdSegment'" -Because 'K8s 的号段配置要和服务 yaml 同源'
    Assert-Match -Text $deploy -Pattern '(?m)^\$\{matchIdSegmentBlock\}\s*$' -Because '漏了这一行,K8s 上 match 起不来'
    Assert-NotMatch -Text (Read-RepoText $script:MatchManifestPath) -Pattern 'snowflake' -Because 'match 不再申领 snowflake 槽位,不需要缓存卷'
}

exit (Complete-TestRun -SuiteName 'id_issuer_deploy_contract')
