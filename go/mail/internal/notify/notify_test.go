package notify

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	kafka "github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
	"mail/generated/pb/game"
	kafkapb "proto/contracts/kafka"
	locator "proto/player_locator"
)

// capturePublisher只收下发布的消息、不连Kafka；真实broker的寻址与分区由notify_integration_test.go在显式开关下覆盖。
type capturePublisher struct{ messages []kafka.Message }

func (p *capturePublisher) WriteMessages(_ context.Context, messages ...kafka.Message) error {
	p.messages = append(p.messages, messages...)
	return nil
}

// TestPublishCarriesTargetPlayerIdentity：提醒的PushToPlayerEvent必须带目标玩家，gate靠它核对会话当前玩家。
// 漏填时gate按兼容位放行，session被复用后提醒会推给另一个玩家，且全程零报错。
func TestPublishCarriesTargetPlayerIdentity(t *testing.T) {
	cache := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: cache.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	const (
		player    = uint64(90001)
		sessionID = uint32(4242)
		mailID    = uint64(77)
	)
	raw, err := proto.Marshal(&locator.PlayerSession{PlayerId: player, State: locator.PlayerSessionState_SESSION_STATE_ONLINE, GateId: "5", GateInstanceId: "mail-unit-gate-instance", SessionId: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if err = cache.Set(fmt.Sprintf("player:session:%d", player), string(raw)); err != nil {
		t.Fatal(err)
	}
	publisher := &capturePublisher{}
	result, err := (&Notifier{Redis: rdb, Publisher: publisher}).publish(context.Background(), player, mailID)
	if err != nil || result != "sent" {
		t.Fatalf("发布结果: %s %v", result, err)
	}
	if len(publisher.messages) != 1 {
		t.Fatalf("应恰好发布一条，实际%d条", len(publisher.messages))
	}
	command := &kafkapb.GateCommand{}
	if err = proto.Unmarshal(publisher.messages[0].Value, command); err != nil {
		t.Fatal(err)
	}
	if command.GetEventId() != uint32(game.ContractsKafkaPushToPlayerEventEventId) || command.GetPlayerId() != player || command.GetSessionId() != sessionID {
		t.Fatalf("信封错误: %+v", command)
	}
	event := &kafkapb.PushToPlayerEvent{}
	if err = proto.Unmarshal(command.GetPayload(), event); err != nil {
		t.Fatal(err)
	}
	if event.GetSessionId() != sessionID || event.GetPlayerId() != player {
		t.Fatalf("gate投递身份错误: session=%d player=%d，期望%d/%d", event.GetSessionId(), event.GetPlayerId(), sessionID, player)
	}
}
