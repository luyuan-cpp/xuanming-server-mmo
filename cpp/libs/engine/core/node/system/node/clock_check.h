#pragma once

#include <cstdint>
#include <cstring>

// 时钟启动门槛(C++ 节点):和 Kubernetes API server 的时钟差得太多就拒启。Go 服务(go/shared/clockcheck)和
// Java 服务(java/server_startup 的 ClockCheck)是同一套规则、同一个开关;为什么要这一道、运维怎么处置见 docs/ops/clock-sync-runbook.md。
// 这里只放纯判定,发请求、读 Date 头在 node.cpp 的 EnsureClockOrDie。
namespace clock_check
{
	// 允许的最大偏差。Date 头只到秒,加上往返,测量误差在两秒以内;30 秒只拦明显错的时钟,更细的偏差靠 NTP 和节点告警。
	inline constexpr int64_t kMaxOffsetMillis = 30 * 1000;

	// 设成 warn 时超限只告警、照常启动(API server 自己的时钟错了时的应急开关);其余取值(包括不设)都按拒启。
	inline constexpr const char *kModeEnv = "MMORPG_CLOCK_CHECK_MODE";

	enum class Verdict
	{
		kOk,
		kWarnOnly,
		kRefuse,
	};

	// 本机时钟减参照时钟(毫秒)。参照是回应的 Date 头,只到秒:本机取请求前后的中点,参照取那一秒的中点。
	inline int64_t OffsetMillis(int64_t sentMillis, int64_t receivedMillis, int64_t dateSeconds)
	{
		return sentMillis + (receivedMillis - sentMillis) / 2 - (dateSeconds * 1000 + 500);
	}

	// mode 是 kModeEnv 的取值,可以是空指针。
	inline Verdict Judge(int64_t offsetMillis, const char *mode)
	{
		if (offsetMillis >= -kMaxOffsetMillis && offsetMillis <= kMaxOffsetMillis)
		{
			return Verdict::kOk;
		}
		return mode != nullptr && std::strcmp(mode, "warn") == 0 ? Verdict::kWarnOnly : Verdict::kRefuse;
	}
}
