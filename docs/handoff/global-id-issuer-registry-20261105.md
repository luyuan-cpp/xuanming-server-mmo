# 交接:发号器三份记录与全局号源登记处(交 Codex 验证,2026-11-05)

> 状态:**Claude 已写完代码、测试和文档,未编译、未运行任何测试,待 Codex 按本文验证。** Java 版本轮不实现(用户 11-05:「java 先不要实现」),只在 Java 仓 `PARITY.md` 记待做。
> 需求来源:用户 11-05 贴的三个问题(两套 etcd 同时回档会重发、地域码防重靠人工、Java 版未对齐)和 [10-28 交接](./global-id-prelaunch-claude-20261028.md)。10-28 交接写的是 SQL `id_segment` 号段,10-30 起发号器已换成 etcd,本轮按当前代码修。
> 设计以 [总体方案](../design/player-data-tidb-scaling-plan.md) §6.3 为准,运维以 [发号器运维手册](../ops/id-issuer-runbook.md) 为准。

## 1. 改了什么

**问题 1:两套本地 etcd 同时回档后重发。** 两份本地记录都被恢复到同一份旧快照时,旧代码按 `max(两份记录, 时间下限)` 继续;计数器每次预留前进约 256 秒,重启多了跑在时钟前面,时钟正常也会重发。改法:加第三份记录,放在全世界共用的全局号源登记处(etcd),一块号三份都 CAS 成功才发;写序固定为本地两份先、登记处最后,本地两份都落后登记处就能识别为回档并上报 `records_behind_registry`,新块从登记处水位之上开始。首次初始化和事故恢复拆成不同入口,恢复只按幸存记录取上界,没有可信上界就拒绝;手册里「加一天 / 30 天」删掉。

**问题 2:地域码防重靠人工。** 登记处每个码一条记录,绑定发号世系(初始化时随机生成,也写进两份本地记录)和两套本地 etcd 的集群号;码按从小到大认领,「下一个可认领的码」在同一个 Txn 里推进,用过的码(记录丢了也一样)永不再分配,退役码留墓碑。重复部署同一个码:初始化被拒、发号被拒;克隆快照:集群号对不上被拒;同一对 etcd 的多个副本照常发号。

**问题 3:Java。** 本轮不实现,记待做。

| 文件 | 改动 |
|---|---|
| `go/data_service/internal/idissuer/issuer.go` | 三份记录预留、地域码绑定校验、`Init`、`Recover`、`Retire`、`OpenRegistry`;删掉 `Bootstrap` 和初始化的「一天余量」 |
| `go/data_service/internal/idissuer/etcd_store.go` | 本地记录改成 JSON(计数器 + 世系),不认旧纯数字格式;新增 `EtcdRegistry`(`/id_registry/v1/region/NN`、`/id_registry/v1/next_region`) |
| `go/data_service/internal/idissuer/issuer_test.go` | 内存替身重写,覆盖用户列的全部验收场景 |
| `go/data_service/internal/idissuer/etcd_integration_test.go` | 新增,构建标签 `etcd_integration`,真 etcd 回归 + 真快照回档演练 |
| `go/data_service/internal/config/config.go`、`config_test.go` | `IdIssuerConfig.RegistryEtcd`;`IssuerEndpoints` 返回三组地址,生产三组两两无交集 |
| `go/data_service/internal/svc/servicecontext.go`、`id_issuer_region_test.go` | 装配登记处 |
| `go/data_service/data_service.go`、`data_service_entry_test.go` | 新入口 `-id-issuer-recover [-id-issuer-rebind]`、`-id-issuer-retire`、`-id-registry-open -id-registry-next N`;`-id-issuer-init` 改走 `Init` |
| `go/data_service/internal/logic/id_segment_logic.go`、`_test.go`,`internal/metrics/metrics.go`,`internal/constants/error_codes.go` | 新拒绝原因的错误码映射(`records_missing` / `refused`),指标取值 |
| `deploy/k8s/manifests/infra/etcd-id-registry.yaml` | 新增登记处 StatefulSet |
| `tools/scripts/k8s_deploy.ps1`、`dev_tools.ps1` | `-IdRegistryEtcd`(staging/prod 必填)、`-HostIdRegistry`;渲染 `RegistryEtcd`;入口打印 `Id issuer: region=… registry=…` |
| `tools/scripts/tests/lib/deploy_capture.ps1`、`k8s_client_entry_contract.tests.ps1` | staging/prod 夹具补 `-IdRegistryEtcd` |
| `tools/scripts/tests/id_issuer_deploy_contract.tests.ps1`、`logical_db_deploy_contract.tests.ps1` | 登记处清单、接线、手册、渲染和拒绝的契约 |
| 文档 | 总体方案 §6.1~§6.3、发号器运维手册(重写)、开新地域手册、K8s 开服手册、`horizontal-scale-logical-db.md` §9、`data_service_role_and_scope.md`、`deploy/k8s/README.md`、`deploy/k8s/AGENTS.md`、`AGENTS.md` §7.1、`PROGRESS.md`、`docs/README.md` |

## 2. 验证(Codex 执行,按顺序)

所有命令在仓库根 `D:\luyuan\xuanming-server-mmo-cpp` 起算。失败时保留完整输出,不要重试第二遍就改代码,报回来。

### 2.1 dev 数据清理(先做)

本地记录换了格式,旧的纯数字记录会被当成损坏。只清本机 compose 的 etcd(容器名 `etcd`),不要动别的环境:

```powershell
docker exec etcd etcdctl del --prefix /id_issuer/
docker exec etcd etcdctl del --prefix /id_registry/
```

### 2.2 Go 编译与单测(内存替身)

工作目录 `go/data_service`:

```powershell
go build ./...
go vet ./internal/idissuer/ ./internal/config/ ./internal/svc/ ./internal/logic/ .
go test -count=1 ./internal/idissuer/ ./internal/config/ ./internal/logic/
go test -count=1 -run 'TestNewIdIssuer|TestCheckExpectedRegion' ./internal/svc/
go test -count=1 -run TestSelectEntry .
go vet -tags etcd_integration ./internal/idissuer/
```

通过标准:全部 exit 0;`internal/idissuer` 里 `TestRecovery_BothLocalRestoredToOldSnapshotWithCorrectClock`、`TestBinding_SecondDeploymentWithTheSameCodeIsRefused`、`TestBinding_ClonedConfigAndSnapshotIsRefused`、`TestRecovery_EverythingLostIsRefused`、`TestRecovery_WholeRegistryLostRebuiltFromRegions` 都在跑且通过(`-v` 核一下)。Windows 上 `go test` 报 Access denied 时改用 `go test -c -o <临时目录>\x.test.exe` 再运行。再跑一遍 `-race`:`go test -race -count=1 ./internal/idissuer/`。

### 2.3 PowerShell 契约测试

```powershell
pwsh -NoProfile -File tools/scripts/tests/id_issuer_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/logical_db_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/redis_player_cluster_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_redis_split_contract.tests.ps1
```

通过标准:全部 exit 0。staging/prod 的 DryRun 现在多要 `-IdRegistryEtcd`,夹具 `deploy_capture.ps1` 已补;如果别的套件报 `must pass -IdRegistryEtcd`,说明它绕过了夹具,报回来,不要在部署脚本里放宽。

### 2.4 真 etcd 回归(与 2.2 分开报告)

起 5 个单节点 etcd,引导令牌各不相同(集群号就不同)。镜像用官方 `quay.io/coreos/etcd:v3.5.15`(与客户端同版本,带 `etcdctl`、`etcdutl`,没有 shell):

```powershell
$img = 'quay.io/coreos/etcd:v3.5.15'
$nodes = @(@{ n = 'p'; port = 23791 }, @{ n = 's'; port = 23792 }, @{ n = 'r'; port = 23793 }, @{ n = 'p2'; port = 23794 }, @{ n = 's2'; port = 23795 })
foreach ($x in $nodes) {
  docker run -d --name "idissuer-it-$($x.n)" -p "127.0.0.1:$($x.port):2379" $img /usr/local/bin/etcd --name $x.n --data-dir /etcd-data `
    --listen-client-urls http://0.0.0.0:2379 --advertise-client-urls "http://127.0.0.1:$($x.port)" `
    --listen-peer-urls http://0.0.0.0:2380 --initial-advertise-peer-urls http://127.0.0.1:2380 `
    --initial-cluster "$($x.n)=http://127.0.0.1:2380" --initial-cluster-token "idissuer-it-$($x.n)"
}
```

工作目录 `go/data_service`:

```powershell
$env:MMORPG_ID_ISSUER_IT_PRIMARY = '127.0.0.1:23791'
$env:MMORPG_ID_ISSUER_IT_SECONDARY = '127.0.0.1:23792'
$env:MMORPG_ID_ISSUER_IT_REGISTRY = '127.0.0.1:23793'
$env:MMORPG_ID_ISSUER_IT_PRIMARY2 = '127.0.0.1:23794'
$env:MMORPG_ID_ISSUER_IT_SECONDARY2 = '127.0.0.1:23795'
go test -tags etcd_integration -count=1 -v -run 'TestEtcdIntegration' ./internal/idissuer/
```

通过标准:`TestEtcdIntegration_ClustersAreIndependent`、`_StoreContract`、`_BothLocalRolledBack`、`_SecondPairWithTheSameCodeIsRefused` 四条都 PASS,没有 SKIP。用完删容器:`docker rm -f idissuer-it-p idissuer-it-s idissuer-it-r idissuer-it-p2 idissuer-it-s2`。

### 2.5 真快照回档演练(问题 1 的验收,单独报告)

三个 etcd 用命名卷,方便拍快照和恢复:

```powershell
$img = 'quay.io/coreos/etcd:v3.5.15'
function Start-DrillEtcd($n, $port, $dataDir) {
  docker run -d --name "idissuer-drill-$n" -p "127.0.0.1:${port}:2379" -v "idissuer-drill-${n}:/etcd-data" $img /usr/local/bin/etcd --name $n --data-dir $dataDir `
    --listen-client-urls http://0.0.0.0:2379 --advertise-client-urls "http://127.0.0.1:$port" `
    --listen-peer-urls http://0.0.0.0:2380 --initial-advertise-peer-urls http://127.0.0.1:2380 `
    --initial-cluster "$n=http://127.0.0.1:2380" --initial-cluster-token "idissuer-drill-$n"
}
Start-DrillEtcd p 23801 /etcd-data/data
Start-DrillEtcd s 23802 /etcd-data/data
Start-DrillEtcd r 23803 /etcd-data/data
$env:MMORPG_ID_ISSUER_IT_PRIMARY = '127.0.0.1:23801'
$env:MMORPG_ID_ISSUER_IT_SECONDARY = '127.0.0.1:23802'
$env:MMORPG_ID_ISSUER_IT_REGISTRY = '127.0.0.1:23803'
$env:MMORPG_ID_ISSUER_IT_STATE = Join-Path $env:TEMP 'idissuer-drill-state.json'
```

在 `go/data_service` 依次:

1. 段 A,领第一段:
   ```powershell
   $env:MMORPG_ID_ISSUER_IT_PHASE = 'drill-a'; go test -tags etcd_integration -count=1 -v -run TestEtcdDrill ./internal/idissuer/
   ```
2. 给本地两套拍快照(登记处不拍):
   ```powershell
   docker exec idissuer-drill-p /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 snapshot save /etcd-data/snap.db
   docker exec idissuer-drill-s /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 snapshot save /etcd-data/snap.db
   ```
3. 段 B,再重启发号器 5 次,计数器跑到时钟前面:
   ```powershell
   $env:MMORPG_ID_ISSUER_IT_PHASE = 'drill-b'; go test -tags etcd_integration -count=1 -v -run TestEtcdDrill ./internal/idissuer/
   ```
4. 把本地两套恢复到第 2 步的快照,成员地址和令牌不变(集群号应该不变),登记处不动:
   ```powershell
   foreach ($n in @('p', 's')) {
     docker rm -f "idissuer-drill-$n"
     docker run --rm -v "idissuer-drill-${n}:/etcd-data" --entrypoint /usr/local/bin/etcdutl $img snapshot restore /etcd-data/snap.db `
       --data-dir /etcd-data/restored --name $n --initial-cluster "$n=http://127.0.0.1:2380" `
       --initial-cluster-token "idissuer-drill-$n" --initial-advertise-peer-urls http://127.0.0.1:2380
   }
   Start-DrillEtcd p 23801 /etcd-data/restored
   Start-DrillEtcd s 23802 /etcd-data/restored
   ```
5. 段 C,新进程领段:
   ```powershell
   $env:MMORPG_ID_ISSUER_IT_PHASE = 'drill-c'; go test -tags etcd_integration -count=1 -v -run TestEtcdDrill ./internal/idissuer/
   ```

通过标准:三段都 PASS。段 C 断言新段在全部历史预留之上、与状态文件里每一段都不重叠,并且上报了 `records_behind_registry`。段 C 如果报 `not the ones bound to this region code`,说明恢复改变了集群号:记下来(这关系到手册 §4.1「原地恢复不用 rebind」是否成立),不要改代码。

清理:
```powershell
docker rm -f idissuer-drill-p idissuer-drill-s idissuer-drill-r
docker volume rm idissuer-drill-p idissuer-drill-s idissuer-drill-r
Remove-Item $env:MMORPG_ID_ISSUER_IT_STATE
```

### 2.6 dev 冒烟(可选)

清完 2.1 之后按平常方式起本机 data_service 和 login,建一个角色。dev 档三份记录都在本机 etcd 里,第一次预留自动认领地域码 0:`docker exec etcd etcdctl get --prefix /id_registry/` 能看到 `region/00`,`/id_issuer/counter/primary` 的值是 JSON。

## 3. 没做的、剩下的风险

- **全部未编译、未运行。** 上面任何一步失败都按失败报。
- **Java 版没做**(用户要求)。Java 的建角仍走进程内 Snowflake,`lastMs` 只在进程内,换机复用 worker 时可能重号;已在 Java 仓 `PARITY.md` 记待做。两版不共享身份空间,上线前必须补上。
- **staging / K8s 演练没做**:登记处跨集群访问(私网)、`-HostIdRegistry` 的 `infra-up`、`-id-registry-open` / `-id-issuer-init` / `-id-issuer-recover` 在 Pod 里跑,都只写了手册,按手册 §5 在 staging 走一遍。
- **登记处没开认证**,和现有 etcd 一致,只能走私网。开认证、加 TLS 是后续项。
- **集群号绑定拦不住「完全相同清单 + 相同令牌」的恢复**:那种环境只要连着登记处,水位让它和原环境串行,不重号;但它能拿到发号资格。手册写了不要这样做。
- **三处同时回档**(本地两套 + 登记处)仍然查不出来。手册规定登记处不许从备份恢复,只从各地域本地记录重建。
- scene_id(`shared/snowflakealloc`)不在本轮范围;另一个会话在处理 `fenceclock.go` 与 `allocator.go` reclaim 的遗留。
