# 客户端请求幂等键：场景服经济操作的去重账本（随存档落盘）

**Created:** 2026-11-05
**状态:** 三处接入点都已落码：神秘商人购买、新建加点方案、强化。强化接在「数据覆写审计 R2 交接」会话交付的 `expected_level` 之上。2026-11-06 负责人定「改成存盘」，账本从内存改为玩家存档字段 25。服务端代码随 `a8af9372d` 入库；2026-11-08 Codex 的 `build/verification/naming-audit-20261108/` 跑过 §8 的单测：`bag_test` 932 条通过、0 条失败、2 条跳过（跳过的与本功能无关；本功能新增的 40 条全是 OK），`cross_zone_test` 458 条通过。Unity 客户端 2026-11-12 已按 §6 与交接文档落码（客户端仓 main，未生成、未编译、未运行，见 `docs/PROGRESS.md` 同日「客户端·强化期望等级与幂等键、临时格取回、进场失败后换角」条）。§8 第 6 步的 robot 冒烟 2026-11-13 已改完三个场景（未编译、未运行，见 `docs/PROGRESS.md` 同日条目）。跨连接恢复（交接 §9）仍没做；Java 版待做（§7）。
**任务来源:** 负责人 2026-11-05：「玩家经常做一些和经济相关的，比如使用道具，服务器响应没回来，它又对着原来道具使用一次……玩家真正意图是只使用一次」。同键换参数要拒绝（「防客户端 bug 把旧键用在新操作上 这个也要」），防重和幂等「这两个也要」。11-05 先定「不要存盘，也不要太复杂」；11-06 问「如果存盘的话怎么弄最标准，能搞成一个配置存不存盘吗」，看过 §5.3 的取舍后定「改成存盘」，不做开关。
**关联:** [client-rpc-callback.md](client-rpc-callback.md)（§2.2：结果未知时复用业务幂等键重发）、[client-request-ordering.md](client-request-ordering.md)（§4.8：`gate_call_seq` 不能当幂等键；§5.2 I5：gate → scene 超时回 1003、不重发）、[data-overwrite-rollback-audit-r2-20261019.md](data-overwrite-rollback-audit-r2-20261019.md)（r2-equip#1）、[equipment-enhance-inlay.md](equipment-enhance-inlay.md)、[mystery-merchant.md](mystery-merchant.md)、[ecs-component-access-rules.md](ecs-component-access-rules.md)、[player-save-direct-commit.md](player-save-direct-commit.md)（存盘分行）、帮会经济的同类先例 `go/guild/internal/logic/economy_logic.go` 的 `withClientRequest`。

## 0. 结论

- 服务端分不清「玩家真想做两次」和「回包没到又发了一次」，只有发起方知道两条请求是不是同一次操作。所以由**客户端给每次操作生成一个键**，所有重发都带这个键，服务端按 (玩家, 键) 去重。
- 场景服给每名玩家记最近 32 个键，**作为玩家存档的一个字段**（`player_database.client_request_ledger = 25`），和它保护的货币、背包、限购记录同一次存盘提交。不建表、不写新 SQL、不进 Redis。崩溃回档、下线重登、跨节点迁移之后，带原键的重发照样认得出来。
- 同键同参数：不再执行，回首次结果（推不出来的部分存在账本里，其余取当前状态），并标 `replayed = true`。重复的请求不会执行（防重），客户端也知道第一次成了（幂等）。
- 同键换了参数或换了接口：回 `kInvalidParameter`，不执行。这一条防的是客户端缺陷把旧键用在新操作上。
- 账本缺失或加载校验不过：带键的请求回 `kServiceUnavailable`，不执行（fail-closed）；不带键的照常。
- 键为 0 表示旧客户端，行为与加字段之前完全相同。

## 1. 问题

- 客户端到 gate 是 TCP，gate 到 scene 的 RPC 不会自己重发；gate 等 12s 没回包就给客户端回 1003「结果未知」（`client-request-ordering.md` §5.2 I5）。第二条请求只可能来自客户端：玩家又点了一次，或者客户端按 `OutcomeUnknown` 重发。
- 信封里的 `ClientRequest.id` 按连接自增、每次发送换号、重连后从 1 重数；gate 的 `gate_call_seq` 按调用编号。两者都不代表「同一次操作」，不能当幂等键（`client-request-ordering.md` §4.8、`client-rpc-callback.md` 附录 A）。
- 场景服里会扣资源的客户端请求逐个核过（2026-11-05 代码）：

| 接口 | 扣什么 | 重复的第二条会怎样 | 结论 |
|---|---|---|---|
| `EnhanceEquip` | 金币、材料，掷骰 | 掷骰失败且不掉级时等级不变，同一个 `expected_level` 仍被受理：再扣一次、再掷一次 | 接入 |
| `BuyMysteryMerchantGoods` | 货币；发物；记限购 | 限购没满就再买一份 | 接入 |
| `CreateAttributeScheme` | 金币 | 方案数没到上限就再建一个、再扣一次 | 接入 |
| `RetrieveTemporaryItems` | 不扣（临时格搬回背包） | 物品已不在临时格，`kBagTemporaryItemNotFound`，两个包都不动 | 不接入（R2 那批新增，按设计天然防重） |
| `InlayGem` | 宝石 1 颗 | 孔已占用，拒绝 | 不接入：前置状态挡住 |
| `RemoveGem` | 免费 | 孔已空，拒绝 | 不接入 |
| `EquipItem` / `UnequipItem` | 不扣 | 物品已不在原处，拒绝 | 不接入 |
| `ResetAttributePoints` / `ResetPetPoints` | 金币 | 已没有可洗的点，`kAttributeNothingToChange` / `kPetNothingToChange` | 不接入 |
| `RenamePet` | 金币 | 同名不收钱，`kPetNothingToChange` | 不接入 |
| `UpgradeSkill` | 金币 | `expected_rank` 对不上，整笔拒绝 | 不接入 |
| `AllocateAttributePoints` / `AllocatePetPoints` | 不扣 | 传的是目标值，重放结果不变 | 不接入 |
| `ClaimMissionReward` | 发奖 | 领过之后不再可领，拒绝 | 不接入 |
| `ClaimActivityVitalityReward` | 发奖 | 自带持久 `request_id` 账本（`player_activity.cpp`） | 已有 |

「不接入」的几个靠前置状态做到了防重，但重复的那条拿到的是一个拒绝，不是首次结果。scene TCP 路径同一连接先进先出，首次的回包总先到，客户端正常不会被误导。要让这几个也做到幂等，按 §3.4 的接入顺序加，每个 handler 十几行，留作可选后续。

仓库目前没有「使用道具」的客户端协议（`bag_system.h:345` 只留了接入口的注释）。以后加的时候必须带 `client_request_id`，按 §3.4 接入。

## 2. 契约

### 2.1 客户端

1. 玩家每发起一次操作，生成一个非 0 的随机 64 位数作键。要用密码学或高质量随机源，不能用自增号，否则重装或换设备后会撞号。
2. 这次操作拿到**明确结果**之前，所有重发都带同一个键，其余字段原样不变。明确结果指成功，或带 tip 的拒绝。重发包括：超时、断线重连、gate 回 1003，以及回包没到时玩家又点了同一个按钮、点的还是同一个对象。
3. 拿到明确结果之后才换新键。之后再点就是新的一次操作。
4. 收到 `replayed = true`，按「首次那一次已经成功」处理：套用回包里的快照，不提示失败。
5. 同一个键收到 `kInvalidParameter`，说明客户端自己把旧键用在了新操作上，按缺陷记日志。
6. 服务端记录随存档走，重连之后、甚至换了节点之后带原键重发都是安全的，前提是这个键还在账本里：没有被同一玩家之后 32 次以上带键的操作挤出去（§2.2 第 7 条），账本也没有被人工清空（§5.2）。客户端第一版仍只在同一条连接上自动重发（§6），重连之后先刷新快照给玩家看，是为了少做一段跨连接保留待发请求的逻辑，不是因为服务端会忘。

### 2.2 服务端

| 业界规则 | 本实现 |
|---|---|
| 1. 键由发起方生成 | 客户端生成 `client_request_id`，服务端不发号；信封 id、`gate_call_seq` 不算 |
| 2. 按用户隔离 | 账本是玩家存档的字段，范围是 (玩家, 键) |
| 3. 存参数指纹，同键换参数拒绝 | 指纹 = 请求清掉键之后做确定性序列化，取 SHA-256 的前 16 字节；同时比较接口种类；不同就回 `kInvalidParameter`，不执行 |
| 4. 存首次结果 | 记录里存从当前状态推不出来的部分：强化的成败与等级、新建的方案号；背包、货架、面板取当前状态；应答标 `replayed` |
| 5. 同键并发不并行执行 | scene 单线程，同一玩家的请求逐条同步执行，首次执行完之前重发不可能开始，所以不需要「处理中」状态。接入点改成等下游再回包（`RpcDoneCallback::Adopt`）之前，必须先补上这个状态 |
| 6. 去重记录与效果一起提交 | 同一次同步调用里先生效、后记键，中间不让出 EventLoop；账本与货币、背包同在 `player_database`，同一次存盘提交（不变量 I3） |
| 7. 有保留期 | 每名玩家最近 32 条，满了淘汰 seq 最小的；客户端用随机 64 位键，保留期内不会重复 |
| 防重 | 同键同参数的重发不执行 |
| 幂等 | 重发拿到首次结果，并标 `replayed` |

## 3. 实现

### 3.1 存储

- 协议：`proto/common/component/client_request_ledger_comp.proto`。
  - `ClientRequestRecord { key, kind, fingerprint, seq, enhance_success, enhance_level, created_scheme_id }`。
  - `ClientRequestLedgerComp { records, next_seq }`。
- 存档字段：`player_database.client_request_ledger = 25 [(OptionPersistGroup) = 7]`，放 WALLET 组。带键的操作都会改货币，这一行本来就要写，不会让存盘多写一行。投影成行是按描述符通用做的（`player_store_projection.cpp`），不用另外登记；混版本台账由 proto 生成器发世代号。
- 组件类别是「持久数据」（`ecs-component-access-rules.md`）：
  - 唯一创建点是 `PlayerDatabaseMessageFieldsUnmarshal`：无条件挂上，原样拷入，不在加载时整理。然后跑 `client_request_ledger::Validate`，不过就挂运行时标签 `ClientRequestLedgerInvalidComp`。标签照 `PlayerInboxLedgerInvalidComp`：不阻断登录、不改写原数据。加载时也不自动清空：玩家正在上线，可能正有一次刚发出的重发，清空会让它再执行一次。人工修复见 §5.2。
  - `PlayerDatabaseMessageFieldsMarshal` 用 `FindPersistedComp` 取它，缺了就整份拒存，位置在 currency 那一对 `CopyFrom` / `SaveToProto` 之外。
  - 接入点只 `try_get`，任何路径都不懒建。
- 不用为它额外触发存盘：效果和记录在同一份快照里，哪一次存盘都是一起存，要丢也一起丢。

### 3.2 账本（`cpp/libs/services/scene/player/system/client_request_ledger.{h,cpp}`）

- 对 proto 的纯函数，放在 `namespace client_request_ledger`，照 `asset_op_ledger` 的写法：
  - `Check(ledger, key, kind, fingerprint, result)` 返回 `kNew`、`kReplay` 或 `kMismatch`。
  - `Remember(...)`：先淘汰到不足 32 条（按 seq，不按下标），再追加，seq 取 `next_seq`，旧存档的 `next_seq = 0` 按 1 处理。
  - `Validate(ledger)`：条数超过 256，键为 0 或重复，指纹不是 16 字节，seq 为 0、重复或不小于 `next_seq`，任一条成立都判坏。**不校验种类**：新版本写进来的种类原样携带，同一个键再来时按「种类不同」拒绝。256 这个上限比容量宽，将来下调容量时旧存档不会被判坏。
- `ClientRequestKind` 的值随存档落盘，**不可重排、不可复用**，删掉的种类留空号：1 强化、2 神秘商人购买、3 新建加点方案，0 不合法。最后一项是 `kCount`，名字表用 `static_assert` 对齐。
- 指纹算法随存档落盘，是契约，不能改：清键之后做确定性序列化（`SetSerializationDeterministic`），再取 SHA-256 的前 16 字节。序列化写法照 `service_call_auth::Digest`。请求以后加字段会自动算进指纹。

### 3.3 接入层（`namespace client_request_dedup`）

- `Check`：
  - 键为 0 时答 `kNew`。
  - 挂着失效标签，或组件不在（加载不变量被破坏），答 `kUnavailable`，并打 `metric=client_request_unavailable` 日志（`reason=ledger_invalid` / `ledger_missing`）。
  - 其余情况交给纯函数。命中打 `metric=client_request_replay`，冲突打 `metric=client_request_mismatch`。日志里有 player_id，它不当指标 label。
- `Remember`：键为 0 时什么都不做。组件不在或已判坏时打 ERROR、不写；正常走不到，因为 `Check` 已经按 `kUnavailable` 拦下了。

### 3.4 接入顺序（每个接入点相同）

1. 取 `key = request.client_request_id()`。为 0 时跳过第 2、4 步。
2. **在一切依赖状态的校验之前** `Check`。这些校验包括战斗中、冻结、余额、物品在不在、轮次对不对、乐观比较。首次已经成功，状态已经变了，重放要是先过这些校验，会被拒掉，客户端就以为第一次也失败了（帮会同理，见 `withClientRequest` 第 1 道）。
   - `kReplay`：按首次结果加当前状态组应答，标 `replayed`，直接返回。
   - `kMismatch`：回 `kInvalidParameter`，直接返回。
   - `kUnavailable`：回 `kServiceUnavailable`，直接返回。
3. 走原流程。
4. 原流程**返回成功之后、组应答之前** `Remember`。应答组不出来（快照建不出来）也照样记，因为操作已经生效了。

检查放在能看到整个请求的那一层：神秘商人的 `PlayerMysteryMerchantSystem::Buy` 本来就接收整个请求，放在系统里；强化和新建方案的系统函数只拿到拆开的参数，放在 handler 里。

### 3.5 只记成功

业务拒绝没有副作用，不记，同一个键重发会重新判定。提交段里本来就转人工处理的异常分支也不记：神秘商人的 `[UNPAID_GRANT]`，强化的 `[ENHANCE_PARTIAL]` 和 `[INSTANCE_LOST]`。这些分支返回错误码，客户端按明确结果处理，会换键；只有那条回包又恰好丢了，重发才会按新的一次操作判。这和没有去重时的行为一样，也受限购等前置约束。

### 3.6 体积

一条记录约 40 字节（键 9、种类 2、指纹 18、seq 约 3、结果约 4，另加字段头）。每名玩家最多 32 条，所以 WALLET 行最多多约 1.3KB。只有执行过带键请求的玩家才有记录。

## 4. 接入清单

| 接入点 | 位置 | 首次结果 | 状态 |
|---|---|---|---|
| 神秘商人购买 | `PlayerMysteryMerchantSystem::Buy`：入口在 `valid(player)` 之后查账；成交后 `Remember`；成功组应答抽成 `FillSuccessReply`，供首次与重放共用 | 无（货架与背包取当前） | 2026-11-08 单测通过；robot 冒烟未跑 |
| 新建加点方案 | `SceneAttributeClientPlayerHandler::CreateAttributeScheme` | `created_scheme_id` | 2026-11-08 单测通过；robot 冒烟未跑 |
| 强化 | `SceneBagClientPlayerHandler::EnhanceEquip`：先查键，命中就重放；没命中才进 `PlayerEquipSystem::Enhance`（里面有 `expected_level` 乐观比较）。指纹含 `item_id` 与 `expected_level` | `enhance_success`、`enhance_level` | 2026-11-08 单测通过；robot 冒烟未跑 |

协议字段：

- `BuyMysteryMerchantGoodsRequest.client_request_id = 8`，`BuyMysteryMerchantGoodsResponse.replayed = 4`。
- `CreateAttributeSchemeRequest.client_request_id = 2`，`CreateAttributeSchemeResponse.replayed = 4`。
- `EnhanceEquipRequest.client_request_id = 3`（2 是 R2 会话的 `expected_level`），`EnhanceEquipResponse.replayed = 6`。

项目未上线，新加的字段不涉及存量兼容。

强化同时有两道保护，各管一段：
- 幂等键：管「同一次操作的重发」。
- `expected_level`：管「客户端手里的等级过期了」，以及被挤出账本的旧键。

## 5. 已知窗口与取舍

### 5.1 改成存盘之后关掉的窗口

11-05 的内存版在三种情况下会把同一个键当成新操作：玩家下线重登，跨节点迁移，scene 进程重启或崩溃。崩溃时还有一个更隐蔽的窗口：首次效果已经随上一次存盘落库，键却丢了。改成存盘之后，记录和效果同生同死：

- 正常存盘：两者一起写进库。
- 崩溃回档：两者一起退回到上一次存盘的时刻。如果首次效果没落库，重发再执行一次是对的；如果落库了，记录也在，重发会被识别为重放。

### 5.2 仍然存在的窗口

- **被挤出账本**：一名玩家在一次操作结果未知期间，又成功执行了 32 次以上带键的操作，最早那个键会被淘汰。它的重发会被按新操作判，退化成没有去重时的行为，不会凭空多发东西。强化另有 `expected_level` 兜着。
- **提交段的异常分支**（§3.5）：本来就转人工处理，不记键。
- **账本判坏**：这名玩家带键的强化、神秘商人购买、新建方案一律回 `kServiceUnavailable`、不执行，其余玩法和登录不受影响，直到修复。
  - 信号：加载时打一条 ERROR `[ClientRequest] metric=client_request_ledger_invalid source=loader`，带 `player_id` 与 `reason=`；之后每次带键请求打一条 WARN `metric=client_request_unavailable reason=ledger_invalid`。Loki ruler 接线之前，值班固定查询（与收件箱同一个流选择器，见 `deploy/k8s/player-inbox-alerts.yaml` 文件头）：`count_over_time({job="cpp_nodes", service="scene"} |= "[ClientRequest]" |= "metric=client_request_ledger_invalid" [10m]) > 0`。
  - `reason=ledger_missing` 不是数据问题：持久组件没挂上，是加载代码的缺陷，修代码，不动存档。
  - 处置：账本只由本功能写入，判坏只可能来自缺陷，先留存原账本写进工单，再修缺陷。项目上线前玩家数据可以整体清空，不需要专门工具；上线后的写回手段由负责人定，写回的内容按下面的要求。
  - **整体清空不是无条件安全的**（2026-11-13 改正，原文写的是「玩家离线时清空是安全的」）。账本里每一条记录都代表一次已经生效的操作（只记成功，§3.5）。清空会丢掉这名玩家全部的去重记录，之后任何一次带旧键的重发都会被当成新操作：再扣一次、再买一份、再建一个方案。
  - **修复只修坏处，不删合法的键**。按 `Validate` 判坏的几种原因逐条处理，修完必须能过 `Validate`：
    - 键为 0 的记录删掉。键为 0 的请求不查账本，这种记录对不上任何请求，删了不丢东西。
    - 同一个键出现多次时，只留 seq 最大的一条。留下一条，带这个键的重发就只会被重放或按「换了参数」拒绝，不会再执行。
    - 指纹不是 16 字节的记录不删：键留着，指纹改成 16 个零字节。这个键再来时按「换了参数」回 `kInvalidParameter`，不会再执行；删掉它，这个键的重发就会再执行一次。
    - 条数超过 256 时，只留 seq 最大的 32 条，与正常淘汰留下的一样。
    - seq 为 0、重复或不小于 `next_seq` 时，按原来的 seq 排序（相同的按数组下标）重排成 1..n，`next_seq = n + 1`。
  - 整体清空只在两个条件同时满足时才可以用：玩家离线；能登录这名玩家的客户端都只在同一条连接上重发。Unity 第一版（2026-11-12）满足后一条：结果不明时在同一条连接上原样重发一次，断线或换了连接就不再重发。两个条件都成立时没有在途的重发，清空不会让已生效的操作再执行；「离线」判断不准、清空那一刻恰好有一次重发时，那一次会被当成新操作执行。robot 的神秘商人冒烟会重登之后带原键重发（§8 第 6 步），只用测试账号，不对它做修复。客户端一旦做了 §6 的跨连接、跨 App 重启恢复，整体清空就不能再用：客户端可能拿着一个待确认的请求，过几天重新登录时才带原键重发，清空之后这次重发会再执行一次。
  - 这一点和收件箱账本不同：收件箱的条目会被投递方长期重投，那边任何情况下都禁止清空（`player-inbox-runbook.md` §10.4）。

### 5.3 为什么不做成开关（负责人 2026-11-06 定）

开关能做：启动时读一个配置项，关掉时这个字段写空、加载时忽略。两个方向切换都安全，账本只起保护作用，切换不会改坏任何数据。不做的理由：
- 去重是正确性功能，正确性不该取决于配置。
- 两种模式都得测、都得讲清楚。
- 关掉只省约 1KB，却把 §5.1 的窗口放了回来。
- 项目没上线，也不需要上线用的急停开关。

### 5.4 客户端缺陷导致的同参数复用

客户端把旧键用在一次**参数完全相同**的新操作上，服务端分辨不出来，会按重放处理：这一次什么都不做，回 `replayed`。比如对同一件装备再强化一次，`expected_level` 又恰好没变。这正是第 3 条规则「每次操作一个新键」要防的事，只能靠客户端遵守契约。

## 6. 客户端（Unity）

2026-11-06 负责人授权出细则交 Codex 实施，细则见 [交接文档](../handoff/client-request-idempotency-unity-20261106.md)，那份文档写明了改哪些文件、哪几行、用例和验证命令。规则摘要：

- 三个调用点原本就有单飞：回包没到时再点不发第二个包。这一条保留；不为第二次点击生成新键。
- 每次点击（本地检查都过了之后）生成一个新键，写进请求。生成方法与帮会经济共用一个实现（`ClientRequestKeys.TryCreate`，照 `GuildClient.EconomyRequestId`）。
- 结果不明（`rpc timeout` 或信封 1003）、连接没变、还没重发过时：原样重发一次（同一个键、其余字段不变）。重发也不明，就走各调用点现有的「结果未知」收尾。
- 断线、连接换了：第一版不重发，重拉快照，让玩家看过结果再决定。服务端记录随存档走，以后要做跨连接、跨 App 重启的自动恢复是安全的：照帮会用 `DurableRequestFileStore` 在本地记下待确认的请求，重连后带原键重发。这是可选后续，不在第一版。做这个后续时有两个前提：待确认的请求在本地保留期间，同一玩家带键的操作不能超过账本容量（32 条，§2.2 第 7 条），否则那个键会被挤出去；§5.2 的人工修复不能再用整体清空，只能用逐条修复。
- `replayed = true` 按成功处理：它就是首次的结果，玩家之前没看到过。
- 强化请求同时要填 `expected_level`（`equipment-enhance-inlay.md` §6.1），否则强化过一次的装备会一直被拒。

## 7. Java 版（AGENTS §12）

改的是客户端可见协议，Java 版要对齐：`game-proto` 同步这几个字段，Java scene 的神秘商人购买、新建加点方案、强化按同一顺序接入。账本放进 Java 的玩家状态，和货币同一次围栏写。状态：**Java 待做**，登记在 Java 仓 `PARITY.md`。

## 8. 验证（交 Codex）

1. 前提：R2 会话那一批在 `data/tip/Tip.xlsx` 新加的码（`EquipEnhanceLevelChanged` 和三个临时格的码）要先导表，强化用例用到 `kEquipEnhanceLevelChanged`。导表按 `AGENTS.md` §4 第 4 条。
2. 协议：在仓库根执行 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。客户端未授权，所以用 no-unity 配置。生成之后核对以下几处：
   - `cpp/generated/proto/proto.vcxproj` 里有新的 `client_request_ledger_comp.pb.cc`。生成器会维护这个工程，不要手改。
   - `proto/persist_schema_gen.txt` 登记了字段 25 和新消息的字段。
3. 编译：按 `AGENTS.md` §10.1，`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。改了 scene 库，测试工程链接的是预编好的 `scene.lib`，要先编库再编测试。
4. 单测：
   - `bag_test.exe --gtest_filter=ClientRequestLedgerTest.*:ClientRequestDedupTest.*:ClientRequestAttributeSchemeTest.*:PlayerMysteryMerchantTest.*:PlayerEquipTest.Enhance*:PersistRoundTripTest.*:PlayerFeaturePersistenceTest.MarshalRefusesWhenAnyPersistedComponentIsMissing`。
   - 强化的 4 条新用例：`EnhanceResendWithTheSameClientKey*`（2 条）、`EnhanceWithAClientKeyReusedForAnotherOperationIsRefused`、`EnhanceRefusedForGoldDoesNotRememberTheClientKey`。
5. 回归：`bag_test.exe` 全量；`cross_zone_test.exe` 全量（它的手搭玩家补了新组件）。
6. 冒烟：三个冒烟场景（强化、神秘商人、新建方案）2026-11-13 已按[交接文档](../handoff/client-request-idempotency-unity-20261106.md) §8 改完：每个都加了「同一个键原样重发一次」，神秘商人另加了「重登后带原键重发」。Codex 编译 robot 并在 dev 环境里跑，命令见 `docs/PROGRESS.md` 2026-11-13 条目。项目未上线，跑之前清空玩家数据：新建方案那一步要求账号还没开满方案。
7. 通过标准：全绿；三个冒烟的退出码都是 0。失败时保留第一个失败用例名和断言原文，不改断言、不连续重试。

## 9. 变更记录

- 2026-11-05：初稿，只放内存。落码神秘商人购买、新建加点方案；强化待接。
- 2026-11-06：
  - 强化接入（`client_request_id = 3`、`replayed = 6`），接在 R2 会话的 `expected_level = 2` 之上。
  - 工程登记：场景库加账本两个文件，bag_test 加 `client_request_ledger_test.cpp`。Linux 的 `cpp/libs/services/scene/CMakeLists.txt` 由 R2 会话补了一行。这个文件由 `build_linux.sh` 第 2 步按 `.vcxproj` 重新生成，`scene.vcxproj` 的条目已挪到同一位置，重新生成不会产生差异。
  - 负责人授权出 Unity 细则交 Codex（`docs/handoff/client-request-idempotency-unity-20261106.md`）。
  - 负责人定「改成存盘」：账本改为 `player_database` 字段 25（WALLET 组），组件改为持久数据，指纹改为 SHA-256 前 16 字节，淘汰改按显式 seq，加载校验与失效标签照收件箱账本。不做开关（§5.3）。
  - 夹具同步：满套持久组件的手搭玩家（`bag_test.cpp`、`cross_zone_test.cpp`、两个 `EmplaceIfAbsent` 补齐函数）都挂上新组件；「缺组件整份拒存」清单与存盘往返用例都覆盖到它。
  - `player_database_1` 由「玩家数据存储策略优化」会话同期删除，与本改动互不影响；它会把字段 25 补进 `player-save-direct-commit.md` §1.1 的钱包行。
- 2026-11-13：
  - robot 三个冒烟场景按交接 §8 补上同键重发，神秘商人另补重登后重发（§8 第 6 步）。未编译、未运行。
  - §5.2 改正：原文说「玩家离线时清空这个字段是安全的」，这个说法过宽。清空会丢掉全部去重记录，带旧键的重发会再执行一次，只在两个条件同时满足时才不出事（玩家离线；客户端只在同一条连接上重发）。修复改为默认逐条修、不删合法的键。§2.1 第 6 条补上保留期前提；§6 的跨连接恢复补上它对 §5.2 的约束。
  - §4 的状态列按 2026-11-08 的单测结果更新。
