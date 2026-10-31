#requires -Version 7
<#
.SYNOPSIS
    本机压测与故障演练的 Redis 清库(AGENTS.md §6.2):compose 的单实例 redis,加上玩家域开发集群(deploy/compose.redis-cluster.yml)正在跑时的每个主节点。

.DESCRIPTION
    Redis Cluster 的 FLUSHALL 只清本节点。只清单实例的话,玩家域开发集群上的 owner_epoch、位置、会话全部留着,下一轮登录读到的是上一轮的残留,
    而且不报错。dev_tools.ps1 redis-wipe-local、stress_round19.ps1、currency_crash_window.ps1 共用这一份。
    K8s 环境用 k8s_deploy.ps1 -Command redis-wipe,不走这里。

    集群节点按容器名认:redis-player-cluster-<序号>,端口 7100 + 序号(compose 文件里写死的对应关系)。
    先问 ROLE,是 master 才 FLUSHALL SYNC,再读 DBSIZE;任何一个主节点没回 OK 或 DBSIZE 不是 0 即 throw。集群容器在跑却一个主都没找到同样 throw:
    那说明集群没建好,不当成已清空。
    单实例沿用原来的做法:本机装了 redis-cli 就用它,否则 docker exec redis。
#>

function Get-LocalPlayerClusterContainers {
    param([string[]]$DockerContextArgs = @())

    $names = @(& docker @DockerContextArgs ps --filter 'name=redis-player-cluster-' --format '{{.Names}}' 2>$null)
    if ($LASTEXITCODE -ne 0) { return @() }
    return @($names | Where-Object { $_ -cmatch '^redis-player-cluster-[0-9]+$' } | Sort-Object)
}

# 清一个集群节点:返回 master / slave。主节点清完核对 DBSIZE=0。
function Invoke-LocalPlayerClusterNodeFlush {
    param(
        [Parameter(Mandatory = $true)][string]$Container,
        [string[]]$DockerContextArgs = @()
    )

    $ordinal = [int]($Container -replace '^redis-player-cluster-', '')
    $port = [string](7100 + $ordinal)
    $role = @(& docker @DockerContextArgs exec $Container redis-cli --raw -p $port role 2>&1 | Select-Object -First 1)
    if ($LASTEXITCODE -ne 0) { throw "redis-cli ROLE failed on ${Container}: $role" }
    $roleText = ([string]$role[0]).Trim()
    if ($roleText -cne 'master') { return $roleText }
    $reply = [string](& docker @DockerContextArgs exec $Container redis-cli --raw -p $port flushall sync 2>&1)
    if ($LASTEXITCODE -ne 0 -or $reply.Trim() -cne 'OK') { throw "FLUSHALL failed on ${Container}: $reply" }
    $dbSize = [string](& docker @DockerContextArgs exec $Container redis-cli --raw -p $port dbsize 2>&1)
    if ($LASTEXITCODE -ne 0 -or $dbSize.Trim() -cne '0') { throw "DBSIZE after FLUSHALL on $Container is '$($dbSize.Trim())'" }
    return $roleText
}

function Invoke-LocalRedisFlushAll {
    param(
        # 输出回调:调用方各有各的日志风格。
        [Parameter(Mandatory = $true)][scriptblock]$WriteLine,
        [string]$DockerContext = ''
    )

    $hasDocker = [bool](Get-Command docker -ErrorAction SilentlyContinue)
    $contextArgs = @()
    if (-not [string]::IsNullOrWhiteSpace($DockerContext)) { $contextArgs = @('--context', $DockerContext) }

    if (Get-Command redis-cli -ErrorAction SilentlyContinue) {
        $reply = [string](& redis-cli FLUSHALL 2>&1)
    }
    elseif ($hasDocker) {
        $reply = [string](& docker @contextArgs exec redis redis-cli FLUSHALL 2>&1)
    }
    else {
        throw 'redis-cli not on PATH and docker unavailable: cannot flush redis'
    }
    if ($LASTEXITCODE -ne 0 -or $reply.Trim() -cne 'OK') { throw "FLUSHALL failed on the local redis: $reply" }
    & $WriteLine 'redis FLUSHALL OK (control domain, and the player domain unless the local cluster is in use)'

    if (-not $hasDocker) { return }
    $containers = @(Get-LocalPlayerClusterContainers -DockerContextArgs $contextArgs)
    if ($containers.Count -eq 0) { return }
    $masters = 0
    foreach ($container in $containers) {
        if ((Invoke-LocalPlayerClusterNodeFlush -Container $container -DockerContextArgs $contextArgs) -ceq 'master') {
            $masters++
            & $WriteLine "player cluster $container FLUSHALL OK, DBSIZE=0"
        }
    }
    if ($masters -eq 0) { throw "player cluster containers are running ($($containers -join ',')) but none is a master: the cluster is not formed, not treating it as flushed" }
    & $WriteLine "player cluster flushed: $masters primaries"
}
