# 本地日志观测台:Grafana + Loki + Alloy(C++ / Go / Java 三语言统一接入)

> **状态:** 2026-09-13 落地并在本机验证通过。适用于 Windows 本机开发环境;
> k8s 部署侧的采集方案见文末「生产 / k8s」。

---

## 0. 一分钟上手

```bat
dev.bat obs          :: 起 Grafana + Loki + Alloy(需要 Docker Desktop 在跑)
start http://localhost:3000
```

打开后左侧「仪表板」→ 文件夹「玄冥本地」→「游戏服务日志总览(C++ / Go / Java)」。
顶部可以按 **来源(语言)/ 服务 / zone / 级别 / 关键字** 过滤,下方就是三种语言合在一起的日志流。
也可以直接用「探索(Explore)」或「Logs Drilldown」自由查。

```bat
dev.bat obs-down     :: 停掉(Loki 里已收的日志保留在 docker 卷里)
```

---

## 1. 这套东西是怎么工作的

打个比方:三种语言的服务各自往本机文件里写日记(它们本来就在这么做,**一行代码都没改**),
Alloy 是一个盯着这些文件的抄写员,每来一行就贴上标签(哪种语言、哪个服务、哪个 zone、什么级别)
送进 Loki 这个档案库;Grafana 是档案库的前台,负责查和画图。

```
run/logs/go_services/*.stdout.log   (go-zero logx, JSON)      ─┐
run/logs/cpp_nodes/*.stdout.log     (muduo, 文本+颜色码)        ├─▶ Alloy(采集+解析+打标签)─▶ Loki(存 7 天)─▶ Grafana(查/看)
run/logs/java/*.log 等              (Spring Boot, JSON 或文本)  ─┘
```

| 组件 | 容器名 | 端口 | 配置文件 |
|---|---|---|---|
| Loki 3.5 | `xm-loki` | 3100 | `deploy/observability/loki/loki.yaml` |
| Alloy | `xm-alloy` | 12345(调试页) | `deploy/observability/alloy/config.alloy` |
| Grafana | `xm-grafana` | 3000 | `deploy/observability/grafana/provisioning/*`(数据源 + 看板自动导入) |

compose 文件:`deploy/docker-compose.observability.yml`,和 `deploy/docker-compose.yml`(Kafka/Redis/MySQL/etcd)互不依赖。
本地开发 Grafana 免登录(匿名即管理员),**不要原样搬到公网**。

---

## 2. 三种语言各自的接入细节

### 2.1 Go(go-zero `logx`)

- **不用改代码。** `logx` 默认就是一行一条 JSON 打到 stdout,`tools/scripts/go_services.ps1` 已经把 stdout/stderr
  重定向到 `run/logs/go_services/<实例>.stdout.log / .stderr.log`。
- 行长这样:`{"@timestamp":"2026-09-13T03:07:37.138-04:00","caller":"login/login.go:67","content":"...","level":"info"}`
- Alloy 解析:`@timestamp` 当日志时间、`level` 当标签、`caller` 放进结构化元数据;
  `logx.Errorw(..., logx.Field("method", ...))` 这类附加字段仍在原始 JSON 行里,查询时用 `| json` 就能当字段过滤。
- 文件名 → 标签:`z2_login.stdout.log` → `zone=2, service=login, instance=z2_login, stream=stdout`;
  `data_service_1.stdout.log` → `service=data_service, instance=data_service_1`(没有 `zN_` 前缀就没有 `zone` 标签)。
- `level=stat` 是 go-zero 每分钟自己打的 CPU/内存统计,量很大;不想看就在级别过滤里去掉,
  或在服务 yaml 里加 `Log: { Stat: false }` 关掉。

### 2.2 C++(muduo `LOG_INFO` 等)

- **不用改代码。** muduo 的日志经 `Node::AsyncOutput` 同时写两处:自己滚动的 `bin/logs/cpp_nodes/<节点>.<UTC时间>.<主机>.<pid>.log`,
  以及控制台(`LogToConsole`,带颜色码);`tools/scripts/cpp_nodes.ps1` 把控制台重定向到了 `run/logs/cpp_nodes/<实例>.stdout.log`。
  Alloy 采的是后者,因为文件名里有 `z1_gate` 这种实例名。
- 行长这样:`20260913 15:24:58.979463 36460 INFO  Node created ... - E:\...\node.cpp:366`
  - 时间是 **东八区**(`Node::SetupTimeZone` 固定 +8,和系统时区无关),Alloy 按 `Asia/Shanghai` 解析,所以和 Go/Java 的时间能对齐。
  - `36460` 是线程 id(标签外的结构化元数据 `tid`)。
  - `Build Info` 这类多行消息只有第一行带时间戳,Alloy 会把后续行并回同一条。
- 级别映射:`TRACE/DEBUG/INFO/WARN/ERROR/FATAL/SYSERR` → 小写标签 `level`。
- 如果某次节点不是用 `cpp_nodes.ps1` 起的(没有 stdout 重定向),`config.alloy` 文末有一段注释掉的
  `bin/logs/cpp_nodes/*.log` 采集配置,取消注释即可(按 pid 区分实例)。

### 2.3 Java(Spring Boot:gateway_node / config_node)

- **改了一处配置,没改代码:** `application.yaml` 加了
  ```yaml
  logging:
    structured:
      format:
        console: logstash
  ```
  Spring Boot 3.4 自带的结构化日志,控制台从文本变成一行一条 JSON:
  `{"@timestamp":"...","level":"INFO","logger_name":"c.game.gateway.X","thread_name":"main","message":"...","stack_trace":"..."}`。
  启动脚本(`start_game.ps1` / `dev_mprocs_proc.ps1`)把 stdout 重定向到 `run/logs/java/` 或 `run/logs/game-launcher/<时间戳>/gateway.stdout.log`。
- **旧 jar 没重打包也能用:** Alloy 同时认 Spring Boot 默认的文本格式
  (`2026-09-05T02:13:53.989-04:00  INFO 26768 --- [gateway-node] [ main] c.game.X : 消息`),堆栈续行自动并回上一条。
- 想临时恢复文本:启动时加参数 `--logging.structured.format.console=`(空值),或环境变量 `LOGGING_STRUCTURED_FORMAT_CONSOLE=`。
- `java/springboot_satoken_auth_starter`(SA-Token demo,`run/logs/sa_token.log`)也在采集范围,没有改它的配置。

---

## 3. 标签约定(三种语言一致)

| 标签 | 取值 | 来源 |
|---|---|---|
| `job` | `go_services` / `cpp_nodes` / `java` | 固定 |
| `lang` | `go` / `cpp` / `java` | 固定 |
| `service` | `login` `db` `scene_manager` `player_locator` `data_service` `match` `client_rpc_router` / `gate` `scene` `battle` / `gateway` `config_node` `sa_token` | 文件名 |
| `zone` | `1` `2` … | 文件名 `zN_` 前缀;没有前缀就没有这个标签 |
| `instance` | `z1_gate` `data_service_1` … | 文件名 |
| `stream` | `stdout` / `stderr` | 文件名(Java 没有) |
| `level` | `trace` `debug` `info` `warn` `error` `fatal` `syserr` `stat` `slow` … | 行内解析,统一小写 |
| `filename` | 容器内路径,如 `/host/run/logs/go_services/login.stdout.log` | Alloy 自动加 |
| `env` | `local` | Alloy `external_labels` |

结构化元数据(不是标签,但能在行详情里看到、能用 `| caller="..."` 过滤):Go `caller`、C++ `tid`、Java `logger` / `thread`。

标签故意只用这几个低基数的字段;**不要**把 player_id / session_id 之类做成标签(Loki 会被撑爆),
查这类东西用行过滤:`{job=~".+"} |= "player_id=12345"`。

---

## 4. 常用查询(Explore 里直接贴)

```logql
# 三种语言合流,只看错误
{job=~"go_services|cpp_nodes|java", level=~"error|fatal|syserr"}

# zone 1 的 gate + login 一起看(跨语言排一次登录)
{zone="1", service=~"gate|login"}

# 某个玩家 id 在所有服务里的足迹
{job=~".+"} |= "1837401234567890"

# Go JSON 里按字段过滤:某个 RPC 方法的故障
{job="go_services"} | json | method="/login.LoginService/Login" | level="error"

# 每个服务每分钟错误条数(画图)
sum by (service) (count_over_time({level=~"error|fatal"}[1m]))

# C++ 里按线程看(结构化元数据)
{service="scene"} | tid="36460"
```

---

## 5. 自检 / 排障

| 现象 | 看哪里 | 常见原因 |
|---|---|---|
| Grafana 里一条都没有 | `http://localhost:12345` → Components,看 `loki.source.file.*` 是否 Healthy、`loki.write.local` 有无报错 | Docker Desktop 没把 E: 盘共享给容器;`run/logs` 目录不存在 |
| 只缺某一种语言 | `docker logs xm-alloy \| findstr <job名>` | 文件超过 48 小时没写(`ignore_older_than = "48h"`,重启服务就有新行);文件名不符合 2/3 节的约定 |
| 时间对不上 | 行详情里的时间 vs 文件里的时间 | C++ 固定东八区、Go/Java 带时区偏移,Alloy 已分别处理;系统时区改了不影响 |
| Loki 报 `entry too far behind` / `timestamp too old` | `docker logs xm-alloy` | 同一文件里时间倒流(服务重启后覆盖写同名文件);30 天以上的旧行会被拒,属正常 |
| 新起了一个服务看不到 | `config.alloy` 的 `path_targets` | 日志文件不在采集的目录 / 通配符之外 |

改了 `config.alloy` 后:`docker compose -f deploy\docker-compose.observability.yml restart alloy`(Alloy 启动时校验语法,错了容器会退出,`docker logs xm-alloy` 看第一行)。

彻底清空 Loki 数据:`docker compose -f deploy\docker-compose.observability.yml down -v`。

---

## 6. 生产 / k8s

本文只覆盖本机文件采集。k8s 里服务日志走容器 stdout,采集器换成 Alloy DaemonSet 的
`loki.source.kubernetes`(或 `discovery.kubernetes` + `loki.source.file` 读 `/var/log/pods`),
解析规则(`loki.process` 里三段)可以原样搬过去,标签改从 pod label 取。
Loki 也从单机文件存储换成对象存储 + 多副本。`docs/ops/log-management.md` 的「Production (K8s)」一节是总方针。

相关文档:`docs/design/distributed-tracing.md`(trace_id 打通后,日志行里带 `trace_id` 就能在 Grafana 里一键从日志跳到链路)、
`docs/design/error-reporting.md`。
