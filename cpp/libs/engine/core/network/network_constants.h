#pragma once

#include <cstdint>

// 客户端单条请求内 repeated/map 字段的元素数量上限(防超大包放大 DoS)。
// 宝宝条目点全量目标等合法业务字段现为 21 项,保留 64 项上限兼顾 DoS 护栏与合法业务余量。
constexpr uint32_t kProtoFieldCheckerThreshold = 64;
