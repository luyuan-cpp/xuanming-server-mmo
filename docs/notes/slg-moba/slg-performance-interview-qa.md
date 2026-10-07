# SLG 沙盘性能优化面试 Q&A（率土之滨风格）

**Created:** 2026-04-07

---

## Q1: SLG 服务器性能优化的核心思路是什么？和 MMORPG 有什么不同？

**A:**

### SLG 性能特征

| 特征 | SLG | MMORPG |
|------|-----|--------|
| 主要瓶颈 | 大地图内存 + 行军/视野计算 | 场景同步 + 技能碰撞 |
| 计算模型 | 事件驱动 + 懒计算 | 逐帧 tick |
| 并发模型 | Region Actor + 异步消息 | AOI 广播 + 帧同步/状态同步 |
| 数据规模 | 百万格子 × 5万行军 × 1万玩家 | 几十~几百人/场景 |
| CPU 特征 | 低频高复杂度（寻路、战斗） | 高频低复杂度（帧 tick） |
| 内存特征 | 大地图常驻内存 ~1GB | 场景对象按需加载 |

### 三大优化支柱

1. **懒计算（Lazy Evaluation）**：不主动算，只在查询时算
2. **空间分块（Region Actor）**：大地图分块并行处理
3. **事件驱动（Event-Driven）**：用 timer/callback 代替 tick 轮询

---

## Q2: 懒计算在 SLG 中有哪些应用场景？价值有多大？

**A:**

### 行军位置 — 最经典的懒计算
```
// 不是每帧更新50K行军的位置
// 只在需要时算:
current_tile = path[floor((now - start_time) * speed)]
```
**节省**: 50K 行军 × 30 fps tick = 150 万次/秒 → 0 次/秒（查询时才算）

### 资源产出
```
// 不是每秒给玩家加资源
// 查询时算:
current_gold = last_gold + (now - last_update) * production_rate
```
**节省**: 10K 在线玩家 × 4 种资源 × 每秒 tick = 4 万次/秒 → 0

### 建筑建造
```
// 不是每秒检查建造是否完成
// 注册一个 timer 在完成时刻触发:
timer.schedule(build_finish_time, [building_id] { complete_build(building_id); })
```

### 城墙修复
```
// 不逐秒修城墙
current_wall_hp = wall_hp + floor((now - last_damage) / 3600) * repair_rate
```

### 科技研究、士兵训练、采集进度
全部同理：存 `{start_time, rate, last_value}`，查询时公式算。

### 价值量化
| 系统 | tick 模型计算量/秒 | 懒计算 计算量/秒 | 节省 |
|------|-------------------|------------------|------|
| 行军位置 | 150 万 | ~500（按查询频率） | 99.97% |
| 资源产出 | 4 万 | ~100 | 99.75% |
| 建筑倒计时 | 2 万 | 0（timer 驱动） | 100% |
| 总计 | ~200 万次 | ~600 次 | **99.97%** |

---

## Q3: Region Actor 并发模型是什么？如何实现？

**A:**

### 问题
1200×1200 地图 = 144 万格子，单线程处理所有操作会成瓶颈。

### Region 划分
```
地图切成 Region, 每个 32×32 格子
1200 / 32 ≈ 38 × 38 = 1444 个 Region
```

### Actor 模型
```
每个 Region 是一个 Actor:
  - 拥有自己的消息队列
  - 串行处理队列中的消息（无锁）
  - Region 间通过异步消息通信

线程池 (N 线程) 调度 Region:
  - 每个 Region 的消息在同一时刻只被一个线程处理
  - 不同 Region 的消息可并行处理
```

### 实现
```cpp
class Region {
    uint32_t region_id;
    std::queue<Message> inbox;
    std::mutex inbox_mutex;  // 仅保护入队，处理时无锁
    
    // 区域内数据（32×32 格子 + 区域内行军）
    Tile tiles[32][32];
    vector<March*> local_marches;
    
    void process_message(Message& msg) {
        switch (msg.type) {
            case MARCH_ENTER_REGION:
                handle_march_enter(msg);
                break;
            case MARCH_LEAVE_REGION:
                handle_march_leave(msg);
                break;
            case BUILD_REQUEST:
                handle_build(msg);
                break;
            // ...
        }
    }
};

class RegionScheduler {
    ThreadPool pool;
    vector<Region> regions;
    
    void dispatch(uint32_t region_id, Message msg) {
        regions[region_id].inbox.push(msg);
        pool.schedule([&, region_id] {
            regions[region_id].process_all_messages();
        });
    }
};
```

### 跨 Region 操作
行军穿越多个 Region:
```
March 从 Region A → Region B:
1. Region A 处理: march.leave()，发消息给 Region B
2. Region B 收到消息: march.enter()
3. 消息传递是异步的，但每个 Region 内部是串行的
→ 无锁 + 并行
```

### 性能
- 1444 个 Region，8 线程 → 最高 8 个 Region 并行处理
- 单 Region 32×32 = 1024 格子 + 局部行军，数据量小，cache 友好
- 热点 Region（主城集中区）可能成瓶颈 → 监控 + 动态负载

---

## Q4: 行军系统的性能优化有哪些关键点？

**A:**

### 1. 路径存储优化（72% 内存节省）
```
方向编码: start_tile + dirs[] (1B/步)
vs uint32 tile_id 数组 (4B/步)

50K 行军 × 100 格平均:
  原始: 50K × 100 × 4B = 20MB
  优化: 50K × (4 + 100) = 5.2MB
```

### 2. Occupant 时间窗口优化
```cpp
// 每格维护时间窗口列表，但大多数格子是空的
// 用稀疏结构:
unordered_map<uint32_t, vector<Occupant>> occupant_map;
// 只有被行军经过的格子才有条目

// 定期清理过期条目
void gc_expired_occupants(int64_t now) {
    for (auto it = occupant_map.begin(); it != occupant_map.end(); ) {
        auto& vec = it->second;
        vec.erase(remove_if(vec.begin(), vec.end(),
            [now](auto& o) { return o.leave_time < now; }), vec.end());
        if (vec.empty()) it = occupant_map.erase(it);
        else ++it;
    }
}
```

### 3. 碰撞检测优化
```
// Occupant 按 enter_time 排序
// 新行军注册时二分查找，跳过已过期的条目
auto it = lower_bound(occupants.begin(), occupants.end(), new_enter);
// 只检查时间窗口可能重叠的条目
```

### 4. Timer 优化（Hashed Timing Wheel）
```
50K 行军 → 50K 到达 timer
用 Timing Wheel 代替 priority_queue:
  - 插入: O(1) vs O(log n)
  - 取消: O(1) vs O(n) 或 lazy delete
  - tick: O(1) amortized

轮盘配置:
  - 256 槽 × 4 层
  - 最大延迟: 256^4 tick ≈ 40 亿 tick
  - tick 精度: 100ms（SLG 不需要毫秒级）
```

### 5. 行军创建/销毁池化
```cpp
ObjectPool<March> march_pool(100000);
March* m = march_pool.acquire();
// ... 使用 ...
march_pool.release(m);
// 避免频繁 new/delete 产生内存碎片
```

---

## Q5: 视野/迷雾系统的性能如何优化？

**A:**

### 基础模型
```
每格维护: map<player_id, int> observer_count;
observer_count > 0 → 该玩家可见此格

视野来源:
  - 己方城池/建筑 (固定范围)
  - 行军路过 (动态)
  - 斥候塔 (大范围)
```

### 瓶颈分析
- 每个玩家平均看 ~500-2000 格
- 10K 在线玩家 → 10M-20M 格 observer 条目
- 行军移动时需要增减 observer_count

### 优化方案

#### 1. 滑动窗口增量更新
```
行军从 tile A → tile B (相邻格):
  视野范围 R = 3 (7×7)

传统: 
  取消 A 周围 49 格的 observer → 49 个 decrement
  添加 B 周围 49 格的 observer → 49 个 increment
  共 98 次更新

滑动窗口:
  A→B 移动方向: 右
  只取消左边一列 (7 格): 7 个 decrement
  只添加右边一列 (7 格): 7 个 increment
  共 14 次更新 → 节省 86%
```

#### 2. observer_count 用数组而非 map
```cpp
// 每格用 player_id 到 count 的映射太重 (hash map 开销)
// 方案: 联盟级别聚合
// observer_count[alliance_id] 而非 observer_count[player_id]
// 联盟数 << 玩家数 (通常 100-500 vs 10K)
```

#### 3. 视野脏标记 + 批量更新
```
不是每次移动立刻更新视野:
1. 标记 Region 为"视野脏"
2. 每 200ms 批量处理所有脏 Region 的视野变更
3. 合并：行军连续移动 3 格 → 只算头尾差异
```

#### 4. 建筑视野预计算
```
城池/箭塔的视野是固定的:
  建造时一次性算 observer_count
  拆除时一次性减
  不需要每帧更新
```

---

## Q6: 大地图内存如何优化？

**A:**

### 格子数据结构精简
```cpp
// BAD: 每格 64 字节
struct Tile_Bad {
    int32_t terrain;         // 4
    int32_t building_id;     // 4
    int32_t owner_id;        // 4
    int32_t alliance_id;     // 4
    vector<Occupant> occs;   // 24+ (vector 本身)
    map<int, int> observers; // 48+ (map 本身)
    // padding 等... 合计 ~80+ bytes
};
// 1.44M × 80 = 115 MB 仅格子数据

// GOOD: 精简到 16 字节
struct Tile_Good {
    uint8_t terrain;         // 1 (枚举, <256 种)
    uint8_t flags;           // 1 (位域: is_blocked, is_resource, etc.)
    uint16_t owner_id;       // 2 (玩家编号, <65K)
    uint32_t building_id;    // 4 (0=无建筑)
    uint32_t data;           // 4 (扩展数据, 按需解释)
    uint16_t alliance_id;    // 2
    uint16_t reserved;       // 2
};
// 1.44M × 16 = 23 MB

// occupant 和 observer 用稀疏结构外置
unordered_map<uint32_t, vector<Occupant>> tile_occupants; // 只存有行军经过的格子
unordered_map<uint32_t, SmallMap<uint16_t, uint8_t>> tile_observers; // 稀疏
```

### 地图总内存预算
```
格子数组 (1.44M × 16B):           23 MB
Occupant 表 (50K 行军, 稀疏):    ~10 MB
Observer 表 (稀疏, 联盟聚合):    ~20 MB
行军数据 (50K × ~100B):          ~5 MB
路径存储 (方向编码):              ~5 MB
建筑队列:                        ~2 MB
HPA* 抽象图:                     ~1 MB
─────────────────────────────────
总计:                            ~66 MB (远小于 1GB 预算)
```

---

## Q7: Timer（定时器）系统的性能如何优化？

**A:**

### SLG 中的 Timer 来源
```
行军到达:           50K (峰值)
建筑建造完成:       10K
科技研究完成:       5K
训练完成:           10K
资源采集完成:       5K
buff/debuff 到期:   20K
─────────────────────
总计:              ~100K 活跃 timer
```

### 数据结构选择

| 方案 | 插入 | 取消 | 触发 | 内存 |
|------|------|------|------|------|
| std::priority_queue | O(log n) | O(n) | O(log n) | 紧凑 |
| std::set/map | O(log n) | O(log n) | O(log n) | 红黑树节点开销 |
| **Hashed Timing Wheel** | **O(1)** | **O(1)** | **O(1) amortized** | 数组 |
| Hierarchical Timing Wheel | O(1) | O(1) | O(1) amortized | 多层数组 |

### Hashed Timing Wheel 实现
```cpp
class TimingWheel {
    static constexpr int SLOTS = 256;
    static constexpr int TICK_MS = 100;  // 100ms 精度
    
    struct TimerNode {
        uint64_t timer_id;
        int64_t expire_tick;
        function<void()> callback;
        TimerNode* next;
    };
    
    TimerNode* slots[SLOTS];
    int64_t current_tick = 0;
    
    uint64_t add(int64_t delay_ms, function<void()> cb) {
        int64_t expire = current_tick + delay_ms / TICK_MS;
        int slot = expire % SLOTS;
        auto* node = pool.acquire();
        node->expire_tick = expire;
        node->callback = move(cb);
        node->next = slots[slot];
        slots[slot] = node;
        return node->timer_id;
    }
    
    void tick() {
        current_tick++;
        int slot = current_tick % SLOTS;
        auto* node = slots[slot];
        TimerNode* keep_head = nullptr;
        while (node) {
            auto* next = node->next;
            if (node->expire_tick <= current_tick) {
                node->callback();
                pool.release(node);
            } else {
                // 还没到期（多圈），留着
                node->next = keep_head;
                keep_head = node;
            }
            node = next;
        }
        slots[slot] = keep_head;
    }
};
```

### Hierarchical Timing Wheel (多层)
```
Layer 0: 256 slots × 100ms = 25.6s    (近期)
Layer 1: 256 slots × 25.6s = 109min   (中期)
Layer 2: 256 slots × 109min = 19.4day  (远期)

近期 timer 在 Layer 0 直接触发
远期 timer 在高层存储，降级到 Layer 0 后触发
所有操作仍是 O(1)
```

---

## Q8: 数据库写入性能如何优化？

**A:**

### Write-Behind 模式
```
玩家操作 → 内存更新 → Kafka 消息 → DB Service 批量写 MySQL

优势:
1. 玩家操作响应不等 DB
2. DB Service 可合并写入 (batch insert/update)
3. Kafka 提供可靠传输 + 缓冲
```

### 批量写入
```go
// DB Service 消费 Kafka, 攒批写入
batch := make([]WriteOp, 0, 100)
ticker := time.NewTicker(200 * time.Millisecond)

for {
    select {
    case msg := <-kafka_consumer:
        batch = append(batch, parse(msg))
        if len(batch) >= 100 {
            flush(batch)
            batch = batch[:0]
        }
    case <-ticker.C:
        if len(batch) > 0 {
            flush(batch)
            batch = batch[:0]
        }
    }
}

func flush(batch []WriteOp) {
    // 分组 by 表名
    grouped := group_by_table(batch)
    for table, ops := range grouped {
        // batch upsert
        exec_batch_upsert(table, ops)
    }
}
```

### 脏标记优化
```cpp
// 不是每次修改都发 Kafka
// 标记脏，定时刷新
struct PlayerData {
    bool dirty = false;
    int64_t last_flush_time;
    
    void modify() {
        // ... 修改数据 ...
        dirty = true;
    }
};

// 每 5 秒检查脏数据
void flush_dirty_players() {
    for (auto& [id, data] : players) {
        if (data.dirty) {
            kafka_produce("db-write", serialize(data));
            data.dirty = false;
            data.last_flush_time = now();
        }
    }
}
```

### 关键操作直写
少数操作不能走 write-behind:
- 充值/消费 → 直写 MySQL + Redis（防丢）
- 交易记录 → 直写（审计需求）
- 其他操作可容忍几秒延迟

---

## Q9: 网络层性能如何优化？

**A:**

### 协议优化
```
1. Protobuf 序列化: 比 JSON 小 5-10x, 解析快 10x
2. 消息合并: 同帧多个小消息合并为一个 TCP 包
3. 增量同步: 只发变化 delta, 不发全量

// 行军更新举例:
// BAD: 每秒发 50K 行军的完整位置
// GOOD: 不发。客户端根据 {path, start_time, speed} 自行插值
// 只在状态变化时发 (召回/加速/到达/战斗)
```

### 连接管理
```
Gate Node 管连接:
- 每连接一个 buffer (避免频繁 alloc)
- Epoll/IOCP 多路复用
- 心跳 30s, 超时 90s 断开
- 写缓冲区高水位保护 (防慢客户端撑爆内存)
```

### 消息频控
```cpp
struct RateLimiter {
    int32_t tokens;
    int32_t max_tokens;
    int64_t last_refill;
    int32_t refill_rate;  // tokens per second
    
    bool allow(int64_t now) {
        // 补充 token
        int64_t elapsed = now - last_refill;
        tokens = min(max_tokens, tokens + elapsed * refill_rate / 1000);
        last_refill = now;
        
        if (tokens > 0) { tokens--; return true; }
        return false;  // 拒绝
    }
};

// 全局: 每连接 30 msg/s
// 细分: 行军操作 5 msg/s, 聊天 3 msg/s
```

---

## Q10: 如何做性能监控和 profiling？

**A:**

### 关键指标
```
1. QPS/TPS: 每秒处理请求数 (分接口统计)
2. P99 延迟: 99 分位请求延迟 (目标 <50ms)
3. Region 处理时间: 每个 Region 单次处理耗时
4. Timer 触发延迟: timer 实际触发时间 vs 预期时间
5. 内存使用: RSS, 各模块分配量
6. GC (Go service): GC 暂停时间, 频率
7. Kafka lag: 消费者落后量
8. MySQL QPS: 写入/查询速率
```

### C++ profiling
```cpp
// 轻量级内嵌 profiler
class ScopedTimer {
    const char* name;
    chrono::steady_clock::time_point start;
public:
    ScopedTimer(const char* n) : name(n), start(chrono::steady_clock::now()) {}
    ~ScopedTimer() {
        auto elapsed = chrono::steady_clock::now() - start;
        auto us = chrono::duration_cast<chrono::microseconds>(elapsed).count();
        metrics::record_latency(name, us);
    }
};

#define PROFILE_SCOPE(name) ScopedTimer _timer_##__LINE__(name)

void process_march() {
    PROFILE_SCOPE("process_march");
    // ...
}
```

### 热点 Region 监控
```
每分钟统计每个 Region 的:
- 处理消息数
- 平均处理时间
- 行军通过数量
- observer 增减次数

热点 Region (top 10) 告警 → 可能需要玩法层面疏散 (如限制城池密度)
```

### 压测方法
```
1. Robot 客户端 (go): 模拟 10K 玩家同时在线
2. 行军压测: 每 robot 每分钟出征 2 次 → 20K 行军/分钟
3. 战斗压测: 集中向同一区域出征 → 触发大量碰撞/战斗
4. 开服压测: 模拟第一天 10K 新玩家同时上线
5. 目标: P99 < 100ms, 内存 < 1GB, 不 OOM
```

---

## Q11: MapService 单点如何保证可用性？

**A:**

### 问题
MapService 是有状态大单体，不可水平扩，挂了就是全服挂。

### 方案

#### 1. 快速重启 + 数据恢复
```
崩溃 → 自动重启 (K8s restart) → 从 Redis/DB 加载状态 → 恢复
目标恢复时间: < 30 秒

恢复内容:
- 行军: 从 DB 加载 {path, start_time, speed}, 懒计算当前位置
- 建筑: 从 DB 加载, 懒计算当前状态
- 视野: 重建 observer_count (可容忍短暂看不见)
- Timer: 重建 (到达/建造/训练)
```

#### 2. Redis Checkpoint
```
每 10 秒将关键内存状态快照到 Redis:
- 所有活跃行军 snapshot
- 建筑队列 snapshot
- 格子变更 delta log

崩溃恢复时:
Redis 有最近 10s 的数据 → 丢失最多 10s 的操作
比全量从 MySQL 恢复快 10x
```

#### 3. 主备切换（高级）
```
Primary MapService (读写)
  ↓ 实时复制内存变更 (状态日志)
Standby MapService (只读，跟随)

Primary 挂 → Standby 提升为 Primary (< 5s)
实现复杂度高，多数中小型 SLG 不做，靠快速重启兜底
```

---

## Q12: 开服洪峰（万人同时涌入）如何应对？

**A:**

### 瓶颈分析
```
开服第一秒:
- 10K 登录请求涌入
- 10K 创建角色
- 10K 新手主城安置（需要找空格子）
- 10K 新手引导触发（出征、建造）
```

### 方案

#### 1. 登录排队
```
Login Service 前面加排队:
  - 每秒放行 500 人
  - 超出的进排队队列
  - 客户端显示"排队中，前方 N 人"
  - 排到了发 Token，Token 有时效
```

#### 2. 预分配主城位置
```
开服前预算好 10K 个主城位置:
  - 均匀散布在地图上
  - 按注册顺序分配
  - 不需要实时搜索空格子
```

#### 3. 分批加载
```
玩家登录后:
  - 第一包: 只给基本信息 (主城坐标、资源量)
  - 延迟加载: 视野范围内地图数据
  - 按需加载: 其他系统数据
```

#### 4. 横向扩容无状态服务
```
Login Service: 4 pods → 16 pods (开服前扩)
BattleService: 2 pods → 8 pods
Gate Node: 4 → 8 (每个承载 1.5K 连接)
```

#### 5. 限流保护
```
每个接口有 rate limit
超限返回"服务器繁忙"
保护 MapService 不被打穿
```

---

## Q13: 赛季大战（国战）的性能如何保障？

**A:**

### 场景
赛季决战：3-5 个联盟同时进攻要塞，数千行军、数百场战斗集中爆发。

### 针对性优化

#### 1. BattleService 弹性扩容
```
正常: 2 pods, 每 pod 4 核
国战: 自动扩到 8 pods
触发条件: battle QPS > 100/s or 待处理队列 > 200
```

#### 2. 战斗排队 + 优先级
```
战斗请求队列:
  - 玩家相关: 高优先级 → 立即处理
  - NPC/城防: 低优先级 → 可排队

处理保证:
  - 高优先级 P99 < 100ms
  - 低优先级 P99 < 500ms
```

#### 3. 战报异步推送
```
正常: 战斗完立刻推送双方
国战: 
  - 战斗完先记录结果（影响地图状态）
  - 战报压入推送队列，后台批量发
  - 客户端可手动请求刷新
```

#### 4. 视野更新降频
```
正常: 行军每步更新视野
国战高密度区域: 
  - 200ms 批量更新一次
  - 合并多步移动的视野变化
  - 短暂视野不精确可接受
```

---

## Q14: 如何检测和处理内存泄漏？

**A:**

### C++ 内存泄漏检测
```cpp
// 1. Debug 模式用 AddressSanitizer / LeakSanitizer
// CMake: -DCMAKE_CXX_FLAGS="-fsanitize=address,leak"

// 2. 自定义内存追踪
class MemoryTracker {
    static unordered_map<string, size_t> allocation_counts;
    
    template<typename T>
    static T* tracked_new(const char* tag) {
        allocation_counts[tag]++;
        return new T();
    }
    
    static void dump_stats() {
        for (auto& [tag, count] : allocation_counts) {
            LOG_INFO("MemTracker: {} = {} objects", tag, count);
        }
    }
};

// 3. 周期性内存快照
// 每小时记录一次 RSS → 如果持续增长 → 泄漏
```

### 常见泄漏场景（SLG）
```
1. 行军结束后 path 未释放 → 用对象池 + 显式 release
2. Timer callback 持有对象引用 → weak_ptr
3. Observer count 只增不减 → 确保行军销毁时减回
4. 战报未清理 → 定期 trim 到 N 条
5. Kafka 消息积压 → 监控 lag, 超限告警
```

---

## Q15: 多线程下如何避免竞态条件？

**A:**

### Region Actor 模型天然避免
```
核心原则: 数据归属明确
  - 格子数据归属所在 Region
  - Region 内串行处理 → 无竞态
  - 跨 Region 用异步消息 → 无共享内存

需要锁的极少数场景:
  - 全局 ID 生成器 (atomic 即可)
  - 全局排行榜 (读写锁, 读多写少)
  - 在线玩家表 (concurrent hash map)
```

### 行军跨 Region 的一致性
```
行军从 Region A 到 Region B:

1. Region A 处理 "march leave" → 发消息 → Region B
2. 在消息到达 Region B 之前, 行军在两个 Region 都不存在 (短暂不一致)
3. Region B 收到消息 → 处理 "march enter"

这个短暂不一致在 SLG 中可接受:
  - 不是所有操作都涉及跨 Region
  - 查询时对边界行军做公式校正
```

### 读写分离
```
场景查询 (如"这个格子有什么"):
  - 可以读稍微旧的数据 (eventual consistency)
  - 不需要加锁
  
状态变更 (如"在这里建造"):
  - 必须通过 Region Actor 消息
  - 串行化保证一致性
```

---

## Q16: GC 优化（Go 服务）

**A:**

### 问题
Go 服务（Login、DB Service）在高 QPS 下 GC 暂停可能影响延迟。

### 优化手段

```go
// 1. 减少堆分配
// BAD: 每次请求 new 一个临时对象
result := &Response{...}

// GOOD: 对象池
var responsePool = sync.Pool{
    New: func() interface{} { return &Response{} },
}
resp := responsePool.Get().(*Response)
defer responsePool.Put(resp)

// 2. 预分配 slice/map
// BAD:
m := make(map[string]int) // 空 map, 后续扩容触发 GC

// GOOD:
m := make(map[string]int, 1000) // 预分配

// 3. 调整 GOGC
// 默认 GOGC=100, 内存充裕时可调大
// GOGC=200: GC 频率减半, 内存多使用一倍
runtime.SetGCPercent(200)

// 4. 减少指针
// 更少的指针 = GC 扫描更快
// 用 value type (struct) 代替 pointer type (*struct) 存储
```

---

## Q17: 配表热更新对性能的影响？

**A:**

### 场景
策划修改数据表（武将属性、建筑参数）→ 需要不停服更新。

### 方案：双 buffer 切换
```cpp
class ConfigManager {
    atomic<ConfigData*> current;
    ConfigData* backup;
    
    void hot_reload(const string& new_config_path) {
        // 1. 在 backup buffer 加载新配表
        backup->load(new_config_path);
        
        // 2. 原子切换
        ConfigData* old = current.exchange(backup);
        
        // 3. 等所有正在使用 old 的请求完成
        // (使用 RCU 或引用计数)
        wait_for_drain(old);
        
        // 4. old 变为新的 backup
        backup = old;
    }
};
```

### 性能影响
- 切换本身 O(1)，不影响性能
- 加载新配表可能耗时（大配表 100ms 级别）→ 异步线程执行
- 内存翻倍（两份配表）→ 配表通常 <100MB，可接受

---

## Q18: SLG 性能面试常见陷阱？

**A:**

1. **"为什么不用逐帧 tick？Unity/UE 都是 tick 模型"**
   → SLG 不是动作游戏。50K 行军逐帧 tick = 白白浪费 CPU。懒计算 O(1) 查位置才是正道。"只在需要的时候算"是 SLG 性能的核心哲学。

2. **"MapService 不能扩怎么办？"**
   → 有状态大单体不意味着性能差。Region Actor 可利用多核。144 万格子 <100MB 内存。单机轻松承载 10K 在线。SLG 不是 MMO，不需要百万同时在线。

3. **"为什么不用 Redis 做 MapService？"**
   → 格子状态 + 行军 + 视野是紧耦合的复合读写，Redis 的 Key-Value 模型做不到高效的空间查询和时空碰撞检测。内存比 Redis 强 100 倍。

4. **"内存 1GB 够吗？"**
   → 上面算过，格子 23MB + 行军 5MB + 路径 5MB + 视野 20MB + 其他 13MB ≈ 66MB。加上运行时开销、缓冲区，512MB 绰绰有余。

5. **"压测的目标是什么？"**
   → P99 延迟 <100ms，内存 <1GB，QPS >10K/s，同时在线 10K，并发行军 50K，战斗 1000 场/秒。这些数字要能脱口而出。
