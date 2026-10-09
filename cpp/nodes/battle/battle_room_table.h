#pragma once

// battle 房间表:房间的增删与"单元创建 / 销毁"回调绑在一起(集群外入口 D82 / D85)。
//
// 为什么单独成类:房间 = Agones 单元,GameServerLifecycle 的单元计数要求每次插表 / 移除
// **恰好**通知一次。漏通知 → GameServer 零房间却卡在 Allocated,Fleet 回收不了;
// 多 / 错通知 → 有房间却回了 Ready,Fleet 缩容 / 滚动时可能直接删掉它,战斗中途作废。
// 底层容器藏在本类里,能改表的只有 Emplace / Erase,两者各自触发回调 —— 调用方没有
// "直接写 map 绕过回调"的路径,这条不变量不再靠 grep 和评审守。
//
// 与 battle_push_policy.h 同一条纪律:不依赖 muduo / protobuf / 引擎 / gNode,只做表与回调,
// 不打日志 —— 重复插入等异常由调用点(BattleRoomManager::EmplaceRoom)记日志。这样本头能在
// 没有整套引擎的机器上直接编单测(tests/battle_room_table_test.cpp)。
//
// 线程模型:不加锁,只允许单线程访问(battle 里是 muduo loop 线程)。
// 回调在表已更新之后同步调用,必须快速返回;回调里可以 Find,不得 Emplace / Erase(重入)。

#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <unordered_map>
#include <utility>
#include <vector>

namespace battle_room_table
{

// 未设置的回调 = 不通知(本地 / 测试)。
struct Hooks
{
	// 房间**已插表之后**调用(单元确实创建成功)。
	std::function<void(uint64_t roomId)> onCreated;
	// 房间**已从表里移除并销毁之后**调用(单元确实销毁)。
	std::function<void(uint64_t roomId)> onRemoved;
};

template <typename Room>
class RoomTable
{
public:
	void SetHooks(Hooks hooks) { hooks_ = std::move(hooks); }

	Room *Find(const uint64_t roomId)
	{
		const auto it = rooms_.find(roomId);
		return it == rooms_.end() ? nullptr : it->second.get();
	}

	const Room *Find(const uint64_t roomId) const
	{
		const auto it = rooms_.find(roomId);
		return it == rooms_.end() ? nullptr : it->second.get();
	}

	// 插入并恰好触发一次 onCreated,返回表内对象。
	// 拒绝(返回 nullptr、不触发回调)的两种情况:
	//   * roomId 已存在:表里那间保持原样;room 按值传入,随形参一起销毁
	//     (调用方应在插表之前判过幂等,走到这里说明是程序缺陷);
	//   * room 为空。
	Room *Emplace(const uint64_t roomId, std::unique_ptr<Room> room)
	{
		if (room == nullptr)
		{
			return nullptr;
		}
		const auto [it, inserted] = rooms_.try_emplace(roomId, std::move(room));
		if (!inserted)
		{
			return nullptr;
		}
		if (hooks_.onCreated)
		{
			hooks_.onCreated(roomId);
		}
		return it->second.get();
	}

	// 移除(同时销毁房间对象)并恰好触发一次 onRemoved,返回 true;
	// 房间不存在时 no-op、不触发回调,返回 false。调用之后该房间的指针 / 引用全部悬空。
	bool Erase(const uint64_t roomId)
	{
		if (rooms_.erase(roomId) == 0)
		{
			return false;
		}
		if (hooks_.onRemoved)
		{
			hooks_.onRemoved(roomId);
		}
		return true;
	}

	// 当前全部 roomId 的快照(顺序不保证)。批量移除时先抄 id 再逐个 Erase,不能边遍历边删。
	std::vector<uint64_t> Ids() const
	{
		std::vector<uint64_t> ids;
		ids.reserve(rooms_.size());
		for (const auto &entry : rooms_)
		{
			ids.push_back(entry.first);
		}
		return ids;
	}

	bool Empty() const { return rooms_.empty(); }
	std::size_t Size() const { return rooms_.size(); }

private:
	Hooks hooks_;
	std::unordered_map<uint64_t, std::unique_ptr<Room>> rooms_;
};

} // namespace battle_room_table
