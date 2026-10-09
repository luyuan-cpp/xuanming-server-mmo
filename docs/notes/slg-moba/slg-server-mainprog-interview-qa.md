# SLG 服务器主程序面试 Q&A（率土之滨风格）

**Created:** 2026-04-07

---

## Q1: SLG 服务器整体架构是什么？各服务的职责？

**A:**

### 架构全景
```
客户端 ←TCP→ Gate Node (C++) ←gRPC→ Login Service (Go)
                                  ←→ Scene Manager (Go)
                                  ←→ Player Locator (Go)
                                  
Gate Node ←Kafka→ MapService (C++, 有状态大单体)
                    ├─ 行军管理
                    ├─ 格子状态
                    ├─ 视野计算
                    ├─ 碰撞检测
                    └─ 触发战斗 ──gRPC──→ BattleService (无状态, 可扩)

MapService ←Kafka→ DB Service (Go) → MySQL
MapService ←→ Redis (玩家热数据)
```

### 各服务职责

| 服务 | 语言 | 有状态 | 职责 |
|------|------|--------|------|
| Gate Node | C++ | 半(连接) | TCP 接入、协议编解码、消息路由、频控 |
| Login Service | Go | 无 | 登录认证、角色创建、Gate 分配 |
| MapService | C++ | **是** | 大地图核心：格子、行军、视野、碰撞、建筑 |
| BattleService | C++ | 无 | 纯函数战斗计算 |
| Scene Manager | Go | 无 | 场景分配、负载均衡 |
| Player Locator | Go | 无 | 玩家位置查询（在哪个服务器） |
| DB Service | Go | 无 | Kafka 消费 → MySQL 批量写入 |
| Data Service | Go | 无 | 数据层 RPC |
| Auth Service | Java | 无 | Token 认证 (sa-token) |

### 为什么 MapService 是单体？
- **紧耦合数据**：格子状态、行军路径、视野、碰撞窗口互相依赖
- **高频交互**：行军每步都涉及格子 + 视野 + 碰撞，拆开 RPC 延迟不可接受
- **内存量可控**：<1GB，单机承载
- **Region Actor 并行**：不需要拆成多进程也能利用多核

---

## Q2: 为什么选择多语言混合架构（C++ + Go + Java）？

**A:**

### 分工原则：**用合适的语言做合适的事**

| 语言 | 场景 | 原因 |
|------|------|------|
| C++ | MapService、Gate、Battle | CPU 密集、内存敏感、长连接 |
| Go | Login、DB、SceneManager | 并发友好、开发效率高、微服务生态 |
| Java | Auth | Spring Boot + sa-token 成熟方案 |

### C++ 的不可替代性
- MapService 内存 tightly packed 数据结构（Tile 16B），Go 的 GC + 指针膨胀会浪费 3-5x 内存
- Battle 纯计算 <1ms/场，C++ 比 Go 快 3-10x
- Gate 维护万级长连接，C++ + epoll/IOCP 性能上限高

### Go 的优势场景
- Login/DB 是 I/O 密集型（Redis/MySQL/Kafka），Go 的 goroutine + channel 简洁高效
- 微服务开发速度快，部署简单（单 binary）
- go-zero 框架提供 etcd 注册/发现、限流、监控

---

## Q3: 服务注册与发现怎么做？

**A:**

### 方案：etcd 注册 + watch

```
服务启动 → 在 etcd 写一个带 TTL 的 key:
  /services/login/instance-1 = {addr: "10.0.0.1:9001", weight: 100}
  TTL = 10s

其他服务 watch /services/login/ 前缀:
  - 新 key 出现 → 加入可用列表
  - key 消失 (TTL 过期) → 从列表移除
  
心跳: 每 3s 续约 TTL
```

### Go 服务 (go-zero)
```go
// 内置 etcd 注册
server := zrpc.MustNewServer(c.RpcServerConf, func(s *grpc.Server) {
    pb.RegisterLoginServiceServer(s, srv)
})
// go-zero 自动注册到 etcd，自动续约
```

### C++ 节点
```cpp
// C++ 节点启动时注册到 etcd
etcd::Client etcd("http://etcd:2379");
etcd.set("/services/gate/gate-1", 
    R"({"addr":"10.0.0.2:9100","node_id":1})", 
    etcd::KeepAlive(10s));
```

### 节点发现流程
```
Gate Node 启动:
  1. 注册自己到 etcd: /services/gate/{gate_id}
  2. Watch /services/map_service → 发现 MapService 地址
  3. Watch /services/login → 发现 Login 地址
  4. 建立 gRPC 连接
  
MapService 启动:
  1. 注册自己
  2. Watch /services/battle → 发现 BattleService 地址
```

---

## Q4: 网络层（TCP 协议）如何设计？

**A:**

### 自定义二进制协议

```
+----------+----------+----------+----------+
| Length   | MsgId    | Flags    | Payload  |
| (4 bytes)| (4 bytes)| (2 bytes)| (N bytes)|
+----------+----------+----------+----------+

Length: 整包长度 (不含自身 4 字节)
MsgId:  消息 ID (对应 protobuf message type)
Flags:  压缩标记、加密标记、序列号
Payload: protobuf 序列化后的二进制数据
```

### 粘包处理
```cpp
// 经典 Length-prefixed framing
class TcpDecoder {
    Buffer recv_buf;
    
    vector<Message> decode() {
        vector<Message> result;
        while (recv_buf.readable_bytes() >= 4) {
            uint32_t length = recv_buf.peek_uint32();
            if (recv_buf.readable_bytes() < 4 + length) break;  // 不完整
            
            recv_buf.skip(4);
            auto msg = parse_message(recv_buf.read(length));
            result.push_back(msg);
        }
        return result;
    }
};
```

### 安全措施
```
1. 包长度上限: 64KB (防巨包攻击)
2. 消息频控: 每连接 30 msg/s
3. 非法 MsgId: 断开连接
4. Token 认证: 登录成功前只允许 Login 消息
5. 加密: AES-128 (可选, 关键消息如充值)
6. 防重放: 消息序列号递增
```

---

## Q5: 登录流程怎么设计？

**A:**

### 完整登录流程
```
客户端                  Gate            Login           Auth(Java)      Redis/MySQL
  |                      |               |                |               |
  |--[1. 连接TCP]------->|               |                |               |
  |                      |               |                |               |
  |--[2. LoginReq]------>|               |                |               |
  |  (account,token)     |               |                |               |
  |                      |--[3. gRPC]--->|                |               |
  |                      |  Verify       |--[4. gRPC]--->|               |
  |                      |               |  CheckToken    | verify JWT    |
  |                      |               |<--[5. OK]-----|               |
  |                      |               |                |               |
  |                      |               |--[6. Load]----|-------------->|
  |                      |               |  PlayerData    |               |
  |                      |               |<--[7. Data]---|---------------|
  |                      |               |                |               |
  |                      |               |--[8. 分配Gate]--|               |
  |                      |               |  (检查各 Gate 负载)           |
  |                      |<--[9. OK]----|                |               |
  |                      |  (player_data, session_token) |               |
  |                      |               |                |               |
  |<--[10. LoginResp]----|               |                |               |
  |  (session, player)   |               |                |               |
```

### 关键设计点

#### 1. Token 认证（双层）
```
第一层: 平台 Token (OAuth2/JWT) → Auth Service 验证
第二层: 游戏 Session Token → Login Service 签发, Gate 校验
  Session Token: JWT(player_id, gate_id, expire, HMAC)
```

#### 2. Gate 分配策略
```go
func assignGate(gates []GateInfo) *GateInfo {
    // 1. 过滤满负载 Gate
    available := filter(gates, g => g.online < g.max_capacity * 0.9)
    
    // 2. 加权选择 (连接数少的优先)
    total_weight := sum(available, g => g.max_capacity - g.online)
    r := rand.Intn(total_weight)
    for _, g := range available {
        r -= (g.max_capacity - g.online)
        if r <= 0 { return &g }
    }
    return &available[0]
}
```

#### 3. 断线重连
```
客户端断线:
  → 30s 内用 session_token 重连同一 Gate
  → Gate 检查 session 有效 → 恢复连接, 不走全量加载
  → 超时 → 清理 session, 需重新登录
```

#### 4. 顶号（同账号重复登录）
```
A 已在线, B 用同账号登录:
  1. Login 检测 A 已在线 (Redis session)
  2. 通知 A 所在 Gate → 给 A 发"被顶号"消息 → 断开 A
  3. B 正常登录流程
```

---

## Q6: Gate Node 的核心职责和实现？

**A:**

### 职责
```
1. TCP 连接管理 (接入/断开/心跳)
2. 协议编解码 (binary → protobuf → binary)
3. 消息路由 (client msg → 对应后端服务)
4. 频控 (rate limiting per connection)
5. Kafka 消费 (接收 MapService 推送给客户端的消息)
6. 写缓冲区管理 (高水位保护)
```

### 实现架构
```cpp
class GateNode {
    // 网络层 (epoll/IOCP)
    TcpServer server;
    
    // 连接管理
    unordered_map<uint64_t, ConnectionPtr> connections;  // conn_id → Connection
    unordered_map<uint64_t, uint64_t> player_to_conn;    // player_id → conn_id
    
    // 后端 gRPC 连接
    GrpcChannelCache channel_cache;  // 到各后端服务的 gRPC channel
    
    // Kafka 消费者
    KafkaConsumer consumer;  // 监听 gate-{gate_id} topic
    
    void on_client_message(ConnectionPtr conn, Message msg) {
        // 1. 频控
        if (!conn->rate_limiter.allow()) {
            conn->close("rate limited");
            return;
        }
        
        // 2. 根据 MsgId 路由
        auto route = routing_table[msg.id];
        switch (route.target) {
            case MAP_SERVICE:
                forward_to_map_service(conn->player_id, msg);
                break;
            case LOGIN_SERVICE:
                forward_to_login(conn, msg);
                break;
            // ...
        }
    }
    
    void on_kafka_message(KafkaMessage kmsg) {
        // MapService 要推送给客户端的消息
        auto player_id = kmsg.get_player_id();
        auto it = player_to_conn.find(player_id);
        if (it != player_to_conn.end()) {
            connections[it->second]->send(kmsg.payload);
        }
    }
};
```

### 写缓冲区高水位保护
```cpp
void Connection::send(const Buffer& data) {
    write_buffer.append(data);
    if (write_buffer.size() > HIGH_WATER_MARK) {
        // 客户端接收太慢，断开防止内存爆
        LOG_WARN("Connection {} write buffer overflow, closing", id);
        close("slow client");
    }
}
```

---

## Q7: 线程模型如何设计？

**A:**

### Gate Node 线程模型
```
Main Thread: 启动、配置加载、信号处理
IO Thread Pool (4): epoll/IOCP 处理网络 I/O
  - 每个线程负责一部分连接 (round-robin 分配)
  - 收到完整消息后投递到逻辑队列
Logic Thread (1): 串行处理所有逻辑
  - 路由、session 管理、频控
  - 保证无竞态
Kafka Consumer Thread (1): 消费 Kafka 消息
  - 解析后投递到逻辑队列
Timer Thread (1): 心跳检测、定时清理
```

### MapService 线程模型
```
Main Thread: 事件循环 + 调度
Region Worker Pool (8): 处理 Region 消息
  - 每个 Region 的消息串行
  - 不同 Region 并行
Pathfinding Pool (2-4): 异步寻路计算
Timer Thread (1): Timing Wheel tick
Kafka Producer Thread (1): 异步发送消息
DB Flush Thread (1): 定时刷脏数据到 Kafka
```

### 线程间通信
```cpp
// 无锁消息队列 (MPSC: multiple producer, single consumer)
template<typename T>
class MPSCQueue {
    struct Node { T data; Node* next; };
    atomic<Node*> head;
    Node* tail;
    
    void push(T item) {  // 多线程安全
        auto* node = new Node{move(item), nullptr};
        node->next = head.exchange(node, memory_order_acq_rel);
    }
    
    vector<T> drain() {  // 单线程消费
        auto* h = head.exchange(nullptr, memory_order_acq_rel);
        // 反转链表，按推入顺序处理
        // ...
    }
};
```

---

## Q8: gRPC 在服务间通信中的使用和注意事项？

**A:**

### 使用场景
```
同步请求-响应:
  Gate → Login: VerifyToken (登录)
  MapService → BattleService: SimulateBattle (战斗)
  Client → SceneManager: EnterScene (进入场景)

异步控制消息:
  MapService → Gate: 通过 Kafka (RoutePlayer, KickPlayer)
  不用 gRPC stream（连接数爆炸问题）
```

### Channel 管理
```cpp
class GrpcChannelCache {
    // 每个目标服务维护一个 channel (复用连接)
    unordered_map<string, shared_ptr<grpc::Channel>> channels;
    mutex mu;
    
    shared_ptr<grpc::Channel> get(const string& addr) {
        lock_guard<mutex> lk(mu);
        auto it = channels.find(addr);
        if (it != channels.end()) return it->second;
        
        auto ch = grpc::CreateChannel(addr, grpc::InsecureChannelCredentials());
        channels[addr] = ch;
        return ch;
    }
};
// 注意: Channel 由 GateNode 实例持有，不用 static 
```

### 超时设置
```cpp
// 必须设置 deadline, 否则永远等待
grpc::ClientContext ctx;
ctx.set_deadline(chrono::system_clock::now() + chrono::milliseconds(500));

auto status = stub->SimulateBattle(&ctx, request, &response);
if (status.error_code() == grpc::DEADLINE_EXCEEDED) {
    // 超时处理: 重试 or 降级
}
```

### Go 服务的 gRPC 超时
```go
// go-zero 默认 2000ms, 必须显式配置
Timeout: 5000  // 在 yaml 配置

// 或代码中
ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
defer cancel()
```

---

## Q9: Kafka 在架构中的角色和使用方式？

**A:**

### 使用场景

| 场景 | Topic 模式 | 生产者 | 消费者 |
|------|-----------|--------|--------|
| Gate 控制消息 | `gate-{gate_id}` | MapService | Gate Node |
| DB 异步写入 | `db-write` | MapService | DB Service |
| 行军事件日志 | `march-events` | MapService | 数据分析 |
| 聊天消息 | `chat-{channel}` | Gate | Gate (广播) |

### Gate 控制消息
```protobuf
message GateCommand {
    uint64 target_instance_id = 1;  // 防僵尸消息
    oneof command {
        RoutePlayer route = 2;      // 路由玩家到场景
        KickPlayer kick = 3;        // 踢人
        PushMessage push = 4;       // 推送消息给客户端
    }
}
```

### 防僵尸消息
```
Gate 重启后 instance_id 变化:
  旧消息: target_instance_id = 100
  新 Gate: current_instance_id = 101
  → 丢弃旧消息，不处理
```

### DB 写入
```go
// MapService 产生脏数据 → Kafka → DB Service → MySQL
// Kafka 保证: 即使 DB Service 暂时挂了, 消息不丢
// 顺序性: 同一玩家的写入 → 同一 partition (by player_id hash)
```

### 配置
```yaml
# Go service config ([]string, not comma string)
Kafka:
  Brokers:
    - "kafka:9092"
  Topic: "gate-commands"
  Group: "gate-group"
```

---

## Q10: 数据层架构（Redis + MySQL + Kafka）如何设计？

**A:**

### 分层职责
```
         热数据 (内存)
MapService Memory ──→ 格子、行军、视野（纯内存，不持久化到 Redis）
              ↓ 
         温数据 (Redis)
Redis ──→ 玩家在线数据、session、排行榜、实时查询
              ↓
         冷数据 (MySQL)
MySQL ──→ 玩家存档、战报历史、配置、审计日志
              ↓
         归档 (S3/OSS)
S3 ──→ 赛季归档、历史战报、日志分析
```

### Redis 使用
```
Key 设计:
  player:{id}:info     → Hash (基本信息)
  player:{id}:army     → Hash (军队数据)
  player:{id}:session  → String (session token, TTL 30min)
  march:{id}           → Hash (行军数据, TTL = 行军时长 + buffer)
  rank:power            → SortedSet (战力排行)
  online:gate:{id}     → Set (Gate 上的在线玩家)
```

### MySQL 表设计
```sql
-- 玩家主表 (分库分表 by player_id % 16)
CREATE TABLE player_{0..15} (
    player_id BIGINT PRIMARY KEY,
    name VARCHAR(32),
    level INT,
    resources JSON,      -- {gold, wood, iron, food}
    city_data JSON,      -- 主城数据
    army_data JSON,      -- 军队数据
    updated_at TIMESTAMP,
    INDEX idx_alliance (alliance_id)
);

-- 战报表 (按时间分区)
CREATE TABLE battle_reports (
    report_id BIGINT PRIMARY KEY,
    attacker_id BIGINT,
    defender_id BIGINT,
    result TINYINT,
    report_blob MEDIUMBLOB,  -- zstd compressed protobuf
    created_at TIMESTAMP,
    PARTITION BY RANGE (UNIX_TIMESTAMP(created_at)) (
        PARTITION p202601 VALUES LESS THAN (UNIX_TIMESTAMP('2026-02-01')),
        PARTITION p202602 VALUES LESS THAN (UNIX_TIMESTAMP('2026-03-01')),
        ...
    )
);
```

### 写入路径选择
```
关键操作 (不能丢):
  充值/消费 → 直写 MySQL + Redis
  交易 → 直写 MySQL (有事务)

普通操作 (可容忍几秒延迟):
  资源变化 → 写 Redis → Kafka → DB Service → MySQL
  行军创建 → 写 Redis → Kafka
  建筑升级 → 写 Redis → Kafka
```

---

## Q11: 崩溃恢复（Crash Recovery）怎么做？

**A:**

### MapService 崩溃恢复流程
```
1. 崩溃检测: K8s liveness probe 失败 → 自动重启 Pod
   恢复时间目标: < 30s

2. 状态加载:
   a. 从 Redis 加载:
      - 所有活跃行军 {march_id, path, start_time, speed, state}
      - 所有建筑状态 {tile, building_type, level, build_start, build_end}
      - 在线玩家列表
   
   b. 从 MySQL 补充 (Redis 中没有的):
      - 玩家完整存档
      - 联盟数据

3. 状态重建:
   a. 行军:
      for each march:
        current_pos = path[floor((now - start_time) * speed)]
        if current_pos >= path.end:
          trigger_arrival(march)  // 已到达，补发到达事件
        else:
          rebuild_occupants(march, current_pos)  // 重注册 occupant
          rebuild_arrival_timer(march)            // 重建 timer
   
   b. 建筑:
      for each building:
        if now >= build_end_time:
          complete_build(building)  // 补发建造完成
        else:
          rebuild_timer(building, build_end_time - now)
   
   c. 视野: 全量重算 observer_count (最慢，~5s)

4. 开放服务:
   - 通知 Gate "MapService ready"
   - Gate 开始转发客户端请求
```

### 数据一致性保证
```
潜在问题: 崩溃前的 Kafka 消息可能还没被 DB Service 消费

解决:
  - Kafka 的 at-least-once 保证 → 消息不丢
  - DB Service 用 idempotent upsert → 重复消费无副作用
  - 恢复后 Redis 和 MySQL 可能有短暂不一致
  → 以 Redis 为准 (更新), MySQL 最终一致
```

---

## Q12: 赛季/合服/开服的服务端处理？

**A:**

### 开服
```
1. 部署一套新的 K8s namespace (mmorpg-zone-{name})
2. 数据库初始化: 基础配表、NPC 配置
3. 地图预生成: 资源田位置、NPC 城池
4. 预分配 10K 主城位置 (均匀散布)
5. 配置 Gate 对外暴露 (LoadBalancer / NodePort)
6. 登录白名单 → 内测
7. 全量开放
```

### 赛季结算
```
赛季结束:
  1. 禁止新行军
  2. 等待所有在途行军到达 / 超时强制结算
  3. 计算排名: 联盟领地、个人战力/杀兵
  4. 发放赛季奖励
  5. 数据归档 → S3
  6. 重置地图 (保留角色数据, 清除领地/建筑)
  7. 新赛季开始
```

### 合服
```
两个服合并:
  1. 选一个服为主服
  2. 迁移副服玩家数据 → 主服 DB
  3. 重名处理: 副服玩家名加后缀 "S2-"
  4. 重叠主城位置: 迁移副服主城到空位
  5. 联盟合并: 不自动合并, 保持独立
  6. 更新 Gate 路由: 两个入口都指向主服
```

---

## Q13: 配表（策划数据表）系统如何管理？

**A:**

### 配表流程
```
策划编辑 Excel → 导出工具 → 生成代码/数据
  ├─ C++: 生成 .h/.cpp, 编译为表加载代码
  ├─ Go: 生成 .go 结构体
  └─ Client: 生成 .json / .bytes

运行时:
  服务端启动时加载配表到内存
  热更新: reload 命令 → 重新加载 → 双 buffer 原子切换
```

### 配表类型
```
静态配表 (策划管):
  - hero_table: 武将属性/技能/升级
  - building_table: 建筑类型/等级/产出
  - troop_table: 兵种属性/克制
  - skill_table: 技能效果/CD/目标
  - reward_table: 奖励包配置

运行时配置 (运维管):
  - server_config: 开服时间/赛季配置
  - activity_config: 活动时间/奖励
```

### 配表校验
```
导出时自动校验:
  1. 外键检查: skill 表引用的 buff_id 是否存在
  2. 数值范围: attack > 0, hp > 0
  3. 唯一性: id 不重复
  4. 枚举合法: troop_type ∈ {INFANTRY, CAVALRY, ARCHER}
  5. 公式校验: 数值溢出检查 (int32 够不够)
```

---

## Q14: 安全与反作弊的设计？

**A:**

### 服务端权威原则
```
核心规则: 客户端是不可信的, 一切判定以服务器为准

客户端发的: "我要出征从 A 到 B"
服务端做的:
  1. 验证 A 是玩家的城 → 篡改起点无效
  2. 验证 B 在行军距离内 → 篡改终点无效
  3. 验证有空闲部队 → 篡改军队无效
  4. 服务端寻路 → 客户端提供的路径无用
  5. 服务端算战斗 → 客户端不参与
```

### 具体防护

#### 1. 协议层
```
- 消息频控: 30 msg/s per connection
- 包大小限制: 64KB max
- 非法消息断开: unknown MsgId → close
- Token 校验: 每条消息携带 session token
```

#### 2. 逻辑层
```
- 行军距离校验: 不能超过行军距离上限
- 冷却校验: 出征/建造等有 CD
- 资源校验: 训练兵 → 检查资源是否足够 → 扣减原子操作
- 加速道具: 服务端验证道具存在 + 扣减, 不信客户端时间
```

#### 3. 防脚本
```
// 行为异常检测
struct PlayerBehaviorMonitor {
    int actions_per_minute;
    int64_t last_action_time;
    vector<int64_t> action_intervals;
    
    bool is_suspicious() {
        // 1. 操作频率异常高 (>200/min)
        if (actions_per_minute > 200) return true;
        
        // 2. 操作间隔极其均匀 (机器人特征)
        float variance = calc_variance(action_intervals);
        if (variance < 0.01) return true;  // 方差太小 = 机器
        
        // 3. 24 小时不间断在线
        if (continuous_online_hours > 20) return true;
        
        return false;
    }
};
```

#### 4. GM 工具
```
- 查看任意玩家数据
- 修改资源/等级 (有审计日志)
- 封号/禁言 (原因 + 时长)
- 重跑战斗验证 (用 BattleInput 重新 SimulateBattle)
- 回档 (恢复到 N 小时前的快照)
```

---

## Q15: 全球化/多时区服务器如何设计？

**A:**

### 时区处理
```
核心原则: 服务端统一用 UTC, 客户端做本地化显示

服务端:
  - 所有时间戳都是 UTC epoch (int64 ms)
  - 赛季计算/定时活动用 UTC

客户端:
  - 收到 UTC timestamp → 转换为本地时区显示
  - 发送 UTC timestamp 给服务端

跨时区活动:
  - "每日 20:00 活动" → 按服务器 UTC 时间
  - 不同时区玩家看到不同本地时间
  - 如需分时区: activity_config 配 timezone_offset
```

### 多区域部署
```
方案1: 全球一服 (高延迟但统一)
  - 服务器在中心机房
  - 边缘加速 (CDN for 静态资源)
  - SLG 延迟不敏感 → 200ms 可接受

方案2: 分区服 (低延迟但分割)
  - 每个区域独立部署服务器
  - 玩家只能玩本区
  - 跨区活动需要额外联动设计

SLG 推荐: 分区服 (率土之滨模型)
  - 每个服务器 = 一个赛季实例
  - 服务器在目标地区机房
  - 玩家按地区/语言分配
```

---

## Q16: 日志系统如何设计？

**A:**

### 日志层次
```
Level:
  ERROR: 必须处理的错误 (DB 断连、OOM)
  WARN:  需要关注的异常 (频控触发、重连)
  INFO:  关键业务流程 (登录、出征、战斗结果)
  DEBUG: 开发调试 (仅开发环境)

分类:
  system.log:  服务运行状态
  business.log: 业务操作 (登录/出征/战斗/充值)
  error.log:   错误日志
  access.log:  请求日志
```

### 结构化日志
```cpp
// 用结构化 JSON 日志，方便 ELK/Loki 检索
LOG_INFO({
    {"event", "march_created"},
    {"player_id", player_id},
    {"march_id", march_id},
    {"from_tile", from},
    {"to_tile", to},
    {"path_length", path.size()},
    {"timestamp", now_utc()}
});
```

### 敏感操作审计
```
必须记录的操作:
  - 登录/登出 (IP, device, time)
  - 充值/消费 (金额, 道具, 余额)
  - GM 操作 (操作者, 目标, 变更内容)
  - 交易 (双方, 物品, 数量)
  - 封号/解封 (原因, 时长)

保留期: 至少 180 天
```

---

## Q17: 热更新/停服维护如何做？

**A:**

### 配表热更新（不停服）
```
1. 上传新配表到文件服务器
2. 发 GM 命令: reload_config
3. MapService/BattleService 各自重新加载
4. 双 buffer 原子切换 → 零停机
```

### 代码热更新（需停服）
```
滚动更新 (K8s):
  1. 新镜像部署
  2. 先停 MapService (公告维护)
  3. 等待所有行军到达 / 暂停
  4. 保存状态到 Redis
  5. 滚动更新所有 pod
  6. 新版本启动 → 从 Redis 恢复
  7. 开服

维护时间: 通常凌晨 4:00, 时长 10-30 分钟
```

### 不停服更新（无状态服务）
```
Login/DB/Auth 等无状态服务:
  K8s rolling update → 零停机
  旧 pod 优雅关闭 (drain connections)
  新 pod 就绪后接收流量
```

---

## Q18: 如何保证消息的可靠性和顺序性？

**A:**

### 消息可靠性

#### TCP 层
```
TCP 自身保证: 有序、可靠、不重复
但 TCP 断连 → 消息丢失可能

解决:
  - 关键消息要求 ACK
  - 断线重连时从 server 拉取缺失消息
  - 服务端缓存最近 N 条推送消息 (per player)
```

#### Kafka 层
```
at-least-once: acks=all, 消息不丢
幂等消费:
  - DB Service 用 upsert (INSERT ON DUPLICATE KEY UPDATE)
  - 消息携带唯一 ID，消费者去重
```

### 消息顺序性
```
同一玩家的操作需要有序:
  - Kafka: 同一 player_id 的消息 → 同一 partition
  - partition key = player_id
  - 同一 partition 内保证顺序

不同玩家可以乱序:
  - 不同 partition 并行消费
  - 提高吞吐
```

---

## Q19: ID 生成方案？

**A:**

### Snowflake ID
```
64-bit ID:
  | 1 bit (unused) | 32 bit (timestamp) | 17 bit (node_id) | 15 bit (sequence) |
  
  timestamp: 秒级 (自定义 epoch 2026-01-01)
  node_id:   最多 131072 个节点
  sequence:  每秒每节点 32768 个 ID

特点:
  - 全局唯一, 无需中心发号
  - 递增 (同节点), 可排序
  - 高性能: 纯本地计算

适用:
  - player_id, march_id, battle_report_id
  - 所有需要唯一 ID 的实体
```

### 实现
```cpp
class SnowFlake {
    static constexpr int kNodeBits = 17;
    static constexpr int kStepBits = 15;
    static constexpr int64_t kEpoch = 1773446400;  // 2026-03-12 00:00:00 UTC
    
    int64_t node_id;
    int64_t last_timestamp = 0;
    int64_t step = 0;
    
    uint64_t generate() {
        int64_t now = time(nullptr) - kEpoch;
        if (now == last_timestamp) {
            step++;
            if (step >= (1 << kStepBits)) {
                // 当前秒用完，等下一秒
                while (time(nullptr) - kEpoch == now) {}
                now = time(nullptr) - kEpoch;
                step = 0;
            }
        } else {
            step = 0;
        }
        last_timestamp = now;
        return (now << (kNodeBits + kStepBits)) | (node_id << kStepBits) | step;
    }
};
```

---

## Q20: ECS（Entity Component System）在 SLG 中的应用？

**A:**

### 适用场景
```
MapService 中的实体:
  - 行军 (March): 有位置、速度、路径、所属玩家、状态
  - 建筑 (Building): 有位置、类型、等级、建造进度
  - 城池 (City): 有位置、所有者、驻军、城墙
  - 资源田 (Resource): 有位置、类型、产量、采集者

用 ECS 管理:
  Entity: 唯一 ID
  Component: 数据 (PositionComp, MarchComp, BuildingComp)
  System: 逻辑 (MarchSystem, BuildingSystem, VisionSystem)
```

### 示例
```cpp
// Component
struct PositionComp {
    uint32_t tile_id;
};

struct MarchComp {
    vector<uint8_t> path_dirs;
    uint32_t start_tile;
    int64_t start_time;
    float speed;
    MarchState state;
    uint64_t player_id;
};

struct VisionComp {
    int32_t vision_range;
};

// System
class MarchSystem {
    void on_march_created(entt::registry& reg, entt::entity march) {
        auto& mc = reg.get<MarchComp>(march);
        auto& pc = reg.get<PositionComp>(march);
        
        // 注册 occupant
        register_occupants(mc);
        
        // 注册到达 timer
        auto timer_id = timer_wheel.add(
            calc_arrival_time(mc),
            [march = march]() { handle_arrival(march); }
        );
        reg.emplace<TimerComp>(march, timer_id);
    }
    
    uint32_t get_current_tile(MarchComp& mc, int64_t now) {
        int64_t elapsed = now - mc.start_time;
        int tiles = (int)(elapsed * mc.speed);
        return decode_tile(mc.start_tile, mc.path_dirs, 
                          min(tiles, (int)mc.path_dirs.size()));
    }
};
```

### ECS vs 传统 OOP 在 SLG 中的选择
```
ECS 优势:
  - 数据连续存储, cache 友好
  - 组合灵活: "有采集功能的行军" = MarchComp + GatherComp
  - System 独立, 便于测试

何时不用 ECS:
  - 逻辑简单的辅助系统 (不需要为了 ECS 而 ECS)
  - 单例全局状态 (地图配置, 常量)

实际: MapService 的行军/建筑/城池适合 ECS
      Login/DB 等微服务不需要 ECS
```

---

## Q21: 面试中"从零设计一个 SLG 服务器"如何回答？

**A:**

### 回答框架（5 分钟版）

```
1. 明确规模 (30s)
   "先确认一下规模: 单服 1 万在线, 1200×1200 地图, 5 万并发行军"

2. 架构总览 (60s)
   "采用 MapService 大单体 + 无状态微服务架构:
    - MapService (C++): 有状态, 管地图/行军/视野/碰撞
    - BattleService (C++): 无状态, 纯函数算战斗, 可水平扩
    - Gate Node (C++): 接入层, TCP 连接管理
    - Login/DB/SceneManager (Go): go-zero 微服务
    - 通信: gRPC 同步请求 + Kafka 异步消息"

3. 核心技术决策 (90s)
   "三个关键决策:
    a. 懒计算: 行军位置不 tick, 公式 O(1) 算
    b. Region Actor: 1444 个 Region 并行处理, 无锁
    c. 确定性战斗: 纯函数, 整数, 单 PRNG, event-log 战报"

4. 数据层 (60s)
   "三层: 
    - 内存 (地图/行军) 
    - Redis (玩家热数据) 
    - MySQL (持久化, write-behind via Kafka)"

5. 关键数字 (30s)
   "内存 < 100MB, 单战斗 < 1ms, P99 < 100ms, 
    可扩: BattleService 横向扩, MapService 通过 Region 利用多核"
```

### 延伸问题准备
```
Q: "MapService 挂了怎么办?"
A: "快速重启 <30s, 从 Redis 恢复状态, 懒计算保证行军位置正确"

Q: "10 万人怎么办?"
A: "分服. 每服 1 万人, 10 个服. SLG 天然按赛季分服"

Q: "和 MMORPG 有什么区别?"
A: "事件驱动 vs 帧驱动, 懒计算 vs tick, 战报回放 vs 实时同步"
```

---

## Q22: 面试常见陷阱

**A:**

1. **"MapService 单体不是反微服务模式吗？"**
   → SLG 地图数据紧耦合（格子+行军+视野+碰撞），拆成微服务会引入大量 RPC 延迟和一致性问题。紧耦合数据适合单体，松耦合才拆微服务。微服务不是银弹。

2. **"为什么不用 Redis 存地图？"**
   → 大量复合读写（碰撞检测要遍历格子+读时间窗口+比较）在 Redis 上需要多次 RTT 或 Lua 脚本。内存直接操作快 100 倍。

3. **"Go 也能做 MapService 吧？"**
   → 可以，但 16B/格子在 Go 里可能变成 80B+（GC 指针膨胀），50 万条 Timer 会产生 GC 压力。C++ 在内存密集+计算密集场景有明显优势。

4. **"gRPC 不好用，为什么不 HTTP？"**
   → gRPC 的 protobuf 序列化比 JSON 快 10x/小 5x，HTTP/2 多路复用比 HTTP/1.1 高效，生成代码比手写 REST 可靠。游戏服务间通信优先 gRPC。

5. **"不需要消息队列，直接 gRPC 推不行吗？"**
   → MapService → Gate 如果用 gRPC stream，40K Gate × N MapService 的全连接 = 连接爆炸。Kafka topic 模式解耦了生产者和消费者。
