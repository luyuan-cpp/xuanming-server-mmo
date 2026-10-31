// Package notify只发布发件提交后的可丢失提醒；不承载邮件或资产权威状态。
package notify

import (
	"context"
	"errors"
	"fmt"
	"mail/internal/metrics"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	kafka "github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"mail/generated/pb/game"
	base "proto/common/base"
	kafkapb "proto/contracts/kafka"
	mailpb "proto/mail"
	locator "proto/player_locator"
	"shared/kafkacmd"
)

var outcomes = promauto.NewCounterVec(prometheus.CounterOpts{Name: "mail_notifications_total", Help: "提交后可丢失定向邮件提醒"}, []string{"outcome"})

func init() {
	for _, outcome := range []string{"sent", "offline", "error"} {
		outcomes.WithLabelValues(outcome).Add(0)
	}
}

type Publisher interface {
	WriteMessages(context.Context, ...kafka.Message) error
}
type Notifier struct {
	Redis     redis.UniversalClient
	Publisher Publisher
}

// Personal在请求预算内至多尝试一次；不返回错误，避免推送失败改判已提交的发件。
func (n *Notifier) Personal(ctx context.Context, player, mail uint64) {
	ctx, stop := context.WithTimeout(ctx, 400*time.Millisecond)
	defer stop()
	result, err := n.publish(ctx, player, mail)
	if err != nil {
		outcomes.WithLabelValues("error").Inc()
		metrics.ObservePush(metrics.PushError)
		logx.WithContext(ctx).Errorf("[mail] 提醒未发布，权威邮件已保留 failure=%T", err)
		return
	}
	outcomes.WithLabelValues(result).Inc()
	if result == "sent" {
		metrics.ObservePush(metrics.PushOK)
	} else {
		metrics.ObservePush(metrics.PushOffline)
	}
}
func (n *Notifier) publish(ctx context.Context, player, mail uint64) (string, error) {
	if n == nil || n.Redis == nil || n.Publisher == nil || player == 0 || mail == 0 {
		return "", errors.New("邮件提醒未装配或输入无效")
	}
	raw, err := n.Redis.Get(ctx, fmt.Sprintf("player:session:%d", player)).Bytes()
	if errors.Is(err, redis.Nil) {
		return "offline", nil
	}
	if err != nil {
		return "", err
	}
	session := &locator.PlayerSession{}
	if err = proto.Unmarshal(raw, session); err != nil {
		return "", err
	}
	if session.State != locator.PlayerSessionState_SESSION_STATE_ONLINE {
		return "offline", nil
	}
	if session.PlayerId != player {
		return "", errors.New("会话与玩家键不符")
	}
	if session.GateInstanceId == "" || session.SessionId == 0 {
		return "", errors.New("会话缺目标实例或session")
	}
	gate, err := kafkacmd.ParseNodeID(session.GateId)
	if err != nil {
		return "", err
	}
	body, err := proto.Marshal(&mailpb.MailChangedEvent{MailId: mail})
	if err != nil {
		return "", err
	}
	// player_id是gate的目标玩家身份栅栏：session会被复用，gate投递前核对会话当前玩家，不符即丢弃。
	payload, err := proto.Marshal(&kafkapb.PushToPlayerEvent{SessionId: session.SessionId, PlayerId: player, MessageContent: &base.MessageContent{MessageId: uint32(game.ClientPlayerMailNotifyMailChangedMessageId), SerializedMessage: body}})
	if err != nil {
		return "", err
	}
	value, err := proto.Marshal(&kafkapb.GateCommand{EventId: uint32(game.ContractsKafkaPushToPlayerEventEventId), PlayerId: player, SessionId: session.SessionId, TargetGateId: uint32(gate), TargetInstanceId: session.GateInstanceId, Payload: payload})
	if err != nil {
		return "", err
	}
	// 与共享gate推送同一控制面契约：目标实例围栏、按节点分区、玩家ID作为key。
	err = n.Publisher.WriteMessages(ctx, kafka.Message{Topic: kafkacmd.GateCommandTopic(), Partition: kafkacmd.GateCommandPartition(gate), Key: []byte(strconv.FormatUint(player, 10)), Value: value})
	return "sent", err
}
