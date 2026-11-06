# friend:移植为全局服务 + 客户端经路由服可达

> **2026-10-15 范围更正：读本文末 §9 与 [最新决定 D-15](xuanming-port-decisions-20260910.md#d-15-八域对比选型与容量目标2026-10-15用户最终范围)。** 本文 F1–F3 是历史“四块增量”；本轮八域含 friend / chat / guild / mail / leaderboard / login / auction / trade，目标 DAU 1 亿 / CCU 2000 万，独立客户端同步已授权。最新准则为比较两仓并选择更可扩展实现，正确性/一致性/兼容不降级；旧增量范围和机械照搬均不再作为终态。

**Created:** 2026-09-18
**状态:** 2026-09-18/19 由 Claude 分 F1 / F2 / F3 三批落码。**未编译、未运行任何测试、未提交;proto-gen 与导表器一次都没跑过**(本机 `go` 与 `protoc` 都不存在,`E:\work\tools` 曾被清空)。§7 的验证清单一步都还没执行,本文任何"应该 / 期望"都不是实测结论。
〔2026-09-28 更正:上一段已过期。三批已提交(`f06090b19` 等,见 `friend-handoff-20260920.md` 开头);09-20 22:39 导表、22:41 proto-gen;09-21 `go/friend` 首次编译即通过,真 MySQL(实为 26.7.0)上全部包 314 PASS / 0 FAIL / 0 SKIP,首跑抓到的锁序真死锁已修(`docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md`,随 `9cef7b2ec` 进库);09-25 空库 `-migrate` + 常驻启动 + 两区 `friend-smoke` 通过。§7 的 ①–⑥ 均已执行,⑦ 改由单测 `TestVersionedCache_FillsWhenGenerationKeyNeverWritten` 覆盖(09-21 PASS),逐步证据见 `friend-handoff-20260920.md` §9.4。之后的状态(推荐查询有界化等)以该文 §9.5 为准。〕
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
| F5 | 客户端调不到下行方法 `NotifyFriendEvent`,两道防线:① gate 上行白名单 `IsClientMessageId` 只收客户端能发起的方法(REQUEST / ONE_WAY),`NotifyFriendEvent` 标了 `CLIENT_RPC_PUSH` 不在其中,客户端发上来 gate 计一次非法包(累计到阈值踢线)、不回包,请求到不了路由服与 friend;② friend 会话拦截器只放行**方法白名单**(10 个 C2S),带客户端会话调 `NotifyFriendEvent` 回 `PermissionDenied` | `OptionIsClientProtocolService` 是**服务级**开关,路由表仍按服务登记全部方法(含 `NotifyFriendEvent`)。2026-11-05 起 gate 白名单改按方法方向收口(依据 [client-rpc-callback.md](./client-rpc-callback.md) §7 R4);在此之前生成器只看服务级开关,下行方法也进了 gate 白名单,只靠会话拦截器挡。会话白名单默认拒绝,以后新增 rpc 忘了登记就不会意外对外,作为第二道防线保留。`session.ClientMethods` 在 `session.go:52`;服务端**同时**不实现 `NotifyFriendEvent`(继承 `UnimplementedClientPlayerFriendServer`),但那是第二道防线 ——"今天返回 Unimplemented"不是安全保证,白名单才是。⚠ 这不违反契约 §3 的「东西向方法不得与客户端 service 同 service」:`NotifyFriendEvent` 是 **S2C 下行声明**而不是东西向调用,且生成器要求它必须落在这个 service 才出得了客户端 handler(见 F1);D-9 给的本来就是「拆 service 或按方法名拒绝客户端来源」**二选一**,friend 选的是后者 |
| F6 | **全局锁序统一为「容量守卫最先」**:`AcceptFriend` 的申请行 `FOR UPDATE` 从守卫**之前**下移到守卫**之后** | 批次 F2 把 `AddFriend` 改成权威事务后,它先锁双方 `friend_capacity`、再碰申请行;而批次 F1 交付的 `AcceptFriend` 恰好相反 —— **同一对玩家"一边接受、一边重发申请"就能撞上 InnoDB 1213**。采用 A 仓 2026-08-11 真 MySQL 8.4 压测出的纪律:**任何写事务在拿到容量守卫之前,不得做任何锁定读**。这不会把 B 当初修掉的那个 1213 带回来:B 修的是"过早把 status 从 1 改成 2 会在 `idx_to_player` 前缀上触发 1213",修法是把 **UPDATE** 延后,而 UPDATE 仍在守卫之后;被下移的只是按 `(from_player_id,to_player_id)` 主键的单行 SELECT。落点:锁序全文在 `go/friend/internal/data/friend_repo.go:97-150`,守卫在 `:299`(AddFriend)与 `:462`(AcceptFriend),`lockCapacityRows` 在 `:662` |
| F7 | 写事务隔离级别固定 **READ COMMITTED**;守卫之后的**判定读一律当前读** | RR 的间隙锁会让"同一玩家并发拉黑 16 个不同目标"这类只碰不同行的事务互相挡(未命中的 `FOR UPDATE` 拿的是同一个间隙锁,排他点落回守卫行,插入意向被其余事务的间隙锁挡住 → 成环),A 仓真 MySQL 8.4 实测必炸,且**只在 MySQL 上炸**(TiDB 没有间隙锁)。RC 没有间隙锁,正确性由"守卫串行化 + 主键"提供。代价是 RC 下同一事务内两次普通 SELECT 可能读到不同结果,所以"判定读在守卫之后用当前读"**不是可选优化,是正确性要求**。前提 `binlog_format=ROW` 已核实(`deploy/k8s/manifests/infra/mysql.yaml:60` 显式设置;本地 compose 未设,取 MySQL 8.4 默认)。常量 `go/friend/internal/data/friend_repo.go:151` |
| F8 | 事务外先按 `player_id` 升序 `INSERT IGNORE` 补齐容量行;缺行**按 `friend` 表权威边数建行,绝不猜 0** | 放进事务里会让多个请求各持自己刚插入的新行、又都抢同一个接收者行,形成 insert-intention 死锁(B 侧原注释已记下这个教训)。"绝不猜 0"是 **D-10 修订**明确要保住的实质不变量之一:猜 0 等于给玩家凭空发一整份好友名额。`ensureFriendCapacityRows` 在 `friend_repo.go:866` |
| F9 | **就绪闸 `friend_capacity_backfill_v1` 退役** | 不在本文重复论证 —— 完整裁定与四条理由见 **[D-10 修订(2026-09-18)](./xuanming-port-decisions-20260910.md)**。一句话:它查的 `guild_schema_migration` 按 D-14 第 8 条留在旧库 `mmorpg`,而 friend 断言自己只连 `mmorpg_friend`(`go/friend/internal/config/config.go:296`),这条查询在任何合法配置下都不可能命中,是可证明的死代码。D-10 的另外三条实质不变量(容量锁行是硬上限 / 缺行按权威边数 / versioned cache + Lua CAS)**一条都没动** |
| F10 | 独占逻辑库 **`mmorpg_friend`**,表以 `proto/friend/friend_table.proto` 为唯一事实源,由 `go/schemamigrate` 经服务二进制 `-migrate` 建 | D-14 第 1/2/3/4 条。四张表(`friend` / `friend_request` / `friend_capacity` / `friend_block`)全部整数主键、**零 UNIQUE KEY**、带 TiDB 打散选项;列名与旧 `guild_friend_tables.sql` 逐字对齐(便于万一要搬存量,但**不保证两边 schema 相同** —— `friend_request` 多了 `updated_ms` 且 `status` 类型变了,见该 proto 文件头的对齐说明)。库名常量 `go/friend/internal/data/tables.go:22`,两道断言:`config.Validate` 比对配置库名、`schemamigrate` 连上后再断言 `SELECT DATABASE()` |
| F11 | 建库**只登记一处**:`deploy/mysql-init/00_init_zone_dbs.sql`;`deploy/mysql-init/guild_friend_tables.sql` **整个文件删除** | D-14 第 5 条(建库只登记一处、mysql-init 禁止新增业务表)。删旧脚本不是清理洁癖:回填段会对一张**已经不再建出来的表**执行 UPDATE,空数据卷首次 initdb 会整个脚本失败。**合并回 main 时发现该文件已无内容可留**:帮会二期把公会表迁进了 `mmorpg_guild`(B1)、本次把好友表迁进 `mmorpg_friend`,两边各搬走一半,剩下的只是空壳,所以整体删除而不是只删 friend 段。`guild_schema_migration` 表也随帮会迁库一并消失(它原本就定义在这个文件里) |
| F12 | 四块新能力的形状:黑名单落**独立表** `friend_block`;推荐走**共同好友 → 随机**两级,随机的 pivot 先取 `MIN/MAX(player_id)` 再在区间内随机;配额是 `FriendRedis` 上的一条单 key Lua;清理默认 `report_only` | 拉黑与删好友是两件事(删好友只断当前关系,对方可以立刻加回来),所以必须落自己的表;且拉黑对象**通常不是**好友,塞进好友边就无处存放。pivot 取法照搬 A 仓注释里的理由:直接 `rand` 会落在现有最大 id 之后,`player_id >= pivot` 一行都扫不到、兜底静默返回空(`go/friend/internal/data/recommend_repo.go:112-145`)。**Block 删边必须按 `RowsAffected` 减 `friend_count`** —— 漏减就是计数上漂、玩家永远加不满好友且零报错(`block_repo.go:50`) |
| F13 | 推送 **at-most-once**,只作"去拉"的触发信号 | 链路是 `kafkautil.PushToPlayer` → `gate-cmd_g<N>` → gate → TCP,**没有**重试、回执或离线补推(契约 §5 列了三种丢失窗口)。所以事件体只带 `reason + by_player_id + ts_ms`,客户端收到后**必须**去拉 `GetPendingRequests` / `GetFriendList`。三条纪律:只在 MySQL 提交之后、事务之外推;失败只打日志 + 计指标、**绝不影响 RPC 结果**(`push.go` 里所有函数都不返回 error,从签名上堵死"顺手 return err");不推给操作者本人。触发点只有两个(AddFriend→`REQUEST_RECEIVED` 给 target、AcceptFriend→`REQUEST_ACCEPTED` 给原申请人);Reject / Remove / Block / Unblock 都不推。超时 1.5s 是**推断值**(`go/friend/internal/logic/push.go:49` 写了推导过程),不是压测结论 |
| F14 | 三个新 tip 码(`FriendBlocked` / `FriendBlockListFull` / `FriendTargetInboxFull`)是**业务拒绝,不是 fault** | 本域唯一的 fault 码是 `kServiceUnavailable`(MySQL / Redis 真故障)。分界线决定日志与告警:fault 会打 Error、计 `rpc_inband_fault`、配告警;把"对方收件箱满了"标成 fault 等于让最常见的业务拒绝按秒刷告警。`Tip.xlsx` 的 fault 列只要有人顺手填个 1 就会翻转,所以 `go/friend/internal/constants/constants_test.go:198` 的 `TestTipCodesVerdicts` 把每个码的定性钉死。三个码的常量引用在 `constants.go:58` / `:64` / `:75` |
| F15 | 业务结果**一律 in-band**(`return &Resp{ErrorMessage: …}, nil`),包括存储故障 | gate / 路由服的回包桥接只认"成功响应":返回 gRPC status 时客户端只看到一个信封级失败,`TipInfoMessage` 连同中文文案一起丢掉,玩家看到的是"网络错误"。存储故障用 `constants.ErrStorage` 表达并打 Error 日志,由 `serverbase` 定性成 `rpc_inband_fault`。配套:每次请求套 `config.RequestBudget()`(= `Timeout` − `InBandReplyReserve`),否则 go-zero 的超时拦截器会抢先回 `DeadlineExceeded`、把 in-band 结果整个丢掉 |
| F16 | 缓存键换成**带 hash tag 的 v3**:`friend:{f:<pid>}:list:v3` / `…:req:v3` 及各自的 `:generation` | Redis Cluster 按 `{}` 内容算 slot。没有 hash tag 时"数据键"与"generation 键"会落在不同 slot,两条 Lua 在 Cluster 上直接 CROSSSLOT 报错 —— 而在单机 Redis 上一切正常,**这种缺陷只在换成 Cluster 的那天暴露**。版本号 v2→v3 是因为键名变了,未上线无数据,不做迁移(旧键靠自身 TTL 过期)。`friend_repo.go:210` |
| F17 | 只承诺**路由服模式**可达(`GATE_CLIENT_RPC_ROUTER=1`);**不补 gate 直连白名单**、`node_util.cpp` 一字不改 | D-12。与 chat / guild / trade 同口径。直连模式下不可达是设计内的;翻转落在部署层(本地 `start_game.ps1 -GateRouterMode` 默认 `'1'`,K8s `k8s_deploy.ps1 -GateRouterMode` 默认 `"0"`)。K8s 侧的后果见 §5 第 1 条。**(2026-09-29 更正)** K8s 默认已改为 `"1"`(turn-based §22 D75),见 §5 第 1 条的更正 |

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

〔2026-09-28 更正(以代码为准,锁序本身不变):上图两处 SQL 形状已随 2026-09-21 死锁修复与数据层死锁审计改写 —— 事务外补行由 `INSERT IGNORE` 改为 `INSERT … ON DUPLICATE KEY UPDATE player_id = player_id`(`ensureCapacityRowSQL`;F8 行同此);① 容量守卫由一条 `IN (…) ORDER BY player_id FOR UPDATE` 改为按升序逐行主键点锁(`lockCapacityRowSQL`);② 的锁定判定读一律是完整主键等值点查。RC 除 `BeginTx` 外也在 DSN 层设置(`svc/servicecontext.go` 的 `BuildDSN`)。见 `friend-persistence-architecture.md`「锁序纪律」末条与 `docs/ops/incident-friend-lock-order-deadlock-2026-09-21.md`。〕

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
| `deploy/mysql-init/guild_friend_tables.sql` | **整个文件删除**(合并 main 后:公会表已迁 `mmorpg_guild`、好友表迁 `mmorpg_friend`,两边各搬走一半后该文件已无内容) |
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
   〔2026-09-29 更正:K8s 默认已翻为 `"1"`(`tools/scripts/k8s_deploy.ps1:163-164`,turn-based §22 D75)。前提①被用户豁免为**事后补验**,前提②③已满足。
   friend 在 K8s 默认部署下经 gate → `client-rpc-router` **按设计可达,待补验**(补验前不宣称已验证);前置是 infra 已部署 `client-rpc-router`,
   集群外玩家另需 `-ClientEntryMode external`(podip 下 gate 通告 PodIP)。以 `"0"` 回退运行的环境仍不可达,且回退态不粘滞、每次部署都要显式再传 `0`。〕
2. **`dev_tools.ps1` 的 `k8s-*` 包装不透传 `-GateRouterMode`**。`Invoke-K8sDeploy` 的参数 splat 里没有这一项(整个文件零命中 `GateRouterMode`),所以就算将来要在 K8s 上翻开关,也只能直接调 `k8s_deploy.ps1`,走 `dev_tools.ps1` 会静默用默认值。
   〔2026-09-29 已关闭:`dev_tools.ps1` 与 `k8s_image.ps1` 都加了"留空不覆盖、非空透传"的 `-GateRouterMode`,回滚入口也透传,见 `deploy/k8s/README.md` Optional Flags。〕
3. **`SharedRedis` 禁 Cluster 只有注释、没有硬校验**。`go/friend/internal/svc/servicecontext.go:68` 与 `go/friend/internal/config/config.go:88` 都写了"禁 Cluster",但 `Validate` 里没有任何一条拒绝 `Redis.Type == "cluster"`。共享库上有 C++ scene 与多个 Go 服务的跨 slot Lua / MULTI,误配成 Cluster 会在那些地方炸而不是在 friend 这里。**这是全仓契约级缺口(chat 同样没有)**,该在契约层补一次、不是给 friend 打补丁。
4. **生成的 friend ConfigMap 里 `Redis:` 段刻意没有 `Pass`**(与 chat / match 的同形段一致,**是存量系统性缺口、不是 friend 引入的**)。`deploy/k8s/manifests/infra/redis.yaml` 当前没有 requirepass(args 只有 `redis-server --appendonly yes`),向一个没设密码的 Redis 发 `AUTH` 会被直接拒(`ERR Client sent AUTH, but no password is set`),所以**只给 friend 补 `Pass` 会让 friend 成为唯一起不来的服务**。`tools/scripts/k8s_deploy.ps1` 生成该段的地方已补注释说明这是省略不是遗漏。**哪天给共享 Redis 开了 requirepass,必须同批给 chat / match / friend 三处都补 `Pass`,只补一处会让另外两个起不来。**〔2026-09-28 更正:本条前半已过期 —— 自 `2df78d4a5`(2026-09-19,共享 Redis 改 StatefulSet + PVC,密码走可选 Secret `redis-auth`)起,`infra/redis.yaml` 仅在 `$REDIS_PASSWORD` 非空时追加 `--requirepass`,而 `k8s_deploy.ps1` 给 friend(连同 chat / match / scene-manager 的共享库段)**同批**写了 `Pass: "${redisPassword}"`,正是上面要求的"同批三处都补";dev 档密码为空时两边都不设密码。`k8s_deploy.ps1` 里 friend 段上方那句"本段刻意不写 Pass"的注释因此与代码矛盾,已登记在 `friend-handoff-20260920.md` §9.5 第 10 条,待该文件别人的在途改动提交后再改。下面 staging/prod 阻塞项那一句也随之不再成立(release 档注入 ≥12 位密码 → 建 Secret → 服务端开 requirepass)〕〔2026-09-29:那句注释已由 `3abb0685b` / `faec58293`(09-29)按代码改写为"自 `2df78d4a5` 起……同批写 Pass",脚本里已搜不到"刻意不写 Pass";`friend-handoff-20260920.md` §9.5 第 10 条已关闭〕 ⚠ 连带暴露一条与 friend 无关的 **staging/prod 阻塞项**:`k8s_deploy.ps1:292-293` 让 `MMORPG_REDIS_PASSWORD` 在 staging/prod fail-closed 必填(`MinLength 12`),而共享 Redis 没开 requirepass —— 今天只要按 staging 档跑一次 `k8s_deploy`,login / db / player-locator 生成出来的 `Password:` 就会被无密码 Redis 拒掉。要么给 `infra/redis.yaml` 加 requirepass 并同批补三处 `Pass`,要么放开那道 `MinLength` 门禁;**这说明整套 staging/prod 配置从未被真正跑过**,该单开一条,不在本次范围。
5. **`updated_ms` 是 `Sweep.Mode: delete` 的硬前置**。写入方在批次 F2 已补齐(`AddFriend` 的 upsert、`AcceptFriend` / `RejectFriend` 的状态迁移都写它),但:① 从旧库搬过来的行 `updated_ms` 会是 0,**一进 delete 模式就会被整批判成过期**(所以搬迁必须显式赋值,建议取 `request_time_ms`);② `proto/friend/friend_table.proto:70-74` 的注释仍停留在批次 F1 的事实("本列当前没有任何写入方")—— 批次 F3 禁止改 `proto/friend/**`,这段注释**待随下一次动该文件时修正**(**已于 2026-09-20 收尾批修正**:写入方是四条路径 / 五条语句,剩余前置只有存量搬迁行)。SQL 侧已有一道保险:存在 `status<>pending` 且 `updated_ms=0` 的行时只打错误日志不删。
6. **本次全部代码未编译、未测试**,`proto-gen` 与导表器**一次都没跑过**。三个新 tip 码对应的枚举常量目前**不存在**(`Tip.xlsx` 的三行由主会话在合并到 main 之后加),在导表器跑完之前 `go/friend` 编译不过 —— 这是本仓既定流程(guild 的 4 个新码同样如此),**不是缺陷**,但也绝不能被当成"已完成"。
   **【已关闭,2026-09-21】** 09-20 22:39 导表(三个码落在 15007–15009,均不在 fault 集合)、22:41 proto-gen;09-21 首次编译即通过,真库 314 PASS / 0 FAIL / 0 SKIP(`friend-handoff-20260920.md` §9.4)。
7. **`data/MessageLimiter.xlsx` 还没按新消息号配档位**(消息号要 proto-gen 跑完才确定)。在那之前 gate 对 friend 的消息号用的是默认档(3 次/秒/消息号),所以 robot 冒烟必须靠"同一机器人相邻两次请求间隔 ≥1.1s"兜住,否则会被限流成假红。
   **【已关闭,2026-09-20/21】** proto-gen 之后用 `friend_xlsx_patch.py message-limiter` 补了 friend 的 10 行(连同帮会 B5a 的 5 行,53 → 68 行)并二次导表,`generated/tables/messagelimiter.json` 核对无误(`friend-handoff-20260920.md` §9.4 第 4 步)。
8. **消息号会整体重排**。〔2026-09-28:本次重排已完成 —— 09-20 22:41 那一次 proto-gen 定号,`kMaxRpcMethodCount = 239`(= `message_id.txt` 最大 id 238 + 1);gate / 路由表 / robot 出自同一次生成,C++ 节点 09-21 重编;Unity 侧的 handler 桩出自同一次服务端 proto-gen,协议类与消息号由客户端生成脚本按同一份 `proto/message_id.txt` 于 09-21 补齐并提交为客户端 `fc0a8dc`(`friend-handoff-20260920.md` §9.4 第 3b 步)。下面的"同批生成、同批发布"硬约束对以后每一次 proto-gen 仍然有效〕改名 + 删两个 rpc 会释放旧号 2 / 7 / 11 / 12 / 53 / 76 / 119 / 120,而生成器把空号当**可用号池**重新分配,落位取决于 Go map 的迭代顺序。硬约束:gate 的 `rpc_event_registry`、路由表、Unity、robot 必须出自**同一次** proto-gen 并同批发布。混用两次生成物不报错,只会让客户端发 `AddFriend`、服务端按别的方法解包。**不要手改 `proto/message_id.txt`**。
9. **`proto/db/proto_option.proto` 的 NodeType 枚举里没有 `NODE_FRIEND`**(最大到 `NODE_CLIENT_RPC_ROUTER=31`),所以 `friend.proto` 刻意没写 `OptionFileDefaultNode`(guild 同样没写)。节点类型由 protogen 按目录名回落成 `FriendNodeService`(= 27),路由与 etcd 注册不受影响;代价只有"按该 option 过滤的生成分支会把 friend 当 `NODE_UNSPECIFIED`"。补枚举会牵动 C++ 侧节点表,**未做**。
10. **旧共享库 `mmorpg` 里的 friend 三张表成为孤儿**。批次 F3 删的是 `deploy/mysql-init/guild_friend_tables.sql` 里的建表脚本,**已初始化的存量卷 / PVC 里那三张表仍然在**(initdb 只在空卷执行)。孤儿表不影响正确性,只占空间并会误导排障的人 —— 清理需要人工 DDL,不在本次范围。
11. **`metrics` 没有 Stop 入口**(与 trade 同形):`Start` 起的 HTTP server 随进程退出,停机流程不显式关它。
12. **`friend_capacity` 没有回收路径**:`AddFriend` 会给任意 target 建行,只受每分钟频率配额约束。表会随"被加过好友的人数"单调涨。
    **【已关闭,2026-09-20 收尾批,未编译】** 加 `created_ms` 列 + `(friend_count, created_ms)` 索引,sweep 新增 `SweepIdleCapacityRows`(逐行自动提交删除);同批处理了守卫缺行的 fail-closed 契约(`runGuardedWrite` 有上限重试)以及回收带来的两条新约束。形态见 `friend-persistence-architecture.md`「容量行回收」,过程见 `friend-handoff-20260920.md` §9。副作用:`friend_capacity` 不再与旧共享库逐列一致,搬迁不能再 `INSERT ... SELECT *`(`created_ms` 可以留 0)。
13. **【已关闭,2026-09-20 收尾批】三个方法已全部删除。** 原文:**三个导出方法已零调用方**:`FriendRepo.AreFriends` / `HasPendingRequest` / `CountOutgoingPending`。其中 `AreFriends` 走的是最多陈旧 30 分钟的缓存却长得像个权威判定,**建议删或改名**,否则下一个人会拿它当门禁用。

---

## 7. Codex 验证清单

**本节是增量,不是替代**:批次 F1 的完整清单已经写在 `PROGRESS.md` 的 `## 2026-09-18 friend 移植 F1 批` 条目里(proto-gen 的四条通过标准、`go/friend` 的 gofmt/tidy/build/vet/test、空库 `-migrate` 的四表断言、常驻启动横幅与 etcd NodeInfo 核对、缓存链路手工验证)。**那份的每一步通过标准仍然有效,不在这里重抄。** 本节相对它的四处变化:

1. **把导表提到最前**(①):写 F1 那份时三个新 tip 码还不存在,它们是批次 F2 引入的。
2. **新增 ④ / ④b**(批次 F2 的锁序):真 MySQL 8.4 并发回归与三条 `EXPLAIN`。
3. **新增 ⑥**(批次 F3):两区 robot `friend-smoke`。
4. **⑦ 的键名变了**:F1 那份写的是 `friends:v2:<pid>`,批次 F2 已换成带 hash tag 的 v3(决策 F16),照本节的写法执行。

**执行顺序是硬约束**,前一步没过不要跑下一步。

〔2026-09-28:①–⑥ 已于 2026-09-20 ~ 09-25 全部执行通过,⑦ 改由单测覆盖,证据见 `friend-handoff-20260920.md` §9.4。表中几处判据已随实跑更正(见各格内的更正标注);再在新环境跑时,期望值以该文 §9 为准。2026-11-05 起 ⑥ 的 D-9 反向断言改为期望超时(依据 [client-rpc-callback.md](./client-rpc-callback.md) §7 R4,见该格);09-25 那次是按 R4 之前的口径通过的,重跑以该格现在的判据为准。〕

| # | 步骤 | 目录 / 命令 | 通过标准 | 失败时保留 |
|---|---|---|---|---|
| ① | **导表** | 先在 `data/tip/Tip.xlsx` 的 `//friend_error` 组追加 `FriendBlocked` / `FriendBlockListFull` / `FriendTargetInboxFull` 三行(**fault 列留空**〔2026-09-28 更正:原文写"fault 列必须为 0";按 AGENTS §4,业务拒绝在 `fault` 列**留空**、服务端故障才填 1,三个码已按留空导出,不在 `faults.go` 里〕),PATH 含 `protoc` 35.1 与 `protoc-gen-go`,跑 `python tools/data_table_exporter/run.py tools/data_table_exporter/exporter_config.yaml` | `generated/code/proto/tip/friend_error_tip.proto` 里 friend 段从现在的 7 码(15000–15006)变成 **10 码**,新增三个落在 15007–15009;`go/shared/generated` 同步;`go/shared/generated/tip/faults.go` 里这三个码**不在** fault 集合里 | 导表日志 |
| ② | **proto-gen** | 从源码重建 `proto-gen`(**别复用旧二进制**),再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -UseBinary …` | 见 PROGRESS F1 条目第 1 步的四条断言(`ClientPlayerFriend_ServiceDesc.Methods` 恰好 11 条、路由表 11 行全 `ClientProtocol: true`、`grep -c 'NotifyOnline\|NotifyOffline\|friendpb.FriendService' route_table.go` 为 0、Unity 与 robot 侧生成出 `NotifyFriendEvent` 下行 handler) | 生成日志 + `git diff --stat` 生成物 |
| ③ | **Go 静态与单测** | `cd go/friend`:`gofmt -l .` → `go mod tidy` → `go build ./...` → `go vet ./...` → `go test ./...` | `gofmt -l` 为空(Claude 从未跑过格式化);tidy 后 `github.com/redis/go-redis/v9` **只应出现在 indirect 块**(落进直接依赖块 = 还有一处 import 没从 go-redis 改到 go-zero,是真实缺陷,别直接接受 tidy 结果) | 各步输出 |
| ④ | **真 MySQL 8.4 并发回归(唯一能证明锁序正确的证据)** | `cd go/friend`,设 `FRIEND_TEST_MYSQL_DSN`(唯一门控〔2026-10-10 补:指 MySQL 这组用例;同包后来另有一组连真 TiDB 的用例,门控 `FRIEND_TEST_TIDB_DSN` / 验收开关 `FRIEND_REQUIRE_TIDB_TESTS`,见 `docs/handoff/friend-handoff-20260920.md` §9.5 表后小节〕)与 `FRIEND_REQUIRE_MYSQL_TESTS`(**不是**第二个门控:设了它却没给 DSN 时,`friend_repo_mysql_test.go` 的 `TestFriendIntegrationGateIsHonored` 会 FAIL,把"跑了"与"跳过了"在报告里分开),`go test -count=1 -v ./internal/data` | **必须在报告里明写"看到了 PASS 而不是 SKIP"**(全体 SKIP 时 `go test` 退出码仍是 0 —— A 仓那次 1213 正是这样被盖了一个多月)。必须 PASS 的五个用例:`TestAddFriendGuardBeforeLockingReads_SharedTarget`、`…_DistinctTargets`、`TestBlockGuardBeforeLockingReads_SharedBlocker`、`TestBlockAndAcceptFriendInterleaved`、`TestAddFriendAndAcceptFriendOnSamePair`(后者含"已有 pending"与"无 pending"两个子测试,直接验 F6 那个 ABBA 环)。每个场景断言:无 1213、`friend_count` 与实际边数一致、不出现"既是好友又拉黑"或"拉黑后仍有 pending"。**必须用 MySQL 8.4 而不是 TiDB**(TiDB 没有间隙锁,这条路径在它上面恒绿) | 完整 `-v` 输出 + MySQL `SHOW ENGINE INNODB STATUS` |
| ④b | **`EXPLAIN` 核三条**(锁序书面保证里唯一靠优化器兜底的一环) | 对 `blockedEitherWay`、`friendEdgeExistsForUpdate`、`lockCapacityRows` 的实际 SQL 跑 `EXPLAIN` | 前两条的 `OR` 形式必须走 `index_merge` / `range` 且 key 含 PRIMARY(退化成全索引扫时锁集当场越出守卫域);`lockCapacityRows` 的 `IN + ORDER BY … FOR UPDATE` 必须 `type=range, key=PRIMARY` 且 **Extra 里没有 `Using filesort`**(`ORDER BY` 是结果序不是取锁序,走成全表扫 + filesort 就会按表序取锁,升序纪律与整个防 ABBA 的依据一起失效)〔2026-09-28 更正:本判据**已被取代**。2026-09-21 真库 `EXPLAIN` 证实这三条恰好都退化成了索引全扫(并引发真死锁),修法是不再依赖优化器:守卫之后的锁定读 / 写一律改成**完整主键的等值点锁**(`lockCapacityRowSQL` 按升序逐行、`lockBlockRowSQL` / `lockFriendEdgeRowSQL` 两个方向各一次点查),由确定性回归 `TestLockingStatementsArePrimaryKeyPointLookups` 对生产 SQL 常量做 `EXPLAIN`、断言 `key=PRIMARY` 且用满主键列、SELECT 为 `const`。新判据 = 该用例在真库上 PASS;09-28 在 5 万玩家 / 100 万边的库上复核同样成立(`friend-handoff-20260920.md` §9.4 末段)〕 | EXPLAIN 输出原文 |
| ⑤ | **空库迁移 + 常驻启动** | 在**全新空的** `mmorpg_friend` 上 `friend -f etc/friend.yaml -migrate`,再常驻启动 | 迁移:退出 0、恰好四张表〔2026-09-28 更正:实测是 **5 张表** —— 四张业务表 + `go/schemamigrate` 的台账表 `schema_migrations`;另 `friend_capacity` 自收尾批起多 `created_ms` 与非唯一索引 `(friend_count, created_ms)`〕、主键全整数、**零 UNIQUE KEY**、`friend_request` 有 `updated_ms` 与两条索引;**再跑一次应 0 条语句**(幂等);`-allow-modify` 不带 `-migrate` 必须非 0 退出。启动:有一行恰为 `  FRIEND SERVICE STARTED SUCCESSFULLY`;横幅里 mysql 目标**不含密码**、`friend_redis` 显示 `shared-fallback`、**sweep 那行显示 `mode=report_only`**;`:9180/metrics` 能看到四个 friend 指标的预建 0 值序列〔2026-09-28 更正:现为 **5 个**指标,收尾批新增 `friend_sweep_idle_capacity_rows{mode}`;09-25 实测五个都有预建 0 值序列〕;`etcdctl get --prefix FriendNodeService.rpc/` 的值里 `endpoint` 与 `grpcEndpoint` **都**填了;Ctrl+C 后日志顺序是先注销再排空,总时长 < 24s | 启动日志全文 + `metrics` 抓取 |
| ⑥ | **两区 robot `friend-smoke`** | 两区栈就绪(`zone_config` 里有 zone 2 那一行、**两区 gate 都以 `GATE_CLIENT_RPC_ROUTER=1` 启动**、`mmorpg_friend` 已建),`cd robot` 跑 `.\robot.exe -c etc/friend_smoke.yaml` | 退出 0 且输出一行 `FRIEND_SMOKE_OK player_a=… player_b=… player_c=… gate_a=… gate_b=…`。逐步:`cross_zone=true` 时 A / B 的 gate 地址必须不同;跨区推送两次都在 10s 内到达;重复 `AddFriend` 回 `FriendRequestAlreadySent`;黑名单**两个方向**都回 `FriendBlocked`;推荐结果 ≤20 且不含自己 / 已是好友 / 已拉黑;**D-9 反向断言**(冒烟第 6 步):客户端发 `NotifyFriendEvent` 的消息号必须等满约 10s 超时(`friendSmokeRpcTimeout`)才算通过;收到信封拒绝(哪怕 tip 是 `kServiceUnavailable`)说明 gate 不是按 R4 生成器重新生成后编出的版本,收到业务回包说明推送方法对客户端开放,两者都不能算通过;同一运行窗口的路由服与 friend 日志里**不应出现**这次请求。friend 会话拦截器(`internal/session` 的方法白名单)这道防线经 gate 已走不到,由单测 `TestClientCannotCallNotifyFriendEvent`(`go/friend/internal/session/session_test.go`)与 `TestSessionRejectsNotifyFriendEventFromClient`(`go/friend/friend_test.go`)证明,随 ③ 的 `go test ./...` 跑,两条都要 PASS。判据与 `friend-handoff-20260920.md` §2 第 9 步的 D-9 条一致。〔2026-11-05 依据 [client-rpc-callback.md](./client-rpc-callback.md) §7 R4 改:`NotifyFriendEvent` 标了 `CLIENT_RPC_PUSH`,gate 上行白名单 `IsClientMessageId` 只收客户端能发起的方法(REQUEST / ONE_WAY),这个号发上来 gate 计一次非法包(累计到阈值踢线,本步只发一次)、不回包,请求到不了路由服与 friend。改前的判据是「信封 tip = `kServiceUnavailable`(friend 回 `PermissionDenied`,路由服把一切 gRPC 错误翻成该码,见 `go/client_rpc_router/internal/logic/forwardlogic.go:183`),并在同一运行窗口的 friend 日志里看到"拒绝客户端调用非客户端方法"」;这是 R4 之前的口径,09-25 冒烟按它通过,实际抓到 `[friend] 拒绝客户端调用非客户端方法 /friendpb.ClientPlayerFriend/NotifyFriendEvent`(2026-09-28 更正过日志原文:早先写的"拒绝客户端调用内部方法"按它 grep 会一无所获,逐字以 `go/friend/internal/session/session.go` 为准)。〕 | robot 输出 + 两区 gate 日志 + friend 日志 + 路由服日志 |
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

---

## 9. 八域基线下的 friend 范围更正(2026-10-15)

### 9.1 历史交付与最终范围

§1.1 的“只搬四块能力”是 F1–F3 当时事实，不是现要求的终态。本轮 **friend / chat / guild / mail / leaderboard / login / auction / trade** 八域比较原仓 `D:/luyuan/xuanming-server` 与本仓，目标 **DAU 1 亿 / CCU 2000 万**，授权独立客户端同步、分批端到端完成。用户最新准则为“对比原来，哪个能承载更多玩家用哪个”，以正确性/一致性/兼容不降级为前提，不再机械承诺逐字复用。现行决定见 [D-15](xuanming-port-decisions-20260910.md#d-15-八域对比选型与容量目标2026-10-15用户最终范围)，对比矩阵/批次见 [可行性文档 §13](xuanming-port-feasibility-20260902.md#13-八域能力对比与依赖批次2026-10-15)。

**dialogue / player / inventory / mission / scene_manager 明确排除**；team、match、owner、allocator 等只记必要接缝，不独立移植。auction / trade 已纳入本轮，但 inventory 仍不迁。客户端授权针对本项目 `../mmorpg-client/`，不包括源 UE 客户端。首批申请代次修复只是切片，不能当成全部八域完成。

本次核对源 HEAD 为 **`b1f6c1ebf4583b45a9370ad0522c64196dc792b2`**，2026-10-15 工作区干净。逐项比较好友协议、权威规则、事务/错误、上限、配置/迁移、推送/恢复、查询与热点，再选择实现。现静态候选为保留本仓已修点锁/容量/有界推荐，结合源 request_id/默认 TiDB/恢复能力；无同负载测量，尚不能断言哪边实际承载人数更多。复用源文件可选，需留源版本/内容校验、适配差异与源风险，不要求所有文件逐字相同。

### 9.2 分布式加好友的准确含义

源 friend 使用 `database/sql` 显式事务；分布式存储由 TiDB 提供，**`BeginTx` 不代表单机 MySQL**。源 `run_services.ps1` 默认 friend / chat / guild / mail 使用 `*-dev-tidb.yaml`，`-SocialOnMysql` 才切回 MySQL；`start.ps1` 的 K8s 编排向生成器显式传 `-SocialStore tidb`，而生成器参数本身仍默认 mysql。源 `friend-dev-tidb.yaml` 明确双向建边在 TiDB 悲观事务内运行。

`internal/biz/friend_sharding.go` 的路由、幂等键与跨分片观测不改该建边路径，不是已接通的应用分库 Saga。首要差异是**真实 TiDB 接入/配置/迁移/部署与完整业务契约**，不能看到 `FOR UPDATE` 就强行拆 Saga；跨地域图写入的另行设计须有容量/延迟/故障依据。

推送须分段描述：源好友事务提交后发 Kafka，尚无同事务 outbox，提交→Kafka 仍有缺口；runtime/push 消费后原子分配游标并写缓冲，失败拒绝成功消费并可重放，只解决消费后的交付。好友列表/申请权威补拉继续保留；要求可靠送达的八域事件须在当批完成生产侧持久化边界、去重、重放/保留窗口和故障验证，不能宣称“源仓零丢失”。本次不因此独立移植整个 push 服务。

### 9.3 不能回退的修复与适配接缝

- 保留容量计数、缺行按权威边数重建、按 `RowsAffected` 增减、固定锁序和完整主键点锁。§7 早期 `IN/OR + ORDER BY` 计划保证已由 09-21 真死锁修复取代，旧源 SQL/旧解释不能覆盖确定性点锁回归。
- 保留缓存 generation、Redis Cluster hash tag、时间预算、会话取身份、内部/下行方法拒绝客户端调用、列表上限与推荐 SQL 有界性。10 月 MySQL/TiDB 方言及语句预算随两种真库回归，不因 TiDB 接入删除 MySQL 锁序测试。
- 客户端继续用本仓 gate 帧、tip 配表、消息号与生成链；跨区路由、目标实例和安全门按本仓接缝实现。
- 数据迁移/权限沿本仓每服务数据所有权处理，不直接把本仓连到源 `pandora_social` 或绕过兼容/迁移。功能与数据库事务保证必须对齐，内部代码和部署命名可适配。C++ 资产仍为唯一权威，排除域不迁移。

### 9.4 后续闭环与证据边界

friend 进入八域批次 A：核清源完整契约、修申请代次等回归切片、接真实 TiDB，补配置/迁移/启动/部署与必要差异，再由授权客户端/robot 跑跨区流程；后续随社交批次补通知/补拉与契约差异。每批包含并发申请/接受/拉黑/删除、容量/边一致性、重复、提交结果未知、进程崩溃、推送中断和恢复，实际运行受影响构建/真库测试并区分 PASS / SKIP / 未跑。旧 friend-smoke 通过不替代新验证，也不是 2000 万 CCU 证据。

源默认 TiDB与独立服务只证明实现/装配方向，尚缺行为模型、好友图规模、热点/推荐成本、生产副本/故障拓扑及压测数据。容量按本仓压测纪律另验，不能凭数据库名称承诺达标。Java 八域本次状态为**待逐能力对账与对齐**，不能推定已完成。

**本节仅最终范围与静态事实；本次未改实现、未编译、未测试、未压测。**

### 9.5 申请代次与单事务权威校验移植（2026-10-15，用户指定 AcceptRequest 设计）

用户随后贴出源 `MySQLFriendRepo.AcceptRequest`，明确要求保留该设计并直接移植，同时要求全球同服、强一致性。本批以其业务不变量为准：按申请 ID 定位，仅接收者本人可操作；先取得守卫，再锁定读复核申请代次、目标、状态、双向拉黑与双方硬上限；正向状态、反向 pending 收敛、双向好友边和容量计数同一 RC 事务提交。失败回滚；重复终态操作不重复建边、计数、摘要或推送。保留本仓容量守卫和完整玩家对主键点锁，不把源 OR 锁定查询或依赖 COUNT/gap lock 的上限写法带回。

协议只追加：`FriendRequest.request_id=5`、`AddFriendResponse.request_id=2`、`AcceptFriendRequest.request_id=3`、`RejectFriendRequest.request_id=3`；存储 `FriendRequestRecord.request_id=6`，追加普通 ID 索引，旧字段号/主键保持。Accept/Reject 必须带非零 request_id，操作者来自会话；既有 from_player_id 兼容字段被忽略，不允许退回仅凭玩家对修改或用 hint 授权。申请 ID 使用 data_service 的 `friend_request` 号段，失败明确拒绝，不使用注册 node_id 自造 Snowflake。新建/终态重发换 ID，重复 pending 保留原 ID 和发起时间；入口频控仍有效，幂等不是绕过配额。

旧 pending 的 request_id=0 采用安全按需补齐：列表 SQL 游标关闭后逐玩家对处理，锁内只给仍 pending 且 ID 为 0 的行赋值，保留原时间；重复 Add 遇旧行同样补齐。领号等网络 I/O 在事务外，重新取锁后复核，已经非零的 ID 永不被补号覆盖。补号完成后整页单次 SQL 重读，不能拼接补号前后快照；若最终仍存在零代次则明确报错。ID 定位遇重复记录失败关闭；普通索引必须真实存在，不能把迁移器“缺索引仅告警”当作已就绪。不删除历史申请、不猜测代次。发布须先禁旧版好友写入并排空旧写者，再开放新版与带 ID 的客户端；旧写者忽略 ID 时不能声称代次保护成立。

**本批强一致范围是好友权威关系和申请状态。** 两条列表读直接查询权威 SQL，不以 Redis 列表缓存、缓存失效成功或推送到达充当提交证明；数据库不可确认时回明确错误，不能成功返回旧缓存或空列表。生产要求所有全服好友实例接同一个逻辑 TiDB 事务域并验证后端，不能按 zone 各写独立好友库再异步拼合。开发 MySQL 回归只证明事务行为，不等于分布式部署、跨洲容灾或 2000 万 CCU 验收。TiDB 的 RC/SI 也不会自动把任意业务串行化，不变量由守卫、当前读和条件更新共同保证。

在线状态、通知触发等派生展示不在该 SQL 事务内；仍须区分可过期展示与资产/关系权威。整个游戏的全球单一玩家写者、资产持久化和跨服务交易边界需另外逐域验收，不能因本批 friend 完成就标记整游强一致。客户端本批补齐实际申请入口、保存 request_id 和迟到回调隔离；Java 版执行同一契约。实现、生成、红绿回归、真实库与构建结果在完成后追加，当前不提前声明通过。

### 9.6 首批落地与真实验证（2026-10-15，申请代次及好友权威事务）

Go 已落地申请代次、按 ID/会话主体处理、同操作终态幂等与 Changed 副作用控制、终态重发换号、旧零代次锁外领号/守卫内补齐、补号后单次 SQL 权威快照及两条列表直接 SQL。生产 RequireTiDB 强制验证实际后端，每个物理连接的 DSN 固定 READ-COMMITTED+pessimistic；常驻与 -migrate 同配置。独立客户端补真实申请入口、非零ID流转、会话/迟到回调隔离及未知结果补拉；本批没有把其余好友UI或八域整体标为已完成。

实际证据：

- Go `FRIEND_REQUIRE_MYSQL_TESTS=1 go test ./... -count=1 -timeout=300s` 全包通过，data 包 274.458s；后续同玩家对/硬限/重复语义定向复验通过。`go build ./...`、`go vet ./...` 通过。号段配置包回归通过，store 的所选名称无匹配测试，不能记作执行了存储集成。
- 真 TiDB 8.5.2 上的 `TestTiDBRequestGeneration_TransactionsAndHardLimit` 与 `TestAuthorityDB_TiDBPessimisticOnEveryConnection` 通过（data 6.379s、svc 6.861s）：原生 NONCLUSTERED 主键，重放/换代/迟到请求、16 方并发硬上限 2、历史补号/错误接收者，以及同时持有三个物理连接逐一检查 pessimistic+RC。环境为本地隔离开发拓扑，未验证生产 HA/跨洲故障。
- 混合快照有确定性红→绿：原实现先读 A10 pending+B0，再在补 B 号时并发接受 A，错误返回 A10+B20；修复为全部补号后整页重新 SELECT，只返回 B，回归通过。Java 相反终态误报成功也经独立审查修正，并与同操作重放语义对齐。
- 正规旧 schema 迁移使用 proto2mysql 生成四表旧夹具（仅移除本批新增列/索引）并保留一条旧 pending。真 TiDB 上新二进制 -migrate 自动 ADD COLUMN，因缺普通索引明确 exit 4；按同一 proto2mysql 生成定义补 idx_friend_request_2(request_id)，再次 -migrate 为 exit 0、零漂移。旧行 request_time_ms/updated_ms=123456789、status=0、request_id=0 均保留；零代次业务补齐由上面的真库用例覆盖。DDL 未手写进生产仓储，未操作现有/生产数据库。
- C++ 两个标准生成的 friend.pb.cc / friend_table.pb.cc 使用 VS18 x64 和 protobuf 35.1 逐个隔离编译 exit 0，无 warning/error；未链接完整节点。客户端 Unity 89 PASS / 0 FAIL / 0 SKIP（新增好友22），431 个运行时源码离线编译零错误；未执行真实 gate 端到端。
- K8s 好友入口/RequireTiDB/Host 与生产拒绝门禁用例通过。完整部署契约第二轮为 123/124：剩余 all-up -SkipInfra 多 zone 登录配置夹具期望2块、实际4块；该路径不生成好友配置，保留日志，不修改并行登录工作。首轮另有 PowerShell 子进程 OOM、现已在后次消失，不把第一次失败隐去。

生成边界：隔离快照按标准 dev_tools proto-gen-build 完成构建，proto-gen-run 的 CompilationAndUtilGeneration 阶段已产出本批绑定；随后全局事件防墓碑门禁因其他在途删除的 29 个事件拒绝，未设置绕过环境变量，也未覆盖全局 ID/事件产物。本批仅同步已完成阶段的好友绑定及原有 playersync 所需 scene SyncDomain Go 依赖，再用上述编译验证；不能把脚本包装器的 exit 0 当完整生成流水线成功。

验证原始材料在 ignored build/friend-* 下，包括 proto-run.log、两次正规迁移日志、由 proto 生成的当前/旧表/索引DDL、C++隔离脚本/日志和部署契约摘要。客户端 Unity 原始 XML/日志位于系统临时目录 friend-client-requestid-20261015。Java 最终证据另追加；源码/客户端均未提交，无生产发布/数据迁移/账号登录。

部署必要条件仍未自动完成：先停旧 friend 写者，审阅补索引并迁移到 exit0，预播种 friend_request 号段，使用同一全服 TiDB 事务域及不可回退的号源水位，配齐客户端协议；生产 TiDB 的备份/恢复和故障预算另验。提交后推送/摘要仍是通知旁路，没有与好友事务同提交的 outbox，漏通知由权威补拉恢复，不宣称可靠事件全链路零丢失或整游强一致。DAU1亿/CCU2000万尚无压测证据。
Java最终回执（0.1.0-SNAPSHOT，对应并行保存d57d11f）：完整隔离11模块构建成功，friend上一快照完整MySQL/Redis97项中96通过/1跳过、robot616项执行通过；最终忽略from兼容字段的源码编译及15项FriendServiceTest通过。Java真实TiDB与最终全量真库因Docker引擎停止未执行，已在PARITY和既有friend-spec登记，不以Go真TiDB结果代替Java。机器人smoke也已改为保留/核对代次与时间、按ID接受及按权威列表清理；主树构建受其他域vendor绑定滞后阻断，完整新生成绑定的只读overlay编译通过，未覆盖其他域vendor且未执行真实冒烟。期间工作区被其他操作保存为ab0f27907，前文“未提交”为当时状态；本AI没有执行commit/push/tag。

### 9.7 Java 真 TiDB 迁移修复与最终回归（2026-10-15，续验）

Docker 恢复后补齐上一节未执行的 Java 真库证据。首轮原样 14 项真 TiDB 回归为 13 通过、1 错误：TiDB 8.5.2 不接受同一 `ALTER TABLE` 同时新增 `request_id` 与引用该列的普通索引。没有过滤失败用例；`game-pbmysql` 规划器现按普通列、主键、二级索引分步执行，索引阶段失败可由下次迁移继续。新增自增列及其必需支撑键仍同句，避免破坏 MySQL 对非主键/联合主键非首列自增的约束。只改该模块的规划、执行与两组测试，不手写好友专用 DDL、不更改生成契约或旧行数据。

最终验证：`game-pbmysql` 在隔离 MySQL `13316` 上 132/132；好友完整 MySQL `13316`＋Redis `13379`＋实际 TiDB 生产门禁 98/98，package 成功；原失败选择器的相同 14 项在 TiDB `4000` 上 14/14，含旧表补列/补索引，全部零失败、零错误、零跳过。旧表迁移日志明确分两句执行成功。构建快照 2287 个输入逐文件 SHA 一致，好友主树 46 文件及迁移器主树 4 文件与被测快照一致；好友 jar 内也核实嵌入修复后的迁移器。摘要为主仓 ignored `build/friend-final-verification-20261015.json`，内含原红、最终日志和快照位置；专用随机测试库剩余为零。

Java 版本仍 `0.1.0-SNAPSHOT`，本轮没有发布或运行真实 gate/客户端跨区冒烟；通知 outbox、生产迁移停写和整游强一致的边界仍按上一节，不因真 TiDB 测试通过而扩大结论。

### 9.8 每个物理连接的强读策略与生产拒关（2026-10-15）

权威列表在事务外执行普通 SELECT，只有 RC 与悲观事务仍不够。TiDB 的低精度 TSO 可经全局默认被新连接继承，历史 snapshot、陈旧读与 weak 也可通过 JDBC 会话设置启用；不能让服务通过后端类型门禁后悄悄读旧关系。Go DSN 与 Java 生产 Druid 初始化现逐个物理连接固定：`READ-COMMITTED`、`pessimistic`、空 `tidb_snapshot`、`tidb_read_staleness=0`、`tidb_low_resolution_tso=OFF`、`tidb_read_consistency='strict'`、`tidb_replica_read='leader'`。启动复核不符即拒绝，不回旧缓存；本批只设 SESSION，没有修改 GLOBAL。陈旧读和低精度 TSO 的作用域不同，不把两者都描述为全局继承。正常 follower ReadIndex 也能提供一致读，固定 leader 是本项目显式策略，不是将 follower 一律判为旧数据。

Java 在配置属性绑定之后、Druid 初始化之前追加强制语句，覆盖 URL 或自定义 init SQL 中的弱策略；物理连接淘汰后重新创建仍执行。生产拒绝已提前初始化或无法保证策略的其他连接池。本地 MySQL 不注入 TiDB 专用变量。独立审查发现 `application-prod.yaml` 的默认 true 仍可被命令行关闭，新增真实 Spring 配置回归先复现旧代码错误放行，再在实际静态策略工厂用 `Environment` 检查 active `prod` 并拒绝 `require-tidb=false`，包括 `nacos,prod` 组合；Go 已有等价生产拒关。开发模式保留原 MySQL 兼容路径，没有新增并行运行模式。

本轮最终运行证据：

- Go svc 5/5、零跳过，真实 MySQL 兼容/生产拒绝及 TiDB 8.5.2 同时持有三个不同 CONNECTION_ID 的七项策略均执行；`go build ./...`、`go vet ./internal/svc`、gofmt/diff 检查通过。摘要为 `build/friend-authority-session-20261015/strict-snapshot/final-verification.json`，两处源码与被测输入一致。全好友业务的先前完整真库证据仍见 §9.6，不将这五项称为重跑了全域。
- Java 最终 104/104、零失败、零错误、零跳过，9 reactor package 成功。真实 MySQL `13316`、Redis `13379`、TiDB `4000` 均启用。TiDB 池测试采用实际 Druid 自动装配/属性绑定，在 prod 下分别注入弱读 URL 和历史 snapshot URL，同时借用三连接、强制淘汰、再借三连接，核对每次七项和新连接 ID；逐项污染强读变量的门禁负例也实际执行。测试装配最初缺少 Spring 占位符解析导致两项失败，补入正式自动配置后通过，原失败日志保留；未把该夹具失败解释为业务故障。
- Java 原样 TiDB 核心选择器的旧 14 个用例全部保留，加一项数据库门禁后 15/15、零跳过；独立 9 reactor package 成功。此轮未选择新池测试，新池及后补 prod 拒关由上一条最终 104 项覆盖；不把旧选择器日志拼成同一次执行。

最终 Java 摘要为 `build/friend-strong-read-java-20261015/final-verification.json`，536 个构建输入运行期间零漂移，当前主树好友 45 个输入及迁移器 4 文件与快照一致；jar SHA-256 为 `70239BC2AC63E49A06CAA526E466EA69E9CA57A0F3821C288071B2DD656829D4`，实际含新连接策略类。原选择器证据为 `build/friend-strong-read-tidb-core-20261015/final-verification.json`。随机好友测试库和 Redis DB13 测试键只读审计剩余零。Java 版本 `0.1.0-SNAPSHOT`，无发布、真实 gate 或跨洲故障/容量验收；全球同服仍要求所有写者实际接入同一逻辑 TiDB 权威域，推送 outbox 等 §9.6 未完事项继续保留。
