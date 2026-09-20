# friend:移植为全局服务 + 客户端经路由服可达

**Created:** 2026-09-18
**状态:** 2026-09-18/19 由 Claude 分 F1 / F2 / F3 三批落码。**未编译、未运行任何测试、未提交;proto-gen 与导表器一次都没跑过**(本机 `go` 与 `protoc` 都不存在,`E:\work\tools` 曾被清空)。§7 的验证清单一步都还没执行,本文任何"应该 / 期望"都不是实测结论。
**关联:** [xuanming-port-decisions-20260910.md](./xuanming-port-decisions-20260910.md) 的 D-9 / D-10 及其 **D-10 修订(2026-09-18)** / D-11 / D-12 / D-13 / D-14、[microservice-zone-contract-20260914.md](./microservice-zone-contract-20260914.md)(契约 §2 注册 / §3 客户端入口 / §4 数据归属 / §5 推送 / §7 部署登记)、[friend-persistence-architecture.md](./friend-persistence-architecture.md)(B 侧现状文档,已随本次同批更新)、[client-rpc-router.md](./client-rpc-router.md)、[guild-zone-client-access.md](./guild-zone-client-access.md)(结构样板与 G9 同口径)、[xuanming-port-feasibility-20260902.md](./xuanming-port-feasibility-20260902.md) §3.2 的 friend 行

---

## 0. 一句话

`go/friend` 从"仓库里有代码、没人到得了"变成**全局一份、多副本、客户端经 gate → `client_rpc_router` 可达**的服务:协议改名 `ClientPlayerFriend` 并标 `OptionIsClientProtocolService`,身份只从会话取,表搬进独占库 `mmorpg_friend` 并以 proto 为唯一事实源,写路径按统一锁序重写,补上 A 仓有而 B 仓缺的**黑名单 / 推荐 / 频率配额 / 终态申请清理**四块,并新增 S2C 推送与在线状态拉模型。

---

## 1. 背景与裁定

### 1.1 起点

- B 仓的 `go/friend` 不是空目录,但也不是能上线的东西:**没有部署登记**(`deploy/k8s/manifests/go-svc` 里没有它、`go_services.ps1` 的 `$ServiceCatalogue` 里也没有它,见可行性文档 §4.3 的 P1 表);表手写在 `deploy/mysql-init/guild_friend_tables.sql` 里、和 guild 的表挤在共享库 `mmorpg`;身份直接信请求体里的 `player_id`(D-9 的原始证据);构造了 Kafka gate 命令 builder 却**零调用**(可行性文档 §4.3 的 P1「推送通道『已有』≠『可靠』」)。
- A 仓的 `services/social/friend`(2491 行)在可行性文档 §3.2 里的结论是 **「不搬」** —— 桶为"有",落点写死成「四块增量:黑名单 / 推荐 / 终态请求 GC / per-player 配额 → 现有 `go/friend`」,估 18 人日。本次移植**完全照这条执行**:没有整服替换,只把 A 的四块能力与它的并发纪律搬进 B 的形状(D-10「搬不变量,不搬服务」)。

### 1.2 能力盘点与处置

| A 仓能力 | 处置 | 理由 |
|---|---|---|
| 黑名单(单向拉黑,拦双向加好友) | **port_now** | 可行性文档点名的四块增量之一;B 侧完全没有,客户端没有任何手段拒绝骚扰 |
| 好友推荐(共同好友 → 随机兜底) | **port_now** | 同上;SQL 直接改写成 B 的表名,`recommendAnchor` 的 pivot 取法一并搬(见 F12) |
| 每玩家申请频率配额 | **port_now** | 同上;没有它时 `AddFriend` 是一条无成本的写路径,能被脚本按秒刷成无界的 `friend_capacity` 行 |
| 终态好友申请清理(GC) | **port_now** | 同上;`friend_request` 每对 (from,to) 一行、accept/reject 只翻 status、无 TTL,不清就随社交图的"对数"单调涨 |
| 写事务的锁序纪律与 RC 隔离(A 2026-08-11 那次真 MySQL 8.4 压测的根因结论) | **port_now(最高优先)** | 这是 D-10 意义上的"不变量"。B 侧把 `AddFriend` 改成权威事务之后会与既有 `AcceptFriend` 成 ABBA 环,不搬这条纪律就是在生产上埋一颗随机炸的 1213(见 F6) |
| 在线状态:A 的 presence / locator 口径 | **改造** | 不搬 A 的组件,改读 B 已有的跨运行时契约 key `player:session:{id}`(契约 §4)。B 原来的 `friend:online` 从来没有写者(见 F2) |
| `cellroute` 分片路由 | **skip** | 可行性文档 D6a 已拍"不搬":与 B 的 home_zone 目录路由同职责且互斥,且在 B 的 bwmarrin 雪花布局下 `%4096` 只落 8 个 Cell,没有分片价值 |
| 好友榜 / leaderboard 依赖 | **skip(本期)** | B 侧根本没有 `go/leaderboard`(可行性文档 §3.2 列为"缺 / 新建 / 18 人日")。为一个不存在的服务提前留接口是 YAGNI |
| 昵称 / 头像投影 | **port_later** | B 全仓还没有昵称权威(见 `proto/friend/friend.proto:57-58` 的注释:昵称属帮会二期的玩家档案)。`FriendEntry` / `BlockEntry` / `RecommendEntry` 一律只回 id,落地后 append 新字段即可 |
| `dbcapacity` 库容量预算巡检 | **skip** | 可行性文档的执行清单自陈「不接 Guard.Check 容量巡检(预算未推导)」。没有预算数字的巡检只会产出无法处置的告警 |

### 1.3 批次划分

完整移植约 60 个改动文件,超过 AGENTS §10.2 的 30 文件门禁,拆三批(每批一份冻结规格,规格是未跟踪文件,要点已抄进本文与 `PROGRESS.md`):

- **批次 F1**(20 文件,2026-09-18):协议改名 + 表 proto + 服务骨架 + 配置按契约锚点 + `shared/noderegistry` + `-migrate` 入口。
- **批次 F2**(19 文件,2026-09-18/19):事务重写(RC + 全局锁序)+ 黑名单 / 推荐 / 配额 / 清理 / 推送 / 在线状态。
- **批次 F3**(22 文件,2026-09-19):五处登记 + K8s 部署链 + 迁库 + 运维工具改连 + robot 端到端冒烟 + 本文档。

---

## 2. 决策 F1–F17

> ⚠ 这一列编号 **F1–F17 是决策号**,和上面的**批次** F1 / F2 / F3 不是一回事。下文提到批次时一律写全"批次 F1"。

| # | 决策 | 理由与依据 |
|---|------|------|
| F1 | `service FriendService` → **`ClientPlayerFriend`**,并加 `option (OptionIsClientProtocolService) = true` | 不是洁癖:生成器**只**给服务名含 `ClientPlayer` / `GamePlayer` 的服务出 Unity / robot 下行 handler(`tools/proto_generator/protogen/internal/generator/unity/unity_client_handler.go:185`、`generator/go/robot_case.go:138`)。本期新增 S2C 推送,服务不改名就没有客户端收件人 —— 改名是推送的前提。落点:`proto/friend/friend.proto:231-232` |
| F2 | **删** `NotifyOnline` / `NotifyOffline` 两个 rpc 与它们的四个 message;**删** `friend:online` 全套 Redis 键 | 全仓非生成代码零调用方(已 Grep 确认)。更关键的是:这两个 rpc 是 `friend:online` 的**唯一写者**,而它们从来没被调用过 —— 也就是说那套"登录写键、心跳续期、Pull 查在线"的机制**从未运行**,`GetFriendList` 里的 `is_online` 恒为 false。留着它会让下一个人以为在线状态是有的。同时 D-9 已点名:这两个东西向方法**不能**跟着服务级的 `ClientProtocol` 开关对客户端开放,否则任何客户端可声明任意玩家在线 |
| F3 | 在线状态改成**读共享库 `player:session:{id}` 的拉模型**;读失败**降级为全部离线** | `player:session:{id}` 是 login / player_locator / gate 共同维护的跨运行时契约 key(契约 §4),它有真实写者。实现在 `go/friend/internal/data/session_reader.go`(`BatchOnlineStatus` :104 / `FillOnlineStatus` :152)。降级理由:在线状态是展示态,为它让整个好友列表失败不划算;风险是"好友其实在线却显示离线",**可观测性由 `friend_online_lookup_total{outcome="error"}` 承担**,不是静默降级(AGENTS §11.3)。⚠ MGET 的返回长度与入参长度不等时**整批判离线**而不是按下标取值 —— 错位会把 A 的在线状态贴到 B 身上,比"全部离线"严重得多 |
| F4 | 身份**只从会话取**,请求体里不再有 `player_id`(六个老请求体删字段改 `reserved`) | D-9「身份从会话取必须先于客户端可达」。请求体是客户端可伪造的,拿它做权威身份等于把"以他人身份加 / 删好友"交给客户端。删字段而不是留着不读,是因为"字段还在但服务端忽略"最容易让人以为它有效;写 `reserved` 是防 1 号字段将来被新字段复用后,旧客户端的 `player_id` 被静默解成别的值。落点 `proto/friend/friend.proto:82-84` 等六处;会话解码在 `go/friend/internal/session/session.go:44` |
| F5 | 客户端来源只放行**方法白名单**(10 个 C2S),`NotifyFriendEvent` 对客户端 `PermissionDenied` | `OptionIsClientProtocolService` 是**服务级**开关,内部 / 下行方法会跟着一起进 gate 白名单与路由表。白名单默认拒绝,以后新增 rpc 忘了登记就不会意外对外。`session.ClientMethods` 在 `session.go:52`;服务端**同时**不实现 `NotifyFriendEvent`(继承 `UnimplementedClientPlayerFriendServer`),但那是第二道防线 ——"今天返回 Unimplemented"不是安全保证,白名单才是。⚠ 这不违反契约 §3 的「东西向方法不得与客户端 service 同 service」:`NotifyFriendEvent` 是 **S2C 下行声明**而不是东西向调用,且生成器要求它必须落在这个 service 才出得了客户端 handler(见 F1);D-9 给的本来就是「拆 service 或按方法名拒绝客户端来源」**二选一**,friend 选的是后者 |
| F6 | **全局锁序统一为「容量守卫最先」**:`AcceptFriend` 的申请行 `FOR UPDATE` 从守卫**之前**下移到守卫**之后** | 批次 F2 把 `AddFriend` 改成权威事务后,它先锁双方 `friend_capacity`、再碰申请行;而批次 F1 交付的 `AcceptFriend` 恰好相反 —— **同一对玩家"一边接受、一边重发申请"就能撞上 InnoDB 1213**。采用 A 仓 2026-08-11 真 MySQL 8.4 压测出的纪律:**任何写事务在拿到容量守卫之前,不得做任何锁定读**。这不会把 B 当初修掉的那个 1213 带回来:B 修的是"过早把 status 从 1 改成 2 会在 `idx_to_player` 前缀上触发 1213",修法是把 **UPDATE** 延后,而 UPDATE 仍在守卫之后;被下移的只是按 `(from_player_id,to_player_id)` 主键的单行 SELECT。落点:锁序全文在 `go/friend/internal/data/friend_repo.go:97-150`,守卫在 `:299`(AddFriend)与 `:462`(AcceptFriend),`lockCapacityRows` 在 `:662` |
| F7 | 写事务隔离级别固定 **READ COMMITTED**;守卫之后的**判定读一律当前读** | RR 的间隙锁会让"同一玩家并发拉黑 16 个不同目标"这类只碰不同行的事务互相挡(未命中的 `FOR UPDATE` 拿的是同一个间隙锁,排他点落回守卫行,插入意向被其余事务的间隙锁挡住 → 成环),A 仓真 MySQL 8.4 实测必炸,且**只在 MySQL 上炸**(TiDB 没有间隙锁)。RC 没有间隙锁,正确性由"守卫串行化 + 主键"提供。代价是 RC 下同一事务内两次普通 SELECT 可能读到不同结果,所以"判定读在守卫之后用当前读"**不是可选优化,是正确性要求**。前提 `binlog_format=ROW` 已核实(`deploy/k8s/manifests/infra/mysql.yaml:60` 显式设置;本地 compose 未设,取 MySQL 8.4 默认)。常量 `go/friend/internal/data/friend_repo.go:151` |
| F8 | 事务外先按 `player_id` 升序 `INSERT IGNORE` 补齐容量行;缺行**按 `friend` 表权威边数建行,绝不猜 0** | 放进事务里会让多个请求各持自己刚插入的新行、又都抢同一个接收者行,形成 insert-intention 死锁(B 侧原注释已记下这个教训)。"绝不猜 0"是 **D-10 修订**明确要保住的实质不变量之一:猜 0 等于给玩家凭空发一整份好友名额。`ensureFriendCapacityRows` 在 `friend_repo.go:866` |
| F9 | **就绪闸 `friend_capacity_backfill_v1` 退役** | 不在本文重复论证 —— 完整裁定与四条理由见 **[D-10 修订(2026-09-18)](./xuanming-port-decisions-20260910.md)**。一句话:它查的 `guild_schema_migration` 按 D-14 第 8 条留在旧库 `mmorpg`,而 friend 断言自己只连 `mmorpg_friend`(`go/friend/internal/config/config.go:296`),这条查询在任何合法配置下都不可能命中,是可证明的死代码。D-10 的另外三条实质不变量(容量锁行是硬上限 / 缺行按权威边数 / versioned cache + Lua CAS)**一条都没动** |
| F10 | 独占逻辑库 **`mmorpg_friend`**,表以 `proto/friend/friend_table.proto` 为唯一事实源,由 `go/schemamigrate` 经服务二进制 `-migrate` 建 | D-14 第 1/2/3/4 条。四张表(`friend` / `friend_request` / `friend_capacity` / `friend_block`)全部整数主键、**零 UNIQUE KEY**、带 TiDB 打散选项;列名与旧 `guild_friend_tables.sql` 逐字对齐(便于万一要搬存量,但**不保证两边 schema 相同** —— `friend_request` 多了 `updated_ms` 且 `status` 类型变了,见该 proto 文件头的对齐说明)。库名常量 `go/friend/internal/data/tables.go:22`,两道断言:`config.Validate` 比对配置库名、`schemamigrate` 连上后再断言 `SELECT DATABASE()` |
| F11 | 建库**只登记一处**:`deploy/mysql-init/00_init_zone_dbs.sql`;`deploy/mysql-init/guild_friend_tables.sql` 里 friend 的建表段与 `friend_capacity` 回填段**删除** | D-14 第 5 条(建库只登记一处、mysql-init 禁止新增业务表)。删旧脚本不是清理洁癖:回填段会对一张**已经不再建出来的表**执行 UPDATE,空数据卷首次 initdb 会整个脚本失败,连 guild 的表都建不出来。guild 的部分与 `guild_schema_migration` 表本身**一字不动**(guild 的积分迁移门禁还在用它) |
| F12 | 四块新能力的形状:黑名单落**独立表** `friend_block`;推荐走**共同好友 → 随机**两级,随机的 pivot 先取 `MIN/MAX(player_id)` 再在区间内随机;配额是 `FriendRedis` 上的一条单 key Lua;清理默认 `report_only` | 拉黑与删好友是两件事(删好友只断当前关系,对方可以立刻加回来),所以必须落自己的表;且拉黑对象**通常不是**好友,塞进好友边就无处存放。pivot 取法照搬 A 仓注释里的理由:直接 `rand` 会落在现有最大 id 之后,`player_id >= pivot` 一行都扫不到、兜底静默返回空(`go/friend/internal/data/recommend_repo.go:112-145`)。**Block 删边必须按 `RowsAffected` 减 `friend_count`** —— 漏减就是计数上漂、玩家永远加不满好友且零报错(`block_repo.go:50`) |
| F13 | 推送 **at-most-once**,只作"去拉"的触发信号 | 链路是 `kafkautil.PushToPlayer` → `gate-cmd_g<N>` → gate → TCP,**没有**重试、回执或离线补推(契约 §5 列了三种丢失窗口)。所以事件体只带 `reason + by_player_id + ts_ms`,客户端收到后**必须**去拉 `GetPendingRequests` / `GetFriendList`。三条纪律:只在 MySQL 提交之后、事务之外推;失败只打日志 + 计指标、**绝不影响 RPC 结果**(`push.go` 里所有函数都不返回 error,从签名上堵死"顺手 return err");不推给操作者本人。触发点只有两个(AddFriend→`REQUEST_RECEIVED` 给 target、AcceptFriend→`REQUEST_ACCEPTED` 给原申请人);Reject / Remove / Block / Unblock 都不推。超时 1.5s 是**推断值**(`go/friend/internal/logic/push.go:49` 写了推导过程),不是压测结论 |
| F14 | 三个新 tip 码(`FriendBlocked` / `FriendBlockListFull` / `FriendTargetInboxFull`)是**业务拒绝,不是 fault** | 本域唯一的 fault 码是 `kServiceUnavailable`(MySQL / Redis 真故障)。分界线决定日志与告警:fault 会打 Error、计 `rpc_inband_fault`、配告警;把"对方收件箱满了"标成 fault 等于让最常见的业务拒绝按秒刷告警。`Tip.xlsx` 的 fault 列只要有人顺手填个 1 就会翻转,所以 `go/friend/internal/constants/constants_test.go:198` 的 `TestTipCodesVerdicts` 把每个码的定性钉死。三个码的常量引用在 `constants.go:58` / `:64` / `:75` |
| F15 | 业务结果**一律 in-band**(`return &Resp{ErrorMessage: …}, nil`),包括存储故障 | gate / 路由服的回包桥接只认"成功响应":返回 gRPC status 时客户端只看到一个信封级失败,`TipInfoMessage` 连同中文文案一起丢掉,玩家看到的是"网络错误"。存储故障用 `constants.ErrStorage` 表达并打 Error 日志,由 `serverbase` 定性成 `rpc_inband_fault`。配套:每次请求套 `config.RequestBudget()`(= `Timeout` − `InBandReplyReserve`),否则 go-zero 的超时拦截器会抢先回 `DeadlineExceeded`、把 in-band 结果整个丢掉 |
| F16 | 缓存键换成**带 hash tag 的 v3**:`friend:{f:<pid>}:list:v3` / `…:req:v3` 及各自的 `:generation` | Redis Cluster 按 `{}` 内容算 slot。没有 hash tag 时"数据键"与"generation 键"会落在不同 slot,两条 Lua 在 Cluster 上直接 CROSSSLOT 报错 —— 而在单机 Redis 上一切正常,**这种缺陷只在换成 Cluster 的那天暴露**。版本号 v2→v3 是因为键名变了,未上线无数据,不做迁移(旧键靠自身 TTL 过期)。`friend_repo.go:210` |
| F17 | 只承诺**路由服模式**可达(`GATE_CLIENT_RPC_ROUTER=1`);**不补 gate 直连白名单**、`node_util.cpp` 一字不改 | D-12。与 chat / guild / trade 同口径。直连模式下不可达是设计内的;翻转落在部署层(本地 `start_game.ps1 -GateRouterMode` 默认 `'1'`,K8s `k8s_deploy.ps1 -GateRouterMode` 默认 `"0"`)。K8s 侧的后果见 §5 第 1 条 |

**另有三条随契约照做、不单独论证的**:注册用 `shared/noderegistry` 且 NodeInfo 的 `Endpoint` 与 `GrpcEndpoint` **双填**(只填前者路由服拨不通,guild 踩过;契约 §2);yaml 的 `Etcd` 段**显式写 `Key: ""`** 而不是整行省略(D-13,省略会让 `conf.MustLoad` Fatal、K8s Pod CrashLoop);`Timeout` 必须 ≤ 路由服 `ForwardTimeoutMs`(5000)− 1000 = 4000(契约 §3)。

---

## 3. 请求流与写路径形状

```
客户端 FriendClient ── ClientRequest{message_id=proto-gen 重新分配} ──▶ gate
gate:   会话校验 → ≤1KB → MessageLimiter → IsClientMessageId → ForwardRequest
路由服: route.Table[message_id] → FriendNodeService(全局池,随机实例)→ 原始字节 Invoke,
        透传 x-session-detail-bin
friend: grpcstats → killswitch → session(解会话 + 方法白名单) → serverbase → logic
logic:  session.ClientPlayerID(ctx) 取 me → 套 RequestBudget → 纯校验 + 频率配额
        → data 层权威事务 → 提交后失效缓存 → 事务外推送
S2C:    friend → kafkautil.PushToPlayer → gate-cmd_g<N> → 对方所在 zone 的 gate → 客户端
```

所有写路径(`AddFriend` / `AcceptFriend` / `RemoveFriend` / `Block`)逐字遵守同一条锁序,**顺序本身就是正确性**:

```
事务外:按 player_id 升序 INSERT IGNORE 补齐双方 friend_capacity 行(自动提交)
BeginTx(READ COMMITTED)
  ① 容量守卫:SELECT … FROM friend_capacity WHERE player_id IN (…) ORDER BY player_id FOR UPDATE
  ② 一切"能不能做"的判定读(拉黑 / 好友边 / 申请行)都在守卫之后,且用当前读
  ③ 写入
Commit
提交后:失效缓存 → S2C 推送(都在事务外;提交之后的任何失败都不得改变方法返回值)
```

容量守卫行同时是**"这一对玩家"的串行化载体**:凡是会改动这两人之间关系的写事务都锁同一对容量行,于是两两互斥。RC 下的探针自己挡不住并发插入(没有间隙锁),**挡住并发的是这把守卫**。新增任何写路径时先拿守卫,否则本域所有"权威判定"会静默退化成 check-then-act。

---

## 4. 改动集

### 批次 F1 —— 协议 / 表 / 骨架(20 文件)

| 位置 | 改动 |
|---|---|
| `proto/friend/friend.proto` | 服务改名 `ClientPlayerFriend` + `OptionIsClientProtocolService`;删 `NotifyOnline/Offline` 与四个 message;六个请求体删 `player_id` 改 `reserved`;新增 `Block` / `Unblock` / `ListBlocks` / `RecommendFriends` 与 S2C `NotifyFriendEvent(FriendEventS2C)` |
| `proto/friend/friend_table.proto`(新) | 四张表的唯一事实源;整数主键、零 UNIQUE KEY、TiDB 打散选项;`friend_request` 新增 `updated_ms` 与 `(status,updated_ms)` 索引 |
| `go/friend/internal/config/`(+ 测试) | 按契约锚点重写:`Timeout` 10000→4000、`Etcd.Key` 显式空串、`LeaseTTL` 500→60、MySQL 改结构化字段且断言 `DBName == mmorpg_friend`、`Schema.AutoMigrate` 用 `*bool`、`Friend.*` 各阈值与 `Sweep` 段 |
| `go/friend/etc/friend.yaml` | 端口 50400 / metrics `:9180` 顶层单行;顶层 `ZoneId` 写在所有嵌套段之前;`FriendRedis` 本地整段注释掉(回落共享库) |
| `go/friend/internal/session/`(新,+ 测试) | 会话解码 + 10 个 C2S 方法白名单;测试遍历 `ServiceDesc.Methods` 断言"除 `NotifyFriendEvent` 外全在白名单" |
| `go/friend/internal/lifecycle/`(新,+ 测试) | trade 那份的**第 4 个副本**(24s 硬截止 / 5s 排空);定稿后应抽到 `go/shared`,在那之前四份必须同改 |
| `go/friend/internal/metrics/`(新) | `friend_push_total` / `friend_rate_quota_total` / `friend_online_lookup_total` / `friend_sweep_pending_rows`;label 全是有限枚举、**不含 player_id**;`Start` 里预建全部 label 组合的 0 值序列 |
| `go/friend/internal/data/tables.go`(新) | 库名常量 + 表清单(顺序固定 边 → 申请 → 配额 → 拉黑) |
| `go/friend/internal/svc/servicecontext.go` | 双 Redis 句柄(`SharedRedis` 契约 key 唯一句柄 / `FriendRedis` 私有 key);MySQL DSN 带 `sql_mode=STRICT_TRANS_TABLES`;删掉 `LoadTables`(friend 不读运行时配表) |
| `go/friend/friend.go`(+ 测试) | 照 `go/trade/trade.go` 逐段移植:`-migrate` / `-allow-modify` 与 D-14 退出码、`ensureSchema` 的 Up/Plan 两态、先起服再注册、`advertisedHost` 按 `POD_IP` 回落、拦截器四层顺序 |
| 删除 `go/friend/internal/node/` | 改用 `shared/noderegistry`(D-11 / 契约 §2) |
| `docs/design/xuanming-port-decisions-20260910.md` | **追加** D-10 修订(2026-09-18):就绪闸退役 |

### 批次 F2 —— 存储与业务逻辑(19 文件)

| 位置 | 改动 |
|---|---|
| `go/friend/internal/data/friend_repo.go` | 全局锁序 + RC;`AddFriend` 改成八步权威事务;`AcceptFriend` 申请行锁下移 + 补拉黑复核 + 反向 pending 收敛;`RemoveFriend` 先 fail-closed 判是不是好友再建容量行;列表读加 `LIMIT`;缓存键换 v3;删 `friend:online` 全套 |
| `go/friend/internal/data/block_repo.go`(新) | `Block` / `Unblock` / `ListBlocks`;删边按 `RowsAffected` 减计数;两个方向的 pending 一并置 rejected |
| `go/friend/internal/data/session_reader.go`(新) | `player:session:{id}` 批量 MGET → `PlayerSession`;只认 `SESSION_STATE_ONLINE`;按 `ListReadHardLimit` 分批;失败降级全部离线 |
| `go/friend/internal/data/recommend_repo.go` + `internal/logic/recommend.go`(新) | 共同好友 → 随机兜底;`exclude` 超 `RecommendMaxExclude` 回 `ErrInvalidParameter`(gate 单包 1KB) |
| `go/friend/internal/data/sweep_repo.go` + `internal/logic/sweep.go`(新) | 终态申请清理;默认 `report_only` 只 COUNT + WARN + 更新 gauge;多副本各跑各的、不引入 leader |
| `go/friend/internal/logic/push.go`(新) | S2C 推送三步(读会话 → 组命令 → 写 Kafka);不返回 error |
| `go/friend/internal/logic/rate_quota.go`(新) | 每分钟申请配额;单 key Lua、Cluster 安全;Redis 故障 **fail-open** |
| `go/friend/internal/logic/friend_logic.go` | 存储故障全面 in-band;整请求预算;请求体 id 校验;接四个新 RPC;`FriendStore` / `SessionStore` 接口 + 三行编译期接缝断言 |
| `go/friend/internal/constants/`(+ 测试) | 三个新 friend 码引用;`TestTipCodesVerdicts` 钉住定性 |
| 五个 `*_mysql_test.go` / `*_test.go` | 真 MySQL 门控的并发回归(含移植自 A 仓的 1213 用例)+ logic 层假 store 单测 |

### 批次 F3 —— 部署链 / 运维 / 冒烟 / 文档(22 文件)

| 位置 | 改动 |
|---|---|
| `tools/scripts/go_services.ps1` / `go_svc_image.ps1` / `k8s_deploy.ps1` / `start_game.ps1` / `tests/k8s_migrate_gate.tests.ps1` | **五处登记**(契约 §7):端口 50400、metrics `:9180` 五处逐字一致;friend 与 chat/guild/trade 同为**可选** exe(缺二进制只告警跳过,不拖垮登录);`start_game.ps1` 加 `mmorpg_friend` 预检(库缺或无权 → 跳过 friend 并打印补建命令,不拒启整个游戏) |
| `tools/scripts/dev_tools.ps1` | 删 merge 审计里的 friend Redis DB 3 参数与注释(`friend:online` 已退役) |
| `deploy/k8s/manifests/go-svc/friend.yaml`(新) | Service + Deployment + 同文件 PDB;`replicas: 2` + `podAntiAffinity`;gRPC 探针;`preStop` 5s 与进程 24s 硬截止配套;`POD_IP` 走 Downward API;不写 namespace、不挂 snowflake 卷(friend 不发号) |
| `deploy/k8s/manifests/go-svc/friend-migrate.yaml`(新) | 同镜像 `-f <yaml> -migrate`;`wait-mysql` initContainer;`backoffLimit` / `podFailurePolicy` 对齐 D-14 退出码语义(0 成功 / 1 失败 / 3 锁忙可重试 / 4 需人工) |
| `deploy/mysql-init/00_init_zone_dbs.sql` | 加 `CREATE DATABASE mmorpg_friend` + `GRANT`(建库的唯一登记处) |
| `deploy/mysql-init/guild_friend_tables.sql` | **删** friend 三张表的 `CREATE TABLE` 与 `friend_capacity` 回填段 / 门禁行写入;guild 部分一字不动 |
| `deploy/k8s/README.md` | 存量卷 / PVC 手工建库步骤、TiDB BR 按库恢复清单、manifests 目录说明各加 friend |
| `tools/merge_zone/audit_checks.go` / `audit_resources.go` / `integration_test.go` | 在线审计去掉 `friend:online`;friend / friend_request 审计改连 `mmorpg_friend`;删 `friendRDB` 句柄与 `-friend-redis-*` 参数。**只改连接目标,不加迁移逻辑** —— friend 的行里没有 `zone_id` / `home_zone` 列,合服后自动存活 |
| `tools/data_consistency_check/main.go` | `checkFriendOrphans` 改连 `mmorpg_friend`(独占库后需要第二个连接) |
| `robot/friend_smoke_scenario.go` + `etc/friend_smoke.yaml`(新)、`config.go`、`main.go` | 三账号两区 `friend-smoke`,八步(含跨区推送、权威拉取、黑名单双向拦、推荐弱断言、**D-9 反向断言**、幂等清理) |
| `go/friend/friend.go` | 接上 `logic.StartSweep`(批次 F2 写出但无调用方,在此之前清理是死代码);启动横幅加一行 sweep 模式与间隔 |
| `docs/design/friend-port-20260918.md`(本文,新) / `friend-persistence-architecture.md` | 设计落纸;B 侧现状文档更新到新形态 |

**不在三批之内**(由主会话在合并回 main 之后串行做):`data/tip/Tip.xlsx` 的三个新码、`data/MessageLimiter.xlsx` 按新消息号配档位、契约 §7 端口表补 mail / rank 的号。

---

## 5. 不做项

每条都写清"为什么不做",避免下一个人把它当漏项补上:

1. **好友榜 / leaderboard 依赖**:B 仓根本没有 `go/leaderboard`(可行性文档 §3.2 列为"缺 / 新建 / 18 人日")。为一个不存在的服务提前留接口或留字段是 YAGNI(AGENTS §11.2),而且会占出一个永远填不满的 proto 字段号。
2. **昵称 / 头像投影**:B 全仓还没有昵称权威。`FriendEntry` / `BlockEntry` / `RecommendEntry` 一律只回 id,客户端按 id 另查。等帮会二期的最小昵称落地后在这些 message 上 **append** 新字段即可 —— 现在占号反而要承诺一个填不满的字段(理由写在 `proto/friend/friend.proto:57-58`)。
3. **`cellroute` 分片路由**:可行性文档 D6a 已拍"不搬"。它与 B 的 home_zone 目录路由同职责且互斥,且在 B 的 bwmarrin 雪花布局下 `%4096` 只落 8 个 Cell,没有分片价值。
4. **presence 订阅式推送(好友上下线实时通知)**:见 `friend-persistence-architecture.md` 的 Pull / Push 对比 —— 200 好友 × 10K 登录/秒 = 2M 推送/秒。本期在线状态走拉模型(F3);真要做"你的好友 XXX 上线了",要先有 presence 订阅层与投递缓冲,那是独立立项。
5. **`dbcapacity` 库容量预算巡检**:可行性文档的执行清单自陈「不接 Guard.Check 容量巡检(**预算未推导**)」。没有预算数字的巡检只会产出无法处置的告警。
6. **旧共享库 `mmorpg` 里三张存量 friend 表的数据搬迁**:未上线、没有老客户端也没有老号(`proto/friend/friend.proto:34-35` 按 AGENTS §4.3 记的同一条前提),新库由 `schemamigrate` 从基线建全、没有历史边。**若将来真要导存量,必须先重新设计一次带 durable 标记的回填门禁** —— 不能直接跑导入(D-10 修订的"代价与残留"已把这条写死)。
7. **`SharedRedis` 禁 Cluster 的硬校验**:这是**全仓契约级**缺口(chat 同样没有),不该只在 friend 里补一个局部特例。已登记为 §6 第 3 条。

---

## 6. 已知缺口

> 这一节按"会让人误以为已经好了"的顺序排。

1. **K8s 上玩家到不了 friend**。`tools/scripts/k8s_deploy.ps1:140` 的 `-GateRouterMode` 默认仍是 `"0"`,gate 不走路由服;而 friend 按 D-12 **只承诺路由服模式**可达。所以 K8s 上 friend"部署得起来但玩家不可达",与 **chat / trade 同一个缺口**,不是配置错误。翻成 `1` 的三个前提(K8s 上以路由模式跑通一次 battle-smoke、路由服 manifest 已落地、路由服用 POD_IP 通告)见 D-12。
2. **`dev_tools.ps1` 的 `k8s-*` 包装不透传 `-GateRouterMode`**。`Invoke-K8sDeploy` 的参数 splat 里没有这一项(整个文件零命中 `GateRouterMode`),所以就算将来要在 K8s 上翻开关,也只能直接调 `k8s_deploy.ps1`,走 `dev_tools.ps1` 会静默用默认值。
3. **`SharedRedis` 禁 Cluster 只有注释、没有硬校验**。`go/friend/internal/svc/servicecontext.go:68` 与 `go/friend/internal/config/config.go:88` 都写了"禁 Cluster",但 `Validate` 里没有任何一条拒绝 `Redis.Type == "cluster"`。共享库上有 C++ scene 与多个 Go 服务的跨 slot Lua / MULTI,误配成 Cluster 会在那些地方炸而不是在 friend 这里。**这是全仓契约级缺口(chat 同样没有)**,该在契约层补一次、不是给 friend 打补丁。
4. **生成的 friend ConfigMap 里 `Redis:` 段刻意没有 `Pass`**(与 chat / match 的同形段一致,**是存量系统性缺口、不是 friend 引入的**)。`deploy/k8s/manifests/infra/redis.yaml` 当前没有 requirepass(args 只有 `redis-server --appendonly yes`),向一个没设密码的 Redis 发 `AUTH` 会被直接拒(`ERR Client sent AUTH, but no password is set`),所以**只给 friend 补 `Pass` 会让 friend 成为唯一起不来的服务**。`tools/scripts/k8s_deploy.ps1` 生成该段的地方已补注释说明这是省略不是遗漏。**哪天给共享 Redis 开了 requirepass,必须同批给 chat / match / friend 三处都补 `Pass`,只补一处会让另外两个起不来。** ⚠ 连带暴露一条与 friend 无关的 **staging/prod 阻塞项**:`k8s_deploy.ps1:292-293` 让 `MMORPG_REDIS_PASSWORD` 在 staging/prod fail-closed 必填(`MinLength 12`),而共享 Redis 没开 requirepass —— 今天只要按 staging 档跑一次 `k8s_deploy`,login / db / player-locator 生成出来的 `Password:` 就会被无密码 Redis 拒掉。要么给 `infra/redis.yaml` 加 requirepass 并同批补三处 `Pass`,要么放开那道 `MinLength` 门禁;**这说明整套 staging/prod 配置从未被真正跑过**,该单开一条,不在本次范围。
5. **`updated_ms` 是 `Sweep.Mode: delete` 的硬前置**。写入方在批次 F2 已补齐(`AddFriend` 的 upsert、`AcceptFriend` / `RejectFriend` 的状态迁移都写它),但:① 从旧库搬过来的行 `updated_ms` 会是 0,**一进 delete 模式就会被整批判成过期**(所以搬迁必须显式赋值,建议取 `request_time_ms`);② `proto/friend/friend_table.proto:70-74` 的注释仍停留在批次 F1 的事实("本列当前没有任何写入方")—— 批次 F3 禁止改 `proto/friend/**`,这段注释**待随下一次动该文件时修正**。SQL 侧已有一道保险:存在 `status<>pending` 且 `updated_ms=0` 的行时只打错误日志不删。
6. **本次全部代码未编译、未测试**,`proto-gen` 与导表器**一次都没跑过**。三个新 tip 码对应的枚举常量目前**不存在**(`Tip.xlsx` 的三行由主会话在合并到 main 之后加),在导表器跑完之前 `go/friend` 编译不过 —— 这是本仓既定流程(guild 的 4 个新码同样如此),**不是缺陷**,但也绝不能被当成"已完成"。
7. **`data/MessageLimiter.xlsx` 还没按新消息号配档位**(消息号要 proto-gen 跑完才确定)。在那之前 gate 对 friend 的消息号用的是默认档(3 次/秒/消息号),所以 robot 冒烟必须靠"同一机器人相邻两次请求间隔 ≥1.1s"兜住,否则会被限流成假红。
8. **消息号会整体重排**。改名 + 删两个 rpc 会释放旧号 2 / 7 / 11 / 12 / 53 / 76 / 119 / 120,而生成器把空号当**可用号池**重新分配,落位取决于 Go map 的迭代顺序。硬约束:gate 的 `rpc_event_registry`、路由表、Unity、robot 必须出自**同一次** proto-gen 并同批发布。混用两次生成物不报错,只会让客户端发 `AddFriend`、服务端按别的方法解包。**不要手改 `proto/message_id.txt`**。
9. **`proto/db/proto_option.proto` 的 NodeType 枚举里没有 `NODE_FRIEND`**(最大到 `NODE_CLIENT_RPC_ROUTER=31`),所以 `friend.proto` 刻意没写 `OptionFileDefaultNode`(guild 同样没写)。节点类型由 protogen 按目录名回落成 `FriendNodeService`(= 27),路由与 etcd 注册不受影响;代价只有"按该 option 过滤的生成分支会把 friend 当 `NODE_UNSPECIFIED`"。补枚举会牵动 C++ 侧节点表,**未做**。
10. **旧共享库 `mmorpg` 里的 friend 三张表成为孤儿**。批次 F3 删的是 `deploy/mysql-init/guild_friend_tables.sql` 里的建表脚本,**已初始化的存量卷 / PVC 里那三张表仍然在**(initdb 只在空卷执行)。孤儿表不影响正确性,只占空间并会误导排障的人 —— 清理需要人工 DDL,不在本次范围。
11. **`metrics` 没有 Stop 入口**(与 trade 同形):`Start` 起的 HTTP server 随进程退出,停机流程不显式关它。
12. **`friend_capacity` 没有回收路径**:`AddFriend` 会给任意 target 建行,只受每分钟频率配额约束。表会随"被加过好友的人数"单调涨。
13. **三个导出方法已零调用方**:`FriendRepo.AreFriends` / `HasPendingRequest` / `CountOutgoingPending`。其中 `AreFriends` 走的是最多陈旧 30 分钟的缓存却长得像个权威判定,**建议删或改名**,否则下一个人会拿它当门禁用。

---

## 7. Codex 验证清单

**本节是增量,不是替代**:批次 F1 的完整清单已经写在 `PROGRESS.md` 的 `## 2026-09-18 friend 移植 F1 批` 条目里(proto-gen 的四条通过标准、`go/friend` 的 gofmt/tidy/build/vet/test、空库 `-migrate` 的四表断言、常驻启动横幅与 etcd NodeInfo 核对、缓存链路手工验证)。**那份的每一步通过标准仍然有效,不在这里重抄。** 本节相对它的四处变化:

1. **把导表提到最前**(①):写 F1 那份时三个新 tip 码还不存在,它们是批次 F2 引入的。
2. **新增 ④ / ④b**(批次 F2 的锁序):真 MySQL 8.4 并发回归与三条 `EXPLAIN`。
3. **新增 ⑥**(批次 F3):两区 robot `friend-smoke`。
4. **⑦ 的键名变了**:F1 那份写的是 `friends:v2:<pid>`,批次 F2 已换成带 hash tag 的 v3(决策 F16),照本节的写法执行。

**执行顺序是硬约束**,前一步没过不要跑下一步。

| # | 步骤 | 目录 / 命令 | 通过标准 | 失败时保留 |
|---|---|---|---|---|
| ① | **导表** | 先在 `data/tip/Tip.xlsx` 的 `//friend_error` 组追加 `FriendBlocked` / `FriendBlockListFull` / `FriendTargetInboxFull` 三行(**fault 列必须为 0**),PATH 含 `protoc` 35.1 与 `protoc-gen-go`,跑 `python tools/data_table_exporter/run.py tools/data_table_exporter/exporter_config.yaml` | `generated/code/proto/tip/friend_error_tip.proto` 里 friend 段从现在的 7 码(15000–15006)变成 **10 码**,新增三个落在 15007–15009;`go/shared/generated` 同步;`go/shared/generated/tip/faults.go` 里这三个码**不在** fault 集合里 | 导表日志 |
| ② | **proto-gen** | 从源码重建 `proto-gen`(**别复用旧二进制**),再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary …` | 见 PROGRESS F1 条目第 1 步的四条断言(`ClientPlayerFriend_ServiceDesc.Methods` 恰好 11 条、路由表 11 行全 `ClientProtocol: true`、`grep -c 'NotifyOnline\|NotifyOffline\|friendpb.FriendService' route_table.go` 为 0、Unity 与 robot 侧生成出 `NotifyFriendEvent` 下行 handler) | 生成日志 + `git diff --stat` 生成物 |
| ③ | **Go 静态与单测** | `cd go/friend`:`gofmt -l .` → `go mod tidy` → `go build ./...` → `go vet ./...` → `go test ./...` | `gofmt -l` 为空(Claude 从未跑过格式化);tidy 后 `github.com/redis/go-redis/v9` **只应出现在 indirect 块**(落进直接依赖块 = 还有一处 import 没从 go-redis 改到 go-zero,是真实缺陷,别直接接受 tidy 结果) | 各步输出 |
| ④ | **真 MySQL 8.4 并发回归(唯一能证明锁序正确的证据)** | `cd go/friend`,设 `FRIEND_TEST_MYSQL_DSN`(唯一门控)与 `FRIEND_REQUIRE_MYSQL_TESTS`(**不是**第二个门控:设了它却没给 DSN 时,`friend_repo_mysql_test.go:118` 的 `TestFriendIntegrationGateIsHonored` 会 FAIL,把"跑了"与"跳过了"在报告里分开),`go test -count=1 -v ./internal/data` | **必须在报告里明写"看到了 PASS 而不是 SKIP"**(全体 SKIP 时 `go test` 退出码仍是 0 —— A 仓那次 1213 正是这样被盖了一个多月)。必须 PASS 的五个用例:`TestAddFriendGuardBeforeLockingReads_SharedTarget`、`…_DistinctTargets`、`TestBlockGuardBeforeLockingReads_SharedBlocker`、`TestBlockAndAcceptFriendInterleaved`、`TestAddFriendAndAcceptFriendOnSamePair`(后者含"已有 pending"与"无 pending"两个子测试,直接验 F6 那个 ABBA 环)。每个场景断言:无 1213、`friend_count` 与实际边数一致、不出现"既是好友又拉黑"或"拉黑后仍有 pending"。**必须用 MySQL 8.4 而不是 TiDB**(TiDB 没有间隙锁,这条路径在它上面恒绿) | 完整 `-v` 输出 + MySQL `SHOW ENGINE INNODB STATUS` |
| ④b | **`EXPLAIN` 核三条**(锁序书面保证里唯一靠优化器兜底的一环) | 对 `blockedEitherWay`、`friendEdgeExistsForUpdate`、`lockCapacityRows` 的实际 SQL 跑 `EXPLAIN` | 前两条的 `OR` 形式必须走 `index_merge` / `range` 且 key 含 PRIMARY(退化成全索引扫时锁集当场越出守卫域);`lockCapacityRows` 的 `IN + ORDER BY … FOR UPDATE` 必须 `type=range, key=PRIMARY` 且 **Extra 里没有 `Using filesort`**(`ORDER BY` 是结果序不是取锁序,走成全表扫 + filesort 就会按表序取锁,升序纪律与整个防 ABBA 的依据一起失效) | EXPLAIN 输出原文 |
| ⑤ | **空库迁移 + 常驻启动** | 在**全新空的** `mmorpg_friend` 上 `friend -f etc/friend.yaml -migrate`,再常驻启动 | 迁移:退出 0、恰好四张表、主键全整数、**零 UNIQUE KEY**、`friend_request` 有 `updated_ms` 与两条索引;**再跑一次应 0 条语句**(幂等);`-allow-modify` 不带 `-migrate` 必须非 0 退出。启动:有一行恰为 `  FRIEND SERVICE STARTED SUCCESSFULLY`;横幅里 mysql 目标**不含密码**、`friend_redis` 显示 `shared-fallback`、**sweep 那行显示 `mode=report_only`**;`:9180/metrics` 能看到四个 friend 指标的预建 0 值序列;`etcdctl get --prefix FriendNodeService.rpc/` 的值里 `endpoint` 与 `grpcEndpoint` **都**填了;Ctrl+C 后日志顺序是先注销再排空,总时长 < 24s | 启动日志全文 + `metrics` 抓取 |
| ⑥ | **两区 robot `friend-smoke`** | 两区栈就绪(`zone_config` 里有 zone 2 那一行、**两区 gate 都以 `GATE_CLIENT_RPC_ROUTER=1` 启动**、`mmorpg_friend` 已建),`cd robot` 跑 `.\robot.exe -c etc/friend_smoke.yaml` | 退出 0 且输出一行 `FRIEND_SMOKE_OK player_a=… player_b=… player_c=… gate_a=… gate_b=…`。逐步:`cross_zone=true` 时 A / B 的 gate 地址必须不同;跨区推送两次都在 10s 内到达;重复 `AddFriend` 回 `FriendRequestAlreadySent`;黑名单**两个方向**都回 `FriendBlocked`;推荐结果 ≤20 且不含自己 / 已是好友 / 已拉黑;**D-9 反向断言**:客户端发 `NotifyFriendEvent` 的消息号必须拿不到业务回包,信封 tip = `kServiceUnavailable`(friend 回 `PermissionDenied`,路由服把一切 gRPC 错误翻成该码,见 `go/client_rpc_router/internal/logic/forwardlogic.go:183`),**并在同一运行窗口的 friend 日志里看到"拒绝客户端调用内部方法"** —— 信封不保留原始 gRPC code,只靠机器人收到失败不能证明拒绝原因 | robot 输出 + 两区 gate 日志 + friend 日志 + 路由服日志 |
| ⑦ | **缓存 miss→fill→hit 手工验证**(唯一没有自动测试覆盖的改型) | `redis-cli DEL 'friend:{f:<pid>}:list:v3' 'friend:{f:<pid>}:list:v3:generation'` → 调一次 `GetFriendList` → `redis-cli GET 'friend:{f:<pid>}:list:v3'` | 第三步**必须非空**。若恒为空而调用又不报错,就是 go-zero `GetCtx` 把 `redis.Nil` 吞成 `("", nil)`、generation 读成空串比不上 Lua 里的 `"0"` 那条修复丢了(批次 F1 复验抓到的最危险一条)。⚠ 键名带 hash tag,shell 里**必须加引号**,否则 `{}` 会被展开 | `redis-cli MONITOR` 抓到的 EVAL 参数(看 `ARGV[1]` 是不是空串) |

**责任归属提醒**:本 worktree 里 F1 / F2 / F3 三批的全部文件都从未编译过,首次 `go build` 大概率先报骨架与跨包签名问题,不是最后那批部署登记引入的。

---

## 8. 发布顺序

**friend 新二进制 + 迁移 Job → `client_rpc_router` → gate**,一步都不能提前。

- 反过来(先换 gate)会在窗口内让 **gate 认新消息号而路由表还没有**,请求被路由服当 `unknown_message` 拒掉;玩家看到的是"点了没反应"。契约 §3 已把这条写成通用规则(「发布顺序:路由服先,gate 后」),friend 只是又一个实例。
- `friend-migrate` Job 必须在 friend Deployment 之前 Complete(D-14 第 4 条的门禁,`k8s_deploy.ps1` 在 ConfigMap 之后、Deployment 之前 delete + apply 它)。退出码 3(锁忙)由 Job 的 backoff 重试;1 或 4 直接中断发布。
- **建库不在 Job 里**:存量卷 / PVC 不会重跑 initdb,必须先手工执行 `deploy/mysql-init/00_init_zone_dbs.sql` 里 `mmorpg_friend` 的两句(建库 + GRANT),否则 migrate Job 以 1 退出并点名库名。
- 回退到 gate 直连模式,等于 friend / chat / guild / trade 这些只承诺路由模式的服务**同时**不可达。回退前先用 killswitch 关掉对应方法并发公告(D-12)。
- 三个新 tip 码的 `Tip.xlsx` 行与导表器必须跟着 friend 二进制**同批**发布:码不在表里时 `serverbase` 会把它判成 `VerdictUnknown`,客户端拿不到文案。
