# 角色属性加点系统(问道式三池)

> 落地日期:2026-09-03。实现分布:`proto/common/component/player_attribute_comp.proto`、
> `proto/scene/player_attribute.proto`、`cpp/libs/services/scene/player/system/player_attribute.{h,cpp}`、
> `cpp/libs/services/scene/player/system/attribute_allocation_rules.h`、
> `cpp/nodes/scene/handler/rpc/player/player_attribute_handler.cpp`(守护段)、
> 策划表 `data/AttributePool.xlsx` / `AttributeDimension.xlsx` / `AttributeRule.xlsx` / `AttributeAutoPlan.xlsx`。
> 客户端见 `client/unity` 的 `Assets/Scripts/Game/Attribute/` 与 `Assets/Scripts/UI/Ugui/Attribute/`。

## 1. 目标与非目标

**目标**:玩家按等级获得三类点数(属性点 / 相性点 / 仙魔点),分配到若干"一级属性维度"上,
由服务器换算成六项"二级属性"(气血 / 法力 / 物伤 / 法伤 / 速度 / 防御),并真正参与战斗结算。
支持多套加点方案(切换 / 新开 / 改名)、自动加点推荐、按池洗点。

**非目标(一期不做)**:
- 经验与升级曲线(本系统只消费 `LevelComp.level`,不产出等级;升级由未来的经验系统触发 `PlayerUpgradeEvent`,本系统只监听);
- 洗点道具 / 分池部分洗点(只做整池重置 + 金币);
- 装备 / 丹药写入 `bonus_values` 的具体来源(字段与算法已就位,写入方待背包/装备系统);
- 相性(五行)之间的相生相克战斗修正 —— 相性点当前只贡献二级属性,元素克制留给技能系统。

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
  `reset_cost_gold` / `reset_free_below_level`。当前四行:属性点(1 级解锁,每级 5 点)、相性点(1 级,每级 1 点,单项上限 50)、
  仙魔点(60 级解锁,每级 1 点),以及宝宝属性点(行 4,`owner_type=1`,见 `player-pet.md`)。
- **`AttributeDimension`** —— 维度 + **系数矩阵**:`pool_id`(idx)、`name` / `desc`(直接下发客户端做面板文案与悬浮说明)、
  `sort`、`base_per_level`(不占点的自然成长)、以及六列 double 系数 `max_health / max_mana / physical_attack /
  magic_attack / speed / defense`。**加一个新属性维度 = 加一行表,不改任何代码。**
  角色属性点当前系数(2026-09-10 策划定):体质每点 +50 气血上限、+5 防御;灵力每点 +40 法伤、+10 法力上限;
  力量每点 +50 物伤;敏捷每点 +3 速度。相性、仙魔、宝宝池(401-404)
  2026-09-11 同比上调:各列乘以角色属性点该列的涨幅(气血 ×5/3、法力 ×1/2、物伤 ×10、法伤 ×40/3、速度 ×1、
  防御 ×5/2),保持它们相对属性点的原倍率 —— 金相 53 法伤、木相 67 血 + 10 蓝、水相 12.5 防、火相 4 速、土相 60 物伤;
  仙攻 160 法伤、仙护 20 防、魔攻 120 物伤、魔体 167 血;宝宝 42 血 + 3.75 防 / 7.5 蓝 + 33 法伤 / 40 物伤 / 2.5 速。
  以后再改角色属性点系数,这三组要按同一规则一起换算。
  `desc` 只写定性说明、不写系数数字:面板不下发系数,文案里写数字就成了第二份真相,改系数时极易漏改。
- **角色等级上限 85**(`player_level_rules.h` 的 `playerlevel::kMaxLevel`,`PlayerAttributeSystem::kMaxLevel` 是它的别名;
  2026-09-10 由 200 改为 85)。经验表未落地前是代码常量。`GmSetPlayerLevel` 越界即 `kInvalidParameter`;
  读存档(登录 / 跨 zone 落地 / 回档都走 `PlayerDatabaseMessageFieldsUnmarshal`)时超限等级压回 85 并打 WARN,
  随后 §3.1 的"已分配 > 总量"收敛整池返还多出的点。按此口径属性点满级总量 = 85 × 5 = 425。
- **`AttributeRule`** —— 单行全局规则:方案数上限 / 免费方案数 / 开新方案金币 / 切换冷却秒 / 方案名长度。
- **`AttributeAutoPlan`** —— 自动加点方案:`class_id`(idx,0 = 通用兜底)× `pool_id` × `dimension[]` 优先序 + `weight[]`。

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
维度值(dimension_value) = base_per_level × level + allocated(当前方案) + bonus_values
二级属性 = ClassTable 初值(仅 max_health / max_mana / speed) + Σ(维度值 × 该维度的对应系数)
```

`Recalculate` 一次算完六项写进 `DerivedAttributesComp`,并把 `speed` **同步直写**
`BaseAttributesComp.speed` —— 回合引擎的出手序与逃跑判定只读后者,不同步就等于加了敏捷不生效。

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
| 物伤 | `.physical_attack` → 快照 13 → `BattleActorState.physical_attack`(18) | 引擎 `ExecuteAttack`(**普攻**) |
| 法伤 | `.magic_attack` → 快照 14 → `BattleActorState`(19) | 引擎 `ExecuteSkill`(**技能**);实时战斗 `skill.cpp` 同口径 |
| 防御 | `.defense` → 快照 15 → `BattleActorState`(20) | 双侧 `CalculateFinalDamage` 减法项 |
| 速度 | 直写 `BaseAttributesComp.speed` | 引擎出手序 / 逃跑 |

统一后的伤害公式(回合引擎与实时战斗**逐字节镜像**):

```
final = base × (1 + strength × 0.1) + attackBonus - armor - defense
final = final × (1 - resistance × 0.01)
crit(critchance/100) → ×2,饱和到 0
```

`attackBonus` 普攻取物伤、技能取法伤。**怪物与老存档这三项都是 0,公式退化为改动前的老口径**
(引擎单测 `DerivedDefenseReducesIncomingDamageAdditively` 的 defense=0 分支就是这条兼容性断言)。

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
3. **单维度不超 `dimension_cap`**(相性 50);
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
- **端到端**:`robot/robot.exe -c etc/attribute_smoke.yaml`(`attribute_smoke_scenario.go`,12 步):预备(等级归 1 /
  两池洗点 / 切回首方案 / GM 发币,同账号可反复跑)→ 面板形状 → 1→30 级属性点恰好 +145 → 自动加点只算不落 →
  确认后剩余归零、二级属性变大 → 幂等(144)/ 只增不减(134)/ 相性超上限(135)/ 未解锁池(131)→ 开方案精确扣金币、
  新方案干净、跨 60s 冷却切回不串档 → **下线重登后等级 / 方案 / 已分配 / 二级属性原样恢复**(落库往返 + db 列)→
  30 级洗点精确扣金币、全额返还。通过打 `ATTRIBUTE_SMOKE_OK`。
- **客户端**:Roslyn 离线编译 + 副本工程 EditMode(`AttributeClientTests` 11 条,含响应体 tip 拒绝路径)。

## 8. 已知缺口

- **经验系统未接**:等级只能靠 `GmSetPlayerLevel` 改;战斗引擎已产出 `exp_gain`,落地待经验系统。
- **`class_id` 未随 `PlayerAllData` 下发 scene**:`ResolveClassRow` 与 `AutoAllocate` 的职业分支已写好,
  但当前全职业取 `ClassTable` 首行 / `AttributeAutoPlan` 的 `class_id=0` 兜底行。
- **`bonus_values` / `bonus_points` 无写入方**:等装备、丹药、任务奖励系统。
- **相性未参与元素克制**:五行目前只是六项二级属性的另一组系数。
- **GM 鉴权**:`GmSetPlayerLevel` 目前与 `GmAddCurrency` 同口径(开发期直连),上线前须并入
  gate 的 GM 鉴权白名单(`gate_security.h`)。降级路径已有"已分配 > 总量整池清零"的收敛兜底。
- **数值平衡**。2026-09-10 角色系数上调后曾出现"怪物打不出伤害 / 低级副本一两回合结束 / 其它池相对价值倒挂";
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
- **滚动发布窗口**:`AttributeDimension` 不在 `BattleTableFingerprint` 里(只算 skill / buff / cooldown / skillpermission /
  dungeon / monster),跨 zone PVP 的快照由各 zone 用本地表算。新旧镜像的 zone 同时开放匹配时,同配置双方的力量单点物伤
  会是 50 对 5,且没有任何告警——改这张表的发布须先全量替换 scene,再开跨 zone 匹配。
- **tip 文案**:服务端只下发裸编号;客户端 `AttributeClient.DescribeTip` 镜像了 `Tip.xlsx` 的 attribute_error 组
  (25000-25014,base=25000)做中文映射,改表要同步。全仓统一的 tip 文案下发机制仍是缺口。
