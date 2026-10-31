#pragma once

#include <cstddef>

// 玩家存盘直接提交 TiDB(docs/design/player-save-direct-commit.md)的共用常量。
namespace player_store_path
{

// 认领响应的上限,与 go/db 的 playerstore.MaxClaimResponseBytes 相同。响应是一个玩家的全部行,可以超过默认的 4 MiB,
// 连 go/db 的通道要放大接收上限;收不下会按结果未知重试,这个角色就再也进不来了。
inline constexpr std::size_t kMaxClaimResponseBytes = 8 * 1024 * 1024;

} // namespace player_store_path
