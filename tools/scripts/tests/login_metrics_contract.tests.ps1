#requires -Version 7
<#
.SYNOPSIS
    K8s 上 login 指标端点的契约测试(L-18 观测批,2026-10-23)。
.DESCRIPTION
    login 的 go-zero 指标(rpc_client_requests_*、login_gate_directory_reads_total、login_task_result_delivery_total 等)
    只有在 ConfigMap 写了 Prometheus 段时才会被记录与导出;集群按 Pod 模板的 prometheus.io/* 注解抓取,
    容器还要声明同一个端口。三处必须一致、同批发布,任一处漏改都会让 L-14 压测里 login 侧整张表变成 n/a。
    本测试只读文本(tools/scripts/k8s_deploy.ps1 的 login ConfigMap 分支与 Pod 注解写入段、
    deploy/k8s/manifests/go-svc/login.yaml),不执行部署入口,不访问任何集群。
.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/login_metrics_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

$repoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$deployText = [IO.File]::ReadAllText((Join-Path $repoRoot 'tools/scripts/k8s_deploy.ps1'))
$manifestText = [IO.File]::ReadAllText((Join-Path $repoRoot 'deploy/k8s/manifests/go-svc/login.yaml'))

# New-GoSvcConfigMapYaml 的 login 分支:从 `"login" {` 截到下一个服务分支 `"player-locator" {`。
# 不假设分支里只有一个 here-string,只在这一段文本里找。
function Get-LoginConfigMapBranch {
    $m = [regex]::Match($deployText, '(?s)\n\t\t"login"\s*\{(?<body>.*?)\n\t\t"player-locator"\s*\{')
    if (-not $m.Success) { throw '找不到 New-GoSvcConfigMapYaml 的 login 分支(或它后面不再紧跟 player-locator 分支)' }
    return $m.Groups['body'].Value
}

Test-Case 'login ConfigMap 写了 Prometheus 段:Host 0.0.0.0、Port 9101、Path /metrics' {
    $branch = Get-LoginConfigMapBranch
    $prom = [regex]::Match($branch, '(?m)^Prometheus:[ ]*\r?\n  Host:[ ]*(?<host>\S+)[ ]*\r?\n  Port:[ ]*(?<port>\d+)[ ]*\r?\n  Path:[ ]*(?<path>\S+)[ ]*\r?$')
    Assert-True -Condition $prom.Success -Because 'login ConfigMap 必须有 Prometheus 段(没有它 go-zero 不起 agent,指标样本被直接丢弃)'
    Assert-Equal -Expected '0.0.0.0' -Actual $prom.Groups['host'].Value -Because 'Host 为空时 StartAgent 直接 return;写 127.0.0.1 集群也抓不到'
    Assert-Equal -Expected '9101' -Actual $prom.Groups['port'].Value -Because '与本机 go/login/etc/login.yaml 和 stress_snap.ps1 的 login 端口同为 9101'
    Assert-Equal -Expected '/metrics' -Actual $prom.Groups['path'].Value -Because '抓取注解的 path 是 /metrics'
    Assert-Equal -Expected 1 -Actual ([regex]::Matches($branch, '(?m)^Prometheus:').Count) -Because 'login 分支里 Prometheus 段只能有一处'
}

Test-Case 'login manifest 声明 metrics 容器端口 9101,且 Pod 模板不手写 annotations' {
    Assert-Match -Text $manifestText -Pattern '(?m)^[ ]+- containerPort: 9101[ ]*\r?\n[ ]+name: metrics[ ]*\r?$' -Because '容器端口要与 ConfigMap 的 Prometheus.Port 一致'
    Assert-NotMatch -Text $manifestText -Pattern '(?m)^[ ]+annotations:' -Because 'Pod 模板注解只由 k8s_deploy.ps1 的 Add-PodTemplateAnnotation 一次写入,手写会让它拒绝部署'
}

Test-Case 'k8s_deploy 给 login Pod 模板恒写 prometheus.io/* 注解,端口同为 9101' {
    $scrape = [regex]::Matches($deployText, "\`$loginAnnotations\['prometheus\.io/scrape'\]\s*=\s*'true'")
    $port = [regex]::Matches($deployText, "\`$loginAnnotations\['prometheus\.io/port'\]\s*=\s*'(?<port>\d+)'")
    $path = [regex]::Matches($deployText, "\`$loginAnnotations\['prometheus\.io/path'\]\s*=\s*'(?<path>[^']+)'")
    Assert-Equal -Expected 1 -Actual $scrape.Count -Because 'prometheus.io/scrape 注解写入点只能有一处'
    Assert-Equal -Expected 1 -Actual $port.Count -Because 'prometheus.io/port 注解写入点只能有一处'
    Assert-Equal -Expected 1 -Actual $path.Count -Because 'prometheus.io/path 注解写入点只能有一处'
    Assert-Equal -Expected '9101' -Actual $port[0].Groups['port'].Value -Because '注解端口要与 ConfigMap 的 Prometheus.Port、容器端口一致'
    Assert-Equal -Expected '/metrics' -Actual $path[0].Groups['path'].Value -Because '注解 path 要与 ConfigMap 的 Prometheus.Path 一致'
    # 恒写:不能被排队 / 微信这类开关的 if 包住。注解在 `if ($SvcName -eq 'login') {` 块的顶层,
    # 排在削峰与微信指纹之后(那两条测试按「annotations: 的下一行」取指纹,不能被抢第一行)。
    $block = [regex]::Match($deployText, "(?s)if \(\`$SvcName -eq 'login'\) \{\s*\n\s*\`$loginAnnotations = \[ordered\]@\{\}(?<body>.*?)Add-PodTemplateAnnotation")
    Assert-True -Condition $block.Success -Because '找不到 login 的 Pod 注解写入块'
    $body = $block.Groups['body'].Value
    $promAt = $body.IndexOf("`$loginAnnotations['prometheus.io/scrape']")
    Assert-True -Condition ($promAt -gt $body.LastIndexOf('PeakShavingHashAnnotation') -and $promAt -gt $body.LastIndexOf('LoginOAuthSecretHashAnnotation')) -Because '抓取注解排在两条指纹之后'
    $lineStart = $body.LastIndexOf("`n", $promAt) + 1
    Assert-Equal -Expected "`t`t" -Actual $body.Substring($lineStart, $promAt - $lineStart) -Because '抓取注解在 login 块的顶层(两个制表符缩进),不在任何开关分支里'
}

exit (Complete-TestRun -SuiteName 'login_metrics_contract')
