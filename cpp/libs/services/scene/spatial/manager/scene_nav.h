#pragma once

#include <memory>
#include <unordered_map>

#include "spatial/comp/nav_comp.h"

// 经 unique_ptr 间接持有:NavComp 不可拷贝/移动(见 nav_comp.h),
// 且 navQuery 内部存着 &navMesh,定址后不能搬家 —— 堆上定址一次到位。
using SceneNavMapComp = std::unordered_map<uint32_t, std::unique_ptr<NavComp>>;

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

    void AddNav(uint32_t id, std::unique_ptr<NavComp> nav) { sceneNav.emplace(id, std::move(nav)); }

    bool Contains(uint32_t id) { return sceneNav.contains(id); }

	SceneNavMapComp sceneNav;
};
