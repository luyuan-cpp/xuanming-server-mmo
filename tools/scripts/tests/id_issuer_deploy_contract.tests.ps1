<#
.SYNOPSIS
    发号器部署契约:etcd 清单、部署接线、入口拒绝顺序,以及 biz_tag 清单三处一致。

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
. "$PSScriptRoot/lib/deploy_capture.ps1"

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

# k8s_deploy.ps1 的顶层函数按 AST 解析一次,只抽函数、不跑脚本入口(与 k8s_migrate_gate.tests.ps1 同一做法)。
$script:DeployFunctions = @{}
foreach ($node in ([System.Management.Automation.Language.Parser]::ParseFile($script:DeployPath, [ref]$null, [ref]$null)).FindAll({ param($a) $a -is [System.Management.Automation.Language.FunctionDefinitionAst] }, $false)) {
    $script:DeployFunctions[$node.Name] = $node
}

function Import-DeployFunction {
    param([Parameter(Mandatory = $true)][string[]]$Name)
    foreach ($n in $Name) {
        if (-not $script:DeployFunctions.ContainsKey($n)) { throw "夹具前提:k8s_deploy.ps1 里没有函数 $n" }
        Set-Item -Path "Function:script:$n" -Value $script:DeployFunctions[$n].Body.GetScriptBlock()
    }
}

function Get-ThrownMessage {
    param([Parameter(Mandatory = $true)][scriptblock]$Action)
    try { & $Action | Out-Null } catch { return $_.Exception.Message }
    return ''
}

function Get-TagSet {
    param([Parameter(Mandatory = $true)][string]$Text, [Parameter(Mandatory = $true)][string]$Pattern)
    $match = [regex]::Match($Text, $Pattern)
    Assert-True -Condition $match.Success -Because "找不到 biz_tag 清单: $Pattern"
    return (@($match.Groups[1].Value -split ',' | ForEach-Object { $_.Trim(' ', '"') } | Where-Object { $_ } | Sort-Object) -join ',')
}

Test-Case 'dev 托管登记处在 infra-up / all-up 的任何写计划之前拒绝,不依赖部署 data-service' {
    foreach ($command in @('infra-up', 'all-up')) {
        # 正式脚本入口仍走完整前置校验;DryRun 把原本的写操作变成计划,不能让 Namespace 创建先于拒绝。
        $run = Invoke-DeployDryRun -Arguments @('-Command', $command, '-DryRun', '-SkipPreflight', '-SkipGoSvc', '-SkipJavaSvc',
            '-BattleReplicas', '0', '-NoCppLogSidecar', '-HostIdRegistry')
        Assert-True -Condition ($run.ExitCode -ne 0) -Because "$command 的 dev 档必须拒绝托管登记处"
        Assert-Match -Text $run.Output -Pattern 'HostIdRegistry.*staging and prod only' -Because "$command 必须因登记处档位不合法而拒绝"
        Assert-NotMatch -Text $run.Output -Pattern '\[dry-run\] kubectl |--- BEGIN MANIFEST ---' -Because "$command 拒绝前不能到达任何 kubectl 或清单写计划"
    }
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
    Assert-Match -Text $text -Pattern 'value:\s*"mmorpg-etcd-id-secondary-__INFRA_NAMESPACE__-__KUBE_CLUSTER_UID__"' -Because '引导令牌要和主 etcd 不同,防止误把对方当成员;还要带本 K8s 集群的身份,否则两个集群里的这套 etcd 集群号相同'
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
    Assert-Match -Text $text -Pattern 'value:\s*"mmorpg-etcd-id-registry-__INFRA_NAMESPACE__-__KUBE_CLUSTER_UID__"' -Because '引导令牌和两套发号 etcd 都不同,并带托管它的 K8s 集群的身份'
    Assert-Match -Text $text -Pattern 'value:\s*"etcd-id-registry-0=http://etcd-id-registry-0\.etcd-id-registry-headless\.' -Because '成员表指向自己的 headless Service'
    Assert-Match -Text $text -Pattern '(?m)^\s*publishNotReadyAddresses:\s*true\s*$' -Because '不发布未就绪地址,成员永远互相找不到'
    Assert-Equal -Expected (Get-NonCommentText (Read-RepoText $script:SecondaryPath) | Select-String -Pattern '(?m)^\s*image:\s*(\S+)' | ForEach-Object { $_.Matches[0].Groups[1].Value }) `
        -Actual ($text | Select-String -Pattern '(?m)^\s*image:\s*(\S+)' | ForEach-Object { $_.Matches[0].Groups[1].Value }) -Because '和另外两套 etcd 同一镜像,钉版本一起改'
}

Test-Case '部署脚本:登记处只在 -HostIdRegistry 的集群起,地址写进 data-service ConfigMap,入口先解析' {
    $text = Read-RepoText $script:DeployPath
    Assert-Match -Text $text -Pattern '(?s)if \(\$HostIdRegistry\) \{\s*\$infraManifests \+= "etcd-id-registry\.yaml"' -Because '只有显式托管登记处的集群才加入清单,dev 拒绝由真实入口回归验证'
    Assert-Match -Text $text -Pattern '(?s)if \(\$HostIdRegistry\) \{\s*Wait-ForStatefulSetReady -Namespace \$InfraNamespace -StatefulSetName "etcd-id-registry"' -Because '-WaitReady 要等它成 quorum'
    Assert-Match -Text $text -Pattern 'RegistryEtcd: \[\$registryList\]' -Because '登记处地址漏了,生产发号器装配失败'
    Assert-Match -Text $text -Pattern '(?s)if \(\$deploysDataService -or \$HostIdRegistry -or -not \[string\]::IsNullOrWhiteSpace\(\$IdRegistryEtcd\)\) \{\s*\$registry = @\(Resolve-IdRegistryEtcd\)' -Because '部署 data-service 或显式给登记处参数时,都在写路径入口解析'
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

# ── 地域码绑定的部署侧(docs/handoff/global-id-issuer-registry-20261105.md §8.4)────────────────────────────
# 发号器把地域码绑定到「部署身份 + 两套本地 etcd 的集群号」。部署身份是 kube-system 的 UID,data-service 运行期用自己的
# ServiceAccount 读;etcd 的集群号只由 peer 地址和引导令牌算出,所以三套 etcd 的令牌也带上这个 UID。这里钉住部署侧的形状,
# 真正渲染出来的样子由 k8s_deploy_contract.tests.ps1 末尾两条 DryRun 用例核对。

$script:RbacPath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/go-svc/data-service-identity-rbac.yaml'
$script:DataServiceManifestPath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/go-svc/data-service.yaml'
$script:GoSvcManifestsDirFixture = Join-Path $script:RepoRoot 'deploy/k8s/manifests/go-svc'

Test-Case '三套 etcd 的引导令牌都带命名空间和本 K8s 集群的身份' {
    foreach ($path in @('deploy/k8s/manifests/infra/etcd.yaml', 'deploy/k8s/manifests/infra/etcd-id-secondary.yaml', 'deploy/k8s/manifests/infra/etcd-id-registry.yaml')) {
        $text = Get-NonCommentText (Read-RepoText (Join-Path $script:RepoRoot $path))
        $tokens = @([regex]::Matches($text, 'name: ETCD_INITIAL_CLUSTER_TOKEN\s*\n\s*value:\s*"([^"]+)"') | ForEach-Object { $_.Groups[1].Value })
        Assert-Equal -Expected 1 -Actual $tokens.Count -Because "$path 恰好一个引导令牌"
        Assert-Match -Text $tokens[0] -Pattern '^mmorpg-etcd(-id-secondary|-id-registry)?-__INFRA_NAMESPACE__-__KUBE_CLUSTER_UID__$' `
            -Because "$path 的令牌只带命名空间的话,同一份清单部署到两个 K8s 集群,etcd 集群号相同,地域码绑定分不出两套环境"
    }
}

Test-Case 'Apply-Infra 在第一个写操作之前读集群 UID,循环里替换 __KUBE_CLUSTER_UID__' {
    $applyInfra = $script:DeployFunctions['Apply-Infra']
    Assert-True -Condition ($null -ne $applyInfra) -Because '夹具前提:k8s_deploy.ps1 里有 Apply-Infra'
    $commands = @($applyInfra.Body.FindAll({ param($a) $a -is [System.Management.Automation.Language.CommandAst] }, $true))
    $uidRead = @($commands | Where-Object { $_.GetCommandName() -eq 'Get-KubeClusterUid' } | Sort-Object { $_.Extent.StartOffset })
    $ensureNs = @($commands | Where-Object { $_.GetCommandName() -eq 'Ensure-Namespace' } | Sort-Object { $_.Extent.StartOffset })
    Assert-Equal -Expected 1 -Actual $uidRead.Count -Because 'Apply-Infra 只读一次集群 UID'
    Assert-True -Condition ($ensureNs.Count -ge 1 -and $uidRead[0].Extent.StartOffset -lt $ensureNs[0].Extent.StartOffset) `
        -Because '读不到要在第一个写操作(Ensure-Namespace)之前拒绝,什么都不写'
    Assert-Match -Text $applyInfra.Extent.Text -Pattern '\$manifestContent = \$manifestContent\.Replace\("__KUBE_CLUSTER_UID__", \$kubeClusterUid\)' `
        -Because '三套 etcd 的令牌在 apply 之前替换成读到的 UID'
}

Test-Case 'Get-KubeClusterUid:只认 API server 给的小写 UUID,读不到或格式不对就拒绝,DryRun 给固定标记' {
    Import-DeployFunction -Name @('Get-KubeClusterUid', 'Build-KubectlBaseArgs')
    $script:KubeContext = 'contract-ctx'
    $script:KubeConfig = ''
    $script:StubCalls = [System.Collections.Generic.List[string]]::new()
    # 同名函数优先于外部命令:& kubectl 落到这个桩,不碰真实集群。
    Set-Item -Path 'Function:script:kubectl' -Value {
        $script:StubCalls.Add(($args -join ' '))
        $global:LASTEXITCODE = $script:StubExitCode
        return $script:StubOutput
    }
    try {
        $script:DryRun = $false
        $script:StubExitCode = 0
        $script:StubOutput = '1b4e28ba-2fa1-11d2-883f-0016d3cca427'
        Assert-Equal -Expected '1b4e28ba-2fa1-11d2-883f-0016d3cca427' -Actual (Get-KubeClusterUid) -Because '读到合法 UID 原样返回'
        Assert-Equal -Expected '--context contract-ctx get namespace kube-system -o jsonpath={.metadata.uid}' -Actual $script:StubCalls[0] `
            -Because '读 kube-system 的 metadata.uid,且发到部署目标的 context'
        foreach ($case in @(
                @{ Exit = 1; Output = ''; Why = 'kubectl 失败(没有权限、连不上集群)' }
                @{ Exit = 0; Output = ''; Why = '输出为空' }
                @{ Exit = 0; Output = '1B4E28BA-2FA1-11D2-883F-0016D3CCA427'; Why = '大写不是 API server 给的格式' }
                @{ Exit = 0; Output = 'not-a-uid'; Why = '不是 UUID' }
            )) {
            $script:StubExitCode = $case.Exit
            $script:StubOutput = $case.Output
            $message = Get-ThrownMessage { Get-KubeClusterUid }
            Assert-Match -Text $message -Pattern 'kube-system' -Because "$($case.Why)时必须拒绝,报错点名读的是什么"
            Assert-Match -Text $message -Pattern 'Nothing was written\.$' -Because "$($case.Why)时必须写明什么都没写"
        }
        $script:DryRun = $true
        $script:StubCalls.Clear()
        Assert-Equal -Expected '00000000-0000-0000-0000-000000000000' -Actual (Get-KubeClusterUid) -Because 'DryRun 不连集群,给固定标记值'
        Assert-Equal -Expected 0 -Actual $script:StubCalls.Count -Because 'DryRun 一次 kubectl 都不调'
    }
    finally {
        Remove-Item -Path 'Function:script:kubectl' -ErrorAction SilentlyContinue
        $script:DryRun = $false
    }
}

Test-Case 'data-service 读集群身份的 RBAC:专用 ServiceAccount,只读 namespaces/kube-system,每个 zone 一个绑定' {
    $text = Get-NonCommentText (Read-RepoText $script:RbacPath)
    $kinds = @([regex]::Matches($text, '(?m)^kind:\s*(\S+)\s*$') | ForEach-Object { $_.Groups[1].Value })
    Assert-Equal -Expected 'ServiceAccount,ClusterRole,ClusterRoleBinding' -Actual ($kinds -join ',') -Because '只有这三个对象'
    Assert-Match -Text $text -Pattern 'kind: ServiceAccount\s*\nmetadata:\s*\n  name: data-service\s*\n' -Because '用专用 ServiceAccount,不把权限挂到命名空间的 default 上'
    $role = [regex]::Match($text, '(?s)kind: ClusterRole\s*\n(.*?)(?=\n---|\z)').Groups[1].Value
    Assert-Match -Text $role -Pattern '(?m)^  name: mmorpg-cluster-identity-reader\s*$' -Because 'ClusterRole 全集群一份,名字固定'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($role, '(?m)^  - apiGroups:').Count) -Because '只有一条规则'
    Assert-Match -Text $role -Pattern 'apiGroups: \[""\]\s*\n\s*resources: \["namespaces"\]\s*\n\s*resourceNames: \["kube-system"\]\s*\n\s*verbs: \["get"\]' `
        -Because '只给 get,只限 kube-system 一个命名空间,不给 list / watch'
    $binding = [regex]::Match($text, '(?s)kind: ClusterRoleBinding\s*\n(.*)').Groups[1].Value
    Assert-Match -Text $binding -Pattern '(?m)^  name: mmorpg-cluster-identity-__ZONE_NAMESPACE__\s*$' -Because '每个 zone 一个绑定,名字带命名空间,Remove-Zone 按同一个名字删'
    Assert-Match -Text $binding -Pattern '(?s)roleRef:.*?kind: ClusterRole\s*\n\s*name: mmorpg-cluster-identity-reader' -Because '绑定的是上面那个只读角色'
    Assert-Match -Text $binding -Pattern 'subjects:\s*\n\s*- kind: ServiceAccount\s*\n\s*name: data-service\s*\n\s*namespace: __ZONE_NAMESPACE__' `
        -Because 'subject 写明命名空间,只授权本 zone 的 data-service'

    $manifest = Get-NonCommentText (Read-RepoText $script:DataServiceManifestPath)
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($manifest, '(?m)^      serviceAccountName: data-service\s*$').Count) -Because 'Pod 模板恰好一处用 data-service 身份'
    Assert-NotMatch -Text $manifest -Pattern 'automountServiceAccountToken:\s*false' -Because 'ServiceAccount 令牌就是读集群身份的凭据,不能关'
}

Test-Case 'New-DataServiceIdentityRbacYaml:按 zone 命名空间渲染,占位没换干净就在写操作之前拒绝' {
    Import-DeployFunction -Name @('New-DataServiceIdentityRbacYaml')
    $script:GoSvcManifestsDir = $script:GoSvcManifestsDirFixture
    $rendered = New-DataServiceIdentityRbacYaml -Namespace 'mmorpg-zone-z1'
    Assert-Match -Text $rendered -Pattern '(?m)^  name: mmorpg-cluster-identity-mmorpg-zone-z1\s*$' -Because '绑定名带上本 zone 命名空间'
    Assert-Match -Text $rendered -Pattern '(?m)^    namespace: mmorpg-zone-z1\s*$' -Because 'subject 指向本 zone 命名空间'
    Assert-NotMatch -Text $rendered -Pattern '(?-i)__[A-Z][A-Z0-9_]*__' -Because '占位全部替换'

    $broken = Join-Path ([IO.Path]::GetTempPath()) ('idissuer-rbac-' + [Guid]::NewGuid().ToString('N'))
    $null = New-Item -ItemType Directory -Path $broken
    try {
        [IO.File]::WriteAllText((Join-Path $broken 'data-service-identity-rbac.yaml'), "kind: ServiceAccount`nmetadata:`n  name: __OTHER__`n")
        $script:GoSvcManifestsDir = $broken
        Assert-Match -Text (Get-ThrownMessage { New-DataServiceIdentityRbacYaml -Namespace 'mmorpg-zone-z1' }) -Pattern 'placeholder __OTHER__.*Nothing was written\.' `
            -Because '清单多了没人替换的占位,要在任何写操作之前拒绝'
    }
    finally {
        Remove-Item -LiteralPath $broken -Recurse -Force
        $script:GoSvcManifestsDir = $script:GoSvcManifestsDirFixture
    }
}

Test-Case 'Apply-OneGoSvc 只给 data-service 渲染 RBAC,渲染在写操作之前,apply 先于工作负载;zone-down 删本 zone 的绑定' {
    $applyOne = $script:DeployFunctions['Apply-OneGoSvc'].Extent.Text
    Assert-Match -Text $applyOne -Pattern '(?s)if \(\$SvcName -eq ''data-service''\) \{\s*\$dataServiceIdentityRbac = New-DataServiceIdentityRbacYaml -Namespace \$Namespace\s*\}' `
        -Because '只给 data-service 渲染'
    $renderAt = $applyOne.IndexOf('New-DataServiceIdentityRbacYaml -Namespace')
    # 本函数里最早的写操作:工作负载密钥的 Secret 或 ConfigMap,以先出现的为准。
    $writeAts = @(@('Sync-WorkloadSecrets -Namespace', 'New-GoSvcConfigMapYaml') | ForEach-Object { $applyOne.IndexOf($_) } | Where-Object { $_ -ge 0 })
    Assert-True -Condition ($writeAts.Count -ge 1) -Because '夹具前提:Apply-OneGoSvc 里能找到写 Secret 或 ConfigMap 的地方'
    Assert-True -Condition ($renderAt -ge 0 -and $renderAt -lt ($writeAts | Measure-Object -Minimum).Minimum) -Because '渲染失败要在 Secret、ConfigMap 落地之前拦下,不留半截部署'
    $applyAt = $applyOne.IndexOf('-InputContent $dataServiceIdentityRbac')
    $canaryAt = $applyOne.IndexOf('if ($Canary) {', $renderAt)
    $workloadAt = $applyOne.LastIndexOf('Invoke-KubectlWithInputFile -Args @("apply", "-n", $Namespace) -InputContent $manifestContent')
    Assert-True -Condition ($applyAt -gt $renderAt -and $applyAt -lt $canaryAt -and $applyAt -lt $workloadAt) `
        -Because 'RBAC 先于 StatefulSet(稳定版与金丝雀都是),否则第一次预留读身份拿到 403'
    $removeZone = $script:DeployFunctions['Remove-Zone'].Extent.Text
    Assert-Match -Text $removeZone -Pattern 'Invoke-Kubectl -Args @\("delete", "clusterrolebinding", "mmorpg-cluster-identity-\$namespace", "--ignore-not-found"\) -AllowFailure' `
        -Because 'ClusterRoleBinding 是集群级对象,删命名空间带不走;名字与清单里的 mmorpg-cluster-identity-__ZONE_NAMESPACE__ 一致'
}

exit (Complete-TestRun -SuiteName 'id_issuer_deploy_contract')
