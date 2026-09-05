#include "google/protobuf/util/json_util.h"
#include "core/utils/file/file2string.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/condition_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

void ConditionTableManager::Load() {
    auto snap = std::make_unique<Snapshot>();

    if (UseProtoBinaryTables()) {
        const std::string path = GetConfigDir() + "condition.pb";
        const auto contents = File2String(path);
        if (!snap->data.ParseFromString(contents)) {
            LOG_FATAL << "Condition binary parse failed: " << path;
        }
    } else {
        const std::string path = GetConfigDir() + "condition.json";
        const auto contents = File2String(path);
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents.data(), &snap->data); !result.ok()) {
            LOG_FATAL << "Condition" << result.message().data();
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
        for (const auto& elem : row_data.condition1()) {
            snap->condition1Index.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.condition2()) {
            snap->condition2Index.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.condition3()) {
            snap->condition3Index.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.condition4()) {
            snap->condition4Index.emplace(elem, &row_data);
        }
    }

    snapshot = std::move(snap);
}

std::pair<const ConditionTable*, uint32_t> ConditionTableManager::FindById(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        LOG_ERROR << "Condition table not found for ID: " << tableId;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const ConditionTable*, uint32_t> ConditionTableManager::FindByIdSilent(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
