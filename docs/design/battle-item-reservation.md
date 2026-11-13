# 战斗道具预留(两阶段:先预留,再扣减或作废)

> 状态:2026-11-10 落码,**未编译、未测试**,待 Codex 验证(§9)。负责人 2026-11-10 拍板「按预留方案做」。
> Java 版(0.1.0-SNAPSHOT)待做,登记在 Java 仓 `PARITY.md`(§10)。
> 2026-11-12:原先记在 §7 的两条残余已做,同样未编译 —— 两个背包都放不下的掉落改投邮件([battle-overflow-mail.md](battle-overflow-mail.md)),
> 客户端看得到被押住的数量(§4.1)。
> 前文:[turn-battle-gap-closure.md](turn-battle-gap-closure.md) §11.2「FIGHTING 冻结到期先查 SQL 终局」与其中「2026-11-10 复核」一段。

## 1. 要解决的问题

回合制战斗在 battle 节点上按 scene 给的**快照**打:备战时 scene 把人物背包里可参战的道具按配置号与数量抄一份给 battle
(`PlayerBattleSystem::BuildBattleSnapshot`),战斗里喝掉多少由 battle 记账,结算经 Match SQL → 发件箱 → 收件箱回到 scene,
scene 再按结算从背包里扣(`ApplySettlementToEntity`)。从开局到结算应用这段时间,快照里那些道具必须还在背包里,否则结算只能
按当时的持有量夹紧 —— 玩家在战斗里喝了药,却把同一批药先经交易、帮会捐献或邮件转走,等于「药白喝」,可以逐局刷。

此前挡住这件事的只有**冻结**(`InBattleComp`):冻结期间资产通道、换场景、跨 zone、强化等都按「在战」拒绝。冻结有期限:
截止 + 60s 终局宽限 + 60s 投递余量到期,到期先查 SQL 终局,COMPLETE 而本地还没应用、或终局还没落定就续冻,最多续 15 分钟
(`kFreezeHoldCapMs`)。上限之后照常解冻,晚到的结算按持有量夹紧。残余有两种来源:

- Match 发件箱积压超过约 17 分钟,结算更晚才到;
- **玩家可控**:两个背包都满,结算整笔延后(放不下掉落),玩家熬过上限、解冻之后把药转走,再取回临时格让结算应用
  (`bag-rule-policy-layering.md` §(7) 记过)。

不能简单去掉上限(2026-11-10 复核,详见 `turn-battle-gap-closure.md` §11.2):冻结期间玩家腾不出位置(整理、穿脱、神秘商人、
资产通道都拒,背包没有使用 / 丢弃 / 出售入口),两包全满时冻结若不随上限解开就永远解不开;FIGHTING 锁也活不过离线期。

## 2. 做法

**冻结管「人在不在战斗里」,预留管「这一局带进战斗的东西还押着」。** 备战时把快照里的道具按配置号与数量记成一份预留,
结算只从这份预留里扣,扣完连同余量一起释放;这一局作废或永远不会有结算时整份释放。冻结仍按原规则到期、续冻、按上限解开 ——
解开之后玩家能正常游戏,只是这几样道具转不走、用不掉,晚到的结算照样扣得出。这是「远端按快照执行、事后结算」的标准两阶段做法。

| 项 | 内容 |
|---|---|
| 预留什么 | 快照里的道具副本:人物背包里 `battle_usable`、可按配置号计数(`ItemStore::IsCountableByConfig`)的单位,按配置号求和,**扣掉别的局已经押着的数量** |
| 存在哪 | 持久组件 `PlayerItemHoldComp`(`proto/common/component/player_item_hold_comp.proto`),落在 `player_database.item_holds`(字段 26,PROFILE 组)。与背包同一次存档、同一个整笔序号提交 |
| 规则在哪 | `cpp/libs/modules/bag/item_hold.h`(纯函数:累计、增删、加载校验);`BagService`(唯一闸口,`bag_service.h`「物品数量预留」一段) |
| 只约束 | **这名玩家的人物背包**(`kInventory`)里的可计数单位;仓库、装备栏、临时格、动态包不受影响 |
| 种类 | `ItemHoldKind`:今天只有 `kBattle`(`hold_id = battle_id`)。存 `uint32`,本版本不认识的种类照样计入占用 |

### 2.1 生命周期

| 时刻 | 动作 | 位置 |
|---|---|---|
| 备战(PrepareBattle)冻结之前 | `BagService::PlaceHold(battle_id, 快照道具, review_after = 冻结到期时刻)`;建不起来就整笔拒绝备战、不留冻结;建起来之后请一次关键进度存盘(1 秒内) | `PlayerBattleSystem::PrepareBattle` |
| 结算应用 | 消耗按「可计数总量 - 别的局的预留」扣(本局预留可用),成功后**同一帧**释放本局预留,随结算那一次存档提交 | `ApplySettlementToEntity` + `ConsumeInventoryAndGrant(consumeFromHoldId)` |
| match 取消备战(在线) | 摘冻结 + 释放预留 | `CancelBattlePrepare` |
| 作废结算应用(2026-11-12:match 的 ABORT 给每名成员投一份 VOID 结算,[battle-instance-loss-early-abort.md](./battle-instance-loss-early-abort.md) §5.2) | 释放预留(一个都不扣,reason = void_settlement),随收件箱账本那一次存档提交;冻结在存档确认后解开 | `ApplyVoidSettlement` |
| 冻结到期,SQL 终局 ABORT / 没有可裁决原件 | 解冻 + 释放预留 | `OnExpiredFightingTerminalAnswer` |
| 冻结到期,本地已应用过这一局 | 解冻 + 释放预留(兜底,正常已在结算时释放) | `ResolveExpiredFightingFreeze` |
| **续冻到上限** | **只解冻,不放预留** | 同上 |
| 备战到期(PREPARING 期限过) | 只摘冻结,预留留着(迟到的确认还可能让这一局开打) | `ReapExpiredFreezes` |
| 冻结已不在的预留 | reaper 每拍复核:到了 `review_after_ms`、玩家此刻可改,就查 SQL 终局;ABORT / 没有可裁决原件放,其余留着 | `ReviewBattleItemHolds` |

冻结已不在而预留还在的情形:续冻到上限解冻、备战到期、玩家离线时被取消(离线取消只删锁,预留在存档里)、重登时锁已过期。

### 2.2 冻结已不在的预留怎么收尾

`ReviewBattleItemHolds`(`ReapExpiredFreezes` 的最后一步)只看「种类是战斗、复核时刻已过、这一局的冻结不在本实体上」的预留:

- 玩家没登记在本节点、或此刻不可改(交接 / 退出中 / 没有归属围栏,`player_mutability`)→ 下一拍再看。释放预留是存档改动,
  交接完成后在目的地复核,退出后下次加载再复核。
- 这一局的结算已在本实体应用过(收件箱最近结局环)→ 释放(异常兜底)。
- 否则发一次 SQL 终局查询(`MatchInternal.GetBattleTerminalDecision`,与冻结到期**共用**在途登记与判丢失 `SendTrackedTerminalQuery`):

| 回答 | 处置 |
|---|---|
| ABORT | 释放 |
| NO_ORIGINAL(没有终局,也没有可裁决的原件) | 释放:COMPLETE 永远落不了库 |
| COMPLETE | 留着:结算迟早到,到了从预留里扣 |
| PENDING(有原件而终局还没落定) | 留着 |
| UNKNOWN(没发出去 / 传输失败 / 验签不过 / 在途丢失) | 留着,下一拍再问 |

**没有上限**(`battle_freeze_hold::IsBattleHoldReleasable`):留着只占住这一局带进战斗的道具,玩家其余的东西照常能动,
不会像冻结那样把人卡住;放早了,晚到的结算就只能按持有量夹紧。越过「复核时刻 + 15 分钟」仍没收尾时每 10 分钟一行
ERROR `metric=battle_item_hold_overdue`(`IsHoldOverdueLogDue`)。

回答在途期间这一局的冻结又挂上了(迟到的确认、按锁重建),回答交给冻结那条路,预留复核不动它。

## 3. 不变量

1. 人物背包里每个配置号的可计数总量 ≥ 全部预留里这个配置号的数量之和。只有结算扣它**自己那份**预留时可以低于其余预留之外的部分。
2. 预留只在「不会再从它扣」的时刻释放:结算已应用、备战被取消、SQL 终局 ABORT、没有可裁决原件。按期限解冻不释放。
3. 结算只能动用自己那份预留(`consumeFromHoldId = battle_id`),别的局押着的数量照样不许碰。
4. 战斗快照不含别的局押着的数量:两局不会把同一瓶药各算进自己的副本。
5. 读缺失按「没有预留」(与 `PlayerItemGainBlockComp` 同一类有记录的例外,`ecs-component-access-rules.md`);写缺失拒绝;
   存盘缺失整份拒存。缺组件的实体存盘被整份拒绝,资产通道与收件箱只在落盘后终结,拿出去的东西成不了盘上事实。

## 4. 各出包入口

| 入口 | 被预留挡住时 | 调用方 |
|---|---|---|
| `BagService::CheckRemovable`(按数量预检) | 回 `RemovalVerdict::kHeld`(优先级:确定不足 > 版本错位 > 被预留占住 > 够) | 资产通道扣物品、强化扣材料、镶嵌扣宝石 |
| `BagService::CheckRemovableInstances`(按实例预检) | `kHeld` | 资产通道按 guid 扣、穿上装备 |
| `RemoveItemsExact` | 整批拒、一件不扣,`kBagInsufficientItems` + ERROR(预检之后状态变了才会走到) | 强化、镶嵌、资产通道、GM 回收 |
| `RemoveItemsByGuid` | 整批拒、一件不扣,`kAssetInBattle`(RETRY 类,`mutated=false`) | 资产通道、GM 回收 |
| `RemoveItemsClamped` | 夹到可出包数量 | 无生产调用方 |
| `RemoveItem` | 拒,`kBagInsufficientItems` | 无生产调用方 |
| `ConsumeInventoryAndGrant` | 消耗夹到「总量 - 别的预留」;`consumeFromHoldId` 那一份可用 | 战斗结算 |
| `PlayerEquipSystem::Equip`(穿上 = 把这一件移出人物背包,`Bag::TakeInstance`) | 纯校验段先问 `CheckRemovableInstances`,`kHeld` 回 `kFeatureUnavailable`、两包不动 | 穿戴协议 |
| 整理 / 取回临时格 / 入包 / 卸下装备 | 不受影响(不减少人物背包的数量) | — |

人物背包里减少数量的入口只有上表这些(2026-11-10 按源码核对):`Bag` 的扣除原语在生产代码里只由 `BagService` 调
(`RemoveItem` / `RemoveItems`;`RemoveItemByPos` 没有生产调用方),
`Bag::TakeInstance` 从人物背包取件只有穿戴一处(`Bag::MoveInstancesTo` 只用于临时格 → 人物背包)。今天的物品表里没有
既是装备又可参战的行,穿戴这道闸让不变量 1 不依赖这条配表约定。以后新加「移出人物背包」的入口(存仓库、丢弃、出售等),
必须在纯校验段问 `CheckRemovable` / `CheckRemovableInstances`。

调用方的处置:

- **资产通道扣款**(交易托管等):`kHeld` 与版本错位同一处理 —— 等确定性校验跑完,回 RETRY + `kAssetInBattle`、不记账
  (`metric=asset_debit_items_held`)。记成终态会让对侧白白退款或中止。
- **GM 回收**:按「可出包数量」收;不够而差额就在预留里时回 RETRY + `kAssetInBattle`(`metric=asset_recall_items_held`),
  等那一局结算或作废后收全。按实例回收时人物背包里至多挑可出包的件数。
- **强化 / 镶嵌**:回 `kFeatureUnavailable`(暂不可用),与版本错位同码。
- **穿上装备**:回 `kFeatureUnavailable`。押的是数量不是哪一件:两件同款押一件,穿哪一件都行,第二件不行。

这**不是**「局中挡一切背包写」的战斗闸:D48 红线照旧(战斗闸只放在玩法入口层),预留只管数量、不看 `InBattleComp`,
战斗结算扣的正是它自己那份,不会被自己拦住。

### 4.1 客户端看得到被押住的数量(2026-11-12)

`BagInfo.held_items`(字段 4,`BagHeldItemInfo { config_id, count }`)列出人物背包里每种物品被预留占住的数量,按 `config_id` 升序,
没有预留时为空。只有人物背包的快照带它:预留只管人物背包,临时格、仓库的 `BagInfo` 不带。

- 填充点只有一处:`player_feature_snapshot.cpp` 建人物背包快照时的 `FillHeldItems`。数量取 `BagService::HeldCount`,再按背包里这种物品的
  可计数总量夹紧;背包里此刻没有的种类不列。正常情况下预留不会比持有多(出包入口只动未预留的部分),夹紧只在不变量被破坏时起作用:
  给玩家看的是「包里这几样有多少动不了」,多出来的部分没有可标的格子。
- 它是给显示用的派生量,不落库,也不是第二份规则。能不能拿走仍只由服务端的 `CheckRemovable` / `CheckRemovableInstances` 判;
  客户端只用它把「可交易数量」算对、把原因写明白。
- 随人物背包快照下发(`GetBag` 与各背包操作的回包)。预留的变化不单独推送:客户端打开背包、聚宝斋选物品时拉到的是当时的值。
- 客户端(`../mmorpg-client`):`EquipBagModel.HeldCountOf / AvailableCountOf / AvailableInStack / HeldLine`;背包物品详情多一行
  「战斗结算中 N(结算后可交易)」;聚宝斋选物品时显示占用与可交易数量,只能选可交易的部分,全被押住的不能选。
- 用例:`cpp/tests/bag_test/item_hold_test.cpp` 的 `InventorySnapshotListsHeldQuantitiesForTheClient`;客户端
  `EquipForgeModelTests.HeldAndAvailableCountsFollowTheServerHeldItems`、`HeldLineOnlyShowsForKindsWithHeldQuantity`。

这是客户端可见的协议,两版共享:Java 版登记待做(§10)。

## 5. 持久化与兼容

- 新字段 `player_database.item_holds = 26`,PROFILE 组。新组件只在加载时创建(`PlayerDatabaseMessageFieldsUnmarshal`),
  存盘缺它整份拒存(`PlayerDatabaseMessageFieldsMarshal`)。行投影按描述符通用生成,不用另外登记;混版本台账由 proto 生成器发世代号。
- 加载时 `item_hold::Validate` 只做卫生检查:不合法的记录原样携带、照样计入占用,只打 ERROR `metric=item_hold_invalid`。
  清掉它等于把可能还在等结算的道具放出去,而它是不是坏数据要人看。
- 新的三个可携带类型(`PlayerItemHoldComp` / `ItemHold` / `ItemHoldLine`)进了往返一致性测试的夹具(`persist_round_trip_test.cpp`)。
- 项目尚未上线(2026-10-18 负责人确认),没有存量数据;旧存档没有这个字段 = 没有预留,合法。

## 6. 可观测性

scene 进程没有 Prometheus 端点,信号是结构化日志(英文):

| 日志 | 级别 | 含义 |
|---|---|---|
| `metric=battle_item_hold_released reason=…` | INFO | 预留释放;reason = settlement_applied / prepare_cancelled / void_settlement / ABORT / NO_ORIGINAL / applied_here |
| `metric=battle_item_hold_overdue` | ERROR | 预留越过「复核时刻 + 15 分钟」仍没收尾,每 10 分钟一行 |
| `metric=asset_debit_items_held` / `asset_recall_items_held` | WARN | 资产通道因预留回 RETRY |
| `metric=item_hold_invalid source=loader` | ERROR | 加载校验不过 |
| `metric=battle_freeze_hold_cap_reached` | ERROR | 续冻到上限解冻(文案已改为「道具仍押着」) |

值班固定查询(Loki,写法同 `deploy/k8s/asset-channel-alerts.yaml` 头注释):

```
count_over_time({job="cpp_nodes", service="scene"} |= "metric=battle_item_hold_overdue" [10m]) > 0
```

处置见 [player-inbox-runbook.md](../ops/player-inbox-runbook.md) §6.1「战斗道具预留」。match 一侧的发件箱积压告警
(`deploy/k8s/match-outcome-outbox-alerts.yaml`)是同一件事更早的信号。

## 7. 代价与残余

- 预留期间这一局带进战斗的道具转不走、用不掉(交易、帮会捐献、强化、镶嵌、GM 回收都暂不可用;穿戴同样挡,
  只在既是装备又可参战的行上会碰到,今天的表里没有);结算一到即放开。
  积压或 match 不可用越久,押得越久,有 `battle_item_hold_overdue` 告警。
- 备战时人物背包里**全部**可参战的道具都被押住(快照就是这么取的),不只是这一局真正会喝掉的那几瓶。
- 两个背包都满、而人物背包里全是被押着的满堆时,玩家腾不出位置。2026-11-12 起这一局不再延后:掉落整批改投邮件,
  扣消耗、入账、释放预留照常进行([battle-overflow-mail.md](battle-overflow-mail.md);负责人 2026-11-11 定「背包满是放到邮件里面的」)。
  只有邮件接不住时(没配发件密钥、待寄队列满、请求号暂缺)才回到整笔延后,预留随之继续押着。
- 玩家离线时被取消的备战:预留留到下次加载后由复核收尾(查到 ABORT / NO_ORIGINAL 即放)。
- 客户端看得到被押住的数量(§4.1,2026-11-12)。预留的变化不单独推送,界面上的数量是上一次拉背包时的值;
  以为能交易而被服务端挡住时,仍按 RETRY 之后的提示处理。
- HP / MP、宠物状态的覆盖时序仍由冻结与续冻管,预留不涉及。

## 8. 改动清单

| 类别 | 文件 |
|---|---|
| proto | `proto/common/component/player_item_hold_comp.proto`(新)、`proto/common/database/mysql_database_table.proto`(字段 26) |
| 背包 | `cpp/libs/modules/bag/item_hold.h`(新)、`bag_service.{h,cpp}`、`cpp/libs/modules/modules.vcxproj` |
| 存档 | `cpp/libs/services/scene/player/system/player_database_loader.cpp` |
| 战斗 | `cpp/libs/services/scene/battle/system/{player_battle.h, player_battle.cpp, battle_freeze_hold_rules.h}` |
| 资产 / 装备 | `cpp/libs/services/scene/player/system/{asset_op_system.cpp, player_equip.h, player_equip.cpp}`(强化 / 镶嵌预检、穿戴闸) |
| 测试 | `cpp/tests/bag_test/item_hold_test.cpp`(新)、`battle_freeze_hold_test.cpp`、`player_battle_settlement_test.cpp`、`persist_round_trip_test.cpp`、`player_feature_persistence_test.cpp`、`player_equip_attribute_test.cpp`、`player_equip_test.cpp`、`bag_test.cpp`、`bag_test.vcxproj(.filters)`;`cpp/tests/cross_zone_test/cross_zone_test.cpp` |
| 文档 | 本文、`turn-battle-gap-closure.md` §11.2、`bag-rule-policy-layering.md` §(7)、`player-inbox-runbook.md` §6.1、`ecs-component-access-rules.md`(持久数据表与读缺失例外)、`player-save-direct-commit.md` §1.1(资料行字段)、`deploy/k8s/match-outcome-outbox-alerts.yaml`(只改描述)、`deploy/k8s/asset-channel-alerts.yaml`(头注释固定查询:改上限那条的描述,加 `battle_item_hold_overdue`)、`docs/README.md` |
| 2026-11-12 显示占用数量 | `proto/scene/player_bag.proto`(`BagHeldItemInfo`、`BagInfo.held_items = 4`)、`cpp/libs/services/scene/player/system/player_feature_snapshot.cpp`(`FillHeldItems`)、`cpp/tests/bag_test/item_hold_test.cpp`;客户端 `Assets/Scripts/UI/Ugui/Gameplay/{EquipBagModel.cs, GameplayWindow.Fidelity.cs}`、`Assets/Scripts/UI/Ugui/Jubaozhai/CommerceWindow.cs`、`Assets/Tests/EditMode/Battle/EquipForgeModelTests.cs`。两包都放不下改投邮件的改动清单在 [battle-overflow-mail.md](battle-overflow-mail.md) §11 |
| 别的会话已改 | `go/match/internal/authority/terminal_abort.go` 里 `deadlineAbortHardCapMs` 的注释(「数据覆盖审计交接文档」会话 11-10 改,纯注释、未编译):scene 按上限解冻只放开人,道具仍由预留押着;晚到的 COMPLETE 从预留里扣,晚到的 ABORT / 没有可裁决原件只释放预留;等待上限决定的是「人被冻多久」。`kFreezeHoldCapMs` 没变,`TestDeadlineAbortBoundsFitSceneFreezeHold` 的不等式照旧成立 |

## 9. 验证(全部未执行,交 Codex)

仓库根目录,串行,失败不重试,保留首个错误与失败用例输出:

1. 协议生成(新增一个组件 proto、`player_database` 加字段):
   `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`。
   通过标准:生成 `cpp/generated/proto/common/component/player_item_hold_comp.pb.{h,cc}`,并已登记进 `cpp/generated/proto/proto.vcxproj`
   与 `CMakeLists.txt`;`proto/persist_schema_gen.txt` 出现 `player_database#26 … group=4` 与三个新类型的行;`go/proto` 下同名绑定生成。
   2026-11-10 23:37 核对:工作区里这些产物已经在了(23:08 有一次生成跑过,proto 源之后没再改),重跑一次确认没有差异即可。
2. `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`:0 个 `error C` / `LNK`,`/WX` 无新告警。改了 modules 与 scene 库,先编库再跑测试。
3. `build\cpp\tests\bag_test.exe --gtest_filter=ItemHoldRules.*:BagServiceItemHoldTest.*:BattleItemHoldRules.*:BattleItemHoldTest.*:BattleFreezeHold*:PlayerBattleSettlementItemTest.Inbox*Hold*:PlayerBattleSettlementTest.SnapshotItemsExcludeQuantitiesHeldByAnotherBattle:PersistRoundTripTest.*:PlayerFeaturePersistence*:PlayerEquipTest.EquipRefusesToTakeTheLastHeldUnitOutOfTheInventory:PlayerEquipTest.Enhance*:PlayerEquipTest.Inlay*`
   全绿,再跑一次完整 `bag_test.exe`;`currency_test.exe --gtest_filter=AssetOpSystemTest.*:AssetOpRecallTest.*` 与 `cross_zone_test.exe` 完整套件不回归。
4. `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`:match-outcome-outbox-alerts 用例照旧通过(只改了描述文字);
   「LogQL 契约」用例照旧通过(`asset-channel-alerts.yaml` 多了一条 `{job="cpp_nodes", service="scene"}` 的固定查询,写法与其余几条相同)。
5. `cd go/match && go build ./...`(只改了注释)。
6. 2026-11-12 的显示占用数量(§4.1)改了客户端可见的 proto,要重新生成服务端与客户端两边的绑定,再编译、跑
   `bag_test.exe --gtest_filter=BagServiceItemHoldTest.InventorySnapshotListsHeldQuantitiesForTheClient` 与客户端 EditMode 的
   `EquipForgeModelTests`。它与改投邮件同批验证,完整步骤见 [battle-overflow-mail.md](battle-overflow-mail.md) §12。

## 10. Java 版

Java 版的冻结与结算模型不同(断线即写回、重连新建实例,战斗锁在 Redis 哈希里),但「冻结解开之后晚到的结算按持有夹紧」是同一类问题,
要做同一组行为:备战时按快照占住道具、结算从占用里扣并释放、取消 / 作废时释放、出包入口只动未占用的数量、冻结不在时按终局复核。
登记在 Java 仓 `PARITY.md`「2026-11-10 战斗道具预留(Java 待做)」。本批没有改 Java 代码。
`BagInfo.held_items`(§4.1)是客户端可见的协议,Java 版要同批补上,与改投邮件的两个字段一起登记在 `PARITY.md` 2026-11-12 的条目里。
