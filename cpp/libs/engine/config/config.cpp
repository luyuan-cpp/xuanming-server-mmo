#include "config.h"
#include <cstdio>
#include <yaml-cpp/yaml.h>
#include <muduo/base/Logging.h>
#include <node_config_manager.h>

#include "node_log_level.h"
#include "persist_schema_regression_mode.h"

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

// GameConfig.player_inbox.periodic_fetch_minutes 的缺省值(docs/design/player-inbox-service.md §7.3、D14)。
// proto3 的 uint32 默认是 0,而 0 的含义是"关闭在线兜底",所以缺键时必须由加载器显式写入,不能依赖 proto 默认。
constexpr uint32_t kDefaultPlayerInboxPeriodicFetchMinutes = 30;
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

	// 日志级别(见 node_log_level.h):数值即 muduo 枚举,只认 0..5 的整数;缺键 / 空值 / 非整数 / 越界都拒绝启动
	// (gate 准入设计 P0-3)。以前缺键静默落到 proto 默认 0 = TRACE,越界值原样进 Node::InitLogSystem 的 static_cast。
	{
		const YAML::Node levelNode = root["LogLevel"];
		// 缺键、YAML 空值(`LogLevel:` / `~`)与序列 / 映射都折成空串,由 Parse 一并拒绝。
		// 不走 as<uint32_t>():非整数会抛 yaml-cpp 异常,得不到下面这行带枚举对照的说明。
		const std::string rawLevel = (levelNode && levelNode.IsScalar()) ? levelNode.Scalar() : std::string();
		const auto level = node_log_level::Parse(rawLevel);
		if (!level)
		{
			// 写法同 node.cpp 的 FatalStartupConfig:Node 构造时已切到异步日志,LOG_FATAL 的 abort 可能赶在落盘之前,
			// Linux 上 kubectl logs 也看不到;先同步写一行 stderr(kubectl logs --previous 可见)。原值是运维手写的数字,不敏感。
			const std::string reason = "LogLevel must be an integer 0..5 (muduo Logger::LogLevel: 0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL), got " +
									   (rawLevel.empty() ? std::string("missing / empty value") : "'" + rawLevel + "'") +
									   " in " + filename + "; refusing to start (a missing key used to mean 0=TRACE)";
			std::fprintf(stderr, "FATAL LogLevel config: %s\n", reason.c_str());
			std::fflush(stderr);
			LOG_FATAL << "readBaseDeployConfig: " << reason;
			return false; // LOG_FATAL 已 abort;这一行只为不在 nullopt 上解引用
		}
		baseConfig.set_log_level(*level);
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

	// 审计 topic 世代号(见 config.proto audit_topic_generation / modules/audit/audit_topic.h)。
	// 可以不写:不写 = 0 = 按第一代(_g1)处理,存量 yaml 不改也能起。
	// 必须与 go/data_service 的 Kafka.TopicGeneration 相等,否则流水 / 快照写进没人消费的 topic。
	if (root["AuditTopicGeneration"])
	{
		baseConfig.set_audit_topic_generation(root["AuditTopicGeneration"].as<uint32_t>());
	}

	// 玩家存盘 DBTask topic 世代号(见 config.proto db_task_topic_generation /
	// services/scene/player/constants/player.h GetDbTaskTopic)。
	// 可以不写:不写 = 0 = 第一代,topic 名仍是不带后缀的 db_task_zone_{zone},存量 yaml 不改也能起。
	// 必须与 go/db、go/login 的 Kafka.TopicGeneration 相等,否则换代后存盘写进没人消费的旧 topic。
	if (root["DbTaskTopicGeneration"])
	{
		baseConfig.set_db_task_topic_generation(root["DbTaskTopicGeneration"].as<uint32_t>());
	}

	// 严格提交屏障 C3 的模式(见 config.proto persist_schema_regression_mode / persist_schema_regression_mode.h)。
	// 在这里一次性归一化:缺键 / 空串 = warn;warn / enforce 原样;其余拒绝启动。运行期只会见到两个字面量,
	// 非法值不会进 Lua 的 ARGV[10](那会让脚本返回 error_reply、OnSaved 无限重试,全服存不进盘)。
	{
		// YAML 空值(`PersistSchemaRegressionMode:` 或 `~`)是 Null 节点,yaml-cpp 的 as<std::string>() 会把它变成字面量
		// "null";与缺键、空串一样按 warn,和 login、start_game.ps1 的口径一致。
		const YAML::Node modeNode = root["PersistSchemaRegressionMode"];
		const std::string rawMode = (modeNode && !modeNode.IsNull()) ? modeNode.as<std::string>() : std::string();
		const auto mode = persist_schema_regression_mode::Normalize(rawMode);
		if (!mode)
		{
			LOG_FATAL << "readBaseDeployConfig: PersistSchemaRegressionMode must be 'warn' or 'enforce' (or absent = warn), got '"
					  << rawMode << "' in " << filename;
			return false; // LOG_FATAL 已 abort;这一行只为不在 nullopt 上解引用
		}
		baseConfig.set_persist_schema_regression_mode(std::string(*mode));
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

	// C++ 节点 unary gRPC 调用的 deadline(见 config.proto GrpcClientConfig)。可以不写:缺席 = 全部用内置默认。
	// 这里只原样读入;键名是否认识、值是否为 0 由 grpc_call_deadline::Apply 校验(那里知道 ENodeType)。
	if (root["GrpcClient"] && root["GrpcClient"]["CallDeadlineMs"])
	{
		auto &deadlines = *baseConfig.mutable_grpc_client()->mutable_call_deadline_ms();
		for (const auto &entry : root["GrpcClient"]["CallDeadlineMs"])
		{
			deadlines[entry.first.as<std::string>()] = entry.second.as<uint32_t>();
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

	// 玩家持久收件箱的 scene 消费端(docs/design/player-inbox-service.md §3.3、§7.3)。整段或单键缺失都取默认:
	// consumer_enabled = false(上线顺序第 4 步的回滚下限:新字段只往返、不消费,§11.7);
	// periodic_fetch_minutes = 30(显式写入,见 kDefaultPlayerInboxPeriodicFetchMinutes)。
	// K8s 的 node-config 由 tools/scripts/k8s_deploy.ps1 从 git 跟踪的 bin/etc/game_config.yaml 整段复制 player_inbox 段
	// (收件箱交接 §6 H1:开关只认那份 yaml,部署前有写前门禁);缺段 / 缺键才走这里的默认。
	{
		auto *inboxConfig = gameConfig.mutable_player_inbox();
		inboxConfig->set_consumer_enabled(false);
		inboxConfig->set_periodic_fetch_minutes(kDefaultPlayerInboxPeriodicFetchMinutes);
		if (const YAML::Node inboxNode = root["player_inbox"]; inboxNode)
		{
			if (inboxNode["consumer_enabled"])
			{
				inboxConfig->set_consumer_enabled(inboxNode["consumer_enabled"].as<bool>());
			}
			if (inboxNode["periodic_fetch_minutes"])
			{
				inboxConfig->set_periodic_fetch_minutes(inboxNode["periodic_fetch_minutes"].as<uint32_t>());
			}
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

		// 口令的环境变量覆盖(后者生效,与上面 SCENE_NODE_TYPE / ZONE_ID 同一模式):带口令的环境从 Secret
		// 注入,不写进 yaml;本机验收也不必改被跟踪的文件。口令值不进日志。
		if (const char *envPassword = GetNonEmptyEnv("ZONE_REDIS_PASSWORD"))
		{
			gameConfig.mutable_zone_redis()->set_password(envPassword);
			LOG_INFO << "ZONE_REDIS_PASSWORD env override applied (value hidden)";
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