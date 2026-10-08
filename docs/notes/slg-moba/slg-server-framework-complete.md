# 率土之滨风格 SLG 游戏服务器框架 — 完整设计文档

**Created:** 2026-04-02  
**Purpose:** 全面描述如何从零搭建一个率土之滨风格的 SLG 游戏服务器，涵盖架构、每个子系统的设计与实现细节。

---

## 目录

1. [游戏特征分析](#1-游戏特征分析)
2. [整体架构](#2-整体架构)
3. [大地图系统](#3-大地图系统)
4. [行军系统](#4-行军系统)
5. [寻路系统](#5-寻路系统)
6. [战斗系统](#6-战斗系统)
7. [视野与战争迷雾](#7-视野与战争迷雾)
8. [建筑与资源系统](#8-建筑与资源系统)
9. [武将与部队系统](#9-武将与部队系统)
10. [联盟系统](#10-联盟系统)
11. [赛季与排行榜](#11-赛季与排行榜)
12. [跨服战](#12-跨服战)
13. [网络层与协议](#13-网络层与协议)
14. [数据存储与持久化](#14-数据存储与持久化)
15. [运维与部署](#15-运维与部署)
16. [安全与反作弊](#16-安全与反作弊)
17. [性能优化](#17-性能优化)
18. [GM 工具与运营支撑](#18-gm-工具与运营支撑)
19. [离线玩家与推送](#19-离线玩家与推送)
20. [机器人与压测](#20-机器人与压测)

---

## 1. 游戏特征分析

率土之滨是一款赛季制大地图 SLG，核心特征：

| 特征 | 说明 |
|------|------|
| 大地图 | 1200×1200 格子（~144万格），所有玩家共享一张大地图 |
| 赛季制 | 每个服务器运行一个"赛季"（通常 2-3 个月），结束后开新赛季 |
| 回合制战斗 | 行军到达目标格自动触发回合制战斗，非实时 |
| 行军 | 部队在地图上移动，有速度、路径，可被拦截 |
| 资源采集 | 占领资源点，定时产出 |
| 建筑 | 主城内建筑升级（经院/校场/兵营等），加速、增加属性 |
| 武将 | 三国武将系统，搭配技能/兵种/兵书 |
| 联盟 | 核心社交单位，联盟战争、领地占领 |
| 离线不掉线 | 玩家下线后城池/部队依然在地图上，可被攻击 |
| 大规模并发 | 单服 5000-10000 人同时在线，数万支行军并发 |

### 与 MMORPG 的核心差异

| 维度 | MMORPG | SLG |
|------|--------|-----|
| 地图模型 | 多个小副本/场景 | 一张超大共享地图 |
| 坐标系 | 连续浮点坐标 | 离散格子坐标 |
| 同步模型 | 帧同步/状态同步（每帧） | 事件驱动 + 懒计算（无 tick） |
| 战斗模型 | 实时动作/技能 | 回合制确定性模拟 |
| AOI | 实体中心（"谁在我附近"） | 格子中心（"我能看到哪些格子"） |
| 实体生命周期 | 玩家上线才创建 | 永驻内存（离线也在地图上） |
| 关键瓶颈 | 同屏人数、帧率 | 行军并发数、大地图内存 |

---

## 2. 整体架构

### 2.1 服务划分

```
┌──────────────────────────────────────────────────────────────┐
│                        Client (Unity)                         │
│                 TCP/WebSocket 长连接 → Gate                   │
└──────────────────────┬───────────────────────────────────────┘
                       │
┌──────────────────────▼───────────────────────────────────────┐
│                      Gate Service                             │
│  ① 连接管理  ② 协议解析  ③ 消息路由                          │
│  ④ 心跳检测  ⑤ Token 验证  ⑥ 流量限制                       │
└──────────────────────┬───────────────────────────────────────┘
                       │ 内部 gRPC / Kafka
          ┌────────────┼────────────────────────────┐
          │            │                            │
┌─────────▼──┐  ┌──────▼──────┐  ┌──────────────────▼──────┐
│ MapService │  │ BattleService│  │ BuildService/ArmyService │
│（有状态）   │  │（无状态）    │  │ AllianceService/ChatSvc  │
│ 大地图核心  │  │ 战斗计算     │  │ 各业务微服务             │
└─────┬──────┘  └──────────────┘  └─────────────────────────┘
      │                                    │
      │          ┌─────────────┐           │
      └──────────▶ Data Layer  ◀───────────┘
                 │ Redis + MySQL│
                 │ + Kafka      │
                 └─────────────┘
```

### 2.2 各服务职责

| 服务 | 类型 | 职责 | 扩展方式 |
|------|------|------|----------|
| **Gate** | 有状态 | 客户端连接管理、协议解析、消息路由、Token 验证 | 水平扩展，Login 按负载分配 |
| **MapService** | 有状态 | 大地图数据、行军管理、视野计算、格子状态 | 单服一个实例（Region 级并发） |
| **BattleService** | 无状态 | 确定性战斗模拟，输入→输出纯函数 | 水平扩展 worker pool |
| **BuildService** | 有状态 | 建筑升级 timer、科技研究 | 可按玩家分片 |
| **ArmyService** | 有状态 | 武将管理、部队编制、征兵 | 可按玩家分片 |
| **AllianceService** | 有状态 | 联盟 CRUD、联盟战争、外交 | 可按联盟 ID 分片 |
| **ChatService** | 有状态 | 世界/联盟/私聊频道 | 按频道分片 |
| **LoginService** | 无状态 | 账号验证、Gate 分配、Token 签发 | 水平扩展 |
| **DataService** | 有状态 | 跨服数据路由、Redis 连接池、持久化 | 按 Realm 分片 |
| **RankService** | 有状态 | 排行榜计算与查询 | Redis sorted set |
| **MailService** | 有状态 | 邮件收发、系统邮件群发 | 按玩家分片 |

### 2.3 核心设计原则

1. **MapService 不可水平扩展** — 一张大地图 = 一个 MapService 实例。内部用 Region 级并发。
2. **BattleService 可水平扩展** — 纯函数无状态，worker pool。
3. **事件驱动，非 tick 驱动** — 行军位置用公式算，不需要每帧 tick。
4. **离线实体永驻** — 玩家下线 ≠ 移除实体，城池/部队仍在内存。
5. **服务端权威** — 所有计算在服务端，客户端只发意图（intent），不发结果。

---

## 3. 大地图系统

### 3.1 地图数据结构

```cpp
// 核心格子结构
struct Tile {
    uint32_t tile_id;       // = y * MAP_WIDTH + x
    TileType type;          // 平地/山地/河流/城池/资源点
    uint32_t owner_id;      // 所属玩家 (0 = 无主)
    uint32_t alliance_id;   // 所属联盟
    BuildingInfo* building; // 建筑信息 (null = 空地)
    
    // 视野
    robin_hood::unordered_map<uint32_t, uint16_t> observer_count; // player_id → count
    
    // 行军占用 (时空碰撞检测)
    small_vector<OccupancyEntry, 2> occupants; // 行军占用记录
    
    // 资源
    ResourceInfo* resource;  // 资源点信息
};

// 行军占用记录
struct OccupancyEntry {
    uint64_t army_id;
    uint32_t enter_time;  // 进入该格时间戳
    uint32_t leave_time;  // 离开该格时间戳
    uint32_t player_id;   // 所属玩家
};

// Region = 32×32 格子
struct Region {
    uint16_t region_id;
    Tile tiles[32][32];
    std::mutex lock;  // Region 级锁（或无锁 actor）
    
    // 该 Region 内活跃行军列表
    std::vector<uint64_t> active_marches;
};
```

### 3.2 地图分区（Region）

- 1200×1200 / 32×32 = 37.5 × 37.5 ≈ 38×38 = **1444 个 Region**
- 每个 Region 独立处理，减少全局锁竞争
- 跨 Region 行军：在 Region 边界做 handoff
- Region 级处理方式选择：
  - **方案 A**: 线程池 + Region 锁（简单，推荐）
  - **方案 B**: Actor model（每个 Region 一个 actor/coroutine）
  - **方案 C**: 单线程 + 事件循环（最简单，适合单服规模小）

### 3.3 地图坐标与索引

```cpp
constexpr int MAP_WIDTH = 1200;
constexpr int MAP_HEIGHT = 1200;
constexpr int REGION_SIZE = 32;
constexpr int REGIONS_PER_ROW = (MAP_WIDTH + REGION_SIZE - 1) / REGION_SIZE; // 38

inline uint32_t TileId(int x, int y) { return y * MAP_WIDTH + x; }
inline int TileX(uint32_t id) { return id % MAP_WIDTH; }
inline int TileY(uint32_t id) { return id / MAP_WIDTH; }
inline uint16_t RegionId(int x, int y) { return (y / REGION_SIZE) * REGIONS_PER_ROW + (x / REGION_SIZE); }
```

### 3.4 内存估算

| 项目 | 数据量 | 内存 |
|------|--------|------|
| 1.44M 格子 × 128B | — | ~185 MB |
| observer_count (假设平均 2 个观察者/格) | 2.88M entries × 8B | ~23 MB |
| 行军数据 (50K 行军 × 1KB) | — | ~50 MB |
| Region 结构 (1444 个) | — | < 1 MB |
| **总计** | — | **~260 MB** |

结论：单台 8GB 内存机器绰绰有余。

---

## 4. 行军系统

### 4.1 行军核心数据

```cpp
struct March {
    uint64_t army_id;
    uint32_t player_id;
    MarchState state;       // MARCHING / BATTLING / GATHERING / RETURNING / GARRISONED
    
    // 路径
    std::vector<uint32_t> path;  // 格子 ID 序列
    uint32_t start_time;         // 出发时间戳
    float speed;                 // 格/秒
    
    // 部队
    ArmyComposition troops;      // 兵种/数量
    std::array<GeneralSlot, 3> generals; // 最多 3 个武将
    
    // 目标
    uint32_t target_tile_id;
    MarchTarget target_type;     // ATTACK / GATHER / REINFORCE / SCOUT
    
    // 计时器
    TimerId arrival_timer;
    TimerId vision_update_timer;
};
```

### 4.2 懒计算位置（核心优化）

```cpp
// 不需要每帧 tick，任何时刻都能算出当前位置
uint32_t GetCurrentTile(const March& march, uint32_t now) {
    uint32_t elapsed = now - march.start_time;
    int tile_index = static_cast<int>(elapsed * march.speed);
    if (tile_index >= march.path.size()) {
        return march.path.back(); // 已到达
    }
    return march.path[tile_index];
}
```

**为什么不用 tick？**
- 50K 行军 × 每帧计算 = 巨大 CPU 开销
- SLG 行军位置是线性推进，完全可以用公式计算
- 只在需要时才算（玩家查询、碰撞检测、到达事件）
- 省下的 CPU 全部用于真正需要实时计算的系统

### 4.3 行军生命周期

```
创建行军 → 寻路 → 注册路径（时空碰撞检测）→ 注册到达 timer
                                                    │
    ┌───────────────────────────────────────────────┘
    │
    ▼
[MARCHING] ──到达目标──→ [BATTLING] ──战斗结束──→ [RETURNING]
    │                       │                        │
    │撤回                   │战败                    │到达主城
    ▼                       ▼                        ▼
[RETURNING]            [RETURNING]              [DISBANDED]
```

### 4.4 时空碰撞检测（March Encounter Detection）

这是 SLG 行军系统的精华：不用 tick，如何检测两支行军在同一格子同一时间相遇？

```cpp
// 注册新行军时，检测路径上每个格子是否有时间重叠
void RegisterMarch(March& newMarch) {
    for (int i = 0; i < newMarch.path.size(); i++) {
        uint32_t tileId = newMarch.path[i];
        float enterTime = newMarch.start_time + i / newMarch.speed;
        float leaveTime = newMarch.start_time + (i + 1) / newMarch.speed;
        
        Tile& tile = GetTile(tileId);
        
        // 检查该格已有的军队占用记录
        for (auto& occupant : tile.occupants) {
            // 是否敌对
            if (!IsEnemy(newMarch.player_id, occupant.player_id)) continue;
            
            // 时间重叠检测
            if (enterTime < occupant.leave_time && occupant.enter_time < leaveTime) {
                // 发生碰撞！在重叠时间点安排战斗
                float battleTime = std::max(enterTime, occupant.enter_time);
                ScheduleBattle(newMarch.army_id, occupant.army_id, tileId, battleTime);
                return; // 第一个碰撞点就开打
            }
        }
        
        // 注册占用
        tile.occupants.push_back({newMarch.army_id, enterTime, leaveTime, newMarch.player_id});
    }
}
```

**关键点：**
- 所有检测在行军**注册时一次性完成**，O(path_length)
- 之后零开销，不需要每帧检测
- 撤回/加速 → 清除旧占用记录 → 重新注册剩余路径

### 4.5 行军召回

```cpp
void RecallMarch(uint64_t army_id) {
    March& march = GetMarch(army_id);
    uint32_t now = CurrentTime();
    uint32_t currentTile = GetCurrentTile(march, now);
    
    // 1. 清除原路径上 now 之后的所有占用记录
    ClearFutureOccupancy(march, now);
    
    // 2. 取消到达 timer
    CancelTimer(march.arrival_timer);
    
    // 3. 计算回程路径（原路返回）
    march.path = ComputeReturnPath(currentTile, march.home_tile);
    march.start_time = now;
    march.state = RETURNING;
    
    // 4. 重新注册（新路径的碰撞检测 + 到达 timer）
    RegisterMarch(march);
    
    // 5. 通知客户端
    PushMarchUpdate(march);
}
```

---

## 5. 寻路系统

### 5.1 算法选型

| 算法 | 适用场景 | 性能 (1024×1024, 30% 障碍) |
|------|----------|----------------------------|
| A* | 基础，所有格子地图 | ~250ms, 800K 节点展开 |
| JPS | 均匀代价格子图加速 | ~12ms, 15K 节点展开 |
| HPA* | 超大地图分层 | ~3ms, 200 节点展开 + 预处理 2s |

**推荐组合：JPS + HPA***

### 5.2 JPS (Jump Point Search)

```cpp
// JPS 核心：跳跃搜索，跳过对称路径
TileId JumpStraight(TileId current, Direction dir, TileId goal) {
    TileId next = Step(current, dir);
    if (!IsPassable(next)) return INVALID;
    if (next == goal) return next;
    
    // 检查是否有强制邻居（forced neighbor）
    if (HasForcedNeighbor(next, dir)) return next; // 这是跳点
    
    return JumpStraight(next, dir, goal); // 继续跳
}

// 对角跳跃额外检查水平/垂直方向
TileId JumpDiagonal(TileId current, Direction dir, TileId goal) {
    TileId next = Step(current, dir);
    if (!IsPassable(next)) return INVALID;
    if (next == goal) return next;
    if (HasForcedNeighbor(next, dir)) return next;
    
    // 对角跳时，分解成两个正交方向各检查一次
    if (JumpStraight(next, HorizontalComponent(dir), goal) != INVALID) return next;
    if (JumpStraight(next, VerticalComponent(dir), goal) != INVALID) return next;
    
    return JumpDiagonal(next, dir, goal);
}
```

### 5.3 HPA* (Hierarchical Pathfinding A*)

```
预处理（离线/地图加载时）:
1. 将 1200×1200 地图切成 32×32 chunks（38×38 = 1444 个 chunk）
2. 扫描相邻 chunk 边界，找连续可通行段中点 → 作为"入口点"
3. chunk 内 A* 算入口点两两距离
4. 构建抽象图（入口点 = 节点，距离 = 边权）

在线查询:
1. 将 start/goal 临时插入 chunk 的抽象图
2. 在抽象图上跑 A*（节点数从百万降到几千）
3. 途经 chunk 内做精细 JPS 拼出完整路径
```

### 5.4 动态障碍处理

- 建筑建造/拆除 → 只需重算受影响 chunk 的入口点和内部距离
- SLG 中地图变化频率低，重算开销可接受
- 活跃行军受影响时主动通知重新寻路

### 5.5 路径缓存

```cpp
struct PathCacheKey {
    uint32_t start_tile;
    uint32_t end_tile;
    uint64_t map_version;  // 地图障碍 CRC / version counter
};

// LRU 缓存，命中率 > 50%（相同路线多人行军）
LRUCache<PathCacheKey, std::vector<uint32_t>> path_cache(10000);
```

---

## 6. 战斗系统

### 6.1 核心设计：确定性模拟

```cpp
// 战斗是纯函数，不依赖任何外部状态
BattleReport SimulateBattle(
    const ArmySnapshot& attacker,
    const ArmySnapshot& defender,
    uint32_t seed,
    TerrainType terrain
);
```

**确定性约束：**
1. **禁用浮点** — 用整数百分比（`atk * 120 / 100`），或定点数
2. **单一 PRNG** — xoshiro256 / PCG，seed 固定 → 结果固定
3. **固定调用顺序** — PRNG 调用必须有确定的顺序
4. **无外部依赖** — 不依赖系统时间、内存地址、容器遍历顺序

### 6.2 战斗流程

```
初始化：根据双方武将/兵种/技能构建战斗单位
    │
    ▼
回合循环（最大 N 回合，如 8 回合）:
    │
    ├─ 主动技能阶段：按速度排序，每个武将释放主动技能
    │     ├─ 选择目标（策略：最低HP/最高攻击/随机）
    │     ├─ 计算伤害：base_dmg = (atk * skill_coeff / 100) - def * def_factor / 100
    │     ├─ 暴击判定：rand() < crit_rate → dmg *= crit_mult / 100
    │     ├─ 闪避判定：rand() < dodge_rate → dmg = 0
    │     ├─ 应用伤害，记录 BattleAction
    │     └─ 触发被动技能/buff
    │
    ├─ 普攻阶段：未使用技能的武将执行普通攻击
    │
    ├─ Buff/DOT 结算
    │
    ├─ 死亡检查：兵力 <= 0 的武将阵亡
    │
    └─ 胜负判定：一方全灭 → 结束
    │
    ▼
生成 BattleReport
```

### 6.3 BattleReport 结构

```protobuf
message BattleReport {
    BattleInput input = 1;           // 完整输入快照（用于重放验证）
    repeated Round rounds = 2;       // 每回合
    BattleResult result = 3;         // 胜负
    ArmyCasualties attacker_casualties = 4;  // 攻方损失
    ArmyCasualties defender_casualties = 5;  // 守方损失
    uint32 checksum = 6;            // CRC32 校验
}

message Round {
    uint32 round_number = 1;
    repeated BattleAction actions = 2;
}

message BattleAction {
    ActionType type = 1;    // NORMAL_ATTACK / SKILL / BUFF / HEAL / DEATH
    uint32 source_slot = 2; // 释放者位置
    uint32 target_slot = 3; // 目标位置
    uint32 skill_id = 4;
    int32 damage = 5;       // 伤害值（正数=伤害，负数=治疗）
    int32 hp_after = 6;     // 目标当前 HP（关键！客户端不需要自己算）
    bool is_crit = 7;
    bool is_dodge = 8;
    repeated BuffEvent buff_events = 9;
}
```

**为什么包含 `hp_after`？**
- 客户端只需播放动画 + 显示 hp_after，不需要嵌入战斗逻辑
- 避免客户端计算误差导致显示不一致
- 任意 action 可独立渲染，支持快进/跳转

### 6.4 多军围城（Siege）

```cpp
struct SiegeSession {
    uint32_t target_tile;
    uint64_t defender_army;
    std::priority_queue<ArrivalEvent> arrival_queue; // 按到达时间排序
    
    // 防守方连续作战（HP不满血恢复），攻方依次挑战 → 消耗战
    void ProcessNextAttacker() {
        if (arrival_queue.empty()) {
            EndSiege();
            return;
        }
        auto attacker = arrival_queue.top(); arrival_queue.pop();
        BattleReport report = SimulateBattle(attacker.army, defender_army, GenSeed(), terrain);
        ApplyCasualties(report);
        
        if (defender_army.is_dead()) {
            CaptureCity(target_tile, attacker.player_id);
        } else {
            // 防守方存活但可能受伤，继续下一个攻击者
            ProcessNextAttacker();
        }
    }
};
```

### 6.5 BattleService 架构

```
MapService 发现两支军队碰撞
    │
    │ BattleRequest (attacker_snapshot, defender_snapshot, seed, terrain)
    ▼
BattleService Worker Pool (无状态，可水平扩展)
    │
    │ 纯函数计算
    ▼
BattleReport
    │
    │ Kafka / gRPC 回调
    ▼
MapService 应用结果（减兵、占领、视野更新）
```

- 单次战斗 < 1ms
- Worker Pool 可动态扩缩
- 批量战斗时（大规模攻城）可用 Kafka 队列缓冲

---

## 7. 视野与战争迷雾

### 7.1 核心算法：引用计数

```cpp
// 玩家能看到某格子 ⟺ observer_count[player_id] > 0
void AddObserver(Tile& tile, uint32_t player_id) {
    tile.observer_count[player_id]++;
    if (tile.observer_count[player_id] == 1) {
        // 首次可见，推送格子信息给玩家
        PushTileReveal(player_id, tile);
    }
}

void RemoveObserver(Tile& tile, uint32_t player_id) {
    auto it = tile.observer_count.find(player_id);
    if (it != tile.observer_count.end()) {
        it->second--;
        if (it->second <= 0) {
            tile.observer_count.erase(it);
            // 格子变为迷雾
            PushTileFog(player_id, tile);
        }
    }
}
```

### 7.2 视野来源

| 来源 | 视野半径 | 更新频率 |
|------|----------|----------|
| 主城 | 5-8 格 | 建筑升级时变化 |
| 行军部队 | 3-5 格 | 每移动一格更新 |
| 哨塔（了望台） | 10-15 格 | 建造/拆除时更新 |
| 盟友共享 | 同盟友视野 | 联盟变动时批量更新 |

### 7.3 预计算偏移表

```cpp
// 视野半径 = 5 时，预计算所有相对偏移 (dx, dy)
static const std::vector<std::pair<int,int>>& VisionOffsets(int radius) {
    static std::map<int, std::vector<std::pair<int,int>>> cache;
    if (cache.count(radius)) return cache[radius];
    
    auto& offsets = cache[radius];
    for (int dy = -radius; dy <= radius; dy++) {
        for (int dx = -radius; dx <= radius; dx++) {
            if (dx*dx + dy*dy <= radius*radius) { // 圆形视野
                offsets.push_back({dx, dy});
            }
        }
    }
    return offsets;
}

// 建筑/驻军视野更新
void UpdateStaticVision(uint32_t player_id, int cx, int cy, int radius, bool add) {
    for (auto& [dx, dy] : VisionOffsets(radius)) {
        int x = cx + dx, y = cy + dy;
        if (!InBounds(x, y)) continue;
        if (add) AddObserver(GetTile(x, y), player_id);
        else     RemoveObserver(GetTile(x, y), player_id);
    }
}
```

### 7.4 行军滑动窗口更新

```
行军从 A → B → C → D，视野半径 = 3

位于 B 时：观察 B 周围 3 格内所有格子
移动到 C 时：
  - 新增：C 的视野范围但不在 B 视野范围内的格子 → AddObserver
  - 移除：B 的视野范围但不在 C 视野范围内的格子 → RemoveObserver

优化：只有 "前进方向前方一行" 和 "后方一行" 变化
     不需要对整个视野范围重新计算
```

```cpp
void UpdateMarchVision(const March& march, uint32_t prevTile, uint32_t newTile, int radius) {
    int px = TileX(prevTile), py = TileY(prevTile);
    int nx = TileX(newTile),  ny = TileY(newTile);
    
    // 增量计算：新增视野
    for (auto& [dx, dy] : VisionOffsets(radius)) {
        int x = nx + dx, y = ny + dy;
        if (!InBounds(x, y)) continue;
        // 如果这个格不在旧视野范围内，才需要 add
        if ((x - px)*(x - px) + (y - py)*(y - py) > radius*radius) {
            AddObserver(GetTile(x, y), march.player_id);
        }
    }
    
    // 增量计算：移除视野
    for (auto& [dx, dy] : VisionOffsets(radius)) {
        int x = px + dx, y = py + dy;
        if (!InBounds(x, y)) continue;
        if ((x - nx)*(x - nx) + (y - ny)*(y - ny) > radius*radius) {
            RemoveObserver(GetTile(x, y), march.player_id);
        }
    }
}
```

### 7.5 视口请求（地图拖拽）

客户端拖拽地图时需要看到的数据分两层：

| 层 | 说明 | 数据来源 |
|---|---|---|
| **视野数据**（push） | 玩家有视野的格子的动态信息（敌军、资源量） | MapService 主动推送 |
| **视口数据**（pull） | 玩家拖拽可见区域的静态/基础信息（地形、城名） | 客户端请求，服务端按权限过滤 |

```
客户端请求 ViewportRequest(region_id)
    │
    ▼
服务端：
    ① 地形/建筑基础信息 → 直接返回（无权限限制）
    ② 动态实体（敌军/资源量）→ 只返回玩家有视野权限的
    ③ 频率限制：每秒最多 5 次视口请求
```

### 7.6 联盟视野共享

```cpp
// 联盟成员加入/退出时批量同步视野
void OnAllianceMemberJoin(uint32_t newMemberId, uint32_t allianceId) {
    auto& members = GetAllianceMembers(allianceId);
    for (auto memberId : members) {
        if (memberId == newMemberId) continue;
        // 将已有成员的视野共享给新成员
        ShareVision(memberId, newMemberId);
        // 将新成员的视野共享给已有成员
        ShareVision(newMemberId, memberId);
    }
}
```

---

## 8. 建筑与资源系统

### 8.1 主城建筑

```cpp
struct Building {
    uint32_t config_id;     // 建筑类型
    uint32_t level;
    BuildingState state;    // IDLE / UPGRADING / DEMOLISHED
    uint32_t upgrade_finish_time;  // 升级完成时间戳
    TimerId upgrade_timer;
};

// 建筑升级 — 时间驱动
void StartUpgrade(Building& building, uint32_t playerId) {
    // 1. 校验资源
    auto cost = GetUpgradeCost(building.config_id, building.level + 1);
    if (!ConsumeResources(playerId, cost)) return;
    
    // 2. 计算时间（基础时间 × 加速率）
    uint32_t duration = GetBaseDuration(building.config_id, building.level + 1);
    duration = ApplySpeedBuff(playerId, duration); // 科技/联盟加速
    
    // 3. 设置 timer
    building.state = UPGRADING;
    building.upgrade_finish_time = Now() + duration;
    building.upgrade_timer = SetTimer(duration, [&building, playerId]() {
        building.level++;
        building.state = IDLE;
        OnBuildingUpgradeComplete(playerId, building);
    });
}
```

### 8.2 资源系统

四种基本资源：粮食、木材、铁矿、石料 + 高级资源：铜币、玉璧

```cpp
struct PlayerResources {
    int64_t food;
    int64_t wood;
    int64_t iron;
    int64_t stone;
    int64_t gold;     // 付费货币
    
    // 产量 = 基础产量 × (1 + 科技加成 + 联盟加成 + VIP加成)
    int64_t food_production_per_hour;
    
    // 保护量（低于此值不会被掠夺）
    int64_t food_protection;
    
    // 上限
    int64_t food_capacity;
};

// 资源产出 — 懒计算（不需要每秒 tick）
int64_t GetCurrentFood(Player& player, uint32_t now) {
    uint32_t elapsed = now - player.last_resource_update;
    int64_t produced = player.food_production_per_hour * elapsed / 3600;
    int64_t current = std::min(player.food + produced, player.food_capacity);
    return current;
}

// 只在需要时才"结算"：消耗资源、被攻击、查询
void SettleResources(Player& player, uint32_t now) {
    player.food = GetCurrentFood(player, now);
    player.wood = GetCurrentWood(player, now);
    // ...
    player.last_resource_update = now;
}
```

### 8.3 野外资源点采集

```
行军到达资源点 → 进入 GATHERING 状态 → 按速率采集 → 采满/被攻击/召回 → 返城
```

```cpp
struct GatherSession {
    uint64_t army_id;
    uint32_t resource_tile_id;
    ResourceType type;
    int64_t total_capacity;      // 部队可携带总量
    int64_t gathered_amount;     // 已采集量
    int64_t gather_rate_per_sec; // 采集速度
    uint32_t start_time;
    TimerId finish_timer;
};

// 采集完成时间 = total_capacity / gather_rate_per_sec
// 如果资源点剩余量 < capacity，则提前完成
```

---

## 9. 武将与部队系统

### 9.1 武将

```cpp
struct General {
    uint32_t general_id;      // 唯一 ID
    uint32_t config_id;       // 武将配置表 ID（关羽/张飞/...）
    uint32_t level;
    uint32_t star;            // 星级 (1-5)
    Rarity rarity;            // 稀有度
    
    // 属性
    int32_t base_atk;
    int32_t base_def;
    int32_t base_spd;
    int32_t base_int;  // 智力
    
    // 技能 (最多 1 主动 + 2 被动/追击/指挥)
    std::array<SkillSlot, 3> skills;
    
    // 兵种适性
    int32_t infantry_aptitude;
    int32_t cavalry_aptitude;
    int32_t archer_aptitude;
    
    // 兵书
    std::array<uint32_t, 3> war_books;
};
```

### 9.2 部队编制

```cpp
struct ArmyComposition {
    uint32_t general_ids[3];     // 主将 + 副将1 + 副将2
    TroopType troop_type;        // 步兵/骑兵/弓兵
    int32_t troop_count;         // 当前兵力
    int32_t max_troop_count;     // 最大兵力（由主城兵营等级决定）
};

// 征兵：消耗资源 + 时间，补充兵力
void Recruit(Player& player, ArmyComposition& army, int32_t count) {
    auto cost = CalculateRecruitCost(army.troop_type, count);
    if (!ConsumeResources(player, cost)) return;
    
    uint32_t duration = CalculateRecruitTime(count, player.barracks_level);
    SetTimer(duration, [&army, count]() {
        army.troop_count = std::min(army.troop_count + count, army.max_troop_count);
    });
}
```

### 9.3 技能系统

```cpp
enum SkillTrigger {
    ACTIVE,         // 主动技能（每回合释放一次）
    PASSIVE,        // 被动（满足条件自动触发）
    PURSUIT,        // 追击（普攻后触发）
    COMMAND,        // 指挥（战斗开始时施加 buff）
};

struct Skill {
    uint32_t skill_id;
    SkillTrigger trigger;
    TargetPolicy target_policy;   // ENEMY_MIN_HP / ENEMY_MAX_ATK / RANDOM / SELF / ALL_ENEMIES
    int32_t damage_coefficient;   // 伤害系数 (百分比)
    int32_t trigger_rate;         // 触发概率 (万分比)
    std::vector<BuffEffect> buffs; // 附带的 buff
};
```

---

## 10. 联盟系统

### 10.1 核心数据

```cpp
struct Alliance {
    uint32_t alliance_id;
    std::string name;
    uint32_t leader_id;
    std::vector<AllianceMember> members;  // 最大 200 人
    
    // 领地
    std::unordered_set<uint32_t> territory_tiles;  // 拥有的格子
    
    // 科技
    std::map<uint32_t, uint32_t> tech_levels;  // tech_id → level
    
    // 外交
    std::set<uint32_t> ally_alliances;   // 同盟
    std::set<uint32_t> enemy_alliances;  // 敌对
    
    // 集结
    std::vector<RallySession> active_rallies;
};
```

### 10.2 联盟集结（Rally）

```
发起集结 → 联盟成员加入 → 倒计时结束 → 合并部队 → 进攻

技术要点：
1. Rally 是有时限的（如 5 分钟倒计时）
2. 参与者的部队在集结点"停靠"
3. 倒计时结束后所有部队合并为一支大军出发
4. 战斗时按加入顺序轮流上阵（或合并属性）
```

### 10.3 领地系统

```cpp
// 领地规则：
// 1. 占领一个格子需要与己方领地相邻
// 2. 领地连通性：如果中间格被攻占，可能造成领地断裂
// 3. 要塞是领地核心，要塞被破则丢失关联领地

void OccupyTile(uint32_t allianceId, uint32_t tileId) {
    // 检查相邻是否有己方领地
    if (!HasAdjacentTerritory(allianceId, tileId)) return;
    
    tile.owner_id = /* 占领者 */;
    tile.alliance_id = allianceId;
    alliance.territory_tiles.insert(tileId);
    
    // 更新连通性
    RecalcTerritoryConnectivity(allianceId);
}
```

---

## 11. 赛季与排行榜

### 11.1 赛季生命周期

```
开服 → 保护期(3天) → 自由发展期(1周) → 占领州郡期 → 赛季高潮(巅峰对决) → 赛季结算 → 关服
```

| 阶段 | 时长 | 特点 |
|------|------|------|
| 保护期 | 3天 | 新手保护，不可被攻击 |
| 发展期 | 7天 | 开放攻击，但州郡未开放 |
| 州郡争夺 | 数周 | 攻占州郡城池，联盟竞争 |
| 巅峰对决 | 1-2周 | 州府/洛阳争夺 |
| 结算 | — | 排名奖励，赛季结束 |

### 11.2 排行榜

```
使用 Redis Sorted Set:

ZADD rank:power {score} {player_id}
ZADD rank:kills {score} {player_id}
ZADD rank:alliance_power {score} {alliance_id}

ZREVRANGE rank:power 0 99 WITHSCORES   -- Top 100
ZREVRANK rank:power {player_id}         -- 我的排名
```

### 11.3 赛季结算

```python
# 赛季结束流程
def season_end():
    # 1. 冻结地图（只读）
    freeze_map()
    
    # 2. 计算排名奖励
    rewards = calculate_season_rewards()
    
    # 3. 发放奖励（邮件）
    for player_id, reward in rewards:
        send_mail(player_id, "赛季奖励", reward)
    
    # 4. 归档数据
    archive_battle_reports()  # 战报 → S3 冷存储
    archive_rankings()        # 排行榜 → MySQL
    
    # 5. 玩家核心数据迁移到新赛季
    migrate_player_data()  # 武将、技能保留；资源/领地重置
    
    # 6. 旧 MapService 可以关闭
    shutdown_map_service()
```

---

## 12. 跨服战

### 12.1 架构

```
服务器 A 的玩家 ──────┐
                      │  部队快照复制
服务器 B 的玩家 ──────┼──→ 临时战场服务器（独立 MapService + 独立地图）
                      │
服务器 C 的玩家 ──────┘
                      
关键原则：
1. 战场服务器完全独立，有自己的 MapService
2. 参战部队是快照复制，非引用（隔离性）
3. 战场独立运行，结果通过消息队列同步回原服
4. 绝不让两个 MapService 直接通信
```

### 12.2 流程

```
1. 报名 → 各服收集参战玩家/联盟
2. 开始 → 创建临时战场服务器，导入部队快照
3. 战斗 → 在战场服务器上正常进行
4. 结束 → 结果发送到各原服：奖励、伤亡同步
5. 销毁 → 临时战场服务器关闭
```

---

## 13. 网络层与协议

### 13.1 客户端-Gate 协议

```
包头格式 (变长):
[body_size: 4 bytes] [message_id: 4 bytes] [protobuf body: N bytes]

或者使用 TLV:
[length: 4 bytes] [type: 2 bytes] [sequence: 4 bytes] [body: N bytes]
```

### 13.2 消息分类

| 类型 | 说明 | 示例 |
|------|------|------|
| Request/Response | 客户端请求，服务端回复 | 出兵、建造、查询 |
| Push | 服务端主动推送 | 被攻击通知、行军更新、聊天消息 |
| Heartbeat | 心跳保活 | 客户端每 30s 发一次 |

### 13.3 内部通信

| 路径 | 方式 | 原因 |
|------|------|------|
| Gate ↔ MapService | gRPC | 请求-响应，低延迟 |
| MapService → BattleService | Kafka / gRPC | 战斗请求可异步 |
| 跨服务广播 | Kafka | 解耦，异步 |
| Gate 控制消息 | Kafka topic `gate-{id}` | 按项目现有架构 |

### 13.4 协议压缩

- Protobuf 序列化（默认）
- 大包（>1KB）启用 zstd/lz4 压缩
- 地图增量推送（Delta Update），不发全量快照

---

## 14. 数据存储与持久化

### 14.1 存储选型

| 数据类型 | 存储 | 原因 |
|---------|------|------|
| 玩家数据（热） | Redis | 低延迟读写 |
| 地图格子状态 | 内存（MapService） | 超高频访问 |
| 行军数据 | 内存 + Redis WAL | 需要故障恢复 |
| 战报 | MySQL (blob) | 长期存储，按 battle_id 查询 |
| 邮件 | MySQL | 持久化 |
| 排行榜 | Redis Sorted Set | O(log N) 更新 |
| 联盟数据 | Redis + MySQL | 热数据 Redis，冷数据 MySQL |
| 配置表 | 内存（启动加载） | 只读 |

### 14.2 持久化策略

```
Write-Behind (异步写入):
┌──────────────────────────────────────────────────┐
│ 内存修改 → dirty flag → 定时批量 flush → Redis/MySQL │
│                                                      │
│ 关键操作（战斗结算）→ 立即写 WAL → 异步写 MySQL       │
└──────────────────────────────────────────────────┘
```

### 14.3 行军崩溃恢复

```cpp
// 启动时恢复所有行军中的部队
void RecoverMarches() {
    auto marches = LoadMarchesFromRedis(); // {army_id, path, start_time, speed, state}
    uint32_t now = CurrentTime();
    
    for (auto& march : marches) {
        if (march.state != MARCHING) continue;
        
        uint32_t currentTile = GetCurrentTile(march, now);
        if (currentTile == march.path.back()) {
            // 已过到达时间 → 立即执行到达逻辑
            ExecuteArrival(march);
        } else {
            // 未到达 → 重新注册剩余路径的 timer 和碰撞检测
            int currentIndex = FindPathIndex(march, currentTile);
            RegisterRemainingPath(march, currentIndex);
        }
    }
}
```

**关键洞察：懒计算模型 = 无中间状态丢失。** 任何时间点都能从公式恢复精确位置。

---

## 15. 运维与部署

### 15.1 单服部署

```yaml
# docker-compose 示例
services:
  gate:
    image: slg-gate:latest
    ports:
      - "8001:8001"    # 客户端连接
    deploy:
      replicas: 2      # 2 个 Gate
  
  map-service:
    image: slg-map:latest
    deploy:
      replicas: 1      # ⚠️ 单实例，不可水平扩展
    mem_limit: 2G
  
  battle-service:
    image: slg-battle:latest
    deploy:
      replicas: 4      # 无状态，按需扩展
  
  login:
    image: slg-login:latest
    deploy:
      replicas: 2
  
  redis:
    image: redis:7
    mem_limit: 4G
  
  mysql:
    image: mysql:8
  
  kafka:
    image: kafka:3
```

### 15.2 容量规划

| 组件 | CPU | 内存 | 数量 |
|------|-----|------|------|
| Gate | 2核 | 1GB | 2 |
| MapService | 4核 | 4GB | 1 |
| BattleService | 2核 | 512MB | 2-4 |
| Redis | 2核 | 4GB | 1 主 + 1 从 |
| MySQL | 4核 | 8GB | 1 主 + 1 从 |
| Kafka | 2核 | 2GB | 3 broker |

单服总需求：~20 核 20GB → 一台 32C64G 服务器可跑 **2-3 个服**。

### 15.3 滚动更新

```
SLG 特殊性：MapService 是有状态单实例，不能滚动更新
方案：
1. 公告维护时间
2. 保存 MapService 完整状态到持久化（Redis/文件）
3. 停止旧 MapService
4. 启动新 MapService，从持久化恢复
5. 恢复时间 < 1 分钟（加载 200-300MB 数据）
```

---

## 16. 安全与反作弊

| 威胁 | 防御 |
|------|------|
| 地图透视（看穿迷雾） | 服务端视野权限过滤，无视野数据不下发 |
| 加速行军 | 服务端计算/验证行军速度 |
| 伪造战斗结果 | 战斗完全在服务端执行 |
| 资源无限 | 所有资源变更服务端验证 |
| 重放攻击 | 请求时间戳 + 序列号 |
| 脚本/多开 | 操作频率检测 + 行为分析 |
| 协议篡改 | 连接 Token (HMAC) + 关键操作签名 |
| 内存修改 | 服务端权威，客户端只是显示层 |

**核心原则：Server is the single source of truth. Client sends intent, not result.**

---

## 17. 性能优化

### 17.1 总览

| 优化点 | 策略 | 效果 |
|--------|------|------|
| 行军位置 | 懒计算（公式） | 省去 50K 行军逐帧 tick |
| 视野更新 | 滑动窗口增量 | O(视野边界) 而非 O(视野面积) |
| 地图并发 | Region 分区 | 减少全局锁竞争 |
| 战斗 | 异步 Worker Pool | 不阻塞主循环 |
| 资源产出 | 懒结算 | 无需每秒 tick |
| 定时器 | Hashed Timing Wheel | O(1) 插入/删除 |
| 推送 | Delta Update | 减少带宽 |
| 寻路 | JPS + HPA* + 缓存 | 3ms 一次寻路 |
| 建筑/资源 timer | 超长 timer 存 DB | 减少内存 timer 数量 |

### 17.2 Hashed Timing Wheel

```
SLG 中大量定时事件：行军到达、建筑完成、采集完成、保护期结束...

传统 heap timer: O(log N) insert/delete
Timing Wheel:    O(1) insert/delete

配置：
- 100ms 一个 slot
- 3600 slots/wheel = 6 分钟一圈
- 超过 6 分钟的事件放 overflow list
- 超过数小时的（建筑升级）存 DB，不占内存 timer
```

### 17.3 热点优化

```
热点场景：赛季末期洛阳争夺战，数百支军队涌向同一格子

解决方案：
1. 高频战斗 → BattleService worker pool 扩容
2. 围城队列化 → SiegeSession 管理有序处理
3. 视野更新合并 → 同一 Region 的视野变更批量推送
4. 客户端限流 → 大规模战斗时降低推送频率
```

---

## 18. GM 工具与运营支撑

### 18.1 必备 GM 功能

| 功能 | 说明 |
|------|------|
| 封禁/解封玩家 | 登录封禁、聊天禁言 |
| 查询玩家数据 | 资源、武将、行军状态全景查看 |
| 修改资源 | 加金/减金、补发道具 |
| 查战报 | 按 battle_id / player_id 查询 |
| 封禁物品/货币 | BlockItem / BlockCurrency |
| 批量回收 | BatchRecallItems (dry_run 模式) |
| 公告发布 | 全服/指定服滚动公告 |
| 服务器状态 | 在线人数、行军数、内存/CPU |
| 赛季管理 | 开赛/结赛/维护 |
| 日志追踪 | 交易日志、异常检测 |

### 18.2 异常检测

```
AnomalyDetector: 滑动窗口速率监控
- 每玩家每资源类型：maxCountPerWindow, maxAmountPerWindow
- 超阈值 → Kafka 告警 → 运营后台/企业微信通知
- 可配置自动封禁
```

---

## 19. 离线玩家与推送

### 19.1 离线攻击流程

```
攻击者 → 行军 → 到达离线玩家城池 → 正常战斗流程
    │
    ▼
战斗结算 → 减兵/扣资源 → 生成战报
    │
    ├─ 攻击者在线 → 立即推送战报
    │
    └─ 防守者离线 → 
        ├─ 战报存入邮件/收件箱
        ├─ 推送通知（APNs/FCM）
        └─ 下次上线时拉取所有待处理战报
```

### 19.2 内存管理

```
所有玩家（无论在线/离线）的地图实体**永驻内存**：
- 城池位置、等级、视觉信息
- 部队驻防状态
- 资源点占领信息

但离线玩家的**详细数据**可以不常驻：
- 武将详情 → Redis (按需加载)
- 背包 → Redis (按需加载)
- 邮件 → MySQL (查询时拉取)

即：地图层面的"壳"常驻，业务层面的"内容"按需加载。
```

---

## 20. 机器人与压测

### 20.1 压测场景

| 场景 | 目标 | 关键指标 |
|------|------|----------|
| 登录洪峰 | 5000人同时登录 | 登录延迟 P99 < 3s |
| 大规模行军 | 50K 并发行军 | CPU < 70%, 内存 < 80% |
| 攻城战 | 200 支军队攻打同一城池 | 战斗处理延迟 < 5s |
| 地图拖拽 | 10K 玩家同时拖拽地图 | 视口响应 < 200ms |
| 聊天洪峰 | 世界频道 100条/秒 | 消息延迟 < 500ms |

### 20.2 机器人行为

```go
// 每个 goroutine 模拟一个玩家
func BotRoutine(playerId int) {
    client := ConnectGate()
    client.Login(playerId)
    
    for {
        action := PickRandomAction() // 按权重随机选择行为
        switch action {
        case "march":
            target := RandomEmptyTile()
            client.SendMarch(target)
        case "build":
            client.UpgradeRandomBuilding()
        case "gather":
            target := FindNearestResource()
            client.SendGather(target)
        case "chat":
            client.SendChatMessage("test message")
        case "scroll_map":
            region := RandomRegion()
            client.RequestViewport(region)
        }
        
        time.Sleep(RandDuration(1*time.Second, 10*time.Second))
    }
}
```

---

## 附录 A：技术栈推荐

| 层 | 推荐技术 |
|----|----------|
| 语言 | C++ (MapService/BattleService), Go (微服务), Java (运营后台) |
| 网络框架 | muduo (C++ TCP), go-zero (Go gRPC) |
| 序列化 | Protobuf |
| 消息队列 | Kafka |
| 缓存 | Redis |
| 数据库 | MySQL |
| 服务发现 | etcd |
| 部署 | Docker + Kubernetes |
| 监控 | Prometheus + Grafana |
| 日志 | ELK (Elasticsearch + Logstash + Kibana) |

## 附录 B：开发里程碑

| 阶段 | 内容 | 周期 |
|------|------|------|
| M1 基础框架 | Gate + MapService + 大地图加载 + 基础协议 | 2 个月 |
| M2 核心玩法 | 行军 + 寻路 + 战斗 + 视野 | 3 个月 |
| M3 经济系统 | 建筑 + 资源 + 武将 + 征兵 | 2 个月 |
| M4 社交系统 | 联盟 + 聊天 + 邮件 | 1.5 个月 |
| M5 运营系统 | GM 工具 + 排行榜 + 赛季 | 1.5 个月 |
| M6 联调压测 | 机器人 + 压测 + 性能优化 | 2 个月 |
| M7 跨服战 | 跨服战场 + 赛季末玩法 | 2 个月 |
| **总计** | — | **~14 个月** |
