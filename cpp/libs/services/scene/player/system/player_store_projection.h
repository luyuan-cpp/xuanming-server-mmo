#pragma once

#include <array>
#include <cstddef>
#include <string>

#include "proto/common/database/mysql_database_table.pb.h"
#include "proto/db/proto_option.pb.h"
#include "proto/player_store/player_store.pb.h"

// 把 player_database 投影成 player_store 的片段,以及把片段拼回去。
// 规格见 docs/design/player-save-direct-commit.md §1.1、§5。
//
// 内存结构不变:PlayerAllData、loader、Unmarshal 都不知道片段的存在。字段属于哪一行由 proto 字段选项
// (OptionPersistGroup)声明,每一行怎么存由 PlayerStoreKind 的枚举值选项(OptionPersistRowSavePolicy)声明,
// 启动时校验,漏标拒绝启动,不会静默丢数据。
namespace player_store_rows
{
    // 数组下标就是 PlayerStoreKind 的值。下标 0(写权行)和退役的 1、2、5 不用,恒为空串,永远不算脏。
    inline constexpr std::size_t kKindCount = 10;
    static_assert(kKindCount == static_cast<std::size_t>(playerstore::PlayerStoreKind_ARRAYSIZE));

    using KindBytes = std::array<std::string, kKindCount>;

    // kind 是不是一行数据:枚举里有(退役号不算)且不是写权行。遍历 KindBytes 时用它跳过空位。
    bool IsDataKind(std::size_t kind);

    // 这一行的存盘方式。只对 IsDataKind 为真的 kind 有意义;ValidatePersistGroups 通过之后不会是 UNSPECIFIED。
    PersistRowSavePolicy RowSavePolicy(std::size_t kind);

    // 校验 player_database 每个字段都声明了 OptionPersistGroup、取值是一行数据,以及每行数据都声明了存盘方式。
    // 失败时 error 点名字段或行。
    bool ValidatePersistGroups(std::string &error);

    // 投影全部 kind,确定性序列化。db 在调用期间被短暂改动(字段交换到临时消息再换回来,不拷贝内容),
    // 返回时恢复原样。顶层有未知字段就拒绝:新二进制写的字段被旧二进制丢掉,比拒写更糟。
    bool ProjectPlayer(player_database &db, KindBytes &out, std::string &error);

    // 把片段拼回去。逐行解析、清除不属于本 kind 的字段再合并,所以字段换行之后库里两行各有一份也不会
    // 合并出双份。
    // 未知或退役的 kind(旧布局留下的 1、2、5 行)整份拒绝:那是另一种布局写的,按现在的分行解读会丢字段。
    // 顶层有更新版本才有的字段就失败(认领时已按世代拒绝,这里是纵深防御);已退役字段的残留直接丢掉。
    bool AssemblePlayer(const google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> &rows,
                        player_database &db, std::string &error);
}
