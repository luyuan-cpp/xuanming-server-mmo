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
		return inflight_ ? std::optional{inflight_->kind} : std::nullopt;
	}

	Check BeforeSend(double now, double timeout)
	{
		if (inflight_)
		{
			if (now - inflight_->sentAt < timeout) return {Admission::kBusy, std::nullopt};
			abandoned_ = std::exchange(inflight_, std::nullopt);
			abandonedUntil_ = now + kLateWindowSeconds;
			return {Admission::kOpenedLateWindow, abandoned_};
		}
		if (abandoned_)
		{
			if (now < abandonedUntil_) return {Admission::kBusy, std::nullopt};
			return {Admission::kExpiredLateWindow, std::exchange(abandoned_, std::nullopt)};
		}
		return {Admission::kReady, std::nullopt};
	}

	uint64_t Reserve(GuidKind kind, double now)
	{
		assert(!inflight_ && !abandoned_);
		const auto sequence = ++nextSequence_;
		inflight_ = Request{sequence, kind, now};
		return sequence;
	}

	std::optional<Completion> Take(uint64_t sequence)
	{
		if (inflight_ && inflight_->sequence == sequence)
			return Completion{std::exchange(inflight_, std::nullopt)->kind, false};
		if (abandoned_ && abandoned_->sequence == sequence)
			return Completion{std::exchange(abandoned_, std::nullopt)->kind, true};
		return std::nullopt;
	}

	void Clear()
	{
		inflight_.reset();
		abandoned_.reset();
	}

private:
	std::optional<Request> inflight_;
	std::optional<Request> abandoned_;
	double abandonedUntil_ = 0;
	uint64_t nextSequence_ = 0;
};
}
