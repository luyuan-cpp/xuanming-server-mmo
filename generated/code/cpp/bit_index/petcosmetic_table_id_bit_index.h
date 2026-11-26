
#pragma once
#include <cstdint>
#include <unordered_map>

const std::unordered_map<uint64_t, uint32_t> PetCosmeticBitMap{
    { 1, 0 },
    { 2, 1 },
    { 3, 2 },
};

constexpr uint32_t kPetCosmeticBitmapCapacity = 3;
