# 跨节点/离线名匠代打与控辅强化公平受益设计

## 1. 需求分析

### 1.1 跨节点/离线名匠代打
当前 C++（`player_equipment_v1.cpp` / `player_craft.cpp`）、Java（`EquipmentCraftService` / `SkillCraftService`）与 Python（`scene_equip_craft.py` / `scene_skill_craft.py`）的宗师代打/代制摊位要求宗师必须与买家**在同一 scene 节点且在线可改**，并直接在买家事务中同步调用 `CurrencySystem::AddCurrency(proxyMaster, ...)` 写另一名玩家的余额。这带来两个现实问题：
1. **深夜/低人口/多分线无法成交**：宗师下线或切换分线后摊位立刻不可见且无法下单，生活玩家离线期间无法产生代打收益，新玩家深夜找不到宗师。
2. **双玩家跨写存盘撕裂风险**：买家扣款与宗师入账分属两个玩家存档，单节点同步改写两名玩家状态既无法跨节点扩展，也违背“单玩家事务边界 + 异步资产通道”的资产不变量。

目标能力：
- **营业摊位进共享目录**：宗师开启/更新/关闭代打摊位时写入同区共享目录（控制集群 Redis Hash + scene 本地写穿/定时刷新缓存），且角色加载若持久化态 `proxy_stall_open=true` 则自动补写目录；`ListCraftProxyStalls` 从共享目录读取，支持跨分线与离线宗师 24 小时挂单展示。
- **买家本地原子掷品相与署名**：买家所在 scene 节点按共享目录的宗师摊位快照在本地完成合法性校验、材料与流通银两扣减、宗师档位品相/返页掷骰及永久宗师署名（零物品移交、不涨买家熟练度）。
- **95% 净银两走 `asset_op` 异步入账**：5% 税费照旧销毁，95% 净银两（`ProxyNetGold(fee)`）写入买家同事务持久化的待寄邮件队列 `PlayerMailOutboxComp`（WALLET 持久组，与扣款/扣料/防重账本同库事务落盘），待买家存档提交确认后经 `MailAdmin.SendPersonalMail` 幂等发信，由邮件服务落 `mail_asset_op` 并经 `ASSET_OP_STREAM_SYSTEM_CREDIT`（`AssetCredit`）异步安全入账，生活玩家上线即可收代打分红。

### 1.2 控辅强化公平受益
当前 `equip_attribute_rules.h` 的 `EnhancesBaseAttribute` 仅放大 V1 基础装（`equip_profile=1`）的 `kBothAttacks` / `kPhysicalAttack` / `kMagicAttack` / `kDefense`，未包含 `kMaxHealth`；同时 V1 武器基础属性仅有 `伤害`（`kBothAttacks`），而治疗/护盾/控场技能均不读取物攻/法攻，导致控辅流派冲 `+10/+11` 武器时几乎零收益（纯输出则吃满 `+20%/+24%` 武器基础攻击）。此外，Java（`EquipmentRules` / `EquipmentDisplay`）与 Python（`scene_player.py` / `scene_assets.py`）此前遗漏了 `EnhancesBaseAttribute` 过滤，导致 V1 鞋子基础速度在展示/属性计算中被误放大。

目标能力：
- **速度红线绝对不动**：三服统一执行 `EnhancesBaseAttribute`，保证 V1 装备基础速度（`DerivedStat::kSpeed`）与速度固定词条（`effect=4`）**永不随强化放大**（编译期 `static_assert` + 三服单测守住）。
- **基础气血纳入 V1 基础强化**：`EnhancesBaseAttribute` 对 `kV1Basic` 纳入 `DerivedStat::kMaxHealth`，后续或现有带基础气血的 V1 基础装与攻防同口径受益。
- **控辅固定词条按克制比例享受强化收益**：在 `equip_attribute_rules.h` 定义唯一纯规则：V1 基础装（含武器）强化时，其已安装的奶/盾方向词条（`effect=5` 施放治疗、`effect=6` 护盾容量）按强化万分比的 **50%** 分享增益（`+10` 得 `+10%` 相对增益即 `800 -> 880 bps`；`+11` 得 `+12%` 即 `800 -> 896 bps`），控场命中词条（`effect=11`）按强化万分比的 **25%** 分享增益（`+10` 得 `+5%` 即 `400 -> 420 bps`；`+11` 得 `+6%` 即 `400 -> 424 bps`，且战斗命中仍受 `AilmentChance` 硬顶 `85%/95%` 约束）；输出攻防百分比词条（`effect=1/2`）与速度词条（`effect=4`）份额恒为 `0`（不重复吃强化、不放大速度）。
- **有效来源上限同步克制放宽**：表配置 `source_cap` 仍保持 `3200/1600`，有效收集上限随最高强化共享系数放大至 `3584 bps`（奶/盾，4 件 +11 合计 `+3.84%`）与 `1696 bps`（控场命中，4 件 +11 合计 `+0.96` 百分点）；零强化件保持 `3200/1600` 原值不变；战斗快照校验（`MaximumEffectiveSourceBps`）、治疗/护盾面板分解（`SupportEffectSummary`）与装备详情展示（`FillDisplay`）三处共用同一公式。

---

## 2. 设计分析

### 2.1 共享代打摊位目录（三服同构）
- **数据结构**：直接复用现有 `CraftProxyStallInfo` protobuf 作为目录条目值，零新增客户端协议消息、零协议兼容负担。
- **Redis 键与集群域**：
  - C++：控制集群 `tlsControlRedis`，键 `craft_proxy_stall:{ctl:<zone_id>}`（复用已登记的 `ControlFamily::kSceneControl`，与 `PlayerMysteryMerchantSystem` 同族，不改 `kControlFamilyPrefixes`），Hash Field 为十进制 `master_player_id`，Value 为 `CraftProxyStallInfo` 二进制序列化字节。
  - Java：在 `RedisKeys` 新增 `craftProxyStalls(int zoneId)`（`<ns>:world:{z:<zone>}:craft-proxy-stalls`），遵循 `NamespaceLiteralGuardTest`。
  - Python：在 scene 挂载 `CraftProxyStallDirectory`（内存态 + 可选 Redis 同步），被 `scene_equip_craft.py` 与 `scene_skill_craft.py` 共享。
- **读写与生命周期**：
  - `SetProxyStall(open=true)`：宗师存档写完后同步更新本地目录缓存，并向控制集群发 `HSET`（二进制 `%b`）。
  - `SetProxyStall(open=false)`：宗师存档写完后同步从本地目录缓存移除，并向控制集群发 `HDEL`。
  - 角色加载（`PlayerDatabaseMessageFieldsUnmarshal` / Java 登录加载 / Python 进场）：若 `proxy_stall_open=true` 且具备至少一项宗师资格且手续费合法，自动补写目录；否则不写入。
  - 定时刷新：C++ `CraftProxyStallDirectory::Start(loop)` 每 5 秒在 scene EventLoop 执行一次 `HGETALL`（无新线程、回调零捕获），失败保留旧缓存；单测无 Redis 时本地共享目录缓存照常跨实体/离线工作。
  - 合法性过滤：`ResolveStall(masterId, needForge, needScroll)` 校验 `masterId != buyerId`、对应宗师档位 `>= kMasteryMaster`、对应手续费 `ValidProxyFee(50..100)`；若宗师恰巧在线于本节点，额外复核本节点实体的 `IsPlayerMutable` 与实时 `proxy_stall_open`/费用，避免同节点刚关摊的毫秒级陈旧缓存。

### 2.2 95% 净银两异步 `asset_op` 入账链路
复用既有的 crash-safe 待寄邮件出箱 + 邮件服务 + `ASSET_OP_STREAM_SYSTEM_CREDIT`（`AssetCredit`）闭环，**不新建微服务、不破坏 I6 单流单一 seq 写者不变量、不新增跨服务密钥**：
1. **持久结构增量扩展（`proto/common/component/player_mail_outbox_comp.proto`）**：
   - 新增消息 `MailOutboxCurrency { uint32 currency_type = 1; uint64 amount = 2; }`。
   - `MailOutboxEntry` 末尾追加两个向前/向后兼容字段：
     - `uint64 recipient_player_id = 8;`（`0` = 发给本出箱所属玩家，完全兼容既有战斗溢出条目；`> 0` = 发给指定收件人如代打宗师）。
     - `repeated MailOutboxCurrency currencies = 9;`（按 `currency_type` 严格升序、互不重复、`amount > 0`、至多 4 种，与 `go/shared/mailgrant.Bundle` 同限）。
   - `MailOutboxSource` 新增 `kCraftProxyFee = 2`（`source_id = buyerClientRequestId`）。
   - `ValidateEntry` 更新为：允许仅有物品、仅有货币或二者兼有，但 `items` 与 `currencies` 不能同时为空；`currencies` 校验币种已知且严格升序、数量 `> 0 && <= INT64_MAX`。
2. **买家事务内原子写入**：
   - `PlayerEquipmentV1System::Commit` 与 `PlayerCraftSystem::CraftSkillBook` 在提交前调用 `PlayerMailOutboxSystem::PrepareProxyFeePayout(buyer, masterId, netGold, clientRequestId, buyerName, preparedMail)`：检查发件器已配置、买家 `PlayerMailOutboxComp` 存在且未满（`< 32`）、从 `txlog` 号段分配唯一 `request_id`。
   - 失败（发件器未配置/号段暂缺/队列已满）在任何资产修改前直接拒单，不欠发不出之账。
   - 提交段内与扣料、扣流通银两、记录 `ClientRequestLedgerComp` 同帧调用 `CommitPreparedProxyFee`，写入 `PlayerMailOutboxComp`（同属 WALLET 持久组 group=7，与扣款同库事务提交）。
3. **落盘围栏后的幂等投递与 `asset_op` 入账**：
   - 买家存档快照确认提交（`IsSnapshotCommitted`）后，`PlayerMailOutboxSystem::Tick()` 重建确定性 `mailpb::SendMailRequest`（`sender_kind = 5 ("battle")`，`recipient_player_id = entry.recipient_player_id != 0 ? entry.recipient_player_id : ownerPlayerId`，`attachments.currencies` 携带 `{GOLD, netGold}`），经 `MailAdmin.SendPersonalMail` 幂等发信。
   - `go/mail` / `game-mail` / Python `mail` 将个人邮件落库；宗师上线后领取邮件附件，邮件服务写入 `mail_asset_op`（`ASSET_OP_STREAM_SYSTEM_CREDIT`），由投递工作器签名调用 `SceneNodeGrpc.AssetCredit`（`asset_op`）原子入账流通银两。
   - **已删角终止处理**：若收件宗师在挂单后已删角，邮件服务返回 `kMailRecipientDeleted`；`ClassifySend` 识别为不可重试终态 `kTerminalDrop`，打印带 `player_id / recipient_player_id / request_id / amount` 的 WARN 审计日志并摘除该条目，防止阻塞买家出箱。
4. **Java / Python 一致实现**：
   - Java：在 `PlayerState`（`game-player-store`）新增 `PlayerMailOutboxState mail_outbox`（与钱包/背包同事务存盘），新增轻量 `CraftProxyStallDirectory` 与 `CraftProxyPayoutOutbox`（对接 `MailAdminService.sendPersonalMail`，默认可注入桩供单测验证完整异步发信与重试/删除）。
   - Python：同步 `player_mail_outbox_comp.proto` 后扩展既有 `SceneMailOutbox`（支持 `recipient_player_id`、`currencies`、`prepare_proxy_fee`），在 `scene_equip_craft.py` 与 `scene_skill_craft.py` 统一接入共享目录与异步出箱。

### 2.3 控辅强化公平受益（纯规则单一事实源）
在 `cpp/libs/services/scene/player/system/equip_attribute_rules.h` 定义唯一规则并逐字同步至 Java `EquipmentRules.java` 与 Python `scene_player.py`/`scene_equip_craft.py`：
- `EnhancesBaseAttribute(profile, effect, parameter)`：
  - `kLegacy`: `true`
  - `kV1Basic`: `kBothAttacks` 或 `kDerivedFlat` 且参数为 `kMaxHealth / kPhysicalAttack / kMagicAttack / kDefense`（**`kSpeed` 恒为 `false`**）。
  - 其他 profile: `false`。
- `V1FixedAffixEnhanceSharePermyriad(profile, affixEffect)`：
  - `kV1Basic` + `effect in {5, 6}`（治疗/护盾）：`5000`（50% 强化份额）。
  - `kV1Basic` + `effect == 11`（障碍命中）：`2500`（25% 强化份额）。
  - 其余一律 `0`（含 `effect == 4` 速度、`effect in {1, 2}` 攻强百分比）。
- `ScaleV1FixedAffixByEnhance(profile, affixEffect, qualityScaledBps, enhanceBonusPermyriad)`：
  - `effectiveBonus = enhanceBonusPermyriad * share / 10000`
  - 返回 `ScaleByPermyriad(qualityScaledBps, effectiveBonus)`（向下取整、防溢出）。
- `MaxV1FixedAffixEffectiveCap(affixEffect, baseSourceCap)`：
  - 按 `+11` 顶配强化（`2400`）算对应通道最大有效上限：`effect 5/6` -> `3200 -> 3584`；`effect 11` -> `1600 -> 1696`；其余不变。
- `CollectEffects`（C++ `PlayerEquipmentV1System::CollectEffects` / Java `FixedEquipmentEffects.collect` / Python `scene_equip_craft.py`）：
  - 对穿戴的 `kV1Basic` 装备按有效强化等级取 `enhanceBonusPermyriad`，对每条固定词条在品相缩放后再调用 `ScaleV1FixedAffixByEnhance`。
  - 同一通道的聚合上限取 `ScaleV1FixedAffixByEnhance(kV1Basic, effect, baseMinCap, maxEnhanceOnChannel)`，既确保零强化件维持原上限 `3200/1600`，也确保 4 件全套控辅冲 `+10/+11` 不被旧上限吞掉收益。
  - `support_sources` 中每条记录写入增强后的 `raw_bonus_bps` 与更新后的 `source_cap_bps`，保证 `ValidEquipmentSources`（逐条有效和 == 总值）恒为 `true`。
- 展示一致性（`FillDisplay`）：已安装固定词条在 `BagItemInfo.fixed_affix_slots[].selected.value_bps` 展示当前生效值（含强化克制增益），与面板/战斗完全同口径。

---

## 3. 思想分析

1. **复用既有一等公民设施，拒绝另起炉灶**：代打分红不绕开 `asset_op`、不新开写库路径、不破坏 I6 单流单一写者，直接借道已跑通 SQL 提交围栏的 `PlayerMailOutboxComp -> MailAdmin -> mail_asset_op -> AssetCredit` 链路；摊位目录复用 `ControlFamily::kSceneControl` 与 `CraftProxyStallInfo` 现有结构。
2. **所有数值口径归口单一纯规则头**：控辅强化份额、速度零放大红线与上限推导全部收敛到 `equip_attribute_rules.h`（无 ECS/网络依赖），三服逐字对照实现，避免展示、属性重算与战斗校验分叉。
3. **克制平衡而非数值膨胀**：控辅不按 100% 放大（避免奶盾过量拉长回合或控场突破封顶），采用 50%（奶盾，满套 +11 仅多 +3.84%）与 25%（控场命中，满套 +11 仅多 +0.96 百分点且受 85% 硬顶）两档克制系数，同时锁死“速度永不强化”。

---

## 4. 漏洞分析

| 风险点 | 成因 | 阻断设计 |
|---|---|---|
| 买家扣银两后进程崩溃导致宗师收不到分红，或未扣款却发了分红 | 跨玩家双写无分布式事务 | 出箱条目落在买家 `PlayerMailOutboxComp`（WALLET 组与扣款同事务提交），且 `Tick` 严格等待 `IsSnapshotCommitted` 确认已落盘后才发信；`MailAdmin` 按 `(sender_kind, request_id)` 幂等判重。 |
| 重复点击/网络重试导致给宗师多发一笔 95% 分红 | 客户端重放同一 `client_request_id` | `ClientRequestLedgerComp` 先行幂等拦截重放，重放路径不进入提交段、不追加出箱条目。 |
| 宗师关摊后缓存尚未刷新（最多 5 秒窗口）被下单 | 跨节点定时拉取延迟 | 宗师本节点关摊立刻 `HDEL` + 清本地缓存；同节点下单实时复核实体状态；跨节点最多 5 秒窗口内按买家预览确认的旧合法报价成交，宗师照常按该报价收取 95% 净分成，无资产损失。 |
| 离线宗师删角后买家出箱永远卡死重试 | `go/mail` 返回 `kMailRecipientDeleted` | `mail_outbox_rules::ClassifySend` 将 `kMailRecipientDeleted` 判为终态放弃（`kTerminalDrop`），打 WARN 审计日志并摘除条目，不阻塞买家后续战斗溢出/代打邮件。 |
| 绑定银两借代打洗成宗师流通银两 | `proxyFeeGold` 若允许扣绑定银两则洗白 | `proxyFeeGold` 仍 100% 走 `strictGold`（仅扣流通银两 `kCurrencyGold`），5% 销毁，95% 经邮件发放流通银两，T/B 防洗白红线零松动。 |
| 4 件套纯奶/纯控冲 +11 被旧 `3200/1600` 上限截成 0 收益 | 旧 `source_cap` 恰好等于 `4 * 800` / `4 * 400` | 有效上限随强化共享系数放宽至 `3584` / `1696`，战斗 `MaximumEffectiveSourceBps` 同步放宽，且 `ValidEquipmentSources` 保持严格恒等。 |
| V1 鞋子速度在 Java/Python 被强化误放大 | Java/Python 此前漏判 `EnhancesBaseAttribute` | 三服同步收口 `EnhancesBaseAttribute` 并新增 V1 鞋子 +10/+11 速度恒定回归单测。 |
| 异步回调悬空实体 / 裸指针成员违规 | 新增 Redis 定时刷新与回调 | 目录回调零捕获仅写全局缓存，不持有 `entt::entity` 或裸指针成员；通过 `test_cpp_no_raw_pointer_member.py` 门禁检查。 |

---

## 5. 步骤分析

1. **设计落盘与索引刷新**：落地本设计文档并运行 `gen_docs_index.py`。
2. **C++/Go 权威仓实施**：
   - 更新 `player_mail_outbox_comp.proto`，运行 `proto-gen-run` 刷新生成码与 `persist_schema_gen.txt`。
   - 升级 `mail_outbox_rules.h`、`player_mail_outbox.{h,cpp}` 支持指定收件人、货币附件、`kCraftProxyFee` 来源与删角终态摘除。
   - 新增 `craft_proxy_stall_directory.{h,cpp}`（共享目录 + 控制集群 Redis `HSET/HDEL/HGETALL` + EventLoop 刷新），接入 `scene/main.cpp`、`player_database_loader.cpp`、`player_craft.cpp` 与 `player_equipment_v1.cpp`。
   - 升级 `equip_attribute_rules.h`、`equipment_battle_rules.h`、`equipment_battle_effects.cpp`、`support_effect_rules.h`、`player_equipment_v1.cpp` 实现控辅强化克制受益与速度零放大守卫。
   - 补齐/更新 C++ 单元测试，执行裸指针静态检查、串行 MSBuild (`/m:1`) 编译与 GTest/Go 测试。
3. **Java 仓同步实施**：
   - 同步合约 `ContractSync`，更新 `RedisKeys.java`、`PlayerState` 出箱字段、`EquipmentRules.java`、`EquipmentDisplay.java`、`FixedEquipmentEffects.java`、`BattleEquipment.java`、`EquipmentCombatRules.java`、`CraftProxyStallDirectory`、`CraftProxyPayoutOutbox`、`EquipmentCraftService.java`、`SkillCraftService.java`、`SceneNode.java`。
   - 补齐/更新 JUnit 测试并更新 `PARITY.md`，执行 Maven 测试验证。
4. **Python 仓同步实施**：
   - 同步 `player_mail_outbox_comp.proto` 并重建协议，升级 `scene_mail_outbox.py`、`scene_player.py`、`scene_assets.py`、`scene_equip_craft.py`、`scene_skill_craft.py`、`battle_dropin_effects.py`。
   - 补齐/更新 pytest 用例并执行测试验证。
5. **客户端核查与收尾**：确认无客户端协议破坏项，核对全部改动 `git status`/`git diff` 不污染既有未提交改动。

---

## 6. 执行步骤

- [ ] 写入设计文档并运行 `python tools/scripts/gen_docs_index.py`
- [ ] 更新 `player_mail_outbox_comp.proto` 并运行 `proto-gen-run`
- [ ] C++ 实现 `mail_outbox_rules.h` / `player_mail_outbox.{h,cpp}` 货币与指定收件人支持
- [ ] C++ 实现 `craft_proxy_stall_directory.{h,cpp}` 并接入 `player_equipment_v1.cpp` / `player_craft.cpp` / `player_database_loader.cpp` / `scene/main.cpp`
- [ ] C++ 实现 `equip_attribute_rules.h` 控辅强化受益与战斗/支持预览/展示一致性
- [ ] C++ 补齐单元测试、跑 `test_cpp_no_raw_pointer_member.py`、串行编译与全量相关单测 + Go 单测
- [ ] Java 同步实现两项功能、补齐 JUnit 测试、更新 `PARITY.md` 并通过 Maven 测试
- [ ] Python 同步实现两项功能、补齐 pytest 用例并通过测试
- [ ] 核查客户端与跨仓回归，输出验证报告
