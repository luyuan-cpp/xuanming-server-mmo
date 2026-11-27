# 《五行奇谈》装备品相 · 技能制书 · 名匠手艺 V2–V5 演进策划与工程设计

> 日期：2026-11-26  
> 状态：**全部闭环验收完成 — 服务端（C++ 权威逻辑、协议、限流表、`bag_test` 40/40 与 `scene.exe --validate-tables` 83/83）、Unity 客户端（`client_compile_check` 814 文件 0 错误、NUnit 157/157 全绿）与 Codex 交付的 5 张 V2–V5 增量透明 PNG + `.meta`（`image@3462274` 逐字节 SHA-256 核对通过）已全链路接入完毕**  
> 上位承接：[V1/V1.1 装备品相、率土式制书与封卷设计](equipment-quality-and-skill-crafting-v1.md) · [率土式叠加成长战法 V2（卷 7）](growing-passive-skills-v2.md) · [旧装回收与玩家交易 V1](equipment-recycling-trade-economy-v1.md) · [项目设定](project-vision.md)  
> 关联交接单：[V2–V5 增量图标纯功能交接单](ui-handoff/equipment-craft-v2-v5-ui-handoff.md)

---

## 1. 需求分析

### 1.1 背景与 V1/V1.1 之后尚存的 4 项高可玩性体验缺口

V1 与 V1.1 已经闭环了「装备 5 档品相打磨（只升不降+8 次硬保底）+ 无损换词条 + 7 卷 95 技永久制书 + 80% 拆页 + `3414 封卷道籍` 封卷/开卷流通 + 主城原子名匠代打」。但在真实长线游玩中，仍有 **4 个直击玩家爽点与社交黏性的结构缺口**：

1. **V2 缺口 · 主城西市名匠缺乏「江湖口碑与身份辨识」**：目前所有宗师在西市列表里只差一个 `50–100` 银两的标价，买家只能机械挑最便宜的 50 银两，缺少「谁是本服真正在铸极品的大宗师」「亲手打过多少把极品」「佩戴哪块专属零属性宗师雅号印款」的回合制社交排面与筛选手段。
2. **V3 缺口 · 逐次手动单磨过于琐碎，且缺少全身品相工坊健检**：全身 6 件可打磨装（12 条词条）每条最多磨 8 次，逐手点「打磨一次」+ 二次确认操作冗余；玩家也无法一眼看到全身还有几条未满极品、最坏按现有保底进度还要准备多少重铸砂与银两。
3. **V4 缺口 · 7 卷 95 技缺乏藏经阁收集反馈、批量拆页与流派缺书指引**：随着卷 1–7 扩充至 95 项永久技能，玩家很难一眼看清各卷收集度、缺少零战力膨胀的收集里程碑残页反哺；随机拓印连出多本重复常规书时逐本点拆页繁琐；新手/中阶玩家配流派时仍需跨 7 个卷宗翻找缺哪几本书。
4. **V5 缺口 · 缺少「同队情义零费代打」与「封卷道籍悬赏求书板」**：当前宗师给同队好友/队友代打也被迫收 50–100 银两与 5% 税；而买家想要某本特定可交易常规技能书（尤其是带宗师署名的热门书）时，只能等别人先摆货，缺少主动出银两托管发榜求书、卖家手握 `3414 封卷道籍` 或书架常规书一键揭榜交卷的供需撮合闭环。

### 1.2 硬红线（严禁触碰）

- **0 数值天花板膨胀**：绝不新增第 6 档品相、不加“全极品套装共鸣额外战力”、不提高 100% 词条上限；名匠雅号与藏经阁里程碑均为**纯社交荣誉或绑定残页循环反哺**。
- **0 抽卡 / 0 碎装 / 0 采集体力条 / 0 稀世占比超标**：永久池稀世占比保持 `8 / 95 = 8.42% <= 10%`。
- **0 UGC 内容安全漏洞**：名匠招牌与雅号全部采用**服务端权威预设白名单枚举**，不开放任意自由输入文本，天然规避合规审核风险。
- **0 洗白与防绑定染毒**：智能连磨严格沿用 `TRADEABLE_ONLY` 隔离；悬赏求书发榜仅允许扣除**非绑定银两**，揭榜仅接受 `bound == false && rarity_tier == 0` 的常规书/封卷并扣 5% 集市银两税销毁；卷宗收集里程碑仅发放**永久绑定悟道残页（`3406`, `bound = true`）**。
- **0 新线程 / 全操作幂等防重放**：全部写 RPC 接入 `ClientRequestLedgerComp` + `client_request_id` + 乐观修订水位。

---

## 2. 设计分析

### 2.1 V2 · 名匠江湖口碑、6 档零属性雅号印款与西市筛选排序

1. **持久化名匠履历字段（`PlayerCraftComp` 新增，同事务落盘）**：
   - `proxy_orders_completed`（`uint32`）：作为宗师累计完成的原子代打/代制订单总数。
   - `supreme_forged_count`（`uint32`）：亲手或代工产出 5 档金标极品（非豁免词条）累计次数。
   - `rare_books_crafted_count`（`uint32`）：亲手或代工撰出稀世绝学书累计次数。
   - `master_showcase_tag`（`uint32`）：服务端预设安全招牌枚举（`0=综合宗师 / 1=专攻极品铸器 / 2=专攻典籍制卷 / 3=平价快单接单 / 4=双绝天工名匠`）。
   - `equipped_craft_title_id`（`uint32`）：当前佩戴的零属性宗师雅号印款 ID（`0..6`），仅可佩戴已解锁 ID。
2. **6 档零属性宗师雅号里程碑（由 `equip_craft_quality_rules.h` 纯规则实时计算，老档自动回溯解锁）**：

| `title_id` | 雅号名称 | 解锁门槛（0 战力加成） | 署名呈现示例 |
|---:|---|---|---|
| `1` | 铸器宗师 | 铸器 Lv3（经验 `>= 60`） | `铸器宗师·清风` |
| `2` | 百炼宗师 | 铸器 Lv3 且 `supreme_forged_count >= 5` | `百炼宗师·清风` |
| `3` | 天工神匠 | 铸器 Lv3 且 `supreme_forged_count >= 20` | `天工神匠·清风` |
| `4` | 制卷宗师 | 制卷 Lv3（经验 `>= 60`） | `制卷宗师·清风` |
| `5` | 万卷书圣 | 制卷 Lv3 且（`rare_books_crafted_count >= 3` 或 `proxy_orders_completed >= 15`） | `万卷书圣·清风` |
| `6` | 双绝天工 | 铸器 Lv3 且制卷 Lv3 | `双绝天工·清风` |

3. **署名升级与向后兼容**：当宗师佩戴已解锁 `equipped_craft_title_id` 时，产出极品装或技能书的 `crafter_name` 自动刻为 `<雅号>·<角色名>`；未选（`0`）时保持 V1 原样 `<角色名>`，前端兼容两种展示。
4. **西市口碑展示与排序筛选**：`CraftProxyStallInfo` 下发口碑三项统计、招牌枚举、佩戴雅号与情义免税标记；客户端西市支持按「综合口碑 / 极品件数 / 最低手工费」排序及按「全部 / 铸器宗师 / 制卷宗师 / 同队免税」筛选；`SetCraftProxyStallRequest` 一并支持更新招牌、雅号与同队情义免税开关（即便关摊也可独立更新雅号）。

---

### 2.2 V3 · 工坊智能定档连磨（原子保底即停）与全身品相健检

1. **协议最小增量（复用既有 `PreviewEquipmentCraft` / `CommitEquipmentCraft`，零新增 RPC）**：
   - `EquipmentCraftChoice` 新增：
     - `target_quality_tier`（`uint32`）：`0` 表示 V1 单次打磨；`2..5` 表示「智能定档连磨至该品相（须严格大于当前品相档）」。
     - `max_polish_attempts`（`uint32`）：单次请求允许的最多手数（`0/1` 退化为单磨，`2..8` 受 `kPolishPityLimit = 8` 封顶）。使用定星保底符（`use_guarantee_charm == true`）时硬锁为 `1` 手直接出极品。
   - `PreviewEquipmentCraftResponse` 新增：`max_polish_attempts`、`max_polish_sand_cost`、`max_polish_gold_cost`（按当前 `8 - polish_pity_count` 与玩家实际可负担上限预估最坏预算）。
   - `CommitEquipmentCraftResponse` 新增：`polish_attempts_used`、`repeated PolishStepTrace polish_steps`、`actual_gold_spent`。
   - `ClientRequestRecord` 新增：`equipment_polish_attempts_used = 38`、`equipment_actual_gold_spent = 39`，弱网重放精准复现实际手数与实耗银两。
2. **原子精准扣费语义**：
   - 服务端先校验第 1 手必填材料/银两/防染毒隔离并锁定 `effectiveMaxAttempts = min(requestedMax, kPolishPityLimit - currentPity, maxAffordableSteps)`；
   - 在单次 `CommitEquipmentCraft` 内串行执行 `ResolvePolish`，一旦 `tier >= targetTier || tier >= 5` 立即终止并得到 `actualAttempts`；
   - 随后通过 `BagService::ExchangeInventoryExact` **仅按 `actualAttempts` 精准扣除实际手数的重铸砂、基础银两与代打手工费**（若用 2 手就出极品，只扣 2 份砂与 2 手费用，绝不多扣），并按实际手数累加铸器熟练度与宗师口碑（出极品时 +1 `supreme_forged_count`）。
3. **全身品相工坊健检（客户端纯 C# 模型 + `EquipmentWindow.Polish.cs`）**：
   - 实时扫描全部可打磨装备（已穿戴优先），汇总「极品槽位数 / 总可打磨槽位数」、「速度豁免槽数」、基于每条现有 `polish_pity_count` 精确估算的「满极品最坏还需重铸砂与银两」，并提供一键直跳最低品相词条槽按钮。

---

### 2.3 V4 · 典籍藏经阁（7 卷里程碑位图、批量拆页与流派缺书体检）

1. **卷宗收集里程碑位图（遵循 `docs/rules/bit-storage-and-table-ids.md`）**：
   - `PlayerCraftComp` 新增 `bytes claimed_scroll_milestone_bits = 15;`（低位在前，去末尾全 0 字节，高位未知 bit 原样透传）。
   - 位号编码：卷宗 `scroll_id >= 1` 的 **小成档（`tier = 1`，本卷已解锁技能数 `>= ceil(total / 2)`）** 对应 bit `(scroll_id - 1) * 2`；**圆满档（`tier = 2`，本卷 `100%` 解锁）** 对应 bit `(scroll_id - 1) * 2 + 1`。
   - 奖励内容（0 战力、防洗白）：小成领 **`20` 张绑定悟道残页**（`3406`, `bound = true`）；圆满领 **`40` 张绑定悟道残页**（`3406`, `bound = true`）。
   - 新增权威幂等 RPC **`ClaimScrollCollectionMilestone`（558）**（`ClientRequestKind::kClaimScrollMilestone = 24`）。
2. **批量一键拆页（`BatchDismantleSkillBooks`，559）**：
   - 支持单次批量传入 `1..20` 个 `book_id`（`ClientRequestKind::kBatchDismantleSkillBooks = 25`）；服务端校验零重复、全在书架、全为常规书（`rarity_tier == 0`），单事务销毁全部书并一次性发放 `sum(dismantle_fragment_count)` 张绑定悟道残页。
   - 客户端提供「一键勾选全部已习得/多余重复常规书」安全辅助，永不自动选中未习得孤本或稀世书。
3. **6 大流派配招缺书体检（客户端 `SkillCraftModels.BuildBookAdvisor` + `SkillCraftWindow`）**：
   - 覆盖 6 大核心流派（物理速推 / 法术爆发 / 反击肉核 / 毒火持续 / 护盾辅助 / 封印控场），实时对照 `book_unlocked_skill_ids` 与 `scrolls`，逐项标明已学/缺失及所属卷宗，点击缺失技能直接跳转至对应卷宗选中并定位到「定向拓印 / 找宗师代制」。

---

### 2.4 V5 · 江湖工坊社交闭环（同队情义零费代打 + 西市封卷悬赏求书板）

1. **同队情义零费原子代打（复用 `scene` 既有 `TeamId` 组件，零跨服竞态）**：
   - `PlayerCraftComp` 新增 `bool team_free_proxy = 18;`（宗师可在西市名匠设置中开关）。
   - 当买家与宗师同属一个非空队伍（`buyerTeam.team_id() != 0 && buyerTeam.team_id() == masterTeam.team_id()`）且宗师开启 `team_free_proxy` 时，装备代打/打磨与典籍代制的手工费自动降为 **`0` 银两（0 税、无跨账号资金流动）**，同时完整保留宗师 10% 极品/返页率、宗师雅号署名与口碑单数累计。
2. **西市「封卷悬赏求书板」（托管银两、零骗局供需撮合）**：
   - `PlayerCraftComp` 新增 `repeated SkillBookWantedOrder wanted_orders = 16;` 与 `uint64 next_wanted_order_seq = 17;`。
   - 规则硬约束：
     - 仅限求购启用中的常规永久技能（`rarity_tier == 0`）；
     - 悬赏额范围 `100..50000` 银两，发榜时**强制只扣非绑定银两**（防绑定银两洗白成流通银两）；
     - 单人同时最多挂 `3` 条生效悬赏单；支持勾选 `require_master_signature`（仅收带宗师署名的封卷/书）；
     - 随时可撤单并 **100% 原额退回银两**；
     - 揭榜交卷时卖家可用背包内匹配的未绑定 `3414 封卷道籍` 或书架内匹配的未绑定常规书一键履约；严格禁止自己揭自己的榜；成交扣 `5%` 银两税销毁（`ProxyNetGold`），净银两入卖家账，原技能书（保留 `bound = false` 与完整宗师署名）直接送入买家书架；买家书架已满（上限 `100` 本，见下条）时拒绝这一次交付，卖家的书与银两都不动。
     - 未学书架同时至多存 `100` 本（2026-11-26，数据覆写审计 r2-craft#2）：满了制书、开卷、被揭榜送书都拒绝，先研习、拆页或封卷腾位置。
     - **待落码的口径变化（负责人 2026-11-26 定，设计见 [悬赏求书迁 trade 托管](../design/skill-book-wanted-order-trade-escrow.md)）**：揭榜改由 go/trade 托管合同完成，发榜时托管悬赏银两、揭榜时托管封卷道籍、先把书交给买家再付卖家 95%；书架里的书由客户端先封卷再揭榜（仍是一键）；买家收到的是背包里一枚封卷道籍（可一键开卷入书架）；悬赏榜全服可见。落地之前按上一条的现行规则运行。
   - 新增 4 个权威 RPC：
     - `PostSkillBookWantedOrder`（`560`，`kPostWantedOrder = 26`）
     - `CancelSkillBookWantedOrder`（`561`，`kCancelWantedOrder = 27`）
     - `ListSkillBookWantedOrders`（`562`，只读查询全城在线悬赏单 + 本人悬赏单）
     - `FulfillSkillBookWantedOrder`（`563`，`kFulfillWantedOrder = 28`）

---

## 3. 思想分析

1. **为什么 V2–V5 全部坚持“社交排面 + 操作爽感 + 收集反哺 + 供需撮合”，而零碰战力上限？**  
   回合制长青经济的核心生命力来自「人与人的连接」和「刷图/构筑的正反馈」，而不是无穷无尽的新数值坑。名匠口碑雅号让宗师有真实江湖地位，智能定档连磨消除无意义重复点击，藏经阁里程碑把 95 技的广度转化成良性循环，同队情义代打与悬赏求书板把“打书换钱、帮队友打极品”落成有温度的社交场景。
2. **为什么悬赏求书支持「背包 `3414` 封卷」与「书架未绑书」双源直交？**  
   既让已经封卷在背包流通的 `3414` 随时能变现，又避免卖家为了揭榜还得先多点一次「封卷入包」再揭榜的冗余操作，所有校验复用同一份 `bound == false && rarity_tier == 0 && skill_table_id` 权威防线。
3. **为什么不把所有招牌/雅号做成玩家自由输入？**  
   严格遵守全仓 UGC 合规红线与 KISS 原则：预设精选道家工坊雅号与招牌无需引入额外异步内容审核回调链，且在 UI 排版与审校上 100% 可控。

---

## 4. 漏洞分析与防线总表

| 潜在漏洞 / 风险 | 攻击路径或后果 | 硬防线 |
|---|---|---|
| **连磨多扣砂或零成本“偷看随机结果”** | 若拆成多次提交或在扣费前返回结果，玩家可断线白嫖或被多扣未使用手数 | 单次原子 `CommitEquipmentCraft` 内按 `effectiveMaxAttempts` 串行结算后，仅按实际 `actualAttempts` 执行 `ExchangeInventoryExact`；幂等键落 `ClientRequestLedgerComp` 防重放双扣。 |
| **连磨静默混扣绑定砂污染可交易装备** | 玩家给纯 T 装备开 8 手连磨，第 3 手用完 T 砂后偷偷扣 B 砂变绑 | `allow_bound_materials == false` 时按 `RemovableTradableCount` 计算可负担手数，绝不混扣 1 份绑定砂或绑定定星符。 |
| **未解锁宗师雅号伪造佩戴或超范围招牌** | 客户端篡改 `equipped_craft_title_id = 6` 或 `master_showcase_tag = 99` | 服务端 `SetCraftProxyStall` 强制核对 `IsCraftTitleUnlocked(...)` 与 `tag <= 4`，非法直接拒并返回 `kInvalidParameter`。 |
| **卷宗收集里程碑重复领取刷残页** | 并发或重发 `ClaimScrollCollectionMilestone` 反复领 20/40 残页 | `claimed_scroll_milestone_bits` 位图查重 + `expected_revision` + `ClientRequestLedgerComp` 三重锁，且产出的残页强制 `bound = true` 不可交易。 |
| **批量拆页传重复 `book_id` 或混入稀世绝学** | 传 `[101, 101]` 试图把 1 本书拆出 32 张残页，或误拆稀世绝学 | 服务端用 `std::set` 查重并逐本校验存在且 `rarity_tier == 0`，任一非法整批原子回滚拒绝。 |
| **悬赏求书用绑定银两发榜再撤单/小号揭榜洗成流通银两** | 拿任务/广告给的绑定银两发榜，撤单或小号揭榜洗出非绑银两 | 发榜强制 `strictGold = reward_gold, preferBindGold = 0`（严禁使用绑定银两），揭榜严禁 `poster == seller`，成交扣 5% 银两税销毁。 |
| **悬赏揭榜用绑定书或无署名书冒充宗师书** | 交绑定常规书洗白，或交学徒书骗取宗师署名悬赏高价 | 服务端严格拦截 `bound == true || rarity_tier != 0`，且 `require_master_signature` 时必验 `!crafter_name.empty() && crafter_player_id != 0`。 |
| **揭榜跨两名玩家的存档（数据覆写审计 r2-craft#1）** | 现行 `FulfillSkillBookWantedOrder` 在同一帧改发榜人与卖家两份存档、各自存盘，两笔提交互不相干（同一节点也一样，并不是原子提交）；两笔之间进程崩溃或一方存盘被拒，就多出一份银两或一本书 | 改走 go/trade 托管合同（求购单 kind 7）：发榜托管银两、揭榜托管封卷道籍、先交书后付 95%，客户端改调 trade；设计与分批见 [skill-book-wanted-order-trade-escrow.md](../design/skill-book-wanted-order-trade-escrow.md)，落地前的残余见该文 §7.1。书架上限（r2-craft#2）去掉了「把存档顶到行上限让一方必败」的人为构造路子。 |

---

## 5. 步骤分析

1. **阶段 1（策划与交接落盘）**：落盘本文与纯功能图标交接单 [equipment-craft-v2-v5-ui-handoff.md](ui-handoff/equipment-craft-v2-v5-ui-handoff.md)，刷新 `docs/README.md`。
2. **阶段 2（服务端协议、限流表与生成物）**：扩展 `player_craft_comp.proto`、`client_request_ledger_comp.proto`、`player_bag.proto`、`player_skill.proto`，在 `data/MessageLimiter.xlsx` 登记 `545..550`，执行 `proto-gen-run` 与全量导表 `run.py`。
3. **阶段 3（服务端 V2–V5 实现与单元测试）**：更新 `equip_craft_quality_rules.h`、`client_request_ledger.{h,cpp}`、`player_equipment_v1.cpp`、`player_craft.{h,cpp}`、`player_skill_handler.cpp`、`player_handoff_gate.h`，在 `player_craft_quality_test.cpp` 补齐 V2/V3/V4/V5 全套 GTest，跑裸指针门禁、`msbuild`、`bag_test` 与 `scene.exe --validate-tables`。
4. **阶段 4（Unity 客户端全链路接入与测试）**：同步 `sync_tables.ps1` / `gen_proto.ps1` / `gen_messageids.ps1`，更新 `PlayerFeaturesClient.Crafting.cs`、`EquipmentCraftUiState.cs`、`EquipmentScreenModel.cs`、`EquipmentWindow.Polish.cs`、`SkillCraftClient.cs`、`SkillCraftModels.cs`、`SkillCraftWindow.cs`，补齐客户端 NUnit 单测并通过编译与单测验收。

---

## 6. 执行步骤

- [x] **步骤 1**：落盘本设计文档与异机 Codex 纯功能图标交接单 [equipment-craft-v2-v5-ui-handoff.md](ui-handoff/equipment-craft-v2-v5-ui-handoff.md)，刷新 `docs/README.md`。
- [x] **步骤 2**：更新服务端 4 份 `.proto` 与 `data/MessageLimiter.xlsx`（`558..563`），执行 `proto-gen-run` 与导表并通过 `scene.exe --validate-tables`（83 表）。
- [x] **步骤 3**：实现服务端 V2（口碑/6 档零属性雅号/预设安全招牌/筛选）、V3（智能定档连磨原子保底即停）、V4（7 卷里程碑位图 + 20 本批量拆页）、V5（同队情义 0 费代打 + 悬赏求书板）及全套 C++ GTest 验证（40/40 通过，裸指针门禁 13/13 通过）。
- [x] **步骤 4**：同步 Unity 客户端契约（83 表 / 92 proto / 201 MessageIds），完整接入 V2–V5 业务客户端、纯 C# 模型（含全身品相健检与 6 大流派缺书体检）与 uGUI 界面，`client_compile_check.ps1`（810 文件 0 错误）与 7 套 NUnit 单测（151/151）全绿。
- [x] **步骤 5**：同步更新 `docs/PROGRESS.md` 并完成 Codex 交付 5 张 V2–V5 增量透明 PNG（`image@346227407d9abe5aa5a5143b83132b2eb6926d80`）的客户端导入、`.meta` 配置（含 `badge_team_free_proxy` `x/z=20` 横向九宫格）、无字底板运行时叠加字与 `SeasonEconomyUiArtTests` 自动化契约单测（157/157 全绿）。
