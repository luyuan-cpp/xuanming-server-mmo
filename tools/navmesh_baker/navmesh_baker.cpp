// navmesh_baker: 离线烘焙场景导航网格(寻路阻挡数据)。
//
// 输入(二选一):
//   --painted-city <TianyongPaintedCity.cs>
//       直接解析客户端天墉城 painted-city 源文件里内嵌的 WalkMaskBase64
//       (150x150 可走位图,单格 2m,覆盖 Unity 世界 x∈[50,350] z∈[0,300]),
//       按"可走格出地面 quad、不可走格留洞"生成三角面。这份 mask 就是客户端
//       TianyongPaintedCity.IsPaintingWalkable 的判定数据,两端语义一比一。
//   --obj <mesh.obj>
//       通用 OBJ 三角网输入(未来真 3D 场景从 Unity 导出用这条路)。
//
// 输出:
//   --out <scene.bin>  'MSET' v1 格式 tile 集,与
//       cpp/libs/services/scene/spatial/system/recast.cpp 的 LoadNavMesh
//       逐字节兼容(同一套 ue5navmesh 头文件编译,dtReal=double 布局)。
//
// 坐标系约定(必须与客户端 WorldCoordinateConverter 一致):
//   导航网格烘焙在 Unity 坐标系(Y-up,米)。服务器坐标是 Z-up,
//   查询侧转换:nav = (server.y, server.z, server.x),
//               server = (nav.z, nav.x, nav.y)。
//   见 cpp/libs/services/scene/spatial/system/nav_query.h。
//
// 默认参数对齐客户端判定语义:
//   agent radius = 0 —— 客户端 TianyongPlayerController 只用脚底点查 mask,
//   不做半径收缩;服务器校验同一个点。
//   cs=0.25 且 mask 格 2m 为其整数倍,导航边界与 mask 格线对齐 —— 但注意
//   rcFilterLedgeSpans 会把紧邻"洞"(不可走格/画面边缘)的那一圈体素判成
//   悬崖(邻格没有 span → 视为无限下落)并剔除,所以**网格边界比 mask 格线
//   向内缩 1 个体素(0.25m)**。这一圈内缩是有意保留的:它保证服务器吸附出的
//   边界点永远落在客户端 mask 的可走格内(不会正好压在格线上被 floor 判到
//   墙那一侧),代价是服务器裁决的贴墙位置比客户端最多差 ~0.3m ——
//   scene 侧 kMoveCorrectionEpsilon(spatial/constants/nav.h)取 0.5m 就是
//   为了吞掉这个差值,改 cs 时要一起改。
//   地面几何放在 y = -ch:Recast 光栅化把恰好压在体素边界上的平面向上取整
//   一个体素,几何放在 y=0 时可走面会落在 +ch(0.2m);下移一个体素后可走面
//   回到 y≈0(探针输出 nearest 的 y 可以直接核对,偏 ±0.2 说明取整模型
//   和这里的假设不一致,改 kGroundY 即可,不影响水平语义)。
//
// 出生点探针(数据契约自检):
//   --probe x,y,z   Unity 坐标(米,Y-up),可重复。烘焙完成后对最终网格做
//       findNearestPoly,任一探针不在网格上则**不写文件**并以非零退出 ——
//       防止把和出生点契约不符的网格交给 scene 节点(scene 侧
//       NavigationSystem::LoadNavBins 也会用同一契约再探针一次)。
//   --painted-city 模式默认自带天墉城出生点探针 (200,0,180)
//       (客户端 TianyongMapDefinition.DefaultSpawn,服务器 (180,200,0)),
//       用 --no-default-probe 关闭。
//
// 构建/运行(由 Codex 执行,Windows MSVC):
//   cmake -S tools/navmesh_baker -B tools/navmesh_baker/build
//   cmake --build tools/navmesh_baker/build --config Release
//   tools/navmesh_baker/build/Release/navmesh_baker.exe ^
//       --painted-city ../mmorpg-client/Assets/Scripts/World/Tianyong/TianyongPaintedCity.cs ^
//       --out data/scene_nav_bin/tianyong_scene.bin

#include <cctype>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <sstream>
#include <string>
#include <vector>

#include "Recast/Recast.h"
#include "Detour/DetourNavMesh.h"
#include "Detour/DetourNavMeshBuilder.h"
#include "Detour/DetourNavMeshQuery.h"

namespace
{

// ---------------------------------------------------------------------------
// MSET v1 文件头 —— 与 recast.cpp 的 LoadNavMesh 完全相同的结构体定义,
// 同一套 ue5navmesh 头 + 同一 ABI 下 fwrite 整个结构体,布局天然一致。
// 改这里必须同步改 recast.cpp,反之亦然。
// ---------------------------------------------------------------------------
struct NavMeshSetHeader
{
	int32_t magic{0};
	int32_t version{0};
	int32_t numTiles{0};
	dtNavMeshParams params;
};

struct NavMeshTileHeader
{
	dtTileRef tileRef{0};
	int32_t dataSize{0};
};

constexpr int kNavMeshSetMagic = 'M' << 24 | 'S' << 16 | 'E' << 8 | 'T'; // 'MSET'
constexpr int kNavMeshSetVersion = 1;

// ---------------------------------------------------------------------------
// 天墉城 painted-city 常量 —— 与客户端源码保持一致:
//   TianyongMapDefinition: Width=400, Depth=300
//   TianyongPaintedCity: MaskResolution=150, PaintingWorldRect=(50,0,300,300)
// mask 索引 i = cy*150+cx,MSB first;cy 沿画面 y 轴向下(cy=0 在 z=300)。
// ---------------------------------------------------------------------------
constexpr int kMaskResolution = 150;
constexpr double kMaskCellSize = 2.0;
constexpr double kPaintingMinX = 50.0;
constexpr double kPaintingMaxZ = 300.0;

// 地面几何的 y(见文件头"地面几何放在 y = -ch"):必须等于 -BakeConfig::cellHeight。
constexpr double kGroundY = -0.2;

struct TriMesh
{
	std::vector<double> verts;  // x,y,z * nverts
	std::vector<int> tris;      // v0,v1,v2 * ntris
};

bool DecodeBase64(const std::string& in, std::vector<uint8_t>& out)
{
	auto value = [](char c) -> int {
		if (c >= 'A' && c <= 'Z') return c - 'A';
		if (c >= 'a' && c <= 'z') return c - 'a' + 26;
		if (c >= '0' && c <= '9') return c - '0' + 52;
		if (c == '+') return 62;
		if (c == '/') return 63;
		return -1;
	};
	uint32_t acc = 0;
	int bits = 0;
	for (char c : in)
	{
		if (c == '=' || c == '\r' || c == '\n') continue;
		const int v = value(c);
		if (v < 0) return false;
		acc = acc << 6 | static_cast<uint32_t>(v);
		bits += 6;
		if (bits >= 8)
		{
			bits -= 8;
			out.push_back(static_cast<uint8_t>(acc >> bits & 0xff));
		}
	}
	return true;
}

// 从 TianyongPaintedCity.cs 里抠出 WalkMaskBase64 常量:
// 定位标识符后,收集直到分号为止的所有双引号字符串片段并拼接。
bool ExtractWalkMask(const std::string& csPath, std::vector<uint8_t>& mask)
{
	std::ifstream file(csPath, std::ios::binary);
	if (!file)
	{
		std::fprintf(stderr, "error: cannot open %s\n", csPath.c_str());
		return false;
	}
	std::stringstream ss;
	ss << file.rdbuf();
	const std::string text = ss.str();

	// 必须锚在常量**定义**上:文件里第一次出现的 "WalkMaskBase64" 是 LoadMask()
	// 里的用法 `Convert.FromBase64String(WalkMaskBase64);`,从那里往后扫到分号
	// 一个字符串字面量都没有,base64 为空(2026-09-05 审查发现)。
	size_t anchor = text.find("const string WalkMaskBase64");
	if (anchor == std::string::npos)
	{
		anchor = text.find("WalkMaskBase64 =");
	}
	if (anchor == std::string::npos)
	{
		std::fprintf(stderr, "error: WalkMaskBase64 definition not found in %s\n", csPath.c_str());
		return false;
	}

	std::string base64;
	bool inString = false;
	for (size_t i = anchor; i < text.size(); ++i)
	{
		const char c = text[i];
		if (c == '"')
		{
			inString = !inString;
			continue;
		}
		if (inString)
		{
			base64.push_back(c);
		}
		else if (c == ';')
		{
			break;
		}
	}
	if (base64.empty())
	{
		std::fprintf(stderr,
			"error: WalkMaskBase64 definition found but no string literal follows it in %s\n",
			csPath.c_str());
		return false;
	}

	if (!DecodeBase64(base64, mask))
	{
		std::fprintf(stderr, "error: WalkMaskBase64 is not valid base64\n");
		return false;
	}
	const size_t expected = (kMaskResolution * kMaskResolution + 7) / 8;
	if (mask.size() < expected)
	{
		std::fprintf(stderr, "error: mask too short: %zu bytes, expected >= %zu\n",
			mask.size(), expected);
		return false;
	}
	return true;
}

inline bool MaskWalkable(const std::vector<uint8_t>& mask, int cx, int cy)
{
	const int i = cy * kMaskResolution + cx;
	return (mask[i >> 3] & 0x80 >> (i & 7)) != 0;
}

// 可走格 → 地面三角面。同一行的连续可走格合并成一个 quad 降三角数。
// 顶点绕序保证 recast 三角法线朝 +Y(cross(v1-v0, v2-v0) 的 y 分量为正),
// 这样 rcMarkWalkableTriangles 会把它们标成可走面。
TriMesh BuildMaskGeometry(const std::vector<uint8_t>& mask)
{
	TriMesh mesh;
	auto addQuad = [&mesh](double x0, double x1, double z0, double z1) {
		const int base = static_cast<int>(mesh.verts.size() / 3);
		const double quad[4][3] = {
			{x0, kGroundY, z0}, {x0, kGroundY, z1}, {x1, kGroundY, z1}, {x1, kGroundY, z0}};
		for (auto& v : quad)
		{
			mesh.verts.push_back(v[0]);
			mesh.verts.push_back(v[1]);
			mesh.verts.push_back(v[2]);
		}
		// (x0,z0)->(x0,z1)->(x1,z1) 与 (x0,z0)->(x1,z1)->(x1,z0),法线 +Y。
		const int tris[6] = {base, base + 1, base + 2, base, base + 2, base + 3};
		mesh.tris.insert(mesh.tris.end(), tris, tris + 6);
	};

	int quadCount = 0;
	for (int cy = 0; cy < kMaskResolution; ++cy)
	{
		// 画面 y 向下:cy 行覆盖 z∈[maxZ-(cy+1)*cell, maxZ-cy*cell]。
		const double z0 = kPaintingMaxZ - (cy + 1) * kMaskCellSize;
		const double z1 = kPaintingMaxZ - cy * kMaskCellSize;
		int runStart = -1;
		for (int cx = 0; cx <= kMaskResolution; ++cx)
		{
			const bool walkable = cx < kMaskResolution && MaskWalkable(mask, cx, cy);
			if (walkable && runStart < 0)
			{
				runStart = cx;
			}
			else if (!walkable && runStart >= 0)
			{
				const double x0 = kPaintingMinX + runStart * kMaskCellSize;
				const double x1 = kPaintingMinX + cx * kMaskCellSize;
				addQuad(x0, x1, z0, z1);
				++quadCount;
				runStart = -1;
			}
		}
	}
	std::printf("mask geometry: %d quads, %zu verts, %zu tris (ground y=%.2f)\n",
		quadCount, mesh.verts.size() / 3, mesh.tris.size() / 3, kGroundY);
	return mesh;
}

// 极简 OBJ 读取:只认 v / f,f 支持 "f a b c" 与 "f a/../.. b/../.. c/../..",
// 多边形面按扇形拆三角;索引支持负数(相对末尾)。
bool LoadObj(const std::string& path, TriMesh& mesh)
{
	std::ifstream file(path);
	if (!file)
	{
		std::fprintf(stderr, "error: cannot open %s\n", path.c_str());
		return false;
	}
	std::string line;
	while (std::getline(file, line))
	{
		std::istringstream ls(line);
		std::string tag;
		ls >> tag;
		if (tag == "v")
		{
			double x = 0, y = 0, z = 0;
			ls >> x >> y >> z;
			mesh.verts.push_back(x);
			mesh.verts.push_back(y);
			mesh.verts.push_back(z);
		}
		else if (tag == "f")
		{
			std::vector<int> face;
			std::string item;
			while (ls >> item)
			{
				const int raw = std::atoi(item.c_str());  // "12/3/4" -> 12
				if (raw == 0) continue;
				const int nverts = static_cast<int>(mesh.verts.size() / 3);
				face.push_back(raw > 0 ? raw - 1 : nverts + raw);
			}
			for (size_t i = 2; i < face.size(); ++i)
			{
				mesh.tris.push_back(face[0]);
				mesh.tris.push_back(face[i - 1]);
				mesh.tris.push_back(face[i]);
			}
		}
	}
	if (mesh.tris.empty())
	{
		std::fprintf(stderr, "error: no triangles in %s\n", path.c_str());
		return false;
	}
	std::printf("obj geometry: %zu verts, %zu tris\n",
		mesh.verts.size() / 3, mesh.tris.size() / 3);
	return true;
}

struct BakeConfig
{
	double cellSize = 0.25;       // cs:2m mask 格的整数分之一,边界precisely对齐
	double cellHeight = 0.2;      // ch
	double agentRadius = 0.0;     // 见文件头:0 = 与客户端点判定语义一致
	double agentHeight = 1.8;     // 客户端 TianyongMapConfig.playerHeight
	double agentMaxClimb = 0.35;  // 客户端 playerStepOffset
	double walkableSlopeDeg = 50.0;
	int tileSizeVx = 128;         // 128 * 0.25m = 32m/tile
};

class StdoutBuildContext final : public rcContext
{
protected:
	void doLog(const rcLogCategory category, const char* msg, const int len) override
	{
		std::printf("[rc:%d] %.*s\n", static_cast<int>(category), len, msg);
	}
};

// 单个 tile:沿用 RecastDemo Sample_TileMesh 的标准流程
// (rasterize → filter → compact → erode → region → contour → polymesh
//  → detailmesh → dtCreateNavMeshData)。
unsigned char* BuildTile(rcContext& ctx, const TriMesh& mesh, const BakeConfig& bake,
	int tx, int ty, const double* meshBMin, const double* meshBMax, int& outDataSize)
{
	outDataSize = 0;

	rcConfig cfg;
	std::memset(&cfg, 0, sizeof(cfg));
	cfg.cs = bake.cellSize;
	cfg.ch = bake.cellHeight;
	cfg.walkableSlopeAngle = bake.walkableSlopeDeg;
	cfg.walkableHeight = static_cast<int>(std::ceil(bake.agentHeight / cfg.ch));
	cfg.walkableClimb = static_cast<int>(std::floor(bake.agentMaxClimb / cfg.ch));
	cfg.walkableRadius = static_cast<int>(std::ceil(bake.agentRadius / cfg.cs));
	cfg.maxEdgeLen = static_cast<int>(12.0 / cfg.cs);
	cfg.maxSimplificationError = 1.3;
	cfg.minRegionArea = 8 * 8;
	cfg.mergeRegionArea = 20 * 20;
	cfg.regionPartitioning = RC_REGION_WATERSHED;
	cfg.regionChunkSize = bake.tileSizeVx / 2;
	cfg.maxVertsPerPoly = DT_VERTS_PER_POLYGON;
	cfg.tileSize = bake.tileSizeVx;
	cfg.borderSize = cfg.walkableRadius + 3;
	cfg.width = cfg.tileSize + cfg.borderSize * 2;
	cfg.height = cfg.tileSize + cfg.borderSize * 2;
	cfg.detailSampleDist = cfg.cs * 6.0;
	cfg.detailSampleMaxError = cfg.ch * 1.0;

	const double tileWorldSize = cfg.tileSize * cfg.cs;
	cfg.bmin[0] = meshBMin[0] + tx * tileWorldSize;
	cfg.bmin[1] = meshBMin[1];
	cfg.bmin[2] = meshBMin[2] + ty * tileWorldSize;
	cfg.bmax[0] = meshBMin[0] + (tx + 1) * tileWorldSize;
	cfg.bmax[1] = meshBMax[1];
	cfg.bmax[2] = meshBMin[2] + (ty + 1) * tileWorldSize;
	cfg.bmin[0] -= cfg.borderSize * cfg.cs;
	cfg.bmin[2] -= cfg.borderSize * cfg.cs;
	cfg.bmax[0] += cfg.borderSize * cfg.cs;
	cfg.bmax[2] += cfg.borderSize * cfg.cs;

	// 只喂与本 tile(含 border)相交的三角形。
	std::vector<int> tileTris;
	const int triCount = static_cast<int>(mesh.tris.size() / 3);
	for (int t = 0; t < triCount; ++t)
	{
		double triMin[2] = {1e300, 1e300};
		double triMax[2] = {-1e300, -1e300};
		for (int k = 0; k < 3; ++k)
		{
			const double* v = &mesh.verts[mesh.tris[t * 3 + k] * 3];
			triMin[0] = std::fmin(triMin[0], v[0]);
			triMin[1] = std::fmin(triMin[1], v[2]);
			triMax[0] = std::fmax(triMax[0], v[0]);
			triMax[1] = std::fmax(triMax[1], v[2]);
		}
		if (triMax[0] < cfg.bmin[0] || triMin[0] > cfg.bmax[0] ||
			triMax[1] < cfg.bmin[2] || triMin[1] > cfg.bmax[2])
		{
			continue;
		}
		tileTris.push_back(mesh.tris[t * 3]);
		tileTris.push_back(mesh.tris[t * 3 + 1]);
		tileTris.push_back(mesh.tris[t * 3 + 2]);
	}
	if (tileTris.empty())
	{
		return nullptr;
	}
	const int tileTriCount = static_cast<int>(tileTris.size() / 3);
	const int vertCount = static_cast<int>(mesh.verts.size() / 3);

	rcHeightfield* solid = rcAllocHeightfield();
	if (!solid || !rcCreateHeightfield(&ctx, *solid, cfg.width, cfg.height,
			cfg.bmin, cfg.bmax, cfg.cs, cfg.ch))
	{
		std::fprintf(stderr, "tile(%d,%d): rcCreateHeightfield failed\n", tx, ty);
		rcFreeHeightField(solid);
		return nullptr;
	}

	std::vector<unsigned char> triAreas(tileTriCount, 0);
	rcMarkWalkableTriangles(&ctx, cfg.walkableSlopeAngle, mesh.verts.data(), vertCount,
		tileTris.data(), tileTriCount, triAreas.data());
	rcRasterizeTriangles(&ctx, mesh.verts.data(), vertCount, tileTris.data(),
		triAreas.data(), tileTriCount, *solid, cfg.walkableClimb);

	rcFilterLowHangingWalkableObstacles(&ctx, cfg.walkableClimb, *solid);
	rcFilterLedgeSpans(&ctx, cfg.walkableHeight, cfg.walkableClimb, *solid);
	rcFilterWalkableLowHeightSpans(&ctx, cfg.walkableHeight, *solid);

	rcCompactHeightfield* chf = rcAllocCompactHeightfield();
	if (!chf || !rcBuildCompactHeightfield(&ctx, cfg.walkableHeight, cfg.walkableClimb,
			*solid, *chf))
	{
		std::fprintf(stderr, "tile(%d,%d): rcBuildCompactHeightfield failed\n", tx, ty);
		rcFreeCompactHeightfield(chf);
		rcFreeHeightField(solid);
		return nullptr;
	}
	rcFreeHeightField(solid);
	solid = nullptr;

	if (cfg.walkableRadius > 0 && !rcErodeWalkableArea(&ctx, cfg.walkableRadius, *chf))
	{
		std::fprintf(stderr, "tile(%d,%d): rcErodeWalkableArea failed\n", tx, ty);
		rcFreeCompactHeightfield(chf);
		return nullptr;
	}

	if (!rcBuildDistanceField(&ctx, *chf) ||
		!rcBuildRegions(&ctx, *chf, cfg.borderSize, cfg.minRegionArea, cfg.mergeRegionArea))
	{
		std::fprintf(stderr, "tile(%d,%d): region build failed\n", tx, ty);
		rcFreeCompactHeightfield(chf);
		return nullptr;
	}

	rcContourSet* cset = rcAllocContourSet();
	if (!cset || !rcBuildContours(&ctx, *chf, cfg.maxSimplificationError, cfg.maxEdgeLen, *cset))
	{
		std::fprintf(stderr, "tile(%d,%d): rcBuildContours failed\n", tx, ty);
		rcFreeContourSet(cset);
		rcFreeCompactHeightfield(chf);
		return nullptr;
	}
	if (cset->nconts == 0)
	{
		rcFreeContourSet(cset);
		rcFreeCompactHeightfield(chf);
		return nullptr;
	}

	rcPolyMesh* pmesh = rcAllocPolyMesh();
	if (!pmesh || !rcBuildPolyMesh(&ctx, *cset, cfg.maxVertsPerPoly, *pmesh))
	{
		std::fprintf(stderr, "tile(%d,%d): rcBuildPolyMesh failed\n", tx, ty);
		rcFreePolyMesh(pmesh);
		rcFreeContourSet(cset);
		rcFreeCompactHeightfield(chf);
		return nullptr;
	}

	rcPolyMeshDetail* dmesh = rcAllocPolyMeshDetail();
	if (!dmesh || !rcBuildPolyMeshDetail(&ctx, *pmesh, *chf, cfg.detailSampleDist,
			cfg.detailSampleMaxError, *dmesh))
	{
		std::fprintf(stderr, "tile(%d,%d): rcBuildPolyMeshDetail failed\n", tx, ty);
		rcFreePolyMeshDetail(dmesh);
		rcFreePolyMesh(pmesh);
		rcFreeContourSet(cset);
		rcFreeCompactHeightfield(chf);
		return nullptr;
	}
	rcFreeCompactHeightfield(chf);
	rcFreeContourSet(cset);

	unsigned char* navData = nullptr;
	int navDataSize = 0;
	if (pmesh->npolys > 0)
	{
		// dtQueryFilter 默认 includeFlags=0xffff 且要求 poly->flags 非零,
		// 全部置 1(单一"可走"类型;将来分水面/草地再拆位)。
		for (int i = 0; i < pmesh->npolys; ++i)
		{
			pmesh->flags[i] = 1;
		}

		dtNavMeshCreateParams params;
		std::memset(&params, 0, sizeof(params));
		params.verts = pmesh->verts;
		params.vertCount = pmesh->nverts;
		params.polys = pmesh->polys;
		params.polyAreas = pmesh->areas;
		params.polyFlags = pmesh->flags;
		params.polyCount = pmesh->npolys;
		params.nvp = pmesh->nvp;
		params.detailMeshes = dmesh->meshes;
		params.detailVerts = dmesh->verts;
		params.detailVertsCount = dmesh->nverts;
		params.detailTris = dmesh->tris;
		params.detailTriCount = dmesh->ntris;
		params.walkableHeight = bake.agentHeight;
		params.walkableRadius = bake.agentRadius;
		params.walkableClimb = bake.agentMaxClimb;
		params.tileX = tx;
		params.tileY = ty;
		rcVcopy(params.bmin, pmesh->bmin);
		rcVcopy(params.bmax, pmesh->bmax);
		params.cs = cfg.cs;
		params.ch = cfg.ch;
		params.buildBvTree = true;

		if (!dtCreateNavMeshData(&params, &navData, &navDataSize))
		{
			std::fprintf(stderr, "tile(%d,%d): dtCreateNavMeshData failed\n", tx, ty);
			navData = nullptr;
			navDataSize = 0;
		}
	}
	rcFreePolyMeshDetail(dmesh);
	rcFreePolyMesh(pmesh);

	outDataSize = navDataSize;
	return navData;
}

bool SaveNavMesh(const std::string& path, const dtNavMesh& navMesh)
{
	std::FILE* fp = std::fopen(path.c_str(), "wb");
	if (!fp)
	{
		std::fprintf(stderr, "error: cannot write %s\n", path.c_str());
		return false;
	}

	// 整个结构体 fwrite:先清零,让对齐填充字节确定,同一输入产出逐字节相同的
	// bin(否则 git 里每次重烘都是"变了"的二进制)。
	NavMeshSetHeader header;
	std::memset(&header, 0, sizeof(header));
	header.magic = kNavMeshSetMagic;
	header.version = kNavMeshSetVersion;
	header.numTiles = 0;
	for (int i = 0; i < navMesh.getMaxTiles(); ++i)
	{
		const dtMeshTile* tile = navMesh.getTile(i);
		if (tile && tile->header && tile->dataSize > 0)
		{
			++header.numTiles;
		}
	}
	header.params = *navMesh.getParams();
	std::fwrite(&header, sizeof(header), 1, fp);

	for (int i = 0; i < navMesh.getMaxTiles(); ++i)
	{
		const dtMeshTile* tile = navMesh.getTile(i);
		if (!tile || !tile->header || tile->dataSize <= 0) continue;

		NavMeshTileHeader tileHeader;
		std::memset(&tileHeader, 0, sizeof(tileHeader));
		tileHeader.tileRef = navMesh.getTileRef(tile);
		tileHeader.dataSize = tile->dataSize;
		std::fwrite(&tileHeader, sizeof(tileHeader), 1, fp);
		std::fwrite(tile->data, tile->dataSize, 1, fp);
	}
	std::fclose(fp);
	std::printf("saved %s: %d tiles\n", path.c_str(), header.numTiles);
	return true;
}

int NextPow2(int v)
{
	int p = 1;
	while (p < v) p <<= 1;
	return p;
}

struct ProbePoint
{
	double v[3];  // Unity 坐标 (x, y, z)
	std::string label;
};

// "x,y,z" → ProbePoint。
bool ParseProbe(const std::string& text, ProbePoint& out)
{
	double x = 0, y = 0, z = 0;
	if (std::sscanf(text.c_str(), "%lf,%lf,%lf", &x, &y, &z) != 3)
	{
		return false;
	}
	out.v[0] = x;
	out.v[1] = y;
	out.v[2] = z;
	out.label = text;
	return true;
}

// 对最终网格做探针:每个点 findNearestPoly,搜索范围与 scene 侧
// nav_query.cpp 的 kSnapExtents 完全一致(水平 ±2m、垂直 ±4m),
// 保证"烘焙器说在网格上" ⇔ "scene 节点说在网格上"。
bool ProbeNavMesh(const dtNavMesh& navMesh, const std::vector<ProbePoint>& probes)
{
	if (probes.empty()) return true;

	dtNavMeshQuery query;
	if (dtStatusFailed(query.init(&navMesh, 2048)))
	{
		std::fprintf(stderr, "error: dtNavMeshQuery::init failed for probing\n");
		return false;
	}
	const dtReal extents[3] = {2.0, 4.0, 2.0};
	const dtQueryFilter filter;
	bool allOk = true;
	for (const ProbePoint& p : probes)
	{
		const dtReal center[3] = {p.v[0], p.v[1], p.v[2]};
		dtPolyRef ref = 0;
		dtReal nearest[3] = {0, 0, 0};
		const dtStatus status = query.findNearestPoly(center, extents, &filter, &ref, nearest);
		const bool ok = dtStatusSucceed(status) && ref != 0;
		std::printf("probe unity=(%.2f,%.2f,%.2f) server=(%.2f,%.2f,%.2f): %s",
			p.v[0], p.v[1], p.v[2], p.v[2], p.v[0], p.v[1], ok ? "ON MESH" : "OFF MESH");
		if (ok)
		{
			std::printf(" nearest=(%.2f,%.2f,%.2f) poly=%llu", nearest[0], nearest[1], nearest[2],
				static_cast<unsigned long long>(ref));
		}
		std::printf("\n");
		allOk = allOk && ok;
	}
	return allOk;
}

}  // namespace

int main(int argc, char** argv)
{
	std::string paintedCityPath;
	std::string objPath;
	std::string outPath;
	BakeConfig bake;
	std::vector<ProbePoint> probes;
	bool defaultProbe = true;

	for (int i = 1; i < argc; ++i)
	{
		const std::string arg = argv[i];
		auto next = [&]() -> const char* { return i + 1 < argc ? argv[++i] : ""; };
		if (arg == "--painted-city") paintedCityPath = next();
		else if (arg == "--obj") objPath = next();
		else if (arg == "--out") outPath = next();
		else if (arg == "--cs") bake.cellSize = std::atof(next());
		else if (arg == "--ch") bake.cellHeight = std::atof(next());
		else if (arg == "--agent-radius") bake.agentRadius = std::atof(next());
		else if (arg == "--agent-height") bake.agentHeight = std::atof(next());
		else if (arg == "--agent-climb") bake.agentMaxClimb = std::atof(next());
		else if (arg == "--tile-size") bake.tileSizeVx = std::atoi(next());
		else if (arg == "--probe")
		{
			ProbePoint p;
			const std::string text = next();
			if (!ParseProbe(text, p))
			{
				std::fprintf(stderr, "bad --probe value (want x,y,z): %s\n", text.c_str());
				return 1;
			}
			probes.push_back(p);
		}
		else if (arg == "--no-default-probe") defaultProbe = false;
		else
		{
			std::fprintf(stderr, "unknown arg: %s\n", arg.c_str());
			return 1;
		}
	}
	if (outPath.empty() || (paintedCityPath.empty() == objPath.empty()))
	{
		std::fprintf(stderr,
			"usage: navmesh_baker (--painted-city <TianyongPaintedCity.cs> | --obj <mesh.obj>)"
			" --out <scene.bin>\n"
			"       [--cs 0.25] [--ch 0.2] [--agent-radius 0] [--agent-height 1.8]"
			" [--agent-climb 0.35] [--tile-size 128]\n"
			"       [--probe x,y,z ...] [--no-default-probe]\n");
		return 1;
	}
	if (!paintedCityPath.empty() && defaultProbe)
	{
		// 天墉城出生点契约:客户端 TianyongMapDefinition.DefaultSpawn (200,0,180),
		// 服务器 spatial/constants/nav.h kTianyongSpawn* = (180,200,0)。
		ProbePoint spawn;
		ParseProbe("200,0,180", spawn);
		spawn.label = "tianyong-default-spawn";
		probes.push_back(spawn);
	}

	TriMesh mesh;
	if (!paintedCityPath.empty())
	{
		std::vector<uint8_t> mask;
		if (!ExtractWalkMask(paintedCityPath, mask)) return 1;
		mesh = BuildMaskGeometry(mask);
	}
	else if (!LoadObj(objPath, mesh))
	{
		return 1;
	}
	if (mesh.tris.empty())
	{
		// 全零 mask / 错误文件:不能让 bounds 停在 ±1e300 把 tile 数算成负数。
		std::fprintf(stderr, "error: input has no walkable geometry, nothing to bake\n");
		return 1;
	}

	double bmin[3] = {1e300, 1e300, 1e300};
	double bmax[3] = {-1e300, -1e300, -1e300};
	const size_t vertCount = mesh.verts.size() / 3;
	for (size_t v = 0; v < vertCount; ++v)
	{
		for (int k = 0; k < 3; ++k)
		{
			bmin[k] = std::fmin(bmin[k], mesh.verts[v * 3 + k]);
			bmax[k] = std::fmax(bmax[k], mesh.verts[v * 3 + k]);
		}
	}
	// 平面几何 y 方向零厚度,给高度场留出余量。
	bmin[1] -= bake.cellHeight;
	bmax[1] += bake.agentHeight + bake.cellHeight;

	const double tileWorldSize = bake.tileSizeVx * bake.cellSize;
	const int tileCountX = static_cast<int>(std::ceil((bmax[0] - bmin[0]) / tileWorldSize));
	const int tileCountY = static_cast<int>(std::ceil((bmax[2] - bmin[2]) / tileWorldSize));
	std::printf("bounds: (%.2f %.2f %.2f)-(%.2f %.2f %.2f), tiles %dx%d\n",
		bmin[0], bmin[1], bmin[2], bmax[0], bmax[1], bmax[2], tileCountX, tileCountY);

	dtNavMeshParams nmParams;
	std::memset(&nmParams, 0, sizeof(nmParams));
	rcVcopy(nmParams.orig, bmin);
	nmParams.tileWidth = tileWorldSize;
	nmParams.tileHeight = tileWorldSize;
	nmParams.maxTiles = NextPow2(tileCountX * tileCountY);
	nmParams.maxPolys = 1 << 20;
	// ue5navmesh 的 dtNavMeshParams 扩展字段(内存优化把这些从 tile header
	// 挪进了 mesh params):bvQuantFactor = 1/cs,与 dtCreateNavMeshData
	// 写进 tile BV 树的量化一致,不填对 BV 查询直接失灵。
	nmParams.walkableHeight = bake.agentHeight;
	nmParams.walkableRadius = bake.agentRadius;
	nmParams.walkableClimb = bake.agentMaxClimb;
	nmParams.bvQuantFactor = 1.0 / bake.cellSize;

	dtNavMesh navMesh;
	if (dtStatusFailed(navMesh.init(&nmParams)))
	{
		std::fprintf(stderr, "error: dtNavMesh::init failed\n");
		return 1;
	}

	StdoutBuildContext ctx;
	ctx.enableLog(true);
	int builtTiles = 0;
	for (int ty = 0; ty < tileCountY; ++ty)
	{
		for (int tx = 0; tx < tileCountX; ++tx)
		{
			int dataSize = 0;
			unsigned char* data = BuildTile(ctx, mesh, bake, tx, ty, bmin, bmax, dataSize);
			if (!data) continue;
			const dtStatus status =
				navMesh.addTile(data, dataSize, DT_TILE_FREE_DATA, 0, nullptr);
			if (dtStatusFailed(status))
			{
				std::fprintf(stderr, "tile(%d,%d): addTile failed\n", tx, ty);
				dtFree(data, DT_ALLOC_PERM_TILE_DATA);
				continue;
			}
			++builtTiles;
		}
	}
	if (builtTiles == 0)
	{
		std::fprintf(stderr, "error: no tiles built, refusing to write empty navmesh\n");
		return 1;
	}
	std::printf("built %d tiles\n", builtTiles);

	if (!ProbeNavMesh(navMesh, probes))
	{
		std::fprintf(stderr,
			"error: probe point(s) off the navmesh, refusing to write %s"
			" (spawn contract broken — check mask/coordinate mapping)\n", outPath.c_str());
		return 1;
	}

	return SaveNavMesh(outPath, navMesh) ? 0 : 1;
}
