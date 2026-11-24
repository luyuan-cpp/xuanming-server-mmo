
#pragma once
#include <cstdint>
#include <unordered_map>

const std::unordered_map<uint64_t, uint32_t> TrialJournalSealBitMap{
    { 1011, 0 },
    { 1012, 1 },
    { 1013, 2 },
    { 1021, 3 },
    { 1022, 4 },
    { 1023, 5 },
    { 1031, 6 },
    { 1032, 7 },
    { 1033, 8 },
    { 1041, 9 },
    { 1042, 10 },
    { 1043, 11 },
};

constexpr uint32_t kTrialJournalSealBitmapCapacity = 12;
