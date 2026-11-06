#pragma once

#include <algorithm>

// 服务发现的兜底重拉节奏(docs/design/control-plane-zone-scoping-20261105.md §3.1)。纯函数,随机数与时间由调用方给。
//
// 正常态靠 watch:Range 之后从快照的下一个 revision 建 watch,事件不漏;断流(OnWatchStreamRestarted)与压缩
// (compact_revision)时立即逐前缀重拉。周期性的全量 Range 只是纵深防御,原来每 2 秒一次,在全服 N_cpp 个进程上
// 是 etcd 读量的平方项(容量模型 §2.2),现在拉长到约 60 秒,并加抖动避免所有进程同一时刻打 etcd。
namespace discovery_resync
{
	// 兜底全量 Range 的基准周期(秒)。
	inline constexpr double kResyncIntervalSeconds = 60.0;
	// 每一拍在基准上乘 [1 - r, 1 + r) 的随机系数。
	inline constexpr double kResyncJitterRatio = 0.2;

	// unit01 是 [0, 1) 上的均匀随机数;越界的值钳回区间(调用方随机源出错时不至于给出 0 或负延迟)。
	constexpr double ResyncDelaySeconds(double unit01)
	{
		const double u = std::clamp(unit01, 0.0, 1.0);
		return kResyncIntervalSeconds * (1.0 - kResyncJitterRatio + 2.0 * kResyncJitterRatio * u);
	}

	// watch 因压缩以外的原因被取消后,按前缀退避重拉:1 秒起,翻倍,封顶 30 秒;重新建成 watch 后清零。
	// 原来等下一次 2 秒全量 Range 重建,周期拉长之后不能再等它。
	inline constexpr double kCancelRetryMinSeconds = 1.0;
	inline constexpr double kCancelRetryMaxSeconds = 30.0;

	// current 是上一次用过的退避;≤ 0 表示还没退避过。
	constexpr double NextCancelRetrySeconds(double current)
	{
		if (current < kCancelRetryMinSeconds)
		{
			return kCancelRetryMinSeconds;
		}
		return std::min(current * 2.0, kCancelRetryMaxSeconds);
	}
} // namespace discovery_resync
