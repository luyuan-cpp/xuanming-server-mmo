# 交接:发号器三份记录与全局号源登记处(交 Codex 验证,2026-11-05)

> 状态:**Codex 已修复 §4.1 三项问题并补回归；最新证据见 §5。Go 固定副本构建、单测及真 etcd 回归通过；部署大套和本轮快照演练尚未全部完成。Claude Code 11-10 独立审核见 §6：发现 1 个高危问题（同命名空间部署出的 etcd 集群号相同，地域码绑定分不出两套环境），整体验收不通过，待用户决定是否修复。** Java 版本轮不实现(用户 11-05:「java 先不要实现」),只在 Java 仓 `PARITY.md` 记待做。
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

## 4. 2026-11-05 Codex 验证记录

以下为本次实际运行证据；上文 §2 是执行步骤，§3 的“全部未运行”是原交接时状态。

- 环境：Windows/amd64，Go 1.27.2，`GOTOOLCHAIN=local`。日志目录：`C:/Users/luyua/AppData/Local/Temp/idissuer-verify-20261106T040745Z-967fed05/`（目录时间为 UTC）。本轮不改产品代码、生成协议或 Java 代码，不提交、不推送。
- 格式：本批 14 个 Go 文件 `gofmt -l` 无输出、退出 0。
- 全量构建：首轮 `go build ./...` 退出 1，`internal/noderegistry/registry.go:162` 引用 `NodeInfo.ReleaseTrack`，当时 `go/proto/common/base/common.pb.go` 尚无该字段；源契约 `proto/common/base/common.proto` 已有 `release_track = 15`。这是另一会话的发布轨道生成缺口，本轮未擅自重生全仓协议。日志 `01-build.log`。独立验证期间其他会话已补出该字段并修改服务其他文件，本轮没有对更新后的全量服务复跑构建，不能沿用首轮错误断言当前仍缺字段，也不能声称当前整包编译通过。
- 用户在构建失败后明确选择“继续独立验证发号器”。`go vet ./internal/idissuer/ ./internal/config/` 和 `go vet -tags etcd_integration ./internal/idissuer/` 均退出 0；`go test -count=1 -v ./internal/idissuer/ ./internal/config/` 退出 0，分别为 53 / 44 个顶层测试，另有 62 个配置子项，0 FAIL、0 SKIP。§2.2 点名的五个验收测试均实际运行并 PASS。日志 `02-vet-isolated.log`、`03-unit-isolated.log`、`04-vet-integration.log`。
- `internal/logic`、`internal/svc`、主程序入口的验证在首轮构建失败后未执行，待共享输入稳定后随全量构建复验，不能以独立包通过替代。
- 竞态检测未执行：Windows 当前 `CGO_ENABLED=0`，未发现可用 MinGW 工具链；已安装的两个 Linux SDK 镜像只有 Go 1.18 gccgo，不能满足本模块 Go 1.27.2。没有安装或升级工具。
- 真 etcd 首次启动失败，未进入真实用例：官方 `quay.io/coreos/etcd:v3.5.15`（镜像摘要 `sha256:0934690612905554eb61ddefb9faaaecb47c2f6931dbb453e694358092ee8990`）会把默认的 `http://localhost:2380` peer 广告地址改为容器 IP，随后与 `--initial-cluster` 不一致而退出。五个一次性容器的完整日志保存在 `06-etcd-*-container.log`，已按创建时完整 CID 清理。本节上方的启动及恢复示例已统一改为显式 `http://127.0.0.1:2380`。报告原因后，用户明确选择“按修正命令继续”，才重建隔离环境。
- **真 etcd 回归通过**：新建五个独立单节点 etcd，分别使用独立引导令牌；`go test -tags etcd_integration -count=1 -v -timeout 2m -run TestEtcdIntegration ./internal/idissuer/` 退出 0，§2.4 四条用例全 PASS、0 SKIP。日志 `07-etcd-start-corrected.log`、`08-etcd-integration.log`。
- **真快照回档演练通过**：另建三个独立节点和三个命名卷。段 A 发号后仅对本地 P/S 执行 `etcdctl snapshot save`；段 B 再建五个发号器并领段；停止 P/S，用 `etcdutl snapshot restore` 恢复到段 A 快照，保持成员地址和令牌相同，登记处 R 不变。段 C 新进程领到 `[1297472900701356032, 1297472900768464896)`，下界等于登记处保存的历史预留上界，与状态文件中六段历史发号区间均不重叠，且 `records_behind_registry` 恰好一次。三段均退出 0、PASS、0 SKIP。P/S 的恢复前后集群号分别保持 `16882647215227572047` / `11935257836932555252`，本次确实不需要 rebind。日志 `drill-{a,b,c}.log`、`11-snapshot-*.log`、`12-restore-*.log`，身份对比 `14-cluster-identity.json`。
- **清理**：两轮五节点回归环境、三节点演练环境均已按本次完整 CID 删除，三个演练命名卷也已删除；状态 JSON、两份快照和日志保留在证据目录，未触碰其他容器或共用开发数据。
- **源码边界**：`idissuer` 与 `config` 共 9 份源文件首尾 SHA-256 一致（`isolated-source-check.json`）；整个 `data_service` 抽取的 168 份输入中另有 15 份被其他会话修改，包括 `servicecontext.go` 和生成文件。因此本次独立包结果不等于当前整个工作区固定版本的验收。
- **PowerShell 契约**：`id_issuer_deploy_contract` 9/9、`logical_db_deploy_contract` 36/36，均退出 0；`k8s_deploy_contract` 退出 1，287 项中 286 PASS、1 FAIL。唯一失败是 MySQL 告警规则总数：断言要求 5，实际 7，增加了 `GatewayHikariAcquireTimeout`、`GatewayHikariSaturated`，与本批登记处无关。未改断言、未重试；失败后没有启动 `k8s_client_entry_contract`、`redis_player_cluster_contract`、`k8s_redis_split_contract`。日志 `ps-01-id_issuer_deploy_contract.log`、`ps-02-logical_db_deploy_contract.log`、`ps-03-k8s_deploy_contract.log`，退出码 `ps-results.jsonl`。所有实际调用均为文本/隔离 DryRun，没有真实部署。
- **部署输入限制**：第一套没有结束瞬时哈希，不能证明整套输入稳定；第二套运行中 `k8s_deploy.ps1` 变化；第三套运行中被测部署脚本和测试文件自身都变化。`ps-source-before.json`、`ps-source-during-02.json`、`ps-03-source-{before,after,changes}.json` 保留证据。因此上述次数只陈述当次运行，不能当作当前工作区固定版本验收；后续应待输入稳定后处理告警断言并重新完整验证。
- 本轮没有清理共用 dev etcd 的 `/id_issuer/` 或 `/id_registry/`，隔离测试不依赖这一步；可选 dev 冒烟、staging/K8s 演练未执行。Java `0.1.0-SNAPSHOT` 继续待做，已核 Java 仓 `PARITY.md` 的同日登记项。

### 4.1 首轮静态发现（当时未修复；后续修复见 §5）

1. `EtcdRegistry.Open` 在扫描地域记录之后，提交事务仅比较 `next_region` 不存在，没有复核扫描范围。并发 `Recover/Retire` 在扫描后写入一个不小于 `next` 的码时，开启仍可能成功，违反接口“next 高于全部已有码”的契约；内存替身将扫描与提交置于同一把锁，未覆盖这一交错。
2. `Issuer.read` 按 P→S→G 分开读取，另一副本在 P/S 读取后完成预留时，当前视图的 G 会高于旧 P/S。`nextStart` 在 CAS 发现冲突前就可能上报 `records_behind_registry`，把正常并发说成双本地回档；这一项本身不证明会重号。
3. `Apply-Infra` 对 dev + `-HostIdRegistry` 的拒绝位于 `Ensure-Namespace` 之后，而 `infra-up` 没有走 `Resolve-IdRegistryEtcd` 的早期校验，可能先创建命名空间再报“Nothing was written”。新增测试只覆盖纯函数与 `zone-up`，没有覆盖这个入口。

## 5. 2026-11-06 修复与 2026-11-10 交接收尾

用户授权 Codex 修复后，交 Claude Code 按自己的思路独立审核；交接文字由用户复制。**用户 11-10 已在 Claude Code 会话里发起审核，结论见 §6。** 本节记录客户端日期；部分日志的 UTC 日期比本地晚一天。

### 5.1 任务与改动边界

`idissuer` 向 data_service 的永久 ID 分配、直接发号调用和运维入口提供统一规则：只有本地两份记录与全局登记处三次 CAS 全部成功后才交付新块；地域码绑定世系与两套本地 etcd，首次初始化、恢复和永久退役分别处理。审核应覆盖这条完整业务线，而不仅是本节已知问题。

本次修复的产品及测试文件如下；原实现的完整接线范围仍见 §1。

| 文件 | 修复与回归 |
|---|---|
| `go/data_service/internal/idissuer/etcd_store.go` | `Open` 的提交事务同时比较开启指针不存在、地域范围的修改版本不晚于扫描版本；冲突有界重读，阻止扫描后并发恢复/退役插入已用码而仍开启 |
| `go/data_service/internal/idissuer/registry_open_integration_test.go` | 真实 etcd 的范围读后插入并发写者；同码恢复、更大码恢复、同码退役必须拒绝，小码恢复仍可开启；事务由 etcd 执行 |
| `go/data_service/internal/idissuer/issuer.go` | 改为 G→P→S 读取，防止正常并发被误报为本地双回档；仅本地高于 G 时复读登记处完整记录，变化则整轮重读，复读失败则零写入并拒绝发号。差距大于一块的日志不再直接断言登记处回档 |
| `go/data_service/internal/idissuer/issuer_observation_test.go` | 两种正常并发交错不误报；真实回档后即使下一次写失败仍上报差异；登记处复核读失败不写入、不发号 |
| `tools/scripts/k8s_deploy.ps1` | 携带 `-HostIdRegistry` 或显式登记处地址的入口均做早期预检，dev 拒绝发生在命名空间等写入之前 |
| `tools/scripts/tests/id_issuer_deploy_contract.tests.ps1` | 新增实际 CLI 的 `infra-up` / `all-up -SkipGoSvc -HostIdRegistry` 拒绝顺序回归 |
| `tools/scripts/tests/k8s_deploy_contract.tests.ps1` | 旧告警总数断言改为完整七条名称集合；存档池阈值按当前设计匹配 `ExtraZoneStores` / `TidbNonZoneStores` |
| `deploy/k8s/mysql-connection-alerts.yaml` | 同步已有固定 K 存档库设计：旧 zone 存档池阈值 0、生产 TiDB 档非 zone 池阈值 64；注释保留 MySQL 小 K 档独立核算要求 |

设计 §6.3、发号器运维手册已同步非原子读视图、条件复核与诊断边界。告警修复是处理首轮失败时发现的同源漂移，不是将断言放宽到实际输出；有修复前后日志。其他会话的 Redis match 总账、发布轨道和存盘代码等均未纳入本线修改。

11-10 复查发现上述代码已由其他会话收录在 `de6891478`（`保存服务端发号、部署与回归改动`）。本线未执行提交或推送。当前工作区还有大量其他会话改动，不能将整个工作区 diff 当成本任务；也不能只看当前未提交 diff 就认为修复不存在。

### 5.2 验证输入与证据位置

持久证据根目录：`D:/luyuan/xuanming-server-mmo-cpp/build/verification/idissuer-fix-20261106-a3181b57/`，属于已忽略的本地目录，不随 Git 传递。§4 的旧 `%TEMP%` 目录本次已找不到，不依赖其可访问性。

11-06 将 `go/data_service`、`go/shared`、`go/proto` 共 852 个文件复制到证据目录 `input/go/`，复制时源文件与副本 SHA-256 一致；Go 构建及以下大范围测试在该固定副本执行，使用 `GOTOOLCHAIN=local`、进程内 `GOMAXPROCS=2`、`-mod=readonly`。11-10 再核全部副本输入，0 个文件变化。哈希见 `go-source-before.json`、`fixed-input-after-20261110.json`。

11-10 当前工作区的 `issuer.go`、`etcd_store.go`、两个新增回归测试与固定副本 SHA-256 全部一致，当前发号器单测首尾核心哈希也一致，见 `current-core-{compare,after}-20261110.json`。这不等于当前整个 data_service 及共享依赖与 11-06 相同；大范围构建结论只适用于固定副本。

### 5.3 实际运行结果

| 范围 | 结果 | 证据（相对证据根） |
|---|---|---|
| `Open` 并发窗口 | 修复前 3 个应拒绝场景失败，修复后 4 子项全部通过 | `02-open-red.log`、`03-open-green.log` |
| 并发诊断、复核失败、真实回档观测 | 确定交错的红/绿回归，最终 12 个定向顶层用例通过 | `observation/` 下 `red.log`、`registry-red.log`、`recheck-red.log`、`final-green.log` |
| 固定副本 `go build -mod=readonly ./...` | exit 0；首轮缺失 `ReleaseTrack` 的阻塞已解除 | `04-build.log`、`go-results.json` |
| 固定副本普通 vet 及带标签 vet | 均 exit 0，范围同 §2.2 | `05-vet.log`、`08-vet-integration.log` |
| 固定副本 idissuer / config / logic 单测 | 57 / 47 / 96 个顶层 PASS，合计 200，0 FAIL、0 SKIP | `06-unit.log`、`unit-summary.json` |
| 固定副本 svc 接线与主程序入口 | 3 个 svc 顶层、1 个主程序顶层 PASS，exit 0 | `07-wiring.log` |
| 11-10 当前工作区 idissuer 单测 | 57 PASS，0 FAIL、0 SKIP，exit 0 | `12-current-issuer-20261110.log`、`current-issuer-result-20261110.json` |
| 真 etcd 完整回归 | 首次 4/5 PASS、1 FAIL；用户授权串行复跑一次后 5/5 PASS、0 SKIP、exit 0，包含新增 Open 的 4 子项 | `10-etcd-integration.log`、`11-etcd-integration-serial.log` |
| 登记处部署契约 | 11-06 修复后 10/10；11-10 两次均 10/10、exit 0；首轮 18 输入稳定，末轮部署脚本被其他会话修改 | `deploy/issuer-contract-{red,green}.log`、`deploy/current-20261110-*`、`deploy/current-final-20261110-*` |
| 告警契约与 CLI 最小回归 | 各 1/1，exit 0，有修复前红灯 | `deploy/mysql-alert-{red,green}.log`、`deploy/entry-{red,green}.log` |

实际 Go 命令（固定副本工作目录 `input/go/data_service`；当前单测工作目录为主仓 `go/data_service`）：

```powershell
go build -mod=readonly ./...
go vet -mod=readonly ./internal/idissuer/ ./internal/config/ ./internal/svc/ ./internal/logic/ .
go test -mod=readonly -count=1 -v -timeout 3m ./internal/idissuer/ ./internal/config/ ./internal/logic/
go test -mod=readonly -count=1 -v -timeout 2m -run 'TestNewIdIssuer|TestCheckExpectedRegion|TestSelectEntry' ./internal/svc/ .
go vet -mod=readonly -tags etcd_integration ./internal/idissuer/
go test -mod=readonly -tags etcd_integration -count=1 -v -timeout 2m -run TestEtcdIntegration ./internal/idissuer/
```

真 etcd 使用五个独立令牌的隔离单节点、镜像 `quay.io/coreos/etcd:v3.5.15`，端口 24791~24795，对应 §2.4 五个环境变量。首次完整回归在 `SecondPairWithTheSameCodeIsRefused` 的主记录写入收到 `etcdserver: request timed out`，同刻主节点记录 `slow fdatasync` 8.326 秒。此日志支持环境 I/O 延迟的判断，不能单凭它证明所有超时原因。先报告并取得用户“继续，串行重跑一次并完成演练”的答复，再原命令复跑；未改超时或断言，保留首轮失败。当前部署清单已采用 etcd 3.6.16，本轮真实测试的 3.5.15 证据不能代替部署版本验证。

11-10 首轮部署定向测试的 18 份直接输入和辅助文件首尾哈希一致，涵盖脚本、测试、设计、运维手册、清单与配置；随后设计文档仅将历史“未运行”标签改为本节证据链接。最终文档状态复跑仍为 10/10、exit 0，但 13 秒运行期间 `k8s_deploy.ps1` 被其他会话修改，其余 17 份输入稳定；没有继续自动重复运行。末轮结果只能陈述当次通过，不能证明最新部署脚本的固定版本验收；也不等于部署大套通过。

### 5.4 尚未完成、环境与清理状态

- **部署大套未完成**：11-06 `k8s_deploy_contract` 被中断，日志落盘 311 项（285 PASS、26 FAIL），没有套件总结、最终退出码或结束输入哈希。已见发布轨道辅助函数声明顺序、稳定清单 canary 匹配、Redis mock 参数捕获、旧 MySQL 总账预期、`TryGetProperty` 重载等失败，后半还有原生子进程异常与空输出；不能将它们全部认定为产品缺陷，也不能宣称当前大套仍有完全相同问题。当前脚本与大套已有后续改动。计划的后五套没有启动证据。见 `deploy/full-01-k8s_deploy_contract.log`、`deploy/recovered-status-20261110.json`。本轮未扩展修改这些功能。
- **本轮新快照演练只到 A**：11-06 A 段和 P/S 真快照成功；发出 `[1297472900298702848, 1297472900298702948)`，预留到 `1297472900365811712`。`drill/state.json`、`after-a-summary.json`、两份 `snapshot-*.db` 和哈希均保留。B、P/S 恢复、C 尚未执行，不能用 §4 的旧版完整演练替代本次修复版验证。
- **11-10 续跑环境阻塞**：启动已安装 Docker Desktop 后，它因 Secrets Engine 无法将 `engine.sock` 重命名为 `.stale` 而退出。只读检查显示重解析点、Error 1920；未读取凭据、改 ACL、删 socket 或重置 Docker/WSL。证据 `drill/resume-blocker-20261110.json`、`docker-startup-error-20261110.log`。
- **清理未获最终回执**：11-06 integration 五容器清理命令中断；11-10 engine 不可用，无法确认删除结果。drill 三容器/命名卷在 A 段后有意保留。恢复 Docker 后，先依据 `integration-containers.json` / `drill-containers.json` 中完整 CID、名称、卷名核实所有权，只清理匹配本任务的资源；不要使用按宽泛名称批量删除的命令。若继续演练，必须先保留原数据和快照，R 不得回档。
- `-race` 未执行（原 Windows 验证环境无可用 CGO/MinGW）；当前整个 data_service 的再次构建、staging/K8s 真实部署、跨地域网络及 dev 建角冒烟未执行。没有性能结论。认证/TLS、三处同时回档和同清单同令牌克隆等设计限制仍见 §3。
- Java 按用户要求继续只记待办，本次不实现；未处理其他会话的 Snowflake 任务。

### 5.5 复制给 Claude Code 的独立审核请求

```text
请独立审核 D:\luyuan\xuanming-server-mmo-cpp 的“永久 ID 发号器 + 全局号源登记处”整条业务线。

交接入口：docs/handoff/global-id-issuer-registry-20261105.md，先看 §5 的最新状态，再结合 §1、设计 docs/design/player-data-tidb-scaling-plan.md §6、docs/ops/id-issuer-runbook.md 和实际代码。

请按你自己的判断选择审核方法、重点和必要的扩展范围。可以质疑设计、实现、测试及 Codex 的解释，不限于已列出的修复，也不预设应该通过。当前 diff 混有大量其他会话工作；本线修复已被后续提交 de6891478 收录，不能只看未提交 diff。

本线已有：三份 CAS 预留、地域码世系和集群绑定、Init/Recover/Retire/Open、配置与部署接线。后续修复包括 Open 扫描到提交的竞态、非原子三处读取导致的诊断误报、登记处复核失败拒绝、HostIdRegistry 写入前预检，以及告警与存档规划漂移。

证据与边界都在 §5：固定副本构建及 200 项单测通过；当前发号器 57 项与部署契约 10 项通过；真实 etcd 串行复验 5/5。部署大套中断未完成，新快照演练仅 A 段与快照完成，B/C 被 Docker 启动故障阻塞；race、staging 和部署版本 etcd 3.6.16 尚未验证。不要把这些未完成项当成通过。

这次先交审核结果，Java 不实现。请保留其他会话改动，不自行提交或推送。若发现问题，请给出具体位置、触发条件、影响与依据；需要新增运行证据时说明要验证的假设和命令。没有实质问题也请如实说明，并列出证据边界。审核结论由你独立决定。
```

## 6. 2026-11-10 Claude Code 独立审核结论

按 §5.5 的请求在 Claude Code 会话里做的只读审核：没有改产品代码和测试，没有运行任何构建或测试（AGENTS §10.1）。

**结论：整体验收不通过。** 一个地域码只对应一对本地 etcd 时，三份 CAS 预留、写序与读序、Init / Recover / Retire / Open、打散都没找到会重号的路径；11-06 的三处修复静态看是对的。问题出在这个前提：本仓的部署清单让不同物理集群的 etcd 报出相同的集群号，登记处的绑定分不出两套环境（§6.2）。另有一处中危、两处低危。

### 6.1 看了什么

- `issuer.go`、`etcd_store.go` 逐行看，与 `de6891478` 一致，工作区无改动。按 CAS 线性化推演了：多副本并发；三处写的任一步失败或结果未知；一份或两份记录回退，包括快照恢复带来的 revision 重复；挂起很久的旧进程恢复；rebind 时旧进程还在跑；Open 与 Recover / Retire 并发。
- 配置与入口：`config.go` 的 `IssuerEndpoints`，`servicecontext.go` 的 `NewIdIssuer` / `checkExpectedRegion`，`data_service.go` 的入口互斥和 `-id-registry-next` 必填。AllowAutoSeed 和地域码 0 都只放给 dev、test。
- 打散：`go/shared/idsegment/mix.go` 与 `cpp/libs/modules/id_segment/guid_mix.h` 每一步都在 63 位上可逆，所以一一对应。Go 的 `idsegment.Client`、C++ 的 `GuidSegmentClient` 都经它发号；data_service 进程内的 snapshot_row、rollback_audit、gm_recall_op 也走 `idsegment.Client`。
- 调用方抽查：
  - login 建角只有号段一条路，`idsegment.Minter` 没有回退。
  - match 的 battle、challenge、team 走号段。
  - login 里的 `PlayerIDGen`（snowflake）没有发号调用方，只剩节点槽位和水位维护（设计 §6.4 已记，可单独清理）。
  - scene_id 仍是 snowflake，设计明示不在本线。
  - C++ 调用方没有逐个看。
- 部署：三套 etcd 清单的引导参数；`k8s_deploy.ps1` 的 `Resolve-IdRegistryEtcd` 和写路径预检。
- 测试：`issuer_test.go` 的夹具和 Binding / Recovery 用例、`issuer_observation_test.go`、`etcd_integration_test.go`、`registry_open_integration_test.go`。

### 6.2 高危：地域码绑定靠的 etcd 集群号，在本仓部署里分不出两套环境

**位置**

- 绑定只比两个集群号：`issuer.go` 的 `boundHere`（约 620 行）。`reservable`、`Init`、`Recover` 都靠它判断「是不是登记处绑的那一对」。
- 集群号的来源：`deploy/k8s/manifests/infra/etcd.yaml` 约 148–160 行、`etcd-id-secondary.yaml` 约 134–142 行。引导令牌和 peer 地址只由 `__INFRA_NAMESPACE__` 推出，没有任何集群唯一的成分。`etcd-id-registry.yaml` 也一样。
- 命名空间：`k8s_deploy.ps1` 的 `-InfraNamespace` 默认 `mmorpg-infra`；K8s 开服手册的部署示例写的也是 `InfraNamespace = "mmorpg-infra"`；开新地域手册没有要求各地域换命名空间。克隆旧配置时，命名空间必然相同。

**依据**：etcd 引导时，成员号是「排好序的 peer 地址 + 令牌」的 SHA1 前 8 字节，集群号是「排好序的成员号」的 SHA1 前 8 字节，里面没有时间和随机数。所以两个 K8s 集群只要命名空间相同，`etcd` 的集群号就相同，`etcd-id-secondary` 的也相同。佐证有三条：

- §4 演练里，同成员地址、同令牌恢复后集群号不变；
- `TestEtcdIntegration_ClustersAreIndependent` 的失败信息本身就要求用不同的 `--initial-cluster-token`；
- §2.4 的真 etcd 回归给五个节点各用一个令牌，正好绕开了生产的形态。

**触发和后果**（R 是在用的地域码，A 是原环境，B 是另一个 K8s 集群里同命名空间的环境）：

1. **克隆旧配置和快照**：B 恢复了 A 的两份本地记录（世系相同），集群号也相同，`reservable` 全部通过，B 直接发号，不需要任何运维操作。用户对问题 2 的验收「克隆旧配置和快照不得获得第二份资格」不成立。A 的下一次预留会报 `records_behind_registry`，手册 §3 把它解释成「本地两套都被旧数据覆盖过」，方向错了。
2. **重复部署同一个码，R 还没发过号（Blocks=0）**：B 的 `-id-issuer-init` 走「接着做完没做完的初始化」分支（`Init` 约 776–785 行），建出两份本地记录，输出 `region code claimed ... created`。两套都有资格。
3. **重复部署同一个码，R 已经发过号**：
   - B 的初始化和发号报 `both local reservation records are missing`，而不是 `was already claimed` 或 `not the ones bound`。错误文本和手册 §2.2 的处置表都把人引向 §4.2 的 `-id-issuer-recover -id-issuer-rebind`。
   - rebind 时集群号没变，登记处一个字节都不写，只给 B 建两份本地记录（`Recover` 约 875–894 行）。A 并不像手册 §4.2 第 3 步说的那样「从此预留被拒」，两套同时有资格。
4. **两套都有资格之后会重号**：登记处完好时，两套靠登记处的 CAS 串行，不重号。但设计 §6.2 和手册 §4.3 说能自愈的两类登记处事故，这时都会重号：
   - **登记处回档**：A 按自己的本地水位继续（`registry_behind` 分支），起点低于 B 已经发出的块。那些块只记在 B 的本地记录和回档前的登记处里，A 的新块和它们重叠。
   - **登记处丢了这个码，或者整个丢了**：手册让在用地域跑 `-id-issuer-recover`，按本地最大值重新登记。先跑的若是水位低的那一套，另一套发过的块就不在新水位之下；另一套随后再跑，只会得到 `nothing to repair`。
   - **时间下限挡不住**：B 的当前块从预留时刻往后覆盖约 256 秒的序号。A 在这段时间里按时钟抬起点，仍然落在 B 的块里。

**测试为什么没发现**：

- 内存替身把第二套环境和克隆都建模成集群号不同（`issuer_test.go` 约 1107 行的 `sibling(11, 12)`、约 1137–1138 行的 `clone(11)` / `clone(12)`，注释写着「集群号不同」）。
- 真 etcd 回归用的是五个不同的令牌。
- 两边都没建模「同命名空间 → 同集群号」这个实际部署形态。§3 提到过「完全相同清单 + 相同令牌」，但当成了罕见的恢复情形；在本仓的清单下，它就是默认情形。

**建议修法**

- **根因**：etcd 官方配置说明建议每个集群用唯一的 `--initial-cluster-token`。部署脚本给三套 etcd 的令牌加一个本 K8s 集群唯一的值，例如 `kube-system` 命名空间的 UID，由 `infra-up` 读出来渲染（DryRun 用参数或夹具值）。修好之后：
  - 不同物理集群的 etcd 集群号必然不同，上面 1–3 都被拒，4 不会发生。
  - 同一 K8s 集群里原地恢复，令牌不变、集群号不变，行为照旧。
  - 灾备重建到新的 K8s 集群，集群号会变，按 §4.2 rebind，正是设计意图。
  - 项目未上线，没有存量 etcd 要迁。改令牌会让已有的 etcd StatefulSet 滚动重启一次（有数据时 etcd 忽略引导参数），dev 和 kind 上可以接受。
- **残余**：直接拷贝 etcd 数据目录（PVC）做的克隆，仍然保留原集群号。要连这种克隆也拦住，有两个方向，需要负责人取舍：
  - 绑定里再加一个由部署脚本注入的部署身份（同样取自 K8s 集群），初始化时写进登记处，只有 rebind 能改；
  - 改成「最后提交者」围栏：每块写一个随机令牌进三份记录，两套环境分开之后，先提交的一方保留资格。代价是「本地两套同时回档」从自动继续变成拒发、走恢复。
- **文档**：下面几处都要写明，拦截依赖唯一令牌：
  - 设计 §6.2 表里「另一套环境配了同一个地域码、或者克隆了快照」一行，§6.3「地域码唯一绑定」一段；
  - 手册 §1、§2.2、§3、§4.2；
  - 开新地域手册 §1、§6。

  处置表还要补：另一套同码环境也会表现为 `records_behind_registry` 或 `both local reservation records are missing`。
- **登记处身份（低危，一并考虑）**：地域配置只有登记处地址，没有钉住「是哪一套登记处」。在第二个集群误带 `-HostIdRegistry`，再照手册 §2.2 处置表里「not opened → 新世界走 §2.1」去开启，就多了一个世界，用过的码可以被重复认领。令牌修好之后，可以在地域配置里钉住登记处的集群号。

**要补的运行证据**（交 Codex，只验假设，不改产品代码）

- 假设 H1：令牌和 peer 地址都相同的两个 etcd，集群号相同。
- 假设 H2：在这种形态下，现有代码会让第二套环境拿到资格。

按 §2.4 起五个节点，只改两处：`p2` 用和 `p` 相同的 `--name p --initial-cluster-token idissuer-it-p`，`s2` 用和 `s` 相同的 `--name s --initial-cluster-token idissuer-it-s`。peer 地址本来都是 `http://127.0.0.1:2380`，端口和环境变量照旧。

先核 H1，比较 `p` 与 `p2`、`s` 与 `s2` 输出里的 `cluster_id`：

```powershell
foreach ($n in @('p', 'p2', 's', 's2')) { docker exec "idissuer-it-$n" /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 endpoint status -w json }
```

再核 H2，工作目录 `go/data_service`：

```powershell
go test -mod=readonly -tags etcd_integration -count=1 -v -timeout 2m -run 'TestEtcdIntegration_SecondPairWithTheSameCodeIsRefused' ./internal/idissuer/
```

修复前的预期：

- 两组 `cluster_id` 分别相同；
- 用例在 Init 那一步失败，报 `both local reservation records are missing`（`ErrCounterMissing`），而不是期望的 `ErrRegionClaimed`。

这个失败证明问题存在，不是回归，不要改断言。用完按完整 CID 清理容器。

修复后的绿灯：

- 部署契约新增两条：两次渲染只差「集群唯一值」时，三套令牌都不同；同一集群重复渲染时，令牌稳定。
- 真 etcd 回归改用部署清单渲染出的令牌形态起 `p2` / `s2`，§2.4 的四条照旧通过。
- 如果选了「部署身份」方向，再加内存用例：
  - `sibling(1, 2)`、`clone(1)` / `clone(2)` 这种集群号相同、部署身份不同的环境，初始化报 `ErrRegionClaimed`，发号报 `ErrBindingMismatch`；
  - 两套曾同时有资格时，登记处回档后再领段，用 `allocated` 断言不重叠。

### 6.3 中危：退役不核绑定，任何能连登记处的配置都能永久退役一个在用的码

- **位置**：`issuer.go` 的 `Retire`（约 902–922 行）。读到这个码，只要还没退役就改成退役，不看本地记录、世系和集群号。
- **触发**：
  - 手册 §4.3 第 3 步要运维「复制一份 data-service 配置,把 Region 改成那个码,跑 -id-issuer-retire」；
  - staging 和 prod 共用一个登记处（手册 §2.1、开新地域手册 §3）。

  码填错，或者在 staging 的配置里填了生产码，都会直接退役一个在用的生产码。
- **后果**：那个地域发完手里的块就停，峰值时约一小时，低峰更久。没有撤销退役的入口，只能换新码重新部署、初始化。不会重号（新码的值域不相交），但会停服，还会烧掉一个码，一共只有 63 个。
- **建议**：
  - 码在登记处是 active 时，`Retire` 要求从绑定的那一对上执行：本地记录的地域、世系和登记处一致，集群号也一致；否则拒绝，并提示到那个地域去执行。
  - 那个地域已经不在了，就加一个显式确认开关。
  - 登记处里没有这个码（重建时补墓碑），照旧允许。
  - 补用例：从没有绑定的环境退役在用码，被拒，登记处不变。

  即使问题 1 没修，这道核对也有效，因为 staging 本地记录的地域和世系对不上生产码。

### 6.4 低危：staging 演练会手工改共用的登记处

手册 §5 要求「定期在 staging 上做」，第 6 条是「删掉登记处这个码的记录」。staging 和 prod 共用一个登记处，这一步就是在生产登记处上手工删键，和 §4.7「不要手工改记录」冲突；手滑删的是生产码，就是一次 ID 安全事件。

另外，问题 1 修好之前，第 5 条「另起一对本地 etcd、用同一个地域码部署」在同命名空间下会得到 §6.2 第 2、3 种结果，而不是手册写的 `was already claimed`。

建议：会改登记处的演练只在隔离的登记处上做（本机自动化演练，或一套演练专用的登记处），并在手册里写明。

### 6.5 证据边界

- 本节全部是静态审核，上面引用的 Codex 运行结果没有重跑。
- 11-06 的三处修复，静态看都对：
  - `Open` 的范围比较在没有键时按零值比较，恒成立；扫描后有新键写入时比较失败，重读。
  - G→P→S 读序加复核，挡住了正常并发被误报成回档。
  - 写路径预检在任何写入之前。

  三处都有 Codex 的红绿证据。
- 没有逐个审：C++ 号段调用方、logic 层的错误码映射、部署脚本里登记处以外的部分。Java 按用户要求只记待做。
- 修复版的快照演练（B 段、恢复、C 段）、`-race`、部署大套、etcd 3.6.16、staging 演练仍然没跑，见 §5.4。问题 1 与这些项无关；修完之后，要按 §6.2 补跑部署契约和真 etcd 回归。
