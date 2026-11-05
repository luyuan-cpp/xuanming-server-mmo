#pragma once

#include <string>

#include "proto/common/base/config.pb.h"

namespace node_log_level
{
// 启动阶段专用：同步写出拒启原因并以非零状态退出。
[[noreturn]] void RejectStartup(const std::string &reason);
} // namespace node_log_level

bool readBaseDeployConfig(const std::string &filename, BaseDeployConfig &baseConfig);

bool readGameConfig(const std::string &filename, GameConfig &gameConfig);

std::string GetConfigDir();
std::string GetDataRootDir();
