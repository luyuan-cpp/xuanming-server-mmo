# 《五行奇谈》全局玩法统一收口设计方案与单一事实源总账（V1.1 拍板定稿）

> 日期：2026-11-25（由 2026-11-24 提案收口定稿；同步核对 11-24 至 11-25 已拍板落码的全部权威 V2/V3 规格，完成全仓下位玩法文档口径扫平）
> 状态：**已拍板并完成下位文档统一收口**（不覆盖工作区并行会话未提交文件；本稿作为跨系统单一事实源索引与剩余 P1 缺口清单）
> 上位宪法：[项目设定](project-vision.md)（六条红线 + 准入四问：**好玩优先、真不肝、免费 100% 同战力上限、官方 7:3、物价稳得住、游戏内防骗与资金安全**）

---

## 1. 需求分析

### 1.1 收口背景与两日演进核对
2026-11-24 首次全盘体检发现仓库在早期 V1 稿件间存在 4 处根部冲突与 5 处口径分裂（正式 PVP 预设 vs 自养撞车、每日 20 份 vs 永久 660 撞车、首领参数三稿打架、商业化落点空心化、遗留旧稿残留“神兽充值抽奖 / 不可指挥 / 每日捐献 8 次 / 已删角色闻砚生”等）。

2026-11-24 至 11-25 期间，负责人陆续拍板并推动多条核心线落地了更精细的正式规格（`season-pvp-pve-economy-unified-v1.md`、`equipment-economy-charter.md`、`equipment-quality-and-skill-crafting-v1.md`、`battle-skill-and-pet-cultivation-expansion.md`、`boss-trial-planning-ui.md` V2、`long-term-progression-v2.md` V2.1、`ranked-play-v2-overhaul.md` / `v3`、`guild-play-v2.md`、`battle-festival-beast-implementation-contract.md`、`spirit-shop-and-starter-packs-v1.md` 等）。本轮拍板收口的目标是：

1. **绝不回退已拍板落码的新规格**：以 2026-11-25 最新权威基线为准，修正 11-24 初版提案中已被更优落地口径取代的细项（如正式会武锁定纯真实养成、技能装配落地为 `1 开场 + 3 被动 + 1 追击`、阶段二材料周上限由 100 调至 150 且 `+11` 锁定 100 颗百炼石/次）。
2. **终结全仓遗留旧稿误导**：为已全面被替代的 V1/候选稿加注明确的「已作废/已替代」顶部横幅并保留原路径不破链接；对仍生效但残留旧数字/旧规则的遗留文档补齐收口覆盖块。
3. **显式隔离剩余未落地 P1 缺口**：将尚未落码的玩法闭环短板（角色平滑升级曲线、低级基础装原件无损升档、常规银两循环校验、挂机月卡升级为「仙途轻享卡」、Java 独立服 `PARITY.md` 同步）单列追踪，避免误报“已全部完成”。

---

## 2. 设计分析：五大核心系统权威定稿与单一事实源总表

### 2.1 全仓玩法单一事实源矩阵（所有下位文档与 UI 交接一律以此为准）

| 玩法域 | 唯一权威事实源 | 2026-11-25 拍板收口口径 | 已替代/需受覆盖的旧稿 |
|---|---|---|---|
| **上位红线与策划硬规** | [project-vision.md](project-vision.md) | 六条红线 + 准入四问（好玩优先、真不肝、免费 100% 同战力上限、官方 7:3、物价稳得住、防骗与资金安全）+ 第三节协作硬规（全端逻辑/代码/UI 闭环、**有可玩性才扩/严禁硬扩展**、协议与编测归 Gemini、Codex 只作图、写完 `.md` 须直接给可复制正文、**只告诉 Codex 做什么 UI 严禁给 GPT 定风格/色号**） | — |
| **核心循环与两阶段材料账** | [core-gameplay-loop.md](core-gameplay-loop.md) + [season-pvp-pve-economy-unified-v1.md](season-pvp-pve-economy-unified-v1.md) | **阶段一**：每角色永久 **660 份**无日清（首套 600 + 60 余量；单账号终身 ≤60 份可交易且仅限胜局完整基础 10 中至多 1 份；普通胜 10 / 困难胜 15 / PVP 胜 15 / PVP 达标平负 10 全绑定，负/平须满足 `≥3 回合 && ≥2 手操 && 有效贡献`）。<br>**阶段二**：满 660 后自动切入**每周 150 份全绑定**平滑池（UTC+8 周一 05:00 重置，2026-11-25 从 100 调至 150）。 | [battle-season-one-simple-design.md](battle-season-one-simple-design.md)、[battle-season-one-ui-handoff.md](ui-handoff/battle-season-one-ui-handoff.md)（已替代） |
| **正式 PVP 与轮换娱乐场** | [ranked-play-v2-overhaul.md](ranked-play-v2-overhaul.md) + [ranked-play-v3-tactical-prep.md](ranked-play-v3-tactical-prep.md) + [season-team-tournament-v1.md](season-team-tournament-v1.md) | **正式「人宠会武」（`PVP_TEAM`）固定使用本人真实养成**（不混入新手借用预设）；冷启动开关 `allowed_team_sizes = [1, 3]`，V2 含主目标凝锋 +20%、20 回合上限、战意渐盛、校准五段门槛与 6 胜锋芒、第 5 回合体面投降、V3 15 秒战前换印/换备宠。<br>**6 套 V2 4 技能免费预设仅用于娱乐轮换（护卫救援 PVE / 物法易势 5V5）与自定义切磋房**，绝不写正式 Elo。跨区战队赛走《群仙问道赛》。 | [ranked-play-season-ui-candidate.md](ranked-play-season-ui-candidate.md)、[ranked-play-v1-implementation.md](ranked-play-v1-implementation.md)（已作废） |
| **首领试炼与五行试炼志** | [boss-trial-planning-ui.md](boss-trial-planning-ui.md)（V2）+ [long-term-progression-v2.md](long-term-progression-v2.md)（V2.1） | 三大首领（破阵/灵障/玄毒）+ 破阵 V2 五战阵（含困难双卫同阵）、慢重劈排常规行动后、12/15 回合上限、集中破防与 2–5 人策应卫、普通 10 / 困难 15 份；试炼志 V2.1 三卷多维「攻·守·巧」战法印 + 复盘快照 + 法宝拓印/绑定重铸砂里程碑 + `M901` 专属形象。 | [long-term-progression-v1.md](long-term-progression-v1.md)（已作废） |
| **装备八部位、`+11`、品相与制书** | [equipment-v1-affixes-artifacts.md](equipment-v1-affixes-artifacts.md) + [season-pvp-pve-economy-unified-v1.md](season-pvp-pve-economy-unified-v1.md) + [equipment-quality-and-skill-crafting-v1.md](equipment-quality-and-skill-crafting-v1.md) | 八部位（4 基础装 +1～+10 必成、2 首饰、2 法宝），**唯一 `+11` 顶格突破**（`material_count = 100` 颗百炼石/次、10% 概率、不掉级、满 20 次硬保底、+24% 总攻防）；**5 档窄区间品相**（80%–100%，速度豁免，8 次只升不降硬保底，重铸换词条 100% 保留品相与保底计数）；率土式拆技制书（稀世 ≤10%，拆解返 80% 残页）；旧 `equip_profile=0` 与宝石系统封存不向新玩家开放。 | [equipment-eight-slot-planning.md](equipment-eight-slot-planning.md)（加覆盖说明） |
| **经济总纲、防骗与官方 7:3 商业化** | [equipment-economy-charter.md](equipment-economy-charter.md) + [spirit-shop-and-starter-packs-v1.md](spirit-shop-and-starter-packs-v1.md) + [equipment-recycling-trade-economy-v1.md](equipment-recycling-trade-economy-v1.md) | 官方 7:搬砖 3；官方灵石百宝阁直售（101–103 终身共享 300 权重绑定提速包、201 定星保底符、202 天工秘墨、301 形象卡、302 改名卡）+ 300 灵石月卡基座（P1 升级为轻便利+搬砖门票「仙途轻享卡」，绝不做掉落翻倍或登录打卡）；卖家信用（过户清空+异地陌生设备清 0+防刷分）、游戏内防骗、防绑定染毒。 | [equipment-recycling-trade-economy-candidate.md](equipment-recycling-trade-economy-candidate.md)（已作废） |
| **人宠技能装配与宠物养成** | [battle-skill-and-pet-cultivation-expansion.md](battle-skill-and-pet-cultivation-expansion.md) + [season-skills-and-seal-design.md](season-skills-and-seal-design.md) + [player-pet.md](../design/player-pet.md) | `A40` 正式定名「太清灵盾」；人物已学主动全可用 + **`1 开场 + 3 被动 + 1 追击` 装配槽（3 套免费方案 + `exclusive_group` 互斥）** + 4 免费赛季技能 + 单槽 4 选 1 赛季印；宝宝 3 分类手动每回合 1 招 + 3 免费条目预设 + 五阶升阶（20→28 点，同档封顶不碾压）；**彻底废止“神兽充值抽奖”与“宝宝不可手动指挥”旧口径**。 | [player-pet.md](../design/player-pet.md) §8 旧述（已同步修正） |
| **帮会玩法与低人口轻量化** | [guild-play-v2.md](guild-play-v2.md) + [guild-play-v2-ui-handoff.md](ui-handoff/guild-play-v2-ui-handoff.md) + [battle-jianghu-grudge-duel-bounty-v1.md](battle-jianghu-grudge-duel-bounty-v1.md) | 取消 2v2 排队池，升级为**全天无匹配随时开打 + 野外 3 处灵脉据点（每帮限 1）+ 守旗阵离线快照 + 江湖追杀令与仇敌录**；`guild-phase2/` 重肝默认值（每日捐献 8 次 / 405 万升级资金 / 中秋 10 人同时在线）上线前配表强制轻量化覆盖。 | [guild-play-v1.md](guild-play-v1.md)、[guild-play-ui-handoff.md](ui-handoff/guild-play-ui-handoff.md)（已作废）；[guild-phase2/README.md](../design/guild-phase2/README.md)（加覆盖块） |
| **世界地图、活动神兽与外观收藏** | [world-map-play-v2.md](world-map-play-v2.md) + [battle-festival-beast-implementation-contract.md](battle-festival-beast-implementation-contract.md) + [character-appearance-switch.md](character-appearance-switch.md) | 世界地图开服严控「主城 + 2～3 野外图」Fill-First 聚人 + 双层灵潮 + 22 宠分图栖息 + 异步在场感；「瑞兽迎春」14 天不限日次累计 3 胜免费领绑定神兽（接两阶段 660/周 150 材料账）；永久多人物收藏衣柜现役 8 人（2 初始免费 + 3402 形象卡永久买断 + `card_unlockable=0` 玩法专属 + 3 收藏系列），彻底移除已删角色 `17_ghost_script_calligrapher_boy`（闻砚生）；灵宠翻牌纯免费休闲 0 材料。 | [world-map-play-v1.md](world-map-play-v1.md)（已作废）；[battle-festival-beast-v1.md](battle-festival-beast-v1.md)、[character-background-collection-design.md](character-background-collection-design.md)、[pet-memory-minigame-v1.md](pet-memory-minigame-v1.md)（已同步对齐） |

### 2.2 相对 2026-11-24 初版提案的四项拍板变更说明
为避免新老读者混淆，明确记录以下四处已由负责人最新拍板替代 11-24 提案初稿的结论：
1. **正式会武不混入新手借用预设**：11-24 提案曾建议“正式论剑允许新手勾选免费借用套”；最新拍板明确为**正式会武 100% 纯本人真实养成**，6 套 V2 4 技能免费预设仅归娱乐轮换（护卫救援 / 物法易势 5V5）与自定义切磋房，彻底划清边界。
2. **阶段二与 `+11` 取代“满 660 仅给银两+绑定砂”**：11-24 提案建议 660 后只转银两与重铸砂；最新已落码口径升级为**阶段二每周 150 份全绑定自选材料（支撑唯一 `+11` 100 石/次 20 次硬保底 + 全法宝/第二套）+ 试炼志 V2.1/帮战 V2/锦标赛同步提供绑定重铸砂、悟道残页与法宝拓印**，横向换流派与温和长线追求兼得。
3. **技能装配槽采用 `1 开场 + 3 被动 + 1 追击` 而非 `4 主动 + 2 自动`**：已落地口径保持人物全部已学主动技能可随时在局内见招拆招（更符合经典回合制博弈手感），策略取舍落在 `1 开场 + 3 被动 + 1 追击`（加 `exclusive_group` 互斥）与 3 套战外预设方案上。
4. **帮会主舞台升级为 V2 全天无匹配帮战**：不再沿用 V1 的 2v2 匹配队列，改以 [guild-play-v2.md](guild-play-v2.md) 为帮会玩法唯一主轴。

---

## 3. 思想分析

1. **为什么保留历史 V1 文件原位并加「已作废」横幅，而不是批量物理搬移到 `docs/archive/`？**
   仓库的 `docs/PROGRESS.md`、`equipment-economy-charter.md`、`world-map-play-v2.md`、`long-term-progression-v2.md`、`guild-play-v2.md` 及异机 UI 交接记录中存在数十条指向旧 V1 文件的相对链接（且并行会话正在编辑 `docs/PROGRESS.md` 与 `docs/README.md`）。沿用 `ranked-play-season-ui-candidate.md` 与 `ranked-play-v1-implementation.md` 已确立的**顶部权威作废横幅**规范，既能零成本阻断误读，又实现零断链、零并行冲突。
2. **为什么必须单列“已落地基线”与“剩余 P1 玩法缺口”？**
   诚实区分“文档已收口且 C++/Go 已落码”与“仍待工程补齐的 P1 短板”，能避免开服前出现“玩法规则看似齐全，但新号进游戏后升级靠 GM、低级装造完舍不得强化、月卡只有断线托管”的真实断层。

---

## 4. 漏洞分析与剩余 P1 开放缺口清单

以下缺口不属于文档冲突，而是需要在正式不删档前按优先级补齐的工程与配置项：

| 缺口项 | 当前现状 | 潜在玩法风险 | 收口建议与防线 |
|---|---|---|---|
| **G1. 角色平滑升级曲线未接通** | [player-attribute-allocation.md](../design/player-attribute-allocation.md) 与 [player-pet.md](../design/player-pet.md) 明确经验已累计入账但升级曲线未接，角色升级仍依赖 `GmSetPlayerLevel` | 新号无法通过首场试炼自然升级加点，卡住技能重数、宠物携带等级与高档装备 | 已落盘完整六节工程与 UI 设计：[player-leveling-and-battle-economy-v1.md](player-leveling-and-battle-economy-v1.md)（1→85 级曲线、全玩法经验与周平滑银两软上限、双服同构与客户端 UI），待并行会话提交后分 3 批落地 |
| **G2. 低级 V1 基础装无损升档/继承未落码** | `+11` 保底与品相重铸文档已约定“未来装备升档 100% 继承强化、保底与品相”，但具体升档接口与消费规则尚未落地 | 1 级新手用首套 660 额度打造 1 级武器后，升到 30/60 级若无法无损升档会直接浪费永久材料 | 优先落地「四件 V1 基础装花轻量银两原地升档（或随 `min(生效等级, 开放档)` 无损重算）」，100% 保留 `enhance_level`、`enhance_pity_count`、已装词条及 5 档品相/打磨进度 |
| **G3. 银两（`kCurrencyGold`）产出/回收平衡待实测** | 重铸换词条、打磨品相、买普通宠、摆摊税、帮战追杀令均耗银两，但瑞兽迎春等部分活动关闭银两掉落 | 免费玩家可能出现“有材料有重铸砂却缺 20 银两改词条”的卡手，或脚本刷爆银两 | 在首领试炼、会武、帮战 V2 与明雷首领保持稳定温和银两产出（带防刷递减与日/周软顶），首测采集单角色 7 日银两净流校验 |
| **G4. 「挂机月卡」→「仙途轻享卡」权益升级待落地** | [afk-monthly-card.md](../design/afk-monthly-card.md) 当前仅实装断线跟队托管（`afk_pass_expires_at`），尚未接入 3→6 配装方案槽、搬砖可交易资格门票等总纲规划 | 30 元月卡在单人轻量局为主的体验下购买动机偏弱，且缺少搬砖防多开门票闸 | 复用既有 `afk_pass_expires_at` 单一事实源，在 P1 挂载配装方案槽扩展、离线摆摊时长延长与经济总纲定义的可交易产出资格（严禁月卡掉落翻倍或每日领灵石） |
| **G5. Java 独立服对账（`AGENTS.md §12`）与并行草稿尾数** | C++/Go 已领跑多项 V2/V3 玩法，部分新模块在 Java 仓 `xuanming-server-mmo/PARITY.md` 仍处于待同步状态；另有并行会话未提交文档 `spirit-shop-and-starter-packs-v1.md`（§6 第 404 行）与 `wuxing-gameplay-expansions-v1.md`（第 12/20/101 行）仍残留旧 `周 100` / `660/100` 字样 | 双服运行语义漂移；并行文档合入后可能重新引入 `周 100` 旧数 | 持续按 `PARITY.md` 逐项核销并跑通 `ContractSync --check`；并行会话收尾上述两份未提交文件时同步改为 `周 150` / `660/150` |

---

## 5. 步骤分析（阶段划分）

1. **阶段 0 · 文档统一收口（本轮已完成）**：
   - 将本稿升级为单一事实源总账与现状对账表；
   - 为 7 份已全面被替代的 V1/候选稿加注顶部「已作废/已替代」横幅（原位保留不破链接）；
   - 为 11 份仍被引用但残留旧规则的遗留文档补齐 2026-11-25 口径对齐覆盖块或精准修订；
   - 将 6 份 11-24/25 活跃文档中残留的旧 `周 100` 与 `61 项自由技能` 口径统一修正为 `周 150` 与最新正式技能库口径。
2. **阶段 1 · P1 玩法体验补齐（按需单独立项授权）**：
   - 优先落盘并实现 G1（角色平滑升级曲线）与 G2（V1 基础装无损升档继承），随后接通 G4（仙途轻享卡权益挂载）与 G3（首测银两流验证）。
3. **阶段 2 · 双服全量核销与上线前配表锁死**：
   - 按 `guild-phase2/README.md` 顶部 2026-11-25 覆盖块执行上线前轻量化改表；
   - 核销 `xuanming-server-mmo/PARITY.md` 全部待同步条目并通过双服回归与真机试玩验收。

---

## 6. 执行步骤（本轮文档收口已落地清单）

1. **总纲定稿**：更新本文件 [gameplay-unified-design-v1.md](gameplay-unified-design-v1.md)，锚定 2026-11-25 全部权威基线并单列 G1–G5 剩余缺口。
2. **7 份已替代旧稿顶部加注作废横幅**：[world-map-play-v1.md](world-map-play-v1.md)、[equipment-recycling-trade-economy-candidate.md](equipment-recycling-trade-economy-candidate.md)、[long-term-progression-v1.md](long-term-progression-v1.md)、[guild-play-v1.md](guild-play-v1.md)、[guild-play-ui-handoff.md](ui-handoff/guild-play-ui-handoff.md)、[battle-season-one-simple-design.md](battle-season-one-simple-design.md)、[battle-season-one-ui-handoff.md](ui-handoff/battle-season-one-ui-handoff.md)。
3. **11 份遗留现役文档补齐口径对齐**：[equipment-eight-slot-planning.md](equipment-eight-slot-planning.md)、[equipment-v1-affixes-artifacts.md](equipment-v1-affixes-artifacts.md)、[equipment-recycling-trade-economy-v1.md](equipment-recycling-trade-economy-v1.md)、[battle-festival-beast-v1.md](battle-festival-beast-v1.md)、[battle-festival-beast-logic-handoff.md](battle-festival-beast-logic-handoff.md)、[battle-festival-beast-ui-handoff.md](ui-handoff/battle-festival-beast-ui-handoff.md)、[player-pet.md](../design/player-pet.md)、[afk-monthly-card.md](../design/afk-monthly-card.md)、[pet-memory-minigame-v1.md](pet-memory-minigame-v1.md)、[guild-phase2/README.md](../design/guild-phase2/README.md)、[character-background-collection-design.md](character-background-collection-design.md)。
4. **6 份近期活跃文档同步最新数值（`周 100 → 周 150` / 技能库口径）**：[long-term-progression-v2.md](long-term-progression-v2.md)、[ranked-play-v2-overhaul.md](ranked-play-v2-overhaul.md)、[ranked-play-v2-ui-handoff.md](ui-handoff/ranked-play-v2-ui-handoff.md)、[season-pvp-lunjian-v1-repaired.md](season-pvp-lunjian-v1-repaired.md)、[season-team-tournament-v1.md](season-team-tournament-v1.md)、[guild-play-v2.md](guild-play-v2.md)。
5. **收尾自检**：全批严格 `< 30` 文件、零触碰并行会话未提交文件，执行 `git diff --check` 与关键字复扫确认无残留冲突口径。
