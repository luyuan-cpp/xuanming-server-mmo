#include "google/protobuf/util/json_util.h"
#include "core/utils/file/file2string.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/attributeautoplan_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

void AttributeAutoPlanTableManager::Load() {
    auto snap = std::make_unique<Snapshot>();

    if (UseProtoBinaryTables()) {
        const std::string path = GetConfigDir() + "attributeautoplan.pb";
        const auto contents = File2String(path);
        if (!snap->data.ParseFromString(contents)) {
            LOG_FATAL << "AttributeAutoPlan binary parse failed: " << path;
        }
    } else {
        const std::string path = GetConfigDir() + "attributeautoplan.json";
        const auto contents = File2String(path);
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents.data(), &snap->data); !result.ok()) {
            LOG_FATAL << "AttributeAutoPlan" << result.message().data();
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
        for (const auto& elem : row_data.dimension()) {
            snap->dimensionIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.weight()) {
            snap->weightIndex.emplace(elem, &row_data);
        }
        snap->classIdIndex[row_data.class_id()].push_back(&row_data);
        snap->poolIdIndex[row_data.pool_id()].push_back(&row_data);
    }

    snapshot = std::move(snap);
}

std::pair<const AttributeAutoPlanTable*, uint32_t> AttributeAutoPlanTableManager::FindById(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        LOG_ERROR << "AttributeAutoPlan table not found for ID: " << tableId;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const AttributeAutoPlanTable*, uint32_t> AttributeAutoPlanTableManager::FindByIdSilent(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
