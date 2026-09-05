#include "google/protobuf/util/json_util.h"
#include "core/utils/file/file2string.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/mirror_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

void MirrorTableManager::Load() {
    auto snap = std::make_unique<Snapshot>();

    if (UseProtoBinaryTables()) {
        const std::string path = GetConfigDir() + "mirror.pb";
        const auto contents = File2String(path);
        if (!snap->data.ParseFromString(contents)) {
            LOG_FATAL << "Mirror binary parse failed: " << path;
        }
    } else {
        const std::string path = GetConfigDir() + "mirror.json";
        const auto contents = File2String(path);
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents.data(), &snap->data); !result.ok()) {
            LOG_FATAL << "Mirror" << result.message().data();
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
        snap->sceneIdIndex[row_data.scene_id()].push_back(&row_data);
        snap->mainSceneIdIndex[row_data.main_scene_id()].push_back(&row_data);
    }

    snapshot = std::move(snap);
}

std::pair<const MirrorTable*, uint32_t> MirrorTableManager::FindById(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        LOG_ERROR << "Mirror table not found for ID: " << tableId;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const MirrorTable*, uint32_t> MirrorTableManager::FindByIdSilent(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
