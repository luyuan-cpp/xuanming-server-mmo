#include "config.h"
#include <yaml-cpp/yaml.h>
#include <muduo/base/Logging.h>
#include <node_config_manager.h>
#include <cstdlib>
#include <cstring>

namespace
{
// Returns a non-empty environment value, or nullptr when the variable is
// unset or empty. Used to let K8s / container env override selected fields
// from the baseline YAML files without shipping per-pod config files.
const char *GetNonEmptyEnv(const char *name)
{
    const char *value = std::getenv(name);
    if (value == nullptr || value[0] == '\0')
    {
        return nullptr;
    }
    return value;
}
} // namespace

bool readBaseDeployConfig(const std::string &filename, BaseDeployConfig &baseConfig)
{
	YAML::Node root = YAML::LoadFile(filename);

	// Parse Etcd config
	if (root["Etcd"]["Hosts"])
	{
		for (const auto &host : root["Etcd"]["Hosts"])
		{
			baseConfig.add_etcd_hosts(host.as<std::string>());
		}
	}
	if (root["Etcd"]["KeepaliveInterval"])
	{
		baseConfig.set_keep_alive_interval(root["Etcd"]["KeepaliveInterval"].as<uint32_t>());
	}
	if (root["Etcd"]["NodeTTLSeconds"])
	{
		baseConfig.set_node_ttl_seconds(root["Etcd"]["NodeTTLSeconds"].as<uint32_t>());
	}
	if (root["Etcd"]["NodeRemovalGraceSeconds"])
	{
		baseConfig.set_node_removal_grace_seconds(root["Etcd"]["NodeRemovalGraceSeconds"].as<uint32_t>());
	}

	if (root["LogLevel"])
	{
		baseConfig.set_log_level(root["LogLevel"].as<uint32_t>());
	}

	if (root["HealthCheckInterval"])
	{
		baseConfig.set_health_check_interval(root["HealthCheckInterval"].as<uint32_t>());
	}

	if (root["services"])
	{
		for (const auto &service : root["services"])
		{
			auto &s = *baseConfig.add_services();
			s.set_name(service["name"].as<std::string>());
			s.set_url(service["url"].as<std::string>());
		}
	}

	if (root["service_discovery_prefixes"])
	{
		for (const auto &prefix : root["service_discovery_prefixes"])
		{
			baseConfig.add_service_discovery_prefixes(prefix.as<std::string>());
		}
	}

	// deployservice_prefix
	if (root["deployservice_prefix"])
	{
		baseConfig.set_deployservice_prefix(root["deployservice_prefix"].as<std::string>());
	}

	if (root["TableDataDirectory"])
	{
		baseConfig.set_table_data_directory(root["TableDataDirectory"].as<std::string>());
	}

	if (root["TableDataFormat"])
	{
		baseConfig.set_table_data_format(root["TableDataFormat"].as<std::string>());
	}

	if (root["DataRootDirectory"])
	{
		baseConfig.set_data_root_directory(root["DataRootDirectory"].as<std::string>());
	}

	if (root["GateTokenSecret"])
	{
		baseConfig.set_gate_token_secret(root["GateTokenSecret"].as<std::string>());
	}

	if (root["GateMaxConnections"])
	{
		baseConfig.set_gate_max_connections(root["GateMaxConnections"].as<uint32_t>());
	}

	if (root["BattleTokenSecret"])
	{
		baseConfig.set_battle_token_secret(root["BattleTokenSecret"].as<std::string>());
	}

	if (root["BattleMaxConnections"])
	{
		baseConfig.set_battle_max_connections(root["BattleMaxConnections"].as<uint32_t>());
	}

	// 集群号,缺省 0。C++ 侧 2026-09-08 起没有消费者(scene 退出了 snowflake 槽位协议,
	// 永久 guid 走号段,不编码集群);只是原样读入,与 Go 服务的 ClusterId 保持同一份 yaml 口径。
	// 不再提供 CLUSTER_ID 环境覆盖 —— 没有消费者就没有覆盖的理由。
	if (root["ClusterId"])
	{
		baseConfig.set_cluster_id(root["ClusterId"].as<uint32_t>());
	}

	// 永久 guid 号段(见 config.proto IdSegmentConfig):一种 GUID 一块配置,按 Kind 名字
	// 对上 GuidSegmentRegistry 里的实例(item / txlog / snapshot;以后 pet / guild 只加一块)。
	// 逐键显式读,与本文件其他块一致;缺键 = proto 默认值(0),由 scene 侧的 Enable 校验并拒绝。
	if (root["IdSegments"])
	{
		IdSegmentConfig &segConfig = *baseConfig.mutable_id_segment();
		for (const auto &kindNode : root["IdSegments"])
		{
			IdSegmentKindConfig &kindConfig = *segConfig.add_kinds();
			if (kindNode["Kind"])
			{
				kindConfig.set_kind(kindNode["Kind"].as<std::string>());
			}
			if (kindNode["Enabled"])
			{
				kindConfig.set_enabled(kindNode["Enabled"].as<bool>());
			}
			if (kindNode["InitialStep"])
			{
				kindConfig.set_initial_step(kindNode["InitialStep"].as<uint32_t>());
			}
			if (kindNode["MinStep"])
			{
				kindConfig.set_min_step(kindNode["MinStep"].as<uint32_t>());
			}
			if (kindNode["MaxStep"])
			{
				kindConfig.set_max_step(kindNode["MaxStep"].as<uint32_t>());
			}
		}
	}

	// Kafka config
	if (root["Kafka"])
	{
		const YAML::Node &kafkaNode = root["Kafka"];
		KafkaConfig &kafkaConfig = *baseConfig.mutable_kafka();

		if (kafkaNode["Brokers"])
		{
			for (const auto &broker : kafkaNode["Brokers"])
			{
				kafkaConfig.add_brokers(broker.as<std::string>());
			}
		}
		if (kafkaNode["Topics"])
		{
			for (const auto &topic : kafkaNode["Topics"])
			{
				kafkaConfig.add_topics(topic.as<std::string>());
			}
		}
		if (kafkaNode["GroupID"])
		{
			kafkaConfig.set_group_id(kafkaNode["GroupID"].as<std::string>());
		}
		if (kafkaNode["EnableAutoCommit"])
		{
			kafkaConfig.set_enable_auto_commit(kafkaNode["EnableAutoCommit"].as<bool>());
		}
		if (kafkaNode["AutoOffsetReset"])
		{
			kafkaConfig.set_auto_offset_reset(kafkaNode["AutoOffsetReset"].as<std::string>());
		}
		// 控制面命令 topic 的分区契约(docs/design/control-plane-topic-partitioning-20260908.md)。
		// 三个键都可以不写:不写 = 0 = 用 node_command_topic.h 里的编译期默认值。
		if (kafkaNode["CommandTopicPartitions"])
		{
			kafkaConfig.set_command_topic_partitions(kafkaNode["CommandTopicPartitions"].as<uint32_t>());
		}
		if (kafkaNode["CommandTopicGeneration"])
		{
			kafkaConfig.set_command_topic_generation(kafkaNode["CommandTopicGeneration"].as<uint32_t>());
		}
		if (kafkaNode["DisableLegacyPerNodeTopic"])
		{
			kafkaConfig.set_disable_legacy_per_node_topic(kafkaNode["DisableLegacyPerNodeTopic"].as<bool>());
		}
	}

	return true;
}

bool readGameConfig(const std::string &filename, GameConfig &gameConfig)
{
	YAML::Node root = YAML::LoadFile(filename);

	if (root["SceneNodeType"])
	{
		gameConfig.set_scene_node_type(root["SceneNodeType"].as<uint32_t>());
	}

	if (root["ZoneId"])
	{
		gameConfig.set_zone_id(root["ZoneId"].as<uint32_t>());
	}

	// Environment overrides (last-wins). These let a single image be reused
	// across two Deployments with different roles, without templating YAML
	// per pod. See docs/ops/scene-node-role-split.md for the rollout flow.
	//   SCENE_NODE_TYPE: 0=main world, 1=instance, 2=main world cross, 3=instance cross.
	//   ZONE_ID:          per-zone override when the same image is shared.
	if (const char *envRole = GetNonEmptyEnv("SCENE_NODE_TYPE"))
	{
		try
		{
			const uint32_t role = static_cast<uint32_t>(std::stoul(envRole));
			gameConfig.set_scene_node_type(role);
			LOG_INFO << "SCENE_NODE_TYPE env override applied: " << role;
		}
		catch (const std::exception &ex)
		{
			LOG_WARN << "Ignore invalid SCENE_NODE_TYPE env value '" << envRole << "': " << ex.what();
		}
	}

	if (const char *envZone = GetNonEmptyEnv("ZONE_ID"))
	{
		try
		{
			const uint32_t zone = static_cast<uint32_t>(std::stoul(envZone));
			gameConfig.set_zone_id(zone);
			LOG_INFO << "ZONE_ID env override applied: " << zone;
		}
		catch (const std::exception &ex)
		{
			LOG_WARN << "Ignore invalid ZONE_ID env value '" << envZone << "': " << ex.what();
		}
	}

	// Parse zone Redis config
	if (root["zoneredis"])
	{
		YAML::Node zoneredisNode = root["zoneredis"];

		if (zoneredisNode["host"])
		{
			gameConfig.mutable_zone_redis()->set_host(zoneredisNode["host"].as<std::string>());
		}
		if (zoneredisNode["port"])
		{
			gameConfig.mutable_zone_redis()->set_port(zoneredisNode["port"].as<uint32_t>());
		}
		if (zoneredisNode["password"])
		{
			gameConfig.mutable_zone_redis()->set_password(zoneredisNode["password"].as<std::string>());
		}
		if (zoneredisNode["db"])
		{
			gameConfig.mutable_zone_redis()->set_db(zoneredisNode["db"].as<uint32_t>());
		}
	}
	else
	{
		LOG_ERROR << "Warning: zoneredis configuration not found in config file.";
		return false;
	}

	return true;
}

void InitThreadLocalConfig()
{
	tlsNodeConfigManager = gNodeConfigManager;
}

std::string GetConfigDir()
{
	return tlsNodeConfigManager.GetBaseDeployConfig().table_data_directory();
}

std::string GetDataRootDir()
{
	return tlsNodeConfigManager.GetBaseDeployConfig().data_root_directory();
}

bool UseProtoBinaryTables()
{
	return tlsNodeConfigManager.GetBaseDeployConfig().table_data_format() == "binary";
}