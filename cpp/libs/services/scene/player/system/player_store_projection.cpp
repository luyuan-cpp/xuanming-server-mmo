#include "player/system/player_store_projection.h"

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

        constexpr auto kLegacyIndex = static_cast<std::size_t>(playerstore::PLAYER_STORE_KIND_LEGACY1);

        struct GroupTable
        {
            // 下标是 PlayerStoreKind 的值。元数据字段(player_id、围栏、世代)值为 0,不进任何片段。
            std::array<std::vector<const FieldDescriptor *>, kKindCount> fields;
            // 下标是 FieldDescriptor::index()。
            std::vector<std::size_t> groupOf;
            std::string error;
        };

        GroupTable BuildGroups()
        {
            GroupTable table;
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
                if (group >= kKindCount || group == kLegacyIndex)
                {
                    table.error = "player_database." + std::string(field->name()) + " has an invalid OptionPersistGroup";
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

    bool ValidatePersistGroups(std::string &error)
    {
        error = Groups().error;
        return error.empty();
    }

    bool ProjectPlayer(player_database &db, const player_database_1 &db1, KindBytes &out, std::string &error)
    {
        const GroupTable &groups = Groups();
        if (!groups.error.empty())
        {
            error = groups.error;
            return false;
        }
        const auto *reflection = db.GetReflection();
        if (!reflection->GetUnknownFields(db).empty())
        {
            error = "player_database has unknown top-level fields; refusing to save and lose them";
            return false;
        }
        out[0].clear();
        player_database scratch;
        for (std::size_t kind = 1; kind < kKindCount; ++kind)
        {
            if (kind == kLegacyIndex)
            {
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
        if (!SerializeDeterministic(db1, out[kLegacyIndex]))
        {
            error = "failed to serialize player_database_1";
            return false;
        }
        return true;
    }

    bool AssemblePlayer(const google::protobuf::RepeatedPtrField<playerstore::PlayerStoreRow> &rows,
                        player_database &db, player_database_1 &db1, std::string &error)
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
            if (kind == kLegacyIndex)
            {
                if (!db1.ParseFromString(row.payload()))
                {
                    error = "failed to parse player_database_1";
                    return false;
                }
                continue;
            }
            if (kind == 0 || kind >= kKindCount)
            {
                error = "unknown player_store kind " + std::to_string(kind);
                return false;
            }
            player_database part;
            if (!part.ParseFromString(row.payload()))
            {
                error = "failed to parse player_store kind " + std::to_string(kind);
                return false;
            }
            const auto *reflection = part.GetReflection();
            const auto &partName = part.GetDescriptor()->full_name();
            if (unknown_field_carry::ContainsNewerVersionFields(std::string_view(partName.data(), partName.size()),
                                                                reflection->GetUnknownFields(part)))
            {
                error = "player_store kind " + std::to_string(kind) + " has fields of a newer version";
                return false;
            }
            // 剩下的是退役字段的残留。丢掉:留着会进 Carry 组件,下一次存盘被 ProjectPlayer 拒绝。
            reflection->MutableUnknownFields(&part)->Clear();
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
}
