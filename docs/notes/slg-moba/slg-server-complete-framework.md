# 率土之滨 SLG 游戏服务器 —— 完整框架设计

**Updated:** 2026-04-02

---

## 目录

1. [游戏特征分析](#1-游戏特征分析)
2. [整体服务架构](#2-整体服务架构)
3. [大地图系统](#3-大地图系统)
4. [行军系统](#4-行军系统)
5. [战斗系统](#5-战斗系统)
6. [迷雾/视野系统](#6-迷雾视野系统)
7. [建筑/城建系统](#7-建筑城建系统)
8. [武将/兵种系统](#8-武将兵种系统)
9. [联盟系统](#9-联盟系统)
10. [赛季系统](#10-赛季系统)
11. [跨服/合服](#11-跨服合服)
12. [数据层设计](#12-数据层设计)
13. [网络与协议](#13-网络与协议)
14. [定时器与调度](#14-定时器与调度)
15. [反作弊与安全](#15-反作弊与安全)
16. [运维与灾备](#16-运维与灾备)
17. [性能优化总纲](#17-性能优化总纲)
18. [开发工具链](#18-开发工具链)

---

## 1. 游戏特征分析

### 1.1 率土之滨的核心玩法

| 特征 | 说明 |
|------|------|
| 类型 | 大地图 SLG（策略战争） |
| 地图 | 1200×1200 格子，全服共享一张大地图 |
| 核心循环 | 占地 → 建设 → 征兵 → 行军 → 战斗 → 扩张 |
| 战斗 | 回合制自动战斗，确定性模拟 + 战报回放 |
| 联盟 | 联盟是核心组织单位，联盟战争是终局内容 |
| 赛季制 | 每赛季 ~3个月，赛季结束数据归档重开 |
| 在线规模 | 单服 5000~10000 人同时在线 |
| 离线交互 | 离线玩家的城池仍可被攻击 |

### 1.2 与 MMORPG 的核心差异

| 维度 | MMORPG | SLG（率土之滨） |
|------|--------|----------------|
| 地图 | 多个小副本/场景 | 一张巨型共享地图 |
| 坐标系 | 连续浮点坐标 | 离散格子坐标 |
| 同步模型 | 帧同步/状态同步 | 事件驱动 + 懒计算 |
| AOI | 实体中心（谁在我附近） | 格子中心（我能看到哪些格子） |
| 战斗 | 实时动作 | 回合制自动 + 战报 |
| 在线要求 | 强在线 | 离线也产生交互 |
| 状态变化频率 | 极高（60fps） | 低频（秒~分钟级事件） |

### 1.3 技术挑战排序

1. **大地图内存与并发** — 百万格子、万级行军并存
2. **行军碰撞检测** — 无逐帧 tick 下发现路径交叉
3. **战斗确定性** — 跨平台一致性、战报可回放
4. **迷雾视野** — 实时增量推送、海量玩家订阅
5. **联盟大规模协同** — 集结、全服广播、排名
6. **赛季生命周期** — 归档、迁移、重开
7. **离线玩家交互** — 不在线也要能被攻击/访问

---

## 2. 整体服务架构

### 2.1 服务拓扑

```
                     ┌─────────────┐
                     │   Client    │
                     └──────┬──────┘
                            │ TCP/WebSocket
                     ┌──────▼──────┐
                     │  GateService │  (连接管理, 协议路由, 频控)
                     └──────┬──────┘
                            │
              ┌─────────────┼─────────────┐
              │             │             │
       ┌──────▼──────┐ ┌───▼────┐ ┌──────▼──────┐
       │ MapService   │ │BattleSvc│ │ AllianceSvc │
       │ (大地图主服务) │ │(战斗计算)│ │  (联盟管理)  │
       └──────┬──────┘ └────────┘ └─────────────┘
              │
    ┌─────────┼──────────┐
    │         │          │
┌───▼──┐ ┌───▼──┐ ┌─────▼────┐
│BuildSvc│ │ArmySvc│ │ ChatSvc  │
│(建筑) │ │(部队) │ │  (聊天)  │
└───────┘ └───────┘ └──────────┘
              │
       ┌──────▼──────┐
       │ Data Layer   │
       │ Redis + MySQL │
       │ + Kafka       │
       └──────────────┘
```

### 2.2 各服务职责

| 服务 | 状态性 | 水平扩展 | 职责 |
|------|--------|---------|------|
| **GateService** | 有状态（连接） | 多实例 | TCP 连接管理，协议路由，心跳，频控 |
| **MapService** | **有状态（全地图）** | **不可水平扩** | 地图数据、行军、碰撞、视野、建筑占位 |
| **BattleService** | 无状态 | **可水平扩** | 纯确定性战斗计算，输入→输出函数 |
| **BuildService** | 有状态 | 单实例 | 建筑建造/升级队列、定时器 |
| **ArmyService** | 有状态 | 单实例 | 武将、兵种、招募、编队 |
| **AllianceService** | 有状态 | 单实例 | 联盟管理、外交、集结召集 |
| **ChatService** | 轻状态 | 可扩 | 聊天频道、消息分发 |
| **RankService** | 计算型 | 单实例 | 排行榜计算与缓存 |
| **DataService** | 无状态代理 | 可扩 | Redis/MySQL 路由，数据持久化 |
| **SchedulerService** | 调度型 | 单实例 | 赛季进度、全服事件、定时重算 |

### 2.3 MapService — 核心中的核心

MapService 是整个 SLG 服务器最关键的模块：

- **不可水平扩展**：一张大地图必须在一个进程中完整存在（否则行军跨进程极其复杂）
- **采用 Region 分区**：地图分 Region（如 32×32 = 1024 格子/Region），Region 之间用锁或 Actor 模型隔离
- **内存驻留**：所有地图数据常驻内存，包括离线玩家的城池
- **推送模型**：格子状态变化时主动推送给该格子的观察者

```
MapService 内部结构:
├── TileManager          — 1.44M 格子数据 (owner, building, terrain, resources)
├── MarchManager         — 活跃行军队列 (path, schedule, collision registry)
├── VisionManager        — 视野订阅 (per-tile observer counter)
├── RegionManager        — Region 分区与并发隔离
├── CollisionDetector    — 时空碰撞检测
├── ResourceManager      — 资源点状态与采集管理
└── EventBus             — 内部事件分发 (tile_changed, march_arrived, vision_entered)
```

### 2.4 单服 vs 微服务

率土之滨这类 SLG 的核心逻辑（MapService）天然是**单体有状态的**。最佳实践：

- **核心状态服务（Map + Build + Army）合并为一个进程**：减少网络开销，简化事务一致性
- **计算密集型（Battle）独立为无状态服务**：可水平扩展
- **辅助服务（Chat、Rank、Alliance）可选独立**：降低核心进程复杂度
- **数据层（Redis / MySQL / Kafka）独立部署**

实际上很多 SLG 项目采用 "**大单体 + 少量微服务**" 架构，而不是纯微服务。

---

## 3. 大地图系统

### 3.1 地图数据结构

```cpp
struct Tile {
    uint32_t tile_id;          // = y * MAP_WIDTH + x
    TileType terrain;          // 平地/山地/河流/沼泽/资源点
    uint64_t owner_player_id;  // 0 = 无主
    uint32_t building_id;      // 0 = 无建筑
    uint8_t  building_level;
    uint8_t  resource_type;    // 0=无, 1=木, 2=铁, 3=粮, 4=石
    uint32_t resource_amount;
    // 视野相关（见 §6）
    // 行军占用相关（见 §4）
};

// 1200 × 1200 = 1,440,000 tiles
// 每 Tile ~64-128 bytes → 总内存 ~90-180 MB
Tile tiles[MAP_HEIGHT][MAP_WIDTH];
```

### 3.2 Region 分区

```
地图 1200×1200，Region 大小 32×32
→ 38×38 = 1444 个 Region
→ 每个 Region 管理 1024 个格子

Region 用途:
1. 并发隔离：不同 Region 的操作可并行
2. 视野订阅单位：玩家订阅若干 Region 的变化
3. HPA* 寻路的 Chunk 单位
```

### 3.3 格子状态变更流程

```
事件（建筑完成/占领/行军经过）
  → TileManager.UpdateTile(tile_id, changes)
    → 更新内存中的 Tile 数据
    → VisionManager.NotifyTileChanged(tile_id, changes)
      → 遍历该 tile 的 observer 列表
      → 对每个在线观察者 push TileUpdate 消息
    → 持久化（dirty flag + 批量写入）
```

### 3.4 地图坐标系

```
格子坐标: (x, y)，0-indexed
tile_id = y * MAP_WIDTH + x
x = tile_id % MAP_WIDTH
y = tile_id / MAP_WIDTH

邻居（8方向）:
  (-1,-1) (0,-1) (1,-1)
  (-1, 0)        (1, 0)
  (-1, 1) (0, 1) (1, 1)

距离计算（Octile）:
  dx = abs(x1-x2), dy = abs(y1-y2)
  dist = max(dx,dy) + (√2-1) * min(dx,dy)
  整数近似: dist = max(dx,dy)*10 + min(dx,dy)*4  (×10 精度)
```

---

## 4. 行军系统

### 4.1 行军数据结构

```cpp
struct March {
    uint64_t army_id;
    uint64_t player_id;
    MarchState state;          // MARCHING, GATHERING, RETURNING, BATTLING
    std::vector<uint32_t> path; // 完整路径 tile_id 序列 (受行军距离上限约束)
    int64_t start_time_ms;     // 出发时间戳
    int32_t speed;             // 格/秒 × 1000 (毫秒精度)
    uint32_t current_path_idx; // 当前在 path 中的 index (懒计算)
    uint32_t dest_tile_id;
    MarchPurpose purpose;      // ATTACK, REINFORCE, GATHER, SCOUT, RETURN
};

// 行军距离上限（非常重要的设计约束）:
// path.size() 不会无限大，受 max_march_range 约束
// 典型范围: 初始 ~20 格，满级/满科技 ~150 格
// 1200×1200 地图对角线 ~1700，但玩家永远达不到这个长度
```

### 4.2 行军距离上限 (March Range Limit)

**率土之滨的核心设计**: 玩家不能无限远行军，有一个 `max_march_range` 上限。

```
行军距离上限的来源:
├── 基础值: ~20 格 (新手)
├── 领地加成: 只能向自己领地/盟友领地相邻 N 格范围行军
├── 科技研究: "行军距离" 科技每级 +5 格
├── 武将技能: 某些武将被动增加行军距离
├── VIP 等级: VIP 等级加成
├── 道具/buff: 临时增加行军距离
└── 上限: ~100-150 格 (满级满科技)

验证 (服务端, 发起行军时):
  CalcMaxMarchRange(player):
    base = 20
    base += tech_level[TECH_MARCH_RANGE] * 5
    base += general_passive_bonus(army.main_general)
    base += vip_bonus[player.vip_level]
    base += buff_bonus(player, BUFF_MARCH_RANGE)
    return min(base, HARD_CAP)  // 硬上限 ~150-200

  ValidateMarch(player, path):
    if path.size() > CalcMaxMarchRange(player):
      return ERROR_MARCH_TOO_FAR  // 拒绝
    // 还要检查: 目的地是否在领地扩展范围内
    if not IsInMarchableArea(player, path.back()):
      return ERROR_OUT_OF_TERRITORY

领地规则 (率土之滨特色):
  - 只能向自己/盟友已占领土地的相邻格子行军
  - 不能跳过空白区域直接飞到远处
  - 因此实际可行军范围 = 领地边界向外延伸 max_march_range 格
  - 这天然限制了 path 长度：即使上限 150 格，实际很多行军只有 10-30 格
```

**性能意义**:

| 指标 | 无上限 (理论最坏) | 有上限 (实际) |
|------|-------------------|---------------|
| path.size() | ~1700 (对角线) | ≤ 150 |
| 单次碰撞检测 | ~1700 × N | ≤ 150 × N |
| occupant 注册 | ~1700 条 | ≤ 150 条 |
| 单行军内存 | ~7KB | ≤ 600B |
| 50K 行军总 path 内存 | ~340MB | ≤ 30MB |
| 视野事件数 | ~1700 | ≤ 150 |

行军距离上限同时解决了**游戏设计**（逼迫扩张领地才能远征）和**性能**（路径长度有界）两个问题。

### 4.3 懒计算位置模型 (核心！)

**不需要逐帧 tick！** 这是 SLG 和 MMORPG 最大的架构差异。

```
前置条件:
  行军注册时，A* 已算出完整路径并存储:
  path = [tile_3, tile_7, tile_11, tile_15, ...]   // 起点到终点的格子序列
  每个元素是一个 tile_id (tile_id = y * MAP_WIDTH + x)

位置公式:
  elapsed = now - start_time
  tiles_traveled = floor(elapsed * speed)
  current_tile = path[min(tiles_traveled, path.length - 1)]

  公式做的是"查表"而非寻路:
  - path[] 是预计算好的完整格子序列（包含起点和终点）
  - tiles_traveled 是"走过了几个格子"的下标偏移
  - path[tiles_traveled] 直接取出当前所在的格子 tile_id
  → 任何时刻 O(1) 算出当前位置，无需逐帧推进

客户端同步:
  服务器只发送 {path, start_time, speed}
  客户端自行插值显示行军动画
  仅在速度变化/召回时重新下发
```

#### Path 存储优化

格子地图上相邻格子只有 8 个方向，每步只需 3 bit，无需存完整 tile_id。

```
方案对比 (150 步路径):

1. 原始方案: vector<uint32_t>
   150 × 4B = 600 B
   优点: 简单, O(1) 随机访问 path[idx] 直接得到 tile_id
   缺点: 最大

2. 方向编码 (推荐): 起点 tile_id + vector<uint8_t> directions
   方向 0~7 只需 3 bit, 但用 uint8_t 存更简单:
   4B (起点) + 150 × 1B = 154 B  (节省 74%)

   enum Dir : uint8_t { N=0, NE=1, E=2, SE=3, S=4, SW=5, W=6, NW=7 };

   struct CompactPath {
       uint32_t start_tile;
       std::vector<uint8_t> dirs;

       // 还原第 idx 步的 tile_id:
       uint32_t tile_at(int idx) const {
           uint32_t t = start_tile;
           for (int i = 0; i < idx; i++)
               t = neighbor(t, dirs[i]);
           return t;
       }
   };

   // 懒计算配合: 缓存 current_tile + current_idx
   // 行军只往前走, 每次只需从缓存位置往前推 1 步 → O(1)

3. 位压缩方向: 起点 + 3 bit/步 紧凑排列
   4B + ceil(150 × 3 / 8) = 4 + 57 = 61 B  (节省 90%)
   缺点: 位操作代码复杂, 可读性差

4. RLE 方向 (路径直线段多时极优):
   起点 + [(direction, count), ...]
   直线 50 格 = 1 对 (2B) 而非 50B
   A* 输出通常方向变化少: 5~15 段
   → 4B + 15 × 2B = 34 B  (节省 94%)
   缺点: 弯曲路径退化; 随机访问需累加 count

5. JPS 跳点 (寻路输出直接存):
   只存 JPS 输出的跳点坐标, 跳点之间是直线/对角线
   150 步路径通常 5~15 个跳点
   → 15 × 4B = 60 B  (节省 90%)
   还原: 跳点间线性插值
   缺点: 需配合 JPS 寻路器; 地形代价不同时不能用 JPS
```

**推荐: 方案 2 (uint8_t 方向数组)** — 实现简单 + 效果好:
- 代码直观，不需要位操作
- 方向数组可直接用于视野滑动窗口更新 (取 dirs[i] 算新增/移除偏移)
- 懒计算只需缓存 `{current_tile, current_idx}`，每次推进 O(1)
- 碰撞检测注册时: 从 start_tile 顺序推导每步 tile_id，只遍历一次

**50K 行军内存对比:**

| 方案 | 单条 (150步) | 单条 (30步) | 50K 行军 (avg 30步) |
|------|-------------|------------|-------------------|
| uint32 数组 | 600 B | 120 B | 6 MB |
| uint8 方向数组 | 154 B | 34 B | **1.7 MB** |
| 3-bit 位压缩 | 61 B | 16 B | 0.8 MB |
| RLE 方向 | ~34 B | ~14 B | 0.7 MB |

有行军距离上限后平均路径只有 10~30 步, 方案 2 已经做到 **50K × ~34B ≈ 1.7 MB**。
进一步压缩收益极小, 不值得增加代码复杂度。

### 4.4 时空碰撞检测 (Spatiotemporal Collision)

**问题**：没有逐帧 tick，如何检测两支军队在路径上相遇？

**解决方案**：在行军注册时预计算碰撞。

```
每个格子维护经过的部队时间窗口:
occupants[tile_id] = [{army_id, enter_time, leave_time}, ...]

新行军注册:
  for each tile in path:
    my_enter = start_time + tile_index / speed
    my_leave = start_time + (tile_index + 1) / speed
    for each existing in occupants[tile]:
      if is_enemy(existing.army_id, new_army_id):
        if my_enter < existing.leave_time && existing.enter_time < my_leave:
          collision_time = max(my_enter, existing.enter_time)
          schedule_battle(collision_time, tile, new_army_id, existing.army_id)
          break  // 只触发最早的碰撞
```

**复杂度**：O(path_length × avg_occupants_per_tile)，注册时一次性完成，无运行时开销。

### 4.5 召回/变速处理

召回和变速本质上是同一类操作：**销毁旧行军 + 创建新行军**。不存在"修改正在行军的 March 对象"的操作。

#### 为什么不能原地修改？

懒计算模型依赖 `{path, start_time, speed}` 三元组不变：
- `occupants[tile]` 里记录的时间窗口是根据旧三元组算的
- 已注册的定时器（碰撞、到达、视野）也绑定旧时间
- 如果原地改 speed，所有已注册数据全部失效 → 不如销毁重建

#### 4.5.1 召回流程

```
MarchManager.RecallMarch(army_id):

  // ① 确定当前位置
  march = marches[army_id]
  elapsed = now - march.start_time
  tiles_traveled = floor(elapsed * march.speed)
  current_idx = min(tiles_traveled, march.path.length - 1)
  current_tile = march.path[current_idx]

  // ② 清除旧行军在未来格子的 occupant 记录
  //    只需清除 current_idx+1 到 path.end 的格子
  //    已经走过的格子 (0..current_idx) 的 occupant 已自然过期
  for i in [current_idx + 1, path.length):
    occupants[path[i]].remove(army_id)

  // ③ 取消所有已注册的定时器
  //    包括: 到达事件、碰撞事件、视野进出事件、超时定时器
  cancel_timers(army_id)    // 一个 army 可能有几十到上百个定时器

  // ④ 标记旧 March 为 CANCELLED（不复用）
  march.state = CANCELLED

  // ⑤ 生成返程路径
  //    方案 A (简单): 反转已走过的路径 reverse(path[0..current_idx])
  //    方案 B (精确): 从 current_tile A* 重新寻路到 home_tile
  //    率土之滨用方案 A: 来时的路保证在领地内, 反转即可
  return_path = reverse(march.path[0..current_idx])

  // ⑥ 创建新行军 (RETURN 状态)
  new_march = CreateMarch(
    army_id   = army_id,
    path      = return_path,
    start_time = now,
    speed     = march.speed * return_speed_multiplier,  // 有些游戏回城加速
    state     = RETURNING,
    purpose   = RETURN
  )

  // ⑦ 新行军走完整注册流程
  //    → 注册 occupant 时间窗口
  //    → 碰撞检测 (返程路上也可能撞到敌军!)
  //    → 注册定时器 (到达、视野)
  RegisterMarch(new_march)

  // ⑧ 通知客户端
  send_to_client(army_id.player, MarchRecalled{
    army_id, return_path, now, new_march.speed
  })
```

**关键细节**：
- 步骤 ②③ 必须在 ⑥⑦ 之前完成，否则新旧 occupant 会冲突
- 返程也要做碰撞检测 — 敌军在你回家路上也能拦截你
- `return_speed_multiplier` 是游戏设计参数（率土之滨返程速度 = 去程速度）

#### 4.5.2 变速处理（加速道具 / buff）

```
MarchManager.ChangeSpeed(army_id, new_speed):

  // 与召回一样: 定位当前位置
  march = marches[army_id]
  elapsed = now - march.start_time
  current_idx = min(floor(elapsed * march.speed), march.path.length - 1)

  // 清除 remaining occupant + 取消 remaining 定时器
  for i in [current_idx + 1, path.length):
    occupants[path[i]].remove(army_id)
  cancel_timers(army_id)   // 只取消未来的定时器

  // 创建新 March: 路径 = 剩余路径, 新速度
  remaining_path = march.path[current_idx .. end]
  new_march = CreateMarch(
    army_id    = army_id,
    path       = remaining_path,
    start_time = now,            // 关键: 以现在为新起点
    speed      = new_speed,
    state      = march.state,
    purpose    = march.purpose
  )

  // 完整注册 (occupant + 碰撞检测 + 定时器)
  RegisterMarch(new_march)

  // 通知客户端重新插值
  send_to_client(army_id.player, MarchSpeedChanged{
    army_id, remaining_path, now, new_speed
  })
```

**对比召回和变速**：

| 步骤 | 召回 | 变速 |
|------|------|------|
| 定位当前位置 | 相同 | 相同 |
| 清除旧 occupant/timer | 相同 | 相同 |
| 新路径 | reverse(已走路径) | 剩余路径不变 |
| 新 start_time | now | now |
| 新 speed | 可能有返程加速 | 新速度值 |
| 碰撞重检测 | 是 | 是 |
| 客户端通知 | MarchRecalled | MarchSpeedChanged |

本质上两者都是 **"在当前位置切一刀，旧行军销毁，新行军重建"**。

#### 4.5.3 边界情况

```
1. 召回瞬间刚好碰撞:
   if march.state == BATTLING:
     return ERROR_CANNOT_RECALL_IN_BATTLE

2. 连续多次加速:
   每次加速都是完整的销毁+重建
   性能: O(remaining_path × occupants) 每次
   频率: 极低（加速道具有 CD），不是性能瓶颈

3. 行军刚出发就召回 (current_idx == 0):
   return_path = [home_tile]   // 原地返回，长度 1
   RegisterMarch 会直接触发到达事件 → 军队立即回城

4. 行军已到达目的地 (state != MARCHING):
   如果在采集/驻防中, 召回走不同逻辑:
   → 先 cancel 采集/驻防
   → 然后从 dest_tile 直接寻路回城 (不是反转)

5. 服务器崩溃恢复后变速:
   与正常变速一样 — 公式算出当前位置, 然后销毁重建
   因为恢复时已经重建了所有 occupant/timer
```

### 4.6 行军事件时间线

```
行军注册时预先注册以下定时器:
├── 每经过一个格子的 enter_time → 视野 frontTile++
├── 每离开一个格子的 leave_time → 视野 tailTile--
├── 碰撞事件 → 触发战斗
├── 到达目的地 → 触发到达逻辑 (攻击/驻守/采集/侦察)
└── 最大行军时间 → 超时强制返回

注意：这些定时器都是一次性注册，不需要每帧检查。
```

### 4.7 行军持久化与崩溃恢复

```
持久化数据: {army_id, path, start_time, speed, state, purpose}
存储: Redis Hash + MySQL 备份

崩溃恢复:
  1. 加载所有 state=MARCHING 的记录
  2. 对每条记录用公式算当前位置
  3. 如果已过到达时间 → 立即执行到达逻辑
  4. 否则 → 重建剩余路径的 occupant 记录 + 重注册定时器
  5. 重新运行碰撞检测（可能发现恢复期间错过的碰撞）
```

---

## 5. 战斗系统

### 5.1 确定性战斗引擎

**核心设计原则**：战斗是纯函数，给定相同输入永远产生相同输出。

```cpp
// 纯函数接口
BattleReport SimulateBattle(
    const ArmySnapshot& attacker,
    const ArmySnapshot& defender,
    uint64_t seed,
    TerrainType terrain
);

// 确定性约束:
// 1. 不使用浮点数 — 所有乘除用整数百分比 (atk * 120 / 100)
// 2. 不使用系统时间、内存地址
// 3. 不使用无序容器（unordered_map 迭代顺序不确定）
// 4. 单一 PRNG，固定调用顺序
// 5. 固定处理顺序：按 slot 0,1,...,N 遍历
```

### 5.2 战斗流程

```
SimulateBattle():
  1. 初始化双方阵容 (3武将×3兵种，各有 HP/ATK/DEF/速度/技能)
  2. 确定先手 (根据速度排序，速度相同按固定规则)
  3. 循环回合 (最多 N 回合):
     a. 按先手顺序每个存活单位执行行动:
        - 检查被控状态 (眩晕/沉默)
        - 选择技能 (判断是否可释放: 冷却、怒气)
        - 选择目标 (按技能规则: 最低HP/随机/前排优先)
        - 计算伤害 = ATK * 技能倍率 / 100 * (1000 / (1000 + 目标DEF))
        - 判定暴击/闪避 (PRNG)
        - 应用 buff/debuff
        - 记录 BattleAction
     b. 检查胜负条件 (一方全灭)
  4. 超过最大回合 → 防守方胜
  5. 生成 BattleReport
```

### 5.3 ArmySnapshot (军队快照)

```protobuf
message ArmySnapshot {
    uint64 player_id = 1;
    uint64 army_id = 2;
    repeated GeneralSlot generals = 3;  // 最多3个武将槽位
}

message GeneralSlot {
    uint32 general_id = 1;
    uint32 level = 2;
    uint32 star = 3;
    int32  hp = 4;       // 当前HP (续战时可能非满)
    int32  max_hp = 5;
    int32  atk = 6;
    int32  def = 7;
    int32  spd = 8;
    int32  troop_count = 9;
    repeated SkillSnapshot skills = 10;
    repeated BuffSnapshot buffs = 11;
}
```

### 5.4 BattleReport 结构

```protobuf
message BattleReport {
    uint64 battle_id = 1;
    BattleInput input = 2;           // 完整输入（可用于验证重放）
    BattleResult result = 3;         // WIN_ATTACKER / WIN_DEFENDER
    repeated BattleRound rounds = 4;
    BattleSummary summary = 5;       // 伤亡、经验、掉落
}

message BattleRound {
    int32 round_number = 1;
    repeated BattleAction actions = 2;
}

message BattleAction {
    ActionType type = 1;        // NORMAL_ATTACK, SKILL, BUFF, DEBUFF, HEAL, DEATH
    int32 source_slot = 2;      // 发起者槽位
    int32 target_slot = 3;
    uint32 skill_id = 4;
    int32 damage = 5;           // 正=伤害，负=治疗
    int32 target_hp_after = 6;  // 目标当前HP (客户端直接用这个值)
    bool is_crit = 7;
    bool is_dodge = 8;
    repeated BuffEvent buff_events = 9;
}
```

**关键设计**：每个 action 内含 `target_hp_after`，客户端只需播放动画+显示血量，无需任何战斗逻辑。

### 5.5 多军团攻城 (Siege)

```
攻城流程:
  1. 多支军队按到达时间排队
  2. SiegeSession 管理整个攻城过程
  3. 每支进攻军队依次与守军战斗
  4. 守方每场战斗后:
     - 不完全恢复HP (消耗战特点)
     - 守军援军可随时加入 (重排队列)
  5. 守方全灭 → 城池易主
  6. 进攻方全灭 → 攻城失败

// 集结攻城 (Rally):
  1. 联盟成员接受集结号召
  2. 所有参与军队在集结点集合 (等待时间)
  3. 集结完成后统一出发
  4. 到达目标后按整体军力对抗
```

### 5.6 战斗性能

```
单次战斗模拟: < 1ms（纯 CPU 计算，无 IO）
万人同时战斗: 使用 BattleService worker pool
  - 无状态服务，水平扩展
  - 每个 worker 独立执行 SimulateBattle
  - MapService 发送 BattleRequest 到 BattleService 队列
  - BattleService 返回 BattleReport
  - MapService 应用战斗结果（HP 变更、城池归属）
```

---

## 6. 迷雾/视野系统

### 6.1 核心数据结构

```cpp
// 每个格子维护一个观察者引用计数表
struct TileVision {
    // key: player_id, value: observer_count（该玩家有多少个来源在观察此格子）
    std::unordered_map<uint64_t, int16_t> observers;
};

// 玩家能看到格子 ⟺ observers[player_id] > 0

// 预计算视野偏移表（固定半径）
// 例如视野半径=3，预计算所有 |dx|+|dy|<=3 的偏移
std::vector<std::pair<int,int>> vision_offsets;
// 预先排序，方便增量更新
```

### 6.2 视野来源

```
每个玩家的视野来源:
├── 城池 (主城 + 分城) — 固定位置，大视野半径
├── 行军部队 — 移动中，视野半径较小
├── 瞭望塔建筑 — 固定位置，增加额外视野
├── 侦察 — 临时单次揭露
└── 联盟共享视野 — 盟友视野可选择性共享
```

### 6.3 行军滑动窗口更新

```
行军经过路径 A → B → C → D → E，视野半径 = R

在格子 C 时：
  visible = all tiles within R of C

移动到 D 时（增量更新）：
  新增 visible: tiles within R of D but not within R of C (前方扇形)
  失去 visible: tiles within R of C but not within R of D (后方扇形)

实现:
  MarchVisionUpdate(old_tile, new_tile, player_id, radius):
    for offset in vision_offsets:
      old_abs = old_tile + offset
      new_abs = new_tile + offset
      if new_abs not in range(old_tile, radius):  // 进入视野
        tiles[new_abs].observers[player_id]++
        if observers[player_id] == 1:  // 从0变1，首次可见
          push_tile_reveal(player_id, new_abs)
      if old_abs not in range(new_tile, radius):  // 离开视野
        tiles[old_abs].observers[player_id]--
        if observers[player_id] == 0:  // 变0，不再可见
          push_tile_fog(player_id, old_abs)
```

### 6.4 视野推送优化

```
问题: 万人在线，每人视野 ~100-200 格子，推送量巨大

优化策略:
1. 批量推送: 同一 tick 内的多个格子变化合并为一个消息
2. Region 订阅: 客户端只订阅当前视口覆盖的 Region
3. 增量推送 (delta): 只推送变化的格子，不推送完整快照
4. 压缩: tile_id 用 varint，重复字段用 run-length encoding
5. 频控: 非关键视野变化（资源轻微变化）降频推送
6. 离线玩家不推: observer 存在但 session 不在线则跳过
```

### 6.5 客户端拖动地图

```
客户端拖动地图请求流程:
1. 客户端发送 ViewportRequest{center_x, center_y, width, height}
2. 服务器检查权限: 请求区域是否在玩家视野范围内
3. 合法区域 → 返回 Region 级别的地图数据
4. 非法区域（超出视野）→ 返回迷雾标记
5. 地形数据（不变）→ 客户端本地缓存，服务器不重复发

Rate limit: 每秒最多 N 次 viewport 请求
```

---

## 7. 建筑/城建系统

### 7.1 建筑类型

```
建筑类型:
├── 主城 (唯一，占 3×3 格子，不可移动)
├── 资源建筑 (农田/伐木场/矿山/采石场)
├── 军事建筑 (兵营/训练场/瞭望塔/城墙)
├── 功能建筑 (仓库/市场/研究所)
├── 外部建筑 (占领格子上的建筑: 要塞/箭塔)
└── 特殊建筑 (联盟建筑: 盟主府/联盟仓库)
```

### 7.2 建筑队列

```cpp
struct BuildQueue {
    uint64_t player_id;
    uint32_t max_slots;        // 默认1，VIP可增加
    struct BuildTask {
        uint32_t building_id;
        uint32_t target_level;
        int64_t  start_time;
        int64_t  finish_time;
        bool     is_demolish;
    };
    std::vector<BuildTask> tasks;  // 当前进行中的建造
    // 加速: 使用道具减少剩余时间
    // 即时完成: 使用金币直接完成
};

// 建造完成 → 定时器触发 → 更新 Tile 数据 → 推送视野更新
// 升级效果: 资源产出+, 兵种解锁, 视野范围+, 容量+
```

### 7.3 资源系统

```
四种基础资源: 木材、铁矿、粮食、石材
+ 特殊资源: 金币（付费/任务）

资源产出:
  - 城内资源建筑: 按等级每小时固定产出
  - 采集: 派军队到资源点，按速度逐渐获取
  - 懒计算: 不逐秒更新资源量
    resource_now = resource_last + rate * (now - last_update_time)
    只在需要时 (消耗/查询) 才实际计算

资源保护:
  - 仓库容量上限 → 超过部分可被掠夺
  - 被攻击时按比例损失未保护资源
```

---

## 8. 武将/兵种系统

### 8.1 武将系统

```
武将属性:
├── 基础属性: 攻击力/防御力/智力/速度/统率
├── 成长: 等级(经验)、星级(突破)、觉醒
├── 技能: 主动技能(1) + 被动技能(多个)
│   ├── 技能等级可升级
│   └── 技能搭配形成"战法"组合
├── 兵种适性: 枪兵S/骑兵A/弓兵B 等
└── 羁绊: 特定武将组合获得额外加成

武将获取:
├── 抽卡 (gacha)
├── 活动赠送
├── 赛季排名奖励
└── 碎片合成
```

### 8.2 兵种系统

```
兵种克制:
  枪兵 → 骑兵 → 弓兵 → 枪兵 (三角克制)
  克制方 +20% 伤害, 被克制方 -20% 伤害

兵种特性:
├── 枪兵: 高防御，适合防守
├── 骑兵: 高速度，行军快，冲锋伤害
├── 弓兵: 高攻击，远程先手
└── 特殊兵种: 攻城车(对建筑+), 侦察骑(视野+)

征兵:
  - 消耗资源 + 时间
  - 兵营等级决定征兵上限和速度
  - 部队规模 = 武将统率力 × 兵种系数
```

### 8.3 编队

```
编队规则:
  - 每支军队最多 3 个武将
  - 主将决定行军速度和特殊技能
  - 前排/中排/后排位置影响被攻击概率
  - 兵种搭配影响整体效果

编队验证 (服务端):
  1. 武将不能同时在两支军队中
  2. 武将等级满足出征要求
  3. 兵力 > 0
  4. 行军队列未满
```

---

## 9. 联盟系统

### 9.1 联盟基础功能

```
联盟管理:
├── 创建/解散
├── 邀请/申请/审批
├── 职位: 盟主/副盟主/长老/精英/成员
├── 权限系统 (按职位分配权限)
├── 盟内聊天
├── 联盟公告/事件日志
└── 联盟科技 (全盟共享加成)

联盟外交:
├── 同盟 (互相标绿，共享视野，不可交战)
├── 敌对 (标红，优先战斗提示)
└── 中立 (默认)
```

### 9.2 联盟战争机制

```
集结 (Rally):
  1. 发起者选定目标和集结时间
  2. 盟友选择是否参与，派遣部队
  3. 所有参与部队在集结点集合
  4. 倒计时结束 → 统一出发

联盟攻城:
  - 目标: NPC 城池 / 敌方联盟领地
  - 持续攻城 → 消耗战
  - 守方联盟可增援
  - 耐久度系统: 建筑有耐久，被攻击后下降

赛季目标:
  - 攻占中央区域 / 特定城池
  - 联盟积分排名
  - 赛季奖励根据最终排名发放
```

### 9.3 联盟领地

```
联盟领地计算:
  - 联盟成员占领的格子形成联盟领地
  - 相邻格子连通 → 形成连片领地
  - 领地内可建造联盟建筑
  - 领地边界格子 → 前线

领地维护:
  - 定时检查领地连通性（BFS/Union-Find）
  - 飞地（不连通的领地）会逐渐失去
  - 核心区域加成更高
```

---

## 10. 赛季系统

### 10.1 赛季生命周期

```
Timeline:
Day 1        - 赛季开始，新地图初始化
Day 1~7      - 新手保护期 (不可PVP)
Day 7~30     - 早期发展期 (开放 PVP)
Day 30~60    - 中期争夺 (开放高级区域)
Day 60~80    - 后期决战 (开放中央区域)
Day 80~90    - 赛季结算 (最终排名固定)
Day 90       - 赛季结束

赛季进度由 SchedulerService 管理:
  - 按预设时间表推进阶段
  - 开放/关闭地图区域
  - 触发全服事件 (NPC 入侵、天灾等)
```

### 10.2 赛季结束处理

```
结算流程:
1. 冻结数据（只读模式）
2. 计算最终排名（个人 + 联盟）
3. 发放赛季奖励（邮件 + 赛季商店货币）
4. 归档数据:
   a. 热数据（排行榜、战争摘要）→ 归档数据库
   b. 战报 → 冷存储 (S3/OSS)
   c. 玩家核心数据 → 迁移到新赛季
5. 可选: 保留赛季回顾（历史战报查看）
6. 开始新赛季 → 初始化新地图

玩家数据迁移:
  保留: 武将(含等级星级)、部分资源、赛季通行证进度
  重置: 地图占领、建筑(保留等级上限)、兵力
  转化: 旧赛季排名 → 新赛季初始加成
```

### 10.3 赛季服务器管理

```
服务器分配:
  - 每个赛季 = 一个独立的 "服务器"
  - 新赛季开始时启动新的 MapService 实例
  - 旧赛季结束时关闭对应 MapService，保留 DB
  - 一台物理机可运行多个赛季实例

容量规划:
  - 开服: 高负载（万人涌入）→ 预分配更多资源
  - 中期: 稳定负载 → 正常配置
  - 末期: 玩家流失 → 缩容
  - 赛季间: 维护窗口
```

---

## 11. 跨服/合服

### 11.1 跨服战场

```
跨服战场设计:
  - 独立的临时 BattlefieldService（有自己的地图 + MapService）
  - 参战联盟从各自赛季服务器复制军队快照/数据快照到战场
  - 战场独立运行，所有逻辑不依赖原服
  - 战场结算后，结果同步回各原服
  - 原服和战场不直接 RPC，通过消息队列异步同步

跨服战场类型:
  ├── 联盟争霸赛 (8/16 联盟淘汰赛)
  ├── 全服混战 (多服联合地图)
  └── 赛季冠军赛 (各服冠军联盟对决)

关键原则: 两个 MapService 绝不直接通信，通过中间件隔离
```

### 11.2 合服策略

```
合服触发条件:
  - 赛季活跃人数低于阈值
  - 赛季末尾合并到更大的服务器

合服方案 (推荐):
  - player_id 不变（Snowflake 生成，不含 zone 信息）
  - 合并 DB 数据（Zone A 的 Redis → 导入 Zone B 的 Redis）
  - 更新路由映射表: player_id → new_zone_id
  - 业务代码零修改

合服注意事项:
  - 名字冲突: 合服后名字加 "[原服名]" 前缀
  - 排行榜重算
  - 联盟数据合并/保留
  - 邮件/聊天记录取舍
```

---

## 12. 数据层设计

### 12.1 数据分层

```
                热         ←——————→         冷
┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐
│  Memory  │  │  Redis   │  │  MySQL   │  │ S3/OSS   │
│ (地图/行军)│  │ (玩家数据)│  │ (持久化) │  │ (历史战报)│
└──────────┘  └──────────┘  └──────────┘  └──────────┘
  延迟: 0       延迟: <1ms    延迟: 1-10ms   延迟: 100ms+
```

### 12.2 内存数据（MapService 进程内）

```
常驻内存:
  - 全部 1.44M 格子数据 (~180MB)
  - 所有活跃行军 (~50K 条, 几十 MB)
  - 视野观察者表 (按需分配)
  - 建筑队列 (所有玩家)
  
非常驻:
  - 离线玩家详细数据 → Redis (按需加载)
  - 武将详情 → Redis
  - 战报 → DB
```

### 12.3 Redis 使用

```
Redis 数据模型:

// 玩家数据
player:{player_id}:profile     → Hash (名字/等级/经验)
player:{player_id}:resources   → Hash (木/铁/粮/石/金)
player:{player_id}:generals    → Hash (field=general_id, value=protobuf)
player:{player_id}:army        → List of Army protobuf
player:{player_id}:buildings   → Hash (field=building_id, value=protobuf)
player:{player_id}:mails       → Sorted Set (score=timestamp)

// 联盟数据
alliance:{alliance_id}:info    → Hash (名字/等级/成员数)
alliance:{alliance_id}:members → Set of player_ids

// 排行榜
rank:power       → Sorted Set (score=power, member=player_id)
rank:kills       → Sorted Set
rank:alliance    → Sorted Set

// Session
session:{player_id}            → String (gate_node_id, 带TTL)
```

### 12.4 MySQL 持久化

```
核心表设计:
  players          — 玩家基础信息
  player_resources — 资源数据 (定期快照)
  generals         — 武将数据
  armies           — 部队数据
  buildings        — 建筑数据
  alliances        — 联盟数据
  alliance_members — 成员关系
  marches          — 行军记录 (活跃 + 历史)
  battle_reports   — 战报 (protobuf blob)
  transaction_log  — 交易日志 (审计)
  map_tiles        — 地图格子持久化 (按 Region 批量)

写入策略:
  - 方案A (默认): 每次变更通过 Kafka → DB Service → MySQL
  - 方案B (高吞吐): DB Worker 内 batch flush (同 worker 保证串行)
  - 关键操作 (战斗结算): WAL 立即写
  - 地图数据: dirty Region 标记 + 定期批量 flush
```

### 12.5 Kafka 使用

```
Kafka Topics:
  battle_request    — MapService → BattleService (战斗请求)
  battle_result     — BattleService → MapService (战斗结果)
  db_task           — 各服务 → DBService (数据持久化)
  gate_command      — 各服务 → GateService (推送消息给客户端)
  anomaly_alert     — 异常检测告警
  alliance_event    — 联盟事件广播
  march_event       — 行军事件 (用于日志/统计)

同一玩家的消息用 player_id 作 key → 保证同 partition 内顺序
```

---

## 13. 网络与协议

### 13.1 客户端 ↔ 服务器协议

```
传输层: TCP (长连接) + TLS
应用层: 自定义二进制协议头 + Protobuf body

消息头:
  [4B total_length][2B msg_id][2B flags][protobuf body...]

flags:
  bit 0: compressed (zstd)
  bit 1: encrypted (AES-GCM beyond TLS, 可选)

心跳: 每 30s client → server PING, server → client PONG
超时: 90s 无心跳 → 断开

频控:
  - 按 msg_id 分类频控
  - 行军/建造 等操作型: 每秒 5 次
  - 地图浏览: 每秒 10 次
  - 聊天: 每秒 2 次
  - 超频 → 告警 → 封号
```

### 13.2 推送策略

```
推送类型:
├── 即时推送: 战斗结果、被攻击通知、聊天消息
├── 延迟批量推送: 视野更新 (50ms batch)、资源变化
├── 请求-响应: 查看玩家信息、打开界面
└── 周期推送: 排行榜更新 (每分钟)

大包优化:
  - 登录首包（全量数据）: 分批发送，优先发关键数据（主城+附近地图）
  - 地图数据: Region 级别分包，视口外不发
  - 战报: 只发摘要，详情按需请求
```

### 13.3 服务间通信

```
内部通信:
  同步 RPC: gRPC (请求 ↔ 响应，如战斗请求)
  异步消息: Kafka (事件通知，如战斗结果、数据持久化)

为什么混合：
  - gRPC: 需要立即返回的操作 (登录验证、数据查询)
  - Kafka: 不需要立即返回、需要可靠投递 (持久化、事件通知)

服务发现: etcd (服务注册 + 健康检查)
```

---

## 14. 定时器与调度

### 14.1 Hashed Timing Wheel

```
为什么不用最小堆:
  - 10 万个定时器（行军、建造、buff、cooldown）
  - 最小堆: insert O(log N), delete O(log N)
  - 时间轮: insert O(1), delete O(1), tick O(1)

配置:
  精度: 100ms per slot
  一圈: 3600 slots = 6 分钟
  长定时器 (>6min): overflow list → 每圈检查一次降级到主轮
  超长定时器 (>1h): 存 DB，不占内存

使用场景:
├── 行军到达 (秒~分钟级)
├── 建造完成 (分钟~小时级)
├── 征兵完成 (分钟级)
├── Buff/Debuff 过期 (秒级)
├── 视野 enter/leave 事件 (秒级)
├── 采集完成 (分钟~小时级)
└── 赛季阶段切换 (天级 → DB 存储)
```

### 14.2 定时任务调度

```
SchedulerService:
  - 赛季阶段切换 (Day N → 开放新区域)
  - 排行榜刷新 (每 5min 重算)
  - 资源结算 (每小时，但用懒计算所以通常不需要)
  - 联盟科技研究完成
  - 活动开始/结束
  - 日常重置 (每天 0:00)
  - 每周重置

实现: Cron 表达式 + DB 存储 + 幂等执行
```

---

## 15. 反作弊与安全

### 15.1 安全原则

**核心：服务器是唯一权威。客户端发送意图，不发送结果。**

### 15.2 威胁与防御

| 威胁 | 防御 |
|------|------|
| 地图透视 (看穿迷雾) | 服务器端视野权限过滤，未揭露格子不发数据 |
| 加速行军 | 服务器计算行军速度，客户端只做显示 |
| 伪造战斗结果 | 战斗完全服务端运行 |
| 资源篡改 | 所有资源变更服务端验证 |
| 重放攻击 | 请求时间戳 + 序列号 + 签名 |
| 多开/脚本 | 操作频率检测 + 行为分析 + 设备指纹 |
| 协议篡改 | TLS + 消息签名 |
| 内存修改 | 关键数据服务端为准，客户端只做缓存 |

### 15.3 异常检测

```
AnomalyDetector:
  - 滑动窗口速率监控 (per player per type)
  - 设置: maxCount / maxAmount / windowSeconds
  - 超阈值 → Kafka 告警 → GM 工具通知
  - 可配置自动封禁 (临时/永久)

GM 工具:
  - 冻结账号
  - 回收资源/物品
  - 查询交易流水
  - 批量修复 (exploit 影响范围)
```

---

## 16. 运维与灾备

### 16.1 部署架构

```
Kubernetes 集群:
├── mmorpg-infra (基础设施命名空间)
│   ├── etcd
│   ├── redis
│   ├── mysql
│   └── kafka
├── mmorpg-zone-{season_name} (每个赛季一个命名空间)
│   ├── gate (Deployment, 可扩)
│   ├── map-service (StatefulSet, 单实例)
│   ├── battle-service (Deployment, 可扩)
│   ├── alliance-service
│   ├── login
│   ├── data-service
│   └── scheduler
```

### 16.2 高可用

```
MapService (有状态, 最关键):
  - 主备模式 (Active-Standby)
  - 备机持续从 Kafka 消费事件日志，保持近实时副本
  - 主故障 → 备机提升 (< 30s 切换)
  - 或: 快速重启 → 从 Redis/DB 恢复状态 (< 2min)

无状态服务 (Gate, Battle):
  - 多副本 + 自动扩缩容 (HPA)
  - 任一副本故障 → K8s 自动替换

数据层:
  - Redis: Sentinel / Cluster (自动故障转移)
  - MySQL: 主从复制 + 半同步
  - Kafka: 3 副本，min.insync.replicas=2
```

### 16.3 数据备份与回滚

```
备份策略:
  - Redis: RDB 每 4h + AOF (秒级持久化)
  - MySQL: 每 6h 全量快照 + binlog 增量
  - 关键事件 (大规模战斗/赛季结算): 触发即时快照

回滚能力:
  - 单玩家回滚: 从快照恢复，重放交易日志
  - 区域回滚: 按 Region 恢复地图数据
  - 全服回滚: 最后手段，从 MySQL 备份恢复
```

### 16.4 监控指标

```
核心指标:
├── MapService
│   ├── event_processing_latency_p99
│   ├── active_marches_count
│   ├── tile_update_rate (per second)
│   ├── vision_push_rate
│   └── memory_usage
├── BattleService
│   ├── battle_queue_depth
│   ├── battle_latency_p99
│   └── throughput (battles/sec)
├── Gate
│   ├── online_players
│   ├── message_rate
│   └── connection_errors
├── Data Layer
│   ├── redis_hit_rate
│   ├── mysql_write_latency
│   ├── kafka_consumer_lag
│   └── kafka_partition_throughput
```

---

## 17. 性能优化总纲

| 领域 | 策略 | 效果 |
|------|------|------|
| 地图分区 | Region 独立处理，减少全局锁 | 并发度提升 38× |
| 懒计算 | 行军位置按需求算，不逐帧 tick | 50K 行军零 CPU 开销 |
| 增量视野 | 滑动窗口更新，不全量重算 | 推送量减少 95% |
| 异步战斗 | Worker Pool 队列，不阻塞主循环 | 主循环延迟 < 10ms |
| AOI | 玩家只订阅视口 Region | 推送量 ÷ 100 |
| 冷热数据 | 活跃行军内存；离线/历史走 DB | 内存 < 1GB |
| 定时器 | Hashed Timing Wheel | 10万定时器 O(1) 操作 |
| 协议 | Delta push + zstd 压缩 | 带宽减少 70% |
| 资源计算 | 懒计算（结果=上次值+速率×时间差） | 零定时器开销 |
| 战报 | Protobuf + zstd 压缩 | 每条 500B~2KB |

---

## 18. 开发工具链

```
技术栈推荐:
├── 语言: C++ (核心/MapService) + Go (微服务) + Python (工具)
├── 协议: Protobuf (Wire) + gRPC (内部 RPC) + Kafka (异步)
├── 数据: Redis (Cache) + MySQL (Persist) + S3 (Cold)
├── 部署: Docker + Kubernetes
├── 监控: Prometheus + Grafana
├── 日志: ELK (Elasticsearch + Logstash + Kibana)
├── CI/CD: GitHub Actions / Jenkins
├── 服务发现: etcd
├── 寻路: 自研 A* + JPS + HPA*
├── PRNG: xoshiro256 (确定性)
├── 压缩: zstd (战报/协议)
└── 压测: 机器人框架 (一个 goroutine = 一个客户端)

开发流程:
  Proto 定义 → 代码生成 → 实现逻辑 → 单元测试 → 压测 → 部署
```

---

## 附录: 核心数据规模估算

| 维度 | 数值 | 说明 |
|------|------|------|
| 地图格子数 | 1,440,000 | 1200×1200 |
| 单格子内存 | 64~128 B | 包含所有字段 |
| 地图总内存 | 90~180 MB | |
| 同时在线 | 5K~10K | 单赛季服 |
| 总注册 | 50K~200K | 单赛季 |
| 并发行军 | 10K~50K | 峰值 |
| 行军距离上限 | 20~150 格 | 初始~满级, 天然限制 path 长度 |
| 单行军数据 | ~120-600 B | 结构体 + path 平均 10~30 tiles |
| 行军总内存 | ~6-30 MB | 50K 行军, 有距离上限 |
| 视野总内存 | ~100 MB | 按需分配 |
| 进程总内存 | < 1 GB | MapService |
| 单次战斗 | < 1 ms | CPU 时间 |
| 战报大小 | 500B~5KB | protobuf + zstd |
| 最大 QPS | ~50K msg/s | 10K 人 × 5 msg/s |
