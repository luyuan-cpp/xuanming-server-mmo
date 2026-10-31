#pragma once

// 指名广播的 (session_id, player_id) 成对编解码。
//
// 服务两种消息:节点间 RPC 的 BroadcastToPlayersRequest(scene → gate,proto/gate/gate_service.proto)
// 与 Kafka 的 contracts::kafka::BroadcastToPlayersEvent(Go / battle → gate,proto/contracts/kafka/gate_event.proto)。
// 两者 session_list / session_bitmap_base / session_bitmap / player_list 的字段号、语义与顺序契约完全相同
// (routing_identity_test 用 static_assert 钉住字段号),所以下面都是对消息类型的模板,只有一份实现。
//
// 为什么单独成一个纯头(docs/design/routing-identity-audit-20260908.md R13):
// 广播里目标玩家身份是**按位置对应**的 —— player_list[i] 对应"按 session_id 升序"
// 的第 i 个目标。编码在 scene(client_message_sender.cpp)与 Go(go/shared/kafkautil 的
// EncodeBroadcastTargets)、解码在 gate(handler/gate_push_delivery.cpp),任意两边错开一格
// 就是把每个人的身份都比对成别人的,而这种错位编译器看不见、集成测试也未必碰得到。
// C++ 两侧收进同一个头,由一个单测直接跑 Encode→ForEach 的往返(cpp/tests/routing_identity_test);
// Go 编码器与这里的编解码读同一份向量 go/shared/kafkautil/testdata/broadcast_target_vectors.json 对拍
// (cpp/tests/kafka_command_test/broadcast_target_vectors_test.cpp)。
//
// 编码有两种形态(沿用既有的体积优化):
//   - bitmap:session 密集时更省(base + span/8 字节);gate 按 bit 序遍历。
//     位序:第 N 位(第 N/8 字节的 1 << (N%8),字节内低位在前)置位 = session (base + N)。
//   - list  :稀疏时更省;发送方按升序填。
// 两种形态下"第 i 个目标"的定义完全一致 = session_id 升序的第 i 个,
// 因为 bit 序天然就是 session_id 升序。这就是顺序契约唯一的支点。

#include <algorithm>
#include <bit>
#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

// 模板本身不依赖它;保留是为了既有调用方(scene client_message_sender.cpp 等)经本头拿到 RPC 请求类型。
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

	// 去重后的目标数达到它才考虑 bitmap。Go 侧 EncodeBroadcastTargets 必须同阈值、同体积判据
	// (共享向量逐字段钉住两边的输出)。
	inline constexpr std::size_t kBitmapThreshold = 32;

	// 编码到 msg(会**就地稳定排序并按 sessionId 去重** targets,同一 session 重复时保留输入里的第一个)。
	//
	// 去重是对齐前提而不是优化:同一个 session 出现两次时,bitmap 只会置一位,
	// 而 player_list 会多一项 —— 之后每一项都错位。
	// 必须是稳定排序:std::sort 不稳定,同一 session 带不同玩家时留下哪一个取决于标准库实现,
	// Go 侧 EncodeBroadcastTargets(同样是稳定排序、保留首个)与共享向量都无法逐字对齐。
	template <typename BroadcastMsg>
	inline void Encode(BroadcastMsg &msg, std::vector<Target> &targets)
	{
		std::stable_sort(targets.begin(), targets.end());
		targets.erase(std::unique(targets.begin(), targets.end(),
								  [](const Target &a, const Target &b)
								  { return a.sessionId == b.sessionId; }),
					  targets.end());

		msg.clear_session_list();
		msg.clear_session_bitmap();
		msg.clear_session_bitmap_base();
		msg.clear_player_list();

		if (targets.empty())
		{
			return;
		}

		for (const auto &target : targets)
		{
			msg.mutable_player_list()->Add(target.playerId);
		}

		// Bitmap: base(uint32) + N/8 bytes.  List: ~3 bytes per varint (session IDs > 16384).
		// 选哪种只看 session 部分 —— player_list 两种形态一模一样,换编码省不掉它。
		if (targets.size() >= kBitmapThreshold)
		{
			const uint32_t minId = targets.front().sessionId;
			const uint32_t maxId = targets.back().sessionId;
			// 跨度按 uint64 算(与 Go 侧同口径):uint32 下 [0, UINT32_MAX] 的跨度会回绕成 0,
			// 算出一个装不下目标的空 bitmap,随后按偏移写越界。
			const uint64_t span = static_cast<uint64_t>(maxId) - minId + 1;
			const uint64_t bitmapBytes = (span + 7) / 8;

			if (bitmapBytes + 6 < static_cast<uint64_t>(targets.size()) * 3)
			{
				msg.set_session_bitmap_base(minId);
				std::string bitmap(static_cast<std::size_t>(bitmapBytes), '\0');
				for (const auto &target : targets)
				{
					const uint32_t offset = target.sessionId - minId;
					bitmap[offset / 8] |= static_cast<char>(1 << (offset % 8));
				}
				msg.set_session_bitmap(std::move(bitmap));
				return;
			}
		}

		for (const auto &target : targets)
		{
			msg.mutable_session_list()->Add(target.sessionId);
		}
	}

	// 目标数 = ForEach 的回调次数:bitmap 非空时数置位,否则数 session_list。
	// 两种形态同时出现时与 ForEach 一样以 bitmap 为准。
	template <typename BroadcastMsg>
	inline std::size_t TargetCount(const BroadcastMsg &msg)
	{
		const auto &bitmap = msg.session_bitmap();
		if (bitmap.empty())
		{
			return static_cast<std::size_t>(msg.session_list_size());
		}
		std::size_t count = 0;
		for (const char byte : bitmap)
		{
			count += static_cast<std::size_t>(std::popcount(static_cast<uint8_t>(byte)));
		}
		return count;
	}

	// player_list 能否按位置与目标一一对应:留空(老发送方的兼容位,ForEach 一律给 0 = 不校验)
	// 或长度恰好等于目标数。非空却不齐只可能是发送方编码有缺陷 —— 无论补 0 还是截断,
	// 错位之后每一项都会比对成别人的身份,调用方应当整条拒绝(gate_push_delivery.cpp)。
	template <typename BroadcastMsg>
	inline bool PlayerListAligned(const BroadcastMsg &msg)
	{
		const auto playerListSize = static_cast<std::size_t>(msg.player_list_size());
		return playerListSize == 0 || playerListSize == TargetCount(msg);
	}

	// 解码:按契约顺序回调 fn(sessionId, playerId)。
	// player_list 留空(老发送方)时一律给 0 = 不校验。非空却不齐的消息应先经 PlayerListAligned 整条拒绝;
	// 这里仍按「不足补 0、多余忽略」处理,只为任何输入下都绝不越界读。
	template <typename BroadcastMsg, typename Fn>
	inline void ForEach(const BroadcastMsg &msg, Fn &&fn)
	{
		const auto &playerList = msg.player_list();
		int index = 0;
		auto nextPlayerId = [&playerList, &index]() -> uint64_t
		{
			if (index >= playerList.size())
			{
				return 0;
			}
			return playerList.Get(index++);
		};

		if (!msg.session_bitmap().empty())
		{
			const uint32_t base = msg.session_bitmap_base();
			const auto &bitmap = msg.session_bitmap();
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

		for (const auto sessionId : msg.session_list())
		{
			const uint64_t playerId = nextPlayerId();
			fn(static_cast<uint32_t>(sessionId), playerId);
		}
	}
} // namespace broadcast_targets
