# 7 套非永久「战法·加点」流派方案槽位系统完整设计方案（V1）

> **日期**：2026-11-26  
> **状态**：设计定稿，进入 C++/Go 权威服、Java 独立服与 Unity 客户端（纯 C# 模型 + 可交互 uGUI）全链路实现，并配套异机 Codex 纯功能 UI 切片交接单。  
> **上位红线**：[项目设定](project-vision.md)（好玩优先 / 真不肝 / 小小氪金 0 P2W / 官方 7:3 / 物价稳得住 / 防骗安全 / 零拒审防下架）  
> **衔接文档**：[挂机月卡 / 仙途轻享卡](../design/afk-monthly-card.md) · [角色属性加点系统](../design/player-attribute-allocation.md) · [自由技能体系策划总览](skill-planning/README.md) · [装备流派方案预设 V1](equipment-loadout-presets-v1.md) · [客户端请求幂等](../design/client-request-idempotency.md) · [UI 纯功能交接单](ui-handoff/skill-build-loadout-7slots-ui-handoff.md)

---

## 1. 需求分析

### 1.1 要解决的核心问题
1. **流派方案必须「战法 + 四维加点」一体化**：回合制里一套完整流派由「战法装配（1 开场 + 3 被动 + 1 追击）」与「四维加点（体质/灵力/力量/敏捷）」共同决定。过去战法方案与加点方案分开管理、分开收费（甚至各自有不同的上限），玩家切换一次流派要操作两次，体验割裂。
2. **总共可开 7 套方案，支持「一套一套按需售卖」且「非永久」**：
   - 永久买断收完一次即断流，不利于个人开发者长线可持续经营；
   - 按套非永久租用既能让只需要 3 套的轻度微氪玩家以极低门槛（¥3/月）体验，又能让研究 PVE 输出、PVP 抢速、帮战生存、困难首领特化等多流派的玩家按需开满 7 套。
3. **严守不逼氪、不逼肝、不过审拒审红线**：
   - 必须保证免费玩家零痛点；
   - 绝不能制造“5 个付费槽位 5 个不同到期日、隔三差五弹催费”的骚扰体验；
   - 绝不能采用商店自动扣款订阅，全部走游戏内灵石手动开通/续期；
   - 到期**绝不删除玩家已配好的战法与加点数据**。

### 1.2 真实可玩性扩展（克制、不硬扩展）
在 7 套「战法 + 加点」一体化方案上，只做真正提升流派构筑乐趣的 3 项高价值扩展，坚决不做无意义堆料（不做方案专属隐藏属性、不做排行榜/点赞、不做自动扣款）：
1. **6 套回合制经典流派加点一键试配模板**（`4力1体`、`4灵1体`、`3敏1体1力`、`2灵2体1敏`、`全敏先手`、`全体抗伤`）：按当前角色总点数实时折算成草稿，3 秒搭出新流派。
2. **跨方案一键复制**：把已配好的任意方案（如方案一）一键复制到当前编辑方案再微调，免去重复勾选战法与点 425 点属性的繁琐。
3. **方案内透明洗点重配**：若编辑后的加点低于该套已存点数，不强迫玩家退回属性页洗点，只需勾选「允许洗点重配」即可在保存本套方案时按标准池价格（30 级以下免费，满级按表扣银两）原子完成洗点并写入新点。

---

## 2. 设计分析

### 2.1 7 套方案槽位权限与非永久统一到期模型

| 槽位组成 | 套数 | 获取方式 | 周期与归属 |
|---|---:|---|---|
| **永久免费基础位** | **2 套**（方案 1～2） | 创角永久免费 | 永久免费（覆盖日常 PVE + PVP/高难核心两套，零氪 0 卡脖子） |
| **满级长期纯玩奖励位** | **+1 套**（达 85 级满级） | 角色有效等级 ≥ 85 自动永久解锁（0 打卡、0 额外任务） | 永久免费（0 氪长期纯玩最终拥有 **3 套永久免费**，方案 1～3） |
| **仙途轻享卡附赠位** | **+2 套**（生效期） | 复用既有 `afk_pass_expires_at`（300 灵石/30 天） | 轻享卡有效期内自动生效（85 级前 4 套、满级后自带 5 套，零重复收费） |
| **按套按月租用位** | **+1～+5 套**（总封顶 7 套） | 游戏内真实灵石手动开通/扩容/续期：**30 灵石 / 套 / 30 天**（约 ¥3/套/月） | **所有租用套共享同一个统一到期日**，剩余上限 **180 天** |

- **有效解锁套数权威公式（C++ / Java / 客户端纯函数同口径）**：
  - `freeSchemes = 2 + (effectiveLevel >= 85 ? 1 : 0)`
  - $\text{unlocked} = \min\big(7,\; \text{freeSchemes} + (\text{lightCardActive} ? 2 : 0) + (\text{rentActive} ? \text{rentedExtraSlots} : 0)\big)$
- **防多花冤枉钱护栏**：
  - 当前可租最大额外套数 `maxRentable = max(0, 7 - freeSchemes - (lightCardActive ? 2 : 0))`（85 级前无轻享卡最多可租 5 套、有轻享卡最多可租 3 套；85 级满级后免费升至 3 套，无轻享卡最多只需租 4 套、有轻享卡最多只需租 2 套即开满 7 套，服务端直接拒绝超出 7 套上限的无意义超买）。
  - 租期内若因开通轻享卡或升至 85 级导致 `maxRentable` 下降，`minAllowedInTerm = min(activeRented, maxRentable)`，允许按新上限直接续期而不被「租期禁缩减」卡住、也绝不逼多花冤枉钱。
  - 若玩家租了 3 套后轻享卡到期，其有效套数平滑变为 `freeSchemes + 3` 套（已付灵石 100% 有效），此时若想补回 7 套再扩容即可。
- **统一到期日计费公式（64 位无溢出 + 向上取整防白嫖）**：
  - 常量：`kRentUnitPriceDiamond = 30`（真实灵石 `kCurrencyDiamond = 1`，严禁混扣绑定灵石），`kRentPeriodDays = 30`（`2,592,000` 秒），`kRentMaxRemainingDays = 180`（`15,552,000` 秒）。
  - **未在租期（`rentExpiresAtSec <= nowSec`）**：须 `add_periods >= 1`，费用 `= target_rented_slots * 30 * add_periods`，新到期 `= nowSec + add_periods * 30 天`。
  - **已在租期（`remainingSec = rentExpiresAtSec - nowSec > 0`）**：
    - 禁止租期内缩套退款（`target_rented_slots < currentRentedSlots` 拒绝；到期后可自由只续更少套数）；
    - `addedSlots = target_rented_slots - currentRentedSlots`，要求 `addedSlots > 0 || add_periods > 0`；
    - **扩套对齐原到期日差价**（`addedSlots > 0`）：`prorateCost = ceil(addedSlots * 30 * remainingSec / periodSec)`（整数算式 `(addedSlots * 30 * remainingSec + periodSec - 1) / periodSec`，严格 `>= 1` 灵石）；
    - **续期费**（`add_periods > 0`）：`renewCost = target_rented_slots * 30 * add_periods`，新到期 `= rentExpiresAtSec + add_periods * periodSec`，严格校验 `newExpiresAt - nowSec <= 180 天`；
    - 总费用 `totalCost = prorateCost + renewCost`。

### 2.2 「战法 + 四维加点」单一事实源绑定
不复制第二份属性点数真相，而是让流派方案 `scheme_id = 1..7` 直接与 `PlayerAttributeComp.schemes` 的同名 `scheme_id = 1..7` 建立 **1:1 绑定**：
1. **自动零费建加点方案**：在「战法·加点流派」中保存或切换到已解锁方案 `N`（`1..7`）时，若 `PlayerAttributeComp` 尚无 `scheme_id = N`，服务端自动零费创建对应空加点方案 `N`（不另扣 1000 银两开方案费）。
2. **原子保存与安全洗点**：
   - `SaveSkillLoadout` 同时支持保存自定义方案名（UTF-8 1..12 字符 + 敏感词过滤）、定位标签（`0 默认 / 1 输出 / 2 控场 / 3 辅助 / 4 生存`）、战法装配（1 开场 + 3 被动 + 1 追击）与该套四维加点（`update_attribute_allocation = true`）。
   - 先完整预检战法合法性与加点合法性（仅允许角色属性池 `101..104`、总和 ≤ 有效总点数、防 64 位溢出）；若任意维度低于该套已存点数且未勾选 `reset_attribute_if_needed`，直接拒回 `kSkillLoadoutAttributeResetRequired`（守住洗点规则）；勾选则先验余额并按 `AttributePool.reset_cost_gold`（30 级以下免费）扣银两，再与战法在同一次 EventLoop 同步调用里提交并仅触发 1 次 `Recalculate` + `PushPanel`。
3. **一键切换与冷却容错**：
   - `SwitchSkillLoadout` 默认（`skip_attribute_sync = false`）同步切换激活战法方案 `N` 与激活属性加点方案 `N`，统一只做 1 次重算（沿用既有按比例保持 HP/MP 规则防往返回血）；
   - 若属性方案正处于 60 秒切换冷却期，不阻断战法切换，在 `attribute_sync_notice` 中明确告知冷却剩余秒数。
4. **到期「只锁不删」体验底线**：
   - 租期或轻享卡到期后，`scheme_id > unlocked` 的方案**完整保留在存档与下发快照中**，前端可只读预览已配战法与四维加点，仅禁止编辑与切入；
   - 若到期时玩家正激活着已锁定的高编号方案，战斗内照常按快照结算，战外可切回 1..unlocked 方案（切走后须续期才能切回），绝不半夜把玩家强行切回空点方案；续期后 100% 原样复活。
5. **跨系统对齐（2026-11-26 方案 A 三合一席位升级）**：
   - `PlayerAttributeSystem::MaxSchemesFor` 对齐至当前有效流派方案上限（2..7）；
   - `PlayerEquipLoadoutSystem` 同步升级至 **7 套方案并直接共享本系统同一份流派席位权限**（装备 + 战法 + 加点三合一共享，交一份钱同时开启，到期只锁不删；详规见 [equipment-loadout-presets-v1.md](equipment-loadout-presets-v1.md)）；其联动战法选项自动读取 1..unlocked（最高 7 套）并优先展示玩家自定义流派名。

### 2.3 持久化、协议、幂等与错误码契约
1. **持久组件增量（零新表、零新列，复用 `GROWTH` 组与 `WALLET` 组）**：
   - `proto/common/component/player_skill_comp.proto`（Java 侧同步 `game-player-store/.../player_state.proto` + 升级持久台账）：
     - `SkillLoadoutScheme` 追加：`string name = 6;`
     - `PlayerSkillListComp` 追加：`uint32 rented_extra_slots = 8;`、`uint64 rent_expires_at_sec = 9;`
   - `proto/common/component/client_request_ledger_comp.proto` 的 `ClientRequestRecord` 追加：`skill_loadout_rented_slots = 42`、`skill_loadout_rent_expires_at_sec = 43`、`skill_loadout_rent_spent_diamond = 44`。
   - `ClientRequestKind` 追加 `kRentSkillLoadoutSlots = 30`；`TransactionType` 追加 `TX_SKILL_LOADOUT_RENT = 48`。
2. **客户端协议增量（`proto/scene/player_skill.proto`，严格末尾追加 RPC）**：
   - `SkillLoadoutSchemeInfo` 追加：`name(6)`、`attribute_allocated(7)`、`attribute_remaining(8)`、`attribute_total_points(9)`。
   - `SkillLoadoutSnapshot` 追加：`free_schemes(11)`、`light_card_bonus_schemes(12)`、`rented_extra_slots(13)`、`rent_expires_at_sec(14)`、`server_now_sec(15)`、`rent_unit_price_diamond(16)`、`rent_period_days(17)`、`rent_max_remaining_days(18)`、`diamond_balance(19)`、`attribute_reset_cost_gold(20)`、`attribute_switch_cooldown_until(21)`、`active_attribute_scheme_id(22)`。
   - `SaveSkillLoadoutRequest` 追加：`name(8)`、`update_attribute_allocation(9)`、`attribute_allocated(10)`、`reset_attribute_if_needed(11)`；`SaveSkillLoadoutResponse` 追加 `attribute_sync_notice(5)`。
   - `SwitchSkillLoadoutRequest` 追加 `skip_attribute_sync(4)`；`SwitchSkillLoadoutResponse` 追加 `attribute_sync_notice(5)`。
   - `SceneSkillClientPlayer` 末尾追加：`rpc RentSkillLoadoutSlots (RentSkillLoadoutSlotsRequest) returns (RentSkillLoadoutSlotsResponse);`（同步登记 `player_handoff_gate.h` 与 `MessageLimiter.xlsx`）。
3. **Tip 错误码（`data/tip/Tip.xlsx` 的 `skill_error` 段尾追加 7 条）**：
   - `SkillLoadoutNameInvalid`、`SkillLoadoutRentPriceChanged`、`SkillLoadoutRentDiamondNotEnough`、`SkillLoadoutRentStackLimit`、`SkillLoadoutRentSlotsInvalid`、`SkillLoadoutAttributeInvalid`、`SkillLoadoutAttributeResetRequired`。

### 2.4 五问准入自检（[project-vision.md §二](project-vision.md#二后续所有子文档准入自检写设计前必答-5-问)）

| 准入问题 | 核验结论 |
|---|---|
| **1. 普通零氪玩家** | 永久免费享有 2 套完整「战法 + 加点」流派方案（PVE 输出 + PVP/高难功能）与 100% 相同 425 点战力上限，无打卡、无卡脖子。 |
| **2. 个人开发者收入** | 第 3～7 套按月灵石单套直售（¥3/套/月起，100% 官方灵石消费），与轻享卡互补并形成健康长线复购现金流，完全符合官方 7:3 引擎。 |
| **3. 物价与长期循环** | 方案槽位纯绑定角色权限、零可交易产出，不会冲击市场物价；反而鼓励玩家多配流派，间接拉动多套装备、重铸砂与技能书横向消耗。 |
| **4. 防骗与资产安全** | 全程强制非 0 `client_request_id` 持久化幂等账本防弱网双扣、报价防呆、64 位无溢出计费、先验再扣、到期只锁不删，零丢数据与零错扣风险。 |
| **5. 商店过审/防下架安全** | 全走既有灵石充值档位 + 游戏内手动灵石开通/续期，无自动扣款订阅、无外链/切支付、无诱导文案、自定义方案名过敏感词检测，首审 100% 干净合规。 |

---

## 3. 思想分析

1. **为什么坚持「统一到期日」而不是 5 个槽位各算各的到期时间？**  
   多槽位独立到期会在第 2 个月制造高频到期弹窗焦虑，逼退轻度玩家；将多套租用抽象成「租用额外套数 `k` + 唯一到期日 + 扩套按剩余时长向上取整补差价」，既兑现了“一套一套按需买、非永久”，又把续费心智压到最低（和云盘扩容一样直觉）。
2. **为什么让战法方案和加点方案 1:1 共享槽位号而不另存副本？**  
   遵守「单一事实源」原则：`PlayerAttributeComp.schemes` 继续作为加点数值唯一真相，战斗快照、属性面板、装备重算、按比例保血防刷血全套不变量自动继承，零双写分叉风险。
3. **为什么月卡附赠位从 +3 调为 +2 且总上限抬到 7 套？**  
   旧版「免费 3 + 月卡 3 = 6 套」已把 95% 需求白送在月卡里，单套租用没有商业空间；调整为「免费 2（保底零氪不卡脖子）+ 轻享卡 +2（月卡自带 4 套依然超值）+ 单套租用 1..5 套（封顶 7 套）」，散人微氪（¥3）、月卡党（+¥3～¥9 补齐 7 套）与重度流派党全部自然分层。

---

## 4. 漏洞分析与强制防线

| 编号 | 潜在漏洞 / 风险 | 破坏后果 | 强制防线 |
|---|---|---|---|
| **V-1** | **连点或弱网重发 `RentSkillLoadoutSlots` 重复扣灵石** | 玩家被扣双倍灵石或超额叠加租期 | 强制 `client_request_id != 0` 接入 `ClientRequestLedgerComp`（`kRentSkillLoadoutSlots = 30`），且在一切状态预检前先查重；重放原样回首次结果并标 `replayed = true`。 |
| **V-2** | **租期最后几分钟加开 4 套白嫖 0 灵石扩容** | 整数除法向下取整算成 0 灵石免费开槽 | 扩套补差价强制向上取整 `(added * 30 * remSec + periodSec - 1) / periodSec`，`remSec > 0` 时严格 `>= 1` 灵石。 |
| **V-3** | **月卡生效期间误买第 4、5 套无效槽位白花钱** | `2 + 2 + 5 = 9` 超出 7 套上限多扣灵石 | 服务端强制 `target_rented_slots <= MaxRentableExtraSlots(lightCardActive)`（月卡生效上限 3，无月卡上限 5），超限直接拒回 `kSkillLoadoutRentSlotsInvalid`。 |
| **V-4** | **通过 `SaveSkillLoadout` 直接把旧方案某维点改低绕过洗点费** | 绕过洗点银两消耗免费洗点 | 服务端逐维比对已存点数，任一已知维度降低且未显式传 `reset_attribute_if_needed = true` 一律拒回 `kSkillLoadoutAttributeResetRequired`；传 `true` 则按 `AttributePool.reset_cost_gold` 透明扣银两。 |
| **V-5** | **来回切换高体方案与全敏方案白嫖满血/满蓝** | 破坏回合制残血带出战斗规则 | `SaveLoadout` / `SwitchLoadout` 的属性重算沿用 `PlayerAttributeSystem::Recalculate` 比例保持公式 `hp = hp * newMax / oldMax` 且加点切换受 60s 冷却与 `InBattleComp` 拦截。 |
| **V-6** | **客户端篡改加点 map 传入超大 `uint32` 整数溢出或宝宝维度** | 溢出绕过总点数校验或串改宝宝点 | 所有点数累加全程 `uint64_t` 饱和校验，严格校验每个键属于角色属性池（`101..104`），并复核 `attribute_allocation_rules.h` 纯规则。 |
| **V-7** | **方案自定义名称注入 TMP 富文本标签或敏感词** | UI 排版破坏或内容安全过审风险 | 服务端按 UTF-8 1..12 字符校验并拒绝控制字符与 `<` `>`，过敏感词检测；客户端所有 `TMP_Text` 强制 `richText = false`。 |
| **V-8** | **租期到期时删掉方案或强行把玩家切到空点方案** | 玩家配好的 425 点方案丢失引发严重客诉 | 持久化与下发快照始终保留全部已保存的 1..7 方案，仅锁死 `scheme_id > unlocked` 的编辑与切入；当前激活高号方案允许保留至玩家主动切走。 |

---

## 5. 步骤分析与执行步骤

1. **阶段 1 · 文档与 UI 交接落盘**：写入本设计文档与 [skill-build-loadout-7slots-ui-handoff.md](ui-handoff/skill-build-loadout-7slots-ui-handoff.md)，注册文档索引。
2. **阶段 2 · 协议、配表与双端生成**：更新 `player_skill_comp.proto`、`client_request_ledger_comp.proto`、`transaction_log.proto`、`player_skill.proto`，通过幂等脚本向 `Tip.xlsx` 追加 7 条错误码；执行服务端导表与 `proto-gen-run`，补齐 `MessageLimiter.xlsx` 新 RPC 限流行并同步客户端 `gen.bat client`。
3. **阶段 3 · C++ 权威服实现与 GTest 验收**：升级 `skill_loadout_rules.h`、`afk_pass_rules.h`、`client_request_ledger.{h,cpp}`、`player_attribute.{h,cpp}`、`player_skill.{h,cpp}`、`player_equip_loadout.cpp`、`player_skill_handler.cpp`、`player_handoff_gate.h`；通过 `test_cpp_no_raw_pointer_member.py`、`test_cpp_thread_sources.py`、`check_log_text_english.py`，串行 `msbuild /m:1` 编译并跑绿 `skill_test`、`bag_test` 等全部定向单测。
4. **阶段 4 · Java 独立服对齐与 Maven 验收**：按双版本契约同步更新 Java `player_state.proto`（及持久化 schema 台账）、`SkillLoadoutRules`、`AfkPassRules`、`PlayerSkills`、`AttributeService`、`ClassSkillService`、`ClassSkillFeature`、`ClientRequestLedger`、`SkillLoadoutTest` 与 `PARITY.md`，跑绿 `game-player-store` 与 `game-scene` 单测。
5. **阶段 5 · Unity 客户端模型、可交互 uGUI 与 NUnit 验收**：升级 `SkillBookModels.cs`、`SkillBookMapper.cs`、`SkillBookClient.cs`、`SkillLoadoutSubPanel.cs`（7 套页签、自定义名、定位标签、四维加点编辑 + 6 套模板 + 复制方案 + 统一到期按套租用弹窗 + 专属切片自动换肤槽位），补充 NUnit 单测并跑绿 `client_compile_check.ps1` 与 EditMode 测试。
