#include "google/protobuf/util/json_util.h"
#include "core/utils/file/file2string.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/pet_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

void PetTableManager::Load() {
    auto snap = std::make_unique<Snapshot>();

    if (UseProtoBinaryTables()) {
        const std::string path = GetConfigDir() + "pet.pb";
        const auto contents = File2String(path);
        if (!snap->data.ParseFromString(contents)) {
            LOG_FATAL << "Pet binary parse failed: " << path;
        }
    } else {
        const std::string path = GetConfigDir() + "pet.json";
        const auto contents = File2String(path);
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents.data(), &snap->data); !result.ok()) {
            LOG_FATAL << "Pet" << result.message().data();
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
        for (const auto& elem : row_data.aptitude_min()) {
            snap->aptitudeMinIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.aptitude_max()) {
            snap->aptitudeMaxIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.skill()) {
            snap->skillIndex.emplace(elem, &row_data);
        }
    }

    snapshot = std::move(snap);
}

std::pair<const PetTable*, uint32_t> PetTableManager::FindById(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        LOG_ERROR << "Pet table not found for ID: " << tableId;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const PetTable*, uint32_t> PetTableManager::FindByIdSilent(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
