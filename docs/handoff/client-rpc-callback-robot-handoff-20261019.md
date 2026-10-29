# 交接:robot 请求自带回调(按 MessageContent.id 认领回包)(2026-10-19)

> 写给换账号后接手 **robot 部分**的会话。整个功能(gate / Unity / 生成器 / 待拍板项)的总交接见 [client-rpc-callback-handoff-20261019.md](client-rpc-callback-handoff-20261019.md)。事实源是设计文档 [client-rpc-callback.md](../design/client-rpc-callback.md):robot 部分在 §6 与 §6.1,验证步骤在 §9 第 5 步。
> 本文只讲 robot:做到哪、哪些没提交、怎么验、接着迁什么、迁的时候会踩什么坑。
> 写作时间 2026-10-19 10:50(America/New_York),本仓 HEAD `c63b22dca`。同一工作区有多个会话并行,接手前先按 §7 重新核对。
> **2026-10-20 续做**(会话「做完 robot 剩余迁移」):§5 的 A、B 两类已迁完并经语义评审,随用户快照 `c35e1133d` 入库(§1.1);验证命令已按新范围重写(§2);C 类因周额度没做,分组与做法写在 §8,下一个会话可以直接照做。robot/ 场景迁移由本会话负责,`pkg/client.go` 的 `CallSyncThen` 归会话 53a2d1,R1 / 生成器 / Java 归会话 6b6cd9(10-20 三方已确认分工)。
> **2026-10-23 收尾**:C 类已按 §8 全部迁完并经评审(§9);§5 的 A / B / C 三类全部完成,只剩 D、E 两类按约定不迁。验证命令见 §2 与 §2.1。全部未编译,待 Codex 验证。

## 1. 现状

全部**未编译、未测试**(AGENTS §10.1,执行交给 Codex)。

| 部分 | 文件 | 提交状态 |
|---|---|---|
| 运行时 `Call` / `CallSync` / `LateReplies`、在途表、`SendRequest` 改发 id 0、收包按 id 认领、`Close` / `SwapConn` 让在途调用失败 | `robot/pkg/client.go`;`robot/pkg/redirect.go`(只改注释) | 已入库。是用户的批量快照 `53361d055` 带进去的,不是本会话提交的 |
| 单测 14 条 | `robot/pkg/client_test.go` | 初版在 `53361d055`;工作区另有 +3 行未提交(并发用例等 Close goroutine 退出) |
| 四个场景共用的 `smokeCaller` | `robot/smoke_rpc_call.go`(新) | `53361d055` |
| friend 迁移 | `robot/friend_smoke_scenario.go` | `53361d055` |
| guild / team / trade 迁移;team_follow 构造改为注入 `caller` | `robot/{guild_smoke_scenario,team_smoke_scenario,trade_smoke_scenario,team_follow_smoke_scenario}.go` | **工作区未提交** |
| 复核确认的 5 处陈旧注释订正(只改注释,§3) | `robot/{guild_smoke_scenario,team_follow_smoke_scenario,guild_economy_smoke,team_backfill_smoke_scenario,mail_smoke_scenario,ordering_smoke_groups}.go` | **工作区未提交** |
| 文档 | 设计 §6 标题、§6.1 实施记录、§9 第 5 步;`docs/PROGRESS.md` 条目「Claude:robot 请求自带回调」;本文 | 设计初版和 PROGRESS 第一条已入库;以上改动在工作区 |

本功能在工作区未提交的 robot 文件共 9 个(都在 `robot/` 下)。提交时只认这 9 个,外加设计文档 `docs/design/client-rpc-callback.md`、本文,以及 `docs/README.md`、`docs/PROGRESS.md` 里本功能那几行;其余改动是别的会话的:

- `guild_smoke_scenario.go`
- `team_smoke_scenario.go`
- `trade_smoke_scenario.go`
- `team_follow_smoke_scenario.go`
- `team_backfill_smoke_scenario.go`
- `guild_economy_smoke.go`
- `mail_smoke_scenario.go`
- `ordering_smoke_groups.go`
- `pkg/client_test.go`

**HEAD 不是坏的**:HEAD 的 `SendRequest` 已改发 id 0,但 guild / team / trade 在 HEAD 上还是旧的按消息号轮询,照样能按 message_id 收到回包。工作区迁移即使暂不提交,也不会让 HEAD 编不过或跑不通。

(10-20 注:上面 9 个文件已随用户快照 `c35e1133d` 入库。)

### 1.1 10-20 续做的状态

全部**未编译、未测试**。

| 部分 | 文件 | 提交状态 |
|---|---|---|
| `CallSyncThen` 与 3 条 `TestCallSyncThen*`(会话 53a2d1 所加) | `robot/pkg/client.go`、`robot/pkg/client_test.go` | `c35e1133d` |
| `smokeCaller` 加 `callWithin`、`callDispatch` | `robot/smoke_rpc_call.go` | `c35e1133d` |
| `smokeCaller` 的 stats 可为 nil、timeoutErr 为 nil 时包裹 `pkg.ErrRPCTimeout`(为 §8 准备) | `robot/smoke_rpc_call.go` | **工作区未提交** |
| A 类:mail、rank、sync 的 `friendRpc` | `robot/{mail,rank,sync}_smoke_scenario.go` | `c35e1133d` |
| B 类:travel / sync 的 `travelTo`、travel 的 `expectTravelRejected` 与金币读写(不再用 `readGoldBalance` / `gmAddGold`)、新增纯函数单测 5 条 | `robot/{travel_smoke_scenario,travel_smoke_wire,sync_smoke_scenario,sync_smoke_scenario_test}.go` | `c35e1133d`;补评审的修复见 §1.2 |
| B 类:channel 的 `requestEnterScene`、mystery_merchant 的 `merchantCall` | `robot/{channel,mystery_merchant}_smoke_scenario.go` | `c35e1133d` |
| B 类:ordering 的单发与流水线等待(`callAll`),回包超时 8s → 15s(I4) | `robot/ordering_smoke_{scenario,groups}.go` | `c35e1133d` |
| B 类:team 的排队请求(`matchCall`)、`startSoloBattle`,team_backfill 的 P4 / `backfillCancelQueue` / `backfillExpectQueueState` | `robot/team_smoke_scenario.go`、`robot/team_backfill_smoke_scenario.go` | `c35e1133d` |
| 文档 | 设计 §6 标题、§6.1 进度表与「10-20 续做」、§9 第 5 步指向本文;本文 §1.1、§2、§5、§8;PROGRESS 条目 | **工作区未提交** |

工作流:实现 + 语义评审 `wf_26396b1d-800`(5 组各一个实现、一个评审,中高级发现逐条对抗复核,再修复;sync / travel 组的评审撞额度失败,由 `wf_0f932c90-afc` 补跑)。4 组评审只报低级问题,都已修:mail 第 7 步反向断言补「无请求号回包」计数(未回显请求号的信封不再被当成 gate 丢弃)、ordering 超时提到 15s、rank 文件头的过期消息号、channel / ordering / team 的陈旧注释。

### 1.2 sync / travel 补评审

`wf_0f932c90-afc`(10-20 改动已入库后重开,对照 `c63b22dca` 评审):2 条低级发现,无中高级。
- 信封拒绝的诊断提示没覆盖 R1 的 1003、还写着「gate 没换新二进制」:改为 `travelSmokeEnvelopeHint`,10-23 再收敛为委托共享的 `sceneRequestEnvelopeHint`(只把限流指到 travel 自己的节流常量)。
- 服务端没回显请求号时,id = 0 的 TravelToZone 回包被丢弃、拒绝码丢失:10-22 travel 诊断组补了「未回显请求号回包」计数与诊断(`travelSmokeUncorrelatedReplies`),超时文案优先给出这条原因;判定逻辑不变。
- 金币槽取值改用共享的 `goldSlotValue`(单测 `TestGoldSlotValue`),travel 不再保留副本。

## 2. 验证(给 Codex,按顺序;任何一步失败保留输出并停下,不要连续重试)

(10-20 按 §1.1 的新范围重写。)工作目录 `robot/`,不需要额外环境变量。

1. `gofmt -l .`(只列不写,不要 `-w`):期望不列出 §1 / §1.1 里本功能改过的文件。列出了就回报文件名与 `gofmt -d` 摘要。
2. `go build -mod=vendor ./...`
3. `go vet -mod=vendor ./...`
4. `go test -mod=vendor -race ./pkg/...`
   通过标准:全绿,无 `DATA RACE`。重点看 `TestCallDoneExactlyOnceUnderConcurrentOutcomes`(10s 内完成)、3 条 `TestCallSyncThen*`(会话 53a2d1 所加),以及其余 `TestCall*` / `TestSendRequest*` / `TestSwapConn*` / `TestReply*` / `TestClose*`。
5. `go test -mod=vendor -race -run "TestSyncSmoke|TestTravelSmoke" -v .`:sync / travel 新增的 5 条纯函数单测(`TestTravelSmokeJudgeTravelReply`、`TestTravelSmokeScanHopUsesCallOutcome`、`TestTravelSmokeJudgeRejection`、`TestTravelSmokeReplyRejection`、`TestTravelSmokeGold`)与既有 `TestSyncSmoke*` 全部 PASS。
6. `go test -mod=vendor ./...`:确认主包既有单测(`battle_direct_conn_test.go`、`pkg/redirect_test.go` 等)不受影响。`ownership_fault_fullstack_test.go` 没设 `OWNERSHIP_FAULT_REQUIRE=1` 时 Skip,属正常。
7. 冒烟:`go build -mod=vendor -o robot.exe .`,然后在 `robot/` 下逐个执行 `.\robot.exe -c etc/<yaml>`。
   - 共同前提:各 yaml 文件头列出的服务就绪;gate 是含 `85a457a55`(直连回包回显请求号)的新编译版;经路由服的冒烟要求 gate 以 `GATE_CLIENT_RPC_ROUTER=1` 启动。

   | yaml | 覆盖 | 本项通过标准 |
   |---|---|---|
   | `friend_smoke` / `guild_smoke` / `team_smoke` / `trade_smoke` | 10-19 首批;team 另含 10-20 的排队请求与 `startSoloBattle` | `*_SMOKE_OK`。trade 第 2 步的超时是 gate 丢弃 TradeAdmin,属预期。team 默认 `cross_zone: true` 需双 zone,单 zone 改成 false;S8 日志应出现「StartTeamMatch rejected while B was in battle」 |
   | `team_follow_smoke`(可选) | 10-19 改了构造 | `TEAM_FOLLOW_SMOKE_OK` |
   | `team_backfill_smoke` | CancelQueue(Empty 响应,经 gRPC 照常回包)/ GetQueueStatus / JoinQueue | `TEAM_BACKFILL_SMOKE_OK` |
   | `mail_smoke` | mail call | `MAIL_SMOKE_OK`。第 7 步两个 MailAdmin 各等约 10s 超时,属预期;本冒烟有发件和领取副作用 |
   | `rank_smoke` | rank call | `RANK_SMOKE_OK`;跑的时候不要有别的 1V1 排队 |
   | `sync_smoke` | friendRpc、travelTo | 单 zone:`SYNC_SMOKE_OK … cross_zone=off`。跨 zone 腿要在双 zone 环境跑:把 yaml 复制到不入库的 `logs/sync_smoke_cross_zone.yaml`,只改 `cross_zone: true`,期望 `cross_zone=checked` |
   | `travel_smoke`(双 zone) | travelTo、expectTravelRejected、金币读写 | `TRAVEL_SMOKE_OK`,其中 `reject_tip`、`reject_map_tip` 非 0,`gold_back = gold_home + 7` |
   | `channel_smoke`(地图 ≥ 2 条线) | requestEnterScene | `CHANNEL_SMOKE_OK`,C1–C7 全部 PASS。只出现 `CHANNEL_SMOKE_SKIP reason=single_channel` 不算验证 |
   | `merchant_smoke` | merchantCall | `MERCHANT_SMOKE_OK`,M1–M8 全部 PASS。SKIP 不算验证;最好让商人不在机器人所在的线,这样 M3 会切一次线 |
   | `ordering_smoke`(路由模式) | callAll(单发与流水线) | `ORDERING_SMOKE_OK … rounds=20`,O1–O7 全部 PASS;`Gate pushed SendTipToClient` 为 0 |

   - 全局通过标准:每个冒烟退出码 0;robot 输出里 `reply without request id dropped` 与 `[rpc] late reply dropped` 都为 0。
   - 失败时保留 robot 输出中的 `*_SMOKE_FAIL step=… reason=…` 行,以及同窗口的 gate、路由服(client_rpc_router)、对应业务服日志。

失败的快速定位:

- **出现 `reply without request id dropped`**:服务端没回显请求号。先查 gate 是否为新编译版;scene TCP 路径(channel / merchant / travel)看 `scene_handler.cpp` 的 `set_id`。
- **friend 第 6 步 / guild 第 9 步拿不到信封拒绝**:看路由服是否把上游 PermissionDenied 翻成了 1003 并带回 id。
- **trade 第 2 步**:期望的就是超时,即 gate 丢弃 TradeAdmin 消息号。
- **R1 落地之后**(会话 6b6cd9 在做 gate 侧):scene 派发前早退的请求会收到带请求号的 1003 信封,不再超时,失败原因里会显示信封 tip=1003。白名单外的消息号仍然不回包,上面这几个「期望超时」的步骤不受影响。

### 2.1 第二批(C 类)追加验证(10-23)

§2 第 1–6 步(gofmt / build / vet / 单测)已覆盖全部改动文件,照跑即可。在此之上追加:

1. 单测:`go test -mod=vendor -race -run "TestDialBattleDirect|TestBattleDirect|TestEquip|TestAttributePanelOutcome|TestAssetArrival|TestMallArrival|TestFeature|TestTravelSmoke|TestSyncSmoke|TestGoldSlotValue" -v .` 全部 PASS、无 `DATA RACE`;再跑 `go test -mod=vendor ./logic/handler/ -run Feature -v`。新增或改动的用例:
   - battle:`TestDialBattleDirectParticipantPullsStateOnReady`(新增断言:请求号非 0、`LateReplies` 为 0)、`TestDialBattleDirectDropsUncorrelatedStateReply`、`TestBattleDirectStateReplyDoneCountsAndDispatches`、`TestBattleDirectPullBattleStateReportsNotSent`。
   - equip:`TestEquipFeatureOutcomeSeparatesEnvelopeBodyTipAndIncompleteReplies`;`TestAttributePanelOutcomeSeparatesEnvelopeAndBodyRejections`(10-23 由 equip 的面板判定用例改为测 attribute / pet / equip 共用的纯函数 `attributePanelOutcome`)。
   - features:`TestFeatureLobbyCall*` 三条。
   - travel 诊断:`TestTravelSmokeUncorrelatedReplies`、`TestTravelSmokeUncorrelatedCause`、`TestTravelSmokeDescribeTimeout`;`TestTravelSmokeJudgeRejection` 多了一个参数;`TestTravelSmokeReplyRejection` 的 GM 闸期望文案改为 `gate GM guard rejected it`。
   - `TestGoldSlotValue`(原 `TestTravelSmokeGold`,改测共享的 `goldSlotValue`)。
2. 冒烟(在 `robot/` 下 `.\robot.exe -c <配置>`;前提同 §2 第 7 步,另需 GM 闸为 dev / test):

   | 配置 | 覆盖 | 本项通过标准 |
   |---|---|---|
   | `etc/chat_smoke.yaml`(默认双 zone;单 zone 复制到 `logs/` 改 `cross_zone: false`) | chat 的 SendChat / PullChatHistory | `CHAT_SMOKE_OK`;第 5 步 tip 为 kMessageSizeExceeded |
   | `etc/robot.currency-crash.yaml`(currency-crash-snapshot,需 satoken 服务) | `readGoldBalance` / `gmAddGold` | 日志有 `currency-crash-snapshot leg complete`;`logs/currency_crash_window/snapshot.json` 没有 error 字段,`post_balance == pre_balance + 10000` |
   | `etc/attribute_smoke.yaml`、`etc/pet_smoke.yaml` | 属性 / 宝宝请求与金币 helper | `ATTRIBUTE_SMOKE_OK`、`PET_SMOKE_OK` |
   | `etc/equip_smoke.yaml`、`etc/equip_forge_smoke.yaml`、`etc/equip_forge_topup.yaml`、`etc/mall_arrival_smoke.yaml` | 背包请求(callDispatch)、属性面板、金币;mall 复用 equip 会话 | `EQUIP_SMOKE_OK`、`EQUIP_FORGE_SMOKE_OK`、`EQUIP_FORGE_TOPUP_OK`(会往账号里累加物品与银两)、`MALL_ARRIVAL_SMOKE_OK`(前置见该 yaml 文件头) |
   | `etc/battle_smoke.yaml`、`etc/battle_smoke_cross_zone.yaml`、`etc/rank_smoke.yaml` | 大厅 JoinQueue / WatchBattle、直连 GetBattleState 补拉 | `BATTLE_SMOKE_OK`(日志有 `[battle-smoke] JoinQueue accepted` 与 `WatchBattle accepted`)、`CROSS_ZONE_MATCH_OK`(两侧 `state_replies>=1`)、`RANK_SMOKE_OK` |
   | `etc/team_smoke.yaml`;可选 `etc/guild_smoke.yaml` 且 `activities: true` | 直连补拉改动的下游回归 | `TEAM_SMOKE_OK`、`GUILD_SMOKE_OK` |
   | `etc/robot.behavioral.yaml`(stress + behavioral,20 个机器人,跑约 2 分钟后 Ctrl+C) | 压测主循环的 ListSkills、`sendAndRecvLocal` | 每个机器人都 entered game;`timed out waiting for skill list`、`skill list unavailable`、`robot panic` 都为 0;reporter 的 login_stuck 为 0;`behavior_test_results.csv` 的 cast_skill 行没有 `no owned skills`。msg_sent 每会话多 1 属预期 |
   | login-test 本地配置:复制 `etc/robot.yaml` 到不入库的 `logs/login_test.yaml`,只改 `mode: "login-test"`、`robot_count: 5` | `prepareBehaviorClient`、`testSkillCast`、`testSceneSwitch` | 各用例 Passed |
   | features-smoke 本地配置 `logs/features_smoke.local.yaml`(`mode: "features-smoke"`,字段照 `config.FeaturesSmokeConfig`,账号与认证照 `etc/equip_smoke.yaml`) | 大厅与直连的 `callVia` | 分三轮:只读(只填 account)期望 `FEATURES_READ_OK` 与各 `*_SKIPPED`、`FEATURES_SMOKE_OK`;加 `bag_type: 0`、`sort_bag: true` 期望 `FEATURES_SORT_OK … second_sort_changed=false`;战斗全链加 `battle_config_id: 1`、`scope: 0`、`accept_mission_id: 12`、`claim_mission_id: 12`、`verify_relogin: true`,期望 `FEATURES_BATTLE_OK`、`FEATURES_CLAIM_OK … duplicate_rejected=true`、`FEATURES_RELOGIN_OK`、`FEATURES_SMOKE_OK`(同一角色只能过一次,重跑前清数据或换号) |
   | asset-arrival-prepare 本地配置 `logs/asset_arrival_prepare.local.yaml`(`mode: asset-arrival-prepare`,`robot_count: 1`,显式 `zone_id`,`auth_type: password`,`use_http_login: false`,`asset_arrival_prepare: {run_mode: dev, dedicated: true, account: robot_assetarrival_<后缀>, gold_target: 17..100000, equipment: true, hold_seconds: 120, output_path: <尚不存在的绝对路径>}`) | equip 复用路径 | stdout 输出一行只含 player_id / item_guid / zone_id 的 JSON,output_path 写出同样内容,保活满 hold_seconds 后退出码 0 |

   - 全局通过标准同 §2,另外 `[battle-direct] catch-up GetBattleState got no reply` 为 0,不出现非 0 的 `uncorrelated_replies` 或 `without a request id`。
   - 可选负向检查:路由模式下停掉 match 再跑 `battle_smoke`,应在几秒内 `BATTLE_SMOKE_FAIL step=a-join-queue`(信封 1003 或超时文案),不应等约 40s 才报 b-wait-battle-start。做完恢复 match。
   - 玩家数据可以清空(项目未上线):features 战斗全链与 equip 系列会改账号存档,反复跑前清库或换账号即可。

## 3. 静态复核结论(工作流 `wf_5cd420ca-79e`,只读,不代替编译)

做法:两个维度查找(`pkg` 运行时与单测;`main` 包的 smokeCaller 与场景迁移),每条发现再派一个复核者尝试驳倒。另有一路盘点剩余迁移点(§5),一路核对功能整体状态。

- **pkg 维度:0 条成立,2 条被驳倒。**
  - 「SwapConn 不看 Close 的终态」:与 redirect 竞争时会留下新连接。但这是改造前就有的行为,本次没有变坏;终态只约定 `Call` 一律 NotSent,这一点仍然成立。
  - 「并发用例对 CallSync 只数返回次数」:各完成路径共用 `finishCall` / `completeAll`,320 个 `Call` 已经覆盖了恰好一次。
  - 查过且干净的:标识符与导入、muduo API、双 `%w`、vet 各分析器、gofmt 对齐、锁顺序(只有 `callMu → connMu`)、恰好一次、回调不在锁内、单测确定性,以及本机监听不 Accept 时 muduo 能连上且 Close 及时返回。
- **main 维度:5 条成立,全是陈旧注释**(迁移后仍写「按消息号认领」),已订正,见 §1。

## 4. 运行时契约速记(继续迁移时必须遵守)

- **`Call` 只用于恰好回一个包的方法。** scene TCP 路径上响应恰好是 `Empty` 的方法是单向的,gate 不回包,用 `Call` 必然超时:`MoveStart` / `MoveStop` / `MoveSync`、`SceneInfoC2S`、`GetSyncDigest`。这类保留 `SendRequest` 加推送等待。gRPC 路径(路由服 / 直连)的 Empty 响应照常回包,可以用 `Call`。
- **带请求号的回包不进 onMessage,生成的 handler 也就不会跑。** 凡是等「生成 handler 写进 Player 的状态」的(面板 / 列表序号、FeatureResponse、货币 ready 通道、chatInbox、技能就绪),迁到 `Call` 后都必须改成直接解析 `Call` 的结果,或者在 done 里手动调 handler,否则只会超时。
- **done 跑在 RecvLoop 或定时器 goroutine 上,不得阻塞。** `CallSync` 禁止在 RecvLoop goroutine 上调用,包括消息处理器、done 回调和重定向重登录(`pkg/redirect.go` 的 `ReloginFunc`)。
- **断线:** `Close` 是终态,在途调用以 `ErrRPCDisconnected` 失败,之后的 `Call` 一律 `ErrRPCNotSent`。`SwapConn` 只让旧连接上的调用失败。vendored muduo 被对端断开后会静默重拨、不报错,这种情况只能等超时。
- **场景 onMessage 的写法(照 friend / guild / team / trade):**
  - 请求消息号上 id = 0 的消息交给 `smokeDropUncorrelatedReply` 丢弃,不交 `handler.MessageBodyHandler`:信封拒绝会被解成全零响应,看上去像受理。
  - 收到 `SendTipToClient` 时调 `caller.onGateTipMessage(msg)`,让 gate 推 kServiceUnavailable 时快速失败。
  - 回包的 `MsgRecv` 计数已移进 `smokeCaller`。
- **`smokeCaller` 同一时刻只服务一次等待。** 这与「一个机器人只由一个 goroutine 驱动」的约定一致。要在同一连接上流水线发多条(ordering 冒烟),直接用 `gc.Call` 各自登记回调。
- **业务幂等键不能拿信封 id 代替**:一次操作一个键,所有重发共用(设计附录 A 末条)。

## 5. 剩余迁移清单(按设计 §6 第 4 步的顺序;行号取自 2026-10-19 工作区)

### A. 同形 call(10-20 已迁,见 §1.1)

| 位置 | 说明 |
|---|---|
| `robot/mail_smoke_scenario.go:731` `mailSmokeBot.call` | 有 `awaiting` 兜底和 `gateTips` 快速失败。兜底只挡住两次调用之间到达的迟到回包;上一次超时请求的回包若在下一次同号调用期间到达,仍会被误认。第 7 步对推送号 `NotifyMailChanged` 本身发请求,迁后靠 id 区分。onMessage 认领 8 个邮件号,迁移时保留推送分支,其余改 `smokeDropUncorrelatedReply`。上层 `rpc` / `expect` 经它走 |
| `robot/rank_smoke_scenario.go:439` `rankSmokeBot.call` | 只处理 GetRankPage,`awaiting` 的局限同 mail。没有 SendTipToClient 快速失败,`honourSpacing` 控制间隔 |
| `robot/sync_smoke_scenario.go:767` `syncSmokeBot.friendRpc` | 与 friend 迁移前的 call 同形,但基于留底 records,没有 `awaiting`。调用方 `syncSmokePreclean`、`rejectPendingFrom`、`pendingRequestIDsFrom` |

### B. 其余等待帮助函数(10-20 已迁,见 §1.1;下面写明「可以不迁」的推送等待照旧)

- channel:`channel_smoke_scenario.go:560` `requestEnterScene`(EnterScene 同步应答);mystery_merchant `mystery_merchant_smoke_scenario.go:90` `merchantCall`(经 `channelSmokeBot.send`)。
- travel:`travel_smoke_scenario.go:885` `expectTravelRejected`、`:680` `travelTo`。travelTo 正常路径回包可有可无,旧连接会被关。
- sync:`sync_smoke_scenario.go:891` `travelTo`,与 travel 同形。
- ordering:`ordering_smoke_scenario.go:297` `waitReplies`,以及调用它的 `:346` `pipelineJoinThenCancel`、`:377` `queueState`、`:397` `cancelWithTicket`;`ordering_smoke_groups.go:693` `pipeline`、`:717` `rpcTip`。流水线场景迁移时用多个并发 `gc.Call`,各自按 id 认领。O7 组在 RecvLoop 已启动的连接上连发 Login → EnterGame,不是同步握手,也可以迁。
- team:`team_smoke_scenario.go:663` `send` 配合 `:949` `teamSmokeWaitPush`,用于 JoinQueue / CancelQueue / GetQueueStatus;`:808` `startSoloBattle`;`team_backfill_smoke_scenario.go:228`、`:364` `backfillCancelQueue`、`:378` `backfillExpectQueueState`。换场景(`:768` `switchScene`)等的是 NotifyEnterScene 推送,可以不迁。

### C. 依赖生成 handler 副作用的等待(10-22 ~ 10-23 已迁,逐处做法见 §9)

- attribute:`attribute_smoke_scenario.go:518` `send`、`:442` `callTolerant`、`:530` `waitSuggestion`。
- pet:`pet_smoke_scenario.go:506` `send`、`:453` `callTolerant`、`:485` `callAttributePanel`、`:517` `waitSuggestion`。
- equip:`equip_smoke_scenario.go:632` `call`、`:756` `attributePanel`。equip_forge、equip_forge_topup、asset_arrival_prepare 都复用它们。
- features:`features_smoke_scenario.go:417-423` `call` / `callVia`;`features_battle_smoke.go:174` `callDirect`(直连)。
- 货币:`currency_crash_window_scenario.go:51` `readGoldBalance`、`:78` `gmAddGold`。attribute、pet、equip_forge、travel、currency 多处调用;读不出余额时分不清是余额为 0 还是被拒。
- chat:`chat_smoke_scenario.go:383` `send`、`:404` `sendChat`、`:423` `pullHistory`,都等 chatInbox。
- channel:`channel_smoke_scenario.go:490` `requestSceneInfo`。SceneInfoC2S 是单向,等的是推送,保留。
- battle 大厅侧:`battle_smoke_scenario.go:313`(JoinQueue)、`:182`(WatchBattle);`battle_smoke_cross_zone_scenario.go:244`(rank 冒烟也复用)。回包只打日志,等 WaitBattleStart / WaitBattleAssigned。
- 压测主循环与登录测试:`main.go:541`(ListSkills → WaitSkillsReady);`login_test_scenarios.go:1123` `prepareBehaviorClient`、`:1154` `testSkillCast`、`:1197` `testSceneSwitch`。

### D. 不迁:同步握手,跑在 RecvLoop 之前或之上,只能 `SendRequest + RecvOne`

- `login.go:138` `sendAndRecvTimeout`
- `main.go:880` `sendAndRecvLocal`,以及 `:83` 注册的重定向重登录
- `features_smoke_scenario.go:373` `featureAuthCall`
- `travel_smoke_scenario.go:503` `travelSmokeStrictRelogin`
- `pkg/redirect.go:184` `followRedirectLocked`
- `login_test_scenarios.go` 的 `:434` / `:646` / `:704` / `:894` / `:931`(异常登录用例,故意不读回包或用 RecvOne 数回包)

### E. 不必迁:发出即走,或单向

移动上报、直连 `SetAutoBattle`(`team_smoke_scenario.go:874`、`battle_smoke_scenario.go:357`、`battle_smoke_cross_zone_scenario.go:278`、`guild_activity_smoke.go:735`)、LeaveGame / Disconnect 收尾、`data_stress.go`、`logic/ai/robot_ai.go` 的 AI 循环、`login.go:268` 已弃用的 TCP RefreshToken。

### 迁移陷阱(迁之前先看)

- `battle_direct_conn.go:181` 补拉 GetBattleState 的回包由 `record()` 计入 `stateReplies`(`:222`),`battle_smoke_scenario.go:378` 与 `battle_smoke_cross_zone_scenario.go:300` 断言它 ≥ 1。改成 `Call` 后回包不经 onMessage,这两处断言会失败,要一起改。
- ~~travel 的 `replyRejection`(`travel_smoke_scenario.go:627` 起)依赖回包既进留底又走通用分发。~~ 10-20 已改为纯函数 `travelSmokeReplyRejection`,从 Call 结果取。
- `login.go:268` 的 RefreshToken 靠生成 handler `ClientPlayerLoginRefreshTokenHandler` 写回新 token。若迁到 `Call`,要在 done 里自己写。
- 设计 §6.1 的进度表把 chat、currency_crash_window、battle_smoke 大厅侧、channel 的 requestSceneInfo 归为「其余等待帮助函数」。按它们实际等待的对象看,属于 C 类,以本文为准;§6.1 表已指向本文。

## 6. 顺带发现的既有缺陷(10-22 已修,见 §9.2)

- `main.go:879` `sendAndRecvLocal` 不检查信封 `error_message`,空 body 会被当成成功(`login.go` 的 `sendAndRecvTimeout` 有这道检查)。
- `login.go:138` `sendAndRecvTimeout` 与 `login_test_scenarios.go:683` `testDuplicateLoginRequest` 超时后留下阻塞在 `RecvOne` 的 goroutine,之后会和 RecvLoop 抢 `Recv`。
- §5 B 类的留底认领、mail / rank 的 `awaiting` 兜底,都可能把上一次超时请求的迟到回包当成本次结果。迁到 `Call` 即修。(10-20:A、B 两类已修,C 类仍有,随 §8 修。)

## 7. 接手前必做与已知坑

- **重新核对工作区**:`git status --short robot docs` 加 `git log -1 -- <文件>`。用户会定期整批快照提交(「补充…」「合并…」),本表「未提交」的文件随时可能已入库:`53361d055` 就带走了本功能前半部分,`fc3ff045e` 合入了 mail / rank / team_follow / team_backfill 等新场景。
- **不要自己跑构建 / 测试 / 冒烟**(AGENTS §10.1),命令照 §2 交给 Codex。
- PROGRESS 里 Codex 的对账条目已注意到「其他工作正在修改 robot 的 guild/team/trade 冒烟源码」。Codex 准入前应审实际 diff,而不是只看描述。
- 提交只在用户发话时做,且只提交 §1 列出的文件。
- `smokeCaller` 本身没有单测,靠四个冒烟验证。
- 记录:运行时实现与迁移在会话「robot 客户端改为按请求号回调」完成;只读复核工作流 `wf_5cd420ca-79e`;上一级总交接见文首链接。

## 8. 第二批:C 类迁移方案(10-20 写;10-22 ~ 10-23 已按此执行,结果见 §9)

**为什么没做**:第一批(14 个代理,约 250 万子代理 token)跑完时周额度已到 83%,补评审期间 15 分钟又涨到 87%(同期还有会话 53a2d1、6b6cd9 在跑)。C 类要 6 个落码代理外加评审,极可能跑到一半撞上周额度,留下半截改动并把账号锁到 10-22 16:00(America/New_York)。所以只把方案写到能直接开工的程度。

### 8.1 C 类共通做法(在 §4 契约之上)

- C 类的等待依赖生成 handler 把回包写进 `gameobject.Player`(面板序号 / 来源、`FeatureResponse(cursor)` 快照、货币 ready 通道、chatInbox、技能就绪)。迁到 Call 后回包不经 onMessage,handler 不跑。逐处二选一,并在交付里写明选了哪种:
  1. **直接解析 Call 的结果**:等待方只要响应本身、Player 上那份状态别处没人读时用。最干净,也是 §5.5 生成器改造后退役手写 handler 的方向。
  2. **`smokeCaller.callDispatch(..., dispatch)` 或 `gc.CallSyncThen(..., onReply)`**:Player 状态别处还在读(先全包 grep 读取者)时用。dispatch 通常是 `handler.MessageBodyHandler`,在 RecvLoop 上执行完才返回,副作用与迁移前同一 goroutine、同一收包顺序。**不要**在 CallSync 返回后由调用方 goroutine 补跑 handler:会与同连接上后到的推送交错,旧回包可能盖掉新推送。返回后读快照(如 `FeatureResponse(messageId, cursor)`)可以立刻读到,不必再轮询。
- **信封错误**:`callDispatch` 不把信封交给 dispatch,而是作为 envelopeTip 返回。迁移前 `prepareBehaviorClient`(`login_test_scenarios.go`)的收包回调把属性消息的信封错误翻译成 Player 上的 tip(`SetAttributePanel(nil, tip, msgId)`),equip 的「全零响应」判据也是为信封拒绝准备的;迁移后由调用点直接处理 envelopeTip,判据随之改写,报错文案保留。
- `RecordFeatureBody`(`logic/handler/player_features_response.go`)里的响应完整性判据(包缺 layout 等)是唯一权威。选做法 1 时不要在场景里复制一份,改用做法 2 读快照,或直接调用它。
- `robot/logic/**`(handler、gameobject)被多个场景共用,第二批不改;确有需要就在交付里提出。
- 共享帮助函数签名不变:`readGoldBalance` / `gmAddGold`(attribute、pet、equip_forge 等在用;travel 10-20 起已不用)、`prepareBehaviorClient`、`battleDirectConn` 的现有方法(可以加新方法)、rank 冒烟复用的 `battle_smoke_cross_zone_scenario.go` 跨区战斗函数。
- 等待点所在连接的 RecvLoop 必须已经启动(Call 的回包只能由 RecvLoop 或 RecvOne 派发);迁之前逐处确认。
- **R1**(会话 6b6cd9,gate 侧):请求号非 0、内层响应不是恰好 Empty 的请求,遇到 scene 派发前早退(会话不在、玩家仍在加载、请求解析失败等 11 处)时,gate 回带请求号的 1003(`tiptable.CommonError_kServiceUnavailable`)信封,Call 收到 `*pkg.ServerTipError{Tip: 1003}`,不再超时。断言「scene 丢弃 → 超时」的步骤要改成断言 1003。10-20 grep 过现有的「期望超时」断言,都是 gate 白名单丢弃或推送窗口,不受影响。
- `smokeCaller` 的 stats 可为 nil;场景没有超时哨兵时 timeoutErr 传 nil(包裹 `pkg.ErrRPCTimeout`)。

### 8.2 分组(文件互不重叠,可并行;按波次排序)

第一波先做被别组依赖的三组,第二波的代理就能直接看到定稿的共享帮助函数:

| 波 | 组 | 只改这些文件 | 等待点与要点 |
|---|---|---|---|
| 1 | currency-chat | `currency_crash_window_scenario.go`、`chat_smoke_scenario.go` | `readGoldBalance`(:51)、`gmAddGold`(:78):签名不变;先 grep Player 货币状态(`GetCurrencyValue`、`GetLastBalanceAfter`、ready 通道)的全部读取者再选做法;顺带让「读不出余额」能区分余额为 0 和被拒。chat 的 `send`(:383)、`sendChat`(:404)、`pullHistory`(:423)等的是 chatInbox:响应直接解析,聊天推送仍走 handler inbox;`gateRejection` / `envelopes` / `gateTips` 改为 `caller.onGateTipMessage` 与 envelopeTip |
| 1 | main-login | `main.go`、`login_test_scenarios.go`、`login.go` | 压测主循环 `main.go:541` ListSkills → `WaitSkillsReady`:用做法 2(AI 循环读 Player 技能),保持 5s 与只告警的语义,别给 4.5 万机器人加逐次日志。`prepareBehaviorClient`(:1123,ListSkills + sleep 1500ms)、`testSkillCast`(:1154,ReleaseSkill 回包 = ack)、`testSceneSwitch`(:1197,等 NotifyEnterScene 推送,可只把请求改 Call 以便快速失败)。D 类同步握手不迁。顺带修 §6 第一条:`sendAndRecvLocal` 补信封错误检查(照 `sendAndRecvTimeout` 返回 `*envelopeError`,先核对调用方对它的处理);§6 第二条 RecvOne goroutine 泄漏先核实超时后有没有调用方继续用这个连接起 RecvLoop,没有就只补注释说明不变式,有就做最小修复 |
| 1 | battle | `battle_smoke_scenario.go`、`battle_smoke_cross_zone_scenario.go`、`battle_direct_conn.go`、`battle_direct_conn_test.go` | 大厅侧 JoinQueue(`battle_smoke_scenario.go:313`、`battle_smoke_cross_zone_scenario.go:244`)、WatchBattle(:182):回包现在只打日志,改成 Call 后可以让拒绝快速失败,开战 / 观战仍等推送;先查 JoinQueue 的生成 handler 有没有写 Player 状态再选做法。直连补拉 `battle_direct_conn.go:181` GetBattleState:改 `gc.Call`,done 里 `stateReplies` 计数并把成功回包交给原 onMessage(信封错误只记日志),保住 `battle_smoke_scenario.go:378`、`battle_smoke_cross_zone_scenario.go:300` 的 ≥ 1 断言。SetAutoBattle 发出即走,不迁 |
| 2 | attribute-pet | `attribute_smoke_scenario.go`、`pet_smoke_scenario.go` | attribute 的 `send`(:518)、`callTolerant`(:442)、`waitSuggestion`(:530);pet 的 `send`(:506)、`callTolerant`(:453)、`callAttributePanel`(:485)、`waitSuggestion`(:517)。按「面板序号 + 来源消息号」认领;GmSetPlayerLevel 先推一份面板(NotifyAttributePanelChanged)再回响应,迁后响应按请求号就是本次的,推送照旧经 handler 写 Player |
| 2 | equip | `equip_smoke_scenario.go`、`equip_forge_smoke_scenario.go`、`equip_forge_topup_scenario.go`、`asset_arrival_prepare.go` 及各自 `_test.go` | `call`(:632,`FeatureResponse(cursor)`)与 `attributePanel`(:756);`panelAfter` 等的是推送,保留。forge / topup / asset_arrival 复用这两个函数 |
| 2 | features | `features_smoke_scenario.go`、`features_battle_smoke.go` 及各自 `_test.go` | `call` / `callVia`(:417-423)、直连 `callDirect`(`features_battle_smoke.go:174`,经 `battleDirectConn`;直连连接上同样回显请求号,`battle_client_edge.cpp:436,491`)。`HandleFeatureMessage` / `HandleFeatureBattleMessage` 会处理信封;用 `callDispatch` 时信封不交 dispatch,快照不会写,必须看 envelopeTip。`featureAuthCall`(:373)是同步握手,不迁 |

### 8.3 执行建议

- 规模:每组「实现 → 语义评审 → 中高级发现逐条对抗复核 → 修复」,与第一批相同(第一批 5 组共 14 个代理、约 250 万子代理 token)。按波开,每波开之前看 5 小时与周额度;周额度高于 90% 就不开。
- 实现代理提示里带续做说明(先 `git diff` 看现状、在半截基础上补完),并点名 §4、§8.1 与第一批已迁好的参照(friend / mail / travel / ordering)。
- 用户会不定期整批快照提交:评审代理要用 `git diff <迁移前提交> -- <文件>` 看改动,不要只用 `git diff`;10-20 的补评审就因为改动已入库而重开过一次。
- 冒烟(`robot/etc/` 下,10-20 核对过文件存在):`attribute_smoke`、`pet_smoke`、`equip_smoke`、`equip_forge_smoke`、`equip_forge_topup`、`chat_smoke`、`battle_smoke`、`battle_smoke_cross_zone`、`robot.currency-crash.yaml`(currency-crash-snapshot 模式)、`robot.behavioral.yaml`(stress 模式 + behavioral profile,覆盖 `main.go` 的 ListSkills 等待)。login-test、features-smoke、asset-arrival-prepare 没有入库的 yaml,要用对应的 `mode:` 另写不入库的本地配置。通过标准与 §2 的全局标准相同。

## 9. 第二批 C 类实施记录(10-22 ~ 10-23,会话「Robot handoff documentation [4de6a5]」,未编译)

工作流:第一波 `wf_562937ab-7ad`(进程中断,battle / main+login 的评审未跑完);第二波与第一波补评审 `wf_d1b64090-2f6`(撞 5 小时额度,3 个代理失败);收尾 `wf_dd238941-6ce`。每组「实现 → 语义评审 → 中高级发现逐条对抗复核 → 修复」;失败的代理都没有改过文件。提交状态:两波实现与大部分评审修复已随用户快照(`2c07f2c28`、`9d2784fbf`)入库;本会话 10-23 的收尾改动(§9.3 equip 一行与 6b6cd9 转交的注释)在工作区,以 `git status` 为准。

### 9.1 逐组做法

| 组 | 文件 | 做法 |
|---|---|---|
| currency + chat | `currency_crash_window_scenario.go`、`chat_smoke_scenario.go` | `readGoldBalance` / `gmAddGold` 签名不变,改为直接解析(全包没有别的读者读 Player 货币状态);信封拒绝、scene 业务拒绝(kFeatureUnavailable 的提示按方法区分退出闸 / GM 闸)、超时分开报,业务拒绝不再返回 `(0, nil)`。新增共享的 `goldSlotValue`、`sceneRequestEnvelopeHint`。chat 的 SendChat / PullChatHistory 走 `smokeCaller` 直接解析响应,删掉 chatInbox 等待与 envelopes / gateTips |
| main + login | `main.go`、`login_test_scenarios.go`、`login.go` | 压测主循环 ListSkills:`startListOwnedSkills` 用 `gc.Call`,done 在 RecvLoop 上交 handler(保住被动技能剔除),主 goroutine 仍只等 5s、超时只告警。`prepareBehaviorClient` 同样处理,保留 1.5s 停顿(它也在等 AOI 推送),之后到达的失败也会记日志。`testSkillCast` 直接解析 ReleaseSkillResponse(判据收紧:必须收到无错的回包);`testSceneSwitch` 请求改 Call 以快速失败,推送计数等待保留。两处收包回调丢弃 ListSkills / JoinQueue / WatchBattle 上 id = 0 的回包 |
| battle | `battle_smoke_scenario.go`、`battle_smoke_cross_zone_scenario.go`、`battle_direct_conn.go`(+`_test`) | 大厅 JoinQueue / WatchBattle 直接解析(生成 handler 只打日志),拒绝快速失败,开战 / 观战仍等推送;失败补 Warn 日志(rank 冒烟按先 A 后 B 读结果,B 的拒绝原因靠这行留痕);新增断言 b-watch-battle-id。直连补拉 GetBattleState 经新增的 `battleDirectConn.Call`,done 里先 record(计 stateReplies)再交 onMessage |
| attribute + pet | `attribute_smoke_scenario.go`、`pet_smoke_scenario.go` | 直接解析响应(面板 / 宝宝列表 / 加点建议),序号游标与来源判断不再需要;allowedTips 只放行业务码,信封拒绝一律报错。属性判定收敛为共享纯函数 `attributePanelOutcome`,信封报错共用 `sceneRequestEnvelopeRejection` |
| equip | `equip_smoke_scenario.go`(+`_test`)、`equip_forge_smoke_scenario.go`、`equip_forge_topup_scenario.go` | 背包请求 `callDispatch(…, handler.MessageBodyHandler)` 后立刻读 `FeatureResponse(cursor)`,保住 `RecordFeatureBody` 这一唯一判据;信封拒绝直接报错,不再表现为「全零响应」。`attributePanel` 改用共享的 `requestAttributePanel`。`panelAfter` 等的是推送,保留。asset_arrival_prepare 与 mall_arrival 复用会话,接口不变 |
| features | `features_smoke_scenario.go`(+`_test`)、`features_battle_smoke.go`(+`_test`) | `callVia` 改为接连接(大厅 gc 或直连 `direct.gc`),用 `CallSyncThen` 把回包(含信封)交 `recordFeatureReply`(复用 `HandleFeature*` 的信封处理)后立刻读快照;两条 RecvLoop 回调丢弃本场景请求号上 id = 0 的消息 |
| travel 诊断 | `travel_smoke_scenario.go`、`sync_smoke_scenario.go`(+`_test`) | 见 §1.2 |

### 9.2 交接 §6 两个既有缺陷

- `sendAndRecvLocal` 补信封错误检查,返回 `*envelopeError`;调用方只 `%w` 包装,除了「空 body 不再被当成功」没有别的行为差异。
- 握手超时后残留的 RecvOne goroutine:核实发现 `loginAndEnterWithAuth` 的回落链(access_token → http-login → satoken / local)与重定向重登录都会在同一连接上继续握手、成功后还起 RecvLoop,会被残留 goroutine 抢包。修法:超时错误包裹新哨兵 `errLoginStuck`(文案仍含 STUCK),超时后不再在同一连接上回落,由外层换新连接重试;重定向路径超时直接失败。

### 9.3 评审结论

| 组 | 发现 | 处理 |
|---|---|---|
| currency + chat | 4 条 low | kFeatureUnavailable 提示按方法区分;共享 helper 不再依赖 travel 私有函数;travel / equip_forge 的过时注释由本会话与 equip 组修掉 |
| main + login | 3 条 low | 收包回调补丢弃 JoinQueue / WatchBattle 的 id = 0 回包;`prepareBehaviorClient` 之后到达的失败也记日志;重定向路径注释订正 |
| battle | 1 medium(复核成立)+ 2 low | 失败补 Warn 日志;1003 文案改为「也可能结局未知」;stats 为 nil 时信封补拉回包漏计 MsgRecv,只写注释 |
| attribute + pet | 2 low | 注释订正;两份相同的信封报错合并 |
| equip | 5 low | 本会话收尾:合并 equip / attribute 重复的面板判定(`attributePanelOutcome`)与信封报错;改 3 个 yaml 文件头、`logic/handler/player_features_response.go` 与 `scene_bag_client_player_equip_item.go` 的注释、`docs/design/equipment-attributes.md` §10.6;equip 超时提示补「或 gate 没回显请求号」 |
| features | 1 low | 订正「信封拒绝 = 没到业务逻辑」的注释(1003 可能是结局未知) |
| travel 诊断 | 1 medium(复核驳倒:基线即如此,本次是改进)+ 1 low | low 已修(信封拒绝时不再打印自相矛盾的 body tip) |

另:会话 6b6cd9 转交的三处旧生成器注释(`logic/handler/match_service_responses.go`、`guild_smoke_scenario.go`、`trade_smoke_scenario.go`)已按新规则订正;match 的三个应答 handler 写在非标准文件名里,生成表不登记,`init()` 补登记仍然需要。

### 9.4 已知限制与后续(不阻塞)

- `prepareBehaviorClient` 只丢弃 ListSkills / JoinQueue / WatchBattle 上 id = 0 的回包;背包 / 属性号上 id = 0 的回包(服务端没回显请求号)仍会被 handler 静默落进 Player,对应 Call 以超时收场,equip 的超时文案已点名这种可能。不能一律丢弃 id = 0:AI 循环等仍用 SendRequest,需要 handler。
- match 全球权威模式(`GlobalMatch.Enabled`,非 dev / test 档强制)要求 JoinQueue / WatchBattle 带业务幂等键 `request_id` 与地域探测的 `NetworkPlanId`;robot 冒烟迁移前后都没填,只能在 dev 档跑。要支持需另开任务(一次操作一个键,重发共用)。
- `gameobject.Player.WaitSkillsReady` 已无调用者,`robot/logic/**` 本轮没清理。
- 退役 REQUEST 方法的旧手写 handler 时,必须同一批先重跑 proto 生成(`message_body_handler.go` 是生成物,随之去掉该条),再编译 robot;只删文件不重生成会编不过(会话 6b6cd9 提醒)。
- Codex 已生成 `robot/generated/rpc/` 类型化调用桩(如 `rpc.ClientPlayerFriend.AddFriend(c, req, timeout, done)` / `AddFriendSync(ctx, c, req, timeout)`)。现有场景用 `smokeCaller` / `gc.Call`,是否改用调用桩由后续决定,不影响正确性。
- §12 双版本:本轮只改本仓 robot 冒烟工具及其注释与设计文档,不涉及客户端可见契约;Java 版 game-robot 是否要做同类迁移未核对,按约定不往 Java `PARITY.md` 加行。
