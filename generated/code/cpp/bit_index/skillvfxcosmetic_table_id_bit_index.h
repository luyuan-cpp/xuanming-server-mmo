
#pragma once
#include <cstdint>
#include <unordered_map>

const std::unordered_map<uint64_t, uint32_t> SkillVfxCosmeticBitMap{
    { 1, 0 },
    { 2, 1 },
    { 3, 2 },
};

constexpr uint32_t kSkillVfxCosmeticBitmapCapacity = 3;
