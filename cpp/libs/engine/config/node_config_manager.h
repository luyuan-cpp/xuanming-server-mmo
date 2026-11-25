#pragma once

#include "config.h"

class NodeConfigManager {

public:
	NodeConfigManager() = default;

	[[nodiscard]] ::BaseDeployConfig& GetBaseDeployConfig()
	{
		return baseDeployConfig;
	}

	[[nodiscard]] ::GameConfig& GetGameConfig()
	{
		return gameConfig;
	}

private:
	::BaseDeployConfig baseDeployConfig;
	::GameConfig gameConfig;
};

extern thread_local NodeConfigManager tlsNodeConfigManager;
extern NodeConfigManager gNodeConfigManager;

