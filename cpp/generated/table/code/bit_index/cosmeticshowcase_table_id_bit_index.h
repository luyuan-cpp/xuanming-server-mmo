
#pragma once
#include <cstdint>
#include <unordered_map>

const std::unordered_map<uint64_t, uint32_t> CosmeticShowcaseBitMap{
    { 1, 0 },
    { 2, 1 },
    { 3, 2 },
    { 4, 3 },
    { 5, 4 },
    { 6, 5 },
    { 7, 6 },
    { 8, 7 },
    { 9, 8 },
    { 10, 9 },
    { 11, 10 },
    { 12, 11 },
    { 13, 12 },
};

constexpr uint32_t kCosmeticShowcaseBitmapCapacity = 13;
