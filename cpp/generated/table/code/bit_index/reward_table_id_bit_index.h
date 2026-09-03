
#pragma once
#include <cstdint>
#include <unordered_map>

const std::unordered_map<uint64_t, uint32_t> RewardBitMap{
    { 1, 0 },
    { 2, 1 },
    { 3, 2 },
    { 4, 3 },
    { 5, 4 },
    { 6, 5 },
    { 7, 6 },
};

constexpr uint32_t kRewardMaxBitIndex = 7;
