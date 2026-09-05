#include <gtest/gtest.h>

#include <string>

#include "data/battle_table_fingerprint.h"

#include "table/proto/skill_table.pb.h"
#include "table/proto/buff_table.pb.h"
#include "table/proto/cooldown_table.pb.h"
#include "table/proto/skillpermission_table.pb.h"
#include "table/proto/dungeon_table.pb.h"
#include "table/proto/monster_table.pb.h"

// 战斗配表指纹单测(cross-zone-matchmaking.md §10):
// 直接构造六张表的 proto 对象喂 ComputeFrom,不依赖表管理器/Excel 数据。
// 覆盖:同一份表两次计算相同;改任一张表的任一字段则不同;长度/字符集契约。

namespace {

using turnbattle::BattleTableFingerprint;

struct TableSet {
    SkillTableData skill;
    BuffTableData buff;
    CooldownTableData cooldown;
    SkillPermissionTableData permission;
    DungeonTableData dungeon;
    MonsterTableData monster;

    std::string Fingerprint() const {
        return BattleTableFingerprint::ComputeFrom(skill, buff, cooldown, permission, dungeon, monster);
    }
};

// 装配一套有代表性的表数据(含 repeated 与 map 字段,覆盖确定性序列化的关键路径)
TableSet MakeTables() {
    TableSet tables;

    auto* skill = tables.skill.add_data();
    skill->set_id(101);
    skill->add_targeting_mode(1);
    skill->add_skill_type(1);
    skill->set_cooldown_id(9);
    skill->add_effect(201);

    auto* buff = tables.buff.add_data();
    buff->set_id(201);
    buff->set_duration(12.0);
    buff->set_interval(6.0);
    buff->add_interval_effect(10.0);
    buff->set_max_layer(3);
    // map 字段:确定性序列化按 key 排序,插入序不影响指纹
    (*buff->mutable_tag())["poison_tag"] = true;
    (*buff->mutable_tag())["alpha_tag"] = true;

    auto* cooldown = tables.cooldown.add_data();
    cooldown->set_id(9);
    cooldown->set_duration(12000);

    auto* permission = tables.permission.add_data();
    permission->set_id(1);

    auto* dungeon = tables.dungeon.add_data();
    dungeon->set_id(7);
    dungeon->add_monster(1001);

    auto* monster = tables.monster.add_data();
    monster->set_id(1001);

    return tables;
}

TEST(BattleTableFingerprintTest, SameTablesProduceSameFingerprint) {
    const auto first = MakeTables();
    const auto second = MakeTables();

    const auto fingerprintA = first.Fingerprint();
    const auto fingerprintB = second.Fingerprint();

    EXPECT_EQ(fingerprintA, fingerprintB);
    // 同一对象重复计算也稳定(缓存/无缓存路径同值)
    EXPECT_EQ(fingerprintA, first.Fingerprint());
}

TEST(BattleTableFingerprintTest, FingerprintIsHexPrefixOfFixedLength) {
    const auto fingerprint = MakeTables().Fingerprint();
    ASSERT_EQ(fingerprint.size(), BattleTableFingerprint::kHexLength);
    for (const char c : fingerprint) {
        const bool isHex = (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
        EXPECT_TRUE(isHex) << "非 hex 字符: " << c;
    }
    // 空表集合也有确定的指纹,且与非空不同
    const TableSet empty;
    EXPECT_EQ(empty.Fingerprint().size(), BattleTableFingerprint::kHexLength);
    EXPECT_NE(empty.Fingerprint(), fingerprint);
}

TEST(BattleTableFingerprintTest, AnyFieldChangeInAnyTableChangesFingerprint) {
    const auto base = MakeTables().Fingerprint();

    {
        auto tables = MakeTables();
        tables.skill.mutable_data(0)->set_cooldown_id(10);
        EXPECT_NE(tables.Fingerprint(), base) << "skill 字段变化未反映到指纹";
    }
    {
        auto tables = MakeTables();
        tables.buff.mutable_data(0)->set_max_layer(4);
        EXPECT_NE(tables.Fingerprint(), base) << "buff 字段变化未反映到指纹";
    }
    {
        auto tables = MakeTables();
        (*tables.buff.mutable_data(0)->mutable_tag())["extra_tag"] = true;
        EXPECT_NE(tables.Fingerprint(), base) << "buff map 字段变化未反映到指纹";
    }
    {
        auto tables = MakeTables();
        tables.cooldown.mutable_data(0)->set_duration(12001);
        EXPECT_NE(tables.Fingerprint(), base) << "cooldown 字段变化未反映到指纹";
    }
    {
        auto tables = MakeTables();
        tables.permission.mutable_data(0)->set_id(2);
        EXPECT_NE(tables.Fingerprint(), base) << "skillpermission 字段变化未反映到指纹";
    }
    {
        auto tables = MakeTables();
        tables.dungeon.mutable_data(0)->add_monster(1002);
        EXPECT_NE(tables.Fingerprint(), base) << "dungeon 字段变化未反映到指纹";
    }
    {
        auto tables = MakeTables();
        tables.monster.add_data()->set_id(1002);
        EXPECT_NE(tables.Fingerprint(), base) << "monster 新增行未反映到指纹";
    }
}

TEST(BattleTableFingerprintTest, TableOrderIsPartOfContract) {
    // 把 skill 与 buff 的内容互换位置喂进去(类型不同无法直接互换,这里用"两张空表 vs
    // 各放一行"验证段边界):skill 有行 + buff 空 与 skill 空 + buff 有行 必须不同,
    // 说明表名/段长前缀确实把各段隔开,不会因为拼接错位而碰撞。
    TableSet onlySkill;
    onlySkill.skill.add_data()->set_id(1);
    TableSet onlyBuff;
    onlyBuff.buff.add_data()->set_id(1);
    EXPECT_NE(onlySkill.Fingerprint(), onlyBuff.Fingerprint());
}

}  // namespace
