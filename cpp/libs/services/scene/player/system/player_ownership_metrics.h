#pragma once

// 把归属交接 / owner_epoch 的进程级计数挂到 Prometheus 出口(engine/core/metrics),
// 补上 runbook(docs/ops/cross-zone-failure-test-runbook.md §6)的观测缺口 G1 / G2 / G3。
//
// 导出的指标(全部只有低基数标签,玩家号不进指标):
//   scene_travel_handoff_events_total{event}   counter,event 取 travel_handoff_stats::kFields 里的事件 key
//   scene_travel_handoff_frozen_ms_total       counter,走到终态的交接累计冻结毫秒
//   scene_travel_handoff_frozen_ms_max         gauge,  单次交接最长冻结毫秒(进程启动以来)
//   scene_travel_handoff_frozen_players        gauge,  此刻挂着 PlayerFrozenComp 的玩家数(G2)
//   scene_travel_handoff_oldest_frozen_ms      gauge,  此刻冻结最久的那名玩家已冻结的毫秒,没有冻结为 0(G2)
//   scene_owner_epoch_events_total{event}      counter,event 取 owner_epoch_stats::kFields 的 key
// 计数本身不变(relaxed 原子 + 30s 汇总日志),这里只是第二个读者,字段名与汇总行同一份字段表。
//
// 采集回调在 Node 的 EventLoop(即 scene 逻辑线程)上执行,读 tlsEcs 的冻结组件是安全的。
// scene 启动时调用一次;重复调用不会重复登记(注册表按名字拒绝),只打一行 ERROR。
void RegisterPlayerOwnershipMetrics();
