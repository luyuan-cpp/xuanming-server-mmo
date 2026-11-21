# 观战分级扩容（L0 / L1 / L2）：环境受限后续验证交接（2026-11-21）

> 设计文档：[battle-spectate-tiered-fanout.md](../design/battle-spectate-tiered-fanout.md)
> Linux 回环实测报告：[stress-spectate-fanout-20261121.md](../stress/stress-spectate-fanout-20261121.md)
> 完整验收记录：[PROGRESS.md](../PROGRESS.md)（2026-11-21「观战分级扩容」条目）与 Java 仓 `PARITY.md`

## 0. 已完成范围（不要重做）

原 `docs/handoff/spectate-tiered-fanout-20261030/CODEX.md` 全部开发与本机可达验收已于 2026-11-21 闭环并清理：
- C++ / Go 服务端全部代码、第二轮评审修正、真 TiDB（0 SKIP）、C++ 全套单测、部署契约与实栈 4 组端到端冒烟（L0、LOBBY 中继、25s 手动保活、battle 停机 `SOURCE_LOST`）全部通过。
- Unity 客户端（`D:\luyuan\mmorpg-client`）LOBBY 接入、158 独立水位去重、重连原 `request_id` 重发已落码，离线编译 547/547 0 错，`SpectateClientStateMachineTests` 32/32 PASS。
- 独立 Java 仓（`D:\luyuan\xuanming-server-mmo`）补齐第二轮评审 10 项、`game-robot` dev 原操作与大厅档 351/352、本地 163 跨会话栅栏，完成全模块单测 + 真 Redis 7.2 & 8.10.2（各 4/4，0 SKIP）+ 真 TiDB v8.5.8 `GlobalMatchStoreIntegrationTest`（62/62，0 SKIP）。
- Linux 单机 WSL2 回环容量实测已完成并定稿默认参数（`kFanoutBatch=512`、`kSoftWatermarkBytes=256KB`、`BroadcastAutoGateThreshold=32`、`BroadcastMaxConcurrent=4`）。

本交接单**只保留两项当前本机/开发阶段无法替代的后续验证**，完成后直接删除本文件。

## 1. 待做一：公网上线前目标物理机 / 云主机混跑复测 Gate 扇出 p99

**为什么延期**：2026-11-21 的 Linux 容量实测（[stress-spectate-fanout-20261121.md](../stress/stress-spectate-fanout-20261121.md)）跑在单机 WSL2 TCP 回环上，未包含真实物理网卡软中断、公网 TLS/WSS（或边缘代理）加解密、跨主机 RTT 丢包抖动、以及 `gate` 主 `EventLoop` 同时承接真实场景 AOI 推送与常规玩家 RPC 的 CPU 竞争。

**执行时机**：具备生产同规格 Linux 物理机 / 云主机压测环境（上线前压测阶段）。

**验收内容与通过标准**：
1. 在目标 Linux 生产构建（`-O2` Release `gate`）上，混跑常规大厅在线压测负载（心跳 + 场景移动/AOI 广播 + 常规 RPC）与观战 L1 / L2 扇出：
   - 单台 `gate` 满载 `GateMaxConnections`（或目标单机在线上限，如 4.5k / 10k / 20k）中分别有 25%、50%、100% 在线连接订阅热门 5v5 / 5v5+pets 赛事局（2s/回合）。
   - 同时接入一定比例的慢客户端 / 弱网客户端（模拟公网限速或高丢包）。
2. 核对指标与通过条件：
   - 常规大厅 RPC p99 延迟与心跳存活不受观战扇出拖垮（若单批停顿 p99 超预算，把 `HubLimits::fanoutBatch` 从 `512` 下调至 `256` 复测）。
   - 物理网卡出口带宽留足余量（尤其千兆网卡节点注意 20k × 5v5+pets @2s ≈ 503 Mbps，已占链路一半，需核对单机连接上限或升级万兆网卡）。
   - 慢客户端触发 `metric=gate_spectate soft_watermark_skips > 0`，且不因观战帧触发 2 MB 硬高水位断线。
   - 若开启 L2 并发多场，同步核对 Kafka broker 出向总带宽与 `Spectate.BroadcastMaxConcurrent`（默认 4）及 `Spectate.BroadcastAutoGateThreshold`（默认 32）是否需按实际 `N_g` 调整。
3. 将目标机实测结果追加至 `docs/stress/` 新报告，并同步回填 [battle-spectate-tiered-fanout.md](../design/battle-spectate-tiered-fanout.md) §8。

## 2. 待做二：独立 Java 仓全栈进程 LOBBY 观战端到端冒烟

**为什么延期**：独立 Java 仓（`D:\luyuan\xuanming-server-mmo`）已完成全部单测、真 Redis 7.2 / 8.10.2 分片 pub/sub 集成测试与真 TiDB v8.5.8 62/62 集成测试，但本机未单独拉起完整 Java 多进程栈（`game-gateway` / `game-login` / `game-gate` / `game-scene` / `game-battle` / `game-match`）跑真人进程间 LOBBY 观战冒烟。

**前置条件**：独立 Java 仓按其 `docs/porting/` 与 `deploy/` 文档拉起完整本地或测试环境切片（Redis 7.2+、TiDB v8.5.8 / MySQL、`GAME_MATCH_SPECTATE_RELAY_ENABLED=true`、`GAME_SPECTATE_TOKEN_SECRET` 合法配置）。

**验收内容与通过标准**：
1. L0 直连冒烟：运行 Java `game-robot` `BattleScenario`（含第 16 步 dev 观战原操作 `addObserver` / `removeObserver`）与 `BattleCrossZoneScenario`（含 `Z5` 1V1 拒收 16018 与 `z5-public-watch` 公开局跨区观战），全部通过。
2. LOBBY 档实栈端到端冒烟（使用 `game-robot` 已补齐的 `SpectateSteps` 351/352 能力）：
   - 占满房间直连席（或小额注入 `observerSeatQuota` / 房间直连上限）触发下一名观众拿到 `SPECTATE_DELIVERY_LOBBY` + `spectate_ticket`；
   - 大厅连接发 `SubscribeSpectate(351)` 与 `UnsubscribeSpectate(352)`，断言大厅连接依次收到 161 → 每回合 158 → 终局 166，不建 battle 直连；
   - 手动回合等满 25s 断言不提前收到 `SOURCE_LOST`（验证 Java `BattleRoomServiceImpl` 10s 冻结心跳）；
   - 优雅停机 `game-battle` 节点时，正在进行的对局中继观众在 <10s 内立刻收到 166 `SPECTATE_END_SOURCE_LOST`（不等 30s 空闲超时）。
3. 跑通后更新 Java 仓 `PARITY.md` 观战 P1/P2 行「已知残余」。

## 3. 附注：2026-11-21 验证期间保留的他线未闭合项（非观战线缺陷）

本次验证在共享 `main` 工作区执行，以下失败项均属于其他并行会话的在途改动，本线未触碰，待各自负责线收口后顺带跑一次无过滤全量绿灯即可：
- `xuanming-server-mmo-cpp`：
  - `tools/proto_generator/protogen/internal`：`cpp/nodes/scene/handler/grpc/scene_node_service.cpp:GmExecutePlayerCommand`（他线脏改）；
  - `rpc_controller_test`：`RedisCommandRouteTest` 2 例；
  - `k8s_deploy_contract.tests.ps1`：`mysql-connection-alerts`、`chat ConfigMap` 2 例；`k8s_client_entry_contract.tests.ps1`：`ws-edge` / `redis-auth` 13 例。
- `xuanming-server-mmo`：
  - `game-gate/MessageRoutesTest` 与 `game-robot/GuildScenarioTest` 各 1 例（均因他线新增 `GuildService.RenameGuild` 未同步路由/测试表）；
  - `game-pbmysql` 1 例（他线 `guild.renamed_ms` golden DDL）。
