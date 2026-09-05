#include "google/protobuf/util/json_util.h"
#include "core/utils/file/file2string.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/mission_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

void MissionTableManager::Load() {
    auto snap = std::make_unique<Snapshot>();

    if (UseProtoBinaryTables()) {
        const std::string path = GetConfigDir() + "mission.pb";
        const auto contents = File2String(path);
        if (!snap->data.ParseFromString(contents)) {
            LOG_FATAL << "Mission binary parse failed: " << path;
        }
    } else {
        const std::string path = GetConfigDir() + "mission.json";
        const auto contents = File2String(path);
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents.data(), &snap->data); !result.ok()) {
            LOG_FATAL << "Mission" << result.message().data();
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
        for (const auto& elem : row_data.condition_id()) {
            snap->conditionIdIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.next_mission_id()) {
            snap->nextMissionIdIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.target_count()) {
            snap->targetCountIndex.emplace(elem, &row_data);
        }
        snap->rewardIdIndex[row_data.reward_id()].push_back(&row_data);
    }

    snapshot = std::move(snap);
}

std::pair<const MissionTable*, uint32_t> MissionTableManager::FindById(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        LOG_ERROR << "Mission table not found for ID: " << tableId;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const MissionTable*, uint32_t> MissionTableManager::FindByIdSilent(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
