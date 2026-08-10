package kafkautil

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/IBM/sarama"
	"github.com/zeromicro/go-zero/core/logx"
)

// TopicSpec describes a topic to ensure exists with specific retention.
type TopicSpec struct {
	Name          string
	Partitions    int32
	RetentionMs   int64 // -1 = use broker default
	ReplicaFactor int16 // 0 = use default (1)
}

// EnsureTopics creates missing topics, verifies an immutable partition-count
// contract, and applies retention config. Existing topics are never expanded in
// place: changing the partition count remaps keyed messages and breaks the
// per-player ordering cursor used by db service. A planned expansion must use a
// new topic generation after the old topic and retry queues are fully drained.
func EnsureTopics(brokers []string, specs []TopicSpec) error {
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_0_0_0

	admin, err := sarama.NewClusterAdmin(brokers, cfg)
	if err != nil {
		return fmt.Errorf("kafka admin connect: %w", err)
	}
	defer admin.Close()

	existing, err := admin.ListTopics()
	if err != nil {
		return fmt.Errorf("kafka list topics: %w", err)
	}

	for _, spec := range specs {
		if strings.TrimSpace(spec.Name) == "" {
			return fmt.Errorf("kafka topic name is empty")
		}
		if spec.Partitions <= 0 {
			return fmt.Errorf("kafka topic %s has invalid partition contract %d", spec.Name, spec.Partitions)
		}
		replica := spec.ReplicaFactor
		if replica <= 0 {
			replica = 1
		}

		retentionStr := fmt.Sprintf("%d", spec.RetentionMs)

		if _, exists := existing[spec.Name]; !exists {
			topicDetail := &sarama.TopicDetail{
				NumPartitions:     spec.Partitions,
				ReplicationFactor: replica,
			}
			if spec.RetentionMs > 0 {
				topicDetail.ConfigEntries = map[string]*string{
					"retention.ms": &retentionStr,
				}
			}
			if err := admin.CreateTopic(spec.Name, topicDetail, false); err != nil && !errors.Is(err, sarama.ErrTopicAlreadyExists) {
				return fmt.Errorf("kafka create topic %s: %w", spec.Name, err)
			}
			logx.Infof("kafka topic create requested: %s (partitions=%d, retention=%dms)",
				spec.Name, spec.Partitions, spec.RetentionMs)

			// login and db can race on first boot. Refresh metadata even when
			// CreateTopic returned TopicAlreadyExists, then verify that the
			// winner created the exact same routing contract.
			var err error
			existing, err = admin.ListTopics()
			if err != nil {
				return fmt.Errorf("kafka refresh topics after creating %s: %w", spec.Name, err)
			}
		}

		detail, exists := existing[spec.Name]
		if !exists {
			return fmt.Errorf("kafka topic %s is still absent after create", spec.Name)
		}
		if detail.NumPartitions != spec.Partitions {
			return fmt.Errorf("kafka topic %s partition contract mismatch: broker=%d config=%d; in-place expansion is forbidden, drain the old topic/retry queues and switch both login+db to a new TopicGeneration",
				spec.Name, detail.NumPartitions, spec.Partitions)
		}

		markerPrefix, expectedMarker := partitionContractMarker(spec.Name, spec.Partitions)
		markerExists := false
		for topicName := range existing {
			if !strings.HasPrefix(topicName, markerPrefix) {
				continue
			}
			if topicName != expectedMarker {
				return fmt.Errorf("kafka topic %s immutable partition marker conflicts: found=%s expected=%s; use a new TopicGeneration",
					spec.Name, topicName, expectedMarker)
			}
			markerExists = true
		}
		if !markerExists {
			cleanupPolicy := "compact"
			markerDetail := &sarama.TopicDetail{
				NumPartitions:     1,
				ReplicationFactor: replica,
				ConfigEntries: map[string]*string{
					"cleanup.policy": &cleanupPolicy,
				},
			}
			if err := admin.CreateTopic(expectedMarker, markerDetail, false); err != nil && !errors.Is(err, sarama.ErrTopicAlreadyExists) {
				return fmt.Errorf("create immutable partition marker %s: %w", expectedMarker, err)
			}
			logx.Infof("kafka immutable partition marker ensured: topic=%s partitions=%d marker=%s",
				spec.Name, spec.Partitions, expectedMarker)
		}

		if spec.RetentionMs > 0 {
			// 必须用增量接口:sarama 的 AlterConfig 走的是 Kafka 遗留 AlterConfigs
			// 协议,语义是**全量替换**该 topic 的动态配置 —— 只提交 retention.ms
			// 会把运维在 broker 上手工设过的其它覆盖项(cleanup.policy、
			// max.message.bytes 等)全部抹回默认值,而且每次服务启动都抹一遍。
			// IncrementalAlterConfigs(KIP-339,broker ≥2.3;上面 cfg.Version
			// 已声明 3.0)按条目 SET,只动列出的键。
			entries := map[string]sarama.IncrementalAlterConfigsEntry{
				"retention.ms": {
					Operation: sarama.IncrementalAlterConfigsOperationSet,
					Value:     &retentionStr,
				},
			}
			if err := admin.IncrementalAlterConfig(sarama.TopicResource, spec.Name, entries, false); err != nil {
				return fmt.Errorf("kafka alter topic %s retention: %w", spec.Name, err)
			}
			logx.Infof("kafka topic %s retention updated to %dms", spec.Name, spec.RetentionMs)
		}
	}

	return nil
}

func partitionContractMarker(topic string, partitions int32) (prefix, name string) {
	sum := sha256.Sum256([]byte(topic))
	prefix = fmt.Sprintf("__mmorpg_partition_contract_%x_p", sum)
	return prefix, fmt.Sprintf("%s%d", prefix, partitions)
}
