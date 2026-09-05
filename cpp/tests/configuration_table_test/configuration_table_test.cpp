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

#include <cstdlib>
#include <cstring>

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

int main(int argc, char **argv)
{
	::testing::InitGoogleTest(&argc, argv);
	if (!test_config::FindAndLoadTestConfig(argc, argv))
		return 1;
	LoadTables();
	return RUN_ALL_TESTS();
}
