#include <fstream>
#include <sstream>
#include <string>

#include "google/protobuf/util/json_util.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/rankedpreset_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

bool RankedPresetTableManager::ParseSnapshotFromDir(
    const std::string& configDir,
    const bool useBinary,
    std::shared_ptr<const Snapshot>& out,
    std::string& err) {
    std::string prefix = configDir;
    if (!prefix.empty() && prefix.back() != '/' && prefix.back() != '\\') {
        prefix.push_back('/');
    }
    const std::string path = prefix + (useBinary ? "rankedpreset.pb" : "rankedpreset.json");
    std::ifstream input(path, std::ios::binary);
    if (!input.is_open()) {
        err = "failed to open RankedPreset table file: " + path;
        return false;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    if (input.bad()) {
        err = "failed to read RankedPreset table file: " + path;
        return false;
    }
    const std::string contents = buffer.str();

    auto snap = std::make_shared<Snapshot>();
    if (useBinary) {
        if (!snap->data.ParseFromString(contents)) {
            err = "RankedPreset binary parse failed: " + path;
            return false;
        }
    } else {
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents, &snap->data); !result.ok()) {
            err = std::string("RankedPreset json parse failed: ") + std::string(result.message());
            return false;
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
        for (const auto& elem : row_data.skill_ids()) {
            snap->skillIdsIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.pet_entry_ids()) {
            snap->petEntryIdsIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.pet_entry_points()) {
            snap->petEntryPointsIndex.emplace(elem, &row_data);
        }
        snap->petTableIdIndex[row_data.pet_table_id()].push_back(&row_data);
    }

    out = std::move(snap);
    return true;
}

void RankedPresetTableManager::Load() {
    std::shared_ptr<const Snapshot> snap;
    std::string err;
    if (!ParseSnapshotFromDir(GetConfigDir(), UseProtoBinaryTables(), snap, err)) {
        LOG_FATAL << err;
    }
    InstallSnapshot(std::move(snap));
}

std::pair<const RankedPresetTable*, uint32_t> RankedPresetTableManager::FindById(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        LOG_ERROR << "RankedPreset table not found for ID: " << tableId;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const RankedPresetTable*, uint32_t> RankedPresetTableManager::FindByIdSilent(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
