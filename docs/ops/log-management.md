# Log Management

## Directory Layout (Local Development)

Runtime artifacts live under `run/`, separate from build outputs in `bin/`:

| Path | Purpose |
|------|---------|
| `run/logs/go_services/` | Go service stdout/stderr logs |
| `run/logs/cpp_nodes/`   | C++ node stdout/stderr logs |
| `run/logs/sa_token.log` | SA-Token (Java) dev server log |
| `run/logs/robot/`       | Robot load-test client logs |
| `run/pids/*.pid.json`   | PID files for tracked processes |
| `run/scratch/`          | Transient debug dumps and test outputs |

`bin/` remains the C++ runtime working directory (hosts `gate.exe`, `scene.exe`, `bin/etc/`, `bin/nodes/`, `bin/script/`, plus the built Go binaries in `bin/go_services/`).

Use `dev.bat` to manage logs:

```bash
dev clean-logs    # Delete all log files under run\logs
dev logs          # Interactive menu to pick a process to tail
dev logs <name>   # Tail a specific process (e.g. dev logs login)
dev logs all      # Dump last 60 lines of every process
dev obs           # Grafana + Loki + Alloy: 三种语言的日志合在一个网页里看 (http://localhost:3000)
dev obs-down      # 停掉上面的观测台
```

### 本地 Grafana / Loki 日志台(2026-09-13)

`dev obs` 起 `deploy/docker-compose.observability.yml`:Alloy 盯着 `run/logs/go_services`、`run/logs/cpp_nodes`、
`run/logs/java`、`run/logs/game-launcher/<时间戳>/gateway.*.log`(start_game.ps1 起的网关)和 `run/logs/sa_token.log`
下的文件,按语言各自的格式解析(Go = go-zero JSON,C++ = muduo 文本,Java = Spring Boot JSON/文本),
打上 `job / service / zone / instance / level` 标签送进 Loki,Grafana 预置了数据源和看板「游戏服务日志总览」。
服务本身不用改代码(Java 只加了一段 `logging.structured` 配置)。
详细的接入方式、标签约定、常用查询和排障见 [grafana-loki-local-logs.md](grafana-loki-local-logs.md)。

## Production (K8s)

### Collection Pipeline

```
Container stdout/stderr
    → Fluentd / Filebeat (DaemonSet)
    → Elasticsearch / Loki / ClickHouse
    → Kibana / Grafana (query & alert)
```

- Applications write structured JSON logs to stdout.
- Every log entry should include `trace_id` for cross-service correlation.
- Log level defaults to WARN in production; temporarily switch to DEBUG for troubleshooting.

### Local Rotation (Prevent Disk Full)

Container runtime handles log rotation automatically:

| Platform | Config | Example |
|----------|--------|---------|
| Docker | `--log-opt` | `max-size=100m`, `max-file=5` |
| K8s | kubelet | `containerLogMaxSize: 100Mi`, `containerLogMaxFiles: 5` |
| Bare metal | logrotate | Daily/size-based rotation + gzip compression |

### Tiered Storage

| Tier | Retention | Storage | Use Case |
|------|-----------|---------|----------|
| Hot | 7–30 days | Elasticsearch / Loki | Real-time query, alerting |
| Warm | 30–90 days | S3 / OSS (compressed) | On-demand investigation |
| Cold | 90 days – years | Glacier / Archive storage | Audit, compliance |

### Key Practices

1. **Structured logging** — JSON format with consistent fields (`timestamp`, `level`, `trace_id`, `service`, `message`).
2. **Sensitive data masking** — Never log passwords, tokens, or PII in plaintext.
3. **Disk monitoring** — Alert when node disk usage exceeds 80%.
4. **Log level control** — WARN by default in production. Enable DEBUG temporarily via config reload or env var, not code change.
5. **Retention policy** — Define per-environment retention; automate cleanup with lifecycle policies.

### This Project

- K8s deployments: logs go to stdout, collected by Fluentd into Elasticsearch. No application-level rotation needed.
- Docker Compose (local infra): use Docker's built-in log rotation via `deploy/docker-compose.yml` logging config.
- C++ nodes:muduo 日志经 `Node::AsyncOutput` 写 `bin/logs/cpp_nodes/` 滚动文件;**写控制台那一路只在 Windows 生效**
  (`#ifdef WIN32`),Linux / k8s 容器里 muduo 的业务日志**不进 stdout**,只在 `/app/bin/logs/cpp_nodes/*.log`。
  k8s 侧因此**不走 stdout**:`k8s_deploy.ps1` 默认给每个 C++ Pod 加一个 Alloy sidecar,与业务容器共享 `node-logs` 卷只读读这些文件,
  直接送 infra namespace 里的 Loki(`manifests/infra/loki.yaml`);容器 stdout 上剩下的 gRPC / librdkafka stderr 仍需另配 DaemonSet。
  细节见 [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §6。
  本地(Windows)由 `cpp_nodes.ps1` 把控制台 stdout/stderr 分别重定向到 `run/logs/cpp_nodes/<实例>.stdout.log / .stderr.log`;
  stdout 是 4096 字节整块缓冲,最新的几十条日志会滞后,排查以 `bin/logs/cpp_nodes/` 下的 muduo 文件为准。
  stderr 里是 gRPC/abseil 和 librdkafka 自己的日志。
- Go services:go-zero `logx` 控制台模式下 info/debug/stat 写 stdout,error/slow 等告警级别写 stderr;本地由 `go_services.ps1` 分别重定向到
  `run/logs/go_services/<实例>.stdout.log / .stderr.log`。
