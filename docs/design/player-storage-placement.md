# 玩家存储落点(Placement):归属与存储拆分、单玩家搬库、Phase 2 全局库

> **状态**: v2.3 — 2026-09-28(v2.3 只改 §4.3 / §9 / §10.1 的能力标记参数口径)。v1 经 5 视角对抗评审(42 条成立,含 2 条 blocker)后重写,见 §16;v2.1 按同批落码的实现汇报修订了与实现不符之处(各节标「实现口径」);v2.2 同步复核修复的行为变化(标「v2.2」)。汇总见 §16。
> 代码与本文同批落码,**未编译、未跑测试**(AGENTS.md §10.1,交 Codex / 用户验证);落码清单与验证命令见 PROGRESS.md「2026-09-28 玩家存储落点」条目(§11.3)。
> **范围**: 把「玩家属于哪个区」(home_zone,逻辑归属)与「玩家主数据存在哪个库」(placement,物理落点)拆开;
> 合服只改归属、不再搬玩家行;新增冻结式单玩家 / 批量搬库;TiDB Phase 2 全局库落成「多一种落点库」,可逐玩家迁移、逐玩家回退。
> 同批修补合服工具 `tools/merge_zone` 的已确认缺口(§12)。
> **关联**: [global-data-layer-tidb-decision.md](./global-data-layer-tidb-decision.md)(Phase 2 路线,本文修订其落地方式)、
> [cross-zone-scene-travel.md](./cross-zone-scene-travel.md)(CZ-2 共享 Redis、owner_epoch)、
> [server-merge-gap-fixes.md](./server-merge-gap-fixes.md)、[../ops/merge-zone-runbook.md](../ops/merge-zone-runbook.md)、
> [zone-home-and-physical-storage-conversation-20260924.md](./zone-home-and-physical-storage-conversation-20260924.md)(概念讨论原文)。

---

## 0. 结论(先读这节)

1. **两个字段,两种操作**:`player:zone:{id}`(home_zone)回答「属于哪个区」;新增 `player:placement:{id}`(落点库 + 版本)回答「数据在哪个库」。
   **合服只改 home_zone**(pin 模式,不拷行);**搬库只改 placement**。
2. **选库只在 go/db 一处发生,按「当前记录」选**:go/db 处理每条任务时一次 MGET 读 `player:placement:{id}` 与 `player:zone:{id}`,
   有效落点 = 落点记录 ?? home_zone ?? 本进程 zone。生产者(C++ scene、login)**不带任何落点字段**,下发链一行不改 —— 落点只有一份真相。
3. **搬库 = 冻结 → 等在途写完成 → 拷贝 → 切换**,不要求玩家离线。冻结期间 go/db 把该玩家的写延后(不耗重试次数);
   切换后这些写和任何迟到写都按既有的 per-key 顺序游标落到新库。搬库不换 topic、不换分区,所以游标在搬库前后是同一个全序。
4. **旧数据零迁移即可上线**:没有落点记录的玩家,有效落点 = home_zone,与今天的写路径等价;只有合服 pin / 搬库 / 批量钉落点时才写记录。
5. **Phase 2 = 新增一个非 zone 落点库**(默认 id `1000000`,库名 `player_store_1000000_db`):新号可直接钉到全局库,存量按 zone 分批搬,旧 zone 库无人指向后退役。
   **生产切换仍受 TiDB 决策 §D8 卡住**(应用级回档未通电,§11.4)。
6. 顺手修掉两处老隐患:login 预加载按「login 所在 zone」读库(访客冷缓存会读空档、被当新号覆盖,§2.3);合服窗口里源区归属玩家仍能从别的区进场、往已排空的源 topic 写(§12 A16)。

## 1. 术语

| 名词 | 含义 | 存放 | 谁改 |
|---|---|---|---|
| home_zone | 逻辑归属:榜单、市场、公会、频道、合服按它分组;也决定存盘 topic(CZ-2) | mapping Redis `player:zone:{id}` | 建角登记;合服改写 |
| 落点记录 | 玩家主数据(`player_database` / `player_database_1` / `player_centre_database`)所在库 + 版本 | mapping Redis `player:placement:{id}` | 建角钉(可选);合服 pin;批量钉;搬库 |
| 有效落点 | 有记录 = 记录的库;无记录 = home_zone 的 zone 库;都没有 = 处理任务的 go/db 所在 zone 库 | — | — |
| storage_id | 落点库编号:`1..999999` = `zone_{id}_db`;`≥1000000` = `player_store_{id}_db` | — | — |
| 冻结 | 记录处于搬迁中(`…:frozen:{run_id}`):go/db 延后该玩家的写,读照常 | 同落点记录 | 只有搬库工具 |

## 2. 现状与问题(代码核对,2026-09-28)

### 2.1 今天「home_zone = 存储库」由三处隐式维持

- 写:C++ `SavePlayerToRedis` 按 `PlayerHomeZoneComp` 选 `db_task_zone_{home}`(`player_lifecycle.cpp` 约 2477-2521);go/db 一进程一 zone,消费哪个 topic 就写哪个库(`go/db/db.go:56-62`)。
- 建角:home = login 所在 zone,首条行由首次存盘写入 home 库。
- 合服:先把玩家行从 `zone_src_db` 拷到 `zone_dst_db`,再改 `player:zone`(`tools/merge_zone/main.go` 步骤 1 → 5)。

### 2.2 为什么要拆

合服被迫跨库搬玩家行(最重、最难续跑的一步,§12 A4);想把一批玩家挪到别的库(退役机器、Phase 2)时,没有「只改落点」的手段,也没有防旧写的办法。

### 2.3 读路径隐患(本设计顺带修)

login 预加载按 **login 自己的 zone** 发读任务(`go/login/login.go:85-86`),go/db 读不到行时返回「成功 + 空档」(`key_ordered_consumer.go` 约 320-326),
scene 拿空档按新号初始化后按 home topic 存盘 —— 访客冷缓存时**空白角色覆盖真实行**。按有效落点读之后,访客从任何 zone 进来都读 home(或落点)库。

## 3. 不变量

- **P-1 单一真源**:玩家主数据只以有效落点库为准;其他库里的同 id 行是冷副本,在线路径不读不写。
- **P-2 版本只增**:落点记录的 version 只由搬库切换 +1;冻结 / 解冻不改版本。
- **P-3 选库只看当前记录**:go/db 选库只看处理那一刻的记录(外加落库后的复核,§6.3),不信任消息里的任何落点信息 —— 消息里也没有。
- **P-4 搬库不换 topic**:落点变化不改变玩家的存盘 topic 与分区,go/db 的 per-key 顺序游标(`consumer:applied:{topic}:{key}:{msgType}`)在搬库前后连续,
  「游标更新的写落到当前落点」因而永远正确。home_zone 变化(合服)会换 topic,只能在停服排空下进行(P3/P4)。
- **P-5 缺席即旧语义**:没有记录时有效落点 = home_zone;`Placement.Required=true` 后缺席一律 fail-closed。
- **P-6 过期 topic 的写不落库**:写任务所在 topic 的 zone ≠ 玩家当前 home_zone,说明它产生于 home 变化之前(合服前的滞留写),进死信,不写任何库。

## 4. 数据形态

### 4.1 键与值(Go 侧唯一出处 `go/shared/placement`;`tools/merge_zone` 是独立 module,镜像一份,用同一组测试向量字面量钉住)

```
键:   player:placement:{player_id}                 # 与 player:zone 同库(data_service MappingRedis,恒 DB 0),无 TTL
值:   "{storage_id}:{version}"                      # 稳定态,例 "102:1"
      "{storage_id}:{version}:frozen:{run_id}"      # 冻结(搬库进行中);run_id 仅 [A-Za-z0-9_-],≤64
```

- 值畸形 → 读方 fail-closed(按查询失败处理),**不得**当作缺席。
- `storage_id`、`version` 必须 > 0。

### 4.2 落点库名(确定性派生,不配置)

| storage_id | 库名 |
|---|---|
| `1..999999` | `zone_{id}_db`(与 `go/db/internal/config.ZoneDBName` 同规则) |
| `≥1000000` | `player_store_{id}_db` |

常量:`FirstNonZoneStorageID = 1000000`,`DefaultGlobalStorageID = 1000000`。库名由 id 唯一决定,go/db 与工具都不需要 id→库名配置表(评审 #16)。

### 4.3 能力标记

go/db 启动成功后写 `db:capability:zone:{Z}` = `placement-routing-v1`。合服 pin 模式与搬库工具在动手前检查相关 zone 的标记,
没有标记 = 那个 zone 此刻没有新版 go/db 在跑(旧版按本进程 zone 选库),拒绝执行。**写过记录之后禁止把 go/db 回退到旧版**(§13)。

实现口径(v2.2,标记是心跳,不是一次性登记):
- 启动时同步写一次,`SET … EX 90s`;之后每 30s 续写一次(`go/db/internal/kafka/placement_route.go`:`PlacementCapabilityTTL=90s`、`PlacementCapabilityRefreshInterval=30s`,单次写预算 5s)。
- 进程退出**不主动 DEL**(同一 zone 可能还有别的副本在跑),由 TTL 收尾。回退到旧版、进程被 kill 或崩溃后,标记在 90s 内自然消失。
- 标记只证明「最近 90s 内这个 zone 有新版 go/db 在续写」。原先的无 TTL 标记在回退或新旧混跑后会继续替旧版作证,工具据此放行,旧版会把被钉走的玩家写进错库。
- 续写失败打 ERROR(每个间隔至多一条),恢复后打一条 INFO;Redis 恢复后下一拍自动补写,**不需要重启**。启动期写失败不拒启。
- **管不住的两个窗口**只能靠部署纪律(§13):滚动发布新旧 Pod 重叠期间,标记已经存在;回退后 TTL 到期之前,标记仍残留。
- **与合服 T-0 流程的交互**(v2.3,负责人决定;工具已改,未经运行验证):runbook 的 T-0 在合服之前对 src、dst 都跑 zone-down,两区的 go/db 随之停掉,90s 后两把标记都消失,原先的缺省 `src,dst` 在 C 阶段必然被拒。现口径:
  - `-db-capability-zones` **没有缺省值**。值是逗号分隔的 zone 号,或字面量 `none`(「此刻没有别的 zone 在跑,不检查」);缺省 / 空串即拒绝并提示;`src` / `dst` 记号删除。
  - pin 合服、pin 清单的撤销、copy 合服中清单玩家已有落点记录的情形:必填,接受 `none`。列表里出现 src 或 dst 照常检查,已下线就被拒,提示写明「src/dst 在 T-0 已下线,请只列仍在运行的 zone;dst 的能力改在 zone-up 之后用 `-mode capability-check` 核对」。copy 撤销给了才查。
  - `-mode relocate`:必填,不接受 `none`(搬库在线进行,所有在跑的 zone 都必须列出并有标记);清单玩家 home zone 自动补查保留(§9)。
  - 新增 `-mode capability-check -db-capability-zones <list>`:只读,逐 zone 报告 present / missing / unreadable(值 ≠ `placement-routing-v1` 算 missing 并打印实际值,读失败算 unreadable),退出码 0 / 1 / 2 与 audit 同口径(2 优先);不接受 `none`。参数用法错误(没给 / `none` / 写错)按 2 退出:一个标记都没查 = 没查成,不与 missing 的 1 同码(否则运维会照 exit 1 的处置去换 go/db)。runbook §8 Step 6:dst zone-up 之后、`/open` 之前必须 exit 0,否则不开服、先把 dst 的 go/db 换成新版。
  - 只有两个 zone 的环境在 T-0 写 `none`。撤销的 S 段另有一条独立检查(§10.4「设计没写、按正确性补上」第 5 条):有「记录 ≠ src」的人时仍要求 src 的标记,src 的 go/db 必须在跑。

### 4.4 持久化

落点记录与 `player:zone` 同一风险等级(Redis 是唯一在线真源)。本批的兜底:
(1) `Placement.Required=true` 后缺席即 fail-closed,不会静默写错库;
(2) 合服 / 搬库清单(manifest)逐人记录最终 `(storage_id, version)`,Redis 整体丢失时按清单重放 + 其余玩家按 home_zone 回填。
TiDB 决策 D1 的「映射持久化兜底表」仍是遗留项(§11.4)。

实现口径(v2.1):
- 重放没有专用工具,步骤见 runbook「映射丢失的恢复」。合服清单的 `placement_existing` 记下合服前已有的记录,没有记录的人按模式补:pin 为 `"{src}:1"`,copy 为无记录。搬库清单逐人的 `final` 就是切换后的值。
- **兜底有一个缺口**:`PinOnCreate` 且 `NewPlayerStorageId` 指向非本 zone 库(Phase 2 全局库)建出的新号,任何 zone 库里都没有行,也不在任何清单里,Redis 整体丢失后 home 与落点都无从恢复。打开这种配置之前,要么先落 D1 兜底表,要么明确接受这一风险(已列入 §11.4)。这一条是文档同步时发现的,不是实现偏差,设计本身未改。

### 4.5 Redis 层不拆(CZ-2 保持)

落点只管 MySQL 玩家主数据。`PlayerAllData:{pid}`、`{MsgType}:{pid}`、`battle:settlement:pending:{pid}`、资产通道回执、账号目录等仍在全 zone 共享的同一 Redis,键不带落点,搬库不迁它们。
依赖这一点的读者(scene 加载、match 读 `PlayerAllData`、battle 离线结算)本批不改。

## 5. 协议变更(只追加字段号)

| 消息 | 变更 | 用途 |
|---|---|---|
| `RegisterPlayerZoneRequest`(`proto/data_service/data_service.proto`) | `uint32 storage_id = 3;` | 非 0:与 `player:zone` 同一段 Lua 原子钉 `"{storage_id}:1"` |
| `GetPlayerHomeZoneResponse` | `bool home_zone_merging = 2;` | home_zone 正处于合服围栏内(`merge:in_progress:{home}` 存在),scene_manager 拒绝进场(A16) |

**生产者消息(`DBTask`、`RoutePlayerEvent`、`PlayerEnterGameNodeRequest`)不加任何字段**(v1 曾加,评审后撤回,§16)。

## 6. go/db:多库 + 按记录选库

### 6.1 库注册表

- 启动时照旧打开本 zone 库并做既有断言;其余库在第一次被指向时按需打开(singleflight),之后常驻。
- 可打开 = 名字在 `dbguard` 白名单里,**或** `Placement.AllowStoreFamilies=true`(默认)且名字属于 `zone_{1..999999}_db` / `player_store_{≥1000000}_db` 两个家族。
  按需打开**从不建库**,只 Ping + 只读 schema 闸;白名单防「ZoneId 填错静默建新库」的原有语义不变。
- 前提:所有落点库与 go/db 的 `Database.Hosts` 在同一 MySQL 实例 / TiDB 集群(与合服工具同一前提)。
- 每库独立 `*sql.DB` + proto2mysql 模型(proto2mysql 靠 DSN 默认库选库);额外库连接池 `Placement.ExtraStoreMaxOpenConn`(默认 8)、`ExtraStoreMaxIdleConn`(默认 2)。不设库数上限。
- 按需打开失败 = **延后重试且不耗重试次数**(与排序锁忙同口径),ERROR 日志 + `db_placement_store_open_total{result="error"}`;不进死信(死信无消费者)。

实现口径(v2.1):
- **不耗预算只对写生效,读照常耗预算。** 「写进死信 = 丢盘」对读不成立:读不改数据,等待方早已超时。读也豁免的话,库长期打不开时每次登录都往重试队列加一条永远排不完的读,会挤掉真正需要重试的写。
- **单次按需打开预算 10s,失败后冷却 10s。** 同一个库的并发请求在 singleflight 上等打开结果,所在的 worker 子分片跟着停住。没有冷却,一个连不上的库会让每条指向它的任务都在 worker 上等满超时,同子分片里无关的玩家一起被拖住。冷却期内直接复用上次的错误,不重复计数、不重复打 ERROR。
- `db_placement_store_open_total` 的 result 多一个 `rejected`(不被放行,属于配置问题),与 `error`(连不上 / 库不存在 / schema 闸不过)分开。
- 本 zone 库照旧由 `InitDB` 打开,再登记为落点 ZoneId。启动期有两道拒启:ZoneId 不在 `1..999999`;`config.ZoneDBName(ZoneId)` 与 `placement.StoreDBName(ZoneId)` 不一致。
- **proto2mysql 核实结论**:生成的 SQL 都是裸表名,库由连接池 DSN 的默认库决定。f3b308f 版 `OpenDB` 只在池里一条连接上 `USE`,main(83fed85)版改为只校验 `SELECT DATABASE()`。所以多库必须一库一池,不能靠 `USE` 切换。实现两种 `OpenDB` 下都正确,最终以 v0.2.0 实跑为准。
- `cmd/migrate -storage-id N`:库名 = `placement.StoreDBName(N)`,放行与业务服务调同一个 `AdmitStore`(白名单或家族)。但 `-create-database` 仍只认外部白名单 `DB_ALLOWED_DATABASES`,因为业务服务从不按需建库。外部白名单为空且不是 warn 档时拒绝。`N=0` 逐字节保持旧行为。

### 6.2 选库规则

一次 MGET 读 `player:placement:{key}`、`player:zone:{key}`(`Placement.Redis`,默认复用 `RedisClient`;dev / K8s 下与 mapping Redis 同实例 DB 0)。记本进程 zone 为 `Z`。

**写任务**(在既有排序锁、游标守卫、owner_epoch 守卫之后):

| 情形 | 结果 | outcome |
|---|---|---|
| MGET 失败 / 值畸形 | 延后重试(耗预算,同其他 Redis 故障) | `lookup_error` |
| home 存在且 `home ≠ Z` | 进死信,不落库(P-6) | `stale_topic` |
| 有记录且冻结 | 延后重试,**不耗预算** | `frozen_deferred` |
| 有记录且稳定 | 落 `record.storage_id` 库 | `placed` |
| 无记录,`Required=true` | 进死信 | `missing_required` |
| 无记录,home 存在 | 落 home 库(此时 home == Z) | `home` |
| 无记录,home 缺席 | 落 Z 库(旧语义) | `legacy` |

**读任务**:不看冻结(冻结期间源库不再变化,读它得到的正是将被拷走的内容);有记录落记录库,无记录落 home 库,都没有落 Z 库。
`Required=true` 且无记录 → 任务失败。home 与 Z 不同是常态(访客从别区登录),不适用 P-6。

- 批内合并:只合并同一 `(key, msgType)` 的写,选库发生在合并之后的单条任务上,规则不变。
- 共享缓存 `{MsgType}:{pid}` 的回写在复核(§6.3)通过之后。

实现口径(v2.1):
- 判定由纯函数 `decideWritePlacement` / `decideReadPlacement` 按上表逐格给出(`go/db/internal/kafka/placement_route.go`)。`stale_topic` 与 `missing_required`(写)复用既有的 `quarantineOrderingConflict` 进死信。
- **home 值 ≥ 1000000 按畸形处理(`lookup_error`)**:它派生出的是 `player_store_*`,不是任何 zone 的库。
- **`Required=true` 且无记录的读**:立即向 `task:result:{id}` 发 `Success=false` 回执(login 预加载秒级失败),然后 ACK;不重试、不进死信。回执写失败才按 Redis 故障延后。
- 选库那次 MGET 发生在 `op_total` 计时之外。`Placement.Redis` 缺省时复用 `RedisClient`。**它必须指向 mapping Redis 的主节点**,配成只读副本会破坏 §6.3 的保证。

### 6.3 落库后复核(关掉 TOCTOU)

选库时记下记录快照 `(present, storage_id, version, frozen)`。SQL 执行完、写回缓存与标记游标之前,再读一次记录:

| 选库时 | 复核时 | 写任务 | 读任务 |
|---|---|---|---|
| 稳定 `(s,v)` | 稳定 `(s,v)` | 通过 | 通过 |
| 稳定 `(s,v)` | 冻结 `(s,v)` | **可重试,不标记游标**:工具可能已在锁过期后拷完,不能确定本次写被拷走 | 通过(冻结期间源库不变) |
| 任意 | 版本 / 库变化,或记录出现 / 消失 | 可重试,不标记游标 | 丢弃结果并重试(不写共享缓存) |
| 无记录 | 无记录 | 通过 | 通过 |

- 可重试的写再来时:冻结中 → 延后;已切换 → 落新库。旧库那一份按 P-1 是冷副本。同一内容可能落两次,整行 REPLACE,幂等;
  若在此期间已有更新的写落了新库,重投的这条被游标判为过期丢弃,符合 per-key LWW。
- 因此正确性**不依赖排序锁 TTL**:即使锁过期、工具提前拷贝,落在旧库的那次写也会在切换后重投到新库。R2 等锁只是减少重投与 R4 比对失败。

实现口径(v2.1):
- **「无记录 → 无记录」这一格还要比 home_zone,比上表更严。** 无记录玩家的库由 home 决定。copy 模式合服若恰在 SQL 与复核之间拷走行并改了 home,只比记录会判通过并标记游标,这笔写就静默留在已不是真源的源库。现在判据是「有效落点(home ?? Z)变化 = 路由变化」:可重试,重投时按 P-6 进死信。代价只是罕见窗口里多一次重试。有记录时仍只比 `(storage_id, version)`。判定表见 `recheckPasses`。
- **复核那次读 Redis 失败**:按普通 Redis 故障处理,计 `lookup_error`,耗预算,不标记游标。
- 复核卡在公共路径 `runDBOp` 里,顺序固定为 SQL → 复核 → 缓存回写 / 读结果发布,任何 handler 都绕不过去。复核不通过计 `recheck_moved`,同时计入 `db_task_result_total{result="error"}`(与其他可重试失败同口径);`op_handler` 阶段耗时现在包含复核那次读。
- 重试预算豁免(`retryBudgetExempt`)汇总:排序锁忙;落点冻结;复核发现落点变化(读写都豁免);落点库打不开(只豁免写)。
- (v2.2)日志口径:冻结期间重试任务的重排只打 DEBUG `retry task rescheduled (placement frozen)`,否则批量搬库时每轮重试都刷一条 ERROR,淹没真故障。冻结长时间不解除看 `frozen_deferred` 计数告警(§14)。落点库不可用(写)等其他原因的重排仍打 ERROR `retry task rescheduled`。

## 7. C++(只补一件:DB task topic 世代号,Phase 2 前置,合服审计缺口 48/61)

- `GetDbTaskTopic(zoneId, generation)`:`generation ≤ 1` 返回 `db_task_zone_{zone}`,`≥ 2` 返回 `db_task_zone_{zone}_g{generation}`(与 Go `DbTaskTopicForGeneration` 同规则;注意与审计 topic 第一代就带 `_g1` 不同)。
- 世代号来自 `BaseDeployConfig` 新字段 `db_task_topic_generation`(yaml 键 `DbTaskTopicGeneration`,缺键 / 0 = 1),读法照 `AuditTopicGeneration`。
- K8s node ConfigMap 从 go/db `Kafka.TopicGeneration` 取值写入;`start_game.ps1` 与契约测试钉住三方(C++ / go/db / login)相等。
- 下发链与 `SavePlayerToRedis` 的选 topic 逻辑不变(仍按 home_zone)。

实现口径(v2.1):
- 字段是 `BaseDeployConfig.db_task_topic_generation = 22`。
- `player.h` 的 `GetDbTaskTopic` 只做拼接、不读配置;唯一调用点从 `tlsNodeConfigManager` 取世代号后传进去。旧的单参重载已删除。
- scene 只在启动时读这个值,换代必须重启全部 scene。
- 换代时的启动顺序是 **db → login → scene**。broker 开着 auto.create,scene 抢先发消息会把新代 topic 建成 1 分区,db 的分区契约随即永久失败。完整流程见 [db-task-kafka-partition-contract.md](./db-task-kafka-partition-contract.md) 离线扩容一节。
- 三道门禁在同一处核对三方相等:
  - `start_game.ps1` 启动前核对,缺键、非正整数或不一致都中止;
  - `k8s_deploy.ps1` 生成 node ConfigMap 时从 go/db 的 `ServerConfig.Kafka.TopicGeneration` 取值,同时核对 login,不一致就 throw;
  - 契约测试 `k8s_deploy_contract.tests.ps1`。

## 8. Go 服务

### 8.1 data_service

- `RegisterPlayerZone`:`storage_id != 0` 时,同一段 Lua 在 SETNX `player:zone` 成功后 SETNX `player:placement:{id} = "{storage_id}:1"`;home 已存在则两键都不动(冲突语义不变)。合服围栏语义不变,实现上改为「预检 + 提交点原子复核」:写入前单独 EXISTS 预检(查询失败当封锁),写入脚本开头再 EXISTS `merge:in_progress:{home}` 复核,命中回 3 → `ErrZoneMergeInProgress`、两键都不写。堵的是「预检放行 → merge_zone 立围栏并收完清单 → 脚本才写入」的窗口(否则该玩家不在清单里,合服后映射仍指向源区,落点也不在任何清单里)。
- `GetPlayerHomeZone`:一段 Lua 读 `player:zone:{id}`,存在时再 EXISTS `merge:in_progress:{home}`,返回 `home_zone_merging`。其余语义不变。

实现口径(v2.1):
- **落点记录已存在时保留原值。** 如果 home 这次新写成了,`player:placement` 却已经存在(例如映射被 `DeletePlayerZone` 删掉、落点记录还留着),脚本返回 2:不覆盖落点,登记照样算成功,只打 ERROR `[RegisterPlayerZone] placement record already present, kept as is`。理由是按 P-1,已有记录指向数据真正所在的库,用请求里的 storage_id 覆盖会让后续读写去错库;而让登记失败又会卡住建角。运维看到这条日志,应核对该玩家的落点记录。
- **内部路由不查围栏。** RPC 用的是新增的 `Router.GetPlayerHomeZoneAndMergeFence`;`ClientForPlayer`、snapshot、debug_fetch 等内部路由继续用单 GET 的 `GetPlayerHomeZone`,不为每次调用多付一次 EXISTS。
- **围栏读失败按合服中处理(fail-closed)。** 脚本里用 `redis.pcall` 读围栏,读失败时返回 `home_zone_merging=true`,并打 ERROR `[GetPlayerHomeZone] merge fence unreadable, treated as merging`。Redis 整体故障仍返回 Unavailable,NotFound 的文案契约不变。
- **部署前提:mapping Redis 是单实例。** `GetPlayerHomeZone` 的脚本访问了未在 KEYS 里声明的围栏键(执行前拿不到 home),现状是 go-zero node、DB 0。(`RegisterPlayerZone` 的写入脚本已把围栏键显式声明为 `KEYS[2]`,落点键为可选的 `KEYS[3]`,但三个键同样不带 hash tag。)如果以后改成 Cluster,EXISTS 会跨 slot 报错,走 pcall 失败分支,结果是拒绝全部进场,不会静默放行。那时这段脚本要重新设计。
- 围栏键前缀经 ARGV 传进 Lua,单测钉住「前缀 + zone == `placement.MergeFenceKey(zone)`」。
- `debug_import` 导入玩家时不钉落点(storage_id=0)。
- **遗留**:`DeletePlayerZone` / `DeletePlayerZoneIfLocked` 只删 `player:zone`、不删 `player:placement`。删号流程与落点记录的关系待定(§11.4)。

### 8.2 scene_manager

- `resolveHomeZone` 得到 `home_zone_merging=true` 时返回 `ErrHomeZoneMerging = 21`(可重试,在任何写之前拒绝)。
  合服窗口内源区归属玩家不能从任何 zone 进场,从而不会往已排空的源 topic 写(A16,评审 #5)。
- 其余不变:不查、不下发落点。

实现口径(v2.1):
- **拒绝收在 `resolveHomeZone` 里面**,三个调用点天然都覆盖到:跨区第一条腿预检、同区首次落点 / 换图 / 同落点重连(3b)、第二条腿消费等待落点。
- **映射缺失时忽略 merging**(NotFound / 旧版 Unknown 文案 / `HomeZoneId=0`),按既有未映射策略处理。旧版 data_service 不填这个字段,读出来是 false,行为不变。
- **同区路径顺序有调整。** 核对发现同节点 owner_epoch 补种(Setnx)原来在 3b 之前,属于「先写后拒」。现顺序:换手门 → 3b 归属查询(含合服围栏)→ 同节点补种 → guard 组装 → 同落点重连 / 预占 / 落点。换手门只 GET 标记,且与补种互斥(一个要求跨物理节点,一个要求同节点),所以挪动后任何请求的结局都不变。唯一可观察的差异:补种失败(19)与归属查询失败(20)或合服拒绝(21)同时成立时,现在先回后者。几种都是可重试码,状态都不改。
- **不覆盖的两种入口:**
  - 只送连接的跨区重定向(含 login 首登 `RedirectOnEnter`):不写 location / epoch,它的第二条腿会在目标 zone 的 3b 被拒。
  - 没有 GateId 的 EnterScene:不发路由事件,也不让节点加载玩家;但**照常**铸 epoch、CAS 写 location、扣旧场景人数,跨节点时还会派发 ReleasePlayer。它不查归属(`TestEnterScene_HomeZoneNotQueriedWithoutGateRoute` 钉住),所以也不过合服围栏。「拒绝先于任何写」只对带 GateId 的请求成立。
- **前提**:没有 GateId 的入口不被合服围栏覆盖,靠的是「所有生产调用方都带 GateId」(login `entergamelogic.go`,以及 C++ 侧 4 处 `EnterSceneRequest` 构造点,如 `scene_manager_response_handler.cpp:142`)。以后新增不带 GateId 的调用方(疏散 / GM / 机器人)之前,必须先把 3b 的围栏检查从 GateId 条件里拆出来:不带 GateId 时也调 `resolveHomeZone`,只采用它的拒绝结果,放在同节点补种与第 5b 步之前,同时修改上述用例。`enterscenelogic.go` 第 3b 步的注释交叉引用了这条前提。
- 指标:`scene_manager_enter_scene_rejected_total{reason="home_zone_merging"}`,zone_id 为 gate zone;`home_zone_lookup_total` 对这类请求仍记 `outcome=mapped`。
- 21 在 C++ scene 和 login 都按通用失败处理:scene 解冻并回 tip;login 推 `kEnterSceneFailed`,保留会话。客户端没有专门文案。

### 8.3 login

- CreatePlayer:`Placement.PinOnCreate`(默认 false)为 true 时,把 `Placement.NewPlayerStorageId`(0 = 本 login 所在 zone)作为 `storage_id` 传给 `RegisterPlayerZone`。
  Phase 2 打开时填全局库 id;打开 `Required` 之前**必须**先打开 `PinOnCreate`(§13)。
- 读路径不改:读任务仍发往本 zone topic,由 go/db 按有效落点选库(§6.2),§2.3 的访客读错库随之修复。

实现口径(v2.1):
- 配置块 `Placement`(整块 optional,零值即关闭):`PinOnCreate`(bool,默认 false)、`NewPlayerStorageId`(uint32,默认 0 = `Node.ZoneId`)。取值收口在 `PlacementConf.StorageIDForNewPlayer`。
- `etc/login.yaml` 里写成显式关闭,而不是注释掉。(v2.2)键名拼写由 `placement_conf_test.go` 的必填探针 `placementKeysProbe` 机械守住:`TestLoginYamlSpellsOutPlacementKeys` 用它加载 `go/login/etc/login.yaml` 与 `deploy/login-stack.linux/login.yaml`,段名或键名写错即加载失败;`TestPlacementKeysProbeRejectsTypos` 是探针自己的反例。零值断言 `TestEtcYamlPlacementBlockIsOff` 只守「当前是关着的」。
- **零值只在 go/db 未开 `Required` 时是安全方向**(v2.2):任一 go/db 已开 `Placement.Required=true` 之后,`PinOnCreate` 不是回滚开关,规则见 §13「回退」。
- 核对依据:
  - 启动日志 `[placement] pin_on_create=<bool> new_player_storage_id=<id>`;
  - 建角日志 `[home-zone] registered player:zone ... storage_id=<id>`(0 = 不钉)。
- **打开 `PinOnCreate` 之前 data_service 必须已是新版。** 旧版会把 storage_id 当 proto3 未知字段忽略,结果是「钉失败、建角成功」,且没有报错。
- 不校验 `NewPlayerStorageId` 的取值范围:任何非 0 值都合法(§4.2),库是否存在由 go/db 在第一次被指向时判定。

## 9. 搬库(`merge_zone -mode relocate`)

输入:`-relocate-player-ids` 或 `-relocate-source-storage`(整库),`-relocate-target-storage`,`-relocate-batch-size`(默认 100),`-manifest-path`,
`-db-capability-zones`(必须覆盖所有在跑的 zone;没有缺省值,不接受 `none`,§4.3)。

| 步骤 | 内容 | 失败处置 |
|---|---|---|
| R0 | 校验 S≠T、两库存在、列名对齐;检查 `-db-capability-zones` 每个 zone 的能力标记;清单(run_id、S、T、玩家、逐人状态)**在任何写之前**落盘 | 拒绝 |
| R1 | 冻结(每人一段 Lua,原子完成下列检查与写):有效落点必须 == S(无记录时要求 `player:zone == S`);`merge:in_progress:{home}` 不存在;记录不在冻结态。通过则写 `"{S}:{v}:frozen:{run}"`(无记录按 v=1) | 不满足的人跳过并记入清单 |
| R2 | 等在途写:对每人每张玩家表,等排序锁 `distributed:lock:kafka:ordering:{topic}:{key}:{msgType}` 不存在(topic 按其 home_zone 与世代号拼) | 超时 → 本批解冻 |
| R3 | 拷贝:每人一个事务,把各玩家表的行从 S 拷到 T。T 已有该玩家的行 = 冷副本(P-1,R1 已证明 T 不是当前落点)→ 同事务先删后插,计数记入清单 | 回滚该人事务,解冻该人 |
| R4 | 逐人逐表逐字节比对 S 与 T | 不一致 → 解冻该人 |
| R5 | 切换:Lua CAS `"{S}:{v}:frozen:{run}"` → `"{T}:{v+1}"`,逐人记入清单 | CAS 失败(记录被人动过)→ 报警,该人保持原状 |

- 不要求离线:冻结期间在线玩家的存盘被延后,切换后按游标落到 T;Redis 里的 `PlayerAllData` 与库无关,不受影响。
- 源库的行保留为冷副本;撤回 = 反向 relocate(R3 会覆盖 S 里的冷副本)。
- 工具崩溃:冻结态带 run_id 留在 Redis,该玩家的写持续延后(不丢,指标 `frozen_deferred` 上涨应告警);
  `-mode relocate -manifest-path <同一份>` 续跑,或 `-mode relocate-abort -manifest-path` 把本 run 的冻结全部解回稳定态。
- 与合服互斥:R1 的 Lua 检查合服围栏;合服在立围栏后检查清单玩家无冻结记录(§10.1)。
- 缓存:不删。T 的内容 = S 在冻结时刻的内容,之后的写都经游标落 T。

实现口径(v2.1):
- **R1 的原子性。** 条件在 Go 侧判定,用与 go/db 同一套编解码。Lua 只拿刚读到的记录原值与 home 原值做 CAS,同时检查围栏,再写入。判定依据的两个值在写入那一刻被原子确认没变,与「Lua 里完成检查与写」语义等价。不在 Lua 里再写一份记录解析,是为了避开前导零、64 位版本号超出 double 精度造成两份解析分叉。
- **`-relocate-source-storage` 始终必填**(R1 要判「有效落点 == S」)。`-relocate-player-ids` 只用来缩小范围。不给则整库搬:记录指向 S 的,加上 S 为 zone 库时无记录且 home==S 的。整库枚举的是扫描那一刻的集合,期间新钉到 S 的人要再跑一次。
- **R2 期限**:新增 `-relocate-lock-wait`,默认 150s,略大于 go/db 排序锁 TTL 2m。锁键核实自 go/db 源码,是 `distributed:lock:kafka:ordering:{db_task_zone_<home>[_g<gen>]}:{pid}:{表名}`(msgType 就是表名),在共享 DB 0。每把锁至少观察到一次不存在即通过;超时则整批解冻。
- **能力检查加严。** 除 `-db-capability-zones` 外,还自动检查清单玩家的 home zone。列表外且无标记的 zone,其玩家在 R1 跳过,不整批拒绝:处理他们存盘的 go/db 无法证明按落点选库。参数没有缺省值、不接受 `none`,`src` / `dst` 记号已删除(v2.3),必须写显式 zone 号。
- **清单**:`kind="relocate"`、`relocate_version=1`,每批在 R1 后、R5 后各落盘一次。崩溃后清单可能落后于 Redis,续跑时从 Redis 认领本 run 的冻结值或 `"{T}:{v+1}"`。逐人终态有 `switched` / `skipped` / `unfrozen` / `cas_failed`;逐人 `final` 是 Redis 丢失时重放的值。dry-run 只写 `<path>.dryrun.json`。
- **解冻结果。** 原本无记录的玩家解冻后留下稳定态 `"{S}:1"`,不删键:有效落点与冻结前相同,版本不变(P-2)。`relocate-abort` 之后该清单不能再续跑。
- **退出码**:全部切换成功为 0,否则为 1。R5 的 CAS 失败记为 `cas_failed`,需要人工处理。
- 前提:S、T 都在 `-mysql-dsn` 所在的同一个 MySQL 实例上。全局库用 `go/db/cmd/migrate -storage-id <id> -command up -create-database` 预建。

## 10. 合服

### 10.1 pin 模式(`-player-rows-mode pin`,默认)

前置:`-db-capability-zones` 列出的 zone 全部有能力标记,否则拒绝并提示改用 copy 模式或先升级 go/db。参数**没有缺省值**(v2.3,§4.3):写全部仍在跑的 zone;T-0 时 src、dst 已 zone-down,只列其他仍在跑的 zone,没有就写 `none`(不查);列了 src / dst 照常检查、已下线即被拒。dst 的能力在它 zone-up 之后、开服之前用 `-mode capability-check -db-capability-zones <dst>` 核对,必须 exit 0。

1. 立围栏后、写任何东西之前:清单玩家不得有冻结记录。
2. 钉落点:清单里每个玩家,若无记录则 SETNX `"{src}:1"`(有效落点就是 src);已有记录的不动。
3. 不拷玩家行、不拷 data Redis blob、不删玩家缓存。
4. 其余步骤照旧:公会 zone_id、聚宝斋 market_zone、公会榜、按清单改 `player:zone`、热状态、公告。
5. 仍要 P3/P4(home 变了,存盘 topic 从 src 换到 dst)。合服窗口内 scene_manager 按 A16 拒绝源区归属玩家进场。
6. unmerge:只改回 home;落点保持 src(与 home=src 一致)。pin 清单的撤销同样必填 `-db-capability-zones`(可 `none`,口径同上)。
7. 清单记录 `player_rows_mode`;续跑必须同模式,verify / unmerge 按清单里的模式判断。

### 10.2 copy 模式(兼容旧 go/db)

照旧拷行,但**只处理没有落点记录的玩家**;有记录的玩家行在其落点库,不拷。

### 10.3 批量钉落点(`-mode pin-placement -zone N`)

给 home==N 且无记录的玩家 SETNX `"{N}:1"`。有效落点不变,在线执行安全。全部 zone 钉完且 `PinOnCreate` 已开,才能打开 `Required`。

### 10.4 实现口径(v2.1)

**步骤顺序**:P1 → R → C → F → R2 → 0 → G → P2~P7 → N → S → X → M → 步骤 1~7(`tools/merge_zone/main.go` 顶部注释)。
- R(围栏之前):读既有清单,拒读 dry-run 预览,核对 src / dst 与本次一致。拿错清单时不碰围栏就退出。
- C(围栏之前):**首跑**的 pin 模式检查能力标记。
- R2(v2.2,围栏之下,只在续跑时):续跑必须与清单同模式(没有 `player_rows_mode` 字段的旧清单按 copy 处理);copy 模式下玩家表集合不得变化;pin 模式核对能力标记。这三项原先在围栏之前 `log.Fatalf`,挪下来是因为续跑时运维已按中止文案 DEL 了上一次的围栏,两个 zone 处于半合服状态,拒绝时不该让它们敞着。
- S:两种模式都在 N 之后、M 之前扫一遍清单玩家的落点记录。有冻结或畸形记录即拒绝;已有记录写进清单 `placement_existing`,续跑时逐人比对,不一致即拒绝。
- X(v2.2):合走标记检查提前到清单之前,见 §12 A10。
- 步骤 1:pin 模式叫 `pin_placement`,copy 模式仍叫 `player_rows`。
- F 与 M 之间的拒绝(R2 / 0 / G / P2~P7 / N / S / X / M)统一走 `refuse`,围栏处置见 §12 A2。

**pin 模式细节**:
- 每人一次 CAS:期望无记录、home==src,写入 `"{src}:1"`。记录已是 `"{src}:1"` 或等于清单记下的原值,视为正常;其余情况中止并保留围栏。
- 与 pin 矛盾的 `-skip-player-rows` 直接拒绝。

**blob 与 data Redis(待拍板)**:data_service 按 home_zone 选 data Redis 集群。src 与 dst 不在同一集群时,pin 合服后 `player:{id}:*` 会读不到。所以工具在 pin 模式下拒绝 `-migrate-player-blobs`(除非同时给 `-skip-player-blob-migration`),提示改用 copy 模式。copy 模式仍为全部清单玩家拷 blob(blob 跟 home 走),拷行与删缓存只针对无记录者。§10.1 第 3 条「pin 不拷 blob」在多集群部署下是否保留,需要拍板(§11.4)。

**设计没写、按正确性补上的五条**:
1. copy 模式下,清单玩家里只要有人已有记录,同样要求能力标记:他们的行不动,dst 的 go/db 必须按记录选库。
2. unmerge 同样检查冻结记录并拒绝,因为撤销也会改 home。
3. unmerge 在 copy 模式下,1' 跳过此刻有落点记录的玩家:目标库那一行可能正是他的真源(例如合服后被 pin-placement 钉在了目标区)。
4. 续跑比对 `placement_existing`(见上文 S)。
5. (v2.2)unmerge 的 S 段:清单玩家里有「落点记录 ≠ src」的人(含畸形记录;例如合服后被 pin-placement 钉在 dst,或被 relocate 搬走)时,要求 **src** 的能力标记,不满足则在任何写之前拒绝。这些人的有效落点不随 home 改回,撤销之后处理他们存盘的是 src 的 go/db,旧版会写进 `zone_src_db` 里的旧副本。两种模式都查;只查 src,因为撤销之后 dst 不再处理这批人。记录 == src 或无记录的人不计入。能力标记是 90s 心跳(§4.3),所以此时 src 的 go/db 必须在跑。

**verify:manifest_rows 的判据**:每人「落点记录 ?? dst」所指库的 `player_database` 都要有行。设计原文是「记录 ?? home」,两者在 `verify:manifest_mapping`(逐人断言 home==dst)通过时等价;不通过时按 home 去查第三区的库只会得出一条难懂的 INFRA。放行条件:copy 模式看 `player_rows` 步骤、pin 模式看 `pin_placement` 步骤是否完成。

**pin-placement 细节**:
- N 有合服围栏或 `merge:merged_into` 标记就拒绝;逐人 CAS 时再检查围栏,运行中出现围栏立即停下。
- 每批 500 人。报告 `candidates / pinned / already_pinned / kept_existing / home_moved`。
- 不要求能力标记,因为钉的值等于 home,有效落点不变。

**清单版本号未升**:新字段都可缺省,旧清单按 copy 读。搬库清单用 `kind`,新版合服加载器认出 kind 即拒读,旧版加载器读到 `version=0` 也拒读。

## 11. Phase 2(全局库)

### 11.1 做法

1. 建库:`go/db/cmd/migrate` 新增 `-storage-id`(库名按 §4.2 派生):`-storage-id 1000000 -command up -create-database`(TiDB 方言表选项已在 proto 上,§D3)。
2. go/db 默认允许 `player_store_*_db` 家族,无需逐 zone 配置;关掉 `AllowStoreFamilies` 的环境须把库名加入白名单。
3. 新号:login `Placement.PinOnCreate=true`、`NewPlayerStorageId=1000000`。
4. 存量:按 zone `-mode relocate -relocate-source-storage N -relocate-target-storage 1000000`,可在线分批。
5. 退役:`-mode storage-audit -storage N` 报告有效落点仍为 N 的玩家数,为 0 后下线 `zone_N_db`。
6. 此后合服一律 pin 模式,零玩家行迁移(TiDB 决策 D7 的目标语义)。

### 11.2 与 TiDB 决策文档的差异

决策文档设想一步把玩家表收敛到全局库;本设计拆成逐玩家可回退的搬迁,任何时刻每个玩家只有一个真源。topic、partition 契约、写合并、per-key LWW 全部不动(D5)。

### 11.3 本批落码

见 PROGRESS.md 条目「2026-09-28 玩家存储落点(placement)+ 合服工具缺口修复 落码」(以实际提交为准)。该条目列出了各组件改动的文件、未验证项、交给 Codex 的验证清单(按依赖排序)与剩余风险。**全部未编译、未测试。**

### 11.4 仍卡住生产切换的前置(本批不做)

- **§D8 应用级回档**:`RollbackFence` 生产为 nil,且回档写的是无人读取的 `player:{id}:<field>` 键空间。混库后「只恢复 zone_N_db」不再等于「回档 zone N」。
- 映射(`player:zone` + `player:placement`)持久化兜底表(D1)。
- 真实集群演练:pin 合服 + relocate + 反向 relocate,各跑一次并记录耗时。
- 压测:go/db 每条写多一次 MGET、落库后一次 GET,按 AGENTS.md §6 出对比表后再下性能结论。
- data_service 号段水位地板在 `player_database` 与全局库同库时才生效(`store/schema.go`、`id_segment_store.go`),全局库落地后要复核。
- 同表结构副本(`go/player_locator/mysql_database_table.sql`)与按 `zone_<N>_db` 清库的脚本(`stress_round19.ps1`、`currency_crash_window.ps1`)在混库后要改。
- (v2.1 补)pin 合服在 src / dst 分属不同 data Redis 集群时的 blob 处置(§10.4),目前工具要求这类部署改用 copy 模式。
- (v2.1 补)删号流程与 `player:placement` 的关系:`DeletePlayerZone*` 只删 `player:zone`,残留的落点记录会在同 id 重新登记时被保留(§8.1)。
- (v2.1 补)钉到非 zone 库的新号在 Redis 整体丢失后无法恢复(§4.4),依赖 D1 兜底表。
- (v2.1 补)跨 zone 读写共享缓存 `{MsgType}:{pid}` 的竞态:访客在 B 区预加载时,读 A 库并回写缓存,可能与 A 区 go/db 的写回交错,把缓存盖回旧版本。这是既有问题(以前 B 读的是错库),本批只是缩小了窗口。

## 12. 合服工具缺口修复(本批)

| # | 缺口 | 修法 |
|---|---|---|
| A1 | 映射完整性守卫只比行数,可被「有映射无行」抵消 | 集合比较:源库行的 player_id 里**没有任何 `player:zone` 映射**的即拒绝(有映射但指向别区的是冷副本,不算) |
| A2 | 围栏晚于收集;改映射全量扫描、不受清单约束 | 先立围栏再收集;围栏之后任何预检失败走正常释放(不 log.Fatalf);改映射按清单逐键 CAS(值==src → dst),`已是 dst` + `本次改成功` 必须等于清单人数 |
| A3 | dry-run 写清单,apply 同路径被当续跑 | dry-run 不写 `-manifest-path`,改写 `<path>.dryrun.json` 供人看;apply 不读它 |
| A4 | 步骤 1 半途失败重跑被 ID SAFETY 卡死;公会重名断言在写之后 | 撞号预检在任何写之前对全部表做完;续跑时目标行与源行逐字节相同(按列名全列比较,含目标独有行检查)视为已拷;失败走保留围栏中止路径;重名断言挪到围栏之后、清单之前 |
| A5 | 清单 Tables 每次被覆盖 | 步骤 1 完成后不再覆盖;续跑发现表集合变化即拒绝 |
| A6 | `-verify-merged` 下界断言含目标区原住民 | 要求清单;逐 id 核对:映射 == dst;copy 模式目标库有行,pin 模式有效落点库有行 |
| A7 | unmerge 无前置门禁、不失效玩家缓存 | 复用预检(目标区口径):目标区无活节点、清单玩家无会话、目标 topic 排空;copy 模式补缓存失效 |
| A8 | 公会榜快照可能陈旧 | 步骤 4 在维护锁内重读源榜,以重读结果为准,实际写入集合回写清单;重读为空时不写(快照只作撤销依据,v2.2) |
| A9 | 审计 guild_member 失败降级为 info | 未跳过 guild 时改为 INFRA;孤儿查询错误不再吞掉;跳过 guild 时报 skipped |
| A10 | 合服后映射丢失时回填会把人钉回源区 | 改映射**之前**写 `merge:merged_into:{src}`;回填拒绝对它执行;unmerge 删除。Redis 整体丢失的恢复按清单重放(runbook) |
| A11 | 清热状态时 zone_id=0 的旧 location 失去反查依据 | 删场景键之前,对全量 location 做一次 zone_id=0 检查 |
| A12 | dev_tools 不转发 guild 跳过开关;DB 15 残留 | 补转发与新参数;清理注释 |
| A13 | 文档:P7 组队门禁、T-1 彩排口径、zone-down 前排空、K8s 无 guild、RollbackPlayer 不可用等 | runbook / gap-fixes / 设计文档同步 |
| A14 | C++ db_task topic 无世代号 | §7 |
| A15 | TiDB 上步骤 3 / 3b 及撤销的自动提交点更新走乐观 2PC,与 DisbandGuild 悲观锁互等 | 每个 id 一个显式悲观 RC 短事务:`SELECT … WHERE pk=? FOR UPDATE` → 带 zone 复核的 UPDATE → COMMIT;保留既有逐条点更新 / 升序 / 复查计数 / 1213·1205 有界重试 / 保留围栏中止(死锁审计 #17,`ff39a13f1`) |
| A16 | 合服窗口内源区归属玩家仍可从别区进场,往已排空的源 topic 写 | §8.1 / §8.2 |

实现口径(v2.1):
- **A2 的围栏释放。** M 之后的失败(步骤 1~6 与清单 persist)一律走 `abortKeepingFence`:保留围栏,打印 `LEFT IN PLACE` 与本次 run_id,以及「核对 run_id → DEL → 用原命令重跑」的续跑指引。F 之后、M 之前的拒绝统一走 `refuse`(v2.2),按是否处于半合服状态分流:
  - **首跑**(没有清单):本次什么都没写,`mergeFence.refuseAndRelease` 打原因 → 释放围栏 → exit 1。
  - **续跑**(清单已存在,且步骤 7 `post_merge_flag` 未标记完成):上一次运行可能已写到一半,改走 `abortKeepingFence` 保留围栏,文案带「续跑在清单更新之前被拒」、`LEFT IN PLACE` 与本次 run_id。判据用「清单存在」而不是某一步的完成标记,因为清单落盘之后、步骤 1 标完成之前也可能已提交了几张表。
  - **清单已标记步骤 7 完成**:上一次已跑完全程,dst 可能已经开服,照常释放围栏。
  - **撤销侧**:立围栏后先只读判断是否已有清单玩家的 `player:zone == src`(`manifestPlayersBackAt`,分批 MGET)。有 = 半撤销(上一次撤销在 5' 之后中止),P、S 段的拒绝改走 `abortKeepingFence` 保留围栏;MGET 失败也按半撤销处理(fail-closed)。否则照常释放。副作用(偏保守):合服当初带了 `-skip-player-mapping`,或合服在步骤 5 之前停下后改跑撤销时,清单玩家都还是 src,撤销的门禁拒绝也会保留围栏。unmerge 里 7'/5'/4' 等写之后的失败路径沿用原 `log.Fatalf`,同样保留围栏。
  - 原先的已知风险「续跑在清单落盘前被拒会释放围栏」由此修复(v2.2)。
- **A2 的改映射。** Lua CAS 返回 0 键不存在 / 1 本次改成 / 2 已是 dst / 3 指向第三区。清单外仍指向源区的玩家只打 WARN,开服前由 `verify:mapping_src` 拦住。
- **A4**:纯判定 `decideTableCopy`;续跑时「与源行逐列 `<=>` 相同的行数 == 目标行数 == 源行数」才视为已拷。撤销 1' 复用同一个 `identicalRowCondition`。公会重名断言没写专门回归:`uk_guild` 保证帮名全局唯一,测试库里触发不了。
- **A6**:删除 `verify:target_zone_rows` 与 `verify:mapping_src` 里的下界断言,`verify:mapping_src` 只保留「源区清零」。新增 `verify:manifest_mapping`(清单人数与 `-expected-src-players` 不符时直接 block)与 `verify:manifest_rows`(步骤未完成报 warn NOT VERIFIED)。清单缺失或是 dry-run 预览时报 INFRA(exit 2)。
- **A7**:撤销完整复用 `runPreflight` 的 P2~P7(按目标区口径,含 P5 锁与 P7 组队),需要 `-kafka-consumer-groups-cmd` 或 `-assume-kafka-drained`。
- **A8**:写 ZSET 之前先把要写的集合落进清单;清单详情为 `members=N source_gone=bool`(v2.2,原为 `from_snapshot`)。v2.2 起:
  - 锁内重读源榜为空时**一律不写**目标榜,只 DEL 源键空壳。源榜为空只可能是续跑(上一次 MULTI/EXEC 已并走)或 guild 服合法清空(公会全部解散、按 MySQL 重建),两种情况下再 ZADD 快照只会回退分数、复活已解散的公会。纯判定 `chooseRankWrite`。
  - 目标榜改用 `ZADD NX`,不覆盖 guild 服在步骤 3 之后写进目标榜的新分数。
  - 清单新增 `rank_members_unwritten`(omitempty):M 阶段读快照时置 true,交给写入回调时清成 false。快照从没交给过写入时,撤销依据记为空,撤销不会把已解散的公会 ZADD 回源区。旧清单没有这个字段,读作 false(「可能写过」),撤销依据按旧行为保留快照。
  - 步骤 4 做完后,M 阶段不再重读、不再覆盖 `rank_members`。
- **A9**:同时给 `-skip-guild-mysql` 与 `-skip-guild-rank` 才算「跳过帮会」,审计的 `guild_member` 报 SKIPPED(warn)。v2.2 起 `-verify-merged` 的 `verify:guild_zone` / `verify:guild_rank` 也认这两个开关,同样报 SKIPPED(warn),不再恒为 INFRA;只给一个开关时照常断言。
- **A10**:标记值是 JSON `{"dst":N,"run_id":"..."}`,无 TTL。v2.2 起在清单落盘之前(X 段,`checkMergedIntoBeforeMerge`)先查一次:源区已合进别的 zone、目标区自己已被合走、标记读失败或读不懂,都在第一次写之前拒绝(走 `refuse`)。步骤 5 的 `markMergedInto` 保留同一判定作为最后一道防线。步骤 5 已完成或带 `-skip-player-mapping` 时不查。unmerge 只删 dst 与 run_id 都对得上的那一把,对不上只打 WARN。
- **A11**:补扫跳过清单玩家(第 1 步已按全部口径处理过);报告新增 `zone0_sweep`。
- **A12**:dev_tools 已补齐转发(帮会跳过、撤销的 Kafka 门禁、`-VerifyMerged` 的清单、pin/copy、能力标记、四个新命令);`Get-MergeMappingRedis` 注释里的「(今天是 15)」已删。契约测试是 `tools/scripts/tests/dev_tools_merge_zone_contract.tests.ps1`。(v2.2)退出码透传:`Invoke-MergeZoneGo` 改为在 `tools/merge_zone` 里 `go build` 到一个 GUID 临时路径、直接执行产物,最后显式 `exit $code`,merge_zone 的 0 / 1 / 2 原样成为 `pwsh -File` 的进程退出码;编译失败时工具不运行,退出码为 2。修复前经 `pwsh -File` 调用恒为 0,`go run` 又把非零码压成 1。契约测试新增子进程用例钉住这一点。
- **A15**:不在 `BeginTx` 里再指定隔离级别,会话级 RC 由 DSN 保证。每行 4 次往返。TiDB 侧仍以 `tidb_txn_mode=pessimistic` 为部署前提。
- **未 import `go/shared/placement`**:shared 模块会带入 sarama、go-zero、etcd、grpc 与更新版 go-redis,等于给独立 module 引入新依赖。按 §4.1 镜像为 `placement_codec.go`,测试向量逐字相同(用 diff 核对)。

## 13. 上线顺序

1. data_service、scene_manager(A16;新字段缺省 false,行为不变)。
2. go/db(`Required=false`):无记录时按 home / Z 选库。与今天的差别只有两处:读任务按 home 读(修 §2.3)、home ≠ Z 的写进死信(P-6)。
   上线后观察 `db_placement_guard_total{outcome}`:`stale_topic` 应恒 0(非 0 说明有 home_zone 未知回落进程 zone 的存盘,先查 C++ `home_zone_unknown`)。
3. C++ 世代号(只在世代号仍为 1 时上线,topic 名不变)。
4. 以上全部就绪、能力标记齐全后,才允许:pin 合服、relocate、`PinOnCreate`。
5. 可选:所有 zone `-mode pin-placement`,`PinOnCreate=true` 之后再 `Required=true`。

**回退**:第 4 步之前任何组件可独立回退。第 4 步之后**禁止把 go/db 回退到旧版**(旧版按本进程 zone 选库,会把被钉到别处的玩家写错库);在第 5 步之前,其余组件可回退。
第 5 步之后(任一 go/db 已开 `Placement.Required=true`),还禁止以下操作(v2.2):
- 把任何 login 的 `PinOnCreate` 改回 false(它不是回滚开关);
- 把 `NewPlayerStorageId` 改成没有 go/db 能打开的库;
- 把 login 或 data_service 回退到不支持 storage_id 的旧版。

违反时建角照常成功、login 零报错,但新号只有 `player:zone`、没有 `player:placement`,预加载秒级失败(§6.2 读任务 `Success=false`),存盘全部进死信(`missing_required`)。要回退这些组件,先关掉全部 go/db 的 `Required`。

**能力标记的部署纪律**(v2.2,§4.3 的两个管不住的窗口):
- 第 4 步之前回退某个 zone 的 go/db 后,必须等满 90s,或在 mapping Redis DB 0 上 `DEL db:capability:zone:{Z}`,然后才能执行 pin 合服、relocate,或打开 `PinOnCreate`。
- go/db 发布完成(新 Pod 全部 Ready、旧 Pod 全部退出)之前,不得执行上述操作:新旧 Pod 重叠期间标记已经存在,心跳管不住这个窗口。

实现口径(v2.1)补充的上线约束:
- **第 1 步**:A16 要 data_service 与 scene_manager **都**上线后才生效,两者先后不限。
- **第 2 步**:
  - 访客预加载会由登录 zone 的 go/db 按需打开 home 区的库,所以所有 zone 库 / 落点库必须与各 go/db 的 `Database.Hosts` 在同一 MySQL 实例或 TiDB 集群。
  - `AllowStoreFamilies=false` 的环境必须把所有可能的 home 区库写进白名单,否则访客登录失败。
  - K8s 的 go-svc-db ConfigMap 不写 `Placement` 段,落点 Redis 复用 `RedisClient`,它与 data-service 的 MappingRedis 同实例、同为 DB 0,由 `k8s_deploy_contract` 钉住。以后拆开 mapping Redis 时,必须同时给 go/db 显式写 `Placement.Redis`。
  - 能力标记在消费者启动、metrics 起来之后同步写一次,随后每 30s 心跳续写(TTL 90s,§4.3)。写失败只打 ERROR(前缀 `[placement]`),不拒启;Redis 恢复后下一拍自动补写,不需要重启。
- **第 4 步**:打开 `PinOnCreate` 之前还要确认 data_service 已是新版(§8.3)。
- **第 5 步**:顺序固定为:全部 login 开 `PinOnCreate` → 全部 zone 跑完 `-mode pin-placement` → 才开 go/db 的 `Required=true`。
- **合服工具默认 pin**:go/db 未升级、没有能力标记的环境,合服会在 C 阶段被拒,必须显式传 `-player-rows-mode copy`。

## 14. 指标(低基数,不带 player_id)

- `db_placement_guard_total{op,outcome}`(outcome 见 §6.2;加 `recheck_moved`)
- `db_placement_store_open_total{result}`、`db_placement_open_stores`(gauge)
- scene_manager:既有拒绝指标加 `reason="home_zone_merging"`

实现口径(v2.1):
- `db_placement_guard_total{op=read|write}` 的 outcome 取值:`placed|home|legacy|stale_topic|frozen_deferred|missing_required|lookup_error|recheck_moved`。复核读失败也计 `lookup_error`;`frozen_deferred` 每个重试周期计一次(v2.2:冻结期间的重试重排只打 DEBUG,不打 ERROR,冻结是否卡住只能看这个计数,§6.3)。
- `db_placement_store_open_total{result=ok|error|rejected}`,每个冷却期最多计一次;`db_placement_open_stores` 含本 zone 库。
- scene_manager 的 zone_id 为 gate zone。
- 告警建议(本批未落 `deploy/k8s/*-alerts.yaml`):
  - `stale_topic` 应恒为 0;
  - `frozen_deferred` 持续上涨 = 冻结未解除(搬库崩溃,查 relocate 清单);
  - `recheck_moved` 只该在搬库窗口出现;
  - `home_zone_merging` 在合服窗口外持续非 0 = `merge:in_progress` 围栏残留。

## 15. 测试要求

- `go/shared/placement` 编解码表驱动测试;`tools/merge_zone` 镜像用同一组字面量。
- go/db:§6.2 每一格;§6.3 复核通过 / 切换后重投 / 读结果丢弃;冻结延后不耗预算;注册表按需打开、家族与白名单、打开失败延后。
- data_service:`RegisterPlayerZone` 带 / 不带 storage_id 的原子性;`GetPlayerHomeZone` 返回 merging。
- scene_manager:merging 拒绝发生在 location / owner_epoch 任何写之前。
- merge_zone:A1–A16 各一条回归;pin 模式端到端;relocate 冻结 / 等锁超时解冻 / 冷副本覆盖 / 逐字节比对失败 / 切换 / 续跑 / abort;能力标记缺失拒绝;与合服围栏互斥。
- C++:编译后按 §13 第 2、3 步的指标核对。

## 16. 修订历史

- v1(2026-09-28):落点字段沿 `RoutePlayerEvent → gate → PlayerEnterGameNodeRequest → DBTask` 下发,go/db 比对消息版本;搬库靠 LEO 排空 + 离线检查。
- v2(2026-09-28):对抗评审 42 条成立(workflow `wf_b4fec566-ec8`)。两条 blocker:排空之后、切换之前放行的写会落进已拷完的源库;
  切换后的迟到写(含 librdkafka 本地队列里的最后一次存盘)被当作 stale 丢弃,而 Redis 已接受这份数据。另有 11 条 major 源自消息携带版本带来的新旧混跑问题。
  改为:按当前记录选库 + 冻结延后 + 落库后复核 + 依赖 per-key 游标全序;撤回生产者消息上的全部落点字段;库名确定性派生;新增能力标记、A16。
- v2.1(2026-09-28):同批落码后,按各组件实现汇报修订与实现不符之处,已定设计不变,各节以「实现口径(v2.1)」标出。主要偏差:
  - go/db:落点库打不开时只豁免写的预算;按需打开加 10s 预算与 10s 冷却;复核在无记录时连 home 一起比;home ≥ 1000000 判畸形;`Required` 下无记录的读秒回失败;store_open 多一个 `rejected`。
  - data_service:落点记录已存在时保留原值并打 ERROR;围栏读失败按合服中处理;内部路由不查围栏。
  - scene_manager:同区路径把换手门与 3b 前移到同节点补种之前。
  - merge_zone:
    - 搬库的 R1 由 Go 判定加 Lua 原值 CAS;新增 `-relocate-lock-wait`;
    - pin 模式拒绝 `-migrate-player-blobs`(待拍板);
    - copy 模式有记录者也要能力标记;unmerge 拒绝冻结玩家、copy 模式 1' 跳过有记录者;
    - verify 按「记录 ?? dst」判断。
  - 文档同步时新发现两项:钉到全局库的新号在 Redis 丢失后无法恢复(§4.4、§11.4);`-verify-merged` 的帮会断言不认跳过开关(§12 A9)。§11.3 改为指向 PROGRESS 条目。
- v2.2(2026-09-28):按同批复核修复的汇报同步,设计目标不变。全部未编译、未测试。
  - go/db:能力标记改为 90s TTL + 30s 心跳,进程退出不 DEL(§4.3);冻结期间的重试重排改打 DEBUG(§6.3)。§13 补上回退与发布期间的部署纪律。
  - data_service:`RegisterPlayerZone` 的合服围栏改为「预检 + 提交点原子复核」(§8.1)。
  - scene_manager:只改注释与文档;§8.2 如实写明没有 GateId 的 EnterScene 照常写 location / epoch、不过围栏,及其前提。
  - login:§8.3 补键名探针;§13 补 `Required` 打开之后的回退禁令。
  - merge_zone:
    - 续跑 / 半撤销时,清单落盘前的拒绝保留围栏(§12 A2);
    - 续跑的清单校验挪到围栏之下(R2);
    - 合走标记前置检查(X,A10);
    - 步骤 4 源榜为空不写、目标榜 `ZADD NX`、清单加 `rank_members_unwritten`(A8);
    - `-verify-merged` 的帮会断言认跳过开关(A9);
    - unmerge 对「记录 ≠ src」的人要求 src 能力标记(§10.4 第 5 条)。
  - dev_tools:merge-zone 系列退出码透传(§12 A12)。
  - 文档同步时新发现:能力标记改成 TTL 之后,与 runbook T-0 先 zone-down 两区的流程冲突,pin 合服的默认 `-db-capability-zones src,dst` 在 T-0 必被拒(§4.3,runbook §5.1),工具默认值与检查口径待负责人决定。
- v2.3(2026-09-28):负责人对 v2.2 遗留项的决定落码(未编译、未测试)。`-db-capability-zones` 去掉缺省值与 `src` / `dst` 记号,接受字面量 `none`(合服 / 撤销;搬库与 capability-check 不接受);新增只读 `-mode capability-check`(exit 0 / 1 / 2),runbook §8 Step 6 开服前必须 exit 0(§4.3、§9、§10.1)。
