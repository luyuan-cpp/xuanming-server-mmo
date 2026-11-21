#include <fstream>
#include <sstream>
#include <string>

#include "google/protobuf/util/json_util.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/buff_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

bool BuffTableManager::ParseSnapshotFromDir(
    const std::string& configDir,
    const bool useBinary,
    std::shared_ptr<const Snapshot>& out,
    std::string& err) {
    std::string prefix = configDir;
    if (!prefix.empty() && prefix.back() != '/' && prefix.back() != '\\') {
        prefix.push_back('/');
    }
    const std::string path = prefix + (useBinary ? "buff.pb" : "buff.json");
    std::ifstream input(path, std::ios::binary);
    if (!input.is_open()) {
        err = "failed to open Buff table file: " + path;
        return false;
    }
    std::ostringstream buffer;
    buffer << input.rdbuf();
    if (input.bad()) {
        err = "failed to read Buff table file: " + path;
        return false;
    }
    const std::string contents = buffer.str();

    auto snap = std::make_shared<Snapshot>();
    if (useBinary) {
        if (!snap->data.ParseFromString(contents)) {
            err = "Buff binary parse failed: " + path;
            return false;
        }
    } else {
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents, &snap->data); !result.ok()) {
            err = std::string("Buff json parse failed: ") + std::string(result.message());
            return false;
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
        for (const auto& elem : row_data.interval_effect()) {
            snap->intervalEffectIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.sub_buff()) {
            snap->subBuffIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.target_sub_buff()) {
            snap->targetSubBuffIndex.emplace(elem, &row_data);
        }
    }

    snap->healthRegenerationExpr.Init({
        "level", 
        "health"
    });

    out = std::move(snap);
    return true;
}

void BuffTableManager::Load() {
    std::shared_ptr<const Snapshot> snap;
    std::string err;
    if (!ParseSnapshotFromDir(GetConfigDir(), UseProtoBinaryTables(), snap, err)) {
        LOG_FATAL << err;
    }
    InstallSnapshot(std::move(snap));
}

std::pair<const BuffTable*, uint32_t> BuffTableManager::FindById(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        LOG_ERROR << "Buff table not found for ID: " << tableId;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const BuffTable*, uint32_t> BuffTableManager::FindByIdSilent(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
