#include "navigation.h"

#include <memory>

#include "muduo/base/Logging.h"

#include "config/config.h"
#include "table/code/basescene_table.h"
#include "spatial/system/recast.h"
#include "spatial/constants/nav.h"
#include "spatial/manager/scene_nav.h"

void NavigationSystem::LoadNavBins()
{
	auto &configAll = FindAllBaseSceneTable();
	for (auto &item : configAll.data())
	{
		if (item.nav_bin_file().empty())
		{
			continue;
		}

		// 堆上就地构造:navQuery.init 会把 &navMesh 存进内部裸指针,
		// 对象必须先落在最终地址上再做加载/初始化(旧写法先在栈上装好
		// 再 emplace,浅拷贝进 map 后栈局部析构,map 里全是悬垂指针,
		// 线程退出时再 double free)。
		auto nav = std::make_unique<NavComp>();
		const std::string navPath = GetDataRootDir() + item.nav_bin_file();
		if (!RecastSystem::LoadNavMesh(navPath.c_str(), &nav->navMesh))
		{
			// fail-closed:加载失败绝不注册空网格 —— 查询方拿到一个
			// "存在但零 tile"的导航,寻路会静默给出穿墙直线。
			LOG_ERROR << "skip nav registration for scene " << item.id()
					  << " (nav bin load failed): " << navPath;
			continue;
		}
		const dtStatus initStatus = nav->navQuery.init(&nav->navMesh, kMaxMeshQueryNodes);
		if (dtStatusFailed(initStatus))
		{
			LOG_ERROR << "skip nav registration for scene " << item.id()
					  << " (navQuery init failed): " << navPath;
			continue;
		}
		SceneNavManager::Instance().AddNav(item.id(), std::move(nav));
	}
}
