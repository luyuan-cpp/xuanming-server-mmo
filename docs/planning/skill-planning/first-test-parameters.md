# 62项正式技能首测参数

更新：2026-11-23。本页为当前有效 62 项正式原型及 63 条 Buff 的可调首测配置（2026-11-21 撤回人物单体/普攻系 8 项与 10 条对应 Buff，旧编号与 ID 永久保留不复用；`A18`/`T01` 取消人物资格转为宝宝专属；`A15` 保留；`B12` 修正为 `pet_max_targets=2`；2026-11-23 新增 `A41 玄镜映法` 敌方技能复制主动技，Boss 与禁复制技免疫）。**表已落地，所有新增倍率、概率、耗蓝、冷却与未明确定案的边界仍待平衡**；不是最终平衡结论。运行实现、导表与测试结论统一见[实施清单](implementation.md)，此页不提前宣称测试通过。

## 1. 事实源与覆盖范围

运行配置来自 `data/Skill.xlsx`、`data/Buff.xlsx`，结构来自同名 `data/schema/*_table.proto`。`tools/scripts/skill_catalog.py` 是本轮可复现的初始化目录与校验器，后续策划调数应同步更新本页及校验基线，生成脚本遇到已存在但不同的值会拒绝覆盖。

正式目录为人物/共享主动29、宝宝专属主动12（含 A18）、被动15、追击3（均为宝宝专属）、开场3，共62。策略S/SP候选、暂缓E候选、撤回项（`P11`、`A07`/`A17`/`A28`/`A32`/`A37`/`P10`/`T02`/`T03`）和所有合并旧编号不生成新原型。旧46条Skill和26条Buff原样保留。新增63条Buff承载真实操作；P01/P02/P03/P04/P06沿既有 `passive_stat` 聚合入口，不再重复挂属性Buff。

Skill ID分别为A10000、B11000、P12000、T13000、O14000加编号。Buff ID为该Skill ID乘10加本原型效果序号。这是编表约定；运行时只读取操作、事件、主体资格和参数，禁止按策划编号或这些ID段分支。

## 2. 本轮首测口径

- 人物和宝宝共用原型只计一次，宝宝限制由 `caster_mask`、`pet_max_targets` 表达。人物技能不锁职业。普通行动每轮一次，反击、追击及开场初始化不额外占普通行动。
- 技能全量解锁入口仍走系统既有学习/快照流程；首测解锁等级1、最高10重，不新增任务/抽取/刷材料门槛。这里不设置人物4＋2或宝宝2/3招装配上限。
- 比例字段全部为小数。主动直接伤害基础为 `4*level` 加对应攻击属性乘 `attack_multiplier`，仍经过正式伤害、减免、暴击等管线。治疗/容量为 `flat_value + level*per_level + value_basis*ratio`，施加时快照。主动伤害及数值效果每增加一重提高基础量2%；scene静态属性被动每重提高基础比例5%。Buff属性比例、命中率、触发概率、限次、持续时间、反弹和追击倍率首测直接读取表值，不暗加另一条全局成长曲线。
- 新模型CD数值表示再次施放的回合偏移，CD3在第1轮施放后最早第4轮再用；旧模型继续原有完整回合CD口径。耗蓝按最大法力比例，普通非零费用向上取整且至少1；零耗蓝反击独立处理。
- 持续轮数的 `tick_on_apply=1` 包含施加轮末，0从下一轮末起扣时。A03/A14/A25/A35均5轮；A16首测3轮，排速只影响下次排序；控制保留至少一个后续完整回合窗口。A26当轮末可结算，最多5次。
- A19毒3次、A30烧3次；毒同名跨来源同强刷新、强者覆盖，弱者不替换。蔓毒每份延长1次且刷新保留已延长标记；毒火联动须同时有毒和烧，各提前1次，消费原状态及原施加者强度，不能在轮末重复跳伤。
- A24实际扣血才唤醒；护盾全吸收、未命中不唤醒。A38冻结隔离伤害及新障碍，旧持续状态时钟继续；友方治疗/保护/净化允许，敌方抽蓝/驱散/劝退阻断。
- A26死亡保留回生印，轮末持续伤害后结算印记再判胜负；复活20%最大气血并消费整印。重新施加新印可再次复活，没有每场一次限制。A33每目标净化1项，优先控制；不附加原A11候选少量治疗。A34每目标驱散1项。
- A10/A12/A26独立治疗基础分别为20/17.5/6×人物等级，再乘技能支持等级系数；不读取攻击面板。初始曲线锚定旧首测Lv30/法攻600/rank1强度，尚非平衡结论。
- A40仅独立施放、七目标。容量为16×人物等级×技能支持等级系数（按A10普通治疗基础的80%编表，但运行时不调用治疗产生护盾）；持续2轮含当轮、耗蓝8%、CD3。同名容量按剩余容量择强，不因弱覆盖延时；目标死亡移除，来源死亡保留；可驱散。只吸收直接伤害，不吸收毒、反射、转伤及资源费用。
- A41玄镜映法为人物专属主动、敌方单目标、耗蓝10%、CD3、自身 `forbid_copy=1`。优先复制目标本回合选定的可复制主动技能，若目标本回合未选可复制技能则从其已学可复制主动技能中确定性随机抽一招释放（方案 A）；以施法者自身属性、A41自身重数与85%数值折算零耗蓝释放，不进原技冷却，不递归触发追击或反击。抄到友方/自身技能自动重映射到己方。Boss（`Monster.is_boss=1`）、带 `SkillCopy` 免疫标签的单位，以及标 `forbid_copy=1`/生命代价/限次/资源转换的技能均不可复制；被动 `P09`/`P19` 同步拦截 Boss 技能与禁复制技。
- 类型护罩→普通容量盾→法力盾→援护预算→实际气血伤害→反射依次处理，优先级10/20/30/40/50。B05/B06最多吸收2次且有容量；B14受益者用自己的法力吸收50%伤害，每吸收1点耗1法力。A36分担25%单体直接伤害，来源每轮承担最多自身最大气血30%，不循环转伤。
- B12首测法术、两目标（`pet_max_targets=2`）、按实际扣血30%吸血给施法宝宝，不吃治疗修正、不从护罩吸收量获益。B13降至1气血后按已付气血125%削蓝，当前1气血不可施放。B15耗自身25%最大法力，按实际已付法力2倍临时增最大气血并回血，持续3轮，到期回归原上限。B16每战1次、牺牲自身，对其他存活友方治疗其最大气血60%并净化最多3项。
- B09劝退不等于死亡，保留气血与单位身份。首测离场后等待2个完整回合：第1轮劝退，最早第4轮由其人物主人提交召回行动；主人消耗本轮正常行动，召回宝宝本轮不追加行动，下一轮恢复正常指令。不能召回别人的宝宝或死亡宝宝。返回截止为施放轮＋配置轮数＋1，`PET_RETREAT`事件值1表示退场、0表示召回。
- P08原毒施加者每轮首次实际毒伤恢复自身最大气血4%，自身灼烧不能借目标身上其他人的毒触发。P09/P19首测20%概率、每轮最多1次、复制同技能且使用自身属性、零耗蓝；逐段实际扣血后立即触发，群攻的每个受击者各自遵守本轮1次上限，反击释放时来袭者须仍存活。不等待主行动结束；派生动作不再互相触发追击或反击。P18每轮首次实际受敌方直接伤害回5%最大法力。
- T01仅限宝宝、首测35%概率、50%物理倍率、最多2敌，每轮最多1次；T05挂20%破防3轮且不附伤；T06法术行动后35%概率追加40%法术倍率、最多2目标、每轮最多1次，不能每个命中目标重复触发。
- O01第3轮开始每轮加5%直接伤害、最多3层；来源死亡暂停且不补增长。O02开局前2轮直接减伤20%，来源死亡暂停但时钟照走。O03优先主人，单目标盾1倍法攻＋5×等级，前2轮有效，来源死亡不撤回已生成容量。复活不重新执行开场。
- Pet22行保持原名字、模型、品质、资质和商业字段；旧法术14映射共享A06，石灵首测A14/P04，金猊A02/T01，其余已有法术宝宝映射A06。这是初始技能配置，不限制后续合法学习，也不复制已合并B01/B02/B03/P12/T04。

## 3. 完整Skill映射和首测资源

表中“自身”指自动监听宿主；O技能按开场友方选人。Buff参数详见下一节。角色/宝宝目标上限不重复计入原型数量。

| 原型 | Skill ID | 名称 | 主体 | 目标（人物／宝宝） | 耗蓝 | CD | effect / init_effect / 静态属性 |
|---|---:|---|---|---|---:|---:|---|
| A02 | 10002 | 横戈裂岳 | 人物、宝宝 | 敌5／敌2 | 5% | 0 | 100020 |
| A03 | 10003 | 戈鸣断势 | 人物 | 敌5 | 8% | 2 | 100030, 100031 |
| A06 | 10006 | 紫霄雷阵 | 人物、宝宝 | 敌5／敌2 | 5% | 0 | 100060 |
| A08 | 10008 | 九霄归墟 | 人物 | 敌5 | 15% | 4 | 100080 |
| A10 | 10010 | 青莲润生 | 人物、宝宝 | 友7／友1 | 6% | 0 | 100100 |
| A12 | 10012 | 丹心返照 | 人物 | 友7 | 10% | 2 | 100120 |
| A14 | 10014 | 灵鼎护元 | 人物、宝宝 | 友7／友1 | 6% | 2 | 100140 |
| A15 | 10015 | 太虚澄心 | 人物 | 自身1 | 0% | 2 | 100150 |
| A16 | 10016 | 青羽催行 | 人物、宝宝 | 友7／友1 | 6% | 2 | 100160 |
| A18 | 10018 | 碎甲击 | 宝宝 | 敌2 | 5% | 1 | 100180, 100181 |
| A19 | 10019 | 蚀骨毒 | 人物、宝宝 | 敌5／敌2 | 8% | 2 | 100190 |
| A20 | 10020 | 封灵咒 | 人物 | 敌5 | 10% | 3 | 100200 |
| A21 | 10021 | 闭灵锁魄 | 人物 | 敌5 | 8% | 3 | 100210 |
| A22 | 10022 | 禁武缚刃 | 人物 | 敌5 | 8% | 3 | 100220 |
| A23 | 10023 | 乱心惑神 | 人物 | 敌5 | 10% | 3 | 100230 |
| A24 | 10024 | 七魄入梦 | 人物 | 敌7 | 10% | 3 | 100240 |
| A25 | 10025 | 战意昂扬 | 人物、宝宝 | 友7／友1 | 6% | 2 | 100250 |
| A26 | 10026 | 生生不息 | 人物、宝宝 | 友7／友1 | 10% | 3 | 100260 |
| A27 | 10027 | 幻影迷踪 | 人物、宝宝 | 友7／友1 | 6% | 2 | 100270 |
| A29 | 10029 | 蔓毒术 | 人物 | 敌5 | 6% | 2 | 100290 |
| A30 | 10030 | 焚心炎 | 人物 | 敌5 | 8% | 2 | 100300, 100301 |
| A31 | 10031 | 毒火交攻 | 人物 | 敌5 | 10% | 2 | 100310, 100311 |
| A33 | 10033 | 清心荡秽 | 人物、宝宝 | 友7／友1 | 6% | 1 | 100330 |
| A34 | 10034 | 驱灵散势 | 人物、宝宝 | 敌7／敌1 | 8% | 2 | 100340 |
| A35 | 10035 | 断脉咒 | 人物 | 敌5 | 8% | 2 | 100350 |
| A36 | 10036 | 同袍共济 | 人物 | 友7 | 8% | 3 | 100360 |
| A38 | 10038 | 冰封玄狱 | 人物 | 敌7 | 12% | 3 | 100380 |
| A39 | 10039 | 天崩裂岳 | 人物 | 敌5 | 15% | 4 | 100390 |
| A40 | 10040 | 独立护盾（名称待定） | 人物 | 友7 | 8% | 3 | 100400 |
| A41 | 10041 | 玄镜映法 | 人物 | 敌1 | 10% | 3 | 100410 |
| B05 | 11005 | 如意灵环 | 宝宝 | 友1 | 6% | 2 | 110050 |
| B06 | 11006 | 神龙护体 | 宝宝 | 友1 | 6% | 2 | 110060 |
| B07 | 11007 | 乾坤反震 | 宝宝 | 友1 | 6% | 2 | 110070 |
| B08 | 11008 | 五色返照 | 宝宝 | 友1 | 6% | 2 | 110080 |
| B09 | 11009 | 游说退敌 | 宝宝 | 敌1 | 10% | 3 | 110090 |
| B11 | 11011 | 连战之印 | 宝宝 | 友1 | 6% | 2 | 110110 |
| B12 | 11012 | 摄魂汲血 | 宝宝 | 敌2 | 8% | 2 | 110120 |
| B13 | 11013 | 舍命破灵 | 宝宝 | 敌1 | 0% | 4 | 110130 |
| B14 | 11014 | 法力护盾 | 宝宝 | 友1 | 8% | 2 | 110140 |
| B15 | 11015 | 移灵养元 | 宝宝 | 自身1 | 25% | 4 | 110150 |
| B16 | 11016 | 舍身救主 | 宝宝 | 友1 | 0% | 0 | 110160, 110161 |
| P01 | 12001 | 砺刃藏锋 | 人物 | 自身1 | 0% | 0 | physicalAttack 12% |
| P02 | 12002 | 灵台凝元 | 人物 | 自身1 | 0% | 0 | magicAttack 12% |
| P03 | 12003 | 铁骨铸魂 | 人物 | 自身1 | 0% | 0 | defense 12% |
| P04 | 12004 | 百战归元 | 人物、宝宝 | 自身1／自身1 | 0% | 0 | maxHp 15% |
| P05 | 12005 | 悬壶济世 | 人物 | 自身1 | 0% | 0 | 120050 |
| P06 | 12006 | 流云轻身 | 人物、宝宝 | 自身1／自身1 | 0% | 0 | speed 10% |
| P07 | 12007 | 凝神节用 | 人物 | 自身1 | 0% | 0 | 120070 |
| P08 | 12008 | 借毒养元 | 人物 | 自身1 | 0% | 0 | 120080 |
| P09 | 12009 | 反戈一击 | 人物 | 自身1 | 0% | 0 | 120090 |
| P14 | 12014 | 灵障 | 宝宝 | 自身1 | 0% | 0 | 120140 |
| P15 | 12015 | 忠心护主 | 宝宝 | 自身1 | 0% | 0 | 120150 |
| P16 | 12016 | 凝心守神 | 宝宝 | 自身1 | 0% | 0 | 120160 |
| P17 | 12017 | 卸力护身 | 宝宝 | 自身1 | 0% | 0 | 120170 |
| P18 | 12018 | 灵息回转 | 宝宝 | 自身1 | 0% | 0 | 120180 |
| P19 | 12019 | 法术反击 | 人物 | 自身1 | 0% | 0 | 120190 |
| T01 | 13001 | 乘胜追击 | 宝宝 | 自身1 | 0% | 0 | 130010 |
| T05 | 13005 | 裂甲追击 | 宝宝 | 自身1 | 0% | 0 | 130050 |
| T06 | 13006 | 灵术回响 | 宝宝 | 自身1 | 0% | 0 | 130060 |
| O01 | 14001 | 后发制人 | 人物 | 友7 | 0% | 0 | 140010 |
| O02 | 14002 | 固阵待敌 | 人物 | 友7 | 0% | 0 | 140020 |
| O03 | 14003 | 守主灵契 | 宝宝 | 友1 | 0% | 0 | 140030 |

## 4. 全部Buff效果与参数

未列出的数值列为空/0；`infinite_duration=1`为常驻监听，非无限次触发许可。概率0按契约表示确定触发；限次列0表示该列不限，派生链边界仍生效。`tag`支持净化和联动，`stack_key`解决同名跨来源覆盖。每条数据可通过校验脚本和正式schema核对。

| Buff ID | 原型 | 操作 | 事件 | 参数 |
|---:|---|---|---|---|
| 100020 | A02 | 直接伤害 | 施放/挂载 | `{}` |
| 100030 | A03 | 直接伤害 | 施放/挂载 | `{}` |
| 100031 | A03 | 属性修正 | 施放/挂载 | `{"modifier_stat":1,"ratio":-0.2,"duration_rounds":5,"tag":["Debuff"],"tick_on_apply":1}` |
| 100060 | A06 | 直接伤害 | 施放/挂载 | `{}` |
| 100080 | A08 | 直接伤害 | 施放/挂载 | `{}` |
| 100100 | A10 | 治疗 | 施放/挂载 | `{"value_basis":0,"per_level":20}` |
| 100120 | A12 | 治疗 | 施放/挂载 | `{"value_basis":0,"per_level":17.5}` |
| 100140 | A14 | 属性修正 | 施放/挂载 | `{"modifier_stat":3,"ratio":0.25,"duration_rounds":5,"tag":["Buff"],"tick_on_apply":1}` |
| 100150 | A15 | 回蓝 | 施放/挂载 | `{"value_basis":4,"ratio":0.25}` |
| 100160 | A16 | 属性修正 | 施放/挂载 | `{"modifier_stat":4,"ratio":0.25,"duration_rounds":3,"tag":["Buff"],"tick_on_apply":1}` |
| 100180 | A18 | 直接伤害 | 施放/挂载 | `{}` |
| 100181 | A18 | 属性修正 | 施放/挂载 | `{"modifier_stat":3,"ratio":-0.25,"duration_rounds":3,"tag":["Break"],"tick_on_apply":1,"stack_key":100181,"refresh_policy":1}` |
| 100190 | A19 | 持续伤害 | 施放/挂载 | `{"value_basis":2,"ratio":0.25,"per_level":2,"state_kind":7,"tick_count":3,"duration_rounds":3,"application_chance":0.75,"tick_on_apply":1,"tag":["Poison","Debuff"],"stack_key":100190,"refresh_policy":1}` |
| 100200 | A20 | 控制 | 施放/挂载 | `{"state_kind":1,"duration_rounds":1,"application_chance":0.65,"tag":["SealActive","Control","Debuff"],"tick_on_apply":0,"refresh_policy":1}` |
| 100210 | A21 | 控制 | 施放/挂载 | `{"state_kind":2,"duration_rounds":2,"application_chance":0.75,"tag":["SealMagic","Control","Debuff"],"tick_on_apply":0,"refresh_policy":1}` |
| 100220 | A22 | 控制 | 施放/挂载 | `{"state_kind":3,"duration_rounds":2,"application_chance":0.75,"tag":["SealPhysical","Control","Debuff"],"tick_on_apply":0,"refresh_policy":1}` |
| 100230 | A23 | 控制 | 施放/挂载 | `{"state_kind":4,"duration_rounds":1,"application_chance":0.65,"tag":["Confusion","Control","Debuff"],"tick_on_apply":0,"refresh_policy":1}` |
| 100240 | A24 | 控制 | 施放/挂载 | `{"state_kind":5,"duration_rounds":2,"application_chance":0.65,"tag":["Sleep","Control","Debuff"],"tick_on_apply":0,"refresh_policy":1}` |
| 100250 | A25 | 属性修正 | 施放/挂载 | `{"modifier_stat":15,"ratio":0.2,"duration_rounds":5,"tag":["Buff"],"tick_on_apply":1}` |
| 100260 | A26 | 回生印 | 施放/挂载 | `{"value_basis":0,"per_level":6,"secondary_ratio":0.2,"duration_rounds":5,"tick_count":5,"tick_on_apply":1,"keep_on_death":1,"tag":["Buff","Revive"],"refresh_policy":1}` |
| 100270 | A27 | 属性修正 | 施放/挂载 | `{"modifier_stat":7,"ratio":0.15,"duration_rounds":3,"tag":["Buff"],"tick_on_apply":1}` |
| 100290 | A29 | 延长持续伤害 | 施放/挂载 | `{"required_tag":"Poison","effect_count":1}` |
| 100300 | A30 | 直接伤害 | 施放/挂载 | `{}` |
| 100301 | A30 | 持续伤害 | 施放/挂载 | `{"value_basis":2,"ratio":0.2,"per_level":2,"state_kind":8,"tick_count":3,"duration_rounds":3,"application_chance":0.8,"tick_on_apply":1,"tag":["Burn","Debuff"],"refresh_policy":1}` |
| 100310 | A31 | 直接伤害 | 施放/挂载 | `{}` |
| 100311 | A31 | 催动持续伤害 | 施放/挂载 | `{"required_tag":"Poison","second_required_tag":"Burn","effect_count":1}` |
| 100330 | A33 | 净化 | 施放/挂载 | `{"effect_count":1}` |
| 100340 | A34 | 驱散 | 施放/挂载 | `{"effect_count":1}` |
| 100350 | A35 | 属性修正 | 施放/挂载 | `{"modifier_stat":6,"ratio":-0.35,"duration_rounds":5,"tag":["Debuff"],"tick_on_apply":1,"application_chance":0.8}` |
| 100360 | A36 | 援护 | 施放/挂载 | `{"ratio":0.25,"value_basis":3,"secondary_ratio":0.3,"duration_rounds":3,"tick_on_apply":1,"require_living_caster":1,"tag":["Buff"],"effect_priority":40}` |
| 100380 | A38 | 控制 | 施放/挂载 | `{"state_kind":6,"duration_rounds":1,"application_chance":0.55,"tag":["Freeze","Control","Debuff"],"tick_on_apply":0,"refresh_policy":1}` |
| 100390 | A39 | 直接伤害 | 施放/挂载 | `{}` |
| 100400 | A40 | 容量盾 | 施放/挂载 | `{"value_basis":0,"per_level":16,"duration_rounds":2,"tick_on_apply":1,"tag":["Buff","Shield"],"refresh_policy":2,"effect_priority":20}` |
| 100410 | A41 | 复制敌方技能 | 施放/挂载 | `{"ratio":0.85,"tag":["SkillCopy"]}` |
| 110050 | B05 | 容量盾 | 施放/挂载 | `{"value_basis":2,"ratio":1.2,"per_level":6,"hit_limit":2,"damage_filter":2,"duration_rounds":3,"tick_on_apply":1,"refresh_policy":2,"tag":["Buff","Shield"],"effect_priority":10}` |
| 110060 | B06 | 容量盾 | 施放/挂载 | `{"value_basis":2,"ratio":1.2,"per_level":6,"hit_limit":2,"damage_filter":1,"duration_rounds":3,"tick_on_apply":1,"refresh_policy":2,"tag":["Buff","Shield"],"effect_priority":10}` |
| 110070 | B07 | 反射 | 受击后 | `{"damage_filter":1,"secondary_ratio":0.25,"duration_rounds":3,"tick_on_apply":1,"tag":["Buff"],"effect_priority":50}` |
| 110080 | B08 | 反射 | 受击后 | `{"damage_filter":2,"secondary_ratio":0.25,"duration_rounds":3,"tick_on_apply":1,"tag":["Buff"],"effect_priority":50}` |
| 110090 | B09 | 劝退 | 施放/挂载 | `{"application_chance":0.5,"state_kind":9,"duration_rounds":2}` |
| 110110 | B11 | 追击伤害 | 普攻后 | `{"ratio":0.5,"damage_filter":1,"target_count":1,"pet_target_count":1,"hit_limit":1,"duration_rounds":2,"tick_on_apply":0,"tag":["Buff"]}` |
| 110120 | B12 | 直接伤害 | 施放/挂载 | `{"secondary_ratio":0.3}` |
| 110130 | B13 | 抽蓝 | 施放/挂载 | `{"value_basis":5,"ratio":1.25}` |
| 110140 | B14 | 法力盾 | 施放/挂载 | `{"ratio":0.5,"secondary_ratio":1,"duration_rounds":3,"tick_on_apply":1,"tag":["Buff","Shield"],"effect_priority":30}` |
| 110150 | B15 | 临时气血上限 | 施放/挂载 | `{"value_basis":6,"ratio":2,"duration_rounds":3,"tick_on_apply":1,"tag":["Buff"]}` |
| 110160 | B16 | 治疗 | 施放/挂载 | `{"value_basis":3,"ratio":0.6}` |
| 110161 | B16 | 净化 | 施放/挂载 | `{"effect_count":3}` |
| 120050 | P05 | 属性修正 | 施放/挂载 | `{"modifier_stat":8,"ratio":0.15,"infinite_duration":1}` |
| 120070 | P07 | 属性修正 | 施放/挂载 | `{"modifier_stat":9,"ratio":-0.15,"infinite_duration":1}` |
| 120080 | P08 | 条件回血 | 持续伤害后 | `{"required_tag":"Poison","value_basis":3,"ratio":0.04,"per_round_limit":1,"infinite_duration":1}` |
| 120090 | P09 | 镜像技能 | 受击后 | `{"damage_filter":1,"ratio":1,"trigger_chance":0.2,"per_round_limit":1,"infinite_duration":1}` |
| 120140 | P14 | 属性修正 | 施放/挂载 | `{"modifier_stat":10,"ratio":0.15,"infinite_duration":1}` |
| 120150 | P15 | 属性修正 | 施放/挂载 | `{"modifier_stat":11,"ratio":0.25,"infinite_duration":1}` |
| 120160 | P16 | 属性修正 | 施放/挂载 | `{"modifier_stat":12,"ratio":0.2,"infinite_duration":1}` |
| 120170 | P17 | 属性修正 | 施放/挂载 | `{"modifier_stat":13,"ratio":0.3,"infinite_duration":1}` |
| 120180 | P18 | 条件回蓝 | 受击后 | `{"value_basis":4,"ratio":0.05,"per_round_limit":1,"infinite_duration":1}` |
| 120190 | P19 | 镜像技能 | 受击后 | `{"damage_filter":2,"ratio":1,"trigger_chance":0.2,"per_round_limit":1,"infinite_duration":1}` |
| 130010 | T01 | 追击伤害 | 普攻后 | `{"ratio":0.5,"damage_filter":1,"target_count":1,"pet_target_count":2,"trigger_chance":0.35,"per_round_limit":1,"infinite_duration":1}` |
| 130050 | T05 | 触发子Buff | 普攻后 | `{"per_round_limit":1,"infinite_duration":1,"target_sub_buff":[130051]}` |
| 130051 | T05 | 属性修正 | 施放/挂载 | `{"modifier_stat":3,"ratio":-0.2,"duration_rounds":3,"tick_on_apply":1,"tag":["Break","Debuff"],"stack_key":100181,"refresh_policy":1}` |
| 130060 | T06 | 追击伤害 | 技能后 | `{"ratio":0.4,"damage_filter":2,"target_count":2,"pet_target_count":2,"trigger_chance":0.35,"per_round_limit":1,"infinite_duration":1}` |
| 140010 | O01 | 成长光环 | 轮开始 | `{"modifier_stat":5,"ratio":0.05,"start_round":3,"stack_limit":3,"require_living_caster":1,"keep_on_death":1,"refresh_policy":1,"tag":["Buff"],"infinite_duration":1}` |
| 140020 | O02 | 属性修正 | 施放/挂载 | `{"modifier_stat":16,"ratio":-0.2,"duration_rounds":2,"tick_on_apply":1,"require_living_caster":1,"keep_on_death":1,"refresh_policy":1,"tag":["Buff"]}` |
| 140030 | O03 | 容量盾 | 施放/挂载 | `{"value_basis":2,"ratio":1,"per_level":5,"duration_rounds":2,"tick_on_apply":1,"refresh_policy":2,"tag":["Buff","Shield"],"effect_priority":20}` |

## 5. 可复现生成与验证

依赖Python（导表器既有openpyxl）和Node的 `@oai/artifact-tool`。先使用现有官方依赖环境，不安装或升级系统工具。`ARTIFACT_NODE_MODULES`设为当前环境提供的Node依赖目录，`PYTHON`可选指定Python解释器。

```powershell
python tools/scripts/skill_catalog.py plan --output build/verification/skill-catalog/catalog-plan.json
node tools/scripts/skill_catalog_xlsx.mjs build/verification/skill-catalog/catalog-plan.json --preview
node tools/scripts/skill_catalog_xlsx.mjs build/verification/skill-catalog/catalog-plan.json
python tools/scripts/skill_catalog.py check
```

预览文件保存在计划目录。脚本按最新源文件哈希校验并拒绝并行覆盖；写格由artifact-tool完成，再将已校验目标单元格移回原OOXML包，保留原样式、列宽、页签及非目标数据。再次执行相同计划应报告 `already current`，不重写文件。手工改表后需重新生成计划；不同已有值会中止，不能用旧计划覆盖新策划调整。

本页可通过 `python tools/scripts/skill_catalog.py documentation --output docs/planning/skill-planning/first-test-parameters.md` 从初始化目录重建。生成/核对源表后，由主流程执行 `dev.bat gen`（或项目现有导表入口），禁止手写generated产物。

首测验收不能只有62行存在：必须通过全部引用闭合、合法主体、目标数、事件顺序、时长、消耗、死亡/复活/净化/派生链和同seed重放用例。数值强弱仍要用实战对局验证；本次目录闭合不代表已经平衡。
