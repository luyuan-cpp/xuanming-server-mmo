#pragma once

#include <cstdint>

// 发号时的打散函数,与 go/shared/idsegment/mix.go 的 MixID 逐位相同,两边用同一组固定样例对拍
// (cpp/tests/bag_test/bag_test.cpp 的 MixGuidMatchesGoGolden、go/shared/idsegment/mix_test.go)。
// 设计见 docs/design/player-data-tidb-scaling-plan.md §6.1。
//
// 发号器发的是递增的计数器值,直接当 id 用的话,新号都挤在索引最右边一个 Region。这个函数把它们
// 打散到 63 位空间里。每一步都可逆,所以一一对应,打散不影响唯一性;0 只对应 0,而计数器恒大于 0。
//
// 改了它就等于改了发出去的 id,新号可能撞上已经发过的号。正式发号之后绝不能改。
constexpr uint64_t kGuidMixMask = (uint64_t{1} << 63) - 1;
constexpr uint64_t kGuidMixC1 = 0xbf58476d1ce4e5b9ULL;
constexpr uint64_t kGuidMixC2 = 0x94d049bb133111ebULL;

constexpr uint64_t MixGuid(uint64_t x)
{
    x &= kGuidMixMask;
    x ^= x >> 30;
    x = (x * kGuidMixC1) & kGuidMixMask;
    x ^= x >> 27;
    x = (x * kGuidMixC2) & kGuidMixMask;
    x ^= x >> 31;
    return x;
}
