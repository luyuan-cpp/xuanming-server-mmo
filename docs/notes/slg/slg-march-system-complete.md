# SLG 行军系统完整设计（率土之滨风格）

**Created:** 2026-04-07

---

## 一、地图拓扑结构

### 1.1 三层地图模型

```
┌─────────────────────────────────────────────────────┐
│  第一层：州级拓扑图（State Graph）                     │
│  - 节点 = 州（司隶、豫州、冀州、荆州 ...）             │
│  - 边 = 关隘（函谷关、虎牢关、武关 ...）              │
│  - 边状态：locked / open / contested                  │
├─────────────────────────────────────────────────────┤
│  第二层：Chunk 抽象图（HPA* Abstract Graph）          │
│  - 节点 = Chunk 边界入口点                            │
│  - 边 = Chunk 内入口点间最短距离                      │
│  - 预处理产物，用于快速寻路                            │
├─────────────────────────────────────────────────────┤
│  第三层：格子图（Tile Grid）                           │
│  - 1200 × 1200 格子（144 万格）                       │
│  - 每格有地形类型、归属州、建筑、领地归属              │
│  - A* / JPS 在此层运行                                │
└─────────────────────────────────────────────────────┘
```

### 1.2 州（State / Province）

每个州是地图上一个**连续的格子区域**，有明确边界：

```cpp
struct State {
    uint32_t state_id;          // 州 ID（如 1=司隶, 2=豫州 ...）
    string   name;              // "司隶"
    vector<uint32_t> tiles;     // 该州包含的所有 tile_id
    vector<uint32_t> passes;    // 该州连接的关隘 ID 列表
    // 赛季相关
    bool     is_open;           // 赛季进度是否已开放此州
};
```

每个格子知道自己属于哪个州：

```cpp
uint8_t tile_to_state[MAP_WIDTH * MAP_HEIGHT];  // tile_id -> state_id
```

### 1.3 关隘（Pass / Gate）

关隘是州与州之间的**唯一通道**，对应地图上一组特定格子：

```cpp
struct Pass {
    uint32_t pass_id;
    string   name;              // "函谷关"
    uint32_t state_a;           // 连接的州 A
    uint32_t state_b;           // 连接的州 B
    vector<uint32_t> tiles;     // 关隘占据的格子（通常 3~10 格）
    PassStatus status;          // LOCKED / OPEN / CONTESTED / DESTROYED
    uint64_t  owner_alliance;   // 占领联盟 ID（0 = 无主）
    // 开放条件
    uint32_t  unlock_season_day; // 赛季第 N 天自动解锁（0 = 需攻占）
};

enum PassStatus {
    LOCKED,      // 不可通行，赛季进度未到
    OPEN,        // 已开放，任何人可通过
    CONTESTED,   // 攻城战进行中，参战方可进入
    DESTROYED,   // 被攻破，开放通行
};
```

### 1.4 地形类型

```cpp
enum TerrainType : uint8_t {
    PLAIN      = 0,  // 平原 - 移动代价 1.0
    FOREST     = 1,  // 森林 - 移动代价 1.5
    SWAMP      = 2,  // 沼泽 - 移动代价 2.0
    HILL       = 3,  // 丘陵 - 移动代价 1.3
    MOUNTAIN   = 4,  // 山脉 - 不可通行
    WATER      = 5,  // 水域 - 不可通行（无水军时）
    PASS       = 6,  // 关隘 - 移动代价 1.0，但有特殊通行规则
    CITY       = 7,  // 城池 - 不可通行（除非攻城）
};

// 移动代价表
float terrain_cost[] = {1.0, 1.5, 2.0, 1.3, INF, INF, 1.0, INF};
```

### 1.5 山脉格子的条件通行（山寨机制）

山脉（MOUNTAIN）**并非绝对不可进入**。游戏中 NPC 山寨（土匪营地）刷新在山上，玩家可以攻打：

```
通行规则:
  - 山格作为行军 **途经格（中间路径）** → 不可通行，必须绕行
  - 山格作为行军 **终点** 且有山寨 → 可进入（攻击山寨）
  - 山寨被清除后 → 恢复为普通山脉，不可通行
```

实现：A* 邻居展开时根据是否为终点格做条件判断：

```cpp
// A* 中判断格子是否可通行
bool is_tile_passable(uint32_t tile, uint32_t target_tile) {
    TerrainType terrain = get_terrain(tile);
    
    switch (terrain) {
        case MOUNTAIN:
            // 山格只在作为终点且有山寨/可攻击对象时可进入
            return (tile == target_tile && has_attackable_object(tile));
        
        case WATER:
            return (tile == target_tile && has_attackable_object(tile));
        
        case CITY:
            // 攻城目标可进入
            return (tile == target_tile);
        
        default:
            return terrain_cost[terrain] < INF;
    }
}
```

**行军路径表现：**
```
玩家主城        山脉山脉山脉
   ○              ▲▲▲
   |              ▲山寨▲    <- 山寨在山上
   +--→--→--→--→--↗▲▲▲
         沿平地绕行     ↑ 最后一步上山
```

山寨刷新规则（服务端）：
```cpp
struct BanditCamp {
    uint32_t camp_id;
    uint32_t tile_id;          // 所在山格
    uint32_t level;            // 山寨等级 1~10
    int64_t  spawn_time;       // 刷新时间
    int64_t  respawn_cd;       // 被打后重新刷新冷却
    bool     is_alive;         // 是否存活
};

// 山寨刷新位置选择：只在 MOUNTAIN 格子上刷
vector<uint32_t> get_bandit_spawn_tiles() {
    vector<uint32_t> candidates;
    for (uint32_t tile = 0; tile < MAP_SIZE; tile++) {
        if (get_terrain(tile) == MOUNTAIN && !has_building(tile)) {
            candidates.push_back(tile);
        }
    }
    return candidates;
}
```

这个设计的巧妙之处：
1. **山脉不再是纯废地**：有山寨可打，给玩家提供 PvE 目标和资源来源
2. **天然限制参战人数**：山格周围可站人的平地有限，不会被无限堆人
3. **寻路成本自然增高**：打山寨需要绕山走更远，消耗更多行军时间
4. **战略价值**：高级山寨产出好，但位置偏远，是高等级玩家的争夺目标

---

## 二、州际连通性系统

### 2.1 州连通图（State Connectivity Graph）

这是一个**运行时动态维护**的小型无向图：

```cpp
struct StateGraph {
    // adjacency: state_id -> [(neighbor_state_id, pass_id), ...]
    unordered_map<uint32_t, vector<pair<uint32_t, uint32_t>>> adjacency;
    
    // 只包含 OPEN / DESTROYED 状态的关隘形成的边
    // LOCKED / CONTESTED 的关隘不构成通行边
    
    void on_pass_status_changed(uint32_t pass_id, PassStatus old_s, PassStatus new_s);
    bool is_reachable(uint32_t from_state, uint32_t to_state);
    vector<uint32_t> find_state_path(uint32_t from_state, uint32_t to_state);
};
```

节点数量极少（率土之滨约 13 个州），所以 BFS/DFS 都是 O(1) 级别。

### 2.2 连通性变更事件

关隘状态变化是**全服事件**，触发场景：

| 触发 | 说明 |
|------|------|
| 赛季进度 | 第 N 天到达，关隘自动 LOCKED -> OPEN |
| 攻城战开始 | LOCKED -> CONTESTED，允许参战方进入 |
| 攻城战结束（攻方胜） | CONTESTED -> DESTROYED（永久开放） |
| 攻城战结束（守方胜） | CONTESTED -> LOCKED（重新封锁） |
| GM 指令 | 手动开关关隘（运营/测试用） |

变更流程：

```
关隘状态变更
  |
  v
1. 更新 StateGraph 邻接表
  |
  v
2. 更新关隘格子的通行属性
   - OPEN/DESTROYED: 关隘格子 passable = true
   - LOCKED: 关隘格子 passable = false
  |
  v
3. HPA* 增量更新（关隘所在 chunk 的入口点和内部距离）
  |
  v
4. 全服广播关隘状态变更消息
   - 客户端更新大地图关隘图标/颜色
   - 客户端更新可行军范围显示
  |
  v
5. 检查已有行军是否受影响（可选）
   - 关隘关闭 -> 经过该关隘的行军可以选择：
     a. 继续（行军已注册路径不受影响，到达后判定）
     b. 强制停滞（严格逻辑）
   - 通常选 a，减少服务端复杂度
```

### 2.3 连通性校验（寻路前置）

```cpp
// 行军请求到来时，第一步就是州际连通校验
PathResult request_march(Player* player, uint32_t from_tile, uint32_t to_tile) {
    uint32_t from_state = tile_to_state[from_tile];
    uint32_t to_state   = tile_to_state[to_tile];
    
    // 同州行军 -> 跳过州级校验
    if (from_state == to_state) {
        return do_tile_pathfinding(from_tile, to_tile);
    }
    
    // 跨州行军 -> 先查州连通图
    auto state_path = state_graph.find_state_path(from_state, to_state);
    if (state_path.empty()) {
        return PathResult::error(ERR_STATES_NOT_CONNECTED, 
            "无法到达目标：州际关隘未开放");
    }
    
    // 将必经关隘作为路径点（waypoint）
    vector<uint32_t> waypoints = collect_pass_tiles(state_path);
    return do_tile_pathfinding_with_waypoints(from_tile, to_tile, waypoints);
}
```

---

## 三、完整寻路流程（两级架构）

### 3.1 流程图

```
玩家点击出征 (from_tile, to_tile)
  |
  v
[1] 前置校验
  |- 部队是否空闲（非行军/非战斗/非采集中）
  |- 行军队列是否已满（通常上限 3~5 队）
  |- 目标格是否合法（非山脉/非水域）
  |- 行军距离预估是否在上限内（曼哈顿距离快速排除）
  |
  v
[2] 州际连通校验 <-- 关键步骤
  |- from_state = tile_to_state[from_tile]
  |- to_state   = tile_to_state[to_tile]
  |- 同州 -> 直接进入格子寻路
  |- 跨州 -> BFS 查州连通图
  |    |- 不连通 -> 返回 ERR_STATES_NOT_CONNECTED
  |    |- 连通 -> 提取必经关隘格子作为 waypoints
  |
  v
[3] 格子级寻路
  |- 同州：HPA* / JPS+A* 直接寻路
  |- 跨州：分段寻路
  |    |- from_tile -> pass_1_entry_tile (JPS/HPA*)
  |    |- pass_1_exit_tile -> pass_2_entry_tile (JPS/HPA*)
  |    |- ... -> to_tile
  |    |- 拼接所有段为完整路径
  |- 无路径 -> 返回 ERR_NO_PATH
  |
  v
[4] 行军距离校验
  |- path.length > max_march_distance(player) -> 返回 ERR_TOO_FAR
  |
  v
[5] 路径编码存储
  |- 转为方向编码: start_tile + dirs[] (每步 1 byte)
  |- 预计算 cum_dist[]（加权模型时）
  |
  v
[6] 时空碰撞检测
  |- 遍历路径每格，计算 [enter_time, leave_time]
  |- 检查与已有行军的时间窗口重叠
  |- 记录碰撞点（遭遇战触发位置）
  |
  v
[7] 注册行军
  |- 在每个途经格子注册 occupant 时间窗口
  |- 设置到达定时器
  |- 写入 DB/Redis: {march_id, player_id, path, start_time, speed, state}
  |
  v
[8] 广播
  |- 通知沿途视野内其他玩家
  |- 下发客户端: {march_id, path, start_time, speed}
```

### 3.2 跨州分段寻路详解

跨州行军不能 A* 一次搜完整张地图（太慢），而是以关隘为中继点分段搜索：

```
例：从豫州 tile_A 到冀州 tile_B
州级路径：豫州 -> [虎牢关] -> 司隶 -> [壶关] -> 冀州

分段寻路：
  段1: tile_A -----JPS/HPA*-----> 虎牢关入口格 (豫州侧)
  段2: 虎牢关入口格 --穿越关隘--> 虎牢关出口格 (司隶侧)  [固定短路径]
  段3: 虎牢关出口格 --JPS/HPA*--> 壶关入口格 (司隶侧)
  段4: 壶关入口格 ---穿越关隘---> 壶关出口格 (冀州侧)    [固定短路径]
  段5: 壶关出口格 ---JPS/HPA*---> tile_B

最终路径 = 段1 + 段2 + 段3 + 段4 + 段5
```

关隘本身通常只有 3~10 格，穿越关隘的路径可以**预计算并缓存**。

### 3.3 关隘通行权限

不是所有开放的关隘都允许所有人通过：

```cpp
bool can_traverse_pass(Player* player, Pass* pass) {
    switch (pass->status) {
        case OPEN:
        case DESTROYED:
            return true;  // 任何人可通行
        case CONTESTED:
            // 攻城战期间，只有参战双方可进入
            return is_war_participant(player, pass);
        case LOCKED:
            return false;  // 未开放
    }
}
```

---

## 四、行军数据模型

### 4.1 行军对象

```cpp
struct March {
    uint64_t march_id;         // 全局唯一 ID (snowflake)
    uint64_t player_id;
    uint64_t alliance_id;
    
    // 路径数据
    uint32_t start_tile;       // 起点 tile_id
    vector<uint8_t> dirs;      // 方向编码路径
    uint32_t path_length;      // 总步数
    
    // 时间数据
    int64_t  start_time_ms;    // 出发时间戳 (毫秒)
    float    speed;            // 格/秒
    vector<float> cum_dist;    // 累积距离（加权模型）
    
    // 状态
    MarchState state;
    uint32_t target_tile;      // 目的地
    MarchPurpose purpose;      // 出征目的
    
    // 部队信息
    uint32_t army_id;          // 部队 ID
    vector<uint32_t> hero_ids; // 武将列表
    uint32_t troop_count;      // 兵力
};

enum MarchState {
    MARCHING,     // 行军中
    RETURNING,    // 召回中
    BATTLING,     // 战斗中（在目标格交战）
    GATHERING,    // 采集中
    GARRISONING,  // 驻防中
    SCOUTING,     // 侦察中
    ARRIVED,      // 已到达
};

enum MarchPurpose {
    ATTACK,       // 攻击
    REINFORCE,    // 增援
    GATHER,       // 采集
    SCOUT,        // 侦察
    GARRISON,     // 驻防
    TRANSFER,     // 迁城
    RALLY,        // 集结
};
```

### 4.2 位置计算（懒计算公式）

```cpp
// 简化模型 O(1)
uint32_t get_current_tile_simple(const March& m, int64_t now_ms) {
    float elapsed = (now_ms - m.start_time_ms) / 1000.0f;
    int tiles_traveled = (int)floor(elapsed * m.speed);
    tiles_traveled = clamp(tiles_traveled, 0, (int)m.path_length - 1);
    return decode_tile_at(m.start_tile, m.dirs, tiles_traveled);
}

// 加权模型 O(log n) -- 对角线距离修正
uint32_t get_current_tile_weighted(const March& m, int64_t now_ms) {
    float elapsed = (now_ms - m.start_time_ms) / 1000.0f;
    float distance = elapsed * m.speed;
    // 二分查找 cum_dist 得到步数索引
    int index = upper_bound(m.cum_dist.begin(), m.cum_dist.end(), distance) 
                - m.cum_dist.begin() - 1;
    index = clamp(index, 0, (int)m.path_length - 1);
    return decode_tile_at(m.start_tile, m.dirs, index);
}
```

### 4.3 到达时间计算

```cpp
// 简化模型
int64_t get_arrival_time_simple(const March& m) {
    float total_time = m.path_length / m.speed;  // 秒
    return m.start_time_ms + (int64_t)(total_time * 1000);
}

// 加权模型
int64_t get_arrival_time_weighted(const March& m) {
    float total_dist = m.cum_dist.back();
    float total_time = total_dist / m.speed;
    return m.start_time_ms + (int64_t)(total_time * 1000);
}
```

---

## 五、行军注册与碰撞检测

### 5.1 每格时间窗口

```cpp
// 简化模型
TimeWindow get_time_window_simple(const March& m, int tile_index) {
    float enter = m.start_time_ms / 1000.0f + tile_index / m.speed;
    float leave = m.start_time_ms / 1000.0f + (tile_index + 1) / m.speed;
    return {enter, leave};
}

// 加权模型
TimeWindow get_time_window_weighted(const March& m, int tile_index) {
    float enter = m.start_time_ms / 1000.0f + m.cum_dist[tile_index] / m.speed;
    float leave = m.start_time_ms / 1000.0f + m.cum_dist[tile_index + 1] / m.speed;
    return {enter, leave};
}
```

### 5.2 碰撞检测

```cpp
struct TileOccupant {
    uint64_t march_id;
    uint64_t player_id;
    float enter_time;
    float leave_time;
};

// 每个格子维护占用列表（按 enter_time 排序）
vector<TileOccupant> tile_occupants[MAP_WIDTH * MAP_HEIGHT];

bool check_collision(float new_enter, float new_leave, 
                     float exist_enter, float exist_leave) {
    return new_enter < exist_leave && exist_enter < new_leave;
}

void register_march_occupants(March& m) {
    for (int i = 0; i < m.path_length; i++) {
        uint32_t tile = decode_tile_at(m.start_tile, m.dirs, i);
        auto tw = get_time_window(m, i);
        
        // 检查碰撞
        for (auto& exist : tile_occupants[tile]) {
            if (check_collision(tw.enter, tw.leave, exist.enter_time, exist.leave_time)) {
                if (is_hostile(m.player_id, exist.player_id)) {
                    // 调度遭遇战
                    float encounter_time = max(tw.enter, exist.enter_time);
                    schedule_encounter(m.march_id, exist.march_id, tile, encounter_time);
                }
            }
        }
        
        // 注册占用
        tile_occupants[tile].push_back({
            m.march_id, m.player_id, tw.enter, tw.leave
        });
    }
}
```

---

## 六、行军状态变更

### 6.1 召回（Recall）

```
玩家点击召回
  |
  v
1. 校验: state == MARCHING（战斗中/采集中不可召回）
  |
  v
2. 懒计算当前位置
   current_index = get_current_tile_index(march, now)
  |
  v
3. 清除旧行军后半段
   - 移除 path[current_index+1 .. end] 的所有 occupant
   - 取消到达定时器
  |
  v
4. 构建回程路径
   - 反转 path[0 .. current_index]
   - 编码为方向序列
  |
  v
5. 创建新行军
   - path = reversed_path
   - start_time = now
   - speed = return_speed（通常 = 原速 × 1.5 加速回城）
   - state = RETURNING
   - purpose = RETURN
  |
  v
6. 对回程路径做碰撞检测 + 注册 occupant
  |
  v
7. 广播路径变更给沿途玩家和客户端
```

### 6.2 加速（Speed Boost）

```
加速道具使用 / buff 生效
  |
  v
1. 懒计算当前位置
   current_index = get_current_tile_index(march, now)
  |
  v
2. 清除旧行军后半段 occupant + 定时器
  |
  v
3. 截取剩余路径
   remaining_path = path[current_index .. end]
  |
  v
4. 创建新行军
   - path = remaining_path
   - start_time = now
   - speed = original_speed × boost_multiplier
   - 其他字段不变
  |
  v
5. 重做碰撞检测 + 注册 occupant + 重建定时器
  |
  v
6. 广播
```

### 6.3 到达目标

```
到达定时器触发
  |
  v
1. 确认位置（防御性校验）
   actual_tile = get_current_tile(march, now)
   assert(actual_tile == target_tile)
  |
  v
2. 根据 purpose 分发
   |- ATTACK -> 发起战斗（PvP / PvE）
   |- REINFORCE -> 加入目标城池防守队列
   |- GATHER -> 开始采集定时器
   |- SCOUT -> 返回侦察报告
   |- GARRISON -> 进入驻防状态
   |- RALLY -> 加入集结点等待
  |
  v
3. 清除所有途经格子的 occupant（行军已结束）
  |
  v
4. 更新 march.state（BATTLING / GATHERING / GARRISONING ...）
  |
  v
5. 广播行军消失 / 战斗开始
```

---

## 七、集结行军（Rally）

集结是 SLG 特有的多人协作行军：

```
发起集结 (leader, target_tile, rally_duration)
  |
  v
1. 创建集结点
   - rally_id, leader_id, target_tile
   - expire_time = now + rally_duration (通常 5min / 10min / 30min)
   - participants = []
  |
  v
2. 其他玩家加入集结
   - 各自寻路到集结点（leader 的主城或指定地点）
   - 到达后加入 participants 等待
  |
  v
3. 集结时间到 / leader 手动发起
   - 从集结点统一寻路到 target_tile
   - 所有参战部队合并为一路行军
   - speed 取最慢部队的速度
  |
  v
4. 到达目标
   - 多部队联合发起攻击
   - 战力 = 所有参战部队战力之和（有上限）
```

---

## 八、视野与行军可见性

### 8.1 谁能看到行军？

```cpp
bool can_see_march(Player* viewer, March* march, int64_t now) {
    uint32_t march_tile = get_current_tile(*march, now);
    
    // 1. 行军在 viewer 的视野范围内？
    if (is_in_vision(viewer, march_tile)) return true;
    
    // 2. 行军经过 viewer 的领地格子？
    if (is_viewer_territory(viewer, march_tile)) return true;
    
    // 3. 联盟共享视野？
    if (same_alliance(viewer, march->player_id) && 
        alliance_shared_vision_enabled()) return true;
    
    // 4. 侦察塔/瞭望塔覆盖范围？
    if (is_in_watchtower_range(viewer, march_tile)) return true;
    
    return false;
}
```

### 8.2 行军进出视野的通知

```
行军移动到新格子时（定时器或懒计算时检查）:
  对新格子周围视野范围内的玩家:
    - 之前看不到 -> 现在看到 -> 发送 MarchAppear 消息
    - 之前看得到 -> 现在看不到 -> 发送 MarchDisappear 消息
    
优化: 不逐格检查，而是在路径注册时预计算
  - 对每段连续可见区域记录 [enter_index, leave_index]
  - 用定时器在进入/离开时触发通知
```

---

## 九、行军距离与领地约束

### 9.1 行军距离上限

```cpp
int max_march_distance(Player* p) {
    int base = 20;                      // 1 级主城基础
    base += p->castle_level_bonus();    // 主城等级 +2~3 每级
    base += p->tech_bonus();            // 军事科技 +30~50
    base += p->hero_skill_bonus();      // 武将被动技能 +10~30
    base += p->vip_bonus();             // VIP 等级 +5~20
    base += p->alliance_buff();         // 联盟科技 +10~30
    base += p->item_buff();             // 临时道具 +10~50
    return min(base, 200);              // 硬上限 200
}
```

### 9.2 领地约束

率土之滨的关键机制：**只能向己方/盟友领地相邻格出征**

```
玩家 A 的领地:
  ████
  ████
  ████

允许攻击的目标: 领地边缘外一圈的格子
  XXXX XX
  X████XX
  X████XX
  X████XX
  XXXXXXX

不能攻击: 领地不相邻的远处格子（即使在行军距离内）
  
例外:
  - 侦察（scout）可以无视领地约束
  - 迁城有独立的距离规则
  - 集结发起点如果在盟友领地内也合法
```

```cpp
bool is_valid_march_target(Player* player, uint32_t target_tile) {
    // 1. 目标在行军距离内？
    int dist = manhattan_distance(player->castle_tile, target_tile);
    if (dist > max_march_distance(player)) return false;
    
    // 2. 目标与己方/盟友领地相邻？
    if (march_purpose != SCOUT) {
        if (!is_adjacent_to_friendly_territory(player, target_tile)) {
            return false;
        }
    }
    
    // 3. 目标格可攻击？（非己方建筑、非盟友领地等）
    return is_valid_attack_target(player, target_tile);
}
```

---

## 十、速度计算

### 10.1 行军速度公式

```cpp
float calc_march_speed(Player* player, Army* army, TerrainType terrain) {
    // 基础速度（格/秒）
    float base_speed = 1.0f;
    
    // 武将加成
    base_speed *= (1.0f + army->hero_speed_bonus());  // 如 +30%
    
    // 科技加成
    base_speed *= (1.0f + player->tech_speed_bonus()); // 如 +20%
    
    // 联盟加成
    base_speed *= (1.0f + player->alliance_speed_bonus()); // 如 +10%
    
    // 道具/buff 加成
    base_speed *= (1.0f + player->item_speed_bonus()); // 如加速令 +50%
    
    // 兵种修正（骑兵快、步兵慢）
    base_speed *= army->troop_type_speed_modifier();
    
    // 该速度是"格/秒"，地形代价在寻路时已体现在路径选择中
    // 加权模型下，经过高代价格子时 cum_dist 增长更快 -> 经过该格耗时更长
    
    return base_speed;
}
```

### 10.2 路径上不同地形的处理

加权模型中，地形代价已融入路径的 `cum_dist[]`：

```cpp
void precompute_cum_dist(March& m) {
    m.cum_dist.resize(m.path_length + 1);
    m.cum_dist[0] = 0;
    
    uint32_t prev_tile = m.start_tile;
    for (int i = 0; i < m.path_length; i++) {
        uint32_t cur_tile = decode_tile_at(m.start_tile, m.dirs, i);
        
        float step_cost = 1.0f;
        
        // 对角线修正
        if (is_diagonal_move(m.dirs[i])) {
            step_cost *= 1.414f;
        }
        
        // 地形代价
        step_cost *= terrain_cost[get_terrain(cur_tile)];
        
        m.cum_dist[i + 1] = m.cum_dist[i] + step_cost;
        prev_tile = cur_tile;
    }
}
```

---

## 十一、定时器系统

### 11.1 行军相关定时器

每个行军注册时创建的定时器：

| 定时器 | 触发时间 | 作用 |
|--------|----------|------|
| 到达定时器 | arrival_time | 触发到达事件（攻击/采集等） |
| 经过关隘定时器 | pass_enter_time | 校验关隘通行权限（可选） |
| 视野进入定时器 | per-viewer | 通知其他玩家行军出现 |
| 视野离开定时器 | per-viewer | 通知其他玩家行军消失 |

### 11.2 定时器精度

```
行军位置精度 = 1 格
speed = 1.5 格/秒
-> 每格耗时 = 0.67 秒 = 670ms

定时器精度要求: < 100ms 足够
实际游戏 tick: 通常 200ms~500ms 检查一次

碰撞判定精度: 格级别（同一格同一时间窗口 = 碰撞）
不需要 MMORPG 那种 sub-tile 精度
```

---

## 十二、持久化与崩溃恢复

### 12.1 需要持久化的数据

```
Redis (热数据, 行军生命周期内):
  march:{march_id} -> {player_id, start_tile, dirs, start_time, speed, state, purpose, target_tile, army_id, hero_ids, troop_count}

MySQL (冷数据, 定期归档):
  - 行军日志（出征/到达/战斗/召回记录）
  - 战报
```

### 12.2 崩溃恢复流程

```
MapService 重启
  |
  v
1. 从 Redis 加载所有 state != ARRIVED 的行军
  |
  v
2. 对每个行军:
   a. 懒计算当前位置 current_index
   b. if current_index >= path_length - 1:
        -> 行军已到达，触发到达事件（补发）
   c. else:
        -> 从 current_index 开始重注册 occupant
        -> 重建到达定时器（arrival_time - now）
        -> 重建视野定时器
  |
  v
3. 重建 tile_occupants 索引
  |
  v
4. 开放接收新请求
```

**关键优势：** 懒计算公式只依赖 `{path, start_time, speed}`，不需要任何中间状态。崩溃后这三个值从 Redis 读出即可完美恢复。

---

## 十三、性能预估

### 13.1 典型负载

| 指标 | 值 |
|------|-----|
| 同时在线 | 10K 玩家 |
| 活跃行军 | 30K~50K（每人 3~5 队） |
| 行军请求峰值 | ~200/秒 |
| 单次寻路耗时 | HPA* ~3ms，JPS ~12ms |
| 位置查询 | O(1) 或 O(log n) |
| 每格 occupant 数 | 平均 <1，热点格 ~10 |

### 13.2 内存占用

| 数据 | 大小 |
|------|------|
| 格子地图 | 1200×1200 × 1 byte (terrain) = 1.4MB |
| State 映射 | 1200×1200 × 1 byte = 1.4MB |
| HPA* 抽象图 | ~几千节点 = ~100KB |
| 50K 行军路径 | 方向编码 50K × 100 格平均 = ~5MB |
| tile_occupants | 稀疏存储，只有途经格子有数据 ~10MB |
| **总计** | **~20MB** |

---

## 十四、总结：行军请求完整生命周期

```
[客户端] 玩家点击目标格出征
    |
    v
[校验层] 部队/队列/距离/领地 校验
    |
    v
[州级连通] BFS 查询州连通图 -- 不通就返回错误
    |  提取必经关隘 waypoints
    v
[格子寻路] 分段 JPS/HPA* --> 拼接完整路径
    |
    v
[距离检查] path.length <= max_march_distance
    |
    v
[编码存储] start_tile + dirs[] + cum_dist[]
    |
    v
[碰撞检测] 遍历每格时间窗口, 检查敌方行军重叠
    |  发现碰撞 -> 调度遭遇战
    v
[注册占用] 每格写入 occupant, 创建到达定时器
    |
    v
[持久化]  写入 Redis
    |
    v
[广播]    通知沿途视野内玩家 + 下发客户端 {path, start_time, speed}
    |
    v
[行军中]  服务端不 tick, 用懒计算公式 O(1) 查位置
    |
    |-- 召回 -> 定位当前格, 反转路径, 重建行军
    |-- 加速 -> 定位当前格, 新速度重建行军
    |-- 遭遇战定时器触发 -> 进入战斗
    |
    v
[到达]    定时器触发 -> 按 purpose 分发（攻击/采集/驻防...）
    |
    v
[清理]    移除所有 occupant, 更新状态
    |
    v
[归档]    行军日志写入 MySQL
```
