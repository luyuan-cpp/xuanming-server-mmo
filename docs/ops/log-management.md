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
2026-11-11 起三种语言的 `trace_id` / `span_id` / `player_id` 进了结构化元数据,看板顶部多了这两个过滤框:
一条 trace_id 能查到一次请求经过的各服务的行(网关的登录请求从 Java 网关到 login;进游戏后客户端的一条消息从 gate 到 scene),
按 player_id 能串起一个玩家在各服务的行(设计见 [distributed-tracing.md](../design/distributed-tracing.md))。

## Production (K8s)

> 本节的 Collection Pipeline / Local Rotation / Tiered Storage / Key Practices 是**通用参考口径**,
> 写的是"生产上应该长成什么样",不是本仓已经部署的样子 ——
> `deploy/k8s/manifests/infra/loki.yaml` 的头部注释和 [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §6
> 引的正是这几段目标形态,所以这里**只在个别条目下加了指向本仓实际情况的注记**
> (见 Collection Pipeline 的前两条),结论性内容保留原样。
> 本仓**当前实际**跑的是什么,只看本节末尾的 [This Project](#this-project)。

### Collection Pipeline

```
Container stdout/stderr
    → Fluentd / Filebeat (DaemonSet)
    → Elasticsearch / Loki / ClickHouse
    → Kibana / Grafana (query & alert)
```

- Applications write structured JSON logs to stdout.
  **这一条对 C++ 节点不成立**:Linux 下 muduo 的业务日志根本不进 stdout(原因见 This Project),
  所以本仓的 C++ 业务日志走的是"共享卷直接读文件",而不是上面这条管线。Go / Java 与 C++ 容器自己的 stdout / stderr
  走的就是这条,DaemonSet 用的是 Alloy(2026-11-12,见 This Project)。
- Fluentd / Filebeat 与 Elasticsearch / ClickHouse / Kibana:**本项目未部署**,这里只是通用参考;
  本仓落地的是 Alloy + Loki + Grafana。
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

本仓 k8s 上**当前实际**的日志形态(2026-09-19;2026-11-12 补上容器 stdout / stderr 与 trace 字段),与上面的通用参考对照着看:

- **C++ 节点的业务日志 = 共享卷 + Alloy sidecar → 集群内 Loki(已落码)**。不走 stdout,DaemonSet 不参与这一路:
  每个 C++ Pod 带一个名为 `log-sidecar` 的 **k8s 原生 sidecar**(写在 `initContainers` 里,带 `restartPolicy: Always`),
  与业务容器共享 `node-logs` 卷(sidecar 侧只读),直接读 `/app/bin/logs/cpp_nodes/*.log` 推给 infra namespace 的 Loki
  (`deploy/k8s/manifests/infra/loki.yaml`)。生成逻辑集中在 `tools/scripts/k8s_deploy.ps1` 的 `New-CppLogSidecar*`,
  默认开启,`-NoCppLogSidecar` / `-CppLogSidecarImage` / `-LokiPushUrl` 三个开关已从 `dev_tools.ps1`、`k8s_image.ps1` 透传。
  用原生 sidecar 而不是普通容器,是因为:① Agones 给 GameServer Pod 写死 `restartPolicy: Never`,普通容器形态下
  sidecar OOM 后永不重启,而 Agones 的健康检查只看游戏容器、不会把 GameServer 置 Unhealthy,日志会静默断流;
  ② Pod 终止时 kubelet 会等业务容器完全退出后再停原生 sidecar,scene 那 60 秒 drain 期写的日志才追得完
  (普通容器是与业务容器同时收 SIGTERM)。细节见 [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §6。
- **C++ 节点的日志级别(2026-10-30 补)**:`bin/etc/base_deploy_config.yaml` 的 `LogLevel` 直接按 muduo 枚举解释
  (0=TRACE 1=DEBUG 2=INFO 3=WARN 4=ERROR 5=FATAL),必填,缺键或越界拒启。K8s 的 `node-config` / `battle-node-config`
  由 `k8s_deploy.ps1` 从这里镜像,默认 2 = INFO(2026-10-22 前写死 1 = DEBUG)。只在启动时读一次:上面 Key Practices 第 4 条说的
  config reload 与环境变量对 C++ 都不适用(`MUDUO_LOG_DEBUG` 会被配置覆盖)。临时看 DEBUG 的做法、预检拦截与重启条件见
  [演练手册](cross-zone-failure-test-runbook.md) §3.1 第 7 条。
- **集群内 Loki 的两种形态(2026-11-10 落码,未编译、未在集群上验证;[去单点总纲](../design/no-single-node-horizontal-scaling-20261001.md) §10)**:
  `k8s_deploy.ps1 -LokiReplicas` 选,只认 1 和 3,缺省按档位(非 dev 或非 custom 取 3)。
  - 1:`infra/loki.yaml`,单副本加本地盘,只给 dev、kind。
  - 3:`infra/loki-ha.yaml`,单体模式 3 副本(StatefulSet,`-target=all`),memberlist 组环、`replication_factor: 3`,
    日志块与 TSDB 索引在共享 S3,每个 Pod 的 WAL 在自己的 PVC,PDB `minAvailable: 2`。Service 名和推送地址不变(`loki.<infra>:3100`),sidecar 和 Grafana 不用改。
  - 对象存储:生产只接外部 S3 兼容存储(`-LokiS3Endpoint host:port`、`-LokiS3Bucket`、`-LokiS3Region`、`-LokiS3ForcePathStyle`;
    访问密钥经环境变量 `MMORPG_LOKI_S3_ACCESS_KEY_ID`、`MMORPG_LOKI_S3_SECRET_ACCESS_KEY` 注入成 Secret)。云上用云厂商的对象存储,自建机房用平台层的 Rook-Ceph RGW。
    dev、kind 上验证 3 副本用 `-LokiS3DevFixture`(单进程 SeaweedFS,数据在 emptyDir,不进 staging、prod)。
  - 1 → 3 要 `-ConfirmLokiTopologyChange`,旧 PVC 里的日志不迁移;3 → 1 拒绝。细节见 `deploy/k8s/README.md`「Loki:单副本与单体模式 3 副本」。
- **容器 stdout / stderr = 读 `/var/log/pods` 的 Alloy DaemonSet → 同一个 Loki(2026-11-12 已落码,未运行)**:
  infra namespace 里的 `pod-log-collector`,每个节点一个,采 Go 服务与 Java 网关的全部日志(清单标了 `tier: go-svc` / `java-svc` 的 Pod),
  以及 C++ 节点容器自己的 stdout / stderr(gRPC / abseil、librdkafka、未捕获异常、gate 的启动行)。解析规则照搬本机 `config.alloy`,
  时间用 CRI 时间戳;标签从 Pod 元数据取(`service` = `app` 标签,`zone` 取自命名空间)。与 C++ sidecar 同开同关(`-NoCppLogSidecar`)、
  推同一个地址,只随 `infra-up` 部署。细节、权限(root 只读 hostPath,PSA baseline 会拒)与排障见
  [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §6.11。
- **这套方案采不到的东西**(别以为都进了 Loki):infra 里 etcd / Kafka / Redis / MySQL / Loki 自己的日志;没有文本的崩溃
  (段错误、被信号杀掉);DaemonSet 没部署的集群里上面那一路的全部内容。Loki 里的 `level="fatal"` 覆盖 `LOG_FATAL` 与未捕获异常,
  查不到仍不等于没崩,崩没崩看 Pod 的 `Last State`。
- **链路关联字段**:本机(2026-11-11)与集群(2026-11-12,未运行)都把 `trace_id` / `span_id` / `player_id` 提成结构化元数据,
  只在那一行真带了值时才写,绝不做标签。集群里 C++ 由 sidecar 的两条行尾正则取,Go / Java 由 DaemonSet 照本机的规则取
  ([distributed-tracing.md](../design/distributed-tracing.md) §9.2)。查询:`{env="k8s"} | trace_id="<32 位>"`、`{env="k8s"} | player_id="<id>"`。
  sidecar 规则这一改会让下一次 `zone-up` 滚动重启 C++ 节点。
- **未部署**:Fluentd / Filebeat / Elasticsearch / Kibana 本项目一个都没有。
- **本地文件只留最近一小段**:业务容器的启动命令里带一个每 5 分钟跑一次的清理循环,把
  `/app/bin/logs/cpp_nodes/*.log` 按时间排序后只留最近 8 个(≈64MiB)—— muduo 每 8MiB 滚一个新文件且**从不删旧文件**。
  `node-logs` 另加 `emptyDir.sizeLimit: 2Gi` 作为远端兜底。要查更早的日志只能去 Loki。
  兜底口径别记错:本机 kind 的 `evictionHard` 是 nodefs/imagefs 0%,`DiskPressure` **永远不会置位**,
  所以盘写满的表现不是"节点级驱逐挑谁",而是 MySQL / Kafka / etcd / Loki 一起 ENOSPC 写失败、存档有损坏风险且不会自愈;
  `sizeLimit` 走的是 kubelet 的 `emptyDirLimitEviction`,与 `DiskPressure` 无关,只驱逐越界的那个 Pod。
  正常情况下清理循环让用量停在几十 MiB,碰不到这道闸。
- **改 sidecar 配置 = 滚动重启 C++ 节点**。pod 模板上带一个 `mmorpg.io/cpp-log-sidecar-config-hash` 注解:
  只改 ConfigMap 不会让跑着的 Alloy 重读配置,而 pod 模板逐字节不变时 `kubectl apply` 是 no-op,所以把配置哈希打进模板。
  代价是改 `-LokiPushUrl` 或改解析规则后,下次 zone-up 会滚动重启 C++ 节点(Agones 侧是 Fleet RollingUpdate 换 GameServer),**要挑时机**。
- **验证到哪一步了**(2026-09-19):本机 kind 集群的临时 namespace 里端到端跑通过一次 ——
  `zone-up -DryRun` 真实产出的 sidecar ConfigMap 与 gate Deployment 原样 apply(只把业务容器镜像换成
  `ubuntu:24.04`、启动命令尾部换成按 muduo 格式写日志的循环),Loki 用本仓 `infra/loki.yaml` 原样部署在同一 namespace:
  Pod `2/2 Running`、info / warn / error 三类条目都查得到、muduo 行时间戳按 +08:00 正确解析、
  `Dropped log messages at ...` 独立成条且 `level=warn`、流标签里确认没有 `filename` 与 `pid`、
  清理循环把 12 个 `.log` 降到 8 个且 sidecar 的 `loki_source_file_files_active_total` 同步降到 8。
  **仍未验证**:没有用真实 C++ 镜像跑过(上面是 `ubuntu:24.04` 假节点写 muduo 格式行);
  **没有在真实 zone / infra namespace 上 apply 过**(只在临时 namespace);
  Agones Fleet 未经真实 Agones 准入校验(本机 kind 没装 Agones controller);
  `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 的契约用例尚未运行。
  完整验证记录见 [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §7。

本地 / Docker Compose 形态(与上面的 k8s 形态无关):

- Docker Compose (local infra): use Docker's built-in log rotation via `deploy/docker-compose.yml` logging config.
- C++ nodes(本地 Windows):muduo 日志经 `Node::AsyncOutput` 写 `bin/logs/cpp_nodes/` 滚动文件;**写控制台那一路只在 Windows 生效**
  (`#ifdef WIN32`),这正是上面"Linux / k8s 容器里业务日志不进 stdout"的根因。
  本地(Windows)由 `cpp_nodes.ps1` 把控制台 stdout/stderr 分别重定向到 `run/logs/cpp_nodes/<实例>.stdout.log / .stderr.log`;
  stdout 是 4096 字节整块缓冲,最新的几十条日志会滞后,排查以 `bin/logs/cpp_nodes/` 下的 muduo 文件为准。
  stderr 里是 gRPC/abseil 和 librdkafka 自己的日志。
- Go services:go-zero `logx` 控制台模式下 info/debug/stat 写 stdout,error/slow 等告警级别写 stderr;本地由 `go_services.ps1` 分别重定向到
  `run/logs/go_services/<实例>.stdout.log / .stderr.log`。
