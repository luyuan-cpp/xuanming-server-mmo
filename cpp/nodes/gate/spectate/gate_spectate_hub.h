#pragma once

// gate 的观战订阅中心:L1 中继 / L2 广播两档的边缘扇出(docs/design/battle-spectate-tiered-fanout.md §15.5、§7)。
//
// 职责:记住「哪场比赛有哪些会话在看」,把 battle 经 Kafka 发来的观战帧扇出给这些会话,源站失联时替 battle 收尾。
// 凭证校验在 gate_spectate_ticket.h,会话表、连接、主 loop 与时钟经 HubPorts 注入(装配在 gate_spectate_client.h),
// 所以核心逻辑可以直接单测(tests/gate_spectate_hub_test.cpp)。
//
// 线程模型:所有方法只能在 gate 主 loop 线程调用,不加锁。Kafka 消费线程经 queueInLoop 把帧交回主 loop。
//
// 生命周期(AGENTS.md §11.7):只存 session_id,不存连接,发送时由 sendFrame 查会话表并 lock() 弱引用;
// 续发闭包只捕获 weak_ptr<SpectateHub> 与值,hub 没了续发自动失效。所以必须经 std::make_shared 创建。
//
// 帧语义(每帧都是全量状态,跳过中间帧只少播一回合动画,下一帧自愈,所以这里从不补发):
//   - 实例:只认条目登记的 battle_instance_id,其它实例的帧丢弃(防僵尸)。条目的实例以订阅凭证为准。
//   - 两条水位,各管一件事(客户端去重同口径,写成一条水位就会复发同一个缺陷,见 battle-spectate-tiered-fanout.md §6.1 / §9):
//     158 只与 158 比较:TURN 只收 round_index > lastTurnRound,决定「这帧发不发」。161 不推进这条水位:STATE(r) 是第 r 回合
//     结算前的快照,紧接着的 TURN(r) 必须照常扇出,否则每次挂接 / 升档都少播一回合,发生在最后一回合时看不到终局那回合的结算。
//     cachedStateRound 只决定「缓存换不换」:TURN(r) 带的 state 是结算后的状态,回合是 r + 1(终局回合停在 r,之后只有 END,
//     不影响判定),所以 STATE(r) 记 r、TURN(r) 记 r + 1,STATE 只收 round_index >= cachedStateRound。
//     END 只收一次。升档时中继与广播重叠、Kafka 重复投递都由这些单调规则吸收。
//   - 首帧(D69):客户端收到 161 才算进入观战,所以一个会话收到的第一帧一定是 161:订阅时有缓存就立刻发;
//     没有缓存或首帧被软水位跳过时,下一个 STATE / TURN 给它发 161(TURN 则由该帧的 state 合成),不发 158。
//     已经在看的会话不收 STATE。
//   - 扇出:同一帧只编码一次;每批最多 fanoutBatch 个目标,余下经 defer 续发;
//     续发途中来了新的 TURN,没发完的目标直接改发新帧(合并,计 coalesced),名单接着上一轮停下的位置发:
//     过载时大家轮流丢帧,而不是表头帧帧都收到、表尾一帧也收不到。
//   - 软水位:状态帧在连接输出缓冲过高时跳过并计数,不等 2MB 高水位把大厅连接强关。
//   - 终局:166 不合并。它不带状态也不带事件,抢占在途的一轮,没轮到的会话就永远看不到最后一回合;battle 又总是紧接着最后
//     一回合的 TURN 发 END,两帧几乎总在同一个 Kafka batch 里。所以 END / 源站失联都等在途的一轮发完,再对全部订阅者扇出 166。
//     END 扇出后留墓碑,墓碑期内新的订阅直接收到 166;源站失联合成 166(SOURCE_LOST)后直接删条目,
//     不留墓碑:失联可能只是暂时的,新凭证说明 battle 还在,后续帧要能重建条目。
//
// 计数只增不减,低基数,不带 battle_id / player_id;每 metricInterval 一行 `metric=gate_spectate ...`,由 SweepIdle 驱动。

#include <chrono>
#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <unordered_map>
#include <vector>

#include "engine/core/type_define/type_define.h"

class MessageContent;
class SpectateTicketPayload;

namespace contracts::kafka
{
	class SpectateFrameEvent;
}

namespace gate_push
{
	class EncodedFrame;
}

namespace gate_spectate
{
	using SteadyTime = std::chrono::steady_clock::time_point;

	// 这一帧能不能被软水位跳过。状态帧每帧全量,跳过一帧下一帧自愈;终局帧之后不会再有帧,跳过它客户端就永远等不到收尾,
	// 而它只有几十字节,所以不受软水位限制(真卡死的连接仍由 2MB 高水位强关)。
	enum class FrameDelivery : uint8_t
	{
		kBestEffort,
		kMustDeliver,
	};

	enum class SendResult : uint8_t
	{
		kSent,
		kBufferFull,  // 输出缓冲超过软水位,这一帧跳过;订阅保留
		kSessionGone, // 会话不存在或连接已释放;hub 顺手摘除订阅
	};

	// 外部依赖全部显式注入。sendFrame 不得同步回调 hub(muduo 的 send 不会同步触发断开回调,满足这一条)。
	struct HubPorts
	{
		std::function<SendResult(SessionId, const gate_push::EncodedFrame &, FrameDelivery)> sendFrame;
		std::function<void(std::function<void()>)> defer; // 排到主 loop 队尾(queueInLoop),让两批之间能处理网络事件
		std::function<SteadyTime()> steadyNow;
	};

	struct HubLimits
	{
		// 一次 2 万人的扇出若不分批,会卡住这台 gate 上所有会话的收发、定时器和 RPC 回包(§7)。
		std::size_t fanoutBatch{512};
		// 必须大于手动行动窗口 6s 加结果提交等待(§5.3)。
		std::chrono::seconds idleTimeout{30};
		std::chrono::seconds endedRetention{600};
		uint64_t deadlineGraceMs{60'000};
		std::chrono::seconds metricInterval{30};
	};

	struct HubCounters
	{
		uint64_t framesFannedOut{0};    // 开始扇出的帧数(有目标的才算)
		uint64_t softWatermarkSkips{0}; // 按会话计
		uint64_t coalesced{0};          // 按会话计:没发完就被新帧取代的目标
		uint64_t ticketRejects{0};      // 由装配层经 NoteTicketRejected 报告
		uint64_t sourceLost{0};         // 按条目计:合成 SOURCE_LOST 收尾的场次
		uint64_t instanceMismatch{0};   // 实例与条目不符被丢弃的帧
		uint64_t invalidFrames{0};      // 字段缺失、种类与消息号不符、无法解析的帧
	};

	class SpectateHub : public std::enable_shared_from_this<SpectateHub>
	{
	public:
		SpectateHub(HubPorts ports, HubLimits limits);

		// 前置条件:ticket 已通过 VerifySpectateTicket(本函数不再校验身份与期限)。
		// 一个会话同时只订一场,订别的场时先退旧场;同场重复订阅保持订阅并重发首帧。
		// 条目的实例与凭证不符时以凭证为准并清掉缓存。墓碑期内不建立订阅,直接发一帧 166。
		void Subscribe(SessionId sessionId, const SpectateTicketPayload &ticket);

		// 与当前订阅不符时什么都不做(幂等)。
		void Unsubscribe(SessionId sessionId, uint64_t battleId);

		// 会话销毁时调用(所有 erase 路径都要调,否则 session_id 复用后新连接会收到旧订阅的帧)。
		// 回选角(LeaveGame,TCP 不断)也调用:这条连接之后可能换角色,而订阅凭证绑的是离开的那个角色。
		// 只动订阅表,不发帧、不碰连接;同一个会话之后可以重新订阅。
		void OnSessionClosed(SessionId sessionId);

		// 收到一帧观战帧。没有订阅者的场次也建条目缓存帧,让之后的订阅者立刻拿到首帧。
		void OnFrame(const contracts::kafka::SpectateFrameEvent &event);

		// 周期巡检:有订阅者的条目超过 idleTimeout 无帧,或过了 deadline_ms + deadlineGraceMs,合成 166 收尾并删除;
		// 没有订阅者的这类条目直接删除;到期的墓碑删除。顺带按 metricInterval 打指标行。
		void SweepIdle(SteadyTime nowSteady, uint64_t nowWallMs);

		void NoteTicketRejected() { ++counters_.ticketRejects; }

		const HubCounters &Counters() const { return counters_; }
		std::size_t SubscriberCount() const { return battleOfSession_.size(); }
		std::size_t EntryCount() const { return entries_.size(); }

	private:
		enum class Phase : uint8_t
		{
			kLive,       // 正常收帧
			kEnded,      // 收到过 END:不再收帧;在途的一轮发完后扇出 166,之后留墓碑,直到 endedRetention 到期
			kSourceLost, // 源站失联:不再收帧;在途的一轮发完后扇出合成的 166,发完就删条目
		};

		struct Subscriber
		{
			bool receivedState{false}; // 已收到过带全量状态的帧;false 时下一帧必须是 161(D69)
		};

		struct Entry
		{
			std::string battleInstanceId;
			std::unordered_map<SessionId, Subscriber> subscribers;
			Phase phase{Phase::kLive};
			// 两条水位各管一件事。STATE(r) 是第 r 回合结算前的快照,TURN(r) 是第 r 回合结算后的结果,两者回合号相同但
			// TURN 更新;只用一条水位的话,挂接时的 STATE(r) 会把紧接着的 TURN(r) 判成重复丢掉,观众少看一回合。
			// -1 = 还没收过,round_index 是 uint32,存得下。
			int64_t lastTurnRound{-1}; // 已扇出过的最大 TURN 回合:只有 TURN 推进,决定「这帧发不发」
			int64_t cachedStateRound{-1}; // 缓存里状态所处的回合:STATE(r) 记 r,TURN(r) 记 r + 1,决定「缓存换不换」
			SteadyTime lastFrameSteady{};
			SteadyTime endedAt{};
			uint64_t deadlineMs{0};

			std::shared_ptr<const MessageContent> latestState;           // 最新的 161 / 158
			std::shared_ptr<const gate_push::EncodedFrame> firstFrame;   // latestState 对应的 161 编码,惰性生成,换帧即失效
			std::shared_ptr<const gate_push::EncodedFrame> finalFrame;   // 166;kEnded / kSourceLost 时非空

			// 正在进行的一轮扇出:一帧 + 这一帧开始时的目标快照。新订阅者不在快照里:它们已经从缓存拿到了同一帧的状态。
			std::shared_ptr<const gate_push::EncodedFrame> fanoutFrame;
			std::vector<SessionId> fanoutTargets;
			std::size_t fanoutNext{0};
			// 下一轮 TURN 的名单从 AllSubscribers 序的第几个开始(累计值,NextTurnTargets 里有推导)。
			std::size_t turnFanoutStart{0};
			// 非 0 = 已排了一个续发,至多一个。续发闭包核对这个号:条目删除重建或号已被消费后,旧闭包自动作废。
			uint64_t continuationToken{0};
		};

		static std::vector<SessionId> AllSubscribers(const Entry &entry);
		// TURN 扇出的名单:全部订阅者,转到上一轮停下的位置。前置条件:有订阅者。
		static std::vector<SessionId> NextTurnTargets(Entry &entry);
		bool IsSourceSilent(const Entry &entry, SteadyTime nowSteady, uint64_t nowWallMs) const;

		void RebindToInstance(Entry &entry, const std::string &battleInstanceId);
		void DetachSession(SessionId sessionId);

		void OnStateFrame(uint64_t battleId, Entry &entry, int64_t round, std::shared_ptr<const MessageContent> content);
		void OnTurnFrame(uint64_t battleId, Entry &entry, int64_t round, std::shared_ptr<const MessageContent> content);
		void OnEndFrame(uint64_t battleId, Entry &entry, const MessageContent &content);
		void CloseAsSourceLost(uint64_t battleId, Entry &entry);

		// 惰性生成首帧:161 原样编码;158 解析出 state 合成 161(observer_count = 0 表示未知)。无缓存或合成失败返回空。
		std::shared_ptr<const gate_push::EncodedFrame> FirstFrameOf(Entry &entry);

		void SendToSubscriber(SessionId sessionId, Subscriber &subscriber, const gate_push::EncodedFrame &frame,
							  FrameDelivery delivery);

		// 下面四个调用之后 entry 可能已被删除(kSourceLost 发完),调用方不得再访问它。
		// 没有在途的一轮时对全部订阅者开始扇出 finalFrame;有的话什么都不做,由 FinishFanout 在那一轮发完后再调。
		void StartFinalFanoutUnlessBusy(uint64_t battleId, Entry &entry);
		void StartFanout(uint64_t battleId, Entry &entry, std::shared_ptr<const gate_push::EncodedFrame> frame,
						 std::vector<SessionId> targets);
		void PumpFanout(uint64_t battleId, Entry &entry);
		void FinishFanout(uint64_t battleId, Entry &entry);

		void ContinueFanout(uint64_t battleId, uint64_t token);
		void DeliverToTarget(Entry &entry, SessionId sessionId);
		void LogMetricsIfDue(SteadyTime now);

		HubPorts ports_;
		HubLimits limits_;
		std::unordered_map<uint64_t, Entry> entries_;
		std::unordered_map<SessionId, uint64_t> battleOfSession_; // 不变量:有记录 ⇔ 会话在该场条目的 subscribers 里
		HubCounters counters_;
		uint64_t lastContinuationToken_{0};
		SteadyTime lastMetricAt_{};
	};
} // namespace gate_spectate
