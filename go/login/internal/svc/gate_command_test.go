package svc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	game "login/generated/pb/game"
	kafkapb "proto/contracts/kafka"
	"shared/kafkacmd"
)

// 本文件盯住 login 发往 gate 的两条控制面命令(BindSession / KickPlayer)的寻址与守卫。
//
// 为什么要跨模块读 shared 的向量文件:寻址契约(topic 名 + partition = node_id % P)
// 有三份实现在跑 —— C++ 的 node_command_topic.h、Go 的 shared/kafkacmd、以及各生产者
// 的调用方式。login 只要有一处算得不一样,命令就落到没人 assign 的分区上、静默全丢,
// Kafka 一个错都不报。所以这里直接对拍 C++ 与 Go 共用的那份向量,
// 而不是在测试里重算一遍 node_id % 256(那只能证明测试和实现抄了同一个公式)。
//
// 设计文档:docs/design/control-plane-topic-partitioning-20260908.md

type commandPartitionVectors struct {
	DefaultPartitions int32  `json:"default_partitions"`
	DefaultGeneration uint32 `json:"default_generation"`
	Cases             []struct {
		Name            string `json:"name"`
		Base            string `json:"base"`
		NodeID          uint64 `json:"node_id"`
		Partitions      int32  `json:"partitions"`
		Generation      uint32 `json:"generation"`
		ExpectPartition int    `json:"expect_partition"`
		ExpectTopic     string `json:"expect_topic"`
	} `json:"cases"`
}

// vectorPath 指向 go/shared/kafkacmd/testdata/command_partition_vectors.json,
// 即 C++ 的 kafka_command_test 与 Go 的 kafkacmd 单测读的同一份文件。
var vectorPath = filepath.Join("..", "..", "..", "shared", "kafkacmd", "testdata", "command_partition_vectors.json")

func loadCommandPartitionVectors(t *testing.T) commandPartitionVectors {
	t.Helper()
	data, err := os.ReadFile(vectorPath)
	if err != nil {
		t.Fatalf("read shared partition vectors %s: %v", vectorPath, err)
	}
	var v commandPartitionVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("parse shared partition vectors: %v", err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("shared partition vector file has no cases")
	}
	return v
}

// requireDefaultContract 保证本进程跑在向量描述的那套默认契约上。
// CommandContract() 会读环境变量覆盖,被覆盖时向量里的期望值本来就不适用 —— 跳过
// 比假绿好。
func requireDefaultContract(t *testing.T) {
	t.Helper()
	c := kafkacmd.CommandContract()
	if c.Partitions != kafkacmd.DefaultCommandTopicPartitions || c.Generation != kafkacmd.DefaultCommandTopicGeneration {
		t.Skipf("process command contract overridden by env (partitions=%d generation=%d); vectors describe the default contract",
			c.Partitions, c.Generation)
	}
}

// TestGateCommandAddressingMatchesSharedVectors 是与 C++/其它 Go 服务的对拍本体:
// 同一个 gate node id,login 算出的 (topic, partition) 必须与共享向量逐字相同。
func TestGateCommandAddressingMatchesSharedVectors(t *testing.T) {
	requireDefaultContract(t)
	v := loadCommandPartitionVectors(t)

	// 向量文件里写死的默认值必须与 kafkacmd 的常量一致,否则"对拍过了"却不是线上
	// 跑的那套契约。
	if v.DefaultPartitions != kafkacmd.DefaultCommandTopicPartitions {
		t.Fatalf("vector default_partitions=%d but kafkacmd has %d",
			v.DefaultPartitions, kafkacmd.DefaultCommandTopicPartitions)
	}
	if v.DefaultGeneration != kafkacmd.DefaultCommandTopicGeneration {
		t.Fatalf("vector default_generation=%d but kafkacmd has %d",
			v.DefaultGeneration, kafkacmd.DefaultCommandTopicGeneration)
	}

	const instanceID = "gate-instance-uuid"
	applied := 0
	for _, c := range v.Cases {
		// 只取 gate 侧、且有效契约等于本进程默认契约的用例:login 的运行时路径没有
		// "临时换一套分区数"的入口,别的用例(代号 2/3、P=1024)由 kafkacmd 自己的
		// 单测覆盖。
		if c.Base != kafkacmd.GateCommandTopicBase {
			continue
		}
		if kafkacmd.NormalizeCommandTopicPartitions(c.Partitions) != kafkacmd.DefaultCommandTopicPartitions ||
			kafkacmd.NormalizeCommandTopicGeneration(c.Generation) != kafkacmd.DefaultCommandTopicGeneration {
			continue
		}
		// node_id 0 在向量里只是"纯函数不许崩"的用例;login 侧它是 fail-closed 的
		// (见 buildGateCommandMessage 的守卫三,由 TestGateCommandFailsClosed 覆盖)。
		if c.NodeID == 0 {
			continue
		}
		applied++

		gateID := strconv.FormatUint(c.NodeID, 10)

		bind, err := buildBindSessionCommand(gateID, instanceID, 7, 1001, 2)
		if err != nil {
			t.Fatalf("%s: buildBindSessionCommand(%s): %v", c.Name, gateID, err)
		}
		kick, err := buildKickPlayerCommand(gateID, instanceID, 7, 1001)
		if err != nil {
			t.Fatalf("%s: buildKickPlayerCommand(%s): %v", c.Name, gateID, err)
		}

		for path, msg := range map[string]gateCommandMessage{"bind session": bind, "kick": kick} {
			if msg.Topic != c.ExpectTopic {
				t.Errorf("%s/%s: topic for gate %s = %q, want %q",
					c.Name, path, gateID, msg.Topic, c.ExpectTopic)
			}
			if int(msg.Partition) != c.ExpectPartition {
				t.Errorf("%s/%s: partition for gate %s = %d, want %d",
					c.Name, path, gateID, msg.Partition, c.ExpectPartition)
			}
			// 老的一节点一 topic 名字必须彻底消失:只要还有一条命令写 gate-<id>,
			// Kafka.DisableLegacyPerNodeTopic 就不能置 true。
			if msg.Topic == "gate-"+gateID {
				t.Errorf("%s/%s: still producing to the legacy per-node topic %q", c.Name, path, msg.Topic)
			}
		}
	}

	if applied == 0 {
		t.Fatal("no gate-cmd vector case matched the default contract; vectors or contract drifted")
	}
}

// TestGateCommandTargetFieldsArePopulated:两级过滤的两个字段都必须填上。
// target_gate_id 留 0 会让消费端 ValidateCommandTarget 的整数过滤直接放行
// (同分区上几百个 gate,每条消息都退化成一次字符串比较)。
func TestGateCommandTargetFieldsArePopulated(t *testing.T) {
	requireDefaultContract(t)

	const (
		gateID     = "300"
		instanceID = "b0a1-uuid"
		sessionID  = uint32(42)
		playerID   = uint64(90001)
	)

	cases := []struct {
		name         string
		build        func() (gateCommandMessage, error)
		wantEventID  uint32
		wantEnterGs  uint32
		wantSession  uint32
		wantPlayerID uint64
	}{
		{
			name: "bind session",
			build: func() (gateCommandMessage, error) {
				return buildBindSessionCommand(gateID, instanceID, sessionID, playerID, 3)
			},
			wantEventID:  uint32(game.ContractsKafkaBindSessionEventEventId),
			wantEnterGs:  3,
			wantSession:  sessionID,
			wantPlayerID: playerID,
		},
		{
			name: "kick",
			build: func() (gateCommandMessage, error) {
				return buildKickPlayerCommand(gateID, instanceID, sessionID, playerID)
			},
			wantEventID:  uint32(game.ContractsKafkaKickPlayerEventEventId),
			wantSession:  sessionID,
			wantPlayerID: playerID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := tc.build()
			if err != nil {
				t.Fatalf("build: %v", err)
			}

			var cmd kafkapb.GateCommand
			if err := proto.Unmarshal(msg.Payload, &cmd); err != nil {
				t.Fatalf("unmarshal GateCommand: %v", err)
			}
			if cmd.GetTargetGateId() != 300 {
				t.Errorf("target_gate_id = %d, want 300 (0 disables the cheap numeric filter)", cmd.GetTargetGateId())
			}
			if cmd.GetTargetInstanceId() != instanceID {
				t.Errorf("target_instance_id = %q, want %q", cmd.GetTargetInstanceId(), instanceID)
			}
			if cmd.GetEventId() != tc.wantEventID {
				t.Errorf("event_id = %d, want %d", cmd.GetEventId(), tc.wantEventID)
			}
			if cmd.GetEnterGsType() != tc.wantEnterGs {
				t.Errorf("enter_gs_type = %d, want %d", cmd.GetEnterGsType(), tc.wantEnterGs)
			}
			if cmd.GetSessionId() != tc.wantSession {
				t.Errorf("session_id = %d, want %d", cmd.GetSessionId(), tc.wantSession)
			}
			if cmd.GetPlayerId() != tc.wantPlayerID {
				t.Errorf("player_id = %d, want %d", cmd.GetPlayerId(), tc.wantPlayerID)
			}
			// 分区号必须与 target_gate_id 同源:300 % 256 = 44。
			if msg.Partition != 44 {
				t.Errorf("partition = %d, want 44 (300 %% 256)", msg.Partition)
			}
			if msg.PartitionKey != strconv.FormatUint(playerID, 10) {
				t.Errorf("partition key = %q, want %q", msg.PartitionKey, strconv.FormatUint(playerID, 10))
			}
		})
	}
}

// TestGateCommandFailsClosed:两道守卫都必须**在碰 Kafka 之前**拦下。
//
// 刻意用零值 ServiceContext(KafkaClient 为 nil)去调**导出的**发送方法:
// 守卫要是漏了,这里会 nil 解引用 panic 而不是悄悄变绿 —— 也就顺带证明了守卫
// 确实长在真正的发送路径上,而不只是长在被测的那个 helper 里。
func TestGateCommandFailsClosed(t *testing.T) {
	requireDefaultContract(t)
	sc := &ServiceContext{}

	cases := []struct {
		name       string
		gateID     string
		instanceID string
		wantSubstr string
	}{
		{
			name:       "empty gate instance id",
			gateID:     "5",
			instanceID: "",
			wantSubstr: "empty gate_instance_id",
		},
		{
			name:       "non-numeric gate id",
			gateID:     "gate-a",
			instanceID: "uuid-1",
			wantSubstr: "invalid node id",
		},
		{
			name:       "empty gate id",
			gateID:     "",
			instanceID: "uuid-1",
			wantSubstr: "invalid node id",
		},
		{
			name:       "gate id overflows uint32",
			gateID:     "4294967296",
			instanceID: "uuid-1",
			wantSubstr: "invalid node id",
		},
		{
			// proto3 没有 presence:SessionDetails.gate_node_id 没填就是 0。
			// 放过去等于同时废掉寻址(落 0 号分区)和过滤(target_gate_id=0 放行)。
			name:       "unset gate node id (zero)",
			gateID:     "0",
			instanceID: "uuid-1",
			wantSubstr: "gate node id is 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sc.SendBindSessionToGate(tc.gateID, tc.instanceID, 1, 2, 0, 0)
			if err == nil {
				t.Fatal("SendBindSessionToGate: want error, got nil (command would be unroutable/unfilterable)")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("SendBindSessionToGate error = %q, want it to mention %q", err, tc.wantSubstr)
			}

			err = sc.KickSessionOnGate(tc.gateID, tc.instanceID, 1, 2)
			if err == nil {
				t.Fatal("KickSessionOnGate: want error, got nil (command would be unroutable/unfilterable)")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("KickSessionOnGate error = %q, want it to mention %q", err, tc.wantSubstr)
			}
		})
	}
}

// TestGateCommandTopicIsRecognizedAsCommandTopic 把 login 算出的 topic 交回
// kafkacmd.IsCommandTopic 过一遍:生产端的 sarama 分区器正是靠这个判据决定
// "尊重显式分区还是走哈希"。两者对不上 = 命令被哈希打散,消费端收不到。
func TestGateCommandTopicIsRecognizedAsCommandTopic(t *testing.T) {
	requireDefaultContract(t)

	msg, err := buildKickPlayerCommand("7", "uuid-7", 1, 2)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !kafkacmd.IsCommandTopic(msg.Topic) {
		t.Fatalf("IsCommandTopic(%q) = false; the producer would hash-partition this command", msg.Topic)
	}
}
