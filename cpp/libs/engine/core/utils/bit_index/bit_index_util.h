#pragma once

#include <bitset>
#include <cstddef>
#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <unordered_map>

// Bit-index map type shared by all generated *_table_id_bit_index.h headers.
using BitIndexMap = std::unordered_map<uint64_t, uint32_t>;

// Look up the bit position for |id| in |bitMap|.
// Returns std::nullopt when the id has no mapping (table was changed / id invalid).
inline std::optional<uint32_t> GetBitIndex(const BitIndexMap& bitMap, uint64_t id)
{
    if (const auto it = bitMap.find(id); it != bitMap.end())
    {
        return it->second;
    }
    return std::nullopt;
}

// Test whether |id| is marked in |bits| according to |bitMap|.
// Returns false when id is not present in bitMap.
template <std::size_t N>
bool TestBit(const BitIndexMap& bitMap, const std::bitset<N>& bits, uint64_t id)
{
    const auto idx = GetBitIndex(bitMap, id);
    return idx.has_value() && bits.test(*idx);
}

// Set / clear the bit for |id| in |bits| according to |bitMap|.
// Returns false (no-op) when id is not present in bitMap.
template <std::size_t N>
bool SetBit(const BitIndexMap& bitMap, std::bitset<N>& bits, uint64_t id, bool value = true)
{
    const auto idx = GetBitIndex(bitMap, id);
    if (!idx.has_value())
    {
        return false;
    }
    bits.set(*idx, value);
    return true;
}

// 存档里的位图(proto bytes 字段):第 i 位在第 i / 8 个字节的第 i % 8 位(低位在前)。
// 写入只在置位时加长,所以只要从空串或同样写出的串开始,末尾就不会有全 0 的字节。
inline bool TestPackedBit(std::string_view bits, std::size_t index)
{
    return index / 8 < bits.size() && ((static_cast<unsigned char>(bits[index / 8]) >> (index % 8)) & 1u) != 0;
}

inline void SetPackedBit(std::string& bits, std::size_t index)
{
    if (bits.size() <= index / 8) bits.resize(index / 8 + 1, '\0');
    bits[index / 8] = static_cast<char>(static_cast<unsigned char>(bits[index / 8]) | (1u << (index % 8)));
}

// Reward-decision result for a completed entry.
enum class RewardAction : uint8_t
{
    kNone,       // no reward configured
    kAutoGrant,  // reward should be granted immediately
    kClaimable,  // reward exists but player must claim manually
};

// Determine what reward action to take for |id|.
// Config must provide: uint32_t GetRewardId(uint32_t) const;
//                      bool     AutoReward(uint32_t)  const;
// No inheritance required — any Config type with these two methods works.
template <typename Config>
RewardAction GetRewardAction(const Config& config, uint32_t id)
{
    if (config.GetRewardId(id) <= 0)
    {
        return RewardAction::kNone;
    }
    return config.AutoReward(id) ? RewardAction::kAutoGrant
                                 : RewardAction::kClaimable;
}
