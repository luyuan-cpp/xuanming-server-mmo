# 本地日志观测台:Grafana + Loki + Alloy(C++ / Go / Java 三语言统一接入)

> **状态:** 2026-09-13 接入;2026-09-14 修正 C++ 解析规则、钉死镜像版本,并在本机清空重采、逐文件对账。
> 适用于 Windows 本机开发环境;k8s 侧见 §6。

---

## 0. 一分钟上手

```bat
dev.bat obs
```

浏览器打开 <http://localhost:3000/d/xm-game-logs>,就是看板「游戏服务日志总览(C++ / Go / Java)」。
也可以在左侧「仪表板 → 玄冥本地」里找到它。
顶部可以按 **来源(语言)/ 服务 / zone / 级别 / 关键字** 过滤,下方是三种语言合在一起的日志流。
想自由查询,用左侧「探索(Explore)」或「Drilldown → Logs」。

```bat
dev.bat obs-down
```

这条命令停掉整套观测台。Loki 已收的日志留在 docker 卷里,下次 `dev.bat obs` 还在,最多保留 7 天。

**前提是 Docker Desktop 在跑。** 电脑睡眠唤醒后 Docker Desktop 常常是停的。把它打开后,三个容器会自动恢复(`restart: unless-stopped`)。

---

## 1. 这套东西是怎么工作的

打个比方:三种语言的服务各自往本机文件里写日记,它们本来就在这么做,**服务代码一行没改**。
Alloy 是一个盯着这些文件的抄写员:每来一行,它先认出时间和级别,再贴上标签(哪种语言、哪个服务、哪个 zone),然后送进 Loki。
Loki 是档案库,Grafana 是档案库的前台,负责查询和画图。

```
run/logs/go_services/<实例>.stdout.log / .stderr.log   Go   go-zero logx,一行一条 JSON
run/logs/cpp_nodes/<实例>.stdout.log / .stderr.log     C++  muduo 文本;stderr 里是 gRPC/abseil 文本
run/logs/game-launcher/<时间戳>/gateway.*.log           Java start_game.ps1 起的网关
run/logs/java/*.log、run/logs/sa_token.log              Java 手工重定向的网关 / SA-Token demo
        │
        ▼
   Alloy:盯文件 → 合并多行 → 解析时间和级别 → 打标签
        │
        ▼
   Loki:存 7 天  ──▶  Grafana:查询 / 看板
```

| 组件 | 镜像(钉死版本) | 容器名 | 端口 | 配置文件 |
|---|---|---|---|---|
| Loki | `grafana/loki:3.5.8` | `xm-loki` | 3100 | `deploy/observability/loki/loki.yaml` |
| Alloy | `grafana/alloy:v1.10.0` | `xm-alloy` | 12345(调试页) | `deploy/observability/alloy/config.alloy` |
| Grafana | `grafana/grafana:13.2.1` | `xm-grafana` | 3000 | `deploy/observability/grafana/provisioning/*`(数据源和看板自动导入) |

compose 文件是 `deploy/docker-compose.observability.yml`。它和 `deploy/docker-compose.yml`(Kafka/Redis/MySQL/etcd)互不依赖。

**为什么 Alloy 停在 v1.10.0。** v1.19.2 的 `loki.source.file` 有读取位置回归:一次读完很多行后,`positions.yml` 里只记下"第一行之后"的偏移。
例如读完 344 KB 的文件,只记了 172 字节。Alloy 一重启,就把文件几乎从头再读一遍。
带时间戳的行会被 Loki 去重,但启动 banner、JVM 警告这类没时间戳的行会重复入库。
v1.10.0 记录的位置等于文件大小,解析结果和 v1.19.2 逐条一致。升级前请先跑 §5.3 的自检。

本地 Grafana 免登录,匿名访问即管理员。**不要把这套配置原样搬到公网。**

---

## 2. 三种语言各自怎么接的

### 2.1 Go(go-zero `logx`)

- **不用改代码。** `tools/scripts/go_services.ps1` 把每个实例的 stdout 和 stderr 分别重定向到 `run/logs/go_services/<实例>.stdout.log / .stderr.log`。
- go-zero 控制台模式下,info、debug、stat 写 stdout,error、slow 等告警级别写 stderr。两路都采。
- 行长这样:`{"@timestamp":"2026-09-13T03:07:37.138-04:00","caller":"login/login.go:67","content":"...","level":"info"}`
- Alloy 取 `@timestamp` 当日志时间,取 `level` 当标签,把 `caller` 放进结构化元数据。
  `logx.Errorw(..., logx.Field("method", ...))` 这类附加字段仍在原始 JSON 里,查询时加 `| json` 就能按字段过滤。
- **go-zero 没有 warn 级别。** 它的级别是 debug / info / error / severe / fatal / slow / stat / alert。
  其中 `slow` 是慢调用告警(例如 Redis slowcall),看板把它算进"警告"。偶尔出现的 `warn` 来自内嵌的 etcd 客户端。
- 文件名决定标签:`z2_login.stdout.log` 得到 `zone=2, service=login, instance=z2_login, stream=stdout`。
  `data_service_1.stdout.log` 得到 `service=data_service, instance=data_service_1`。文件名没有 `zN_` 前缀,就没有 `zone` 标签。
- `level=stat` 是 go-zero 每分钟打的 CPU/内存统计,量很大。不想看就在级别过滤里去掉,或在服务 yaml 里加 `Log: { Stat: false }` 关掉。
- 不是 JSON 的行原样入库,没有 `level` 标签。例如启动 banner `=====`、redis 客户端的纯文本提示、panic 堆栈。

### 2.2 C++(muduo `LOG_INFO` 等)

- **不用改代码。** muduo 日志经 `Node::AsyncOutput` 同时写两处:一处是 muduo 自己滚动的 `bin/logs/cpp_nodes/<节点>.<时间>.<主机>.<pid>.log`,另一处是控制台。
  `tools/scripts/cpp_nodes.ps1` 把控制台的 stdout 和 stderr 重定向到 `run/logs/cpp_nodes/<实例>.stdout.log / .stderr.log`。
  Alloy 采的是后者,因为这里的文件名带 `z1_gate` 这种实例名。
- **stdout** 行长这样:`20260913 15:24:58.979463 36460 INFO  Node created ... - E:\...\node.cpp:366`
  - 时间:Windows 下 `Node::SetupTimeZone` 固定东八区,和系统时区无关,Alloy 按 `Asia/Shanghai` 解析。Linux 下读 `zoneinfo/Asia/Hong_Kong`,k8s 镜像里已经准备好。
  - `36460` 是线程号,**右对齐占 5 列**。不足 5 位时前面补空格,例如 `...370638  5584 INFO`。
    早先的规则漏了这一点,线程号小于 10000 的节点整份日志都没有 `level`,连 ERROR 也认不出来。2026-09-14 已修。
  - 行首带控制台颜色码,Alloy 先去掉再解析。
  - `Build Info` 这类多行消息只有第一行带时间戳,后续行会并回同一条。
  - 级别:muduo 只输出 TRACE / DEBUG / INFO / WARN / ERROR / FATAL 六种。`LOG_SYSERR` 实际按 ERROR 输出,行尾带 errno 文案。
- **stderr** 里主要是 gRPC / abseil 自己的日志,行长这样:
  `E0913 07:40:56.694940   52672 chttp2_transport.cc:1425] ipv4:127.0.0.1:53000: Received a GOAWAY ... too_many_pings`
  首字母是级别:E=error,W=warn,I=info,F=fatal。时间是 UTC 且不带年份,Alloy 用采集当年补上。
- 如果某次节点不是用 `cpp_nodes.ps1` 起的,就没有 stdout 重定向。`config.alloy` 文末有一段注释掉的 `bin/logs/cpp_nodes/*.log` 采集配置,取消注释即可,它按 pid 区分实例。

### 2.3 Java(Spring Boot)

- **改了配置,没改代码。** `java/gateway_node/src/main/resources/application.yaml` 和 `java/config_node/src/main/resources/application.yml` 各加了一段:
  ```yaml
  logging:
    structured:
      format:
        console: logstash
  ```
  这是 Spring Boot 3.4 自带的结构化日志。**jar 要重新打包才生效。** 生效后控制台一行一条 JSON:
  `{"@timestamp":"...","level":"INFO","logger_name":"c.game.gateway.X","thread_name":"main","message":"...","stack_trace":"..."}`
- 没重新打包的旧 jar 也能采。Alloy 同时认 Spring Boot 默认的文本格式,例如
  `2026-09-05T02:13:53.989-04:00  INFO 26768 --- [gateway-node] [ main] c.game.X : 消息`,堆栈续行会并回上一条。
- 想临时恢复文本格式,启动时加参数 `--logging.structured.format.console=`(空值)。
- **只有落了文件的启动方式才采得到:**

  | 启动方式 | 日志去哪 | 能否采到 |
  |---|---|---|
  | `tools/scripts/start_game.ps1`(一键起服并开游戏) | `run/logs/game-launcher/<时间戳>/gateway.stdout.log / .stderr.log` | 能 |
  | `dev.bat ui`(mprocs;`dev_mprocs_proc.ps1` 里跑 `mvnw spring-boot:run`) | 只显示在 mprocs 面板,不落文件 | **不能** |
  | 手工 `java -jar`,并把输出重定向到 `run/logs/java/gateway*.log` 或 `config_node*.log` | 你指定的文件 | 能 |
  | `dev.bat start-satoken` | `run/logs/sa_token.log` | 能(`service=sa_token`,文本格式) |

  仓库里没有脚本往 `run/logs/java/` 写文件,这个目录是给手工启动约定的位置。`config_node` 目前也没有本地启动脚本,要看它的日志只能手工起并重定向。
  手工起网关的例子(PowerShell,仓库根目录):
  ```powershell
  Start-Process java -WorkingDirectory java\gateway_node -ArgumentList '-jar','target\gateway-node-0.0.1-SNAPSHOT.jar' -RedirectStandardOutput run\logs\java\gateway.stdout.log -RedirectStandardError run\logs\java\gateway.stderr.log -WindowStyle Hidden
  ```
- stderr 里 JVM 自己的 `WARNING: ...` 行没有 `level` 标签。

---

## 3. 标签约定

| 标签 | 取值 | Go | C++ | Java |
|---|---|---|---|---|
| `job` | `go_services` / `cpp_nodes` / `java` | ✓ | ✓ | ✓ |
| `lang` | `go` / `cpp` / `java` | ✓ | ✓ | ✓ |
| `service` | Go:`login` `db` `scene_manager` `player_locator` `data_service` `match` `client_rpc_router`…;C++:`gate` `scene` `battle`;Java:`gateway` `config_node` `sa_token` | ✓ | ✓ | ✓ |
| `zone` | `1` `2` …,只在文件名带 `zN_` 前缀时才有 | ✓ | ✓ | 无 |
| `instance` | `z1_gate` `data_service_1` … | ✓ | ✓ | 无 |
| `stream` | `stdout` / `stderr` | ✓ | ✓ | 无 |
| `level` | 统一小写,取值见下表;认不出格式的行没有这个标签 | ✓ | ✓ | ✓ |
| `filename` | 容器内路径,如 `/host/run/logs/go_services/login.stdout.log` | ✓ | ✓ | ✓ |
| `env` | `local` | ✓ | ✓ | ✓ |

| 语言 | 可能出现的 `level` |
|---|---|
| Go | `debug` `info` `error` `severe` `fatal` `slow` `stat` `alert`,内嵌 etcd 客户端偶有 `warn` |
| C++ | stdout:`trace` `debug` `info` `warn` `error` `fatal`;stderr(gRPC):`info` `warn` `error` `fatal` |
| Java | `trace` `debug` `info` `warn` `error` |

结构化元数据不是标签,但能在行详情里看到,也能用 `| caller="..."` 过滤。Go 有 `caller`,C++ 有 `tid`,Java 有 `logger` 和 `thread`。

标签只用上面这些低基数字段。**不要**把 player_id、session_id 之类做成标签,Loki 会被撑爆。查这类值用行内搜索:`{job=~".+"} |= "player_id=12345"`。

---

## 4. 常用查询(Explore 里直接贴)

```logql
# 三种语言合流,只看错误
{job=~"go_services|cpp_nodes|java", level=~"error|fatal|severe"}

# 警告(Go 的慢调用级别叫 slow)
{job=~".+", level=~"warn|slow|alert"}

# zone 1 的 gate 和 login 一起看(跨语言排查一次登录)
{zone="1", service=~"gate|login"}

# 某个玩家 id 在所有服务里的足迹
{job=~".+"} |= "1837401234567890"

# Go 的 JSON 字段过滤:某个 RPC 方法的错误
{job="go_services", level="error"} | json | method="/login.LoginService/Login"

# 每个服务每分钟的错误条数(画图)
sum by (service) (count_over_time({job=~".+", level=~"error|fatal|severe"}[1m]))

# C++ 按线程看(结构化元数据)
{service="scene"} | tid="5584"

# 正则里有反斜杠时用反引号,不要用双引号
{job="cpp_nodes"} |~ `kind=\w+ biz_tag`
```

看板顶部的「关键字」框直接填正则,例如 `kafka|redis`,不区分大小写。看板内部用反引号包它,所以框里不要输入反引号。

---

## 5. 自检 / 排障

| 现象 | 原因和处理 |
|---|---|
| 看板打开是空的,Explore 里却查得到 | 看板默认只看最近 1 小时,服务没在跑时这段时间里没有日志。把右上角时间范围调大,例如「过去 2 天」。 |
| C++ 节点最新的一两条日志迟迟不出现 | C++ 节点的 stdout 重定向写文件时有缓冲,文件末尾常停在半行,Alloy 要等这一行写完整才读。节点再打下一条日志时它才会出现。用停服脚本强制结束进程时,这半行来不及写出,Loki 里会少最后一条。完整内容以 `bin/logs/cpp_nodes/` 下 muduo 自己的日志文件为准,它约 1 秒刷一次盘。 |
| Grafana 里一条都没有 | 打开 <http://localhost:12345> 看 `loki.source.file.*` 是否 Healthy,再看 `docker logs xm-alloy`。常见原因是 Docker Desktop 没起,或服务不是用脚本起的,没有 `run/logs` 文件。 |
| 只缺某个服务 | 该文件超过 48 小时没写过(`ignore_older_than = "48h"`),或它的启动方式不落文件(见 §2.3 的表)。 |
| 刚 `dev.bat obs` 时只看得到最近的日志,几小时前的要等几分钟 | 正常。Loki 只从内存里查最近一段,老时间戳的块要落盘后才查得到(`chunk_idle_period: 3m`),本机实测启动 1 分钟时只查得到一部分,7 分钟内全部可查。 |
| 某个 C++ 节点整份日志都没有 `level` | 行首格式和 §2.2 的行例不一致了。对照着改 `config.alloy` 里 cpp 段的三处正则:`stage.multiline`、`stage.match` 的 selector、`stage.regex`。 |
| Loki 容器反复重启 | `docker logs xm-loki` 的第一行多半是 `field ... not found`,说明 `loki.yaml` 写了 3.x 已经不存在的键,例如 `query_ingesters_within`。改完先用 §5.1 校验。 |
| Loki 挂过一阵子,那段时间的日志缺了 | Alloy 发不出去时会退避重试一段时间,超时就丢弃这批,Loki 恢复后也不会补发。需要补就按 §5.4 清空重采,只能补回 48 小时内写过的文件。 |
| 启动 banner 之类的行出现两份 | Alloy 重启后从记录的位置接着读,v1.10.0 最多重读每个文件的最后一行。如果换成有位置回归的版本(如 v1.19.2),会几乎整文件重读。 |
| 时间对不上 | C++ stdout 是东八区,gRPC stderr 是 UTC,Go 和 Java 自带时区偏移,Alloy 分别处理。行详情里能看到解析后的时间。 |

### 5.1 改完 Loki 配置先校验(PowerShell,仓库根目录)

```powershell
docker run --rm -v "${PWD}\deploy\observability\loki\loki.yaml:/etc/loki/loki.yaml:ro" grafana/loki:3.5.8 '-config.file=/etc/loki/loki.yaml' '-verify-config'
docker compose -f deploy\docker-compose.observability.yml restart loki
```

第一条没有输出、`$LASTEXITCODE` 为 0,才执行第二条。配置里有不存在的键时,它会打印 `field ... not found` 并返回 1。
两个参数的引号不能省:PowerShell 会把不带引号的 `-config.file=...` 从点号处拆开,Loki 只会报 `flag provided but not defined: -config`。

### 5.2 改完 Alloy 配置,先不入库试跑(Git Bash,仓库根目录)

把配置里送往 Loki 的出口换成 `loki.echo`,每条日志连同标签打到屏幕上,不会写进 Loki:

```bash
MSYS_NO_PATHCONV=1 docker run --rm \
  -v "$(pwd -W)/deploy/observability/alloy/config.alloy:/cfg/config.alloy:ro" \
  -v "$(pwd -W)/run/logs:/host/run/logs:ro" \
  --entrypoint sh grafana/alloy:v1.10.0 -c '
sed -e "s#loki.write.local.receiver#loki.echo.e.receiver#g" /cfg/config.alloy > /tmp/c.alloy
echo "loki.echo \"e\" {}" >> /tmp/c.alloy
/bin/alloy run --storage.path=/tmp/data /tmp/c.alloy > /tmp/out 2>&1 &
sleep 40; grep -E "level=(warn|error)" /tmp/out | head; grep -c "component_id=loki.echo.e" /tmp/out'
```

没有 warn/error、条数合理后再重启:`docker compose -f deploy\docker-compose.observability.yml restart alloy`。

### 5.3 升级 Alloy 前的读取位置自检(Git Bash)

```bash
MSYS_NO_PATHCONV=1 docker run --rm --entrypoint sh grafana/alloy:<新版本> -c '
mkdir -p /tmp/t
printf "local.file_match \"t\" {\n  path_targets = [{__path__ = \"/tmp/t/*.log\"}]\n  sync_period = \"1s\"\n}\nloki.source.file \"t\" {\n  targets = local.file_match.t.targets\n  forward_to = [loki.echo.e.receiver]\n}\nloki.echo \"e\" {}\n" > /tmp/c.alloy
i=0; while [ $i -lt 2000 ]; do echo "line $i padding-padding-padding-padding-padding-padding-padding-padding-padding-padding"; i=$((i+1)); done > /tmp/t/a.log
/bin/alloy run --storage.path=/tmp/data /tmp/c.alloy > /dev/null 2>&1 &
sleep 25; wc -c < /tmp/t/a.log; cat /tmp/data/loki.source.file.t/positions.yml'
```

**通过标准:** `positions.yml` 里的数字等于上面打印的文件大小,本机 v1.10.0 是 178890。
如果只有几十字节(v1.19.2 是 87),这个版本有位置回归,不要升级。

### 5.4 清空重来

```bat
docker compose -f deploy\docker-compose.observability.yml down -v
dev.bat obs
```

`down -v` 会删掉 Loki 的日志数据、Alloy 的读取位置和 Grafana 的本地设置。之后重新采集 48 小时内写过的文件。

---

## 6. 生产 / k8s

本文只覆盖本机文件采集。k8s 里服务日志走容器 stdout,采集器换成 Alloy DaemonSet 的 `loki.source.kubernetes`,或者用 `discovery.kubernetes` 加 `loki.source.file` 读 `/var/log/pods`。
`loki.process` 里三种语言的解析段可以原样搬过去,标签改从 pod label 取;Alloy 版本同样要先过 §5.3 的自检。
Loki 也要从单机文件存储换成对象存储加多副本。`docs/ops/log-management.md` 的「Production (K8s)」一节是总方针。

相关设计:`docs/design/distributed-tracing.md`(日志行里带上 `trace_id` 后,可以在 Grafana 里从日志跳到链路)、`docs/design/error-reporting.md`。

---

## 7. 本机验证记录(2026-09-14)

- **版本组合:** Loki 3.5.8、Alloy v1.10.0、Grafana 13.2.1。
- **全量重采:** 清空 Loki 和 Alloy 的数据卷后重新采集,Alloy 发出 7010 条,没有丢弃,没有报错。
- **逐文件对账:** 同一份配置在一次性容器里用 `loki.echo` 试跑,按"文件 × 级别"得到 45 行计数,和 Loki 实际入库逐行一致。
- **级别识别:** C++ 的 info / warn / error 都识别出来了,包括 gRPC stderr 里的 error;Go 的 info / error / slow / stat / debug 和 Java 的 info / warn 也都识别出来了。没有级别的只剩启动 banner、JVM 警告这类非日志行。
- **看板:** 6 个查询面板的表达式经 Grafana `/api/ds/query` 执行成功。时间范围取 3 天时,错误数 37、警告数 25,和对账数字一致。
- **实时追加:** 往被采集的文件追加行,Alloy 3 秒内读到;C++ 行的级别和东八区时间正确。
- **读取位置:** v1.10.0 记录的位置等于文件大小。v1.19.2 在容器本地文件上同样出现位置回归,说明问题在 Alloy 本身,不在 Docker Desktop 的挂载。
- **文档里的命令:** §5.1 在 PowerShell 里执行,好配置返回 0,带未知键的配置返回 1;§5.2 和 §5.3 从本文原样提取后在 Git Bash 执行通过。`dev.bat obs` / `obs-down` 用替身副本在 cmd 里跑通。
- **真实起服:** 用 `start_game.ps1`(不开客户端)起全套服务后,18 个正在写的日志文件里,能解析的最新一行都已进 Loki,最新一条约 20 秒前写入。看板默认的最近 1 小时窗口显示 11 个服务、错误 13、警告 5。gate / scene / battle 带 `zone=1`,网关日志按 JSON 解析出级别。
- **C++ 行尾缓冲:** scene、battle、gate 的 stdout 文件末尾都停在半行,三个文件最后一条完整的行都已在 Loki 里。用停服脚本强制结束后,半行没有补写,Loki 里各少最后一条;这三条的完整内容都能在 `bin/logs/cpp_nodes/` 的 muduo 日志里找到。
- **未验证:** Java 结构化 JSON 只在 gateway_node 上实际运行过,config_node 没有运行。
