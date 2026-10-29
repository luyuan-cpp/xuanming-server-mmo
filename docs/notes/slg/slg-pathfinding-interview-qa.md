# SLG 沙盘寻路面试 Q&A（率土之滨风格）

**Created:** 2026-04-07

---

## Q1: SLG 大地图寻路和 MMORPG 寻路有什么本质区别？

**A:** 区别体现在三个维度：

| 维度 | SLG | MMORPG |
|------|-----|--------|
| 地图结构 | 离散格子图（1200×1200） | 连续 3D 地形 |
| 寻路时机 | 行军注册时一次性算完整路径 | 每帧/每秒实时更新 |
| 移动模型 | 格到格跳跃，懒计算 O(1) 读位置 | 连续坐标插值，逐帧推进 |
| 导航数据 | 格子图 + 障碍 bitmap | NavMesh + 多边形 |
| 频率 | 低频（玩家主动出征） | 高频（摇杆/点击持续移动） |
| 动态性 | 中（建筑/领地变化） | 高（NPC/怪物实时避障） |

**核心差异：** SLG 寻路是"一次计算、长时间使用"，路径存下来用懒计算公式 O(1) 查位置；MMORPG 是"持续计算、实时响应"。

---

## Q2: SLG 为什么用 A* 而不用 NavMesh？

**A:** 因为 SLG 地图本质是离散格子图，不是连续地形。

- **格子图天然是图结构**：每个格子是节点，相邻格子是边，A* 直接适用
- **NavMesh 为连续地形设计**：多边形剖分、漏斗算法等，都是为了在连续空间找路
- **格子图上 NavMesh 多余**：先把格子合并成多边形再搜索，不如直接在格子图上 A*
- **格子有丰富语义**：每个格子有地形类型、建筑、领地归属，A* 可直接利用
- **动态变化简单**：格子状态变了，A* 下次搜索自动避开；NavMesh 需要重新烘焙

---

## Q3: A* 算法的核心原理是什么？实现上的关键数据结构？

**A:**

**原理：**
- 维护 openSet（待探索）和 closedSet（已探索）
- 每个节点有：g(n) = 起点到 n 的实际代价，h(n) = n 到终点的启发式估计，f(n) = g(n) + h(n)
- 每次从 openSet 取 f 最小节点展开邻居
- h(n) 不高估（admissible）则保证最优路径

**关键数据结构：**
1. **openSet → 最小堆（priority_queue）**：O(log n) 取最小 f 值
2. **closedSet → 哈希集合**：O(1) 判断是否已探索
3. **g_score map**：记录每个节点的最优 g 值
4. **came_from map**：回溯路径

**启发函数选择：**
- 4 方向：曼哈顿距离 `|dx| + |dy|`
- 8 方向：Octile 距离 `max(|dx|, |dy|) + (√2 - 1) * min(|dx|, |dy|)`
- Octile 用整数近似：`max * 10 + min * 4`（避免浮点）

---

## Q4: A* 在大地图上的性能瓶颈是什么？

**A:** 1200×1200 地图（144 万格子），最坏情况展开节点数巨大。

**具体数据（1024×1024，30% 障碍）：**
- 展开节点：~800K
- 耗时：~250ms
- 内存：每个节点 ~40 字节（g/f/parent/状态），800K 节点 ~32MB

**瓶颈来源：**
1. **对称路径**：开阔区域大量等价路径都被展开
2. **堆操作**：800K 次 push/pop，每次 O(log n)
3. **缓存不友好**：哈希表随机访问，cache miss 高

**解决方案：** JPS 剪枝 + HPA* 分层

---

## Q5: JPS（Jump Point Search）的原理是什么？为什么比 A* 快 10-30 倍？

**A:**

**核心思想：** 跳过对称等价路径，只在"跳点"处停下。

**跳点定义：** 一个节点是跳点，当且仅当：
1. 它是目标点，或
2. 它有"强制邻居"（forced neighbor）—— 因为障碍物导致必须从这个点转弯

**搜索过程：**
1. **直线跳跃**：沿水平/垂直方向一直走，直到碰到障碍/跳点/地图边界
2. **对角跳跃**：沿对角线方向走时，每步先向两个分量方向做直线跳跃
3. 只有跳点才加入 openSet

**为什么快：**
- A* 在开阔区域展开所有邻居（8 个方向都展开）
- JPS 直接跳到下一个有意义的点，中间节点全部跳过
- 1024×1024 地图：A* 展开 ~800K → JPS 展开 ~15K（减少 50 倍）

**限制：**
- 只适用于均匀代价方格图（每一步代价相同）
- 不支持不同地形代价（沼泽=2，平地=1 时不能用原版 JPS）
- 输出跳点序列，需要插值得到逐格路径

---

## Q6: 如果地形有不同代价（沼泽、山地），JPS 不能用怎么办？

**A:** 三种方案：

1. **加权 A* + 优化**：
   - 用标准 A* 但做工程优化：flat array 替代 hash map、aligned memory、SIMD 距离计算
   - 适合代价种类多、地图不太大的场景

2. **JPS+ 变体（Bounded JPS）**：
   - 预计算每个格子 8 个方向的跳跃距离表
   - 离线 O(N) 扫描，在线查表 O(1) 跳跃
   - 地形变化时只更新受影响行/列
   - 不支持加权，但可以把高代价格子视为"半障碍"在应用层控制

3. **HPA*（Hierarchical Pathfinding A*）**：
   - 上层抽象图用 A* 找粗路径，下层 chunk 内用加权 A* 找精细路径
   - 支持任意代价，性能优秀
   - **推荐方案**

实际 SLG 中，多数地形在行军层面简化为 {可通行, 不可通行, 减速}，减速格可以特殊处理（A* 代价 ×1.5），JPS 用于可通行部分。

---

## Q7: HPA*（Hierarchical Pathfinding A*）的原理？预处理和在线查询分别做什么？

**A:**

### 预处理（离线，一次性）
1. **切分 Chunk**：地图切成 N×N 的 chunk（如 32×32），1200÷32 ≈ 38×38 = 1444 个 chunk
2. **扫描入口点**：相邻 chunk 边界上连续可通行段取中点作为"入口点"（entrance）
3. **算内部距离**：chunk 内 A* 计算入口点两两最短距离
4. **构建抽象图**：入口点 = 节点，内部距离 = 边权

预处理结果：一个节点数 ~几千（远小于 144 万）的抽象图。

### 在线查询
1. **插入 S/G**：把起点/终点临时插入所在 chunk 的抽象图（算到 chunk 入口点的距离）
2. **抽象图 A***：在抽象图上找最短路径（节点少，极快）
3. **精细化**：途经的每个 chunk 内做 A*/JPS 拼出完整逐格路径

### 性能数据（1024×1024，30% 障碍）
| 步骤 | 时间 |
|------|------|
| 预处理 | ~2s（一次性） |
| 抽象图 A* | <1ms |
| 精细化 | ~2ms |
| **总在线耗时** | **~3ms** |

vs 纯 A* ~250ms，JPS ~12ms。

### 动态更新
地图变化（建筑建造/拆除）→ 只重算受影响 chunk 的入口点和内部距离 → 单个 chunk 更新 <1ms。

---

## Q8: SLG 行军注册时寻路的完整流程是什么？

**A:**

```
玩家点击出征
  ↓
1. 前置校验
   - 部队是否空闲
   - 目标格是否合法（非障碍、在行军距离内、领地约束）
   - 行军队列是否已满
  ↓
2. 寻路计算
   - HPA* / JPS+A* 算出完整路径 path[] (tile_id 数组)
   - 如果无路径 → 返回失败
  ↓
3. 路径优化存储
   - 转换为方向编码: start_tile + dirs[] (每步 1 byte，8方向)
   - 节省 ~70% 内存
  ↓
4. 碰撞检测（时空重叠检查）
   - 遍历 path 每个格子
   - 算每格 [enter_time, leave_time]
   - 检查该格已有行军的时间窗口是否重叠
   - 重叠条件: new_enter < existing_leave && existing_enter < new_leave
  ↓
5. 注册行军
   - 在每个途经格子注册 occupant 时间窗口
   - 创建定时器: 到达目标格 → 触发事件（攻击/采集/驻防）
   - 存储: {march_id, player_id, path, start_time, speed, state}
  ↓
6. 广播
   - 通知沿途视野内的玩家（有新行军出现）
   - 发送 {path, start_time, speed} 到客户端
```

---

## Q9: 行军路径存储优化 — 方向编码是什么原理？

**A:**

**原始方案：** `vector<uint32_t> path`，每个 tile_id 4 字节
- 50 格路径 = 200 字节
- 50K 行军 × 100 格平均 = ~20MB

**方向编码：** 相邻格子只有 8 个方向（N/NE/E/SE/S/SW/W/NW），用 3-bit 即可表示
- 存储：`start_tile (4B) + vector<uint8_t> dirs`（每步 1 字节，实际只需 3-bit）
- 50 格路径 = 4 + 50 = 54 字节（vs 200 字节，节省 73%）
- 50K 行军 × 100 格 = ~5.2MB（vs ~20MB）

**还原路径：**
```cpp
const int dx[] = {0, 1, 1, 1, 0, -1, -1, -1};  // 8 方向
const int dy[] = {-1, -1, 0, 1, 1, 1, 0, -1};

tile_id current = start_tile;
for (auto dir : dirs) {
    int x = current % MAP_WIDTH + dx[dir];
    int y = current / MAP_WIDTH + dy[dir];
    current = y * MAP_WIDTH + x;
    // current 就是路径上的下一个格子
}
```

**进一步优化：**
- 3-bit 打包：8 个方向放 3 字节（省 3%）— 增加复杂度，收益小
- RLE 编码：连续直线段（如连续 20 个 E）压缩为 `{dir=E, count=20}` — 对有很多直线段的路径有效
- JPS 跳点存储：只存跳点（路径上拐弯的地方），中间直线段隐式还原 — 约 90% 压缩

---

## Q10: 行军位置公式（懒计算）是什么？为什么不逐帧 tick？

**A:**

### 公式
```
elapsed = now - start_time
tiles_traveled = floor(elapsed * speed)
current_tile = path[min(tiles_traveled, len(path) - 1)]
```

### 为什么不逐帧 tick
SLG 不是动作游戏，不需要 60fps 更新：
1. **行军是匀速直线运动**（在格子序列上），完全可预测
2. **只在需要时算**：碰撞检测、视野查询、到达事件
3. **O(1) vs O(T)**：tick 模型需要 50K 行军 × 每秒 tick = 大量无意义计算
4. **崩溃恢复**：公式直接算出正确位置，无中间状态可丢
5. **时间跳跃友好**：断线重连、时间加速，公式直接给正确结果

### 对角线问题
如果 8 方向移动，对角线实际距离 √2 但公式按 1 格算，视觉偏快 41%。

**解决：** 加权模型
```
// 注册时预计算累积距离
cum_dist[0] = 0
for i in 1..path.length:
    cum_dist[i] = cum_dist[i-1] + (is_diagonal(i) ? 1.414 : 1.0)

// 查询时二分
distance = elapsed * speed
index = binary_search(cum_dist, distance)  // O(log n)
```

1000 格路径只需 ~10 次比较。决策：先用简化模型，策划觉得不好再改。

---

## Q11: 行军过程中路径怎么碰撞检测？时空碰撞的数学模型？

**A:**

### 每格时间窗口
每个行军在路径上的每个格子都有 [enter_time, leave_time]：
```
enter_time[i] = start_time + i / speed
leave_time[i] = start_time + (i + 1) / speed
```

### 碰撞条件
两个行军在同一格子时间窗口重叠 = 碰撞：
```
collision = (A.enter < B.leave) && (B.enter < A.leave)
```

### 数据结构
每个格子维护一个占用列表：
```cpp
struct TileOccupant {
    uint32_t march_id;
    float enter_time;
    float leave_time;
};
// per-tile: vector<TileOccupant> occupants; (sorted by enter_time)
```

### 注册时检测流程
```
for each tile in path:
    [enter, leave] = calc_time_window(tile_index, start_time, speed)
    for each existing in tile.occupants:
        if enter < existing.leave && existing.enter < leave:
            if is_hostile(existing.player_id, my_player_id):
                // 碰撞！ → 触发遭遇战 or 拦截
                schedule_encounter(my_march, existing.march, tile, max(enter, existing.enter))
    tile.occupants.push({march_id, enter, leave})
```

### 优化
- occupants 按 enter_time 排序 → 可二分查找，跳过过期条目
- 冷门格子 occupants = 0，热门格子（要塞附近）可能 10+
- 大多数格子 O(1)，总复杂度 O(path_length × avg_occupants)

---

## Q12: 召回行军和加速行军怎么处理？为什么不能原地修改？

**A:**

### 核心原则：销毁旧行军 + 创建新行军

**不能原地修改 speed 或方向的原因：**
1. **occupant 时间窗口失效**：改了 speed，所有途经格子的 [enter, leave] 全错了，其他行军的碰撞检测基于旧窗口
2. **定时器失效**：到达事件的触发时间基于旧 speed 计算
3. **视野更新失效**：沿途玩家的 observer_count 增减基于旧时间窗口

### 召回流程
```
1. 懒计算定位当前位置 current_tile = path[floor((now - start_time) * speed)]
2. 清除 path[current_index+1 .. end] 的 occupant/timer
3. 反转 path[0 .. current_index] 得到回程路径
4. 创建新行军 {path=reversed, start_time=now, speed=return_speed, state=RETURNING}
5. 对新路径做碰撞检测、注册 occupant/timer
6. 广播路径变更
```

### 加速流程
```
1. 懒计算定位当前位置
2. 清除 path[current_index+1 .. end] 的 occupant/timer
3. 剩余路径 path[current_index .. end] 不变
4. 创建新行军 {path=remaining, start_time=now, speed=new_speed}
5. 重做碰撞检测、注册
```

### 边界情况
- 战斗中不可召回（BATTLING 状态拒绝）
- 刚出发召回 = current_index = 0，原地返回
- 已到达（采集中/驻防中）走不同逻辑分支

---

## Q13: 行军距离上限是怎么设计的？有什么性能意义？

**A:**

### 游戏设计
- 基础距离：~20 格（1 级主城）
- 满级：~100-150 格（科技 + 武将技能 + VIP + 联盟 buff）
- **领地约束**：只能向己方/盟友领地相邻格出征 → 逼迫扩张领地

### 性能意义

| 约束 | 效果 |
|------|------|
| 路径长度有限 | A*/JPS 搜索空间天然截断，不会搜遍全图 |
| 存储可控 | 50K 行军 × 150 格平均 → 方向编码 ~7.5MB |
| 碰撞检测快 | O(path_length) 注册，150 格 × 少量 occupants |
| Timer 数量可控 | 每行军 1 个到达 timer，50K 行军 = 50K timer |

### 实现
```cpp
int max_march_distance(Player* p) {
    int base = 20;
    base += p->tech_bonus();        // 科技加成 +30~50
    base += p->hero_skill_bonus();  // 武将被动 +10~30
    base += p->vip_bonus();         // VIP +5~20
    base += p->alliance_buff();     // 联盟 +10~30
    return min(base, 200);          // 硬上限 200
}
```

---

## Q14: 动态障碍（建筑建造/拆除、领地变化）怎么更新寻路？

**A:**

### 格子状态变化来源
1. **建造**：玩家在格子上造建筑 → 格子变为不可通行
2. **拆除**：建筑被摧毁 → 格子变为可通行
3. **领地变化**：占领/失去领地 → 影响行军距离约束（不影响通行性）
4. **临时事件**：沙尘暴、洪水 → 区域临时不可通行

### 处理方式

**纯 A*/JPS（无预处理层）：**
- 下一次寻路就自动避开新障碍
- 已有行军不受影响（路径已注册）
- 需要时可强制重新寻路（极少见）

**HPA*（有预处理层）：**
```
格子 (x, y) 状态变化
  ↓
1. 确定所在 chunk: chunk_x = x / CHUNK_SIZE, chunk_y = y / CHUNK_SIZE
2. 重新扫描该 chunk 与 4 个邻居 chunk 的边界入口点
3. 重算该 chunk 内入口点间距离
4. 更新抽象图的对应边
耗时: < 1ms（单 chunk 32×32 格子 A* 极快）
```

### 已有行军怎么办？
- **不主动中断**：玩家体验差
- **终点被占 → 行军到达时判定失败**：到达事件触发时检查目标格状态
- **关键路径被断** → 可选：服务器主动通知玩家"路径不通，行军停滞"

---

## Q15: 寻路请求量大怎么办？如何防止寻路阻塞主线程？

**A:**

### 请求量估算
- 10K 在线玩家，高峰期每人每分钟 1 次出征 → ~170 次/秒
- 每次寻路 HPA* ~3ms → 170 × 3ms = 510ms/s → 占一个核的 51%

### 方案

1. **异步寻路线程池**
```
主线程收到出征请求
  ↓ 投递到寻路队列
寻路线程池 (2-4 线程)
  ↓ 计算完成，结果回投主线程
主线程处理注册/碰撞检测
```

2. **请求优先级**
- 玩家主动出征 → 高优先级
- NPC/AI 行军 → 低优先级
- 相同起终点缓存结果（短时间内多人去同一目标）

3. **路径缓存**
```cpp
struct PathCacheKey {
    uint32_t from_tile;
    uint32_t to_tile;
    uint32_t map_version;  // 地图变化时 version++
};
LRU<PathCacheKey, vector<uint8_t>> path_cache(10000);
```

4. **限流**
- 每玩家每秒最多 2 次寻路请求
- 队列满时拒绝并返回"服务器繁忙"

---

## Q16: 路径平滑和拐角优化在 SLG 中需要吗？

**A:**

### 短答案：通常不需要

SLG 地图是格子图，行军是"格到格"跳跃，不是连续移动：
- 客户端通过插值动画让行军看起来平滑
- 服务端只关心格子序列，不关心中间位置

### 客户端侧
客户端收到 `{path, start_time, speed}` 后：
1. 根据公式算出当前在第 N 格和第 N+1 格之间
2. 在两格中心之间做线性插值
3. 连续拐弯可以做贝塞尔曲线平滑（纯视觉效果）

### 唯一需要服务端处理的情况
如果有"行军移动动画影响碰撞判定"的需求（如两军在格子间交错），需要更精细的时空碰撞模型。但多数 SLG 以格子为最小碰撞单位，不需要。

---

## Q17: 如何处理多层地图（地上/地下/水域）的寻路？

**A:**

### 建模方式
```
// 每层是独立的二维格子图
Layer ground_layer(1200, 1200);  // 地面
Layer underground_layer(1200, 1200);  // 地下
Layer water_layer(1200, 1200);  // 水域

// 层间传送点（入口/出口）
struct Portal {
    uint32_t from_tile;
    uint8_t from_layer;
    uint32_t to_tile;
    uint8_t to_layer;
    float traverse_cost;
};
```

### 寻路方式
1. **层内**：正常 A*/JPS/HPA*
2. **跨层**：Portal 作为特殊邻居加入图中
3. **HPA* 适配**：Portal 视为特殊入口点，加入抽象图

### 实际 SLG 设计
多数率土之滨风格 SLG 只有单层地面，水域作为障碍处理。如果有地下城/水战，按上述方式建模。

---

## Q18: 寻路算法怎么写单元测试？

**A:**

```cpp
// 1. 基础正确性
TEST(Pathfinding, SimpleDirectPath) {
    Grid grid(10, 10);  // 无障碍
    auto path = find_path(grid, {0,0}, {9,9});
    ASSERT_FALSE(path.empty());
    EXPECT_EQ(path.front(), tile_id(0, 0));
    EXPECT_EQ(path.back(), tile_id(9, 9));
    // 路径长度 = 对角线步数，应为最优
    EXPECT_EQ(path.size(), 10);
}

// 2. 障碍绕行
TEST(Pathfinding, ObstacleAvoidance) {
    Grid grid(10, 10);
    grid.set_blocked(5, 0, 5, 8);  // 竖墙，留口子在 (5,9)
    auto path = find_path(grid, {0,0}, {9,0});
    ASSERT_FALSE(path.empty());
    // 路径不能穿过障碍
    for (auto tile : path) {
        EXPECT_FALSE(grid.is_blocked(tile));
    }
}

// 3. 无路径
TEST(Pathfinding, NoPath) {
    Grid grid(10, 10);
    grid.set_blocked(5, 0, 5, 9);  // 完全隔断
    auto path = find_path(grid, {0,0}, {9,0});
    EXPECT_TRUE(path.empty());
}

// 4. 性能基准
TEST(Pathfinding, PerformanceBenchmark) {
    Grid grid(1024, 1024);
    grid.random_obstacles(0.3);
    auto start = now();
    auto path = find_path(grid, {0,0}, {1023,1023});
    auto elapsed = now() - start;
    EXPECT_LT(elapsed, 10ms);  // HPA* 应在 10ms 内
}

// 5. 方向编码一致性
TEST(PathEncoding, RoundTrip) {
    auto path = find_path(grid, from, to);
    auto dirs = encode_directions(path);
    auto decoded = decode_directions(path[0], dirs);
    EXPECT_EQ(path, decoded);
}
```

---

## Q19: 行军系统崩溃恢复如何保证一致性？

**A:**

### 核心优势：懒计算天然支持恢复

**恢复流程：**
1. MapService 崩溃重启
2. 从 Redis/DB 加载所有活跃行军 `{march_id, path, start_time, speed, state}`
3. 对每个行军：
   - 懒计算当前位置：`current = path[floor((now - start_time) * speed)]`
   - 如果 `current >= path.length - 1` → 行军已到达，触发到达事件
   - 否则重新注册 occupant 时间窗口（从 current_index 开始）
   - 重建到达 timer
4. 重建视野 observer_count

### 为什么不丢状态？
- 行军位置是公式算的，不需要中间状态快照
- 只要 `{path, start_time, speed}` 在 DB 里就能完美恢复
- 对比 tick 模型：需要每 tick 的位置快照，丢了就不一致

### 注意事项
- 恢复期间暂停接收新请求（加载完成后开放）
- Timer 精度：恢复后重建的 timer 可能比原始晚几百毫秒，可接受
- 碰撞窗口重建后，已经过去的碰撞事件需要补发

---

## Q20: SLG 寻路面试中常见的陷阱问题？

**A:**

1. **"为什么不用 Dijkstra？"** → Dijkstra 是 A* 的特例（h=0），A* 有启发函数更快。但如果要一对多（找到所有可达格子），Dijkstra 更合适。

2. **"JPS 能否处理加权地形？"** → 原版不行。有 WC-JPS（Weighted-Cost JPS）变体但复杂度高，不如 HPA*。

3. **"怎么确保路径最优？"** → A* 保证最优需要 h(n) admissible（不高估）。HPA* 可能产生次优路径（chunk 间精度损失），但实际偏差 <5%，SLG 可接受。

4. **"地图变化频繁怎么办？"** → SLG 地图变化低频（建筑操作），HPA* 增量更新足够。如果是 MOBA/RTS 高频障碍变化，则不适合 HPA*。

5. **"寻路和 ECS 怎么结合？"** → 寻路是纯函数工具，不属于 ECS component。行军注册后的 MarchComp 属于 ECS 管理。

6. **"从未听说过 HPA*，只知道 A*"** → 面试中如果只答 A* 不答分层方案，说明缺乏大地图实战经验。核心是"百万格子不能暴力搜"这个直觉。
