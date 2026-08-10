#pragma once

#include "Detour/DetourNavMeshQuery.h"

// 场景导航网格 + 查询器。
//
// dtNavMesh / dtNavMeshQuery 都是"用户声明析构 + 无 move + 未删除 copy"的
// vendored 类型,内部持有 m_tiles / m_posLookup / 节点池等裸指针:隐式拷贝
// 是逐成员浅拷贝,两份对象共享同一批堆块,谁先析构谁把对方变成悬垂,
// 第二次析构就是 double free(此前 LoadNavBins 的栈局部 + map emplace
// 正是这样炸的)。显式删除拷贝/移动,强制经堆间接持有(unique_ptr)。
// 另外 navQuery.init 存的是 &navMesh 裸指针,对象定址后不能再搬家。
struct NavComp
{
    NavComp() = default;
    NavComp(const NavComp&) = delete;
    NavComp& operator=(const NavComp&) = delete;
    NavComp(NavComp&&) = delete;
    NavComp& operator=(NavComp&&) = delete;

    dtNavMesh navMesh;
    dtNavMeshQuery navQuery;
};
