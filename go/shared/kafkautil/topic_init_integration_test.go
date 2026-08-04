package kafkautil

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/IBM/sarama"
)

func TestEnsureTopicsImmutablePartitionContractIntegration(t *testing.T) {
	broker := os.Getenv("KAFKA_INTEGRATION_BROKER")
	if broker == "" {
		t.Skip("set KAFKA_INTEGRATION_BROKER to run the Kafka integration test")
	}
	topic := fmt.Sprintf("mmorpg_partition_contract_it_%d", time.Now().UnixNano())
	_, marker2 := partitionContractMarker(topic, 2)
	_, marker3 := partitionContractMarker(topic, 3)

	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_0_0_0
	admin, err := sarama.NewClusterAdmin([]string{broker}, cfg)
	if err != nil {
		t.Fatalf("create cleanup admin: %v", err)
	}
	t.Cleanup(func() {
		for _, name := range []string{topic, marker2, marker3} {
			if err := admin.DeleteTopic(name); err != nil && !errors.Is(err, sarama.ErrUnknownTopicOrPartition) {
				t.Logf("cleanup topic %s: %v", name, err)
			}
		}
		_ = admin.Close()
	})

	spec2 := []TopicSpec{{Name: topic, Partitions: 2, RetentionMs: 60_000}}
	if err := EnsureTopics([]string{broker}, spec2); err != nil {
		t.Fatalf("create exact contract: %v", err)
	}
	if err := EnsureTopics([]string{broker}, spec2); err != nil {
		t.Fatalf("repeat exact contract must be idempotent: %v", err)
	}
	if err := EnsureTopics([]string{broker}, []TopicSpec{{Name: topic, Partitions: 3, RetentionMs: 60_000}}); err == nil {
		t.Fatal("same topic name must reject a changed configured partition count")
	}

	if err := admin.CreatePartitions(topic, 3, nil, false); err != nil {
		t.Fatalf("simulate external live expansion: %v", err)
	}
	if err := EnsureTopics([]string{broker}, []TopicSpec{{Name: topic, Partitions: 3, RetentionMs: 60_000}}); err == nil {
		t.Fatal("old immutable marker must reject config updated to match an externally expanded topic")
	}
}
