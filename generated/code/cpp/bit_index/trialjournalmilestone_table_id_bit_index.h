
#pragma once
#include <cstdint>
#include <unordered_map>

const std::unordered_map<uint64_t, uint32_t> TrialJournalMilestoneBitMap{
    { 101, 0 },
    { 102, 1 },
    { 103, 2 },
    { 104, 3 },
};

constexpr uint32_t kTrialJournalMilestoneBitmapCapacity = 4;
