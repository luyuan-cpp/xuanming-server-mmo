# 交接:发号器三份记录与全局号源登记处(交 Codex 验证,2026-11-05)

> 状态:**Codex 已修复 §4.1 三项问题并补回归；最新证据见 §5。Go 固定副本构建、单测及真 etcd 回归通过；部署大套和本轮快照演练尚未全部完成。Claude Code 11-10 独立审核见 §6：发现 1 个高危问题（同命名空间部署出的 etcd 集群号相同，地域码绑定分不出两套环境），整体验收不通过，待用户决定是否修复。11-11 第二轮独立审核见 §7：同意 §6，另补修法限制与 4 个新问题（1 个中低、3 个低）。11-11 已按「令牌 + 运行期部署身份」修 §6.2、§6.3，见 §8。其余查出的问题（§6.4、§6 低危、§7.3 四项）同日在 §9 全部修完，包括登记处双向 TLS、按顺序认领、换码参数 `-IdIssuerRegion`。全部未编译、未运行，待 Codex 按 §8.3、§9.2 验证。** Java 版本轮不实现(用户 11-05:「java 先不要实现」),只在 Java 仓 `PARITY.md` 记待做。
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

## 7. 2026-11-11 Claude Code 第二轮独立审核

用户另开 Claude Code 会话，按同一份审核请求再审一遍。只读：没有改产品代码和测试，没有运行任何构建或测试（AGENTS §10.1）。对象是 `de6891478` 收录的版本：`internal/idissuer` 在工作区没有改动；`data_service.go`、`servicecontext.go`、`dev_tools.ps1`、`k8s_deploy.ps1` 的未提交改动来自别的会话（英文日志、追踪、存档库分片），不涉及发号器逻辑。

**结论：同意 §6，整体验收不通过。** §6.2 的根因按 etcd 源码核实成立。另外补两类内容：§6.2 修法的限制，以及 §6 没列的 4 个问题（1 个中低、3 个低）。

### 7.1 对 §6 的核实

- **§6.2 成立。**
  - 算法：etcd v3.6.16（部署版本）`server/etcdserver/api/membership/member.go` 的 `computeMemberID` 只哈希「排序后的 peer 地址 + 令牌」；引导时 `cluster.go` 的 `NewClusterFromURLsMap` 传 `now=nil`；`genID` 再哈希排序后的成员号。成员名不参与，没有时间和随机数。
  - 部署形态：三份清单的令牌和 peer 地址只由 `__INFRA_NAMESPACE__` 推出，开新地域手册又是一个地域一个 K8s context。所以默认部署下，每个地域报的都是同一对集群号，绑定在地域之间没有区分度。
  - 测试：内存夹具和 Codex 的真 etcd 环境都没覆盖这个形态；真 etcd 环境见证据目录 `start-etcd-tests.ps1` 第 10 行，每个节点 `--initial-cluster-token $name`。
- **补一个比克隆更常见的触发：把地域整体迁到新的 K8s 集群（命名空间不变）。** 新集群初始化报 `both local reservation records are missing`，照手册 §4.2 跑 rebind。rebind 不改登记处，手册第 3 步说的「原来那两套从此预留被拒」不会发生，切换期间新旧两边同时有资格。
- **§6.3、§6.4、§6.5 同意。** §6.5 说 `Open` 的范围比较在空范围上恒成立，这条有真 etcd 佐证：`TestEtcdIntegration_StoreContract` 在空登记处上 `Open` 成功（`11-etcd-integration-serial.log`）。

### 7.2 §6.2 修法的限制（影响验收口径）

- **只改令牌，挡不住集群级备份恢复。** Velero、VolumeSnapshot 一类工具恢复的是 PVC 里的 etcd 数据目录。集群号存在数据里，有数据时 etcd 忽略引导令牌。§6.2 把这算作残余「直接拷贝 PVC」，但它正是集群级备份恢复的标准形态，比如拿生产备份刷 staging，而 staging 和生产共用登记处。
- **「部署身份」不能放在会被一起恢复的地方。** 部署脚本渲染进 ConfigMap、Secret、环境变量的值，会和 PVC 一起被恢复回来。可行的做法有两种：data-service 在运行期从环境读（比如经 K8s API 读 `kube-system` 命名空间的 UID，要加 RBAC）；或者走「最后提交者」围栏。
- **「最后提交者」围栏要容忍一拍滞后。** 写序是本地两份先、登记处后，预留中断后本地会比登记处多走一次。规则如果写成「本地令牌等于登记处令牌」，每次中断都会把本环境自己锁死。本地记录要同时保留上一个已提交的令牌，判定按「本地当前或上一个令牌等于登记处」。
- **只修令牌时，设计和手册要写明边界。** 「克隆快照被拒」只对重新引导出来的 etcd 成立，对数据目录级的克隆不成立。

### 7.3 新问题

**中低：换码这条兜底路径和存档库命名冲突。**

- 位置：
  - 手册 §4.4 第 3 步、§4.5 把「退役旧码、用新码部署」当最后兜底，并说旧码发过的 ID 继续有效。
  - 设计 §7.2 第 2 条（已入库）规定 `storage_id = 1000000 + 地域码 × 1000 + 片号`。
  - 工作区里别的会话未提交的 `k8s_deploy.ps1` `Assert-PlayerStoreAllowlist`，要求 `MMORPG_DB_ALLOWED_DATABASES` 恰好是本地域码的 K 个库；login 的 `Placement.Region` 也由同一个 `-Region` 渲染。
- 触发：§4.4、§4.5，以及 §6.3 误退役之后的任何一次换码。
- 影响：
  - 换码之后，部署预检拒绝列出旧码的存档库；
  - 不列的话，老玩家所在的库没有写资格，存不了盘。

  不影响唯一性，影响这几条事故能不能恢复。
- 要负责人定：把存档命名用的地域号和发号器的地域码拆成两个参数，或者在预检和手册里写明换码时保留旧码的库。

**低：认领允许跳号，一次输入错误就永久烧掉一段码。**

- 位置：`etcd_store.go` 的 `ClaimNew` 只要求 `region ≥ next`，提交后 `next = region + 1`；`-id-registry-next` 只限 1..64。
- 触发：`-Region` 和 Secret 里的 `Region` 一起写错，比如 5 写成 50。`checkExpectedRegion` 只比这两处，两处是同一个人填的。重建时 `-id-registry-next` 填大也一样。
- 影响：中间的码全部作废，没有撤销入口。总共 63 个码，每次换码还要用掉一个。不影响唯一性。
- 建议：`ClaimNew` 要求 `region == next`，跳号要显式开关；重建时 `Open` 默认要求 `next` 等于已有最大码 +1。

**低：dev / test 档可以指向共用登记处，而且不受绑定约束。**

- 位置：`config.go` 的 `IssuerEndpoints` 在 `AllowAutoSeed=true` 时照收显式的 `RegistryEtcd`；`servicecontext.go` 的 `NewIdIssuer` 允许 `Mode=test` 开 `AllowAutoSeed`。这时发号器跳过独立性和绑定核对；登记处没有这个码就直接 CAS 认领，不走 `next_region`。
- 触发：部署脚本的 dev 档拒绝 `-IdRegistryEtcd`，所以只有手写配置会触发。
- 影响：这个环境成为在用码的第二个发号环境，后果和 §6.2 同类。
- 建议：`AllowAutoSeed` 时禁止显式 `RegistryEtcd`，或者限定 `Region=0`。

**低：logic 层指标的 label 基数没有封顶。**

- 位置：`id_segment_logic.go` 只按格式判定 tag，格式合法但没登记的 tag 原样记成 `biz_tag` label，和注释「label 基数必须有界」不符。
- 建议：label 只取 `EffectiveTags` 里的值。
- 顺带一处：`ErrSameCluster`、`ErrRegionMismatch` 走 default 分支，记成 `source_error` 并返回可重试错误。它们是配置错误，指标上应归 `refused`。

### 7.4 独立推演没找到问题的部分

- **唯一性的核心论证。** 登记处线性化、不回档也不丢键时，所有通过核对的预留在这个码的键上排成全序。每次成功预留的起点不低于读到的水位，而水位在 `claimAfter`、`Recover`、`Retire` 下只增不减，所以块不重叠；多个环境同码时也一样。重号的必要条件只有两种：至少两个环境有资格，且登记处这个码回档或丢失；或者三处同时回档。
- **失败与重试。** 任一步出错或结果未知，整块作废。客户端对 Txn 的自动重试最多把已提交的写看成冲突，不会误认领。快照恢复会让 revision 重复，但要三处 CAS 同时偶然命中才有害，登记处不回档时不会发生。
- **打散和调用方。** 打散在 63 位上每步可逆，Go 与 C++ 逐位一致。Go `client.go`、C++ `guid_segment_client.cpp` 都按服务端给的 `hi` 截止，不假设 `hi-lo == step`。`Minter` 没有回退；`mixIds=false` 只出现在测试里。
- **其余**：值域上限、地域位核对，以及 `Open`、`ClaimNew`、`Recover`、`Retire` 之间的并发，都没找到问题。

### 7.5 证据边界与补验证

- 全部是静态审核。etcd 集群号的算法按 v3.6.16 源码核实；3.5.15 没有单独看，§4 的演练结果与之一致。
- 没看：C++ 号段的业务调用方（只看了 `GuidSegmentClient` 本体）、部署脚本登记处以外的部分、Java。
- 没有任何运行证据：
  - 大量副本同时重启时的预留争抢（每次最多 8 次重试，登记处跨地域），不下性能结论；
  - 登记处的跨集群访问。登记处 Service 只是 ClusterIP，跨集群全靠手工配网络，也没演练过。

  §5.4 的未完成项仍未完成。
- **H1 / H2**：按 §6.2 的命令。我核过可行：`SecondPairWithTheSameCodeIsRefused` 的第二对和第一对共用键根，令牌相同时 `Init` 会落到 `ErrCounterMissing`。
- **H3**：已有数据目录的 etcd 换了引导令牌重启，集群号不变。这对应 §7.2 第一条，修复前后都应成立，不是回归。用本地已有的 3.5.15 镜像，单节点：

  ```powershell
  $img = 'quay.io/coreos/etcd:v3.5.15'
  docker volume create idissuer-h3
  docker run -d --name idissuer-h3-a -p 127.0.0.1:24811:2379 -v idissuer-h3:/etcd-data $img /usr/local/bin/etcd --name p --data-dir /etcd-data/data `
    --listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://127.0.0.1:24811 `
    --listen-peer-urls http://0.0.0.0:2380 --initial-advertise-peer-urls http://127.0.0.1:2380 `
    --initial-cluster p=http://127.0.0.1:2380 --initial-cluster-token idissuer-h3-cluster-a
  docker exec idissuer-h3-a /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 endpoint status -w json
  docker rm -f idissuer-h3-a
  docker run -d --name idissuer-h3-b -p 127.0.0.1:24812:2379 -v idissuer-h3:/etcd-data $img /usr/local/bin/etcd --name p --data-dir /etcd-data/data `
    --listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://127.0.0.1:24812 `
    --listen-peer-urls http://0.0.0.0:2380 --initial-advertise-peer-urls http://127.0.0.1:2380 `
    --initial-cluster p=http://127.0.0.1:2380 --initial-cluster-token idissuer-h3-cluster-b
  docker exec idissuer-h3-b /usr/local/bin/etcdctl --endpoints=http://127.0.0.1:2379 endpoint status -w json
  docker rm -f idissuer-h3-b
  docker volume rm idissuer-h3
  ```

  预期两次输出的 `cluster_id` 相同。Docker 目前因 §5.4 的启动故障不可用，恢复后再跑；容器和卷按上面的名字清理，不要按宽泛名称批量删。
- 其余几项靠代码就能确定，不需要运行证据。修复时各补一条内存用例：
  - 从没绑定的环境退役在用码，被拒；
  - `next=1` 时初始化 5，被拒，登记处不变；
  - `AllowAutoSeed` 带显式 `RegistryEtcd`，配置报错。

## 8. 2026-11-11 §6.2、§6.3 修复（未编译，待 Codex 验证）

用户 11-11 选「按令牌 + 运行期身份修」§6.2。用户在另一个会话「全球同服发号三个问题修复」也要求修 §6.2、§6.3，两个会话这样分工：

- 本会话「永久ID发号器与全局号源审核」：Go 代码（含 §6.3 退役保护）、设计、两本手册、AGENTS §7.1 一句。
- 另一个会话：部署侧，即 RBAC 清单、`data-service.yaml`、`k8s_deploy.ps1`、部署契约测试，见 §8.4。

没有编译，没有运行任何测试（AGENTS §10.1）。Java 不涉及：Java 版没有发号器和登记处，`PARITY.md` 里已有的发号器待做项照旧。

### 8.1 改了什么

绑定从「世系 + 两套本地 etcd 的集群号」改成「世系 + 部署身份 + 两套本地 etcd 的集群号」。部署身份是 `k8s:<kube-system 命名空间的 UID>`，data-service 运行期经 K8s API 读。

| 文件 | 改动 |
|---|---|
| `go/data_service/internal/idissuer/issuer.go` | 部署身份接入：<br>• `Options.Deployment` 是身份读取器，生产必须给，`New` 拒绝缺省；登记处记录新增 `Claim.Deployment`。<br>• `boundHere` 同时比身份和两个集群号，空身份不算绑定。<br>• `Init`、`Recover`（rebind 与重新登记）和预留都写入、核对身份。<br>• 身份第一次需要时读，成功后缓存；读失败不写任何记录，上报 `identity_error`。<br>退役保护（§6.3）：<br>• 签名改为 `Retire(ctx, unbound)`。<br>• 码在用时，`retirableHere` 要求本地记录的地域和世系、部署身份、集群号都对得上；`unbound` 跳过核对。<br>• 登记处没有这个码时照旧写墓碑。 |
| `.../idissuer/etcd_store.go` | 登记处 JSON 加可缺省的 `deployment` 字段，有值时校验格式；没有这个字段的旧记录照读，身份为空 |
| `.../idissuer/issuer_test.go` | 夹具：每个 harness 一个部署身份，`elsewhere()` 是集群号相同、身份不同的另一个 K8s 集群。新增 10 个用例，见下；`TestNew_RejectsBadOptions`、`TestValidClaim` 补了身份的情形 |
| `.../idissuer/etcd_integration_test.go` | 每个环境一个身份。`SecondPair` 用例记下两对 etcd 的集群号是否相同，设了 `MMORPG_ID_ISSUER_IT_SECOND_PAIR_SAME_TOKENS=1` 时断言相同。演练状态文件带身份，没有身份的旧状态拒绝续跑。`StoreContract` 验证 `deployment` 在真 etcd 上往返 |
| `go/data_service/internal/clusteridentity/`（新） | 只用标准库：带 ServiceAccount 令牌、只信任集群 CA，`GET /api/v1/namespaces/kube-system`，返回 `k8s:<uid>`。单测 5 个，用 `httptest` 的 TLS 服务 |
| `go/data_service/internal/svc/servicecontext.go`、`id_issuer_region_test.go` | `AllowAutoSeed=false` 时接上 `clusteridentity.InCluster().Read`，dev 不接。新增接线用例 `TestNewIdIssuer_ProductionWiresTheDeploymentIdentity` |
| `go/data_service/data_service.go`、`data_service_entry_test.go` | 新开关 `-id-issuer-retire-unbound`，只能和 `-id-issuer-retire` 一起用；rebind、init 的输出带部署身份；`TestSelectEntry` 加 3 行 |
| `go/data_service/internal/metrics/metrics.go` | 预留结果的帮助文本加 `identity_error` |
| 文档 | 设计 §6.1~§6.3；发号器手册的状态行、§1、§2.2、§3、§4、§4.6、§4.7、§5；开新地域手册 §1、§6；AGENTS §7.1 一句 |

新增的 10 个 `idissuer` 用例：

- `TestBinding_AnotherKubernetesClusterWithIdenticalEtcdIsRefused`：§6.2 默认形态，码已发号，另一个集群初始化报 `ErrRegionClaimed` 而不是 `ErrCounterMissing`；
- `TestBinding_AnotherKubernetesClusterCannotFinishAnUnusedClaim`：码已认领、`Blocks=0`；
- `TestBinding_BackupRestoredIntoAnotherKubernetesClusterIsRefused`：数据目录原样恢复到另一个集群；
- `TestBinding_RebindToAnotherKubernetesClusterFencesTheOldOne`：灾备 rebind 后原集群被拒，两边的段不重叠；
- `TestBinding_ClaimWithoutDeploymentNeedsRebind`；
- `TestReserve_DeploymentIdentityFailureWritesNothingAndSuccessIsCached`；
- `TestDeployment_MalformedIdentityIsRefused`；
- `TestRetire_ACodeInUseIsRefusedFromAnUnboundDeployment`；
- `TestRetire_UnboundNeedsTheExplicitSwitch`；
- `TestClaimJSON_CarriesTheDeployment`。

`clusteridentity` 的 5 个用例：成功、不在 K8s 里、403 点名缺的权限、对象不对、证书不是集群 CA 签的。

### 8.2 设计取舍

- **身份为什么运行期读、不由部署脚本渲染**：渲染进 ConfigMap、Secret、环境变量的值，会和 PVC 一起被集群级备份恢复带走（§7.2）。kube-system 的 UID 由 API server 建集群时生成，按对象恢复出来的是新 UID。这是 K8s 生态里常用的集群标识。
- **为什么不用 client-go**：data_service 只要这一个字段。client-go 在 data_service 的 go.mod 里只是间接依赖，提成直接依赖会牵连一串传递包的校验和，而服务按 `-mod=readonly` 构建。
- **K8s API 是软依赖**：构造不做 IO，第一次预留或运维操作才读，成功后缓存到进程结束。读不到那次失败、不写任何记录；手里的块照发，下一次预留再读。
- **etcd 令牌也带上 kube-system UID**：这是加固，判定靠部署身份。同一个 K8s 集群里的不同命名空间，令牌本来就带命名空间，集群号不同。
- **兼容**：
  - 11-11 之前写的登记处记录没有身份，生产预留拒绝，`-id-issuer-recover -id-issuer-rebind` 补上。项目未上线，没有存量；dev 不核绑定。
  - 新旧二进制不能混跑：旧二进制按严格 JSON 读到 `deployment` 字段会当作记录损坏，fail-closed。
  - 11-06 的演练 A 段状态没有身份，`TestEtcdDrill` 的 B、C 段会明确拒绝，演练要从 A 段重来。旧证据保留不动。
- **仍然拦不住**：
  - 整个 K8s 控制面连同 kube-system 一起从备份克隆出第二个集群；
  - 同一个 K8s 集群里拷贝 etcd 数据目录、强制起成另一套。

  两种都要有人刻意操作，这时两边只能靠登记处水位串行，登记处完好时不重号，已写进设计 §6.3。

### 8.3 交 Codex 验证（Go 与真 etcd）

工作目录 `go/data_service`，沿用 §5.3 的 `GOTOOLCHAIN=local`、`-mod=readonly`。整模块构建会带上工作区里其他会话的未提交改动；发号器以外的编译失败单独报告，不要顺手修。

```powershell
gofmt -l ./internal/idissuer ./internal/clusteridentity ./internal/svc ./internal/metrics ./data_service.go ./data_service_entry_test.go
go build -mod=readonly ./...
go vet -mod=readonly ./internal/idissuer/ ./internal/clusteridentity/ ./internal/svc/ ./internal/logic/ ./internal/metrics/ .
go vet -mod=readonly -tags etcd_integration ./internal/idissuer/
go test -mod=readonly -count=1 -v -timeout 3m ./internal/idissuer/ ./internal/clusteridentity/
go test -mod=readonly -count=1 -v -timeout 2m -run 'TestNewIdIssuer|TestCheckExpectedRegion|TestSelectEntry' ./internal/svc/ .
go test -mod=readonly -count=1 ./internal/logic/ ./internal/config/
```

通过标准：

- 全部 exit 0，`gofmt -l` 没有输出；
- §8.1 列的新用例都实际运行并 PASS，`idissuer`、`clusteridentity` 两个包 0 SKIP；
- Windows 上报 Access denied 时，改用 `go test -c` 再运行。

有 CGO 的环境再跑一遍：`go test -race -count=1 ./internal/idissuer/ ./internal/clusteridentity/`。

**真 etcd 回归，用部署的默认形态**：第二对的成员名和引导令牌与第一对相同，peer 地址本来就相同，所以集群号相同。Docker 修好之后，按 §5.4 先核实并清理旧资源，再起五个节点：

```powershell
$img = 'quay.io/coreos/etcd:v3.5.15'
$nodes = @(
  @{ c = 'p';  name = 'p'; token = 'idissuer-it-p'; port = 24791 },
  @{ c = 's';  name = 's'; token = 'idissuer-it-s'; port = 24792 },
  @{ c = 'r';  name = 'r'; token = 'idissuer-it-r'; port = 24793 },
  @{ c = 'p2'; name = 'p'; token = 'idissuer-it-p'; port = 24794 },
  @{ c = 's2'; name = 's'; token = 'idissuer-it-s'; port = 24795 }
)
foreach ($x in $nodes) {
  docker run -d --name "idissuer-it-$($x.c)" -p "127.0.0.1:$($x.port):2379" $img /usr/local/bin/etcd --name $x.name --data-dir /etcd-data `
    --listen-client-urls http://0.0.0.0:2379 --advertise-client-urls "http://127.0.0.1:$($x.port)" `
    --listen-peer-urls http://0.0.0.0:2380 --initial-advertise-peer-urls http://127.0.0.1:2380 `
    --initial-cluster "$($x.name)=http://127.0.0.1:2380" --initial-cluster-token $x.token
}
$env:MMORPG_ID_ISSUER_IT_PRIMARY = '127.0.0.1:24791'
$env:MMORPG_ID_ISSUER_IT_SECONDARY = '127.0.0.1:24792'
$env:MMORPG_ID_ISSUER_IT_REGISTRY = '127.0.0.1:24793'
$env:MMORPG_ID_ISSUER_IT_PRIMARY2 = '127.0.0.1:24794'
$env:MMORPG_ID_ISSUER_IT_SECONDARY2 = '127.0.0.1:24795'
$env:MMORPG_ID_ISSUER_IT_SECOND_PAIR_SAME_TOKENS = '1'
go test -mod=readonly -tags etcd_integration -count=1 -v -timeout 2m -run 'TestEtcdIntegration' ./internal/idissuer/
```

通过标准：

- 5 条 `TestEtcdIntegration_*` 全部 PASS、0 SKIP；
- 日志里有 `the second pair reports the same etcd cluster ids as the first: true`。这一行同时证明 §6.2 的 H1：令牌和地址相同，集群号就相同。

用完按上面五个名字删容器。H2（修复前的失败形态）不再需要单独复现：修复前的代码在这个环境里会在 `Init` 报 `ErrCounterMissing`。

**快照回档演练**按 §2.5 从 A 段重来。用新的状态文件路径和新的命名卷，不覆盖 11-06 的 `drill/` 证据。§7.5 的 H3（数据目录不变、换令牌重启，集群号不变）照旧可以跑，它证明的是「只改令牌挡不住数据级克隆」，与本次修复的结论一致。

### 8.4 部署侧（「全球同服发号三个问题修复」会话负责）

本会话和它约定的名字与行为：

- **RBAC 清单** `deploy/k8s/manifests/go-svc/data-service-identity-rbac.yaml`：
  - 每个 zone 命名空间一个 ServiceAccount `data-service`；
  - 全集群一个 ClusterRole，只允许 get `namespaces`，`resourceNames: ["kube-system"]`；
  - 每个 zone 一个 ClusterRoleBinding，名字带上 zone 命名空间。

  `data-service.yaml` 的 Pod 用这个 ServiceAccount，不关闭令牌挂载。NetworkPolicy 如果限制出口，要放行到 API server。RBAC 必须先于 data-service 的 StatefulSet apply。
- **etcd 引导令牌**：形态是 `mmorpg-etcd-__INFRA_NAMESPACE__-__KUBE_CLUSTER_UID__`，secondary 和 registry 同样追加。
  - `Apply-Infra` 在第一个写操作之前用 `kubectl get namespace kube-system -o jsonpath={.metadata.uid}` 读取，按 UUID 校验，读不到就拒绝、什么都不写；
  - DryRun 用固定标记值；
  - 已有数据的 etcd 忽略令牌，改它只触发一次滚动重启。

**部署侧回执（2026-11-11，「全球同服发号三个问题修复」会话写；未运行任何测试）**

改动文件：

| 文件 | 改动 |
|---|---|
| `deploy/k8s/manifests/go-svc/data-service-identity-rbac.yaml`（新） | ServiceAccount `data-service`；ClusterRole `mmorpg-cluster-identity-reader`，只有 get `namespaces`、`resourceNames: ["kube-system"]`；ClusterRoleBinding `mmorpg-cluster-identity-__ZONE_NAMESPACE__`，subject 写明命名空间 |
| `deploy/k8s/manifests/go-svc/data-service.yaml` | Pod 模板加 `serviceAccountName: data-service`，不关令牌挂载 |
| `deploy/k8s/manifests/infra/etcd.yaml`、`etcd-id-secondary.yaml`、`etcd-id-registry.yaml` | `ETCD_INITIAL_CLUSTER_TOKEN` 追加 `-__KUBE_CLUSTER_UID__` |
| `tools/scripts/k8s_deploy.ps1` | 新函数 `Get-KubeClusterUid`：读 `kube-system` 的 UID，只认小写 UUID，读不到抛错并写明 `Nothing was written.`，DryRun 返回全零标记。<br>`Apply-Infra`：在 `Ensure-Namespace` 之前调用它，打一行 `Kubernetes cluster uid: …`，循环里替换 `__KUBE_CLUSTER_UID__`。<br>新函数 `New-DataServiceIdentityRbacYaml`：按 zone 命名空间渲染，占位没换干净就抛错。<br>`Apply-OneGoSvc`：只给 data-service 渲染（在 Secret、ConfigMap 写入之前），在稳定版和金丝雀的工作负载之前 apply。<br>`Remove-Zone`：删命名空间后再删本 zone 的 ClusterRoleBinding（`--ignore-not-found`）；共用的 ClusterRole 不删 |
| `tools/scripts/tests/id_issuer_deploy_contract.tests.ps1` | 改两条旧令牌断言，新增 6 条：三套令牌的形态、`Apply-Infra` 读取先于第一个写操作、`Get-KubeClusterUid` 的成功 / 四种拒绝 / DryRun（按 AST 抽函数，kubectl 用同名函数替身）、RBAC 清单形状与 data-service 的 ServiceAccount、RBAC 渲染与占位拒绝、`Apply-OneGoSvc` 的渲染与 apply 顺序和 `Remove-Zone` |
| `tools/scripts/tests/k8s_migrate_gate.tests.ps1` | 恢复真函数 `New-DataServiceIdentityRbacYaml`；kubectl 替身认 ClusterRoleBinding 清单，记 `apply:IdentityRbac`；data-service 用例断言 RBAC 先于工作负载、绑定名和 subject 带 `audit-zone`、Pod 用 `serviceAccountName: data-service`；Prepare 用例断言不 apply RBAC |
| `tools/scripts/tests/k8s_deploy_contract.tests.ps1` | 末尾新增 2 条，复用已有的 DryRun 输出：zone-up 里 RBAC 先于 data-service 的 StatefulSet、绑定落在 `mmorpg-zone-contract-test`；infra-up 里主 etcd 的令牌是 `mmorpg-etcd-contract-battle-infra-00000000-0000-0000-0000-000000000000`，不留占位 |
| `deploy/k8s/README.md`、`deploy/k8s/AGENTS.md` | 新增「地域码绑定到 K8s 集群」一条，AGENTS 的发号器一条补一句 |

DryRun 输出里的样子（默认 `-InfraNamespace mmorpg-infra`）：

```text
Kubernetes cluster uid: 00000000-0000-0000-0000-000000000000 [dry-run marker] (carried by the etcd bootstrap tokens)
value: "mmorpg-etcd-mmorpg-infra-00000000-0000-0000-0000-000000000000"
value: "mmorpg-etcd-id-secondary-mmorpg-infra-00000000-0000-0000-0000-000000000000"     # staging / prod
value: "mmorpg-etcd-id-registry-mmorpg-infra-00000000-0000-0000-0000-000000000000"      # -HostIdRegistry
```

真部署时全零换成 `kubectl get namespace kube-system -o jsonpath={.metadata.uid}` 读到的值。部署账号要能读 `namespaces/kube-system`，还要能建 ClusterRole / ClusterRoleBinding（一般就是集群管理员）。已有数据的 etcd 忽略引导令牌，改令牌只触发一次滚动重启；有 C++ 节点在跑的集群，主 etcd 的这次滚动放进全栈维护窗口。项目未上线，首次部署没有这个问题。

**交 Codex（部署侧，仓库根目录，按顺序；失败保留完整输出，不连续重试）**：

```powershell
pwsh -NoProfile -Command "`$e = `$null; [void][System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path 'tools/scripts/k8s_deploy.ps1'), [ref]`$null, [ref]`$e); if (`$e) { `$e | ForEach-Object { `$_.ToString() }; exit 1 }"
pwsh -NoProfile -File tools/scripts/tests/id_issuer_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/etcd_maintenance_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/logical_db_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
```

通过标准：

- 解析无错误；`id_issuer_deploy_contract`、`k8s_migrate_gate`、`etcd_maintenance_contract` 全部 PASS、退出 0。
- `k8s_deploy_contract` 末尾两条新用例 PASS。§5.4 记过这套在别的领域有既有失败，逐条列出失败名，按是否涉及 data-service、RBAC、etcd 令牌分开报告，不要改断言放行。
- 后两套是回归：zone-up 的 DryRun 多了一块 RBAC 清单，若有按块计数的用例变红，报出用例名和输出。

可选 kind 冒烟（`kind-mmorpg` 上 `infra-up`、`zone-up` 之后）：

```powershell
kubectl --context kind-mmorpg get namespace kube-system -o jsonpath='{.metadata.uid}'
kubectl --context kind-mmorpg -n mmorpg-infra get sts etcd -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="ETCD_INITIAL_CLUSTER_TOKEN")].value}'
kubectl --context kind-mmorpg auth can-i get namespaces/kube-system --as=system:serviceaccount:<zone 命名空间>:data-service
kubectl --context kind-mmorpg auth can-i list namespaces --as=system:serviceaccount:<zone 命名空间>:data-service
```

期望：令牌以第一条读到的 UID 结尾；第三条 `yes`，第四条 `no`。dev 档 data-service 不读身份，Go 侧读取逻辑由 `clusteridentity` 单测覆盖。PROGRESS 由本会话单独追加。

### 8.5 仍未解决（11-11 第二批已全部处理，见 §9）

- §6.4：staging 演练第 6 步在共用登记处上手工删键。§9 改为在隔离的登记处上演练。
- §6 低危「地域配置没钉住登记处是哪一套」：§9 由登记处双向 TLS 钉住。
- §7.3 四项：换码与存档库命名冲突、认领允许跳号、dev/test 档可指向共用登记处、指标 label 基数。§9 逐项修了。

## 9. 2026-11-11 第二批：查出的问题全部修完（未编译、未运行，待 Codex 验证）

用户要求这条线上查出的问题全部现在修，不等压测。两个会话按文件分工，不交叉：

- 「永久ID发号器与全局号源审核」：登记处双向 TLS 的 Go 侧、发号器手册、开新地域手册、设计 §6.3。
- 「全球同服发号三个问题修复」：下表其余 Go 改动、全部部署侧、AGENTS §7.1、本节、PROGRESS。

那个会话中途结束，这一节由后者补写。

### 9.1 改了什么

| 问题 | 修法 | 文件 |
|---|---|---|
| §7.3 认领允许跳号 | 只能认领「下一个可认领的码」本身，大于它报 `ErrRegionOutOfOrder`，什么都不写。`Open` 规则不变，理由见设计 §6.3「开启登记处不强制等于已有最大码 +1」 | `idissuer/issuer.go`、`etcd_store.go`、`issuer_test.go`（`TestInit_RefusesACodeAboveTheNextClaimableOne`）、`etcd_integration_test.go`（`StoreContract` 加跳号一行） |
| §7.3 dev/test 可指向共用登记处 | `AllowAutoSeed=true` 时显式 `RegistryEtcd` 或 `RegistryTLS` 都报错 | `config/config.go`、`config_test.go` |
| §7.3 指标 label 基数 | label 只取登记过的 biz_tag，其余记 `unregistered` / `invalid`；`ErrSameCluster`、`ErrRegionMismatch` 归 `refused`，不回 gRPC 错误 | `logic/id_segment_logic.go`、`_test.go`（`TestBizTagLabel_OnlyRegisteredTagsBecomeLabels`），`metrics/metrics.go` 的 Help |
| §7.3 换码与存档库命名冲突 | 部署新参数 `-IdIssuerRegion`（缺省等于 `-Region`），只渲染 data-service 的 `Issuer.Region` 和 `MMORPG_ID_ISSUER_EXPECTED_REGION`。存档库清单、mysql-init 预建库、login 的 `Placement.Region` 用新函数 `Resolve-RegionCode`，只认 `-Region` | `k8s_deploy.ps1`、`dev_tools.ps1`；`svc/servicecontext.go` 报错文字、`login/internal/config/config.go` 注释 |
| §6 登记处身份没钉住；原 §3「登记处没开认证」 | 登记处只走双向 TLS，世界 CA 签服务端和客户端证书：<br>• Go：`idissuer/registry_tls.go` 的 `LoadRegistryTLS`；`NewEtcdRegistry` 带 TLS；`config` 的 `RegistryTLS`（生产三项必填，dev 必须空）；svc 装配。<br>• 部署：`etcd-id-registry.yaml` 改 https 并开 `--client-cert-auth`，证书来自 Secret `etcd-id-registry-tls`，探针和抓取走 2381；`data-service.yaml` 挂 Secret `id-registry-client-tls` 到 `/etc/mmorpg/id-registry-tls`；ConfigMap 模板写三个路径；`Assert-TlsSecretProvisioned` 在写操作之前核对两份 Secret | Go：`registry_tls.go`、`registry_tls_test.go`、`registry_tls_integration_test.go`、`etcd_store.go`、`config.go`、`svc/servicecontext.go`。部署：上述清单、`k8s_deploy.ps1` |
| §6.4 staging 演练改共用登记处 | 演练改在隔离的登记处上做 | 发号器手册 §5 |

部署侧的契约测试：
- `id_issuer_deploy_contract`：TLS 清单、两处 Secret 核对的位置、`Assert-TlsSecretProvisioned` 的放行和四种拒绝。
- `logical_db_deploy_contract`：换码函数、换码 DryRun、TLS 路径 DryRun。
- `k8s_client_entry_contract`：转发名单。
- `etcd_maintenance_contract`：挂载点只认数据卷，登记处抓取端口 2381。
- `k8s_migrate_gate`：TLS 核对先于 ConfigMap，并核证书卷。

文档：发号器手册（§1、§2.2 错误表、§4.4、§4.5、§4.7、§5、§6 证书签发）、开新地域手册、设计 §6.3、`deploy/k8s/README.md`、AGENTS §7.1。

**钉住登记处靠什么、还剩什么**：地域只认世界 CA 签的登记处证书；CA 私钥离线保存，不进集群。所以地址写错、或者别的集群误起了一套登记处，握手都会失败。认领又只能按顺序，误开的第二个世界里也认领不了台账上分配的码。剩下一种刻意操作拦不住：把真登记处的服务端证书 Secret 拷到另一个集群，在那里起登记处并开启。手册写明服务端证书只发给托管集群。

### 9.2 交 Codex

失败保留完整输出，不连续重试，不放宽断言。工作区混有其他会话的改动；发号器以外的失败单独报告，不要顺手修。

**① Go（工作目录 `go/data_service`，`GOTOOLCHAIN=local`）**：

```powershell
gofmt -l ./internal/idissuer ./internal/clusteridentity ./internal/config ./internal/logic ./internal/metrics ./internal/svc ./data_service.go ./data_service_entry_test.go
go build -mod=readonly ./...
go vet -mod=readonly ./internal/idissuer/ ./internal/clusteridentity/ ./internal/config/ ./internal/logic/ ./internal/metrics/ ./internal/svc/ .
go vet -mod=readonly -tags etcd_integration ./internal/idissuer/
go test -mod=readonly -count=1 -v -timeout 3m ./internal/idissuer/ ./internal/clusteridentity/ ./internal/config/ ./internal/logic/
go test -mod=readonly -count=1 -v -timeout 2m -run 'TestNewIdIssuer|TestCheckExpectedRegion|TestSelectEntry' ./internal/svc/ .
```

通过标准：
- 全部 exit 0，`gofmt -l` 无输出；
- §8.1 的 10 个用例，加上本节的 `TestInit_RefusesACodeAboveTheNextClaimableOne`、`TestBizTagLabel_OnlyRegisteredTagsBecomeLabels`、`TestIdSegmentRegistryTLSFiles`、`TestLoadRegistryTLS_*` 4 个、`TestIdSegmentIssuerEndpoints` 里两个 dev 拒绝子例，都实际 PASS，`idissuer`、`clusteridentity` 0 SKIP；
- 有 CGO 时再跑 `go test -race -count=1 ./internal/idissuer/ ./internal/clusteridentity/`。

在 `go/login` 目录跑 `go build -mod=readonly ./...`：只改了注释，确认能编。

**② 真 etcd（Docker 修好之后）**：
- 先按 §8.3 起五个节点，用默认形态的同名同令牌，跑 `TestEtcdIntegration`，含 `StoreContract` 的跳号一行。
- 再跑双向 TLS：

```powershell
$dir = Join-Path $env:TEMP ('idissuer-tls-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory $dir | Out-Null
$env:MMORPG_ID_ISSUER_IT_TLS_FIXTURE_OUT = $dir
go test -mod=readonly -tags etcd_integration -count=1 -v -run 'TestEtcdIntegration_WriteRegistryTLSFixture' ./internal/idissuer/
docker run -d --name idissuer-it-tls -p 127.0.0.1:24796:2379 -v "${dir}:/tls:ro" quay.io/coreos/etcd:v3.5.15 /usr/local/bin/etcd --name tls --data-dir /etcd-data `
  --listen-client-urls https://0.0.0.0:2379 --advertise-client-urls https://127.0.0.1:24796 `
  --client-cert-auth --trusted-ca-file /tls/ca.crt --cert-file /tls/server.crt --key-file /tls/server.key `
  --listen-peer-urls http://0.0.0.0:2380 --initial-advertise-peer-urls http://127.0.0.1:2380 `
  --initial-cluster tls=http://127.0.0.1:2380 --initial-cluster-token idissuer-it-tls
$env:MMORPG_ID_ISSUER_IT_TLS_REGISTRY = '127.0.0.1:24796'
$env:MMORPG_ID_ISSUER_IT_TLS_DIR = $dir
go test -mod=readonly -tags etcd_integration -count=1 -v -run 'TestEtcdIntegration_RegistryOverMutualTLS' ./internal/idissuer/
docker rm -f idissuer-it-tls
Remove-Item -Recurse -Force $dir
```

通过标准：`RegistryOverMutualTLS` PASS、0 SKIP。容器只按上面的名字删。

**③ PowerShell（仓库根目录，按顺序）**：

```powershell
pwsh -NoProfile -Command "`$e = `$null; foreach (`$f in 'tools/scripts/k8s_deploy.ps1', 'tools/scripts/dev_tools.ps1') { [void][System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path `$f), [ref]`$null, [ref]`$e); if (`$e) { `$e | ForEach-Object { `$_.ToString() }; exit 1 } }"
pwsh -NoProfile -File tools/scripts/tests/id_issuer_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/etcd_maintenance_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/logical_db_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
```

通过标准：
- 解析无错，前五套全部 PASS、退出 0；
- `k8s_deploy_contract` 末尾的两条发号器用例 PASS，这套已知的其他失败逐条列名、分开报告。

**④ 可选 kind 冒烟**：按 §8.4 末尾的四条 kubectl。

### 9.3 上线前要做的（不是代码）

- 运维建世界 CA，私钥离线保存。给托管集群签登记处服务端证书，SAN 覆盖各地域 `-IdRegistryEtcd` 用的主机名和 `etcd-id-registry.<infra 命名空间>`；给每个地域签客户端证书。
- 托管集群 infra 命名空间预置 `etcd-id-registry-tls`。每个 zone 命名空间预置 `id-registry-client-tls`，zone 命名空间要先建。
- 运维预置的 data_service 配置写 `IdSegment.Issuer.RegistryTLS` 三个路径。
- 部署账号能读 `namespaces/kube-system`，能建 ClusterRole / ClusterRoleBinding。

Java 0.1.0-SNAPSHOT：Java 版没有发号器和登记处，`PARITY.md` 11-05 的发号器待做照旧。
