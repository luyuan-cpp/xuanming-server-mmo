package kafka

import (
	"context"
	"errors"
	"fmt"
	"login/internal/config"
	"os"
	"strconv"
	"testing"
	"time"

	db_proto "proto/db"
	"shared/kafkautil"

	"github.com/IBM/sarama"
	"github.com/IBM/sarama/mocks"
	"google.golang.org/protobuf/proto"
)

// dbTaskGoldenKeys 与 go/shared/kafkautil/testdata/dbtask_partition_vectors.json 的 key 集合相同。
var dbTaskGoldenKeys = []string{
	"0", "1", "42", "1001", "10001", "123456789",
	"7311864093319258112", "7311864093319258113", "7311864094007124480",
	"9223372036854775807", "18446744073709551615",
}

// testKafkaConfig 是能通过 sarama Validate 的最小 login Kafka 配置(幂等生产者)。
func testKafkaConfig() config.KafkaConfig {
	return config.KafkaConfig{
		PartitionCnt:     10,
		InitialPartition: 10,
		DialTimeout:      time.Second,
		ReadTimeout:      time.Second,
		WriteTimeout:     time.Second,
		RetryMax:         3,
		RetryBackoff:     100 * time.Millisecond,
		ChannelBuffer:    256,
		Idempotent:       true,
	}
}

func TestDBTaskProducerConfigValidates(t *testing.T) {
	cfg := newDBTaskProducerConfig(testKafkaConfig())
	if err := cfg.Validate(); err != nil {
		t.Fatalf("db_task producer config must validate: %v", err)
	}
	if !cfg.Producer.Idempotent {
		t.Error("Producer.Idempotent must follow KafkaConfig.Idempotent=true")
	}
	if cfg.Net.MaxOpenRequests != 1 {
		t.Errorf("Net.MaxOpenRequests = %d, want 1 (required by idempotency)", cfg.Net.MaxOpenRequests)
	}
	if cfg.Producer.RequiredAcks != sarama.WaitForAll {
		t.Errorf("Producer.RequiredAcks = %v, want WaitForAll", cfg.Producer.RequiredAcks)
	}
	if cfg.Producer.Partitioner == nil {
		t.Fatal("Producer.Partitioner must be set")
	}
}

// TestDBTaskProducerConfigUsesCRCPartitioner 确认主生产者的分区器就是契约分区器,而不是 sarama 默认的 FNV。
func TestDBTaskProducerConfigUsesCRCPartitioner(t *testing.T) {
	cfg := newDBTaskProducerConfig(testKafkaConfig())
	ours := cfg.Producer.Partitioner("db_task_zone_1")
	fnv := sarama.NewHashPartitioner("db_task_zone_1")
	differsFromFNV := 0
	for _, key := range dbTaskGoldenKeys {
		for _, n := range []int32{7, 10, 16, 20} {
			msg := &sarama.ProducerMessage{Topic: "db_task_zone_1", Key: sarama.StringEncoder(key)}
			got, err := ours.Partition(msg, n)
			if err != nil {
				t.Fatalf("partitioner(%q, %d): %v", key, n, err)
			}
			want, err := kafkautil.DBTaskPartition([]byte(key), n)
			if err != nil {
				t.Fatalf("DBTaskPartition(%q, %d): %v", key, n, err)
			}
			if got != want {
				t.Errorf("key %q n=%d: producer partition %d, want contract partition %d", key, n, got, want)
			}
			fnvPart, err := fnv.Partition(msg, n)
			if err != nil {
				t.Fatalf("fnv partitioner(%q, %d): %v", key, n, err)
			}
			if fnvPart != got {
				differsFromFNV++
			}
		}
	}
	if differsFromFNV == 0 {
		t.Fatal("every golden key lands where the default FNV partitioner would put it: the partitioner was not actually replaced")
	}
}

// TestSendTaskRoutesByDBTaskContract:mock 生产者按同一份配置(契约分区器)分区,断言 SendTask 发出的消息
// 分区等于 DBTaskPartition、key 等于 DBTaskKey。
func TestSendTaskRoutesByDBTaskContract(t *testing.T) {
	sp := mocks.NewSyncProducer(t, newDBTaskProducerConfig(testKafkaConfig()))
	sp.TopicConfig.SetDefaultPartitions(10)
	want, err := kafkautil.DBTaskPartition([]byte("1001"), 10)
	if err != nil {
		t.Fatalf("DBTaskPartition: %v", err)
	}
	sp.ExpectSendMessageWithMessageCheckerFunctionAndSucceed(contractMessageChecker("1001", want))
	t.Cleanup(func() { _ = sp.Close() })

	p := newLockTestProducer(t, sp, 10)
	if err := p.SendTask(context.Background(), &db_proto.DBTask{TaskId: "route-1", Key: 1001, Op: db_proto.DBOp_DB_OP_READ}, "1001"); err != nil {
		t.Fatalf("SendTask: %v", err)
	}
}

func TestSendTasksRoutesByDBTaskContract(t *testing.T) {
	sp := mocks.NewSyncProducer(t, newDBTaskProducerConfig(testKafkaConfig()))
	sp.TopicConfig.SetDefaultPartitions(10)
	want, err := kafkautil.DBTaskPartition([]byte("1001"), 10)
	if err != nil {
		t.Fatalf("DBTaskPartition: %v", err)
	}
	sp.ExpectSendMessageWithMessageCheckerFunctionAndSucceed(contractMessageChecker("1001", want))
	sp.ExpectSendMessageWithMessageCheckerFunctionAndSucceed(contractMessageChecker("1001", want))
	t.Cleanup(func() { _ = sp.Close() })

	p := newLockTestProducer(t, sp, 10)
	tasks := []*db_proto.DBTask{
		{TaskId: "route-a", Key: 1001, Op: db_proto.DBOp_DB_OP_READ, MsgType: "a"},
		{TaskId: "route-b", Key: 1001, Op: db_proto.DBOp_DB_OP_READ, MsgType: "b"},
	}
	if err := p.SendTasks(context.Background(), tasks, "1001"); err != nil {
		t.Fatalf("SendTasks: %v", err)
	}
}

// contractMessageChecker 断言消息的 key 与 sarama 回填的分区都符合契约。
func contractMessageChecker(wantKey string, wantPartition int32) mocks.MessageChecker {
	return func(msg *sarama.ProducerMessage) error {
		if msg.Key == nil {
			return errors.New("db_task message has no key")
		}
		key, err := msg.Key.Encode()
		if err != nil {
			return fmt.Errorf("encode key: %w", err)
		}
		if string(key) != wantKey {
			return fmt.Errorf("key %q, want %q", key, wantKey)
		}
		if msg.Partition != wantPartition {
			return fmt.Errorf("partition %d, want contract partition %d", msg.Partition, wantPartition)
		}
		return nil
	}
}

// TestSendTaskRejectsMismatchedKey:key 与 task.Key 不符时必须在发送前拒绝。
// mock 不设任何 expectation:一旦真的发出去,mock 会自己报 "No more expectation"。
func TestSendTaskRejectsMismatchedKey(t *testing.T) {
	sp := mocks.NewSyncProducer(t, newDBTaskProducerConfig(testKafkaConfig()))
	p := newLockTestProducer(t, sp, 10)

	if err := p.SendTask(context.Background(), &db_proto.DBTask{TaskId: "mismatch-1", Key: 42, Op: db_proto.DBOp_DB_OP_READ}, "43"); err == nil {
		t.Error("SendTask must reject a Kafka key that is not DBTaskKey(task.Key)")
	}
	if err := p.SendTasks(context.Background(), []*db_proto.DBTask{{TaskId: "mismatch-2", Key: 42, Op: db_proto.DBOp_DB_OP_READ}}, "43"); err == nil {
		t.Error("SendTasks must reject a Kafka key that is not DBTaskKey(task.Key)")
	}
	if err := sp.Close(); err != nil {
		t.Errorf("mock close: %v", err)
	}
}

func TestSendTaskRejectsUnspecifiedAndUnknownOp(t *testing.T) {
	for _, op := range []db_proto.DBOp{db_proto.DBOp_DB_OP_UNSPECIFIED, db_proto.DBOp(99)} {
		t.Run(op.String(), func(t *testing.T) {
			// 不登记发送期望，任何误发送都会由 mock 判为失败。
			sp := mocks.NewSyncProducer(t, newDBTaskProducerConfig(testKafkaConfig()))
			t.Cleanup(func() { _ = sp.Close() })
			p := newLockTestProducer(t, sp, 10)
			task := &db_proto.DBTask{Key: 42, Op: op, TaskId: "invalid-op"}
			if err := p.SendTask(context.Background(), task, "42"); err == nil {
				t.Fatal("未指定或未知操作必须在发送前拒绝")
			}
		})
	}
}

func TestSendTasksInvalidOpCannotPartiallySend(t *testing.T) {
	for _, op := range []db_proto.DBOp{db_proto.DBOp_DB_OP_UNSPECIFIED, db_proto.DBOp(99)} {
		t.Run(op.String(), func(t *testing.T) {
			sp := mocks.NewSyncProducer(t, newDBTaskProducerConfig(testKafkaConfig()))
			t.Cleanup(func() { _ = sp.Close() })
			p := newLockTestProducer(t, sp, 10)
			tasks := []*db_proto.DBTask{
				{Key: 42, Op: db_proto.DBOp_DB_OP_READ, TaskId: "valid-first"},
				{Key: 42, Op: op, TaskId: "invalid-second"},
			}
			if err := p.SendTasks(context.Background(), tasks, "42"); err == nil {
				t.Fatal("批量包含非法操作时必须整批拒绝，不能先发送合法前缀")
			}
		})
	}
}

// TestDBTaskReadAndWriteShareKafkaPartitionIntegration 在真实 broker 上证明:用 kafkautil.NewDBTaskPartitioner
// 发的写(模拟 C++ scene)与 login 主生产者配置发的读,落在同一分区、且读的 offset 排在写之后。
// 没有 KAFKA_INTEGRATION_BROKER 时 Skip(L1 里是 hermetic 的)。
//
// topic 名刻意不用 db_task_zone_ 前缀(参照 shared/kafkautil 的集成用例用 mmorpg_*_it_<nanos>):
// 指向 staging broker 时不会被上线抽样或告警误纳入。分区器装在配置上,与 topic 名无关。
func TestDBTaskReadAndWriteShareKafkaPartitionIntegration(t *testing.T) {
	broker := os.Getenv("KAFKA_INTEGRATION_BROKER")
	if broker == "" {
		t.Skip("set KAFKA_INTEGRATION_BROKER to run the Kafka integration test")
	}
	const partitions = int32(10)
	topic := fmt.Sprintf("mmorpg_dbtask_partition_it_%d", time.Now().UnixNano())

	adminCfg := sarama.NewConfig()
	adminCfg.Version = sarama.V3_6_0_0
	admin, err := sarama.NewClusterAdmin([]string{broker}, adminCfg)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// 先注册的 Cleanup 最后执行:生产者先关,再删 topic。
	t.Cleanup(func() {
		if err := admin.DeleteTopic(topic); err != nil && !errors.Is(err, sarama.ErrUnknownTopicOrPartition) {
			t.Logf("cleanup topic %s: %v", topic, err)
		}
		_ = admin.Close()
	})
	retention := "60000"
	if err := admin.CreateTopic(topic, &sarama.TopicDetail{
		NumPartitions:     partitions,
		ReplicationFactor: kafkautil.DefaultReplicationFactor(),
		ConfigEntries:     map[string]*string{"retention.ms": &retention},
	}, false); err != nil {
		t.Fatalf("create topic %s: %v", topic, err)
	}

	// R:login 主生产者的配置(newDBTaskProducerConfig),与生产路径一样经 client 建 SyncProducer。
	cfg := testKafkaConfig()
	cfg.Brokers = []string{broker}
	cfg.Topic = topic
	readClient, err := sarama.NewClient(cfg.Brokers, newDBTaskProducerConfig(cfg))
	if err != nil {
		t.Fatalf("create read client: %v", err)
	}
	t.Cleanup(func() { _ = readClient.Close() })

	// 等 metadata 可见:10s 截止(单调时钟),每 100ms 轮询一次。
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := readClient.RefreshMetadata(topic); err == nil {
			if got, err := readClient.Partitions(topic); err == nil && len(got) == int(partitions) {
				break
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("topic %s metadata did not show %d partitions within 10s", topic, partitions)
		}
		time.Sleep(100 * time.Millisecond)
	}

	readProducer, err := sarama.NewSyncProducerFromClient(readClient)
	if err != nil {
		t.Fatalf("create read producer: %v", err)
	}
	t.Cleanup(func() { _ = readProducer.Close() })

	// W:模拟 C++ scene 的写(librdkafka consistent_random 的 Go 对应物)。
	// SyncProducer 要求 Return.Successes=true;acks 与 R 一致。
	writeCfg := sarama.NewConfig()
	writeCfg.Version = sarama.V3_6_0_0
	writeCfg.Producer.Return.Successes = true
	writeCfg.Producer.RequiredAcks = sarama.WaitForAll
	writeCfg.Producer.Partitioner = kafkautil.NewDBTaskPartitioner
	writeProducer, err := sarama.NewSyncProducer([]string{broker}, writeCfg)
	if err != nil {
		t.Fatalf("create write producer: %v", err)
	}
	t.Cleanup(func() { _ = writeProducer.Close() })

	const playerID = uint64(7311864093319258112)
	key := kafkautil.DBTaskKey(playerID)
	want, err := kafkautil.DBTaskPartition([]byte(key), partitions)
	if err != nil {
		t.Fatalf("DBTaskPartition: %v", err)
	}

	send := func(producer sarama.SyncProducer, op db_proto.DBOp) (int32, int64) {
		t.Helper()
		value, err := proto.Marshal(&db_proto.DBTask{
			Key:    playerID,
			Op:     op,
			TaskId: "it-" + op.String() + "-" + strconv.FormatInt(time.Now().UnixNano(), 10),
		})
		if err != nil {
			t.Fatalf("marshal %s task: %v", op, err)
		}
		partition, offset, err := producer.SendMessage(&sarama.ProducerMessage{
			Topic: topic,
			Key:   sarama.StringEncoder(key),
			Value: sarama.ByteEncoder(value),
		})
		if err != nil {
			t.Fatalf("send %s: %v", op, err)
		}
		return partition, offset
	}

	pW, oW := send(writeProducer, db_proto.DBOp_DB_OP_WRITE)
	pR, oR := send(readProducer, db_proto.DBOp_DB_OP_READ)
	if pW != want || pR != want {
		t.Fatalf("write partition=%d read partition=%d, both must equal contract partition %d", pW, pR, want)
	}
	if oR <= oW {
		t.Fatalf("read offset %d must be after write offset %d in partition %d", oR, oW, want)
	}
}
