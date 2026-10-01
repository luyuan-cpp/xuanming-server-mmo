package kafkautil

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IBM/sarama"
	"github.com/zeromicro/go-zero/core/logx"
)

// TopicSpec describes a topic to ensure exists with specific retention.
type TopicSpec struct {
	Name          string
	Partitions    int32
	RetentionMs   int64 // -1 = use broker default
	ReplicaFactor int16 // 0 = use the deployment contract (DefaultReplicationFactor)
}

const (
	// EnvTopicReplicationFactor 是**部署级**的 topic 副本数契约:broker 有几个、topic 存几份,
	// 是部署决定的事,不是某个服务的配置。K8s 上由 k8s_deploy.ps1 按 -KafkaBrokers 统一注入到每个
	// go-svc(单 broker = 1,≥3 个 broker = 3);本机 / docker-compose 是单 broker,不设,取默认 1。
	//
	// 为什么不让 broker 的 default.replication.factor 说了算(CreateTopics 传 -1):那需要 CreateTopics v4
	// (KIP-464),而本仓库钉的 sarama v1.43.1 最高只发 v3,-1 会被 broker 以 InvalidReplicationFactor 拒掉。
	EnvTopicReplicationFactor = "KAFKA_TOPIC_REPLICATION_FACTOR"

	defaultTopicReplicationFactor int16 = 1
)

var (
	topicReplicationOnce sync.Once
	topicReplication     int16
)

// DefaultReplicationFactor 读一次 EnvTopicReplicationFactor 并缓存。非法值记 ERROR 后回落到 1:
// 与 kafkacmd.CommandContract 同一口径 —— 一个打错的环境变量不该让服务起不来,但必须留痕。
// 回落到 1 之后,若集群实际要求更多副本,下面 ensureTopics 的副本数核对会把它拦下来。
func DefaultReplicationFactor() int16 {
	topicReplicationOnce.Do(func() {
		topicReplication = parseReplicationFactor(os.Getenv(EnvTopicReplicationFactor))
		logx.Infof("kafkautil: topic replication contract replication_factor=%d", topicReplication)
	})
	return topicReplication
}

func parseReplicationFactor(raw string) int16 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultTopicReplicationFactor
	}
	v, err := strconv.ParseInt(raw, 10, 16)
	if err != nil || v <= 0 {
		logx.Errorf("kafkautil: invalid %s=%q, falling back to %d",
			EnvTopicReplicationFactor, raw, defaultTopicReplicationFactor)
		return defaultTopicReplicationFactor
	}
	return int16(v)
}

// 只暴露初始化需要的管理操作，测试不连接真实 Kafka。
type topicAdmin interface {
	ListTopics() (map[string]sarama.TopicDetail, error)
	CreateTopic(string, *sarama.TopicDetail, bool) error
	IncrementalAlterConfig(sarama.ConfigResourceType, string, map[string]sarama.IncrementalAlterConfigsEntry, bool) error
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

	return ensureTopics(admin, specs, DefaultReplicationFactor(), time.Now, time.Sleep)
}

// defaultReplica 是 spec 没有显式写 ReplicaFactor 时用的副本数(生产路径传 DefaultReplicationFactor())。
func ensureTopics(admin topicAdmin, specs []TopicSpec, defaultReplica int16, now func() time.Time, sleep func(time.Duration)) error {
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
			replica = defaultReplica
		}
		if replica <= 0 {
			replica = defaultTopicReplicationFactor
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
			logx.Infof("kafka topic create requested: %s (partitions=%d, replication=%d, retention=%dms)",
				spec.Name, spec.Partitions, replica, spec.RetentionMs)

			// 创建成功不代表每个 broker 已看到新 topic。login/db 并发启动时
			// AlreadyExists 也需等 metadata 可见，再沿用下面的分区和 marker 校验。
			existing, err = waitForCreatedTopicMetadata(admin, spec.Name, now, sleep)
			if err != nil {
				return err
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
		// 副本数不够 = 这个 topic 仍然是单点:唯一持有它的 broker 一倒,消息就写不进、读不出,
		// 而集群看起来是"多 broker、有冗余"的。不在启动期拦下,就会一直带着假冗余跑到出事那天。
		// 副本数**多于**契约不算错(运维手工加过副本)。ReplicationFactor==0 表示元数据里没带副本信息,不猜。
		// 修法:topic 是空的就删掉重建;有数据就用 kafka-reassign-partitions.sh 补副本。副本数可以原地改
		// (与分区数不同,它不参与寻址),所以这里不要求换 TopicGeneration。
		if detail.ReplicationFactor > 0 && detail.ReplicationFactor < replica {
			return fmt.Errorf("kafka topic %s replication contract mismatch: broker=%d config=%d (%s); the topic has fewer replicas than the deployment requires, raise it with kafka-reassign-partitions.sh or recreate the empty topic",
				spec.Name, detail.ReplicationFactor, replica, EnvTopicReplicationFactor)
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

const (
	topicMetadataVisibilityTimeout = 10 * time.Second
	topicMetadataPollInterval      = 100 * time.Millisecond
)

// 只重试已请求创建却暂不可见的 metadata；查询错误直接返回，不能掩盖权限或网络故障。
// 截止使用单调时间并计入查询耗时。Sarama 的 ListTopics 不接受 context，单次在途
// 查询仍受其 socket 超时约束；这里不另开无法取消的 goroutine，也不在截止后继续操作。
func waitForCreatedTopicMetadata(admin topicAdmin, topic string, now func() time.Time, sleep func(time.Duration)) (map[string]sarama.TopicDetail, error) {
	deadline := now().Add(topicMetadataVisibilityTimeout)
	for now().Before(deadline) {
		topics, err := admin.ListTopics()
		if err != nil {
			return nil, fmt.Errorf("kafka refresh topics after creating %s: %w", topic, err)
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			break
		}
		if _, exists := topics[topic]; exists {
			return topics, nil
		}
		delay := topicMetadataPollInterval
		if delay > remaining {
			delay = remaining
		}
		sleep(delay)
	}
	return nil, fmt.Errorf("kafka topic %s metadata still absent or query unfinished within %s after create: %w", topic, topicMetadataVisibilityTimeout, context.DeadlineExceeded)
}

func partitionContractMarker(topic string, partitions int32) (prefix, name string) {
	sum := sha256.Sum256([]byte(topic))
	prefix = fmt.Sprintf("__mmorpg_partition_contract_%x_p", sum)
	return prefix, fmt.Sprintf("%s%d", prefix, partitions)
}
