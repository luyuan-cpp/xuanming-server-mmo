package svc

import (
	"context"
	"errors"
	"testing"

	"chat/generated/pb/game"
	"chat/internal/config"
	chatpb "proto/chat"
	kafkapb "proto/contracts/kafka"
	"shared/kafkacmd"

	"github.com/prometheus/client_golang/prometheus/testutil"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// 世界频道推送的 Kafka 形状(docs/design/chat-world-channel-fanout.md §4)。不连 Kafka:writer 是替身。

type fakeKafkaWriter struct {
	messages []kafkago.Message
	err      error
}

func (w *fakeKafkaWriter) WriteMessages(_ context.Context, msgs ...kafkago.Message) error {
	w.messages = append(w.messages, msgs...)
	return w.err
}

type failingPusher struct{}

func (failingPusher) PushWorld(context.Context, *chatpb.ChatMessage) error {
	return errors.New("kafka unavailable")
}

func worldMessage() *chatpb.ChatMessage {
	return &chatpb.ChatMessage{SenderPlayerId: 123, Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, Content: "大家好", SendTimeMs: 99}
}

func TestWorldPublisherWritesBroadcastToAllEventKeyedBySender(t *testing.T) {
	w := &fakeKafkaWriter{}
	p := &worldPublisher{writer: w, topic: "chat-world-bcast_g7", messageID: 4242}
	message := worldMessage()

	require.NoError(t, p.PushWorld(context.Background(), message))
	require.Len(t, w.messages, 1, "一条世界消息在 Kafka 只写一次,扇出由每台 gate 读全分区完成")
	got := w.messages[0]
	require.Equal(t, "chat-world-bcast_g7", got.Topic)
	require.Equal(t, "123", string(got.Key), "key = 发送者 player_id:同一个人的消息落同一分区、保持顺序")

	event := &kafkapb.BroadcastToAllEvent{}
	require.NoError(t, proto.Unmarshal(got.Value, event), "载荷是 BroadcastToAllEvent 本身,不是 GateCommand 信封")
	require.EqualValues(t, 4242, event.GetMessageContent().GetMessageId())
	decoded := &chatpb.ChatMessage{}
	require.NoError(t, proto.Unmarshal(event.GetMessageContent().GetSerializedMessage(), decoded))
	require.True(t, proto.Equal(message, decoded), "推的就是历史里的那份显示原件")
}

func TestNewWorldPublisherDisabledWithoutBrokers(t *testing.T) {
	p, w := newWorldPublisher(config.Config{})
	require.Nil(t, p)
	require.Nil(t, w)
}

func TestNewWorldPublisherUsesWorldTopicAndPushMessageID(t *testing.T) {
	p, w := newWorldPublisher(config.Config{Kafka: config.KafkaConf{Brokers: []string{"127.0.0.1:1"}}})
	require.NotNil(t, p)
	require.NotNil(t, w)
	t.Cleanup(func() { _ = w.Close() })
	require.Equal(t, kafkacmd.ChatWorldBroadcastTopic(), p.topic, "topic 名与 gate 消费的同出一处契约")
	require.EqualValues(t, game.ClientPlayerChatNotifyWorldChatMessageId, p.messageID)
	require.IsType(t, &kafkago.Hash{}, w.Balancer, "按 key 哈希选分区,不按显式分区")
	require.Equal(t, kafkago.RequireOne, w.RequiredAcks)
	require.False(t, w.Async, "同步写:返回 nil 才代表 broker 收到")
}

func TestPublishWorldSkipsWhenDisabledReplayedOrNotWorld(t *testing.T) {
	(&ServiceContext{}).PublishWorld(context.Background(), worldMessage()) // 没配 Kafka:什么都不做,不 panic

	w := &fakeKafkaWriter{}
	sc := &ServiceContext{WorldPush: &worldPublisher{writer: w, topic: "t", messageID: 1}}
	sc.PublishWorld(context.Background(), nil)
	private := worldMessage()
	private.Channel = chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE
	private.TargetPlayerId = 456
	sc.PublishWorld(context.Background(), private)
	require.Empty(t, w.messages, "重放(nil)与私聊都不推")

	sc.PublishWorld(context.Background(), worldMessage())
	require.Len(t, w.messages, 1)
}

func TestPublishWorldSwallowsErrorsAndCountsThem(t *testing.T) {
	before := testutil.ToFloat64(worldPushTotal.WithLabelValues(pushOutcomeError))
	sc := &ServiceContext{WorldPush: failingPusher{}}
	sc.PublishWorld(context.Background(), worldMessage())
	require.Equal(t, before+1, testutil.ToFloat64(worldPushTotal.WithLabelValues(pushOutcomeError)),
		"推送失败只计数、记日志,不往外抛:发送已经提交")
}
