// data_stress is a Kafka load generator that mimics the cpp Scene's
// SavePlayerToRedis pipeline (cpp/libs/services/scene/player/system/player_lifecycle.cpp).
//
// It produces N monotonically-versioned `player_database` writes per player to
// the same Kafka topic the production consumer reads from.
//
// 分区契约:key 用 kafkautil.DBTaskKey(玩家 id 的十进制),分区器用 kafkautil.NewDBTaskPartitioner
// (CRC-32(key) mod N),与生产 C++ scene 写存盘(librdkafka consistent_random)是同一个函数,
// 所以同一玩家的压测写与生产 scene 的写落在同一分区(docs/design/db-task-kafka-partition-contract.md)。
//
// SEQ ENCODING
//
//   Each write stamps `player_database.stress_test_probe` with:
//     * test_seq: monotonic per-player sequence (1..writesN)
//     * test_sig: SHA-256(player_id || msg_type || test_seq) truncated to
//                 16 bytes — lets the verifier detect torn / mis-routed /
//                 re-assembled payloads. See go/db/internal/stresstest for
//                 the canonical definition shared with the verifier.
//
// EXPECTED-STATE MAP
//
//   Before exit, the driver writes the highest seq it produced for each
//   player to Redis under key:
//
//       verify:expected:<msg_type>:<player_id> = <max_seq>
//
//   The verifier compares this against MySQL + Redis cache reads.
//
// USAGE EXAMPLE
//
//   go run ./cmd/data_stress \
//     -kafka 127.0.0.1:9092 -zone 1 \
//     -redis 127.0.0.1:6379 \
//     -players 1000 -writes 100 -concurrency 64
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	db_config "db/internal/config"
	"db/internal/stresstest"
	dbpb "proto/common/database"
	taskpb "proto/db"
	"shared/generated/persistschema"
	"shared/kafkautil"

	"github.com/IBM/sarama"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// expectedKey returns the Redis key the verifier reads for a given
// (msg_type, player_id) probe.
func expectedKey(msgType string, playerID uint64) string {
	return fmt.Sprintf("verify:expected:%s:%d", msgType, playerID)
}

func main() {
	var (
		kafkaAddrs    = flag.String("kafka", "127.0.0.1:9092", "comma-separated kafka brokers")
		redisAddr     = flag.String("redis", "127.0.0.1:6379", "redis addr (single node)")
		redisPassword = flag.String("redis-password", "", "redis password (optional)")
		redisDB       = flag.Int("redis-db", 0, "redis DB number")
		zoneID        = flag.Uint("zone", 1, "zone id (decides Kafka topic db_task_zone_<zone>)")
		playersN      = flag.Int("players", 100, "number of distinct player_ids")
		playerStart   = flag.Uint64("player-start", 1_000_000, "starting player_id")
		writesN       = flag.Int("writes", 50, "writes per player (the converged seq)")
		concurrency   = flag.Int("concurrency", 32, "max concurrent producer goroutines")
		burstSleepMs  = flag.Int("burst-sleep-ms", 0, "optional sleep between writes per player to avoid filling broker buffers")
		flushTimeout  = flag.Duration("flush-timeout", 30*time.Second, "max time to wait for all sends to complete after producer Close()")
		dryRun        = flag.Bool("dry-run", false, "skip Kafka send, only print the plan")
	)
	flag.Parse()

	if *playersN <= 0 || *writesN <= 0 {
		log.Fatalf("-players and -writes must be > 0")
	}
	topic := db_config.DbTaskTopic(uint32(*zoneID))

	rc := redis.NewClient(&redis.Options{
		Addr:     *redisAddr,
		Password: *redisPassword,
		DB:       *redisDB,
	})
	defer func() { _ = rc.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rc.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	var producer sarama.SyncProducer
	if !*dryRun {
		cfg := sarama.NewConfig()
		cfg.Version = sarama.V3_5_0_0
		cfg.Producer.Return.Successes = true
		cfg.Producer.Return.Errors = true
		cfg.Producer.RequiredAcks = sarama.WaitForAll
		cfg.Producer.Idempotent = true
		cfg.Producer.Retry.Max = 5
		cfg.Producer.Retry.Backoff = 200 * time.Millisecond
		// Idempotent producer requires this:
		cfg.Net.MaxOpenRequests = 1
		// 与 C++ scene 写存盘用的是同一个分区函数。不设就是 sarama 默认的 FNV,同一玩家会落到别的分区,
		// go/db 比较写游标时会把它判为 orderingIncomparable 送进死信。
		cfg.Producer.Partitioner = kafkautil.NewDBTaskPartitioner
		cfg.ClientID = "data_stress"

		brokers := splitCSV(*kafkaAddrs)
		var err error
		producer, err = sarama.NewSyncProducer(brokers, cfg)
		if err != nil {
			log.Fatalf("kafka producer: %v", err)
		}
		defer func() {
			closeCtx, c := context.WithTimeout(context.Background(), *flushTimeout)
			defer c()
			done := make(chan struct{})
			go func() { _ = producer.Close(); close(done) }()
			select {
			case <-done:
			case <-closeCtx.Done():
				log.Printf("warning: producer close timed out after %s", *flushTimeout)
			}
		}()
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("shutdown signal received, cancelling ...")
		cancel()
	}()

	totalWrites := int64(*playersN) * int64(*writesN)
	log.Printf("data_stress: zone=%d topic=%s players=%d writes/player=%d total=%d concurrency=%d dry_run=%v",
		*zoneID, topic, *playersN, *writesN, totalWrites, *concurrency, *dryRun)

	startTS := time.Now()
	var sent atomic.Int64
	var failed atomic.Int64

	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup

	for i := 0; i < *playersN; i++ {
		select {
		case <-ctx.Done():
			break
		case sem <- struct{}{}:
		}

		playerID := *playerStart + uint64(i)
		wg.Add(1)
		go func(playerID uint64) {
			defer wg.Done()
			defer func() { <-sem }()
			driveOnePlayer(ctx, producer, topic, rc, playerID, *writesN, *burstSleepMs, &sent, &failed, *dryRun)
		}(playerID)
	}
	wg.Wait()

	elapsed := time.Since(startTS)
	log.Printf("data_stress done: sent=%d failed=%d elapsed=%s rate=%.0f writes/s",
		sent.Load(), failed.Load(), elapsed, float64(sent.Load())/elapsed.Seconds())
}

// driveOnePlayer produces writes 1..N for one player_id and writes the
// expected seq to Redis. Each write embeds seq into the
// uint64_pb_component.registration_timestamp field so the verifier can detect
// convergence to seq=N.
func driveOnePlayer(
	ctx context.Context,
	producer sarama.SyncProducer,
	topic string,
	rc *redis.Client,
	playerID uint64,
	writesN int,
	burstSleepMs int,
	sent *atomic.Int64,
	failed *atomic.Int64,
	dryRun bool,
) {
	playerKey := kafkautil.DBTaskKey(playerID)

	for seq := uint64(1); seq <= uint64(writesN); seq++ {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgType := string((&dbpb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName())
		// 盖本二进制的持久 schema 世代:引导期关闭后未盖戳的写会被 go/db 送进死信
		// (docs/design/mixed-version-persist-field-protection.md §6.2 C0)。
		row := &dbpb.PlayerDatabase{
			PlayerId:         playerID,
			StressTestProbe:  stresstest.MakeProbe(playerID, msgType, seq),
			PersistSchemaGen: persistschema.Gen,
		}
		body, err := proto.Marshal(row)
		if err != nil {
			failed.Add(1)
			log.Printf("marshal player_database (player=%d seq=%d): %v", playerID, seq, err)
			continue
		}
		dbTask := &taskpb.DBTask{
			Key:     playerID,
			Op:      taskpb.DBOp_DB_OP_WRITE,
			MsgType: msgType,
			Body:    body,
			TaskId:  fmt.Sprintf("stress:%d:%s:%d:%d", playerID, msgType, seq, time.Now().UnixNano()),
		}
		taskBytes, err := proto.Marshal(dbTask)
		if err != nil {
			failed.Add(1)
			log.Printf("marshal DBTask (player=%d seq=%d): %v", playerID, seq, err)
			continue
		}

		if dryRun {
			sent.Add(1)
			continue
		}

		_, _, err = producer.SendMessage(&sarama.ProducerMessage{
			Topic: topic,
			Key:   sarama.StringEncoder(playerKey),
			Value: sarama.ByteEncoder(taskBytes),
		})
		if err != nil {
			failed.Add(1)
			log.Printf("kafka send (player=%d seq=%d): %v", playerID, seq, err)
			continue
		}
		sent.Add(1)

		if burstSleepMs > 0 {
			time.Sleep(time.Duration(burstSleepMs) * time.Millisecond)
		}
	}

	// Record expected converged seq for this (msg_type, player) so the
	// verifier knows what to look for.
	msgType := string((&dbpb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName())
	if err := rc.Set(ctx, expectedKey(msgType, playerID), uint64(writesN), 24*time.Hour).Err(); err != nil {
		log.Printf("write expected key (player=%d): %v", playerID, err)
	}

	// Also publish a global enrollment set so the verifier can iterate
	// without needing to know the player id range.
	if err := rc.SAdd(ctx, "verify:enrolled:"+msgType, playerID).Err(); err != nil {
		log.Printf("publish enrolled (player=%d): %v", playerID, err)
	}
}

func splitCSV(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
