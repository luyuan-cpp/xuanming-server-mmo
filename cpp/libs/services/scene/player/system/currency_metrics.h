#pragma once

// 把货币模块的进程级计数挂到 Prometheus 出口(engine/core/metrics),写法同 player_ownership_metrics.h。
//
// 导出的指标(没有标签,玩家号不进指标):
//   scene_currency_clawback_version_skew_total  counter,补缴扣还因欠款带更新版本字段而跳过的次数
//                                               (modules/currency/system/currency_clawback_stats.h;逐次另有一条
//                                               metric=persist_version_skew where=currency_clawback 的 WARN,玩家号只在那一行)
// 计数本身不变(relaxed 原子),这里只是读者。采集回调只读原子,不碰 ECS;启动即以 0 导出,告警的 increase 抓得到第一次
// (deploy/k8s/persist-version-skew-alerts.yaml)。
//
// scene 启动时调一次;重复调用不会重复登记(注册表按名字拒绝),只打一行 ERROR。
void RegisterCurrencyMetrics();
