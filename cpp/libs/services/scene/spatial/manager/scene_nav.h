#pragma once

#include <memory>
#include <unordered_map>

#include "spatial/comp/nav_comp.h"

// 经智能指针间接持有:NavComp 不可拷贝/移动(见 nav_comp.h),
// 且 navQuery 内部存着 &navMesh,定址后不能搬家 —— 堆上定址一次到位。
// shared_ptr 而非 unique_ptr:多个场景配置 id 指向同一个 nav_bin_file 时
// (当前 21 行只有 3 个文件,且三份还是同一次烘焙的拷贝)共享同一份网格,
// 不必每行各加载一份。dtNavMeshQuery 只读查询,多 id 共用一个实例是安全的
// (同线程内串行使用;管理器本身就是 thread_local)。
using SceneNavMapComp = std::unordered_map<uint32_t, std::shared_ptr<NavComp>>;

class SceneNavManager
{
public:
    SceneNavManager() = default;

    SceneNavManager(const SceneNavManager&) = delete;
    SceneNavManager& operator=(const SceneNavManager&) = delete;

    static SceneNavManager& Instance() {
        thread_local SceneNavManager instance;
        return instance;
    }

    void AddNav(uint32_t id, std::shared_ptr<NavComp> nav) { sceneNav.emplace(id, std::move(nav)); }

    bool Contains(uint32_t id) { return sceneNav.contains(id); }

    // 场景配置 id → 导航;未烘焙/未注册返回 nullptr(LoadNavBins fail-closed,
    // 加载失败的场景不会出现在表里)。指针生命周期 = 本线程整个进程期,
    // NavComp 不可搬家(见 nav_comp.h),裸指针可以安全短持。
    NavComp* Get(uint32_t id)
    {
        const auto it = sceneNav.find(id);
        return it == sceneNav.end() ? nullptr : it->second.get();
    }

	SceneNavMapComp sceneNav;
};
