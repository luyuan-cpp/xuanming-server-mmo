#include "player/system/player_store_projection.h"

#include <string_view>
#include <vector>

#include <google/protobuf/io/coded_stream.h>
#include <google/protobuf/io/zero_copy_stream_impl_lite.h>

#include "core/utils/proto/unknown_field_carry.h"
#include "proto/db/proto_option.pb.h"

namespace player_store_rows
{
    namespace
    {
        using google::protobuf::FieldDescriptor;

        constexpr auto kOwnerIndex = static_cast<std::size_t>(playerstore::PLAYER_STORE_KIND_OWNER);

        bool IsKnownKind(std::size_t kind)
        {
            return kind < kKindCount && playerstore::PlayerStoreKind_IsValid(static_cast<int>(kind));
        }

        // 允许放进只顺带(PERSIST_ROW_SAVE_PIGGYBACK)行的字段,白名单。只顺带的行被定时存盘推迟时最多晚写
        // piggybackFallbackSec,所以只放丢了不心疼的字段:资产、账本、任务领取标记这类随 I3 一起提交、
        // 丢了玩家会明显感知的字段一律不许(规格 §1.1)。加字段要先过这条再改这里,不能只改 proto。
        constexpr std::string_view kPiggybackAllowedFields[] = {"transform"};

        bool PiggybackAllowed(std::string_view fieldName)
        {
            for (const std::string_view allowed : kPiggybackAllowedFields)
            {
                if (allowed == fieldName)
                {
                    return true;
                }
            }
            return false;
        }

        struct GroupTable
        {
            // 下标是 PlayerStoreKind 的值。元数据字段(player_id、围栏、世代)值为 0,不进任何片段。
            std::array<std::vector<const FieldDescriptor *>, kKindCount> fields;
            // 下标是 PlayerStoreKind 的值。只有数据行有值,其余是 UNSPECIFIED。
            std::array<PersistRowSavePolicy, kKindCount> policy{};
            // 下标是 FieldDescriptor::index()。
            std::vector<std::size_t> groupOf;
            std::string error;
        };

        // 每一行数据都要声明存盘方式;写权行不声明。
        bool BuildPolicies(GroupTable &table)
        {
            const auto *kinds = playerstore::PlayerStoreKind_descriptor();
            for (int i = 0; i < kinds->value_count(); ++i)
            {
                const auto *value = kinds->value(i);
                const auto kind = static_cast<std::size_t>(value->number());
                if (kind == kOwnerIndex)
                {
                    continue;
                }
                if (kind >= kKindCount)
                {
                    table.error = "PlayerStoreKind." + std::string(value->name()) + " is out of KindBytes range";
                    return false;
                }
                const auto policy = value->options().GetExtension(OptionPersistRowSavePolicy);
                if (policy != PERSIST_ROW_SAVE_PERIODIC && policy != PERSIST_ROW_SAVE_PIGGYBACK)
                {
                    table.error = "PlayerStoreKind." + std::string(value->name()) + " has no OptionPersistRowSavePolicy";
                    return false;
                }
                table.policy[kind] = policy;
            }
            return true;
        }

        GroupTable BuildGroups()
        {
            GroupTable table;
            if (!BuildPolicies(table))
            {
                return table;
            }
            const auto *descriptor = player_database::descriptor();
            table.groupOf.resize(static_cast<std::size_t>(descriptor->field_count()));
            for (int i = 0; i < descriptor->field_count(); ++i)
            {
                const FieldDescriptor *field = descriptor->field(i);
                const auto &options = field->options();
                if (!options.HasExtension(OptionPersistGroup))
                {
                    table.error = "player_database." + std::string(field->name()) + " has no OptionPersistGroup";
                    return table;
                }
                const auto group = static_cast<std::size_t>(options.GetExtension(OptionPersistGroup));
                // 0 是元数据;其余必须是现存的数据行,退役号(1、2、5)和不存在的号都拒绝。
                if (group != kOwnerIndex && !IsKnownKind(group))
                {
                    table.error = "player_database." + std::string(field->name()) + " has an invalid OptionPersistGroup " +
                                  std::to_string(group) + " (retired or unknown kind)";
                    return table;
                }
                if (table.policy[group] == PERSIST_ROW_SAVE_PIGGYBACK &&
                    !PiggybackAllowed(std::string_view(field->name().data(), field->name().size())))
                {
                    table.error = "player_database." + std::string(field->name()) + " is in a piggyback row (kind " +
                                  std::to_string(group) + "); only fields in kPiggybackAllowedFields may be deferred";
                    return table;
                }
                table.groupOf[static_cast<std::size_t>(field->index())] = group;
                table.fields[group].push_back(field);
            }
            return table;
        }

        const GroupTable &Groups()
        {
            static const GroupTable table = BuildGroups();
            return table;
        }

        // CodedOutputStream 在析构时才把多申请的缓冲退回去,所以 out 要等本函数返回之后才是最终内容。
        bool SerializeDeterministic(const google::protobuf::Message &message, std::string &out)
        {
            out.clear();
            google::protobuf::io::StringOutputStream output(&out);
            google::protobuf::io::CodedOutputStream coded(&output);
            coded.SetSerializationDeterministic(true);
            return message.SerializeToCodedStream(&coded) && !coded.HadError();
        }
    } // namespace

    bool IsDataKind(std::size_t kind)
    {
        return kind != kOwnerIndex && IsKnownKind(kind);
    }

    bool IsNewerKind(std::size_t kind)
    {
        return kind >= kKindCount;
    }

    PersistRowSavePolicy RowSavePolicy(std::size_t kind)
    {
        return kind < kKindCount ? Groups().policy[kind] : PERSIST_ROW_SAVE_UNSPECIFIED;
    }

    bool ValidatePersistGroups(std::string &error)
    {
        error = Groups().error;
        return error.empty();
    }

    bool ProjectPlayer(player_database &db, KindBytes &out, std::string &error)
    {
        const GroupTable &groups = Groups();
        if (!groups.error.empty())
        {
            error = groups.error;
            return false;
        }
        const auto *reflection = db.GetReflection();
        // 行里不认识的顶层字段由存盘状态机按行保管,不经过这份消息(AssemblePlayer)。这里出现就写不回原来的行。
        if (!reflection->GetUnknownFields(db).empty())
        {
            error = "player_database has unknown top-level fields that belong to no row; refusing to save and lose them";
            return false;
        }
        player_database scratch;
        for (std::size_t kind = 0; kind < kKindCount; ++kind)
        {
            // 写权行、退役空位恒为空串:out 可能是复用的数组,不清掉会留下上一次的内容。
            if (!IsDataKind(kind))
            {
                out[kind].clear();
                continue;
            }
            const auto &fields = groups.fields[kind];
            reflection->SwapFields(&db, &scratch, fields);
            const bool serialized = SerializeDeterministic(scratch, out[kind]);
            reflection->SwapFields(&db, &scratch, fields);
            if (!serialized)
            {
                error = "failed to serialize player_store kind " + std::to_string(kind);
                return false;
            }
        }
        return true;
    }

    bool AssemblePlayer(const google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> &rows,
                        player_database &db, std::string &error)
    {
        const GroupTable &groups = Groups();
        if (!groups.error.empty())
        {
            error = groups.error;
            return false;
        }
        for (const auto &row : rows)
        {
            const auto kind = static_cast<std::size_t>(row.kind());
            // 更新版本才有的行:里面只有本二进制不认识的字段,不读。存盘也不会写它,库里那一行原样留着。
            if (IsNewerKind(kind))
            {
                continue;
            }
            // 写权行不随数据返回;剩下不是数据行的只有退役号(旧布局的 1、2、5),整份拒绝:
            // 按现在的分行去解读别的布局写的行,不属于本行的字段会被清掉,等于静默丢数据。
            if (!IsDataKind(kind))
            {
                error = "retired player_store kind " + std::to_string(kind) +
                        " (a row from a retired row layout; the project has not launched, rebuild the database)";
                return false;
            }
            player_database part;
            if (!part.ParseFromString(row.payload()))
            {
                error = "failed to parse player_store kind " + std::to_string(kind);
                return false;
            }
            const auto *reflection = part.GetReflection();
            // 这一行里本二进制不认识的顶层字段(更新版本加的,或已退役字段的残留)不并进消息:它们属于这一行,
            // 由存盘状态机按行保管、写这一行时原样接回去(CarriedTopLevelBytes、PlayerStoreClient)。
            // 这里清掉的只是内存里的这一份,库里的还在;并进消息的话 ProjectPlayer 分不清它们该写回哪一行。
            reflection->MutableUnknownFields(&part)->Clear();
            // 只认字段的当前分组:两行各有一份时(旧数据)按当前分组取,合并不翻倍。它不防丢失 —— 字段换组后
            // 旧分组那一行里的这份会在这里被清掉,所以换组由 proto 生成器拒绝(持久 schema 台账记 group=,
            // 数据覆写审计 R2 的 N1,player-save-direct-commit.md §1),换存储位置按 expand / contract 走。
            std::vector<const FieldDescriptor *> present;
            reflection->ListFields(part, &present);
            for (const FieldDescriptor *field : present)
            {
                if (groups.groupOf[static_cast<std::size_t>(field->index())] != kind)
                {
                    reflection->ClearField(&part, field);
                }
            }
            db.MergeFrom(part);
        }
        return true;
    }

    bool CarriedTopLevelBytes(const std::string &payload, std::string &carried)
    {
        carried.clear();
        player_database part;
        if (!part.ParseFromString(payload))
        {
            return false;
        }
        carried = unknown_field_carry::Capture(part);
        return true;
    }
}
