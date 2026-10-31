#pragma once

#include <array>
#include <cstddef>
#include <string>

#include "proto/common/database/mysql_database_table.pb.h"
#include "proto/player_store/player_store.pb.h"

// 把 player_database 投影成 player_store 的片段,以及把片段拼回去。
// 规格见 docs/design/player-save-direct-commit.md §1.1、§5。
//
// 内存结构不变:PlayerAllData、loader、Unmarshal 都不知道片段的存在。字段属于哪一组由 proto 字段选项
// (OptionPersistGroup)声明,启动时校验,漏标字段拒绝启动,不会静默丢数据。
namespace player_store_rows
{
    // 数组下标就是 PlayerStoreKind 的值。下标 0(写权行)不用。
    inline constexpr std::size_t kKindCount = 6;
    static_assert(kKindCount == static_cast<std::size_t>(playerstore::PlayerStoreKind_ARRAYSIZE));

    using KindBytes = std::array<std::string, kKindCount>;

    // 校验 player_database 每个字段都声明了 OptionPersistGroup,且取值合法。失败时 error 点名字段。
    bool ValidatePersistGroups(std::string &error);

    // 投影全部 kind,确定性序列化。db 在调用期间被短暂改动(字段交换到临时消息再换回来,不拷贝内容),
    // 返回时恢复原样。顶层有未知字段就拒绝:新二进制写的字段被旧二进制丢掉,比拒写更糟。
    bool ProjectPlayer(player_database &db, const player_database_1 &db1, KindBytes &out, std::string &error);

    // 把片段拼回去。逐行解析、清除不属于本 kind 的字段再合并,所以字段换组之后库里两行各有一份也不会
    // 合并出双份。kLegacy1 单独解析:它是另一个消息,字段号有重叠,不能参与拼接。
    // 顶层有更新版本才有的字段就失败(认领时已按世代拒绝,这里是纵深防御);已退役字段的残留直接丢掉。
    bool AssemblePlayer(const google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> &rows,
                        player_database &db, player_database_1 &db1, std::string &error);
}
