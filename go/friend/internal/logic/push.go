// push.go —— friend 的 S2C 推送(规格 §3.5),模板取 go/match/internal/logic/push.go。
//
// # 投递语义:at-most-once,推送只是"去拉"的触发信号
//
// 这条链路是 Kafka gate-cmd_g<N> → gate → TCP,**没有**重试、没有回执、没有离线补推。
// 玩家恰好在推送那一刻掉线、或者 Kafka 抖一下,这条事件就永久丢了。所以:
//   - 客户端收到它之后**必须**去拉 GetPendingRequests / GetFriendList,事件体里刻意只带
//     reason + by_player_id + ts_ms,不带列表(带了就成了数据同步,丢一条就对不上账);
//   - 服务端**不得**把推送成功当成业务成功的一部分 —— 申请已经落库,推不到只影响红点的
//     及时性,不影响正确性。
//
// # 三条纪律(写新的触发点前先读)
//
//  1. **只在 MySQL 提交之后、事务之外推**。在事务里推 = 事务还没提交,gate 已经把
//     "你有新申请"送到对方屏幕上,对方立刻拉列表却什么都没有;事务回滚就更糟,推了一个
//     从未发生的事件。
//  2. **失败只打日志 + 计指标,绝不影响 RPC 结果**。本文件所有函数都不返回 error,
//     就是为了从签名上堵住"顺手 return err"。
//  3. **不推给操作者本人**。他刚点的按钮,响应里已经有结果;再推一条只会让客户端
//     收到自己动作的回声,红点逻辑要额外去重。
//
// 触发点只有两个:AddFriend 成功 → 推 REQUEST_RECEIVED 给 target;
// AcceptFriend 成功 → 推 REQUEST_ACCEPTED 给原申请人。
// Reject(A 仓为避免社交尴尬刻意不推)/ Remove / Block / Unblock 都不推。
package logic

import (
	"context"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"friend/generated/pb/game"
	"friend/internal/metrics"
	pb "proto/friend"
	plpb "proto/player_locator"
	"shared/kafkautil"
)

// pushTimeout 覆盖"读会话 + 组命令 + 写 Kafka"这一整段。
//
// 1.5s 是**推断值**,推导:服务端 Timeout 4000ms → 整请求预算 RequestBudget 3500ms
// (减 InBandReplyReserve 500ms),减去 MySQL 权威事务在最坏情况下的占用(容量守卫要等
// 同一对玩家的并发写事务提交),剩给推送的余量按 1.5s 估。
// 它同时受 ctx(整请求预算)封顶 —— 见 pushFriendEvent 里为什么用请求 ctx 派生而不是
// context.Background()。真实占用要等压测数据,别把这个数字当实测结论。
const pushTimeout = 1500 * time.Millisecond

// playerSessionKey 是 player_locator 维护的会话键(值 = proto PlayerSession)。
//
// ⚠ 跨运行时契约 key(契约 §4):键名是 login / player_locator / gate / friend 几方的共同
// 约定,**只能经 SharedRedis 读**,而且这里不许加 friend 自己的 hash tag 或版本前缀 ——
// 改了键名就读不到别人写的值,而且全程零报错(GET 未命中在业务上长得像"玩家离线")。
// data/session_reader.go 里有同一个键的批量读法;两处刻意各写一份而不抽公共常量,
// 是因为抽到 shared/ 需要新增共享抽象(F1 §0 第 7 条禁止),代价是**改一处必须改两处**。
func playerSessionKey(playerID uint64) string {
	return fmt.Sprintf("player:session:%d", playerID)
}

// pushReasonLabel 把 proto 枚举映射成 metrics 的 reason label。
//
// 未知枚举返回 ("", false):label 必须是有限枚举(AGENTS.md §9),
// 拿一个空串或数字进去会凭空造出一条谁也不认识的时间序列。
func pushReasonLabel(reason pb.FriendEventReason) (string, bool) {
	switch reason {
	case pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED:
		return metrics.ReasonRequestReceived, true
	case pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_ACCEPTED:
		return metrics.ReasonRequestAccepted, true
	default:
		return "", false
	}
}

// pushFriendEvent 把一条 FriendEventS2C 推给 toPlayerID 的客户端。
//
// byPlayerID 是触发事件的对方玩家(申请人 / 同意者),不是收件人。
// 调用方**不需要**检查返回值:本函数无返回值,一切失败都已就地记录。
func (l *FriendLogic) pushFriendEvent(ctx context.Context, toPlayerID uint64, reason pb.FriendEventReason, byPlayerID uint64) {
	label, known := pushReasonLabel(reason)
	if !known {
		logx.WithContext(ctx).Errorf("[friend] 拒绝推送未知事件类型 reason=%d to=%d:新增枚举要同时加 metrics reason 常量与预建序列",
			int32(reason), toPlayerID)
		return
	}
	if toPlayerID == 0 {
		logx.WithContext(ctx).Errorf("[friend] 拒绝推送 reason=%s:收件人为 0", label)
		metrics.ObservePush(label, metrics.OutcomeError)
		return
	}

	svcCtx := l.deps.SvcCtx
	// KafkaWriter 为 nil = Brokers 未配(本地单机联调常态)。直接跳过并计 error:
	// 不 panic(推送是弱依赖,不该让好友服起不来),但也不记 ok —— "推送整个关着"必须在
	// 指标上看得见,否则线上漏配 Brokers 会表现为"功能正常但红点从不亮",没有任何告警。
	if svcCtx.KafkaWriter == nil || svcCtx.GateCommandBuilder == nil {
		logx.WithContext(ctx).Errorf("[friend] WARN 推送通道未配置(KafkaWriter/GateCommandBuilder 为 nil),丢弃 reason=%s to=%d",
			label, toPlayerID)
		metrics.ObservePush(label, metrics.OutcomeError)
		return
	}

	// ① 读会话。
	sess, err := l.loadPlayerSession(ctx, toPlayerID)
	if err != nil {
		logx.WithContext(ctx).Errorf("[friend] 读会话失败,丢弃推送 reason=%s to=%d: %v", label, toPlayerID, err)
		metrics.ObservePush(label, metrics.OutcomeError)
		return
	}
	// ② 只推 ONLINE。DISCONNECTING(断线等待重连,30s 租约)刻意不推:那一刻 gate 上
	// 已经没有可写的 TCP 连接,推过去只是白占一次 Kafka 写。玩家重连后自己会拉列表。
	if sess == nil || sess.GetState() != plpb.PlayerSessionState_SESSION_STATE_ONLINE {
		metrics.ObservePush(label, metrics.OutcomeOffline)
		return
	}

	body, err := proto.Marshal(&pb.FriendEventS2C{
		Reason:     reason,
		ByPlayerId: byPlayerID,
		// ts_ms 取服务端时钟:客户端用它给红点排序,用客户端本地时间会因时钟漂移乱序。
		TsMs: l.deps.now().UnixMilli(),
	})
	if err != nil {
		logx.WithContext(ctx).Errorf("[friend] 序列化 FriendEventS2C 失败 reason=%s to=%d: %v", label, toPlayerID, err)
		metrics.ObservePush(label, metrics.OutcomeError)
		return
	}

	// ③ 经共享收口发出。kafkautil.PushToPlayer 会 fail-closed 拒掉空 gate_instance_id
	// (防僵尸不变量,AGENTS.md §7 第 2 条)与非数字 gate_id(算不出分区就等于静默丢)。
	//
	// ctx 从**请求 ctx**派生而不是 context.Background()(这里与 match 的模板不同,是刻意的):
	// 推送发生在返回 in-band 响应之前,用 Background 时 Kafka 卡住会把整个请求拖过 go-zero
	// 的服务端超时拦截器,客户端拿到 gRPC DeadlineExceeded —— 业务已经成功却回了个失败信封。
	// 挂在请求预算下,最坏情况是推送被预算砍掉(计 error),响应照常发出。
	pushCtx, cancel := context.WithTimeout(ctx, pushTimeout)
	defer cancel()
	err = kafkautil.PushToPlayer(pushCtx, svcCtx.KafkaWriter, svcCtx.GateCommandBuilder, kafkautil.PlayerGateInfo{
		PlayerID:       toPlayerID,
		SessionID:      sess.GetSessionId(),
		GateID:         sess.GetGateId(),
		GateInstanceID: sess.GetGateInstanceId(),
	},
		// ⚠ 待 proto-gen 后核对:消息号常量名按本仓生成器的命名规律推导 ——
		// `<服务名><方法名>MessageId`(现有例:FriendServiceAddFriendMessageId、
		// MatchServiceNotifyChallengeInviteMessageId)。F1 把服务名从 FriendService 改成
		// ClientPlayerFriend,所以重新发号后应当是下面这个名字,且 friend 原来的
		// 2/7/11/12/53/76/119/120 号会被释放。proto-gen 跑完请核对
		// go/friend/generated/pb/game/message_id.go 里的实际名字再改这一行。
		uint32(game.ClientPlayerFriendNotifyFriendEventMessageId), body)
	if err != nil {
		logx.WithContext(ctx).Errorf("[friend] 推送失败 reason=%s to=%d gate=%s: %v",
			label, toPlayerID, sess.GetGateId(), err)
		metrics.ObservePush(label, metrics.OutcomeError)
		return
	}
	metrics.ObservePush(label, metrics.OutcomeOK)
}

// loadPlayerSession 读一名玩家的会话。返回 (nil, nil) 表示键不存在(玩家未登录)。
//
// 解码失败**不当"离线"处理**,而是当错误往上报:PlayerSession 是别的运行时写的 proto,
// 解不开只可能是键被别的东西占用或 proto 不兼容,把它记成 offline 会让一次协议事故
// 表现为"全服都离线"且零异常指标(metrics 里 offline 与 error 分开就是为了这个)。
func (l *FriendLogic) loadPlayerSession(ctx context.Context, playerID uint64) (*plpb.PlayerSession, error) {
	// GetCtx 吞 redis.Nil 并返回空串(go-zero core/stores/redis 的语义),
	// 所以"键不存在"必须靠空串判,不能靠 err != nil。
	raw, err := l.deps.SvcCtx.SharedRedis.GetCtx(ctx, playerSessionKey(playerID))
	if err != nil {
		return nil, fmt.Errorf("get player session %d: %w", playerID, err)
	}
	if raw == "" {
		return nil, nil
	}
	sess := &plpb.PlayerSession{}
	if err := proto.Unmarshal([]byte(raw), sess); err != nil {
		return nil, fmt.Errorf("unmarshal PlayerSession %d: %w", playerID, err)
	}
	return sess, nil
}
