# Region → Cell 确定性蜂窝路由对齐设计（`go/shared/cellroute`）

> 日期：2026-11-24  
> 对标来源：`xuanming-server/pkg/cellroute` 与 `pkg/cellroute/etcdtable`  
> 状态：核心 4096 槽位确定性路由器、`AtomicTable`、`etcdwatch` 热更及 `go/login` 可选装配已落地并通过单测。  
> Java 独立版（`xuanming-server-mmo`）状态：本阶段为服务端内部无状态路由基础组件（默认未配置不启用），未修改客户端可见 proto/配表，暂无需同步 `xuanming-server-mmo`；后续在登录回包或网关转发正式暴露 `region_id`/`cell_id` 时同步 `PARITY.md`。

---

## 1. 需求分析

`xuanming-server-mmo-cpp` 已有面向存储层的 `shared/placement`（`player_store_{storage_id}_db`）与 `shared/dirshard`（64 逻辑目录分片），用于决定持久存档落在哪个数据库；但在面向百万级 CCU 的无状态计算/接入层（如多 Region / 多 Cell 的 `login`、`gate`、`push` 亲和路由与按区间平滑扩缩容）尚缺轻量的确定性算点路由层。

将 `xuanming-server/pkg/cellroute`（4096 逻辑槽位 → 物理 `(RegionID, CellID)` + `AtomicTable` 无锁热更）移植到 `go/shared/cellroute`，并直接复用本仓成熟的 `shared/etcdwatch` 三道防线（KeepAlive + RequireLeader + 周期全量对账）实现热更新。

---

## 2. 设计分析

1. **与 `shared/placement` 的清晰分工**：
   - `shared/placement` / `dirshard`：管**持久化数据存储落库**（建角后写入 `player_route.storage_id`，终生不变，扩容靠搬库）；
   - `shared/cellroute`：管**在线无状态接入/计算亲和落点**（`logical_cell = player_id % 4096` → 查内存只读表得 `(RegionID, CellID)`，扩容时通过 etcd 热切部分逻辑槽区间到新 Cell，全过程算而不查库）。
2. **核心原语（`go/shared/cellroute`）**：
   - `LogicalCellCount = 4096`、`LogicalRegionCount = 64`（与 `xuanming-server` 逐项一致）；
   - `StaticTable`（不可变 4096 项表，构造期校验所有 `CellID` 均在 `regionOfCell` 登记且 `RegionID` 一致，结构性杜绝 Region/Cell 错配）；
   - `AtomicTable`（`atomic.Pointer[StaticTable]` 无锁热替换，读路径零锁）；
   - `BuildBalancedEntries([]CellSpec)`（连续区间均分铺表，便于后续按段迁移）；
   - `ParseTableJSON` / `MarshalTableJSON`（与 etcd KV 存储互通）；
   - `WatchEtcd(ctx, client, key, atomicTable, interval)`：复用 `shared/etcdwatch.Watch`，任何坏表/截断/错配均 fail-closed 保留旧表并打英文 ERROR 日志。
3. **服务接线（`go/login`）**：
   - `login/internal/config.Config` 新增可选 `CellRoute cellroute.Conf`（支持 `Mode: "" | "static" | "etcd"`）；
   - `login/internal/svc/servicecontext.go` 装配 `CellRouter *cellroute.Router`（默认 `Mode == ""` 时为 `nil`，零行为变化；配置非法时启动期 fail-fast 拒启，停止时释放 watch goroutine）。

---

## 3. 思想分析

- **复用既有底座，不造轮子**：`mmo-cpp` 已有经压测打磨的 `shared/etcdwatch`，`cellroute` 直接挂在其之上，既继承断连重连、RequireLeader 与周期全量对账，又保持代码极简。
- **零锁热路径 + fail-closed 热更**：在线每次路由仅为一次取模 + 一次 `atomic.Pointer.Load` + 切片下标读取；热更只在新表完整校验通过后原子切指针。

---

## 4. 漏洞分析

| 隐患 | 防护机制 |
|---|---|
| 运维推送缺槽或错配 Region 的坏路由 JSON | `ParseTableJSON` 与 `NewStaticTable` 强校验 4096 项与 `regionOfCell` 自洽，失败拒绝替换、保留旧表 |
| etcd 网络闪断导致路由表永久陈旧 | 通过 `shared/etcdwatch.Watch` 的 `WithRequireLeader` + 指数退避重连 + 周期全量对账兜底 |
| `player_id == 0` 误走路由落入 0 号 Cell | `Router.Route(0)` 显式报错拒绝 |

---

## 5. 步骤分析

1. 落地 `go/shared/cellroute/{cellroute.go,codec.go,etcdwatch.go,cellroute_test.go}`；
2. 接线 `go/login/internal/config` 与 `go/login/internal/svc/servicecontext.go`（含 `Stop()` 清理与配置校验），补 `cellroute_test.go`；
3. 后续按百万 CCU 排期将 `CellRouter` 透出到多 Region 选 Gate 与跨区消息桥。

---

## 6. 执行步骤与验收口径

- `cd go/shared && go test -count=1 ./cellroute/... && go vet ./cellroute/...`；
- `cd go/login && go test -count=1 ./internal/config/... ./internal/svc/... && go vet ./internal/config/... ./internal/svc/...`；
- `python tools/scripts/check_log_text_english.py` 零违例。
