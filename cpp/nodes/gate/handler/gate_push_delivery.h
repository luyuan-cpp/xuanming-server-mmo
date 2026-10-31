#pragma once

// gate 把服务端推送写进客户端连接的唯一收口。
//
// 两条入口共用:节点间 RPC(GateHandler,scene → gate)与 Kafka(GateEventHandler,Go 服务 / battle → gate)
// 的单播、指名广播、场景广播、全服广播都只经这里,不再各写一份(以前 Kafka 版没有身份栅栏,
// 场景 / 全服广播是空实现,见 docs/design/battle-spectate-tiered-fanout.md §14 G2 / G3)。
//
// 线程模型:只能在 gate 主 loop 线程调用。会话表 tlsSessionManager、计数与采样状态都是 thread_local,
// 从别的线程调用读到的是另一张空表,推送会被静默当成「会话不存在」。
//
// 身份栅栏(docs/design/routing-identity-audit-20260908.md R13 / R14):单播与指名广播写 socket 之前
// 核对「会话当前绑定的玩家 == 目标玩家」(session_identity_fence.h ClassifyPush 三态):
//   - 不符:丢弃并计数(session_id 序号回绕或 gate node_id 复用后,这条推送本来会写进别人的连接);
//   - 目标玩家为 0(发送方没带,兼容位;login 在 Bind 之前的失败提示显式走这一位):照发并计数,每条入口每线程首次记一行 INFO。
// 场景 / 全服广播没有目标玩家可比对,不过栅栏;只推已绑定玩家的会话,未登录的连接不收游戏广播。
//
// 编码一次:广播的同一条 MessageContent 只编码一份客户端帧字节(EncodedFrame),对每个会话 conn->send(data, len) 复用。
// 前提是出站帧与会话无关;gate codec 将来若按会话签名,这条复用即作废。单播仍经 gate codec 编码。
//
// 观测:每条入口一份 thread_local 计数(投递、unfenced、栅栏丢弃、会话不存在、无连接、无效消息、
// scene_id 为 0),不按玩家分维度。丢弃类第一条必打、此后每 1024 条一行
// `metric=gate_push_dropped path=<rpc|kafka> reason=<fence|invalid|scene0> total=N`,紧跟一行带定位信息与
// 全部计数的明细;会话不存在是断线竞态的常态,只计数并打 DEBUG。
//
// 推送是尽力而为的单向通知:函数不回执、不抛异常,丢弃即结束,由计数与采样日志留痕。

#include <cstddef>
#include <cstdint>

#include "muduo/net/Buffer.h"
#include "muduo/net/TcpConnection.h"
#include "network/codec/codec.h"
#include "proto/common/base/message.pb.h"

class BroadcastToPlayersRequest;

namespace contracts::kafka
{
	class BroadcastToPlayersEvent;
}

namespace gate_push
{
	// 推送从哪条入口进来。只作计数与日志的维度(低基数);kCount 是数组长度,不是合法入口。
	enum class Path : uint8_t
	{
		kRpc,
		kKafka,
		kCount,
	};

	// 每条入口一份。各项互不相交:一次投递尝试恰好落进其中一项。invalidMessage 与 droppedScene0 按条计
	// (整条拒绝),其余按会话计;场景 / 全服广播跳过的未绑定会话不是目标,不计。
	// 不按玩家分维度(玩家号是高基数);只在 gate 主 loop 线程读写,不需要原子。
	struct DeliveryCounters
	{
		uint64_t delivered{0};         // 写进连接:身份栅栏通过,或场景 / 全服广播(不过栅栏)
		uint64_t deliveredUnfenced{0}; // 写进连接:发送方没带目标玩家(兼容位),未校验
		uint64_t droppedFence{0};      // 目标玩家与会话当前绑定的玩家不符
		uint64_t sessionNotFound{0};   // 会话已不在本 gate(断线竞态的常态)
		uint64_t noConnection{0};      // 会话还在、连接已释放(稳态下不该出现)
		uint64_t invalidMessage{0};    // 缺 message_content,或指名广播的 player_list 与目标数不齐
		uint64_t droppedScene0{0};     // 场景广播的 scene_id 为 0
	};

	// 本线程、这条入口至今的累计计数。计数还没有登记进节点 /metrics,线上的出口仍是采样明细行;这里给单测读,
	// 只有计数能区分「校验通过」与「未校验放行」,这两者写到 socket 的字节相同。
	const DeliveryCounters &Counters(Path path);

	// 一条 MessageContent 编码后的客户端帧字节(长度前缀 + 类型名 + 载荷 + 校验和,codec.cpp fillEmptyBuffer),
	// 编码一次,对每个连接复用同一份字节。观战扇出(spectate/gate_spectate_hub.h)也用它。
	//
	// 只能 send(data, len):对共享 Buffer 调 conn->send(Buffer*) 时,muduo 在 loop 内写完会 retrieveAll()
	// 清空传入的缓冲,第一个连接发完共享缓冲就空了(battle-spectate-tiered-fanout.md §14 G6)。
	// send(data, len) 只读这段字节:写得进 socket 的直接写,剩下的各自拷进该连接的 outputBuffer。
	class EncodedFrame
	{
	public:
		explicit EncodedFrame(const MessageContent &content)
		{
			ProtobufCodec::fillEmptyBuffer(&bytes_, content);
		}

		void SendTo(const muduo::net::TcpConnectionPtr &conn) const
		{
			conn->send(bytes_.peek(), static_cast<int>(bytes_.readableBytes()));
		}

		const char *data() const { return bytes_.peek(); }
		std::size_t size() const { return bytes_.readableBytes(); }

	private:
		muduo::net::Buffer bytes_;
	};

	// 取消息里的 message_content,缺失返回空指针。message 字段有存在位,缺失时 message_content() 是默认实例,
	// 直接往下传就是给客户端推一个空帧;所以单播 / 场景 / 全服三个入口都收指针,空指针一律整条拒绝。
	template <typename Msg>
	const MessageContent *ContentOf(const Msg &msg)
	{
		return msg.has_message_content() ? &msg.message_content() : nullptr;
	}

	// 单播:缺 content 整条拒绝并计一次无效消息;会话不存在 / 连接已释放 / 身份栅栏不符时丢弃并计数。
	// targetPlayerId 为 0 = 发送方没带(兼容位)。
	void DeliverToSession(Path path, uint32_t sessionId, uint64_t targetPlayerId, const MessageContent *content);

	// 指名广播:(session_id, player_id) 按 broadcast_target_codec.h 的顺序契约解码,逐个目标过身份栅栏。
	// 整条先校验:缺 message_content,或 player_list 非空却与目标数不齐(PlayerListAligned 为假)时整条拒绝、
	// 计一次无效消息,不做部分投递。两个重载共用 gate_push_delivery.cpp 里同一份模板实现。
	void DeliverToTargets(Path path, const BroadcastToPlayersRequest &request);
	void DeliverToTargets(Path path, const contracts::kafka::BroadcastToPlayersEvent &event);

	// 场景广播:推给本 gate 上 sceneId 匹配、且已绑定玩家的会话。缺 content 整条拒绝并计一次无效消息;
	// sceneId 为 0 整条拒绝并计数 —— 没进场景的会话 sceneId 就是 0,按 0 匹配会把消息推给所有还没进场景的连接。
	void DeliverToScene(Path path, uint64_t sceneId, const MessageContent *content);

	// 全服广播:推给本 gate 上所有已绑定玩家的会话。缺 content 整条拒绝并计一次无效消息。
	void DeliverToAll(Path path, const MessageContent *content);
} // namespace gate_push
