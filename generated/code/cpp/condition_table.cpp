#include <fstream>
#include <sstream>
#include <string>

#include "google/protobuf/util/json_util.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/condition_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

bool ConditionTableManager::ParseSnapshotFromDir(
    const std::string& configDir,
    const bool useBinary,
    std::shared_ptr<const Snapshot>& out,
    std::string& err) {
    std::string prefix = configDir;
    if (!prefix.empty() && prefix.back() != '/' && prefix.back() != '\\') {
        prefix.push_back('/');
    }
    const std::string path = prefix + (useBinary ? "condition.pb" : "condition.json");
    std::ifstream input(path, std::ios::binary);
    if (!input.is_open()) {
        err = "failed to open Condition table file: " + path;
        return false;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    if (input.bad()) {
        err = "failed to read Condition table file: " + path;
        return false;
    }
    const std::string contents = buffer.str();

    auto snap = std::make_shared<Snapshot>();
    if (useBinary) {
        if (!snap->data.ParseFromString(contents)) {
            err = "Condition binary parse failed: " + path;
            return false;
        }
    } else {
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents, &snap->data); !result.ok()) {
            err = std::string("Condition json parse failed: ") + std::string(result.message());
            return false;
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

    out = std::move(snap);
    return true;
}

void ConditionTableManager::Load() {
    std::shared_ptr<const Snapshot> snap;
    std::string err;
    if (!ParseSnapshotFromDir(GetConfigDir(), UseProtoBinaryTables(), snap, err)) {
        LOG_FATAL << err;
    }
    InstallSnapshot(std::move(snap));
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
