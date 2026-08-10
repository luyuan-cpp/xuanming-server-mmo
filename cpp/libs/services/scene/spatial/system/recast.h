#pragma once

#include "Detour/DetourNavMeshQuery.h"

class RecastSystem
{
public:
	// 返回 false 表示网格没有加载成功(文件缺失/头损坏/init 失败),
	// 调用方必须跳过该场景的导航注册,不能把空网格当有效导航用。
	static bool LoadNavMesh(const char* path, dtNavMesh* mesh);
};
