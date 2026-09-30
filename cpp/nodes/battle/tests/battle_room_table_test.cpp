// battle 房间表单测(集群外入口 D82 / D85):每次插表 / 移除恰好触发一次生命周期回调,
// Agones 单元计数不会因为重复插入、删除不存在的键、批量清空而错位。
//
// 与 battle_push_policy_test.cpp 同一种组织方式:**不进 battle.vcxproj 的 ClCompile 列表**,
// 不会被 MSBuild 编进 battle 可执行文件,也不在 CMakeLists.txt 里 —— 独立跑。
// battle_room_table.h 刻意不依赖 muduo / protobuf / 引擎,就是为了让这份测试能在没有
// 整套 C++ 引擎的机器上直接编出来。
//
// 跑法(仓库根目录,Linux 或带 g++ 的容器;gtest 用仓库里已经 vendored 的那份):
//
//   GT=third_party/grpc/third_party/googletest/googletest
//   g++ -std=c++23 -I cpp/nodes/battle -I "$GT/include" -I "$GT"
//       cpp/nodes/battle/tests/battle_room_table_test.cpp
//       "$GT/src/gtest-all.cc" "$GT/src/gtest_main.cc"
//       -lpthread -o /tmp/battle_room_table_test
//   /tmp/battle_room_table_test
//
// (上面四行是同一条命令,换行只为可读;实际执行时接成一行。)

#include <gtest/gtest.h>

#include <algorithm>
#include <cstdint>
#include <initializer_list>
#include <map>
#include <memory>
#include <vector>

#include "battle_room_table.h"

namespace
{

using battle_room_table::Hooks;
using battle_room_table::RoomTable;

// 测试用房间:析构时计数,用来验证"重复插入时新对象随形参销毁"的所有权契约。
struct FakeRoom
{
	explicit FakeRoom(int marker, int *destroyed = nullptr) : marker(marker), destroyed(destroyed) {}
	~FakeRoom()
	{
		if (destroyed != nullptr)
		{
			++*destroyed;
		}
	}

	int marker = 0;
	int *destroyed = nullptr;
};

// 记录每个 roomId 收到的回调次数(onCreated / onRemoved 分开记)。
struct HookRecorder
{
	std::map<uint64_t, int> created;
	std::map<uint64_t, int> removed;

	Hooks MakeHooks()
	{
		return Hooks{[this](const uint64_t roomId) { ++created[roomId]; },
					 [this](const uint64_t roomId) { ++removed[roomId]; }};
	}

	int TotalCreated() const
	{
		int total = 0;
		for (const auto &[roomId, count] : created)
		{
			total += count;
		}
		return total;
	}

	int TotalRemoved() const
	{
		int total = 0;
		for (const auto &[roomId, count] : removed)
		{
			total += count;
		}
		return total;
	}
};

TEST(BattleRoomTable, EmplaceFiresOnCreatedExactlyOnce)
{
	RoomTable<FakeRoom> table;
	HookRecorder recorder;
	table.SetHooks(recorder.MakeHooks());

	FakeRoom *room = table.Emplace(7, std::make_unique<FakeRoom>(70));

	ASSERT_NE(room, nullptr);
	EXPECT_EQ(room->marker, 70);
	EXPECT_EQ(table.Find(7), room);
	EXPECT_EQ(table.Size(), 1u);
	EXPECT_EQ(recorder.created[7], 1);
	EXPECT_EQ(recorder.TotalRemoved(), 0);
}

TEST(BattleRoomTable, DuplicateEmplaceIsRejectedWithoutHookAndKeepsOriginalRoom)
{
	RoomTable<FakeRoom> table;
	HookRecorder recorder;
	table.SetHooks(recorder.MakeHooks());
	int duplicateDestroyed = 0;

	FakeRoom *original = table.Emplace(7, std::make_unique<FakeRoom>(70));
	FakeRoom *duplicate = table.Emplace(7, std::make_unique<FakeRoom>(71, &duplicateDestroyed));

	EXPECT_EQ(duplicate, nullptr);
	// 表里那间原样保留,单元计数不重复加
	EXPECT_EQ(table.Find(7), original);
	EXPECT_EQ(table.Find(7)->marker, 70);
	EXPECT_EQ(table.Size(), 1u);
	EXPECT_EQ(recorder.created[7], 1);
	// 所有权契约:按值传入的新房间随形参销毁,不泄漏也不留在调用方手里
	EXPECT_EQ(duplicateDestroyed, 1);
}

TEST(BattleRoomTable, NullRoomIsRejectedWithoutHook)
{
	RoomTable<FakeRoom> table;
	HookRecorder recorder;
	table.SetHooks(recorder.MakeHooks());

	EXPECT_EQ(table.Emplace(7, nullptr), nullptr);
	EXPECT_TRUE(table.Empty());
	EXPECT_EQ(recorder.TotalCreated(), 0);
}

TEST(BattleRoomTable, EraseFiresOnRemovedExactlyOnceAndDestroysRoom)
{
	RoomTable<FakeRoom> table;
	HookRecorder recorder;
	table.SetHooks(recorder.MakeHooks());
	int destroyed = 0;
	table.Emplace(7, std::make_unique<FakeRoom>(70, &destroyed));

	EXPECT_TRUE(table.Erase(7));
	EXPECT_EQ(table.Find(7), nullptr);
	EXPECT_EQ(destroyed, 1);
	EXPECT_EQ(recorder.removed[7], 1);

	// 第二次移除同一个键:no-op,不再触发(计数不会被减成负数)
	EXPECT_FALSE(table.Erase(7));
	EXPECT_EQ(recorder.removed[7], 1);
}

TEST(BattleRoomTable, EraseUnknownIdIsNoOpWithoutHook)
{
	RoomTable<FakeRoom> table;
	HookRecorder recorder;
	table.SetHooks(recorder.MakeHooks());
	table.Emplace(7, std::make_unique<FakeRoom>(70));

	EXPECT_FALSE(table.Erase(8));
	EXPECT_EQ(table.Size(), 1u);
	EXPECT_EQ(recorder.TotalRemoved(), 0);
}

TEST(BattleRoomTable, BulkEraseViaIdsSnapshotFiresOncePerRoom)
{
	// AbortAllRooms 的形状:先抄 id,再逐个 Erase
	RoomTable<FakeRoom> table;
	HookRecorder recorder;
	table.SetHooks(recorder.MakeHooks());
	for (const uint64_t roomId : {1u, 2u, 3u})
	{
		table.Emplace(roomId, std::make_unique<FakeRoom>(static_cast<int>(roomId)));
	}

	std::vector<uint64_t> ids = table.Ids();
	std::sort(ids.begin(), ids.end());
	EXPECT_EQ(ids, (std::vector<uint64_t>{1, 2, 3}));

	for (const uint64_t roomId : ids)
	{
		EXPECT_TRUE(table.Erase(roomId));
	}

	EXPECT_TRUE(table.Empty());
	EXPECT_EQ(recorder.TotalRemoved(), 3);
	for (const uint64_t roomId : ids)
	{
		EXPECT_EQ(recorder.removed[roomId], 1);
	}
}

TEST(BattleRoomTable, HooksObserveTableAlreadyUpdated)
{
	// 契约:onCreated 时房间已在表里;onRemoved 时房间已不在表里
	// (最后一个房间移除之后 Agones 才能回 Ready,顺序反了会在房间还在时放出容量)
	RoomTable<FakeRoom> table;
	bool presentOnCreated = false;
	bool absentOnRemoved = false;
	table.SetHooks(Hooks{[&table, &presentOnCreated](const uint64_t roomId)
						 { presentOnCreated = table.Find(roomId) != nullptr; },
						 [&table, &absentOnRemoved](const uint64_t roomId)
						 { absentOnRemoved = table.Find(roomId) == nullptr; }});

	table.Emplace(7, std::make_unique<FakeRoom>(70));
	table.Erase(7);

	EXPECT_TRUE(presentOnCreated);
	EXPECT_TRUE(absentOnRemoved);
}

TEST(BattleRoomTable, UnsetHooksAreSkipped)
{
	RoomTable<FakeRoom> table;

	EXPECT_NE(table.Emplace(7, std::make_unique<FakeRoom>(70)), nullptr);
	EXPECT_TRUE(table.Erase(7));
	EXPECT_TRUE(table.Empty());
}

} // namespace
