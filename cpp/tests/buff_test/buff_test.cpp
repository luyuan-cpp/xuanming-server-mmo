// BuffSystem 周期回调期间改写 buff 列表的回归用例。
//
// BuffSystem::OnIntervalThink 依次调三路 impl 的周期回调。第一路 BuffImplSystem 会顺着
// TickCombatIdleBuff -> AddSubBuffs -> AddOrUpdateBuff 往同一张 BuffListComp(unordered_map)
// 里插子 buff(可能扩容),子 buff 的 DispelBuffsOnAwake 还可能把发起 tick 的这条 buff 自己驱散掉。
// 旧写法在三路之间一直端着同一个 buffIt:扩容后迭代器失效,驱散后解引用的是已释放节点。
//
// 配表用夹具自带的 buff.json(写进临时目录再 Load),不依赖 bin/ 下的真实配表 ——
// 真实配表里目前没有能触发这条路径的组合(PROGRESS 2026-10-14 四路检查 P2)。
//
// 另有两组 entt 误用审计(docs/design/ecs-entt-misuse-audit-20261015.md)的回归用例:
// - BuffStackingTest(C003):no_caster 的 buff 跨施法者叠层、按施法者隔离的 buff 各占一条,
//   层数从 1 起算、封顶 max_layer(新条目层数与回合引擎 StackOrRefreshExistingBuff 一致);
// - BuffListLifecycleTest(F037):BuffListComp 是按需状态 —— 只在第一次真正插入条目时创建,
//   帧末列表空了即摘掉；被拒绝 / 纯驱散 / 叠层判断不补空表。

#include <gtest/gtest.h>

#include <cstdint>
#include <filesystem>
#include <fstream>
#include <initializer_list>
#include <ranges>
#include <string>
#include <system_error>

#include <google/protobuf/unknown_field_set.h>

#include "combat/buff/comp/buff_comp.h"
#include "combat/buff/constants/buff.h"
#include "combat/buff/system/buff.h"
#include "combat_state/constants/combat_state.h"
#include "muduo/net/EventLoop.h"
#include "proto/common/component/buff_comp.pb.h"
#include "proto/common/component/actor_combat_state_comp.pb.h"
#include "proto/common/component/battle_comp.pb.h"
#include "proto/common/component/actor_comp.pb.h"  // BaseAttributesComp / LevelComp:回血读点用例
#include "proto/common/component/actor_attribute_state_comp.pb.h"  // DerivedAttributesComp(max_health)
#include "proto/common/component/player_comp.pb.h"  // Player:玩家标签
#include "player/comp/player_frozen_comp.h"
#include "player/system/player_level_rules.h"  // playerlevel::kMaxLevel
#include "table/code/buff_table.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "thread_context/ecs_context.h"
#include "time/system/time.h"
#include <node_config_manager.h>

namespace
{
namespace fs = std::filesystem;

// 夹具配表 id 段,与真实配表无关(测试进程只加载下面这份 buff.json)。
constexpr uint32_t kOwnerDispelledBySub = 9001;  // 周期 tick 加子 buff,子 buff 反手驱散它
constexpr uint32_t kDispelCombatIdle = 9002;     // 纯驱散 buff,驱散带 combat_idle 标签的 buff
constexpr uint32_t kOwnerWithManySubs = 9003;    // 周期 tick 一次加 16 个子 buff,撑爆桶数
constexpr uint32_t kPlainSubBuff = 9004;         // 无任何回调副作用的普通子 buff
constexpr uint32_t kNoCasterStackable = 9005;    // no_caster=1、max_layer=3:跨施法者叠层
constexpr uint32_t kCasterScopedStackable = 9006; // no_caster=0、max_layer=3:按施法者隔离叠层
constexpr uint32_t kImmuneToBlockedTag = 9007;
constexpr uint32_t kBlockedByImmunity = 9008;
constexpr uint32_t kStackableMaxLayer = 3;
constexpr uint32_t kInfiniteZeroDuration = 9010;
constexpr uint32_t kInfinitePositiveDuration = 9011;
constexpr uint32_t kInstantDuration = 9012;
constexpr uint32_t kFiniteDuration = 9013;
constexpr uint32_t kIdleBonusOwner = 9014;
constexpr uint32_t kOwnerWithInstantChild = 9015;
constexpr uint32_t kOwnerDispelledByPersistentChild = 9016;
constexpr uint32_t kOwnerWithDispelThenPlainChild = 9018;
constexpr uint32_t kOtherIdleBonusOwner = 9019;
constexpr uint32_t kNoCasterBonusOwner = 9020;
constexpr uint32_t kOtherNoCasterBonusOwner = 9021;
constexpr uint32_t kNextAttackDispelledByChild = 9022;
constexpr uint32_t kPermanentSilence = 9026;
constexpr uint32_t kHealthRegenerationByLevel = 9027;  // 按已损气血回血(buff_type 42),每跳回已损气血的 level%
// 下线保留(keep_on_logout=1)的夹具行,BuffKeepOnLogoutTest 用。
constexpr uint32_t kKeptTimed = 9030;             // 600 秒、max_layer=3
constexpr uint32_t kKeptPermanent = 9031;         // 永久
constexpr uint32_t kIdleOwnerOfKeptChild = 9032;  // 不保留的脱战父,加成子项是 9031
constexpr uint32_t kKeptImmuneToKeptTag = 9033;   // 免疫 kept_blocked 标签
constexpr uint32_t kKeptWithBlockedTag = 9034;    // 带 kept_blocked 标签
constexpr uint64_t kKeptTimedDurationMs = 600000;
constexpr int kManySubBuffCount = 16;

// buff_type 一律显式写:缺省 0 是 kBuffTypeMovementSpeedReduction,会顺带去标记属性重算。
// duration 取 -1(永久),不走到期定时器分支。
// 脱战窗口取 5 秒，用例以创建时记录的单调时间为锚推进，不读墙钟、不等待真实时间。
std::string FixtureBuffTableJson()
{
	std::string manySubs;
	for (int i = 0; i < kManySubBuffCount; ++i)
	{
		manySubs += (i == 0 ? "" : ",") + std::to_string(kPlainSubBuff);
	}

	return R"({"data":[)"
		R"({"id":9001,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,)"
		R"("tag":{"combat_idle":true},"sub_buff":[9002]},)"
		R"({"id":9002,"buff_type":35,"duration":-1,"dispel_tag":{"combat_idle":true}},)"
		R"({"id":9003,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,)"
		R"("sub_buff":[)" + manySubs + R"(]},)"
		R"({"id":9004,"buff_type":10,"duration":-1},)"
		R"({"id":9005,"buff_type":10,"duration":-1,"no_caster":1,"max_layer":3},)"
		R"({"id":9006,"buff_type":10,"duration":-1,"max_layer":3},)"
		R"({"id":9010,"buff_type":10,"duration":0,"infinite_duration":1},)"
		R"({"id":9011,"buff_type":10,"duration":60,"infinite_duration":2},)"
		R"({"id":9012,"buff_type":10,"duration":0},)"
		R"({"id":9013,"buff_type":10,"duration":60},)"
		R"({"id":9014,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,"sub_buff":[9010]},)"
		R"({"id":9015,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,"sub_buff":[9012]},)"
		R"({"id":9016,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,)"
		R"("tag":{"combat_idle":true},"sub_buff":[9017]},)"
		R"({"id":9017,"buff_type":10,"duration":0,"infinite_duration":1,"dispel_tag":{"combat_idle":true}},)"
		R"({"id":9018,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,)"
		R"("tag":{"combat_idle":true},"sub_buff":[9002,9004]},)"
		R"({"id":9019,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,"sub_buff":[9010]},)"
		R"({"id":9020,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,"sub_buff":[9005]},)"
		R"({"id":9021,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,"sub_buff":[9005]},)"
		R"({"id":9022,"buff_type":36,"duration":-1,"tag":{"combat_idle":true},"sub_buff":[9017]},)"
		R"({"id":9026,"buff_type":31,"duration":-1,"max_layer":1},)"
		R"({"id":9027,"buff_type":42,"duration":0,"infinite_duration":1,"interval":1,"health_regeneration":"level*health/100"},)"
		R"({"id":9030,"buff_type":10,"duration":600,"max_layer":3,"keep_on_logout":1},)"
		R"({"id":9031,"buff_type":10,"duration":-1,"keep_on_logout":1},)"
		R"({"id":9032,"buff_type":43,"duration":-1,"interval":1,"combat_idle_seconds":5,"sub_buff":[9031]},)"
		R"({"id":9033,"buff_type":10,"duration":-1,"keep_on_logout":1,"immune_tag":{"kept_blocked":true}},)"
		R"({"id":9034,"buff_type":10,"duration":-1,"keep_on_logout":1,"tag":{"kept_blocked":true}},)"
		R"({"id":9007,"buff_type":10,"duration":-1,"immune_tag":{"blocked":true}},)"
		R"({"id":9008,"buff_type":10,"duration":-1,"tag":{"blocked":true}})"
		R"(]})";
}

// Buff 仅保存施法者整数，不持有施法上下文。
uint64_t ContextFromCaster(entt::entity caster)
{
	return entt::to_integral(caster);
}

uint64_t AddBuffOrFail(entt::entity actor, uint32_t buffTableId)
{
	const auto [result, buffId] = BuffSystem::AddOrUpdateBuff(actor, buffTableId);
	EXPECT_EQ(kSuccess, result);
	EXPECT_NE(UINT64_MAX, buffId);
	return buffId;
}

// 只把创建/受击入口已记录的时间作为锚，测试通过显式时间入口推进整个等待窗口。
uint64_t IdleWindowStartedAt(entt::entity actor, uint64_t ownerId)
{
	const auto& entry = tlsEcs.actorRegistry.get<BuffListComp>(actor).at(ownerId);
	const auto data = std::dynamic_pointer_cast<BuffNoDamageOrSkillHitInLastSecondsComp>(entry.dataPbPtr);
	EXPECT_NE(nullptr, data);
	return data ? data->last_time() : 0;
}
} // namespace

// 各组用例共用的配表夹具:每个 suite 开始时写入并加载上面的 buff.json(Load 是整份快照替换,重复加载无副作用)。
class BuffTableFixture : public ::testing::Test
{
protected:
	static void SetUpTestSuite()
	{
		tableDir = fs::temp_directory_path() / "mmorpg_buff_test_tables";
		std::error_code ec;
		fs::create_directories(tableDir, ec);
		ASSERT_FALSE(ec) << ec.message();
		{
			std::ofstream out(tableDir / "buff.json", std::ios::binary | std::ios::trunc);
			ASSERT_TRUE(out.is_open());
			out << FixtureBuffTableJson();
		}

		auto &deployConfig = tlsNodeConfigManager.GetBaseDeployConfig();
		deployConfig.set_table_data_format("json");
		deployConfig.set_table_data_directory((tableDir / "").string());
		BuffTableManager::Instance().Load();
		ASSERT_TRUE(BuffTableManager::Instance().Exists(kOwnerDispelledBySub));
		ASSERT_TRUE(BuffTableManager::Instance().Exists(kOwnerWithManySubs));
		ASSERT_TRUE(BuffTableManager::Instance().Exists(kNoCasterStackable));
		ASSERT_TRUE(BuffTableManager::Instance().Exists(kCasterScopedStackable));
	}

	static void TearDownTestSuite()
	{
		std::error_code ec;
		fs::remove_all(tableDir, ec);
	}

	void TearDown() override
	{
		tlsEcs.actorRegistry.clear();
	}

	static inline fs::path tableDir;
};

class BuffIntervalThinkTest : public BuffTableFixture {};
class BuffStackingTest : public BuffTableFixture {};
class BuffListLifecycleTest : public BuffTableFixture {};

// 子 buff 在第一路回调里把发起 tick 的 buff 驱散掉:后两路回调必须停手,
// 不能再拿旧迭代器去解引用已 erase 的节点(Debug 下 MSVC 的迭代器检查会直接断言)。
TEST_F(BuffIntervalThinkTest, SubBuffDispelsOwner_StopsBeforeRemainingHandlers)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto ownerId = AddBuffOrFail(actor, kOwnerDispelledBySub);

	BuffSystem::OnIntervalThink(actor, ownerId, IdleWindowStartedAt(actor, ownerId) + 5000);

	const auto &buffList = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	EXPECT_FALSE(buffList.contains(ownerId));
	EXPECT_TRUE(buffList.empty()) << "纯驱散子 buff 不应留在列表里";
}

// 同一场景走每帧入口:UpdatePeriodicBuff 在回调后重查,owner 没了就不再写回计时器。
// 驱散之后身上一条 buff 都不剩,帧末连同 BuffListComp 一起摘掉(按需状态的结束)。
TEST_F(BuffIntervalThinkTest, SubBuffDispelsOwner_ViaFrameUpdate)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto ownerId = AddBuffOrFail(actor, kOwnerDispelledBySub);

	BuffSystem::Update(1.0, IdleWindowStartedAt(actor, ownerId) + 5000);  // 恰好 tick 一次

	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<BuffListComp>(actor));
}

// 子 buff 插入让 unordered_map 扩容:owner 仍在,且 16 个子 buff 全部登记进 owner 的
// sub_buff_list_id、全部真实存在于列表中。
//
// MSVC 的 unordered_map 扩容不会让节点迭代器失效,所以旧写法在本平台上这条用例也是绿的;
// 它守的是"回调中途扩容"这一场景的可观察结果,真正红/绿取决于上面的驱散用例。
TEST_F(BuffIntervalThinkTest, SubBuffsRehashList_OwnerRecordsEverySubBuff)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto ownerId = AddBuffOrFail(actor, kOwnerWithManySubs);
	const auto bucketsBefore = tlsEcs.actorRegistry.get<BuffListComp>(actor).bucket_count();

	BuffSystem::OnIntervalThink(actor, ownerId, IdleWindowStartedAt(actor, ownerId) + 5000);

	const auto &buffList = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	ASSERT_GT(buffList.bucket_count(), bucketsBefore) << "夹具没能触发扩容,用例失去意义";
	ASSERT_TRUE(buffList.contains(ownerId));

	const auto &owner = buffList.at(ownerId).buffPb;
	EXPECT_TRUE(owner.has_added_sub_buff());
	EXPECT_EQ(static_cast<int>(kManySubBuffCount), owner.sub_buff_list_id_size());
	EXPECT_EQ(static_cast<std::size_t>(kManySubBuffCount + 1), buffList.size());
	for (const auto &[subBuffId, activated] : owner.sub_buff_list_id())
	{
		EXPECT_TRUE(buffList.contains(subBuffId)) << "sub buff " << subBuffId;
		EXPECT_FALSE(activated);
	}
}

// no_caster 的 buff 不区分施法者:不同施法者(以及没有施法者时)反复挂,始终只有一条,
// 层数从 1 起、封顶 max_layer。
// 修复前 processed_caster 存的是 entt::null,叠层时与施法者整数比较永远不等;没有施法者时
// 又直接不叠 —— 每挂一次都新建一条,列表里会有 4 条(C003)。
TEST_F(BuffStackingTest, NoCasterBuff_StacksAcrossCastersUpToMaxLayer)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto casterA = ContextFromCaster(tlsEcs.actorRegistry.create());
	const auto casterB = ContextFromCaster(tlsEcs.actorRegistry.create());

	const auto [firstResult, firstId] = BuffSystem::AddOrUpdateBuff(actor, kNoCasterStackable, casterA);
	ASSERT_EQ(kSuccess, firstResult);
	ASSERT_NE(UINT64_MAX, firstId);

	for (const auto &context : {casterB, kNoBuffCaster, casterA})
	{
		const auto [result, buffId] = BuffSystem::AddOrUpdateBuff(actor, kNoCasterStackable, context);
		EXPECT_EQ(kSuccess, result);
		EXPECT_EQ(UINT64_MAX, buffId) << "应叠到已有条目上,而不是新建";
	}

	const auto &buffList = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	ASSERT_EQ(1u, buffList.size());
	ASSERT_TRUE(buffList.contains(firstId));
	const auto &stacked = buffList.at(firstId).buffPb;
	EXPECT_EQ(kStackableMaxLayer, stacked.layer()) << "施加 4 次,层数应封顶在 max_layer";
	EXPECT_EQ(0u, stacked.processed_caster()) << "no_caster 统一记 0(无施法者)";
	EXPECT_EQ(casterA, stacked.caster()) << "caster 仍记录首次施加者";
}

// 按施法者隔离的 buff:同一施法者叠层,不同施法者各占一条;新条目是第 1 层。
// 修复前新条目从第 0 层起算,同一施法者挂两次只有 1 层,max_layer=N 实际能挂 N+1 次。
TEST_F(BuffStackingTest, CasterScopedBuff_StacksPerCasterFromLayerOne)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto casterA = ContextFromCaster(tlsEcs.actorRegistry.create());
	const auto casterB = ContextFromCaster(tlsEcs.actorRegistry.create());

	const auto [resultA, buffIdA] = BuffSystem::AddOrUpdateBuff(actor, kCasterScopedStackable, casterA);
	ASSERT_EQ(kSuccess, resultA);
	ASSERT_NE(UINT64_MAX, buffIdA);

	const auto [restackResult, restackId] = BuffSystem::AddOrUpdateBuff(actor, kCasterScopedStackable, casterA);
	EXPECT_EQ(kSuccess, restackResult);
	EXPECT_EQ(UINT64_MAX, restackId) << "同一施法者应叠层";

	const auto [resultB, buffIdB] = BuffSystem::AddOrUpdateBuff(actor, kCasterScopedStackable, casterB);
	ASSERT_EQ(kSuccess, resultB);
	ASSERT_NE(UINT64_MAX, buffIdB) << "不同施法者应新建一条";

	const auto &buffList = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	ASSERT_EQ(2u, buffList.size());
	EXPECT_EQ(2u, buffList.at(buffIdA).buffPb.layer());
	EXPECT_EQ(1u, buffList.at(buffIdB).buffPb.layer());
	EXPECT_EQ(casterA, buffList.at(buffIdA).buffPb.processed_caster());
	EXPECT_EQ(casterB, buffList.at(buffIdB).buffPb.processed_caster());
}

TEST_F(BuffStackingTest, CasterScopedBuffWithoutCasterCreatesIndependentLayerOneEntries)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto [firstResult, firstId] = BuffSystem::AddOrUpdateBuff(actor, kCasterScopedStackable, kNoBuffCaster);
	const auto [secondResult, secondId] = BuffSystem::AddOrUpdateBuff(actor, kCasterScopedStackable, kNoBuffCaster);
	ASSERT_EQ(kSuccess, firstResult);
	ASSERT_EQ(kSuccess, secondResult);
	ASSERT_NE(UINT64_MAX, firstId);
	ASSERT_NE(UINT64_MAX, secondId);
	ASSERT_NE(firstId, secondId);
	const auto& buffs = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	ASSERT_EQ(2u, buffs.size());
	EXPECT_EQ(1u, buffs.at(firstId).buffPb.layer());
	EXPECT_EQ(1u, buffs.at(secondId).buffPb.layer());
	EXPECT_EQ(kNoBuffCaster, buffs.at(firstId).buffPb.caster());
	EXPECT_EQ(kNoBuffCaster, buffs.at(secondId).buffPb.caster());
}

// BuffListComp 是按需状态:身上还没有 buff 的实体(生产里所有刚初始化的 actor 都是这样)
// 第一次挂 buff 时就地创建列表,不能因为「缺组件」被拒。
// 守的是本批上一版的回退:那一版把它当必需组件、缺了返回 kServiceUnavailable,
// 而生产代码没有任何地方预先挂它 —— 玩家身上所有 buff 都会被拒。
TEST_F(BuffListLifecycleTest, AddOrUpdateBuff_FirstBuffCreatesBuffList)
{
	const auto actor = tlsEcs.actorRegistry.create();

	const auto [result, buffId] = BuffSystem::AddOrUpdateBuff(actor, kPlainSubBuff);

	ASSERT_EQ(kSuccess, result);
	ASSERT_NE(UINT64_MAX, buffId);
	const auto *buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(actor);
	ASSERT_NE(nullptr, buffList);
	EXPECT_TRUE(buffList->contains(buffId));
}

TEST_F(BuffListLifecycleTest, SilenceBuffAddsAndRemovesCombatStateWithoutEventWiring)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto [result, buffId] = BuffSystem::AddOrUpdateBuff(actor, kPermanentSilence);
	ASSERT_EQ(kSuccess, result);
	ASSERT_NE(UINT64_MAX, buffId);
	const auto* states = tlsEcs.actorRegistry.try_get<CombatStateCollectionComp>(actor);
	ASSERT_NE(nullptr, states);
	ASSERT_TRUE(states->states().contains(kActorCombatStateSilence));
	EXPECT_TRUE(states->states().at(kActorCombatStateSilence).sources().contains(buffId));

	BuffSystem::RemoveBuff(actor, buffId);

	states = tlsEcs.actorRegistry.try_get<CombatStateCollectionComp>(actor);
	ASSERT_NE(nullptr, states);
	EXPECT_FALSE(states->states().contains(kActorCombatStateSilence));
}

// 不产生新条目的施加(这里是纯驱散 buff)不得给实体凭空挂上一张空表。
// 修复前 AddOrUpdateBuff 在驱散判断之前就 get_or_emplace,纯驱散 buff 打在没有 buff 的实体上,
// 会留下一个空的 BuffListComp(F037)。
TEST_F(BuffListLifecycleTest, AddOrUpdateBuff_PureDispelOnBareEntity_DoesNotCreateBuffList)
{
	const auto bare = tlsEcs.actorRegistry.create();

	const auto [result, buffId] = BuffSystem::AddOrUpdateBuff(bare, kDispelCombatIdle);

	EXPECT_EQ(kSuccess, result);
	EXPECT_EQ(UINT64_MAX, buffId) << "纯驱散 buff 不落地";
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<BuffListComp>(bare));
}

// 叠层判断是只读路径,不得改变实体的组件集合。修复前它对 BuffListComp 是 get_or_emplace。
TEST_F(BuffListLifecycleTest, HandleExistingBuff_DoesNotCreateBuffList)
{
	const auto bare = tlsEcs.actorRegistry.create();
	const auto context = ContextFromCaster(tlsEcs.actorRegistry.create());

	EXPECT_FALSE(BuffSystem::HandleExistingBuff(bare, kCasterScopedStackable, context));
	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<BuffListComp>(bare));
}

TEST_F(BuffListLifecycleTest, ReadPathsOnBareEntityDoNotCreateBuffState)
{
	const auto bare = tlsEcs.actorRegistry.create();
	EXPECT_EQ(kSuccess, BuffSystem::CheckBuffCreation(bare, kPlainSubBuff));
	EXPECT_FALSE(BuffSystem::DispelBuffsOnAwake(bare, kPlainSubBuff));
	BuffSystem::OnIntervalThink(bare, 12345);
	BuffSystem::Update(0.0);
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<BuffListComp>(bare));
}

TEST_F(BuffListLifecycleTest, RemoveBuffSet_UnremovableEntryLeavesNoExtraStateAcrossFrames)
{
	constexpr uint64_t kBuffId = 900001;
	ASSERT_FALSE(BuffTableManager::Instance().Exists(UINT32_MAX));
	const auto actor = tlsEcs.actorRegistry.create();
	tlsEcs.actorRegistry.emplace<BuffListComp>(actor)[kBuffId].buffPb.set_buff_table_id(UINT32_MAX);

	BuffSystem::RemoveBuff(actor, UInt64Set{kBuffId});
	BuffSystem::Update(0.0);
	BuffSystem::Update(0.0);

	std::size_t componentPoolCount = 0;
	for (auto&& [componentType, pool] : tlsEcs.actorRegistry.storage()) {
		if (pool.contains(actor)) {
			++componentPoolCount;
		}
	}
	EXPECT_EQ(1u, componentPoolCount) << "不可移除的 Buff 只保留原列表，不留下额外待删状态";
	const auto* buffs = tlsEcs.actorRegistry.try_get<BuffListComp>(actor);
	ASSERT_NE(nullptr, buffs);
	EXPECT_TRUE(buffs->contains(kBuffId));
}

TEST_F(BuffListLifecycleTest, RejectedAddDoesNotCreateBuffList)
{
	const auto frozen = tlsEcs.actorRegistry.create();
	tlsEcs.actorRegistry.emplace<PlayerFrozenComp>(frozen);
	const auto inBattle = tlsEcs.actorRegistry.create();
	tlsEcs.actorRegistry.emplace<InBattleComp>(inBattle);
	const auto missingTable = tlsEcs.actorRegistry.create();

	EXPECT_EQ(UINT64_MAX, std::get<1>(BuffSystem::AddOrUpdateBuff(frozen, kPlainSubBuff)));
	EXPECT_EQ(UINT64_MAX, std::get<1>(BuffSystem::AddOrUpdateBuff(inBattle, kPlainSubBuff)));
	EXPECT_NE(kSuccess, std::get<0>(BuffSystem::AddOrUpdateBuff(missingTable, UINT32_MAX)));
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<BuffListComp>(frozen));
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<BuffListComp>(inBattle));
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<BuffListComp>(missingTable));
}

TEST_F(BuffListLifecycleTest, ImmuneAddLeavesExistingBuffListUnchanged)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto immunityId = AddBuffOrFail(actor, kImmuneToBlockedTag);
	const auto [result, buffId] = BuffSystem::AddOrUpdateBuff(actor, kBlockedByImmunity);
	EXPECT_NE(kSuccess, result);
	EXPECT_EQ(UINT64_MAX, buffId);
	const auto* buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(actor);
	ASSERT_NE(nullptr, buffList);
	EXPECT_EQ(1u, buffList->size());
	EXPECT_TRUE(buffList->contains(immunityId));
}

// 列表在 buff 移除的当场不摘(调用栈上可能还有人端着它),帧末发现空了才摘。
// 修复前从来不摘:挂过一次 buff 的实体永远带着一张空表,每帧都被 view 遍历。
TEST_F(BuffListLifecycleTest, Update_RemovesEmptiedBuffListAtFrameEnd)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto buffId = AddBuffOrFail(actor, kPlainSubBuff);

	BuffSystem::RemoveBuff(actor, buffId);
	const auto *buffList = tlsEcs.actorRegistry.try_get<BuffListComp>(actor);
	ASSERT_NE(nullptr, buffList) << "移除 buff 的当场不该摘列表";
	EXPECT_TRUE(buffList->empty());

	BuffSystem::Update(0.0);

	EXPECT_FALSE(tlsEcs.actorRegistry.all_of<BuffListComp>(actor)) << "列表空了应在帧末摘掉";
}

class BuffDurationTest : public BuffTableFixture {};

TEST_F(BuffDurationTest, InfiniteZeroDuration_RemainsUntilExplicitRemoval)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kInfiniteZeroDuration);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(actor).contains(id));
	if (tlsEcs.actorRegistry.get<BuffListComp>(actor).contains(id)) {
		BuffSystem::RemoveBuff(actor, id);
		EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(actor).empty());
	}
}

TEST_F(BuffDurationTest, InfinitePositiveDuration_DoesNotArmExpiryTimer)
{
	muduo::net::EventLoop loop;
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kInfinitePositiveDuration);
	EXPECT_FALSE(tlsEcs.actorRegistry.get<BuffListComp>(actor).at(id).expireTimerTaskComp.IsActive());
	BuffSystem::RemoveBuff(actor, id);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(actor).empty());
}

TEST_F(BuffDurationTest, ZeroDurationWithoutInfiniteFlag_StillExpiresImmediately)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kInstantDuration);
	EXPECT_FALSE(tlsEcs.actorRegistry.get<BuffListComp>(actor).contains(id));
}

TEST_F(BuffDurationTest, PositiveDurationWithoutInfiniteFlag_StillArmsExpiryTimer)
{
	muduo::net::EventLoop loop;
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kFiniteDuration);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(actor).at(id).expireTimerTaskComp.IsActive());
	BuffSystem::RemoveBuff(actor, id);
}

// 同一施法者再施加同一条是刷新:不另起一条,剩余时长回到表里的整时长(重挂满时,与回合引擎同一口径)。
// 没有 EventLoop 时定时器不挂,到期时刻照样在同一处更新。
TEST_F(BuffDurationTest, ReapplyingRefreshesTheExpiryToTheFullDuration)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto caster = tlsEcs.actorRegistry.create();
	const auto [first, id] = BuffSystem::AddOrUpdateBuff(actor, kFiniteDuration, ContextFromCaster(caster));
	ASSERT_EQ(kSuccess, first);
	ASSERT_NE(UINT64_MAX, id);
	auto& entry = tlsEcs.actorRegistry.get<BuffListComp>(actor).at(id);
	entry.expireAtMs = 1; // 当作快到期了

	const uint64_t before = TimeSystem::NowMillisecondsUTC();
	const auto [again, newId] = BuffSystem::AddOrUpdateBuff(actor, kFiniteDuration, ContextFromCaster(caster));
	const uint64_t after = TimeSystem::NowMillisecondsUTC();
	EXPECT_EQ(kSuccess, again);
	EXPECT_EQ(UINT64_MAX, newId) << "刷新,不另起一条";
	EXPECT_EQ(1u, tlsEcs.actorRegistry.get<BuffListComp>(actor).size());
	EXPECT_GE(entry.expireAtMs, before + 60000);
	EXPECT_LE(entry.expireAtMs, after + 60000);
}

class BuffCombatIdleTest : public BuffTableFixture {};

TEST_F(BuffCombatIdleTest, NewlyAddedBuff_DoesNotGrantBeforeIdleWindow)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kIdleBonusOwner);
	const auto start = IdleWindowStartedAt(actor, id);
	BuffSystem::OnIntervalThink(actor, id, start);
	BuffSystem::OnIntervalThink(actor, id, start + 4999);
	const auto& list = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	EXPECT_EQ(1u, list.size());
	EXPECT_FALSE(list.at(id).buffPb.has_added_sub_buff());
}

TEST_F(BuffCombatIdleTest, ExactIdleBoundary_GrantsPermanentBonusOnlyOnce)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kIdleBonusOwner);
	const auto start = IdleWindowStartedAt(actor, id);
	BuffSystem::OnIntervalThink(actor, id, start + 5000);
	const auto& list = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	ASSERT_EQ(2u, list.size());
	ASSERT_EQ(1, list.at(id).buffPb.sub_buff_list_id_size());
	const auto childId = list.at(id).buffPb.sub_buff_list_id().begin()->first;
	EXPECT_TRUE(list.contains(childId));
	BuffSystem::OnIntervalThink(actor, id, start + 5000);
	BuffSystem::OnIntervalThink(actor, id, start + 10000);
	EXPECT_EQ(2u, list.size());
	EXPECT_TRUE(list.contains(childId));
	EXPECT_EQ(1, list.at(id).buffPb.sub_buff_list_id_size());
}

TEST_F(BuffCombatIdleTest, FrameUpdate_GrantsOnlyAfterCompleteIdleWindow)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kIdleBonusOwner);
	const auto start = IdleWindowStartedAt(actor, id);
	BuffSystem::Update(1.0, start + 4999);
	EXPECT_EQ(1u, tlsEcs.actorRegistry.get<BuffListComp>(actor).size());
	BuffSystem::Update(1.0, start + 5000);
	EXPECT_EQ(2u, tlsEcs.actorRegistry.get<BuffListComp>(actor).size());
	BuffSystem::Update(5.0, start + 10000);
	EXPECT_EQ(2u, tlsEcs.actorRegistry.get<BuffListComp>(actor).size());
}

TEST_F(BuffCombatIdleTest, EarlierFrameTimestamp_DoesNotUnderflowIntoBonus)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kIdleBonusOwner);
	const auto start = IdleWindowStartedAt(actor, id);
	ASSERT_GT(start, 0u);
	BuffSystem::OnIntervalThink(actor, id, start - 1);
	EXPECT_EQ(1u, tlsEcs.actorRegistry.get<BuffListComp>(actor).size());
}

// 用公开的子 Buff 发放接口建立已获得加成的状态，单独检查受击、移除与级联生命周期。
void GrantBonusOrFail(entt::entity actor, uint64_t ownerId)
{
	auto& owner = tlsEcs.actorRegistry.get<BuffListComp>(actor).at(ownerId);
	const auto [row, result] = BuffTableManager::Instance().FindById(owner.buffPb.buff_table_id());
	ASSERT_EQ(kSuccess, result);
	ASSERT_NE(nullptr, row);
	EXPECT_TRUE(BuffSystem::AddSubBuffs(actor, row, owner));
}

TEST_F(BuffCombatIdleTest, SkillHit_StripsBonusAndAllowsNextIdleWindow)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto attacker = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kOwnerWithManySubs);
	BuffSystem::OnIntervalThink(actor, id, IdleWindowStartedAt(actor, id) + 5000);
	ASSERT_EQ(kManySubBuffCount + 1u, tlsEcs.actorRegistry.get<BuffListComp>(actor).size());
	BuffSystem::OnSkillHit(attacker, actor);
	const auto& list = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	EXPECT_EQ(1u, list.size());
	EXPECT_TRUE(list.at(id).buffPb.sub_buff_list_id().empty());
	EXPECT_FALSE(list.at(id).buffPb.has_added_sub_buff());
	const auto reset = IdleWindowStartedAt(actor, id);
	BuffSystem::OnIntervalThink(actor, id, reset + 4999);
	EXPECT_EQ(1u, list.size());
	BuffSystem::OnIntervalThink(actor, id, reset + 5000);
	EXPECT_EQ(kManySubBuffCount + 1u, tlsEcs.actorRegistry.get<BuffListComp>(actor).size());
}

TEST_F(BuffCombatIdleTest, TakingDamage_ResetsOnlyVictim)
{
	const auto attacker = tlsEcs.actorRegistry.create();
	const auto victim = tlsEcs.actorRegistry.create();
	const auto attackerId = AddBuffOrFail(attacker, kOwnerWithManySubs);
	const auto victimId = AddBuffOrFail(victim, kOwnerWithManySubs);
	GrantBonusOrFail(attacker, attackerId);
	GrantBonusOrFail(victim, victimId);
	DamageEventComp damage;
	damage.set_damage(1);
	BuffSystem::OnAfterTakeDamage(attacker, victim, damage);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(attacker).at(attackerId).buffPb.has_added_sub_buff());
	EXPECT_EQ(kManySubBuffCount + 1u, tlsEcs.actorRegistry.get<BuffListComp>(attacker).size());
	EXPECT_FALSE(tlsEcs.actorRegistry.get<BuffListComp>(victim).at(victimId).buffPb.has_added_sub_buff());
	EXPECT_EQ(1u, tlsEcs.actorRegistry.get<BuffListComp>(victim).size());
	const auto reset = IdleWindowStartedAt(victim, victimId);
	BuffSystem::OnIntervalThink(victim, victimId, reset + 4999);
	EXPECT_EQ(1u, tlsEcs.actorRegistry.get<BuffListComp>(victim).size());
	BuffSystem::OnIntervalThink(victim, victimId, reset + 5000);
	EXPECT_EQ(kManySubBuffCount + 1u, tlsEcs.actorRegistry.get<BuffListComp>(victim).size());
}

TEST_F(BuffCombatIdleTest, OwnerExpires_RemovesEveryBonusChild)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kOwnerWithManySubs);
	GrantBonusOrFail(actor, id);
	BuffSystem::OnBuffExpire(actor, id);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(actor).empty());
}

TEST_F(BuffCombatIdleTest, ChildExpires_DropsStaleIdWithoutRegranting)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kOwnerWithManySubs);
	GrantBonusOrFail(actor, id);
	const auto childId = tlsEcs.actorRegistry.get<BuffListComp>(actor).at(id).buffPb.sub_buff_list_id().begin()->first;
	BuffSystem::OnBuffExpire(actor, childId);
	const auto& owner = tlsEcs.actorRegistry.get<BuffListComp>(actor).at(id).buffPb;
	EXPECT_FALSE(owner.sub_buff_list_id().contains(childId));
	EXPECT_TRUE(owner.has_added_sub_buff());
	BuffSystem::OnIntervalThink(actor, id, IdleWindowStartedAt(actor, id) + 10000);
	EXPECT_EQ(kManySubBuffCount, tlsEcs.actorRegistry.get<BuffListComp>(actor).size());
	EXPECT_FALSE(owner.sub_buff_list_id().contains(childId));
}

TEST_F(BuffCombatIdleTest, InstantChild_IsNotRecordedAsLiveBonus)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kOwnerWithInstantChild);
	GrantBonusOrFail(actor, id);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(actor).at(id).buffPb.sub_buff_list_id().empty());
}

TEST_F(BuffCombatIdleTest, PersistentChildDispelsOwner_LeavesNoOrphan)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kOwnerDispelledByPersistentChild);
	GrantBonusOrFail(actor, id);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(actor).empty());
}

TEST_F(BuffCombatIdleTest, PureDispelChildRemovesOwner_StopsRemainingChildren)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto id = AddBuffOrFail(actor, kOwnerWithDispelThenPlainChild);
	GrantBonusOrFail(actor, id);
	EXPECT_TRUE(tlsEcs.actorRegistry.get<BuffListComp>(actor).empty());
}

// 两个来源的持续加成必须分别撤销，不能在同施法者或 no_caster 叠层时共享同一子条目。
void CheckIndependentBonusOwners(uint32_t firstTable, uint32_t secondTable)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto context = ContextFromCaster(tlsEcs.actorRegistry.create());
	const auto [firstResult, first] = BuffSystem::AddOrUpdateBuff(actor, firstTable, context);
	const auto [secondResult, second] = BuffSystem::AddOrUpdateBuff(actor, secondTable, context);
	ASSERT_EQ(kSuccess, firstResult);
	ASSERT_EQ(kSuccess, secondResult);
	BuffSystem::OnIntervalThink(actor, first, IdleWindowStartedAt(actor, first) + 5000);
	BuffSystem::OnIntervalThink(actor, second, IdleWindowStartedAt(actor, second) + 5000);
	const auto& list = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	ASSERT_EQ(4u, list.size());
	ASSERT_EQ(1, list.at(first).buffPb.sub_buff_list_id_size());
	ASSERT_EQ(1, list.at(second).buffPb.sub_buff_list_id_size());
	const auto survivingChild = list.at(second).buffPb.sub_buff_list_id().begin()->first;
	BuffSystem::RemoveBuff(actor, first);
	EXPECT_EQ(2u, list.size());
	EXPECT_TRUE(list.contains(second));
	EXPECT_TRUE(list.contains(survivingChild));
	BuffSystem::RemoveBuff(actor, second);
	EXPECT_TRUE(list.empty());
}

TEST_F(BuffCombatIdleTest, SameCasterOwners_KeepIndependentBonuses)
{
	CheckIndependentBonusOwners(kIdleBonusOwner, kOtherIdleBonusOwner);
}

TEST_F(BuffCombatIdleTest, NoCasterChildren_KeepIndependentBonuses)
{
	CheckIndependentBonusOwners(kNoCasterBonusOwner, kOtherNoCasterBonusOwner);
}

TEST_F(BuffCombatIdleTest, StandaloneBuffBeforeOrAfterBonus_KeepsItsOwnLifetime)
{
	for (const bool standaloneFirst : {true, false}) {
		const auto actor = tlsEcs.actorRegistry.create();
		const auto context = ContextFromCaster(tlsEcs.actorRegistry.create());
		uint64_t standaloneId = UINT64_MAX;
		if (standaloneFirst) {
			standaloneId = std::get<1>(BuffSystem::AddOrUpdateBuff(actor, kNoCasterStackable, context));
		}
		const auto owner = std::get<1>(BuffSystem::AddOrUpdateBuff(actor, kNoCasterBonusOwner, context));
		BuffSystem::OnIntervalThink(actor, owner, IdleWindowStartedAt(actor, owner) + 5000);
		if (!standaloneFirst) {
			standaloneId = std::get<1>(BuffSystem::AddOrUpdateBuff(actor, kNoCasterStackable, context));
		}
		const auto& list = tlsEcs.actorRegistry.get<BuffListComp>(actor);
		ASSERT_EQ(3u, list.size());
		BuffSystem::RemoveBuff(actor, owner);
		EXPECT_EQ(1u, list.size());
		EXPECT_TRUE(list.contains(standaloneId));
	}
}

TEST_F(BuffCombatIdleTest, NextAttackChildDispelsOwner_PreservesIndependentChild)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto owner = AddBuffOrFail(actor, kNextAttackDispelledByChild);
	GrantBonusOrFail(actor, owner);
	const auto& list = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	EXPECT_FALSE(list.contains(owner));
	ASSERT_EQ(1u, list.size());
	EXPECT_EQ(9017u, list.begin()->second.buffPb.buff_table_id());
}

TEST_F(BuffListLifecycleTest, LastBuffRemovalEndsOnDemandState)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto buffId = AddBuffOrFail(actor, kPlainSubBuff);
	BuffSystem::RemoveBuff(actor, UInt64Set{buffId});
	BuffSystem::Update(0.0);
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<BuffListComp>(actor));
}

// ---------------------------------------------------------------------------
// 按已损气血回血的等级读点(2026-11-05;混版本持久字段保护设计 §12.2,规则见 player_level_rules.h 文件头)
//
// 回血 buff 每跳把 playerlevel::CappedLevel(存储等级) 作 level 参数喂给回血表达式:超上限按上限,0 按 1;
// LevelComp 的存储值原样不动。"0 级按 1 级"是这一批的新行为:表达式直接吃 0 级时一滴不回。
// 真表 17 号回血 buff 是 "0.013*level*health",77 级起一跳就回满已损气血(0.013 × 77 > 1),85 级与 100 级
// 看不出差别;所以夹具行 9027 用同一类型、同一对参数(level、已损气血)的线性式 "level*health/100"。
// ---------------------------------------------------------------------------
class BuffHealthRegenerationLevelTest : public BuffTableFixture
{
protected:
	static constexpr uint64_t kMaxHealth = 100000;
	// 已损 10000:1 级一跳回 100,85 级回 8500,都回不满;若直读存储等级,0 级回 0、100 级回 10000(回满)。
	static constexpr uint64_t kHealth = 90000;
	// 回血这一路不看时间;时间只喂脱战计时那一路(BuffImplSystem),对 buff_type 42 不起作用。
	static constexpr uint64_t kAnyNowMs = 1000;

	// 夹具行在给定等级下的表达式值,与回血实现同一个取值入口。
	static double ExpressionHealAt(uint32_t level)
	{
		BuffTableManager::Instance().SetHealthRegenerationParam(
			{static_cast<double>(level), static_cast<double>(kMaxHealth - kHealth)});
		return BuffTableManager::Instance().GetHealthRegeneration(kHealthRegenerationByLevel);
	}

	// 只带气血组件、还没有等级组件的受伤实体;isPlayer 时挂上玩家标签。
	static entt::entity NewWounded(bool isPlayer)
	{
		const auto actor = tlsEcs.actorRegistry.create();
		if (isPlayer)
		{
			tlsEcs.actorRegistry.emplace<Player>(actor);
		}
		tlsEcs.actorRegistry.emplace<BaseAttributesComp>(actor).set_health(kHealth);
		tlsEcs.actorRegistry.emplace<DerivedAttributesComp>(actor).set_max_health(kMaxHealth);
		return actor;
	}

	// 挂上回血 buff、走周期回调入口跳一次,返回回了多少气血。
	static uint64_t HealOnce(entt::entity actor)
	{
		const auto buffId = AddBuffOrFail(actor, kHealthRegenerationByLevel);
		BuffSystem::OnIntervalThink(actor, buffId, kAnyNowMs);
		return tlsEcs.actorRegistry.get<BaseAttributesComp>(actor).health() - kHealth;
	}

	// 只有存储等级不同的玩家跳一次,返回回了多少气血;存储等级必须原样。
	static uint64_t HealedByOneTick(uint32_t storedLevel)
	{
		const auto actor = NewWounded(/*isPlayer=*/true);
		tlsEcs.actorRegistry.emplace<LevelComp>(actor).set_level(storedLevel);
		const uint64_t healed = HealOnce(actor);
		EXPECT_EQ(storedLevel, tlsEcs.actorRegistry.get<LevelComp>(actor).level()) << "回血不改写存储等级";
		return healed;
	}
};

TEST_F(BuffHealthRegenerationLevelTest, StoredLevelZeroAndAboveCapHealAsLevelOneAndTheCap)
{
	const auto* row = BuffTableManager::Instance().FindById(kHealthRegenerationByLevel).first;
	ASSERT_NE(nullptr, row);
	ASSERT_EQ(kBuffTypeHealthRegenerationBasedOnLostHealth, row->buff_type());
	// 夹具前提:表达式随等级上涨,0 级低于 1 级、超上限高于上限 —— 否则下面两条相等恒成立。
	ASSERT_LT(ExpressionHealAt(0), ExpressionHealAt(1));
	ASSERT_LT(ExpressionHealAt(playerlevel::kMaxLevel), ExpressionHealAt(playerlevel::kMaxLevel + 15));

	const uint64_t atLevelOne = HealedByOneTick(1);
	const uint64_t atCap = HealedByOneTick(playerlevel::kMaxLevel);
	ASSERT_GT(atLevelOne, 0u);
	ASSERT_GT(atCap, atLevelOne) << "等级必须真的进了回血量";
	ASSERT_LT(kHealth + atCap, kMaxHealth) << "夹具前提:上限等级一跳也回不满,否则超上限的一跳被气血上限截成同一个数";

	EXPECT_EQ(atLevelOne, HealedByOneTick(0)) << "存储等级 0 按 1 级回血,不再一滴不回";
	EXPECT_EQ(atCap, HealedByOneTick(playerlevel::kMaxLevel + 15)) << "超上限的存储等级按上限回血";
}

// 玩家缺 LevelComp 是加载不变量被破坏:这一跳不回血,也不补建组件(以前按 1 级回)。
// 非玩家目标(NPC / 怪物)本来就没有等级组件,照常按 1 级回(组件访问规则;player_level_rules.h 文件头)。
TEST_F(BuffHealthRegenerationLevelTest, PlayerWithoutLevelIsNotHealedWhileANonPlayerHealsAsLevelOne)
{
	const uint64_t atLevelOne = HealedByOneTick(1);
	ASSERT_GT(atLevelOne, 0u);

	const auto creature = NewWounded(/*isPlayer=*/false);
	EXPECT_EQ(atLevelOne, HealOnce(creature)) << "非玩家没有等级组件,按 1 级回血";
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<LevelComp>(creature));

	const auto player = NewWounded(/*isPlayer=*/true);
	EXPECT_EQ(0u, HealOnce(player)) << "玩家缺等级组件,这一跳不回血";
	EXPECT_EQ(kHealth, tlsEcs.actorRegistry.get<BaseAttributesComp>(player).health());
	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<LevelComp>(player)) << "读路径不补建持久组件";
}

// ---------------------------------------------------------------------------
// 下线保留的 Buff(docs/design/realtime-cooldown-buff-persistence.md):存档写出、加载恢复、叠层不看施法者。
// 时间只在调用前后各读一次 UTC 毫秒夹出区间,不等待真实时间。没有 EventLoop 时到期定时器不挂、条目照样在列表里;
// expireAtMs 与定时器在同一处设置,不依赖定时器挂没挂上。
// ---------------------------------------------------------------------------
class BuffKeepOnLogoutTest : public BuffTableFixture
{
protected:
	static SavedBuffComp Saved(uint32_t buffTableId, uint32_t layer, uint64_t expireAtMs)
	{
		SavedBuffComp saved;
		saved.set_buff_table_id(buffTableId);
		saved.set_layer(layer);
		saved.set_expire_at_ms(expireAtMs);
		return saved;
	}

	// 等同加载:存档条目挂成暂存组件,再走 InitPlayerFromAllData 里的恢复入口。
	static void Restore(entt::entity actor, std::initializer_list<SavedBuffComp> records)
	{
		auto& pending = tlsEcs.actorRegistry.emplace<SavedBuffsPendingComp>(actor);
		for (const auto& record : records)
		{
			*pending.buffs.Add() = record;
		}
		BuffSystem::RestoreSavedBuffs(actor);
	}

	static const BuffEntry* FindByTable(entt::entity actor, uint32_t buffTableId)
	{
		const auto* list = tlsEcs.actorRegistry.try_get<BuffListComp>(actor);
		if (list == nullptr)
		{
			return nullptr;
		}
		for (const auto& entry : *list | std::views::values)
		{
			if (entry.buffPb.buff_table_id() == buffTableId)
			{
				return &entry;
			}
		}
		return nullptr;
	}
};

// 只写保留的行,按 buff_id 排序;有时限的写到期时刻,永久的写 0;存盘不改实体。
TEST_F(BuffKeepOnLogoutTest, SaveWritesOnlyKeptBuffsInIdOrder)
{
	const auto actor = tlsEcs.actorRegistry.create();
	AddBuffOrFail(actor, kFiniteDuration);
	const uint64_t before = TimeSystem::NowMillisecondsUTC();
	const auto timedId = AddBuffOrFail(actor, kKeptTimed);
	const uint64_t after = TimeSystem::NowMillisecondsUTC();
	const auto permanentId = AddBuffOrFail(actor, kKeptPermanent);
	ASSERT_LT(timedId, permanentId) << "夹具前提:先挂的条目号小";

	google::protobuf::RepeatedPtrField<SavedBuffComp> out;
	ASSERT_TRUE(BuffSystem::SaveKeptBuffs(actor, out));
	ASSERT_EQ(2, out.size()) << "不保留的行不写";
	EXPECT_EQ(kKeptTimed, out[0].buff_table_id());
	EXPECT_EQ(1u, out[0].layer());
	EXPECT_GE(out[0].expire_at_ms(), before + kKeptTimedDurationMs);
	EXPECT_LE(out[0].expire_at_ms(), after + kKeptTimedDurationMs);
	EXPECT_EQ(kKeptPermanent, out[1].buff_table_id());
	EXPECT_EQ(0u, out[1].expire_at_ms()) << "永久 Buff 不记到期时刻";
	EXPECT_EQ(3u, tlsEcs.actorRegistry.get<BuffListComp>(actor).size()) << "存盘只读";
}

// 子项由父项负责:保留的行作为脱战加成挂上时不单独写,免得恢复出没人撤销的孤儿。
TEST_F(BuffKeepOnLogoutTest, KeptSubBuffIsLeftToItsOwner)
{
	const auto actor = tlsEcs.actorRegistry.create();
	const auto ownerId = AddBuffOrFail(actor, kIdleOwnerOfKeptChild);
	GrantBonusOrFail(actor, ownerId);
	ASSERT_NE(nullptr, FindByTable(actor, kKeptPermanent)) << "夹具前提:加成子项是保留的行";

	google::protobuf::RepeatedPtrField<SavedBuffComp> out;
	ASSERT_TRUE(BuffSystem::SaveKeptBuffs(actor, out));
	EXPECT_EQ(0, out.size());
}

// 恢复按存档的到期时刻接着走(不是从头再算一个整时长),到期时刻与存档里的一字不差(重建多少次都不漂移),
// 层数夹到 [1, max_layer],没有施法者,暂存组件摘掉;有 EventLoop 时有时限的挂到期定时器,永久的不挂。
TEST_F(BuffKeepOnLogoutTest, RestoreContinuesFromSavedExpiryWithClampedLayer)
{
	muduo::net::EventLoop loop;
	const auto actor = tlsEcs.actorRegistry.create();
	const uint64_t expireAt = TimeSystem::NowMillisecondsUTC() + 120000;
	Restore(actor, {Saved(kKeptTimed, 5, expireAt), Saved(kKeptPermanent, 0, 0)});

	EXPECT_FALSE(tlsEcs.actorRegistry.any_of<SavedBuffsPendingComp>(actor));
	ASSERT_EQ(2u, tlsEcs.actorRegistry.get<BuffListComp>(actor).size());
	const auto* timed = FindByTable(actor, kKeptTimed);
	ASSERT_NE(nullptr, timed);
	EXPECT_EQ(3u, timed->buffPb.layer());
	EXPECT_EQ(kNoBuffCaster, timed->buffPb.caster());
	EXPECT_EQ(expireAt, timed->expireAtMs);
	EXPECT_TRUE(timed->expireTimerTaskComp.IsActive());
	const auto* permanent = FindByTable(actor, kKeptPermanent);
	ASSERT_NE(nullptr, permanent);
	EXPECT_EQ(1u, permanent->buffPb.layer());
	EXPECT_EQ(0u, permanent->expireAtMs);
	EXPECT_FALSE(permanent->expireTimerTaskComp.IsActive());
	tlsEcs.actorRegistry.clear(); // 定时器要在 loop 析构之前取消
}

// 按加载时的表裁剪:到期的(查不到行的也一样按自己的到期时刻判)、表改成不保留的丢弃,全丢时不挂空表、暂存组件摘掉;
// 剩余时长不超过表里的整时长;存档时永久、表改成有时限的从现在起算一个整时长。
TEST_F(BuffKeepOnLogoutTest, RestoreTrimsByTheTableAtLoadTime)
{
	const auto dropped = tlsEcs.actorRegistry.create();
	Restore(dropped, {Saved(kKeptTimed, 1, 1), Saved(123456, 1, 1), Saved(kFiniteDuration, 1, 0)});
	EXPECT_FALSE((tlsEcs.actorRegistry.any_of<BuffListComp, SavedBuffsPendingComp>(dropped)));

	const auto capped = tlsEcs.actorRegistry.create();
	const auto wasPermanent = tlsEcs.actorRegistry.create();
	const uint64_t before = TimeSystem::NowMillisecondsUTC();
	Restore(capped, {Saved(kKeptTimed, 1, before + 36000000)}); // 写盘节点时钟超前:看上去还剩 10 小时
	Restore(wasPermanent, {Saved(kKeptTimed, 1, 0)});
	const uint64_t after = TimeSystem::NowMillisecondsUTC();
	for (const auto actor : {capped, wasPermanent})
	{
		const auto* entry = FindByTable(actor, kKeptTimed);
		ASSERT_NE(nullptr, entry);
		EXPECT_GE(entry->expireAtMs, before + kKeptTimedDurationMs);
		EXPECT_LE(entry->expireAtMs, after + kKeptTimedDurationMs);
	}
}

// 本节点表里查不到行(更新版本加的行,或这里还没热更到)的条目不生效,留在暂存组件上,存盘原样写回(连未知字段),
// 认识的行照常恢复。丢掉它的话,玩家回到新版本节点时这个 Buff 就永远没了(混版本保护 P1)。
TEST_F(BuffKeepOnLogoutTest, RestoreCarriesRecordsOfRowsThisNodeDoesNotKnow)
{
	constexpr uint32_t kNewerRow = 123456;
	auto newer = Saved(kNewerRow, 2, 0);
	newer.GetReflection()->MutableUnknownFields(&newer)->AddVarint(412348, 9);
	const auto actor = tlsEcs.actorRegistry.create();
	Restore(actor, {newer, Saved(kKeptPermanent, 1, 0)});

	EXPECT_NE(nullptr, FindByTable(actor, kKeptPermanent));
	EXPECT_EQ(nullptr, FindByTable(actor, kNewerRow));
	const auto* pending = tlsEcs.actorRegistry.try_get<SavedBuffsPendingComp>(actor);
	ASSERT_NE(nullptr, pending);
	ASSERT_EQ(1, pending->buffs.size());

	google::protobuf::RepeatedPtrField<SavedBuffComp> out;
	ASSERT_TRUE(BuffSystem::SaveKeptBuffs(actor, out));
	ASSERT_EQ(2, out.size());
	EXPECT_EQ(newer.SerializeAsString(), out[0].SerializeAsString());
	EXPECT_EQ(kKeptPermanent, out[1].buff_table_id());
}

// 恢复不重跑准入:正常施加会被免疫挡住的组合,存档里两条都活着,恢复后两条都在。
TEST_F(BuffKeepOnLogoutTest, RestoreDoesNotRerunAdmissionChecks)
{
	const auto applied = tlsEcs.actorRegistry.create();
	AddBuffOrFail(applied, kKeptImmuneToKeptTag);
	EXPECT_NE(kSuccess, std::get<0>(BuffSystem::AddOrUpdateBuff(applied, kKeptWithBlockedTag))) << "夹具前提:免疫生效";

	const auto restored = tlsEcs.actorRegistry.create();
	Restore(restored, {Saved(kKeptImmuneToKeptTag, 1, 0), Saved(kKeptWithBlockedTag, 1, 0)});
	EXPECT_NE(nullptr, FindByTable(restored, kKeptImmuneToKeptTag));
	EXPECT_NE(nullptr, FindByTable(restored, kKeptWithBlockedTag));
}

// 保留的行不按施法者分条:别人再施加、系统再施加,都叠到恢复出来的那一条上。
TEST_F(BuffKeepOnLogoutTest, KeptBuffStacksOntoRestoredEntryRegardlessOfCaster)
{
	const auto actor = tlsEcs.actorRegistry.create();
	Restore(actor, {Saved(kKeptTimed, 1, TimeSystem::NowMillisecondsUTC() + 60000)});
	const auto caster = tlsEcs.actorRegistry.create();

	const auto [byCaster, casterEntryId] = BuffSystem::AddOrUpdateBuff(actor, kKeptTimed, ContextFromCaster(caster));
	EXPECT_EQ(kSuccess, byCaster);
	EXPECT_EQ(UINT64_MAX, casterEntryId) << "叠层,不另起一条";
	const auto [bySystem, systemEntryId] = BuffSystem::AddOrUpdateBuff(actor, kKeptTimed);
	EXPECT_EQ(kSuccess, bySystem);
	EXPECT_EQ(UINT64_MAX, systemEntryId);

	const auto& list = tlsEcs.actorRegistry.get<BuffListComp>(actor);
	ASSERT_EQ(1u, list.size());
	EXPECT_EQ(3u, list.begin()->second.buffPb.layer());
}

// 更新版本加的 SavedBuffComp 字段:恢复没跑到时原样写回;恢复之后随条目带回,存盘再并回去。
TEST_F(BuffKeepOnLogoutTest, RecordsKeepNewerFieldsAcrossRestoreAndSave)
{
	constexpr int kNewerField = 412346;
	auto record = Saved(kKeptPermanent, 1, 0);
	record.GetReflection()->MutableUnknownFields(&record)->AddVarint(kNewerField, 77);

	const auto pendingOnly = tlsEcs.actorRegistry.create();
	*tlsEcs.actorRegistry.emplace<SavedBuffsPendingComp>(pendingOnly).buffs.Add() = record;
	google::protobuf::RepeatedPtrField<SavedBuffComp> out;
	ASSERT_TRUE(BuffSystem::SaveKeptBuffs(pendingOnly, out));
	ASSERT_EQ(1, out.size());
	EXPECT_EQ(record.SerializeAsString(), out[0].SerializeAsString());

	const auto restored = tlsEcs.actorRegistry.create();
	Restore(restored, {record});
	out.Clear();
	ASSERT_TRUE(BuffSystem::SaveKeptBuffs(restored, out));
	ASSERT_EQ(1, out.size());
	const auto& unknown = out[0].GetReflection()->GetUnknownFields(out[0]);
	ASSERT_EQ(1, unknown.field_count());
	EXPECT_EQ(kNewerField, unknown.field(0).number());
	EXPECT_EQ(77u, unknown.field(0).varint());
}

int main(int argc, char **argv)
{
	::testing::InitGoogleTest(&argc, argv);
	return RUN_ALL_TESTS();
}
