package notify

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	kafka "github.com/segmentio/kafka-go"
	"google.golang.org/protobuf/proto"
	"mail/generated/pb/game"
	kafkapb "proto/contracts/kafka"
	mailpb "proto/mail"
	locator "proto/player_locator"
	"shared/kafkacmd"
)

func TestRealKafkaRedisMailReminderUsesGateFenceAndNoAssetAuthority(t *testing.T) {
	if os.Getenv("MAIL_PUSH_TEST_KAFKA") != "127.0.0.1:19092" {
		t.Skip("仅显式本机隔离Kafka19092/Redis13379")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Conn.CreateTopics固定旧v0；现代broker已删该版本。Client沿现有库协商API，不升级依赖。
	transport := &kafka.Transport{DialTimeout: time.Second}
	defer transport.CloseIdleConnections()
	admin := &kafka.Client{Addr: kafka.TCP("127.0.0.1:19092"), Timeout: 5 * time.Second, Transport: transport}
	topic := kafkacmd.GateCommandTopic()
	if kafkacmd.CommandContract().Generation != 20261016 || kafkacmd.CommandContract().Partitions != 4 {
		t.Fatal("必须使用隔离测试代号/四分区")
	}
	created, err := admin.CreateTopics(ctx, &kafka.CreateTopicsRequest{Topics: []kafka.TopicConfig{{Topic: topic, NumPartitions: 4, ReplicationFactor: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Errors[topic] != nil {
		t.Fatal(created.Errors[topic])
	}
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, e := admin.DeleteTopics(clean, &kafka.DeleteTopicsRequest{Topics: []string{topic}}); e != nil {
			t.Errorf("清测试topic: %v", e)
		}
	}()
	// CreateTopics只确认controller提交；等目标broker已能读该分区offset后才启动一次性发布。
	// 这是隔离基础设施就绪屏障，不是业务发送重试。
	ready, stopReady := context.WithTimeout(ctx, 5*time.Second)
	for {
		offsets, e := admin.ListOffsets(ready, &kafka.ListOffsetsRequest{Topics: map[string][]kafka.OffsetRequest{topic: {kafka.FirstOffsetOf(1)}}})
		if e == nil && len(offsets.Topics[topic]) == 1 && offsets.Topics[topic][0].Error == nil {
			break
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ready.Done():
			timer.Stop()
			stopReady()
			t.Fatalf("新建topic分区未就绪: %v", e)
		case <-timer.C:
		}
	}
	stopReady()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:13379", DB: 5})
	defer rdb.Close()
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	player := binary.LittleEndian.Uint64(random[:]) | 1
	key := fmt.Sprintf("player:session:%d", player)
	defer rdb.Del(context.Background(), key)
	session := &locator.PlayerSession{PlayerId: player, State: locator.PlayerSessionState_SESSION_STATE_ONLINE, GateId: "5", GateInstanceId: "mail-test-gate-instance", SessionId: 99}
	raw, _ := proto.Marshal(session)
	if err = rdb.Set(ctx, key, raw, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	writer := &kafka.Writer{Addr: kafka.TCP("127.0.0.1:19092"), Balancer: &kafkacmd.CommandPartitionBalancer{Fallback: &kafka.Hash{}}, RequiredAcks: kafka.RequireOne, MaxAttempts: 1, BatchTimeout: 5 * time.Millisecond}
	defer writer.Close()
	n := &Notifier{Redis: rdb, Publisher: writer}
	reader := kafka.NewReader(kafka.ReaderConfig{Brokers: []string{"127.0.0.1:19092"}, Topic: topic, Partition: kafkacmd.GateCommandPartition(5), MinBytes: 1, MaxBytes: 1 << 20})
	defer reader.Close()
	if err = reader.SetOffset(kafka.FirstOffset); err != nil {
		t.Fatal(err)
	}
	result, err := n.publish(ctx, player, 77)
	if err != nil || result != "sent" {
		t.Fatalf("实际发布: %s %v", result, err)
	}
	message, err := reader.ReadMessage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(message.Key) != strconv.FormatUint(player, 10) || message.Partition != 1 {
		t.Fatalf("寻址错误: %+v", message)
	}
	command := &kafkapb.GateCommand{}
	if proto.Unmarshal(message.Value, command) != nil || command.TargetInstanceId != session.GateInstanceId || command.TargetGateId != 5 || command.EventId != uint32(game.ContractsKafkaPushToPlayerEventEventId) || command.GetPlayerId() != player {
		t.Fatalf("实例围栏/事件错误: %+v", command)
	}
	event := &kafkapb.PushToPlayerEvent{}
	if proto.Unmarshal(command.Payload, event) != nil || event.SessionId != 99 || event.GetPlayerId() != player || event.MessageContent.MessageId != uint32(game.ClientPlayerMailNotifyMailChangedMessageId) {
		t.Fatalf("客户端寻址错误: %+v", event)
	}
	notice := &mailpb.MailChangedEvent{}
	if proto.Unmarshal(event.MessageContent.SerializedMessage, notice) != nil || notice.MailId != 77 {
		t.Fatalf("提醒正文不含稳定mail_id: %+v", notice)
	}
	if err = rdb.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	if result, err = n.publish(ctx, player, 78); err != nil || result != "offline" {
		t.Fatalf("离线不推仍可列表恢复: %s %v", result, err)
	}
	session.GateInstanceId = ""
	raw, _ = proto.Marshal(session)
	_ = rdb.Set(ctx, key, raw, time.Minute).Err()
	if _, err = n.publish(ctx, player, 79); err == nil {
		t.Fatal("空实例不得关闭gate防僵尸过滤")
	}
}
