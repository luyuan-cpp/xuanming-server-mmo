# 神秘商人(定时随机落线的折扣 NPC)

> **状态(2026-10-09)**:代码、配表、单测、冒烟、客户端都已写完,在隔离分支 `feat/mystery-merchant` 上
> (服务端工作树 `E:\work\xuanming-server-mmo-wt-merchant`,客户端工作树 `E:\work\mmorpg-client-wt-merchant`),**均未提交**。
> **服务端未编译、未 proto-gen、未跑任何测试(AGENTS §10.1)**;只做过:proto 语法校验、沙盒导表(退出码 0)、gofmt。
> 客户端离线编译 0 错误、纯逻辑测试 59/59(离线 NUnit 替身),Unity 内未运行。给 Codex 的清单在 §9。

## 1. 目标与决定

### 1.1 用户需求(2026-10-09)

> 做一个 NPC,不是固定功能,类似以前问道端游的神秘商人。每隔一个小时随机降落到任意一条线路上,
> 出售的商品有 6 折、5 折、4 折的。

### 1.2 用户拍板的三项

| 项 | 决定 |
|---|---|
| 范围 | 全部做完:服务端调度 + 商店 + 配置表 + 冒烟机器人 + 客户端界面与 NPC |
| 停留时长 | 整点出现,停留 30 分钟后离开,下个整点出现在另一条线 |
| 限购 | 每人每件每轮限购(表里配份数),下一轮重置 |

### 1.3 用户未明示、按默认值落地的决定(全部可改表或改常量)

| # | 决定 | 在哪改 |
|---|---|---|
| D1 | 商人只去天墉城(地图 1),六个候选站位随机一个 | `MysteryMerchantSpot` 表:加别的地图的行,商人就会去 |
| D2 | 每轮上架 6 件,从 12 件商品池里按权重不放回地抽 | `MysteryMerchantRule.shelf_size`、`MysteryMerchantGoods.weight` |
| D3 | 每件独立掷折扣:6 折 50% / 5 折 35% / 4 折 15% | `MysteryMerchantRule.weight_discount60/50/40` |
| D4 | 全部用银两结算;原价量级参照宝宝改名 200、加点方案 1000 | `MysteryMerchantGoods.currency_type / price` |
| D5 | 折后价向上取整(原价 1 打 4 折仍是 1,不会白送) | `mystery_merchant_rules.h` `DiscountedPrice` |
| D6 | 必须与商人在同一条线、离站位 8 个单位以内才能买 | `MysteryMerchantRule.interact_range` |
| D7 | 新的一轮尽量不落在上一轮那条线上 | `pickMerchantChannel` 的避让 |
| D8 | 不在同一条线的玩家只知道「在几线」,看不到货架 | `PlayerMysteryMerchantSystem::FillInfo` |
| D9 | 出现公告只发给在线玩家(飘字),中途上线的玩家靠角标得知 | 客户端 `MysteryMerchantUiRoot` |
| D10 | 商人的形象由客户端画(暂用名册角色「灯穗小使」顶替),服务端没有 NPC 实体 | 客户端 `MysteryMerchantNpc.AppearanceId` |
| D11 | 战斗中、跨区冻结中不能买 | `PlayerMysteryMerchantSystem::Buy` |

### 1.4 为什么这样分工

把「神秘商人」拆成两件事:**谁决定他在哪、卖什么**,和**谁负责卖**。

| 环节 | 谁 | 为什么是它 |
|---|---|---|
| 每轮挑线、挑站位、掷货架 | scene_manager 的领导者 | 「全 zone 只有一个商人」需要全 zone 唯一的决策者,而领导者选举这里已经有;线号与「哪些线此刻进得去」只有它知道(分线目录就是它发布的) |
| 查询、购买、公告 | 商人所在线的 scene 节点 | 钱和背包在 scene 节点的内存里,购买必须在那里同步完成 |
| 形象、点击、界面 | 客户端 | 服务端目前没有任何 NPC 实体(`Npc{}` 是空壳,见 `xuanming-port-feasibility-20260902.md`) |

两边之间只有一份 Redis 记录(读模型),与分线目录是同一个套路:scene_manager 写,scene 节点只读。
scene_manager 出任何错,最坏结果是「这一轮没有商人」,不会多发也不会少扣。

## 2. 协议

`proto/scene/player_mystery_merchant.proto`(新文件,service `SceneMysteryMerchantClientPlayer`):

| RPC | 方向 | 说明 |
|---|---|---|
| `GetMysteryMerchantInfo` | C2S 请求-应答 | 纯读取:在不在、在几线、站在哪、何时走、下一次何时来;商人就在请求者这条线时才带货架 |
| `BuyMysteryMerchantGoods` | C2S 请求-应答 | 请求带 `round_id` + `goods_id` + `count` + 报价四项(`expected_price / currency_type / item_config_id / item_count`);成功应答带最新货架与人物背包全量 |
| `NotifyMysteryMerchant` | S2C 推送 | 出现公告:新的一轮开始、或同一轮内换线时,各 scene 节点向自己的在线玩家各推一次 |

消息号在本分支的 `proto/message_id.txt` 末尾**预留**了 264 / 265 / 266(生成器对已登记的 `服务名+方法名` 沿用旧号,
`service_register_info.go` `InitMessageId`)。预留是为了让客户端的 `MessageIds.cs` 现在就能生成。
最初预留的是 250–252;2026-10-15 把分支接到 `origin/main`(a105c3efdb)时,250–252 已正式发给装备强化镶嵌、
253–263 已发给落地摘要与玩家收件箱,于是顺延到 264 起。**合并时如果这三个号又被占用,改成当时的下一个空号**,见 §9 第 0 步。

要点:

- `MysteryMerchantState` 是 Redis 读模型,**不发给客户端**;客户端看到的是 `MysteryMerchantInfo`。
- `MysteryMerchantInfo.here` 在应答里因人而异;在公告里恒为 false(公告是一份消息发给很多人),客户端按 `scene_id` 自己比。
- **轮次号 `round_id` = 这一轮出现时刻的 Unix 秒**(与 `appear_at_ms / 1000` 同值)。不用「第几个间隔」
  (`now / 间隔`):那个数依赖间隔本身,间隔一调大它就变小,版本号跟着倒退。出现时刻只跟墙钟走,间隔不变时逐轮递增;
  改间隔的那一刻对齐点会变,新一轮的轮次号可能比上一轮小(只这一次),处理见 §2.1 与 §4。
- **报价防呆**:客户端把它在货架上看到的这件商品原样回传 —— 每份折后价、结算货币、物品、每份数量。
  服务端现算的有任何一项不一样就拒绝(`ShelfChanged`)并让客户端刷新。配表热更、scene 与 scene_manager 表版本不一致时,
  玩家付出的和拿到的永远是他点下购买时看到的那一份(只核价格的话,「价格没变、物品换了」会漏过去)。
- 客户端零配表:商品名、图标、原价、折后价、限购、已购数全部由服务端下发。
- 玩家存档:`player_database.mystery_merchant = 21`(`PlayerMysteryMerchantComp`:`round_id` + `purchases[]`)。
  18 / 19 / 20 已被存储围栏与收件箱账本占用;按加列纪律取 20 之后的下一个空闲号,声明在末尾。
  只记当前这一轮;轮次号不同 = 整份作废。轮次号不含 zone:跨 zone 访客在两个 zone 的同一个时间窗里共用这一份记录
  (同一个 `goods_id` 的名额共享,只会更严,不会多买)。

### 2.1 Redis 读模型(跨语言契约)

```
HASH mystery_merchant:zone:{zone}
  state = MysteryMerchantState 的序列化字节
  v     = round_id × 1000 + revision(十进制;只供写入脚本比较)
TTL   = 2 × 间隔
```

- 写入方:`go/scene_manager/internal/logic/mystery_merchant.go`(`MysteryMerchantKeyFmt` / `MysteryMerchantStateField`)。
- 读取方:`cpp/libs/services/scene/player/system/player_mystery_merchant.cpp`(`kMysteryMerchantKeyFmt` / `kMysteryMerchantStateField`)。
- 改一边必须改另一边;Go 侧 `TestMysteryMerchantKey_CrossLanguageContract` 钉住字面量。
- 写入走 Lua:新版本号必须严格大于已有的才写。防的是领导权交接窗口里两个副本各掷各的。
- **强制覆盖**(Lua 的第 4 个参数为 `'1'`,跳过版本比较)只用在「现有记录已经不可信」的两种情形:
  记录解析不了 / 写的不是本 zone;或它是按另一个出现间隔排出来的(调表、改覆盖项之后)。
  后一种情形下新一轮的版本号可能比旧记录小,不强制的话要等旧键的 TTL 过期才写得进去。
- **读取方对旧版本的处理**(`merchantrules::ShouldDiscardAsStale`):只有「手里那一轮还没结束」时才丢弃版本更小的记录
  (防 Redis 主从切换把同一轮退回更早的落位);手里那一轮结束之后读到什么都采纳。
  这个出口不能省:没有它,强制覆盖出的小版本记录会被 scene 节点永久拒收,只能重启。
- `revision`:同一轮内第几次落位,从 1 起。只有商人所在的线被回收 / 销毁才 +1;**轮次号与货架不变**(限购按轮次号记)。

## 3. 配置表与提示码

三张新表 + 一段新提示码,全部由幂等脚本 `tools/scripts/mystery_merchant_xlsx_patch.py` 生成(`new-tables`、`tip-codes`)。
schema 在 `data/schema/mysterymerchant{rule,goods,spot}_table.proto`。

### 3.1 `MysteryMerchantRule`(单行规则)

| 列 | 初值 | 范围 | 谁读 |
|---|---|---|---|
| `interval_seconds` | 3600 | 60..86400 | scene_manager。出现时刻对齐到它的整数倍,3600 = 每个整点 |
| `stay_seconds` | 1800 | 30..间隔 | scene_manager。等于间隔 = 一直在场,只是每轮换线换货 |
| `shelf_size` | 6 | 1..32 | scene_manager |
| `weight_discount60 / 50 / 40` | 50 / 35 / 15 | ≥0,和 >0 | scene_manager |
| `interact_range` | 8 | 1..100 | scene(购买距离,服务器坐标单位) |

### 3.2 `MysteryMerchantGoods`(商品池,12 行)

列:`id`、`name`(留空则用物品表的名字)、`item_id`(FK Item)、`item_count`(每份数量)、`currency_type`(0 银两 / 1 灵石 / 2 绑定灵石)、
`price`(每份**原价**,1..10^9)、`weight`(上架权重,0 = 暂不上架)、`limit_count`(每人每轮限购份数,1..99)。
`item_count` 不得超过该物品的堆叠上限(两侧都校验:scene_manager 的配表校验不过、整拍不调度;scene 节点不下发也不卖)。
初始的 12 行都满足(药品类堆叠上限 999,装备类每份 1 个)。

`weight = 0` 同时是**立即停售**的手段:scene 节点在下发与结算时都会再认一次这一列,把某一行改成 0 并热更 scene 的表,
正在卖的这一轮里它马上从货架上消失、买不了(scene_manager 的表只在启动时加载,它要到重启后才不再把这一行排上架)。

初始数据:回血丹 / 回蓝丹 / 培元丹 / 回灵散(各限购 5)、精炼石 / 修行秘录残页 / 灵兽口粮(限购 3)、
玄铁护符 / 藏经阁手札 / 青锋剑 / 青纱袍 / 寒铁剑(限购 1)。展示名与帮会商店卖同一物品时的叫法一致。

折后价 = `ceil(price × 折扣百分比 / 100)`,公式唯一出处是 `mystery_merchant_rules.h` 的 `DiscountedPrice`;
下发货架与结算调的是同一个函数。

### 3.3 `MysteryMerchantSpot`(候选站位,6 行)

列:`id`、`scene_config_id`、`x / y / z`(**服务器坐标**;Unity.x = 服务器 y,Unity.z = 服务器 x)、`desc`。

这张表同时决定「商人会去哪些地图」:只有出现过的 `scene_config_id` 才参与选线。
六个点都在天墉城,每个点都用客户端的可走掩码核对过(与出生点连通,以它为中心的 14×14 单位全部可走):
出生点旁 (176,206)、中央广场西侧 (161,159)、东侧 (161,243)、北广场 (255,201)、南大街 (101,201)、东南小广场 (113,297)。

### 3.4 提示码(新域 `//mystery_merchant_error base=30000 width=1000`,12 个,30000–30011)

最初定的 29000 段在接到新 main 时已被 `player_inbox_error` 占用,改用下一个空闲整千段 30000(脚本里的 `TIP_BASE`)。

`NotPresent`(不在场)、`NotHere`(不在这条线)、`TooFar`、`ShelfChanged`(货架 / 轮次 / 价格对不上)、`LimitReached`、
`CurrencyNotEnough`、`BagFull`、`InBattle`、`Frozen`、`InvalidCount`、`ItemBlocked`、`InternalError`(fault)。
表里的码名不带 `k` 前缀(导表器自己加)。

## 4. scene_manager:调度

`go/scene_manager/internal/logic/mystery_merchant.go`。领导者每 5 秒一拍(`safego.Loop`,点位 `scene_manager.mystery_merchant`),
对每个活跃 zone 调 `ScheduleMysteryMerchantForZone`。一拍里只可能发生三件事之一:

1. **本轮已排好、线还在** → 什么都不写(`kept`);
2. **本轮已排好、但线被回收或销毁** → 同一轮内换一条线,货架与限购不变(`relocated`);
3. **进入了新的一轮且还在停留期内** → 挑线、挑站位、掷货架,写入新记录(`created`)。

细节:

- **轮次**:出现时刻 = `floor(now / 间隔) × 间隔`,`round_id` = 出现时刻的 Unix 秒。调度在整点之后才恢复时,
  只要还在停留期内就照常排(出现时刻仍记整点);停留期已过则不补排(`idle`)。
- **改了出现间隔**(调表后重启、或改了 `IntervalSecondsOverride`):现有记录的 `interval_seconds` 与当前规则不同 →
  旧记录作废,按新间隔重新排并**强制覆盖**(§2.1)。新间隔下的这一轮如果停留期已过,则等到下一轮开始才写
  (这期间 scene 节点照旧记录卖到它的离开时刻)。限购记录按轮次号记,轮次号变了就当作新的一轮。
- **挑线**:读候选地图的分线目录(`world_channel_directory:zone:{z}:{conf}`),只要流畅 / 繁忙的线;一条都没有时退而用爆满的线;
  回收中 / 不可用的永远不选。在这些线里均匀随机,有别的选择时避开上一轮那条。目录还没发布 → `no_channel`,下一拍再试。
- **掷货架**:按权重不放回地抽 `shelf_size` 件,每件再独立掷折扣档;结果按 `goods_id` 升序(= 客户端展示顺序)。
- **换线**:目录里那条线是回收中(CLOSING)→ 立即换;目录里压根没有它 → 连续 3 拍都没有才换
  (排空起步时有一个毫秒级窗口两个集合都读不到它);不可用(节点刚判死)→ 不换,线会在别的节点按同一个 `scene_id` 重建;
  目录整个缺失(领导者刚切换)→ 不动。
- **坏数据**:记录解析不了(或写的不是本 zone)按「没有记录」处理,当拍重新排并强制覆盖;
  配表校验不过则本拍不调度(`invalid_table`),已排好的那一轮不受影响。
  分线目录里的地图号与它所在的键对不上(两侧契约被破坏)→ 本拍报 `error`,不拿错位的数据选线。
- **配表校验**(每拍,不过就不调度):间隔 / 停留 / 货架大小 / 权重的范围;商品的价格、限购、货币、物品存在、
  `item_count ≤ 堆叠上限`;站位所在的地图必须是本 zone 的世界地图(`worldConfIds()`),否则那条线永远选不到却不报错。
- **配表生效时机**:scene_manager 只在启动时加载配置表,改表要重启它;scene 节点的表可以热更。
  两边版本不一致的那段时间由 scene 节点按自己的表再核一遍(§5.2 的「商品可售」)。
- 时间与随机数都是显式参数(`now`、`merchantRand`),单测注入。

### 4.1 配置(`MysteryMerchant` 块,整块可缺省 = 启用)

| 字段 | 缺省 | 说明 |
|---|---|---|
| `Disabled` | false | true = 不调度 |
| `TickSeconds` | 5 | 钳到 [1, 60] |
| `IntervalSecondsOverride` / `StaySecondsOverride` | 0 | 覆盖规则表的间隔 / 停留。**只用于联调与冒烟** |
| `SpotIdOverride` | 0 | 把商人钉在某一个站位。**只用于联调与冒烟**(机器人只会走直线) |

玩法数值在表里,这个块只放部署侧开关。

### 4.2 指标

`scene_manager_mystery_merchant_schedule_total{zone_id, outcome}`,outcome ∈
`created | relocated | kept | idle | no_channel | lost_race | invalid_table | error`。
读数:`created` 每个 zone 每个间隔应当恰好 +1;一个间隔过去而它没动 = 这一轮没有商人,此时看 `no_channel / invalid_table / error`。

## 5. scene 节点

### 5.1 读记录与公告(`PlayerMysteryMerchantSystem`)

- 节点启动后每 5 秒 `HGET mystery_merchant:zone:{zone} state` 一次,结果缓存在一个 `thread_local` 变量里
  (属于 scene 的 EventLoop 线程,与 `tlsEcs` / `tlsRedis` 同一个所有权口径;它不是组件,不挂在任何实体上)。
- 读失败 / 应答形状不对 / 解析失败 → **保留旧缓存**(购买的时间窗按记录自带的离开时刻判,旧缓存不会卖过点)。
  键不存在 → 清空缓存。读到比缓存更旧的版本 → 手里那一轮还没结束就丢弃,已经结束就采纳(§2.1)。
- 发现一份版本号比「已公告过的」更大的记录,且此刻在停留期内 → 向**本节点**的在线玩家发 `NotifyMysteryMerchant`
  (没有会话快照、或会话号无效的实体跳过:广播层会为每个这样的实体打一条 ERROR)。
  每个节点只管自己的玩家,合起来每人恰好一条;不用 `BroadcastToKnownGateClients`(那样每个节点都会让全服收到一条)。
  已经结束的轮次不补发;出现时刻还没到(本机时钟略慢)则下一次刷新再判。
  强制覆盖出的、版本更小的新一轮不补公告(改间隔是运维操作),玩家靠角标与进场查询得知。
- `next_appear_at_ms` 按手里那份记录的出现时刻与间隔往后推;刚过整点的 30 秒宽限期内(本节点还没读到新一轮)
  回答的是「刚过去的这个整点」而不是再下一个整点 —— 否则这时进场的玩家会看到「59 分 50 秒后出现」,几秒后商人却到了。
  客户端把「不晚于现在」显示成「即将出现」,并在宽限期之后自己再问一次。

### 5.2 购买(`Buy`)

三段,同一次调用里同步完成,中间不让出 EventLoop:

1. **只读预检**(任何一处 return 都保证什么都没改):实体有效 → 未冻结 → 不在战斗 → 份数 1..99 → 商人在场 →
   轮次号一致 → 同一条线 → 距离 → 商品在架 → 商品可售(按本节点此刻的表再核一遍,含「未下架」与「每份数量不超堆叠上限」)→
   报价四项一致 → 限购 → 总价 / 总数量不溢出 → 物品未被全服封禁 → 余额够 → 背包放得下。
   (物品封禁只有全服一种:个人封禁表 `PlayerItemBlockList` 已在 ECS 重构中删除。)
2. **修改**:**发物 → 扣钱 → 记名额**。两条流水用 `TX_SHOP_BUY` + 同一个 `correlation_id`(轮次号)。
   - 发物被拒且背包没动(预检查不到的原因,主要是物品号段不可用)→ 钱还没动,直接返回 `kMysteryMerchantInternalError`。
     **没有退款路径**;
   - 发物成功后扣钱 —— 它会失败的条件(金额非正、币种未知、冻结、缺货币组件、余额不足)在预检里已经全部排除,
     且从预检到这里是同一次同步调用。万一仍然失败(编程错误):货已发、名额照记,日志标签 `[UNPAID_GRANT]`,转人工追缴。
   - 「只发出一部分」不会发生:新的 `BagService::AddItems` 是候选提交,契约是「失败时原包一件没动、不发流水」。
3. **组应答**:最新货架 + 人物背包全量。背包快照建不出来不改变「买到了」这个事实(应答不带背包,客户端自己重拉)。

**为什么先发物、后扣钱**(与宝宝洗点「先扣费再改状态」相反):那里的「改状态」不会失败,这里的发物会 ——
封禁与空间之外,还有号段不可用(data_service 故障时的既定 fail-closed)。先扣钱的话,发物失败就得把钱退回去,
而仓里唯一的加钱入口 `AddCurrency` 会先抵扣补缴欠款、遇到币种封禁还会拒绝:有欠款的玩家会「钱扣了、货没到、退款也没拿到」。
先发物则不需要退款:`BagService::AddItems` 自带完整预检并且是候选提交,「被拒 = 一件没动」。

失败时由 handler 把错误码直接写进本次应答的 `error_message`(新 handler 模板不再有 `TRANSFER_ERROR_MESSAGE`,全局 tip 实体也已删除)。

### 5.3 限购记录为什么落在玩家存档里

`PlayerMysteryMerchantComp` 与 `currency` / `bag_component` 同在 `player_database` 一行、同一次存盘落地
(与 `asset_op_ledger`、`settlement_ledger` 是同一条不变量 I3):

- 崩溃回档时「钱、货、已购数」一起退回同一个时刻。放 Redis 的话,回档后会出现「货没了但名额已经用掉」;
- 商人所在的线会换节点(节点判死后重建、空线被 rebalance 迁走)。放节点内存的话,换一次节点名额就清零一次。

购买路径对这个组件用 `try_get`,缺失即拒绝(不现造一份空记录来放行)。它由 `PlayerDatabaseMessageFieldsUnmarshal` 无条件挂上。
存盘路径按新 main 的「先查后写」:`PlayerDatabaseMessageFieldsMarshal` 用 `FindPersistedComp` 确认它在,缺了就整份拒存
(补一份空记录落盘等于把这一轮已买的名额清零)。手搓玩家再存盘的测试夹具因此都补了这个组件:
`bag_test.cpp`、`player_equip_attribute_test.cpp`、`player_feature_persistence_test.cpp`(含「缺哪个组件就拒存」清单)、
`cross_zone_test.cpp`、`db_task_dispatch_test.cpp`。

### 5.4 纯规则(`mystery_merchant_rules.h`,namespace `merchantrules`)

折后价、总价(溢出即拒)、时间窗(半开)、下一次出现时刻(含整点后的宽限期)、水平距离(含边界)、
限购记账(轮次不同先清空)、版本号、旧记录的取舍(`ShouldDiscardAsStale`)。
不碰 ECS、不读表、不取时间;下发与结算共用,单测直接调。

## 6. 客户端

详见客户端仓 `Docs/mystery-merchant-ui.md`。要点:

- `MysteryMerchantClient`(数据层,不引用 UnityEngine):查询 / 购买 / 公告;按 `server_time_ms` 推算服务端时刻来显示倒计时;
  查询最小间隔 2.5 秒(排队补发)、失败后自己退避重试(5 / 10 / 20 / 40 / 60 秒);购买最小间隔 1 秒;
  购买超时后要等重连(应答按消息号先进先出匹配,迟到的旧应答分不清)。换了场景之后,上一个场景里问出来的应答整包丢弃并重问。
- **本地余额不拦购买**:客户端手里的余额是某次快照,scene 不推送余额变化;按它置灰的话玩家攒够了钱也点不了。
  余额只用于显示,买不买得起由服务端回 `CurrencyNotEnough`。
- **购买带报价**:第一次点击拍下报价(轮次 + 报价四项),第二次点击才发;两次点击之间报价变了、或挨得太近(0.4 秒内,双击)都不算确认。
- `MysteryMerchantUiRoot`:右上角角标(「神秘商人 3线 12:34」)、出现公告飘字(同一次落位只弹一次)、
  商人形象(只在「商人在本线 + 我在那张地图」时)、一块跟随屏幕位置的透明热区接住点击(离得远先走过去,走到开店距离内再开)。
- `MysteryMerchantWindow`:三列两行的货架;商人在别的线时给「前往 N线」(走切线功能),进不去时状态行说明原因
  (线路已满 / 冷却中 / 不在同一张地图),并每 5 秒刷新一次线路目录。

## 7. 失败与一致性

| 情形 | 结果 |
|---|---|
| scene_manager 全挂 / 没有领导者 | 新的一轮不出现;已排好的那一轮照常卖到离开时刻 |
| 领导者交接,新旧各跑一拍 | Lua 版本比较:后写的同版本记录被拒(`lost_race`) |
| Redis 抖动 | scene_manager 本拍不写;scene 节点保留旧缓存 |
| scene 节点时钟与 scene_manager 有偏差 | 购买截止按 scene 节点自己的时钟对照记录里的离开时刻;偏差几百毫秒只影响边界那一瞬 |
| 商人所在的线被回收(自动缩容) | 最迟一拍(5 秒)后换线;旧线上的 scene 节点最迟再过 5 秒读到新记录,之后那条线上买不了 |
| 商人所在的 scene 节点宕机 | 线在别的节点按同一个 `scene_id` 重建,记录不用改;限购记录在存档里,不丢 |
| 购买途中换轮 / 换线 | 请求里的 `round_id` 对不上 → `ShelfChanged`;换线后不在同一条线 → `NotHere` |
| 表热更改了价格 / 货币 / 物品 / 每份数量 | 报价四项任何一项对不上 → `ShelfChanged`,客户端自动重新查询 |
| 运营要立刻停售某件商品 | 把那一行的 `weight` 改成 0 并热更 scene 的表:当轮即从货架消失、买不了 |
| 运维改了出现间隔 | scene_manager 作废旧记录、按新间隔重排并强制覆盖;scene 节点在手里那一轮结束后采纳(§2.1) |
| 预检通过后发物被拒(物品号段不可用) | 钱没动、名额没记,返回 `InternalError`;号段恢复后照常可买 |
| 玩家有补缴欠款 / 币种被封禁 | 不影响购买的原子性:没有退款路径,不经过 `AddCurrency` |
| 客户端手里的余额过期 | 不影响能不能点购买;服务端按真实余额裁决 |
| scene 与 scene_manager 的表版本不一致 | 货架上本节点解析不了的商品不下发、买不了(`InternalError` + 日志),其它商品照常 |
| 玩家存档回档 | 钱、货、已购数一起回到同一个时刻 |
| 客户端伪造请求 | 每一道都在服务端重新判:轮次、所在线、距离(用服务端记录的坐标)、在架、报价、限购、余额、背包 |
| 跨节点换场景,新旧节点各发过一条公告 | 客户端按(轮次, 场景实例)去重,同一次落位只提示一次 |

## 8. 没做的 / 已知缺口

1. **服务端没有 NPC 实体**:别的玩家的客户端各自画商人,位置来自同一份坐标,所以看到的一致;但没有「走近才显示」的 AOI,
   商人在本线时整张图的人都能看到他(只有一个静态形象,开销可忽略)。
2. **中途上线的玩家不补公告**:靠进场时的查询与角标得知。
3. **没有全服聊天 / 跑马灯**:仓里没有系统公告通道(聊天的系统频道未开放),公告只是飘字。
4. **商人形象是顶替的**(名册角色「灯穗小使」),没有专属立绘。
5. **合服工具没有登记这把键**(`tools/merge_zone/scene_hot_state.go`):`mystery_merchant:zone:{z}` 自带 TTL(2 个间隔)、
   过期自消,记录里的时间窗过了就无效,留着无害。合服窗口远长于它的 TTL。
6. **限流表没有登记新消息号**:走 gate 默认档(每窗口 3 次)。客户端的查询 / 购买间隔都在默认档之内。
7. **k8s 告警 / 看板**没有为新指标加面板与告警规则。
8. **一次只能买 1 份**(客户端界面);协议与服务端支持 1..99。
9. **站位只在天墉城**;别的地图要先在客户端摆得出形象(目前只有天墉城有绘制好的主城)。
10. **购买距离是软闸**:距离按 scene 记录的玩家坐标判,而那个坐标来自客户端上报(移动校验只管「直线可达」)。
    改客户端的人可以在同一条线的远处购买。影响面只是「少走几步路」(必须已经在商人那条线上),没有为它加硬校验。
11. **商人所在的线「不可用」时不换线**(节点刚判死):线会在别的节点按同一个 `scene_id` 重建,这段时间(通常几十秒)
    那条线上买不了,角标仍然指向它。只有回收 / 销毁才换线。
12. **升级顺序**:`player_database` 新增一列 `mystery_merchant`。必须先对每个 zone 库跑迁移再升级 go/db
    (缺列时 go/db 拒绝启动,这是既有的保护);scene 先于 go/db 升级的话,新字段在落库时被旧 go/db 丢掉,限购记录不持久。
13. **k8s 配置模板里没有 `MysteryMerchant` 块**:块缺省 = 启用、按表调度。**止血步骤**(按顺序):
    ① scene_manager 的配置里加 `MysteryMerchant: {Disabled: true}` 并重启(不再排新的一轮);
    ② 要立刻停掉正在卖的这一轮,再到该 zone 的 Redis 执行 `DEL mystery_merchant:zone:{z}`(scene 节点 5 秒内清空缓存,
    之后购买一律 `NotPresent`)。顺序不能反:调度还开着时删键,下一拍会在停留期内重新排出来。
    只想停售某一件商品见 §3.2。
14. **同一名玩家同一轮里的多笔购买共用一个 `correlation_id`**(轮次号)。流水自带 `player_id` 与时间戳,
    发物那条的 `extra` 里有商品与份数,足够定位一笔购买;没有为每一笔单独发号。
15. **`weight = 0` 立即停售没有单测**(夹具改不了表行);`ResolveGoods` 是下发与结算共用的一条路径,靠联调验证。
16. 评审记录见 §10。

## 9. 交接给 Codex 的验证清单(AGENTS §10.1)

**现状(2026-10-16):第 0–7 步已在主仓实跑通过,结果见 §9.1;第 8 步(客户端)只有离线编译与离线用例,Unity 未运行。**
下面的清单保留作复验用。任何一步的通过标准不满足就停,保留该步写明的日志。

**前置:main 本身要先能生成、能编译。** 2026-10-15 的 ECS 重构第 2–5 批只完成了源码,负责人要求暂缓生成器构建、统一重生成、
编译与测试(`docs/handoff/ecs-refactor/README.md` §7)。本分支的源码已经按那批重构后的接口写(`PlayerGuidComp`、`SceneEntity`、
新 handler 模板、无全局 registry、`AddItems` 新签名),所以**要和那次统一重生成、编译一起做**;在那之前单独编本分支没有意义。

**总原则**(与 `equipment-attributes.md` §9 相同):生成与 C++ 编译都在主仓 `E:\work\xuanming-server-mmo` 做(子模块齐全),
隔离工作树只是源改动的载体;MSBuild 一律串行 `/m:1 /p:Configuration=Debug /p:Platform=x64`。

**0. 带入主仓**
- 分支 `feat/mystery-merchant` 已提交、已接到 `origin/main` a105c3efdb(2026-10-15)之上,未推送。合进 main 时若 main 又前进了:
- **`proto/message_id.txt` 可能冲突**(两边都在文件末尾追加):保留 main 的全部行,把本分支的三行接在后面;
  **如果 264 / 265 / 266 已被 main 上别的方法占用,把这三行改成当时的下一个空号**(三行的方法名不变)。
- **`player_database` 的字段号**:本分支用 21。main 若已用掉 21,改成下一个空闲号并同步本文 §2 与第 6 步。
- `data/tip/Tip.xlsx` 是二进制:取 main 的版本,再跑第 1 步的 `tip-codes`(脚本只往空格子里追加)。
  若 30000 段已被别的域占用,脚本会中止并提示改 `TIP_BASE`。
- `docs/PROGRESS.md` 配了 `merge=union`,两边条目都会保留。

**1. xlsx 补丁确认**
- `py tools/scripts/mystery_merchant_xlsx_patch.py new-tables --dry-run`、`... tip-codes --dry-run`
- 通过标准:都报「已是目标状态」。否则去掉 `--dry-run` 重跑(幂等)。

**2. 导表 + proto-gen**(在主仓根目录;与 ECS 重构的统一重生成同一次做)
- 导表:`dev.bat gen`。
- proto 生成(AGENTS §4):`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build`(生成器源码变了或还没有工具时),
  再 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`
  (不写客户端仓;客户端在第 8 步单独生成 —— 客户端主工作区 `E:\work\mmorpg-client` 里有别人的在制改动)。
- 通过标准:
  - `generated/tables/`:`mysterymerchantrule.json` 1 行、`mysterymerchantgoods.json` 12 行、`mysterymerchantspot.json` 6 行;
  - tip 号只新增 30000–30011(`generated/code/proto/tip/mystery_merchant_error_tip.proto`);
  - `proto/message_id.txt` 里本功能的三个方法保持第 0 步定下的号,其余行不变;
  - 新生成:`cpp/generated/proto/scene/player_mystery_merchant.pb.{h,cc}`、`cpp/generated/proto/common/component/player_mystery_merchant_comp.pb.{h,cc}`、
    `cpp/generated/rpc/service_metadata/player_mystery_merchant_service_metadata.h`、三张表的管理器与 `proto/tip/mystery_merchant_error_tip.pb.{h,cc}`;
  - `cpp/nodes/scene/handler/rpc/player/player_mystery_merchant_handler.{h,cpp}` **无 diff**(手写的骨架与生成器同形;守护段内容都在);
  - `cpp/nodes/scene/handler/rpc/player/player_service_interface.cpp` 多出 `SceneMysteryMerchantClientPlayer` 的注册;
  - `rpc_event_registry.cpp` 的 `IsClientMessageId` 白名单含那三个号(gate 靠它放行客户端消息);
  - `robot/generated/pb/game/message_id.go`、`go/proto/scene/player_mystery_merchant.pb.go`、`go/shared/generated/table/mysterymerchant*_table.go` 出现。
- 工程登记已经手工加好(`table.vcxproj` / `proto.vcxproj` / 两个 `scene.vcxproj` / `bag_test.vcxproj` 及各自的 filters 与 CMakeLists)。
  若生成物的实际文件名与登记的不一致,以生成物为准改登记。
- `data/AGENTS.md` 的配表索引已用 `gen_schema_index.py` 刷新过(45 张表);导表后再跑一次应当无差异。
- 2026-10-15 已在沙盒里按新 main 跑过一次完整导表(`tools/data_table_exporter/tools/sandbox_export.py --compare`):
  三张表的 JSON / C++ / Go / C# 产物与 tip 段 30000–30011 都生成了;与仓内产物相比,「内容不同」的只有表注册清单
  (`all_table.cpp`、`AllTable.cs`、`AllTable.java`、`all_table.py`、`ConfigSubsystem.*`)、`manifest.json` 与 `tip_text.json`,
  既有表的产物逐字节相同。

**3. C++ 编译(严格按序)**
`table` → `proto` → `core` → `rpc` → `grpc_client` → `modules` → `battle` 库 → `scene` 库 → `cpp\nodes\gate`、`cpp\nodes\scene` → `bag_test`。
- gate 必须重编(客户端消息白名单是生成物)。
- 预期的失败形态:`kMysteryMerchant*` / `MysteryMerchant*TableManager` 未声明 → 第 2 步的导表没做;
  `MysteryMerchantState` / `SceneMysteryMerchantClientPlayer*MessageId` 未声明 → proto-gen 没做;
  `mystery_merchant()` 不是 `player_database` 的成员 → `mysql_database_table.pb.*` 没重新生成。
- 首个错误连同上下 20 行保留。

**4. C++ 单测**
- `pwsh tools/scripts/run_cpp_tests.ps1 -Filter bag_test`,再直跑:

| 过滤器 | 通过标准 |
|---|---|
| `MysteryMerchantRulesTest.*` | 11 条 |
| `PlayerMysteryMerchantTest.*` | 25 条 |
| `PlayerFeaturePersistenceTest.*` | 全过(「缺哪个组件就拒存」清单多了 `PlayerMysteryMerchantComp`) |
| 不带过滤 | 没有既有用例变红 |

- `cross_zone_test` 也要跑:它的两个夹具补了 `PlayerMysteryMerchantComp`,漏了会让存盘被拒、用例变红。

- `PlayerMysteryMerchantTest` 的 SetUp 报 "tables not exported yet" → 第 2 步没做或运行目录里的表是旧的。
- 各条拒绝用例会按设计打 WARN / ERROR 日志,不算失败;看 `[  FAILED  ]` 行。
- `PlayerMysteryMerchantTest.GrantRefusedAfterThePrecheckChargesNothing` 会故意把物品号段置为不可用
  (日志里有号段相关的 ERROR,属预期);夹具的 TearDown 会恢复号段,它后面的用例不受影响。

**5. Go**(2026-10-16 起主干要 Go 1.27.2:`%LOCALAPPDATA%\Programs\Go\1.27.2\go\bin\go.exe`;`GOTOOLCHAIN=local`)
- 工作目录 `go\scene_manager`:`go build ./...`、`go vet ./internal/...`、
  `go test ./internal/logic/ -run "Merchant" -count=1`、`go test ./internal/logic/ -run TestIntegration_ -count=1`(既有集成用例不回归)。
- 通过标准:退出码 0;`mystery_merchant_test.go` 的 28 个测试函数全过(函数名都含 `Merchant`)。
- robot(工作目录 `robot\`):`go mod vendor` → `go build -mod=vendor -o robot.exe .` → `go vet -mod=vendor .`。

**6. 数据库迁移**(每个 zone 库)
- 工作目录 `go\db`:`go run ./cmd/migrate up`。退出码 4 = 已是最新,不是失败。
- 通过标准:`player_database` 多一列 `mystery_merchant`(排在 `settlement_ledger` 之后)。不迁移的话 go/db 会拒绝启动。
- 升级顺序:**先迁移 → 再换 go/db → 再换 scene**。scene 先换的话,新字段落库时会被旧 go/db 丢掉(限购记录不持久)。

**7. 起栈与冒烟**
- `go/scene_manager/etc/scene_manager_service.yaml`:把 `MysteryMerchant` 块的五行注释去掉(间隔 120 / 停留 120 / 站位 1)。
- 用同一批产物重启 Go 服务与 gate / scene。scene 启动日志里应有一行
  `[MysteryMerchant] Table check finished: sellable_goods=12 spots=6 interact_range=8`,
  且没有 `[MysteryMerchant] MysteryMerchantGoods row is not sellable`;scene_manager 日志里 5 秒内出现
  `[MysteryMerchant] zone=1 round=…: appeared on map 1 channel N`。(日志文案按仓库 10-13 的约定统一为英文。)
- **先跑既有 login-test 基线**(健康栈 23/23),再在 `robot\` 下:`.\robot.exe -c etc/merchant_smoke.yaml`。
- 通过标准:以 `MERCHANT_SMOKE_OK` 结尾、退出码 0。同一轮里再跑一遍应当改买下一件(或 `MERCHANT_SMOKE_SKIP reason=all_bought`)。
- 失败时保留 `MERCHANT_SMOKE_FAIL` 行及其前 20 行、同时段 scene 日志的 `[MysteryMerchant]` 行、scene_manager 日志的 `[MysteryMerchant]` 行。
- 跑完把 yaml 改回去。

**8. 客户端(`E:\work\mmorpg-client-wt-merchant`)**
- `pwsh -File tools/gen_proto.ps1 -ProtoRoot E:\work\xuanming-server-mmo`、`pwsh -File tools/gen_messageids.ps1 -ProtoRoot E:\work\xuanming-server-mmo`
  (在客户端工作树里跑,`-ProtoRoot` 指向已合入本分支的服务端主仓)。
- 通过标准:`PlayerMysteryMerchant.cs` / `MysteryMerchantErrorTip.cs` 无差异(或只有因 `player_bag.proto` 版本带来的描述符差异;
  `BuyMysteryMerchantGoodsRequest` 必须有 `ExpectedCurrencyType / ExpectedItemConfigId / ExpectedItemCount` 三个字段);
  `MessageIds.cs` 里三个常量的号与第 0 步定下的一致。
- 之后按客户端 `Docs/mystery-merchant-ui.md`「还没验证的」跑 EditMode 用例、截图与联机。

**9. 收尾**:在 `docs/PROGRESS.md` 的条目里补上实跑结果;Java 仓 `PARITY.md` 登记(§11)。

### 9.1 实跑结果(2026-10-16,主仓本机 main,Claude 获用户授权执行)

证据目录:`E:\work-archive-20261015\output\mystery-merchant-build-20261015\`(各步日志、`run*/results.json`)。

| 步骤 | 结果 |
|---|---|
| 0–2 带入 / 导表 / proto-gen | 已随 main 的统一重生成完成;生成物逐项核对 26 项全符合第 2 步的通过标准,手写 handler 与生成器输出逐字节相同 |
| 3 C++ 编译 | 21 个工程(`third_party` 到三个节点)全部 0 错误。中途修了三处与本功能无关的既有问题,见下表 |
| 4 C++ 单测 | 本功能 57/57:`MysteryMerchantRulesTest` 11、`PlayerMysteryMerchantTest` 25、`PlayerFeaturePersistenceTest` 21。`bag_test` 全量 606/622,挂的 16 条见下表(去掉本功能的用例同样是这 16 条);`cross_zone_test` 240/240 |
| 5 Go | `scene_manager` build / vet 通过,`Merchant` 28 个测试函数全过,`go test ./...` 三个包全 ok;robot vendor / build / vet / test 通过 |
| 6 迁移 | `zone_1_db` 补 6 列(含 `mystery_merchant`,`pb:21`) |
| 7 起栈冒烟 | scene 启动日志 `Table check finished: sellable_goods=12 spots=6 interact_range=8`;scene_manager 首拍即排出一轮。login-test 基线 23/23。`merchant-smoke` 连跑三遍全部 `MERCHANT_SMOKE_OK`(见下) |
| 8 客户端 | 已在客户端本机 main;离线编译 0 错误、离线用例 70 条通过。**Unity EditMode / 截图 / 联机未做** |

`merchant-smoke` 三遍(本机主城 16 条线,商人每轮随机落线,三遍机器人都不在商人那条线,M3 都真的切了线):

| 遍次 | 轮次 / 线 | 结果 |
|---|---|---|
| 1 | 1792137000 / 9 线,6 折 | 买商品 1:1 份 + 剩余 4 份,第 6 份被拒 `LimitReached` |
| 2(同一轮、重新登录) | 同上 | 商品 1 已买满被跳过,改买商品 2 并买满 —— 限购记录跨登录保留 |
| 3(换到下一轮) | 1792137120 / 16 线,5 折 | 商品 1 名额重置、再次买满 —— 「每人每件每轮」按轮次重置 |

scene 日志里每笔购买一条 `Purchase succeeded`(份数、单价、折扣、扣款与背包合计都与机器人断言一致),本功能没有 WARN / ERROR。

编译与起栈过程中遇到的、**与本功能无关**的既有问题:

| 问题 | 处理 |
|---|---|
| 目录整理后 `lib/` 缺 `hiredisd.lib` 等,三个节点 LNK1356 | 从 `third_party/grpc/install_vs2026_dbg/lib/` 拷回 4 个库(`lib/` 不进 git) |
| scene 节点没登记 `rpc_replies/player_inbox_response_handler.cpp`,LNK2019 `InitPlayerInboxReply` | 登记进 `scene.vcxproj` / filters / CMakeLists(18bcc8f603) |
| `bag_test` 缺 `muduo_grpc.lib` / `nghttp2.lib`(gRPC 传输换到 muduo_grpc 之后) | 补进链接(18bcc8f603) |
| `cross_zone_test` 一处 `EXPECT_EQ(…, entt::null)` 编不过 | 改成 `EXPECT_TRUE(… == entt::null)`(18bcc8f603) |
| `bag_test` 16 条既有失败:`BagTest.DelItem`、`BagTest.LayerConsistencyPredicateActuallyDetectsBreakage`、`GuidSourcePolicyTest.*` 1、`BagSegmentNotReadyTest.*` 1、`BagRemoveExactTest.*` 2、`BagSelectionOrderTest.*` 5、`PendingSettlementContract.*` 1、`PlayerMissionSystemTest.*` 4 | 未处理。去掉本功能的用例后同样是这 16 条,属 ECS 重构第 3 批之后背包 / 任务 / 结算的既有问题,交对应负责人 |
| `go_services` 构建时 `player_inbox` 因 `go.sum` 缺条目编不出来(`GOPROXY=off`) | 未处理;启动器把它当可选服务跳过 |
| 本机 `mmorpg_friend.friend_request` 缺 proto 新声明的 `idx_friend_request_2(request_id)`,friend 拒启、启动器中止在网关之前 | 按 friend 报错给的修法在本机开发库补了这条普通索引(只加索引,不动数据) |

冒烟用的三个覆盖值只在 scene_manager 启动读配置期间临时打开,读入后立即还原,yaml 没有进任何提交。

## 10. 评审记录

落码之后做了三路只读评审(2026-10-09):C++「人肉编译」+ 逐条推演单测期望值、Go「人肉编译」+ 逐条推演用例、
跨语言契约与一致性。**评审不是编译,也不是运行**:结论是「没有读出编译错误」,不等于「编译通过」。

三路都没有读出会挡编译的问题。读出的逻辑缺口与处理:

| # | 问题 | 处理 |
|---|---|---|
| M1 | 购买先扣钱再发物;发物在预检之后仍可能被拒(物品号段不可用),退款走 `AddCurrency` 会被补缴欠款吃掉、遇币种封禁被拒 | 改成先发物再扣钱,取消退款路径(§5.2);加回归用例 `GrantRefusedAfterThePrecheckChargesNothing` |
| M2 | `round_id = now / 间隔`:间隔一调大版本号倒退,新一轮写不进 Redis,scene 节点也会把它当旧记录丢弃 | 轮次号改为出现时刻的秒数;间隔变了强制覆盖;scene 节点只在手里那一轮进行中才丢弃旧版本(§2.1) |
| M3 | 客户端按「上一次购买带回的余额」置灰购买按钮,之后赚了钱也点不了,且无法自己解开 | 余额只用于显示;买不买得起由服务端裁决(§6) |
| S1 | 商人在别的线、「前往」按钮灰着,不说原因,也不刷新线路目录 | 状态行显示原因;商店开着时每 5 秒刷新目录 |
| S2 | 查询失败后不重试(要等下一次换场景);在途时的新查询被吞掉;「购买结果未知」的提示会被后续查询盖掉 | 失败退避重试;在途时记「回来后再问一次」;该提示保留到重连 |
| S3 | 只核价格:配表热更把物品 / 数量 / 货币换了而价格没变时会按玩家没看到的内容成交;双击会直接确认 | 请求带报价四项;「确认购买」绑定报价;两次点击最小间隔 0.4 秒 |
| S4 | scene 节点不认 `weight = 0`,运营没有立即停售手段;`item_count` 超堆叠上限的行两侧都不拦 | scene 节点下发与结算都认 `weight`;两侧都校验堆叠上限(§3.2、§4) |
| S5 | 同一轮多笔购买共用 `correlation_id` | 不改,记入 §8 第 14 条 |
| S6 | 购买距离可被改客户端的人绕过 | 不改,记入 §8 第 10 条 |
| S7 | 整点后的头几秒 scene 节点还没读到新一轮,`next_appear_at_ms` 会答成再下一个整点;全服客户端到点后同一秒自动查询 | 服务端 30 秒宽限期;客户端显示「即将出现」,自动查询按角色 id 错开 0..19 秒、每个时刻最多 3 次 |
| S8 | 购买距离配小之后,客户端走到的停靠点在开店距离之外,店永远不开 | 停靠距离取 `min(3, 开店距离 − 0.5)` |
| S9 | 客户端按 `server_time_ms` 丢弃「更旧」的结论,跨节点换场景后新节点时钟略慢时会把新结论丢掉 | 新旧比较只在同一个场景内做;当前场景里问出来的应答一律采纳 |
| S10 | 不可用的线不换线 | 不改,记入 §8 第 11 条 |
| S11 | 升级顺序(迁移 → go/db → scene)没写明 | 记入 §8 第 12 条与 §9 第 6 步 |
| — | 站位表里写了本 zone 没有的地图时静默选不到线;分线目录地图号错位时仍会拿来选线 | 配表校验拒绝;目录错位报 `error`(§4) |
| — | 公告发给了没有会话的实体(每个一条 ERROR);跨节点换场景时同一次落位弹两次 | 服务端跳过没有会话快照 / 会话号无效的实体;客户端按(轮次, 场景实例)去重 |
| — | 改了间隔之后、新一轮开始之前,每拍打一条「间隔变了」的日志 | 只在真正覆盖写入时打一条 |

评审修正之后**没有再做一轮评审**;修正涉及的 C++ / Go 代码同样未编译、未运行,以 §9 的结果为准。

### 10.1 接到新 main 时的移植(2026-10-15)

分支原本基于改写前的历史(8c2ac2c440)。`origin/main` 在 10-12 整体改写历史、之后又多了 ECS 重构第 2–5 批、
gRPC / RPC 传输重构、玩家收件箱与存盘管线等提交。用 `git rebase --onto origin/main 8c2ac2c440` 把本分支唯一的提交接过去,
解决了 12 个文件的冲突,再按新接口逐处移植:

| 变化 | 本分支的处理 |
|---|---|
| 消息号 250–252 被装备占用,253–263 被落地摘要 / 收件箱占用 | 顺延到 264–266 |
| `player_database` 字段 18–20 被围栏 / 收件箱占用 | 限购记录改用 21 |
| tip 段 29000 被 `player_inbox_error` 占用 | 改用 30000(`TIP_BASE`),重跑 `tip-codes` |
| handler 生成模板去掉 `TRANSFER_ERROR_MESSAGE`、非 Empty 应答加 `static_assert` | 头文件按新模板重写;购买失败码直接写 `response->mutable_error_message()`(原先写全局 tip 实体,新模板下会**静默丢失错误码**) |
| `tlsEcs.globalRegistry` / `GlobalEntity()` 删除 | 记录缓存改为 `thread_local` 变量 |
| `Guid` 组件改名 `PlayerGuidComp`,日志统一 `ActorLog` | 日志身份改用 `ActorLog(player)`;单测夹具改挂 `PlayerGuidComp` |
| `SceneEntity` 成为独立句柄类型 | 单测夹具的场景句柄改为 `SceneEntity` |
| `BagService::AddItems` 去掉个人封禁参数、改为候选提交(失败一件不动) | 去掉个人封禁预检与「部分发放」分支;封禁用例改用全服封禁入口 |
| 存盘改为「先确认持久化组件都在,缺一个就整份拒存」 | 限购记录接进同一套检查;5 个测试夹具与拒存清单补上 |
| 日志文案统一为英文(10-13) | C++ / Go / 机器人的日志改英文,注释与错误字符串保持中文(与那次提交同一口径) |

这些当时只做了静态核对(接口逐个对照新代码、沙盒导表、protoc 语法检查、gofmt),没有编译;2026-10-16 的编译、单测与起栈冒烟结果见 §9.1。

## 11. Java 版(AGENTS §12)

**待做。** 本机没有 Java 仓:AGENTS §12 记的新路径 `D:\luyuan\xuanming-server-mmo` 与旧路径 `D:\luyuan\wuxingqitan\xuanming-server-mmo-java`
在本机都不存在(2026-10-15 核对,10-16 复核仍不存在),也就无法登记 `PARITY.md`。客户端可见契约的变化:

- `proto/scene/player_mystery_merchant.proto`(3 个 RPC 及其消息);
- `mystery_merchant_error` tip 域(30000–30011);消息号 264–266;
- 三张配置表的数据;
- 行为:§1.3 的 D1–D11、§5.2 的校验顺序(决定了负例拿到哪个提示码)。

Java 版做的时候按 §2、§3、§5 对齐,并在 `PARITY.md` 登记。
