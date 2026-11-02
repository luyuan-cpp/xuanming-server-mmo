# Infrastructure Reconnection Overview

Updated: 2026-04-05

## C++ Nodes (gate / scene / centre)

| Middleware | Library | Auto-Reconnect | Notes |
|-----------|---------|---------------|-------|
| Redis | hiredis (muduo wrapper) | Yes (custom) | `RedisManager::Connect()` 先装好断线回调再建连;断线或握手失败后按重连间隔(默认 3s)重建握手连接,重建任务排到本轮 pending 阶段执行,并带代次复核(`SetupReconnect` 已退役)。连接一律经 `hiredis::Hiredis::Create` 创建、由 `shared_ptr` 持有;`MessageAsyncClient` 持有的是 `shared_ptr<Hiredis>&`,即 `GetZoneRedis()` 那个槽位的引用,所以能看到重连后的新实例。生命周期细节见 `redis-async-client-retry-fix.md`。 |
| Kafka | librdkafka | Yes (library) | Consumer poll auto-recovers. Producer send failures only logged, no retry/dead-letter. |
| MySQL | N/A | N/A | C++ nodes don't connect to MySQL; handled by Go `db` service. |
| etcd | gRPC stubs | Partial | gRPC channel auto-reconnects. Lease keepalive failure = `LOG_FATAL`. Watch stream cancel now triggers `ScheduleWatchReconnect()` (2s delay, re-establishes all watches with tracked revision). |

### Key files
- Redis reconnect: `cpp/libs/engine/thread_context/redis_manager.h/.cpp`
- Redis async client guard: `cpp/libs/engine/infra/storage/redis_client/redis_client.h` (`MessageAsyncClient`)
- Node Redis init + reconnect setup: `cpp/libs/engine/core/node/system/node/node.cpp`
- etcd watch reconnect: `cpp/libs/engine/core/node/system/etcd/etcd_service.cpp` (`OnWatchResponse`, `ScheduleWatchReconnect`)
- Kafka: `cpp/libs/engine/infra/messaging/kafka/kafka_consumer.cpp`, `kafka_producer.cpp`

## Go Services (login / db / scene_manager / player_locator / data_service)

| Middleware | Library | Auto-Reconnect | Notes |
|-----------|---------|---------------|-------|
| Redis | go-redis v9 / go-zero redis | Yes (library) | Connection pool with auto-reconnect. No custom retry wrappers. |
| Kafka (login) | IBM/sarama | Yes (library + custom) | db_task 主生产者按分区契约 CRC-32 mod N 分区(`kafkautil.NewDBTaskPartitioner`,与 C++ librdkafka `consistent_random` 同一个函数,见 `docs/design/db-task-kafka-partition-contract.md`);分区漂移时永久 fence;重连和重试靠 sarama(`Retry.Max`);控制面命令走 plainProducer 的显式分区。(2026-10-16 订正:原文写的 Partition unavailability tracking / consistent hash remap 从来没生效,已删除;已落码,未编译、未测试,待 Codex 验证) |
| Kafka (db) | IBM/sarama | Yes (library + custom) | ConsumerGroup auto-reconnect loop, Redis-backed retry queue, dead letter queue. |
| Kafka (scene_mgr, player_loc) | segmentio/kafka-go | Yes (library) | `kafka.Writer` handles leader discovery. Fire-and-forget. |
| MySQL | go-sql-driver + database/sql | Yes (pool) | `SetConnMaxLifetime(5min)` prevents stale connections after MySQL restart. |
| etcd | etcd/client/v3 + go-zero discov | Yes (gRPC + custom) | Login `NodeRegistry.KeepAlive` now re-grants lease + re-registers all keys on channel close with exponential backoff. go-zero discov handles reconnection automatically. |

### Key files
- Redis: each service's `internal/svc/servicecontext.go`
- Kafka (login producer): `go/login/internal/kafka/key_ordered_producer.go`
- Kafka (db consumer): `go/db/internal/kafka/key_ordered_consumer.go`
- MySQL lifetime: `go/db/internal/logic/pkg/proto_sql/db.go`
- etcd lease re-registration: `go/login/internal/logic/pkg/etcd/registry.go`
- etcd watcher: `go/login/internal/logic/pkg/node/watcher.go`

## Remaining Risks
- C++ Kafka producer: send failures only logged, no retry or dead-letter mechanism.
- C++ etcd lease keepalive TTL=0: still triggers `LOG_FATAL` (by design — identity protection).
- Go etcd NodeWatcher: events during watch stream gap are lost (no snapshot reconciliation).
- Go db service: startup `Ping()` failure only logged, doesn't panic — can start with unreachable Redis.
