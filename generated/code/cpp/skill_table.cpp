#include "google/protobuf/util/json_util.h"
#include "core/utils/file/file2string.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/skill_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

void SkillTableManager::Load() {
    auto snap = std::make_unique<Snapshot>();

    if (UseProtoBinaryTables()) {
        const std::string path = GetConfigDir() + "skill.pb";
        const auto contents = File2String(path);
        if (!snap->data.ParseFromString(contents)) {
            LOG_FATAL << "Skill binary parse failed: " << path;
        }
    } else {
        const std::string path = GetConfigDir() + "skill.json";
        const auto contents = File2String(path);
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents.data(), &snap->data); !result.ok()) {
            LOG_FATAL << "Skill" << result.message().data();
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
        for (const auto& elem : row_data.skill_type()) {
            snap->skillTypeIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.targeting_mode()) {
            snap->targetingModeIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.effect()) {
            snap->effectIndex.emplace(elem, &row_data);
        }
    }

    snap->damageExpr.Init({
        "level"
    });

    snapshot = std::move(snap);
}

std::pair<const SkillTable*, uint32_t> SkillTableManager::FindById(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        LOG_ERROR << "Skill table not found for ID: " << tableId;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const SkillTable*, uint32_t> SkillTableManager::FindByIdSilent(const uint32_t tableId) {
    const auto& snap = GetSnapshot();
    const auto it = snap.idMap.find(tableId);
    if (it == snap.idMap.end()) {
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
