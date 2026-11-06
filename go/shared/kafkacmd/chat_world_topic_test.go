package kafkacmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// 世界频道广播 topic 的基名与分区数是 Go 生产者(chat)、C++ 消费者(gate)与部署预建三方的契约。
// 这里读 C++ 头文件逐字对拍:任何一边改了另一边没跟,gate assign 的分区与预建的不一致,推送静默全丢。
func TestChatWorldBroadcastContractMatchesCpp(t *testing.T) {
	header, err := os.ReadFile(filepath.Join("..", "..", "..", "cpp", "libs", "engine", "core", "node", "system", "node", "node_command_topic.h"))
	if err != nil {
		t.Fatalf("read node_command_topic.h: %v", err)
	}
	base := regexp.MustCompile(`kChatWorldBroadcastTopicBase\s*=\s*"([^"]+)"`).FindSubmatch(header)
	if base == nil || string(base[1]) != ChatWorldBroadcastTopicBase {
		t.Fatalf("C++ kChatWorldBroadcastTopicBase = %q, Go ChatWorldBroadcastTopicBase = %q", base, ChatWorldBroadcastTopicBase)
	}
	partitions := regexp.MustCompile(`kChatWorldBroadcastTopicPartitions\s*=\s*(\d+)`).FindSubmatch(header)
	if partitions == nil {
		t.Fatal("C++ kChatWorldBroadcastTopicPartitions not found")
	}
	n, err := strconv.Atoi(string(partitions[1]))
	if err != nil || int32(n) != ChatWorldBroadcastTopicPartitions {
		t.Fatalf("C++ kChatWorldBroadcastTopicPartitions = %s, Go ChatWorldBroadcastTopicPartitions = %d", partitions[1], ChatWorldBroadcastTopicPartitions)
	}
}

func TestChatWorldBroadcastTopicIsNotACommandTopic(t *testing.T) {
	topic := CommandTopicName(ChatWorldBroadcastTopicBase, 3)
	if topic != "chat-world-bcast_g3" {
		t.Fatalf("topic = %q", topic)
	}
	// 不是 `<x>-cmd_g<N>`:CommandPartitionBalancer 不按显式分区发它,走 Fallback 的 key 哈希。
	if IsCommandTopic(topic) {
		t.Fatalf("%q must not be classified as a command topic", topic)
	}
}
