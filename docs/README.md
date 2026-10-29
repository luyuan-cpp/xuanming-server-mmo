# 文档总览

本目录是服务端的全部文档。第一次来,按下面的顺序读:

1. [design/ARCH.md](design/ARCH.md) —— 架构总览:进程划分、数据流、关键决策
2. [design/onboarding.md](design/onboarding.md) —— 开发者上手指南:环境、构建、起服
3. [ops/k8s-open-server-runbook.md](ops/k8s-open-server-runbook.md) —— 开服与部署的操作步骤
4. 之后按要改的模块,到下面的索引里找对应的设计文档

协议生成的当前命令、输入完整性与守护段规则见[生成器主说明](../tools/proto_generator/README.md)；旧 pbgen 三语杂记已由该入口与专项设计承接。

脚本归属见[脚本主说明](../tools/scripts/README.md)；C++ 串行构建与 scene 编译器 PDB 隔离见[C++ 主说明](../cpp/README.md)。

## 目录约定

| 目录 | 放什么 | 不放什么 |
|------|--------|----------|
| [`design/`](design/) | 架构与各服务的设计决策:为什么这么做、接口与不变量、落码记录 | 压测数据、交接说明、与本项目无关的学习笔记 |
| [`ops/`](ops/) | 运维手册(runbook)、事故复盘、发布检查清单 | 设计推导 |
| [`stress/`](stress/) | 每一轮压测的复盘,以及作为对比基线的 `prev-summary*.txt` | 压测方法论(那是 `design/` 或 `ops/` 的事) |
| [`handoff/`](handoff/) | 会话之间的交接说明与小结:做到哪、剩什么、怎么验 | 长期有效的设计(应沉淀回 `design/`) |
| [`notes/`](notes/) | 调研、问答记录、待办,以及与本项目无直接关系的学习笔记 | 规范性内容 |
| [`archive/`](archive/) | 已过期、仅作历史保留的文档(对应的代码或方案已不存在) | 仍然有效的任何东西 |
| [`teaching/`](teaching/) | 网络与数据结构入门的教学动图和示例代码 | — |
| [`PROGRESS.md`](PROGRESS.md) | 开发流水账,只追加 | — |

文件命名:小写、连字符分隔,服务或模块名打头(`gate-…`、`guild-…`),带日期的写成 `-YYYYMMDD` 后缀。
早期文档里的下划线命名与 `_en` / `_zh` 多语言副本是历史遗留,因为被源码注释按路径引用,没有改名。

新增或改名文档后,运行下面的命令刷新索引(只改两个标记行之间的内容):

```powershell
python tools/scripts/gen_docs_index.py
```

ECS 改造交接见 [总索引](handoff/ecs-refactor/README.md);当前修正与未验收项见 [第 1 批 Codex 实施回执](handoff/ecs-refactor/batch1-codex-receipt.md)。第 1–5 批的代码审查结论见 [审查报告](handoff/ecs-refactor/review-report-20261015.md),修复任务见 [第 6 批任务单](handoff/ecs-refactor/batch6-review-fixes.md)。早期在途证据保留在 [Codex 只读回执](handoff/ecs-refactor/codex-readonly-receipt.md),运行时与存盘采样稿由该回执索引。

跨 zone 的完整节点图、源端双表 SQL ACK 交接时序、目标重登入场时序、[抵达后存盘与 go/db 选库](design/cross-zone-scene-travel.md#1551-zone-2-scene-正常存盘的完整时序)及水平扩展边界见 [跨 zone 场景传送 §15](design/cross-zone-scene-travel.md#15-跨-zone-完整节点与数据流程2026-10-20源码核对)（2026-10-20 源码核对，非本轮运行验收）。

Redis 物理水平扩展的用户目标、当前存盘链问题、源码入口与后续验收要求见 [Claude Code 接手文档](handoff/redis-horizontal-scaling-requirements-20261022.md)；选定方案（玩家域 Redis Cluster 按 `{player_id}` 分片、控制域留原实例、C++ 自研路由层同 slot 停等、补投索引按 slot、Redis ≥ 8.4 原子迁移）与分批计划见 [玩家态 Redis 集群化设计](design/redis-player-store-cluster.md)（2026-10-23 v2，第 1 批已落码未编译）。

全类型节点的平滑停机／缩容、水平扩展、灰度与金丝雀发布需求，及 C++/Go、独立 Java 两版的现状差距与验收口径，见 [节点生命周期与发布需求](design/node-lifecycle-elasticity-release-20261023.md)（2026-10-23，仅文档，尚未完成全类型验收）。

## 索引

<!-- BEGIN GENERATED INDEX: tools/scripts/gen_docs_index.py -->

### 设计文档(`design/`)

#### 总览与入门

- [ARCH.md](design/ARCH.md) — MMO 服务端架构总览 (ARCH.md)
- [architecture-current-state-vs-gaps-2026-05.md](design/architecture-current-state-vs-gaps-2026-05.md) — 架构现状盘点 vs 缺口清单 (2026-05)
- [capacity-model-hok-scale.md](design/capacity-model-hok-scale.md) — 容量模型：王者荣耀量级（S1 / S2 / S3）推算
- [ecs.md](design/ecs.md) — ECS 设计规则 · [EN](design/ecs_en.md) · [中文](design/ecs_zh.md)
- [ecs-component-access-rules.md](design/ecs-component-access-rules.md) — ECS Component Access Rules
- [ecs-entt-misuse-audit-20261015.md](design/ecs-entt-misuse-audit-20261015.md) — entt 误用审计（2026-10-15）
- [onboarding.md](design/onboarding.md) — 开发者上手指南 — MMO Server
- [table-api-reference.md](design/table-api-reference.md) — Configuration Table API Reference · [中文](design/table-api-reference-zh.md)

#### 接入层:网关、gate 与登录

- [auth-provider-framework.md](design/auth-provider-framework.md) — Auth Provider Framework
- [client-access-band-routing.md](design/client-access-band-routing.md) — 客户端接入的标准形态，与 gate 按号段整体转发给路由服（client-rpc-router 续篇，D51–D64）
- [client-request-ordering.md](design/client-request-ordering.md) — 客户端请求按会话保序:路由服水平扩展下的乱序修复(gate 路由 lane)
- [client-rpc-router.md](design/client-rpc-router.md) — 客户端 RPC 路由服(client_rpc_router):gate 唯一的 gRPC 目标
- [dual-token-authentication.md](design/dual-token-authentication.md) — Dual Token Authentication Design (Access + Refresh)
- [gate-connection-admission-control.md](design/gate-connection-admission-control.md) — gate 连接准入控制：muduo 无条件 accept 的业界解法与本仓落地 (2026-09-03)
- [gate-load-balancing-design.md](design/gate-load-balancing-design.md) — Gate 负载均衡、票据与重连边界
- [gate-login-rpc-boundary.md](design/gate-login-rpc-boundary.md) — Gate ↔ Login RPC 边界 (2026-05)
- [gate-scene-relay-architecture.md](design/gate-scene-relay-architecture.md) — Gate–Scene 连接规模与跨 zone 换 Gate
- [gate_client_high_water_mark.md](design/gate_client_high_water_mark.md) — Gate 客户端 TCP 高水位处理（重要） · [EN](design/gate_client_high_water_mark_en.md) · [中文](design/gate_client_high_water_mark_zh.md)
- [hmac-message-signing.md](design/hmac-message-signing.md) — Per-Message HMAC Signing — Design (todo.md #76)
- [java-gateway-portal-decision.md](design/java-gateway-portal-decision.md) — Java Gateway Portal Decision (2026-04-14)
- [k8s-client-entry.md](design/k8s-client-entry.md) — gate / battle 集群外客户端入口(D76–D93)
- [k8s_gate_exposure_guidance.md](design/k8s_gate_exposure_guidance.md) — K8s Gate Exposure Guidance / K8s gate 暴露方式指南 · [EN](design/k8s_gate_exposure_guidance_en.md) · [中文](design/k8s_gate_exposure_guidance_zh.md)
- [login-node-stateless-no-affinity.md](design/login-node-stateless-no-affinity.md) — Login Node: Stateless, No Session Affinity
- [login-queue-2026-05.md](design/login-queue-2026-05.md) — Login Queue (AssignGate Real Queue) — 2026-05
- [open-server-rate-limit-design.md](design/open-server-rate-limit-design.md) — 开服登录削峰与排队入口
- [player-locator-gate-session-index.md](design/player-locator-gate-session-index.md) — player_locator 会话对账:按 gate 实例建会话索引(2026-10-18)
- [player_login_flow.md](design/player_login_flow.md) — 玩家登录/断线/重连/替换登录/超时清理 — 最终态事件流 · [EN](design/player_login_flow_en.md) · [中文](design/player_login_flow_zh.md)
- [serverlist-static-publish-2026-05.md](design/serverlist-static-publish-2026-05.md) — Server-List 静态发布架构 (Static Publish via OSS + CDN) — 2026-05
- [third-party-login-end-to-end-design.md](design/third-party-login-end-to-end-design.md) — QQ / 微信第三方登录端到端联调设计

#### 场景、AOI 与分线

- [afk-detection-design.md](design/afk-detection-design.md) — AFK (挂机) Detection Design
- [agones-scene-node-high-density.md](design/agones-scene-node-high-density.md) — Agones 管理 Scene Node(高密度 GameServer)
- [aoi_priority_design.md](design/aoi_priority_design.md) — AOI Priority, Capacity & Visibility Design · [EN](design/aoi_priority_design_en.md) · [中文](design/aoi_priority_design_zh.md)
- [broadcast-message-size-analysis.md](design/broadcast-message-size-analysis.md) — BroadcastToPlayers 消息体积分析
- [cross_scene_player_messaging.md](design/cross_scene_player_messaging.md) — Cross-Scene Player Messaging (跨场景玩家消息投递) · [EN](design/cross_scene_player_messaging_en.md) · [中文](design/cross_scene_player_messaging_zh.md)
- [enter-scene-zone-routing.md](design/enter-scene-zone-routing.md) — EnterScene 选区路由设计
- [nav-spawn-fix-2026-09-05.md](design/nav-spawn-fix-2026-09-05.md) — 客户端/服务端导航数据、出生点与位置纠正契约
- [scene-creation-architecture.md](design/scene-creation-architecture.md) — Scene Creation Architecture
- [scene-enter-chain-residuals.md](design/scene-enter-chain-residuals.md) — 进场链余项 a–e:踢线、离场、落点、释放与断线上报的身份栅栏
- [scene-enter-session-fence.md](design/scene-enter-session-fence.md) — 入场会话栅栏:登录代次(login_generation)
- [scene-navmesh-pipeline.md](design/scene-navmesh-pipeline.md) — 场景导航网格管线(Recast 烘焙 + 服务器寻路阻挡校验)
- [scene-node-threading-model.md](design/scene-node-threading-model.md) — Scene Node 线程归属、帧循环与停机
- [scene-owner-reentry-barrier.md](design/scene-owner-reentry-barrier.md) — 场景所有权：再入屏障与 owner_epoch
- [scene-switch-release-design.md](design/scene-switch-release-design.md) — SceneManager跨节点切场景释放玩家方案选型
- [world-channel-autoscale.md](design/world-channel-autoscale.md) — 按人数自动扩缩容:大世界频道 / gate / Scene Node 进程
- [world-channel-switch.md](design/world-channel-switch.md) — 玩家主动切线(分线列表 + 选线切换)
- [world-channel-system.md](design/world-channel-system.md) — 主世界场景分线系统 (Main World Scene Lines)

#### 战斗

- [battle-art-prompts.md](design/battle-art-prompts.md) — 战斗美术生图提示词包与资产契约 v1
- [battle-spectate-tiered-fanout.md](design/battle-spectate-tiered-fanout.md) — 观战分级扩容：直连席 → gate 中继 → 全服广播（提案）
- [battle-transport-decision.md](design/battle-transport-decision.md) — battle 节点传输选型定谳(gRPC vs muduo TCP RPC vs Go)
- [global-match-hierarchical-pools.md](design/global-match-hierarchical-pools.md) — 全服匹配：按模板分池、zone→大区→全服逐级扩大范围、每池单写者（2026-10-24 拍板，分批落码）
- [pve-team-size-matching.md](design/pve-team-size-matching.md) — PVE 人数档匹配:人越多怪越多
- [session-extractability-mmo-slg.md](design/session-extractability-mmo-slg.md) — 「战斗抽出去」这套做法能套到哪些玩法:MOBA / MMO / SLG 定谳
- [team-backfill-matching.md](design/team-backfill-matching.md) — 队伍补位匹配(已组好的队伍进人数档队列凑满 N 人)
- [turn-based-battle-server.md](design/turn-based-battle-server.md) — 回合制战斗服设计(turn-based battle server)
- [turn-battle-gap-closure.md](design/turn-battle-gap-closure.md) — 回合制战斗缺口收口 G1–G9(2026-09-17)
- [turn-battle-presentation.md](design/turn-battle-presentation.md) — 回合制战斗表现规格(问道 / 梦幻西游式演出)v1

#### 玩法系统

- [activity_maintenance_auto_shift.md](design/activity_maintenance_auto_shift.md) — 活动遇停服自动顺延设计
- [bag-instance-layout-split.md](design/bag-instance-layout-split.md) — 背包:物品实例层 / 容器布局层拆分 (2026-08-27)
- [bag-rollback-feasibility-analysis.md](design/bag-rollback-feasibility-analysis.md) — Bag 回档可行性分析(per-service rollback)
- [bag-rule-policy-layering.md](design/bag-rule-policy-layering.md) — 背包:玩法规则的策略化分层 (2026-09-01)
- [bag-service-srp-refactor.md](design/bag-service-srp-refactor.md) — Bag → BagService SRP Refactor (2026-03-26) · [EN](design/bag-service-srp-refactor_en.md) · [中文](design/bag-service-srp-refactor_zh.md)
- [character-appearance-identity.md](design/character-appearance-identity.md) — 人物外观身份链
- [chat-sensitive-word-filter.md](design/chat-sensitive-word-filter.md) — Chat Sensitive-Word Filter — Design (todo.md #68)
- [equipment-attributes.md](design/equipment-attributes.md) — 装备属性系统(问道式:基础属性 + 蓝/粉/黄随机属性)
- [equipment-enhance-inlay.md](design/equipment-enhance-inlay.md) — 装备强化与镶嵌(装备属性系统二期)
- [exploit_loss_prevention.md](design/exploit_loss_prevention.md) — Exploit & Loss Prevention Systems (漏洞止损与回收系统) · [EN](design/exploit_loss_prevention_en.md) · [中文](design/exploit_loss_prevention_zh.md)
- [friend-client-spec-20260920.md](design/friend-client-spec-20260920.md) — friend 客户端任务规格(Unity,2026-09-20)
- [friend-persistence-architecture.md](design/friend-persistence-architecture.md) — Friend Service Persistence Architecture
- [friend-port-20260918.md](design/friend-port-20260918.md) — friend:移植为全局服务 + 客户端经路由服可达
- [guild-actor-architecture.md](design/guild-actor-architecture.md) — Guild 存储架构现状与 Actor 历史提案
- [guild-zone-client-access.md](design/guild-zone-client-access.md) — 公会:按 zone 隔离的客户端接入
- [guild_friend_service_notes_zh.md](design/guild_friend_service_notes_zh.md) — 公会与好友服务实现笔记
- [guild_ranking_architecture.md](design/guild_ranking_architecture.md) — Guild Ranking Architecture (2025-03-25) · [EN](design/guild_ranking_architecture_en.md) · [中文](design/guild_ranking_architecture_zh.md)
- [jubaozhai-market.md](design/jubaozhai-market.md) — 聚宝斋(玩家间人民币寄售交易)设计 — 2026-09-14
- [leaderboard-system.md](design/leaderboard-system.md) — 排行榜(leaderboard / rank)v1 设计
- [mail-system.md](design/mail-system.md) — mail(邮件)M1:全局服务 + 系统邮件 / 定向邮件 + 领取短路
- [mystery-merchant.md](design/mystery-merchant.md) — 神秘商人(定时随机落线的折扣 NPC)
- [payment-recharge.md](design/payment-recharge.md) — 真钱充值设计(苹果 IAP / Google Play / 国内安卓 / 小游戏)
- [player-attribute-allocation.md](design/player-attribute-allocation.md) — 角色属性加点系统
- [player-features-ui.md](design/player-features-ui.md) — 背包、任务、活动客户端读取接口
- [player-pet.md](design/player-pet.md) — 宝宝(宠物)系统 —— 核心线
- [stall-system.md](design/stall-system.md) — 摆摊系统设计(草案)
- [team-system.md](design/team-system.md) — 组队系统设计(team-in-match)

#### 玩家数据与存储

- [async-load-disconnect-reconnect-race.md](design/async-load-disconnect-reconnect-race.md) — 异步加载竞态：Redis 加载期间的断线与重连
- [data_service_role_and_scope.md](design/data_service_role_and_scope.md) — data_service 定位与边界
- [db-consumer-group-per-zone.md](design/db-consumer-group-per-zone.md) — go/db 存盘消费组按 zone 拆分与不停服交接
- [db-retry-owner-routing.md](design/db-retry-owner-routing.md) — go/db 重试只交给分区属主（④）与 Kafka 源入队的落盘确认（WAITAOF）
- [db-service-root-credentials.md](design/db-service-root-credentials.md) — go/db 的 MySQL 凭据与建库边界
- [db-task-delivery-and-retention.md](design/db-task-delivery-and-retention.md) — scene DBTask 投递失败后的重存与 db_task 保留期 / Kafka 盘预算（S5）
- [db-task-kafka-partition-contract.md](design/db-task-kafka-partition-contract.md) — DB Task Kafka 分区不可变契约
- [db-task-pipeline-bottlenecks.md](design/db-task-pipeline-bottlenecks.md) — 玩家存盘管线(go/db)的瓶颈、不停服转移与修复计划
- [db-task-store-unavailable-deferral.md](design/db-task-store-unavailable-deferral.md) — go/db 存盘任务：存储不可用时延后、不进死信（L-04）设计评审稿
- [db-write-fencing-mysql.md](design/db-write-fencing-mysql.md) — 玩家存盘 MySQL 条件写（围栏）：go/db、C++、proto、schema 详细设计
- [db_write_behind_dirty_flag_race.md](design/db_write_behind_dirty_flag_race.md) — DB Write-Behind 脏标记方案竞态分析 · [EN](design/db_write_behind_dirty_flag_race_en.md) · [中文](design/db_write_behind_dirty_flag_race_zh.md)
- [db_zone_isolation.md](design/db_zone_isolation.md) — DB Zone Isolation (2026-04-11)
- [global-data-layer-tidb-decision.md](design/global-data-layer-tidb-decision.md) — 全区全服数据层 + TiDB 架构决策
- [landing-recovery-sync-digest.md](design/landing-recovery-sync-digest.md) — 落地恢复：按域版本摘要、场景侧待办唤醒与超窗处置（2026-10-14）
- [new-player-first-enter-no-cold-read.md](design/new-player-first-enter-no-cold-read.md) — 新号首次进场免冷读:建角时写入 PlayerAllData 种子
- [player-async-save-loss-windows.md](design/player-async-save-loss-windows.md) — 玩家异步存盘：已修复问题、剩余丢失窗口与修复任务
- [player-inbox-service.md](design/player-inbox-service.md) — 玩家持久收件箱服务（player_inbox）设计
- [player-route-mapping-durability-proposal.md](design/player-route-mapping-durability-proposal.md) — 玩家归属与落点映射持久化:库为真源 + Redis 完整副本 + 就绪标记(已拍板;第 0 ~ 3 批已落码,未验证;2026-10-19 起读方按身份绑定的就绪标记判可信,见 §13)
- [player-storage-placement.md](design/player-storage-placement.md) — 玩家存储落点(Placement):归属与存储拆分、单玩家搬库、Phase 2 全局库
- [player_live_data_export_runbook.md](design/player_live_data_export_runbook.md) — 玩家线上数据下线导出操作记录
- [proto-compare-dirty-save.md](design/proto-compare-dirty-save.md) — Proto-Compare-Driven Dirty Save — Design (todo.md #204 + #226)
- [redis-async-client-retry-fix.md](design/redis-async-client-retry-fix.md) — Redis 异步客户端重试、重连与生命周期
- [reliable-player-inbox-20261014.md](design/reliable-player-inbox-20261014.md) — 重要玩家消息：持久收件箱、拉取恢复与迁移围栏（2026-10-14）
- [tidb-phase1-and-launch-readiness.md](design/tidb-phase1-and-launch-readiness.md) — TiDB Phase 1 设计与实施清单（含开服准备）

#### 跨服、合服与回档

- [cross-server-currency-auth.md](design/cross-server-currency-auth.md) — Cross-Server Currency Deduction Authorization — Design (todo.md #207)
- [cross-server-rollback-gap-fixes.md](design/cross-server-rollback-gap-fixes.md) — 跨服回档 — 差距修复方案
- [cross-server-rollback-merge-audit.md](design/cross-server-rollback-merge-audit.md) — 跨服 / 回档 / 合服 — 现状盘点与差距分析
- [cross-zone-matchmaking.md](design/cross-zone-matchmaking.md) — 全服跨 Zone 匹配(Cross-Zone Matchmaking)
- [cross-zone-readiness-audit.md](design/cross-zone-readiness-audit.md) — 跨 Zone 就绪审计(Cross-Zone Readiness Audit)
- [cross-zone-scene-travel.md](design/cross-zone-scene-travel.md) — 跨 zone 场景传送(玩家去别的 zone 的地图游玩)
- [cross_server_architecture_principle.md](design/cross_server_architecture_principle.md) — MMO 跨服架构（全局设计文档） · [EN](design/cross_server_architecture_principle_en.md) · [中文](design/cross_server_architecture_principle_zh.md)
- [merge-zone-overhaul-20260908.md](design/merge-zone-overhaul-20260908.md) — 合服（merge_zone）链路整改 —— 全部修复与验证记录（2026-09-08）
- [microservice-zone-contract-20260914.md](design/microservice-zone-contract-20260914.md) — Go 微服务接入 zone 体系的契约 v1(首个落地:chat)
- [mmo_cross_server_architecture.md](design/mmo_cross_server_architecture.md) — MMO Cross-Server Architecture Design (Complete Reference) · [EN](design/mmo_cross_server_architecture_en.md) · [中文](design/mmo_cross_server_architecture_zh.md)
- [server-merge-gap-fixes.md](design/server-merge-gap-fixes.md) — 合服 — 差距修复方案
- [server_merge_design.md](design/server_merge_design.md) — 合服(Server Merge)设计文档
- [single_player_rollback.md](design/single_player_rollback.md) — Single-Player Rollback (单人回档) — MMO/MOBA Microservice Architecture · [EN](design/single_player_rollback_en.md) · [中文](design/single_player_rollback_zh.md)
- [zone_data_rollback.md](design/zone_data_rollback.md) — Zone 数据回档方案 · [EN](design/zone_data_rollback_en.md) · [中文](design/zone_data_rollback_zh.md)
- [zone_data_rollback_discard_tool.md](design/zone_data_rollback_discard_tool.md) — 整 zone 回档清理工具 zone_rollback_discard 设计（R9）

#### ID、节点身份与寻址

- [control-plane-topic-partitioning-20260908.md](design/control-plane-topic-partitioning-20260908.md) — 控制面命令 topic:从"一节点一 topic"改为"固定分区 + 按 node_id 取模寻址"
- [id-routing-session-status-20260909.md](design/id-routing-session-status-20260909.md) — ID / 寻址改造 —— 会话总结与遗留清单(2026-09-09)
- [node-id-overhaul-changelog-20260908.md](design/node-id-overhaul-changelog-20260908.md) — ID 与寻址身份改造 —— 逐文件改动说明(2026-09-08 最终版)
- [node-id-overhaul-plan-20260908.md](design/node-id-overhaul-plan-20260908.md) — Node ID / 永久 ID 改造方案(2026-09-08 草案,未落码)
- [node-id-overhaul-qa-20260908.md](design/node-id-overhaul-qa-20260908.md) — Node ID 改造 —— 问答全记录与改动细节(2026-09-08)
- [node-lifecycle-elasticity-release-20261023.md](design/node-lifecycle-elasticity-release-20261023.md) — 全类型节点：平滑停机、水平扩展与灰度／金丝雀发布需求
- [node-removal-grace-period.md](design/node-removal-grace-period.md) — Node Removal Grace Period (Consumer-side)
- [node_id_conflict_design.md](design/node_id_conflict_design.md) — Node ID 冲突处理设计（重要架构决策） · [EN](design/node_id_conflict_design_en.md) · [中文](design/node_id_conflict_design_zh.md)
- [routing-identity-audit-20260908.md](design/routing-identity-audit-20260908.md) — 路由身份(routing node_id)对抗审计 —— 结论与修复清单(2026-09-08)
- [snowflake-guard-and-node-conflict.md](design/snowflake-guard-and-node-conflict.md) — SnowFlake Guard & Node ID Conflict Handling
- [snowflake-id-allocation.md](design/snowflake-id-allocation.md) — SnowFlake ID Allocation
- [snowflake-node-id-lease-recycling.md](design/snowflake-node-id-lease-recycling.md) — SnowFlake Node ID Lease-Based Recycling

#### 基础设施与中间件

- [distributed-tracing.md](design/distributed-tracing.md) — Distributed Tracing — Design (todo.md #152)
- [double-buffer-queue-optimizations.md](design/double-buffer-queue-optimizations.md) — DoubleBufferQueue Optimizations (2026-04-16)
- [error-reporting.md](design/error-reporting.md) — Error Aggregation & Reporting — Design (todo.md #250)
- [go-zero-rpc-timeout-fix.md](design/go-zero-rpc-timeout-fix.md) — go-zero RPC Client Timeout Fix
- [grpc-muduo-transport.md](design/grpc-muduo-transport.md) — C++ gRPC 传输改由 muduo 驱动(nghttp2 + 自有 gRPC 调用语义)
- [hashed-timing-wheel.md](design/hashed-timing-wheel.md) — Hashed Timing Wheel
- [infra-reconnect-overview.md](design/infra-reconnect-overview.md) — Infrastructure Reconnection Overview
- [kafka-topic-retention-strategy.md](design/kafka-topic-retention-strategy.md) — Kafka topic 保留与耐久策略
- [muduo-rpc-channel.md](design/muduo-rpc-channel.md) — 节点间 muduo RPC:每次调用的回调、延后回包与完整的完成语义(RpcChannel 改造)
- [muduo-timer-cancellation-hazards.md](design/muduo-timer-cancellation-hazards.md) — muduo 定时器取消语义与野引用
- [no-single-node-horizontal-scaling-20261001.md](design/no-single-node-horizontal-scaling-20261001.md) — 消除单节点:所有服务都能水平扩展(2026-10-01)
- [player-handler-deferred-reply.md](design/player-handler-deferred-reply.md) — 玩家级 handler 延后完成(PlayerCallContext)
- [shared-redis-split-by-purpose.md](design/shared-redis-split-by-purpose.md) — 共享 Redis 按用途拆实例(G4a 设计稿)
- [thread-count-monitoring.md](design/thread-count-monitoring.md) — Thread Count Monitoring & Control per Process · [EN](design/thread-count-monitoring_en.md) · [中文](design/thread-count-monitoring_zh.md)
- [traffic-statistics-design.md](design/traffic-statistics-design.md) — Traffic Statistics Design

#### 协议与配表工具链

- [excel-6row-header-format.md](design/excel-6row-header-format.md) — 配置表格式说明入口（旧六行表头已停用） · [EN](design/excel-6row-header-format_en.md) · [中文](design/excel-6row-header-format_zh.md)
- [generated-table-header-decoupling-plan.md](design/generated-table-header-decoupling-plan.md) — 交接：mmorpg 导表器生成头文件解耦（①exprtk ②FK声明化 ③muduo）
- [tip-code-axis.md](design/tip-code-axis.md) — tip 码轴：段是发号器的输入

#### 构建、部署与发布

- [cpp_image_optimization.md](design/cpp_image_optimization.md) — C++ Docker Image Optimization & Split Debug Symbols · [EN](design/cpp_image_optimization_en.md) · [中文](design/cpp_image_optimization_zh.md)
- [docker_k8s_build_deploy.md](design/docker_k8s_build_deploy.md) — Docker & K8s Build/Deploy Guide (Windows) · [EN](design/docker_k8s_build_deploy_en.md) · [中文](design/docker_k8s_build_deploy_zh.md)
- [gateway-k8s-deployment.md](design/gateway-k8s-deployment.md) — Gateway Service Notes
- [release-packaging-standard-20260914.md](design/release-packaging-standard-20260914.md) — 服务端发布打包标准(对标 luyuan-go/xuanming-server)
- [rolling-update-restart-resilience-tests.md](design/rolling-update-restart-resilience-tests.md) — Rolling Update & Node Restart Resilience Tests
- [vs2026_cross_machine_compatibility.md](design/vs2026_cross_machine_compatibility.md) — VS2026 跨电脑兼容性维护记录 · [EN](design/vs2026_cross_machine_compatibility_en.md) · [中文](design/vs2026_cross_machine_compatibility_zh.md)

#### 测试与机器人

- [data-consistency-stress-testing.md](design/data-consistency-stress-testing.md) — Player-Data Consistency: Stress Testing & Verification
- [login-test-anti-stuck-system.md](design/login-test-anti-stuck-system.md) — 登录回归套件与超时检测
- [robot-login-test-scenarios.md](design/robot-login-test-scenarios.md) — Robot Login Test Scenarios

#### 移植记录

- [xuanming-port-decisions-20260910.md](design/xuanming-port-decisions-20260910.md) — 玄冥移植:开工前决策记录(2026-09-10)
- [xuanming-port-feasibility-20260902.md](design/xuanming-port-feasibility-20260902.md) — XuanMing-Server → mmorpg/go 移植可行性定谳（2026-09-02）

#### 未分类

- [account-role-authority.md](design/account-role-authority.md) — 账号角色目录的全局 SQL 权威
- [class-skills.md](design/class-skills.md) — 四职业技能(class skills)服务端设计
- [client-rpc-callback.md](design/client-rpc-callback.md) — 客户端请求自带回调:按请求号认领回包,生成类型化调用桩取代每消息 Handler
- [configurable-templates.md](design/configurable-templates.md) — 战斗策略 / 活动 / 玩法模板可配置化（含玩家自定义模板）
- [data-consistency-guarantee-map-20261020.md](design/data-consistency-guarantee-map-20261020.md) — 数据一致性等级核对（2026-10-20）
- [data-overwrite-rollback-audit-20261018.md](design/data-overwrite-rollback-audit-20261018.md) — 全仓数据问题与覆盖回档审计（2026-10-18）
- [data-overwrite-rollback-audit-r2-20261019.md](design/data-overwrite-rollback-audit-r2-20261019.md) — 全仓数据问题与覆盖回档审计·第二轮（2026-10-19）
- [disconnect-reconnect-audit-20261020.md](design/disconnect-reconnect-audit-20261020.md) — 断线重连恢复审计（2026-10-20）
- [jubaozhai-rmb-consignment-plan.md](design/jubaozhai-rmb-consignment-plan.md) — 聚宝斋人民币寄售：买卖主流程分步计划（D-17 mall 路线）
- [location-release-binding.md](design/location-release-binding.md) — 删 location 绑定"持有者已释放"(审计 §2.1 修复)
- [mall-administration.md](design/mall-administration.md) — 商城后台的受限经济命令
- [mixed-version-persist-field-protection.md](design/mixed-version-persist-field-protection.md) — 新旧版本混跑时的玩家持久字段保护
- [published-contract-compatibility-20261017.md](design/published-contract-compatibility-20261017.md) — 两套已发布客户端契约的合并兼容方案
- [redis-player-store-cluster.md](design/redis-player-store-cluster.md) — 玩家态 Redis 集群化：物理水平扩展设计
- [zone-redis-regression-guard-20261022.md](design/zone-redis-regression-guard-20261022.md) — zone Redis 回退防护：「Redis 回退一族」6 条 low 的合并设计

#### 成套设计

- [guild-phase2/](design/guild-phase2/README.md) — 帮会二期设计(管理审批 / 名字 / 资产通道 / 捐献升级商店 / 活动)

### 运维手册与事故复盘(`ops/`)

#### 事故复盘

- [incident-friend-lock-order-deadlock-2026-09-21.md](ops/incident-friend-lock-order-deadlock-2026-09-21.md) — 事故报告:friend 锁定读被规划成索引全扫描,锁集越出守卫,真死锁 1213
- [incident-gate-tcpconnection-dtor-assert-2026-09-13.md](ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md) — 事故报告:gate 在 `~TcpConnection` 断言崩溃(thread_local 持有连接强引用)
- [incident-local-enter-game-3023-2026-10-22.md](ops/incident-local-enter-game-3023-2026-10-22.md) — 本地测试环境进场失败严重事故记录：3023 与存档协议、数据世代未闭环
- [incident-location-delete-rollback-2026-10-18.md](ops/incident-location-delete-rollback-2026-10-18.md) — 事故报告:删 location 不等持有者落盘,玩家回档到上一次成功存盘
- [incident-scene-ecs-gameplay-20261014.md](ops/incident-scene-ecs-gameplay-20261014.md) — 严重事故记录：任务、实时技能、Buff 与节点生命周期缺陷（2026-10-14）

#### 运维手册

- [audit-topic-generation-bump-runbook.md](ops/audit-topic-generation-bump-runbook.md) — 审计 topic 离线换代 Runbook(player_snapshot / transaction_log 改分区数)
- [cross-zone-failure-test-runbook.md](ops/cross-zone-failure-test-runbook.md) — 跨 Zone 传送 / 归属交接 失败场景测试 Runbook
- [etcd-maintenance-runbook.md](ops/etcd-maintenance-runbook.md) — etcd MVCC 压缩与空间维护 Runbook
- [gate-kernel-tuning-runbook.md](ops/gate-kernel-tuning-runbook.md) — Gate 内核调优 Runbook
- [grafana-loki-local-logs.md](ops/grafana-loki-local-logs.md) — 本地日志观测台:Grafana + Loki + Alloy(C++ / Go / Java 三语言统一接入)
- [k8s-docker-desktop-troubleshooting.md](ops/k8s-docker-desktop-troubleshooting.md) — K8s Docker Desktop Troubleshooting Guide · [EN](ops/k8s-docker-desktop-troubleshooting_en.md) · [中文](ops/k8s-docker-desktop-troubleshooting_zh.md)
- [k8s-open-server-runbook.md](ops/k8s-open-server-runbook.md) — K8s 开服操作手册 · [EN](ops/k8s-open-server-runbook_en.md) · [中文](ops/k8s-open-server-runbook_zh.md)
- [kafka-cluster-production-runbook.md](ops/kafka-cluster-production-runbook.md) — Kafka 集群化生产部署 Runbook
- [linux-staging-stress-runbook.md](ops/linux-staging-stress-runbook.md) — Linux Staging 部署 + 阶梯压测留位
- [log-management.md](ops/log-management.md) — Log Management
- [login-queue-stress-runbook.md](ops/login-queue-stress-runbook.md) — 登录排队压测 — 判读清单
- [merge-zone-runbook.md](ops/merge-zone-runbook.md) — Merge-Zone Runbook(合服操作手册)
- [mysql-backup-pitr-runbook.md](ops/mysql-backup-pitr-runbook.md) — MySQL Backup & PITR Runbook
- [online-debug-data-fetch.md](ops/online-debug-data-fetch.md) — 线上玩家数据拉取与本地复现
- [player-inbox-runbook.md](ops/player-inbox-runbook.md) — 玩家持久收件箱(player_inbox)运维手册
- [release-checklist.md](ops/release-checklist.md) — 服务端发布清单
- [run-directory.md](ops/run-directory.md) — `run/` — Runtime Artifacts
- [stress-3zone-runbook.md](ops/stress-3zone-runbook.md) — 3 Zone × 15000 压测 — Runbook(2026-05)
- [trade-commerce-quarantine-runbook.md](ops/trade-commerce-quarantine-runbook.md) — 交易合同隔离与角色冲突处置手册
- [wechat-qq-sandbox-runbook.md](ops/wechat-qq-sandbox-runbook.md) — WeChat / QQ Sandbox 接入清单 (todo #223 / U)

#### 其他

- [deferred-clawback-bypass-audit-2026-05.md](ops/deferred-clawback-bypass-audit-2026-05.md) — 补缴系统(Deferred Clawback)旁路审计报告
- [godb-pipeline-launch-todo.md](ops/godb-pipeline-launch-todo.md) — 存盘管线（go/db）上线待做
- [pr-templates.md](ops/pr-templates.md) — PR Templates — HTTP /api/login Migration
- [scene-node-role-split.md](ops/scene-node-role-split.md) — Scene Node Role Split Runbook

### 压测复盘(`stress/`)

- [stress-1zone-25k-2026-05-28-callback-wait.md](stress/stress-1zone-25k-2026-05-28-callback-wait.md) — 25k 单 zone 压测:dispatcher GC tick → callback_wait 全链路修复
- [stress-1zone-25k-2026-05-28-deep-dive.md](stress/stress-1zone-25k-2026-05-28-deep-dive.md) — 深挖:46% 后台 preload_failed 完整解剖
- [stress-1zone-25k-2026-05-28-maxopenconn.md](stress/stress-1zone-25k-2026-05-28-maxopenconn.md) — MaxOpenConn 10→30 实测:解了一半,db worker 串行才是真上限
- [stress-1zone-25k-2026-05-28-subshard.md](stress/stress-1zone-25k-2026-05-28-subshard.md) — Worker sub-shard (方案 A):db consumer 单 worker 串行 → 4 路并行
- [stress-1zone-25k-2026-05-29-dispatcher-async-lpop.md](stress/stress-1zone-25k-2026-05-29-dispatcher-async-lpop.md) — Dispatcher subscriber 串行 LPop → 异步:解 95% callback_wait{failed}
- [stress-1zone-45k-2026-05-30-gate-kafka-consumer.md](stress/stress-1zone-45k-2026-05-30-gate-kafka-consumer.md) — 45k Round: gate-group-1 consumer 失活 → 主瓶颈
- [stress-1zone-45k-2026-05-31-db-kafka-partition-starvation.md](stress/stress-1zone-45k-2026-05-31-db-kafka-partition-starvation.md) — Symptoms (Round 10)
- [stress-1zone-45k-2026-05-partition-10.md](stress/stress-1zone-45k-2026-05-partition-10.md) — 单 zone 45k 压测:db_task partition 5→10 实测复盘
- [stress-1zone-45k-2026-06-01-cpp-cold-start-discovery-race.md](stress/stress-1zone-45k-2026-06-01-cpp-cold-start-discovery-race.md) — Stress 1-zone 45k — 2026-06-01 — C++ Cold-Start Discovery Race FIX VERIFIED
- [stress-1zone-45k-2026-06-01-round13-preload-failure.md](stress/stress-1zone-45k-2026-06-01-round13-preload-failure.md) — Stress Round 13 — 45k z1 — Preload TTL Bottleneck Postmortem
- [stress-1zone-45k-2026-06-01-round14.md](stress/stress-1zone-45k-2026-06-01-round14.md) — Round 14 — 1 Zone × 45k Robot Stress (2026-06-01)
- [stress-1zone-45k-2026-06-01-round15.md](stress/stress-1zone-45k-2026-06-01-round15.md) — Stress Test — 1 Zone × 45k Robots — Round 15 (2026-06-01)
- [stress-1zone-45k-2026-06-03-round17.md](stress/stress-1zone-45k-2026-06-03-round17.md) — Stress Test — 1 Zone × 45k Robots — Round 17 (2026-06-03)
- [stress-1zone-45k-2026-06-04-round18.md](stress/stress-1zone-45k-2026-06-04-round18.md) — Stress Test — 1 Zone × 45k Robots — Round 18 (2026-06-04)
- [stress-3zone-2026-05-23-postmortem.md](stress/stress-3zone-2026-05-23-postmortem.md) — 3 Zone × 15000 压测 — 复盘(2026-05-23)
- [stress-test-2026-05-ephemeral-port.md](stress/stress-test-2026-05-ephemeral-port.md) — 压测复盘: ephemeral port 扩容前后对比 (2026-05)
- [stress-test-2026-05-http-login.md](stress/stress-test-2026-05-http-login.md) — 端到端阶梯压测: HTTP /api/login 路径 (2026-05-09)
- [stress-test-progress.md](stress/stress-test-progress.md) — 1000-Robot Stress Test Progress

### 交接说明(`handoff/`)

- [class-skills-handoff-20261019.md](handoff/class-skills-handoff-20261019.md) — 四职业技能接入:交 Codex 的生成 / 编译 / 测试清单(2026-10-19)
- [client-rpc-callback-handoff-20261019.md](handoff/client-rpc-callback-handoff-20261019.md) — 交接:客户端请求自带回调(按请求号认领回包)(2026-10-19)
- [client-rpc-callback-robot-handoff-20261019.md](handoff/client-rpc-callback-robot-handoff-20261019.md) — 交接:robot 请求自带回调(按 MessageContent.id 认领回包)(2026-10-19)
- [configurable-templates-handoff-20261019.md](handoff/configurable-templates-handoff-20261019.md) — 交接：战斗策略 / 活动 / 玩法模板可配置化（含玩家自定义模板）——从设计到落码（2026-10-19）
- [d1-review-fix-handoff-20261023.md](handoff/d1-review-fix-handoff-20261023.md) — 交接:D1 最终语义评审与修复(10-19 ~ 10-23)——移交「映射直读 TiDB」新方向
- [friend-handoff-20260920.md](handoff/friend-handoff-20260920.md) — friend 服务移植 —— 交接文档(2026-09-20)
- [global-economy-claude-review-20261022.md](handoff/global-economy-claude-review-20261022.md) — 全球同服、交易与角色归属：Claude Code 审核入口
- [global-economy-review-20261021.md](handoff/global-economy-review-20261021.md) — 全球交易、角色归属与双版交付审核交接
- [global-final-acceptance-resume-20261027.md](handoff/global-final-acceptance-resume-20261027.md) — 全球同服剩余验收：全新主窗口交接（545范围、实际失败及续跑入口）
- [global-id-prelaunch-claude-20261028.md](handoff/global-id-prelaunch-claude-20261028.md) — 交接：全球同服 ID 发号上线前决策与修复（交 Claude Code，2026-10-28）
- [global-singletons-p0-codex-tasksheet-20261014.md](handoff/global-singletons-p0-codex-tasksheet-20261014.md) — Codex 任务单:全局单点 P0 五项的实测核实(2026-10-14)
- [global-singletons-p0-findings-20261014.md](handoff/global-singletons-p0-findings-20261014.md) — 全局单点 P0 五项:静态调查记录与修复设计(2026-10-14)
- [global-singletons-p0-measure-steps-20261014.md](handoff/global-singletons-p0-measure-steps-20261014.md) — 全局单点 P0 五项:实测步骤原文(2026-10-14)
- [godb-pipeline-codex-tasksheet-20261013.md](handoff/godb-pipeline-codex-tasksheet-20261013.md) — Codex 任务单：存盘管线第 0 批与 proto2mysql 条件写原语的验证（2026-10-13）
- [godb-pipeline-handoff-20261013.md](handoff/godb-pipeline-handoff-20261013.md) — 交接：玩家存盘管线（go/db）瓶颈修复与条件写（2026-10-13）
- [godb-pipeline-handoff-20261013-appendix.md](handoff/godb-pipeline-handoff-20261013-appendix.md) — 附录：存盘管线修复方案明细（工作流原始产出）
- [godb-pipeline-impl-handoff-20261015.md](handoff/godb-pipeline-impl-handoff-20261015.md) — 交接：存盘管线剩余代码落码（2026-10-15）
- [handoff-backlog-2026-09-05.md](handoff/handoff-backlog-2026-09-05.md) — 交接清单:2026-09-02 ~ 09-05 三天工作的留档待办(经代码核实)
- [handoff-client-request-ordering-20261015.md](handoff/handoff-client-request-ordering-20261015.md) — 交接:客户端请求按会话保序(路由服水平扩展下的乱序修复,2026-10-15)
- [handoff-crosszone-20260920.md](handoff/handoff-crosszone-20260920.md) — 跨 zone 场景传送:交接说明(2026-09-20)
- [handoff-d1-remaining-20261019.md](handoff/handoff-d1-remaining-20261019.md) — 交接:D1 归属与落点映射持久化——剩余收尾(2026-10-19)
- [handoff-data-service-field-lua-real-redis-20261019.md](handoff/handoff-data-service-field-lua-real-redis-20261019.md) — 交接:data_service 字段索引三段 Lua 的真 Redis 集成测试(2026-10-19)
- [handoff-gate-dtor-fix-verify-and-stress-20260914.md](handoff/handoff-gate-dtor-fix-verify-and-stress-20260914.md) — 交接:gate `~TcpConnection` 事故第二批修复 —— 构建 / 验证 / 压测(执行方:ChatGPT;方案方:Claude Code)
- [handoff-global-singletons-10m-ccu-20261013.md](handoff/handoff-global-singletons-10m-ccu-20261013.md) — 交接:全局单点盘点、解法与 1000 万在线容量核算(2026-10-13)
- [handoff-global-singletons-p0-20261015.md](handoff/handoff-global-singletons-p0-20261015.md) — 交接:全局单点 P0 五项的落码现状与剩余工作(2026-10-15)
- [handoff-global-singletons-p0-20261015-appendix.md](handoff/handoff-global-singletons-p0-20261015-appendix.md) — 附录:全局单点 P0 部署侧四批的实现报告(2026-10-15)
- [handoff-landing-recovery-and-fencing-20261016.md](handoff/handoff-landing-recovery-and-fencing-20261016.md) — 交接：落地恢复、超窗处置、进场链余项与 MySQL 行内围栏（2026-10-16）
- [handoff-launch-line-remaining-20261019.md](handoff/handoff-launch-line-remaining-20261019.md) — 交接:开服线剩余工作(2026-10-19)
- [handoff-launch-readiness-and-d1-20261013.md](handoff/handoff-launch-readiness-and-d1-20261013.md) — 交接:开服首日瓶颈修复、D1 映射持久化与 S3 容量目标(2026-10-13)
- [handoff-launch-readiness-decisions-20261015.md](handoff/handoff-launch-readiness-decisions-20261015.md) — 交接:开服瓶颈负责人四项决定的收尾与验证(2026-10-15)
- [handoff-orcontinue-closure-20260914.md](handoff/handoff-orcontinue-closure-20260914.md) — 交接：`Lookup*OrContinue` 修复收尾 + exporter-tests CI 复活
- [handoff-tip-axis-and-port-20260903.md](handoff/handoff-tip-axis-and-port-20260903.md) — 交接清单：tip 码轴收尾 + 玄冥移植未开工项
- [handoff-tip-fault-gateway-500-20261018.md](handoff/handoff-tip-fault-gateway-500-20261018.md) — 交接：Java 网关按 fault 列把登录故障回 500
- [mixed-version-persist-handoff-20261019.md](handoff/mixed-version-persist-handoff-20261019.md) — 混版本持久字段保护：交接（2026-10-19）
- [placement-line-handoff-20261019.md](handoff/placement-line-handoff-20261019.md) — 交接:玩家存储落点这条线——完成度核查与剩余工作(落点 + D1 映射持久化,2026-10-19)
- [placement-reverify-checklist-20261012.md](handoff/placement-reverify-checklist-20261012.md) — 玩家存储落点:当前 HEAD 复验清单(交 Codex)
- [player-inbox-handoff-20261015.md](handoff/player-inbox-handoff-20261015.md) — 玩家持久收件箱：实现交接（2026-10-15）
- [redis-ha-deploy-path-handoff-20261019.md](handoff/redis-ha-deploy-path-handoff-20261019.md) — 交接：共享 Redis 高可用形态接进部署路径（2026-10-19）
- [redis-horizontal-scaling-requirements-20261022.md](handoff/redis-horizontal-scaling-requirements-20261022.md) — 交接：Redis 物理水平扩展需求与当前存盘链问题（2026-10-22）
- [redis-player-store-batch2-brief-20261023.md](handoff/redis-player-store-batch2-brief-20261023.md) — 交接：玩家态 Redis 集群化第 2 批（2a–2d）落码简报（2026-10-23）
- [repo-layout-20261007.md](handoff/repo-layout-20261007.md) — 仓库目录整理:交接说明(2026-10-07)
- [rpc-channel-batch7-handoff-20261015.md](handoff/rpc-channel-batch7-handoff-20261015.md) — 交接:RpcChannel 改造收尾——第 7 批、剩余调用点迁移与玩家级延后完成(2026-10-15)
- [session-summary-20260908-09.md](handoff/session-summary-20260908-09.md) — 本轮会话总览与遗留清单（2026-09-08 ~ 09-09）
- [zone-home-and-physical-storage-conversation-20260924.md](handoff/zone-home-and-physical-storage-conversation-20260924.md) — 逻辑归属、物理存储与合服：完整对话记录（mmorpg）
- [zone-rollback-r9-handoff-20261019.md](handoff/zone-rollback-r9-handoff-20261019.md) — 交接：整 zone 回档清理工具（R9）与两个小项的落码（2026-10-19）

### 笔记(`notes/`)

- [2026-09-14-network-failures-and-process-pauses.md](notes/2026-09-14-network-failures-and-process-pauses.md) — 游戏服务器网络故障、网络分区与进程暂停：一手来源调研
- [2026-09-14-network-failures-and-process-pauses-verification.md](notes/2026-09-14-network-failures-and-process-pauses-verification.md) — 网络故障与进程暂停调研：独立证据复核
- [code-rigor-complete-qa-2026-09-23.md](notes/code-rigor-complete-qa-2026-09-23.md) — 如何提升写代码的严谨性——完整问答记录
- [cpp-text-template-libraries-20261021.md](notes/cpp-text-template-libraries-20261021.md) — C++ 文本模板库选型建议（2026-10-21）
- [currency-crash-window-verification.md](notes/currency-crash-window-verification.md) — CurrencyComp 崩溃窗口验证方案
- [event-priority.md](notes/event-priority.md) — Events · [EN](notes/event-priority_en.md) · [中文](notes/event-priority_zh.md)
- [gameplay-introduction-and-resources-research-2026-09-01.md](notes/gameplay-introduction-and-resources-research-2026-09-01.md) — Gameplay 是什么：入门文章与开源项目
- [github-survivors-like-games-research-2026-08-27.md](notes/github-survivors-like-games-research-2026-08-27.md) — GitHub 幸存者割草类游戏源码调研
- [mmorpg-stress-interview-qa.md](notes/mmorpg-stress-interview-qa.md) — MMORPG 项目压测容量与瓶颈优化面试回答
- [NOTES_zh.md](notes/NOTES_zh.md) — 项目笔记 / Project Notes(中文) · [EN](notes/NOTES_en.md)
- [player-data-loading-and-sharding-pain.md](notes/player-data-loading-and-sharding-pain.md) — 玩家数据加载与分库分表的痛点分析
- [redis-oom-data-safety-qa-record.md](notes/redis-oom-data-safety-qa-record.md) — Redis OOM / 数据安全 / 千万在线 — Q&A 决策记录
- [server-crash-memory-debugging-qa.md](notes/server-crash-memory-debugging-qa.md) — 游戏服务器线上崩溃和内存问题排查问答
- [todo_zh.md](notes/todo_zh.md) — 工程待办 / 设计备忘 · [EN](notes/todo_en.md)

### 笔记(`notes/moba/`)

- [moba-battle-target-architecture.md](notes/moba/moba-battle-target-architecture.md) — 会话制对局(MOBA 形态)目标架构基准
- [moba-ds-server-interview-qa.md](notes/moba/moba-ds-server-interview-qa.md) — MOBA 服务器面试 Q&A — DS（Dedicated Server / 战斗服）
- [moba-ds-server-interview-qa-advanced.md](notes/moba/moba-ds-server-interview-qa-advanced.md) — MOBA DS 服务器面试 Q&A — 进阶篇
- [moba-non-ds-server-interview-qa.md](notes/moba/moba-non-ds-server-interview-qa.md) — MOBA 服务器面试 Q&A — 非 DS（大厅 / 匹配 / 房间 / 战绩 / 段位 / 社交）

### 笔记(`notes/slg/`)

- [slg-battle-interview-qa.md](notes/slg/slg-battle-interview-qa.md) — SLG 沙盘战斗系统面试 Q&A（率土之滨风格）
- [slg-interview-qa.md](notes/slg/slg-interview-qa.md) — 率土之滨 SLG 游戏服务器主程序面试 Q&A
- [slg-march-position-formula.md](notes/slg/slg-march-position-formula.md) — SLG 行军位置公式
- [slg-march-system-complete.md](notes/slg/slg-march-system-complete.md) — SLG 行军系统完整设计（率土之滨风格）
- [slg-pathfinding-interview-qa.md](notes/slg/slg-pathfinding-interview-qa.md) — SLG 沙盘寻路面试 Q&A（率土之滨风格）
- [slg-performance-interview-qa.md](notes/slg/slg-performance-interview-qa.md) — SLG 沙盘性能优化面试 Q&A（率土之滨风格）
- [slg-server-architecture-design.md](notes/slg/slg-server-architecture-design.md) — SLG Server Architecture Design (率土之滨 Style)
- [slg-server-complete-framework.md](notes/slg/slg-server-complete-framework.md) — 率土之滨 SLG 游戏服务器 —— 完整框架设计
- [slg-server-framework-complete.md](notes/slg/slg-server-framework-complete.md) — 率土之滨风格 SLG 游戏服务器框架 — 完整设计文档
- [slg-server-interview-complete-qa.md](notes/slg/slg-server-interview-complete-qa.md) — 率土之滨 SLG 服务器主程序面试 —— 全面 Q&A
- [slg-server-mainprog-interview-qa.md](notes/slg/slg-server-mainprog-interview-qa.md) — SLG 服务器主程序面试 Q&A（率土之滨风格）

### 历史归档(`archive/`)

- [centre_decommission_ha_checklist.md](archive/centre_decommission_ha_checklist.md) — Centre Decommission + HA Checklist (2026-03-15) · [EN](archive/centre_decommission_ha_checklist_en.md) · [中文](archive/centre_decommission_ha_checklist_zh.md)
- [centre_decommission_migration_plan.md](archive/centre_decommission_migration_plan.md) — Centre 去中心化迁移方案 (2026-03-15)
- [cross-zone-merge-rollback-audit-2026-05.md](archive/cross-zone-merge-rollback-audit-2026-05.md) — 跨服 / 合服 / 回档 — 真实实现状态审计

<!-- END GENERATED INDEX -->
