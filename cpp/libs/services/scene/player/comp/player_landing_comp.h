#pragma once

#include <cstdint>

// PlayerLandingWakeComp —— 落地唤醒(SPUBLISH assetop:wake:{aw:<k>})的合并窗口记录。
// 设计:docs/design/landing-recovery-sync-digest.md §5.2;分片频道见 redis-cluster-all-domains-20261110.md §4.6。
//
// 只由 PlayerLandingSystem::PublishAssetWake 读写:同一实体在 player_landing::kAssetWakeCoalesceMs(5 秒)内
// 只发一次唤醒,避免同 zone 频繁换图时刷屏。命令真正被控制集群句柄收下之后才写入;没发出去不记,下次进场照发。
//
// 纯运行时状态:不入 PlayerAllData、不序列化、不跨进程,所以是普通 C++ struct 而不是 proto。
// 随实体销毁一起消失;新节点 / 新实体进场时没有它,第一次一定会发 —— 合并窗口只为限频,不承担正确性
// (唤醒只是提示,丢了退回资产通道原有的退避重投,见设计 L-4)。
struct PlayerLandingWakeComp
{
	// 上一次发出唤醒的时刻:单调时钟(std::chrono::steady_clock)毫秒,只能与同一进程内的单调时钟读数比较
	// (AGENTS §11.3:进程内的时间窗口不能因系统时间回拨而失效)。
	uint64_t lastWakeMs{0};
};
