package kafkacmd

import (
	"encoding/json"
	"os"
	"testing"

	kafkago "github.com/segmentio/kafka-go"
)

// commandPartitionVectors 是 testdata/command_partition_vectors.json 的结构。
// 语言中立:C++ 侧(cpp/tests/kafka_command_test)读同一份文件跑同一批用例,
// 两端算出不同的分区 = 命令发到没人 assign 的分区上、静默全丢,这是必须被
// 编译期之外的东西盯住的一致性。
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

func loadCommandPartitionVectors(t *testing.T) commandPartitionVectors {
	t.Helper()
	data, err := os.ReadFile("testdata/command_partition_vectors.json")
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v commandPartitionVectors
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if len(v.Cases) == 0 {
		t.Fatal("vector file has no cases")
	}
	return v
}

// TestCommandPartitionVectors 是与 C++ 的对拍本体。
func TestCommandPartitionVectors(t *testing.T) {
	v := loadCommandPartitionVectors(t)

	// 向量文件里写死的默认值必须与代码常量一致,否则"两边都按向量过了"却和
	// 线上跑的默认契约不是一回事。
	if v.DefaultPartitions != DefaultCommandTopicPartitions {
		t.Fatalf("vector default_partitions=%d but code has %d",
			v.DefaultPartitions, DefaultCommandTopicPartitions)
	}
	if v.DefaultGeneration != DefaultCommandTopicGeneration {
		t.Fatalf("vector default_generation=%d but code has %d",
			v.DefaultGeneration, DefaultCommandTopicGeneration)
	}

	for _, c := range v.Cases {
		got := CommandPartitionForNode(c.NodeID, c.Partitions)
		if got != c.ExpectPartition {
			t.Errorf("%s: partition for node %d with P=%d = %d, want %d",
				c.Name, c.NodeID, c.Partitions, got, c.ExpectPartition)
		}
		topic := CommandTopicName(c.Base, c.Generation)
		if topic != c.ExpectTopic {
			t.Errorf("%s: topic for base %q generation %d = %q, want %q",
				c.Name, c.Base, c.Generation, topic, c.ExpectTopic)
		}
	}
}

func TestCommandTopicBaseName(t *testing.T) {
	if got := CommandTopicBaseName("gate"); got != GateCommandTopicBase {
		t.Errorf("CommandTopicBaseName(gate) = %q, want %q", got, GateCommandTopicBase)
	}
	if got := CommandTopicBaseName("scene"); got != SceneCommandTopicBase {
		t.Errorf("CommandTopicBaseName(scene) = %q, want %q", got, SceneCommandTopicBase)
	}
	// 空短名不许拼出一个只有后缀的名字。
	if got := CommandTopicBaseName(""); got != "" {
		t.Errorf("CommandTopicBaseName(\"\") = %q, want empty", got)
	}
	if got := CommandTopicName("", 1); got != "" {
		t.Errorf("CommandTopicName(\"\", 1) = %q, want empty", got)
	}
}

func TestIsCommandTopic(t *testing.T) {
	cases := map[string]bool{
		"gate-cmd_g1":  true,
		"scene-cmd_g1": true,
		"gate-cmd_g12": true,
		"gate-cmd":     false,
		"gate-cmd_g":   false,
		"gate-cmd_gx":  false,
		"gate-5":       false,
		"db_task_zone_1_g1": false,
		"":                  false,
	}
	for topic, want := range cases {
		if got := IsCommandTopic(topic); got != want {
			t.Errorf("IsCommandTopic(%q) = %v, want %v", topic, got, want)
		}
	}
}

// TestParseNodeIDFailClosed:拿不到数字节点号就必须报错,不能默默发到 0 号分区
// (0 号分区上坐着 node_id 是 256 的倍数的那些节点,命令会被它们读到再丢弃)。
func TestParseNodeIDFailClosed(t *testing.T) {
	if _, err := ParseNodeID("not-a-number"); err == nil {
		t.Error("ParseNodeID accepted a non-numeric id")
	}
	if _, err := ParseNodeID(""); err == nil {
		t.Error("ParseNodeID accepted an empty id")
	}
	if _, err := GateCommandMessage("abc", "k", []byte("v")); err == nil {
		t.Error("GateCommandMessage accepted a non-numeric gate id")
	}
	id, err := ParseNodeID(" 42 ")
	if err != nil || id != 42 {
		t.Errorf("ParseNodeID(\" 42 \") = %d, %v; want 42, nil", id, err)
	}
}

func TestGateCommandMessageAddressing(t *testing.T) {
	msg, err := GateCommandMessage("300", "7", []byte("payload"))
	if err != nil {
		t.Fatalf("GateCommandMessage: %v", err)
	}
	if msg.Topic != GateCommandTopic() {
		t.Errorf("topic = %q, want %q", msg.Topic, GateCommandTopic())
	}
	if want := CommandPartitionForNode(300, DefaultCommandTopicPartitions); msg.Partition != want {
		t.Errorf("partition = %d, want %d", msg.Partition, want)
	}
}

// stubBalancer 记录自己有没有被调用,用来证明命令 topic 没有走 fallback。
type stubBalancer struct{ called bool }

func (s *stubBalancer) Balance(msg kafkago.Message, partitions ...int) int {
	s.called = true
	return 0
}

// TestCommandPartitionBalancer:命令 topic 必须原样落在 Message.Partition 上,
// 其它 topic 交给 fallback。kafka-go 的 Writer 在写入路径上忽略 Message.Partition
// (只问 Balancer),这个 Balancer 就是把它接回来的那一环。
func TestCommandPartitionBalancer(t *testing.T) {
	all := make([]int, DefaultCommandTopicPartitions)
	for i := range all {
		all[i] = i
	}

	fallback := &stubBalancer{}
	b := &CommandPartitionBalancer{Fallback: fallback}

	cmd := kafkago.Message{Topic: GateCommandTopic(), Partition: 44, Key: []byte("k")}
	if got := b.Balance(cmd, all...); got != 44 {
		t.Errorf("command topic routed to partition %d, want 44", got)
	}
	if fallback.called {
		t.Error("command topic must not go through the fallback balancer")
	}

	other := kafkago.Message{Topic: "player_migrate", Partition: 44, Key: []byte("k")}
	if got := b.Balance(other, all...); got != 0 {
		t.Errorf("non-command topic partition = %d, want the fallback's 0", got)
	}
	if !fallback.called {
		t.Error("non-command topic must go through the fallback balancer")
	}

	// 契约不符(broker 只有 1 分区)时不能假装成功地落到别的分区上:
	// 走 fallback 并留 ERROR 日志,由 topic 预建门禁去修。
	fallback.called = false
	if got := b.Balance(cmd, 0); got != 0 {
		t.Errorf("out-of-contract partition = %d, want fallback 0", got)
	}
	if !fallback.called {
		t.Error("out-of-contract command must fall back rather than return a missing partition")
	}
}
