# 四职业技能(class skills)服务端设计

更新：2026-10-23 当前数值基线。四职业技能结构与既有协议已落地，本批仅调整 `Skill.xlsx` 数值，未改协议。
状态：当前配表已正式导出并通过独立数值设计镜像；协议库与战斗测试工程已重建，完整战斗回归 282/282 通过，联机尚未验收。
2026-10-19 首次实现的历史交接见 [class-skills-handoff-20261019.md](../handoff/class-skills-handoff-20261019.md)，其中首次生成待办不能当成当前状态。
需求来源:客户端仓 `Docs/SKILL_LOGIC_HANDOFF.md`(2026-10-09),本文按其 §4 A/B/C/D 实施;§6 宠物技能与截图机制不在本批。

## 1. 结论先说

- 玩家技能书里的 32 个职业技能(ID 1101–1408)进了服务端权威表(`data/Skill.xlsx`),可查询(ListSkills 回完整技能书)、
  可学习 / 修习(新 RPC `UpgradeSkill`,消息号 317)、进战斗能施放(快照带等级,回合引擎按职业技能规则执行)。
- 旧的"每个号都塞 1 / 2 / 13 号占位技能"修掉了:`RegisterPlayer` 只按**当前职业**的 `Class.skill` 发初始技能
  (职业 1~4 = 本职业首个主动 + 首个被动,1 重)。存量号在加载时自动补初始技能;旧占位技能留档但不再生效。
- 被动加成只有一个消费者:六项二级属性与暴击由 scene 属性重算吃进去,战斗用快照里的结果,不会重复叠加;
  治疗加成是战斗专用被动,只由引擎消费。

## 2. 内容基线与配表

技能身份、名称、图标与原始内容来自 `E:/work/image/designs/class-skills-20260928/catalog.json`，首次实施时与客户端资源目录对应。
2026-10-23 起当前倍率、治疗与耗蓝以 `data/Skill.xlsx` 及正式导出表为准，不再声称与旧素材目录的数值逐项一致。
职责参考仅取[问道官方属性说明](https://wd.gyyx.cn/GameHelp/GameHelpDetail.aspx?CurrentParentId=119&GameHelpTypeID=122&ParentId=10&level=2)
（早期版本页面）与[幻唐志端游官方门派定位](https://htz.duoyi.com/)的输出、治疗、速度分工，本作数值不冒充原作精确公式。

| 表 | 改动 |
|---|---|
| `data/schema/skill_table.proto` | 追加 26..46:`class_id` `name` `description` `icon_key` `category` `unlock_level` `max_rank` `target_team` `max_targets` `mana_percent` `cooldown_rounds` `heal_multiplier` `heal_per_level` `execute_heal_threshold` `execute_heal_bonus` `restore_mana_percent` `dispel_count` `initiative` `passive_stat` `passive_percent` `buff_percent`。`class_id = 0` 的老行不读这些列,行为不变。 |
| `data/Skill.xlsx` | 追加 32 行。老列口径:主动 `skill_type = [1]`(普通施放),被动不填 `skill_type`(被动位号 0 会被导表器当空格丢掉,被动改由 `passive_stat` 非空判定);敌 / 友单体 `targeting_mode = [1]`;伤害技 `damage = "10"`;耗蓝不走 `cost_resource`(全 0),走 `mana_percent`;`cooldown_id = 0`(走 `cooldown_rounds`)。 |
| `data/Buff.xlsx` | 追加 11103 / 11104 / 11203 / 11304 / 11403 / 11404:`buff_type` 标"改哪一项"(10 物伤、11 防御、13 速度),`no_caster = 1`、`max_layer = 1`、`duration = 12`(2 回合)、tag `ClassSkillBuff`(减益另带 `Debuff`)。**幅度不在 Buff 表**,在 `Skill.buff_percent`,施加时按技能等级算定写进局内 buff。 |
| `data/Class.xlsx` | 职业 1~4 的 `skill` 种子改为 `[x01, x06]`;职业 5~9 不动(仍是旧通用技能 1)。 |
| `data/tip/Tip.xlsx` | `skill_error` 段追加 7007..7013:`SkillNotInClass` `SkillRankChanged` `SkillUpgradeCountInvalid` `SkillLevelNotEnough` `SkillMaxRankReached` `SkillGoldNotEnough` `SkillUpgradeBusy`。 |

战斗配表指纹(`battle_table_fingerprint.cpp`)整表序列化 Skill / Buff,新列自动进指纹,scene / battle 两端表版本不一致仍会被开局拒绝,无需改代码。

### 2.1 当前伤害、治疗与耗蓝（2026-10-23）

三种常用三目标群攻收紧每目标倍率，保留相对于单体技的总目标数收益。伤害基数仍为 `damage = "10"`，倍率作用于对应物伤或法伤；
其后照常计算减伤、PVP 系数与技能重数加成。

| 技能 | 每目标攻击倍率 | 最多目标 | 最大法力消耗 | 冷却回合 |
|---|---:|---:|---:|---:|
| 1102 横戈裂岳 | 物伤 ×0.70 | 3 | 6% | 1 |
| 1202 紫霄雷阵 | 法伤 ×0.75 | 3 | 6% | 1 |
| 1402 回岚掠阵 | 物伤 ×0.60 | 3 | 6% | 1 |

丹心治疗以「法伤系数 + 每角色等级固定值」标定；表中为重数、治疗被动及急救阈值加成前的基础量。

| 技能 | 每目标基础治疗 | 最多目标 | 最大法力消耗 | 冷却回合 |
|---|---|---:|---:|---:|
| 1301 丹华续脉 | `法伤 ×0.30 + 等级 ×2` | 1 | 4% | 0 |
| 1302 青莲润生 | `法伤 ×0.18 + 等级 ×1` | 3 | 7% | 1 |
| 1303 玉露涤尘 | `法伤 ×0.20 + 等级 ×1`，保留驱散 | 1 | 5% | 2 |
| 1305 丹心返照 | `法伤 ×0.55 + 等级 ×3`，保留残血额外加成 | 1 | 10% | 3 |

主动每重 +2%、被动每重 +5%、等级许可规则本批不变。PVP 直接伤害仍乘 0.3，治疗仍不乘此系数，故本次同步下调治疗系数并核对治疗与伤害、典型血量的比例。
法力消耗仍按上限比例取整，多目标只扣一次；加灵力增加蓝上限也会同比增加耗蓝，不能用更大蓝上限推断可施法次数必然增加。

## 3. 持久化与加载整理

### 3.1 `PlayerSkillComp.rank`(字段 3)

- 职业技能:已学为 `1..max_rank`。**未学的职业技能不在列表里**;`rank = 0` 的职业技能条目没有合法来源,加载时剔除。
- 旧通用技能:不分等级,`rank` 恒 0(加载时把写错的非 0 归零)。
- 生效等级 = `min(存档等级, RankLimit(角色等级))`:GM 降级 / 回档后不会带着超限等级进战斗,存档等级本身不动(钱花过)。
- `ReleaseSkill`(实时场景)拒绝职业被动;职业主动在场景里仍可按老实时流程施放(robot login-test 的施法用例依赖它)。

### 3.2 加载整理(`PlayerSkillSystem::InitializeOnLoad`)

调用点从 `PlayerSkillListComp` emplace 之后挪到职业 / 等级挂上之后、`PlayerAttributeSystem::InitializeOnLoad` 之前
(被动是属性重算的输入)。顺序:`SanitizeSkillList` → 按 `Class.skill` 补初始技能(幂等)。

`SanitizeSkillList` 规则,每条剔除 / 修正都打 WARN:
- ID 为 0、重复记录:剔除;表里没有的 ID 原样保留但不生效,按混版本持久字段保护 §12 防止回退版本删掉新技能;
- 职业技能 `rank = 0`:剔除;`rank > max_rank`:夹到上限;
- **不生效但保留**:别的职业的职业技能(将来转职)、当前职业已有职业技能时的旧占位技能(旧 RegisterPlayer 遍历所有职业种子发下的 1 / 2 / 13)。
  它们留在存档里,但 `HasSkill` / 技能书 / 战斗快照 / 被动全部看不见 —— 不静默删除玩家数据,转职 / 返还上线时再定处置。

项目未上线(AGENTS 会话门禁),不做存档版本号与迁移脚本;上面的整理每次加载都跑,幂等。

## 4. 协议

| 消息 | 字段 | 说明 |
|---|---|---|
| `ListSkillsResponse` | 3 `catalog`、4 `player_level`、5 `class_id`、6 `currency_balance` | `skill_list`(2)保留,只放**现在能用**的条目(robot / 旧客户端拿它当可施放列表);`catalog` 是当前职业完整目录(按 id 升序,未学 rank = 0)。 |
| `SkillCatalogEntry` | 1..18 | 展示投影 + 本角色 `rank` / `rank_limit` / `next_rank_cost` / `upgrade_block_tip_id`(0 = 现在可修一重)/ `duration_rounds`。`icon_key` 是 Resources 相对键(`UI/Skills/1101`)。 |
| `UpgradeSkillRequest` | 1 `skill_table_id`、2 `expected_rank`、3 `count` | 修习者由会话确定;count 只收 1 / 10。 |
| `UpgradeSkillResponse` | 1 `error_message`、2 `state`、3 `upgraded_count`、4 `stop_tip_id` | 整笔拒绝才填 error;部分成功填 stop_tip_id;无论成败 state 都是处理后的完整技能书。 |
| `rpc UpgradeSkill` | 消息号 **317** | origin/main 的 `proto/message_id.txt` 0..316 无空洞,生成器只会发 317;已先登记,客户端 `MessageIds.UpgradeSkill = 317` 同号。 |
| `BattleBuffEntry` | 6 `stat_percent`、7 `applied_round` | 职业技能增减益的局内幅度与施加回合;快照带进来的 buff 一律清零这两个字段。 |
| `BattlePlayerSnapshot` | 21 `skill_ranks` | 已学职业技能(含被动)的生效等级。避开 `combat = 20`。 |
| `BattleActorState` | 28 `skill_ranks` | 引擎复核后的本局等级。避开 `combat = 27`。 |
| `BattleEventItem` | 15 `optional sint64 mana_delta` | 法力实际变化(负消耗 / 正恢复 / 0 无变化),只在 MANA / ITEM 上带。避开 `hit_kind = 14`。 |

## 5. 纯规则(`cpp/libs/services/battle/system/class_skill_rules.h`)

scene 与 battle 共用,零 ECS / 零表依赖:
- `RankLimit(level, unlock, max) = level < unlock ? 0 : min(max, 1 + (level − unlock) / 2)`;`EffectiveRank = min(存档, RankLimit)`。
- `UpgradeCost(n, unlock) = 100 × n² + 10 × unlock`(银两,`kCurrencyGold`)。
- 等级系数:主动伤害 / 治疗与辅助(增减益幅度、回蓝)每重 +2%,被动每重 +5%。
- `ManaCost = ceil(maxMana × mana_percent)`,比例 > 0 至少 1。
- `PlanUpgrade`:逐重修,碰到满重 / 等级上限 / 余额(扣完前几重后的余额)即停,返回步数、终级、总价、停止原因。
- 比例运算用百万分之一整数算法(`CeilRatio` / `FloorRatio`):`0.07 × 100` 不会因浮点误差进成 8,且跨平台逐位一致。

## 6. 战斗

### 6.1 快照与复核
scene `PlayerSkillSystem::FillBattleSnapshot`:`skill_table_ids` 放能施放的(旧通用技能 + 生效的职业主动),`skill_ranks` 放全部生效职业技能等级。
引擎 `InitPlayers` 再按同一规则复核(表里有、职业对、夹到等级上限,0 级丢掉),职业技能在 `skill_ranks` 里没有正等级就不算拥有;宝宝不收职业技能。

### 6.2 施放(`TurnBattleEngine::ExecuteClassSkill`)
- 校验链:拥有 + 等级 > 0、目标阵营、冷却(按技能各自,`cooldown_rounds + 1` 记进 map,不锁 `cooldown_id` 同为 0 的老技能)、沉默许可、法力。
- 目标:`target_id = 0` 由引擎选;选中的合法主目标排第一,其余敌方按阵位 / actor_id,治疗按气血比例再阵位;只选存活未逃的单位(治疗不复活);取 `max(1, max_targets)` 个。主目标在出手前倒下就按规则补位,不消耗随机数。
- 一个合法目标都没有:整手作废,不扣蓝、不进冷却、不出事件。
- 出手时放不出来(被沉默 / 法力被抽干):敌方技能沿用老规则降级普攻;**友方 / 自身技能不降级**(降级等于去打一个随机敌人),这一手作废。
- 顺序:冷却 → SKILL 事件 → 耗蓝(多目标只扣一次,MANA 事件 `mana_delta = −耗蓝`)→ 逐目标:敌方 命中 → 伤害(公式伤害 × 等级系数,暴击照常)→ 死亡;存活目标再 治疗 → 回蓝 → 驱散 → 增减益。
- 治疗 = `(法伤 × heal_multiplier + 施法者等级 × heal_per_level) × 等级系数 × (1 + 治疗加成被动)`,目标气血比例低于阈值再 × `(1 + execute_heal_bonus)`;满血也发一条 HEAL(value 0)。
- 回蓝 = `ceil(目标最大法力 × restore_mana_percent × 辅助系数)`,按缺口封顶,MANA 事件报实际回复量(`mana_delta = +实际`)。
- 驱散:按身上顺序移除至多 `dispel_count` 个减益(局内幅度为负 / 带 `Debuff` 标签 / 毒、灼烧、眩晕、沉默、冰冻)。

### 6.3 先手与速度
回合开始时一次排定出手序:先手优先级(本回合行动是**通过校验**的职业技能才带 `Skill.initiative`)在前,再按吃了增减益的速度,再按 actor_id。
本回合新挂上的速度增减益因此从下一回合起生效。没有职业技能的对局里优先级全 0、速度原样,出手序与改动前逐位一致。

### 6.4 增减益
- 幅度 = `Skill.buff_percent × 辅助系数(施法者等级)`,写进 `BattleBuffEntry.stat_percent`,之后不随升级变化。
- 作用:`buff_type` 10 → 物伤(普攻与物理技能取攻击时)、11 → 防御(受击方减伤公式)、13 → 速度(出手序、逃跑)。
- 同一属性:同向只取最强的一个,正负相加,倍率下限 0.1。同一行重挂刷新持续、幅度取更强者。
- 施加 / 刷新的回合末不扣持续(`applied_round`),"持续 2 回合"是完整的 2 回合。
- 没有这类 buff 时属性原样返回(不经浮点),老对局逐位不变。

## 7. 被动与属性

`PlayerAttributeSystem::Recalculate` 在全部加法项(职业初值 / 成长 / 加点 / 装备)之后按 `PlayerSkillSystem::CollectPassiveBonus` 放大六项二级属性,
暴击被动(百分点)加到 `combat` 的物理 / 法术必杀率(面板同步显示)。每次现算、不读上次结果,重复重算 / 重登不会叠加。
修习被动后立即 `Recalculate(kSkillChanged)` + 推属性面板;`kSkillChanged` 与换装同走"按比例保持当前 HP/MP",反复修习刷不出血蓝。
治疗加成(`outgoingHeal`)不进派生属性,唯一消费者是引擎(按快照 `skill_ranks` 现算)。

## 8. 写入窗口与一致性

- 拒绝窗口:战斗在途(`InBattleComp`)、跨 zone 冻结(`PlayerFrozenComp`)、交接已发起(`PlayerTravelHandoffComp`)→ `kSkillUpgradeBusy`。
- 判定顺序:组件缺失 → count → 技能存在 → 职业 → 写入窗口 → expected_rank → 等级 / 余额(PlanUpgrade)→ 扣费。任何拒绝不扣钱、不改等级。
- 扣费(`CurrencySystem::DeductCurrency`,correlation_id = skill_table_id)与改等级在同一个 scene 回调里先后完成,同属一条 `player_database` 记录,由常规存盘整份落库;不存在一边成功一边丢失的中间态(与属性洗点 Reset 同口径)。
- 重放:同一旧请求的 expected_rank 已对不上,`kSkillRankChanged`,不会二次扣费;客户端超时不猜结果,等刷新。

## 9. 未做 / 遗留

- **宠物技能**(交接文档 §6):协议未定义,客户端宠物上下文只显示"服务端尚未开放",不伪造数据。
- **截图新机制**(五阶毒、法术吸收、友方普攻二连):不在本批,需另立技能表方案。
- **Java 版**：本机没有 `D:/luyuan/xuanming-server-mmo`，无法读取或登记 `PARITY.md`，2026-10-23 当前共享数值表待同步。
  本批未改协议；既有客户端契约、消息号 317、tip 7007..7013 与 Skill/Buff/Class 表仍须由两版保持一致，不能声明 Java 已完成。
- 玩家挂机 / 超时仍只普攻(与改动前一致),宝宝自动选技能不收职业技能。
- 战斗选技目标高亮仍"任意存活单位",服务端按阵营校验并在 0 目标时自动选;客户端按 `targetTeam` 过滤高亮属 Codex 的 UI 接线(见客户端交接文档 §8)。
- 引擎没有给职业技能做 AI;自动战斗的玩家不会放技能。

## 10. 测试

2026-10-23 独立脚本 `tools/scripts/verify_turn_balance.py` 读取实际导出表并复刻现有确定性数值公式；运行命令与证据见
`player-attribute-allocation.md` §7。新表 11101 项检查、280 状态、172 次主动伤害 / 治疗的 1 重与许可最高重比较零失败；旧表同一最终脚本 108 项失败。
角色按标准加点、已解锁技能练至许可最高重；普通装为四部位基础及两条互异蓝词条取 50% 上限（不低于下限），无强化、粉黄或宝石。
85 级普通装丹心单奶理论量 3052，占四职业同档标准方案平均气血 9354.25 的 32.63%；破军 / 玄霄主招打该丹心的无暴击 PVP 伤害为 2696 / 2252，
治疗与两者之比分别 1.132 / 1.355。固定角色属性时，单奶从 1 重的 2586 增至 10 重的 3052。
急救 1305 的 85 级普通装理论治疗为 5521；目标气血低于 30% 时理论量 7178，冷却 3 回合。
实际引擎按目标缺失气血封顶，7178 不是每次必回的实际血量。验证额外覆盖该残血阈值边界 24 项，旧表新增 8 项急救爆发预算失败。
该镜像未覆盖真实施法链、随机暴击、溢出治疗、技能轮转或联机。本批另完成 `turn_battle_engine_test` 完整 282 项原生回归，
含 17 项职业技能引擎用例，零失败、跳过或未运行；证据见 `native-validation.json` 与 `native-tests.xml`。
NoRaw 静态检查因工具缺失跳过；未运行 scene 技能书测试与联机。下表中其它测试入口不视为本批已重跑。

| 用例 | 位置 |
|---|---|
| 纯规则 5 条(等级上限、价格、修习计划、耗蓝 / 整数比例、系数与被动名) | `cpp/tests/turn_battle_engine_test/class_skill_test.cpp` `ClassSkillRulesTest.*` |
| 引擎 17 条(快照复核、宝宝不收、坏阵营、多目标一次扣蓝、等级系数伤害、友方目标与血量排序、群疗不复活、治疗加成 + 残血加成、增益两整回合、防御增益减伤、冷却、先手、减速次回合生效、回蓝封顶、驱散、沉默后友方作废 / 敌方降级、同种子确定性) | 同上 `ClassSkillEngineTest.*` |
| 真表契约(32 行形状、增减益行、职业种子) | `table_battle_data_provider_test.cpp` `ClassSkillCatalogMatchesContentBaseline` |
| scene 技能书 13 条(注册只发本职业、加载整理、5~9 职业保留旧技能、学习扣价、被动修习、10 次停在等级 / 余额、拒绝不扣钱、重放只扣一次、无钱 / 满重 / 等级不足、战斗 / 冻结窗口、快照、被动按生效等级) | `cpp/tests/skill_test/player_skill_book_test.cpp` |
| 既有 `PlayerSkillSystemTest.*`(SanitizeSkillList / RegisterPlayer 不补建组件) | `cpp/tests/skill_test/skill_test.cpp`(行为不变,需复跑) |
