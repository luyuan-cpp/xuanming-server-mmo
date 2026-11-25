#pragma once

#include <cassert>
#include <optional>
#include <utility>

#include "modules/id_segment/guid_segment_registry.h"

namespace id_segment_detail
{
// 所有种类共用一个在途槽。客户端的兜底超时已经判失败，但通道仍给原 RPC 留一个认领窗口。
class RequestTable final
{
public:
	static constexpr double kLateWindowSeconds = 10.0;
	struct Request
	{
		uint64_t sequence;
		GuidKind kind;
		double sentAt;
	};
	enum class Admission : uint8_t { kReady, kBusy, kOpenedLateWindow, kExpiredLateWindow };
	struct Check
	{
		Admission admission;
		std::optional<Request> affected;
	};
	struct Completion
	{
		GuidKind kind;
		bool late;
	};

	std::optional<GuidKind> InFlightKind() const
	{
		return inflight ? std::optional{inflight->kind} : std::nullopt;
	}

	Check BeforeSend(double now, double timeout)
	{
		if (inflight)
		{
			if (now - inflight->sentAt < timeout) return {Admission::kBusy, std::nullopt};
			abandoned = std::exchange(inflight, std::nullopt);
			abandonedUntil = now + kLateWindowSeconds;
			return {Admission::kOpenedLateWindow, abandoned};
		}
		if (abandoned)
		{
			if (now < abandonedUntil) return {Admission::kBusy, std::nullopt};
			return {Admission::kExpiredLateWindow, std::exchange(abandoned, std::nullopt)};
		}
		return {Admission::kReady, std::nullopt};
	}

	uint64_t Reserve(GuidKind kind, double now)
	{
		assert(!inflight && !abandoned);
		const auto sequence = ++nextSequence;
		inflight = Request{sequence, kind, now};
		return sequence;
	}

	std::optional<Completion> Take(uint64_t sequence)
	{
		if (inflight && inflight->sequence == sequence)
			return Completion{std::exchange(inflight, std::nullopt)->kind, false};
		if (abandoned && abandoned->sequence == sequence)
			return Completion{std::exchange(abandoned, std::nullopt)->kind, true};
		return std::nullopt;
	}

	void Clear()
	{
		inflight.reset();
		abandoned.reset();
	}

private:
	std::optional<Request> inflight;
	std::optional<Request> abandoned;
	double abandonedUntil = 0;
	uint64_t nextSequence = 0;
};
}
