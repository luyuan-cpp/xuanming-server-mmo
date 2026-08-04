# Kafka Topic Retention Strategy

**Date**: 2026-04-12

## Principle

Most Kafka messages in this project are fire-and-forget control commands (player routing, session binding, kick, disconnect notifications). They are consumed immediately and have no value after consumption. Only DB write-behind tasks (and future payment-like messages) need disk persistence and longer retention.

## Environment-Specific Configuration

### Dev (docker-compose.yml)

| Setting | Value | Purpose |
|---|---|---|
| `log.retention.ms` | `60000` (60s) | Ephemeral topics expire in 1 minute |
| `log.retention.check.interval.ms` | `120000` (2 min) | Low-frequency cleanup (save CPU) |
| `log.segment.bytes` | `16777216` (16MB) | Smaller segments → faster expiry |
| `log.retention.bytes` | `134217728` (128MB) | Hard cap per partition |
| `KAFKA_HEAP_OPTS` | `-Xms128m -Xmx256m` | Minimal heap for dev |
| `KAFKA_NUM_IO_THREADS` | `1` | Single IO thread (low traffic) |
| `KAFKA_NUM_NETWORK_THREADS` | `1` | Single network thread |
| `KAFKA_BACKGROUND_THREADS` | `2` | Minimal background threads |
| Container limits | 512MB / 0.5 CPU | Hard cap |
| Storage | **tmpfs** (`/tmp/kafka-logs`) | Zero disk I/O |
| `db_task` RetentionMs | `300000` (5 min) | Service-level config |

### Production (K8s kafka.yaml)

| Setting | Value | Purpose |
|---|---|---|
| `log.retention.ms` | `300000` (5 min) | Ephemeral topics expire in 5 minutes |
| `log.retention.check.interval.ms` | `120000` (2 min) | Moderate cleanup frequency |
| `log.segment.bytes` | `33554432` (32MB) | Balanced segment size |
| `log.retention.bytes` | `536870912` (512MB) | Higher cap for production |
| `KAFKA_HEAP_OPTS` | `-Xms512m -Xmx1g` | Production heap |
| Thread counts | Default (IO=8, Net=3, Bg=10) | Full concurrency |
| Pod resources | 512Mi~1.5Gi / 200m~2 CPU | Production limits |
| Storage | Persistent volume (emptyDir) | Data survives container restart |
| `db_task` RetentionMs | `900000` (15 min, prod profile default) | More buffer for pod rescheduling |

## Profile Switch (Enterprise Style)

- Local compose now supports profile-driven env injection via `deploy/env/*.env`.
- K8s deploy now supports `-KafkaProfile dev|prod-like|prod` and can override each Kafka parameter explicitly.

### Compose Profiles

```powershell
# Dev profile (default-friendly)
docker compose --env-file deploy/env/kafka.dev.env -f deploy/docker-compose.yml up -d

# Production-like profile (local load/regression testing)
docker compose --env-file deploy/env/kafka.prod-like.env -f deploy/docker-compose.yml up -d
```

### K8s Profiles

```powershell
# Prod-like defaults for infra and generated Go configs
pwsh -File tools/scripts/k8s_deploy.ps1 -Command infra-up -KafkaProfile prod-like

# Production defaults (broker 5m, db_task 15m)
pwsh -File tools/scripts/k8s_deploy.ps1 -Command all-up -KafkaProfile prod
```

## Per-Topic Retention Matrix

| Topic Pattern | Retention | Persistent? | Rationale |
|---|---|---|---|
| `gate-{id}` | 60s (broker default) | No | Routing/bind/kick commands — consume immediately |
| `scene-{id}` | 60s (broker default) | No | Scene commands — consume immediately |
| `player-events` | 60s (broker default) | No | Disconnect/lease events — consume immediately |
| `db_task_zone_{id}` | **24h** (configurable) | **Yes** | DB write-behind pipeline — survive consumer/operator recovery; config `Kafka.RetentionMs` |
| Future: payment | **longer** (explicit) | **Yes** | Critical financial messages — set per-topic |

## Topic Initialization

`go/shared/kafkautil/topic_init.go` provides `EnsureTopics(brokers, specs)`:
- Creates topics if they don't exist (with specified partition count and retention)
- Enforces an immutable broker-side partition marker; existing topics are never expanded in place
- Updates `retention.ms` for existing topics to match spec
- Uses sarama `ClusterAdmin` API

For `db_task_zone_*`, a partition change requires an offline drain and a new
`TopicGeneration`; see [db-task-kafka-partition-contract.md](./db-task-kafka-partition-contract.md).

### Wiring

- **Login and DB services**: both call `EnsureTopics` before producer/consumer startup; any topic/count/marker mismatch is fatal. Retention defaults to 24h.
- **All other topics**: auto-created by Kafka broker on first produce, inherit 60s default retention.

### Config

```yaml
# Dev (login.yaml)
Kafka:
  RetentionMs: 86400000  # 24 hours

# Production (k8s_deploy.ps1 template, KafkaProfile=prod)
Kafka:
  RetentionMs: 86400000  # 24 hours
```

## Adding a New Persistent Topic

1. Define a `TopicSpec` in the appropriate service's startup code:
   ```go
   kafkautil.EnsureTopics(brokers, []kafkautil.TopicSpec{
       {Name: "payment-orders", Partitions: 3, RetentionMs: 86400000}, // 24 hours
   })
   ```
2. Call it before any producer/consumer uses the topic.
3. Update this document.

## Resource Limits

| Environment | Container | Heap | Limits |
|---|---|---|---|
| Dev | kafka | `-Xms128m -Xmx256m` | 512MB / 0.5 CPU |
| Dev | kafka-ui | `-Xms128m -Xmx256m` | 384MB / 0.5 CPU |
| Prod | kafka | `-Xms512m -Xmx1g` | 1.5Gi / 2 CPU |
