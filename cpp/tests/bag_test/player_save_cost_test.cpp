// 存盘判脏各段的耗时基准(docs/design/player-data-tidb-scaling-plan.md §4.6「下一步」)。
//
// 三档玩家:新号、普通、满背包。每档测四项,各取 p50 / p99:
//   marshal     拼整份数据:player_persist::MarshalPlayerForPersist,和 SavePlayerImpl 是同一个函数
//   project     按行投影并确定性序列化:player_store_rows::ProjectPlayer
//   compare     逐行比字节,和 PlayerStoreClient::DirtyKinds 同一个比法;数据不变时每行都要比完,是最坏情况
//   differencer 旧做法 MessageDifferencer 整份比,同样是数据不变的最坏情况
//
// 只打印耗时,不断言耗时:测试结论不随机器快慢变化。断言的是这几档数据站得住:
//   - 拼数据成功,背包件数和夹具一致(满背包档真的装满了);
//   - 数据不变时没有一行变化,MessageDifferencer 也判相等;
//   - 只改位置时恰好一行变化(不写死是哪一行,分行调整之后照样成立)。
//
// 缺省每项只跑 5 次,只为跑通;要拿数据时设环境变量 PLAYER_SAVE_COST_ITERATIONS(比如 2000)。
// 测试工程现在只有 Debug|x64 能链接,Debug 下 protobuf 慢很多,这里只看各项之间的比例;
// 绝对耗时以压测时 scene 的 [SaveTiming] 汇总日志和 /metrics 为准(save_timing_stats.h)。
// 不连数据库,不依赖号段分配和前一个用例的状态。

#include <gtest/gtest.h>

#include <google/protobuf/util/message_differencer.h>

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <string>
#include <vector>

#include "actor/actor_net_id.h"
#include "core/system/id_generator.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/bag_system.h"
#include "proto/common/database/mysql_database_table.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "services/scene/player/system/player_data_loader.h"
#include "services/scene/player/system/player_persist_marshal.h"
#include "services/scene/player/system/player_store_projection.h"
#include "table/code/attributedimension_table.h"
#include "table/code/attributepool_table.h"
#include "table/code/attributerule_table.h"
#include "table/code/class_table.h"
#include "table/code/condition_table.h"
#include "table/code/mission_table.h"
#include "table/code/pet_table.h"
#include "table/code/petrule_table.h"
#include "table/code/skill_table.h"
#include "thread_context/ecs_context.h"

// Windows SDK 的同名宏会改写 protobuf 反射方法名,此处隔离宏污染。
#ifdef GetMessage
#undef GetMessage
#endif

namespace
{

using Clock = std::chrono::steady_clock;

struct Tier
{
    const char *name;
    int skills;
    int pets;
    int equipment;  // 带三条词条、一颗宝石的装备
    int plainItems; // 普通物品
};

// 容量和 persist_round_trip_test 的夹具一致:背包 80、仓库 140。满背包档把两个包都放满(110 + 110 = 220)。
constexpr uint32_t kInventoryCapacity = 80;
constexpr uint32_t kWarehouseCapacity = 140;

constexpr Tier kTiers[] = {
    {"newcomer", 1, 0, 2, 3},
    {"regular", 8, 3, 20, 30},
    {"full_bag", 20, 8, 110, 110},
};

constexpr int kDefaultIterations = 5;
constexpr int kMaxIterations = 1000000;

int Iterations()
{
    const char *raw = std::getenv("PLAYER_SAVE_COST_ITERATIONS");
    if (raw == nullptr)
    {
        return kDefaultIterations;
    }
    const long parsed = std::strtol(raw, nullptr, 10);
    return parsed < 1 ? kDefaultIterations : static_cast<int>(std::min<long>(parsed, kMaxIterations));
}

// 最近秩法:排序后取第 ceil(p * n) 个。
double Percentile(std::vector<double> micros, double fraction)
{
    if (micros.empty())
    {
        return 0.0;
    }
    std::sort(micros.begin(), micros.end());
    const auto rank = static_cast<std::size_t>(std::ceil(fraction * static_cast<double>(micros.size())));
    return micros[std::min(micros.size() - 1, rank == 0 ? 0 : rank - 1)];
}

template <typename Work>
std::vector<double> Measure(int iterations, Work &&work)
{
    std::vector<double> micros;
    micros.reserve(static_cast<std::size_t>(iterations));
    for (int i = 0; i < iterations; ++i)
    {
        const auto start = Clock::now();
        work();
        micros.push_back(std::chrono::duration<double, std::micro>(Clock::now() - start).count());
    }
    return micros;
}

void Report(const char *tier, const char *op, const std::vector<double> &micros)
{
    std::printf("[SaveCost] tier=%s op=%s iterations=%zu p50_us=%.1f p99_us=%.1f\n", tier, op, micros.size(),
                Percentile(micros, 0.50), Percentile(micros, 0.99));
}

// 和 PlayerStoreClient::DirtyKinds 同一个比法(不含 uncertain:那是客户端的在途状态,和数据无关)。
int ChangedRowCount(const player_store_rows::KindBytes &rows, const player_store_rows::KindBytes &confirmed)
{
    int changed = 0;
    for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
    {
        if (rows[kind] != confirmed[kind])
        {
            ++changed;
        }
    }
    return changed;
}

class PlayerSaveCostTest : public ::testing::Test
{
protected:
    static constexpr uint64_t kPlayerId = (uint64_t{1} << 58) + 331;

    void SetUp() override
    {
        tlsIdGeneratorManager.SetNodeId(96);
        actor_net_id::Install(tlsEcs.actorRegistry);
        // 和 persist_round_trip_test 同一组表:技能、宠物用真实 ID,走「版本内认识这些 ID」的正常加载路径。
        MissionTableManager::Instance().Load();
        ConditionTableManager::Instance().Load();
        ClassTableManager::Instance().Load();
        AttributePoolTableManager::Instance().Load();
        AttributeDimensionTableManager::Instance().Load();
        AttributeRuleTableManager::Instance().Load();
        SkillTableManager::Instance().Load();
        PetTableManager::Instance().Load();
        PetRuleTableManager::Instance().Load();
    }

    void TearDown() override
    {
        for (const auto player : players)
        {
            if (tlsEcs.actorRegistry.valid(player))
            {
                tlsEcs.actorRegistry.destroy(player);
            }
        }
    }

    entt::entity LoadPlayer(const PlayerAllData &input, uint64_t playerId)
    {
        const auto player = tlsEcs.actorRegistry.create();
        EXPECT_NE(0, actor_net_id::Assign(tlsEcs.actorRegistry, player));
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, playerId);
        players.push_back(player);
        PlayerAllDataMessageFieldsUnmarshal(player, input);
        return player;
    }

    static PlayerAllData BuildTier(const Tier &tier, uint64_t playerId)
    {
        PlayerAllData all;
        auto &db = *all.mutable_player_database_data();
        db.set_player_id(playerId);

        auto &transform = *db.mutable_transform();
        transform.mutable_location()->set_x(12.5);
        transform.mutable_location()->set_y(-3.25);
        transform.mutable_location()->set_z(7.0);
        transform.mutable_rotation()->set_y(90.0);
        transform.mutable_scale()->set_x(1.0);
        transform.mutable_scale()->set_y(1.0);
        transform.mutable_scale()->set_z(1.0);

        db.mutable_uint64_pb_component()->set_registration_timestamp(1900000000);
        db.mutable_uint32_pb_component();
        db.mutable_derived_attributes_component()->set_health(100); // 活着:不走职业初值 / 复活修补
        db.mutable_derived_attributes_component()->set_mana(40);
        db.mutable_level_component()->set_level(30);

        auto &currency = *db.mutable_currency();
        currency.add_values(1000);
        currency.add_values(20);

        auto &attribute = *db.mutable_attribute_component();
        auto *scheme = attribute.add_schemes();
        scheme->set_scheme_id(1);
        scheme->set_name("scheme-1");
        attribute.set_active_scheme_id(1);
        attribute.set_next_scheme_id(2);

        auto &profile = *db.mutable_profile_component();
        profile.set_name("save-cost");
        profile.set_appearance_id("a1");
        profile.set_gender(1);

        // 技能:每个技能用不同的表 ID,避免加载时被当成重复技能合并。
        const auto &skills = SkillTableManager::Instance().FindAll();
        const int skillCount = std::min(tier.skills, skills.data_size());
        for (int i = 0; i < skillCount; ++i)
        {
            auto *skill = db.mutable_skill_list()->add_skill_list();
            skill->set_id((uint64_t{1} << 60) + 2000 + static_cast<uint64_t>(i));
            skill->set_skill_table_id(skills.data(i).id());
        }

        const auto &pets = PetTableManager::Instance().FindAll();
        if (pets.data_size() > 0)
        {
            for (int i = 0; i < tier.pets; ++i)
            {
                auto *pet = db.mutable_pet_component()->add_pets();
                pet->set_pet_id((uint64_t{1} << 60) + 3000 + static_cast<uint64_t>(i));
                pet->set_pet_table_id(pets.data(0).id());
                pet->set_level(30);
                pet->set_health(10);
                pet->set_mana(5);
                pet->set_created_at(1900000000);
            }
        }

        // 背包:先放满背包,再放仓库;装备在前,普通物品在后。物品形状和 persist_round_trip_test 一致。
        auto &bag = *db.mutable_bag_component();
        bag.add_capacities(kInventoryCapacity);
        bag.add_capacities(kWarehouseCapacity);
        const int itemCount = tier.equipment + tier.plainItems;
        for (int index = 0; index < itemCount; ++index)
        {
            const auto slot = static_cast<uint32_t>(index);
            const bool inInventory = slot < kInventoryCapacity;
            auto *entry = bag.add_items();
            entry->set_bag_type(static_cast<uint32_t>(inInventory ? kInventory : kWarehouse));
            entry->set_pos(inInventory ? slot : slot - kInventoryCapacity);
            auto &item = *entry->mutable_instance();
            item.set_item_id((uint64_t{1} << 60) + 10000 + static_cast<uint64_t>(index));
            item.set_acquire_seq(static_cast<uint64_t>(index) + 1);
            if (index < tier.equipment)
            {
                item.set_config_id(1);
                item.set_size(1);
                for (uint32_t affixIndex = 0; affixIndex < 3; ++affixIndex)
                {
                    auto *affix = item.mutable_equip()->add_affixes();
                    affix->set_attr_id(3 + affixIndex);
                    affix->set_tier(1);
                    affix->set_value(17 + affixIndex);
                }
                item.mutable_equip()->set_enhance_level(5);
                auto *gem = item.mutable_equip()->add_gems();
                gem->set_socket(1);
                gem->set_gem_config_id(2201);
            }
            else
            {
                item.set_config_id(3);
                item.set_size(2);
            }
        }

        return all;
    }

    std::vector<entt::entity> players;
};

TEST_F(PlayerSaveCostTest, ReportsStageCostsForThreeTiers)
{
    const int iterations = Iterations();
    uint64_t playerId = kPlayerId;
    for (const Tier &tier : kTiers)
    {
        SCOPED_TRACE(tier.name);
        ASSERT_LE(tier.equipment + tier.plainItems, static_cast<int>(kInventoryCapacity + kWarehouseCapacity));
        ++playerId; // 每档一个玩家号,三个实体互不相干
        const auto player = LoadPlayer(BuildTier(tier, playerId), playerId);

        // 拼整份数据。每次用新的输出对象,和 SavePlayerImpl 每次 make_shared 一份相同。
        PlayerAllData marshaled;
        bool marshalOk = true;
        const auto marshalMicros = Measure(iterations, [&]
        {
            PlayerAllData out;
            marshalOk = player_persist::MarshalPlayerForPersist(player, playerId, out) && marshalOk;
            marshaled.Swap(&out);
        });
        ASSERT_TRUE(marshalOk);
        ASSERT_EQ(marshaled.player_database_data().bag_component().items_size(), tier.equipment + tier.plainItems)
            << "背包件数和夹具不一致,这一档测的不是想测的数据量";

        // 按行投影并序列化。
        player_store_rows::KindBytes rows;
        bool projectOk = true;
        std::string error;
        const auto projectMicros = Measure(iterations, [&]
        {
            player_store_rows::KindBytes out;
            projectOk = player_store_rows::ProjectPlayer(*marshaled.mutable_player_database_data(), out, error) && projectOk;
            rows.swap(out);
        });
        ASSERT_TRUE(projectOk) << error;

        // 逐行比字节:数据不变,每行都比到最后一个字节。
        const player_store_rows::KindBytes confirmed = rows;
        int changedRows = 0;
        const auto compareMicros = Measure(iterations, [&] { changedRows += ChangedRowCount(rows, confirmed); });
        EXPECT_EQ(changedRows, 0) << "数据不变却有行判为变化";

        // 旧做法:MessageDifferencer 整份比。
        const PlayerAllData copy = marshaled;
        bool allEqual = true;
        const auto differencerMicros = Measure(iterations, [&]
        {
            allEqual = google::protobuf::util::MessageDifferencer::Equals(marshaled, copy) && allEqual;
        });
        EXPECT_TRUE(allEqual);

        // 只改位置:恰好一行变化。
        PlayerAllData moved = marshaled;
        auto *location = moved.mutable_player_database_data()->mutable_transform()->mutable_location();
        location->set_x(location->x() + 1.0);
        player_store_rows::KindBytes movedRows;
        ASSERT_TRUE(player_store_rows::ProjectPlayer(*moved.mutable_player_database_data(), movedRows, error)) << error;
        EXPECT_EQ(ChangedRowCount(movedRows, confirmed), 1) << "只改位置,应该只有位置所在的那一行变化";

        Report(tier.name, "marshal", marshalMicros);
        Report(tier.name, "project", projectMicros);
        Report(tier.name, "compare", compareMicros);
        Report(tier.name, "differencer", differencerMicros);
        std::string sizes;
        for (std::size_t kind = 1; kind < player_store_rows::kKindCount; ++kind)
        {
            if (!rows[kind].empty())
            {
                sizes += ' ';
                sizes += std::to_string(kind);
                sizes += '=';
                sizes += std::to_string(rows[kind].size());
            }
        }
        std::printf("[SaveCost] tier=%s row_bytes(kind=bytes):%s\n", tier.name, sizes.c_str());
    }
}

} // namespace
