# 配置表热更：表包、轨道指针与进程内整批切换（2026-11-20）

> **状态**：**全部 5 批已落码（2026-11-20，随 `f067d01fc` 入库）**。之后别的线已全量导表（manifest 第 2 版），顺带编过 scene / battle，跑过 `scene.exe --validate-tables`；热更这条线自己的单测、预检、本机冒烟与集群端到端都还没跑。C++ 侧的单测 11-26 补上了（15 条，未编译），同时把表包管理器与 battle 的旧表保留期换成了可注入的时钟。**11-26 晚逐行审查五批代码，查出并修掉 7 处逻辑问题**（sidecar 与发布工具读不了导表器写出的清单、C++ 状态文件字段名与约定不一致、表包没同步下来时重启的进程静默退回镜像旧表、过期的拒绝结论不清、管理接口把上一包的拒绝算到新包头上、scene 三项表校验没接进热更闸门、C++ 启动不核对清单摘要）；**同晚第二轮审部署脚本与可观测性，又修了 8 处**（两个 zone 的同名 Pod 互相覆盖状态键、外接对象存储时不核对凭据 Secret、sidecar 指标没人抓、收敛告警把 stable 与 canary 混着比、需要重启的告警判据写不出「还没重启」、卷的大小上限与表包上限对不上且临时文件不清、解包目录权限依赖两个镜像用户号碰巧相同、状态键每 5 秒无条件写一次），两轮的根因与改法见 §19，连同配套单测都未编译。代码没有待写项。剩下的编译、测试、冒烟与登记交 Gemini，要改代码的交回 Claude，见[交接单](../handoff/config-table-hot-reload-handoff-20261126.md)。独立 Java 仓留到 `PARITY.md` 待做。运维手册见 [docs/ops/table-hot-reload-runbook.md](../ops/table-hot-reload-runbook.md)。
> **相关**：[RPC 热关停](rpc-kill-switch.md)（同一种「etcd 事实源 + 管理接口 + 各进程自己生效」的形态）、[发布轨道](release-track-gray-canary.md)（stable / canary 两条轨道）、[混版本持久字段保护](mixed-version-persist-field-protection.md) §13（表行只增不删）、[配置表 API](table-api-reference-zh.md)、[data/AGENTS.md](../../data/AGENTS.md)、[可配置模板](configurable-templates.md) §3.6（「v1 不做热更」和它列的三条前提，本文把这三条都落实）。
> **核对基线**：HEAD `ccf2840b2` 加 11-20 工作区。同时有多个会话在改，行号会漂，按函数名找。
> **上线状态**：未上线。没有存量指针，没有存量表包，不需要迁移。

## 0. 结论速览

- **不引入 Nacos。** 配置中心要做的事（存一份小配置、各进程订阅、改了就推）etcd 已经在做，热关停就是这个形态。Nacos 不解决真正缺的那一块：进程里正在用的表怎么安全地整批换掉。
- **配置表不进配置中心，进配置中心的只是一个指针。** 表按版本打成不可变的**表包**放对象存储，etcd 里只存「这条轨道现在用哪一包」。
- **指针只按发布轨道分（stable / canary），不按 zone 分。** 「先在一个 zone 试」= 让那个 zone 在 canary 轨道上；「某个 zone 长期和别的不一样」= 写成表里的数据。原因见 §6.2。
- **分发交给每个 Pod 里的 sidecar（`table-sync`）**：它看 etcd、下载、核对摘要、解到本地目录。业务进程只读本地文件，不新增任何网络代码，C++ 不新起线程。
- **进程内两阶段切换**：整批解析 → 整批校验 → 一次换掉全部表。任何一步失败都继续用旧表并告警，不再 `LOG_FATAL`。
- **可以约定生效时刻**：指针带 `activate_at`，各进程提前准备好，到点一起切。全服切换的时间差缩到时钟误差以内，也顺带支持「活动表凌晨五点生效」。
- **哪些表能热更要逐表声明**（`(cfg_hot_reload)`，缺省不能）。改到没声明的表，进程不切换，回报「需要重启」，走滚动重启。
- **战斗每局绑定开局时的那一套表**，打到一半换表不影响这一局。
- **加 Pod 不给任何中心组件加负担**：表包按内容寻址、写好不再改，每个 Pod 的 sidecar 各自从对象存储取、各自核对，没有中心分发服务；etcd 里每条轨道、每条 schema 线只有一个指针键，与 Pod 数无关；各实例的回报键只在状态变化时写，平时只续租。规模估算与上限见 §18。
- **宁可不启动，也不带着旧表起来**：本线表包没同步下来时进程拒绝启动，不退回镜像自带的表。代价是对象存储故障期间新 Pod 起不来（§8.1）。

## 1. 要解决什么

| 诉求 | 现在怎么做 | 问题 |
|---|---|---|
| 改一个数值、开一个活动、停售一件商品 | 重新导表、重打镜像、滚动重启 | 慢，改一个数要走一次完整发布 |
| 全服统一更新 | 逐 zone `zone-up` | 同上 |
| 先在一个 zone 上试 | 给那个 zone 单独打镜像 | 表和代码绑在同一个镜像里，没法只换表 |
| GM 后台点一下就生效 | 做不到 | 没有运行期入口 |
| 看清「现在全服各进程用的是哪一版表」 | 做不到 | 导表器产出了 `manifest.json`（版本号、内容摘要），加载端没有读它、也没有上报 |

不解决的事：进程自己的部署配置（超时、连接池、端口）仍随部署滚动生效；紧急止血仍用热关停；客户端资源怎么下发不在本文范围（§14）。

## 2. 现状（2026-11-20 核对）

**表从哪来。** `tools/data_table_exporter` 把 `data/*.xlsx` 加 `data/schema/*_table.proto` 导成 `generated/tables/`：每张表一份 `.json`、一份 `.pb`，外加 `manifest.json`。现在是 53 张表、108 个文件、581K，manifest 第 43 版。manifest 里有单调递增的 `version`、只随内容变的 `content_digest`、每个文件的 sha256（`core/manifest.py`）。它的文件头写着设计意图：加载端读出版本并上报，就能看出谁落后。**加载端这一半一直没做**，Java 仓 `PARITY.md` 也把「Go / C++ 加载时校验 manifest 的 sha256 与行数」记成本仓的待做。

**表怎么进进程。** 烘在镜像里：`deploy/k8s/Dockerfile.cpp` 把 `generated/tables/` 拷到 `/app/generated/generated_tables/`，`Dockerfile.go-svc` 拷到 `/generated/tables/`。

**C++ 的加载。**

- `Node::LoadAllConfigData()` 调 `LoadTablesAsync()`：按 CPU 核数起一组线程并行解析，全部 join 之后才返回（守卫 `test_cpp_thread_sources.py` 登记的理由就是「启动期」）。
- 每张表一个单例 `XxxTableManager`，里面是 `std::unique_ptr<Snapshot>`。`Load()` 建好新快照后直接 `snapshot = std::move(snap)`，旧快照当场析构。解析失败是 `LOG_FATAL`。
- `ReloadTables()` 就是再跑一遍串行的 `LoadTables()` 再把版本号加一。**全仓没有调用方**（`battle_table_fingerprint.h` 的注释也这么写）。
- 它不是整批原子的：53 张表一张一张换，中途失败会留下半新半旧，而且进程已经 FATAL。
- 表加载完成的钩子是各节点的 `TableLoadHandler::OnLoaded()`（`node_entry.h` 经 `OnTablesLoadSuccess` 挂上）。scene 在里面做四件事：装导航网格（`ConfigSystem::OnConfigLoadSuccessful` → `NavigationSystem::LoadNavBins`）、刷新战斗表指纹、装装备掷属性的挂点并校验装备表、校验神秘商人三张表。后两项校验只打日志，不拒启。battle 在里面查几张战斗表非空并刷新指纹。
- battle 的回合引擎只经 `BattleDataProvider` 接口读表。生产实现 `TableBattleDataProvider` 全进程一个，读全局单例（`BattleRoomManager::SetDataProvider`，每个房间拿同一个）。
- 读表点的分布：`cpp/libs/services/scene` 154 处、`cpp/libs/services/battle` 22 处、`cpp/libs/modules` 11 处、`cpp/nodes/battle` 7 处（都在加载钩子里）、`cpp/libs/engine` 1 处（gate 的 `message_limiter.cpp` 读 `MessageLimiter`）。

**Go 的加载。**

- 每张表的管理器里是 `atomic.Pointer[snapshot]`，单表的读写已经是安全的。
- `ReloadTables()` 先把全部新管理器建好，任何一张失败就整体返回错误，这一步是对的。但提交时是给 53 个包级变量逐个裸赋值（`XxxTableManagerInstance = newXxx`），并发读的时候这是数据竞争，也不是整批原子。同样没有调用方。
- 读表的服务：guild（30 处）、match（14 处，另把 `PlayMode`、`Dungeon` 等编成 `catalog.Catalog`，把官方剧本编成 `battlescript`）、scene_manager（8 处：`World` 在启动时铺世界频道，神秘商人三表与 `Item` 用时现读）、login（3 处）、mail 与 rank（只读 `Item`）。player_locator 调了 `LoadTables` 但没找到读表点。trade、payment、chat、friend、data_service、db 不读表。
- `go/shared/tablequery` 的包注释明写着「本仓 Go 侧配置表没有热更，结果在进程生命周期内恒定」。

**Java。** 本仓的 `java/gateway_node` 不读数据表。`java/config_node` 是日常不启动的查表辅助应用，自带 `POST /api/reload`。

**已有、可以直接照着做的。**

| 已有的东西 | 在哪 | 本方案怎么用 |
|---|---|---|
| etcd 事实源 + 管理接口 + 各进程自己 watch | 热关停：前缀 `/mmorpg/killswitch/`，`AdminKillSwitchController`、`EtcdKillSwitchRuleStore`、`AdminApiKeyFilter` | 指针的存放与写入口照这个形态 |
| 发布轨道 | `MMORPG_RELEASE_TRACK`、`NodeInfo.release_track`，全局服务有 `<svc>-canary`，战斗按整局分轨道 | 指针按轨道分 |
| 原生 sidecar | 每个 C++ Pod 的 Alloy 日志 sidecar（`initContainers` + `restartPolicy: Always`），含 Agones Fleet | `table-sync` 用同一种挂法 |
| 对象存储 | 生产接外部 S3 兼容存储（Loki 三副本、TiDB 备份已在用），dev 有 SeaweedFS 夹具 | 表包另开一个桶 |
| 表行只增不删 | `(cfg_ids_permanent)`，`Item`、`Pet` | 混版本窗口里「查不到的 id」可以判成「还没更新到」 |
| 只存 id、不存行指针 | `data/AGENTS.md`，生成的头文件里也写着 | 继续是硬规则 |
| 战斗表指纹 | `BattleTableFingerprint`，scene 出快照时带、match 比对、battle 开局再比 | 战斗按指纹选哪一套表 |

**文档和代码对不上的地方**（落码时一并改正文）：`mystery-merchant.md`、`guild-phase2/04-asset-channel.md`、`mixed-version-persist-field-protection.md` 里「scene 的表可以热更」是按将来写的；`configurable-templates.md` 里「Go 用裸赋值」只说对了提交那一步；`tablequery` 的包注释说模板生成的是 `m.snap = snap`，现在已经是 `atomic.Pointer`；`table-api-reference-zh.md` §9 描述的是没人调用的 `ReloadTables`。

## 3. 为什么不上 Nacos

把「大厂的配置发布系统」拆开看，每个角色由谁承担：

| 角色 | Nacos 管不管 | 本方案用什么 |
|---|---|---|
| 存小配置、订阅、改了就推 | 管，这是它的本职 | etcd，已有，三副本 |
| 存大体积的数据文件 | 不是为这个设计的 | 对象存储，已是生产的外部依赖 |
| 把文件送到每个实例 | 不管 | `table-sync` sidecar |
| 进程里把旧表换成新表 | 不管 | 导表器生成的两阶段加载 |
| 灰度 | 有按实例的灰度，但认不出「zone」和「一整局战斗」 | 发布轨道，已有 |
| 权限与审计 | 有 | gateway_node 管理接口，已有 |

Nacos 能替掉的只有第一行，而第一行已经有了。再上一套，就是两个配置中心、两套高可用、两套备份，C++ 节点还要另接一个客户端。Java 版已经把 Nacos 当 Dubbo 注册中心用，那边把指针放进 Nacos 配置是顺手的；两版共用的是表包和版本号，不是配置中心（§14）。

## 4. 方案总览

```
策划改 xlsx ──导表──► generated/tables + manifest.json
                         │ table-bundle pack / upload（发布工具，带写凭据）
                         ▼
                对象存储：tables/<schema_digest>/<content_digest>.tar.gz   （不可变）
                         │
GM 后台 / 发布工具 ──► gateway_node /admin/tables ──► etcd 指针（按轨道、按 schema 线）
                                                        │ watch
                              每个 Pod：table-sync sidecar（只读凭据）
                                 下载 → 核对摘要 → 解包 → 写 POINTER.json
                                                        │ 本地目录（emptyDir）
                              业务进程：每 2 秒看一眼 POINTER.json
                                 准备 → 校验 → 到 activate_at 提交 → 写 STATUS.json
                                                        │
                              sidecar 把 STATUS.json 带租约写回 etcd ──► 管理接口汇总、告警
```

三条贯穿全文的规则：

1. **表包不可变，按内容摘要寻址。** 回滚就是把指针指回旧摘要，不存在「改了同一个包」。
2. **失败就保持原状。** etcd 连不上、对象存储连不上、新包校验不过，进程都继续用手上这一套，不降级、不猜。
3. **业务进程只碰本地文件。** 和 etcd、对象存储打交道的只有 sidecar。

## 5. 表包

### 5.1 身份：两个摘要

- `content_digest`：已有，64 位十六进制，只随内容变。它是一包表的名字。
- `schema_digest`：**新增**，16 位十六进制。导表器按「服务端数据布局」算：每张进服务端产物的表，取表名、数据文件名、各字段的号、名、类型、是否 repeated、子消息结构和用到的枚举，排序后做 sha256。注释、索引、外键、位序这些只影响生成代码的 option 不算，只给客户端的列不算。写进 manifest（`schema_version` 升到 2），同时生成成三端的常量。

**一个二进制只加载 `schema_digest` 和自己一样的表包。** 这就是「热更」的精确定义：只改数据、不改结构。加列、加表都要改生成代码，本来就得发新版本，表包随新镜像一起走。

`schema_digest` 相同的一串表包叫一条 **schema 线**。线上通常只有一条；滚动发布一个改了表结构的版本时会有两条并存（旧二进制一条，新二进制一条），所以指针要分线（§6.1）。

### 5.2 格式与存放

- 一个表包 = `generated/tables/` 目录原样打成 `tables-<content_digest>.tar.gz`：全部 `.json`、`.pb` 和 `manifest.json`，平铺，没有子目录。用 gzip 是因为 Go 标准库就能解。
- 对象键：`tables/<schema_digest>/<content_digest>.tar.gz`，旁边放一个同名加 `.sha256` 的文件，内容是压缩包的 sha256。
- 上传工具不覆盖已存在的键：同名已存在且 sha256 不同就报错停下。
- 桶是私有的。sidecar 用只读凭据，发布工具用写凭据，凭据都走 Secret（`deploy/k8s/AGENTS.md` 的注入规则）。
- dev 与 kind 复用现有的 SeaweedFS 夹具，另建一个桶。生产接外部 S3 兼容存储，本仓库不部署生产级对象存储（与 Loki 同一决定）。

### 5.3 哪些表能热更：逐表声明

在 `data/schema/cfg_options.proto` 加表级 option `(cfg_hot_reload) = 60006`，**缺省 false**。导表器把名单生成成三端常量，并在 manifest 每张表的条目里记一份（给工具看）。

进程拿到新包时，按 manifest 里每个文件的 sha256 逐表比对手上这一套：

- 变了的表都声明了可热更 → 热切换；
- 有一张没声明 → **整包都不切**，回报 `needs_restart`，等滚动重启时生效。

以二进制里编进去的名单为准，不以 manifest 为准：一张表能不能在运行中换，取决于读它的代码，而不是数据。

为什么缺省不能：有些表的内容在启动时被「物化」成了运行中的结构。`World`、`BaseScene`、`Mirror` 决定 scene_manager 启动时铺哪些世界频道和副本，scene 的导航网格也在表加载钩子里装；`ZoneGroup` 是 match 的匹配范围树，启动校验失败会拒启。这些表运行中换了，结构不会跟着变。逐表放开的做法见 §13 批 5：每放开一张，写一行「谁读它、读出来存没存、换了以后怎么跟上」。

### 5.4 发布前校验

表包能被指针指到之前，要过三道：

1. **导表期**（已有）：主键、外键、`(cfg_ids_permanent)` 台账、tip 引用。任何一项不过，整批不产出。
2. **用线上的二进制预检**（新增）：节点与各 Go 服务加一个启动参数 `--validate-tables <目录>`，只做「解析、建索引、跑校验」然后退出，退出码非 0 即不通过。发布工具对当前线上的每个镜像各跑一遍。用真实二进制是为了保证预检和线上跑的是同一份校验代码。
3. **各进程准备阶段**（§8.1）：每个进程自己再校验一遍，不过就不切。这是最后一道，不是第一道。

## 6. 指针

### 6.1 键与值

```
/mmorpg/tables/pointer/<轨道>/<schema_digest>          这条轨道上、这条 schema 线用哪一包
/mmorpg/tables/status/<轨道>/<namespace>/<Pod 名>      各实例的回报，带租约，实例没了自动消失
```

回报键里要有 namespace：各 zone 的 namespace 里，StatefulSet 是同名的（`guild-0`、`login-0`、`gate-0`……），而所有 zone 共用 infra 的同一个 etcd。只用 Pod 名的话，同一轨道上两个 zone 的同名 Pod 会写同一个键、互相覆盖：管理接口少算实例，某个实例的「被拒绝」还可能被另一个的「已加载」盖掉，发布工具就不会自动撤回。不在 K8s 里跑（本机）时没有 namespace，只用 Pod 名。管理接口把轨道之后的整段都当实例名，所以解析不用改。

指针的值：

```json
{
  "current": {"content_digest": "<64 hex>", "bundle_sha256": "<64 hex>", "version": 43},
  "next":    {"content_digest": "<64 hex>", "bundle_sha256": "<64 hex>", "version": 44,
              "activate_at_ms": 1795000000000},
  "updated_at_ms": 1794999900000, "updated_by": "ops-name", "reason": "double exp weekend"
}
```

- **生效的那一包**：有 `next` 且当前时刻不早于 `activate_at_ms` 就是 `next`，否则是 `current`。
- **这条线没有指针，或 `current` 为空且 `next` 还没到点**：用镜像里自带的表。镜像自带的表和二进制一定是同一个 `schema_digest`，而且是跟这个二进制一起测过的，所以这个回落是安全的。新版本刚发布、还没人给它发过热更时就是这个状态。
- `bundle_sha256` 是压缩包的摘要，sidecar 在解包**之前**先核对它；`content_digest` 是这包表的名字，进程加载时再按 manifest 逐文件核对。
- `version` 只给人看。

为什么要 `current` 和 `next` 两格：设了 `next` 但还没到点的这段时间里，重启的进程必须知道「现在该用哪一包」。只存一格的话它只能用新包，等于提前切了。

### 6.2 只按发布轨道分，不按 zone 分

负责人的原话里有「统一更新某个 zone」。这个诉求其实是两件事，做法不同：

| 你想要的 | 做法 |
|---|---|
| **先在一个 zone 上试新表**，没问题再全服 | 让那个 zone 在 canary 轨道上，把新包发到 canary 指针。看过指标后再发到 stable 指针 |
| **某个 zone 长期和别的不一样**（新服的开服活动、某个区的倍率） | 写成表里的数据：表行带「对哪些 zone 生效」的条件，所有 zone 跑同一包表，读出不同的结果 |

不给每个 zone 一个指针，原因有三个：

1. **全局服务没有 zone 身份。** battle、match、mail、rank、guild 是全服一个池子，同时服务所有 zone 的玩家。给 zone 5 单独一包表，zone 5 的 scene 用新表，替它结算战斗、撮合、发邮件的全局服务用的还是另一包，一段流程就跨了两版表。热关停当初不做 zone 维度是同一个理由。
2. **发布轨道已经把这个问题解决了。** canary zone 的会话只会被送到 `<svc>-canary` 和 `battle-canary`；战斗只有全员都是 canary 才进 canary 池。指针按轨道分，这条隔离直接就是表的隔离，不用再造一遍。
3. **在线的表版本只能有两个。** 轨道只有两条，所以任何时刻要保证兼容的只有「稳定的」和「正在验证的」这一对。按 zone 分的话，在线版本数可以到 zone 数，兼容性没法逐对检查。

代价：把一个本来在 stable 上的 zone 拿来试表，要先 `zone-up -ReleaseTrack canary` 把它滚到 canary（镜像不变）。常做表灰度的话，留一个常驻的先锋 zone 在 canary 上，之后每次试表都只是改指针。

「表行带 zone 条件」现在没有一张表在用（`ActivitySchedule` 没有 zone 字段，`ZoneGroup` 是给匹配范围用的分区，不能直接借）。第一张需要它的表落地时，由那个功能的设计定列的形状，不在本文范围。

zone 级的开关不走表：停服维护用已有的 `/admin/zones/{id}/maintenance`。

### 6.3 约定生效时刻

- `PUT` 时给 `activate_at_ms`，缺省是「现在 + 60 秒」。60 秒够所有进程把新包下载、解析、校验完。
- 进程一看到 `next` 就开始准备，准备好了等着，本机时钟到点才提交。全服的切换时间差就是时钟误差（启动时已有时钟核对，门槛 30 秒，实际是毫秒级）。
- 到点时还没准备好的进程，准备好就立刻提交，并在回报里显示落后。
- `immediate=true` 表示不等，谁准备好谁先切，全服错开几秒。回滚和紧急修复用它。
- 也可以给一个远一点的时刻，比如活动表凌晨五点生效。到点之前可以撤销。

### 6.4 管理接口

放在 gateway_node，与 `/admin/killswitch` 同一套鉴权（`X-Admin-Key`）和审计日志。GM 后台和发布工具都调它；`etcdctl` 是破窗手段。

| 方法 | 作用 | 校验 |
|---|---|---|
| `GET /admin/tables` | 列出每条轨道、每条 schema 线的指针，生效的是哪一包，以及各实例的回报汇总（已加载、已准备、被拒绝、需要重启各多少，被拒绝的列出原因） | — |
| `PUT /admin/tables` | 给一条线设 `next`。原来的「生效的那一包」折进 `current` | 轨道只能是 stable / canary；摘要格式；`activate_at_ms` 不早于现在 + 60 秒，除非 `immediate`；按 etcd 的 mod_revision 做比较再写，两个人同时发布只有一个成功 |
| `DELETE /admin/tables/next` | 撤销还没到点的 `next` | 已经到点的不能撤，只能再发一次指回旧包 |
| `DELETE /admin/tables` | 删掉一条已经没人用的 schema 线 | 这条线还有实例在回报就拒绝。否则重启的进程会退回镜像里更旧的表 |

gateway_node 不碰对象存储，不验证表包是否存在；指到一个不存在的包，sidecar 会回报同步失败并告警，进程保持原状。

**汇总只认「说的是当前目标那一包」的回报。** 实例回报里的 `pending_state`（已准备、被拒绝、需要重启）带着它当时看到的那一包的摘要 `pending_digest`。指针刚换目标时，实例的回报还停在上一包（进程每 2 秒读一次指针，sidecar 再隔几秒转报）；`GET /admin/tables` 只在 `pending_digest` 等于这条线当前目标（有 `next` 取 `next`，否则取生效的那一包）时才把它计进对应的一类，对不上的不计入任何一类，等它跟上来。否则上一包的拒绝会被算到新包头上，发布工具一轮询就判新包发布失败并撤回。

发布工具 `table-bundle`（与 sidecar 同一个 Go 模块，共用校验与对象存储代码；`dev_tools.ps1` 加包装命令）把一次发布做成有头有尾的操作：

1. `pack` / `upload`：打包、算摘要、上传（已存在就跳过）。
2. `verify`：§5.4 第 2 道预检。
3. `publish`：`PUT` 指针，然后轮询 `GET`。到点之前只要有一个实例回报 `rejected`，就自动 `DELETE next` 并以非 0 退出；到点之后等全部实例回报已加载，列出落后的。
4. `rollback`：把 `next` 设成上一包，`immediate`。

工具中途断了也不会留下坏状态：指针已经设好，各进程照常到点切换；被拒绝的实例保持旧表并告警。

## 7. 分发：`table-sync` sidecar 与本地目录

**为什么不让业务进程自己拉。** C++ 节点没有 HTTP 加 TLS 的客户端，也没有 S3 签名；写一个就要在主循环上做大文件下载，还要守住不新起线程的规则。Go、C++、Java 各写一份下载与校验，是三份要保持一致的代码。sidecar 只写一份，三种语言的进程都只面对同一个本地目录约定。对象存储的凭据也只进 sidecar 容器。

**本地目录约定**（两个 emptyDir：`tables` 由 sidecar 读写、业务容器只读；`tables-status` 反过来）：

```
$MMORPG_TABLE_BUNDLE_ROOT/
  bundles/<content_digest>/        解包后的表目录，和 generated/tables 一样；写好之后不再改
  POINTER.json                     本轨道全部 schema 线的指针；先写临时文件再改名，读到的一定是完整的
$MMORPG_TABLE_STATUS_ROOT/
  STATUS.json                      业务进程写：手上是哪一包、准备中的那一包是什么状态；同样先写后改名
```

`POINTER.json` 的形状：`{"track": "...", "lines": {"<schema_digest>": {current, next, ...}}, "sync_errors": {"<schema_digest>": "<原因>"}}`。`lines` 里只有「引用到的包本地都齐了」的线；`sync_errors` 只在有线没同步下来时出现，按线记一句原因（最长 256 字符）。

`STATUS.json` 的字段名是业务进程、sidecar、管理接口三方之间的约定，三种语言逐个一致（事实源是 Go 的 `tablebundle.StatusReport`）：`service`、`schema_digest`、`source`（`image` / `bundle`）、`loaded_digest`、`loaded_version`、`loaded_at_ms`、`load_seq`、`pending_state`（空、`preparing`、`prepared`、`rejected`、`needs_restart`）、`pending_digest`、`pending_version`、`pending_reason`、`updated_at_ms`。`pod` 与 `track` 进程可以不写，sidecar 转报时补上。sidecar 靠 `loaded_digest` / `pending_digest` 决定哪些包目录不能清，管理接口靠同一组字段汇总，写错一个名字对方就读成空。

**sidecar 做的事：**

1. 启动先全量读前缀 `/mmorpg/tables/pointer/<本轨道>/`，之后 watch，并每 20 秒全量读一次复核（和热关停一样：watch 通道开着不代表读得到）。
2. 对读到的每条线的 `current` 和 `next`：本地没有就下载。先核对压缩包的 sha256，对上了才解包。解包时拒绝带路径的条目、链接、超过 64 MiB 的包（压缩前后都算，上限的来由见下面的「磁盘预算」）、超过 1024 个的文件；解到临时目录，核对 manifest 里的 `schema_digest`、每个文件的大小与 sha256，全部通过才改名成正式目录。
3. **引用到的包全部到位之后**才把这条线写进 `POINTER.json`。业务进程因此永远不会读到一个指向本地还没有的包的指针。某条线这一拍没同步下来（包取不到、校验不过、指针 JSON 写坏）时：沿用上一次成功写出的那条线，并在 `sync_errors` 里记原因；这个 Pod 上从没成功过的线不出现在 `lines` 里，只出现在 `sync_errors` 里。sidecar 自己重启时，先把落盘的 `POINTER.json` 里的线读回来当「上一次」，所以重启后头几拍取不到包也不会让一条线从文件里消失。
4. 每 5 秒读一次 `STATUS.json`，挂在 30 秒租约下写到 `/mmorpg/tables/status/<轨道>/<namespace>/<Pod 名>`。**只在内容变了、或者换了租约时才写键，其余时候只续租。** 存活靠租约表达：续租不写键、不产生新的 revision，Pod 没了 30 秒内键自动消失。`STATUS.json` 只在状态变化时改写，所以稳态下写入量接近 0。每 5 秒无条件写一次的话，etcd 的写入量随 Pod 数线性涨（2000 个 Pod 每秒 400 次写键），而 infra 的 etcd 是全项目共用的，按 revision 只保留最近 100 万条历史，等于别的 watcher 只剩 40 分钟左右的历史可追（§18）。代价：有人绕过管理接口手工删了这个键，要等下一次状态变化或换租约才会补回。值里的 `pod` 一律写 sidecar 的身份（带 namespace），业务进程写进来的那份会被覆盖，键和值才对得上。
5. 清理：不被 `POINTER.json` 和 `STATUS.json` 引用、且放了超过 10 分钟的包目录删掉；没放够 10 分钟的，只留最新的 2 个，多的从旧往新删。sidecar 自己的下载与解包临时文件（`.download-*`、`.extract-*`）放过 10 分钟还在的，是 sidecar 中途被杀留下的，也删掉（一次下载最长 60 秒，正在用的临时文件不会这么老）。保留期只为一个很窄的窗口：业务进程刚读到指针、还没把「正在准备哪一包」写进 `STATUS.json`，指针就又换走了，那一包一定是最近的一两个，所以留 2 个够用。
6. 暴露 `/readyz`（至少成功写过一次 `POINTER.json`）和 `/metrics`。

**挂法**：和 Alloy 日志 sidecar 一样是原生 sidecar（`initContainers` + `restartPolicy: Always`），`startupProbe` 看 `/readyz`。所以业务容器启动时 `POINTER.json` 一定已经在了，启动路径不用等。读表的每一种工作负载都带：C++ 的 gate、scene、battle（含 Agones Fleet 与金丝雀池），Go 的 guild、login、mail、match、rank、scene_manager。Alloy sidecar 踩过的坑同样适用：sidecar 镜像拉不到，业务容器一次都不会启动，Pod 卡在 `Init:ErrImagePull`。

**磁盘预算**：`tables` 卷的 `sizeLimit` 是 1Gi，超了 kubelet 会把整个 Pod 驱逐。所有 Pod 拉的是同一包，一包过大就是整条轨道的 Pod 一起被驱逐，所以单包上限要和卷的上限算在一起。一个 Pod 同一时刻最多放：每条 schema 线的 current 与 next（sidecar 会把本轨道所有线的包都拉下来；滚动升级期间新旧两条线，旧版本下线后按运维手册 `delete-line` 删掉旧线，这里按三条线留余量，6 份）、最近 2 个没人用的包（第 5 步的上限）、正在下载的压缩包与解包临时目录（2 份），共 10 份。1Gi 除以 10 再留余量，单包上限定为 64 MiB；现在全部表约 1.1 MiB，余量 50 多倍。`pack` 打包时就按这个上限拒绝，问题在发布之前暴露，不会等各 Pod 拉包时才撞上。表真的长到需要放大时，`MaxBundleBytes`、卷的 `sizeLimit` 和这里的算式三处一起改。旧值 256 MiB 和 1Gi 是对不上的：两条线各有一个 256 MiB 的包时，再下载一包就到了 1Gi。

**凭据**：sidecar 用只读凭据访问对象存储，凭据放在每个跑 sidecar 的 namespace 里的 Secret（缺省名 `table-sync-s3`，键 `access-key-id`、`secret-access-key`），按 `secretKeyRef`（`optional: false`）引用。缺了这个 Secret，本 namespace 里所有带 sidecar 的 Pod 都卡在 `CreateContainerConfigError`，整个 zone 或全局池起不来；可 `kubectl apply` 本身照样成功，要等 rollout 超时才看得出来。所以外接对象存储时，部署脚本在本 namespace 的任何工作负载之前先核对它（`Assert-TableSyncS3SecretProvisioned`：只列键名和长度，不读值；缺了就停，什么都不写）。dev 夹具的凭据由部署脚本自己写入。

**目录权限**：sidecar 解包用的临时目录建出来是 0700，改名成正式目录后权限不变。现在 sidecar 和所有业务镜像恰好都是用户 10001，所以读得到；但只要哪个镜像换了用户号，业务进程就一个包都读不了。解包时显式把目录放开成 0755，「读得到」不再依赖两个镜像的用户号碰巧相同。

**不开表包模式**：`MMORPG_TABLE_BUNDLE_ROOT` 没设，进程和今天完全一样，读配置里的表目录，没有热更。本机开发、单测、现有的调试栈都是这个状态。

**本机改表即时生效**（给策划和开发用，不需要 etcd 和对象存储）：加一条 `dev_tools.ps1 -Command tables-reload-local`，把刚导出的 `generated/tables` 拷成 `bundles/<摘要>/` 再改写 `POINTER.json`。本机跑着的节点两秒内就会切过去。它用的就是上面这套目录约定，所以也是进程侧热更最便宜的冒烟手段。

**新增依赖**：最终实现**零新第三方库**——在 `go/table_sync/internal/s3client` 用约 230 行 Go 标准库实现最小 AWS SigV4 HTTP 客户端（`HeadObject` / `GetObject` / `PutObject`，兼容 AWS S3、OSS、COS、Ceph RGW、MinIO 与 SeaweedFS），离线构建无需拉取任何外部包。

## 8. 进程内加载

### 8.1 三种语言共同的规则

开了表包模式之后：

**启动时**：读 `POINTER.json`（读不到就启动失败），按自己的 `schema_digest` 找到那条线，取生效的那一包，从 `bundles/<摘要>/` 加载；这条线没有指针就加载镜像自带的表。加载前按 manifest 逐文件核对大小与 sha256，清单里自报的 `content_digest` 还要等于指针给的摘要（和热更路径同一道核对）。启动阶段的失败处理不变：解析失败照旧拒启，校验问题照旧只打日志。

**「没有指针」和「有指针但表包没到位」要分清。** 本线不在 `lines` 里有两种情况：etcd 里确实没有这条线（新版本刚发布、还没人发过热更），用镜像自带的表是对的；etcd 里有，但这个 Pod 的 sidecar 没把包同步下来，这时用镜像自带的表等于静默退回更旧的数据，而同一条线上别的进程跑的是指针指的那一包。所以 `sync_errors` 里有本线时**拒绝启动**（Go 返回错误，C++ `LOG_FATAL`），容器按退避重启，等 sidecar 同步好再起来。已经在跑的进程不受影响：它读到的仍是上一次成功的那条线。

这是拿可用性换一致性，代价要说清楚：
- **代价**：对象存储（或凭据、或包本身）出问题期间，新建的 Pod 起不来。扩容、节点故障后的重新调度、滚动重启、Agones 补战斗服都会卡住，直到 sidecar 取到包。
- **不受影响的**：已经在跑的 Pod，以及同一个 Pod 里业务容器自己崩了重启。卷在 Pod 的生命周期里一直在，`POINTER.json` 和包都还在。sidecar 自己重启也不受影响，它会把落盘的 `POINTER.json` 读回来（§7 第 3 步）。
- **为什么接受**：退回镜像自带的表，看起来服务起来了，实际上同一条线上不同实例用着不同版本的表。掉落、价格、活动开关这类数值会在玩家之间悄悄不一致，事后也查不出是哪些请求落在了旧表上。这和 `AGENTS.md` §11 「正确性 / 数据一致性优先于可用性」一致。起不来会立刻被发现，悄悄用错表不会。
- **怎么及时发现**：`TableSyncFetchFailing` 告警（§11），以及 Pod 反复重启的现象（运维手册 §6）。对象存储本身要按高可用部署；dev 夹具是单进程，只用于 kind。

**运行中**，每 2 秒看一次 `POINTER.json`：

```
发现本线的 next 换了
  └─ 比对逐表摘要
       ├─ 改到了没声明可热更的表 ──► 回报 needs_restart，不切
       └─ 都可热更
            ├─ 准备：整批解析、建索引，放在旁边，不动现役的表
            ├─ 校验：在这套暂存表上跑全部校验与派生数据的编译
            │     ├─ 有一项不过 ──► 丢掉暂存表，回报 rejected 与原因，继续用旧表
            │     └─ 全过 ──► 回报 prepared
            └─ 本机时钟到 activate_at（或 immediate）──► 提交，回报 loaded
```

- **提交是一次换掉全部表**，连同校验阶段编译好的派生数据一起换。不存在半新半旧。
- **被拒绝的包不重试**，直到指针再变。
- **结论跟着指针走**：指针不再指向那一包（`next` 被撤销、指回了现役的包、整条线被删）时，进程把手上「已准备 / 被拒绝 / 需要重启」的结论连同正在准备的暂存表一起清掉并重新回报；之后同一包再次发布，重新准备、重新校验。不清的话，管理接口会一直看到这个进程在拒绝一个指针早就不指的包。
- **同一时刻只有一包在准备**。准备途中指针又变了，丢掉手上的，从头来。
- **派生数据必须两阶段**：凡是「从表里算出来、存在别处」的东西，拆成「在暂存表上算，可能失败」和「提交时装上，不会失败」两步。两步用同一个函数的同一次结果，避免校验时过了、装的时候算出来不一样。
- **启动和热更的严格程度不同，这是有意的**：同一包表，热更时校验不过会被拒绝，而进程重启时只打日志照样加载。启动时拒绝会把一次配表失误变成停服；这个缺口由 §5.4 的预检和 `TableBundleRejected` 告警来堵，不靠拒启。

### 8.2 C++

改的是导表器模板（`generated/` 下的东西不手改）：

- **`TableSet`**：一整套不可变快照加上身份（两个摘要、版本号、加载序号），用 `shared_ptr<const TableSet>` 持有。每个 `XxxTableManager::Instance()` 的读接口都经「当前生效的那一套」取自己的快照，调用点不用改。
- **单表解析与安装分开**：解析函数只返回新快照或错误文本，不碰现役数据，不 `LOG_FATAL`。原来的 `Load()` 保留，等于「解析，失败即 FATAL，装上」，启动路径和现有单测不变。
- **准备分片**：热更时每轮事件循环只解析一张表，用 `queueInLoop` 接力，不起线程，也不让主循环停一整段。`LoadTablesAsync` 仍然只在启动时用，守卫清单里那一条的理由不用改。
- **校验**：节点钩子加一个 `Validate()`，在「把当前生效临时换成暂存表」的作用域里跑。现有校验代码读的是单例，在这个作用域里读到的就是暂存表，所以不用改写。主循环单线程，作用域内不会有别的代码跑。
- **提交钩子分两种**：只在启动时做的（装导航网格、装装备掷属性的挂点）和每次提交都做的（刷新战斗表指纹）。现在它们混在同一个 `OnLoaded()` 里，热更时再跑一遍会把导航网格重装一次。
- **旧的一套不当场析构**：提交后旧 `TableSet` 进一个退役列表，10 分钟后才放手；战斗房间这类显式持有者让它活得更久。10 分钟远大于任何一次协程等待（RPC 超时都在 20 秒以内），所以一次请求里跨了 `co_await` 的行指针不会悬空。**这是兜底，不是许可**：「只存 id、不存行指针」仍是硬规则，定时器闭包、成员里存的行引用过了 10 分钟照样是野指针。
- **生命周期**：负责轮询和接力的对象挂在 `Node` 上，定时器是它的成员；`queueInLoop` 的闭包按 `AGENTS.md` §11.7 只绑 `weak_ptr`。

### 8.3 Go

- 生成 `TableSet`（全部管理器，各自持有一份不变的快照）、`Prepare(dir, useBinary) (*TableSet, error)`、`Commit(*TableSet)`、`Current() *TableSet`。
- 现有的包级单例 `XxxTableManagerInstance` 保留，59 处调用点不用改。`Commit` 时把它们的快照指针逐个换成新的，并发布新的 `Current()`。逐个换有微秒级的窗口，所以规则是：**一次判定里要用到多张表、且这些表必须是同一版的**，先取 `ts := table.Current()`，再经 `ts` 读；只读一张表、或不在乎的，照旧用单例。
- 删掉裸赋值的 `ReloadTables`。
- 各服务的派生数据改成两阶段：match 的 `catalog.Catalog` 与官方剧本在 `Prepare` 阶段用新目录编译，失败即拒绝，`Commit` 时换原子指针。
- 改 `tablequery` 的包注释。

### 8.4 战斗：每局绑定开局时的那一套

回合引擎是确定性的，一局打到一半换了技能表，结果就和回放、观战对不上。

- `TableBattleDataProvider` 改成持有一套 `TableSet`，从它里面读，不再读全局单例。同一套表共用一个 provider。
- 开局时按这一局带来的战斗表指纹，在 battle 手上还活着的几套表（当前的，加上退役列表里的）里找指纹相同的那一套，房间绑定它。找不到的话沿用现有的指纹不一致处理（告警或拒绝，看 `FingerprintMode`）。
- 房间持有 provider，这一套表就活到这一局结束。
- 加一条静态守卫：`cpp/nodes/battle` 与 `cpp/libs/services/battle` 里除了 provider、指纹和启动钩子，不许直接读表单例。
- scene 出战斗快照时带的指纹在每次提交后刷新（`battle_table_fingerprint.h` 的注释已经要求这一点）。

切换的那一瞬间，scene 和 battle 可能差一个时钟误差。先切的 scene 带来的是新指纹，battle 还没切时找不到；因为 battle 也是提前准备好、到点才切，这个窗口是毫秒级，撞上的那一局按现有的指纹不一致处理走。

### 8.5 谁读表、换了以后要跟上什么

| 进程 | 读什么 | 热更时要处理的 |
|---|---|---|
| gate | `MessageLimiter`（限流值） | 用时现查，没有派生数据 |
| scene | 绝大多数表 | 各系统的 `ValidateTables` 全部接进 `Validate()`，任何一项不过就拒绝整包（11-26 补齐三项：装备表原先只在启动时核、神秘商人只打日志、游历手册只在提交后核）；新加带校验的系统要同时登记 `Validate()` 与 `OnCommitted()`；指纹每次提交刷新；导航网格与装备挂点只在启动做 |
| battle | 八张战斗表，经 `BattleDataProvider` | §8.4 |
| scene_manager | `World`（启动时铺场景）、神秘商人三表与 `Item`（用时现读） | `World` 是结构表，不可热更 |
| match | `PlayMode`、`Dungeon`、`ZoneGroup`、官方剧本 | `Catalog` 与剧本两阶段；`ZoneGroup` 是结构表 |
| guild、login | 30 处、3 处 | 批 5 逐处审计 |
| mail、rank | 只读 `Item` | 用时现查 |
| player_locator | 调了加载，没找到读表点 | 核实后去掉加载，不带 sidecar |

在线玩家身上已经算好的数值（属性、已掷出的装备）不会因为换表自动重算，要等下一次触发（升级、穿脱、重登）。这是 `equipment-attributes.md` 已经写明的口径，本文不改。

## 9. 一致性

- **一个 C++ 进程内**：提交发生在两个事件之间，每个事件处理函数从头到尾看到的是同一套表。跨了 `co_await` 的流程，恢复后看到的可能是新表，所以恢复后按 id 重新查。
- **一个 Go 进程内**：单表读取永远是完整的一版；跨表要同版就用 `Current()`。
- **进程之间**：约定生效时刻之后，窗口缩到时钟误差加上个别落后的实例。这个窗口里靠已有的保护：
  - `Item`、`Pet` 的行只增不删，本节点查不到的 id 判成「还没更新到」，回 RETRY 或 DEFER，不丢资产；
  - 报价核对（神秘商人、挂机月卡）：客户端看到的和服务端现算的对不上就让客户端刷新；
  - 战斗整局一套表。
- **表不影响存档格式。** 换表不改任何持久结构，不涉及存档世代。
- **配表的人要守的三条**（同一条 schema 线内）：
  1. `(cfg_ids_permanent)` 的表不删行（导表器已拦）。
  2. 不改一个已有 id 的含义，比如把物品 1001 从药水改成武器。这条机器拦不住。
  3. 其他可热更的表要删行，先确认存档里没有按 id 引用它；每张表放开热更时把这一项写进审计结论（§13 批 5）。
- **回滚**：把指针指回旧包。新包只改了数值时随时可以回。新包加了 `Item` 或 `Pet` 的行、而且这些行已经产出过东西时，回滚等于让这些行消失，引用它们的资产会一直停在 RETRY，直到表再更新回来。这种情况向前修，不回滚。发布工具在回滚前比对两包的 id，命中就要求加 `-Force`。

## 10. 失效模式

| 出了什么事 | 结果 |
|---|---|
| etcd 连不上 | sidecar 不改 `POINTER.json`，进程继续用手上的表。指标显示同步过期，超过 2 分钟告警 |
| 对象存储连不上，或包不存在 | sidecar 不发布指向这个包的指针：这条线沿用上一次成功同步的内容，`POINTER.json` 的 `sync_errors` 记原因。在跑的进程保持原状；这个 Pod 上从没同步成功过这条线时，业务进程启动会被拒绝（不退回镜像里的旧表），等同步好再起来 |
| sidecar 自己重启 | 先把落盘的 `POINTER.json` 读回来当「上一次」，同步失败的线照样沿用旧内容，不会从文件里消失 |
| 发了新包，个别实例的回报还停在上一包 | 管理接口只统计说的是当前目标那一包的回报（§6.4），上一包的拒绝不会被算到新包头上 |
| 撤掉了被拒绝的包 | 进程清掉「被拒绝 / 需要重启」的结论并重新回报；同一包再次发布会重新校验（§8.1） |
| 压缩包被改过 | sha256 对不上，不解包 |
| 新包解析失败、校验不过 | 该进程回报 `rejected`，继续用旧表。到点之前发布工具自动撤销；已经到点就是部分进程新、部分旧，告警后由人回滚或向前修 |
| 进程在准备途中崩了 | 重启后按 `POINTER.json` 加载当时生效的那一包 |
| 设了 `next` 还没到点，进程重启 | 加载 `current`，再准备 `next` |
| sidecar 挂了 | kubelet 重启它。期间进程不受影响，只是收不到新指针；回报的租约过期，管理接口里看不到这个实例 |
| sidecar 镜像拉不到 | 业务容器起不来（原生 sidecar 的固有代价），和 Alloy 一样在部署前把镜像载入节点 |
| 有人删了还在用的指针 | 管理接口拒绝。绕过它直接删 etcd 的话，之后重启的进程会退回镜像里的表，`TableBundleNotConverged` 会响 |
| 本机时钟偏了 | 切换时刻跟着偏。启动核对的门槛是 30 秒 |
| 短时间内连续热更 | 每次多留一套旧表 10 分钟。指标 `table_sets_retained` 看得到。sidecar 的卷里没人用的包最多留 2 个（§7 第 5 步），卷不会被撑爆 |
| 外接对象存储，却忘了建凭据 Secret | 部署脚本在本 namespace 的任何工作负载之前核对，缺了就停，什么都不写（§7「凭据」）。不核对的话，所有带 sidecar 的 Pod 卡在 `CreateContainerConfigError` |
| 发了一个超过 64 MiB 的包 | `pack` 打包时就拒绝。真到了 sidecar（比如绕过工具手工上传），解包时按同一个上限拒绝、记进 `sync_errors`，不会把卷撑到 `sizeLimit` 让 kubelet 驱逐整条轨道的 Pod（§7「磁盘预算」） |
| sidecar 下载或解包到一半被杀 | 留下的 `.download-*` / `.extract-*` 放过 10 分钟后被清理 |
| 两个 zone 有同名 Pod | 回报键带 namespace，各算各的（§6.1）。改之前会在同一轨道上互相覆盖，可能盖掉某个实例的「被拒绝」 |

## 11. 可观测性

业务进程（C++ 走各节点的 `/metrics`，Go 走 Prometheus）：

| 指标 | 类型 | 含义 |
|---|---|---|
| `table_bundle_info{release_track, schema_digest, content_digest, source}` | gauge，恒为 1 | 手上是哪一包。`release_track` 是本进程的发布轨道（`stable` / `canary`，与节点注册同一来源），`content_digest` 取前 12 位，`source` 是 `bundle` 或 `image`。每个进程同一时刻只有一条序列 |
| `table_bundle_needs_restart` | gauge，0 或 1 | 此刻是否停在 `needs_restart`。这是状态量，`table_reload_total{result="needs_restart"}` 是事件计数：后者只能说明发生过，说明不了现在还停在那里 |
| `table_reload_total{result}` | counter | `committed`、`rejected`、`needs_restart` |
| `table_reload_prepare_seconds` | histogram | 一次准备的总耗时 |
| `table_reload_slice_max_seconds` | gauge | C++：最近一次准备里最慢的一片，也就是主循环被占住的最长一段 |
| `table_sets_retained` | gauge | C++：还活着的旧表套数 |

sidecar：`table_sync_etcd_last_read_timestamp_seconds`、`table_sync_bundle_fetch_total{result}`（`hit`、`fetched`、`error`、`verify_failed`、`invalid_pointer`）。

**sidecar 的指标要单独抓。** 它们在 sidecar 自己的 9099 端口上（端口名 `table-sync-http`），而 Pod 上的 `prometheus.io/*` 注解只能指一个端口，指的是业务容器。只按注解抓的话永远抓不到 sidecar，`TableSyncStale` 和 `TableSyncFetchFailing` 就是哑的：表达式没数据不报错，和「一切正常」长得一样。`deploy/k8s/table-sync-podmonitor.yaml` 是一个按端口名选的 PodMonitor：选择器为空，Prometheus Operator 只留带这个端口名的容器，不用给各种工作负载另加标签。端口名两处一致由契约测试守着。

告警（`deploy/k8s/table-bundle-alerts.yaml`，配一份契约测试，告警引用的指标名、标签、失败结果名都要在代码里核对得到）：

| 告警 | 条件 | 级别 |
|---|---|---|
| `TableBundleRejected` | 10 分钟内有进程拒绝过表包 | 严重 |
| `TableBundleNotConverged` | 同一轨道、同一条 schema 线上（按 `release_track`、`schema_digest` 分组），不同实例手上的包不一样，持续 10 分钟。两种语言的 `table_bundle_info` 都要带 `release_track`，否则 stable 和 canary 混在一起比，金丝雀期间必然误报 | 警告 |
| `TableSyncStale` | sidecar 超过 2 分钟没读到 etcd | 警告 |
| `TableSyncFetchFailing` | sidecar 持续 5 分钟以上取包失败（`error` / `verify_failed` / `invalid_pointer`）。所有 Pod 一起失败时谁都没切，`TableBundleNotConverged` 不会响；而这时新 Pod 会拒绝启动（§8.1），所以单独报 | 警告 |
| `TableBundleNeedsRestart` | `table_bundle_needs_restart` 为 1 持续 24 小时。用事件计数 `increase(...[24h])` 配 `for: 24h` 写不出「还没重启」：它只在事件后第 24 小时、窗口刚好滑走时才成立，重没重启都一样 | 提示 |

日志（英文）：指针变化、准备开始与结束、提交、拒绝原因各打一条 INFO 或 ERROR，带两个摘要。启动日志打出手上这一包的 `version` 与 `content_digest`（`configurable-templates.md` §3.6 的第三条前提）。

## 12. 运营怎么用

| 场景 | 步骤 |
|---|---|
| 全服改数值、开活动 | 导表并提交 → `table-bundle publish -Track canary`（有 canary 时）→ 看指标 → `publish -Track stable` |
| 没有 canary 在跑 | 直接 `publish -Track stable`，靠预检 |
| 先在某个 zone 试 | 那个 zone 在 canary 上（常驻先锋 zone，或临时 `zone-up -ReleaseTrack canary`）；要连全局服务和战斗一起试，同时开 `<svc>-canary` 与 `battle-canary` → `publish -Track canary` |
| 某个 zone 长期不同 | 配进表里（§6.2） |
| 定时生效 | `publish -ActivateAt '2026-12-01 05:00:00+08:00'` |
| 发错了 | 到点前：`cancel`。到点后：`rollback`（先看 §9 的回滚限制） |
| 改了结构表 | 照常 `publish`。回报显示 `needs_restart`，再按发布手册滚动重启 |
| 发新代码版本（表结构变了） | 新镜像自带新表，不用设指针。之后给它发热更时，这条新线的指针才出现 |
| 线上是旧版本、main 的表结构已经往前走了 | 从线上版本对应的提交导表再发（临时检出，不建长期分支）。同一个数据修复要给两条线各发一包 |
| 某个方法把库打爆了 | 这不是配表的事，用热关停 |

## 13. 落码分批

每批独立可审、可回滚。前三批不依赖 etcd、对象存储和 K8s，用文件夹具就能测完。

| 批 | 内容 | 主要改动面 | 完成后得到什么 |
|---|---|---|---|
| 1 | 导表器：`schema_digest`、`(cfg_hot_reload)`、manifest 第 2 版、三端常量；C++ 与 Go 的生成模板改成 `TableSet` 加两阶段；启动时按 manifest 核对文件 | `tools/data_table_exporter`（模板与测试）、重新生成的三端代码、`cfg_options.proto` | 行为不变。启动路径多一道文件核对，补上 Java 仓记的那条待做 |
| 2 | 进程侧热更：读 `POINTER.json`、准备、校验、定时提交、写 `STATUS.json`、指标；scene / gate 钩子拆分；Go 各服务的派生数据两阶段；`--validate-tables`；`tables-reload-local` | `cpp/libs/engine`（节点与配置）、`cpp/nodes/{scene,gate}`、`go/shared`、`go/{guild,login,match,scene_manager,mail,rank}`、`dev_tools.ps1` | 本机改表两秒生效 |
| 3 | 战斗每局绑定 | `cpp/libs/services/battle/data`、`cpp/nodes/battle/logic`、守卫测试 | 战斗表可以进可热更名单 |
| 4 | 分发：`go/table_sync`（sidecar 与 `table-bundle`）、gateway_node `/admin/tables`、部署脚本接线（sidecar、两个卷、Secret、dev 的桶）、告警与契约测试、运维手册 | 新 Go 模块、`java/gateway_node`、`tools/scripts`、`deploy/k8s` | 集群上能用 |
| 5 | 逐表放开：按审计结论给表加 `(cfg_hot_reload)` | `data/schema`、本文附表 | 第一批候选：`MessageLimiter`、活动排期与活跃度三张、神秘商人的规则与商品、`GuildShop` |

五批已于 11-20 全部落码。验证命令与通过标准不写在本文，统一见[交接单](../handoff/config-table-hot-reload-handoff-20261126.md) §3。

批 5 每放开一张表，在本文末尾追加一行：谁读它、读出来的东西存没存、换表后怎么跟上、删行有没有存档引用。

## 14. Java 版与客户端

- **Java 版**：待做，本批只有设计。两版共用的是表包格式、manifest 第 2 版、`schema_digest` 的算法、可热更名单和 §7 的本地目录约定；`table-sync` 与语言无关，Java 版可以直接用。指针放哪由 Java 版按自己的惯例定（它已有 Nacos）。Java 版的 `ConfigTables` 本来就是不可变快照、加载时已按 manifest 校验，离这套方案比本仓近。落码批次完成时在 Java 仓 `PARITY.md` 登记。
- **客户端**：协议不变，不需要升级。客户端看得到的表内容（名字、展示价格）变了，要另走客户端的资源更新；两边短时间对不上时，靠已有的报价核对让客户端刷新。服务端表包和客户端资源从同一次导表产出，这件事由发布流程保证，不在本文范围。

## 15. 不做的事

- **不上 Nacos、Apollo**（§3）。
- **不给每个 zone 一个指针**（§6.2）。
- **不做按比例放量**。轨道只有两条，与发布轨道设计同一决定。
- **不热更进程的部署配置**（yaml 里的超时、连接池）。go-zero 和 C++ 节点都不支持，改了就滚动。
- **不做通用的键值开关**。游戏数据一律进表，只有一个事实源；紧急止血用热关停；zone 维护用已有接口。
- **不做常驻的发布控制器**。到点前的自动撤销由发布工具在一次发布里完成。
- **一个进程里不同时服务两套表**。唯一的例外是 battle 的房间，因为一局必须从头到尾一套。
- **不改启动时的严格程度**（§8.1）。
- **不管客户端资源下发**。

## 16. 待核实（2026-11-20 落码前逐项核实结论）

1. **C++ 后台线程**：核对 `test_cpp_thread_sources.py` 登记清单，确认 Kafka、AsyncLogging、Agones 后台线程完全不读配置表；`LoadTablesAsync` 仅在启动期使用并在返回前全部 `join`。
2. **`ExcelExpression` 无状态求值**：已在 `cpp/libs/engine/config/table_expression.h` 保留 `registeredParamNames` 并新增纯 `const` 栈上局部变量求值 `Evaluate(expr, params) const`，生成的 `XxxTableSnapshot::Evaluate*` 同步切换为 `const` 方法（旧 `SetParam`/`Value` 路径保留）。
3. **`TableBattleDataProvider::LoadBattleRewards`**：读外部 `etc/battle_rewards.json`（不在 `generated/tables/`）。改为启动期加载一次为 `std::shared_ptr<const BattleRewardConfig>` 并在每次热更 `Validate` 时针对候选 `TableSet` 重新校验奖励物品与数值。
4. **Go 读表服务审计**：`guild`（30 处）、`login`（3 处）、`mail`（1 处 `Item`）、`rank`（1 处 `Item`）、`scene_manager`（`World` 启动结构表，神秘商人三表与 `Item` 用时现读）全部现查不缓存行指针；`match` 的 `Catalog` 与 `battlescript` 改为两阶段编译 + `atomic.Pointer` 原子替换；`player_locator` 经确认零读表点，已删掉无用 `table.LoadTables` 且不挂 sidecar。`go/shared/tablequery` 已在早期提交移除，当前不存在。
5. **`(cfg_owner)` 规则**：`ColumnDef.is_server` 定义为 `owner in ("server", "common")`，与 `proto_table.proto.j2` 完全一致，`compute_schema_digest` 复用同一判定。
6. **Java 产物**：`java_table_loader.java.j2` 已增补 `SCHEMA_DIGEST` 与 `isHotReloadable(String)` 常量；本仓 `java/gateway_node` 新增 `/admin/tables`（含 etcd `mod_revision` CAS）；独立 Java 仓留到 `PARITY.md` 待做。
7. **Agones Fleet 第二个原生 sidecar**：`New-AgonesFleetYaml` 已改为只要日志 sidecar 或 `table-sync` 任一启用即显式输出 `spec.template.spec.container: $FleetName`；`New-BattleFleetYaml` 原本就恒输出 `container: battle`。
8. **单表解析分片耗时**：C++ 热更按每轮 `queueInLoop` 解析 1 张表分片，暴露 `table_reload_slice_max_seconds` 指标供线上观测，后续大表时可直接切 `.pb` 加载。

## 17. 批 5 可热更表逐表审计台账（`option (cfg_hot_reload) = true`）

| 表名 | 谁读它 | 读出来存没存长期结构 | 换表后怎么跟上 | 删行是否有存档引用风险 |
|---|---|---|---|---|
| `MessageLimiter` | gate (`message_limiter.cpp`) | 否，每次消息现查 | 下次消息限流自动读新快照 | 无（消息号限流规则不进存档） |
| `ActivitySchedule` | scene | 否，按 id 现查 | 立即读新排期与开关 | 仅 `content_identity` 收藏引用，不得复用/删除进行中活动 id |
| `ActivityVitalityRule` | scene | 否，按 id 现查 | 立即读新周期与档位 | 无直接行指针引用，修改周期须递增 `configuration_revision` |
| `ActivityVitalityReward` | scene | 否，按 id 现查 | 立即读新奖励档位 | 玩家已领记录按 id 记账，当期进行中勿删已领行 |
| `MysteryMerchantRule` | scene、scene_manager | 否，按行 1 现查 | scene 经 `Validate()` 校验后生效，报价核对兜底版本窗口 | 无（规则单行 id=1） |
| `MysteryMerchantGoods` | scene、scene_manager | 否，按 id 现查 | `weight=0` 当轮即停售，报价四项核对兜底 | 限购记录仅跨当前一轮，勿在当轮改义已有 id |
| `GuildShop` | go/guild | 否，按 id 现查 | `ValidateEconomyTablesIn` 整批校验通过后生效 | 限购计数按 goods_id 记账，停售改状态/不复用旧 id |
| `GuildDonate` | go/guild | 否，按 id 现查 | `ValidateEconomyTablesIn` 整批校验通过后生效 | 仅记录当日捐献次数，不存行指针 |
| `Skill` | battle、scene、match | battle 每局绑定 `TableSet`；match 编译官方剧本 | battle 新开局按指纹绑定新套表，旧局不受影响；scene 刷新指纹；match 两阶段重编官方剧本 | 角色技能槽/装备词条按 `skill_id` 引用，勿删已有 id |
| `Buff` | battle、scene、match | 同 `Skill` | 同 `Skill` | 战斗内瞬态；仅公式/剧本按 id 引用，勿删已有 id |
| `Cooldown` | battle、scene | battle 每局绑定 `TableSet` | battle 新开局生效，scene 刷新指纹 | 无持久引用 |
| `SkillPermission` | battle、scene | battle 每局绑定 `TableSet` | battle 新开局生效，scene 现查 | 无持久引用 |
| `Monster` | battle、scene、match | battle 每局绑定 `TableSet`；match 校验剧本 | battle 新开局按指纹绑定新套表，match 两阶段重编官方剧本 | 副本/剧本按 id 引用，删行前先确认 `Dungeon` 无引用（导表外键守卫） |

**2026-11-26 复核：台账之后又放开的 20 张表。** 上表写完之后，各功能线陆续给自己的表加了 `(cfg_hot_reload) = true`，可热更名单现在是 33 张，多出来的 20 张没有逐行补进台账。这次只核了两件事：读它的代码有没有在启动时把内容存成长期结构、它的校验有没有接进热更的拒绝闸门。「删行是否有存档引用风险」一栏没有逐张核，由各功能线的设计稿负责。

| 表 | 谁读它 | 存没存长期结构 | 校验接没接进闸门 |
|---|---|---|---|
| `SeasonRule`、`SeasonSeal` | scene `player_season` | 否，用时现查 | `PlayerSeasonSystem::ValidateTables` |
| `TrialJournalEntry`、`TrialJournalMilestone`、`TrialJournalSeal` | scene `player_trial_journal`；`TrialJournalEntry` 另被 match 编进 `Catalog` | scene 否；match 两阶段重编 | `PlayerTrialJournalSystem::ValidateTables`；match 在 `Prepare` 阶段编译 |
| `EquipAffixQuality`、`SkillScrollCraft` | scene `player_craft` | 否 | `PlayerCraftSystem::ValidateTables` |
| `SpiritShopGoods`、`SpiritShopLimitGroup` | scene `player_spirit_shop` | 否 | `PlayerSpiritShopSystem::ValidateTables` |
| `RewardedAdSlot` | scene `player_rewarded_ad` | 否 | `PlayerRewardedAdSystem::ValidateTables` |
| `TravelPassSeason`、`TravelPassTier` | scene `player_travel_pass` | 否 | `PlayerTravelPassSystem::ValidateTables`（**11-26 才接进闸门**，之前只在提交后打日志，坏表能热更上线） |
| `BattleProgressionReward`、`BattleProgressionRule`、`PlayerLevelExp` | scene `player_attribute` | 否 | `PlayerAttributeSystem::ValidateProgressionTables` |
| `FestivalBeastActivity`、`FestivalBeastTier` | scene `player_festival_beast` | 否 | 没有校验函数 |
| `JianghuRule` | scene `player_jianghu`、match `jianghu_duel_logic` | 否，按行 1 现查，缺行用内置缺省值 | 没有校验函数 |
| `GuildWarRule`、`GuildWarMilestone` | go/guild `war` | 否 | `war.ValidateTablesIn`（在候选表上校验） |

以后给一张表加 `(cfg_hot_reload) = true`，同一批补一行到这里；带 `ValidateTables` 的 scene 系统要同时登记 `TableLoadHandler::Validate` 与 `OnCommitted`。
