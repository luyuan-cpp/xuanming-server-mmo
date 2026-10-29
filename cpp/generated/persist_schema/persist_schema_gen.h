// 由 proto 生成器(tools/proto_generator/protogen/internal/persistschema)按 proto/persist_schema_gen.txt 生成,不要手改。
// 设计:docs/design/mixed-version-persist-field-protection.md §3、§4.3。
#pragma once

#include <array>
#include <cstdint>
#include <string_view>

namespace persist_schema
{
// 本二进制认识的玩家持久 schema 世代。存盘给两张玩家表盖章 max(kPersistSchemaGen, 加载世代)(§3.3)。
inline constexpr uint32_t kPersistSchemaGen = 1;

// 显式冻结的持久消息类型全名(§3.2 R1)。往返一致性测试不在这些类型的节点上注入未知字段,但继续向下遍历。
inline constexpr std::array<std::string_view, 6> kFrozenMessageTypes = {
    "BagAllData",
    "BagAllData.DynamicBagData",
    "ItemEntry",
    "PlayerAllData",
    "QuestAllData",
    "QuestScopeData",
};

// 存盘时不原样携带的持久字段 "<消息全名>.<字段名>"(§3.2)。往返一致性测试不经由它们注入。
inline constexpr std::array<std::string_view, 8> kUncarriedFields = {
    "ItemEntry.equip",
    "PlayerAllData.bag_data",
    "PlayerAllData.mail_data",
    "PlayerAllData.quest_data",
    "QuestAllData.active",
    "player_database.merge_state",
    "player_database.stress_test_probe",
    "player_database_1.stress_test_probe",
};

// 已退役的持久字段(台账 retired 行,§3.1),按消息全名、字段号升序。残留字节照样原样携带;
// 判"带着更新版本的字段"(§4.4 守卫)时排除它们,见 unknown_field_carry::CarriesNewerVersionFields。
struct RetiredField
{
    std::string_view message; // 消息全名,不带前导点
    int number;
};
inline constexpr std::array<RetiredField, 0> kRetiredFields = {};

// 字段 <message>#<number> 是否已退役。
[[nodiscard]] constexpr bool IsRetiredField(std::string_view message, int number)
{
    for (const auto &field : kRetiredFields)
    {
        if (field.number == number && field.message == message)
        {
            return true;
        }
    }
    return false;
}
} // namespace persist_schema
