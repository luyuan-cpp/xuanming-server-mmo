# PVE 人数档匹配:人越多怪越多

> 2026-10-09 起草,2026-10-10 按对抗复核修订 · 状态:**已落码,未编译、未跑测试**(AGENTS §10.1,验证清单见 §7)
> 相关:[turn-based-battle-server.md](turn-based-battle-server.md)(开局管线、PVE 数据化 §15)、
> [team-system.md](team-system.md)(整队开战 §E)、[cross-zone-matchmaking.md](cross-zone-matchmaking.md)(队列 key 与评分镜像)

## 1. 要解决的问题

改动前 PVE 只有两档,而且怪物只数与人数无关:

| 入口 | 人数 | 怪物 |
|---|---|---|
| `JoinQueue(PVE_SOLO)` | 1 人,即时开战 | 副本怪物组原样(副本 1 = 2 只) |
| `JoinQueue(PVE_TEAM)` | 必须凑满副本上限(副本 1 = 5 人) | 同上,还是 2 只 |
| `StartTeamMatch`(已组好的队) | 队伍现有人数(1..上限) | 同上,还是 2 只 |

结果是:手上只有两三个客户端时 `PVE_TEAM` 永远凑不满;五人队和单人打的是同一组怪。

目标:**PVE 可以按 1~5 人任意一档匹配,怪物数量随参战人数成倍增加** —— 每人对应一整组怪。
副本 1 的怪物组是 2 只:1 人 2 只(与改动前相同)、2 人 4 只 … 5 人 10 只。

## 2. 怪物只数:引擎按人数生成

规则(`TurnBattleEngine::InitMonsters`):

- 数**进攻方(team 0)玩家数** N。宝宝不算人头 —— 带宠不该让怪变多。
- 只数 = `N × 每人只数`,种类按副本怪物组(`DungeonTable.monster`)的原顺序循环取:
  怪物组 `[1, 2]`、每人 2 只、3 人 → `[1, 2, 1, 2, 1, 2]`。
- 只数上限 `kMaxScaledPveMonsterCount = 10`(阵位前后两排各 5,客户端战斗舞台每边也是 10 个站位),超出的不生成。
- 不消耗随机数:同一副本同一人数永远是同一批怪,回放基线不变。
- 怪物组为空的兜底路径不变(本来就是「按玩家人数生成默认怪」)。
- **活动对局不放大**:请求带 `activity_context`(`IsActivityBattle`,目前只有帮会「同道历练」)时怪物组原样生成。
  活动的难度、胜负与奖励口径是按固定一组怪定的(guild-phase2/06-activities.md),本功能没有理由去改它;
  既有的 guild-smoke 历练段(三人自动必胜)也依赖这一点。

「每人几只」经数据供给接口给出,不写死在引擎里:

```cpp
// BattleDataProvider
// 0 = 只数不随人数变,怪物组原样生成;N>0 = 只数 = 进攻方玩家数 × N
virtual uint32_t GetDungeonMonstersPerPlayer(uint32_t dungeonTableId) const = 0;
```

| 实现 | 返回值 | 说明 |
|---|---|---|
| `TableBattleDataProvider`(生产) | 该副本怪物组的只数 = **每人一整组** | `DungeonTable` 没有单独的「每人几只」列 |
| `MemoryBattleDataProvider`(单测) | 缺省 0,用例可 `SetDungeonMonstersPerPlayer` | 既有用例都是手写怪物组 + 手算回合数 / 奖励,缺省跟人数走会把断言全部作废 |

单测替身的缺省值与生产不同,所以生产取值另由真表契约用例
`TableBattleDataProviderTest.EveryPlayerFacesOneWholeMonsterGroup` 钉住。

### 2.1 为什么是「每人一整组」而不是「N 人 N 只」

第一版写的是 N 人 N 只(从怪物组头开始取 N 只),复核时发现它是个退步:

- 副本 1 = `[1, 2]`,单人只会遇到怪 1,**怪 2 要 2 人以上才出现**;改动前单人就能打到怪 2。
- 怪 2 是物品 11 的唯一掉落来源(`monster.json`),也是击杀条件 2 / 6 / 10 的目标(`condition.json`,
  scene 按结算里的 `defeated_monsters` 推进任务)。这些任务与掉落单人就做不了了。

「每人一整组」没有这些问题:**单人打到的与改动前逐只相同**(只数、种类、任务目标、掉落、难度基线都不变),
多一个人多一组,人均面对的怪与单人一样多。副本 1 的 5 人队正好 10 只,站满两排。

**连带变化(有意的)**:

- 整队开战(`StartTeamMatch`)走的是同一个引擎,同样按人数成倍增加。改动前它无论几人都只打一组。
- 组队不再是「白送」:人均难度与单人相同,而气血是带出战斗的(战后不回满)。单人连打本来就会越打越残,
  组队现在是同一个节奏;客户端「连续战斗」会带着人数档自动重排,残血连打到第三场左右会输。
- **每人的产出都随怪物只数累加**:经验 / 金币是实际打死的每一只之和;掉落是每人、每只、每槽各掷一次(`RollDrops`);
  任务击杀进度每只记一次。每个存活成员各得全额(既有的组队口径),所以 N 人队每人单场的经验、金币、掉落期望、
  击杀计数都是单人的 N 倍。这是「组队各得全额」叠加按人数放大的结果,要压的话改分配规则或调 `Monster` 表,本次没有动。
- 改的是引擎行为、不是配表:配表指纹不变,但 **scene 与 battle 仍应同批替换**(battle 是唯一跑引擎的进程)。

**为什么不给 Dungeon 表加列**:加列要导表(四种语言的生成物 + 客户端表部署 + Java 版同批),而当前开放组队的副本
只有副本 1(match 的 `PveTeamSizeByConfigId` 只配了 `"1": 5`,历练的 `GuildActivity.dungeon_id` 也是 1)。

**已知例外:副本 3 是首领本,开放组队之前必须先加列。** 副本 3 = `[11, 12, 16]`,16 号是首领(12130 血),
数值按「几个人打这一组」定(player-attribute-allocation.md §8)。按每人一整组,3 人会面对 3 个首领;4、5 人因 10 只封顶
拿到同一批怪。当前不可达,但在给 `PveTeamSizeByConfigId` 加 `"3"`、或把人数上限改成查 `DungeonTable.max_team_size`(backlog P2-05)
之前,必须先给 `Dungeon` 表加「每人只数 / 固定阵容」列,让 `TableBattleDataProvider::GetDungeonMonstersPerPlayer` 按行返回
(首领本返回 0 = 原样生成),并同批改 `EveryPlayerFacesOneWholeMonsterGroup` 的断言。

### 2.2 全自动回合间隔随单位数放宽

全员挂机的房间不等 6 秒行动窗口,按固定间隔推进(D13,原先恒为 `kAutoRoundIntervalMs = 2000`)。客户端要在这个间隔里
把刚结算的那一回合演完:每个出手单位约 0.9 秒、压缩上限 6 倍、收尾余量 0.3 秒(`PlaybackBudget`),演不完就整回合跳过
(只剩血条跳变)。固定 2 秒只容得下约 11 个单位;按人数放大后一局最多 5 人 + 5 宠 + 10 怪 = 20 个单位,
4~5 人档全自动时每回合都会被跳过。

所以全自动间隔改为 `AutoRoundIntervalMsFor(N) = max(2000, N × 250)` 毫秒,N 是刚结算那一回合排定出手的单位数
(`TurnBattleEngine::LastActionOrder().size()`;开局首次装填为 0):

| 出手单位数 | 间隔 | 说明 |
|---|---|---|
| ≤ 8 | 2000 ms | 与改动前相同(单人、2 人档、1v1 PVP 等) |
| 10 | 2500 ms | 5v5 PVP 全自动比原来慢 0.5 秒 |
| 15 | 3750 ms | 5 人 + 10 怪 |
| 20 | 5000 ms | 5 人 + 5 宠 + 10 怪 |

手动局的 6 秒窗口不变。客户端不用改(它读服务端下发的 `action_deadline_ms`)。

## 3. 人数档匹配:match 侧

### 3.1 协议

```proto
message JoinQueueRequest {
  ...
  // 只有 MATCH_MODE_PVE_TEAM 读它:想凑成几人的队伍
  //   0 = 该副本的人数上限(加字段前的行为)   1 = 不排队,即时开战
  //   2..上限 = 进这个人数档的队列,凑满即开   超过上限 = 拒绝(kMatchModeNotOpen)
  uint32 team_size = 7;
}
```

- 上限 = `PveTeamSizeByConfigId[battle_config_id]`,按引擎每队上限 5 收口(原有配置,含义从「凑满人数」变成「人数上限」)。
  本地 yaml 是 `"1": 5`,即副本 1 开放 1~5 人五个档。
- 没有新消息号、没有新 tip 码:超上限复用 `kMatchModeNotOpen`(16002),未配置的副本仍是 `kMatchTeamSizeNotConfigured`(16003)。
- 老客户端不填 `team_size` → 0 → 上限档,与改动前行为一致。

### 3.2 队列 key

同一副本的每个人数档是一条独立队列,人数写在 key 里:

```
match:{mq}:queue:{mode}:{config}              不带人数段:PVP 各模式(不变)
match:{mq}:size{N}:queue:{mode}:{config}      PVE 人数档 N
match:{mq}:size{N}:rank:{mode}:{config}       同档的评分镜像
match:{mq}:size{N}:lock:{mode}:{config}       同档的凑单锁
```

- 人数段放在 hash tag 之后、`queue` 之前:`parseQueueKey` / `rankKeyForQueue` 只认尾部三段,不用改;
  仍带 `{mq}`,与注册集同 slot,多 key Lua 在集群下不 CROSSSLOT(`TestSizedQueueKeyRoundTrip` 按 slot 断言)。
- 凑满人数从 key 读回(`queueTeamSize`),不再去问配置 —— 「入队时算一次、弹组时再算一次,两边必须同口径」
  这条隐含约束对人数档不存在了。配置只管上限。
- 凑单锁由队列 key 推出(`lockKeyForQueue`)。不带人数段的队列得到的仍是 `matcherLockKey(mode, config)`。
- gather 失败回队首用票据里记的 `queue_key`;兜底 key(票据没有该字段时才用)按组大小还原出人数档 key。
- **成员校验核对票据的 `queue_key`**(`groupPicker.validate`):队列里的 list 项若属于「现在排着别的队列」的人
  (没取消就离开、票据过期后改排了别的档;或取消时出队失败),只摘掉这条残留、不动他的票据,不会把他拉进没选的人数档。
  这条同时堵住了既有的跨模式 / 跨副本同类误弹。`queue_key` 为空的旧票据不做这项核对。
- 排查「某个人数档卡住」最稳的入口是 `SMEMBERS match:{mq}:index`(含全部人数档队列 key);
  `--scan --pattern 'match:{mq}:queue:*'` 匹配不到人数档。

**上限被调低 / 配置被摘掉之后**:`requiredPlayersForQueue` 对人数档超过当前上限的队列返回 0,该队列
不弹组(不把超编的一组人送去冻结再被引擎拒绝)、排队数据不动、告警限频(每队列 10 秒一条)。玩家取消后可以改排别的档;
成员都主动取消、list 变空之后,matcher 把它从注册集摘掉并把 `queue_depth` 归零(`TestOverLimitSizedQueuePrunedOnceEmpty`)。
已知边界:没取消就离开的人(掉线 / 关客户端,票据 6 小时后过期)留下的 list 项在这个分支不清 —— 成员校验只在弹组路径上跑,
所以这种队列会留在注册集、告警每 10 秒一条,直到上限恢复且该档再次凑够人数,或人工删掉这条 list 与同档的 rank key。

**升级注意**:

- **match 必须整批替换,不能新旧并存**。旧实例不认识 `team_size`:落到旧实例的 `PVE_TEAM` 请求(含不填的)都会成功返回,
  但被写进不带人数段的 `match:{mq}:queue:5:<config>`、按副本上限等人,与新实例的 `size{N}` 队列不是一个池;
  旧 matcher 还会按上限去凑人数档队列(把 2 人档里的人攒到 5 个才弹),而且用的是不带人数段的锁、与新实例不互斥。
  项目未上线,没有做新旧混跑的兼容。
  - K8s:match 是 2 副本、默认滚动更新,常规发布必然新旧并存。本次发布先
    `kubectl -n mmorpg-infra scale deploy/match --replicas=0`,等旧 Pod 真正消失
    (`kubectl -n mmorpg-infra wait --for=delete pod -l app=match --timeout=60s`)再 apply 新镜像;
    或本次临时给 match 的 Deployment 加 `strategy: {type: Recreate}`,发布后去掉(常态要保留 2 副本滚动)。
  - 本机多实例:用 go_services 全停再起,确认没有旧 match 进程。
- **服务端先于或同批于客户端**。服务端(match + battle)没带本改动时,新客户端发的 `team_size` 被当成未知字段忽略:
  2~5 人档一律进旧的上限队列,2~4 人档看起来就是一直排不出来(面板却显示「等待凑满 N 人」),没有任何报错或 tip;
  任何档怪物都不翻倍。1 人档走 `PVE_SOLO`,不受影响。
- 改动前(或混跑窗口内由旧实例)写下的 `PVE_TEAM` 队列(不带人数段)仍会被新 matcher 按上限凑满弹走,但新入队一律进人数档 key,
  两边不是一个池子:旧队列里不满上限的人要自己取消重排(或等票据 6 小时过期)。同样因为未上线,没有做搬迁。

### 3.3 行为

| 请求 | 结果 |
|---|---|
| `PVE_SOLO` | 不变:即时开战 |
| `PVE_TEAM` + `team_size = 1` | 与 `PVE_SOLO` 同一条即时开战路径(matched 票、不入队、失败不回队列) |
| `PVE_TEAM` + `team_size = 2..上限` | 进 N 人档队列,等待序(不看评分)凑满 N 人即开;开局失败时肇事者出局、其余回队首(原有补偿) |
| `PVE_TEAM` + `team_size = 0` | 等同上限档,与显式填上限的人在同一条队列 |
| `StartTeamMatch` | 不变:队伍现有 k 人直接开战,不补位;引擎给 k 组怪 |
| `StartActivityBattle`(帮会历练) | 不变:怪物组原样,不随人数放大 |

取消、票据状态机、matched TTL、跨 zone、观战索引沿用原有实现(它们按票据里记的 `queue_key` 与组大小工作);
matcher 侧新增的只有 §3.2 的两处:成员校验核对 `queue_key`,以及不能凑单的队列的限频告警与空队列剔除。

### 3.4 指标

`match_queue_depth`、`match_starved_anchor_wait_seconds` 增加标签 `team_size`(`"2".."5"`;没有人数档的队列是 `"0"`)。
不加的话同一副本的几个档会互相覆盖同一条时间序列。取值只有个位数种。仓库里没有引用这两条指标的看板 / 告警规则;
外部若有按 `{mode, config}` 精确匹配的查询,需要加上 `team_size` 或改成聚合。

`match_join_queue_total` 的 outcome 新增一个取值 `team_size_over_limit`。

## 4. 客户端(mmorpg-client)

- `BattleQueuePanel`:原「匹配 PVE(单人)/ 匹配 PVE(组队5人)」两个按钮换成一排人数档(1人 … 5人)+ 一个匹配按钮。
  选中的档换古铜色底并在文案前加「√」(U+221A;面板字体 SimKai 没有「✓」的字形);
  匹配按钮文案是「N人 · 怪物×N」(客户端不知道一组怪有几只,只写倍数)。
  1 人发 `PVE_SOLO`,2 人起发 `PVE_TEAM + team_size`。
- `BattleClient.JoinQueue(mode, battleConfigId, teamSize = 0)`:人数档进请求,并记进连续战斗的重排参数。
- `BattleUiStyle.PveTeamMaxSize = 5`:必须与服务端该副本的上限一致。
- `MatchService.cs` 用 protoc 重生成。
- 被拒时客户端只显示编号:toast「错误:加入队列失败(tip=16002)」(客户端还没有 tip 表加载器,不显示中文原因)。
- 已组好队的不走这个面板:队长在队伍面板点开战(原有入口)。

## 5. 没做的(有意)

- **队伍补位匹配**:已组好的 3 人队想再匹配 2 个路人凑 5 人。需要「不可拆的整队票据」进队列、队伍进入匹配态后
  成员变动要撤单、客户端队伍面板要显示匹配中,是 team-system.md 里明确推迟的 v2(§E「v1 不补位」)。
  现在的替代办法:继续邀请人进队,或者按现有人数直接开战。
- **按副本配置每人只数**:见 §2.1;副本 3(首领本)开放组队之前必须先做。
- **组队奖励 / 掉落分配**:仍是每人全额,见 §2.1。
- **不能凑单的队列里死成员的清扫**:见 §3.2 的已知边界。
- **等待时长预估**:`GetQueueStatus.estimated_wait_seconds` 仍是 0(原有 backlog)。
- **robot 冒烟场景**:没有新增「两个机器人排 2 人档」的自动化场景;`robot/vendor` 里的协议副本已同步。
  人工验收步骤见 §7.4。既有的 battle-smoke(单人)、team-smoke、guild-smoke 历练段不需要改:单人与活动对局的怪物只数没变,
  team-smoke 不断言怪物只数与胜负。
- **Java 版(AGENTS §12)**:未做,本机没有 Java 仓库。要同步的客户端契约只有 `JoinQueueRequest.team_size = 7`;
  「每人一整组怪」、人数档队列与全自动回合间隔是服务端内部实现,Java 版按自己的方式做。需在 `PARITY.md` 登记待做。

## 6. 改动清单

| 区域 | 文件 |
|---|---|
| 协议 | `proto/match/match_service.proto`;三份暂存副本 `generated/proto/{_unified,db,login}/proto/match/match_service.proto` |
| 协议生成物 | `go/proto/match/match_service.pb.go`、`robot/vendor/proto/match/match_service.pb.go`、`cpp/generated/proto/match/match_service.pb.{h,cc}`(protoc 窄面重生成:先用改动前的 proto 生成,与入库产物逐字节一致,再生成新的;`_grpc.pb.go`、message_id 未变) |
| 补生成的既有欠账 | `cpp/generated/proto/battle/battle_node.pb.{h,cc}`:proto 没改,是入库产物落后 —— `CreateBattleRequest.activity_context = 9` 在 09-29 加进 proto,C++ 产物还停在 09-06。引擎现在要读这个字段(§2 活动对局不放大),所以同样用 protoc 窄面重生成(用 09-06 的 proto 生成与入库产物逐字节一致);与日后全量 proto-gen 的输出相同 |
| match | `internal/logic/{keys,joinqueuelogic,matcher,gather,team_battle}.go`、`internal/metrics/metrics.go`、`internal/constants/errors.go`(注释)、`internal/config/config.go`(注释)、`etc/match_service.yaml`(注释) |
| match 测试 | 新增 `internal/logic/pve_team_size_test.go`(10 条);`rating_match_test.go`(队列 key 换成人数档)、`ticket_cas_test.go` / `rating_review_fix_test.go`(指标桩多一个参数) |
| 引擎 | `cpp/libs/services/battle/{constants/turn_battle_constants.h, data/battle_data_provider.h, data/table_battle_data_provider.{h,cpp}, system/turn_battle_engine.cpp}` |
| battle 节点 | `cpp/nodes/battle/logic/battle_room_manager.cpp`(`ArmRoundTimer` 的全自动间隔) |
| 引擎测试 | `cpp/tests/turn_battle_engine_test/{memory_battle_data_provider.h, turn_battle_engine_test.cpp(+8 条), table_battle_data_provider_test.cpp(+1 条)}` |
| 文档 | 本文;`turn-based-battle-server.md`(§15 指引、D13 / D14 更正、key 表);`cross-zone-matchmaking.md`(key 表、配置漂移、回滚通配);`turn-battle-presentation.md` §6.1;`tools/scripts/gen_docs_index.py`(「战斗」分类补 `pve-*` 规则)与 `docs/README.md` 索引 |
| 客户端 | `Assets/Scripts/{Game/Battle/BattleClient.cs, Game/Battle/Presentation/PlaybackBudget.cs(注释), UI/Ugui/Battle/BattleQueuePanel.cs, UI/Ugui/Battle/BattleUiStyle.cs, Proto/Generated/MatchService.cs}`、`Assets/Tests/EditMode/Battle/BattleClientAutoBattleTests.cs`(+1 条) |

没有新增 C++ 源文件,vcxproj / CMake 不用动。

## 7. 给 Codex 的验证清单

工作树 `E:\work\xuanming-server-mmo-wt-pvematch`,分支 `feat/pve-team-size-match`。
这棵工作树的 `third_party` 子模块是空的:C++ 要么先 `git submodule update --init --recursive`,要么合进主仓后在主仓编。
以下命令都在 **PowerShell(pwsh)** 下执行。

已有的静态证据(不能代替下面的运行):Go 过了 gofmt;三轮只读复核逐条走查了新旧用例。第二轮(6 个方向 + 逐条反驳核实)
确认的问题已改;第三轮只审第二轮之后新写的代码(活动对局不放大、§2.2、§3.2 的 `queue_key` 核对及其用例、客户端选中态),
确认 4 条并已改:入库的 `battle_node.pb.h` 落后于 proto(已补生成,见 §6)、「✓」在面板字体里没有字形(换成「√」)、
选中底色与一处注释。第三轮之后只改了这四处,没有再复核。

**分两步验收**:

- **只换 match(Go)就能验人数档匹配**。gate / scene / battle 不重编也行:老 gate 会把不认识的 `team_size` 字段原样转发
  (proto3 保留未知字段),老 battle 照常开局 —— 只是怪物还是固定一组、全自动间隔还是 2 秒。§7.1 做完、重启 match 即可走 §7.4 的 1~5 步
  (怪物只数按「固定一组」看)。
- **怪物随人数成倍、全自动间隔放宽要等 C++ 重编**,而主干的 C++ 目前本来就编不过:战斗引擎早已依赖装备属性系统的协议
  (`CombatAttributes`、`hit_kind`、`BATTLE_EVENT_RESIST` 等),这些的 C++ 生成物还没有产出。先做
  [equipment-attributes.md](equipment-attributes.md) §9 的导表与全量 proto-gen(第 3、4 步),再做下面的 §7.2。

### 7.1 Go(先做,最快)

Go 1.26.5 在模块缓存里、不在 PATH:
`C:\Users\luyua\go\pkg\mod\golang.org\toolchain@v0.0.1-go1.26.5.windows-amd64\bin`,环境变量 `GOTOOLCHAIN=local`。

| 步骤 | 工作目录 | 命令 | 通过标准 |
|---|---|---|---|
| 1 | `go\match` | `go build ./...` | 退出码 0 |
| 2 | `go\match` | `go vet ./...` | 退出码 0 |
| 3 | `go\match` | 见下方代码块 | 10 条全过 |
| 4 | `go\match` | `go test ./... -count=1` | 没有既有用例变红(只需 miniredis,不要外部依赖) |
| 5 | `robot` | 见下方代码块 | 退出码 0(别覆盖仓内 `robot.exe`) |

第 3 步(正则里的竖线不要加反斜杠,否则一条都匹配不到、退出码仍是 0):

```powershell
go test ./internal/logic/ -run "TestSizedQueue|TestRequiredPlayersForQueue|TestPveTeamSize|TestQueueDepthReportedPerTeamSize|TestOverLimitSizedQueue|TestStaleEntryFromOtherTier" -count=1 -v
```

第 5 步:

```powershell
go build -mod=vendor -o "$env:TEMP\robot_pve.exe" .
```

失败时保留:首个编译错误连同上下 20 行;测试失败保留该用例的完整输出。
最可能先出错的位置:`pve_team_size_test.go`(全新、从未编译);`matcher.go` 的 `matchQueueOnce`
(`required == 0` 分支里的 `depth, err :=` 遮蔽了外层 `err`,语法合法,但开了 shadow 检查的 vet 会提示)。

### 7.2 C++(MSBuild 必须串行 `/m:1`)

每个工程:

```powershell
msbuild <工程> /m:1 /p:Configuration=Debug /p:Platform=x64
```

前置:equipment-attributes.md §9 的导表 + 全量 proto-gen 已完成(见上)。只能逐文件窄面生成的环境,清单里要有装备涉及的 proto;
本分支已经带上 `match_service.pb.*` 与 `battle_node.pb.*`,这两份不用再生成。

按序(`JoinQueueRequest`、`CreateBattleRequest` 各多了一个字段、对象变大,所有按值使用它们的库都要重编 —— 只重编 proto 的话链接不报错,运行期内存损坏):

1. `cpp\generated\proto\proto.vcxproj`
2. `cpp\generated\rpc\rpc.vcxproj`
3. `cpp\generated\grpc_client\grpc_client.vcxproj`
4. `cpp\libs\services\battle\battle.vcxproj`
5. `cpp\nodes\battle`
6. `cpp\nodes\gate`、`cpp\nodes\scene`(重新链接 proto / rpc / grpc_client)
7. `cpp\tests\turn_battle_engine_test\turn_battle_engine_test.vcxproj`

链接节点之前确认 `rpc.lib`、`grpc_client.lib` 的修改时间晚于 `proto.lib`。

| exe | 过滤器 | 通过标准 |
|---|---|---|
| `turn_battle_engine_test.exe` | `TurnBattleEngineTest.Pve*:TurnBattleEngineTest.AutoRoundInterval*` | 8 条:`PveMonsterCountFollowsTeamSize`、`PveWholeGroupPerPlayerRepeatsGroupForEachPlayer`、`PveActivityBattleKeepsMonsterGroupUnscaled`、`PveMonsterGroupStaysFixedWhenMonstersPerPlayerIsZero`、`PveMonsterCountIgnoresPets`、`PveScaledMonsterCountCapsAtTwoFormationRows`、`PveScaledMonstersAllCountTowardRewards`、`AutoRoundIntervalScalesWithActedActorCount` |
| 同上 | `TableBattleDataProviderTest.*` | 全绿,含新增 `EveryPlayerFacesOneWholeMonsterGroup`(需要能定位 `bin/etc`) |
| 同上 | 不带过滤 | **既有用例一条都不该变红** —— 单测替身缺省不放大,既有用例的怪物只数与改动前相同;有变红说明缺省路径被动到了 |

`battle.vcxproj` 的 Debug|x64 是 `/W3` + 警告即错误。`PveScaledMonstersAllCountTowardRewards` 若红:保留输出里三名玩家各自的
`defeated_monsters_size / exp_gain / gold_gain`,它依赖「力量 20 的玩家一刀 30、怪 30 血」这条既有用例里的伤害口径。

### 7.3 客户端

- 客户端本机 main 自 `537c2bad`(2026-10-10 的每小时自动保存,未推 origin)起已带人数档面板并依赖 `JoinQueueRequest.TeamSize`;
  10-10 复核后的三处小改(选中态、注释)在其后的工作区 / 自动保存里。
- **服务端分支并入服务端 main 之前**,若有人照常用 `-ProtoRoot E:\work\xuanming-server-mmo` 重跑 `tools/gen_proto.ps1`,
  生成的 `MatchService.cs` 会丢掉 `TeamSize`,客户端整体编不过(报错在 `BattleClient.cs`,与对方做的事无关)。
  那种情况下跑完立刻 `git checkout HEAD -- Assets/Scripts/Proto/Generated/MatchService.cs` 还原这一个文件。并入之后无此问题。
- 已由 Claude 跑过的离线编译(Roslyn,不是 Unity):运行时程序集、`EditMode.Battle` 测试程序集(按目录实际 30 个文件)均 0 error。
- 待跑(需要 Unity 许可证):`Unity.exe -batchmode -nographics -projectPath E:\work\mmorpg-client -runTests -testPlatform EditMode -testFilter BattleClient -testResults <xml> -logFile <log>`(不带 `-quit`),
  通过标准:`BattleClientAutoBattleTests.ContinuousBattle_RejoinKeepsPveTeamSize` 及既有用例全绿。
- 面板目视:打开回合战斗面板,人数档一排 5 个按钮不重叠;选中档底色明显不同(古铜色)、文案带「√」;匹配按钮文案随选择变化。
  选中底色 `BattleUiStyle.PveSizeSelectedPlate` 是按贴图像素估算的,**没有人目视确认过**;观感不对只改这一个常量。

### 7.4 联机验收(两个客户端,同一个区)

前置:**没有旧 match 进程**;`go/match/etc/match_service.yaml` 的 `PveTeamSizeByConfigId` 含 `"1": 5`;客户端用带人数档面板的版本。
副本 1 的怪物组是 2 只(怪 1、怪 2)。下面的怪物只数是 match / battle / scene / gate 全部换成新二进制之后的预期;
只换了 match 的阶段,每一步对面都还是 2 只,其余预期不变。

1. **1 人档**:A 选「1人」点匹配 → 立即开战,对面 **2 只怪**(怪 1、怪 2,与改动前一样)。
2. **2 人档排队**:A、B 都选「2人」点匹配 → 第二个人点下去后约 1 秒内两人进同一场战斗,对面 **4 只怪**(怪 1、怪 2、怪 1、怪 2)。
   match 日志应有 `[matcher] 凑单成功 queue=match:{mq}:size2:queue:5:1`。
3. **不同档不串**:A 选「2人」、B 选「3人」各自排队 → 都不开战;B 取消改排「2人」→ 开战。
4. **整队开战**:A 建队邀请 B,队长在队伍面板点开战 → 两人进同一场,对面 **4 只怪**。
5. **超上限**:把 yaml 改成 `"1": 3` 重启 match,客户端选「5人」→ 红色 toast「错误:加入队列失败(tip=16002)」
   (16002 = `kMatchModeNotOpen`;排队面板状态行显示「加入队列失败(tip=16002)」),面板回到空闲、不进队列。
6. **全自动节奏**(可选,需要 4~5 个客户端或带宠):全员开自动后每回合仍有出手演出,不出现「跳过本回合演出」。
