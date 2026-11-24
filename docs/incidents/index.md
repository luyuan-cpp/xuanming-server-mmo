# 崩溃与 P0 事故档案索引

> 移植自 `xuanming-server/docs/incidents/` 治理体系（2026-11-23）。
> 凡确认 runtime fatal、未恢复 panic、C++ 崩溃/assert、OOM/CrashLoop、数据损坏/回档、安全突破、脑裂或大面积不可用的 P0 事故，须当次按 [template.md](./template.md) 建档并登记到本索引；在关闭清单全部满足前不得标记为“已关闭”。

## 活跃与历史事故目录

| 编号 | 级别 | 日期 | 状态 | 简述 | 档案 |
|---|---|---|---|---|---|
| INC-20260913-001 | P0 | 2026-09-13 | 已关闭（历史归档） | Gate `TcpConnection.cc:71` 析构 assert 与 UAF，催生 §11.7 连接/回调生命周期规范 | [incident-gate-tcpconnection-dtor-assert-2026-09-13.md](../ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md) |
| INC-20260921-001 | P0 | 2026-09-21 | 已关闭（历史归档） | 好友并发锁序死锁（1213） | [incident-friend-lock-order-deadlock-2026-09-21.md](../ops/incident-friend-lock-order-deadlock-2026-09-21.md) |
| INC-20261014-001 | P0 | 2026-10-14 | 已关闭（历史归档） | Scene ECS 玩法组件生命周期事故 | [incident-scene-ecs-gameplay-20261014.md](../ops/incident-scene-ecs-gameplay-20261014.md) |
| INC-20261018-001 | P0 | 2026-10-18 | 已关闭（历史归档） | Location 删除与回档竞态 | [incident-location-delete-rollback-2026-10-18.md](../ops/incident-location-delete-rollback-2026-10-18.md) |
| INC-20261022-001 | P0 | 2026-10-22 | 已关闭（历史归档） | 本地进场 3023 错误事故 | [incident-local-enter-game-3023-2026-10-22.md](../ops/incident-local-enter-game-3023-2026-10-22.md) |
