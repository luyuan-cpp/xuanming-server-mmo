# 《五行奇谈》世界地图生态、双层潮汐聚人与低人口在场感设计（V2）

> 日期：2026-11-23（闭环核查与文档修订：2026-11-26）  
> 状态：**M1 全部玩法逻辑、C++/Go 服务端、Java 独立服、Unity 客户端代码及 `WorldMapV2` UI 切图/uGUI 接入均已闭环落地并验证；仅余 `月汐湾(场景3)` 纳入默认开放/灵潮池的后续收口与 M2 玩法扩展待负责人确认**  
> 与 V1 的关系：本稿依据负责人 2026-11-23 确认的四项定调（根治 V1 的 5 个硬伤、开服严控「1 主城 + 2~3 野外」、采用「双层潮汐 + 地图热度」自然聚堆、构建极低人口「异步在场感」）全面替代 [world-map-play-v1.md](world-map-play-v1.md) 的“野外走路拾取核心装备材料”路线；V1 原稿保留作历史候选记录。

---

## 1. 需求分析

### 1.1 核心目标与负责人已拍板方向

围绕[项目设定](project-vision.md)（**好玩优先、不肝、小小氪金**）与[核心循环 V1](core-gameplay-loop.md)（有空就玩、每角色永久首套 660 份装备材料上限、取消每日打卡），本次世界地图玩法需要同时解决两个产品痛点：

1. **让玩家有真实、好玩、各不相同的理由前往不同地图**：不再把所有野外地图做成同质换皮，也不通过强制每日跑图打卡逼肝。
2. **从根源减少开服初期玩家分散、部分地图冷清的问题**：无论开服当下同时在线是 3 人、30 人还是 3,000 人，都能让活人自然聚在同屏、感受到世界有烟火气。

负责人已确认的四大定调（取代 V1）：

| 编号 | 拍板方向 | 解决的 V1 原稿硬伤 |
|---|---|---|
| D1 | **彻底撤掉“走路 150 秒捡 1 份核心装备材料”及其重型跨角色防作弊分源账本** | 硬伤 1（交互无聊且工程投入产出比倒挂）+ 硬伤 5（挤占 660 免费战斗盘与付费/交易空间） |
| D2 | **严格控图：开服只开「1 主城 + 2~3 野外」**，其余 40+ 预览/节庆图走单图限时轮换 | 硬伤 3（地图过多稀释早期人口）+ 硬伤 4（纳入已双端上线的场景 5 碧渊水宫） |
| D3 | **底层聚人路由 +「双层潮汐 + 地图热度」制造自然聚堆** | 硬伤 2（跨图无引力）+ 硬伤 3（现行“最空优先”分线算法与 16 线种子主动拆散玩家） |
| D4 | **极低人口（1–5 人）采用「异步在场感」托底，不用假人充数** | 硬伤 3（低谷期鬼城感），同时为永久收藏外观与交易提供真实展示舞台 |

### 1.2 当前仓库地图资产与既有系统事实盘点（2026-11-23 只读核实）

| 地图（`BaseScene` ID） | 真实地貌与现成资产证据 | 当前双端可达状态 | V2 开服定位 |
|---|---|---|---|
| **1 · 主城 / 天墉城** | 太极广场、南北长街、西市铺面、东北道观、西北双擂台、东南月兔园；客户端已有 23 个静态视觉 NPC（`Tianyong/Npcs/placements.json`）与 6 个神秘商人站位（[mysterymerchantspot.json](../../generated/tables/mysterymerchantspot.json)） | 双端旧图 `main_scene.bin` 可用；本地 [scene_manager_service.yaml](../../go/scene_manager/etc/scene_manager_service.yaml) 遗留 `"1": 16` 压测分线种子，开服环境必须收口为 1 | **首发常驻（社交与市集枢纽）** |
| **2 · 八仙岛**（新皮：八景岛） | 八座仙居环绕中央圆庭、海岸石路与码头 | 双端旧图 `penglai_scene.bin` 导航一致；新版 `bajingbody20261114` 在客户端 `catalog.json` 暂标 `clientOnly: true` | **首发常驻（野外图 A）** |
| **5 · 碧渊水宫** | 320×256 精确水宫图，含 16 个已验单连通地标（南桥、太极中庭、主殿石阶/主殿门、东西北连廊、钟鼓亭、茶亭、东南观景台等，[navigation.json](../../data/scene_nav_bin/current_maps/biyuan-water-palace/biyuan-body-20261123/navigation.json)），自带春节/中秋双景 | **已正式双端对齐并开放联机**（`BaseScene 5` + 客户端 `QdaoMaps20261102/catalog.json` 中 `clientOnly: false`，提交 `4df0ee51e`） | **首发常驻（野外图 B · 旗舰新图）** |
| **3 · 东海渔村 / 月汐湾** | 码头停舟、渔舍渔市、海湾广场与滩涂（新图 `yuexi-bay-20261115` 自带双景，且人物志[祝停云](character-background-collection-design.md)已锚定月汐湾摆渡人家） | **当前失配**：服务端 `BaseScene 3` 已切 `yuexi-bay-20261115`，客户端仍标 `clientOnly: true` 回退旧图 | **修齐双端后立刻开放（野外图 C）** |
| **储备池（4 揽仙镇 + 9 节庆导航图 + 2 手绘节庆图 + 36 端游原图静态预览）** | 见[节庆导航清单](../../data/scene_nav_bin/festival_maps_20261109.json)、[手绘导航清单](../../data/scene_nav_bin/authored_maps_20261112.json)、[端游原图对账设计](../design/pc-original-static-backgrounds-20261123.md) | 均未开放正式传送（`productionTravelEnabled=false`） | **绝不常驻铺开**，留作后续节日/赛季单图限时轮换 |

**可直接复用或扩展的既有系统事实：**

- **神秘商人**：[mystery_merchant.go](../../go/scene_manager/internal/logic/mystery_merchant.go) 的 `spotsByConf` **已支持按 `MysteryMerchantSpot.scene_config_id` 跨多张大世界图随机落位**；客户端 [MysteryMerchantUiRoot.cs](../../../mmorpg-client/Assets/Scripts/UI/Ugui/Gameplay/MysteryMerchantUiRoot.cs) 的 `SyncNpc()` 已支持在任意地图渲染和点击，仅缺跨图“一键前往”的传送分支（当前对非当前图返回 `OtherMapText`）。
- **野外抓宝宝**：[pet.json](../../generated/tables/pet.json) 已有 22 种宠物，[player-pet.md](../design/player-pet.md) 战斗内捕捉（`Dungeon.allow_pet_capture=1`）已通过 C++ / Java 双服与实机冒烟，目前仅挂在单一 `Dungeon 4`，尚未按世界地图分生态池。
- **分级观战（L0 / L1 / L2）**：[battle-spectate-tiered-fanout.md](../design/battle-spectate-tiered-fanout.md) 已完成双服与客户端闭环验收；缺口仅是大世界同屏玩家头顶暴露公开 `watchable_battle_id` 并提供一键点击观战入口。
- **战报回放**：[battle-report-replay.md](../design/battle-report-replay.md) 已完成 PVP 参战者回放闭环，但当前 `BR-8 / BR-9` 明确限定**只录 PVP、且仅列表内参战者可读**；因此野外试炼碑一期只展示权威通关纪录与阵容摘要，公开 PVE 战报回放列为明确的二期扩展，不假定现成可用。
- **离线实体摆摊**：[stall-system.md](../design/stall-system.md) 已拍板主城固定摊位、离线营业、全线同显，开工依赖寄售引擎主干合入等前置。
- **永久收藏衣柜**：[character-appearance-switch.md](character-appearance-switch.md)（2026-11-23 批次二）已升级为永久买断衣柜制，并预留 `card_unlockable = 0` 与服务端内部发放接口 `GrantAppearance`，可承接后续地图风物/试炼专属外观激励。

---

## 2. 设计分析

### 2.1 严格控图与四图生态分工（一图一玩法、绝不同质换皮）

开服首发**只开放 3 张图**：`1 主城 + 2 八仙岛 + 5 碧渊水宫`；修完 `3 月汐湾` 双端一致后开放至**完整 4 图生态（1 主城 + 3 野外）**。

核心经济边界：**核心首套装备材料（660 份永久额度）始终只从战斗产出，三张野外图的明雷首领与常驻首领试炼共用同一份 PVE 基础 10 份**；地图本身主打**灵宠生态、地貌战术差异、奇遇消耗品/重铸砂与社交展示**，不逼玩家每天跑遍全图。

| 地图 | 核心角色 | 专属野生灵宠栖息池（拆分现成 22 宠） | 本图共享明雷首领（共用 PVE 10 份） | 本图独家地标轻玩法（不占 660 核心料） |
|---|---|---|---|---|
| **1 · 主城 / 天墉城** | 社交、交易与约局枢纽 | 无 | 无 | ① 西市**离线实体摆摊街**（全线同显、人走摊在）<br>② 西北双擂台**就地切磋 + 路人围观**<br>③ 太极广场「五岳灵潮榜」查看各图热度与一键直传<br>④ 休闲区挂[灵宠翻牌](pet-memory-minigame-v1.md)（后续支持同桌竞速） |
| **2 · 八仙岛**（新皮：八景岛） | 问道试剑岛（高敏/法系生态） | 仙风高敏向：`云鹤`、`竹风狸`、`霞角鹿`、`岚铎仙`、`砚羽灵`、`烛翎` 等 | **「风眼引风」**（中央圆庭明雷）：第 1 回合风眼预告下轮护势，主动攻击打断者将被点名承受下轮慢动作风击；主打**人宠抗伤分工与防御时机** | **仙居「仙弈残局」**：八座仙居石桌提供 1–2 分钟的固定 3 回合人宠解谜战（借用系统预设人宠与敌阵，如“保护宝宝 + 控制护卫”）。每题首次通关给一次性银两/风物印记/少量重铸砂，可无限免费重玩练手 |
| **5 · 碧渊水宫** | 玄水秘境（肉盾/辅助生态） | 水系耐打向：`汐螺`、`玄潮龟`、`露华灵`、`月弦师`、`霜团貂`、`石灵` 等 | **「水镜钟鸣」**（主殿门/太极中庭明雷）：每 2 回合交替敲响「阳钟（物攻增效、法抗略抬）」与「阴鼓（法攻增效、物抗略抬）」；主打**人宠物法混编与出手节奏** | ① **钟鼓亭「鸣钟聚灵」**：玩家现场敲钟，向同线广播 3 秒太极中庭水面玉波特效 + 15 分钟本图纯视觉莲华光环与寻宠氛围提示（不影响正式 PVP/战力）<br>② **茶亭「水宫试炼碑」**：展示本服近期最快回合通关本图首领的前 3 队真实玩家名与出战灵宠阵容（二期接公开回放） |
| **3 · 东海渔村 / 月汐湾** | 海潮渔港（物攻/爆发生态） | 海滨快攻向：`灵狐`、`金猊`、`赤砂魁`、`符小虎`、`绛铃`、`芽铃童`、`葫团团`、`桂灯童`、`苍嶂麟`、`绯耳灵` 等 | **「涨退潮」**（码头/海湾广场明雷）：公开下轮涨潮（敌方蓄势高减伤）或退潮（敌方破绽易伤）；主打**稳守控场与退潮窗口集中爆发** | **「退潮赶海」**：仅在月汐湾处于灵潮期时，滩涂刷出共享可见的潮汐礁石/漂流宝匣（**每人每轮限交互 3 次，各自可开不抢点**）。随机触发三选一趣味事件：① 跳出高资质野宠立即进入捕捉战；② 拾取银两/灵兽口粮/海湾风物或少量重铸砂；③ 2 分钟海妖奇袭快战 |
| **后续储备图**（节庆/手绘/端游改造） | 限时主题秘境 | 当期返场/主题灵兽 | 当期活动首领（如[瑞兽迎春](battle-festival-beast-v1.md)） | **全区同一时段至多开放 1 张限时活动图**，档期结束关闭传送门，人口自然回流至常驻 1+3 地图 |

> **抓宠生态不逼肝底线**：所有普通宠物仍保留商店银两直买渠道（[player-pet.md](../design/player-pet.md) §10.1）；去对应野外地图亲自遇敌捕捉的差异化乐趣在于**免银两购买 + 有机会遇到更高资质区间胚子 + 灵潮期罕见色/高成长惊喜**，玩家按兴趣前往，不被强迫刷图。

### 2.2 根治分散：Fill-First 分线聚人 + 双层潮汐 + 地图热度

#### 2.2.1 选线路由升级：从「最空优先」改为「Fill-First 软上限顺序填满」

当前 [scene_atomic.go](../../go/scene_manager/internal/logic/scene_atomic.go) 的 `luaReserveBestWorldChannel` 始终选择 `SCARD` 最小的线，在多线状态下会主动把低峰玩家平摊打散。V2 将自动选线升级为两段式可配策略：

1. **开服默认单线**：生产/首测环境所有大世界地图初始 `WorldChannelCount = 1`，压测专用 `WorldChannelCountByConfId: {"1": 16}` 严格隔离在压测配置文件中；启动清理/核对 Redis `world_channels:desired:zone:{z}`。
2. **Fill-First 软上限算法**（`WorldChannelPickPolicy = fill_first`，压测配置可显式保留 `least_loaded`）：
   - 新增配置项 `SoftFillThreshold`（建议默认 `200`，远低于扩容阈值 `ScaleOutPlayerThreshold = 2000`）。
   - 候选线在传入 Lua 脚本前按 `channel_no` 升序排列（1 线、2 线……）。
   - Lua 脚本扫描所有存活且非排空中的候选线：
     - **阶段 A（聚人优先）**：挑**第一个满足 `count < SoftFillThreshold` 的线**（即 1 线未满 200 人时，所有进图玩家 100% 进入 1 线）；
     - **阶段 B（高负载均摊）**：若所有活线均已 `>= SoftFillThreshold`，自动回落到**选 `count` 最小的线**，防止单线过载。
   - **与自动缩容联动**：沿用 [world-channel-autoscale.md](../design/world-channel-autoscale.md) 的 `ScaleInPlayerThreshold = 100` 与 `CooldownSeconds = 120`；高峰过去后，多出来的 2 线人数自然跌破 100 被排空合并回 1 线（确保 `SoftFillThreshold > ScaleInPlayerThreshold` 留足滞回带，防止震荡）。

#### 2.2.2 全服「双层潮汐」聚人机制

| 潮汐层 | 周期与时长 | 覆盖范围 | 驱动力（为什么玩家会自然涌过去） | 防逼肝/防错过约束 |
|---|---|---|---|---|
| **微观潮汐 ·「五行灵潮」** | 每 **60 分钟**（可配 60/90 分钟）轮换 1 次，每次持续 **40 分钟** | 全区同一时刻**仅 1 张**已开放野外图进入灵潮（按轮次轮转，尽量避开上一轮同图） | ① **视觉变脸**：客户端加载轻度灵潮氛围（风雾/水波/退潮礁石）<br>② **神秘商人联动**：本轮整点神秘商人高权重（如 75%–100%）直接落在这张灵潮图的 **1 线地标**<br>③ **灵潮专属事件**：月汐湾刷赶海礁石，八仙岛/水宫刷巡游明雷灵兽与高资质胚子加成 | 每小时自动轮换，玩家任何时段上线 15 分钟都能遇上正在进行的灵潮；错过不扣任何 660 装备额度 |
| **宏观潮汐 ·「今日灵脉旺地」** | 每日 UTC+8 05:00 或每 2–3 天轮换 | 全区仅标 1 张野外图为今日旺地 | 主城「五岳灵潮榜」与地图窗高亮推荐；旺地提供更顺手的公共轻互动（如敲钟特效、试炼碑推荐位）与微微上调的灵潮权重 | **绝不削减其他野外图的基础战斗奖励**，也不设“今日必须去旺地打卡”的红点任务 |

#### 2.2.3 地图热度可视化与一键聚人传送

- `scene_manager` 领导者每 2–5 秒汇总本 zone 所有开放大世界地图的频道人数、当期灵潮图、推荐聚人线（`recommended_scene_id`，即当前未爆满的最小线号活线）与神秘商人简况，写入一把全局读模型 `world_map_overview:{ctl:<zone>}`（与 `world_channel_directory`、`mystery_merchant` 完全同构的“Go 领导者写 Redis → C++/Java Scene 只读转发”模式）。
- **热度聚合档位**（只发枚举档位，绝不暴露玩家列表或把 `player_id` 放进指标标签）：
  - `CROWDED`（热闹）：该图当前总人数 ≥ 阈值 B（首测可配 5–10 人，正式服按在线曲线调参）；
  - `ACTIVE`（有人）：1 ≤ 人数 < 阈值 B；
  - `QUIET`（清幽）：0 人。
- **客户端一键直达**：
  - [CityTravelUiRoot.cs](../../../mmorpg-client/Assets/Scripts/UI/Ugui/Gameplay/CityTravelUiRoot.cs) 地图窗展示每张图的**正式名、开放状态、热度档位、是否当期灵潮/商人所在**，点击灵潮卡或“前往神秘商人”直接发起 `EnterScene(target_scene_config_id, recommended_scene_id)`（若目标线恰好满/回收，服务端自动回落至该图 Fill-First 最佳线，不报错卡死）。
  - 现有 [MysteryMerchantUiRoot.cs](../../../mmorpg-client/Assets/Scripts/UI/Ugui/Gameplay/MysteryMerchantUiRoot.cs) 解除“非当前地图禁止前往”的限制，打通一键跨图直传商人所在图与线。

### 2.3 极低人口（1–5 人）时的「异步在场感」设计

当服务器刚开服或凌晨仅有 1–5 名真人在线时，**拒绝投放低质感假人 AI 充数**，通过四项真实真人痕迹消解空城感：

1. **主城离线实体摆摊**：复用 [stall-system.md](../design/stall-system.md)（人摊分离、支持离线营业 12 小时、全线同显）。凌晨 3 点全服只有 2 个人在线，天墉城西市依然可见十几个真实玩家的摊棚、摊名与待售旧装/材料。
2. **野外「试炼碑」真人留名**：在八仙岛圆庭、碧渊水宫茶亭、月汐湾广场各立一块权威交互石碑，展示本区近期通关该图明雷首领**最少回合的前 3 名/前 3 队**（角色名、通关回合数、出战宝宝种类与达成时间，由战斗权威结算原子更新）。
   - **一期**：展示榜单与人宠阵容摘要（轻量且零存储膨胀）。
   - **二期**：在 [battle-report-replay.md](../design/battle-report-replay.md) 基础上新增“受控公开 PVE 榜首战报”白名单（每图仅保留上榜前 3 份 gzip 回放，换榜即覆盖旧份，彻底避开全量 PVE 录像爆炸与 `BR-9` 越权问题），玩家在石碑旁点一下即可直接观看高手如何用人宠破解该图机制。
3. **同屏「共享明雷 + 激战中一键观战」**：
   - 野外明雷首领与灵兽采用**“全线可见实体、共享触点、各自独立开战”**，彻底消除抢怪摩擦。
   - 处于野外地图且正在进行公开可观战战斗的玩家，在场景 AOI 状态中附带公开标志与 `watchable_battle_id`；路过玩家看到对方头顶“激战中”图标，点一下即可调用已闭环的 `WatchBattle(battle_id)`（[battle-spectate-tiered-fanout.md](../design/battle-spectate-tiered-fanout.md)）进入实时观战。
4. **全内容 1 人带宠可玩**：所有野外明雷首领、灵宠捕捉、仙弈残局、月汐湾赶海均支持 **1 人带宝宝立即可开**，2–5 人已有队伍按实际人数同图同线开打，零强制凑队等待。

### 2.4 四大可复用技术底座与经济护栏

#### 底座一：`WorldDirector`（`go/scene_manager` + Java 同构）
- 承接 `Fill-First` 选线、灵潮轮换状态机、神秘商人灵潮联动权重与 `WorldMapOverview` Redis 读模型发布。
- 严格遵循一库一服务与只读分离：`scene_manager` 领导者写 Redis，各 Scene 节点本地缓存并转发给客户端，不引入跨节点写事务。

#### 底座二：`WorldPoi` 统一地图交互点框架（C++ / Java Scene + Unity）
- 借鉴神秘商人与摆摊的成熟模式：**不在 Scene 节点上维护数百个跑复杂 AI 寻路的怪物实体**，而是以配置表 + 灵潮轮次读模型驱动统一的 `WorldPoi`（权威点位 ID、类型、`scene_config_id`、坐标、交互半径、开放窗口/灵潮条件、关联玩法配置 ID）。
- 一次抽象统一承载 5 类交互：
  1. `POI_BOSS_ENCOUNTER`（本图明雷首领）
  2. `POI_PET_HABITAT`（本图野生灵宠明雷点）
  3. `POI_CHESS_PUZZLE`（八仙岛仙弈残局石桌）
  4. `POI_TIDE_FORAGE`（月汐湾灵潮赶海礁石，按轮次限 3 次 + 客户端幂等键）
  5. `POI_LANDMARK_SOCIAL`（水宫钟鼓亭敲钟广播 / 各图试炼碑查询）
- **权威校验统一收口**：玩家点击任何 POI 时，Scene 节点统一核验① 所在 `scene_config_id` 与 POI 一致；② 服务端权威坐标距离 `<= interact_range`；③ 灵潮窗口有效；④ 未处于战斗/观战/跨区冻结中；⑤ 队伍开战时全员同图同线且就绪；⑥ 写操作（赶海领奖/残局首通）校验幂等键与轮次/位图记录。

#### 底座三：统一回合机制扩展库（与首领试炼、季度轮换共享）
- 不为地图单独造一套战斗框架，统一沉淀三项引擎能力到 `turn_battle_engine`（C++ 与 Java 同构）：
  1. **公开敌方意图预告与慢动作时序**（供常驻首领重劈、八仙岛「风眼引风」）；
  2. **战场周期环境光环**（供碧渊水宫「阳钟/阴鼓」、月汐湾「涨退潮」、核心循环轮换「物法易势」）；
  3. **预设人宠残局开局模式**（借用系统预设人宠与固定回合上限，供八仙岛「仙弈残局」、核心循环轮换「护卫救援」，局内临时状态完全隔离、零污染玩家真实血蓝与背包）。

#### 底座四：商业化与经济护栏（正循环而非互相消耗）
- **660 首套装备材料零泄漏**：赶海、残局、敲钟、抓宠均不产核心装备打造料/百炼石；明雷首领与常驻首领试炼共享同一套 10 份 PVE 基础奖励账本。
- **重铸砂投放严控上限**：若赶海/残局投放少量**重铸砂**（用于试配换词条，[equipment-recycling-trade-economy-v1.md](equipment-recycling-trade-economy-v1.md)），必须满足：① 残局为每题**终身一次**（位图记账，遵循 `AGENTS.md §13` 位图规范）；② 赶海产出的砂首发默认**仅自用（绑定）**且设低概率/账号或角色级周期上限，防止压垮旧装拆砂银两市场；也可首期先只发银两+灵兽口粮+风物印记，待旧装交易跑通后再决定是否投放砂。
- **放大付费外观与交易动机**：通过 Fill-First 聚人、灵潮聚集、同屏观战与主城摆摊，让玩家真正被别人看见，直接拉动已升级的**永久多人物收藏衣柜（3402 形象卡买断制）**与后续宝宝/招式外观的展示与消费意愿；完成三图风物印记可预留接入 `GrantAppearance` 发放 `card_unlockable=0` 专属纪念形象或称号。

### 2.5 双服并行与客户端边界（`AGENTS.md §12`）

- C++/Go 权威服与独立 Java 仓（`luyuan-java/xuanming-server-mmo`）共享同一套客户端可见 proto、配置表 schema 与 tip 错误码；两版各自按惯用结构实现 `WorldDirector`、`WorldPoi` 与回合机制库，并逐里程碑在 `PARITY.md` 对账。
- 客户端新增/扩展集中在：`CityTravelUiRoot`（地图热度、灵潮标识、直传推荐线）、`MysteryMerchantUiRoot`（跨图一键直传）、`WorldPoiPresenter`（统一渲染明雷/地标交互热区与灵潮轻氛围层）、大世界角色头顶“激战中”观战入口。UI 与美术交付仍坚持底板无字、文本分层与多分辨率安全区适配。

---

## 3. 思想分析

1. **为什么放弃“地图产核心材料”，转向“生态 + 潮汐 + 在场感”？**  
   核心装备材料是全游戏最敏感的经济命脉（既连着 660 免费上限，又连着聚宝斋可交易流通和直售提速）。把“玩家愿不愿意走这张地图”绑在核心材料上，必然陷入死循环：给少了玩家不愿走、给多了工作室狂刷并冲垮付费。把核心材料留在战斗里，让地图承载**宠物生态、策略变奏、限时奇遇、视觉与社交展示**，才符合“好玩优先、不肝、小小氪金”的第一原则。
2. **为什么选“读模型 + 统一 POI 点位”而不是在 Scene 节点上跑重型野外怪物实体？**  
   本仓库在神秘商人、分线目录、摆摊上已经验证了“控制面领导者写 Redis 读模型 → Scene 节点只读校验与转发 → 客户端负责形象与热区”是吞吐最高、锁最少、崩溃恢复最干净、C++/Java 双服最易对齐的范式。用统一 `WorldPoi` 承载明雷与地标交互，既能给玩家真实的“地图里看得见怪和宝箱、能点进去打、旁边人能观战”的体验，又不破坏万级在线高密度 Scene 节点的 CPU 预算。
3. **为什么必须把「压测均摊路由」和「玩家社交 Fill-First 路由」解耦？**  
   压测场景追求把海量机器人均匀打散到 16 条线测试极限吞吐；真实开服追求把 30 个活人死死聚在 1 线制造热闹感。两者目标天然相反，把 `WorldChannelPickPolicy` 做成显式配置项（生产默认 `fill_first`，压测显式 `least_loaded`），既治好开服冷清，又不破坏既有压测基线与自动化测试契约。

---

## 4. 漏洞分析

| 风险类别 | 具体隐患 | V2 硬约束与缓解措施 |
|---|---|---|
| **选线与扩缩容震荡** | 若 `SoftFillThreshold <= ScaleInPlayerThreshold`（如都接近 100），2 线刚收到人就被缩容排空，触发玩家被反复迁移 | 启动配置强校验：`SoftFillThreshold >= ScaleInPlayerThreshold * 2` 且 `SoftFillThreshold < ScaleOutPlayerThreshold`；默认 `ScaleIn=100 / SoftFill=200 / ScaleOut=2000`，留出充足滞回带 |
| **压测配置误带入开服** | `scene_manager_service.yaml` 当前给地图 1 配了 16 线，直接部署会让开服 16 线空城 | 拆离压测覆盖配置；开服部署模板显式置 `WorldChannelCount=1`、清空 `WorldChannelCountByConfId` 中 16 线覆盖，并在启动自检日志输出每图实际 `desired` 线数 |
| **场景 3（月汐湾）失配误开放** | 当前服务端 `BaseScene 3` 已切月汐湾导航，客户端仍 `clientOnly: true` 回退旧东海渔村，直接开放会落点越界/穿墙 | `WorldDirector` 开放白名单与神秘商人/灵潮候选表实行双端验收闸：场景 3 未完成客户端解除 `clientOnly` 与双端联机探针前，严禁写入灵潮候选与正式开放表 |
| **明雷抢怪与多开脚本刷赶海/残局** | 工作室批量账号蹲点抢怪，或脚本反复开赶海箱/刷残局洗可交易重铸砂 | ① 明雷全部**共享触点、各自独立开战**，永不先到先得；② 残局首通奖用**角色位图终身只发一次**（严禁复用表 ID，遵 `AGENTS.md §13`）；③ 赶海每轮限 3 次 + 绑定限制 + 客户端幂等键去重（复用 `client_request_ledger`），且不产任何可交易核心材料 |
| **跨图/跨线伪造 POI 与组队越权** | 修改客户端发包在主城触发水宫明雷或月汐湾赶海，或队员不在同图同线强开本图首领 | Scene 节点一律以服务端权威 `scene_config_id`、权威坐标距离、灵潮当前轮次与队伍实时成员同线状态裁决；不信任客户端传来的坐标、地图 ID 或灵潮状态 |
| **同屏观战信息泄漏与 PVE 回放膨胀** | 野外点击观战误露私密房/他人冷却，或给所有 PVE 录战报撑爆 `match` 数据库 | ① 观战直接走已验的观众脱敏帧与 `spectate_visibility` 闸门；② 试炼碑一期只存榜单元数据（前 3 名），二期公开回放按每图固定上限 3 份覆写，**绝不放开全量 PVE 录像** |
| **指标与线程红线** | 统计地图热度/POI 交互把 `player_id` 塞进 Prometheus label，或为灵潮新开 C++ 后台线程 | 严守 `AGENTS.md §9`：指标标签只允许 `{zone_id, scene_config_id, poi_type, outcome}` 有限枚举；C++ 侧全部挂主 EventLoop 定时器/异步回调，绝不新增线程 |
| **双服与配表漂移** | C++/Go 改了协议或 `WorldPoi` 表而 Java 仓未同步，导致双版行为分裂 | 按 `AGENTS.md §4 / §12`，所有客户端可见 `.proto`、新配置表 schema 与 `Tip.xlsx` 先定契约、用官方导表/生成脚本产出，同步客户端编译检查并在 Java 仓 `PARITY.md` 登记闭环状态 |

---

## 5. 步骤分析

按“**先治冷清与跨图引流 → 再铺地图生态与共享交互 → 再做深回合策略与残局 → 最后合入烟火气与长线轮换**”拆分为 4 个独立可验收的里程碑：

| 里程碑 | 范围与核心产出 | 依赖前置 | 阶段完成后的玩家可见变化 |
|---|---|---|---|
| **M1 · 聚人路由、地图收口与神秘商人跨图** | ① `Fill-First` 软上限选线 + 开服 1 线收口；② `WorldDirector` 灵潮轮换与 `WorldMapOverview`（地图热度 + 推荐线）；③ 修齐场景 3 月汐湾双端对齐；④ `MysteryMerchantSpot` 扩充至场景 2/5/3 并打通客户端跨图一键直传 | 现有分线、神秘商人、场景 5 碧渊水宫已就绪 | 开服所有玩家自然聚在同一条线；大地图能看各图热闹度与当期灵潮；整点神秘商人把全服活人吸往八仙岛/碧渊水宫/月汐湾 |
| **M2 · `WorldPoi` 统一交互点与三图生态** | ① 落地 `WorldPoi` 服务端点位表/校验与客户端渲染热区；② 22 宠按场景 2/5/3 拆分野外栖息捕捉池；③ 月汐湾「退潮赶海」每轮限 3 次盲盒事件；④ 碧渊水宫「钟鼓亭鸣钟」同线光效广播；⑤ 同屏激战中头顶标记与一键观战 | M1 完成；复用已有抓宠与 L0/L1/L2 观战 | 去不同地图真正能抓到不同系灵宠、赶海开惊喜、敲钟互动，路过还能围观别人在野外打架 |
| **M3 · 回合机制库、三图专属首领与仙弈残局** | ① 回合引擎补齐公开预告、周期环境光环、预设残局三项通用机制（与首领试炼共仓共码）；② 上线八仙岛「风眼引风」、碧渊水宫「水镜钟鸣」、月汐湾「涨退潮」明雷首领（共享 PVE 10 份基础）；③ 八仙岛「仙弈残局」石桌（终身位图首通）；④ 三图「试炼碑」最少回合榜单 | M2 完成；对齐 [boss-trial-planning-ui.md](boss-trial-planning-ui.md) 战斗机制契约 | 每张野外图都有截然不同的策略战斗手感与高手榜单，低人口也能靠试炼碑看到真实玩家记录 |
| **M4 · 离线烟火气与节庆限时轮换** | ① 接通主城[离线实体摆摊](../design/stall-system.md)（待寄售主干前置收口）；② 接入三图风物印记与永久衣柜 `GrantAppearance` 联动；③ 节庆/储备图单图限时开启与期满自动回流机制；④ 试炼碑二期受控公开榜首战报回放 | 摆摊前置（P-1~P-6）、衣柜批次二合入、节庆图双端导航验收 | 主城凌晨也有活人摊位烟火气，节日开放单张主题秘境不稀释常驻人口 |

---

## 6. 执行步骤与验收标准

### 6.1 M1 执行步骤（最高优先级，立竿见影治冷清）

1. **选线与开服配置收口**：
   - 在 `go/scene_manager` 配置新增 `WorldChannelPickPolicy`（`fill_first` / `least_loaded`）与 `SoftFillThreshold`（启动校验滞回不等式），调整 [scene_atomic.go](../../go/scene_manager/internal/logic/scene_atomic.go) 的 `luaReserveBestWorldChannel` 使其在 `fill_first` 下按 `channel_no` 升序优先填满软上限，超软上限后回落最空线；压测配置显式保持 `least_loaded`。
   - 隔离本地/压测的 `"1": 16` 配置，确认首测/开服环境所有主世界地图初始 `desired = 1`。
2. **灵潮调度与 `WorldMapOverview` 读模型**：
   - 在 `go/scene_manager` 领导者循环新增 `WorldTide` 轮换（每 60 分钟切 1 张已开放白名单野外图，持续 40 分钟）与 `WorldMapOverview` 快照写入 Redis `world_map_overview:{ctl:<zone>}`（汇总各图聚合热度档位、当期灵潮图、推荐线与神秘商人简要落点）。
   - 调整 [mystery_merchant.go](../../go/scene_manager/internal/logic/mystery_merchant.go) 的选线权重：存在当期灵潮图且有合法站位时，按可配权重优先落在该图 1 线。
   - 在 `proto/scene/player_scene.proto` 扩展地图总览查询响应（沿用既有消息追加字段原则，先改源 proto 再用 `dev_tools.ps1 -Command proto-gen-run` 生成并同步客户端 `gen_proto.ps1` / `gen_messageids.ps1` / `client_compile_check.ps1`）。
3. **地图双端对齐与商人扩点**：
   - 修复场景 3（月汐湾）客户端 `clientOnly: true` 与服务端 `yuexi-bay-20261115` 导航的同版联机验收（或先以 `1 + 2 + 5` 三图完成 M1 首验，再在修齐后把场景 3 写入开放白名单）。
   - 更新 `data/MysteryMerchantSpot.xlsx`，加入八仙岛与碧渊水宫（直接取 [navigation.json](../../data/scene_nav_bin/current_maps/biyuan-water-palace/biyuan-body-20261123/navigation.json) 已探针通过的 `teaPavilion`、`gongPavilion`、`centralCourt` 换轴 Z-up 坐标），走正式导表器产出产物。
   - 修改客户端 [CityTravelUiRoot.cs](../../../mmorpg-client/Assets/Scripts/UI/Ugui/Gameplay/CityTravelUiRoot.cs) 与 [MysteryMerchantUiRoot.cs](../../../mmorpg-client/Assets/Scripts/UI/Ugui/Gameplay/MysteryMerchantUiRoot.cs)：展示各图热度档位与灵潮角标，补齐跨图一键直传目标地图与推荐线（目标线失效自动回落）。
4. **Java 独立服同步与 M1 验收**：
   - Java 仓实现同语义的 `fill_first` 选线、`WorldMapOverview` 发布与跨图神秘商人站位表，更新 `PARITY.md`。
   - **M1 验收标准**：① 20 个测试账号依次登录与切入场景 2 / 5，100% 全部落在 1 线互相可见；超过 `SoftFillThreshold` 后第 N+1 人自然溢流到 2 线；② 既有压测/扩缩容单测在 `least_loaded` 与 `fill_first` 下均无震荡；③ 整点神秘商人落到碧渊水宫 1 线茶亭，身处主城的客户端点击“前往”一次直达并成功购买；④ 地图窗正确显示聚合热度档位与当期灵潮，无任何 `player_id` 进入指标标签。

### 6.2 M2 执行步骤（`WorldPoi` 与三图生态）

1. **定义 `WorldPoi` 配置表与协议**：
   - 新增 `WorldPoi.xlsx` 及其 schema（含 `id`、`scene_config_id`、`poi_type`、`x/y/z`、`interact_range`、`require_tide`、`param_id`、`display_name`），`id` 遵守永久不复用纪律。
   - 新增 `proto/scene/player_world_poi.proto`（列出当前地图可用 POI、交互请求带 `client_request_id` 幂等键、鸣钟等 AOI 轻视觉通知）；走正式生成链并同步客户端与 `MessageLimiter.xlsx`。
2. **三图灵宠栖息池拆分**：
   - 在 `Dungeon.xlsx` 与 `Monster.xlsx` 为场景 2 / 5 / 3 分别配置独立 `allow_pet_capture=1` 遇敌档，Scene 节点按玩家当前 `scene_config_id` 校验资格并建房。
3. **赶海盲盒、敲钟广播与同屏一键观战**：
   - 赶海轮次计数挂在玩家存档新位/轮次结构（单轮上限 3 次，同轮次同幂等键重试不重复发奖）；敲钟冷却按点位限频并向同线 AOI 广播一次轻特效消息。
   - 在大世界玩家外观/状态同步中增加公开 `watchable_battle_id`，战斗开始/结束由 Scene 更新并在客户端渲染头顶“激战中”点击入口，直接调现有 `WatchBattle`。
4. **M2 验收标准**：① 在主城伪造场景 5 POI 交互或超距点击全部拒绝；② 同一明雷 3 支队伍同时点击均各自顺利开战、零抢怪，旁边第 4 名玩家点击任一开战者头顶图标成功进入实时观战；③ 月汐湾非灵潮期拒绝赶海，灵潮期第 4 次点击明确提示本轮次数已满，断线重发同 `client_request_id` 不双倍发奖；④ C++ 改动交付前跑通 `test_cpp_no_raw_pointer_member.py` 且不新增线程。

### 6.3 M3 / M4 执行步骤（策略深化、异步烟火气与长线轮换）

1. **M3 回合机制库与首领/残局/试炼碑**：
   - 与 [boss-trial-planning-ui.md](boss-trial-planning-ui.md) 合并开发公开预告/慢动作、周期环境光环、预设人宠残局三大引擎能力（C++ 与 Java 同构），保证断线重连恢复同一回合意图与环境相位。
   - 接入三图明雷首领（校验同图同线与 PVE 基础 10 份同池不重复记账）、八仙岛仙弈残局（玩家存档位图记录每题首通，遵 `AGENTS.md §13` 位图红线）及三图试炼碑前 3 榜。
2. **M4 烟火气与节庆单图轮换**：
   - 待 [stall-system.md](../design/stall-system.md) 前置合入后上线主城离线实体摆摊；将三图风物印记接入 `GrantAppearance`；在 `WorldDirector` 增加节庆限时单图开关与到期优雅回城流程；在 [battle-report-replay.md](../design/battle-report-replay.md) 上补齐每图前 3 份受控公开 PVE 榜首战报回放。
3. **聚合运营验收口径**：
   - 仅统计聚合数据（不含玩家 ID 标签）：各开放地图到达占比、同图同线遇见其他真人比例、灵潮开启后 15 分钟内目标图活跃人数跃升比、神秘商人跨图成交率、三图抓宠与首领试炼选择分布、残局首通率、以及低谷时段（≤5 人在线）留存与回访率。

---

## 少量待拍板细项（不影响 M1 启动）

1. **新图 `八景岛`、`月汐湾` 的场景身份**：是原地替换旧场景 2 / 3（推荐，保持开服只有 1+3 张常驻图不稀释人口），还是独立新 ID 与旧图并存？
2. **微观「五行灵潮」节奏**：默认每 **60 分钟**轮换（持续 40 分钟，与现行整点神秘商人完美同步），还是每 **90 分钟**轮换？
3. **赶海与仙弈残局的轻奖励边界**：一期是只发**银两 + 灵兽口粮 + 风物印记/称号**（零碰装备体系，最稳妥），还是允许附带极少量**仅自用重铸砂**（鼓励玩家试词条，同时设位图/轮次硬上限）？

---

## 7. 大地图与灵潮/热度 UI 交接规格（交付 Codex 作图）

> 协作边界（对齐 `docs/rules/ai-workflow.md §10`）：
> 1. **Codex 只负责 UI 出图与无字透明切图，不参与 C# 编码、协议修改与编译**；切图交付后由本机工程侧接入 Unity uGUI。
> 2. **交接单只描述「做什么 UI」（界面、布局、组件、状态、文案、尺寸与切图清单），不在需求或提示词中额外指定画风形容词、配色/色号或装饰纹样**，避免干扰既有美术库的视觉一致性。

### 7.1 客户端字段契约与工程绑定接口（本机接入切图时使用）

#### 1) 协议字段（`proto/scene/player_scene.proto`，已双端生成）

- 请求：`SceneInfoRequest`（`MessageIds.SceneInfoC2S = 43`），按需置 `with_world_map_overview = 2`（由 `SceneChannelClient.RequestOverview()` 唯一管理，**严禁在别处重新 `RegisterNotify(31/43)`**）。
- 推送：`SceneInfoS2C`（`MessageIds.NotifySceneInfo = 31`），挂载 `WorldMapOverview world_map_overview = 3`：
  - `WorldMapOverview`：`zone_id`、`updated_at_ms`、`server_time_ms`、`repeated WorldMapOverviewEntry maps`、`WorldTideSnapshot tide`、`daily_hotspot_scene_config_id`。
  - `WorldTideSnapshot`：`active`、`round_id`、`scene_config_id`、`recommended_scene_id`、`recommended_channel_no`、`starts_at_ms`、`ends_at_ms`、`next_starts_at_ms`。
  - `WorldMapOverviewEntry`：`scene_config_id`、`total_player_count`、`channel_count`、`WorldMapHeatLevel heat_level`（`0=UNKNOWN / 1=QUIET 清幽 / 2=ACTIVE 有人 / 3=CROWDED 热闹`）、`recommended_scene_id`、`recommended_channel_no`、`is_tide_map`、`has_mystery_merchant`。

#### 2) 客户端纯 C# 视图模型与宿主接口（已实现并通过编译与单测，直接绑定）

| 类 / 文件 | 现成接口 | UI 用途 |
|---|---|---|
| `WorldMapOverviewModels`（`Assets/Scripts/Game/WorldTravel/WorldMapOverviewModels.cs`） | `EstimateServerNowMs(overview, elapsedSeconds)` | 结合 `CityTravelUiRoot.OverviewAgeSeconds` 本地每秒估算服务端时间，驱动倒计时走秒，**零额外发包**。 |
| `WorldMapOverviewModels` | `BuildTideBanner(overview, currentSceneConfigId, mapNameResolver, serverNowMs, canTravelToTideMap, busy)` → `WorldMapTideBannerView` | 直接吐出顶部灵潮横幅全部绑定字段：`Visible`（无数据时整条隐藏）、`Active`（涌动/休歇双态）、`Headline`、`CountdownText`（`剩余 MM:SS` / `下轮倒计时 MM:SS`）、`CanQuickTravel`、`ActionLabel`（`立即前往` / `已在此地` / `正在传送…`）。 |
| `WorldMapOverviewModels` | `BuildPinView(overview, sceneConfigId, currentSceneConfigId, onlineAvailable)` → `WorldMapPinView` | 直接吐出单张地图锚点的结构化状态：`HasOverviewEntry`、`HeatLevel`、`HeatLabel`（`热闹`/`有人`/`清幽`）、`TotalPlayerCount`、`ChannelCount`、`IsCurrentScene`、`IsTideMap`、`IsDailyHotspot`、`HasMysteryMerchant`、`RecommendedSceneId`、`RecommendedChannelNo`。 |
| `WorldMapOverviewModels` | `ShouldShowMapEntryTideBadge(overview, currentSceneConfigId, dismissedTideRoundId)` | 主界面 HUD「地图」入口角标判定（本轮灵潮进行中、玩家未在灵潮图、且本轮未打开过地图时为 `true`）。 |
| `WorldMapOverviewModels` | `FormatDestinationBadges(overview, sceneConfigId)` / `FormatDestinationHint(overview, sceneConfigId)` | 抽屉详情与预览区的兜底标签串与推荐落脚线提示。 |
| `SceneChannelClient`（`Assets/Scripts/Game/WorldTravel/SceneChannelClient.cs`） | `Overview`、`HasOverview`、`OverviewReceivedAt`、`OverviewAgeSeconds`、`RequestOverview(bool force = false)` | 总览缓存与按需拉取（内置 `MinListIntervalSeconds = 2.5f` 防限流节流，断线自动清零）。 |
| `CityTravelUiRoot`（`Assets/Scripts/UI/Ugui/Gameplay/CityTravelUiRoot.cs`） | `CurrentOverview`、`OverviewAgeSeconds`、`ShouldShowTideHudBadge`、`MarkCurrentTideSeen()`、`RequestRefreshOverview()`、`RequestQuickTravelToTide(out string error)`、`RequestTravelToScene(uint sceneConfigId, ulong sceneId, out string error)` | 地图窗宿主全部读写与一键发车入口；`Toggle()` 打开地图窗时已自动调用 `MarkCurrentTideSeen()` 与 `RequestOverview()`。 |
| `CityTravelUiRoot` | `FilterOnlineDestinations(all)` / `FilterGalleryDestinations(all)` | 将目的地拆成 **「游历」（联机正式开放图）** 与 **「览景」（40+ `clientOnly` 静态/节庆赏景图）** 两个分段页签，保留 `CreateDestinations()` 全量返回值不变以兼容既有单测。 |
| `MysteryMerchantWindow` / `MysteryMerchantUiRoot` | `MysteryMerchantShopView.TargetMapName`、`DescribeHeadline`、`DescribeEmpty` | 神秘商人跨图现身时自动显示 `正在 碧渊水宫 · 2线 · MM:SS 后离开` 与 `前往 碧渊水宫 · 2线` 按钮，点击复用 `CityTravelUiRoot.RequestTravelToScene` 一键直达；同图时 `TargetMapName == ""` 保持旧文案逐字不变。 |

### 7.2 界面功能结构、避让区与切图清单（仅功能与结构，不定风格）

#### 1) 现有大地图底图（2560×1080，左上原点）固定避让区
本次直接叠加在现有大地图底图（`world_skin.png`，2560×1080）上，以下既有区域不可遮挡：
- 顶部中央标题区：`x=930..1630, y=0..130`
- 右上关闭按钮：`x=2435, y=12, w=93, h=93`
- 左下「当前位置」牌：`x=60, y=970, w=420, h=76`（地名显示槽 `x=278, y=984, w=170, h=40`）
- 右下两颗既有入口按钮：`修行试炼` `(1895, 960, 305, 80)`、`云外秘境` `(2218, 960, 304, 80)`

#### 2) 本次新增 UI 模块与布局
- **顶部灵潮横幅**：位于顶部标题区正下方居中（建议区域 `x=820..1740, y=126..198`，约 920×72），分为「进行中」与「休歇中」两种状态；无数据时整条隐藏。
  - 进行中态内容：灵潮标识图标 + 标题文案（例：`灵潮涌动 · 碧渊水宫`）+ 倒计时（例：`剩余 34:50`）+ 右侧操作按钮（`立即前往` / `已在此地` / `正在传送…`）。
  - 休歇中态内容：状态文案（`灵潮休歇`）+ 下轮倒计时（例：`下轮倒计时 18:20`），不显示前往按钮。
- **底图可交互地点锚点**（只对应当前实际开放的大世界地图，严禁给底图上其他纯背景地名添加可传送标记）：
  - 地图 `1` 主城（归元城）：约 `(1265, 545)` — 展示地图名、热度标签、当前所在标记、（可选）神秘商人图标。
  - 地图 `2` 八仙岛：约 `(1375, 248)` — 展示地图名、热度标签、灵潮/今日旺地/神秘商人三个状态图标（按实际布尔状态独立显隐）。
  - 地图 `5` 碧渊水宫：约 `(925, 252)` — 同上；处于灵潮中时使用高亮/激活态锚点底座。
  - 地图 `3` 月汐湾（预留位）：约 `(1775, 245)` — 默认未开放，按 `CurrentMapCatalog.FindForOnline(3)?.CanTravelOnline` 动态决定显隐或不可点占位态。
- **底部收放抽屉（默认收起，点击左下「当前位置」或地图锚点展开）**：
  - 顶部增加双页签切换：**「游历」**（仅列已开放联机图，默认选中）与 **「览景」**（列仅预览的静态/节庆图）。
  - 展示选中地图名称 + 状态标签、地点简介、推荐落脚线提示（例：`推荐落脚 1线 · 灵潮余约 35 分`）、景色切换按钮、本区/跨区切换按钮、主行动按钮（`前往碧渊水宫` / `应用景色` / `已在此地` / `尚未开放传送`）及地图预览图区域（展开时避开顶部灵潮横幅）。
- **主界面 HUD「地图」按钮角标**：在主界面「地图」入口按钮右上角叠加一枚小提示角标，有进行中的灵潮且玩家不在该图时亮起。
- **神秘商人窗口跨图前往态**：商人在其他地图时，顶部状态栏显示 `正在 碧渊水宫 · 2线 · 12:34 后离开`，空货架区提示 `神秘商人正在 碧渊水宫 · 2线，前往该地图与线路才能看到货架。`，底栏主按钮为 `前往 碧渊水宫 · 2线`。

#### 3) 输出文件清单（整屏预览 + 无字透明切图）
- **整屏效果图（2560×1080 PNG，3 张）**：
  1. `preview_01_tide_active_2560x1080.png`：默认打开大地图态（顶部灵潮进行中横幅 + 底图 1/2/5 三个地点锚点及其热度/灵潮/今日旺地/神秘商人状态 + 底部抽屉收起）。
  2. `preview_02_drawer_travel_and_gallery_2560x1080.png`：灵潮休歇态 + 展开底部抽屉态（展示「游历 / 览景」双页签、地图预览、景色按钮、本区/跨区按钮与推荐落脚线提示）。
  3. `preview_03_merchant_cross_map_2560x1080.png`：神秘商人跨图现身态（标题、空货架说明与底部「前往 碧渊水宫 · 2线」按钮）。
- **无字透明 PNG 切图（真实 RGBA 透明底，严禁烘焙任何文字或数字）+ `CONTROL-USAGE.md`（记录每张图的像素尺寸、拉伸模式与安全区边距）**：
  1. `tide_banner_active.png`（约 920×72，横向三段拉伸）：顶部灵潮横幅底板（进行中态）。
  2. `tide_banner_resting.png`（约 920×72，横向三段拉伸）：顶部灵潮横幅底板（休歇态）。
  3. `tide_quick_travel_btn.png`（约 220×56，横向三段拉伸）：横幅右侧操作按钮底板。
  4. `map_pin_base_normal.png`、`map_pin_base_selected.png`、`map_pin_base_tide.png`（固定画布）：地图锚点底座（普通 / 选中 / 灵潮激活三态）。
  5. `heat_pill_crowded.png`、`heat_pill_active.png`、`heat_pill_quiet.png`（横向三段拉伸）：三档热度标签底板（对应「热闹 / 有人 / 清幽」，区分视觉主次层级）。
  6. `seal_tide.png`、`seal_hotspot.png`、`seal_merchant.png`（128×128 或 256×256，固定画布）：三枚无字状态小图标（分别代表「灵潮中」「今日旺地」「神秘商人」）。
  7. `drawer_tab_normal.png`、`drawer_tab_active.png`（横向三段拉伸）：底部抽屉「游历 / 览景」分段页签（未选中 / 选中两态）。
  8. `hud_tide_dot.png`（64×64，固定画布）：主界面 HUD「地图」按钮右上角灵潮提示小角标。

### 7.3 本机客户端 uGUI 接入验收与既有单测红线（切图收回后由代码侧执行）

1. **老服/无总览降级**：`CurrentOverview == null` 或 `Tide == null` 时，顶部灵潮横幅、锚点热度标签与三枚状态图标必须自动隐藏。
2. **保留既有测试控件名与初始行为**（不得破坏 `CityTravelWindowTests`、`MysteryMerchantWindowTests`、`WorldMapOverviewTests`）：
   - `CityTravelWindow` 保留：`CloseCityTravel`、`OpenCityTravelOptions`、`CloseCityTravelOptions`、`WorldMapTrials`、`WorldMapSecretRealms`、`TravelToSelectedCity`、`DayAppearance`、`FestivalAppearance`、`TravelZoneCycle`、`CityDestination_<SceneConfigId>`、`CityTravelCurrent`、`SelectedDestinationName`、`SelectedDestinationDescription`、`DestinationAppearanceHint`、`CityTravelStatus`、`SelectedMapPreview`、`Destinations`（`ScrollRect` + `RectMask2D`）。
   - `CityTravelWindow.Show(...)` 初次打开时 `WorldTravelOptions` 保持 `activeSelf == false`。
   - 新增控件统一命名：`WorldMapPin_<SceneConfigId>`、`WorldMapTideBanner`、`WorldMapTideQuickTravel`。
3. **接入后编译验收**：运行 `pwsh -NoProfile -File tools/client_compile_check.ps1` 与 EditMode `MmorpgClient.Tests.EditMode.Tianyong` 测试并确认 16:9 / 21:9 无裁切遮挡。

### 7.4 可直接复制给 Codex 的纯 UI 作图任务（只说做什么 UI，不定风格）

```text
请制作「大世界地图玩法（灵潮横幅 + 地点锚点与热度 + 游历/览景抽屉 + 神秘商人跨图态）」的 UI 效果图与无字透明切图，输出到 D:\work\image\designs\world-map-play-v2-ui\。

【基本要求】
- 本次只需做 UI 效果图与切图，不需要写代码或改工程文件。
- 直接基于现有 2560×1080 大地图底图（world_skin.png）和现有 UI 套系继续制作即可，不另起新风格。
- 所有切图控件、底板、按钮、标签、图标必须为真实 RGBA 透明底，严禁在切图里烘焙任何文字或数字（文字与数字由程序层动态渲染）。

【现有大地图底图（2560×1080，左上原点）固定避让区】
不重画底图本身，以下既有区域不要遮挡：
- 顶部中央标题区：x=930..1630, y=0..130
- 右上关闭按钮：x=2435, y=12, w=93, h=93
- 左下「当前位置」牌：x=60, y=970, w=420, h=76（地名文字区 x=278, y=984, w=170, h=40）
- 右下两颗按钮：「修行试炼」(1895, 960, 305, 80)、「云外秘境」(2218, 960, 304, 80)

【要做的 UI 模块与状态】
1. 顶部灵潮横幅（标题区正下方居中，约 x=820..1740, y=126..198，宽 920×高 72）：
   - 进行中态：左侧灵潮小图标 + 标题（例：灵潮涌动 · 碧渊水宫）+ 倒计时（例：剩余 34:50）+ 右侧操作按钮（文案：立即前往 / 已在此地）。
   - 休歇中态：标题（灵潮休歇）+ 倒计时（例：下轮倒计时 18:20），不显示右侧操作按钮。
2. 地图底图上的可点击地点锚点（仅放以下 3 个开放地点，不要给底图上其他背景地名加可传送标记）：
   - 主城（归元城）：约 (1265, 545) — 示意状态：「热闹」+ 当前所在标记
   - 八仙岛：约 (1375, 248) — 示意状态：「清幽」
   - 碧渊水宫：约 (925, 252) — 示意状态：「有人」+ 灵潮激活态底座 + 「灵潮中」「今日旺地」「神秘商人」三个独立状态小图标位
   - （月汐湾 约 (1775, 245) 预留位，暂不开放）
   - 每个锚点需清晰容纳：地名、三档热度标签（热闹 / 有人 / 清幽）以及最多 3 个状态小图标（灵潮、今日旺地、神秘商人），三档热度有明确主次区分。
3. 底部收放抽屉（默认收起，点击左下「当前位置」或地图锚点展开，展开时不遮挡顶部灵潮横幅）：
   - 顶部双页签：「游历」（联机开放地图，默认） / 「览景」（仅预览的静态与节庆地图）。
   - 抽屉内包含：选中地图标题与状态标签、地点列表横滑条、简介文案、动态提示行（例：推荐落脚 1线 · 灵潮余约 35 分）、景色切换按钮（日景 / 春节 / 元宵 / 中秋）、本区/跨区切换按钮、主行动按钮（例：前往碧渊水宫 / 应用景色 / 尚未开放传送）以及中上部地图实景预览区。
4. 主界面 HUD「地图」按钮右上角小提示角标（用于提示当前有可前往的灵潮）。
5. 神秘商人跨图前往态界面参考（基于现有神秘商人商店窗口）：
   - 顶栏提示：正在 碧渊水宫 · 2线 · 12:34 后离开
   - 中央空货架区提示：神秘商人正在 碧渊水宫 · 2线，前往该地图与线路才能看到货架。
   - 底栏主按钮：前往 碧渊水宫 · 2线

【输出文件清单】
1. 3 张 2560×1080 整屏定版预览 PNG：
   - preview_01_tide_active_2560x1080.png（默认打开大地图：顶部灵潮进行中 + 1/2/5 三个锚点及状态 + 底部抽屉收起）
   - preview_02_drawer_travel_and_gallery_2560x1080.png（灵潮休歇态 + 底部抽屉展开展示「游历/览景」双页签与详情/预览区）
   - preview_03_merchant_cross_map_2560x1080.png（神秘商人跨图现身与一键前往态）
2. ui\ 目录下的无字透明 RGBA PNG 切图 + ui\CONTROL-USAGE.md（写明每张切图的画布尺寸、拉伸模式 horizontal_only_verified / fixed_size_only、Sprite Border 边距与文字安全区）：
   - tide_banner_active.png（约 920×72，横向三段拉伸）：灵潮横幅底板（进行中态）
   - tide_banner_resting.png（约 920×72，横向三段拉伸）：灵潮横幅底板（休歇态）
   - tide_quick_travel_btn.png（约 220×56，横向三段拉伸）：横幅右侧操作按钮底板
   - map_pin_base_normal.png / map_pin_base_selected.png / map_pin_base_tide.png（固定画布）：锚点底座（普通 / 选中 / 灵潮激活）
   - heat_pill_crowded.png / heat_pill_active.png / heat_pill_quiet.png（横向三段拉伸）：三档热度标签底板（热闹 / 有人 / 清幽）
   - seal_tide.png / seal_hotspot.png / seal_merchant.png（128×128 或 256×256，固定画布）：三个无字状态小图标（灵潮 / 今日旺地 / 神秘商人）
   - drawer_tab_normal.png / drawer_tab_active.png（横向三段拉伸）：抽屉「游历 / 览景」页签（未选中 / 选中）
   - hud_tide_dot.png（64×64，固定画布）：HUD 地图按钮右上角提示角标
```

---

## 8. 已落地闭环记录（M1 + M1.5 月汐湾开放 + M2-Lite 三图野外灵宠栖息地）

- **M1（大地图总览 + 双层灵潮 + Fill-First 软上限选线 + 神秘商人跨图一键前往 + 15 张无字切图接入）**：已全端闭环并通过测试；主界面 HUD 补齐了首次入场自动拉总览与关闭大地图期间每 60s 轻量刷新，顶部灵潮横幅标题与当前标记文字安全区已与 `CONTROL-USAGE.md` 对齐。
- **M1.5（月汐湾 Scene 3 纳入开放地图与双层灵潮）**：Go `scene_manager` 与 Java `game-scene-manager` 默认开放大世界地图更新为 `[1, 2, 3, 5]`、灵潮与今日旺地轮转池更新为 `[2, 3, 5]`；`MysteryMerchantSpot.xlsx` 追加月汐湾 3 处经导航网格连通域校验的商人刷新点（`id=13..15`）。
- **M2-Lite（三图野外灵宠栖息地生态捕宠闭环，零新协议/零新表结构）**：
  - 数据表扩展（`tools/scripts/world_map_habitat_xlsx_patch.py`）：`Monster.xlsx`（`201..209`）、`Dungeon.xlsx`（`202` 八景岛·云鹤10级/竹风狸/霞角鹿、`203` 月汐湾·金猊10级/灵狐/符小虎、`205` 碧渊水宫·玄潮龟/汐螺/露华灵，`allow_pet_capture=1, max_team_size=1, scene_id=2/3/5`）、`PlayMode.xlsx`（`402/403/405` `PVE_SOLO`）。
  - 双端服务端场景权威防伪校验：C++ `PlayerBattleSystem::PrepareBattle` 与 Java `PlayerBattleService.prepare` 均在 `dungeon.allow_pet_capture == 1 && dungeon.scene_id > 1` 时强制核验玩家当前所在场景 `scene_config_id == dungeon.scene_id`（不符回 `kFeatureUnavailable` 且零写锁；主城捕宠演练 `Dungeon 4, scene_id=1` 不受限）。
  - Unity 客户端接入：`WorldMapOverviewModels` 栖息地元数据与标题/摘要格式化、`BattleMatchEntryClient.StartWildPetHabitat`（组队态先于 `CanStart` 拦截并明确提示仅限单人出发）、`CityTravelWindow` 抽屉可捕灵宠生态说明、`CityTravelUiRoot` 野外底部居中「寻宠遇敌」HUD 浮条（`WildPetHabitatBar`）。
  - 专属 HUD 浮条切图（Azure `image/main@a9c6026d`，详见 [world-map-habitat-ui-handoff.md](ui-handoff/world-map-habitat-ui-handoff.md)）：`wild_habitat_bar.png`（`800×76`，Border `32/32/18/18`）、`wild_habitat_btn.png`（`190×50`，Border `24/24/12/12`，按钮文字安全区 `31/10/128/30`）、`wild_habitat_icon.png`（`128×128`，Simple）已逐字节 SHA-256 核验落盘至 `mmorpg-client/Assets/Resources/UI/Ugui/WorldMapV2/` 并配齐无冲突 `.meta`。
