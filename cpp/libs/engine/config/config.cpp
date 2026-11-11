#include "config.h"
#include <cstdio>
#include <cstdlib>
#include <yaml-cpp/yaml.h>
#include <muduo/base/Logging.h>
#include <node_config_manager.h>

#include "node_log_level.h"

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

// GameConfig.audit_snapshot.sample_per_mille 的缺省值与上限:全量。
constexpr uint32_t kAuditSnapshotSampleAll = 1000;

// GameConfig.player_inbox.periodic_fetch_minutes 的缺省值(docs/design/player-inbox-service.md §7.3、D14)。
// proto3 的 uint32 默认是 0,而 0 的含义是"关闭在线兜底",所以缺键时必须由加载器显式写入,不能依赖 proto 默认。
constexpr uint32_t kDefaultPlayerInboxPeriodicFetchMinutes = 30;
} // namespace

[[noreturn]] void node_log_level::RejectStartup(const std::string &reason)
{
	std::fprintf(stderr, "FATAL LogLevel config: %s\n", reason.c_str());
	std::fflush(stderr);
	// 此时尚未初始化外部服务；配置拒绝无需崩溃转储，避免 abort() 在 Windows Debug 等待弹窗。
	std::_Exit(EXIT_FAILURE);
}

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
			const std::string reason = "LogLevel must be an integer 0..5 (muduo Logger::LogLevel: 0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL), got " +
									   (rawLevel.empty() ? std::string("missing / empty value") : "'" + rawLevel + "'") +
									   " in " + filename + "; refusing to start (a missing key used to mean 0=TRACE)";
			node_log_level::RejectStartup(reason);
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

	if (root["SpectateTokenSecret"])
	{
		baseConfig.set_spectate_token_secret(root["SpectateTokenSecret"].as<std::string>());
	}

	// 三把令牌密钥的环境变量覆盖(后者生效,与 ZONE_REDIS_PASSWORD 同一模式):K8s 上它们只放 Secret,经 secretKeyRef 注入同名变量,
	// node-config / battle-node-config 里写空串(docs/design/workload-secrets-via-k8s-secret.md §4.1)。本机不设变量,照用 yaml。
	// 缺变量时取 yaml 的值;生产档下为空由各节点既有的空密钥校验拒启。值不进日志。
	if (const char *envSecret = GetNonEmptyEnv("MMORPG_GATE_TOKEN_SECRET"))
	{
		baseConfig.set_gate_token_secret(envSecret);
		LOG_INFO << "MMORPG_GATE_TOKEN_SECRET env override applied (value hidden)";
	}
	if (const char *envSecret = GetNonEmptyEnv("MMORPG_BATTLE_TOKEN_SECRET"))
	{
		baseConfig.set_battle_token_secret(envSecret);
		LOG_INFO << "MMORPG_BATTLE_TOKEN_SECRET env override applied (value hidden)";
	}
	if (const char *envSecret = GetNonEmptyEnv("MMORPG_SPECTATE_TOKEN_SECRET"))
	{
		baseConfig.set_spectate_token_secret(envSecret);
		LOG_INFO << "MMORPG_SPECTATE_TOKEN_SECRET env override applied (value hidden)";
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

	// 进场、离场的审计快照(modules/snapshot/audit_snapshot_policy.h)。整段或单键缺失都取默认:两个触发点都开、全量。
	// 抽样千分比超过 1000 拒绝加载:配错的值不能被悄悄当成全量或零。
	// K8s 的 node-config 由 tools/scripts/k8s_deploy.ps1 从 bin/etc/game_config.yaml 整段复制 audit_snapshot 段。
	{
		auto *snapshotConfig = gameConfig.mutable_audit_snapshot();
		snapshotConfig->set_login_enabled(true);
		snapshotConfig->set_logout_enabled(true);
		snapshotConfig->set_sample_per_mille(kAuditSnapshotSampleAll);
		if (const YAML::Node snapshotNode = root["audit_snapshot"]; snapshotNode)
		{
			if (snapshotNode["login_enabled"])
			{
				snapshotConfig->set_login_enabled(snapshotNode["login_enabled"].as<bool>());
			}
			if (snapshotNode["logout_enabled"])
			{
				snapshotConfig->set_logout_enabled(snapshotNode["logout_enabled"].as<bool>());
			}
			if (snapshotNode["sample_per_mille"])
			{
				const auto perMille = snapshotNode["sample_per_mille"].as<uint32_t>();
				if (perMille > kAuditSnapshotSampleAll)
				{
					LOG_ERROR << "game_config audit_snapshot.sample_per_mille=" << perMille << " exceeds " << kAuditSnapshotSampleAll;
					return false;
				}
				snapshotConfig->set_sample_per_mille(perMille);
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

	// 本 zone 的场景控制域 Redis(docs/design/control-plane-zone-scoping-20261105.md §4.5):scene 读 scene_manager 写的读模型。
	// 整段缺失时取上面的 zoneredis(此刻已含 ZONE_REDIS_PASSWORD 覆盖),开发环境不用改配置;写了这一段就是另一个实例,
	// host 必填,口令只认显式值与 SCENE_CONTROL_REDIS_PASSWORD,不继承 zoneredis 的。只有 scene 连它,gate、battle 读到也不用。
	{
		auto *sceneControl = gameConfig.mutable_scene_control_redis();
		sceneControl->Clear();
		if (const YAML::Node node = root["scene_control_redis"]; node)
		{
			if (!node["host"])
			{
				const std::string reason = "scene_control_redis.host is required when the section is present in " + filename + "; refusing to start";
				std::fprintf(stderr, "FATAL scene_control_redis config: %s\n", reason.c_str());
				std::fflush(stderr);
				LOG_FATAL << "readGameConfig: " << reason;
				return false; // LOG_FATAL 已 abort;这一行只为控制流明确
			}
			sceneControl->set_host(node["host"].as<std::string>());
			sceneControl->set_port(node["port"] ? node["port"].as<uint32_t>() : 6379u);
			if (node["password"])
			{
				sceneControl->set_password(node["password"].as<std::string>());
			}
			if (node["db"])
			{
				sceneControl->set_db(node["db"].as<uint32_t>());
			}
		}
		else
		{
			sceneControl->CopyFrom(gameConfig.zone_redis());
		}
		if (const char *envPassword = GetNonEmptyEnv("SCENE_CONTROL_REDIS_PASSWORD"))
		{
			sceneControl->set_password(envPassword);
			LOG_INFO << "SCENE_CONTROL_REDIS_PASSWORD env override applied (value hidden)";
		}
	}

	// 玩家域 Redis(docs/design/redis-player-store-cluster.md §5.1、§5.6)。整段或单键缺失时显式取默认:
	// mode = standalone;standalone 又没写 seeds 时地址与口令都沿用上面的 zoneredis(此刻已含 ZONE_REDIS_PASSWORD 覆盖),开发环境不用改配置。
	// 写了 seeds 就是另一个实例,口令只认显式值,不继承。
	// 这里只读入与补默认,取值由消费方 scene 的 RedisSystem 在启动时校验:gate、battle 不连玩家域,配错了不该拖累它们。
	{
		auto *playerRedis = gameConfig.mutable_player_redis();
		playerRedis->Clear(); // seeds 是 repeated,同一个对象再读一遍不能累加
		if (const YAML::Node playerRedisNode = root["player_redis"]; playerRedisNode)
		{
			if (playerRedisNode["mode"])
			{
				playerRedis->set_mode(playerRedisNode["mode"].as<std::string>());
			}
			if (const YAML::Node seedsNode = playerRedisNode["seeds"]; seedsNode)
			{
				// 写成标量时迭代不出元素,standalone 下会悄悄回落到 zone_redis,连错 Redis 也没有任何提示。
				if (!seedsNode.IsSequence())
				{
					const std::string reason = "player_redis.seeds must be a list of host:port in " + filename + "; refusing to start";
					std::fprintf(stderr, "FATAL player_redis config: %s\n", reason.c_str());
					std::fflush(stderr);
					LOG_FATAL << "readGameConfig: " << reason;
					return false; // LOG_FATAL 已 abort;这一行只为控制流明确
				}
				for (const auto &seed : seedsNode)
				{
					playerRedis->add_seeds(seed.as<std::string>());
				}
			}
			if (playerRedisNode["password"])
			{
				playerRedis->set_password(playerRedisNode["password"].as<std::string>());
			}
		}
		if (playerRedis->mode().empty())
		{
			playerRedis->set_mode("standalone");
		}
		if (playerRedis->mode() == "standalone" && playerRedis->seeds().empty())
		{
			const auto &zoneRedis = gameConfig.zone_redis();
			playerRedis->add_seeds(zoneRedis.host() + ":" + std::to_string(zoneRedis.port()));
			playerRedis->set_password(zoneRedis.password());
		}
		if (const char *envPassword = GetNonEmptyEnv("PLAYER_REDIS_PASSWORD"))
		{
			playerRedis->set_password(envPassword);
			LOG_INFO << "PLAYER_REDIS_PASSWORD env override applied (value hidden)";
		}
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
