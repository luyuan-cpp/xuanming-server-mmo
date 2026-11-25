
#pragma once
#include <cstdint>
#include <unordered_map>

const std::unordered_map<uint64_t, uint32_t> TrialJournalMilestoneBitMap{
    { 101, 0 },
    { 102, 1 },
    { 103, 2 },
    { 104, 3 },
    { 201, 4 },
    { 202, 5 },
    { 203, 6 },
    { 901, 7 },
    { 301, 8 },
    { 302, 9 },
};

constexpr uint32_t kTrialJournalMilestoneBitmapCapacity = 10;
