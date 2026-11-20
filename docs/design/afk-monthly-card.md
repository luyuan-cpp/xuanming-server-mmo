# 挂机月卡:获得、开通与权益落库

> 2026-11-19 定稿;同日负责人确认「按稿开工」,当天落码。**协议未生成、xlsx 未产出、未编译、未测试**,交 Codex 的命令在
> `docs/PROGRESS.md` 2026-11-19「挂机月卡开通入口」。实际改动见 §10(比定稿时估的 38 个多,多出来的是工程登记、单测与客户端 `.meta`)。
> 起因:离线托管(断线后角色常驻、跟着队长换场景,[断线保留](player-connection-lost-hold.md) §4.1~§4.5)只认
> 玩家域 Redis 键 `player:afk_pass:{<player_id>}`,而仓库里没有任何写入方,真实玩家走不到,也写不了自动冒烟
> (PROGRESS 2026-11-19「组队跟随合并广播的发送层补单测」的核对结论)。

## 0. 结论

- **月卡是角色身上的一个到期时刻**,不是物品。事实源在存档库:`player_database.uint64_pb_component.afk_pass_expires_at`
  (`PlayerUint64Comp` 新增字段 2,PROFILE 行)。scene 改它,随正常存盘提交;别的服务不写。
- **获得只有一个写入口**:玩家花灵石开通(`BuyAfkPass`)。到期 = max(现在, 原到期) + 30 天,可叠加,剩余最多 180 天。
  每次开通带客户端幂等键,同一个键只扣一次钱、只加一次天数。
- **真钱**:不单独卖「月卡商品」,玩家用现有充值档位买灵石再开通(30 元档正好 300 灵石 = 一次开通,§4.2)。
  订单只生效一次由现有充值链路保证。
- **GM**:`GmSetAfkPass(剩余秒数)`,只在 dev / test 放行,给联调和 robot 用。
- **Redis 键只是投影**:scene 在存档提交之后写 `SET player:afk_pass:{id} <到期秒> EXAT <到期秒>`,
  加载时按库里的值重写一次,托管期间随在场心跳每 20 秒补写一次。投影丢了,下一次加载或心跳从库里补回。
- **读方**:scene 改为读自己内存里的那份(它就是这份数据的持有者),不再 GET Redis;
  player_locator 读不到存档,继续读投影,代码不改。

## 1. 现状(2026-11-19 核实)

| 事实 | 位置 |
|---|---|
| scene 断线保留时异步 GET 月卡键,按值判托管 | `cpp/libs/services/scene/player/system/afk_hosting.cpp` `Probe` / `ApplyProbeResult` |
| 留人 / 退出的纯判定、5 秒提前退出余量、查不出来时的宽限 | `connection_lost_hold.h` `ParseAfkPass` / `JudgeHold` / `IsHosting` |
| player_locator 续断线租约、托管归位都读同一个键:没有键 = 无月卡,读失败 = 推迟 10 秒再判 | `go/player_locator/internal/logic/leasemonitor.go` `afkPassState`;`hosted_resume.go` |
| 键形 `player:afk_pass:{<player_id>}`,值是十进制到期 Unix 秒 | `go/shared/redisslot/player_keys.go` `AfkPassKey`;C++ `afk_hosting.cpp` 同形 |
| 没有任何写入方;支付设计把月卡列为 v1 非目标 | 全仓检索;`payment-recharge.md` §1 |
| 存档提交回调拿得到「这一笔提交的整份快照」 | `PlayerLifecycleSystem::OnPlayerStoreSaveDone`(被取代的快照直接返回,结局归取代它的那份) |
| 客户端请求幂等账本随存档落盘,已有 5 种请求 | `client_request_ledger.h`;[幂等键设计](client-request-idempotency.md) |
| 客户端 GM 指令:方法名 `Gm*`,gate 按消息号清单关、scene 按方法名再关,只在 dev / test 放行 | `gate_gm_client_messages.h`;`player_gm_guard.h` |
| 没有「使用道具」协议,没有游戏内货币商城;商城只卖真钱官方商品,发放物只有物品、货币两种 | `client-request-idempotency.md` §1;`payment-recharge.md` §22.7 |

## 2. 目标与非目标

目标:

1. 玩家在游戏里能开通、续费挂机月卡,看得到剩余时长,快到期有提醒。
2. 权益事实在 TiDB,和扣的灵石同一次存盘提交:要么都在,要么都回档。
3. 同一次开通请求不论重发多少次只生效一次;崩溃回档、重登、换节点之后仍然成立。
4. robot 能端到端验证「开通 → 断线 → 队长换场景(同节点、跨节点各一次)→ 托管队员跟过去并继续沿路跟」。

非目标(§12 写了原因):真钱直购月卡商品、月卡道具、自动续费 / 订阅、月卡每日奖励、GM 后台网页接入、Java 版代码。

## 3. 月卡是什么:一个到期时刻

### 3.1 字段

```proto
// proto/common/component/player_comp.proto
message PlayerUint64Comp
{
	uint64 registration_timestamp = 1;
	// 挂机月卡到期的 Unix 秒(docs/design/afk-monthly-card.md)。0 = 从未开通;<= 现在 = 已到期。
	// 唯一写入方 AfkPassSystem(灵石开通、GM);其余代码只读。
	uint64 afk_pass_expires_at = 2;
}
```

- **归属**:存档库的 `player_store` 表(PROFILE 行),表的唯一读写方是 go/db 的 playerstore;scene 持有玩家期间经
  SavePlayer 直接提交([存盘直提](player-save-direct-commit.md))。没有新表、没有新 SQL,行投影按描述符通用处理。
- **为什么放进 `PlayerUint64Comp`**:它本来就是 PROFILE 行里放角色标量的持久组件,加载无条件挂上、存盘缺它整份拒存。
  新开一个顶层字段要多改加载、存盘、所有存盘夹具,换来的只是名字更好看。
- **和扣钱同一次提交**:灵石在 WALLET 行,月卡在 PROFILE 行,一次存盘的所有脏行同事务、同一个整笔序号
  ([存盘直提](player-save-direct-commit.md) §1.1)。崩溃时两者一起回档,不会出现「钱扣了天数没加」或反过来。
- **混版本**:不认识字段 2 的旧 scene 把它当未知字段原样带回(组件整份 `CopyFrom`),不会抹掉。世代台账由 proto 生成器追加。
- **组件类别**:持久数据。读路径只读;写入口只有 `AfkPassSystem`(§4)。

### 3.2 为什么不做成物品

物品方案能让真钱商品、邮件、GM 发物直接复用,但仓库没有「使用道具」协议,要新建一整条用道具链路(配置、预留闸、背包出包入口、
撤销),而月卡本身只需要一个到期时刻。以后要发「月卡兑换券」时,加一个消耗兑换券、调同一个开通函数的入口即可,本稿的事实源不变。

## 4. 获得与开通

### 4.1 灵石开通:`BuyAfkPass`

请求带 `client_request_id`(必须非 0)、`expected_currency_type`、`expected_price`(报价防呆,同神秘商人)。
scene 单线程同步处理,顺序固定:

1. **查幂等键**(排在一切依赖状态的校验之前,[幂等键设计](client-request-idempotency.md) §3.4):
   新种类 `ClientRequestKind::kBuyAfkPass = 6`。重放 → 不再执行,回当前月卡信息并标 `replayed`;同键换参数 → `kInvalidParameter`;
   账本缺失或判坏 → `kServiceUnavailable`;键为 0 → `kInvalidParameter`(新接口没有旧客户端)。
2. 规则表不可用(缺行或取值越界)→ `kAfkPassUnavailable`。
3. 报价对不上(价格、币种与规则表当前值不同,配置热更过)→ `kAfkPassPriceChanged`,客户端刷新后再买。
4. 冻结中(跨区传送、交易封存)→ `kAfkPassFrozen`。战斗中不拦:开通不碰战斗快照。
5. 算新到期 `next = max(now, old) + period`;`next - now > max_remaining` → `kAfkPassStackLimit`。
6. 余额不够 → `kAfkPassCurrencyNotEnough`(服务端裁决,客户端不拿本地余额拦)。
7. **扣灵石** `DeductCurrency(type, price, TX_AFK_PASS_BUY = 32, correlation = client_request_id)`;失败 → `kAfkPassUnavailable`,
   什么都没改(第 4、6 步已排除它会失败的条件,走到这里是缺陷)。
8. **写到期时刻**(内存赋值,不会失败)。先扣钱后加天数:后一步不会失败,所以不需要退款路径。
9. 记幂等键(只记成功,§3.5);请求一次关键进度存盘(1 秒节流),投影随这次提交写出(§5.2)。
10. 回包:当前月卡信息。

第 7~9 步在同一次同步调用里,中间不让出 EventLoop。

### 4.2 真钱:走现有充值档位

不做「月卡商品」。理由:

- 商城的发放物只有物品、货币两种(`payment-recharge.md` §22.7)。月卡做成第三种发放物,要同时改商城存储合同与校验、
  trade 的合同摘要与收敛、资产通道的发放结构,以及撤销扣回(`payment-recharge.md` §22.6 现在只覆盖货币,权益扣回没有定义)。
- 买灵石再开通,撤销规则不用新增:付款被撤销时 trade 照常扣回灵石,扣不到(已花在月卡上)按现有规则转人工、不透支。
- 开通价默认 300 灵石,正好是 30 元档的灵石数,客户端在灵石不足时提示去充值。

订单只生效一次:由现有充值链路保证(一张订单一笔发放,`payment-recharge.md` §0)。开通只生效一次:§4.1 第 1 步。

### 4.3 GM:`GmSetAfkPass`

- 参数 `remaining_seconds`:把到期设为 `now + remaining_seconds`;0 = 清除(到期设 0)。上限 366 天,超过回 `kInvalidParameter`。
- 「设定」而不是「增加」:联调要能精确造出「还剩 2 分钟」来看提醒与到期退出;重放结果不变,不需要幂等键。
- 只在 dev / test 放行:登记进 gate 的 GM 消息清单,scene 侧 `RejectGmClientRpc` 再拦一次(`player_gm_guard.h` 两道锁)。
- 改完同样请求一次关键进度存盘,投影随提交写出。不写流水:GM 改的不是资产,日志行 `metric=afk_pass_gm_set` 带前后值。
- GM 后台网页(`GmExecutePlayerCommand`)不接,见 §12。

### 4.4 规则表 `AfkPassRule`(单行,与 PetRule / MysteryMerchantRule 同套路)

| 列 | 缺省 | 取值范围 | 谁用 |
|---|---|---|---|
| `id` | 1 | 固定 1 | — |
| `period_days` | 30 | 1..366 | 开通一次加多少天 |
| `price_currency_type` | 1(灵石) | 已知的 `CurrencyType` | 开通扣什么 |
| `price` | 300 | 1..10^9 | 开通一次扣多少 |
| `max_remaining_days` | 180 | period_days..3660 | 叠加上限 |
| `remind_before_hours` | 72 | 0..720 | 客户端快到期提醒(经应答下发,客户端不读表) |

取值不合法时 scene 打 ERROR,开通回 `kAfkPassUnavailable`,`AfkPassInfo.price` 回 0(客户端显示「暂不出售」);GM 与托管不受影响。
xlsx 与 Tip 码由脚本 `tools/scripts/afk_pass_xlsx_patch.py` 生成(同 `mystery_merchant_xlsx_patch.py`:只写空格、写后逐格核对、幂等),交 Codex 执行。

### 4.5 Tip 码(新段 `//afk_pass_error base=32000 width=1000`)

| 码 | 文案 | fault |
|---|---|---|
| `kAfkPassPriceChanged` | 挂机月卡价格已变化,请刷新后再试 | |
| `kAfkPassCurrencyNotEnough` | 灵石不足,无法开通挂机月卡 | |
| `kAfkPassStackLimit` | 挂机月卡剩余时长已达上限,暂不能续费 | |
| `kAfkPassFrozen` | 角色当前状态不能开通挂机月卡,请稍后再试 | |
| `kAfkPassUnavailable` | 挂机月卡暂时无法开通 | 1 |

32000 已被占用时脚本报错停下,换下一个空段并同步本表(31000 是支付段)。

## 5. Redis 投影

### 5.1 键与值

`player:afk_pass:{<player_id>}`(键形不变),值 = 十进制到期 Unix 秒。到期在将来:`SET key v EXAT v`(到期自动消失,
K8s 的 Redis 7.2 与本地 8.x 都支持);到期为 0 或已过:`DEL key`。只有 scene 写。

### 5.2 写入时机

1. **存档提交之后**:`OnPlayerStoreSaveDone` 的非拒绝、非被取代分支,拿这一笔提交的快照里的到期值,与本实体记着的
   「上一次已提交的值」(运行时派生组件 `AfkPassCommittedComp`)比较,不同才写。被取代的快照不写,结局归取代它的那份;
   同一玩家同时只有一笔在途存盘,所以回调按提交顺序到达,投影不会倒退。
2. **加载之后**:`InitPlayerFromAllData` 进场成功后,按刚从库里读出的值写一次(SET 或 DEL),同时种下 `AfkPassCommittedComp`。
   这就是「从 TiDB 重建」:回档、投影写丢、Redis 故障转移丢了写入,下一次有节点载入这名玩家时都会改正。
3. **托管期间**:`RefreshHostedPresence` 每 20 秒给托管着的角色续在场心跳时,顺带按 `AfkPassCommittedComp` 补写一次 SET。
   托管期间 player_locator 每 300 秒看一次投影;Redis 丢了这个键,20 秒内补回,不会把付费玩家误清出世界。

写失败只打日志,不重试:第 1 条的下一次变化、第 2 条的下一次加载、第 3 条的下一个心跳周期都会重写。
三处写的都是**已提交**的值,不写内存里还没落盘的值,所以投影永远不会领先于库。

### 5.3 读方核对

| 读方 | 改不改 | 说明 |
|---|---|---|
| scene `AfkHostingSystem` | **改**:读本实体 `PlayerUint64Comp` | scene 就是这份数据的持有者,内存里那份是加载时从库里来、此后只由它自己改的。读自己写的投影,投影一丢就会把有月卡的人当成没有。`Probe` 读内存,但判定排到本轮之后执行(`queueInLoop`),与原来等 Redis 回包同一个时序:断线处理、托管落点入场(EnterScene 第 3.0 步)当中不会当场开启跟随。`ApplyProbeResult` 仍是判定入口(排队那一步与单测共用),判定函数不变;查询失败的分支在生产中不再出现,纯函数保留原样(完整的判定定义,单测照跑)。 |
| player_locator 租约续期与托管归位 | 不改 | 它读不到存档(逻辑库自治,不许跨库),投影正是给它用的。语义不变:没有键 = 无月卡;读失败 = 推迟 10 秒再判。 |

时序([断线保留](player-connection-lost-hold.md) §4.1「时序为什么成立」)依赖两边看到同一个到期值:scene 在到期前 5 秒退出,
player_locator 在看到月卡失效之后才让租约到期。改读法之后两边只在「内存已改、提交还没回来」的那一两秒里不同,而且只会是
scene 的值更新(更晚到期)。离线期间能改到期值的只有 GM,所以这个窗口只在「GM 恰好在旧到期前 5 秒内续期」时出现,
后果是走「保留拖过租约」的慢路径(ReleasePlayer 收尾,多等一个 claim 超时),不丢数据。

### 5.4 故障表

| 故障 | 结果 | 恢复 |
|---|---|---|
| 开通后、提交前节点崩溃 | 灵石和天数一起回档到上一次存盘;投影没写过 | 无需恢复;客户端按结果未知处理,重登后看面板 |
| 提交成功、投影写失败 | 玩家在线时无影响;断线后 player_locator 可能按无月卡清理 | 托管心跳 20 秒内补写;来不及的,下一次登录加载时重写 |
| Redis 丢了投影(故障转移) | 同上 | 同上 |
| 数据回档(运维工具) | 投影可能比库里新 | 下一次加载按库里的值重写(含 DEL) |
| 规则表坏了 | 不能开通,托管照常 | 修表热更 |

## 6. 协议(`proto/scene/player_afk_pass.proto`,新服务 `SceneAfkPassClientPlayer`)

```proto
message AfkPassInfo
{
	uint64 expires_at = 1;          // 到期 Unix 秒;0 = 从未开通或已清除;<= server_now = 已到期
	uint64 server_now = 2;          // 服务器墙钟 Unix 秒:客户端用它算剩余时长,不信本机时钟
	uint32 price_currency_type = 3; // CurrencyType
	uint64 price = 4;               // 开通一次的价格;0 = 暂不出售(规则表不可用)
	uint32 period_days = 5;
	uint32 max_remaining_days = 6;
	uint32 remind_before_hours = 7;
	uint64 balance = 8;             // 玩家此刻持有的 price_currency_type 余额,只供展示
}
message GetAfkPassRequest {}
message GetAfkPassResponse { TipInfoMessage error_message = 1; AfkPassInfo info = 2; }
message BuyAfkPassRequest
{
	uint64 client_request_id = 1;   // 必须非 0;同一次操作的重发带同一个键
	uint32 expected_currency_type = 2;
	uint64 expected_price = 3;
}
message BuyAfkPassResponse { TipInfoMessage error_message = 1; AfkPassInfo info = 2; bool replayed = 3; }
message GmSetAfkPassRequest { uint32 remaining_seconds = 1; }
message GmSetAfkPassResponse { TipInfoMessage error_message = 1; AfkPassInfo info = 2; }

service SceneAfkPassClientPlayer
{
	option (OptionIsPlayerService) = true;
	option (OptionIsClientProtocolService) = true;
	rpc GetAfkPass (GetAfkPassRequest) returns (GetAfkPassResponse);
	rpc BuyAfkPass (BuyAfkPassRequest) returns (BuyAfkPassResponse);
	rpc GmSetAfkPass (GmSetAfkPassRequest) returns (GmSetAfkPassResponse);
	// ── 以下只许在末尾追加:生成的 CallMethod 按 method index 分发 ──
}
```

另加 `TransactionType.TX_AFK_PASS_BUY = 32`(开通扣钱的流水,`correlation_id` = 幂等键)。客户端可见的只有这个服务与 5 个 Tip 码,
Java 版要同批对齐(§11)。

## 7. 客户端(`../mmorpg-client`)

- **网络层** `Assets/Scripts/Game/AfkPass/AfkPassClient.cs`:照 `MysteryMerchantClient`(消息号来自生成的 `MessageIds`、连接世代隔离、
  单飞与连点保护)。开通按 [幂等键设计](client-request-idempotency.md) §2.1:每次点击生成新键,结果不明且连接没变时原样重发一次,
  断线后不重发、重拉一次 `GetAfkPass`。剩余时长 = `expires_at - server_now - 自收到应答起的本地单调时长`。
- **界面**:主界面加「挂机月卡」入口按钮,打开面板显示状态(未开通 / 剩余 X 天 Y 小时 / 已到期)、价格(300 灵石 / 30 天)、
  持有灵石、叠加上限说明和「开通 / 续费」按钮。灵石不足时提示去聚宝斋充值。价格为 0 显示「暂不出售」、按钮置灰。
- **快到期提醒**:登录进场后查一次;剩余在 (0, `remind_before_hours`] 内弹一次提示(每次登录一次);在线期间到期时弹一次「已到期」。
- **视觉**:没有效果图,按客户端 `AGENTS.md`「UI 原图还原验收」只做功能版,视觉验收列为未解决项,不宣称与原图一致。
- EditMode 单测:剩余时长计算、提醒判定、重发规则。生成协议由 Codex 用默认(含 Unity)配置跑。

## 8. robot 冒烟:`afk-pass-follow-smoke`

账号 `robot_9307`(队长 A)、`robot_9308`(队员 B)。写法照 `team_follow_smoke_scenario.go`(所有下行按到达顺序留底,每步动作前记下标)。

| 步 | 做什么 | 断言 |
|---|---|---|
| P0 | 两人登录,清残留队伍;B `GmSetAfkPass(0)` 清月卡 | — |
| P1 | B `GmAddCurrency(灵石, price)` → `GetAfkPass` → `BuyAfkPass(键 K)`;再用 K 重发一次;最后 `GmSetAfkPass(1200)` 把剩余缩到 20 分钟 | 第一次到期 ≈ now + 30 天;重发 `replayed = true`、到期与余额不变。缩短是为了中途失败时,留在世界里的托管角色最晚 20 分钟后自然退出 |
| P2 | A 建队、B 入队 | B 被带到 A 身边并开始跟随(同 team-follow-smoke F2) |
| P3 | B 关 TCP 断开 | 40 秒内 A 不收到 B 的 ActorDestroy(普通保留 25 秒就会退出,留着说明托管生效) |
| P4 | A 走一段 | A 收到 B 的移动广播,B 停在 A 身后约 3 米 |
| P5 | A 依次切到目录里的其它分线,直到同节点、跨节点各成功一次 | 每次切完 15 秒内 A 在新场景看到 B(ActorCreate guid = B),A 再走一段 B 照样跟 |
| P6 | B 重登 | 进场后在 A 身边;`GetAfkPass` 到期与 P1 设定的(缩短后的)一致 |
| P7 | 清理:B `GmSetAfkPass(0)`、离队,A 解散 | — |

同节点 / 跨节点的判法:客户端 actor 号的高 32 位是 scene 的 node_id(`actor_net_id.h`),A 切线前后比较自己 ActorCreate 的
`entity >> 32`。这是冒烟工具对服务端编码的白盒依赖,只写在 robot 里,客户端不用。环境里只有一个 scene 节点或目录里没有
另一节点上的分线时,P5 报 `AFK_PASS_FOLLOW_SMOKE_FAIL step=P5 reason=...` 并说明要起两个 scene 节点、给该地图配至少 2 条分线。
结果行 `AFK_PASS_FOLLOW_SMOKE_OK ...` / `AFK_PASS_FOLLOW_SMOKE_FAIL step=... reason=...`,退出码 0 / 1。

## 9. 不变量

1. 到期时刻只由 `AfkPassSystem` 写:灵石开通与 GM。
2. 扣钱与加天数在同一次同步调用里,随同一次存盘提交。
3. 同一个幂等键至多扣一次钱。
4. 投影只写已提交的值,不领先于库。
5. 托管判定用 scene 内存里的值;player_locator 用投影。

## 10. 改动清单(2026-11-19 落码,共 53 个文件;生成产物与 xlsx 另算)

| 部分 | 文件 |
|---|---|
| 协议与配置(5) | `proto/common/component/player_comp.proto`(字段 2)、`proto/scene/player_afk_pass.proto`(新)、`proto/common/rollback/transaction_log.proto`(`TX_AFK_PASS_BUY = 32`)、`data/schema/afkpassrule_table.proto`(新)、`tools/scripts/afk_pass_xlsx_patch.py`(新,产出 `data/AfkPassRule.xlsx` 与 `data/tip/Tip.xlsx` 的 5 行) |
| scene 逻辑(9) | `player/system/afk_pass_rules.h`(新,纯规则)、`afk_pass.{h,cpp}`(新,含派生组件 `AfkPassCommittedComp`)、`client_request_ledger.h`(种类 6)、`afk_hosting.{h,cpp}`(改读内存)、`connection_lost_hold.h`(注释)、`player_lifecycle.cpp`(加载、提交回调、托管心跳三处挂钩)、`player/comp/player_ownership_comp.h`(`AfkPassRedisKey`) |
| scene 节点(2) | `handler/rpc/player/player_afk_pass_handler.{h,cpp}`(新;`.h` 与服务注册表由 proto-gen 重生成) |
| 工程登记(10) | `cpp/libs/services/scene/scene.vcxproj{,.filters}`、`cpp/nodes/scene/scene.vcxproj{,.filters}`、`cpp/generated/table/table.vcxproj{,.filters}`(新表与新 Tip 段)、`cpp/generated/proto/proto.vcxproj{,.filters}`、`cpp/tests/bag_test/bag_test.vcxproj{,.filters}`。CMakeLists 由 vcxproj 生成,不手改 |
| gate(1) | `cpp/nodes/gate/gate_gm_client_messages.h`(`GmSetAfkPass` 进 GM 清单) |
| 单测(5) | `cpp/tests/bag_test/afk_pass_test.cpp`(新,规则 5 条 + 系统 9 条)、`client_request_ledger_test.cpp`(种类值)、`currency_test/currency_test.cpp`(键形金样)、`currency_test/client_gm_gate_test.cpp`(GM 闸)、`cross_zone_test/cross_zone_test.cpp`(托管读存档 2 条) |
| Go(1) | `go/player_locator/internal/logic/leasemonitor.go`(只改注释:键是 scene 写的投影) |
| robot(4) | `robot/afk_pass_follow_smoke_scenario.go`(新)、`robot/main.go`、`robot/config/config.go`、`robot/etc/afk_pass_follow_smoke.yaml`(新) |
| 客户端(10) | `Assets/Scripts/Game/AfkPass/AfkPassClient.cs`(新)、`Assets/Scripts/UI/Ugui/Gameplay/AfkPassUiRoot.cs`(新)、`Assets/Tests/EditMode/Tianyong/AfkPassClientTests.cs`(新)及三者与目录的 `.meta`、`Assets/Scripts/UI/Ugui/WorldHudNavigation.cs`(入口枚举)、`tools/gen_messageids.ps1`(白名单)、`tools/gen_proto.ps1`(协议清单) |
| 文档(6) | 本文、`player-connection-lost-hold.md` §4.1、`client-request-idempotency.md` §4、`docs/README.md`、`docs/PROGRESS.md`、Java 仓 `PARITY.md` |

go/db、商城、trade、支付都不改;player_locator 只改注释。

## 11. 双版本(AGENTS §12)

客户端可见的新服务、5 个 Tip 码、规则表数据两版共享,Java 版要同批实现;本仓只在 Java 仓 `PARITY.md` 末尾登记待做,不写 Java 代码。

## 12. 不做与后续

| 不做 | 原因 / 什么时候做 |
|---|---|
| 真钱直购月卡商品 | §4.2;要做时新增「权益」发放物并定义撤销扣回 |
| 月卡道具 / 兑换券、邮件发月卡 | §3.2;要做时加一个消耗兑换券调开通函数的入口 |
| 自动续费 / 订阅、月卡每日奖励 | 不在离线托管的需求里 |
| GM 后台网页加「设月卡」 | 后台写通道整体还没启用(`jeecg-gm-administration.md`);启用时给 `GmExecutePlayerCommand` 的 oneof 加第 8 项 |
| 开通退款 | 灵石消费不可退,与其它灵石消费一致 |

## 13. 待负责人确认

1. 月卡做成到期时刻、只用灵石开通,真钱经充值档位(§3.2、§4.2)。
2. 默认数值:300 灵石 / 30 天,剩余上限 180 天,到期前 72 小时提醒(§4.4)。
3. scene 改读内存、不再读 Redis(§5.3)。
4. 约 38 个文件的改动面(§10),含客户端与 robot。
