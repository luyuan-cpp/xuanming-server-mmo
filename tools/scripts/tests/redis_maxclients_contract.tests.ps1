#requires -Version 7
<#
.SYNOPSIS
    共享 Redis 连接上限(P0-E 第 1 批)的静态契约:redis.yaml 显式 maxclients、启动时打印 fd 限值,docker-compose 同值。

.DESCRIPTION
    只读仓库里的静态文件:不调用 k8s_deploy.ps1,不访问集群、容器或网络。
    能直接读清单的理由:Apply-Infra 对 redis.yaml 的 Pod 模板不做任何替换(只替换 __INFRA_NAMESPACE__,
    以及 StatefulSet 自己 metadata 上的模板摘要注解),Pod 模板的渲染结果就是文件本身。
    连接总账、重启闸、部署后核对与包装入口透传(第 2 批)要跑部署脚本,放在 k8s_deploy_contract.tests.ps1 的
    「Redis 连接总账与重启闸」一节。

    守住的东西:
      - T1 MAXCLIENTS 只定义一次,而且是正整数字面量(部署脚本的总账与本文件都从这一行读值);
      - T2 每一条 exec redis-server 都带 --maxclients "$MAXCLIENTS",且这样的行至少 2 条(有口令、无口令两个分支);
      - T3 启动薄壳打印 fd 限值(redis fd limit: 一行,部署后核对与排查都靠它);
      - T4 deploy/docker-compose.yml 的 redis 带 --maxclients,数值与 T1 相同;
      - T5 硬限不足只打 ERROR 不拒启(薄壳里没有 exit),不设 timeout,不自己改 nofile。
      - T6 高可用形态(redis-ha.yaml,2026-10-19 接进部署脚本):MAXCLIENTS 只在 StatefulSet redis 的 Pod 模板里定义一次、与 redis.yaml 同值,
        启动脚本 redis-start.sh 的 `set -- redis-server` 参数表带 --maxclients "$MAXCLIENTS",exec 之前打印同格式的 fd 限值;
      - T7 高可用形态的入口代理:PROXY_MAXCONN 只在 Deployment redis-proxy 的 Pod 模板里定义一次,haproxy 配置的 global / defaults
        两处 maxconn 都由它生成,不留字面量(代理先到线的不等式在 k8s_deploy_contract.tests.ps1 的高可用一节核)。
      - T8 match 私有集群(redis-match-cluster.yaml,2026-11-11):MAXCLIENTS 只定义一次、与 redis.yaml 同值,
        唯一一条 exec redis-server 带 --maxclients "$MAXCLIENTS",exec 之前打印同格式的 fd 限值,硬限不足只打 ERROR。
    修复前(redis.yaml 没有 MAXCLIENTS)T1~T4 以「应当恰好出现 1 次,实际 0 次」一类文本失败,退出码非 0。

    设计与取值:docs/handoff/global-singletons-p0-findings-20261014.md「## P0-E」;运维:deploy/k8s/README.md「Redis 连接上限与总账」。

.EXAMPLE
    pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1
#>

$ErrorActionPreference = 'Stop'
. "$PSScriptRoot/lib/test_harness.ps1"

# 路径一律正斜杠 + Join-Path:CI 在 ubuntu 上跑(.github/workflows/deploy-config-tests.yml)。
$script:RepoRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../..'))
$script:RedisPath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/infra/redis.yaml'
$script:RedisHaPath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/infra/redis-ha.yaml'
$script:RedisMatchClusterPath = Join-Path $script:RepoRoot 'deploy/k8s/manifests/infra/redis-match-cluster.yaml'
$script:ComposePath = Join-Path $script:RepoRoot 'deploy/docker-compose.yml'

# 读文件放在各用例里调用:文件缺失时每条用例各自报红,而不是整个脚本中断。
function Read-RepoText {
    param([Parameter(Mandatory = $true)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { throw "找不到 $Path" }
    return ([IO.File]::ReadAllText($Path) -replace "`r`n", "`n")
}

# 去掉整行注释(YAML 注释与 shell 注释都是去掉缩进后以 # 开头),只对真正生效的内容断言。
function Get-NonCommentText {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)
    return ((($Text -split "`n") | Where-Object { -not $_.TrimStart().StartsWith('#') }) -join "`n")
}

# 取 compose 里某个服务的块:从 `  <服务名>:` 到下一个缩进 ≤ 2 的非注释行(下一个服务或顶层的 volumes:)。
# 写法同 etcd_maintenance_contract.tests.ps1 的同名函数(两份测试文件各自独立运行,不互相 dot-source)。
function Get-ComposeServiceBlock {
    param(
        [Parameter(Mandatory = $true)][string]$Text,
        [Parameter(Mandatory = $true)][string]$Service
    )
    $lines = $Text -split "`n"
    $start = -1
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match ('^  ' + [regex]::Escape($Service) + ':\s*$')) { $start = $i; break }
    }
    if ($start -lt 0) { throw "compose 里找不到服务 $Service" }

    $body = [System.Collections.Generic.List[string]]::new()
    $body.Add($lines[$start])
    for ($i = $start + 1; $i -lt $lines.Count; $i++) {
        $l = $lines[$i]
        $isKeyLine = $l.Trim().Length -gt 0 -and -not $l.TrimStart().StartsWith('#')
        if ($isKeyLine -and ($l.Length - $l.TrimStart().Length) -le 2) { break }
        $body.Add($l)
    }
    return ($body -join "`n")
}

# redis.yaml 的启动薄壳(command 里 `- |` 字面块的正文,不含注释行)。探针里也有 exit / redis-cli,
# 只对启动薄壳断言的条目必须用它,不能扫整份清单。取法与修复设计第八节第 4 步交给 Codex 的取法一致:第一个 `- |` 之后、
# 缩进回到它同级或更浅之前的全部行。
function Get-RedisStartupScript {
    $lines = (Read-RepoText $script:RedisPath) -split "`n"
    $start = -1
    $indent = 0
    for ($i = 0; $i -lt $lines.Count; $i++) {
        $m = [regex]::Match($lines[$i], '^([ ]*)- \|[ \t]*$')
        if ($m.Success) { $start = $i + 1; $indent = $m.Groups[1].Value.Length; break }
    }
    if ($start -lt 0) { throw 'redis.yaml 里找不到启动薄壳(command 下的 `- |` 字面块)' }
    $body = [System.Collections.Generic.List[string]]::new()
    for ($i = $start; $i -lt $lines.Count; $i++) {
        $l = $lines[$i]
        if ($l.Trim().Length -eq 0) { $body.Add(''); continue }
        if (($l.Length - $l.TrimStart().Length) -le $indent) { break }
        $body.Add($l)
    }
    $code = Get-NonCommentText ($body -join "`n")
    if ([string]::IsNullOrWhiteSpace($code)) { throw 'redis.yaml 的启动薄壳是空的' }
    return $code
}

# redis.yaml 里生效的 MAXCLIENTS 值。凡是以 MAXCLIENTS= 开头的赋值行都算一次定义(不论右边是什么),
# 必须恰好 1 次且右边是不带前导 0 的正整数字面量;否则 throw(fail-closed,不取「第一个」糊过去)。
function Get-RedisManifestMaxClients {
    $code = Get-NonCommentText (Read-RepoText $script:RedisPath)
    $definitions = [regex]::Matches($code, '(?m)^[ \t]*MAXCLIENTS=(\S*)[ \t]*$')
    if ($definitions.Count -ne 1) {
        throw "redis.yaml 里 MAXCLIENTS=<值> 的定义应当恰好出现 1 次,实际 $($definitions.Count) 次(定义多于一处时以哪一处为准要靠猜;0 处 = 回到 Redis 缺省 10000)"
    }
    $value = $definitions[0].Groups[1].Value
    if ($value -notmatch '^[1-9][0-9]*$') {
        throw "redis.yaml 的 MAXCLIENTS 必须是不带前导 0 的正整数字面量(实际 '$value'):部署脚本的连接总账按字面量读它,不展开 shell 变量"
    }
    return [long]$value
}

# redis-ha.yaml 里 ConfigMap redis-ha-scripts 某个键(如 redis-start.sh)的字面块正文,不含注释行:`  <键>: |` 之后、
# 缩进回到它同级或更浅之前的全部行。只对该脚本断言(哨兵脚本里有合法的 exit,代理脚本另有 maxconn),不扫整份清单。
function Get-RedisHaScriptBlock {
    param([Parameter(Mandatory = $true)][string]$Key)
    $lines = (Read-RepoText $script:RedisHaPath) -split "`n"
    $start = -1
    $indent = 0
    for ($i = 0; $i -lt $lines.Count; $i++) {
        $m = [regex]::Match($lines[$i], '^([ ]*)' + [regex]::Escape($Key) + ':[ \t]*\|[ \t]*$')
        if ($m.Success) { $start = $i + 1; $indent = $m.Groups[1].Value.Length; break }
    }
    if ($start -lt 0) { throw "redis-ha.yaml 里找不到 ConfigMap 键 ${Key}: |" }
    $body = [System.Collections.Generic.List[string]]::new()
    for ($i = $start; $i -lt $lines.Count; $i++) {
        $l = $lines[$i]
        if ($l.Trim().Length -eq 0) { $body.Add(''); continue }
        if (($l.Length - $l.TrimStart().Length) -le $indent) { break }
        $body.Add($l)
    }
    $code = Get-NonCommentText ($body -join "`n")
    if ([string]::IsNullOrWhiteSpace($code)) { throw "redis-ha.yaml 的 $Key 是空的" }
    return $code
}

# redis-ha.yaml 里 `<Name>=<值>` 的生效定义:必须恰好 1 次、右边是不带前导 0 的正整数字面量,并且落在 kind 为 $Kind、名为 $ObjectName
# 的那一份文档里(Pod 模板的 command;不在脚本 ConfigMap 里 —— 改它要改模板摘要,重启闸与滚动才看得见)。否则 throw。
function Get-RedisHaTemplateLiteral {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Kind,
        [Parameter(Mandatory = $true)][string]$ObjectName
    )
    $code = Get-NonCommentText (Read-RepoText $script:RedisHaPath)
    $definitions = [regex]::Matches($code, '(?m)^[ \t]*' + [regex]::Escape($Name) + '=(\S*)[ \t]*$')
    if ($definitions.Count -ne 1) {
        throw "redis-ha.yaml 里 $Name=<值> 的定义应当恰好出现 1 次,实际 $($definitions.Count) 次"
    }
    $value = $definitions[0].Groups[1].Value
    if ($value -notmatch '^[1-9][0-9]*$') {
        throw "redis-ha.yaml 的 $Name 必须是不带前导 0 的正整数字面量(实际 '$value'):部署脚本按字面量读它"
    }
    $docs = @(($code -split '(?m)^---[ \t]*$') | Where-Object { $_ -match ('(?m)^[ \t]*' + [regex]::Escape($Name) + '=') })
    if ($docs.Count -ne 1 -or $docs[0] -notmatch ('(?m)^kind:[ \t]*' + $Kind + '[ \t]*$') -or $docs[0] -notmatch ('(?m)^  name:[ \t]*' + [regex]::Escape($ObjectName) + '[ \t]*$')) {
        throw "redis-ha.yaml 的 $Name 必须定义在 $Kind $ObjectName 的 Pod 模板里(不在脚本 ConfigMap 里:改值要改模板摘要,重启闸与滚动才看得见)"
    }
    return [long]$value
}

Write-Host "== 共享 Redis maxclients 契约(P0-E 第 1 批)=="

Test-Case 'T1:redis.yaml 的 MAXCLIENTS 恰好定义 1 次,且是正整数' {
    $maxClients = Get-RedisManifestMaxClients
    Assert-True -Condition ($maxClients -gt 0) -Because "MAXCLIENTS 必须为正(实际 $maxClients)"
}

Test-Case 'T2:每一条 exec redis-server 都带 --maxclients "$MAXCLIENTS",这样的行至少 2 条' {
    $code = Get-NonCommentText (Read-RepoText $script:RedisPath)
    $execLines = @(($code -split "`n") | Where-Object { $_ -match '\bexec[ \t]+redis-server\b' })
    Assert-True -Condition ($execLines.Count -ge 2) -Because "有口令、无口令两个分支各一条 exec redis-server(实际 $($execLines.Count) 条)"
    foreach ($line in $execLines) {
        Assert-Match -Text $line -Pattern '--maxclients[ \t]+"\$MAXCLIENTS"(?:[ \t]|$)' -Because "每一条 exec redis-server 都必须带 --maxclients `"`$MAXCLIENTS`"(漏一条 = 那个分支回到 Redis 缺省 10000):$($line.Trim())"
    }
}

Test-Case 'T3:启动薄壳在 exec 之前打印 fd 限值(redis fd limit: 一行)' {
    $code = Get-RedisStartupScript
    $printAt = $code.IndexOf('redis fd limit:')
    Assert-True -Condition ($printAt -ge 0) -Because '启动日志里必须有 redis fd limit: 一行:生产运行时的 nofile 本机无法核实,事后只能靠它判断 maxclients 有没有被下调'
    Assert-Match -Text $code -Pattern 'echo "redis fd limit: soft=\$soft hard=\$hard need=\$need maxclients=\$MAXCLIENTS"' -Because '打印要带软限、硬限、所需 fd 数与 maxclients 四项,排查时才不用再去猜'
    $firstExecAt = [regex]::Match($code, '\bexec[ \t]+redis-server\b').Index
    Assert-True -Condition ($printAt -lt $firstExecAt) -Because 'fd 限值必须在 exec redis-server 之前打印(exec 之后 shell 已被替换,不会再有输出)'
}

Test-Case 'T4:docker-compose 的 redis 带 --maxclients,数值与 redis.yaml 的 MAXCLIENTS 相同' {
    $maxClients = Get-RedisManifestMaxClients
    $block = Get-NonCommentText (Get-ComposeServiceBlock -Text (Read-RepoText $script:ComposePath) -Service 'redis')
    $found = [regex]::Matches($block, '--maxclients[ \t]+(\S+)')
    Assert-Equal -Expected 1 -Actual $found.Count -Because 'compose 的 redis 命令里 --maxclients 应当恰好出现 1 次'
    Assert-Equal -Expected ([string]$maxClients) -Actual $found[0].Groups[1].Value -Because '本机 / 预发与 K8s 用同一个连接上限,改一边必须同改另一边'
}

Test-Case 'T5:硬限不足只打 ERROR 不拒启;不设 timeout;不自己改 nofile' {
    $code = Get-RedisStartupScript
    Assert-Match -Text $code -Pattern 'if \[ "\$hard" -lt "\$need" \]; then echo "ERROR: nofile hard limit' -Because '硬限低于 maxclients + 32 时必须在日志里明说'
    Assert-NotMatch -Text $code -Pattern '\bexit\b' -Because '唯一的 Redis 起不来等于全服停:硬限不足是有意的 fail-open,只打 ERROR,启动薄壳里不能 exit'
    Assert-NotMatch -Text $code -Pattern '--timeout\b' -Because '设了 timeout 会不停踢掉常驻的空闲池连接与 C++ 每进程唯一的那条连接'
    Assert-NotMatch -Text $code -Pattern 'ulimit[ \t]+-[SH]?n[ \t]+(?!2>)\S' -Because 'redis-server 会自己把软限抬到硬限(待实测);薄壳只读不改,读数才是 Redis 启动时真正面对的限值'
}

Test-Case 'T6:redis-ha.yaml 的 MAXCLIENTS 在 StatefulSet redis 的 Pod 模板里定义一次、与 redis.yaml 同值;redis-start.sh 的参数表带 --maxclients,exec 之前打印 fd 限值' {
    $haMax = Get-RedisHaTemplateLiteral -Name 'MAXCLIENTS' -Kind 'StatefulSet' -ObjectName 'redis'
    Assert-Equal -Expected (Get-RedisManifestMaxClients) -Actual $haMax -Because '两种形态共用一份 Redis 连接总账(只从 redis.yaml 读),必须同值'
    $code = Get-RedisHaScriptBlock -Key 'redis-start.sh'
    Assert-Match -Text $code -Pattern ':[ \t]*"\$\{MAXCLIENTS:\?' -Because 'MAXCLIENTS 没注入时脚本必须当场失败,不能悄悄回到 Redis 缺省 10000'
    $argTable = [regex]::Match($code, '(?ms)^[ \t]*set -- redis-server \\\n(?<args>(?:[^\n]*\\\n)*[^\n]*)$')
    Assert-True -Condition $argTable.Success -Because 'redis-start.sh 里应当有 set -- redis-server \ 续行的参数表'
    Assert-Match -Text $argTable.Groups['args'].Value -Pattern '--maxclients[ \t]+"\$MAXCLIENTS"[ \t]*\\' -Because '每个 redis-server 都要显式 --maxclients "$MAXCLIENTS"'
    Assert-NotMatch -Text $code -Pattern '\bexec[ \t]+redis-server\b' -Because '不得有绕过参数表的 exec redis-server(那一条会漏掉 --maxclients)'
    $printAt = $code.IndexOf('echo "redis fd limit: soft=$soft hard=$hard need=$need maxclients=$MAXCLIENTS"')
    $execAt = $code.IndexOf('exec "$@"')
    Assert-True -Condition ($printAt -ge 0 -and $execAt -gt $printAt) -Because 'fd 限值必须用与 redis.yaml 同格式的一行打印,且在 exec "$@" 之前'
    Assert-Match -Text $code -Pattern 'if \[ "\$hard" -lt "\$need" \]; then echo "ERROR: nofile hard limit' -Because '硬限低于 maxclients + 32 时必须在日志里明说(只报错不拒启,与 redis.yaml 同口径)'
    Assert-NotMatch -Text $code -Pattern 'ulimit[ \t]+-[SH]?n[ \t]+(?!2>)\S' -Because '只读不改 nofile'
}

Test-Case 'T7:redis-ha.yaml 的 PROXY_MAXCONN 在 Deployment redis-proxy 的 Pod 模板里定义一次;haproxy 的 global / defaults maxconn 都由它生成,不留字面量' {
    $null = Get-RedisHaTemplateLiteral -Name 'PROXY_MAXCONN' -Kind 'Deployment' -ObjectName 'redis-proxy'
    $code = Get-RedisHaScriptBlock -Key 'proxy-start.sh'
    Assert-Match -Text $code -Pattern ':[ \t]*"\$\{PROXY_MAXCONN:\?' -Because 'PROXY_MAXCONN 没注入时代理必须当场失败'
    Assert-Match -Text $code -Pattern 'echo "  maxconn \$\(\(PROXY_MAXCONN \+ 100\)\)"' -Because 'global 的上限比前端多留一点(诊断口与健康检查)'
    Assert-Match -Text $code -Pattern 'echo "  maxconn \$\{PROXY_MAXCONN\}"' -Because 'defaults(前端业务连接)的上限就是 PROXY_MAXCONN,与 Redis 的 maxclients 对得上账'
    Assert-NotMatch -Text $code -Pattern '(?m)^[ \t]*maxconn[ \t]+\d+' -Because '不得再有写死的 maxconn 数字(那一份不进连接上限不等式)'
}

Test-Case 'T8:redis-match-cluster.yaml 的 MAXCLIENTS 恰好定义 1 次、与 redis.yaml 同值;唯一一条 exec redis-server 带 --maxclients "$MAXCLIENTS",exec 之前打印 fd 限值,硬限不足只打 ERROR' {
    $code = Get-NonCommentText (Read-RepoText $script:RedisMatchClusterPath)
    $definitions = [regex]::Matches($code, '(?m)^[ \t]*MAXCLIENTS=(\S*)[ \t]*$')
    Assert-Equal -Expected 1 -Actual $definitions.Count -Because 'MAXCLIENTS 只在启动薄壳里定义一次(部署脚本的 match 集群总账与 -WaitReady 的逐节点核对都从这一行读值)'
    Assert-Match -Text $definitions[0].Groups[1].Value -Pattern '^[1-9][0-9]*$' -Because '部署脚本按字面量读它,不展开 shell 变量'
    Assert-Equal -Expected (Get-RedisManifestMaxClients) -Actual ([long]$definitions[0].Groups[1].Value) -Because '与核心、玩家域集群同一个上限:节点的 nofile 硬限按同一个 need 配,运维只记一个数'
    # 启动薄壳 = 从 MAXCLIENTS= 一行到 exec redis-server 一行(本集群无口令,只有这一条 exec)。
    $startup = [regex]::Match($code, '(?ms)^[ \t]*MAXCLIENTS=\S*[ \t]*$.*?^[^\n]*\bexec[ \t]+redis-server\b[^\n]*$')
    Assert-True -Condition $startup.Success -Because '启动薄壳应当从 MAXCLIENTS= 一行开始,到 exec redis-server 一行结束'
    $execLines = @(($code -split "`n") | Where-Object { $_ -match '\bexec[ \t]+redis-server\b' })
    Assert-Equal -Expected 1 -Actual $execLines.Count -Because '本集群无口令分支,exec redis-server 只有一条'
    Assert-Match -Text $execLines[0] -Pattern '--maxclients[ \t]+"\$MAXCLIENTS"(?:[ \t]|$)' -Because "exec redis-server 必须带 --maxclients `"`$MAXCLIENTS`"(漏了就回到 Redis 缺省 10000):$($execLines[0].Trim())"
    Assert-Match -Text $startup.Value -Pattern 'echo "redis fd limit: soft=\$soft hard=\$hard need=\$need maxclients=\$MAXCLIENTS"' -Because 'fd 限值用与 redis.yaml 同格式的一行打印,且在 exec redis-server 之前(薄壳从 MAXCLIENTS= 开始、到 exec 结束)'
    Assert-Match -Text $startup.Value -Pattern 'if \[ "\$hard" -lt "\$need" \]; then echo "ERROR: nofile hard limit' -Because '硬限低于 maxclients + 32 时必须在日志里明说'
    Assert-NotMatch -Text $startup.Value -Pattern '\bexit\b' -Because '硬限不足是有意的 fail-open:只打 ERROR,启动薄壳里不能 exit'
    Assert-NotMatch -Text $startup.Value -Pattern 'ulimit[ \t]+-[SH]?n[ \t]+(?!2>)\S' -Because '只读不改 nofile'
}

exit (Complete-TestRun -SuiteName 'redis_maxclients_contract')
