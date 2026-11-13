#include "gtest/gtest.h"
#include "gmock/gmock.h"
#include "muduo/base/Logging.h"
#include "table/code/test_table.h"
#include "table/code/testmultikey_table.h"
#include "table/code/testmultikey_table_comp.h"
#include "table/code/testmultikey_table_fk.h"
#include "table/code/skill_table.h"
#include "table/code/buff_table.h"
#include "table/code/buff_table_comp.h"
#include "../test_config_helper.h"
#include "engine/config/config.h"
#include "modules/snapshot/audit_snapshot_policy.h"

#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <system_error>

void LoadTables();

// TestMultiKey 的主键可重复((cfg_multi)),生成器刻意不提供 FindById —— 「按 id 拿一行」
// 的单值语义不成立。下面这些用例只关心 id=1 的第一行,用 FindAllById 取首个。
static const TestMultiKeyTable *FirstTestMultiKeyRowById(const TestMultiKeyTableManager &mgr,
                                                         uint32_t tableId)
{
    for (const auto *row : mgr.FindAllById(tableId))
        return row;
    return nullptr;
}

// ---------------------------------------------------------------------------
// Portable env helpers for the GameConfigEnvOverride tests below.
// ---------------------------------------------------------------------------
namespace
{
    void SetEnvVar(const char *name, const char *value)
    {
#ifdef _WIN32
        _putenv_s(name, value);
#else
        if (value && value[0] != '\0')
            setenv(name, value, 1);
        else
            unsetenv(name);
#endif
    }

    void ClearEnvVar(const char *name) { SetEnvVar(name, ""); }
} // namespace

// ---------------------------------------------------------------------------
// BaseDeployConfig YAML -> proto 显式映射
// ---------------------------------------------------------------------------

TEST(BaseDeployConfigTest, GateMaxConnectionsUsesShippedSafeDefault)
{
	const auto maxConnections =
		tlsNodeConfigManager.GetBaseDeployConfig().gate_max_connections();

	// 漏掉 config.cpp 的显式字段映射时,proto3 会静默回落到 0(不限)。
	// 这里既钉住仓库默认值,也钉住安全的 session-id 空间上界。
	EXPECT_EQ(maxConnections, 20000u);
	EXPECT_GT(maxConnections, 0u);
	EXPECT_LE(maxConnections, 131071u);
}

// 三把令牌密钥可由环境变量注入(K8s Secret,docs/design/workload-secrets-via-k8s-secret.md §4.1),覆盖 yaml。
// 先读配置、再清变量、最后断言:断言失败提前返回时也不会把变量漏给后面的用例。断言不打印密钥值(AGENTS.md §11.3)。
TEST(BaseDeployConfigEnvOverride, TokenSecretsFromEnvOverrideYaml)
{
	constexpr const char *kGateSecret = "env-gate-token-secret-0123456789abcdef";
	constexpr const char *kBattleSecret = "env-battle-token-secret-0123456789abcd";
	constexpr const char *kSpectateSecret = "env-spectate-token-secret-0123456789ab";
	BaseDeployConfig cfg;
	SetEnvVar("MMORPG_GATE_TOKEN_SECRET", kGateSecret);
	SetEnvVar("MMORPG_BATTLE_TOKEN_SECRET", kBattleSecret);
	SetEnvVar("MMORPG_SPECTATE_TOKEN_SECRET", kSpectateSecret);
	const bool loaded = readBaseDeployConfig("etc/base_deploy_config.yaml", cfg);
	ClearEnvVar("MMORPG_GATE_TOKEN_SECRET");
	ClearEnvVar("MMORPG_BATTLE_TOKEN_SECRET");
	ClearEnvVar("MMORPG_SPECTATE_TOKEN_SECRET");
	ASSERT_TRUE(loaded);
	EXPECT_TRUE(cfg.gate_token_secret() == kGateSecret) << "MMORPG_GATE_TOKEN_SECRET must win over the yaml GateTokenSecret";
	EXPECT_TRUE(cfg.battle_token_secret() == kBattleSecret) << "MMORPG_BATTLE_TOKEN_SECRET must win over the yaml BattleTokenSecret";
	EXPECT_TRUE(cfg.spectate_token_secret() == kSpectateSecret) << "MMORPG_SPECTATE_TOKEN_SECRET must win over the yaml SpectateTokenSecret";
}

// 变量不设或为空串时照用 yaml:本机不设这些变量,行为与引入覆盖之前相同。两次读取的结果逐字段一致即可,不依赖 yaml 里的具体值。
TEST(BaseDeployConfigEnvOverride, TokenSecretsWithoutEnvKeepYaml)
{
	ClearEnvVar("MMORPG_GATE_TOKEN_SECRET");
	ClearEnvVar("MMORPG_BATTLE_TOKEN_SECRET");
	ClearEnvVar("MMORPG_SPECTATE_TOKEN_SECRET");
	BaseDeployConfig withoutEnv;
	ASSERT_TRUE(readBaseDeployConfig("etc/base_deploy_config.yaml", withoutEnv));
	EXPECT_FALSE(withoutEnv.gate_token_secret().empty()) << "the shipped yaml must keep a local GateTokenSecret";

	SetEnvVar("MMORPG_GATE_TOKEN_SECRET", "");
	BaseDeployConfig withEmptyEnv;
	const bool loaded = readBaseDeployConfig("etc/base_deploy_config.yaml", withEmptyEnv);
	ClearEnvVar("MMORPG_GATE_TOKEN_SECRET");
	ASSERT_TRUE(loaded);
	EXPECT_TRUE(withEmptyEnv.gate_token_secret() == withoutEnv.gate_token_secret()) << "an empty variable must not override the yaml";
	EXPECT_TRUE(withEmptyEnv.battle_token_secret() == withoutEnv.battle_token_secret());
	EXPECT_TRUE(withEmptyEnv.spectate_token_secret() == withoutEnv.spectate_token_secret());
}

// ---------------------------------------------------------------------------
// 验证各配置表能正常加载并遍历
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, IterateSkillTable)
{
	for (auto &row : FindAllSkillTable().data())
	{
		LOG_INFO << row.ShortDebugString();
	}
}

TEST(ConfigTableTest, IterateBuffTable)
{
	for (auto &row : FindAllBuffTable().data())
	{
		LOG_INFO << row.ShortDebugString();
	}
}

TEST(ConfigTableTest, IterateTestTable)
{
	for (auto &row : FindAllTestTable().data())
	{
		LOG_INFO << row.ShortDebugString();
	}
}

TEST(ConfigTableTest, IterateTestMultiKeyTable)
{
	for (auto &row : FindAllTestMultiKeyTable().data())
	{
		LOG_INFO << row.ShortDebugString();
	}
}

// ---------------------------------------------------------------------------
// 多键表 equal_range 范围查询
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, MultiKeyUint32RangeQuery)
{
	auto &data = TestMultiKeyTableManager::Instance().GetMUint32KeyMap();
	auto range = data.equal_range(17);
	for (auto it = range.first; it != range.second; ++it)
	{
		LOG_INFO << it->first << ' ' << it->second->ShortDebugString();
	}
}

TEST(ConfigTableTest, MultiKeyInt32RangeQuery)
{
	auto &data = TestMultiKeyTableManager::Instance().GetMInt32KeyMap();
	auto range = data.equal_range(10);
	for (auto it = range.first; it != range.second; ++it)
	{
		LOG_INFO << it->first << ' ' << it->second->ShortDebugString();
	}
}

TEST(ConfigTableTest, MultiKeyStringRangeQuery)
{
	auto &data = TestMultiKeyTableManager::Instance().GetMStringKeyMap();
	auto range = data.equal_range("aa");
	for (auto it = range.first; it != range.second; ++it)
	{
		LOG_INFO << it->first << ' ' << it->second->ShortDebugString();
	}
}

// ---------------------------------------------------------------------------
// 多键表精确查找
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, FindByUint32Key)
{
	auto result = TestMultiKeyTableManager::Instance().FindByUint32Key(14);
	EXPECT_EQ(result.first->id(), 1);
}

TEST(ConfigTableTest, FindByInt32Key)
{
	auto result = TestMultiKeyTableManager::Instance().FindByInt32Key(8);
	EXPECT_EQ(result.first->id(), 1);
}

TEST(ConfigTableTest, FindByStringKey)
{
	auto result = TestMultiKeyTableManager::Instance().FindByStringKey("aa");
	EXPECT_EQ(result.first->id(), 1);
}

// ---------------------------------------------------------------------------
// 每列组件 (per-column ECS component)
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, ScalarCompFromRow)
{
	const auto *row = FirstTestMultiKeyRowById(TestMultiKeyTableManager::Instance(), 1);
	ASSERT_NE(row, nullptr);

	auto idComp = MakeTestMultiKeyIdComp(*row);
	EXPECT_EQ(idComp.value, 1u);

	auto strComp = MakeTestMultiKeyString_keyComp(*row);
	EXPECT_EQ(strComp.value, "aa");

	auto u32Comp = MakeTestMultiKeyUint32_keyComp(*row);
	EXPECT_EQ(u32Comp.value, 14u);

	auto i32Comp = MakeTestMultiKeyInt32_keyComp(*row);
	EXPECT_EQ(i32Comp.value, 8);
}

TEST(ConfigTableTest, RepeatedCompSpan)
{
	const auto *row = FirstTestMultiKeyRowById(TestMultiKeyTableManager::Instance(), 1);
	ASSERT_NE(row, nullptr);

	auto effectComp = MakeTestMultiKeyEffectComp(*row);
	// span points into proto memory
	EXPECT_EQ(effectComp.values.data(), row->effect().data());
	EXPECT_EQ(effectComp.values.size(), static_cast<size_t>(row->effect_size()));
}

TEST(ConfigTableTest, BuffCompStringView)
{
	// Verify string columns produce valid string_view components
	for (auto &row : FindAllBuffTable().data())
	{
		auto comp = MakeBuffHealth_regenerationComp(row);
		// string_view should point into proto memory (not a copy)
		if (!row.health_regeneration().empty())
		{
			EXPECT_EQ(comp.value.data(), row.health_regeneration().data());
		}
	}
}

// ---------------------------------------------------------------------------
// repeated 列值索引 (repeated column value index)
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, RepeatedEffectIndex)
{
	// GetEffectIndex() returns multimap<uint32_t, const Table*>
	auto &idx = TestMultiKeyTableManager::Instance().GetEffectIndex();
	// Every row's effect values should be indexed
	for (auto &row : FindAllTestMultiKeyTable().data())
	{
		for (auto val : row.effect())
		{
			auto range = idx.equal_range(val);
			bool found = false;
			for (auto it = range.first; it != range.second; ++it)
			{
				if (it->second->id() == row.id())
				{
					found = true;
					break;
				}
			}
			EXPECT_TRUE(found) << "effect value " << val
							   << " not indexed for row id " << row.id();
		}
	}
}

TEST(ConfigTableTest, BuffSubBuffIndex)
{
	auto &idx = BuffTableManager::Instance().GetSubBuffIndex();
	for (auto &row : FindAllBuffTable().data())
	{
		for (auto val : row.sub_buff())
		{
			auto range = idx.equal_range(val);
			bool found = false;
			for (auto it = range.first; it != range.second; ++it)
			{
				if (it->second->id() == row.id())
				{
					found = true;
					break;
				}
			}
			EXPECT_TRUE(found) << "sub_buff value " << val
							   << " not indexed for row id " << row.id();
		}
	}
}

// ---------------------------------------------------------------------------
// Reload (hot-reload safety: snapshot swap, no accumulation)
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, ReloadDoesNotAccumulateData)
{
	auto &mgr = TestMultiKeyTableManager::Instance();
	const auto countBefore = mgr.Count();
	ASSERT_GT(countBefore, 0u);

	// Reload the same data file.
	mgr.Load();

	// Count must stay the same — old snapshot replaced, not accumulated.
	EXPECT_EQ(mgr.Count(), countBefore);

	// Data should still be accessible.
	const auto *row = FirstTestMultiKeyRowById(mgr, 1);
	ASSERT_NE(row, nullptr);
	EXPECT_EQ(row->id(), 1u);
}

TEST(ConfigTableTest, ReloadReplacesOldPointers)
{
	auto &mgr = TestMultiKeyTableManager::Instance();

	// Get a pointer before reload.
	const auto *rowBefore = FirstTestMultiKeyRowById(mgr, 1);
	ASSERT_NE(rowBefore, nullptr);
	EXPECT_EQ(rowBefore->id(), 1u);

	// Reload.
	mgr.Load();

	// New pointer should be valid and contain correct data,
	// but will point to a different Snapshot's protobuf storage.
	const auto *rowAfter = FirstTestMultiKeyRowById(mgr, 1);
	ASSERT_NE(rowAfter, nullptr);
	EXPECT_EQ(rowAfter->id(), 1u);
	EXPECT_NE(rowBefore, rowAfter) << "reload should produce a new Snapshot";
}

TEST(ConfigTableTest, ReloadMultiKeyIndicesConsistent)
{
	auto &mgr = TestMultiKeyTableManager::Instance();

	// Reload twice to stress test.
	mgr.Load();
	mgr.Load();

	// Multi-key range query should still work correctly.
	auto &data = mgr.GetMUint32KeyMap();
	auto range = data.equal_range(17);
	std::size_t rangeCount = 0;
	for (auto it = range.first; it != range.second; ++it)
	{
		++rangeCount;
	}
	// Verify no accumulation: count should match initial data.
	const auto countBefore = mgr.Count();
	mgr.Load();
	EXPECT_EQ(mgr.Count(), countBefore);
}

// ---------------------------------------------------------------------------
// Foreign key lookups
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, ScalarForeignKeyLookup)
{
	const auto *row = FirstTestMultiKeyRowById(TestMultiKeyTableManager::Instance(), 1);
	ASSERT_NE(row, nullptr);

	const auto *testRow = GetTestMultiKeyTestRefRow(*row);
	ASSERT_NE(testRow, nullptr);
	EXPECT_EQ(testRow->id(), row->test_ref());
}

TEST(ConfigTableTest, GroupForeignKeyLookup)
{
	const auto *row = FirstTestMultiKeyRowById(TestMultiKeyTableManager::Instance(), 1);
	ASSERT_NE(row, nullptr);

	auto testRows = GetTestMultiKeyTestRefsRows(*row);
	EXPECT_EQ(static_cast<int>(testRows.size()), row->test_refs_size());
	for (int i = 0; i < static_cast<int>(testRows.size()); ++i)
	{
		EXPECT_EQ(testRows[i]->id(), row->test_refs(i));
	}
}

TEST(ConfigTableTest, ForeignKeyAfterReload)
{
	auto &mgr = TestMultiKeyTableManager::Instance();

	const auto *rowBefore = FirstTestMultiKeyRowById(mgr, 1);
	ASSERT_NE(rowBefore, nullptr);
	const auto *fkBefore = GetTestMultiKeyTestRefRow(*rowBefore);
	ASSERT_NE(fkBefore, nullptr);
	const auto fkIdBefore = fkBefore->id();

	mgr.Load();
	TestTableManager::Instance().Load();

	const auto *rowAfter = FirstTestMultiKeyRowById(mgr, 1);
	ASSERT_NE(rowAfter, nullptr);
	const auto *fkAfter = GetTestMultiKeyTestRefRow(*rowAfter);
	ASSERT_NE(fkAfter, nullptr);

	EXPECT_EQ(fkAfter->id(), fkIdBefore);
	EXPECT_NE(fkAfter, fkBefore) << "reload should produce new snapshot";
}

// ---------------------------------------------------------------------------
// Secondary index (idx option)
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, SecondaryIndexByLevel)
{
	// level column has idx option -> GetByLevel(value) returns matching rows
	auto rows = TestMultiKeyTableManager::Instance().GetByLevel(1);
	EXPECT_EQ(rows.size(), 3u) << "3 rows have level=1";
	for (const auto *r : rows)
	{
		EXPECT_EQ(r->level(), 1u);
	}
}

TEST(ConfigTableTest, SecondaryIndexByLevelUnique)
{
	auto rows = TestMultiKeyTableManager::Instance().GetByLevel(0);
	EXPECT_EQ(rows.size(), 1u) << "1 row has level=0";
	if (!rows.empty())
	{
		EXPECT_EQ(rows[0]->id(), 1u);
	}
}

TEST(ConfigTableTest, SecondaryIndexByLevelNotFound)
{
	auto rows = TestMultiKeyTableManager::Instance().GetByLevel(999);
	EXPECT_TRUE(rows.empty());
}

TEST(ConfigTableTest, SecondaryIndexCountByLevel)
{
	EXPECT_EQ(TestMultiKeyTableManager::Instance().CountByLevelIndex(1), 3u);
	EXPECT_EQ(TestMultiKeyTableManager::Instance().CountByLevelIndex(100), 2u);
	EXPECT_EQ(TestMultiKeyTableManager::Instance().CountByLevelIndex(999), 0u);
}

// ---------------------------------------------------------------------------
// FK auto-indexed (scalar FK columns get a secondary index automatically)
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, ForeignKeyAutoIndex)
{
	// test_ref has fk:Test -> auto-indexed
	auto rows = TestMultiKeyTableManager::Instance().GetByTestRef(1);
	EXPECT_EQ(rows.size(), 1u);
	if (!rows.empty())
	{
		EXPECT_EQ(rows[0]->id(), 1u);
	}
}

TEST(ConfigTableTest, ForeignKeyAutoIndexMultiple)
{
	// Rows with test_ref=0 (default, no FK set)
	auto rows = TestMultiKeyTableManager::Instance().GetByTestRef(0);
	EXPECT_EQ(rows.size(), 3u) << "3 rows have test_ref=0";
}

// ---------------------------------------------------------------------------
// Reverse FK (HasMany): find source rows by FK column value
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, ReverseForeignKeyLookup)
{
	// FindTestMultiKeyRowsByTestRef delegates to GetByTestRef
	auto rows = FindTestMultiKeyRowsByTestRef(1);
	EXPECT_EQ(rows.size(), 1u);
	if (!rows.empty())
	{
		EXPECT_EQ(rows[0]->test_ref(), 1u);
	}
}

TEST(ConfigTableTest, ReverseForeignKeyLookupNotFound)
{
	auto rows = FindTestMultiKeyRowsByTestRef(999);
	EXPECT_TRUE(rows.empty());
}

// ---------------------------------------------------------------------------
// Index survives hot-reload
// ---------------------------------------------------------------------------

TEST(ConfigTableTest, SecondaryIndexAfterReload)
{
	auto &mgr = TestMultiKeyTableManager::Instance();

	auto rowsBefore = mgr.GetByLevel(1);
	ASSERT_EQ(rowsBefore.size(), 3u);

	mgr.Load();

	auto rowsAfter = mgr.GetByLevel(1);
	EXPECT_EQ(rowsAfter.size(), 3u);
	// Pointers should differ (new snapshot)
	EXPECT_NE(rowsBefore[0], rowsAfter[0]);
}

TEST(ConfigTableTest, ReverseFKAfterReload)
{
	auto &mgr = TestMultiKeyTableManager::Instance();

	auto before = FindTestMultiKeyRowsByTestRef(2);
	ASSERT_EQ(before.size(), 1u);

	mgr.Load();

	auto after = FindTestMultiKeyRowsByTestRef(2);
	EXPECT_EQ(after.size(), 1u);
	EXPECT_NE(before[0], after[0]) << "reload should produce new snapshot";
}

// ---------------------------------------------------------------------------
// GameConfig env overrides (SCENE_NODE_TYPE / ZONE_ID / GAME_CONFIG_PATH)
// These run after FindAndLoadTestConfig has chdir'd to the runtime dir, so
// etc/game_config.yaml is guaranteed to resolve.
// ---------------------------------------------------------------------------

TEST(GameConfigEnvOverride, SceneNodeTypeFromEnvOverridesYaml)
{
    GameConfig cfg;
    SetEnvVar("SCENE_NODE_TYPE", "1");
    ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg));
    EXPECT_EQ(cfg.scene_node_type(), 1u)
        << "SCENE_NODE_TYPE=1 must win over the yaml default (0)";
    ClearEnvVar("SCENE_NODE_TYPE");
}

TEST(GameConfigEnvOverride, SceneNodeTypeEmptyEnvKeepsYaml)
{
    GameConfig cfg;
    ClearEnvVar("SCENE_NODE_TYPE");
    ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg));
    // The shipped yaml declares SceneNodeType: 0; empty env must not disturb it.
    EXPECT_EQ(cfg.scene_node_type(), 0u);
}

TEST(GameConfigEnvOverride, SceneNodeTypeInvalidEnvIsIgnored)
{
    GameConfig cfg;
    SetEnvVar("SCENE_NODE_TYPE", "not-a-number");
    ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg));
    // Invalid value must log-warn and fall back to the yaml value, not crash.
    EXPECT_EQ(cfg.scene_node_type(), 0u);
    ClearEnvVar("SCENE_NODE_TYPE");
}

TEST(GameConfigEnvOverride, ZoneIdFromEnvOverridesYaml)
{
    GameConfig cfg;
    SetEnvVar("ZONE_ID", "42");
    ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg));
    EXPECT_EQ(cfg.zone_id(), 42u);
    ClearEnvVar("ZONE_ID");
}

TEST(GameConfigEnvOverride, AcceptsAllFourSceneNodeTypeValues)
{
    for (uint32_t t : {0u, 1u, 2u, 3u})
    {
        GameConfig cfg;
        SetEnvVar("SCENE_NODE_TYPE", std::to_string(t).c_str());
        ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg))
            << "failed at t=" << t;
        EXPECT_EQ(cfg.scene_node_type(), t);
    }
    ClearEnvVar("SCENE_NODE_TYPE");
}

// P0-A(docs/handoff/global-singletons-p0-findings-20261014.md)的口令注入,2026-11-12 起落在控制集群这一段:
// 口令可由环境变量 CONTROL_REDIS_PASSWORD 注入(K8s Secret),覆盖 yaml 的 control_redis.password。
// 先读配置、再清变量、最后断言:断言失败提前返回时也不会把变量漏给后面的用例。
// 断言不打印口令值(AGENTS.md §11.3),只说明哪条规则不成立。
TEST(GameConfigEnvOverride, ControlRedisPasswordFromEnvOverridesYaml)
{
    constexpr const char *kEnvPassword = "p0a-env-pass-01";
    GameConfig cfg;
    SetEnvVar("CONTROL_REDIS_PASSWORD", kEnvPassword);
    const bool loaded = readGameConfig("etc/game_config.yaml", cfg);
    ClearEnvVar("CONTROL_REDIS_PASSWORD");
    ASSERT_TRUE(loaded);
    EXPECT_TRUE(cfg.control_redis().password() == kEnvPassword)
        << "CONTROL_REDIS_PASSWORD must win over the yaml control_redis.password";
}

TEST(GameConfigEnvOverride, ControlRedisPasswordWithoutEnvKeepsYaml)
{
    GameConfig cfg;
    ClearEnvVar("CONTROL_REDIS_PASSWORD");
    ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg));
    // 随仓库发布的 bin/etc/game_config.yaml 写的是空串(= 不认证);同时钉住占位口令 "yourpassword" 不再回来。
    // 用 EXPECT_TRUE 而不是 EXPECT_EQ:失败时不把 yaml 里的口令值打进测试输出(AGENTS.md §11.3)。
    EXPECT_TRUE(cfg.control_redis().password().empty())
        << "the shipped yaml must keep control_redis.password empty; inject real passwords via CONTROL_REDIS_PASSWORD";
}

// ---------------------------------------------------------------------------
// GameConfig.player_inbox(docs/design/player-inbox-service.md §3.3、§7.3、§11.7)
// 加载器对缺键显式取默认:consumer_enabled = false、periodic_fetch_minutes = 30。
// 不能依赖 proto3 的 0 —— periodic_fetch_minutes = 0 的含义是"关闭在线兜底"。
// ---------------------------------------------------------------------------

namespace
{
    // 写一份只含 SceneNodeType / ZoneId 的临时 yaml(Redis 两段都可以不写,缺了取默认),extra 追加在末尾;返回路径。
    // 用例结束时删除。
    std::filesystem::path WriteTempGameConfig(const char *fileName, const std::string &extra)
    {
        const auto path = std::filesystem::temp_directory_path() / fileName;
        std::ofstream out(path, std::ios::trunc);
        out << "SceneNodeType: 0\n"
            << "ZoneId: 1\n"
            << extra;
        return path;
    }

    void RemoveTempGameConfig(const std::filesystem::path &path)
    {
        std::error_code ec;
        std::filesystem::remove(path, ec);
    }
} // namespace

TEST(GameConfigPlayerInbox, ShippedYamlEnablesConsumer)
{
    GameConfig cfg;
    ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg));
    // 全球成局的结算只经收件箱到玩家,go/player_inbox/etc/player_inbox.yaml 的 EnqueueTypes 默认列了 Battle:
    // 消费关着结算到不了玩家,k8s_deploy.ps1 的写前门禁也拒绝部署。
    EXPECT_TRUE(cfg.player_inbox().consumer_enabled());
    EXPECT_EQ(cfg.player_inbox().periodic_fetch_minutes(), 30u);
}

TEST(GameConfigPlayerInbox, MissingSectionFallsBackToDefaults)
{
    const auto path = WriteTempGameConfig("game_config_player_inbox_missing.yaml", "");
    GameConfig cfg;
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_FALSE(cfg.player_inbox().consumer_enabled());
    EXPECT_EQ(cfg.player_inbox().periodic_fetch_minutes(), 30u)
        << "缺键必须显式取 30,不能落到 proto3 的 0(= 关闭兜底)";
}

TEST(GameConfigPlayerInbox, MissingKeyInsideSectionFallsBackToDefault)
{
    const auto path = WriteTempGameConfig("game_config_player_inbox_partial.yaml",
                                          "player_inbox:\n  consumer_enabled: true\n");
    GameConfig cfg;
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_TRUE(cfg.player_inbox().consumer_enabled());
    EXPECT_EQ(cfg.player_inbox().periodic_fetch_minutes(), 30u);
}

TEST(GameConfigPlayerInbox, ExplicitValuesWinIncludingZero)
{
    const auto path = WriteTempGameConfig("game_config_player_inbox_explicit.yaml",
                                          "player_inbox:\n  consumer_enabled: true\n  periodic_fetch_minutes: 0\n");
    GameConfig cfg;
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_TRUE(cfg.player_inbox().consumer_enabled());
    EXPECT_EQ(cfg.player_inbox().periodic_fetch_minutes(), 0u) << "显式写 0 = 关闭兜底,必须原样保留";
}

// ---------------------------------------------------------------------------
// GameConfig.control_redis(docs/design/redis-cluster-all-domains-20261110.md §4.2、§5.2)
// 整段或单键缺失 = standalone、127.0.0.1:6379、不认证(同 Go 各服务 ControlRedis 的缺省)。
// zoneredis / scene_control_redis 两个旧段 2026-11-12 起不再读。断言不打印口令值(AGENTS.md §11.3)。
// ---------------------------------------------------------------------------

TEST(GameConfigControlRedis, ShippedYamlIsLocalStandalone)
{
    GameConfig cfg;
    ClearEnvVar("CONTROL_REDIS_PASSWORD");
    ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg));
    EXPECT_EQ(cfg.control_redis().mode(), "standalone");
    ASSERT_EQ(cfg.control_redis().seeds_size(), 1);
    EXPECT_EQ(cfg.control_redis().seeds(0), "127.0.0.1:6379");
}

TEST(GameConfigControlRedis, MissingSectionFallsBackToLocalStandalone)
{
    const auto path = WriteTempGameConfig("game_config_control_redis_missing.yaml", "");
    GameConfig cfg;
    ClearEnvVar("CONTROL_REDIS_PASSWORD");
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded) << "gate、battle 不连 Redis,缺这一段不能拒绝加载";
    EXPECT_EQ(cfg.control_redis().mode(), "standalone");
    ASSERT_EQ(cfg.control_redis().seeds_size(), 1);
    EXPECT_EQ(cfg.control_redis().seeds(0), "127.0.0.1:6379");
    EXPECT_TRUE(cfg.control_redis().password().empty());
}

TEST(GameConfigControlRedis, LegacySectionsAreNotRead)
{
    // 按旧形状渲染的配置:旧段里的地址不会被当成控制集群的地址(scene 连缺省地址,并打一条 WARN)。
    const auto path = WriteTempGameConfig("game_config_control_redis_legacy.yaml",
                                          "zoneredis:\n"
                                          "  host: \"10.9.9.9\"\n"
                                          "  port: 6380\n"
                                          "scene_control_redis:\n"
                                          "  host: \"10.9.9.8\"\n"
                                          "  port: 6381\n");
    GameConfig cfg;
    ClearEnvVar("CONTROL_REDIS_PASSWORD");
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    ASSERT_EQ(cfg.control_redis().seeds_size(), 1);
    EXPECT_EQ(cfg.control_redis().seeds(0), "127.0.0.1:6379");
}

TEST(GameConfigControlRedis, ExplicitClusterKeepsItsSeeds)
{
    const auto path = WriteTempGameConfig("game_config_control_redis_cluster.yaml",
                                          "control_redis:\n"
                                          "  mode: cluster\n"
                                          "  seeds: [\"10.0.1.1:7100\", \"redis-control-1.infra:7100\"]\n");
    GameConfig cfg;
    ClearEnvVar("CONTROL_REDIS_PASSWORD");
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_EQ(cfg.control_redis().mode(), "cluster");
    ASSERT_EQ(cfg.control_redis().seeds_size(), 2);
    EXPECT_EQ(cfg.control_redis().seeds(0), "10.0.1.1:7100");
    EXPECT_EQ(cfg.control_redis().seeds(1), "redis-control-1.infra:7100");
}

TEST(GameConfigControlRedis, ClusterWithoutSeedsStaysEmptyForTheConsumerToReject)
{
    // cluster 不写种子不能悄悄落到本机单实例:由 scene 启动时拒绝。
    const auto path = WriteTempGameConfig("game_config_control_redis_no_seeds.yaml", "control_redis:\n  mode: cluster\n");
    GameConfig cfg;
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_EQ(cfg.control_redis().mode(), "cluster");
    EXPECT_EQ(cfg.control_redis().seeds_size(), 0);
}

// ---------------------------------------------------------------------------
// GameConfig.player_redis(docs/design/redis-player-store-cluster.md §5.1、§5.6)
// 整段缺失 = 与 control_redis 同一个实例或集群(模式、种子与口令都沿用),开发环境不用改配置;
// 写了这一段就是另一个实例或集群,种子与口令只认这一段。断言不打印口令值(AGENTS.md §11.3)。
// ---------------------------------------------------------------------------

TEST(GameConfigPlayerRedis, ShippedYamlFollowsControlRedis)
{
    GameConfig cfg;
    ClearEnvVar("PLAYER_REDIS_PASSWORD");
    ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg));
    EXPECT_EQ(cfg.player_redis().mode(), cfg.control_redis().mode());
    ASSERT_EQ(cfg.player_redis().seeds_size(), 1);
    EXPECT_EQ(cfg.player_redis().seeds(0), cfg.control_redis().seeds(0));
}

TEST(GameConfigPlayerRedis, MissingSectionFollowsControlRedisIncludingPassword)
{
    constexpr const char *kControlPassword = "control-pass-01";
    const auto path = WriteTempGameConfig("game_config_player_redis_missing.yaml",
                                          "control_redis:\n"
                                          "  mode: cluster\n"
                                          "  seeds: [\"10.0.1.1:7100\"]\n");
    GameConfig cfg;
    ClearEnvVar("PLAYER_REDIS_PASSWORD");
    SetEnvVar("CONTROL_REDIS_PASSWORD", kControlPassword);
    const bool loaded = readGameConfig(path.string(), cfg);
    ClearEnvVar("CONTROL_REDIS_PASSWORD");
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_EQ(cfg.player_redis().mode(), "cluster") << "没配 player_redis 时与控制集群是同一个集群,模式也沿用";
    ASSERT_EQ(cfg.player_redis().seeds_size(), 1);
    EXPECT_EQ(cfg.player_redis().seeds(0), "10.0.1.1:7100");
    EXPECT_TRUE(cfg.player_redis().password() == kControlPassword)
        << "没配 player_redis 时口令沿用 control_redis(含环境变量覆盖之后的值)";
}

TEST(GameConfigPlayerRedis, ExplicitClusterKeepsItsOwnSeedsAndDoesNotInheritPassword)
{
    const auto path = WriteTempGameConfig("game_config_player_redis_cluster.yaml",
                                          "player_redis:\n"
                                          "  mode: cluster\n"
                                          "  seeds: [\"10.0.0.1:7000\", \"redis-1.infra:7000\"]\n");
    GameConfig cfg;
    ClearEnvVar("PLAYER_REDIS_PASSWORD");
    SetEnvVar("CONTROL_REDIS_PASSWORD", "control-pass-02");
    const bool loaded = readGameConfig(path.string(), cfg);
    ClearEnvVar("CONTROL_REDIS_PASSWORD");
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_EQ(cfg.player_redis().mode(), "cluster");
    ASSERT_EQ(cfg.player_redis().seeds_size(), 2);
    EXPECT_EQ(cfg.player_redis().seeds(0), "10.0.0.1:7000");
    EXPECT_EQ(cfg.player_redis().seeds(1), "redis-1.infra:7000");
    EXPECT_TRUE(cfg.player_redis().password().empty())
        << "显式配了种子就是另一个集群,不能继承 control_redis 的口令";
}

TEST(GameConfigPlayerRedis, ClusterWithoutSeedsStaysEmptyForTheConsumerToReject)
{
    // 不能悄悄回落到 control_redis:cluster 指向别处是部署错误,由 scene 启动时拒绝。
    const auto path = WriteTempGameConfig("game_config_player_redis_no_seeds.yaml", "player_redis:\n  mode: cluster\n");
    GameConfig cfg;
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_EQ(cfg.player_redis().mode(), "cluster");
    EXPECT_EQ(cfg.player_redis().seeds_size(), 0);
}

TEST(GameConfigPlayerRedis, StandaloneSectionWithoutSeedsDoesNotFallBackToControlRedis)
{
    // 写了这一段就是另一个实例:standalone 也不回落到 control_redis(原来会回落到 zoneredis),种子留空,由 scene 启动时拒绝。
    const auto path = WriteTempGameConfig("game_config_player_redis_standalone_no_seeds.yaml",
                                          "player_redis:\n  mode: standalone\n");
    GameConfig cfg;
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_EQ(cfg.player_redis().mode(), "standalone");
    EXPECT_EQ(cfg.player_redis().seeds_size(), 0);
}

TEST(GameConfigPlayerRedis, PasswordFromEnvOverridesYaml)
{
    constexpr const char *kEnvPassword = "player-env-pass-01";
    const auto path = WriteTempGameConfig("game_config_player_redis_env.yaml",
                                          "player_redis:\n"
                                          "  mode: cluster\n"
                                          "  seeds: [\"10.0.0.1:7000\"]\n"
                                          "  password: \"from-yaml\"\n");
    GameConfig cfg;
    SetEnvVar("PLAYER_REDIS_PASSWORD", kEnvPassword);
    const bool loaded = readGameConfig(path.string(), cfg);
    ClearEnvVar("PLAYER_REDIS_PASSWORD");
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_TRUE(cfg.player_redis().password() == kEnvPassword)
        << "PLAYER_REDIS_PASSWORD must win over the yaml player_redis.password";
}

// ---------------------------------------------------------------------------
// Lookup 守卫宏
// ---------------------------------------------------------------------------
// 回归:LookupXxxOrContinue 曾把 continue 包在 do{...}while(0) 里。continue 绑定的是
// 那个 do-while 自身,跳去求值 while(0) 判假即退出,控制流直接落到宏后面 —— 守卫
// 沦为空操作。全仓 12 处调用点(bag / mission / buff / attribute)都在宏之后立刻
// 解引用注入的行指针,所以配置表掉一行就是空指针崩溃,而不是"跳过这一条"。
//
// 两个用例互为对照:只钉"缺行要跳过",宏恒真跳过也能绿;只钉"有行不跳过",
// 宏恒假不跳过也能绿。必须成对存在。

TEST(LookupGuardMacro, OrContinueSkipsIterationWhenRowMissing)
{
	constexpr uint32_t kMissingId = 0xFFFFFFFFu;
	ASSERT_EQ(TestTableManager::Instance().FindByIdSilent(kMissingId).first, nullptr)
		<< "前提失效:id=" << kMissingId << " 不该存在于 Test 表";

	int reachedBody = 0;
	for (int i = 0; i < 1; ++i)
	{
		LookupTestOrContinue(kMissingId);
		// 只有守卫失效才会走到这里。此时 testRow 就是调用方紧接着要解引用的那个空指针。
		EXPECT_EQ(testRow, nullptr);
		++reachedBody;
	}
	EXPECT_EQ(reachedBody, 0)
		<< "守卫没有跳过迭代 —— continue 大概率又被包回 do{...}while(0) 了";
}

TEST(LookupGuardMacro, OrContinueDoesNotSkipWhenRowPresent)
{
	constexpr uint32_t kPresentId = 1u;
	ASSERT_NE(TestTableManager::Instance().FindByIdSilent(kPresentId).first, nullptr)
		<< "前提失效:Test 表缺 id=" << kPresentId;

	int reachedBody = 0;
	for (int i = 0; i < 1; ++i)
	{
		LookupTestOrContinue(kPresentId);
		EXPECT_EQ(testRow->id(), kPresentId);
		++reachedBody;
	}
	EXPECT_EQ(reachedBody, 1);
}

int main(int argc, char **argv)
{
	::testing::InitGoogleTest(&argc, argv);
	if (!test_config::FindAndLoadTestConfig(argc, argv))
		return 1;
	LoadTables();
	return RUN_ALL_TESTS();
}

TEST(GameConfigAuditSnapshot, ShippedYamlCapturesEverything)
{
    GameConfig cfg;
    ASSERT_TRUE(readGameConfig("etc/game_config.yaml", cfg));
    EXPECT_TRUE(cfg.audit_snapshot().login_enabled());
    EXPECT_TRUE(cfg.audit_snapshot().logout_enabled());
    EXPECT_EQ(cfg.audit_snapshot().sample_per_mille(), 1000u);
}

TEST(GameConfigAuditSnapshot, MissingSectionFallsBackToFullCapture)
{
    const auto path = WriteTempGameConfig("game_config_audit_snapshot_missing.yaml", "");
    GameConfig cfg;
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_TRUE(cfg.audit_snapshot().login_enabled());
    EXPECT_TRUE(cfg.audit_snapshot().logout_enabled());
    EXPECT_EQ(cfg.audit_snapshot().sample_per_mille(), 1000u) << "缺段必须显式取全量,不能落到 proto3 的 0(= 一个都不发)";
}

TEST(GameConfigAuditSnapshot, SectionOverridesAndRejectsOutOfRange)
{
    const auto path = WriteTempGameConfig("game_config_audit_snapshot_partial.yaml",
                                          "audit_snapshot:\n  login_enabled: false\n  sample_per_mille: 250\n");
    GameConfig cfg;
    const bool loaded = readGameConfig(path.string(), cfg);
    RemoveTempGameConfig(path);
    ASSERT_TRUE(loaded);
    EXPECT_FALSE(cfg.audit_snapshot().login_enabled());
    EXPECT_TRUE(cfg.audit_snapshot().logout_enabled()) << "缺键取默认";
    EXPECT_EQ(cfg.audit_snapshot().sample_per_mille(), 250u);

    const auto bad = WriteTempGameConfig("game_config_audit_snapshot_bad.yaml", "audit_snapshot:\n  sample_per_mille: 1001\n");
    GameConfig rejected;
    const bool badLoaded = readGameConfig(bad.string(), rejected);
    RemoveTempGameConfig(bad);
    EXPECT_FALSE(badLoaded) << "超过 1000 的抽样比必须拒绝加载";
}

TEST(AuditSnapshotPolicy, TriggerSwitchesAndStableSampling)
{
    GameConfig::AuditSnapshotConfig cfg;
    cfg.set_login_enabled(false);
    cfg.set_logout_enabled(true);
    cfg.set_sample_per_mille(1000);
    EXPECT_FALSE(audit_snapshot_policy::ShouldCapture(cfg, SNAPSHOT_LOGIN, 42));
    EXPECT_TRUE(audit_snapshot_policy::ShouldCapture(cfg, SNAPSHOT_LOGOUT, 42));
    EXPECT_TRUE(audit_snapshot_policy::ShouldCapture(cfg, SNAPSHOT_GM_MANUAL, 42)) << "按需快照不受开关影响";

    cfg.set_login_enabled(true);
    cfg.set_sample_per_mille(0);
    EXPECT_FALSE(audit_snapshot_policy::ShouldCapture(cfg, SNAPSHOT_LOGIN, 42));
    EXPECT_TRUE(audit_snapshot_policy::ShouldCapture(cfg, SNAPSHOT_PRE_TRADE, 42)) << "抽样不影响按需快照";

    cfg.set_sample_per_mille(300);
    uint32_t sampled = 0;
    for (uint64_t player = 1; player <= 10000; ++player)
    {
        const bool login = audit_snapshot_policy::ShouldCapture(cfg, SNAPSHOT_LOGIN, player);
        EXPECT_EQ(login, audit_snapshot_policy::ShouldCapture(cfg, SNAPSHOT_LOGOUT, player)) << "同一玩家进退场要成对";
        sampled += login ? 1 : 0;
    }
    EXPECT_GT(sampled, 2700u);
    EXPECT_LT(sampled, 3300u);
}
