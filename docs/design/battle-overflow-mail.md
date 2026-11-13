# 战斗掉落两包都放不下时改投邮件

> **状态**：2026-11-11~12 落码（服务端 C++ / Go 配置 / 部署脚本 / 客户端），**未生成协议、未编译、未测试**，待 Codex 按 §12 验证。Java 版 0.1.0-SNAPSHOT 待做（§13）。
> **决定**：负责人 2026-11-11 定「背包满是放到邮件里面的」，范围选「人物背包与临时格都放不下才寄」。
> **关联**：[战斗道具预留](battle-item-reservation.md)、[背包规则分层 §6.6](bag-rule-policy-layering.md)、[回合制收口 §11](turn-battle-gap-closure.md)、[邮件服务](mail-system.md)、[玩家收件箱](player-inbox-service.md)、[存盘直接提交](player-save-direct-commit.md)。

## 1. 要解决的问题

战斗结算的掉落先放人物背包，放不下整批放临时格。两个都放不下时，以前整笔结算延后：这一局的金币、任务进度、气血都卡着，收件箱里后面的局按序也停在这里，要等玩家从临时格取回、腾出位置才继续。

这条兜底有一个腾不出位置的局面：人物背包里全是被战斗道具预留押着的满堆，临时格也满。玩家没有丢弃、出售、存仓入口，这一局一直结不了，预留一直押着（[战斗道具预留](battle-item-reservation.md) §7 记过）。

## 2. 做法

两个包都放不下时，这一局照常应用，掉落整批记进玩家存档里的**待寄队列**，与扣消耗、入账同一帧写入、随同一次存档提交。存档确认提交之后，scene 以 `battle` 身份调邮件服务发个人邮件，附件就是这批掉落；邮件回了 `mail_id` 才从待寄队列里删。玩家腾出背包空位后在邮箱里领取。

| 时刻 | 动作 | 位置 |
|---|---|---|
| 结算应用，人物背包与临时格都放不下 | `BagService::ConsumeInventoryAndGrant` 问去处能不能接住整批（`GrantOverflowRoute::accept`）：按邮件附件上限分封、逐封取请求号、核待寄队列余量 | `PlayerMailOutboxSystem::PrepareBattleOverflow` |
| 同一帧，货币入账等复核都过了 | 发布两包候选（只有消耗，掉落一件不进包），同一段里把条目写进 `PlayerMailOutboxComp` | `GrantOverflowRoute::commit` → `CommitPrepared` |
| 同一帧 | 记近期战斗结果，`items_mailed = true` | `RecordBattleResult` |
| 存档确认提交 | 收件箱确认这一局；推 `BattleEndS2C`（带 `items_mailed`） | `CompleteInboxSettlement` |
| 每秒一拍 | 条目已在最近一次提交的存档快照里、玩家此刻可改，就寄一封（每名玩家至多一封在途） | `PlayerMailOutboxSystem::Tick` |
| 邮件回了 `mail_id` | 删条目，请求存盘 | `OnSendDone` |

放得下就不寄：先人物背包，再临时格，两个都放不下才改投。临时格和取回协议不变。

## 3. 为什么不丢、不重

物品在任一时刻只在三处之一：背包、待寄队列、邮件。每一步换位置都有幂等或同事务保证。

| 崩溃或失败发生在 | 结果 |
|---|---|
| 条目写进内存、存档还没提交 | 结算和条目一起没落盘。收件箱条目还在，重登后结算重做，重新决定进包还是改投；旧请求号从没寄出过，作废 |
| 存档已提交、还没寄 | 条目在盘上。下次加载后照常寄 |
| 已寄出、邮件已建、回包丢了 | 条目还在。重寄用同一个请求号，邮件按 `(sender_kind, request_id)` 判重，回原来那封，再删条目 |
| 条目已从内存删、删除还没落盘 | 下次加载条目又回来，重寄拿回原邮件，再删 |
| 寄送途中玩家交接到别的节点 | 旧节点的回包找不到可改的实体，什么都不做；新节点从存档带着条目，自己再寄，同样判重 |

三条规则撑着这张表：

1. **没落盘不寄。** 只寄「最近一次已提交的存档快照里已经有」的条目，判据与收件箱确认、资产通道相同（`RoleAdmissionSystem::IsSnapshotCommitted`）。先寄后落盘的话，进程一崩结算重做，邮件却已经发出，物品多一份。
2. **请求确定性重建。** 发给邮件的请求由条目逐字段建出，不带过期时刻；同一条目每次字节相同，邮件的载荷摘要对得上。标题、正文原样存在条目里，改版本文案不影响在途条目。
3. **回了 `mail_id` 才删。** 邮件明确拒收、结果未知、没发出去都留着，按退避重试，没有放弃。

请求号取自发号器的 `txlog` 号段（全服唯一的永久 ID）。没有为它新开一个号种：号种要在 data_service 加种子和迁移，而这里只要求 `battle` 这个发件角色下不重复。

加载之后第一笔存盘哪怕没有脏行也会写（`SavePlayerImpl`），所以存档里带回来的条目很快可寄。Tick 发现有条目而没有一条在已提交的快照里时，每 5 秒请求一次存盘（`mail_outbox_rules::kResaveIntervalMs`，与收件箱等落盘的间隔同值）。

## 4. 接口与落点

| 层 | 内容 |
|---|---|
| 存档 | 持久组件 `PlayerMailOutboxComp`（`proto/common/component/player_mail_outbox_comp.proto`），落在 `player_database.mail_outbox`（字段 27，WALLET 行）。只在加载时创建；存盘缺它整份拒存；写入口缺它即拒绝 |
| 背包 | `BagService::GrantOverflowRoute { accept, commit }`，`ConsumeInventoryAndGrant` 的最后一个参数。`BagService` 不知道去处是什么：`accept` 在任何副作用之前问、不得改存档和背包；`commit` 在发布两包候选的同一段里调、不得失败。改投的那一批不铸号、不记获得流水与异常获得，它们在邮件附件领取经资产通道入包时才记 |
| 规则 | `mail_outbox_rules.h`（纯函数）：分封、校验、寄送结果分类、退避、日志节拍 |
| 写入与寄送 | `PlayerMailOutboxSystem`（`player_mail_outbox.{h,cpp}`）。对外依赖收在 `Dependencies` 一个结构里（发不发得出、发一封、是否已落盘、请求存盘、时钟），单测整体替换 |
| 接线 | `PlayerBattleSystem::ApplySettlementToEntity` 把去处接到待寄队列；scene 启动时挂 1 秒定时器，三条停机路径取消 |

一封邮件的附件上限与邮件服务相同（`go/shared/mailgrant.Bundle`）：至多 16 种、合计 4096 件。超出就分成多封，同一种装不下把余量拆到下一封。待寄队列至多 32 条。

## 5. 寄送规则

- 只寄登记在本节点、此刻可改的玩家（`player_mutability`）。交接、退出中、没有归属围栏时不寄，由下一个持有者寄。
- 每名玩家至多一封在途。在途超过「调用期限 + 2 秒」没回按结果未知处理；那一次的回答若迟到，寄到了照样删条目，失败不再重复记。同一条目重寄时请求号相同，靠尝试号区分哪一次是当前在途的。
- 失败后的间隔按连续失败次数翻倍：结果未知（含没发出去）1 秒起、封顶 60 秒；明确拒收 30 秒起、封顶 5 分钟。寄到一封后归零。
- 结果分类与 rank 发奖同一口径：只有拒收码表里列出的业务拒绝算明确拒收（邮箱满、附件无效、请求冲突、参数错）；故障码和不认识的码一律按结果未知，因为邮件提交结果未知时回的就是故障码。
- **验签被拒单列**：mail 对验签不过的请求回 gRPC `UNAUTHENTICATED`（`reason=signature_rejected`）。成因只有两种：两边 `MAIL_PRODUCER_SECRET_BATTLE` 不同，或两边时钟相差超过 5 分钟（签名时间窗口）。验签在业务之前，邮件没有写，所以归为拒收：按 30 秒起的拒收节奏退避、留着条目，每次失败都打 ERROR `mail_outbox_stuck`，不会重复寄。`DefaultSend` 把它记成 `SendOutcome.signatureRejected`，不归入结果未知（那样只会打 WARN，告警不响）。
- 邮箱满（个人邮件上限 200 封）是玩家删信就能好的拒收，打 WARN。附件无效、请求冲突、参数错重试不会变，打 ERROR `mail_outbox_stuck`，要人处理。

身份：发件角色号 5、签名调用方名 `battle`、密钥环境变量 `MAIL_PRODUCER_SECRET_BATTLE`、方法路径 `/mailpb.MailAdmin/SendPersonalMail`。C++ 和 Go 各写一份，`go/mail/internal/data/battle_sender_crosslang_test.go` 逐字比对。

## 6. 接不住时回到整笔延后

下面三种情况 `accept` 不接，结算与以前一样整笔延后、零副作用，延后原因照旧带 `kBagItemNotStacked`，玩家从临时格取回后收件箱照样提前重试：

- **没配发件密钥。** 没有部署 mail 的环境（Docker 调试栈）。欠下一笔永远寄不出去的物品不如不欠。scene 启动时打一行 WARN 说明这个功能关着。
- **待寄队列满。** 邮件长期寄不出去才会攒到 32 条。
- **请求号暂时取不到。** 号段暂缺，会自己恢复。

## 7. 客户端可见的改动

| 字段 | 用途 |
|---|---|
| `BattleEndS2C.items_mailed = 4` | 结算屏在道具行之后加一行「背包已满，以上道具改由邮件发放，请留意邮箱」。写「改由」不写「已」：客户端收到结算时邮件多半还在寄送途中 |
| `PlayerBattleResult.items_mailed = 17` | 近期战斗结果里的同一个事实；`items_gained` 照常列出这批掉落 |
| `BagInfo.held_items = 4` | 人物背包里每种物品被战斗道具预留占住的数量，见 [战斗道具预留](battle-item-reservation.md) §4.1 |

邮件到达走邮件服务原有的提醒推送，客户端邮箱入口亮起。标题「战斗奖励」，正文说明要腾出背包空位后领取。

客户端代码已写（背包详情、聚宝斋选物品、结算屏），说明在客户端仓 `Docs/ClientProtocolCompatibility.md` 末节。

## 8. 部署

| 项 | 内容 |
|---|---|
| 发现 | `bin/etc/base_deploy_config.yaml` 与 `k8s_deploy.ps1` 的 node ConfigMap 模板都加前缀 `MailNodeService.rpc`；scene 的建连白名单与发现集合加 `MailNodeService`；`nodeTypeNameMap` 加一行。邮件全服一份、不分 zone |
| 调用期限 | `GrpcClient.CallDeadlineMs.MailNodeService: 6000`（mail 的 zrpc Timeout 3500 + 门禁要求的 2000 余量，再留 500）；`Assert-GrpcClientDeadlineBudget` 的目标表加 mail |
| 密钥 | Secret `domain-service-secrets` 新增 key `mail-producer-battle`：infra namespace（mail）和每个 zone namespace（scene）都要有，同值，至少 32 字节。两边都是 `optional: false`，缺了 Pod 起不来。mail 的 `Producers` 加 `battle: MAIL_PRODUCER_SECRET_BATTLE` |
| 本机 | `producer_dev_secret.ps1` 生成并复用同一把；`go_services.ps1` 起 mail、`cpp_nodes.ps1` 起 scene 时各自注入 |
| 部署核对 | `k8s_deploy.ps1` 的写路径门禁 `Assert-MailProducerBattleSecretForRun`，早于任何写操作：`infra-up`（本次部署 mail）核对 infra 的键；`zone-up` 与 `all-up` 核对每个 zone 的键，并与 infra 比摘要（同值）。缺键、值为空、不足 32 字节、与 infra 不同，都拒绝部署，一个都不写。`-DryRun` 只打一行说明，不连集群。值只在内存里解码、取长度与摘要，不打印、不写文件；这是该脚本里唯一读取 `domain-service-secrets` 真实值的地方（`Assert-TlsSecretProvisioned` 只查键在不在） |

**上线前要做**：先在各 namespace 的 `domain-service-secrets` 里加 `mail-producer-battle`，再发 mail 和 scene。清单在 [K8s 开服手册](../ops/k8s-open-server-runbook.md) §1，发布清单见 [release-checklist.md](../ops/release-checklist.md) §2.1 ⑦。部署脚本在任何写操作之前核对（`k8s_deploy.ps1` 的 `Assert-MailProducerBattleSecretForRun`：缺键、不足 32 字节、与 infra 不同都拒绝，见 §8 的「部署核对」）。两边值不同，或两边时钟相差超过 5 分钟，Pod 都能起来，但 mail 拒绝验签：scene 侧是 `reason=signature_rejected` 的 stuck（§5、§9），物品不丢，但寄不出去。

mail 是弱依赖：不进 scene 的启动闸，发现不到只让条目按退避留着。

## 9. 可观测性

| 日志 | 级别 | 含义 |
|---|---|---|
| `metric=battle_items_mailed` | INFO | 一局的掉落进了待寄队列（每封一行，带 `request_id`） |
| `metric=mail_outbox_delivered` | INFO | 寄到并删了条目（带 `mail_id`） |
| `metric=mail_outbox_send_failed` | WARN | 没寄到，会重试（邮箱满、邮件不可达、结果未知）。连续失败的第 1 次和之后每 8 次一行 |
| `metric=mail_outbox_send_lost` | WARN | 在途超过调用期限没回 |
| `metric=mail_outbox_stuck` | ERROR | 重试不会变的失败：没配密钥（`reason=no_secret`）、本地签名失败（`reason=sign_failed`）、条目形状不合法（`reason=invalid_entry`），第 1 次和之后每 8 次一行，封顶后约 8 分钟一行；验签被拒（`reason=signature_rejected`，两边密钥不同或时钟相差超过 5 分钟），邮件判附件无效、请求冲突或参数错（`reason=mail_rejected`）：这两类每次重试都打一行，重试间隔封顶 5 分钟。物品欠在队列里，要人处理。两种节拍都保证 10 分钟窗口里看得到 |
| `metric=mail_outbox_full` | WARN | 待寄队列满，这一局回到整笔延后 |
| `metric=mail_outbox_request_id_shortage` | WARN | 请求号暂时取不到 |
| `metric=mail_outbox_invalid` | ERROR | 加载时发现条目不合法；原样携带、照样尝试寄 |

值班固定查询（已写进 `deploy/k8s/asset-channel-alerts.yaml` 头注释，Loki ruler 接线之后转成规则）：

```
count_over_time({job="cpp_nodes", service="scene"} |= "metric=mail_outbox_stuck" [10m]) > 0
```

## 10. 代价与残余

- 改投的物品进了邮件就按邮件的规则走：30 天不领过期。在背包里没有期限。
- 邮件长期不可达时物品停在待寄队列里，这段时间玩家在背包和邮箱里都看不到，只有结算屏那一行提示。物品不会丢，邮件恢复后寄出。客户端没有「待寄」的显示入口：正常情况下邮件在结算后一两秒内到，为事故期间单做一个界面不值得。
- 邮箱满时条目一直等，玩家删信后最多再等 5 分钟。
- 邮件的回包没有签名（`MailAdmin` 的回包没有签名字段，rank 发奖同此），信的是发现到的 mail 节点，与存盘、收件箱回包同一个信任层级。集群内 gRPC 改 mTLS 是全局的事，不在本批。
- 一种推演得出的重复：邮件已建、条目的删除一直没落盘，之后 scene 连续 37 天以上（邮件有效期 30 天 + 清理保留 7 天，且清理开了删除模式）寄不到邮件，判重记录被清掉，重寄会新建一封。玩家这段时间若一直不在线，第一封早已过期未领，不算多得；按现行默认（清理只报告不删）不会发生。接受。
- 号段预检（`HasItemGuidsForDrops`）仍按掉落要铸的实例数核 item 号段，哪怕这批最后改投、一个号都不铸。号段不够时整笔延后，会自己恢复。

## 11. 改动清单

| 类别 | 文件 |
|---|---|
| proto | `proto/common/component/player_mail_outbox_comp.proto`（新）、`proto/common/database/mysql_database_table.proto`（字段 27）、`proto/common/component/player_battle_result_comp.proto`（字段 17）、`proto/battle/player_battle.proto`（`BattleEndS2C` 字段 4）、`proto/scene/player_bag.proto`（`BagHeldItemInfo`、`BagInfo` 字段 4） |
| 背包 | `cpp/libs/modules/bag/bag_service.{h,cpp}`（`GrantOverflowRoute`） |
| 待寄 | `cpp/libs/services/scene/player/system/{mail_outbox_rules.h, player_mail_outbox.h, player_mail_outbox.cpp}`（新）、`scene.vcxproj(.filters)` |
| 结算与快照 | `cpp/libs/services/scene/battle/system/player_battle.cpp`、`cpp/libs/services/scene/player/system/{player_database_loader.cpp, player_feature_snapshot.cpp}` |
| 发现与启动 | `cpp/libs/engine/core/node/system/node/node_util.cpp`、`cpp/nodes/scene/main.cpp` |
| 配置与部署 | `bin/etc/base_deploy_config.yaml`、`go/mail/etc/mail.yaml`、`deploy/k8s/manifests/go-svc/mail.yaml`、`tools/scripts/k8s_deploy.ps1`、`tools/scripts/cpp_nodes.ps1`、`tools/scripts/lib/producer_dev_secret.ps1`、`deploy/k8s/asset-channel-alerts.yaml`（头注释） |
| 测试 | `cpp/tests/bag_test/mail_outbox_test.cpp`（新）、`player_battle_settlement_test.cpp`、`item_hold_test.cpp`、`persist_round_trip_test.cpp`、`player_feature_persistence_test.cpp`、`player_equip_attribute_test.cpp`、`bag_test.cpp`、`bag_test.vcxproj(.filters)`；`cpp/tests/cross_zone_test/{cross_zone_test.cpp, discovery_set_test.cpp}`；`go/mail/internal/data/battle_sender_crosslang_test.go`（新）；`tools/scripts/tests/{k8s_deploy_contract.tests.ps1, k8s_migrate_gate.tests.ps1}` |
| 客户端（`../mmorpg-client`） | `Assets/Scripts/UI/Ugui/Gameplay/{EquipBagModel.cs, GameplayWindow.Fidelity.cs}`、`Assets/Scripts/UI/Ugui/Jubaozhai/CommerceWindow.cs`、`Assets/Scripts/UI/Ugui/Battle/BattleResultPanel.cs`、`Assets/Tests/EditMode/Battle/{EquipForgeModelTests.cs, DamageNumberLayoutTests.cs}`、`Docs/ClientProtocolCompatibility.md` |

## 12. 验证（全部未执行，交 Codex）

仓库根目录，串行，失败不重试，保留首个错误与失败用例输出。

1. **协议生成**。改了客户端可见的 proto，服务端和客户端都要重新生成：
   - 服务端：`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`（要写客户端仓时用默认配置；只生成服务端用 `-ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`）。
   - 客户端：在 `../mmorpg-client` 跑 `tools/gen_proto.ps1`。
   - 通过标准：`cpp/generated/proto/common/component/player_mail_outbox_comp.pb.{h,cc}` 在；`proto/persist_schema_gen.txt` 有 `player_database#27 … group=7`；`BagInfo` 有 `held_items`，`BattleEndS2C` 与 `PlayerBattleResult` 有 `items_mailed`；客户端 `Assets/Scripts/Proto/Generated/` 下出现 `BagHeldItemInfo`、`BagInfo.HeldItems`、`BattleEndS2C.ItemsMailed`。
2. **C++ 编译**：`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`。0 个 `error C` / `LNK`，裸指针成员检查通过。改了 modules、core 与 scene 库，先编库再跑测试。
3. **C++ 测试**：
   - `build\cpp\tests\bag_test.exe --gtest_filter=MailOutboxRules.*:MailOutboxPlayerTest.*:PlayerBattleSettlementItemTest.Inbox*Mail*:PlayerBattleSettlementItemTest.InboxFullBags*:PlayerBattleSettlementItemTest.InboxDropsThatFitTheBagsAreNotMailed:BagServiceItemHoldTest.*:PersistRoundTripTest.*:PlayerFeaturePersistence*` 全绿；
   - 再跑完整 `bag_test.exe`、完整 `cross_zone_test.exe`（含 `DiscoverySetTest.*`）、完整 `currency_test.exe`。
4. **Go**：`cd go/mail && go build ./... && go vet ./... && go test ./... -count=1`。新用例 `TestBattleSenderIdentityMatchesSceneOutbox` 通过（找不到 C++ 源码时会跳过，本机不该跳过）。
5. **部署契约**：`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`、`pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1`。新用例「待寄邮件:node-config 与 bin/etc 都含 MailNodeService.rpc…」通过；mail 的生产者密钥用例含 `BATTLE`。
6. **客户端**：Unity EditMode 跑 `MmorpgClient.Tests.EditMode.Battle`。新用例 `HeldAndAvailableCountsFollowTheServerHeldItems`、`HeldLineOnlyShowsForKindsWithHeldQuantity`、`ResultRewardLines_TellThePlayerWhenTheDropsWereMailed` 通过，原有用例不回归。
7. **冒烟**（本机栈，mail 与 scene 都起）：把角色的人物背包和临时格塞满，打一局有掉落的战斗。期望：结算屏出现邮件提示；scene 日志依次出现 `battle_items_mailed`、`mail_outbox_delivered`；邮箱里有一封「战斗奖励」，附件与掉落一致；腾出空位后领取到账。

## 13. Java 版

0.1.0-SNAPSHOT 待做，已登记 `PARITY.md`。三个客户端可见字段（§7）是两版共享的契约，Java 版要同批补上；待寄队列放在玩家存档里、与结算同一次提交，寄送用 Java 邮件域自己的幂等发件入口。
