#pragma once

#include <cstddef>
#include <memory>

#include "modules/bag/admission_policy.h"
#include "modules/bag/bag_system.h"
#include "modules/bag/container_layout.h"
#include "modules/bag/eviction_policy.h"

// 先进先出淘汰的自由格包,只给用例用。
//
// 2026-11-05 起没有任何生产包配 EvictOldestFirst:临时格改为满即拒(BagProfile::Flat),因为被挤掉的东西
// 没有去处就是丢数据(审计 cpp-economy#2,docs/design/bag-rule-policy-layering.md §6.6)。淘汰轴本身
// (不动点规划、先判后毁、回执落流水、具名槽不淘汰)仍是 IEvictionPolicy 的契约 —— 这里装配一个会淘汰的包,
// 让那些机制用例继续钉住它。原先这些用例借用的是已删除的 BagProfile::Temporary 工厂。
inline BagProfile FifoEvictingProfileForTest(std::size_t capacity)
{
    return BagProfile{std::make_unique<FlatLayout>(capacity),
                      std::make_unique<AcceptAll>(),
                      std::make_unique<EvictOldestFirst>()};
}
