#pragma once

// BroadcastToPlayersRequest 的 (session_id, player_id) 成对编解码。
//
// 为什么单独成一个纯头(docs/design/routing-identity-audit-20260908.md R13):
// 广播里目标玩家身份是**按位置对应**的 —— player_list[i] 对应"按 session_id 升序"
// 的第 i 个目标。编码在 scene(player_message_utils.cpp)、解码在 gate
// (gate_service_handler.cpp),两边错开一格就是把每个人的身份都比对成别人的,
// 而这种错位编译器看不见、集成测试也未必碰得到。把两侧收进同一个头,
// 让一个单测直接跑 Encode→ForEach 的往返(cpp/tests/routing_identity_test)。
//
// 编码有两种形态(沿用既有的体积优化):
//   - bitmap:session 密集时更省(base + span/8 字节);gate 按 bit 序遍历。
//   - list  :稀疏时更省;发送方按升序填。
// 两种形态下"第 i 个目标"的定义完全一致 = session_id 升序的第 i 个,
// 因为 bit 序天然就是 session_id 升序。这就是顺序契约唯一的支点。

#include <algorithm>
#include <cstdint>
#include <vector>

#include "proto/gate/gate_service.pb.h"

namespace broadcast_targets
{
	struct Target
	{
		uint32_t sessionId = 0;
		// 0 = 没有玩家身份可带(兼容位),gate 侧放行不校验。
		uint64_t playerId = 0;

		bool operator<(const Target &other) const { return sessionId < other.sessionId; }
	};

	// 编码到 request(会**就地排序并按 sessionId 去重** targets)。
	//
	// 去重是对齐前提而不是优化:同一个 session 出现两次时,bitmap 只会置一位,
	// 而 player_list 会多一项 —— 之后每一项都错位。
	inline void Encode(BroadcastToPlayersRequest &request, std::vector<Target> &targets)
	{
		constexpr size_t kBitmapThreshold = 32;

		std::sort(targets.begin(), targets.end());
		targets.erase(std::unique(targets.begin(), targets.end(),
								  [](const Target &a, const Target &b)
								  { return a.sessionId == b.sessionId; }),
					  targets.end());

		request.clear_session_list();
		request.clear_session_bitmap();
		request.clear_session_bitmap_base();
		request.clear_player_list();

		if (targets.empty())
		{
			return;
		}

		for (const auto &target : targets)
		{
			request.mutable_player_list()->Add(target.playerId);
		}

		// Bitmap: base(uint32) + N/8 bytes.  List: ~3 bytes per varint (session IDs > 16384).
		// 选哪种只看 session 部分 —— player_list 两种形态一模一样,换编码省不掉它。
		if (targets.size() >= kBitmapThreshold)
		{
			const uint32_t minId = targets.front().sessionId;
			const uint32_t maxId = targets.back().sessionId;
			const uint32_t span = maxId - minId + 1;
			const size_t bitmapBytes = (span + 7) / 8;

			if (bitmapBytes + 6 < targets.size() * 3)
			{
				request.set_session_bitmap_base(minId);
				std::string bitmap(bitmapBytes, '\0');
				for (const auto &target : targets)
				{
					const uint32_t offset = target.sessionId - minId;
					bitmap[offset / 8] |= static_cast<char>(1 << (offset % 8));
				}
				request.set_session_bitmap(std::move(bitmap));
				return;
			}
		}

		for (const auto &target : targets)
		{
			request.mutable_session_list()->Add(target.sessionId);
		}
	}

	// 解码:按契约顺序回调 fn(sessionId, playerId)。
	// player_list 比目标数短(老发送方留空 / 发送方有 bug)时补 0 = 不校验,绝不越界读。
	template <typename Fn>
	inline void ForEach(const BroadcastToPlayersRequest &request, Fn &&fn)
	{
		const auto &playerList = request.player_list();
		int index = 0;
		auto nextPlayerId = [&playerList, &index]() -> uint64_t
		{
			if (index >= playerList.size())
			{
				return 0;
			}
			return playerList.Get(index++);
		};

		if (!request.session_bitmap().empty())
		{
			const uint32_t base = request.session_bitmap_base();
			const auto &bitmap = request.session_bitmap();
			for (size_t i = 0; i < bitmap.size(); ++i)
			{
				const auto byte = static_cast<uint8_t>(bitmap[i]);
				for (int bit = 0; bit < 8; ++bit)
				{
					if (byte & (1 << bit))
					{
						// 取号必须在回调之前、且**每个置位都取一次**:
						// 回调内部提前 return(会话不存在等)不能跳过取号,否则后面全错位。
						const uint64_t playerId = nextPlayerId();
						fn(base + static_cast<uint32_t>(i * 8 + bit), playerId);
					}
				}
			}
			return;
		}

		for (const auto sessionId : request.session_list())
		{
			const uint64_t playerId = nextPlayerId();
			fn(static_cast<uint32_t>(sessionId), playerId);
		}
	}
} // namespace broadcast_targets
