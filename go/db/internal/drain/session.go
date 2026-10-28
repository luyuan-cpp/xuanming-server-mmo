// Package drain是DB现有消费者和补投器的维护装配；它不重新注册writer或建立第二SQL连接。
package drain

import (
	"context"
	"crypto/sha256"
	"db/internal/barrierrestore"
	"db/internal/config"
	"db/internal/kafka"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/saveoutbox"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/redis/go-redis/v9"
	"shared/drainproof"
	"shared/kafkautil"
	"sort"
	"strings"
	"time"
)

// Run只复用调用方当前boot已guard的真实stores。SOURCE关闭时既不retire物理writer，也不关池；
// 队列达到固定全历史水位、所有内存/producer/ownerjoin后才写RELAY原件。
func Run(ctx context.Context, runtime *drainproof.Runtime, app config.Config, parent, mapping *redis.Client, stores *proto_sql.StoreRegistry) error {
	return runOwners(ctx, runtime, app, parent, mapping, stores, nil, nil, nil)
}
func RunExisting(ctx context.Context, runtime *drainproof.Runtime, app config.Config, parent, mapping *redis.Client, stores *proto_sql.StoreRegistry, current *kafka.KeyOrderedKafkaConsumer, replay *saveoutbox.Service, source func(context.Context) ([]byte, error)) error {
	if current == nil || replay == nil || source == nil {
		return drainproof.ErrHold
	}
	return runOwners(ctx, runtime, app, parent, mapping, stores, current, replay, source)
}
func runOwners(ctx context.Context, runtime *drainproof.Runtime, app config.Config, parent, mapping *redis.Client, stores *proto_sql.StoreRegistry, current *kafka.KeyOrderedKafkaConsumer, replay *saveoutbox.Service, closeSource func(context.Context) ([]byte, error)) (result error) {
	if runtime == nil || parent == nil || mapping == nil || stores == nil {
		return drainproof.ErrHold
	}
	if err := barrierrestore.ValidateAuthorityPlan(runtime.Plan); err != nil {
		return err
	}
	topics, groups, counts := map[string]bool{}, map[string]string{}, map[string]int32{}
	prefix := fmt.Sprintf("db_task_zone_%d", app.ZoneId)
	for _, row := range runtime.Plan.KafkaPartitions {
		if row.TopicName != prefix && !strings.HasPrefix(row.TopicName, prefix+"_g") {
			continue
		}
		key := row.Group + "\x00" + row.TopicName
		groups[key] = row.Group
		topics[row.TopicName] = true
		if n := int32(row.Partition) + 1; n > counts[key] {
			counts[key] = n
		}
	}
	if len(groups) == 0 {
		return errors.New("DB维护manifest缺本zone全部历史订阅")
	}
	keys := []string{}
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	matchedCurrent := current == nil
	consumers := []*kafka.KeyOrderedKafkaConsumer{}
	defer func() {
		for _, consumer := range consumers {
			result = errors.Join(result, consumer.StopAndReport(context.Background()))
		}
	}()
	seed := runtime.Seed("RELAY")
	if seed == nil {
		return drainproof.ErrHold
	}
	for _, key := range keys {
		parts := strings.SplitN(key, "\x00", 2)
		cfg := app
		kcfg := &cfg.ServerConfig.Kafka
		kcfg.Topic = parts[1]
		kcfg.GroupID = parts[0]
		kcfg.ConsumerGroup = ""
		kcfg.GroupHandoffFrom = ""
		kcfg.PartitionCnt = counts[key]
		tag := sha256.Sum256([]byte(key))
		kcfg.RetryInstanceId = hex.EncodeToString(seed.InstanceId) + ":" + hex.EncodeToString(tag[:8])
		if current != nil && kcfg.Topic == app.ServerConfig.Kafka.Topic && kcfg.GroupID == app.ServerConfig.Kafka.EffectiveGroupID() {
			consumers = append(consumers, current)
			matchedCurrent = true
			continue
		}
		consumer, err := kafka.NewKeyOrderedKafkaConsumer(cfg, parent, mapping, stores)
		if err != nil {
			return err
		}
		consumers = append(consumers, consumer)
		if err = consumer.Start(); err != nil {
			return err
		}
	}
	if !matchedCurrent {
		return errors.New("P0历史目录遗漏当前实际consumer owner")
	}
	var err error
	if replay == nil {
		replay, err = saveoutbox.Start(ctx, app, parent, mapping, stores, consumers[0])
		if err != nil {
			return err
		}
	}
	if replay == nil {
		return errors.New("维护排空要求dual存盘协议")
	}
	defer func() { result = errors.Join(result, replay.StopAndReport(context.Background())) }()
	prober, err := kafkautil.NewGroupLagProber(app.ServerConfig.Kafka.Brokers)
	if err != nil {
		return err
	}
	defer prober.Close()
	history := []string{}
	for topic := range topics {
		history = append(history, topic)
	}
	sort.Strings(history)
	source, relay := drainproof.New(), drainproof.New()
	// 专用模式从未serve；正常模式则在closeSource中合并真实已接收RPC前沿。
	sourceWritten := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var drainCtx context.Context
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		requested, err := runtime.Requested("SOURCE")
		if err != nil {
			return err
		}
		if requested && !sourceWritten {
			sourceDrainCtx, cancelDrain := context.WithTimeout(ctx, 60*time.Second)
			defer cancelDrain()
			drainCtx = sourceDrainCtx
			if closeSource != nil {
				digest, err := closeSource(drainCtx)
				if err != nil {
					return err
				}
				if err = source.MergeSettled(digest); err != nil {
					return err
				}
			}
			source.CloseAdmission()
			if err = runtime.Write("SOURCE", source); err != nil {
				return err
			}
			sourceWritten = true
		}
		relayRequested, err := runtime.Requested("RELAY")
		if err != nil {
			return err
		}
		if relayRequested && !sourceWritten {
			return errors.New("RELAY关闭请求先于SOURCE收敛")
		}
		if !sourceWritten {
			continue
		}
		if err = drainCtx.Err(); err != nil {
			return err
		}
		remaining, err := replay.DrainHistorical(drainCtx, history)
		if err != nil {
			return err
		}
		allEmpty := remaining == 0
		for _, key := range keys {
			parts := strings.SplitN(key, "\x00", 2)
			lag, err := prober.Probe(parts[0], parts[1], counts[key])
			if err != nil {
				return err
			}
			if lag.Lag != 0 || lag.RetentionGap != 0 {
				allEmpty = false
			}
			queues, err := kafka.RetryQueueDepths(drainCtx, parent, parts[1], counts[key])
			if err != nil {
				return err
			}
			if queues.Dead != 0 || len(queues.OrphanProcessing) != 0 || len(queues.ModesWithoutLease) != 0 {
				return errors.New("DB历史retry存在dead/orphan/registry不等，保持原件")
			}
			if queues.Shared != 0 || queues.PartitionedTotal() != 0 || queues.LegacyProcessing != 0 {
				allEmpty = false
			}
			for _, n := range queues.Instances {
				if n != 0 {
					allEmpty = false
				}
			}
		}
		if !relayRequested || !allEmpty {
			continue
		}
		// 源头和历史水位已固定，关闭补投后先flush/close，再等待全部consumer/命令owner退出。
		if err = replay.StopAndReport(drainCtx); err != nil {
			return err
		}
		for _, consumer := range consumers {
			if err = consumer.StopAndReport(drainCtx); err != nil {
				return err
			}
		}
		for _, consumer := range consumers {
			digest, err := consumer.FrontierDigest(drainCtx)
			if err != nil {
				return err
			}
			if err = relay.MergeSettled(digest); err != nil {
				return err
			}
		}
		digest, err := replay.FrontierDigest(drainCtx)
		if err != nil {
			return err
		}
		if err = relay.MergeSettled(digest); err != nil {
			return err
		}
		relay.CloseAdmission()
		if err = relay.Wait(drainCtx); err != nil {
			return err
		}
		// 所有同boot SQL调用已确定join，永久禁用原grant，防止同进程重开物理池。
		writer, err := proto_sql.CurrentWriterConfig()
		if err != nil {
			return err
		}
		writer.Seal()
		if err = stores.Close(); err != nil {
			return err
		}
		if err = runtime.Write("RELAY", relay); err != nil {
			return err
		}
		return nil
	}
}
