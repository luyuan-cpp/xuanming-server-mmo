#include "navigation.h"

#include <memory>
#include <string>
#include <unordered_map>
#include <filesystem>
#include <fstream>
#include <sstream>
#include "core/utils/encode/sha256.h"

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

// 同一文件只加载与初始化一次;每个场景自己的出生点在注册前单独验证。
std::shared_ptr<NavComp> LoadNavFile(const std::string& navPath)
{
	const bool festival = navPath.find("/festival_maps/") != std::string::npos ||
		navPath.find("\\festival_maps\\") != std::string::npos;
	const bool authored = navPath.find("/authored_maps/") != std::string::npos ||
		navPath.find("\\authored_maps\\") != std::string::npos;
	if (authored && std::filesystem::exists(navPath + ".mask"))
	{ LOG_ERROR << "skip nav registration (authored nav has a residual legacy mask): " << navPath; return nullptr; }
	if (festival || authored)
	{
		std::ifstream checksums(navPath + ".sha256");
		std::string expectedNav, expectedMask, extra;
		if (!(checksums >> expectedNav >> expectedMask) || (checksums >> extra) ||
			expectedNav.size() != 64 || expectedMask.size() != 64)
		{ LOG_ERROR << "skip nav registration (missing checksums): " << navPath; return nullptr; }
		auto matches = [](const std::string& path, const std::string& expected) {
			std::ifstream input(path, std::ios::binary | std::ios::ate);
			if (!input || input.tellg() <= 0 || input.tellg() > 64 * 1024 * 1024) return false;
			input.seekg(0); std::ostringstream bytes; bytes << input.rdbuf();
			return Sha256::HashToHex(bytes.str()) == expected;
		};
		// 在 Detour 解析前核 SHA，损坏的长度/索引不能进入旧 ABI 原生装载器。
		if (!matches(navPath, expectedNav) || !matches(navPath + (authored ? ".authored" : ".mask"), expectedMask))
		{ LOG_ERROR << "skip nav registration (checksum mismatch): " << navPath; return nullptr; }
	}
	// 堆上就地构造:navQuery.init 会把 &navMesh 存进内部裸指针,
	// 对象必须先落在最终地址上再做加载/初始化(旧写法先在栈上装好
	// 再 emplace,浅拷贝进 map 后栈局部析构,map 里全是悬垂指针,
	// 线程退出时再 double free)。
	auto nav = std::make_shared<NavComp>();
	if (authored)
	{
		if (!nav->authoredBounds.Load(navPath + ".authored"))
		{ LOG_ERROR << "skip nav registration (invalid authored nav contract): " << navPath; return nullptr; }
		nav->authoredFlat = true;
	}
	if (!RecastSystem::LoadNavMesh(navPath.c_str(), &nav->navMesh))
	{
		// fail-closed:加载失败绝不注册空网格 —— 查询方拿到一个
		// "存在但零 tile"的导航,寻路会静默给出穿墙直线。
		LOG_ERROR << "skip nav registration (nav bin load failed): " << navPath;
		return nullptr;
	}
	if (authored)
	{
		const dtNavMesh& mesh = nav->navMesh;
		if (CountLoadedTiles(mesh) == 0)
		{ LOG_ERROR << "skip nav registration (empty authored nav): " << navPath; return nullptr; }
		// 手绘源合同要求原始水平面 Y=0；错误 profile、旧高度或损坏的有效数据
		// 不能等到角色移动时再通过吸附修正。粗网格与细节网格都须核对。
		for (int index = 0; index < mesh.getMaxTiles(); ++index)
		{
			const auto* tile = mesh.getTile(index);
			if (!tile || !tile->header) continue;
			auto onGround = [&nav](const dtReal* values, int count) {
				for (int vertex = 0; vertex < count; ++vertex)
					if (!std::isfinite(values[vertex * 3]) || !std::isfinite(values[vertex * 3 + 1]) ||
						!std::isfinite(values[vertex * 3 + 2]) ||
						std::abs(values[vertex * 3 + 1] - nav->authoredBounds.groundY) > 1e-6) return false;
				return true;
			};
			if (!onGround(tile->verts, tile->header->vertCount) ||
				!onGround(tile->detailVerts, tile->header->detailVertCount))
			{ LOG_ERROR << "skip nav registration (authored nav ground mismatch): " << navPath; return nullptr; }
		}
	}
	const dtStatus initStatus = nav->navQuery.init(&nav->navMesh, kMaxMeshQueryNodes);
	if (dtStatusFailed(initStatus))
	{
		LOG_ERROR << "skip nav registration (navQuery init failed): " << navPath;
		return nullptr;
	}
	if (!authored && (festival || std::filesystem::exists(navPath + ".mask")))
	{
		if (!nav->mask.Load(navPath + ".mask"))
		{
			LOG_ERROR << "skip nav registration (missing or invalid exact mask): " << navPath;
			return nullptr;
		}
		// 对真实运行端载入后的 Detour 逐格核对；损坏、错配伴随文件不能变成放行依据。
		const dtQueryFilter filter;
		const dtReal extents[3] = {2, 4, 2};
		for (int row = 0; row < nav->mask.rows; ++row)
			for (int col = 0; col < nav->mask.columns; ++col)
			{
				double x0, x1, z0, z1;
				const bool inside = nav->mask.CellBounds(col, row, x0, x1, z0, z1);
				const dtReal p[3] = {inside ? (x0+x1)/2 : nav->mask.minX + (col+0.5)*nav->mask.cellSize,
					0, inside ? (z0+z1)/2 : nav->mask.minZ + (nav->mask.rows-row-0.5)*nav->mask.cellSize};
				dtPolyRef ref = 0; dtReal nearest[3]{};
				const auto status = nav->navQuery.findNearestPoly(p, extents, &filter, &ref, nearest);
				const bool found = dtStatusSucceed(status) && ref != 0 &&
					std::abs(nearest[0]-p[0]) <= 0.02 && std::abs(nearest[1]) <= 0.02 && std::abs(nearest[2]-p[2]) <= 0.02;
				if (found != nav->mask.Cell(col,row))
				{ LOG_ERROR << "skip nav registration (exact mask mismatch): " << navPath; return nullptr; }
			}
		nav->strictMask = true;
	}

	return nav;
}

bool ProbeSceneSpawn(const std::shared_ptr<NavComp>& nav, const std::string& navPath,
	const uint32_t sceneConfigId)
{
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
		return false;
	}
	LOG_INFO << "nav loaded: " << navPath
			 << " tiles=" << tiles
			 << " orig=(" << params->orig[0] << "," << params->orig[1] << "," << params->orig[2] << ")"
			 << " tile=" << params->tileWidth << "x" << params->tileHeight
			 << " spawn=(" << spawn.x() << "," << spawn.y() << "," << spawn.z() << ")"
			 << " snapped=(" << snappedSpawn.x() << "," << snappedSpawn.y() << "," << snappedSpawn.z() << ")";
	return true;
}

}  // namespace

void NavigationSystem::LoadNavBins()
{
	// 同一个 nav_bin_file 只加载一次,每一行的出生点仍须分别验证。
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
			it = byPath.emplace(navPath, LoadNavFile(navPath)).first;
		}
		if (!it->second || !ProbeSceneSpawn(it->second, navPath, item.id()))
		{
			continue;
		}
		LOG_INFO << "nav registered for scene " << item.id() << ": " << navPath;
		SceneNavManager::Instance().AddNav(item.id(), it->second);
	}
}
