
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
    { 105, 10 },
    { 401, 11 },
    { 402, 12 },
    { 3021, 13 },
    { 3022, 14 },
    { 3031, 15 },
    { 3032, 16 },
    { 3041, 17 },
    { 3042, 18 },
    { 3051, 19 },
    { 3052, 20 },
    { 3061, 21 },
    { 3062, 22 },
    { 3071, 23 },
    { 3072, 24 },
    { 3081, 25 },
    { 3082, 26 },
    { 3091, 27 },
    { 3092, 28 },
    { 3101, 29 },
    { 3102, 30 },
};

constexpr uint32_t kTrialJournalMilestoneBitmapCapacity = 31;
