package kafkautil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/IBM/sarama"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// Expand status constants
const (
	ExpandStatusNormal    = "normal"
	ExpandStatusExpanding = "expanding"
	ExpandStatusCompleted = "completed"
)

// ExpandStatusData is the expand status stored in Redis.
type ExpandStatusData struct {
	Status         string `json:"status"`
	PartitionCount int32  `json:"partition_count"`
	UpdateTime     int64  `json:"update_time"`
}

// SetExpandStatus writes expand status to Redis.
func SetExpandStatus(ctx context.Context, redisClient redis.Cmdable, topic string, status string, partitionCount int32) error {
	key := getExpandStatusKey(topic)
	data := ExpandStatusData{
		Status:         status,
		PartitionCount: partitionCount,
		UpdateTime:     time.Now().UnixMilli(),
	}
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal expand status failed: %w", err)
	}
	return redisClient.Set(ctx, key, dataBytes, 0).Err()
}

// GetExpandStatus reads expand status from Redis.
func GetExpandStatus(ctx context.Context, redisClient redis.Cmdable, topic string) (*ExpandStatusData, error) {
	key := getExpandStatusKey(topic)
	dataBytes, err := redisClient.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return &ExpandStatusData{Status: ExpandStatusNormal, PartitionCount: 0}, nil
		}
		return nil, fmt.Errorf("get expand status failed: %w", err)
	}

	var data ExpandStatusData
	if err := json.Unmarshal(dataBytes, &data); err != nil {
		return nil, fmt.Errorf("unmarshal expand status failed: %w", err)
	}
	return &data, nil
}

// WaitOldPartitionsEmpty 等待旧分区不再保留记录，不读取或证明消费者组的消费进度。
func WaitOldPartitionsEmpty(ctx context.Context, client sarama.Client, topic string, oldPartitionCount int32) error {
	logx.Infof("waiting for old partitions to become empty: topic=%s, oldCount=%d", topic, oldPartitionCount)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for empty old partitions canceled: %w", ctx.Err())
		case <-ticker.C:
			allEmpty := true
			for i := int32(0); i < oldPartitionCount; i++ {
				newestOffset, err := client.GetOffset(topic, i, sarama.OffsetNewest)
				if err != nil {
					logx.Errorf("get newest offset failed: topic=%s, partition=%d, err=%v", topic, i, err)
					allEmpty = false
					break
				}
				oldestOffset, err := client.GetOffset(topic, i, sarama.OffsetOldest)
				if err != nil {
					logx.Errorf("get oldest offset failed: topic=%s, partition=%d, err=%v", topic, i, err)
					allEmpty = false
					break
				}

				if oldestOffset < newestOffset {
					logx.Debugf("partition retains records: topic=%s, partition=%d, oldest=%d, newest=%d",
						topic, i, oldestOffset, newestOffset)
					allEmpty = false
					break
				}
			}

			if allEmpty {
				logx.Infof("all old partitions are empty: topic=%s, oldCount=%d", topic, oldPartitionCount)
				return nil
			}
		}
	}
}

func getExpandStatusKey(topic string) string {
	return fmt.Sprintf("kafka:expand:status:%s", topic)
}

// GetNewPartitionIDs returns IDs of newly added partitions (e.g., old=3, new=5 -> [3,4]).
func GetNewPartitionIDs(oldPartitionCount, newPartitionCount int32) []int32 {
	var newIDs []int32
	for i := oldPartitionCount; i < newPartitionCount; i++ {
		newIDs = append(newIDs, i)
	}
	return newIDs
}

// GetCurrentPartitionCount returns the current partition count for a topic.
func GetCurrentPartitionCount(client sarama.Client, topic string) (int32, error) {
	partitions, err := client.Partitions(topic)
	if err != nil {
		return 0, fmt.Errorf("get partitions failed: %w", err)
	}
	return int32(len(partitions)), nil
}
