#pragma once

// 挂机月卡(docs/design/afk-monthly-card.md):开通、GM 设定、查询,以及玩家域 Redis 投影的写出。
//
// 事实源是存档里的 PlayerUint64Comp.afk_pass_expires_at(持久数据,加载时挂上);本类是它唯一的写入方:
//   - Buy:花灵石开通 / 续费(带客户端幂等键,扣钱与加天数在同一次同步调用里、随同一次存盘提交);
//   - GmSet:GM 设定剩余时长(调用方已过 GM 闸)。
// 读方:离线托管判定(AfkHostingSystem)读 ExpiresAt;别的服务(player_locator)读 Redis 投影。
//
// 投影 player:afk_pass:{<player_id>} 只写**已提交**的值,所以永远不领先于库(设计 §5.2):
//   - OnPlayerLoaded:加载进场成功之后,按刚从库里读出的值写一次(SET 或 DEL)—— 回档、投影写丢、Redis 丢键都在这里改正;
//   - OnSaveCommitted:一笔存盘提交回来,提交的值与上一次已提交的不同才写;
//   - RefreshProjection:托管期间随在场心跳(20 秒)补写一次 SET,Redis 丢了键也不会让 player_locator 把付费玩家清出世界。
// 写失败只打日志、不重试:下一次变化、下一次加载、下一个心跳周期都会重写。
//
// 线程模型:只在 scene loop 线程调用;Redis 回调也在 loop 线程,只捕获值。

#include <cstdint>
#include <functional>

#include "engine/core/type_define/type_define.h" // Guid
#include "entt/src/entt/entity/entity.hpp"

class AfkPassInfo;
class BuyAfkPassRequest;
class BuyAfkPassResponse;
class PlayerAllData;

// 最近一次已提交(加载,或存盘提交回来)的月卡到期时刻。派生运行时数据(ecs-component-access-rules.md):
// 加载进场时由 AfkPassSystem::OnPlayerLoaded 创建,只由 AfkPassSystem 读写,不落库、不序列化。
// 没有它 = 实体不是经加载链建出来的(单测夹具),投影一律不写。
// projectionSeq / confirmedSeq 追踪最近一次投影写出是否已收到玩家域 Redis 成功回包:
// 若某次 SET 或 DEL 因 Redis 抖动失败,后续存盘提交、托管开始与心跳都会自动重发自愈,既不丢投影也不留脏键。
struct AfkPassCommittedComp
{
	uint64_t expiresAtSec{0};
	uint64_t projectionSeq{0};
	uint64_t confirmedSeq{0};
};

class AfkPassSystem
{
public:
	// 配表启动 / 热更校验(SceneNodeHooks::TableLoadHandler::Validate):校验失败拒绝热更。
	static bool ValidateTables();

	// 本实体内存里的到期时刻(事实源的那一份,加载后只由本类改);缺 PlayerUint64Comp 或实体无效返回 0。只读。
	static uint64_t ExpiresAt(entt::entity player);

	// 仙途轻享卡在指定墙钟秒 / 当前墙钟秒是否生效中（单一事实源：expires_at > nowUnixSec）。只读。
	static bool IsActive(entt::entity player, uint64_t nowUnixSec);
	static bool IsActiveNow(entt::entity player);

	// 只读:离线托管期间离队 / 解散时是否自动回默认主城安全区。缺 PlayerUint64Comp 或实体无效返回 false。
	static bool AutoReturnCityOnTeamEnd(entt::entity player);

	// 填当前状态:到期、服务器时间、售价与余额、托管偏好设置与待确认收获简报。规则表不可用时售价、天数、上限、提醒、余额都填 0(客户端显示暂不出售)。
	static void FillInfo(entt::entity player, uint64_t nowUnixSec, AfkPassInfo &info);

	// 开通 / 续费(设计 §4.1)。成功(含重放)返回 kSuccess,response.info 是开通之后的状态;失败返回 tip 码,
	// 什么都没改。nowUnixSec 是墙钟 Unix 秒。
	static uint32_t Buy(entt::entity player, const BuyAfkPassRequest &request, uint64_t nowUnixSec, BuyAfkPassResponse &response);

	// GM 设定剩余时长(设计 §4.3):到期 = now + remainingSeconds,0 = 清除;超过 366 天回 kInvalidParameter。
	// 调用方负责 GM 闸(scene_gm_guard::RejectGmClientRpc);与 Buy 共用 player_mutability 写围栏。成功返回 kSuccess 并填 info。
	static uint32_t GmSet(entt::entity player, uint32_t remainingSeconds, uint64_t nowUnixSec, AfkPassInfo &info);

	// 修改离线托管偏好设置(例如离队 / 解散自动回默认主城安全区)。受 player_mutability 写围栏保护;成功返回 kSuccess 并填 info。
	static uint32_t SetSettings(entt::entity player, bool autoReturnCityOnTeamEnd, uint64_t nowUnixSec, AfkPassInfo &info);

	// 确认已阅离线托管收获简报:仅当存在待确认简报且 startedAt == 0 或与当前简报 started_at 一致时清零,幂等返回 kSuccess 并填 info。
	static uint32_t AckHostedRecap(entt::entity player, uint64_t startedAt, uint64_t nowUnixSec, AfkPassInfo &info);

	// ── 离线托管收获简报生命周期(只汇总真实结算收益,不额外发放任何产出)──
	// 托管开始(ApplyProbeResult 首次置位 hostingStarted):若当前没有进行中的简报则开新会话;
	// 跨节点托管落点重建保留时(started_at != 0 && ended_at == 0)沿用原会话,不丢历史累计。
	static void OnHostingStarted(entt::entity player, uint64_t nowUnixSec);
	// 战斗结算同帧累加(ApplySettlementToEntity):仅当实体正处于离线托管(PlayerConnectionLostComp.hostingStarted)时累加,
	// 随同一次存盘事务落库(满足 I3 原子性)。
	static void RecordHostedBattleSettlement(entt::entity player, bool won, uint64_t expGained, uint64_t silverGained,
											 uint32_t itemCount, uint64_t nowUnixSec);
	// 托管结束(玩家重连接回 ResumeConnectionLostHold 或月卡到期退出 ExpireConnectionLostHolds):盖章 ended_at 并置 pending_ack=true。
	static void OnHostingEnded(entt::entity player, uint64_t nowUnixSec, bool requestSave);

	// ── 投影(设计 §5.2)──
	static void OnPlayerLoaded(entt::entity player, uint64_t nowUnixSec);
	// committed 是这一笔确实提交的整份快照(PlayerLifecycleSystem::OnPlayerStoreSaveDone 的非拒绝、非被取代分支)。
	static void OnSaveCommitted(Guid playerId, const PlayerAllData &committed, uint64_t nowUnixSec);
	static void RefreshProjection(entt::entity player, uint64_t nowUnixSec);
	// 本线程投影写失败(没发出去,或回包不是成功)的累计,进程启动以来。由 AfkHostingSystem::WriteMetrics 导出。
	static uint64_t ProjectionFailures();

	// 单测注入:取代真实的 Redis 写出,收到 (玩家, 到期, true = SET / false = DEL)。生产恒为空。
	using ProjectionSender = std::function<void(Guid playerId, uint64_t expiresAtSec, bool set)>;
	static void SetProjectionSenderForTest(ProjectionSender sender);
	// 单测辅助:控制 SetProjectionSenderForTest 路径是否模拟 Redis 写失败(未确认),用于验证失败自愈重试。生产恒为 false。
	static void SetProjectionFailForTest(bool fail);
};
