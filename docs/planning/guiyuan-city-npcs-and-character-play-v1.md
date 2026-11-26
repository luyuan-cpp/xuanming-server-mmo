# 《五行奇谈》归元城全仓去问道化、8 位原创场景 NPC 与人物系统联动玩法设计（V1）

> 日期：2026-11-26  
> 状态：**已定稿并进入三仓闭环实现（C++/Go 权威服 + Java 独立服 + Unity 客户端全链路落地）**  
> 上位宪法：[project-vision.md](project-vision.md)（六条红线 + 准入四问 + 协作与异机作图硬规）  
> 关联基准：[gameplay-unified-design-v1.md](gameplay-unified-design-v1.md)、[world-map-play-v2.md](world-map-play-v2.md)、[character-appearance-switch.md](character-appearance-switch.md)、[character-background-collection-design.md](character-background-collection-design.md)

---

## 1. 需求分析

### 1.1 背景与必须同时解决的两个问题

1. **版权与合规红线（彻底去《问道》化）**：
   - 客户端历史静态主城清单 `mmorpg-client/Assets/Resources/World/Tianyong/Npcs/placements.json` 中残留 23 位《问道》天墉城风格占位 NPC（如 `杨镖头`、`屠娇娇`、`董老头`、`辛仁皑`、`段铁心`、`无想僧`、`无意僧`、`北斗星使`、`赤灵尊神`、`苏苏`、`仙界神捕`、`无名药铺老板`、`无名武器店老板`、`云游大仙` 等），以及部分在线 UI 文案与传记仍残留 `天墉城`、`八仙岛`、`东海渔村`、`揽仙镇`、`聚宝斋` 等历史旧名。
   - 哪怕像 `月老`、`朱雀`、`善财童子`、`陆压真人` 单个属公有神话或通用称谓，若与上述名单打包出现在同一套主城布局中，仍构成**整体组合抄袭与不正当竞争风险**。必须在玩家可见层全面替换为《五行奇谈》自有原创世界观（**归元城、八景岛、月汐湾、碧渊水宫、青石渡、万象阁**及 **8 位归元城原创核心 NPC**），同时标明旧静态贴图在正式上线前必须物理移除。
2. **场景 NPC × 人物系统的真实可玩性缺口**：
   - 当前在线主城（`BaseScene 1`，使用已验收单连通的 `biyuan-body-20261123` 导航底图）只有动态神秘商人 1 位可交互角色，城内缺少逛街代入感与“活人街坊”。
   - 永久衣柜已补齐 9 位现役原创人物（陆照禾/阿禾、霜见雪、许炽、苏青莲、林知音、桑听雨、祝停云、沈澄、星璇）与 3 册收藏系列（留灯院旧友、水火冰锋、清音济世），但换装行走在归元城里，场景世界对玩家当前身份毫无反馈；人物系统在加点/配装之后，也缺少零成本、无战损的**流派木桩验招场**、**回合残局考题**、**短篇人物奇谈**、**公开江湖名帖**与**组队同游彩蛋**。

### 1.2 上位红线准入四问自检（全部通过）

1. **普通零氪玩家**：8 位归元城 NPC 全免费交互；演武木桩、3 关回合残局、3 篇人物奇谈、3 条顺路捎物、灵潮五行签文、江湖名帖与同游彩蛋均**零每日打卡、零限时欠账、零门槛**（残局与奇谈支持免费 3×3 预设，1 级新手可随时试玩）；快捷 HUD 入口完整保留，不逼跑腿。
2. **个人开发者收入（官方 7:3）**：看人下菜碟彩蛋、人物奇谈试演、收藏系列组队同游气泡与公开江湖名帖，均把衣柜角色从“静默文字页”推向“大世界可见舞台”，自然拉动 10 元永久形象卡与永久外观收藏转化，不把付费流向送给工作室。
3. **物价与长期循环**：演武木桩（`kind=9`）、回合残局（`kind=10`）、归元人物奇谈（`kind=11`）在双服务端均实行**严格零战损隔离 + 零 660/周 150 装备材料 + 零重复战斗经验/银两 + 零赛季印/游历手记刷分 + 零野怪掉落**，彻底堵死脚本无限刷木桩/刷残局/刷奇谈冲击经济。
4. **防骗与资产安全**：所有隔离演武与奇谈绝不消耗背包药品、绝不扣减场外血蓝或灵宠状态、绝不修改装备绑定态；顺路捎物与灵潮求签均为零可交易资产，不构成任何交易诈骗或洗钱口子。

---

## 2. 设计分析

### 2.1 全仓「去问道化」统一替换总表

#### A. 玩家可见地名与交易平台名替换口径
- **执行边界**：全面清理玩家可见中文界面、传记、提示、测试断言中的旧名；**不盲目重命名底层代码标识符、命名空间、proto 类型或表文件名**（如 `TianyongMapRuntime`、`JubaozhaiClient`），以保持跨仓协议与既有工程零断裂。

| 历史旧名（禁止对外展示） | 《五行奇谈》正式原创名 | 适用范围 |
|---|---|---|
| `天墉城` | **归元城** | 主城（`Scene 1`）地图名、安全区提示、帮战/托管/游历/传记展示 |
| `八仙岛` | **八景岛** | 野外图（`Scene 2`）地图名、灵潮/栖息地/帮战/人物传记展示 |
| `东海渔村` | **月汐湾** | 野外图（`Scene 3`）地图名、灵潮/栖息地/帮战展示 |
| `揽仙镇` | **青石渡** | 储备图占位名 |
| `聚宝斋` | **万象阁** | 寄售与公开交易集市 HUD 入口、窗口标题、状态提示与商行页 |

#### B. 旧 23 位静态 NPC 退场与 8 位归元城原创核心 NPC 映射
- 旧 `Assets/Resources/World/Tianyong/Npcs/` 下的 23 张历史静态 PNG 与 `placements.json` **严禁作为正式在线主城内容加载**（仅暂留供历史 `TianyongCityNpcTests` 回归隔离，正式上线构建前整目录物理清理）。
- 在线主城（`Scene 1` 归元城）上线 **8 位 100% 原创核心 NPC**，在 Codex 专属立绘交付前，运行时统一复用项目自有原创形象目录 `QdaoCharacterCatalog`（零外部版权风险），交付后按 `npc_id` 零代码侵入替换专属立绘。
- **站位坐标全部取自已验单连通地标** `data/scene_nav_bin/current_maps/biyuan-water-palace/biyuan-body-20261123/navigation.json`（均在连通域 `component=0` 内，且与 `MysteryMerchantSpot` 的 6 处主城落点错开至少 4~8 单位防重叠）：

| 编号 | 原创 NPC 名称 | 称谓 / 职能 | 归元城地标与已验 Unity 坐标 `(x, z)` | 复用安全原创外观占位（待 Codex 专属替换） | 承接系统直达 | 专属场景玩法 |
|---|---|---|---|---|---|---|
| `guiyuan_smith` | **钟百炼** | 器坊铸师 | 西廊工坊口 `(90.0, 94.75)`（`westGallery`） | `11_jade_fist_flat_top_boy` | 装备打造 / 升档 / 打磨 / 背包 | 装备匠艺指引 + 人物彩蛋 |
| `guiyuan_merchant` | **宋满仓** | 牙行掌柜 | 南中廊市口 `(118.75, 105.06)`（`southMiddleGallery`） | `21_lidazui_hair_cook_boy` | 万象阁集市 / 灵石百宝阁 / 游历手记 | 商行见闻 + 阿禾面铺彩蛋 |
| `guiyuan_sparring` | **齐问锋** | 演武执事 | 中庭北阶 `(121.25, 161.0)`（`mainStairsBottom`） | `10_crimson_spear_girl` | 论剑 / 切磋坊 / 群仙问道赛 | **流派试招木桩（单/群桩）+ 回合残局考题 3 关** |
| `guiyuan_elder` | **玄衡子** | 观星长老 | 钟鼓阶前 `(241.25, 172.25)`（`gongStairs`） | `20_star_formation_master_girl` | 首领试炼 / 五行幻境 / 试炼志 | 阵法与星印问答 + 星璇/沈澄彩蛋 |
| `guiyuan_constable` | **顾青崖** | 巡城执令 | 中央廊道 `(193.75, 169.44)`（`middleGallery`） | `07_moon_shadow_assassin_girl` | 帮战追杀令 / 仇敌录 / 帮会 | **归元奇谈短案《留灯夜话》** |
| `guiyuan_herbalist` | **温半夏** | 百草堂主 | 东廊药铺口 `(255.31, 144.75)`（`eastGallery`） | `03_lotus_healer_girl` | 灵宠养成 / 技能配置 | **归元奇谈短案《药庐双结》** |
| `guiyuan_ferryman` | **周行舟** | 驿渡管事 | 南桥渡口 `(240.0, 38.5)`（`southBridge`） | `19_spirit_beast_tamer_boy` | 大世界灵潮传图 / 切线 | **归元奇谈短案《月汐渡口》+ 3 条不限时「顺路捎物」轻委托** |
| `guiyuan_storyteller` | **莫听风** | 茶肆说书人 | 桥畔茶亭 `(186.25, 117.25)`（`bridgeJunction`） | `15_water_dragon_scholar_boy` | 人物衣柜与人物志 / 社交 | **灵潮五行签文 + 人物「江湖名帖」与纯外观「同行之约」** |

> 注：以上 8 处坐标与 `MysteryMerchantSpot` 主城 6 处点位（`spawn 222.5,58.5`、`centralCourt 125,136`、`mainHallDoor 116.25,186`、`gongPavilion 261.25,185.375`、`teaPavilion 80,53.5`、`eastArchBridge 220,124.125`）全部零重合，均落在同一可走连通域 `component=0` 内。

---

### 2.2 场景 NPC × 人物系统七大玩法闭环规格

#### 玩法 1：8 位原创 NPC 场景实体化、点击寻路与「看人下菜碟」人物彩蛋
- 在归元城（`Scene 1`）实时生成 8 位原创 NPC 实体（无碰撞体，不挡寻路），复用 `MysteryMerchantNpc` 同款屏幕空间点击热区与 `TianyongPlayerController` 自动寻路靠近机制；亦可通过归元城 NPC 名录一键寻路。
- 点击 NPC 打开原创对话交互窗时，读取玩家当前生效外观 `active_appearance_id`（兼容 1 小时免费试用态），匹配全部 9 位现役原创人物的专属台词与头顶气泡反应（无特定关系时展示该 NPC 的归元城风物日常问候）。

#### 玩法 2：`齐问锋` · 人物流派演武试招木桩（双服权威隔离）
- **配置表**：
  - `Monster.xlsx` 追加：`301`（演武铁甲单桩：6000 血 / 90 护甲 / 8 攻 / 60 速）、`302/303/304`（三才演武木桩阵：3200/2200/2200 血 / 65/45/45 护甲 / 10 攻）。
  - `Dungeon.xlsx` 追加：`301`（单桩铁甲木桩，`scene_id=1, max_team_size=1, round_limit=8, boss_trial_kind=9, difficulty=1`）、`302`（三才群攻盾控木桩，`scene_id=1, max_team_size=1, round_limit=8, boss_trial_kind=9, difficulty=2`）。
  - `PlayMode.xlsx` 追加：`451`（`PVE_SOLO -> 301`）、`452`（`PVE_SOLO -> 302`）。
- **双服权威规则**（`kBossTrialKindPracticeDummy = 9`）：
  - `PrepareBattle` 校验玩家身处归元城（`scene_config_id == 1`），且**强制使用本人真实养成**（拒绝 `use_free_preset=true`）；满血蓝无耗材隔离入场。
  - 结算走严格隔离分支：**0 装备材料、0 经验、0 银两、0 赛季点、0 游历点、0 试炼志、0 掉落**，仅回传完整战报数据（伤害/疗盾/控场与回合数）供玩家校验加点、战法装配与人宠五行相生效果。

#### 玩法 3：`齐问锋` · 回合残局考题 3 关（双服权威隔离）
- **配置表**：
  - `Monster.xlsx` 追加：`311/312`（破甲试锋残局）、`313/314`（护主保宠残局）、`315/316`（先手断疗残局）。
  - `Dungeon.xlsx` 追加：`311/312/313`（`scene_id=1, max_team_size=1, round_limit=5, boss_trial_kind=10, difficulty=1`）。
  - `PlayMode.xlsx` 追加：`461/462/463`（`PVE_SOLO -> 311/312/313`）。
- **双服权威规则**（`kBossTrialKindTacticalPuzzle = 10`）：
  - 允许玩家自由选择「本人实战」或「借用免费 3×3 流派预设」（Go/Java `match` 按传入 `character_preset_id/pet_preset_id` 决议免费预设上下文），满血蓝无损隔离入场。
  - 结算同样零 660 材料/零重复刷经验银两/零赛季游历刷分；胜局经权威 `BattleEndS2C` 确认后点亮对应残局「演武明策」通关印记与战术要点复盘（可无限免费重玩）。

#### 玩法 4：`顾青崖 / 温半夏 / 周行舟` · 归元人物奇谈短案 3 篇（串联人物志与 `GrantAppearance` 批次三）
- **3 篇原创奇谈短案**（全程 3~5 分钟、无每日限时、开战前可阅读短引或一键跳过，支持试演免费预设）：
  1. `Dungeon 321`（`PlayMode 471`，`Monster 321`）：**《留灯夜话 · 巷口纸人案》**（引路人：**顾青崖**，联动人物：**陆照禾 / 沈澄**）
  2. `Dungeon 322`（`PlayMode 472`，`Monster 322`）：**《药庐双结 · 寒霜试药案》**（引路人：**温半夏**，联动人物：**苏青莲 / 林知音**）
  3. `Dungeon 323`（`PlayMode 473`，`Monster 323`）：**《月汐渡口 · 潮汐归舟案》**（引路人：**周行舟**，联动人物：**祝停云 / 桑听雨**）
- **双服权威规则**（`kBossTrialKindCharacterTale = 11`）：
  - 与残局同口径：满血蓝隔离、支持免费预设试演、零 660 材料/零重复经济产出；
  - 权威胜局通关后解锁该人物的**隐藏续篇见闻**（与衣柜已有 9 篇人物志互补），且服务端 `PlayerAppearanceSystem::GrantAppearance` 已就绪，二期若策划为新奇谈配置 `card_unlockable=0` 玩法专属角色即可在同结算点零改结构授形。

#### 玩法 5：`周行舟` · 不限时「顺路捎物」轻委托
- 3 条永不过期、绝不每日催肝的跨图顺路见闻单（不占 660、不发可交易料）：
  1. **八景岛 · 鸟谱青墨**（带一盒青松墨去 `Scene 2` 八景岛，回应桑听雨的《没有画的那一页》）
  2. **月汐湾 · 渡船麻缆**（带一捆新麻绳去 `Scene 3` 月汐湾，回应祝停云修旧渡船）
  3. **碧渊水宫 · 听雨茶包**（带一包安神莲心去 `Scene 5` 碧渊水宫，回应苏青莲与林知音）
- 玩家在 `周行舟` 处接单后，正常前往对应野外图抓宠/打本/逛灵潮时，依据服务端权威入场场景 `CurrentSceneConfigId` 自动记为“已送达”，回城或打开窗口即可确认归档并解锁对应归元风物笔记。

#### 玩法 6：`莫听风` · 灵潮五行签文（纯趣味、零战力）
- 按 `(playerId, UTC+8 日期, 当前大世界灵潮图 ID)` 确定性生成当日/本轮五行签文（金/木/水/火/土五象 × 灵潮出行提示，如“水德润物 · 宜携灵宠往碧渊水宫寻幽”），点击求签后可挂 30 分钟**纯视觉头顶吉签短语**（零数值、不求不亏、换日/换潮自动刷新）。

#### 玩法 7：人物「江湖名帖」+ 纯外观「同行之约」+ 收藏系列组队同游气泡
- **江湖名帖**：在 `莫听风` 处或归元城 NPC 面板一键查看/复制自己的公开江湖名帖——实时聚合权威快照：
  - 当前扮演人物档案名与原称呼、已拥有角色数与已点亮收藏系列印章（留灯院旧友 / 水火冰锋 / 清音济世）；
  - 等级 + 根据四维加点（体/灵/力/敏）与治疗护盾加成确定性推导的**擅长流派标签**（如「锋芒力破」「玄法通灵」「疾敏先手」「固本玄守」「济世护主」「五行均衡」）；
  - 永久赛季荣誉铭牌（`SeasonClient`）、五行试炼志战法印进度（`TrialJournalClient`）、出战得意灵宠（`PetClient`）、残局与奇谈见闻达成数；
  - 支持在莫听风处设定一句**纯外观「同行之约」宣言/好友同游标签**（严禁涉敏、仅本地预设短语库选择，绝不新增自由输入 UGC 审核漏口，且零属性羁绊！）。
- **收藏系列组队同游气泡**：组队成员在城内或战前同框时，若队伍中同时出现同收藏系列人物（如 `阿禾 + 祝停云`、`霜见雪 + 许炽`、`苏青莲 + 林知音`），按冷却节流自动在角色头顶弹出 2 秒互动气泡，增强收藏与多人社交感。

---

## 3. 思想分析

1. **为什么坚持“零新 RPC / 零新消息号 / 零 DB 迁移”完成双服权威闭环？**
   - 现有 `BattleSettlementData` 已携带 `battle_config_id`、`battle_difficulty` 与 `boss_trial_kind`，且 `PVE_SOLO` 匹配、免费 3×3 预设解析、场景权威校验与隔离结算路径全部成熟。通过扩充 `boss_trial_kind = 9/10/11` 三个常量与 8 行副本表，只需极少代码即可在 C++/Go 与 Java 双服达成 100% 一致的开战隔离与零刷料安全护栏，不冒改 proto 破坏并行会话的风险。
2. **为什么用 8 位原创核心 NPC 取代 23 位历史《问道》占位 NPC？**
   - 23 位无交互静态贴图既带来严重 IP 侵权隐患，又让主城视觉拥挤却空洞；收口为 8 位各司其职、与 9 位原创主角有生活羁绊的归元城街坊，每位 NPC 都有明确的“系统直达 + 专属彩蛋 + 特色轻玩法”，符合「有可玩性才做、绝不硬堆数量」的上位纪律。
3. **为什么「同行之约」采用预设雅号/短语而非自由输入框？**
   - 严守双端 UGC 内容安全红线（`mmorpg-client/AGENTS.md §2.3`）：任何新增自由输入 `string` 都必须打通服务端 `content_guard` 审核链；采用策划内置的 8 条国风同行短语既满足社交表达，又 100% 免除涉敏合规风险与额外后端维护成本。

---

## 4. 漏洞分析与防线

| 潜在漏洞 / 风险 | 严密防线 |
|---|---|
| **刷木桩 / 残局 / 奇谈无限薅 660 装备材料、经验、银两、赛季印或游历手记点数** | C++/Go 与 Java 双服在 `BattleProgressionRules`、`EquipmentMaterialReward`、`SeasonService`、`TravelPass` 全部显式拦截 `IsNpcIsolatedEncounterKind(boss_trial_kind)`（`9/10/11`），强制输出 0 材料、0 经验、0 银两、0 赛季点、0 游历点；所有新增怪物 `301..323` 的掉落/经验/银两/捕宠全为 0。 |
| **玩家在野外地图伪造发包原地开城内木桩/残局/奇谈** | C++ `PlayerBattleSystem::PrepareBattle` 与 Java `PlayerBattleService.prepare` 对 `boss_trial_kind ∈ {9, 10, 11}` 强制核验玩家当前权威场景 `scene_config_id == dungeon.scene_id`（即归元城 `1`），不符立即返回 `kFeatureUnavailable` 零写锁。 |
| **演武木桩被传入免费预设，误测非本人配装** | 双服 `PrepareBattle` 对 `kBossTrialKindPracticeDummy (9)` 显式拒绝 `use_free_preset == true`，且 Go/Java `match` 仅对 `kind ∈ {5, 10, 11}` 在携带预设参数时解析免费预设。 |
| **木桩/残局/奇谈耗损玩家外场血蓝、宠物或背包药品** | 双服 `PrepareBattle` 与结算统一将 `kind ∈ {9, 10, 11}` 纳入 `isolateBattleTempState` 与资产隔离分支，局内满血蓝开战、战后零回写外场消耗。 |
| **NPC 坐标踩水、穿墙或与神秘商人重叠** | 8 位原创 NPC 全部取自 `biyuan-body-20261123/navigation.json` 单连通域 `component=0` 已验地标，并与 `MysteryMerchantSpot` 6 处主城点位保持 ≥ 4~8 单位间距，单测强断言无重合且全部可达。 |
| **历史《问道》贴图残留上线风险** | 在线主城运行时彻底屏蔽旧 `TianyongCityNpcs`；运行时 8 位原创 NPC 统一加载项目自有原创 `QdaoCharacterCatalog` 形象，并列明旧目录 `Assets/Resources/World/Tianyong/Npcs/` 上线前整包删除清场项。 |
| **WebGL2 小游戏 / IL2CPP 兼容性与回调泄漏** | 客户端新增代码严格零多线程、零 `Task.Run`/`HttpClient`、零 `Reflection.Emit`；`CityNpcUiRoot` 在 `OnDisable`/`OnDestroy` 对称退订全部事件，进战斗/断线/换角自动收起窗口与热区并取消寻路。 |

---

## 5. 步骤分析

1. **阶段 A（策划与作图规范落盘）**：落盘本设计文档与 Codex 纯功能作图交接单 `docs/planning/ui-handoff/guiyuan-city-npcs-ui-handoff.md`，更新 `docs/README.md`。
2. **阶段 B（权威配表增量与导表）**：编写幂等补丁脚本 `tools/scripts/guiyuan_npc_gameplay_xlsx_patch.py`，追加 `Monster 301..323`、`Dungeon 301..323`、`PlayMode 451..473`，修订 `CharacterAppearance` 沈澄传记残余旧地名 `八仙岛→八景岛`，执行导表并验证全表完整性。
3. **阶段 C（C++/Go 权威服隔离与单测）**：更新 `turn_battle_constants.h`、`player_battle.cpp`、`battle_progression_rules.h`、`player_equipment_material_reward.cpp`、`player_season.cpp`、`player_travel_pass.cpp` 与 `go/match/internal/logic/global_match.go`，补充 GTest 与 Go 单测并过编译与静态检查。
4. **阶段 D（Java 独立服同步与 `PARITY.md`）**：同步 4 张裁剪/权威 `.pb` 表与 `manifest.json`，对等更新 `BattleConstants`、`PlayerBattleService`、`BattleSettlementService`、`BattleProgressionRules`、`EquipmentCraftService`、`SeasonService`、`GlobalMatchService` 与单测，更新 `PARITY.md` 并通过 Maven 回归。
5. **阶段 E（客户端去问道化 + 8 位原创 NPC 与人物玩法完整接入）**：
   - 收口客户端玩家可见界面旧地名/平台名并同步测试断言；
   - 实现纯 C# 模型与状态层（`CityNpcModels.cs`、`CityNpcCatalog.cs`、`CityNpcState.cs`）+ 世界表现与 UGUI 交互层（`GuiyuanCityNpcActor.cs`、`CityNpcWindow.cs`、`CityNpcUiRoot.cs`）+ 战斗入口（`BattleMatchEntryClient.StartNpcEncounter`）；
   - 同步客户端 `.pb` 表、补齐 `CityNpcPlayTests.cs`，通过 `client_compile_check.ps1` 与 NUnit 单测验收。

---

## 6. 执行步骤与验收清单

1. **配表生成验收**：运行 `python tools/scripts/guiyuan_npc_gameplay_xlsx_patch.py`（二次运行新增恒为 0）→ 导表工具全量导出 → `scene.exe --validate-tables` / Java `game-table` 校验通过。
2. **C++/Go 验收**：`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py` 全绿；MSBuild `/m:1` 编译 `table`/`scene`/`battle`/`bag_test` 0 错误；GTest 覆盖木桩/残局/奇谈的场景校验、木桩禁预设、隔离结算 0 材料/0 经验/0 银两/0 赛季/0 游历；`go test ./internal/logic/... ./internal/catalog/...` 全绿。
3. **Java 验收**：`mvnw.cmd -B -pl game-battle-engine,game-scene,game-match -am test` 针对新规则与回归用例全绿，`PARITY.md` 登记完整。
4. **Unity 客户端验收**：`tools/sync_tables.ps1` 同步最新 `.pb` → `tools/client_compile_check.ps1` 全量 0 错误 → NUnit 覆盖 8 位 NPC 站位不与神秘商人重合、9×8 人物差异化对话、收藏系列组队气泡、五行签文确定性、江湖名帖流派推导、顺路捎物到位判定、残局/奇谈权威胜局点亮，以及去问道化 UI 文案断言全绿。
