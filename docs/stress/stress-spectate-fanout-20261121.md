# 观战分级扇出：Linux 帧大小与 Gate 单线程容量实测报告（2026-11-21）

> 对应需求：[battle-spectate-tiered-fanout.md](../design/battle-spectate-tiered-fanout.md) §7 / §8 / §11 P0 遗留。
> 性质：**C++ 生产代码直链 Linux 实测**（非 Windows 验证、非模拟估测），用于定稿 `Spectate.BroadcastAutoGateThreshold`、`Spectate.BroadcastMaxConcurrent`、软水位（`kSoftWatermarkBytes`）与分批大小（`kFanoutBatch`）的阈值结论。
> 注：本报告测量 C++ `gate` + Go `match` 路径，不代表独立 Java 版 Redis 分片 pub/sub 频道容量。

## 1. 测试环境与方法

- **OS / Kernel**：`Linux 6.18.58.1-microsoft-standard-WSL2 #1 SMP PREEMPT_DYNAMIC x86_64 GNU/Linux`（Docker 容器 `local/xuanming-independent-sdk:debug-20261114-source-refresh-r1`）
- **CPU**：`12th Gen Intel(R) Core(TM) i7-12700KF`（20 逻辑核；gate 扇出严格跑在单条 `muduo::net::EventLoop` 线程，客户端接收池另起 4 条 epoll 线程仅作 drain）
- **编译器与构建参数**：`g++ (GCC) 13.4.0`，`-std=c++23 -O2 -pthread -fPIC`（对齐生产 Release 优化级别）；`ulimit -n = 1048576`
- **被测生产源码（零删减直接编入）**：
  - `cpp/nodes/gate/spectate/gate_spectate_hub.cpp`（真实 `SpectateHub`：`OnFrame` / `Subscribe` / `NextTurnTargets` / `kFanoutBatch` 分批 `queueInLoop` / 合并与轮转）
  - `cpp/nodes/gate/handler/gate_push_delivery.h`（真实 `gate_push::EncodedFrame`：编码一次，复用 `conn->send(data, len)`）
  - `cpp/libs/engine/core/network/codec/{codec,websocket}.cpp` + 真实生成 Protobuf（`player_battle.pb.cc`、`battle_data.pb.cc`、`message.pb.cc`、`gate_event.pb.cc`、`gate_spectate_ticket.pb.cc`）
  - 真实 `libmuduo_net.a` / `libmuduo_base.a`：单线程 `TcpServer::setThreadNum(0)` 建真实 Linux TCP 回环连接（多监听地址 `127.0.0.1..4` 避免临时端口上限），开启 `TCP_NODELAY`，挂真实 `kSoftWatermarkBytes = 256 KB` 跳帧检查与 `kHardHighWatermarkBytes = 2 MB` 高水位回调。

## 2. 实测一：观众视角真实帧大小（Part A）

按生产 `BuildStateSnapshot` 观众脱敏口径构造（不含 `self_items`、不含 `combat` 装备概率、不含 `skill_cooldown_rounds`），每名玩家带 6 技能 + `skill_ranks` + 完整二级属性与外观 ID，宝宝带 4 技能，每单位带 2~3 个全字段 `BattleBuffEntry`。

| 对局形态 | 161 `SpectateStateS2C`（inner / Msg / TCP 线帧 / Kafka） | 158 `TurnResultS2C`（inner / Msg / TCP 线帧 / Kafka / 事件数） | 166 `SpectateEndS2C`（inner / Msg / TCP 线帧 / Kafka） |
|---|---|---|---|
| **1v1**（2 单位，2 buff/人） | 482 B / 488 B / **515 B** / 522 B | 618 B / 624 B / **651 B** / 658 B（6 事件） | 7 B / 12 B / **39 B** / 45 B |
| **1v1 + 宝宝**（4 单位，2 buff/人） | 840 B / 846 B / **873 B** / 880 B | 1,168 B / 1,174 B / **1,201 B** / 1,208 B（11 事件） | 7 B / 12 B / **39 B** / 45 B |
| **5v5**（10 单位，3 buff/人） | 2,702 B / 2,708 B / **2,735 B** / 2,742 B | 3,340 B / 3,346 B / **3,373 B** / 3,380 B（29 事件） | 7 B / 12 B / **39 B** / 45 B |
| **5v5 + 满宝宝最坏**（20 单位，3 buff/人） | 4,842 B / 4,848 B / **4,875 B** / 4,882 B | 6,561 B / 6,567 B / **6,594 B** / 6,601 B（58 事件） | 7 B / 12 B / **39 B** / 45 B |

**结论**：
- 5v5 典型回合帧（158）TCP 线字节为 **3.37 KB**（Kafka 事件 **3.38 KB**），与设计文档 §8 原先估测的 `F ≈ 3 KB` 高度吻合；5v5 全员带宝宝的极端满载场景约 **6.59 KB**（原估测上界 5 KB 的 1.32 倍）；1v1 仅 **0.65 KB**。
- 166 终局帧固定 **39 B**（TCP）/ **45 B**（Kafka），可安全豁免软水位。

## 3. 实测二：Gate 单线程 TCP 扇出容量（Part B / B2）

背靠背连续推送 10 轮（`N = 20000` 5v5+pets 为 8 轮），在每轮全部 `queueInLoop` 续发完成后立即触发下一轮，测得单核极限吞吐与按 2 秒/回合（0.5 FPS，全自动战斗上限频率）折算的 CPU 与出口带宽：

| 场景 | 订阅数 `N` | `kFanoutBatch` | TCP 线帧 `F` | 单批停顿 p50 / p99 / max | 整轮完成 p50 / p99 / max | 主循环 CPU/轮 | 极限帧率 | @2s/回合 带宽（CPU 占比） | 峰值线速 |
|---|---:|---:|---:|---|---|---:|---:|---|---:|
| 5v5（10 单位） | 500 | 512 | 3,375 B | 6.95 / 8.91 / 8.91 ms | 6.95 / 8.91 / 8.91 ms | 7.32 ms | 143.8 FPS | 0.8 MB/s（0.4%） | 231.4 MB/s |
| 5v5（10 单位） | 2,000 | 512 | 3,375 B | 8.54 / 12.27 / 12.27 ms | 34.72 / 43.77 / 43.77 ms | 36.13 ms | 28.8 FPS | 3.2 MB/s（1.8%） | 185.4 MB/s |
| 5v5（10 单位） | 5,000 | 512 | 3,375 B | 9.03 / 19.45 / 33.80 ms | 94.78 / 137.78 / 137.78 ms | 97.69 ms | 10.6 FPS | 8.0 MB/s（4.9%） | 169.8 MB/s |
| 5v5（10 单位） | 10,000 | 512 | 3,375 B | 8.69 / 15.26 / 17.95 ms | 175.12 / 199.20 / 199.20 ms | 177.55 ms | 5.7 FPS | 16.1 MB/s（8.9%） | 183.8 MB/s |
| 5v5（10 单位） | 15,000 | 512 | 3,375 B | 8.11 / 16.67 / 51.75 ms | 250.75 / 293.52 / 293.52 ms | 258.19 ms | 4.0 FPS | 24.1 MB/s（12.9%） | 192.5 MB/s |
| 5v5（10 单位） | **20,000** | 512 | 3,375 B | **7.39 / 28.29 / 128.28 ms** | **295.41 / 467.96 / 467.96 ms** | **316.65 ms** | **3.4 FPS** | **32.2 MB/s（15.8%）** | **217.9 MB/s** |
| 1v1（2 单位） | 20,000 | 512 | 653 B | 7.94 / 25.03 / 88.36 ms | 331.19 / 409.41 / 409.41 ms | 347.68 ms | 3.0 FPS | 6.2 MB/s（17.4%） | 37.6 MB/s |
| 5v5+pets（20 单位） | 10,000 | 512 | 6,596 B | 8.82 / 19.26 / 40.84 ms | 184.89 / 212.77 / 212.77 ms | 186.12 ms | 5.4 FPS | 31.5 MB/s（9.3%） | 340.2 MB/s |
| 5v5+pets（20 单位） | **20,000** | 512 | 6,596 B | **8.53 / 17.89 / 31.59 ms** | **353.00 / 399.50 / 399.50 ms** | **353.93 ms** | **2.8 FPS** | **62.9 MB/s（17.7%）** | **356.4 MB/s** |

**关键发现**：
1. **瓶颈是每连接 `write()` 系统调用次数，而非字节拷贝**：帧大小相差 10 倍（`653 B` vs `6,596 B`）时，20,000 连接的整轮 CPU 耗时几乎持平（`317~354 ms`，单目标均摊 **`15.8~17.7 µs`**，单核上限约 **56k~63k sends/s**），证明 `EncodedFrame` 编码一次 + 零重编码复用已消除序列化瓶颈。
2. **单台满载 Gate（`GateMaxConnections = 20,000` 全员观战）余量充足**：即便 20,000 人全部观看最坏 5v5+pets 全自动局（0.5 FPS），单线程观战扇出仅占 **17.7% 单核 CPU**（354 ms / 2000 ms），单机出口带宽 **62.9 MB/s（~503 Mbps）**；常规 5v5 为 **15.8% CPU / 32.2 MB/s（~258 Mbps）**。即使 4 场并发 L2 赛事局同时满员分布在 20,000 连接上（由于一人只能订一场，总订阅数上限恒 ≤ 20,000），单机扇出总成本与单场 20,000 人同量级（多 3 次 `EncodedFrame` 编码，可忽略不计）。
3. **单核 50% CPU 警戒线（留 50% 给常规大厅 RPC/心跳/场景推送）**：对应约 **55,000 次/轮（2s 周期）** 或同时段约 **28k~31k sends/s**，高于单机连接上限 `20,000`。

## 4. 实测三：分批大小 `kFanoutBatch` 敏感度（Part B3，5v5、`N = 10,000`）

| `kFanoutBatch` | 单批停顿 p50 / p99 / max | 整轮完成 p50 / p99 | 主循环 CPU/轮 | 峰值线速 |
|---:|---|---|---:|---:|
| 128 | 2.13 / 7.87 / 21.38 ms | 185.67 / 208.34 ms | 186.81 ms | 173.4 MB/s |
| 256 | 4.17 / 13.63 / 42.04 ms | 180.34 / 220.51 ms | 182.99 ms | 178.5 MB/s |
| **512（当前默认）** | **8.04 / 19.95 / 23.11 ms** | **167.42 / 226.26 ms** | **172.36 ms** | **192.2 MB/s** |
| 1024 | 16.39 / 32.59 / 38.86 ms | 162.43 / 214.75 ms | 173.15 ms | 198.2 MB/s |

**结论**：`kFanoutBatch = 512` 是吞吐与主循环停顿的最佳拐点——每批 p50 仅 **8 ms**（p99 ~20 ms，不阻塞心跳与常规 RPC），吞吐比 `128`/`256` 快 7~10%，而放大到 `1024` 几乎无吞吐收益却把单批停顿翻倍到 16.4 ms。**维持默认值 512**（如在老旧 CPU 节点上要求主循环单拍 <5 ms，可下调至 256）。

## 5. 实测四：软水位 256 KB 保护与过载轮转公平性（Part C / Part D）

- **软水位保护（Part C，`N = 200`，其中 50 条慢客户端停读且 `SO_RCVBUF = 16 KB`，150 条健康客户端）**：
  - 连续背靠背灌入 **500 帧 5v5+pets（单连接总推送量 `3.30 MB`，已远超 muduo 2 MB 硬高水位）**：
    - `softWatermarkSkips = 4,800`（平均每条慢连接优雅跳过 ~96 帧状态帧）；
    - **`hardHighWaterHits = 0`**（0 条大厅连接触及 2 MB 强制断线）；
    - 150 条健康连接全程 0 丢帧、0 断线。
  - **结论**：`kSoftWatermarkBytes = 256 KB`（约 38 帧 5v5+pets 或 76 帧 5v5 缓冲，足够吸收数秒公网抖动，又远低于 2 MB 硬断线阈值）实测生效，**维持默认值 256 KB**。
- **过载合并与轮转公平性（Part D，`N = 10,000`，`kFanoutBatch = 512`，连续 40 回合每回合仅来得及发 3 批即被新回合抢占）**：
  - `coalesced_targets = 310,128`；
  - 全部 10,000 名订阅者每人实际收到回合数：**`min = 8, avg = 8.99, max = 9`（极差仅 1）**，彻底消除表头独占 / 表尾饿死。

## 6. 参数定稿建议与限制说明

| 参数 | 当前默认值 | 实测结论与建议 |
|---|---|---|
| `kFanoutBatch`（`gate_spectate_hub.h`） | `512` | **保留 512**（p50 单批停顿 8 ms，20k 全员扇出 ~300–354 ms） |
| `kSoftWatermarkBytes`（`gate_spectate_client.cpp`） | `256 KB` | **保留 256 KB**（3.3 MB 压灌下慢连接 `hardHW = 0`，不踢大厅） |
| `Spectate.BroadcastAutoGateThreshold`（`match`） | `32` | **保留 32**（单场 32 台 gate 走 L1 时 battle 仅产 32×3.4 KB ≈ 108 KB/回合 = 54 KB/s；跨 32 台后再切 L2 写 1 条避免 battle 出口膨胀；全服 gate 总数较小时建议保持 ≤ `N_g / 2`） |
| `Spectate.BroadcastMaxConcurrent`（`match`） | `4` | **保留 4**（4 场 5v5+pets 同时走 L2 时，单台 gate 若无观众也仅消费 `4 × 6.6 KB / 2s = 13.2 KB/s` 空读；Kafka broker 对 `N_g = 2,000` 台 gate 的出向总带宽 = `4 × 2000 × 3.3 KB/s ≈ 26.4 MB/s`，若放开到 100 场则膨胀至 660 MB/s；因此保守维持 4，大型赛事按需调至 8~16 需先核算 Kafka 出口带宽） |

**环境边界提示**：
- 本实测在单机 WSL2 Linux TCP 回环上执行，未计入公网 TLS/WSS 加解密、真实物理网卡软中断、与真实游戏场景/AOI 广播共享同一 `EventLoop` 的额外开销。单台万兆网卡（10 Gbps ≈ 1.25 GB/s）在 20k 5v5+pets 满载（503 Mbps）下仍处安全区，若使用千兆网卡（1 Gbps）则单台 gate 满载 5v5+pets 观战出口带宽已达链路 50%，生产上公网前建议在目标物理机/云主机与真实混跑负载下复测一次 p99。
