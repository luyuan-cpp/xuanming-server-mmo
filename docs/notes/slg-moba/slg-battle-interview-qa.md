# SLG 沙盘战斗系统面试 Q&A（率土之滨风格）

**Created:** 2026-04-07

---

## Q1: SLG 战斗和 MMORPG 战斗的本质区别是什么？

**A:**

| 维度 | SLG 战斗 | MMORPG 战斗 |
|------|---------|------------|
| 操作模型 | 自动战斗，玩家只决策阵容 | 实时操作技能/走位 |
| 计算时机 | 行军到达时瞬算/帧推演 | 每帧实时 tick |
| 确定性 | **必须确定性** | 可以有随机性 |
| 结果交付 | 战报（事后回放） | 实时同步 |
| 并发规模 | 数千场/秒（大规模战争） | 数十人副本 |
| 核心数值 | 兵力 × 属性 × 克制 × 策略 | DPS/HPS/仇恨/技能循环 |

**核心差异：** SLG 战斗是"预计算 + 回放"，不是实时交互。确定性是基石，否则战报无法验证。

---

## Q2: 什么是确定性战斗引擎？为什么 SLG 必须确定性？

**A:**

### 定义
给定相同输入（攻守双方数据 + 种子 + 地形），输出完全一致的结果（每步动作、每点伤害、最终胜负）。

### 为什么必须确定性
1. **战报验证**：服务器算完发战报给双方，如果结果不确定，同一场战斗两次结果不同 → 争议
2. **录像回放**：客户端根据战报播放动画，必须和服务器计算一致
3. **GM 仲裁**：玩家投诉时，GM 重跑 SimulateBattle 必须得到相同结果
4. **反作弊**：确定性 = 可验证 = 不可篡改

### 确定性保证的约束

```
纯函数签名:
BattleReport SimulateBattle(BattleInput input)

BattleInput = {
    ArmySnapshot attacker,      // 武将+兵力+装备+技能+buff 完整快照
    ArmySnapshot defender,
    uint32_t seed,              // 随机种子
    TerrainType terrain,        // 地形加成
    WeatherType weather         // 天气加成（可选）
}
```

### 确定性约束清单

| 约束 | 原因 | 实现 |
|------|------|------|
| 禁用浮点 | 不同平台/编译器浮点结果可能不同 | 整数百分比：`atk * 120 / 100` |
| 单一 PRNG | 多个随机源 → 调用顺序不确定 | 一个 `xoshiro256` 实例，seed 唯一 |
| 固定迭代顺序 | 哈希表遍历顺序不确定 | 用 `vector` + 固定排序/slot 顺序 |
| 无外部依赖 | 系统时间、内存地址 | 纯函数，只依赖 BattleInput |
| 固定精度除法 | `7 * 100 / 3 = 233` 还是 `234`? | 统一截断（向下取整），不用四舍五入 |

---

## Q3: 伤害公式怎么设计？整数数值体系如何保证？

**A:**

### 基础伤害公式
```
base_damage = attacker.atk * troop_count * skill_multiplier / 10000
defense_reduction = base_damage * defender.def / (defender.def + K)
terrain_modifier = terrain_bonus[attacker.terrain]  // 整数百分比，如 110 = 10% 加成
final_damage = (base_damage - defense_reduction) * terrain_modifier / 100
```

### 整数化规则

**核心：所有中间值都是整数，乘法在前，除法在后，最后一步截断。**

```cpp
// BAD: 浮点
float damage = atk * 1.2f * crit_multi;

// GOOD: 整数百分比
int32_t damage = atk * 120 / 100;  // 120 = 1.2 的整数表示

// BAD: 连续除法精度损失
int d = (a / 100) * (b / 100);  // 两次截断

// GOOD: 先乘后除
int d = a * b / 10000;  // 一次截断
```

### 万分比系统
```
100% = 10000
50%  = 5000
1.5% = 150
0.1% = 10

// 技能倍率
skill_multiplier = 12000;  // 120% 伤害
damage = base_atk * troop * skill_multiplier / 10000;
```

### 溢出保护
```cpp
// 大数值相乘可能溢出 int32
// 用 int64 中间值
int64_t temp = (int64_t)atk * troop_count * skill_multiplier;
int32_t damage = (int32_t)(temp / 10000);
```

---

## Q4: 暴击/闪避/反击这些随机事件如何确定性实现？

**A:**

### 单一 PRNG + 固定调用顺序

```cpp
class BattleRNG {
    uint64_t state;  // xoshiro256 state
public:
    explicit BattleRNG(uint32_t seed) { state = splitmix64(seed); }
    
    // 返回 [0, 10000) 的整数，代表万分比
    int32_t next_percent() {
        return next() % 10000;
    }
    
    // 判断是否触发（rate 是万分比）
    bool check(int32_t rate) {
        return next_percent() < rate;
    }
};
```

### 战斗循环中的调用顺序
```cpp
void process_attack(BattleRNG& rng, Unit& attacker, Unit& defender) {
    // 1. 先判闪避（防守方）
    if (rng.check(defender.dodge_rate)) {
        record_action(DODGE, attacker, defender);
        return;
    }
    
    // 2. 再判暴击（攻击方）
    bool is_crit = rng.check(attacker.crit_rate);
    
    // 3. 算伤害
    int32_t damage = calc_damage(attacker, defender);
    if (is_crit) damage = damage * attacker.crit_multi / 10000;
    
    // 4. 判反击（防守方）
    bool counter = rng.check(defender.counter_rate);
    
    // 5. 记录
    record_action(is_crit ? CRIT_ATTACK : NORMAL_ATTACK, attacker, defender, damage);
    
    if (counter) {
        process_counter_attack(rng, defender, attacker);
    }
}
```

**关键：** rng 调用顺序绝对固定。不能因为 if 分支跳过了 rng 调用（否则后续所有随机结果都会偏移）。

### 保底机制（伪随机）
```cpp
// 连续 N 次未暴击，第 N+1 次必定暴击
// 用累积概率而非独立随机
struct PseudoRandom {
    int32_t base_rate;   // 基础暴击率
    int32_t accumulated; // 累积值
    
    bool check(BattleRNG& rng) {
        accumulated += base_rate;
        if (rng.next_percent() < accumulated) {
            accumulated = 0;
            return true;
        }
        return false;
    }
};
```

---

## Q5: 战报（BattleReport）怎么设计？为什么选 Event-Log 而不是 Input-Resimulation？

**A:**

### 战报结构（Protobuf）
```protobuf
message BattleReport {
    BattleInput input = 1;       // 完整输入快照（可用于重跑验证）
    BattleResult result = 2;     // 胜负、损兵、经验
    repeated Round rounds = 3;   // 回合序列
}

message Round {
    int32 round_number = 1;
    repeated BattleAction actions = 2;
}

message BattleAction {
    ActionType type = 1;         // ATTACK/SKILL/BUFF/HEAL/DEATH
    int32 source_slot = 2;       // 攻击方武将槽位
    int32 target_slot = 3;       // 被攻击方武将槽位  
    int32 skill_id = 4;          // 技能 ID（普攻=0）
    int32 damage = 5;            // 伤害/治疗量
    int32 hp_after = 6;          // 目标剩余 HP
    int32 troop_after = 7;       // 目标剩余兵力
    bool is_crit = 8;            // 暴击标记
    bool is_dodge = 9;           // 闪避标记
    repeated BuffEvent buffs = 10; // 附带的 buff 变化
}
```

### 包含 hp_after 的好处
- 客户端**不需要计算**，直接显示 hp_after
- 任何错误只在服务端修复，客户端永远正确
- 支持"跳到任意回合"（不需要重算前面所有回合）

### Event-Log vs Input-Resimulation

| 维度 | Event-Log（推荐） | Input-Resimulation |
|------|-------------------|-------------------|
| 战报内容 | 完整动作序列 | 只存 BattleInput + seed |
| 大小 | ~1-5 KB | ~200 bytes |
| 客户端要求 | 动画播放器 | 必须内嵌完整战斗逻辑 |
| 跨版本兼容 | 旧战报永远可回放 | 逻辑变更 → 旧 seed 跑出不同结果 |
| 安全性 | 客户端无战斗逻辑 | 客户端有完整逻辑可逆向 |
| 存储 | 100K 玩家 × 100 条 → ~50GB | ~2GB |

**选 Event-Log 因为：**
1. 联盟战争历史需长期保存，跨版本必须可回放
2. 客户端薄化，不暴露战斗逻辑
3. 1-5KB 的存储代价完全可接受（protobuf + zstd → ~500B-2KB）

---

## Q6: 多军团攻城（集结战）怎么设计？

**A:**

### 业务流程
```
发起集结号令 → 盟友响应加入 → 集结倒计时 → 多军齐发 → 到达后联合攻城
```

### 技术实现

#### 集结阶段
```
1. 发起者创建 Rally 对象: {rally_id, target_tile, expire_time, max_slots=8}
2. 盟友加入: rally.add_army(player_id, army_snapshot)
3. 倒计时结束或满员 → 触发出发
```

#### 行军阶段
```
// 方案A: 统一行军（推荐）
// 所有参与者的军队合并为一个逻辑行军
// 速度 = min(all_army_speed) 或 集结速度统一值
march = create_rally_march(rally, target, speed=min_speed)

// 方案B: 分别行军，在目标格汇合
// 各自寻路各自走，到达后等待最慢的
// 更灵活但更复杂
```

#### 战斗阶段（核心）
```
AttackRally vs DefenderCity:
    attackers = [army_1, army_2, ..., army_8]  // 多个玩家军队
    defender = city.garrison + city.defense_bonus

// 方案1: 逐波攻击
for (auto& atk_army : attackers) {
    report = SimulateBattle(atk_army, defender_remaining, seed++, terrain);
    apply_casualties(atk_army, defender_remaining, report);
    if (defender_remaining.is_dead()) break;  // 城破
}

// 方案2: 联合编组（更公平）
combined_army = merge_armies(attackers);  // 按武将槽位排列
report = SimulateBattle(combined_army, defender, seed, terrain);
// 战损按各玩家贡献比例分摊
distribute_casualties(report, attackers);
```

### 关键设计点
1. **速度同步**：集结行军速度取最慢者，否则到达不一致
2. **中途退出**：允许退出，剩余人继续
3. **战损分配**：按兵力贡献比例分配损失和奖励
4. **多次战斗**：攻城可能持续多轮（城墙耐久→驻军→最终攻破）
5. **通知**：每波战斗结果实时推送给所有参与者

---

## Q7: 兵种克制系统如何实现？数值设计思路？

**A:**

### 克制矩阵
```
         步兵   骑兵   弓兵
步兵      100   120    80
骑兵       80   100   120
弓兵      120    80   100
```

数值含义：攻方（行）对守方（列）的伤害修正（百分比）。

### 实现
```cpp
// 克制表（静态配置，策划填表）
const int32_t COUNTER_TABLE[TROOP_TYPE_COUNT][TROOP_TYPE_COUNT] = {
    // 步  骑  弓
    {100, 120, 80},  // 步兵 attack
    {80,  100, 120}, // 骑兵 attack
    {120, 80,  100}, // 弓兵 attack
};

int32_t calc_damage(Unit& atk, Unit& def) {
    int32_t base = atk.atk * atk.troop_count;
    int32_t counter_mod = COUNTER_TABLE[atk.troop_type][def.troop_type];
    int32_t damage = base * counter_mod / 100;
    // ... 后续减防、buff、地形 ...
    return damage;
}
```

### 多兵种混编
每个武将可带不同兵种，一支军队 3 个武将可能 3 种兵种：
- 战斗中每对攻防组合独立查克制表
- 策略层面：玩家需要根据敌方兵种组合调整阵容

### 特殊兵种
```
轻骑兵 → 速度快，克制弓兵
重步兵 → 防御高，克制骑兵
连弩兵 → 攻击高，克制步兵
战象   → 全克制但速度慢
```

数值设计原则：**石头剪刀布 + 特殊单位打破平衡 → 迫使玩家动脑组合阵容**

---

## Q8: 技能系统在 SLG 自动战斗中如何运作？

**A:**

### 技能分类
```
1. 主动技能（Active）: 每 N 回合自动释放一次
2. 被动技能（Passive）: 战斗开始时永驻 buff / 触发条件
3. 指挥技能（Commander）: 全军 buff，战斗开始前生效
4. 追击技能（Pursuit）: 普攻后概率触发
5. 阵法技能（Formation）: 和阵型搭配的特殊效果
```

### 回合制战斗循环
```
SimulateBattle(input):
    rng = BattleRNG(input.seed)
    
    // Phase 0: 战前buff（指挥技能）
    apply_commander_skills(all_units)
    
    for round in 1..MAX_ROUNDS:
        // Phase 1: 速度排序决定行动顺序
        action_order = sort_by_speed(alive_units)
        
        for unit in action_order:
            if unit.is_dead(): continue
            
            // Phase 2: 检查是否释放主动技能
            if unit.skill_ready(round):
                target = select_skill_target(unit, rng)
                execute_skill(unit, target, rng)
                unit.skill_cooldown_reset()
            else:
                // Phase 3: 普通攻击
                target = select_attack_target(unit, rng)
                execute_normal_attack(unit, target, rng)
                
                // Phase 4: 追击判定
                if rng.check(unit.pursuit_rate):
                    execute_pursuit(unit, target, rng)
            
            // Phase 5: 回合末 buff tick（中毒/灼烧/回血）
            process_buffs(unit, rng)
        
        // Phase 6: Check 胜负
        if attacker_all_dead() || defender_all_dead():
            break
    
    return build_report(...)
```

### 技能选目标规则
```cpp
Unit* select_skill_target(Unit& caster, Skill& skill, BattleRNG& rng) {
    switch (skill.target_type) {
        case ENEMY_MIN_HP:     // 补刀：攻击血最少的
            return find_min_hp(enemies);
        case ENEMY_MAX_TROOP:  // 集火：攻击兵最多的
            return find_max_troop(enemies);
        case ENEMY_RANDOM:     // 随机
            return enemies[rng.next() % alive_enemy_count];
        case ALLY_MIN_HP:      // 治疗：奶血最少的队友
            return find_min_hp(allies);
        case AOE_ALL_ENEMY:    // AOE：所有敌人
            return nullptr;  // 特殊处理
    }
}
```

### 确定性保证
- 行动顺序：速度相同时按 slot 编号（固定，不随机）
- 技能释放回合：`round % cooldown == 0`（确定性）
- 目标选择：相同条件多个目标时取 slot 最小的

---

## Q9: Buff/Debuff 系统如何设计？

**A:**

### Buff 数据结构
```protobuf
message BuffInstance {
    int32 buff_id = 1;          // buff 模板 ID
    int32 source_slot = 2;      // 谁施加的
    int32 remaining_rounds = 3; // 剩余回合数
    int32 stack_count = 4;      // 叠加层数
    BuffType type = 5;          // ADD_ATK / REDUCE_DEF / DOT / HOT / STUN / ...
    int32 value = 6;            // 数值（万分比或绝对值）
}
```

### Buff 分类
```
属性修改类:
  ADD_ATK(+30%攻击), ADD_DEF(+20%防御), ADD_SPEED(+10速度)
  REDUCE_ATK(-20%攻击), REDUCE_DEF(-30%防御)

持续伤害类 (DoT):
  BURN(灼烧), POISON(中毒), BLEED(流血) — 每回合造成伤害

持续治疗类 (HoT):
  REGEN(回血) — 每回合恢复

控制类:
  STUN(眩晕，跳过行动), TAUNT(嘲讽，强制攻击施加者)
  SILENCE(沉默，不能释放主动技能)

特殊类:
  SHIELD(护盾，吸收N点伤害), IMMUNE(免疫下一次伤害)
  COUNTER_BOOST(反击率提升)
```

### Buff 生命周期
```cpp
void process_buffs(Unit& unit, BattleRNG& rng) {
    for (auto it = unit.buffs.begin(); it != unit.buffs.end(); ) {
        auto& buff = *it;
        
        // 1. DoT/HoT tick
        if (buff.type == BURN || buff.type == POISON) {
            int32_t dot_damage = buff.value;
            unit.take_damage(dot_damage);
            record_action(DOT, buff.source_slot, unit.slot, dot_damage);
        } else if (buff.type == REGEN) {
            int32_t heal = buff.value;
            unit.heal(heal);
            record_action(HOT, buff.source_slot, unit.slot, heal);
        }
        
        // 2. 回合递减
        buff.remaining_rounds--;
        if (buff.remaining_rounds <= 0) {
            it = unit.buffs.erase(it);  // buff 消失
        } else {
            ++it;
        }
    }
}
```

### Buff 叠加规则
- 同源同类：刷新持续时间（不叠加数值）
- 异源同类：叠加层数，数值累加（最多 N 层）
- 互斥 buff：新的替换旧的（如增攻 vs 减攻取后来者）

---

## Q10: 战斗 service 和 MapService 的关系？如何调度？

**A:**

### 架构分工
```
MapService (有状态)           BattleService (无状态)
  ├─ 行军管理                    ├─ SimulateBattle()
  ├─ 碰撞检测                    ├─ 无状态，可水平扩容
  ├─ 视野计算                    └─ 纯函数，输入→输出
  └─ 触发战斗 ──gRPC──→
```

### 战斗触发流程
```
1. 行军到达目标格（MapService timer 触发）
2. MapService 准备 BattleInput:
   - 从内存取攻击方 ArmySnapshot
   - 从内存/Redis 取防守方 ArmySnapshot
   - 生成 seed, 读取地形
3. gRPC 调用 BattleService.SimulateBattle(BattleInput) → BattleReport
4. MapService 处理结果:
   - 扣双方兵力
   - 更新格子控制权（如果攻方胜）
   - 更新行军状态
   - 推送战报给双方玩家
   - 写入 DB (异步)
```

### 为什么 BattleService 独立？
1. **CPU 密集**：战斗计算是纯 CPU 操作，独立 service 不阻塞 MapService 的事件循环
2. **可扩容**：大规模战争时千场同时开打，可水平扩 Battle 节点
3. **可测试**：纯函数，输入输出完全确定，单元测试覆盖整个战斗逻辑

### 替代方案：内嵌式
小型 SLG 也可以把战斗直接在 MapService 内算（省一次 RPC），但：
- 阻塞事件循环
- 不能独立扩容
- 适合单场战斗 <1ms 且并发不高的场景

### 为什么战报必须返回给 MapService？

BattleService 是**无状态纯函数**，只负责计算，不持有任何地图/玩家/连接数据。战报必须回到 MapService 才能产生实际效果：

| MapService 拿到战报后做的事 | 为什么 BattleService 做不了 |
|---|---|
| 扣双方兵力 | 不持有行军/驻军数据 |
| 改格子控制权（攻方胜 → 城池易主） | 不持有地图格子状态 |
| 重算领地连通性、视野 observer_count | 不持有视野表 |
| 更新行军状态（残兵返回/驻防/全灭销毁） | 不持有行军表 |
| 推送战报给双方玩家 | 不持有玩家连接（连接在 Gate） |
| 写 Kafka → DB 持久化 | 不负责持久化 |

**架构本质是关注点分离：**
- BattleService = 纯计算层（"给我输入，还你结果"）
- MapService = 状态层（"我管地图上所有因果关系"）

如果让 BattleService 直接改地图状态，它就变成有状态服务，失去水平扩容能力，也违背了"MapService 是地图唯一权威"的架构原则。

---

## Q11: 如何处理"同时到达"同一格子的多支军队？

**A:**

### 问题描述
两支敌对行军同时（或几乎同时）到达同一格子，谁先谁后？

### 解决方案：确定性排序

```cpp
// 到达时间完全相同时，用 march_id 排序（march_id 全局唯一且递增）
// 先创建的行军先处理
struct ArrivalEvent {
    int64_t arrival_time_ms;  // 到达时间（毫秒精度）
    uint64_t march_id;
    uint32_t target_tile;
};

// TimerWheel 中按 (arrival_time_ms, march_id) 排序
bool operator<(const ArrivalEvent& a, const ArrivalEvent& b) {
    if (a.arrival_time_ms != b.arrival_time_ms) 
        return a.arrival_time_ms < b.arrival_time_ms;
    return a.march_id < b.march_id;  // deterministic tiebreak
}
```

### 多军到达同一城池
```
1. 先到的发起攻城，进入 BATTLING 状态
2. 后到的排队等待
3. 第一场打完 → 第二个上
4. 或者：合并为一次大战（取决于设计）
```

---

## Q12: 攻城战（PvE）如何设计？城墙/耐久/守军多层防御？

**A:**

### 多阶段攻城模型
```
Phase 1: 攻城器械 vs 城墙
  - 城墙 HP = 耐久值
  - 攻城方用特殊兵种/器械对城墙造成伤害
  - 城墙上的守军可反击
  - 城墙血量归零 → 进入 Phase 2

Phase 2: 攻军 vs 驻军
  - 标准 SimulateBattle
  - 驻军有防御加成（城墙残留buff、地形优势）
  - 驻军全灭 → 城池归属改变

Phase 3: 占领 / 掠夺
  - 攻方胜 → 城池控制权转移 / 掠夺资源
  - 攻方败 → 残兵返回
```

### 城池耐久实现
```cpp
struct CityDefense {
    int32_t wall_hp;           // 城墙当前 HP
    int32_t wall_max_hp;       // 城墙最大 HP
    int32_t wall_def;          // 城墙防御力
    int32_t wall_auto_repair;  // 每小时自动修复量
    ArmySnapshot garrison;     // 驻军快照
    int32_t defense_bonus;     // 城墙给驻军的防御加成 (%)
    
    // 城墙自动修复 (懒计算)
    int32_t current_wall_hp(int64_t now) {
        int64_t elapsed = now - last_damage_time;
        int32_t repaired = elapsed / 3600 * wall_auto_repair;
        return min(wall_hp + repaired, wall_max_hp);
    }
};
```

---

## Q13: 战斗结果如何影响地图状态？

**A:**

### 战斗结果处理流程
```cpp
void on_battle_complete(March& march, BattleReport& report) {
    // 1. 兵力更新
    apply_casualties(march.army, report.attacker_casualties);
    apply_casualties(target.garrison, report.defender_casualties);
    
    // 2. 行军状态变更
    if (report.result == ATTACKER_WIN) {
        // 3a. 攻方胜
        if (target.is_city()) {
            transfer_city_ownership(target, march.player_id);
            recalc_territory(target.region);  // 重算领地
            update_alliance_vision(target);    // 更新联盟视野
        } else if (target.is_resource()) {
            start_gathering(march, target);    // 开始采集
        }
        
        // 残兵驻防或返回
        if (march.has_remaining_troops()) {
            march.set_state(STATIONED);  // 驻防
        }
    } else {
        // 3b. 攻方败
        if (march.has_remaining_troops()) {
            create_return_march(march);  // 残兵返回
        } else {
            destroy_march(march);  // 全灭
        }
    }
    
    // 4. 推送战报
    push_report_to_player(march.player_id, report);
    push_report_to_player(target.owner_id, report);
    
    // 5. 异步持久化
    kafka_produce("db-write", serialize(march, target, report));
}
```

---

## Q14: 兵力损失如何计算？如何防止刷兵 exploit？

**A:**

### 损失公式
```
// 每次攻击造成的兵力损失
troop_kill = damage / troop_hp_per_unit

// 例: 伤害 10000, 每个士兵 50 HP → 杀 200 兵
// 防止杀超: min(troop_kill, target.current_troops)
```

### 伤兵与死兵
```
total_loss = 1000 兵
wounded = total_loss * wounded_rate  // 60% 可治疗
dead = total_loss - wounded          // 40% 永久损失

// wounded_rate 受影响：
// - 医院等级（提高伤兵比例）
// - 攻城 vs 野战（攻城死亡率更高）
// - 特定武将技能（提高伤兵恢复率）
```

### 防刷兵 exploit
1. **产兵速度限制**：训练有时间，不能瞬间补兵
2. **资源消耗**：训练需要粮食/木材/铁矿
3. **战斗冷却**：同一城池 N 秒内不能再次被攻击（防止反复刷经验）
4. **出征限制**：每人同时行军队列有上限（如 3-5 支）
5. **服务端验证**：兵力来源完全服务端控制，客户端无法修改

---

## Q15: 战报存储和查询如何设计？

**A:**

### 战报保存完整流程

```
BattleService 返回 BattleReport
        ↓
MapService 处理战斗结果（扣兵/易主/行军状态）
        ↓
   ┌────┴────────────────────────────┐
   ↓                                 ↓
推送给双方玩家（实时）           持久化（异步）
   ↓                                 ↓
MapService → Kafka              MapService → Kafka
topic: gate-{gate_id}           topic: db-write
msg: PushBattleReport           msg: SaveBattleReport
   ↓                                 ↓
Gate Node → TCP → 客户端        DB Service 消费
客户端加入本地战报列表               ↓
                              1. protobuf 序列化 → zstd 压缩
                              2. INSERT battle_reports 表
                              3. Redis LPUSH player:{id}:reports (最近N条ID)
                              4. Redis SET report:{id} (带 TTL 缓存)
```

**关键：实时推送和持久化是两条独立的 Kafka 路径，互不阻塞。**

### DB Service 消费代码
```go
func handleSaveBattleReport(msg kafka.Message) {
    var report pb.BattleReportStorage
    proto.Unmarshal(msg.Value, &report)
    
    // 1. 压缩
    blob := zstdCompress(report.ReportData)
    
    // 2. MySQL 写入
    db.Exec(`INSERT INTO battle_reports 
        (report_id, attacker_id, defender_id, result, report_blob, created_at) 
        VALUES (?, ?, ?, ?, ?, ?)`,
        report.ReportId, report.AttackerId, report.DefenderId,
        report.Result, blob, time.Now())
    
    // 3. Redis 缓存战报内容 (7天TTL)
    rdb.Set(ctx, fmt.Sprintf("report:%d", report.ReportId), blob, 7*24*time.Hour)
    
    // 4. 双方战报列表各加一条
    for _, pid := range []int64{report.AttackerId, report.DefenderId} {
        key := fmt.Sprintf("player:%d:reports", pid)
        rdb.LPush(ctx, key, report.ReportId)
        rdb.LTrim(ctx, key, 0, 99)  // 只保留最近100条
    }
}
```

### 玩家查看历史战报
```
客户端请求"我的战报列表"
  → Gate → DataService
  → Redis LRANGE player:{id}:reports 0 49  (取最近50条ID)
  → 批量 Redis MGET report:{id1}, report:{id2}, ...
  → cache miss 的去 MySQL 查 → 回填 Redis
  → 返回给客户端
```

### 存储方案

#### MySQL 表结构
```sql
CREATE TABLE battle_reports (
    report_id    BIGINT PRIMARY KEY,
    attacker_id  BIGINT,
    defender_id  BIGINT,
    battle_type  TINYINT,         -- PvP/PvE/Rally
    result       TINYINT,         -- Win/Lose/Draw
    tile_x       INT,
    tile_y       INT,
    created_at   TIMESTAMP,
    report_blob  MEDIUMBLOB,      -- zstd compressed protobuf
    INDEX idx_attacker (attacker_id, created_at),
    INDEX idx_defender (defender_id, created_at)
);
```

#### Redis 数据结构
```
player:{id}:reports   → List (最近100条 report_id, LPUSH+LTRIM)
report:{id}           → String (zstd blob, TTL 7天)
alliance:{id}:reports → List (联盟战报, 最近200条)
```

### 查询场景
- 玩家查看"我的战报"：Redis LRANGE → 批量 MGET → cache miss 回源 MySQL
- 联盟战报：Redis LRANGE alliance:{id}:reports
- 历史回放：按 `report_id` 精确查询 → Redis GET → miss → MySQL SELECT
- GM 仲裁：MySQL 精确查 + 反序列化 BattleInput → 重跑验证

### 容量估算
```
10K 玩家，每人每天 10 场战斗
= 100K 战报/天 × 2KB = 200MB/天
30 天 = 6GB
保留 90 天热数据 = ~18GB → MySQL 轻松承受
90 天后归档到 S3 冷存储

Redis 内存:
  100K 战报列表 × 100 ID × 8B = ~80MB (列表)
  热战报缓存 (7天) = 700K × 2KB = ~1.4GB (可调 TTL 控制)
```

---

## Q16: 战斗平衡性如何调试和验证？

**A:**

### 自动化平衡验证工具
```python
# 蒙特卡洛战斗仿真
def balance_test(unit_a, unit_b, trials=10000):
    wins_a = 0
    for i in range(trials):
        result = simulate_battle(unit_a, unit_b, seed=i)
        if result == A_WINS:
            wins_a += 1
    win_rate = wins_a / trials
    print(f"A win rate: {win_rate:.2%}")
    # 期望: 克制关系 ~60-65%, 同类 ~50%
```

### 策划工具
```
1. 数值模拟器: 输入两个阵容，跑 10000 次给出胜率分布
2. 伤害计算器: 输入属性显示各步骤中间值
3. 阵容推荐器: 给定敌方阵容，搜索最优反制组合
4. 版本对比: 修改数值后对比前后胜率变化
```

### 线上监控
```
- 统计每个武将的出场率、胜率
- 统计每个兵种的使用率
- 出场率 >50% 或胜率 >60% → 可能 OP，需要 nerf
- 使用率 <5% → 可能太弱，需要 buff
```

---

## Q17: 战斗系统的单元测试怎么写？

**A:**

```cpp
// 1. 确定性测试（同输入必须同输出）
TEST(Battle, Deterministic) {
    BattleInput input = make_test_input();
    auto report1 = SimulateBattle(input);
    auto report2 = SimulateBattle(input);
    EXPECT_EQ(report1.SerializeAsString(), report2.SerializeAsString());
}

// 2. 克制关系测试
TEST(Battle, TroopCounterSystem) {
    // 步兵 vs 骑兵，步兵应赢 (120% 伤害修正)
    auto input = make_counter_test(INFANTRY, CAVALRY, same_stats);
    int wins = 0;
    for (int seed = 0; seed < 1000; seed++) {
        input.seed = seed;
        auto report = SimulateBattle(input);
        if (report.result == ATTACKER_WIN) wins++;
    }
    // 步兵克骑兵，胜率应 > 55%
    EXPECT_GT(wins, 550);
    EXPECT_LT(wins, 750);  // 但不应碾压
}

// 3. 兵力归零判定
TEST(Battle, TroopDepletion) {
    auto input = make_input(atk_troops=10, def_troops=10000);
    auto report = SimulateBattle(input);
    EXPECT_EQ(report.result, DEFENDER_WIN);
    EXPECT_EQ(report.attacker_remaining_troops, 0);
    EXPECT_GT(report.defender_remaining_troops, 0);
}

// 4. 回合上限
TEST(Battle, MaxRoundsDrawn) {
    // 双方都是纯防御，打不死对方
    auto input = make_tank_vs_tank();
    auto report = SimulateBattle(input);
    EXPECT_EQ(report.rounds.size(), MAX_ROUNDS);
    EXPECT_EQ(report.result, DRAW);  // 或 DEFENDER_WIN（守方优势判定）
}

// 5. Buff 正确性
TEST(Battle, PoisonDotDamage) {
    auto input = make_poison_test();
    auto report = SimulateBattle(input);
    // 检查每回合都有中毒伤害 action
    for (auto& round : report.rounds) {
        bool has_poison = false;
        for (auto& action : round.actions) {
            if (action.type == DOT && action.buff_type == POISON)
                has_poison = true;
        }
        EXPECT_TRUE(has_poison);
    }
}

// 6. 战报可回放
TEST(Battle, ReportConsistency) {
    auto report = SimulateBattle(input);
    // 从战报的 BattleInput 重新跑一次
    auto report2 = SimulateBattle(report.input);
    EXPECT_EQ(report, report2);
}
```

---

## Q18: 大规模战争（国战/赛季决战）如何支撑数千场并发战斗？

**A:**

### 问题规模
赛季决战可能触发：
- 2000+ 行军同时到达，每秒数百场战斗
- 单场 SimulateBattle ~0.5-2ms
- 峰值 ~1000 场/秒 → 需要 ~1-2 核

### 方案

1. **BattleService 水平扩容**
```
MapService ──gRPC──→ BattleService (N pods)
                       ├─ pod 1 (4 core)
                       ├─ pod 2 (4 core)
                       └─ pod 3 (4 core)
负载均衡: gRPC round-robin
```

2. **批量+pipeline**
```
// MapService 不等单个结果，批量发送
batch_requests = collect_pending_battles(max=50, timeout=10ms);
futures = parallel_send(battle_service, batch_requests);
results = await_all(futures);
process_results(results);
```

3. **优先级调度**
```
- 玩家可见战斗 → 高优先级（需要快速出结果给战报）
- NPC/AI 战斗 → 低优先级（可延迟100ms）
- 相同格子多场战斗 → 顺序处理（有因果依赖）
```

4. **内存池 + 对象复用**
```cpp
// 战斗对象频繁创建销毁，用对象池
ObjectPool<BattleContext> battle_pool(1000);
auto* ctx = battle_pool.acquire();
auto report = ctx->simulate(input);
battle_pool.release(ctx);
```

---

## Q19: 战斗数值溢出和边界情况如何防护？

**A:**

```cpp
// 1. 伤害不能为负
damage = max(0, damage);

// 2. 治疗不能超过最大 HP
unit.hp = min(unit.hp + heal, unit.max_hp);

// 3. 兵力不能为负
unit.troops = max(0, unit.troops - kill);

// 4. Buff 叠加不能超限
buff.stack_count = min(buff.stack_count + 1, MAX_STACK);

// 5. 回合数上限
if (round >= MAX_ROUNDS) break;

// 6. 整数溢出保护
int64_t temp = (int64_t)atk * multiplier;
int32_t result = (int32_t)min(temp / 10000, (int64_t)INT32_MAX);

// 7. 除零保护
int32_t reduction = (defender.def > 0) ? 
    damage * defender.def / (defender.def + K) : 0;

// 8. 死亡单位不能行动
if (unit.troops <= 0 || unit.hp <= 0) continue;
```

---

## Q20: 面试常见陷阱问题

**A:**

1. **"为什么不用浮点？IEEE 754 不是确定的吗？"**
   → 同一硬件上确定，跨平台不确定（编译器优化、FPU 模式、SSE vs x87）。SLG 服务器可能跨平台部署/编译。用整数一劳永逸。

2. **"战斗为什么不实时同步？"**
   → SLG 不是动作游戏，玩家不操作战斗过程。实时同步增加复杂度无收益，延迟敏感度低。

3. **"战报太大怎么办？"**
   → protobuf + zstd 压缩后 ~500B-2KB/场。100K 玩家 × 100 条 = 最多几十 GB，完全可控。超限走冷存储。

4. **"怎么保证 PRNG 跨平台一致？"**
   → 用明确定义的算法（xoshiro256），不用 `rand()`/`std::mt19937`（标准没定义实现）。手写或用标准化实现。

5. **"如果两个 BattleService 跑出不同结果？"**
   → 不会。纯函数 + 同输入 = 同输出。如果真出了不同结果 → 说明有 bug（浮点残留/undefined behavior/非确定性容器）。
