#include "navigation.h"

#include <memory>
#include <string>
#include <unordered_map>

#include "muduo/base/Logging.h"

#include "config/config.h"
#include "table/code/basescene_table.h"
#include "spatial/system/nav_query.h"
#include "spatial/system/recast.h"
#include "spatial/system/scene_spawn.h"
#include "spatial/constants/nav.h"
#include "spatial/manager/scene_nav.h"

namespace
{

int CountLoadedTiles(const dtNavMesh& navMesh)
{
	int count = 0;
	for (int i = 0; i < navMesh.getMaxTiles(); ++i)
	{
		const dtMeshTile* tile = navMesh.getTile(i);
		if (tile != nullptr && tile->header != nullptr)
		{
			++count;
		}
	}
	return count;
}

// 加载 + 初始化 + 出生点探针。返回 nullptr 表示该文件不可用(已打 ERROR)。
std::shared_ptr<NavComp> LoadAndProbe(const std::string& navPath, const uint32_t sceneConfigId)
{
	// 堆上就地构造:navQuery.init 会把 &navMesh 存进内部裸指针,
	// 对象必须先落在最终地址上再做加载/初始化(旧写法先在栈上装好
	// 再 emplace,浅拷贝进 map 后栈局部析构,map 里全是悬垂指针,
	// 线程退出时再 double free)。
	auto nav = std::make_shared<NavComp>();
	if (!RecastSystem::LoadNavMesh(navPath.c_str(), &nav->navMesh))
	{
		// fail-closed:加载失败绝不注册空网格 —— 查询方拿到一个
		// "存在但零 tile"的导航,寻路会静默给出穿墙直线。
		LOG_ERROR << "skip nav registration (nav bin load failed): " << navPath;
		return nullptr;
	}
	const dtStatus initStatus = nav->navQuery.init(&nav->navMesh, kMaxMeshQueryNodes);
	if (dtStatusFailed(initStatus))
	{
		LOG_ERROR << "skip nav registration (navQuery init failed): " << navPath;
		return nullptr;
	}

	// 数据契约自检:场景出生点必须落在网格上。旧的 UE 占位 bin(厘米单位、
	// UE 布局,20648 字节)能"成功加载",但和天墉城地图毫无关系 —— 若照常
	// 注册,进场落位会把出生点判成非法、移动裁决把玩家拉进墙里。这种网格
	// 宁可不注册(fail-open:该场景无导航,移动不校验),也不能拿它当真相。
	const Location spawn = SceneSpawnSystem::DefaultSpawnFor(sceneConfigId);
	Location snappedSpawn;
	const bool spawnOnMesh = NavQuerySystem::SnapToMesh(*nav, spawn, snappedSpawn);
	const int tiles = CountLoadedTiles(nav->navMesh);
	const dtNavMeshParams* params = nav->navMesh.getParams();
	if (!spawnOnMesh)
	{
		LOG_ERROR << "skip nav registration (spawn point off navmesh — stale/mismatched nav bin,"
				  << " re-bake with tools/navmesh_baker --painted-city): " << navPath
				  << " tiles=" << tiles
				  << " orig=(" << params->orig[0] << "," << params->orig[1] << "," << params->orig[2] << ")"
				  << " tile=" << params->tileWidth << "x" << params->tileHeight
				  << " spawn=(" << spawn.x() << "," << spawn.y() << "," << spawn.z() << ")";
		return nullptr;
	}
	LOG_INFO << "nav loaded: " << navPath
			 << " tiles=" << tiles
			 << " orig=(" << params->orig[0] << "," << params->orig[1] << "," << params->orig[2] << ")"
			 << " tile=" << params->tileWidth << "x" << params->tileHeight
			 << " spawn=(" << spawn.x() << "," << spawn.y() << "," << spawn.z() << ")"
			 << " snapped=(" << snappedSpawn.x() << "," << snappedSpawn.y() << "," << snappedSpawn.z() << ")";
	return nav;
}

}  // namespace

void NavigationSystem::LoadNavBins()
{
	// 同一个 nav_bin_file 只加载/探针一次,多个场景配置 id 共享(21 行 → 3 个文件)。
	// 失败的文件也记下来,避免每行重复加载、重复刷 21 条同样的 ERROR。
	std::unordered_map<std::string, std::shared_ptr<NavComp>> byPath;

	auto &configAll = FindAllBaseSceneTable();
	for (auto &item : configAll.data())
	{
		if (item.nav_bin_file().empty())
		{
			continue;
		}
		const std::string navPath = GetDataRootDir() + item.nav_bin_file();
		auto it = byPath.find(navPath);
		if (it == byPath.end())
		{
			it = byPath.emplace(navPath, LoadAndProbe(navPath, item.id())).first;
		}
		if (!it->second)
		{
			continue;
		}
		LOG_INFO << "nav registered for scene " << item.id() << ": " << navPath;
		SceneNavManager::Instance().AddNav(item.id(), it->second);
	}
}
