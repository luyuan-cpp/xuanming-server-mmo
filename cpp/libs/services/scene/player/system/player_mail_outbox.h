#pragma once

// 待寄邮件(docs/design/battle-overflow-mail.md):战斗结算两包都放不下时整批改投邮件。
//
// 写入(与结算同一帧,只在 scene loop 线程):
//   BagService::ConsumeInventoryAndGrant 两包都放不下时调 GrantOverflowRoute。战斗结算把它接到这里:
//   PrepareBattleOverflow 在任何副作用之前备好条目(分封、逐封取请求号、核待寄队列余量),
//   CommitPrepared 在发布背包候选的同一段里写进 PlayerMailOutboxComp。条目随这一次存档与扣消耗、入账一起提交。
//
// 寄送(Tick,每秒一次,每名玩家至多一封在途):
//   - 只寄「最近一次已提交的存档快照里已经有」的条目(RoleAdmissionSystem::IsSnapshotCommitted,与收件箱确认、资产通道的
//     落盘判定同一个依据):结算还没落盘就寄出,进程一崩结算会重做,邮件却已经发出,物品就多了一份。
//   - 玩家此刻可改(player_mutability)才寄:删条目是存档改动;交接、退出中、没有归属围栏时由下一个持有者再寄。
//   - 以 battle 身份签 MailAdmin.SendPersonalMail(密钥 MAIL_PRODUCER_SECRET_BATTLE,只有 scene 与 mail 两端持有)。
//     请求由条目逐字段确定性重建、不带过期时刻(邮件按首次落库那一刻算有效期);邮件按 (sender_kind, request_id) 先判重,
//     同一条重寄只会拿回原邮件。
//   - 回了 mail_id 才删条目,并请求一次关键进度存盘。其余一律留着按退避重试(mail_outbox_rules::RetryDelayMs),
//     没有放弃:物品不能因为寄不出去而消失。删了还没落盘就崩,下一次加载重寄,邮件回原邮件,再删。
// 线程模型:只在 scene loop 线程调用。寄送回调不持实体句柄,按 player_id 重新取实体。

#include <cstdint>
#include <functional>
#include <vector>

#include "entt/src/entt/entity/entity.hpp"
#include "mail_outbox_rules.h"

class PlayerMailOutboxSystem
{
public:
	static constexpr double kTickIntervalSec = 1.0;
	// 一封在途超过「调用期限(grpc_call_deadline::Get(MailNodeService))+ 2 秒」还没回,当作结果未知、按退避再寄
	// (GrpcDone 契约本该在期限内以失败完成,这只兜底)。迟到的回答若是寄到了照样删条目。
	static constexpr uint64_t kInFlightLostMarginMs = 2000;
	// 战斗结算改投的标题与正文(玩家可见)。改它们只影响之后新建的条目:已有条目存着自己的原文。
	static constexpr const char* kBattleOverflowTitle = "战斗奖励";
	static constexpr const char* kBattleOverflowBody = "背包已满,本场战斗获得的物品改由邮件发放,请腾出背包空位后领取附件。";

	// 一封备好的待寄邮件(还没写进存档)。
	struct PreparedMail
	{
		uint64_t requestId{0};
		mail_outbox_rules::MailItems items;
	};
	using PreparedMails = std::vector<PreparedMail>;

	// 战斗结算两包都放不下时,在任何副作用之前备好待寄条目:分封、逐封取请求号(发号器 txlog 段)、核待寄队列余量。
	// 返回 kSuccess 时 out 是备好的条目;否则 out 为空,回:
	//   kInvalidParameter   —— 空批次或 battleId 为 0;
	//   kServiceUnavailable —— 玩家身上缺 PlayerMailOutboxComp(加载不变量被破坏,ERROR),或请求号暂取不到(号段暂缺,WARN);
	//   kBagItemNotStacked  —— 待寄队列放不下这几封(邮件长期寄不出去,WARN)。
	// 只取号,不改存档;取了号而这一笔最终没提交,号作废即可(发号器的号不要求连续)。
	static uint32_t PrepareBattleOverflow(entt::entity player, uint64_t battleId, const mail_outbox_rules::MailItems& items,
										  PreparedMails& out);
	// 把备好的条目写进存档,与结算的其余效果同一帧。不会失败:组件与余量已在 PrepareBattleOverflow 里核过。
	static void CommitPrepared(entt::entity player, uint64_t battleId, const PreparedMails& prepared, uint64_t nowMs);

	// 定时器入口(kTickIntervalSec):对每名有可寄条目的玩家至多发一封。
	static void Tick();
	// 同 Tick,时刻由调用方给(单测用显式时刻驱动)。
	static void TickAt(uint64_t nowMs);

	// 一次寄送的回包要点。callOk:传输层拿到了回包。
	struct SendOutcome
	{
		bool callOk{false};
		uint32_t tipId{0};
		uint64_t mailId{0};
	};
	using SendDone = std::function<void(SendOutcome)>;
	// 发一封的结果。只有 kSent 之后 done 会被调用(在 loop 线程上恰好一次,从不在本次调用栈内同步调用);
	// 其余都没发出去,done 不会被调用。底层类型按枚举项个数选;kCount 恒为最后一项。
	enum class SendStart : uint8_t
	{
		kSent,
		kNoReadyNode,  // 发现里没有就绪的 mail 节点:多是短暂不可达
		kNoSecret,     // 没配 MAIL_PRODUCER_SECRET_BATTLE:部署问题,要人处理
		kInvalidEntry, // 条目形状不合法(加载时已打过 mail_outbox_invalid):邮件不会收,要人处理
		kSignFailed,   // 密钥不足 32 字节等:部署问题,要人处理

		kCount
	};
	using SendFn = std::function<SendStart(uint64_t playerId, const MailOutboxEntry& entry, SendDone done)>;
	// 这一条是否已在最近一次已提交的存档快照里(可以寄)。
	using DurableFn = std::function<bool(entt::entity player, uint64_t requestId)>;

	// 寄送完成(生产与单测同一入口)。回了 mail_id 就删条目(不论是不是这一次在途的那封:迟到的「寄到了」照样算数);
	// 其余只处理当前在途的那封,按 mail_outbox_rules::ClassifySend 记一次失败并退避。实体已不在或换了人时什么都不做。
	static void OnSendDone(uint64_t playerId, uint64_t requestId, SendOutcome outcome, uint64_t nowMs);

	// 单测替身。传空恢复生产实现。生产代码不调用。
	static void SetSendForTest(SendFn fn);
	static void SetDurableCheckForTest(DurableFn fn);
};
