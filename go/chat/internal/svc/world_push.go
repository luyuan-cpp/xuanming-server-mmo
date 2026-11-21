package svc

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"chat/generated/pb/game"
	"chat/internal/config"
	chatpb "proto/chat"
	basepb "proto/common/base"
	kafkapb "proto/contracts/kafka"
	"shared/kafkacmd"
	"shared/tracing/kafkagotrace"

	"github.com/prometheus/client_golang/prometheus"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 世界频道推送(docs/design/chat-world-channel-fanout.md §4)。
//
// 一条世界频道消息首次提交成功后,往 chat-world-bcast_g<N> 写一条 BroadcastToAllEvent;每台 gate assign 全部分区,
// 推给本机全部已绑定的会话。推送是至多一次的通知:写 Kafka 失败只记日志和指标,发送应答照常成功,
// 漏掉的那一条客户端在打开面板或重连拉历史时补上。推送在同一次同步 RPC 里、提交之后发出,
// 受 RootAdmission 管(维护排空等它),没有后台发布者。

// worldPushTimeout 是一次推送写 Kafka 的上限,挂在请求预算下:Kafka 卡住时砍掉的是推送,不是已经生效的发送。
// 它远小于 chat 的服务端超时(Timeout 上限 4000ms),发送应答不会因为推送拖过路由服的转发预算。
const worldPushTimeout = 500 * time.Millisecond

const (
	pushOutcomeOK    = "ok"
	pushOutcomeError = "error"
)

var worldPushTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Subsystem: "chat",
	Name:      "world_push_total",
	Help:      "World channel pushes written to chat-world-bcast by outcome (ok|error); not counted while Kafka.Brokers is empty.",
}, []string{"outcome"})

// WorldPusher 是世界频道推送的接缝:生产是下面的 Kafka 实现,业务层用例注入替身。
type WorldPusher interface {
	PushWorld(ctx context.Context, message *chatpb.ChatMessage) error
	PushWorldRetract(ctx context.Context, notify *chatpb.ChatRetractedNotify) error
}

// worldMessageWriter 是推送写 Kafka 的接缝:生产是 *kafkago.Writer,用例换成替身。
type worldMessageWriter interface {
	WriteMessages(ctx context.Context, msgs ...kafkago.Message) error
}

// worldPublisher 把世界频道显示原件与撤回通知包成全服广播写进 Kafka。
type worldPublisher struct {
	writer           worldMessageWriter
	topic            string
	messageID        uint32
	retractMessageID uint32
}

var _ WorldPusher = (*worldPublisher)(nil)

// newWorldPublisher:Brokers 为空返回 (nil, nil),表示本进程不推(本机不起 Kafka 时的正常形态,客户端靠拉历史)。
// 第二个返回值是要在停机时关闭的 writer。
func newWorldPublisher(c config.Config) (*worldPublisher, *kafkago.Writer) {
	if len(c.Kafka.Brokers) == 0 {
		logx.Info("[chat] Kafka.Brokers empty: world channel push disabled; clients see new world messages only by pulling history")
		return nil, nil
	}
	writer := &kafkago.Writer{
		Addr: kafkago.TCP(c.Kafka.Brokers...),
		// 不是命令 topic,不按显式分区:key = 发送者 player_id 哈希选分区,同一个人的消息有序(AGENTS.md §7.3)。
		// gate 读全部分区,落在哪个分区都收得到。
		Balancer: &kafkago.Hash{},
		// 与 friend 同理:RequireOne 加同步写,WriteMessages 返回 nil 才代表 broker 收到。
		RequiredAcks: kafkago.RequireOne,
		Async:        false,
		WriteTimeout: worldPushTimeout,
	}
	return &worldPublisher{
		writer:           writer,
		topic:            kafkacmd.ChatWorldBroadcastTopic(),
		messageID:        uint32(game.ClientPlayerChatNotifyWorldChatMessageId),
		retractMessageID: uint32(game.ClientPlayerChatNotifyWorldChatRetractedMessageId),
	}, writer
}

func (p *worldPublisher) writeBroadcast(ctx context.Context, messageID uint32, senderPlayerID uint64, payloadMsg proto.Message) error {
	body, err := proto.Marshal(payloadMsg)
	if err != nil {
		return fmt.Errorf("marshal world broadcast payload: %w", err)
	}
	event, err := proto.Marshal(&kafkapb.BroadcastToAllEvent{MessageContent: &basepb.MessageContent{
		MessageId:         messageID,
		SerializedMessage: body,
	}})
	if err != nil {
		return fmt.Errorf("marshal BroadcastToAllEvent: %w", err)
	}
	return p.writer.WriteMessages(ctx, kafkago.Message{
		Topic: p.topic,
		Key:   []byte(strconv.FormatUint(senderPlayerID, 10)),
		Value: event,
		// 发言请求的 trace 写进消息头,各 gate 广播时接上同一条 trace(docs/design/distributed-tracing.md §6.2)。
		Headers: kafkagotrace.Headers(ctx),
	})
}

// PushWorld 组 BroadcastToAllEvent 并同步写一条。
func (p *worldPublisher) PushWorld(ctx context.Context, message *chatpb.ChatMessage) error {
	return p.writeBroadcast(ctx, p.messageID, message.SenderPlayerId, message)
}

// PushWorldRetract 通过同一个 chat-world-bcast topic 向全服在线客户端广播撤回通知。
func (p *worldPublisher) PushWorldRetract(ctx context.Context, notify *chatpb.ChatRetractedNotify) error {
	return p.writeBroadcast(ctx, p.retractMessageID, notify.SenderPlayerId, notify)
}

// PublishWorld 在世界频道消息首次提交成功之后调用。没配 Kafka(WorldPush 为 nil)时什么都不做;
// 失败只记日志与指标,不影响发送应答。message 为 nil(重放、限流)或不是世界频道时不推。
func (sc *ServiceContext) PublishWorld(ctx context.Context, message *chatpb.ChatMessage) {
	if sc.WorldPush == nil || message == nil || message.Channel != chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD {
		return
	}
	pushCtx, cancel := context.WithTimeout(ctx, worldPushTimeout)
	defer cancel()
	if err := sc.WorldPush.PushWorld(pushCtx, message); err != nil {
		worldPushTotal.WithLabelValues(pushOutcomeError).Inc()
		// player_id 只进日志,不进指标 label(AGENTS.md §9)。
		logx.WithContext(ctx).Errorf("[chat] World channel push failed; the message is committed and stays in history, sender=%d: %v",
			message.SenderPlayerId, err)
		return
	}
	worldPushTotal.WithLabelValues(pushOutcomeOK).Inc()
}

// PublishWorldRetract 广播世界频道消息撤回通知给全服在线玩家。
func (sc *ServiceContext) PublishWorldRetract(ctx context.Context, notify *chatpb.ChatRetractedNotify) {
	if sc.WorldPush == nil || notify == nil || notify.SenderPlayerId == 0 || notify.SendTimeMs <= 0 {
		return
	}
	pushCtx, cancel := context.WithTimeout(ctx, worldPushTimeout)
	defer cancel()
	if err := sc.WorldPush.PushWorldRetract(pushCtx, notify); err != nil {
		worldPushTotal.WithLabelValues(pushOutcomeError).Inc()
		logx.WithContext(ctx).Errorf("[chat] World channel retract push failed, sender=%d send_time_ms=%d: %v",
			notify.SenderPlayerId, notify.SendTimeMs, err)
		return
	}
	worldPushTotal.WithLabelValues(pushOutcomeOK).Inc()
}
