# 角色属性加点系统

> 落地日期:2026-09-03。实现分布:`proto/common/component/player_attribute_comp.proto`、
> `proto/scene/player_attribute.proto`、`cpp/libs/services/scene/player/system/player_attribute.{h,cpp}`、
> `cpp/libs/services/scene/player/system/attribute_allocation_rules.h`、
> `cpp/nodes/scene/handler/rpc/player/player_attribute_handler.cpp`(守护段)、
> 策划表 `data/AttributePool.xlsx` / `AttributeDimension.xlsx` / `AttributeRule.xlsx` / `AttributeAutoPlan.xlsx`。
> 客户端见 `client/unity` 的 `Assets/Scripts/Game/Attribute/` 与 `Assets/Scripts/UI/Ugui/Attribute/`。

## 1. 目标与非目标

**目标**:玩家按等级获得属性点(2026-09-14 起删掉了相性点 / 仙魔点两池),分配到体质 / 灵力 / 力量 / 敏捷四个"一级属性维度"上,
由服务器换算成六项"二级属性"(气血 / 法力 / 物伤 / 法伤 / 速度 / 防御),并真正参与战斗结算。
支持多套加点方案(切换 / 新开 / 改名)、自动加点推荐、按池洗点。

**非目标(一期不做)**:
- 经验与升级曲线(本系统只消费 `LevelComp.level`,不产出等级;升级由未来的经验系统触发 `PlayerUpgradeEvent`,本系统只监听);
- 洗点道具 / 分池部分洗点(只做整池重置 + 金币);
- 装备 / 丹药写入 `bonus_values` 的具体来源(字段与算法已就位,写入方待背包/装备系统);

## 2. 数据模型

### 2.1 三层数据

| 层 | 载体 | 是否落库 | 说明 |
|---|---|---|---|
| 点数总量 | 无(按 `LevelComp.level` + 表实时算) | ❌ | 等级换算,绝不存量,改表即刻全服生效、不需洗数据 |
| 已分配点 | `PlayerAttributeComp.schemes[].allocated`(dimension_id → 点) | ✅ `player_database.attribute_component`(字段 11) | 玩家唯一的持久化加点数据 |
| 二级属性 | `DerivedAttributesComp` | ❌ | 每次加载 / 加点 / 切方案 / 升级由 `Recalculate` 重算 |

**上线前置:DB 新列**。`player_database.attribute_component`(字段 11)是一列新的 MEDIUMBLOB。`go/db` 只在
`AutoMigrateSchema=true`(本地 `etc/db.yaml`)时启动补列;生产档默认不跑 DDL,而 proto2mysql 的 SELECT/REPLACE 枚举
descriptor 全部字段——存量库缺这一列时**整行玩家数据读写都报 Unknown column**,不只是属性丢失。部署本功能前必须
`cd go/db && go run ./cmd/migrate -command up`(Drift 自动产出 `ALTER TABLE player_database ADD COLUMN attribute_component`);
`go/db/model`、`go/login/model`、`go/player_locator` 三份 `mysql_database_table.sql` 已同步加列,按它手工建表的环境不会缺。

**"总量不落库"是这套设计的核心取舍**:等级涨了就自动多点,策划改 `points_per_level` 不需要补发,
回档 / 跨 zone 迁移也只搬"已分配",不会出现"总量与等级对不上"的第二真相。
代价是任何"额外发点"必须走 `bonus_points`(pool_id → 点)显式存,不能偷偷改总量。

### 2.2 策划表(5 行表头,`tools/data_table_exporter`)

- **`AttributePool`** —— 池:`unlock_level` / `points_per_level` / `base_points` / `dimension_cap`(单维度上限,0=不限)/
  `reset_cost_gold` / `reset_free_below_level`。当前两行:属性点(行 1,1 级解锁,每级 5 点)与宝宝属性点(行 4,`owner_type=1`,见 `player-pet.md`)。
  相性点(行 2)/ 仙魔点(行 3)2026-09-14 按用户决定删除,连同维度 201-205 / 301-304 与自动加点方案 2 / 3 / 12 / 13 / 22 / 23;
  存档里分配到这些维度的点在加载时由 `SanitizeSchemes` 静默清掉(§5),不需要数据迁移。
- **`AttributeDimension`** —— 维度 + **系数矩阵**:`pool_id`(idx)、`name` / `desc`(直接下发客户端做面板文案与悬浮说明)、
  `sort`、`base_per_level`(不占点的自然成长)、以及六列 double 系数 `max_health / max_mana / physical_attack /
  magic_attack / speed / defense`。**加一个新属性维度 = 加一行表,不改任何代码。**
  角色属性点当前系数(2026-09-10 策划定;**2026-09-13 起只作用于自然成长与装备加成**,玩家分配的点改走 §3.1 百分比公式):
  体质每点 +50 气血上限、+60 防御;灵力每点 +40 法伤、+40 法力上限(防御 / 法力为 2026-09-14 单位放大后的值);力量每点 +50 物伤;敏捷每点 +36 速度。
  宝宝池(401-404,每点固定加值):体质 42 血 + 45 防 / 灵力 30 蓝 + 33 法伤 / 力量 40 物伤 / 敏捷 30 速度(`player-pet.md` §6)。
  **速度单位 2026-09-14 起 ×12**(用户"按玩家能接受的范围调整"):敏捷原为每点 +3 速度,玩家分配 1 点只加约 0.09,面板纹丝不动。
  `Class.init_speed`(20 → 240)、维度 104 / 404 的速度系数(3 → 36、2.5 → 30)、`Pet.init_speed`、`Monster.speed`、
  引擎兜底速度(`kMonsterDefaultSpeed` 5 → 60、scene `kFallbackBattleSpeed` 10 → 120)一起乘 12,逃跑系数 `kFleeSpeedFactor` 同除 12。
  速度只用于出手序(纯相对比较)与逃跑(速度差 × 系数),整体放大不改变任何战斗结论;走路速度不读这个属性
  (客户端上报 Velocity,按 `kMaxTrustedClientSpeed` 截断),实时战斗、AOI、冷却也都不读,均不受影响。
  **防御单位 ×12、法力单位 ×4**(同日跟进,同一验收口径:体质分配每点原只 +0.08 防御、灵力 +0.31 法力):
  `Class.init_armor` 10 → 120、`Class.init_mana` 200 → 800,维度 101 / 401 防御 5 → 60、3.75 → 45,102 / 402 法力 10 → 40、7.5 → 30,
  `Pet.init_mana` ×4,`Monster.armor` ×12,引擎兜底 `kMonsterDefaultArmor` 2 → 24,伤害公式等级系数 30 + 10 × 等级 → 360 + 120 × 等级
  (`combat_damage_rules.h` 的 `kDefenseUnitScale`),`Skill` 1 号耗蓝 10 → 40。护甲、防御、等级系数同乘 12,受伤比例逐位不变
  (整数输入、同一个分数);法力上限与耗蓝同乘 4,一管蓝放几次技能不变。抗性是百分比,不乘。
  项目未上线、没有老存档,不做存档迁移;护甲由 `Recalculate` 每次按职业表重写(§3.1)。
  `desc` 只写定性说明、不写系数数字:面板不下发系数,文案里写数字就成了第二份真相,改系数时极易漏改。
- **角色等级上限 85**(`player_level_rules.h` 的 `playerlevel::kMaxLevel`,`PlayerAttributeSystem::kMaxLevel` 是它的别名;
  2026-09-10 由 200 改为 85)。经验表未落地前是代码常量。`GmSetPlayerLevel` 越界即 `kInvalidParameter`;
  读存档(登录 / 跨 zone 落地 / 回档都走 `PlayerDatabaseMessageFieldsUnmarshal`)时超限等级压回 85 并打 WARN,
  随后 §3.1 的"已分配 > 总量"收敛整池返还多出的点。按此口径属性点满级总量 = 85 × 5 = 425。
- **`AttributeRule`** —— 单行全局规则:方案数上限 / 免费方案数 / 开新方案金币 / 切换冷却秒 / 方案名长度;
  2026-09-13 加一列 `alloc_efficiency_bonus`(0.20;填 0 = 纯线性,负数按坏表退回 0.20),含义见 §3.1。
  公式里的 425(满投点数)与 510(除数)**不进表**,由池表与等级上限现算 —— 手抄进表就是第二份真相,
  改等级上限或每级点数时满投会悄悄超过表定比例且零报错(评审 2026-09-13)。
- **`AttributeAllocRatio`**(2026-09-13 新增)—— 角色属性点的**加点收益比例**:`dimension_id`(idx,fk)× `class_id`
  (idx,0 = 非对应职业兜底,N = 对应职业)× 六列 double 比例(0.25 = 满投 +25%)。每个维度两行,查找顺序与
  `AttributeAutoPlan` 一样先职业专属再 `class_id=0`。当前 8 行(职业 id 与客户端 `RoleFlowUi` 一致:1 破军 / 2 玄霄 / 3 丹心 / 4 逐风):

  | 维度 | 非对应职业 | 对应职业 |
  |---|---|---|
  | 力量 → 物伤 | +25% | 破军 +30% |
  | 灵力 → 法伤 / 法力 | +22.5% / +15% | 玄霄 +27% / +15% |
  | 敏捷 → 速度 | +16.67% | 逐风 +20% |
  | 体质 → 气血 / 防御 | +20.83% / +10% | 丹心 +25% / +12% |

  **只覆盖角色属性点池(101-104)**。宝宝池没有比例行,继续按 `AttributeDimension` 每点固定加值。
  满投点数按维度所属池现算,池若有单项上限则按上限算。
- **`AttributeAutoPlan`** —— 自动加点方案:`class_id`(idx,0 = 通用兜底)× `pool_id` × `dimension[]` 优先序 + `weight[]`。
  属性点池(pool 1)的方案 2026-09-13 起**全投本职业主属性**:通用档与破军全投力量、玄霄灵力、丹心体质(行 31)、
  逐风敏捷(行 41)。原先的 3:1:1 / 4:1:1 分散投在集中投资公式下比满投少 8~9% 有效点(85 级通用档 +595 物伤 +171 气血,
  而全投力量 +1062.5 物伤),推荐方案不该系统性劣于公式意图。相性 / 仙魔池的方案行 2026-09-14 随两池一起删除。

### 2.3 组件

```proto
message AttributeScheme { uint32 scheme_id; string name; map<uint32,uint32> allocated; }
message PlayerAttributeComp {
  repeated AttributeScheme schemes; uint32 active_scheme_id; uint32 next_scheme_id;
  uint64 last_switch_time;                 // 切换冷却
  map<uint32,uint32> bonus_points;         // pool_id → 等级之外的额外点(道具/任务)
  map<uint32,uint32> bonus_values;         // dimension_id → 外部加成(装备/丹药),不占点、不随方案变
}
```

## 3. 计算口径

### 3.1 一级 → 二级

```
自然部分 = base_per_level × level + bonus_values                      ← 白送的点 + 装备,按每点固定系数
二级属性 = ClassTable 初值(仅 max_health / max_mana / speed)
         + Σ(自然部分 × 该维度的对应系数)                                ← 所有维度
         + Σ 玩家分配点的增量                                           ← 见下
```

**玩家分配点的增量**(2026-09-13 策划公式,只对有 `AttributeAllocRatio` 行的维度,即角色属性点池):

```
有效点数 E(n) = n × (1 + b × n ÷ s)          b = 0.20(表);s = 该维度满投点数,属性点池 = 85 × 5 = 425(现算)
属性增量      = 标准基础属性 × 比例 × E(n) ÷ E(s)     E(s) = s × (1 + b) = 510(现算)
标准基础属性  = 职业初值 + Σ(base_per_level × 85 × 系数)   ← "85 级标准基础属性",按现有表算,不单独配
```

三个要点:
- **满投正好拿满比例**:n = 425 时 E = 510 = d,增量 = 标准基础 × 比例。例如非破军力量满投 = 4250 × 25% = +1062.5 物伤,
  破军 = +1275;玄霄灵力满投 = 3400 × 27% = +918 法伤 + 4200 × 15% = +630 法力。
- **集中投资**:E(n) 是二次的,分散投总收益更低。425 点四项各 106 点,总有效点 ≈ 445.15 而不是 510,少约 12.7%(反过来说集中投多约 14.6%);
  投一半(212 点)只拿 45.7% 的收益。
- **自然成长与装备不走公式**:每级白送的 1 点仍按每点固定系数(85 级体质 85 点 = +4250 气血),`bonus_values` 同理。
  角色强度主体来自等级成长,加点是 +20%~30% 的锦上添花 —— 对比旧口径力量满投 +21250 物伤,新口径是它的 1/20(破军 1/17)。
- 没有比例行的维度保持每点固定加值,代码路径 `AddLinear`(2026-09-14 起角色四个维度都有比例行,该分支只是兜底)。纯规则在
  `attributerules::FullInvestmentPoints / MakeFormulaRule / EffectivePoints / AllocatedIncrement`(满投点数按池表 + 等级上限现算,
  除数 > 0 才做除法);单测 `AllocFormulaTest` 钉住上表全部验收数字,以及"改等级上限后满投仍正好拿满比例"。
- **面板 `value` 只是点数,不能反推收益**:85 级力量 170 = 85 点自然成长(值 +4250 物伤)+ 85 点分配(值 +184)。
  分配点的单点边际:物伤每点约 +2.1、气血约 +1.9、法伤约 +1.5、法力约 +1.24、速度约 +1.08、防御约 +1.0(丹心 +1.2)
  (2026-09-14 速度 ×12、防御 ×12、法力 ×4 之后;之前速度 +0.09、防御 +0.08、法力 +0.31)。二级属性是整数,向下取整。
  **验收口径**(用户 2026-09-14 "按玩家能接受的范围调整数值"):每分配 1 点,这一维涨的**每一项**二级属性都至少 +1 ——
  单测 `EveryAllocatedPointRaisesEachStatByAtLeastOne` 对 1..425 点逐点钉住(含对应职业档)。

`Recalculate` 一次算完六项写进 `DerivedAttributesComp`,并把 `speed` **同步直写**
`BaseAttributesComp.speed` —— 回合引擎的出手序与逃跑判定只读后者,不同步就等于加了敏捷不生效。
`BaseAttributesComp.armor` 同样每次按 `Class.init_armor` 重写(2026-09-14 起):护甲原本只在建号时写一次、随存档落库,
改表后已建的角色(含本地测试号)不会跟着变;重写后以职业表为准。全仓没有运行时改护甲的地方;
以后装备要加护甲,须在 `Recalculate` 里累加,不能直接改 `BaseAttributesComp.armor`。

当前 HP/MP 跟随上限的规则(`PlayerAttributeSystem::RecalcReason`;2026-09-04 评审后收紧):
- **升级**(`kLevelChanged`,`PlayerUpgradeEvent`):上限抬高按绝对增量补当前值(升级手感);降级只夹。
- **加载 / 加点 / 切方案 / 洗点**(其它 reason):**按比例保持** `hp = hp × newMax / oldMax`(活着至少留 1),
  一升一降往返不净得(唯一例外:极低血量因"至少留 1"往返最多回升到 floor(oldMax/newMax),有上界、再往返不再增长,
  见单测 `RescaleRoundTripHealingIsBounded`)。最初版本对所有路径都"抬高补增量、降低只夹",组合起来就是免费无限回血:
  残血 200/3950 → 洗点(30 级以下免费)夹到 200/1100 → 原样加回 → 200+2850;或开个空方案每 60 秒切一次。
  这直接绕过 `turn-based-battle-server.md` D4"残血带出战斗",故切方案 / 洗点 / 重新加点一律不得成为治疗手段。
- **已分配 > 总量的收敛**(`kLoad` / `kLevelChanged` 前置):等级下降(GM / 回档 / 上限下调后读存档压回上限,§2.2)
  或改表缩点后,某方案某池的已分配可能超过总量;整池清零返还并打 WARN,防止低等级号带着高等级面板进战斗快照。
- 死人(health==0)不因加点复活,复活统一走 `player_revive.h` 的规则。

### 3.2 二级属性进战斗

| 二级属性 | 通道 | 公式位置 |
|---|---|---|
| 气血上限 | `DerivedAttributesComp.max_health` → `BattlePlayerSnapshot.max_health` | 引擎 `ApplyHeal` 封顶、scene 结算二次夹 |
| 法力上限 | `.max_mana` → `BattlePlayerSnapshot.max_mana` | 同上(此前全仓无蓝上限,靠本系统补齐) |
| 物伤 | `.physical_attack` → 快照 13 → `BattleActorState.physical_attack`(18) | 普攻,以及物理技能(`Skill.damage_type = 1`) |
| 法伤 | `.magic_attack` → 快照 14 → `BattleActorState`(19) | 法术技能(`Skill.damage_type = 0`,缺省);实时战斗 `skill.cpp` 同口径 |
| 防御 | `.defense` → 快照 15 → `BattleActorState`(20) | 与护甲合并按比例减伤(`combat_damage_rules.h`,回合 / 实时共用) |
| 速度 | 直写 `BaseAttributesComp.speed` | 引擎出手序 / 逃跑 |

伤害公式(2026-09-13 起回合引擎与实时技能共用 `cpp/libs/services/battle/system/combat_damage_rules.h`):

```
原始伤害 = base × (1 + strength × 0.1) + 攻击 × 攻击倍率
          攻击:普攻取物伤;技能按 Skill.damage_type 取物伤 / 法伤。攻击倍率 = Skill.attack_multiplier(0 按 1 倍,普攻恒为 1)
受伤比例 = max(0.4, 等级系数 ÷ (护甲 + 防御 + 等级系数) × (1 − 抗性%)),等级系数 = 360 + 120 × 目标等级(夹到 1..85;2026-09-14 防御单位 ×12,原 30 + 10 × 等级)
伤害     = 原始伤害 × 受伤比例
          回合制非 PVE 对局再 × kPvpDamageScale(0.3,2026-09-14 由 0.2 重标定);然后 critchance/100 概率 ×2;DEFEND 再减半;向上取整、不超过当前气血
```

改前是"攻击减防御"(`base × (1 + strength × 0.1) + attackBonus − armor − defense`,再乘抗性):防御一旦超过攻击,伤害直接归零,
2026-09-10 系数上调后低级怪全体打不动人。2026-09-13 按用户批准的方案改为比例减伤:防御收益递减、常驻减伤封顶 60%,
任何命中至少打出四成。怪物没有防御,等级取参战玩家最高等级;实时战斗没有 PVP 对局概念,不乘 PVP 系数;
毒 / 灼烧等周期伤害是表里的固定值,两者都不乘。单测:`combat_damage_rules_test.cpp`(纯规则)与 `turn_battle_engine_test` 的
`DerivedDefenseReducesIncomingDamageProportionally` / `PvpDirectDamageIsScaled` / `PhysicalSkillUsesPhysicalAttackWithMultiplier`。

## 4. 协议(`proto/scene/player_attribute.proto`,service `SceneAttributeClientPlayer`,消息号 167-175)

| rpc | 语义 |
|---|---|
| `GetAttributePanel` | 拉全量面板 |
| `AllocateAttributePoints` | **确认加点**:提交某池的"目标已分配值"(全量、幂等) |
| `ResetAttributePoints` | 整池洗点,按表扣金币 |
| `AutoAllocateAttributePoints` | 算推荐分配,**只算不落** |
| `CreateAttributeScheme` / `SwitchAttributeScheme` / `RenameAttributeScheme` | 方案管理 |
| `NotifyAttributePanelChanged` | 服务器主动推面板(升级 / GM / 外部加成变化) |
| `GmSetPlayerLevel` | GM 设等级(开发用,上线前经 gate GM 鉴权收口) |

### 4.1 三条协议纪律

1. **面板全量下发,客户端零配表**:维度名、悬浮说明、单项上限、剩余点、二级属性全部在
   `AttributePanelInfo` 里。策划改表不需要发客户端版本。
2. **写操作一律回全量面板**,客户端整体覆盖本地状态,不做增量合并 —— 消除"客户端算的剩余点
   和服务器不一致"这类最常见的加点 bug。
3. **`Allocate` 是目标值语义,不是增量**:重发同一请求不会重复扣点(幂等)。这让"客户端连点"
   和"网络重试"都天然安全。

## 5. 校验与不变量

纯规则收敛在 `attribute_allocation_rules.h`(零 ECS / 零表 / 零 proto,可单测):

1. **池必须已解锁**(`level >= unlock_level`);
2. **只增不减**:目标值低于当前已分配即拒(`kAttributePointsCannotDecrease`)——
   减点唯一通道是「重置」,否则玩家可以靠"加了再减"绕过洗点收费;
3. **单维度不超 `dimension_cap`**(当前角色与宝宝池都是 0 = 不限,规则保留);
4. **增量总和 ≤ 剩余点**(64 位累加:目标值来自客户端、单项可达 `UINT32_MAX`,外挂发的负数在 uint32 字段里
   也会绕成超大正数;32 位累加时 `UINT32_MAX + 10` 绕成 9,会骗过本条);
5. **零增量拒绝**(`kAttributeNothingToChange`),避免空请求当成功回全量面板。

跨系统不变量(与既有纪律对齐):

- **跨 zone 冻结期间(`PlayerFrozenComp`)拒绝一切写** —— 源 zone 的写不会进目的 zone 的快照,
  写了就是数据分叉(`cross-zone-readiness-audit.md` §11.1);
- **战斗在途(`InBattleComp`)拒绝一切写** —— `BattlePlayerSnapshot` 已出,改属性会让局内数值与面板分叉;
- **表里已删除的维度会在加载时静默清理**(`SanitizeSchemes`),点数自动返还,老存档自愈;
- 洗点 / 开新方案的金币**必须走 `CurrencySystem`**(补缴与封禁钩子都在里面),禁止直写 `CurrencyComp`;
  先扣费成功再改数据,扣费失败什么都不动。

错误码:`data/tip/Tip.xlsx` 的 `//attribute_error` 组(25000-25014,base=25000),生成到
`table/proto/tip/attribute_error_tip.pb.h`。

## 6. 客户端(Unity uGUI)

- `Assets/Scripts/Game/Attribute/AttributeClient.cs` —— 网络状态机,复用 `IBattleTransport` 测试缝;
  `Busy` 单飞写请求;断线清面板。
- `Assets/Scripts/UI/Ugui/Attribute/` —— `AttributeUiRoot`(自有 Canvas,sortingOrder 160)、
  `AttributePanel`(两栏窗)、`AttributeUiWidgets`(**仓库第一个交互 `Slider`**)、`AttributeUiStyle`。
- 交互约束与服务端"只增不减"对齐:滑条下界 = 服务器已确认值,上界 = 已确认值 + 剩余点(再夹 cap);
  本地只保留"待提交增量",点「确认」才发;有未提交增量时切页签 / 切方案会被拦。
- 「重置」是两段语义:先撤销本地未提交增量(免费),没有未提交增量时才真发洗点请求(扣金币)。

## 7. 验证

- **引擎单测**:`turn_battle_engine_test` 57/57(2026-09-03 落地时的数字,之后持续增加),其中属性线 3 条 ——
  物伤只进普攻、法伤只进技能、防御加法减伤且 defense=0 时与老公式逐字节一致。
- **纯规则**:`attribute_allocation_rules.h` 与 `player_level_rules.h` 的单测在
  `turn_battle_engine_test/attribute_allocation_rules_test.cpp`(2026-09-10 补;此前加点规则头文件没有任何用例)。
  覆盖客户端坏数据(uint32 绕回的负数、多项超大值 32 位求和溢出、塞非本池维度)、只增不减 / 跨维度挪点、
  剩余点与单项上限边界、幂等、解锁、85 级满级总量与总量饱和、等级合法范围与存档等级压回上限、
  自动加点逐维分配(权重比例 / 余数按优先序补齐 / 权重 0 跳过)且建议可原样通过校验、HP/MP 按比例往返不回血
  (极低血量有界例外)。已用点 `UsedPoints`(角色 / 宝宝各一份,64 位累加后饱和)在匿名命名空间里,暂无单测。
- **每点可见与单位放大**(2026-09-14):`AllocFormulaTest.EveryAllocatedPointRaisesEachStatByAtLeastOne`(1..425 点逐点);
  `CombatDamageRulesTest.DefenseUnitScaleKeepsReceivedRatio`(护甲 / 防御 / 等级系数同乘 12 后受伤比例不变)。
- **端到端**:`robot/robot.exe -c etc/attribute_smoke.yaml`(`attribute_smoke_scenario.go`,10 步):预备(等级归 1 /
  属性点池洗点 / 切回首方案 / GM 发币,同账号可反复跑)→ 面板形状 → 1→30 级属性点恰好 +145 → 自动加点只算不落 →
  确认后剩余归零、二级属性变大 → 幂等 / 只增不减(2026-09-14 删相性超上限 / 仙魔未解锁两步,规则由纯规则单测覆盖)→ 开方案精确扣金币、
  新方案干净、跨 60s 冷却切回不串档 → **下线重登后等级 / 方案 / 已分配 / 二级属性原样恢复**(落库往返 + db 列)→
  30 级洗点精确扣金币、全额返还。通过打 `ATTRIBUTE_SMOKE_OK`。
- **客户端**:Roslyn 离线编译 + 副本工程 EditMode(`AttributeClientTests` 11 条,含响应体 tip 拒绝路径)。

## 8. 已知缺口

- **经验系统未接**:等级只能靠 `GmSetPlayerLevel` 改;战斗引擎已产出 `exp_gain`,落地待经验系统。
- **`class_id` 下发 scene 已打通(2026-09-13,commit f5983bd86)**:login 入场前 `backfillPlayerClass`(`player_class_backfill.go`)
  把账号角色记录里的职业补进 `PlayerAllData.uint32_pb_component.class`,scene 加载后 `ResolveClassRow` / `AutoAllocate` /
  `FindAllocRatio` 按真实职业取行,破军全投力量拿 +30%。账号记录里职业本身为 0 的老号不猜测职业,仍走 `class_id=0` 兜底档。
  **尚未实测验证**(随本轮其它改动一起待 Codex 冒烟)。
- **`bonus_values` / `bonus_points` 无写入方**:等装备、丹药、任务奖励系统。
- **GM 鉴权**:`GmSetPlayerLevel` 目前与 `GmAddCurrency` 同口径(开发期直连),上线前须并入
  gate 的 GM 鉴权白名单(`gate_security.h`)。降级路径已有"已分配 > 总量整池清零"的收敛兜底。
- **数值平衡(2026-09-14 最终口径)**。用户 09-13 / 09-14 先后拍板:加点改百分比 + 集中投资公式、伤害改比例减伤、
  加 PVP 伤害系数、删相性点 / 仙魔点、速度单位 ×12、防御单位 ×12 / 法力单位 ×4、怪物按新口径重定、"按玩家能接受的范围调整数值"。
  推算口径 = §3.1 加点公式 + §3.2 比例减伤 + 通用自动加点(属性点全投力量)+ 非对应职业档(保守基准:职业已随登录下发,对应职业全投主属性会比推算略强,怪物死得略快),出手序 / 暴击 / 随机目标按引擎语义
  各跑 3000 场模拟,**未经真实战斗验证**。
  - **怪物**(`Monster.xlsx` 1-16 的 health / strength / speed;护甲随防御单位 ×12 同乘、减伤比例不变;抗性 / 暴击 / 奖励不变),三档目标沿用 09-11 / 09-13:
    教学怪 1、2 号参照 1 级没加点的号 5 下打死、每下约 3% 气血;普通怪 3-15 号参照同级通用号 3 下、约 6%、速度 0.9 倍
    (参照等级 4 / 6 / 8 / 10 / 12 / 15 / 18 / 21 / 25 / 28 / 33 / 38 / 45);首领 16 号参照 30 级通用号 12 下(3 人队)、约 12%、速度持平。
    09-13 那版(另一会话)的参照号物伤里含相性土相(30 级 +1800),相性删掉后普通怪血量约减半;力量不变(相性没加气血 / 防御);
    速度随单位 ×12 重算。新值(血 / 力量 / 速度):
    1 号 370/14/240、2 号 360/14/252、3 号 760/53/346、4 号 1050/63/410、5 号 1380/73/475、6 号 1620/82/540、
    7 号 1920/92/605、8 号 2250/106/702、9 号 2690/121/799、10 号 2940/135/896、11 号 3500/154/1026、
    12 号 3670/168/1123、13 号 4340/192/1285、14 号 4670/216/1447、15 号 5550/249/1674、16 号 12130/365/1320。
  - **副本**(胜率 / 回合中位数 / 胜后剩余气血中位数):1 级没加点单人副本 1 = 100% / 9 / 57%(robot battle_smoke 场景,
    远低于 120 秒超时);10~12 级通用单人副本 2 = 100% / 6~7 / 30%~54%;25~28 级通用 3 人副本 3 = 100% / 6~8 / 49%~60%;
    30 级通用单人副本 3 = 0%(预期:首领要组队);10 级没加点单人副本 2 = 77%。
  - **PVP**:`kPvpDamageScale` 0.2 → 0.3。0.2 是按含相性的伤害标的,相性删掉后同级通用 1v1 在 7 / 10 / 30 / 60 / 85 级
    要 15 / 13 / 9 / 8 / 7 下,节奏慢了近一倍;0.3 时为 10 / 8 / 6 / 5 / 5 下(全暴击 5 / 5 / 3 / 3 / 3),
    贴近 09-13 定的"30 级以上约 5 下、全暴击 3~4 下"。1 级 PVP 要 29 下(全暴击 15),低等级 PVP 本就少见,不单独调。
  - **防御 ×12 / 法力 ×4**(同日跟进,§2.2):护甲、防御、等级系数同乘 12,受伤比例逐位不变,上面的怪物血量 / 力量、
    副本胜率与 PVP 下数全部照旧;法力上限与技能耗蓝同乘 4,续航不变。怪物 `armor` 列 3~35 → 36~420。
- **`bonus_points` 一旦有写入方**,角色属性点总量能超过 425,单维度 n 越过满投点数后 E(n) 超线性,收益会超过表定比例。
  目前没有写入方,不做截断;接道具 / 任务发额外点时须决定是否把 n 截到满投点数。
- **数值平衡(2026-09-11 旧口径,保留作对照)**。2026-09-10 角色系数上调后曾出现"怪物打不出伤害 / 低级副本一两回合结束 / 其它池相对价值倒挂";
  2026-09-11 按用户选定处理:其它池同比上调(§2.2),怪物重定(下文)。以下数字均为静态推算(口径 =
  `CalculateFinalDamage` + `Recalculate` + 通用自动加点,不算暴击),**未经战斗实测**:
  ① **怪物重定**(`Monster.xlsx` 1-16 的 health / strength / speed;护甲、抗性、暴击、奖励不变)。设计目标:
     - 新手教学怪(副本 1 的 1、2 号):参照 1 级**没加点**的号,5 下打死,每下掉对方约 3% 气血;
     - 普通怪(3-15 号):参照同级**通用自动加点**的号,3 下打死,每下掉对方约 6% 气血,速度约为对方的 0.9 倍;
     - 首领 16 号(副本 3):参照 30 级通用号,12 下打死(按 3 人队),每下约 12% 气血,速度与对方持平。
     参照等级:1、2 号 1 级;3-15 号依次 4 / 6 / 8 / 10 / 12 / 15 / 18 / 21 / 25 / 28 / 33 / 38 / 45 级;16 号 30 级。
     换算:血量 = 次数 × 玩家普攻伤害;力量 = 目标伤害 ÷ 0.95 + 玩家防御(玩家护甲 10 与普攻基础伤害 10 抵消)。
     参照等级是本次的假设,改它就按同一换算重算这三列。
  ② 推算结果:1 级没加点单人副本 1 约 10 回合胜、剩约 60% 血(robot battle_smoke 的 robot_9001 就是这个场景);
     1 级加点 4 回合;10~12 级通用单人副本 2 约 7 回合、剩 30%~55%;3 人队 25~28 级副本 3 约 7~8 回合、剩 45%~60%;
     单人 30 级打副本 3 会输;10 级完全不加点打副本 2 会输。越级刷低级副本仍然无伤(防御超过怪物力量即归零,属常规设计)。
  ③ **PVP 仍是同级首击秒杀,且更严重**:土相同比上调后物伤更高,同级通用 1v1 在 7 / 30 / 85 级都是一下打死
     (30 级一击约 6450 对 3250 气血),同速按 player_id 升序先手。怪物表管不到 PVP,需要引擎加 PVP 伤害规则,待决。
  ④ 技能表占位值:`Skill.xlsx` 的 damage 仍是 `100*level` / `1000*level` / `10000*level`,职业 1 的 13 号技能
     (`10000*level`,冷却 5 回合)手动释放可秒杀首领;自动战斗只普攻,不受影响。属技能表待定项,与本次改动无关。
- **改系数后的存量角色**:`DerivedAttributesComp` 不落库,改表后首次登录走 kLoad(旧上限未知,只夹不补),存活角色 HP
  不随新上限抬高(例:30 级通用方案满血 2150,新上限 3250 后仍 2150),MP 被截到新上限(1300 → 750)。这是"改属性
  不当治疗"护栏的预期副作用;要满状态上线需一次性 GM 回满或数据迁移。
  2026-09-14 删相性 / 仙魔后同理:老号首登少了这两池的加成,上限下降,HP/MP 被夹到新上限;分配到 201-205 / 301-304 的点
  由 `SanitizeSchemes` 清掉,不返还(池已不存在)。落库的 `BaseAttributesComp.speed` 是旧单位,登录时 `Recalculate` 按新单位覆盖。
  防御 ×12 / 法力 ×4:项目未上线、没有老存档,不做迁移。本地测试号登录时护甲按职业表重写;当前法力只夹不补,
  会偏低但可用(阵亡复活或重建号即回满)。
- **滚动发布窗口**:`AttributeDimension` / `AttributeAllocRatio` / `AttributeRule` 都不在 `BattleTableFingerprint` 里
  (只算 skill / buff / cooldown / skillpermission / dungeon / monster),跨 zone PVP 的快照由各 zone 用本地表和本地 scene
  二进制算。2026-09-13 起分叉已不只是表值:旧 scene 根本不读比例表,按(自然 + 分配)× 每点系数算。新旧镜像的 zone
  同时开放匹配时,同一个 85 级力量满投的号,旧 zone 快照物伤 = (85 + 425) × 50 = 25500,新 zone ≈ 4250 + 1062.5 = 5312,
  差 4.8 倍且没有任何告警——这次发布须先全量替换 scene 二进制与这三张表,再开跨 zone 匹配。
  2026-09-14 速度单位 ×12 同理:`Class` / `AttributeDimension` / `Pet` / `Monster` 四张表与 battle(`kMonsterDefaultSpeed` /
  `kFleeSpeedFactor` / `kPvpDamageScale`)、scene(`kFallbackBattleSpeed`)两个二进制必须同一批替换;混跑时新旧单位的单位
  同场出手,旧单位一方几乎永远后手。发布前清掉在途战斗房间(快照是旧单位)。
  防御 ×12 / 法力 ×4 再加 `Skill` 表与两个二进制里的 `combat_damage_rules.h`
  (battle 回合引擎与 scene 实时技能都编进它)、`kMonsterDefaultArmor`:旧二进制配新表时等级系数仍是 30 + 10 × 等级、
  护甲却是 120、防御按新系数 60 / 点,10 级玩家常驻减伤(护甲 + 防御 + 抗性 5%)从约 35% 顶到 60% 封顶(不封顶约 85%);
  新二进制配旧表则反过来,护甲与防御一起几乎失效,减伤从约 35% 掉到约 8.5%。表与二进制必须同一批替换。
- **tip 文案**:服务端只下发裸编号;客户端 `AttributeClient.DescribeTip` 镜像了 `Tip.xlsx` 的 attribute_error 组
  (25000-25014,base=25000)做中文映射,改表要同步。全仓统一的 tip 文案下发机制仍是缺口。
