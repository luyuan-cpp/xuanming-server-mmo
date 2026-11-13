#pragma once

#include <vector>

#include "entt/src/entt/entity/entity.hpp"
#include "modules/bag/bag_service.h"

// 「腾位通知」(bag_service.h)的测试订阅方:只按顺序记下被通知的玩家实体,作用域结束时卸掉。
//
// 订阅方槽位是进程级的,生产由 scene main 装 PlayerInboxSystem::OnBagSpaceFreed;用例里装这个记录器,
// 看哪个出包入口、在什么条件下通知了几次。构造时清空记录并装上,析构时卸掉(恢复为未安装)并清空,
// 不把状态留给别的用例。只在测试线程(scene loop 的替身)上用。
class ScopedSpaceFreedRecorder
{
public:
	ScopedSpaceFreedRecorder()
	{
		Notified().clear();
		BagService::SetSpaceFreedListener(&Record);
	}
	~ScopedSpaceFreedRecorder()
	{
		BagService::SetSpaceFreedListener(nullptr);
		Notified().clear();
	}
	ScopedSpaceFreedRecorder(const ScopedSpaceFreedRecorder &) = delete;
	ScopedSpaceFreedRecorder &operator=(const ScopedSpaceFreedRecorder &) = delete;

	// 至今收到的通知,按到达顺序。
	static std::vector<entt::entity> &Notified()
	{
		static std::vector<entt::entity> notified;
		return notified;
	}

private:
	static void Record(entt::entity player) { Notified().push_back(player); }
};
