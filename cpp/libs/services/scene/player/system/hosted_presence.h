#pragma once

#include <chrono>
#include <cstdint>
#include <string>
#include <string_view>

#include "engine/core/type_define/type_define.h" // Guid / SessionId
#include "player_exit_intent.h"                  // player_exit::IsBoundSession

// 离线托管角色的在场心跳与托管归位信息(docs/design/player-connection-lost-hold.md §4.5「托管归位」)。
// 只有纯判定、常量与值格式,不碰 ECS、不发命令;发送在 PlayerLifecycleSystem::RefreshHostedPresence。
//
// 契约:
//   - 在场心跳 player:{id}:hosted_presence(player_ownership::HostedPresenceRedisKey):此刻对这名托管角色负责的 scene 节点
//     每 kRefreshPeriod 续一次,存活 kPresenceTtl。"负责"只有四种:托管着他(connection_lost_hold::IsHosting)、带着托管改派
//     票据退出中、持有托管改派待确认条目、持有托管落点看护。键不在 = 没有任何节点对他负责。
//   - 托管归位信息 player:{id}:hosted_return(player_ownership::HostedReturnRedisKey):只由托管着他的节点随心跳写,
//     值 FormatReturnRecord,存活 = 月卡剩余秒数。改派、看护途中登录身份与归属修订都不变,托管着他的那个节点写的一份一直有效。
//   - 读者只有 player_locator:续租时月卡仍有效、心跳不在、归位信息的登录身份就是会话那一次登录 → 向角色所在 zone 的
//     scene_manager 发托管归位(EnterScene,hosted_placement、不带 gate、不带去向、关联号 0)。
//   - 格式与 Go player_locator 的 parseHostedReturnRecord 逐字对齐,两侧单测钉同一条金样;上线后只能换版本号前缀,不改旧版。
namespace hosted_presence
{
	// 续心跳的周期与心跳的存活时长。存活取三个周期:漏两次续期(扫描跳过一秒、一次写失败)不会被误判成没人负责;
	// 换手时源节点最后一次续与目标节点第一次续之间最多隔两个周期,也盖得住。player_locator 每 300 秒才看一次,
	// 再长没有收益,只会拖慢节点崩溃之后的归位。
	inline constexpr std::chrono::seconds kRefreshPeriod{20};
	inline constexpr std::chrono::seconds kPresenceTtl{60};
	static_assert(kPresenceTtl >= 3 * kRefreshPeriod, "presence must survive two missed refreshes");

	// 这一秒要不要替这名玩家续心跳。按玩家号错开(玩家号是打散过的发号器值,取模均匀),同一节点上的托管角色不在同一秒集中写;
	// 每名玩家每个周期恰好一秒到期。nowUnixSec 是墙钟 Unix 秒,由 1 秒节拍传入;节拍偶尔跳过一秒时那一轮顺延到下一个周期。
	constexpr bool IsRefreshDue(Guid playerId, uint64_t nowUnixSec)
	{
		return (playerId + nowUnixSec) % static_cast<uint64_t>(kRefreshPeriod.count()) == 0;
	}

	// 归位信息的版本前缀。
	inline constexpr std::string_view kReturnRecordVersion = "v1";

	// 四个字段都非 0 才写:zone 定 player_locator 把请求发给哪个 scene_manager(location 不在时),归属修订是 EnterScene 的
	// 授权(scene_manager 拒绝 0),会话号与代次是断开那次登录的身份(托管落点要求非 0)。
	constexpr bool ReturnRecordWritable(uint32_t zoneId, uint64_t ownerRevision, SessionId sessionId, uint64_t loginGeneration)
	{
		return zoneId != 0 && ownerRevision != 0 && player_exit::IsBoundSession(sessionId) && loginGeneration != 0;
	}

	// "v1:<zone>:<owner_revision>:<session_id>:<login_generation>",全部十进制、无前导零。
	inline std::string FormatReturnRecord(uint32_t zoneId, uint64_t ownerRevision, SessionId sessionId, uint64_t loginGeneration)
	{
		std::string record(kReturnRecordVersion);
		record += ':';
		record += std::to_string(zoneId);
		record += ':';
		record += std::to_string(ownerRevision);
		record += ':';
		record += std::to_string(sessionId);
		record += ':';
		record += std::to_string(loginGeneration);
		return record;
	}

	// 归位信息的存活秒数 = 月卡剩余秒数:月卡到期之后再归位没有意义,player_locator 那时也不再续租。0 = 不写。
	// 月卡续费后由下一次心跳带着新的到期时刻重写;托管角色不在任何节点上时没人重写,按旧的到期时刻过期(下次登录照常载入)。
	constexpr uint64_t ReturnRecordTtlSec(uint64_t passExpiresAtSec, uint64_t nowUnixSec)
	{
		return passExpiresAtSec > nowUnixSec ? passExpiresAtSec - nowUnixSec : 0;
	}
} // namespace hosted_presence
