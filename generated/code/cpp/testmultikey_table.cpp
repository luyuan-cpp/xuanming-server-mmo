#include "google/protobuf/util/json_util.h"
#include "core/utils/file/file2string.h"
#include "muduo/base/Logging.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "table/code/testmultikey_table.h"

std::string GetConfigDir();
bool UseProtoBinaryTables();

void TestMultiKeyTableManager::Load() {
    auto snap = std::make_unique<Snapshot>();

    if (UseProtoBinaryTables()) {
        const std::string path = GetConfigDir() + "testmultikey.pb";
        const auto contents = File2String(path);
        if (!snap->data.ParseFromString(contents)) {
            LOG_FATAL << "TestMultiKey binary parse failed: " << path;
        }
    } else {
        const std::string path = GetConfigDir() + "testmultikey.json";
        const auto contents = File2String(path);
        if (const auto result = google::protobuf::util::JsonStringToMessage(contents.data(), &snap->data); !result.ok()) {
            LOG_FATAL << "TestMultiKey" << result.message().data();
        }
    }

    for (int32_t i = 0; i < snap->data.data_size(); ++i) {
        const auto& row_data = snap->data.data(i);
        snap->idMap.emplace(row_data.id(), &row_data);
        snap->stringKeyMap.emplace(row_data.string_key(), &row_data);
        snap->uint32KeyMap.emplace(row_data.uint32_key(), &row_data);
        snap->int32KeyMap.emplace(row_data.int32_key(), &row_data);
        snap->mStringKeyMap.emplace(row_data.m_string_key(), &row_data);
        snap->mUint32KeyMap.emplace(row_data.m_uint32_key(), &row_data);
        snap->mInt32KeyMap.emplace(row_data.m_int32_key(), &row_data);
        for (const auto& elem : row_data.effect()) {
            snap->effectIndex.emplace(elem, &row_data);
        }
        for (const auto& elem : row_data.test_refs()) {
            snap->testRefsIndex.emplace(elem, &row_data);
        }
        snap->levelIndex[row_data.level()].push_back(&row_data);
        snap->testRefIndex[row_data.test_ref()].push_back(&row_data);
    }

    snapshot = std::move(snap);
}

std::pair<const TestMultiKeyTable*, uint32_t> TestMultiKeyTableManager::FindByStringKey(const std::string& key) const {
    const auto& snap = GetSnapshot();
    const auto it = snap.stringKeyMap.find(key);
    if (it == snap.stringKeyMap.end()) {
        LOG_ERROR << "TestMultiKey table not found for string_key: " << key;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const TestMultiKeyTable*, uint32_t> TestMultiKeyTableManager::FindByUint32Key(uint32_t key) const {
    const auto& snap = GetSnapshot();
    const auto it = snap.uint32KeyMap.find(key);
    if (it == snap.uint32KeyMap.end()) {
        LOG_ERROR << "TestMultiKey table not found for uint32_key: " << key;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const TestMultiKeyTable*, uint32_t> TestMultiKeyTableManager::FindByInt32Key(int32_t key) const {
    const auto& snap = GetSnapshot();
    const auto it = snap.int32KeyMap.find(key);
    if (it == snap.int32KeyMap.end()) {
        LOG_ERROR << "TestMultiKey table not found for int32_key: " << key;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const TestMultiKeyTable*, uint32_t> TestMultiKeyTableManager::FindByMStringKey(const std::string& key) const {
    const auto& snap = GetSnapshot();
    const auto it = snap.mStringKeyMap.find(key);
    if (it == snap.mStringKeyMap.end()) {
        LOG_ERROR << "TestMultiKey table not found for m_string_key: " << key;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const TestMultiKeyTable*, uint32_t> TestMultiKeyTableManager::FindByMUint32Key(uint32_t key) const {
    const auto& snap = GetSnapshot();
    const auto it = snap.mUint32KeyMap.find(key);
    if (it == snap.mUint32KeyMap.end()) {
        LOG_ERROR << "TestMultiKey table not found for m_uint32_key: " << key;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}

std::pair<const TestMultiKeyTable*, uint32_t> TestMultiKeyTableManager::FindByMInt32Key(int32_t key) const {
    const auto& snap = GetSnapshot();
    const auto it = snap.mInt32KeyMap.find(key);
    if (it == snap.mInt32KeyMap.end()) {
        LOG_ERROR << "TestMultiKey table not found for m_int32_key: " << key;
        return {nullptr, kInvalidTableId};
    }
    return {it->second, kSuccess};
}
