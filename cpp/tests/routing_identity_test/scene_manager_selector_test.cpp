#include <gtest/gtest.h>

#include <cstddef>
#include <cstdint>
#include <optional>
#include <set>
#include <vector>

#include "network/node_utils.h"

// ---------------------------------------------------------------------------
// scene_manager_selector_test —— SceneManager 实例选择(node_utils.h 的 scene_manager_selector::Pick)。
//
// 旧 GetSceneManagerEntity 是 playerId % 已注册实例数,不看通道状态:
//   1) 某个实例崩溃后,它的 etcd 租约 60s 内仍在注册表里,约 1/N 的换图请求打到死实例;
//   2) PlayerId 低 9 位是毫秒内序号、大多为 0,实例数为 2 / 4 时几乎所有玩家都落在第 0 个实例。
// 新规则只在"有更好的选择"时避开坏通道,底线不变:候选非空就一定挑出一个,null 只留给空表
// (调用方拿到 null 走的立即失败分支比"挑一个坏实例、让那次调用失败"更伤,见 node_utils.cpp)。
// 选择规则拆成纯函数,这里直接喂 (实体, 通道状态) 表,不起 gRPC;通道状态为 std::nullopt 表示实体没挂通道。
// GetSceneManagerEntity 本身只是"从注册表拼候选表 + GetState(true) + 兜底告警节流"的薄适配,不在本文件覆盖。
// ---------------------------------------------------------------------------

namespace
{
	using scene_manager_selector::Candidate;
	using scene_manager_selector::Pick;

	const entt::entity kNullEntity = entt::null;
	const uint64_t kNone = entt::to_integral(kNullEntity);

	entt::entity Instance(uint64_t id)
	{
		return static_cast<entt::entity>(id);
	}

	// 比整数而不是比 entt::entity:失败时 gtest 能打印出实例号。
	uint64_t Picked(const std::vector<Candidate> &candidates, Guid playerId)
	{
		return entt::to_integral(Pick(candidates, playerId));
	}

	// 与 login 发的 PlayerId 同形:bwmarrin 布局 41 位毫秒 | 13 位 node | 9 位 step。
	Guid MakePlayerId(uint64_t millis, uint64_t node, uint64_t step)
	{
		return (millis << 22) | (node << 9) | step;
	}

	// 一批"低频建角"的玩家号:各在不同毫秒、step 都是 0 —— 旧实现取模会全压到第 0 个实例的那种形状。
	std::vector<Guid> LowRatePlayerIds(std::size_t count)
	{
		std::vector<Guid> ids;
		ids.reserve(count);
		for (std::size_t i = 0; i < count; ++i)
		{
			ids.push_back(MakePlayerId(/*millis=*/2'592'000'000ull + i * 7919ull, /*node=*/5, /*step=*/0));
		}
		return ids;
	}

	constexpr std::size_t kPlayerCount = 300;
} // namespace

TEST(SceneManagerSelector, EmptyCandidateListPicksNothing)
{
	// 空表是唯一返回 null 的情形(注册表里一个 SceneManager 都没有),与旧实现相同。
	EXPECT_EQ(Picked({}, MakePlayerId(1000, 1, 0)), kNone);
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		EXPECT_EQ(Picked({}, playerId), kNone);
	}
}

TEST(SceneManagerSelector, AllReadySpreadsPlayersOverEveryInstanceAndIsDeterministic)
{
	const std::vector<Candidate> candidates{
		{Instance(1), GRPC_CHANNEL_READY},
		{Instance(2), GRPC_CHANNEL_READY},
		{Instance(3), GRPC_CHANNEL_READY},
	};
	std::size_t hits[4]{};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		const uint64_t first = Picked(candidates, playerId);
		ASSERT_GE(first, 1u);
		ASSERT_LE(first, 3u);
		EXPECT_EQ(Picked(candidates, playerId), first) << "同一输入必须得到同一实例";
		++hits[first];
	}
	// 期望各 100 个;60 是均匀分布下约 5 个标准差之外,只有打散失效才会跌破。
	EXPECT_GE(hits[1], 60u);
	EXPECT_GE(hits[2], 60u);
	EXPECT_GE(hits[3], 60u);
}

TEST(SceneManagerSelector, TwoInstancesShareSnowflakeShapedPlayersEvenly)
{
	const std::vector<Candidate> candidates{
		{Instance(1), GRPC_CHANNEL_READY},
		{Instance(2), GRPC_CHANNEL_READY},
	};
	std::size_t hits[3]{};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		// 前提:这批号全是偶数,旧实现 playerId % 2 会把它们全部交给第 0 个实例。
		ASSERT_EQ(playerId % 2, 0u);
		const uint64_t picked = Picked(candidates, playerId);
		ASSERT_TRUE(picked == 1u || picked == 2u) << "picked=" << picked;
		++hits[picked];
	}
	EXPECT_GE(hits[1], 100u) << "期望约 150;全压一边说明取模前没有打散";
	EXPECT_GE(hits[2], 100u) << "期望约 150;全压一边说明取模前没有打散";
}

TEST(SceneManagerSelector, PrefersReadyOverIdleAndConnecting)
{
	const std::vector<Candidate> candidates{
		{Instance(1), GRPC_CHANNEL_IDLE},
		{Instance(2), GRPC_CHANNEL_READY},
		{Instance(3), GRPC_CHANNEL_CONNECTING},
	};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		EXPECT_EQ(Picked(candidates, playerId), 2u);
	}
}

TEST(SceneManagerSelector, NeverPicksTransientFailureOrShutdownWhileUsableInstanceExists)
{
	// 崩溃的实例:通道被踢去重连后停在 TRANSIENT_FAILURE,etcd 租约到期前仍在注册表里。
	// 只要还有 READY / IDLE / CONNECTING 的实例,就绝不挑它。
	const std::vector<Candidate> besideReady{
		{Instance(1), GRPC_CHANNEL_TRANSIENT_FAILURE},
		{Instance(2), GRPC_CHANNEL_READY},
		{Instance(3), GRPC_CHANNEL_SHUTDOWN},
		{Instance(4), GRPC_CHANNEL_READY},
		{Instance(5), GRPC_CHANNEL_TRANSIENT_FAILURE},
	};
	std::set<uint64_t> used;
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		const uint64_t picked = Picked(besideReady, playerId);
		EXPECT_TRUE(picked == 2u || picked == 4u) << "picked=" << picked;
		used.insert(picked);
	}
	EXPECT_EQ(used, (std::set<uint64_t>{2u, 4u})) << "两个活实例都应分到玩家";

	// 只剩一个 CONNECTING(比如对端主机刚被替换、还在连)时也一样。
	const std::vector<Candidate> besideConnecting{
		{Instance(1), GRPC_CHANNEL_SHUTDOWN},
		{Instance(2), GRPC_CHANNEL_TRANSIENT_FAILURE},
		{Instance(3), GRPC_CHANNEL_CONNECTING},
	};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		EXPECT_EQ(Picked(besideConnecting, playerId), 3u);
	}
}

TEST(SceneManagerSelector, NoReadyFallsBackToIdleAndConnectingInsteadOfFailing)
{
	// 刚启动(通道还没拨过号)或 30 分钟没有调用(客户端空闲超时)时通道是 IDLE,不能因此判"没有 SceneManager"。
	const std::vector<Candidate> allIdle{
		{Instance(1), GRPC_CHANNEL_IDLE},
		{Instance(2), GRPC_CHANNEL_IDLE},
		{Instance(3), GRPC_CHANNEL_CONNECTING},
	};
	std::set<uint64_t> used;
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		const uint64_t picked = Picked(allIdle, playerId);
		ASSERT_NE(picked, kNone);
		used.insert(picked);
	}
	EXPECT_EQ(used, (std::set<uint64_t>{1u, 2u, 3u}));

	// 退到 IDLE / CONNECTING 这一级时同样跳过坏实例。
	const std::vector<Candidate> idleBesideBroken{
		{Instance(1), GRPC_CHANNEL_TRANSIENT_FAILURE},
		{Instance(2), GRPC_CHANNEL_IDLE},
		{Instance(3), GRPC_CHANNEL_SHUTDOWN},
	};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		EXPECT_EQ(Picked(idleBesideBroken, playerId), 2u);
	}
}

TEST(SceneManagerSelector, AllChannelsFailedStillPicksOneDeterministically)
{
	// 全部实例都在 TRANSIENT_FAILURE / SHUTDOWN:不返回 null,落到兜底级照样挑一个(与旧实现"总能挑到"一致)。
	// 那次调用以 UNAVAILABLE 进失败处理器 / 由看门狗收尾;返回 null 反而会让跨 zone 交接在 SET 之后销毁 + 踢线。
	const std::vector<Candidate> candidates{
		{Instance(1), GRPC_CHANNEL_TRANSIENT_FAILURE},
		{Instance(2), GRPC_CHANNEL_SHUTDOWN},
		{Instance(3), GRPC_CHANNEL_TRANSIENT_FAILURE},
	};
	std::set<uint64_t> used;
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		const uint64_t picked = Picked(candidates, playerId);
		ASSERT_NE(picked, kNone);
		ASSERT_GE(picked, 1u);
		ASSERT_LE(picked, 3u);
		EXPECT_EQ(Picked(candidates, playerId), picked) << "同一输入必须得到同一实例";
		used.insert(picked);
	}
	// 兜底级内同样按打散后的 playerId 取模,不是固定压在第一个实例上。
	EXPECT_EQ(used, (std::set<uint64_t>{1u, 2u, 3u}));
}

TEST(SceneManagerSelector, ChannellessEntityIsPickedOnlyWhenNothingElseIsRegistered)
{
	// 没挂通道的实体(TCP 握手声明成 SceneManager 造出的,或单测只挂 NodeInfo 的假节点)挑中后一发送就断言,
	// 所以只要有任何挂了通道的实例 —— 不论状态 —— 都轮不到它。
	const std::vector<Candidate> besideReady{
		{Instance(1), std::nullopt},
		{Instance(2), GRPC_CHANNEL_READY},
	};
	const std::vector<Candidate> besideIdle{
		{Instance(1), std::nullopt},
		{Instance(2), GRPC_CHANNEL_IDLE},
	};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		EXPECT_EQ(Picked(besideReady, playerId), 2u);
		EXPECT_EQ(Picked(besideIdle, playerId), 2u);
	}

	// 注册表里只有它(cross_zone_test 的 ScopedFakeSceneManagerNode 就是这个形状):照样挑中,不返回 null。
	const std::vector<Candidate> onlyChannelless{
		{Instance(1), std::nullopt},
	};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		EXPECT_EQ(Picked(onlyChannelless, playerId), 1u);
	}

	// 都没通道时同样确定性地取模分散。
	const std::vector<Candidate> allChannelless{
		{Instance(1), std::nullopt},
		{Instance(2), std::nullopt},
	};
	std::set<uint64_t> used;
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		const uint64_t picked = Picked(allChannelless, playerId);
		ASSERT_TRUE(picked == 1u || picked == 2u) << "picked=" << picked;
		EXPECT_EQ(Picked(allChannelless, playerId), picked) << "同一输入必须得到同一实例";
		used.insert(picked);
	}
	EXPECT_EQ(used, (std::set<uint64_t>{1u, 2u}));
}

TEST(SceneManagerSelector, FailedChannelIsPreferredOverChannellessEntity)
{
	// 兜底级内再分两级:挂了通道的坏实例(那次调用以 UNAVAILABLE 失败,走失败处理器)排在没挂通道的实体
	// (一发送就断言)之前。若把"没通道"记成 SHUTDOWN 与坏实例同级取模,这里会有约一半玩家挑到 1 / 4。
	const std::vector<Candidate> candidates{
		{Instance(1), std::nullopt},
		{Instance(2), GRPC_CHANNEL_TRANSIENT_FAILURE},
		{Instance(3), GRPC_CHANNEL_SHUTDOWN},
		{Instance(4), std::nullopt},
	};
	std::set<uint64_t> used;
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		const uint64_t picked = Picked(candidates, playerId);
		EXPECT_TRUE(picked == 2u || picked == 3u) << "picked=" << picked;
		used.insert(picked);
	}
	EXPECT_EQ(used, (std::set<uint64_t>{2u, 3u}));

	// 只有一个 TRANSIENT_FAILURE 实例与无通道实体并存时,永远选那个 TRANSIENT_FAILURE 实例。
	const std::vector<Candidate> oneFailedBesideChannelless{
		{Instance(1), std::nullopt},
		{Instance(2), GRPC_CHANNEL_TRANSIENT_FAILURE},
	};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		EXPECT_EQ(Picked(oneFailedBesideChannelless, playerId), 2u);
	}
}

TEST(SceneManagerSelector, PlayerLeavesAnInstanceThatDiesAndReturnsWhenItRecovers)
{
	const std::vector<Candidate> healthy{
		{Instance(1), GRPC_CHANNEL_READY},
		{Instance(2), GRPC_CHANNEL_READY},
		{Instance(3), GRPC_CHANNEL_READY},
	};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		const uint64_t before = Picked(healthy, playerId);

		// 玩家所在的那个实例坏了:必须换到别的活实例上。
		std::vector<Candidate> oneDown = healthy;
		for (auto &candidate : oneDown)
		{
			if (entt::to_integral(candidate.entity) == before)
			{
				candidate.channelState = GRPC_CHANNEL_TRANSIENT_FAILURE;
			}
		}
		const uint64_t during = Picked(oneDown, playerId);
		EXPECT_NE(during, before);
		EXPECT_NE(during, kNone);

		// 恢复之后候选集与原来一致,落点也回到原来的实例(选择只取决于候选集与 playerId)。
		EXPECT_EQ(Picked(healthy, playerId), before);
	}
	// 注意:这里不断言"只有落在坏实例上的玩家才换人"。选择按同档实例数取模,候选集一变多数玩家都会
	// 换实例;scene_manager 没有按玩家驻留在单个实例内存里的状态,所以这不影响正确性(见 node_utils.cpp)。
}

TEST(SceneManagerSelector, NonReadyInstancesDoNotReshuffleReadyPicks)
{
	// 新发现的实例(IDLE)、正在死去的实例(TRANSIENT_FAILURE)或没挂通道的实体进出候选表,不应打乱 READY 级
	// 里已有的分配:取模只数同一级,别级的成员插在哪里都不影响序号。
	const std::vector<Candidate> readyOnly{
		{Instance(1), GRPC_CHANNEL_READY},
		{Instance(2), GRPC_CHANNEL_READY},
	};
	const std::vector<Candidate> withOthers{
		{Instance(7), GRPC_CHANNEL_IDLE},
		{Instance(1), GRPC_CHANNEL_READY},
		{Instance(3), GRPC_CHANNEL_TRANSIENT_FAILURE},
		{Instance(9), std::nullopt},
		{Instance(2), GRPC_CHANNEL_READY},
		{Instance(4), GRPC_CHANNEL_CONNECTING},
	};
	for (const Guid playerId : LowRatePlayerIds(kPlayerCount))
	{
		EXPECT_EQ(Picked(withOthers, playerId), Picked(readyOnly, playerId));
	}
}
