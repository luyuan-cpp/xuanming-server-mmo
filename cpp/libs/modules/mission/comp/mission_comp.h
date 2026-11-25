#pragma once

#include <array>
#include <bitset>
#include <string>
#include <string_view>

#include "proto/common/component/mission_comp.pb.h"
#include "core/type_define/type_define.h"
#include "modules/mission/comp/missions_config_comp.h"
#include "core/utils/registry/game_registry.h"
#include "core/utils/bit_index/bit_index_util.h"
#include "table/code/bit_index/mission_table_id_bit_index.h"

struct AcceptMissionEvent;
struct ConditionEvent;

using MissionsBits = std::bitset<kMissionBitmapCapacity>;

/// Per-scope container for active and completed missions.
class MissionsComp
{
public:
    using EventMissionClassifyMap = std::unordered_map<uint32_t, UInt32Set>;

    MissionsComp();

    // ── Query ────────────────────────────────────────────────────────────

    [[nodiscard]] std::size_t MissionSize() const { return missionList.missions().size(); }
    [[nodiscard]] std::size_t CompleteSize() const { return completedMissions.count(); }
    [[nodiscard]] std::size_t TypeSetSize() const { return typeFilter.size(); }
    [[nodiscard]] std::size_t CanGetRewardSize() const;
    MissionsBits& GetCompleteMissions() { return completedMissions; }
    MissionsBits& GetClaimableRewards() { return claimableRewards; }

    [[nodiscard]] bool IsClaimable(const uint32_t missionId) const
    {
        return TestBit(MissionBitMap, claimableRewards, missionId) || unmappedClaimableIds.contains(missionId);
    }

    [[nodiscard]] bool IsAccepted(uint32_t missionId) const
    {
        return missionList.missions().count(missionId) > 0;
    }

    // 每个任务都有位序(任务表 id 列带位序标记,删掉的任务也保留原位),没有位序的 ID 不是任何版本的任务。
    [[nodiscard]] bool IsComplete(uint32_t missionId) const { return TestBit(MissionBitMap, completedMissions, missionId); }

    [[nodiscard]] bool IsMissionTypeNotRepeated() const { return missionTypeNotRepeated; }

    [[nodiscard]] uint32_t ValidateNotAccepted(uint32_t missionId) const;
    [[nodiscard]] uint32_t ValidateNotCompleted(uint32_t missionId) const;

    // ── Accessors ────────────────────────────────────────────────────────

    [[nodiscard]] const MissionListComp& GetMissionList() const { return missionList; }
    [[nodiscard]] MissionListComp& GetMutableMissionList() { return missionList; }

    [[nodiscard]] const EventMissionClassifyMap& GetEventMissionsClassify() const { return eventMissionsClassify; }
    [[nodiscard]] EventMissionClassifyMap& GetMutableEventMissionsClassify() { return eventMissionsClassify; }

    [[nodiscard]] const UInt32PairSet& GetTypeFilter() const { return typeFilter; }
    [[nodiscard]] UInt32PairSet& GetMutableTypeFilter() { return typeFilter; }

    [[nodiscard]] const MissionsBits& GetCompletedMissions() const { return completedMissions; }
    [[nodiscard]] MissionsBits& GetMutableCompletedMissions() { return completedMissions; }

    // ── Test-only ────────────────────────────────────────────────────────

    [[nodiscard]] const auto& GetEventMissionsClassifyForUnitTest() const { return eventMissionsClassify; }

    // ── Mutators ─────────────────────────────────────────────────────────

    void SetMissionTypeNotRepeated(bool value) { missionTypeNotRepeated = value; }
    void AbandonMission(uint32_t missionId);
    void RestoreCompleted(uint32_t missionId);
    // 存档里的完成位图(MissionListComp.complete_missions):第 i 位就是位序 i,每字节低位在前,末尾全 0 的字节不写。
    // 位序由导表器只增不改、永不复用地发放(tools/data_table_exporter/core/generators/bit_index_gen.py),所以能直接落盘。
    [[nodiscard]] std::string CompletedBitsForSave() const;
    void RestoreCompletedBits(std::string_view bits);
    void RestoreClaimable(uint32_t missionId);
    void ClearClaimable(uint32_t missionId);
    void RebuildIndexes(const IMissionConfig& config);
    [[nodiscard]] const UInt32Set& GetUnmappedClaimableIds() const { return unmappedClaimableIds; }

private:
    MissionListComp missionList;
    EventMissionClassifyMap eventMissionsClassify;
    UInt32PairSet typeFilter;
    bool missionTypeNotRepeated{ true };
    MissionsBits completedMissions;
    MissionsBits claimableRewards;
    // 待领取按任务 ID 存档:本二进制不认识的 ID(更新版本的表才有)原样保留,避免下一次保存静默丢弃。
    UInt32Set unmappedClaimableIds;
    // 存档位图里本二进制放不下的位(位序 >= kMissionBitmapCapacity,更新版本的表才有),存盘时原样带回。
    std::string unknownCompletedBits;
};

using PlayerMissionList = std::array<MissionsComp, MissionListComp::kPlayerMissionSize>;


struct MissionsContainerComp {
	std::unordered_map<uint32_t, MissionsComp> map;

	MissionsComp& GetOrCreate(uint32_t scope) {
		auto it = map.find(scope);
		if (it == map.end()) {
			auto [newIt, _] = map.emplace(scope, MissionsComp{});
			return newIt->second;
		}
		return it->second;
	}

    MissionsComp* GetMutable(uint32_t scope) {
        auto it = map.find(scope);
        return (it == map.end()) ? nullptr : &it->second;
    }

	const MissionsComp* Get(uint32_t scope) const {
		auto it = map.find(scope);
		return (it == map.end()) ? nullptr : &it->second;
	}
};
