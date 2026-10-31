# 玩家域 Redis Cluster 运维手册

设计见 [redis-player-store-cluster.md](../design/redis-player-store-cluster.md)。这里只写怎么操作。

**状态(2026-10-31):未在任何集群上演练过。** 清单、部署脚本和本手册都还没运行,真集群验证按设计 §14.2 做完之前,这里的步骤只是按设计和 Redis 8.10 源码写的预案。容量数字以 `tools/scripts/redis_cluster_capacity.ps1` 的实测为准。

## 1. 一览

| 项 | 值 |
|---|---|
| 里面放什么 | 以 `player_id` 为身份的协调键(位置、归属代次、交接资格、会话、断线租约、gate 索引、战斗锁等)和登录域(令牌、登录态、连接栅栏)。玩家存档不在这里,在 TiDB。控制域(`redis`、`redis-queue-g<N>`)不变 |
| 版本 | `redis:8.10.2`,钉摘要。扩缩容要 8.4 起的原子迁移,不要用浮动标签 |
| 工作负载 | `statefulset/redis-player`(`OnDelete`),`namespace` 是 infra namespace |
| 地址 | 每个 Pod 一个 ClusterIP Service `redis-player-<序号>`,节点宣告 Service IP。种子固定 `redis-player-{0,1,2}.<infra>:6379` |
| 配置 | `redis-player-cluster-config`:`appendfsync everysec`、`noeviction`、`cluster-require-full-coverage no`、`save ""`、`cluster-node-timeout 5000` |
| 口令 | Secret `redis-player-auth`(键 `password`),infra 和每个 zone namespace 各一份,来自环境变量 `MMORPG_PLAYER_REDIS_PASSWORD` |
| 客户端 | scene(每进程每主节点 1 条)、login、player-locator、scene-manager、match、chat、friend、guild、trade、player-inbox、mail。gate、battle 不连 |
| 告警 | `deploy/k8s/redis-player-alerts.yaml`(组 `redis.player-domain`),处置对照见 §11 |

下面的命令都假定这两个定义,口令用 Pod 自己的环境变量,不上本机命令行:

```bash
NS=mmorpg-infra
# 在 redis-player-0 的 redis 容器里跑 redis-cli。查别的节点加 -h <Service IP> -p 6379。
rc() { kubectl -n "$NS" exec -i redis-player-0 -c redis -- sh -c 'if [ -n "$REDIS_PASSWORD" ]; then REDISCLI_AUTH=$REDIS_PASSWORD; export REDISCLI_AUTH; fi; exec redis-cli "$@"' sh "$@"; }
# 某个序号的 Service IP
svcip() { kubectl -n "$NS" get svc "redis-player-$1" -o jsonpath='{.spec.clusterIP}'; }
```

## 2. 部署

参数:

| 参数 | 含义 |
|---|---|
| `-PlayerRedisMasters` | 主节点数。dev 缺省 0,不建集群,玩家域与登录域用各服务现有的单实例。staging、prod 必须显式给,至少 3 |
| `-PlayerRedisReplicasPerMaster` | 每主副本数,缺省 1(`min-replicas-to-write 0`)。2 时取 1,节点数 ×1.5(设计 §7) |
| `-PlayerRedisMaxMemory` | 每节点 `maxmemory`。容器内存按 2 倍渲染。release 档 apply 集群时必须给 |
| `-PlayerRedisStorage` | 每节点 PVC,至少放得下 2 × maxmemory 的 AOF。release 档 apply 集群时必须给,创建后不能改(§8) |
| `MMORPG_PLAYER_REDIS_PASSWORD` | 环境变量。release 档必须有,至少 12 位。dev 可以不给,集群无口令启动 |

这些参数都不粘滞。`infra-up`、`zone-up`、`all-up` 每次都要给同一个 `-PlayerRedisMasters`:漏了在 staging、prod 被拒,在 dev 会把服务渲染回单实例。

```powershell
$env:MMORPG_PLAYER_REDIS_PASSWORD = '<口令>'
# 先看一眼会渲染什么,不碰集群
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-infra-up -ReleaseProfile prod -PlayerRedisMasters 6 -PlayerRedisMaxMemory 8gb -PlayerRedisStorage 64Gi -DryRun <其余参数>
# infra-up 建集群,-WaitReady 等建群 Job 核对完 16384 个 slot,再逐节点核对生效的 maxclients
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-infra-up -ReleaseProfile prod -PlayerRedisMasters 6 -PlayerRedisMaxMemory 8gb -PlayerRedisStorage 64Gi -WaitReady <其余参数>
# zone-up 只渲染客户端,不动集群;集群不存在时拒绝
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up -ReleaseProfile prod -PlayerRedisMasters 6 <其余参数>
```

顺序只有一条:集群先于连它的服务。`infra-up` 内部已经这样排了(集群清单在全局 Go 服务之前);`zone-up` 跑在集群之前会被 `Assert-RedisPlayerPreflight` 拒绝。

部署时打印的两行要看:

- `Redis player cluster: masters=6 replicas_per_master=1 nodes=12 ...`
- `Redis player connection ledger: per_master zones=... steady=... peak=... maxclients=40000 warn_at=32000 ...`。每主节点连接 = Σ(每服务 `PoolSizePerNode` × 副本数) + scene 进程数,和主节点数无关。超过 80% 水位只告警、不阻断,处理见 §7 末尾。

告警规则手工安装:

```bash
kubectl apply -n observability -f deploy/k8s/redis-player-alerts.yaml
```

没有抓取配置时规则没有数据,空表达式不报错,和一切正常长得一样。指标名按 `redis_exporter v1.62.0` 写的,部署后对着 exporter 的 `/metrics` 核对一遍。

## 3. 核对清单

部署后、扩缩容后、节点故障处理后都走一遍。

```bash
rc -p 6379 cluster info | grep -E '^cluster_(state|slots_assigned|slots_ok|known_nodes|size)'
rc --cluster check 127.0.0.1:6379
rc -p 6379 cluster nodes
rc -p 6379 cluster migration status all
```

- `cluster_state:ok`,`cluster_slots_assigned` 和 `cluster_slots_ok` 都是 16384。`--cluster check` 输出 `All 16384 slots covered`,没有 `open slots` 和 `slots in migrating/importing state`。
- 每个主都有副本,主从不在同一台机器。`kubectl -n $NS get pods -l app=redis-player -o wide` 对着 `cluster nodes` 看。
- 每主 slot 数、键数、内存、ops 大致均匀。键数用 `rc --cluster info 127.0.0.1:6379`。
- `kubectl -n $NS logs job/redis-player-cluster-init` 最后一行是 `redis player cluster verified: 16384 slots covered`。这个 Job 只在 `infra-up` 时跑,过期后被清理(1 小时)。
- 客户端:
  - scene 启动日志有 `Player Redis: mode=cluster seeds=... auth=on`,周期日志 `[PlayerRedis] ...` 里 `budget_exhausted`、`null_replies`、`rejected_by_guard` 不涨。`moved` 在迁移时涨是正常的,`topology_refresh_failures` 持续涨要查。
  - login 启动横幅有 `player redis: addrs=redis-player-0... cluster=true`、`login redis: addrs=redis-player-0... cluster=true`。
  - 没有服务日志里有 `CROSSSLOT` 或 `NOAUTH`。
- 进程的 fd 与 `maxclients`:`kubectl -n $NS logs redis-player-0 -c redis | grep 'redis fd limit'`。

## 4. 扩容

设计 §8.1。**要 8.4 以上才有原子迁移,玩家域不要用逐键 `MIGRATE`**(设计 §8.3:声明了缺失键的多键脚本会持续 `TRYAGAIN`,迁移中切主可能同键两份)。

低峰期分批做。迁移期间客户端只看到交接瞬间的 `MOVED` 和极短的停写,C++ 路由层在 10s 预算内重试。

1. **预检。** 走一遍 §3,不能有进行中的迁移(`cluster migration status all` 里没有 `state` 不是终态的任务)。
2. **起新节点。** 带新的主节点数跑 `infra-up`,其它参数不变:

   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-infra-up -ReleaseProfile prod -PlayerRedisMasters 9 -PlayerRedisMaxMemory 8gb -PlayerRedisStorage 64Gi -WaitReady <其余参数>
   ```

   只多起空节点和它们的 Service(`scale-up` 警告是预期的),已有节点不重启。新节点不会自己入群,建群 Job 只核对已入群的节点,并打 `WARNING: nodes not in the cluster`。`zone-up` 不用重跑:种子不变,客户端自己刷新拓扑。
3. **入群。** 按序号依次加,先主后从。假设原来 12 个节点(0..11),新增序号 12..17,12、13、14 当主:

   ```bash
   SEED=$(svcip 0)
   for i in 12 13 14; do rc --cluster add-node "$(svcip $i):6379" "$SEED:6379"; done
   # 等 cluster nodes 里能看到它们,记下新主的节点 ID
   rc -p 6379 cluster nodes | grep -E "$(svcip 12)|$(svcip 13)|$(svcip 14)"
   rc --cluster add-node "$(svcip 15):6379" "$SEED:6379" --cluster-slave --cluster-master-id <序号 12 的节点 ID>
   rc --cluster add-node "$(svcip 16):6379" "$SEED:6379" --cluster-slave --cluster-master-id <序号 13 的节点 ID>
   rc --cluster add-node "$(svcip 17):6379" "$SEED:6379" --cluster-slave --cluster-master-id <序号 14 的节点 ID>
   ```

   主从分散到不同机器,否则一台机器挂掉就丢一片。
4. **迁 slot。** 在**目标节点**上执行,一次一批,slot 段按 `cluster nodes` 现状算:

   ```bash
   rc -h "$(svcip 12)" -p 6379 cluster migration import 0 1364 5461 6826
   # 返回任务 ID
   rc -h "$(svcip 12)" -p 6379 cluster migration status id <任务 ID>
   ```

   只能在主节点上发起,目标必须能 `CLUSTER MIGRATION` 的 slot 状态是稳定的。也可以用 `rc --cluster rebalance "$SEED:6379" --cluster-use-empty-masters`(全部节点 ≥ 8.4 时走原子迁移),但一次动得多,不好控制批量。第一次做要手工分批,记下单批时长和 §4.5 的观察结果。
5. **观察。** 迁移期间看:
   - `[PlayerRedis]` 的 `moved`、`ask` 会涨,`budget_exhausted`、`null_replies` 不应该涨;
   - Go 服务的错误率,只允许重定向;
   - 源节点内存(迁移时 fork,容器内存的 2 倍余量就是留给它的)和 `RedisPlayerMemoryNearMax`。
6. **收尾。** 走一遍 §3,再看每主 slot、键、内存、ops 分布。之后部署都带新的主节点数(少给会被 `shrink` 闸拒绝)。

## 5. 缩容

缩容是反向迁移,没有键形变化,不用停写(设计 §8.4)。**不能直接改 `-PlayerRedisMasters`:部署脚本会拒绝**,直接缩 StatefulSet 等于同时摘掉持有 slot 的主和它的副本。至少留 3 个主。

StatefulSet 只能从最高序号往下缩,所以要下线的就是序号最高的那几个节点。

1. **预检。** 走一遍 §3。算好留下的主能装下全部数据(内存和连接都看)。
2. **让高序号节点不持有 slot。** 序号要被摘掉的主,slot 全部迁给留下的主,在目标上执行 `cluster migration import`,核对任务完成。迁空的主因为 `cluster-allow-replica-migration yes` 会自动变成接收方的副本。
3. **让留下的节点不依赖高序号节点。** 留下的序号里如果有副本的主在被摘的序号上,在这个副本上切到留下的主:

   ```bash
   rc -h <副本 Service IP> -p 6379 cluster replicate <留下的主的节点 ID>
   ```

   核对每个留下的主都还有副本。序号 0、1、2 必须留下(种子),它们是主是从都可以。
4. **摘节点。**

   ```bash
   rc --cluster del-node "$(svcip 0):6379" <节点 ID>
   ```

   `del-node` 会让其余节点忘掉它并把它关掉。用 `cluster nodes` 核对。
5. **缩 StatefulSet,清理对象。** 先缩再部署,部署脚本比的是线上副本数:

   ```bash
   kubectl -n "$NS" scale statefulset redis-player --replicas=<新节点数>
   kubectl -n "$NS" delete svc redis-player-<每个摘掉的序号>
   kubectl -n "$NS" delete pvc data-redis-player-<每个摘掉的序号>
   ```

   PVC 在确认不再需要后再删。
6. **部署。** 之后所有部署带新的 `-PlayerRedisMasters`。走一遍 §3。

回退等于再迁一次 slot。

## 6. 节点故障

先看 `cluster nodes` 和 `--cluster check`,再按下表。

### 主节点挂了

副本在约 5s + 选举后接管,这段时间这 1/M 玩家的写失败,C++ 路由层按 10s 预算重试,Go 侧报错或重试。不用操作。Pod 重启后旧主自动变成新主的副本。主从切换可能丢最后一段异步复制(设计 §7),影响面就是这 1/M。

切完之后:

- 确认新主有副本(旧主回来了);
- 这一片的 `owner_epoch` 铸造要 `WAITAOF 1 1`,没有副本期间这 1/M 玩家的首登与换节点回 7,副本同步完恢复;
- 主从在同一台机器上时重新分散。

### 副本挂了

每主 1 副本时主照常可写(`min-replicas-to-write 0`)。Pod 重启后自动全量或增量同步,每主 25 GB 量级是分钟级。期间这一片没有可切的副本,不要同时动它的主。

### PVC 丢了,节点是空的

节点带着新的节点 ID 起来,Service IP 不变,不会自己入群。`infra-up` 的建群 Job 对这种节点只打 `WARNING`,不碰。

```bash
BAD=<旧节点 ID>
# 在每个活着的节点上忘掉旧 ID(60 秒内都要做完,否则 gossip 又把它传回来)
for i in <活着的序号>; do rc -h "$(svcip $i)" -p 6379 cluster forget "$BAD"; done
rc --cluster add-node "$(svcip <序号>):6379" "$(svcip 0):6379" --cluster-slave --cluster-master-id <它该跟的主的节点 ID>
```

### 一片主从都挂了(`RedisPlayerSlotsNotCovered`)

这是唯一一种 `cluster_state` 还是 `ok`、但部分玩家全部失败的情况(`cluster-require-full-coverage no`)。落在这 1/M slot 的玩家读写都回 `CLUSTERDOWN Hash slot not served`,其余玩家不受影响。

1. **不要跑 `--cluster fix`。** 它可能把缺的 slot 分给空节点,集群看起来好了,旧数据就回不来了。
2. 先救 PVC:把主或副本的任何一个节点带原 PVC 拉起来,它从 `nodes.conf` 恢复身份,slot 自动回来。
3. 主和副本的 PVC 都没了,这 1/M 的协调键就没了,要负责人决定。代次、交接资格、位置都在里面,后果看 [zone Redis 回退防护](../design/zone-redis-regression-guard-20261022.md)。决定接受之后才把这些 slot 分给新节点:`--cluster fix`,或在新主上 `cluster addslots`。

### 多个节点同时报 `cluster_state:fail`

多半是网络分区:与多数派失联的主自己停止服务,读也停(`cluster-allow-reads-when-down no`),这是防脑裂的主要手段,不要改配置绕过去。先修网络,看 16379 总线端口。

### Service 被删了

Service 重建会换 IP,节点的宣告地址、`nodes.conf` 和客户端拓扑都失效。重建后滚动重启这个节点(§7)让它宣告新 IP,再重启 scene:C++ 的种子只在启动时解析一次(设计 §5.6、§11)。**不要手工删这些 Service,也不要给它们写 `clusterIP: None`。**

## 7. 滚动重启

`OnDelete`:改了 Pod 模板(镜像、`MAXCLIENTS`、资源)不会自己重启任何节点,`kubectl rollout status` 也不可用。一个一个来,PDB 只防 `kubectl drain`,不防手工。

1. 列出节点和角色:`rc -p 6379 cluster nodes`。先重启副本,再处理主。
2. 副本:`kubectl -n $NS delete pod redis-player-<序号>`,等它 Ready,确认 `master_link_status:up`:

   ```bash
   rc -h "$(svcip <序号>)" -p 6379 info replication | grep master_link_status
   ```
3. 主:先在它的副本上切主,确认角色换了,再按副本处理:

   ```bash
   rc -h <副本 Service IP> -p 6379 cluster failover
   rc -p 6379 cluster nodes | grep <副本节点 ID>
   ```
4. 下一个节点之前走一遍 `cluster info`,确认 `cluster_state:ok`、没有 `fail` 标记。

配置项改动:

- `redis.conf`(ConfigMap)里能在线改的先 `CONFIG SET`,再改清单,下次滚动时生效。
- 连接逼近上限(`RedisPlayerConnectionsNearMax`、总账告警):每个节点先 `CONFIG SET maxclients <新值>` 止血,再改 `redis-player-cluster.yaml` 的 `MAXCLIENTS` 并滚动。加主节点没有用:每主连接只取决于客户端进程数。别的办法是降 `PoolSizePerNode` 或减副本。
- `maxmemory`:`CONFIG SET maxmemory <新值>` 先在线改,再带新的 `-PlayerRedisMaxMemory` 跑 `infra-up` 并滚动。容器内存跟着变,Pod 模板变了,不滚动不生效。

## 8. 口令轮换与扩盘

**口令轮换。** 新旧口令要并存一段时间,用 ACL 给默认用户加第二个口令:

```bash
# 每个节点(主和副本)都做
rc -h <IP> -p 6379 acl setuser default '>新口令'
rc -h <IP> -p 6379 config set masterauth '新口令'
```

然后带新的 `MMORPG_PLAYER_REDIS_PASSWORD` 跑 `infra-up`、每个 zone 的 `zone-up`:Secret 更新,scene 的配置指纹(`mmorpg.io/player-redis-config-hash`)变化会自动滚动。**Go 服务的 Pod 模板不变,要手工重启**:

```bash
kubectl -n "$NS" rollout restart deployment match chat friend guild trade player-inbox mail
kubectl -n mmorpg-zone-<zone> rollout restart deployment login player-locator scene-manager
```

确认全部客户端换完(`client list` 里 `age` 都比轮换时刻新),再去掉旧口令,按 §7 滚动 Redis 节点让启动参数里的 `--requirepass` 也换成新值:

```bash
rc -h <IP> -p 6379 acl setuser default '<旧口令'
```

**扩盘。** `volumeClaimTemplates` 创建后不可改,部署脚本会拒绝不同的 `-PlayerRedisStorage`。StorageClass 允许扩容时:

```bash
for i in <全部序号>; do kubectl -n "$NS" patch pvc "data-redis-player-$i" -p '{"spec":{"resources":{"requests":{"storage":"128Gi"}}}}'; done
# Pod 不停,只删 StatefulSet 对象
kubectl -n "$NS" delete statefulset redis-player --cascade=orphan
```

之后带新的 `-PlayerRedisStorage` 跑 `infra-up` 重建 StatefulSet 对象。文件系统扩容可能要重启 Pod,按 §7 一个一个来。

## 9. 中断恢复

**迁移中断。** 原子迁移失败时任务回滚,slot 还归源节点。交接前源节点一直保有全部键,不会同键两份。

```bash
rc -h <目标 IP> -p 6379 cluster migration status all
rc -h <目标 IP> -p 6379 cluster migration cancel id <任务 ID>   # 要主动放弃时
```

- 源主在交接前宕机:任务失败,被提升的副本有完整数据。等切主稳定,走 §3,重新发起。
- 目标宕机:任务失败,slot 没动。目标恢复后重新发起。
- `cluster migration status` 里任务一直不结束:看源节点的 fork 内存和复制积压,看日志里的 `Import` / `Migrate` 行。确认卡住再 `cancel`。

**建群 Job 失败。** 看 `kubectl -n $NS logs job/redis-player-cluster-init`。

- `did not answer PONG within 600s`:节点没起来,看 Pod。常见是 PVC 调度不了、`getent` 解析不到 Service。
- `cluster did not settle within 240s`:日志里有每个节点的 `cluster info`。`--cluster create` 半路失败时,节点已经有状态,Job 重跑不会再建。**集群里确实没有数据时**(全新部署),每个节点 `flushall` 再 `cluster reset hard`,然后重跑 `infra-up`。有数据时按 §6 处理,不要 reset。

**部署脚本拒绝。**

| 报错 | 处理 |
|---|---|
| `必须显式给 -PlayerRedisMasters` | staging、prod 没给。照传线上的主节点数 |
| `本次要让服务连玩家域 Redis Cluster ... 没有 statefulset/redis-player` | 先跑 `infra-up` |
| `线上有 statefulset/redis-player ... -PlayerRedisMasters 0` | 少传了参数,照传。不传的后果是服务指回控制域,全员不在线且不报错 |
| `拒绝缩小玩家域 Redis Cluster` | 按 §5 |
| `拒绝变更玩家域 Redis Cluster 的 PVC 大小` | 照传线上值,或按 §8 扩盘 |

**下线集群(只有 dev)。** 服务指回单实例之后,集群上的键不会搬回去。

```bash
kubectl -n "$NS" delete statefulset redis-player
kubectl -n "$NS" delete svc -l mmorpg.io/redis-player-node=true
kubectl -n "$NS" delete svc redis-player-headless
kubectl -n "$NS" delete pvc -l app=redis-player
```

然后不带 `-PlayerRedisMasters` 部署。

## 10. 配套工具在集群下怎么用

| 工具 | 用法 |
|---|---|
| 压测前清库 | `dev_tools.ps1 -Command k8s-redis-wipe -ConfirmWipeAllRedis -InfraNamespace mmorpg-stress-<x>`,逐主节点 `FLUSHALL SYNC` 并核对 `DBSIZE=0`。Cluster 的 `FLUSHALL` 只清本节点,不要只对一个节点执行。本机用 `dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis` |
| 压测快照 | `stress_snap.ps1` 的 `-RedisTargets` 写 `k8s-cluster:<namespace>/redis-player`,每个时刻现列主节点,逐主节点抓。`stress_summarize.ps1` 的 R1、R2 逐主节点出行,再合成一行 `<name>-all` |
| 容量基准 | `redis_cluster_capacity.ps1`,见 `tools/scripts/README.md`。没压到饱和时退出码 2,数字不算证据(设计 §14.3) |
| 维护排空 P0 / P4 | 计划的 `redisTargets` 把每个主节点列成一条(上限 128),`address` 写本机端口转发的地址。取样不跟随重定向,指纹对不上或少了主节点就判未闭合(`go/db/internal/maintenancephase`)。主节点数变了要重新生成计划 |
| 分批脚本 | `deploy-staging.sh`、`stress-linux-tier.sh` 用环境变量 `REDIS_PLAYER_NODES="host:port host:port ..."` 列全部节点 |
| zone 回档 | `k8s_zone_rollback.ps1` 线上是集群时要给 `-PlayerRedisMasters`、`-PlayerRedisSeeds`,口令用 `-PlayerRedisPasswordEnv`。回档工具本身还没有落地,见脚本头注释 |

## 11. 告警对照

| 告警 | 含义 | 处置 |
|---|---|---|
| `RedisPlayerNodeDown` | exporter 连不上本 Pod 的 redis-server | §6 |
| `RedisPlayerClusterStateNotOk` | 节点报告集群状态非 ok,这个节点上的 slot 回 `CLUSTERDOWN` | §6 多个节点同时 fail |
| `RedisPlayerSlotsNotCovered` | 有 slot 没有可用的主 | §6 一片主从都挂了 |
| `RedisPlayerNodesDisagree` | 节点对成员表没有收敛 | 扩缩容刚做完几秒内正常。持续不收敛查 16379 或 `CLUSTER FORGET` 只做了一部分 |
| `RedisPlayerReplicaLinkDown` | 从与主断链,这一片没有可切的副本 | §6 副本挂了,口令看 Secret(副本靠 `--masterauth` 连主) |
| `RedisPlayerMemoryNearMax` | 超过 `maxmemory` 的 80% | §4 扩容。不要改淘汰策略。只有一个节点高查 slot 分布 |
| `RedisPlayerOomRejected` | 已有写被拒 | 立即 §4,或查异常增长的键族 |
| `RedisPlayerConnectionsRejected` | 撞到 `maxclients` | §7 末尾。到线后 redis-cli 自己也连不上 |
| `RedisPlayerConnectionsNearMax` | 超过 `maxclients` 的 80% | §7 末尾 |
| `RedisPlayerAofWriteFailed` | AOF 写失败,持久化失效 | 查 PVC 与磁盘,§8 扩盘 |
| `PlayerLocatorSlotSegmentsUncovered`(`player-locator-alerts.yaml`) | 分片对账有 slot 段长期没人负责 | 看 player_locator 副本与 `player:locator_shard:*` 租约,设计 §5.9 |
| `SceneManagerSceneMemberReconcileErrors`(`scene-manager-alerts.yaml`) | 成员对账读 location 或摘成员持续失败 | 先看玩家域与控制域 Redis 是否可用,设计 §5.10 |
