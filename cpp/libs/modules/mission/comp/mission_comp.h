#pragma once

#include <array>
#include <bitset>

#include "proto/common/component/mission_comp.pb.h"
#include "core/type_define/type_define.h"
#include "modules/mission/comp/missions_config_comp.h"
#include "core/utils/registry/game_registry.h"
#include "core/utils/bit_index/bit_index_util.h"
#include "table/code/bit_index/mission_table_id_bit_index.h"

class AcceptMissionEvent;
class ConditionEvent;

using MissionsBits = std::bitset<kMissionMaxBitIndex>;

/// Per-scope container for active and completed missions.
class MissionsComp : public EventOwner
{
public:
    using EventMissionClassifyMap = std::unordered_map<uint32_t, UInt32Set>;

    MissionsComp();

    // ── Query ────────────────────────────────────────────────────────────

    [[nodiscard]] std::size_t MissionSize() const { return missionList_.missions().size(); }
    [[nodiscard]] std::size_t CompleteSize() const { return completedMissions_.count() + unmappedCompletedIds_.size(); }
    [[nodiscard]] std::size_t TypeSetSize() const { return typeFilter_.size(); }
    [[nodiscard]] std::size_t CanGetRewardSize() const;
    MissionsBits& GetCompleteMissions() { return completedMissions_; }
    MissionsBits& GetClaimableRewards() { return claimableRewards_; }

    [[nodiscard]] bool IsClaimable(const uint32_t missionId) const
    {
        return TestBit(MissionBitMap, claimableRewards_, missionId) || unmappedClaimableIds_.contains(missionId);
    }

    [[nodiscard]] bool IsAccepted(uint32_t missionId) const
    {
        return missionList_.missions().count(missionId) > 0;
    }

    [[nodiscard]] bool IsComplete(uint32_t missionId) const
    {
        if (!MissionBitMap.contains(missionId))
        {
            return unmappedCompletedIds_.contains(missionId);
        }
        return completedMissions_.test(MissionBitMap.at(missionId));
    }

    [[nodiscard]] bool IsMissionTypeNotRepeated() const { return missionTypeNotRepeated_; }

    [[nodiscard]] uint32_t ValidateNotAccepted(uint32_t missionId) const;
    [[nodiscard]] uint32_t ValidateNotCompleted(uint32_t missionId) const;

    // ── Accessors ────────────────────────────────────────────────────────

    [[nodiscard]] const MissionListComp& GetMissionList() const { return missionList_; }
    [[nodiscard]] MissionListComp& GetMutableMissionList() { return missionList_; }

    [[nodiscard]] const EventMissionClassifyMap& GetEventMissionsClassify() const { return eventMissionsClassify_; }
    [[nodiscard]] EventMissionClassifyMap& GetMutableEventMissionsClassify() { return eventMissionsClassify_; }

    [[nodiscard]] const UInt32PairSet& GetTypeFilter() const { return typeFilter_; }
    [[nodiscard]] UInt32PairSet& GetMutableTypeFilter() { return typeFilter_; }

    [[nodiscard]] const MissionsBits& GetCompletedMissions() const { return completedMissions_; }
    [[nodiscard]] MissionsBits& GetMutableCompletedMissions() { return completedMissions_; }

    // ── Test-only ────────────────────────────────────────────────────────

    [[nodiscard]] const auto& GetEventMissionsClassifyForUnitTest() const { return eventMissionsClassify_; }

    // ── Mutators ─────────────────────────────────────────────────────────

    void SetMissionTypeNotRepeated(bool value) { missionTypeNotRepeated_ = value; }
    void AbandonMission(uint32_t missionId);
    void RestoreCompleted(uint32_t missionId);
    void RestoreClaimable(uint32_t missionId);
    void ClearClaimable(uint32_t missionId);
    void RebuildIndexes(const IMissionConfig& config);
    [[nodiscard]] const UInt32Set& GetUnmappedCompletedIds() const { return unmappedCompletedIds_; }
    [[nodiscard]] const UInt32Set& GetUnmappedClaimableIds() const { return unmappedClaimableIds_; }

private:
    MissionListComp missionList_;
    EventMissionClassifyMap eventMissionsClassify_;
    UInt32PairSet typeFilter_;
    bool missionTypeNotRepeated_{ true };
    MissionsBits completedMissions_;
    MissionsBits claimableRewards_;
    // 配置删除后仍保留稳定任务 ID，避免下一次保存静默丢弃历史状态。
    UInt32Set unmappedCompletedIds_;
    UInt32Set unmappedClaimableIds_;
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