# 《五行奇谈》全面反「上班打卡」体验整治与经济平滑化落地设计（V1）

> 日期：2026-11-26  
> 状态：**负责人 2026-11-26 授权全量落地（不怕改动），同步覆盖权威服 C++/Go、Java 独立服、Python 服务端、Unity 客户端与源配表**  
> 上位红线：[项目设定](project-vision.md)（红线第 2 条：真不肝，严禁设计成「像上班」的玩法）· 总仓红线：严禁导致游戏不过审或被迫下架  
> 关联文档：[赛季与材料经济统一 V1](season-pvp-pve-economy-unified-v1.md) · [角色升级与战斗银两 V1](player-leveling-and-battle-economy-v1.md) · [帮会玩法 V2](guild-play-v2.md) · [激励视频合规与双银两 V1](rewarded-ad-compliance-and-economy-v1.md) · [群仙问道赛 V2](season-team-tournament-v2.md) · [离线摆摊设计](../design/stall-system.md)

---

## 一、需求分析

### 1.1 背景与审计发现
2026-11-26 对全仓全部现役策划文档、技术设计与已导出的实表（`generated/tables/*.json`）执行「禁止像上班」红线专项审查，共检出 **8 项** 实质或潜在的日常打卡 / 催肝 / 定点闹钟问题：

| 编号 | 模块与现状问题 | 严重度 | 处置状态 |
|---|---|---|---|
| **A0** | 群仙问道赛 V2 冠军雕像「每日免费观道领礼」（UTC+8 00:00 每日点雕像领 5,000 绑银 + 3 口粮，收益倒挂正常游玩） | 高 | **已完成前置移除**（同日四仓已彻底下线 `SaluteTournamentChampion` 506，仅留纯荣誉玉座/牌匾展示） |
| **A1** | **帮会捐献 `GuildDonate.xlsx`**：仍保留每日 8 次捐献（5×1 万银两 + 2×10 万银两 + 1×100 灵石）与每日 05:00 重置 | 高 | **本次整治**：移除灵石捐献与 10 万大捐，保留单一轻量自愿银两档并切换为**每周重置（每周上限 3 次）** |
| **A2** | **帮会活动 `GuildActivity.xlsx`**：旧二期 3 项日常（`1 元宵灯会` 日 1、`2 中秋团圆` 同时在线日 1、`3 同道历练` 2–5 人强组队日 2）均配 `enabled=true, 0/0` 常开 | 高 | **本次整治**：3 行默认全部 **`enabled=false`**，彻底取消常开日常与强制同时在线/强组队日常，仅保留节庆限时配置能力 |
| **A3** | **帮会商店 `GuildShop.xlsx`**：5 项商品仍配 `limit_period=1`（每日限购）；`305 角色形象卡` 误配 `limit_period=2`（每周 1 次、3600 帮贡）；多项硬货堆叠周清需 8,550+ 帮贡/周 | 高 | **本次整治**：新增 **`limit_period=3`（终身限购）**；清零全部每日限购（改为 `0` 不限购）；`202/204/304/305/306` 珍稀/外观项改**终身限购**；其余实用周耗材上限削减 >80% |
| **A4** | **帮会升级资金 `GuildLevel.xlsx`**：1→10 级累计需 `4,050,000` 资金，迫使帮主天天催捐献 | 中高 | **本次整治**：1→10 级平滑下调至 **`1,007,000`**（1→6 级仅 `162,000`），休闲中小帮自然参战即可升级 |
| **A5** | **帮战 4 档战功宝箱 `GuildWarMilestone.xlsx` & `GuildWarRule.xlsx`**：周 6,000 帮会战功过高且个人门槛 30 偏高，易诱发帮主周日晚催打卡 | 中 | **本次整治**：4 档门槛由 `300/1200/3000/6000` 降为 **`200/600/1200/2000`**，个人参与门槛 `dividend_min_personal_merit` 由 `30` 降为 **`15`**（打 1 场即可领奖），维持周上限防大帮无限刷 `3407` |
| **A6** | **核心双周上限（二阶段装备材料周 150、战斗银两周 30k/45k）**：周一 05:00 一刀切清零，请假/出差 1–3 周永久亏额度 | 中高 | **本次整治**：升级为**「滚存蓄水池（Rollover Cap）」**——材料每周一 +150、最多滚存 **4 周（600）**；战斗银两每周一 +30k 软/+45k 硬、最多滚存 **3 周（90k/135k）** |
| **A7** | **激励视频配额 `RewardedAdSlot.xlsx`**：当前日限 2 次（每日 05:00 重置），未来开启后易变每日看广告打卡 | 中 | **本次整治**：严守 **`enabled=0`** 与 **`currency_type=3 (绑定银两)`** 红线不变，配额改为**每周 14 次（UTC+8 周一 05:00 重置）**，总上限等价且零日清压力 |
| **A8** | **群仙问道赛淘汰赛轮次窗（30 分钟）与离线摆摊默认时长（12 小时）**：偏短，易成定时闹钟 | 中低 | **本次整治**：`TournamentRule.knockout_round_timeout_seconds` 放宽为 **`14400`（4 小时/轮）**；`stall-system.md` 默认离线摆摊上调为 **`48 小时`** |

### 1.2 上位红线准入核对
1. **普通零氪玩家（反「上班」一票否决）**：全系统零每日签到、零每日捐献、零每日限购、零强制同时在线、零强制每日组队；离开 1–3 周回归，材料与银两蓄水池完整保留，绝不产生“漏领血亏”焦虑。**通过**。
2. **应用商店/小游戏零拒审红线**：`RewardedAdSlot.enabled` 维持 `0`、`currency_type` 维持 `3`（物理不可交易绑定银两），客户端文案严格中立零诱导词；不新增任何支付/外链/捐赠入口。**通过**。
3. **经济物价与防脚本**：滚存蓄水池仅累积**自然周增量**且封顶 3–4 周，新号初始池仍为单周额度（材料 150 / 银两 30k/45k），年度防脚本总上限与原周上限完全一致（零通胀）；`305 角色形象卡` 改为终身限购 1 次，反而堵住原每周白嫖 ¥10 形象卡的商业化漏洞。**通过**。

---

## 二、设计分析

### 2.1 二阶段装备材料 4 周滚存蓄水池（Rollover Pool）
- **核心常量**：`kWeeklyLimit = 150`（每周一 05:00 补充量），`kWeeklyRolloverMaxWeeks = 4`，`kWeeklyRolloverMaxPool = 600`（封顶）。
- **存档模型（仅追加字段，零破坏兼容）**：
  - `EquipmentMaterialRewardLedger` 追加 `uint32 stage2_pool_cap = 12;`（语义：当前 `week_starts_at_ms` 生效的可赚取池上限；旧档 `0` 等价于 `150`）。
- **确定性推进算法**（在 `PrepareBattleCredit` 与 `FillSnapshot` 共用纯函数 `ResolveStage2WeekState`）：
  1. 若 `previous.week_starts_at_ms() == 0`：首次进入二阶段，生效上限 `cap = 150`，`earned = 0`（防新入二阶段账号首日爆发 600）。
  2. 若 `weekStart < previous.week_starts_at_ms()`（时钟回拨）：不重置、不增池，维持既有 `prevCap / prevEarned`，且 `PrepareBattleCredit` 拒绝入账（抗回拨）。
  3. 若 `weekStart == previous.week_starts_at_ms()`：同周维持 `cap = effectivePrevCap`，`earned = previous.earned_this_week()`。
  4. 若 `weekStart > previous.week_starts_at_ms()`：
     - `weeksElapsed = min(4, (weekStart - previous.week_starts_at_ms()) / kWeekMs)`（至少为 1）；
     - `carry = prevCap > prevEarned ? (prevCap - prevEarned) : 0`；
     - `newCap = min(600, carry + weeksElapsed * 150)`；`newEarned = 0`。
- **客户端快照兼容**：`EquipmentMaterialRewardSnapshot.weekly_limit` 直接下发当前生效 `cap`（`150..600`），`earned_this_week` 下发本周期已用（`0..cap`），零新增 RPC 字段即可在现有界面直观展示滚存余额。

### 2.2 战斗银两 3 周滚存蓄水池（Rollover Pool）
- **核心常量**：`kSilverRolloverMaxWeeks = 3`（最多滚存 3 周：`90,000` 全额软上限 / `135,000` 硬封顶）。
- **存档模型（仅追加字段，零破坏兼容）**：
  - `BattleEconomyLedger` 追加 `uint64 silver_rollover_soft_cap = 4;`、`uint64 silver_rollover_hard_cap = 5;`（旧档 `0` 等价于单周 `rule.softCap / rule.hardCap`）。
- **确定性推进算法**（在 `battle_progression_rules.h` / `BattleProgressionRules.java` / `battle_settlement.py` 同源实现）：
  1. `current.week_starts_at_ms() == 0`：新号首周初始化 `soft = rule.softCap`，`hard = rule.hardCap`，`used = 0`。
  2. `currentWeekStartMs < current.week_starts_at_ms()`（时钟回拨）：不重置，沿用当前已用与上限。
  3. `currentWeekStartMs > current.week_starts_at_ms()`：
     - `weeksElapsed = min(3, (currentWeekStartMs - prevWeekStart) / kWeekMs)`（至少为 1）；
     - `carrySoft = prevSoft > prevUsed ? (prevSoft - prevUsed) : 0`；
     - `carryHard = prevHard > prevUsed ? (prevHard - prevUsed) : 0`；
     - `newSoft = min(3 * rule.softCap, carrySoft + weeksElapsed * rule.softCap)`；
     - `newHard = max(newSoft, min(3 * rule.hardCap, carryHard + weeksElapsed * rule.hardCap))`；
     - `used = 0`，`battle_exp_this_week = 0`。
  4. 结算明细 `weeklySilverSoftCap` 上报当前生效的 `effectiveSoft`（`30,000..90,000`），玩家直观看到回归滚存额度。

### 2.3 帮会商店终身限购 `PeriodLifetime = 3` 与帮会四表减负
- **`gameday` 周期键扩充（零 DB 迁移）**：
  - `PeriodNone = 0` → `periodKey = 0`（不占计数行）；
  - `PeriodDaily = 1` → `DayKey`（8 位 `YYYYMMDD >= 19700101`，代码保底兼容，实表零使用）；
  - `PeriodWeekly = 2` → `WeekKey`（6 位 `YYYYWW >= 100000`）；
  - `PeriodLifetime = 3` → **`LifetimePeriodKey = 1`**（固定常量 `1`，严格小于 `weekKeyFloor = 100000` 与 `dayKeyFloor = 19700101`，绝不被 `counterCleanupCutoffs` 误清理；`ShopUsage` 查询改为 `period_key IN (dayKey, weekKey, 1)`，完美复用既有幂等占额与拒绝退回事务）。
- **帮会四表目标数值**：
  - `GuildDonate.xlsx`：仅保留 `id=1 随缘银两添香`（`currency_type=0, cost=10000, contrib=20, funds=2000, daily_limit=3` 对应**每周 3 次**），移除 `id=2`、`id=3`。服务端 `DonateToGuild` / `GetGuildDonateOptions` 统一改用 `WeekKey` 与 `NextWeeklyReset`。
  - `GuildActivity.xlsx`：`1/2/3` 全部设 `enabled=false`。
  - `GuildLevel.xlsx`：升级资金 `5000 / 12000 / 25000 / 45000 / 75000 / 115000 / 170000 / 240000 / 320000 / 0`。
  - `GuildShop.xlsx`：全部 5 项日限归零为不限购；`202/204/304/305/306` 改 `limit_period=3`（终身限购 `1/1/2/1/1`）；`104/105/205/206` 保持温和周限 `3/5/6/1`。
  - `GuildWarMilestone.xlsx` & `GuildWarRule.xlsx`：4 档帮会战功 `200 / 600 / 1200 / 2000`；`dividend_min_personal_merit = 15`。

### 2.4 激励视频周池化与锦标赛淘汰赛宽窗
- **激励视频周池（零拒审红线不动）**：
  - `RewardedAdSlot.xlsx`：`enabled` 保持 `0`、`currency_type` 保持 `3`、`reward_amount` 保持 `300`，`daily_limit` 由 `2` 调为 `14`（每周上限 14 次，总上限与原 `2×7` 严格一致）。
  - `rewarded_ad_rules.h` / `RewardedAdRules.java` / `scene_rewarded_ad.py` 周期切点改为 **UTC+8 每周一 05:00**（`ResolveGameWeek`），抗时钟回拨。
  - `RewardedAdModel.cs` 与 `Tip.xlsx`（`RewardedAdDailyLimitReached`）文案更新为「本周剩余 / 下周一 05:00 重置」，零诱导/违禁词。
- **群仙问道赛淘汰赛宽窗**：`TournamentRule.xlsx` 中 `knockout_round_timeout_seconds` 由 `1800` 改为 `14400`（4 小时/轮，双方就绪即刻开打，神机竞猜同步获得 4 小时宽窗）。

---

## 三、思想分析

1. **蓄水池优于「日清/周清」**：用「增量注入 + 封顶蓄水池」替代「到期清空」，在**不增加任何单周/年度脚本产出上限**的前提下，把“定时打卡焦虑”彻底转化为“有空随时兑现的弹性额度”。
2. **终身限购替代高额周限购**：外观、法宝拓印、稀有秘墨属于阶段性成长/收藏目标，做成终身限购后玩家按自己节奏攒帮贡兑换即可，彻底消除“本周差 2,000 帮贡买不到形象卡血亏”的心理绑架。
3. **收益锚定核心游玩，绝不倒挂**：随缘捐献每周上限 60 帮贡/6,000 资金，低于正常打 1–2 场帮战；帮会升级资金与战功宝箱门槛以 10–20 人休闲帮会自然产出为基准，让帮会回归“一起玩、聊得来”而非“交任务上班群”。
4. **最小改动换最大闭环**：全部改动复用既有表列、既有 RPC 消息号与既有 UGUI 面板，不新增线程、不新造数据库表、不新增美术切片，保证四仓快速对齐且回归风险最小。

---

## 四、漏洞分析

| 潜在漏洞 / 边界风险 | 防护与规避方案 |
|---|---|
| **1. 新建小号立即获得 4 周材料 / 3 周银两滚存爆发** | `week_starts_at_ms == 0` 首次入账时强制按**单周额度**初始化（材料 150、银两 30k/45k），仅对已活跃过的老号跨周滚存，杜绝脚本批量建新号首日 4 倍爆发。 |
| **2. 服务器时钟回拨导致反复刷周上限或滚存翻倍** | 全链路坚持严格单调比较：`weekStart < prevWeekStart` 一律不重置、不增池；材料二阶段回拨期直接拒绝入账；`weeksElapsed` 上限钳制在 `4` / `3` 杜绝整数乘法溢出。 |
| **3. 历史计数清理误删 `PeriodLifetime = 3` 终身限购记录** | `LifetimePeriodKey = 1` 严格小于 `weekKeyFloor (100000)` 和 `dayKeyFloor (19700101)`，清理 SQL 的两段 `BETWEEN` 物理永不命中 `1`，并通过单测锁定。 |
| **4. 帮会商店商品改为终身限购后退款漏退或双退** | `ReserveShopOrder` 与 `Finalize` 的 `refundCounter` 均透传 `op.PeriodKey (= 1)`，背包满永久拒绝时精准回退 `period_key = 1` 计数行，与周限购完全同构。 |
| **5. 激励视频调为周 14 次误碰首审红线或绕过冷却** | `RewardedAdSlot.enabled` 仍锁死 `0`，`currency_type` 锁死 `3`，`cooldown_sec = 60` 与 `min_watch_sec = 15` 保持不变，且运行 Unity `RewardedAdModelAndComplianceTests` 强制守卫。 |
| **6. 跨仓持久化 Schema 与配表漂移** | C++ 源 proto 追加字段号后通过 `dev_tools.ps1` 全量生成；Java 经 `ContractSync` 同步并追加 `game-player-store` `0023.schema` 通过 `PlayerStateSchemaLedgerTest`；Python 同步表与 proto 并重编 `schema.pb`；Unity 跑 `gen.bat client`。 |
| **7. 并行会话未提交改动被二进制 `.xlsx` 覆写** | 所有 `.xlsx` 修改统一通过幂等 Python `openpyxl` 脚本**从磁盘实时加载、只改目标行/单元格**，绝不整表覆盖，完整保留并行会话的未提交改动（含 `TournamentRule.xlsx`、`Tip.xlsx`）。 |

---

## 五、步骤分析

1. **文档落盘与索引同步**：写入本设计文档，在 5 份关联策划/设计文档顶部补充 2026-11-26 覆盖口径，`stall-system.md` 默认时长调为 48h，运行 `gen_docs_index.py`。
2. **权威源表与协议更新（`xuanming-server-mmo-cpp`）**：
   - 编写并执行幂等 `tools/scripts/anti_chore_v1_xlsx_patch.py`，精准更新 `GuildDonate.xlsx`、`GuildActivity.xlsx`、`GuildLevel.xlsx`、`GuildShop.xlsx`、`GuildWarMilestone.xlsx`、`GuildWarRule.xlsx`、`RewardedAdSlot.xlsx`、`TournamentRule.xlsx` 与 `data/tip/Tip.xlsx`（仅 2 条文案）；
   - 更新 `client_request_ledger_comp.proto`（追加 3 个滚存池字段）与表 schema 注释；
   - 运行导表与 `proto-gen-run` + `gen_schema_index.py`。
3. **C++ / Go 权威服务端实现与单测**：
   - C++：`player_equipment_material_reward.{h,cpp}`、`client_request_ledger.cpp`、`battle_progression_rules.h`、`player_battle.cpp`、`rewarded_ad_rules.h`、`player_rewarded_ad.cpp` 与 `bag_test` 单测；
   - Go：`go/shared/gameday`、`go/guild`（捐献周化、商店终身限购 `3`、校验与单测）、`go/match`（淘汰赛默认回退常量）。
4. **Java 独立服务端对齐（`xuanming-server-mmo`）**：
   - `ContractSync` 同步契约与表、`player_state.proto` 追加滚存字段与 `0023.schema`；
   - 更新 `EquipmentCraftService`、`PlayerClientRequestLedger`、`BattleProgressionRules`、`BattleSettlementService`、`RewardedAdRules`、`RewardedAdService`、`GameDay`、`EconomyTables`、`GuildEconomyService`、`JdbcEconomyStore`、`PARITY.md` 与对应单测。
5. **Python 服务端对齐（`xuanming-server-mmo-python`）**：
   - 同步变更的 `.proto` 与导出表，重跑 `tools/build_protocol.py`；
   - 更新 `scene_assets.py`、`battle_settlement.py`、`scene_rewarded_ad.py`、`guild/{config,service,store,sql_store}.py`、`match/tournament.py` 与对应 pytest 单测。
6. **Unity 客户端对齐（`mmorpg-client`）**：
   - 同步表与协议；
   - 更新 `EquipmentWindow.Materials.cs`、`CoreSettlementRewardSection.cs`、`PlayerProgressionModels.cs`、`GuildWindow.cs`、`RewardedAdModel.cs` 与对应 EditMode 单测。
7. **全仓编译、静态门禁与单测验收**：运行 C++ 静态门禁 + MSBuild + `bag_test` + `scene --validate-tables`、Go `go test ./...`、Java Maven 测试、Python `pytest`、Unity `client_compile_check.ps1` + EditMode 测试，并追加真实记录至 `docs/PROGRESS.md`。

---

## 六、执行步骤

- [x] Step 1：完成全仓工作区状态与既有代码深度审计（确认 A0 每日观道已由前置会话彻底清除）。
- [x] Step 2：落盘本设计文档并同步关联策划/设计文档与索引。
- [x] Step 3：编写并执行 `tools/scripts/anti_chore_v1_xlsx_patch.py`，更新源 proto 与全量导表/生成。
- [x] Step 4：落地 C++ 滚存蓄水池/激励视频周池与 Go 帮会周捐献/终身限购，并通过 C++/Go 单测与静态门禁。
- [x] Step 5：同步对齐 Java 独立服（含 `0024.schema` 与 `PARITY.md`）并通过 Maven 测试。
- [x] Step 6：同步对齐 Python 服务端并通过 `pytest`。
- [x] Step 7：同步对齐 Unity 客户端文案/模型并通过编译与 EditMode 验收测试。
- [x] Step 8：将真实命令与测试结果追加至 `docs/PROGRESS.md`。
