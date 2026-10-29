// 混版本持久字段保护的往返一致性测试(docs/design/mixed-version-persist-field-protection.md §4.3)。
//
// 守的是机制一的不变量:新版本写下、本二进制看不懂的字段,经过「加载 → 统一存盘入口」一跳之后一个都不丢、
// 不挪位置、不翻倍。做法是在一份覆盖全部持久组件的记录上,给每个可携带节点注入一个本版本不认识的 varint 字段,
// 走一次真实的 PlayerAllDataMessageFieldsUnmarshal → MarshalPlayerForPersist,再逐个核对。
//
// 新增持久字段 / 消息类型时:
//   - 新类型从两张玩家表可达、又没进 BuildEveryPersistedType → 覆盖率断言失败,点名缺的类型;
//   - 新类型的装卸是「拆成运行时结构、存盘再逐字段拼回」→ 注入的字段丢失,点名丢在哪个类型;
//     要么像 CurrencyDebt 那样随对象携带未知字段,要么在 proto 上标 OptionPersistFrozen / OptionPersistUncarried
//     并按设计 §3.2 的规则走。
// 不连数据库、不写真实玩家数据,也不依赖号段分配或前一个用例的状态。

#include <gtest/gtest.h>

#include <google/protobuf/descriptor.h>
// IsProbeTarget 用到 Descriptor::options().map_entry(),要 MessageOptions 的完整定义;descriptor.h 只有前向声明。
#include <google/protobuf/descriptor.pb.h>
#include <google/protobuf/message.h>
#include <google/protobuf/unknown_field_set.h>

#include <algorithm>
#include <cstdint>
#include <iterator>
#include <set>
#include <sstream>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

#include "actor/actor_net_id.h"
#include "core/system/id_generator.h"
#include "engine/core/type_define/player_guid_comp.h"
#include "modules/bag/bag_system.h"
#include "persist_schema/persist_schema_gen.h"
#include "proto/common/component/item_base_comp.pb.h"
#include "proto/common/component/mission_comp.pb.h"
#include "proto/common/component/player_pet_comp.pb.h"
#include "proto/common/database/mysql_database_table.pb.h"
#include "proto/common/database/player_cache.pb.h"
#include "services/scene/player/comp/player_persist_carry_comp.h"
#include "services/scene/player/system/asset_op_ledger.h"
#include "services/scene/player/system/player_data_loader.h"
#include "services/scene/player/system/player_inbox_rules.h"
#include "services/scene/player/system/player_persist_marshal.h"
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

// Windows SDK 的同名宏会改写 protobuf 反射方法名，此处隔离宏污染。
#ifdef GetMessage
#undef GetMessage
#endif

namespace
{

using google::protobuf::Descriptor;
using google::protobuf::FieldDescriptor;
using google::protobuf::Message;
using google::protobuf::Reflection;
using google::protobuf::UnknownField;
using google::protobuf::UnknownFieldSet;

// 注入用的未知字段号:不超过 2^29-1,避开 protobuf 保留段 19000-19999,也远离任何真实字段号。
constexpr int kProbeFieldNumber = 412345;
// 标签从这里起逐个加一;与业务数值拉开距离,失败信息里一眼能认出是探针。
constexpr uint64_t kFirstProbeTag = 7000001;

// descriptor 的名字在不同 protobuf 版本里是 std::string 或 absl::string_view,统一看成 std::string_view。
template <typename Name>
std::string_view AsView(const Name &name)
{
    return std::string_view(name.data(), name.size());
}

bool IsFrozenType(const Descriptor &type)
{
    const auto name = AsView(type.full_name());
    const auto &frozen = persist_schema::kFrozenMessageTypes;
    return std::find(frozen.begin(), frozen.end(), name) != frozen.end();
}

bool IsUncarriedField(const FieldDescriptor &field)
{
    const auto name = AsView(field.full_name());
    const auto &uncarried = persist_schema::kUncarriedFields;
    return std::find(uncarried.begin(), uncarried.end(), name) != uncarried.end();
}

// 注入目标:非冻结、非 map entry 的节点。冻结节点由手写代码整层重建,本层未知字段按设计不携带(§3.2 R1);
// map entry 是 protobuf 合成的键值对,本身留不住未知字段。两者都只穿过、不注入。
bool IsProbeTarget(const Descriptor &type)
{
    return !IsFrozenType(type) && !type.options().map_entry();
}

// 只沿「可携带」的边往下走:消息类型字段,且不是未携带字段(投影、每次重写的探针、旧读者镜像)。
bool IsCarriedMessageEdge(const FieldDescriptor &field)
{
    return field.cpp_type() == FieldDescriptor::CPPTYPE_MESSAGE && !IsUncarriedField(field);
}

using ProbeHit = std::pair<uint64_t, std::string>; // (标签, 所在节点的类型全名)

struct ProbeLedger
{
    uint64_t nextTag{kFirstProbeTag};
    std::vector<ProbeHit> injected;
    std::set<std::string> injectedTypes;
};

// map 字段走重复字段的反射视图(protobuf 的 map 迭代器接口不对外);改动在之后序列化时同步回 map。
void InjectProbes(Message &message, ProbeLedger &ledger)
{
    const Descriptor &type = *message.GetDescriptor();
    const Reflection &reflection = *message.GetReflection();
    if (IsProbeTarget(type))
    {
        const uint64_t tag = ledger.nextTag++;
        reflection.MutableUnknownFields(&message)->AddVarint(kProbeFieldNumber, tag);
        std::string name(AsView(type.full_name()));
        ledger.injected.emplace_back(tag, name);
        ledger.injectedTypes.insert(std::move(name));
    }
    for (int i = 0; i < type.field_count(); ++i)
    {
        const FieldDescriptor &field = *type.field(i);
        if (!IsCarriedMessageEdge(field))
        {
            continue;
        }
        if (field.is_repeated())
        {
            const int size = reflection.FieldSize(message, &field);
            for (int j = 0; j < size; ++j)
            {
                InjectProbes(*reflection.MutableRepeatedMessage(&message, &field, j), ledger);
            }
        }
        else if (reflection.HasField(message, &field))
        {
            InjectProbes(*reflection.MutableMessage(&message, &field), ledger);
        }
    }
}

// 与注入同一套边;冻结节点和 map entry 上的探针也收:出现在那里就是「挪了位置」,类型对不上会被比出来。
void CollectProbes(const Message &message, std::vector<ProbeHit> &found)
{
    const Descriptor &type = *message.GetDescriptor();
    const Reflection &reflection = *message.GetReflection();
    const UnknownFieldSet &unknown = reflection.GetUnknownFields(message);
    for (int i = 0; i < unknown.field_count(); ++i)
    {
        const UnknownField &field = unknown.field(i);
        if (field.number() == kProbeFieldNumber && field.type() == UnknownField::TYPE_VARINT)
        {
            found.emplace_back(field.varint(), std::string(AsView(type.full_name())));
        }
    }
    for (int i = 0; i < type.field_count(); ++i)
    {
        const FieldDescriptor &field = *type.field(i);
        if (!IsCarriedMessageEdge(field))
        {
            continue;
        }
        if (field.is_repeated())
        {
            const int size = reflection.FieldSize(message, &field);
            for (int j = 0; j < size; ++j)
            {
                CollectProbes(reflection.GetRepeatedMessage(message, &field, j), found);
            }
        }
        else if (reflection.HasField(message, &field))
        {
            CollectProbes(reflection.GetMessage(message, &field), found);
        }
    }
}

// 覆盖率的口径:从 PlayerAllData(冻结根,下面就是两张玩家表)沿可携带的边可达的每个注入目标类型。
void CollectCarriedTypes(const Descriptor &type, std::set<std::string> &visited, std::set<std::string> &required)
{
    std::string name(AsView(type.full_name()));
    if (!visited.insert(name).second)
    {
        return;
    }
    if (IsProbeTarget(type))
    {
        required.insert(name);
    }
    for (int i = 0; i < type.field_count(); ++i)
    {
        const FieldDescriptor &field = *type.field(i);
        if (IsCarriedMessageEdge(field))
        {
            CollectCarriedTypes(*field.message_type(), visited, required);
        }
    }
}

// 失败时只列差异:丢了的(注入了、输出里没有或换了类型)与多出来的(翻倍、挪位置)。
std::string DescribeProbeDiff(std::vector<ProbeHit> expected, std::vector<ProbeHit> actual)
{
    std::sort(expected.begin(), expected.end());
    std::sort(actual.begin(), actual.end());
    std::vector<ProbeHit> lost;
    std::vector<ProbeHit> extra;
    std::set_difference(expected.begin(), expected.end(), actual.begin(), actual.end(), std::back_inserter(lost));
    std::set_difference(actual.begin(), actual.end(), expected.begin(), expected.end(), std::back_inserter(extra));
    std::ostringstream out;
    out << "lost(注入了但输出里没有,或不在原类型上):";
    for (const auto &[tag, typeName] : lost)
    {
        out << ' ' << typeName << '#' << tag;
    }
    out << "\nextra(输出里多出来的:翻倍或挪了位置):";
    for (const auto &[tag, typeName] : extra)
    {
        out << ' ' << typeName << '#' << tag;
    }
    return out.str();
}

std::string Join(const std::vector<std::string> &names)
{
    std::string joined;
    for (const auto &name : names)
    {
        if (!joined.empty())
        {
            joined += ", ";
        }
        joined += name;
    }
    return joined;
}

} // namespace

class PersistRoundTripTest : public testing::Test
{
protected:
    static constexpr uint64_t kPlayerId = (uint64_t{1} << 58) + 330;
    static constexpr uint64_t kAcceptedAtMs = 1900000000123;

    void SetUp() override
    {
        tlsIdGeneratorManager.SetNodeId(95);
        actor_net_id::Install(tlsEcs.actorRegistry);
        MissionTableManager::Instance().Load();
        ConditionTableManager::Instance().Load();
        ClassTableManager::Instance().Load();
        AttributePoolTableManager::Instance().Load();
        AttributeDimensionTableManager::Instance().Load();
        AttributeRuleTableManager::Instance().Load();
        // 用真实的表 ID 并加载技能表、宠物表:夹具要走的是「版本内认识这些 ID」的正常加载路径(设计 §4.3)。
        // 第 5 批起 SanitizeSkillList 对查不到表的技能也保留(不生效),但那是另一条路径,不在这里混测。
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

    entt::entity NewPlayer()
    {
        const auto player = tlsEcs.actorRegistry.create();
        EXPECT_NE(0, actor_net_id::Assign(tlsEcs.actorRegistry, player));
        tlsEcs.actorRegistry.emplace<PlayerGuidComp>(player, kPlayerId);
        players.push_back(player);
        return player;
    }

    // 一次「旧二进制读到一份记录、原样存回」:在新实体上真实加载,再走统一存盘入口。
    [[nodiscard]] bool LoadAndMarshal(const PlayerAllData &input, PlayerAllData &out)
    {
        const auto player = NewPlayer();
        PlayerAllDataMessageFieldsUnmarshal(player, input);
        return player_persist::MarshalPlayerForPersist(player, kPlayerId, out);
    }

    // 覆盖全部持久组件的记录,每个可携带类型至少一个节点,且每个节点都撑得过加载期的校验与修补
    // (真实技能 / 宠物表 ID、自洽的资产账本与收件箱账本、合法的背包槽位)。
    // 新增持久组件时这里同步加一份数据 —— 漏了的话覆盖率断言会点名缺的类型。
    PlayerAllData BuildEveryPersistedType(uint32_t mainSchemaGen, uint32_t secondSchemaGen)
    {
        const auto &skills = SkillTableManager::Instance().FindAll();
        const auto &pets = PetTableManager::Instance().FindAll();
        EXPECT_GT(skills.data_size(), 0) << "技能表为空,技能节点会在加载时被删掉";
        EXPECT_GT(pets.data_size(), 0) << "宠物表为空,夹具缺真实的宠物种类";
        const uint32_t skillTableId = skills.data_size() > 0 ? skills.data(0).id() : 0;
        const uint32_t petTableId = pets.data_size() > 0 ? pets.data(0).id() : 0;

        PlayerAllData all;
        auto &db = *all.mutable_player_database_data();
        db.set_player_id(kPlayerId);
        db.set_persist_schema_gen(mainSchemaGen);

        auto &transform = *db.mutable_transform();
        transform.mutable_location()->set_x(12.5);
        transform.mutable_location()->set_y(-3.25);
        transform.mutable_location()->set_z(7.0);
        transform.mutable_rotation()->set_y(90.0);
        transform.mutable_scale()->set_x(1.0);
        transform.mutable_scale()->set_y(1.0);
        transform.mutable_scale()->set_z(1.0);

        db.mutable_uint64_pb_component()->set_registration_timestamp(1900000000);
        auto *skill = db.mutable_skill_list()->add_skill_list();
        skill->set_id((uint64_t{1} << 60) + 851);
        skill->set_skill_table_id(skillTableId);
        db.mutable_uint32_pb_component();
        db.mutable_derived_attributes_component()->set_health(100); // 活着:不走职业初值 / 复活修补
        db.mutable_derived_attributes_component()->set_mana(40);
        db.mutable_level_component()->set_level(30);

        auto &currency = *db.mutable_currency();
        currency.add_values(1000);
        currency.add_values(20);
        auto *debt = currency.add_debts();
        debt->set_currency_type(1);
        debt->set_owed(300);
        debt->set_paid(20);
        debt->set_reason("round-trip");
        debt->set_gm_operator("gm-test");
        debt->set_created_at(1900000000);

        auto &attribute = *db.mutable_attribute_component();
        auto *scheme = attribute.add_schemes();
        scheme->set_scheme_id(1);
        scheme->set_name("scheme-1");
        attribute.set_active_scheme_id(1);
        attribute.set_next_scheme_id(2);

        auto *pet = db.mutable_pet_component()->add_pets();
        pet->set_pet_id((uint64_t{1} << 60) + 801);
        pet->set_pet_table_id(petTableId);
        pet->set_level(30);
        pet->set_health(10);
        pet->set_mana(5);
        pet->set_created_at(1900000000);

        auto &profile = *db.mutable_profile_component();
        profile.set_name("round-trip");
        profile.set_appearance_id("a1");
        profile.set_gender(1);

        // 背包:固定包里一件带装备实例数据(词条 + 宝石)的物品、一件普通物品,外加一个动态包。
        auto &bag = *db.mutable_bag_component();
        bag.add_capacities(80);  // kInventory
        bag.add_capacities(140); // kWarehouse
        auto *equipEntry = bag.add_items();
        auto &equipItem = *equipEntry->mutable_instance();
        equipItem.set_item_id((uint64_t{1} << 60) + 811);
        equipItem.set_config_id(1);
        equipItem.set_size(1);
        equipItem.set_acquire_seq(5);
        auto *affix = equipItem.mutable_equip()->add_affixes();
        affix->set_attr_id(3);
        affix->set_tier(1);
        affix->set_value(17);
        equipItem.mutable_equip()->set_enhance_level(5);
        auto *gem = equipItem.mutable_equip()->add_gems();
        gem->set_socket(1);
        gem->set_gem_config_id(2201);
        equipEntry->set_pos(5);
        equipEntry->set_bag_type(static_cast<uint32_t>(kInventory));
        auto *plainEntry = bag.add_items();
        auto &plainItem = *plainEntry->mutable_instance();
        plainItem.set_item_id((uint64_t{1} << 60) + 812);
        plainItem.set_config_id(3);
        plainItem.set_size(2);
        plainItem.set_acquire_seq(6);
        plainEntry->set_pos(9);
        plainEntry->set_bag_type(static_cast<uint32_t>(kWarehouse));
        auto *dynamicBag = bag.add_dynamic_bags();
        dynamicBag->set_bag_id(90);
        dynamicBag->set_capacity(3);
        auto &dynamicItem = *dynamicBag->add_items()->mutable_instance();
        dynamicItem.set_item_id((uint64_t{1} << 60) + 813);
        dynamicItem.set_config_id(3);
        dynamicItem.set_size(1);
        dynamicItem.set_acquire_seq(7);

        // 任务:两个 scope 各一条进行中的任务(与 player_feature_persistence_test 同一份真实任务 ID)。
        auto &quest = *db.mutable_mission_component();
        quest.set_scoped_state_present(true);
        auto *mainScope = quest.add_scopes();
        mainScope->set_scope(MissionListComp::kPlayerMission);
        auto &mainMission = (*mainScope->mutable_mission_list()->mutable_missions())[7];
        mainMission.set_id(7);
        mainMission.add_progress(5);
        (*mainScope->mutable_mission_list()->mutable_mission_begin_time())[7] = kAcceptedAtMs;
        mainScope->add_completed_mission_ids(11);
        // 活动三层账本均随任务快照携带；覆盖每层未知字段，不能靠标冻结绕过恢复校验。
        mainScope->add_completed_mission_ids(15);
        auto *activityLedger = mainScope->mutable_mission_list()->mutable_activity_ledger();
        auto *activitySource = activityLedger->add_sources();
        activitySource->set_mission_scope(MissionListComp::kPlayerMission);
        activitySource->set_mission_id(15);
        activitySource->set_rule_id(47);
        activitySource->set_configuration_revision(2);
        activitySource->set_period_id(1900000000000);
        activitySource->set_points(8);
        auto *activityClaim = activityLedger->add_claims();
        activityClaim->set_rule_id(47);
        activityClaim->set_configuration_revision(2);
        activityClaim->set_period_id(1900000000000);
        activityClaim->set_threshold_id(7);
        activityClaim->set_request_id("persist-round-trip-activity");
        activityClaim->set_reward_id(1);
        activityClaim->set_claimed_at_ms(kAcceptedAtMs);
        auto *secondaryScope = quest.add_scopes();
        secondaryScope->set_scope(77);
        auto &secondaryMission = (*secondaryScope->mutable_mission_list()->mutable_missions())[7];
        secondaryMission.set_id(7);
        secondaryMission.add_progress(3);

        // 资产通道账本用真实的记账函数写;回执与快照手工补齐(含物品 / 宠物 / 货币),实例字节按生产写法
        // (asset_op_system.cpp)填与身份字段一致的完整序列化。末尾断言两本账本都过加载校验。
        auto &ledger = MutableAssetOpStream(*db.mutable_asset_op_ledger(), ASSET_OP_STREAM_GUILD_DEBIT);
        EXPECT_TRUE(RecordAssetOpOutcome(ledger, 3, 1, AssetOpRecordKind::kApplied, 0));
        EXPECT_TRUE(RecordAssetOpOutcome(ledger, 3, 2, AssetOpRecordKind::kRejected, 1006));
        auto *receipt = ledger.add_receipts();
        receipt->set_seq(1);
        receipt->set_request_digest(std::string(32, '\x5a'));
        receipt->set_correlation_id(77);
        auto *snapshotItem = receipt->mutable_snapshot()->add_items();
        snapshotItem->set_item_uuid((uint64_t{1} << 60) + 821);
        snapshotItem->set_config_id(3);
        snapshotItem->set_stack_size(1);
        ItemComp snapshotItemInstance;
        snapshotItemInstance.set_item_id(snapshotItem->item_uuid());
        snapshotItemInstance.set_config_id(snapshotItem->config_id());
        snapshotItemInstance.set_size(snapshotItem->stack_size());
        snapshotItem->set_instance_proto(snapshotItemInstance.SerializeAsString());
        auto *snapshotPet = receipt->mutable_snapshot()->mutable_pet();
        snapshotPet->set_pet_id((uint64_t{1} << 60) + 802);
        snapshotPet->set_pet_table_id(petTableId);
        snapshotPet->set_level(10);
        PetInstance snapshotPetInstance;
        snapshotPetInstance.set_pet_id(snapshotPet->pet_id());
        snapshotPetInstance.set_pet_table_id(snapshotPet->pet_table_id());
        snapshotPetInstance.set_level(snapshotPet->level());
        snapshotPet->set_instance_proto(snapshotPetInstance.SerializeAsString());
        receipt->mutable_snapshot()->mutable_currency()->set_currency_type(1);
        receipt->mutable_snapshot()->mutable_currency()->set_amount(50);

        auto *settled = db.mutable_settlement_ledger()->add_applied();
        settled->set_battle_id((uint64_t{1} << 60) + 901);
        settled->set_applied_at_ms(kAcceptedAtMs);

        auto *cursor = db.mutable_inbox_ledger()->add_cursors();
        cursor->set_domain(1);
        cursor->set_decided_through_seq(5);
        auto *decision = cursor->add_recent();
        decision->set_seq(5);
        decision->set_outcome(1); // PLAYER_INBOX_OUTCOME_APPLIED
        decision->set_request_id(4242);
        decision->set_msg_type(1);

        auto &merchant = *db.mutable_mystery_merchant();
        merchant.set_round_id(3);
        auto *purchase = merchant.add_purchases();
        purchase->set_goods_id(2);
        purchase->set_count(1);

        auto &second = *all.mutable_player_database_1_data();
        second.set_player_id(kPlayerId);
        second.set_persist_schema_gen(secondSchemaGen);

        // 夹具自洽:校验不过时加载会挂失效标记、打 ERROR,往返仍能过,但测的就是一个被 fail-closed 的玩家了。
        EXPECT_TRUE(ValidateAssetOpLedger(db.asset_op_ledger()).empty()) << ValidateAssetOpLedger(db.asset_op_ledger());
        EXPECT_TRUE(player_inbox_rules::ValidateLedger(db.inbox_ledger()).empty())
            << player_inbox_rules::ValidateLedger(db.inbox_ledger());
        return all;
    }

    // 戳的期望值:max(本二进制世代, 两表加载世代的最小值),0 按 1(设计 §3.3)。
    static uint32_t ExpectedStamp(uint32_t mainSchemaGen, uint32_t secondSchemaGen)
    {
        const uint32_t loaded = std::min(player_persist::EffectiveSchemaGen(mainSchemaGen),
                                         player_persist::EffectiveSchemaGen(secondSchemaGen));
        return std::max(persist_schema::kPersistSchemaGen, loaded);
    }

    std::vector<entt::entity> players;
};

// §4.3 步骤 1-4:每个可携带节点上的未知字段,经一次加载与存盘,原样留在同类型的节点上。
TEST_F(PersistRoundTripTest, UnknownFieldsOnEveryCarriedNodeSurviveLoadAndSave)
{
    const uint32_t mainGen = persist_schema::kPersistSchemaGen + 3;
    const uint32_t secondGen = persist_schema::kPersistSchemaGen + 2;

    // 第一跳只为得到「加载期修补之后」的稳定形状;注入从这份基线出发,避免把加载修补误判成丢字段。
    PlayerAllData baseline;
    ASSERT_TRUE(LoadAndMarshal(BuildEveryPersistedType(mainGen, secondGen), baseline));

    ProbeLedger ledger;
    PlayerAllData probed(baseline);
    InjectProbes(probed, ledger);
    ASSERT_FALSE(ledger.injected.empty());

    // 覆盖率:可携带集合里的每个类型都至少注入过一个节点,否则这个测试守不住它。
    std::set<std::string> visited;
    std::set<std::string> required;
    CollectCarriedTypes(*PlayerAllData::descriptor(), visited, required);
    std::vector<std::string> uncovered;
    for (const auto &name : required)
    {
        if (!ledger.injectedTypes.contains(name))
        {
            uncovered.push_back(name);
        }
    }
    EXPECT_TRUE(uncovered.empty())
        << "夹具没有覆盖这些可携带类型(在 BuildEveryPersistedType 里补一个能撑过加载的节点,"
           "或在 proto 上按设计 §3.2 标冻结 / 未携带): "
        << Join(uncovered);

    // 走字节:等价于新版本写下的记录被本二进制解析,新字段落进各层 UnknownFieldSet。
    PlayerAllData fromNewerVersion;
    ASSERT_TRUE(fromNewerVersion.ParseFromString(probed.SerializeAsString()));

    PlayerAllData resaved;
    ASSERT_TRUE(LoadAndMarshal(fromNewerVersion, resaved));
    std::vector<ProbeHit> found;
    CollectProbes(resaved, found);
    auto expected = ledger.injected;
    std::sort(expected.begin(), expected.end());
    std::sort(found.begin(), found.end());
    EXPECT_EQ(expected, found) << DescribeProbeDiff(expected, found);

    // 存回去的记录再被读一次也一样:输出本身就是下一跳的输入,探针不会越传越少。
    PlayerAllData resavedAgain;
    ASSERT_TRUE(LoadAndMarshal(resaved, resavedAgain));
    std::vector<ProbeHit> foundAgain;
    CollectProbes(resavedAgain, foundAgain);
    std::sort(foundAgain.begin(), foundAgain.end());
    EXPECT_EQ(expected, foundAgain) << DescribeProbeDiff(expected, foundAgain);

    const uint32_t stamp = ExpectedStamp(mainGen, secondGen);
    EXPECT_EQ(stamp, resaved.player_database_data().persist_schema_gen());
    EXPECT_EQ(stamp, resaved.player_database_1_data().persist_schema_gen());
}

// 调用方复用同一份输出(上一次的落盘快照)时,两表顶层与欠款携带的未知字段是替换而不是追加,不会越存越多。
TEST_F(PersistRoundTripTest, MarshalIntoReusedOutputDoesNotDuplicateCarriedFields)
{
    PlayerAllData baseline;
    ASSERT_TRUE(LoadAndMarshal(BuildEveryPersistedType(0, 0), baseline));
    ProbeLedger ledger;
    InjectProbes(baseline, ledger);
    PlayerAllData fromNewerVersion;
    ASSERT_TRUE(fromNewerVersion.ParseFromString(baseline.SerializeAsString()));

    const auto player = NewPlayer();
    PlayerAllDataMessageFieldsUnmarshal(player, fromNewerVersion);
    PlayerAllData out;
    ASSERT_TRUE(player_persist::MarshalPlayerForPersist(player, kPlayerId, out));
    ASSERT_TRUE(player_persist::MarshalPlayerForPersist(player, kPlayerId, out));

    std::vector<ProbeHit> found;
    CollectProbes(out, found);
    auto expected = ledger.injected;
    std::sort(expected.begin(), expected.end());
    std::sort(found.begin(), found.end());
    EXPECT_EQ(expected, found) << DescribeProbeDiff(expected, found);
}

// 盖戳:两表同值,取 max(本二进制世代, 两表加载世代的最小值);未盖戳(0)按 1。
// 取最小值:一张表被截断过的事实不能被另一张表的高戳洗白。
TEST_F(PersistRoundTripTest, BothTablesAreStampedWithMaxOfBinaryAndMinLoadedGeneration)
{
    const uint32_t k = persist_schema::kPersistSchemaGen;
    const std::vector<std::pair<uint32_t, uint32_t>> cases = {
        {0, 0},         // 机制上线前的记录 / 新号种子 → 本二进制世代
        {k + 3, k + 2}, // 两表都比本二进制新 → 取较小的那个
        {k + 3, 0},     // 副表未盖戳 → 按 1 参与取最小,主表的高戳不能把它洗白
    };
    for (const auto &[mainGen, secondGen] : cases)
    {
        SCOPED_TRACE(testing::Message() << "main=" << mainGen << " second=" << secondGen);
        PlayerAllData saved;
        ASSERT_TRUE(LoadAndMarshal(BuildEveryPersistedType(mainGen, secondGen), saved));
        EXPECT_EQ(ExpectedStamp(mainGen, secondGen), saved.player_database_data().persist_schema_gen());
        EXPECT_EQ(ExpectedStamp(mainGen, secondGen), saved.player_database_1_data().persist_schema_gen());
        EXPECT_EQ(kPlayerId, saved.player_database_data().player_id());
        EXPECT_EQ(kPlayerId, saved.player_database_1_data().player_id());
    }
    // 显式钉住 {k+3, 0} 的结果,免得 ExpectedStamp 与被测实现一起写错。
    EXPECT_EQ(k, ExpectedStamp(k + 3, 0));
    EXPECT_EQ(k + 2, ExpectedStamp(k + 3, k + 2));
}

// 两个 Carry 组件是持久数据类:缺了就整份拒存,不补空组件(补空等于把新版本写进来的整列抹掉)。
TEST_F(PersistRoundTripTest, MarshalRefusesWhenEitherCarryComponentIsMissing)
{
    for (const bool dropMain : {true, false})
    {
        SCOPED_TRACE(dropMain ? "PlayerDatabaseCarryComp" : "PlayerDatabase1CarryComp");
        const auto player = NewPlayer();
        PlayerAllDataMessageFieldsUnmarshal(player, BuildEveryPersistedType(0, 0));
        if (dropMain)
        {
            tlsEcs.actorRegistry.remove<PlayerDatabaseCarryComp>(player);
        }
        else
        {
            tlsEcs.actorRegistry.remove<PlayerDatabase1CarryComp>(player);
        }
        PlayerAllData previous;
        previous.mutable_player_database_data()->set_player_id(kPlayerId + 1);
        const auto previousBytes = previous.SerializeAsString();
        EXPECT_FALSE(player_persist::MarshalPlayerForPersist(player, kPlayerId, previous));
        EXPECT_EQ(previousBytes, previous.SerializeAsString()) << "拒绝必须保留原输出";
        EXPECT_FALSE((tlsEcs.actorRegistry.all_of<PlayerDatabaseCarryComp, PlayerDatabase1CarryComp>(player)))
            << "存盘只读,不得补建缺失的载体";
    }
}
