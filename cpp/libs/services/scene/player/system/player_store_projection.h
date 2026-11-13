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

    // kind 是不是更新版本才有的行:号比本二进制认识的都大。行种的号只增、不回填、不复用(退役号写 reserved),
    // 所以更新版本加的行一定落在这里;比它小又不是数据行的只剩写权行和退役号。
    // 更新版本的行里只有本二进制不认识的字段(字段不许换行,proto 生成器守着):认领时不读、存盘时不写,
    // 库里那一行原样留着,等认识它的版本来读(混版本设计 §4.2)。
    bool IsNewerKind(std::size_t kind);

    // 这一行的存盘方式。只对 IsDataKind 为真的 kind 有意义;ValidatePersistGroups 通过之后不会是 UNSPECIFIED。
    PersistRowSavePolicy RowSavePolicy(std::size_t kind);

    // 校验 player_database 每个字段都声明了 OptionPersistGroup、取值是一行数据,以及每行数据都声明了存盘方式。
    // 失败时 error 点名字段或行。
    bool ValidatePersistGroups(std::string &error);

    // 投影全部 kind,确定性序列化,结果只含本二进制认识的字段。db 在调用期间被短暂改动(字段交换到临时消息
    // 再换回来,不拷贝内容),返回时恢复原样。
    // 顶层有未知字段就拒绝。行里本二进制不认识的顶层字段不进内存(见 AssemblePlayer),走到这里的消息顶层不该有
    // 未知字段;有就说明它不是从行里加载来的,投影不知道该把它们写回哪一行,拒绝比写丢强。
    bool ProjectPlayer(player_database &db, KindBytes &out, std::string &error);

    // 把片段拼回去。逐行解析、清除不属于本 kind 的字段再合并,所以字段换行之后库里两行各有一份也不会
    // 合并出双份。
    // 新旧版本混跑时本二进制不认识的内容都不读,也都不丢:
    //   - 更新版本才有的行(IsNewerKind)跳过,库里那一行没有人动;
    //   - 一行里本二进制不认识的顶层字段(更新版本加的,或已退役字段的残留)不并进消息,由存盘状态机按行保管:
    //     PlayerStoreClient 认领时用 CarriedTopLevelBytes 取出,之后每次写这一行都接在投影结果后面。
    //     并进消息就分不清哪个字段属于哪一行,ProjectPlayer 没法把它们写回原来的行。
    //   - 子消息里不认识的字段跟着对象走(unknown_field_carry.h)。
    // 退役的 kind(旧布局留下的 1、2、5 行)整份拒绝:那是另一种布局写的,按现在的分行解读会丢字段。
    bool AssemblePlayer(const google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> &rows,
                        player_database &db, std::string &error);

    // 取出一行里本二进制不认识的顶层字段,原样序列化进 carried;没有时 carried 为空串。
    // 把它接在这一行的投影结果后面,就是更新版本读得回全部字段的一行。返回 false = payload 解析不了。
    // 认领时每行在这里多解析一遍(AssemblePlayer 还要解析一遍),只发生在进场;要省这一遍先拿到实测数据再说。
    bool CarriedTopLevelBytes(const std::string &payload, std::string &carried);
}
