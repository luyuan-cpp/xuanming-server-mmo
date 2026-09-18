#include "google/protobuf/util/json_util.h"
#include "core/utils/file/file2string.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/guildlevel_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

void GuildLevelTableManager::Load() {
    auto snap = std::make_unique<Snapshot>();

    if (UseProtoBinaryTables()) {
        const std::string path = GetConfigDir() + "guildlevel.pb";
        const auto contents = File2String(path);
        if (!snap->data.ParseFromString(contents)) {
            LOG_FATAL << "GuildLevel binary parse failed: " << path;
        }
    } else {
        const std::string path = GetConfigDir() + "guildlevel.json";
        const auto contents = File2String(path);
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents.data(), &snap->data); !result.ok()) {
            LOG_FATAL << "GuildLevel" << result.message().data();
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
    }

    snapshot = std::move(snap);
}

std::pair<const GuildLevelTable*, uint32_t> GuildLevelTableManager::FindById(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        LOG_ERROR << "GuildLevel table not found for ID: " << tableId;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const GuildLevelTable*, uint32_t> GuildLevelTableManager::FindByIdSilent(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
