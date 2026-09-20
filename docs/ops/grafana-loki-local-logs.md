# 本地日志观测台:Grafana + Loki + Alloy(C++ / Go / Java 三语言统一接入)

> **状态:** 2026-09-13 接入;2026-09-14 修正 C++ 解析规则、钉死镜像版本并逐文件对账;
> 2026-09-18 按对抗式复核结果修正采集规则与本文多处说法(见 §7);
> 2026-09-19 k8s 侧 C++ 日志 sidecar 做了一轮审计修复(原生 sidecar / 日志保留 / 解析规则,见 §6;
> 已落码,并在 kind 的临时 namespace 上做完端到端复测,真实 zone / infra namespace 仍未 apply,见 §7)。
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

**两个一开始就会遇到的坑:**

- 看板默认只看**最近 1 小时**。服务没在跑时这段时间没有日志,看板就是空的,把右上角时间范围调大即可。
- 顶部的 **zone 下拉保持 `All`**。选中具体 zone(如 `1`)会把 `start_game.ps1` 起的全部 Go 服务和 Java 网关一起滤掉,
  因为只有 C++ 节点是以 `z1_` 前缀启动的(细节见 §3 的 `zone` 行)。默认 `All` 不会漏任何东西。

---

## 1. 这套东西是怎么工作的

打个比方:三种语言的服务各自往本机文件里写日记。Alloy 是一个盯着这些文件的抄写员:每来一行,
它先认出时间和级别,再贴上标签(哪种语言、哪个服务、哪个 zone),然后送进 Loki。
Loki 是档案库,Grafana 是档案库的前台,负责查询和画图。**服务代码一行没改**,只有 Java 加了一段日志格式配置。

```
run/logs/go_services/<实例>.stdout.log / .stderr.log   Go   go-zero logx,一行一条 JSON
run/logs/cpp_nodes/<实例>.stdout.log / .stderr.log     C++  muduo 文本;stderr 里还有 gRPC 和 librdkafka 的文本
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

### 2.0 哪些启动方式的日志会被采到

采集的前提是**日志落到了文件**。前台运行、只把日志显示在终端面板里的启动方式一律采不到。

| 启动方式 | Go | C++ | Java |
|---|---|---|---|
| `tools/scripts/start_game.ps1`(一键起服) | 能 | 能 | 能(落 `run/logs/game-launcher/<时间戳>/`) |
| `dev.bat start` / `start-go` / `start-cpp` / `start-multi` / `start-zones` | 能 | 能 | 不涉及(网关不在这些命令里) |
| `dev.bat ui` / `ui-cpp` / `ui-go`(mprocs) | **不能** | **不能**(见下) | **不能** |
| 手工 `java -jar` 并重定向到 `run/logs/java/` | — | — | 能 |
| `dev.bat start-satoken` | — | — | 能(`service=sa_token`,文本格式) |

- `dev.bat start` 系列靠 `go_services.ps1` 和 `cpp_nodes.ps1` 做 stdout/stderr 重定向,所以能采。
- mprocs(`dev_mprocs_proc.ps1`)里三种语言都是前台运行、不重定向:Go 服务的日志只在 mprocs 面板里,
  服务 yaml 也没有开 `Log.Mode: file`;C++ 只有 `bin/logs/cpp_nodes/` 下 muduo 自己的文件,而 Alloy 默认不采那个目录(见 §2.2 末尾)。
- Linux 侧的 `tools/scripts/deploy-staging.sh` 会写 `run/logs/java/gateway.log`(能采到),
  但它把 Go 写成 `run/logs/go_services/<服务>.log`、C++ 写成 `run/logs/cpp_nodes/scene.log`,
  **不匹配** `config.alloy` 的 `*.stdout.log` / `*.stderr.log` 通配,这条路径下 Go 和 C++ 采不到。

### 2.1 Go(go-zero `logx`)

- **不用改代码。** `tools/scripts/go_services.ps1` 把每个实例的 stdout 和 stderr 分别重定向到 `run/logs/go_services/<实例>.stdout.log / .stderr.log`。
- go-zero 控制台模式下,info、debug、stat 写 stdout,error、slow、alert 等告警级别写 stderr。两路都采。
- 行长这样:`{"@timestamp":"2026-09-13T03:07:37.138-04:00","caller":"login/login.go:67","content":"...","level":"info"}`
- Alloy 取 `@timestamp` 当日志时间,取 `level` 当标签,把 `caller` 放进结构化元数据。
  `logx.Errorw(..., logx.Field("method", ...))` 这类附加字段仍在原始 JSON 里,查询时加 `| json` 就能按字段过滤。
- **go-zero 没有 warn 级别。** 它实际写进日志的 `level` 是 debug / info / error / fatal / slow / stat / alert。
  `logx.Severe` / `logx.Must` 虽然叫 severe,写进日志的级别是 `fatal`(后面还跟一段 goroutine 堆栈)。
  其中 `slow` 是慢调用告警(例如 Redis slowcall),看板把它算进"警告"。
- **两类第三方库的行也在这些文件里,规则不同:**
  - 内嵌 etcd 客户端(zap)的 JSON 行时间在 `ts` 字段、偏移写成 `-0400` 这种不带冒号的形式,
    Alloy 用单独一段规则解析。Go 日志里偶尔出现的 `warn` 就来自它。
  - go-redis 的纯文本行 `redis: 2026/09/16 10:14:15 pool.go:617: ...` 按**服务所在机器的本地时区**解析,
    换机器换时区时要改 `config.alloy` 里那段的 `location`。
- 文件名决定标签:`z2_login.stdout.log` 得到 `zone=2, service=login, instance=z2_login, stream=stdout`。
  `data_service_1.stdout.log` 得到 `service=data_service, instance=data_service_1`。文件名没有 `zN_` 前缀,就没有 `zone` 标签。
- `level=stat` 是 go-zero 每分钟打的统计:CPU/内存、RPC qps 与耗时分位、限流 shedding、p2c 负载,量很大。
  不想看就在级别过滤里去掉,或在服务 yaml 里加 `Log: { Stat: false }` 关掉。
- 其余不是 JSON 的行原样入库,没有 `level` 标签,时间取 Alloy 读到它的时刻。例如启动 banner `=====`、panic 堆栈。

### 2.2 C++(muduo `LOG_INFO` 等)

- **不用改代码。** muduo 日志经 `Node::AsyncOutput` 写两处:一处是 muduo 自己滚动的 `bin/logs/cpp_nodes/<节点>.<时间>.<主机>.<pid>.log`,
  另一处是控制台(**仅 Windows**,见 §6)。`tools/scripts/cpp_nodes.ps1` 把控制台的 stdout 和 stderr 重定向到
  `run/logs/cpp_nodes/<实例>.stdout.log / .stderr.log`。Alloy 采的是后者,因为这里的文件名带 `z1_gate` 这种实例名。
- **stdout** 行长这样:`20260913 15:24:58.979463 36460 INFO  Node created ... - E:\...\node.cpp:366`
  - 时间:Windows 下 `Node::SetupTimeZone` 固定东八区,和系统时区无关,Alloy 按 `Asia/Shanghai` 解析。Linux 下读 `zoneinfo/Asia/Hong_Kong`,k8s 镜像里已经准备好。
  - `36460` 是线程号,**右对齐占 5 列**。不足 5 位时前面补空格,例如 `...370638  5584 INFO`。
    早先的规则漏了这一点,线程号小于 10000 的节点整份日志都没有 `level`,连 ERROR 也认不出来。2026-09-14 已修。
  - 行首带控制台颜色码,Alloy 先去掉再解析。
  - `Build Info` 这类多行消息只有第一行带时间戳,后续行会并回同一条。
  - 级别:muduo 只输出 TRACE / DEBUG / INFO / WARN / ERROR / FATAL 六种。`LOG_SYSERR` 实际按 ERROR 输出;
    errno 不为 0 时,会在级别之后、正文之前插入 `<错误描述> (errno=N) `,行尾仍是 ` - 文件:行号`。
  - **重要:stdout 写进文件时是 4096 字节整块缓冲,最新的日志会长时间看不到。** 细节和应对见 §5 排障表第一行。
- **stderr** 里有三种非 muduo 的行,Alloy 都已分别识别:
  - gRPC / abseil:`E0913 07:40:56.694940   52672 chttp2_transport.cc:1425] ... GOAWAY ... too_many_pings`。
    首字母是级别(E=error,W=warn,I=info,F=fatal),时间是 UTC 且**不带年份**。
    补年份的规则是:行里的月日比今天晚就算去年的,否则算今年。这样跨年那几秒、以及 1 月读到去年 12 月的旧文件都不会得到"未来时间"
    (未来时间会被 Loki 以 400 拒收,而 `loki.write` 对 4xx 不重试,那批日志会直接丢掉)。
  - librdkafka:`%3|1789614115.711|ERROR|rdkafka#consumer-1| [thrd:...]: ...`。
    开头 `%N` 是 syslog 级别(0-3 = error,4 = warn,5-6 = info,7 = debug),紧跟的是 epoch 秒,两者都按原值入库。
    Kafka 连不上时这里会刷一大批 ERROR。
  - muduo 断言失败:`Assertion failed: ch != channels_.end(), file ..., line 68`,按 `fatal` 记。这行没有时间戳,时间取读取时刻。
- 如果某次节点不是用 `cpp_nodes.ps1` 起的,就没有 stdout 重定向。`config.alloy` 文末有一段注释掉的 `bin/logs/cpp_nodes/*.log` 采集**骨架**,
  只做了"按文件名取 service/pid"和多行合并。**直接取消注释只能用来翻原文,不能当正经采集用**:
  它没有 `stage.timestamp`(时间会变成读取时刻,首次把 48 小时内的旧文件一次读进来时全部挤在同一秒),
  也没有解析 `level`(进不了看板的错误/警告统计面板),而且 `job` 是 `cpp_files`、没有 `zone` / `instance` / `stream`,不在 §3 的标签表里。
  真要长期用,得把 cpp 段的两个 `stage.match` 也抄过去并把 selector 里的 `job` 改成 `cpp_files`,再补一个去 `\r` 的 `stage.replace`。

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
- 哪种启动方式能采到见 §2.0 的表。**Windows 本机**的启动脚本里没有往 `run/logs/java/` 写文件的,
  这个目录是给手工启动约定的位置;`config_node` 也没有本地启动脚本,要看它的日志只能手工起并重定向。
  手工起网关的例子(PowerShell,**在仓库根目录执行**):
  ```powershell
  New-Item -ItemType Directory -Force run\logs\java | Out-Null   # 目录不存在时 pwsh7 直接报错不启动,PS5.1 则日志静默丢失
  Start-Process java -WorkingDirectory java\gateway_node -ArgumentList '-jar','target\gateway-node-0.0.1-SNAPSHOT.jar' -RedirectStandardOutput run\logs\java\gateway.stdout.log -RedirectStandardError run\logs\java\gateway.stderr.log -WindowStyle Hidden
  ```
  重定向的相对路径按 PowerShell 的当前目录解析,和 `-WorkingDirectory` 无关;jar 路径才按 `-WorkingDirectory` 解析。
  这样起的进程 PID 是 Oracle `javapath\java.exe` 壳,真正的 JVM 是它的子进程,按壳的 PID 停不掉网关
  (`start_game.ps1` 已按"监听 8081 的进程"记录 PID,手工起时要自己按命令行找子进程)。
- stderr 里 JVM 自己的 `WARNING: ...` 行没有 `level` 标签,时间取读取时刻。
- `run/logs/sa_token.log` 是 `dev.bat` 用 `>>` **追加**写的。Alloy 已经把 Maven 的 `[INFO]`、`Downloading from ...`、
  JVM 的 `WARNING:`、`Hibernate:` 都当作独立行头,否则补读时下一次启动的输出会被并进上一次运行的最后一条日志、继承它的级别和日期。

---

## 3. 标签约定

| 标签 | 取值 | Go | C++ | Java |
|---|---|---|---|---|
| `job` | `go_services` / `cpp_nodes` / `java` | ✓ | ✓ | ✓ |
| `lang` | `go` / `cpp` / `java` | ✓ | ✓ | ✓ |
| `service` | Go:`login` `db` `scene_manager` `player_locator` `data_service` `match` `client_rpc_router` `chat` `guild` `trade`…;C++:`gate` `scene` `battle`;Java:`gateway` `config_node` `sa_token` | ✓ | ✓ | ✓ |
| `zone` | `1` `2` …,**只在日志文件名带 `zN_` 前缀时才有**。C++ 由 `start_game.ps1` 传 `-Zone 1`,所以是 `z1_gate` 这种;Go 服务只有 `dev.bat start-zones` 才带前缀,`start_game.ps1` / `dev.bat start` 起的 Go 日志**没有 zone 标签** | 视启动方式 | ✓ | 无 |
| `instance` | `z1_gate` `login` `data_service_1` … | ✓ | ✓ | 无 |
| `stream` | `stdout` / `stderr` | ✓ | ✓ | 无 |
| `level` | 统一小写,取值见下表;认不出格式的行没有这个标签 | ✓ | ✓ | ✓ |
| `filename` | 容器内路径,如 `/host/run/logs/go_services/login.stdout.log` | ✓ | ✓ | ✓ |
| `env` | `local` | ✓ | ✓ | ✓ |

| 语言 | 可能出现的 `level` |
|---|---|
| Go | `debug` `info` `error` `fatal` `slow` `stat` `alert`,内嵌 etcd 客户端偶有 `warn` |
| C++ | stdout(muduo):`trace` `debug` `info` `warn` `error` `fatal`;stderr:gRPC 给 `info`/`warn`/`error`/`fatal`,librdkafka 给 `error`/`warn`/`info`/`debug`,断言行给 `fatal` |
| Java | `trace` `debug` `info` `warn` `error` |

结构化元数据不是标签,但能在行详情里看到,也能用 `| caller="..."` 过滤。Go 有 `caller`,C++ 有 `tid`,Java 有 `logger` 和 `thread`。

标签只用上面这些低基数字段。**不要**把 player_id、session_id 之类做成标签,Loki 会被撑爆。查这类值用行内搜索:`{job=~".+"} |= "player_id=12345"`。

以上是**本机这条链路**的口径。k8s 上 C++ 由 sidecar 直接读 muduo 文件,标签不完全一样(没有 `instance` / `stream`,
`filename` 挪进了结构化元数据,多出 `namespace` / `pod`),对照表见 §6.4。

---

## 4. 常用查询(Explore 里直接贴)

```logql
# 三种语言合流,只看错误
{job=~"go_services|cpp_nodes|java", level=~"error|fatal"}

# 警告(Go 的慢调用级别叫 slow)
{job=~".+", level=~"warn|slow|alert"}

# gate 和 login 一起看(跨语言排查一次登录)
# 这里不能加 zone 过滤:start_game.ps1 / dev.bat start 起的 Go 服务文件名是 login.stdout.log,没有 zone 标签,
# 加了 zone="1" 会把 login 静默滤掉。要区分是哪次启动,看 instance 标签。
{service=~"gate|login"}

# 多 zone 启动(dev.bat start-zones,Go 也带 zN_ 前缀)时才可以按 zone 收窄:
{zone="1", service=~"gate|login"}

# 某个玩家 id 在所有服务里的足迹
{job=~".+"} |= "1837401234567890"

# Go 的 JSON 字段过滤:某个 RPC 方法的错误(方法名取自 proto 的 package.Service/Method)
{job="go_services", level="error"} | json | method="/loginpb.ClientPlayerLogin/Login"

# 每个服务每分钟的错误条数(画图)
sum by (service) (count_over_time({job=~".+", level=~"error|fatal"}[1m]))

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
| C++ 节点最新的几十条日志迟迟不出现 | 节点 stdout 重定向到文件后,Windows C 运行库按 **4096 字节整块缓冲**(代码里没有 `setvbuf`,`console_log.cpp` 也从不 `fflush`)。攒满约 4KB(二三十条日志)才落一次盘,所以文件末尾常停在半行。**只要没攒满,最新的最多约 4KB 日志就一直不出现,节点继续打日志也不会立刻把它们带出来**(实测一个每 30 秒打日志的 scene 落后 18 行 / 9 分钟;一个安静的 battle 有一条日志 10 小时都没落盘)。用停服脚本 `Stop-Process -Force` 结束进程时整个缓冲区直接丢。排查以 `bin/logs/cpp_nodes/` 下 muduo 自己的日志文件为准,它最多每 1 秒刷一次盘。 |
| 看板打开是空的,Explore 里却查得到 | 看板默认只看最近 1 小时,服务没在跑时这段时间没有日志。把右上角时间范围调大。另一种情况是 zone 下拉选了具体值,见 §0。 |
| Grafana 里一条都没有 | 打开 <http://localhost:12345> 看 `loki.source.file.*` 是否 Healthy,再看 `docker logs xm-alloy`。常见原因是 Docker Desktop 没起,或服务的启动方式不落文件(见 §2.0 的表)。 |
| 只缺某个服务 | 该文件超过 48 小时没写过(`ignore_older_than = "48h"`),或它的启动方式不落文件(见 §2.0)。 |
| 刚 `dev.bat obs` 时只看得到最近的日志,几小时前的要等几分钟 | 正常。单机 Loki 对「now - 2h41m」以内只查 ingester 内存,更早的只查落盘存储;刚灌进来的旧日志时间戳老、又还没落盘,两头都查不到。等 `chunk_idle_period`(已设 3m)把它们刷进存储即可,本机实测 7 分钟内全部可查。 |
| 某个 C++ 节点整份日志都没有 `level` | 行首格式和 §2.2 的行例不一致了。对照着改 `config.alloy` 里 cpp 段的正则:`stage.multiline` 的 `firstline`、各 `stage.match` 的 selector 和 `stage.regex`。 |
| Loki 容器反复重启 | `docker logs xm-loki` 的第一行多半是 `field ... not found`,说明 `loki.yaml` 里有 3.x 不认的键,**或者把键写进了错误的段**(本项目踩过的是把属于 `querier` 段的 `query_ingesters_within` 写进了 `limits_config`,该键本身在 3.5 仍然存在)。改完先用 §5.1 校验。 |
| 某些行的时间明显不对,集中挤在某一刻 | 解析不出时间的行(启动 banner、JVM `WARNING:`、断言行等)一律用 Alloy 的**读取时刻**。Docker Desktop 停过之后补读、或按 §5.4 清空重采时,这些行会整批挪到补读那一刻。带时间戳的行不受影响。 |
| Loki 挂过一阵子,那段时间的日志缺了 | Alloy 发不出去时会退避重试一段时间,超时就丢弃这批,Loki 恢复后也不会补发。需要补就按 §5.4 清空重采,只能补回 48 小时内写过的文件。 |
| 启动 banner 之类的行出现两份 | Alloy 重启后从 `positions.yml` 记的位置接着读。**正常停止**(`docker compose restart/stop alloy`,收到 SIGTERM)时 v1.10.0 会把位置刷到文件末尾,**一行都不重读**;**被强杀**(kill -9、Docker Desktop 崩溃、断电)时,位置只到最近一次同步点(约每 10 秒刷一次,滞后 10~20 秒),这段时间读过的行会重读;强杀发生在 Alloy 启动后约 20 秒内(positions 还是空的)则整份文件重读。重读的行里,有时间戳的会被 Loki 去重,没时间戳的会重复入库。 |
| 时间对不上 | C++ stdout 是东八区,gRPC stderr 是 UTC,librdkafka 是 epoch 秒,Go/Java 自带时区偏移,go-redis 按本机时区,Alloy 分别处理。行详情里能看到解析后的时间。 |

### 5.1 改完 Loki 配置先校验(PowerShell,仓库根目录)

```powershell
docker run --rm -v "${PWD}\deploy\observability\loki\loki.yaml:/etc/loki/loki.yaml:ro" grafana/loki:3.5.8 '-config.file=/etc/loki/loki.yaml' '-verify-config'
docker compose -f deploy\docker-compose.observability.yml restart loki
```

第一条没有输出、`$LASTEXITCODE` 为 0,才执行第二条。配置里有不存在**或放错段**的键时,它会打印 `field ... not found` 并返回 1。
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
改解析规则时,建议再造一份只有几行、覆盖各种行型的样本目录挂成 `/host/run/logs`,逐行核对级别和时间。

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
注意运行期间 `positions.yml` 本来就滞后真实读取位置约 10~20 秒,这个自检睡够 25 秒才读,已经跨过第一次落盘。

### 5.4 清空重来

```bat
docker compose -f deploy\docker-compose.observability.yml down -v
dev.bat obs
```

`down -v` 会删掉 Loki 的日志数据、Alloy 的读取位置和 Grafana 的本地设置。之后重新采集 48 小时内写过的文件。
注意已经入库的行不会被"修正":改了解析规则只对之后采集的行生效,想让旧数据也按新规则入库,只能这样清空重采。

---

## 6. k8s:C++ 日志不经 stdout,由 sidecar 直接读文件

**背景:C++ 节点在 Linux 下不往 stdout 写业务日志。** `Node::AsyncOutput` 里调用控制台输出的那一句包在 `#ifdef WIN32` 内,
容器里 muduo 的业务日志(包括 `LOG_FATAL` 的崩溃原因)只写 `/app/bin/logs/cpp_nodes/<节点>.*.log`。
所以"读容器 stdout"的常规做法对 C++ 只能拿到 gate 的 `[gate_version]` 启动行和 gRPC / librdkafka 的 stderr,业务日志一条都采不到。

**做法(2026-09-19 已落码;端到端链路已在 kind 的临时 namespace 上实测通过,真实 zone / infra namespace 尚未 apply,见 §7):**
每个 C++ Pod 里跟一个 Alloy sidecar,和业务容器共享同一个 `node-logs` 卷(sidecar 侧只读),
直接读那些 muduo 文件送进 Loki,全程不经过 stdout。

### 6.1 组成、形态与开关

- 生成与注入都在 `tools/scripts/k8s_deploy.ps1`:`New-CppLogSidecarConfigMapYaml` 生成解析规则(ConfigMap `cpp-log-sidecar`),
  `New-CppLogSidecarContainerYaml` / `New-CppLogSidecarVolumeYaml` 注入容器与卷。gate / scene 的 Deployment、
  Agones Fleet(scene)、以及跑在 infra namespace 的 battle 都会带上它。
- **默认开启。** 三个开关 `dev_tools.ps1` 和 `k8s_image.ps1` **都会透传**,不必绕开文档入口去直接调 `k8s_deploy.ps1`:

  | 开关 | 作用 |
  |---|---|
  | `-NoCppLogSidecar` | 关掉 sidecar 本身:它的容器片段、两个卷、config-hash 注解一个不留。**但清理循环和 `node-logs` 的 `sizeLimit: 2Gi` 不受这个开关控制,始终生效**(它们保护的是节点磁盘,与采不采日志无关,见 §6.7) |
  | `-CppLogSidecarImage` | 换 sidecar 镜像。默认钉死 `grafana/alloy:v1.10.0`(原因见 §1);**脚本的默认值**只在 `k8s_deploy.ps1` 定义一处(`dev_tools.ps1` / `k8s_image.ps1` 留空即不覆盖),文档里 `deploy/k8s/README.md` 和本文 §1 的表格另外写了这个版本号,改版本时要一起改 |
  | `-LokiPushUrl` | 推向已有的 Loki,写完整 push 地址。留空 = 用 infra namespace 里那套(见 6.2) |

  ```powershell
  pwsh tools\scripts\dev_tools.ps1 -Command k8s-zone-up -ZoneName 1 -ZoneId 1 -NoCppLogSidecar
  pwsh tools\scripts\dev_tools.ps1 -Command k8s-zone-up -ZoneName 1 -ZoneId 1 -LokiPushUrl http://loki.observability:3100/loki/api/v1/push
  ```

  两个字符串参数留空 = 不覆盖 `k8s_deploy.ps1` 的默认值,所以只传其中一个不会顺手改掉别的默认行为。
- **sidecar 是 k8s 原生 sidecar:** 容器片段放在 `initContainers` 里并带 `restartPolicy: Always`
  (原生 sidecar 1.29 默认开、1.33 GA,本机 kind 的 server 版本是 v1.37.0)。不用普通容器有两个理由,
  两个都表现为"日志静默断流、没有任何人报错":
  - Agones 给 GameServer Pod 写死 `restartPolicy: Never`。普通容器形态下 sidecar 一旦 OOM 就永远停着,
    而 Agones 的健康检查只看游戏容器,不会把 GameServer 置 Unhealthy —— 这个 Pod 余生都不再有日志。
  - Pod 终止时 kubelet 会等业务容器完全退出后再停原生 sidecar,scene 那 60 秒 drain 期里写的存档 / 踢人 / 关服日志才追得完。
    普通容器是和业务容器同时收 SIGTERM,那段最该看的日志全丢。
- sidecar 以 uid/gid **10001** 非 root 运行(与 `deploy/k8s/Dockerfile.cpp` 的 `USER 10001:10001` 同一约定),
  禁提权、根文件系统只读、`capabilities` drop ALL。它三条读写路径都不需要 root:日志目录只读挂载、
  positions 落在单独一个 64Mi 的 emptyDir(`--storage.path`)、配置走 configMap 卷。
- Alloy 的 HTTP 端点绑 `0.0.0.0:12345`(不是 `127.0.0.1`)并声明了名为 `alloy-http` 的 containerPort,
  Pod 外才抓得到它的 `/metrics` 和 `/-/reload`,用法见 6.6。

### 6.2 硬约束:Loki 只随 infra-up 部署

`deploy/k8s/manifests/infra/loki.yaml` 只在 `infra-up` 里 apply,而且只在"没关 sidecar 且没指定 `-LokiPushUrl`"时才 apply。
下面这几种走法会落进"sidecar 装上了,但推无可推"的半配置状态:

- `k8s-zone-up` 单独起 zone,这个集群从没跑过 `infra-up`;
- `k8s-all-up -SkipInfra`;
- 存量集群升级 —— infra 是这次改动之前部署的,里面根本没有 Loki。

这种状态**不会报错**:Alloy 的推送失败只写在它自己的 stdout 里,Pod 正常 Running,部署报成功,表现就是"一条 C++ 日志都查不到"。
脚本现在会在 apply 之前做一次只读探测(`kubectl -n <infra-ns> get svc loki --ignore-not-found`),查不到就 `Write-Warning` 说明该怎么办,
但**不阻断** —— 集群权限或网络抖动不该让部署失败;`-DryRun` 下跳过探测。自己核对:

```powershell
kubectl -n mmorpg-infra get deploy/loki svc/loki
```

两个都在才算齐。缺了就补一次 `pwsh tools\scripts\dev_tools.ps1 -Command k8s-infra-up`(别带 `-SkipInfra`),
或者 `-LokiPushUrl` 指向已有的 Loki,或者 `-NoCppLogSidecar` 关掉别装样子。

另一件事没这么静默,但同样容易认错:**kind 节点不共享宿主的镜像缓存**,`grafana/alloy:v1.10.0` 要先载入节点(预载命令见 `deploy/k8s/README.md`)。
漏了预载时,因为 sidecar 是**原生 sidecar、放在 `initContainers` 里**,gate / scene / battle 的 Pod 会卡在
`Init:ErrImagePull` / `Init:ImagePullBackOff`,**业务容器一次都不会启动**(`kubectl describe pod` 的报错在
**`Init Containers:`** 那一段里)。也就是说 rollout 超时的同时 gate / scene 进程根本没跑起来,不是"只有观测容器坏了"。
这是选原生 sidecar 明知要付的代价:Agones 那条路上用普通容器会让日志在 Pod 余生里静默断流(见 6.1),
而镜像拉不到本来也会让 Pod 不 Ready、gate-entry 没有后端 —— 两种形态都是故障,原生 sidecar 至少报得直白。
逃生口是 `-NoCppLogSidecar`,或者老老实实把镜像预载进 kind。脚本 apply 前会把镜像名和这句提示打出来(一次运行只打一遍)。

### 6.3 怎么看集群里的日志

集群内的 Loki 没有对外入口,`dev.bat obs` 那套 Grafana 也不会自动认识它。**两套 Loki 不共库**,
在同一个 Grafana 里是**两个数据源**,不是一个。先把它 forward 到本机:

```powershell
kubectl -n mmorpg-infra port-forward svc/loki 3101:3100
```

本机侧用 3101 不用 3100:`dev.bat obs` 起的 `xm-loki` 已经占着本机 3100。没跑本机观测台时写 `3100:3100` 也行。
这条命令前台阻塞,保持窗口开着。

**一次性验收(不碰 Grafana):**

```powershell
curl.exe -s -G 'http://127.0.0.1:3101/loki/api/v1/query_range' --data-urlencode 'query={job="cpp_nodes",env="k8s"}' --data-urlencode 'limit=5'
```

返回里有非空的 `values` 就说明采到了。查不到按 6.6 的顺序排。

**接进本机 Grafana(想和 §0 的看板混着看时):** 打开 <http://localhost:3000> → 连接 → 数据源 → 添加新数据源 → Loki,URL 填

```
http://host.docker.internal:3101
```

**不能填 `localhost:3101`**:Grafana 跑在 docker compose 里,容器里的 `localhost` 是它自己。
名字取个能区分的(例如 `Loki-k8s`),保存后在「探索」里切到它查 `{env="k8s"}`。
§0 的看板绑的是本机那个数据源,不会自动把集群数据并进去。

### 6.4 标签口径(与 §3 的差异)

k8s 这条链路的**流标签**:

| 标签 | 取值 | 来自 |
|---|---|---|
| `env` | `k8s`(本机那套是 `local`) | Alloy `external_labels` |
| `job` | `cpp_nodes` | 静态标签 |
| `lang` | `cpp` | 静态标签 |
| `service` | `gate` / `scene` / `battle` | muduo 文件名解析 |
| `level` | `trace` `debug` `info` `warn` `error` `fatal` | 行内解析;丢弃告警行记 `warn`(见 6.9) |
| `zone` | zone 名;**battle 池是 `global`** | 容器环境变量 `ZONE_NAME` |
| `namespace` / `pod` | k8s 独有 | Downward API |

**结构化元数据**(不是标签,但行详情里看得到,也能用 `| pid="..."` 过滤):`pid`、`tid`、`filename`。

两条链路口径不同的地方,查询时别照抄 §3:

- **`filename` 在 k8s 上是结构化元数据,在本机仍然是流标签。** 本机采的是 `run/logs/**` 下名字稳定的重定向文件;
  k8s 采的是 muduo 自己滚的文件,名字带滚动时间戳和 pid,**每滚 8MiB 就会新开一条 Loki 流** ——
  留成标签等于把刚挪进结构化元数据的 pid 从旁边放回去。
- **本机才有的 `instance` / `stream` 在 k8s 上没有。** 容器里没有"实例名"(muduo 文件名里只有节点角色),
  也没有 stdout / stderr 两路之分。**要区分实例按 `pod` 标签。**

### 6.5 改 sidecar 配置 = 滚动重启 C++ 节点

pod 模板上有一条注解 `mmorpg.io/cpp-log-sidecar-config-hash: <Alloy 配置正文 sha256 前 12 位>`(Deployment 与 Agones Fleet 都有)。
它是必需的:只改 ConfigMap 不会让跑着的 Alloy 重读配置(它只在 SIGHUP / `/-/reload` 时重载),
而 pod 模板逐字节没变时 `kubectl apply` 是 no-op —— 没有这条注解,改完解析规则下次部署什么也不会发生。

**代价必须记住:** 改 `-LokiPushUrl` 或改解析规则会让这条注解变,下一次 `zone-up` / `all-up` 就**滚动重启 C++ 节点**;
Agones 侧是 Fleet RollingUpdate 换一批 GameServer。这是业务影响,要挑时机,不是"顺手改个日志配置"。
换 `-CppLogSidecarImage` 同样会滚(tag 就在容器片段的 `image:` 行里,模板照样会变)。

### 6.6 sidecar 自己还活着吗

Alloy 的 `/metrics` 是"还在读、还推得出去吗"的唯一信号(它自己的报错只在容器 stdout 里):

```powershell
kubectl -n mmorpg-zone-1 port-forward deploy/gate 12346:12345
curl.exe -s http://127.0.0.1:12346/metrics | Select-String "loki_source_file_|loki_write_"
```

本机侧用 12346 不用 12345:`dev.bat obs` 起的 `xm-alloy` 已经占着本机 12345(见 §1 的表)。没跑本机观测台时写 `12345:12345` 也行。

**指标名没有 `alloy_` 前缀**(2026-09-19 实测),两个前缀各回答一个问题:

- `loki_source_file_files_active_total` —— **此刻还盯着几个文件**。0 就是根本没盯上 muduo 的日志文件
  (路径 / 挂载 / `ignore_older_than` 的问题);它会跟着清理循环走,旧文件被删掉时对应的 tailer 一起释放,
  所以正常值就是清理循环留下的那几个(实测留 8 个文件时这个指标是 8)。
- `loki_write_sent_entries_total` —— **已经推出去几条**。读得到但这个数不涨,就是推不出去(Loki 地址错或压根没部署,见 6.2);
  它在涨而 Loki 里查不到,往下看时区那条。

查不到日志时的排查顺序:先按 6.2 确认 Loki 在 → `kubectl -n <ns> describe pod <pod>`,看 **`Init Containers:`** 那一段里
sidecar 起没起来(原生 sidecar 在 `initContainers` 里,`kubectl get pod` **没有**单独的列;只想要一个布尔值时用
`kubectl -n <ns> get pod <pod> -o jsonpath='{.status.initContainerStatuses[*].ready}'`)→
`kubectl -n <ns> logs <pod> -c log-sidecar` 看它自己在报什么 → 最后才怀疑解析规则(改规则的代价见 6.5)。

**时区不对 → Loki 静默丢弃(2026-09-19 实测踩到)。** 症状是"日志文件里明明有,Loki 里查不到",而且**没有任何报错**:
C++ 容器里的时间如果写成 UTC,解析规则按 `Asia/Hong_Kong` 给它加上 +08:00,得到的是"8 小时之后"的时间戳,
默认查询窗口里自然查不到,sidecar 自己的日志里也**一个字都不报**。所以碰到这种情况,先核 C++ 容器的时区
(镜像里 `zoneinfo/Asia/Hong_Kong` 在不在、muduo 行头的时间是不是 +8),再去核 push 目标。

### 6.7 日志的保留与销毁

**Pod 里的文件(与 sidecar 开关无关,始终生效)。** muduo 每 8MiB 滚一个新文件且**从不删旧文件**,而 `node-logs` 是 `emptyDir`。
现在有两道闸:

- 业务容器的启动命令里带一个极小的清理循环:每 5 分钟把 `/app/bin/logs/cpp_nodes/*.log` 按时间排序,只留最近 8 个(≈64MiB),其余删掉。
- `node-logs` 的 `emptyDir` 带 `sizeLimit: 2Gi` 作远端兜底;`snowflake-cache` 和 sidecar 的 state 卷各 64Mi。

**盘写满的后果口径别记错。** 本机 kind 的 `evictionHard` 是 nodefs/imagefs 0%,`DiskPressure` **永远不会置位**,
所以写满的表现**不是**"节点级驱逐挑谁",而是 MySQL / Kafka / etcd / Loki 一起 ENOSPC 写失败,存档有损坏风险且不会自愈。
`sizeLimit` 走的是 kubelet 的 `emptyDirLimitEviction`,与 `DiskPressure` 无关,只驱逐越界的那一个 Pod。
正常情况下清理循环让用量停在几十 MiB,碰不到这道闸。Pod 里的文件只用于"exec 进去看最近一段",长期留存靠 Loki。

**Loki 里的数据。** 落在 infra namespace 的 `loki-data-pvc`(10Gi,用集群默认 StorageClass),retention **7 天**。

- `infra-down` / `all-down` 是 `kubectl delete namespace`,会把 Loki、PVC 和**全部已采日志**一起删掉,不可恢复。
  要留证据先按 6.3 用 `query_range` 导出存盘。
- `zone-down` 同样是删 zone namespace,本 zone 的 `cpp-log-sidecar` ConfigMap 随之消失;重开 zone 时 `zone-up` 会重建。
  已经推进 Loki 的日志不受影响(只要 infra 还在)。

### 6.8 资源开销

| | requests | limits |
|---|---|---|
| 每个 sidecar | cpu 20m / memory 64Mi | memory 256Mi |
| Loki(单副本) | cpu 100m / memory 256Mi | memory 1Gi;PVC 10Gi |

sidecar 是**每个 C++ Pod 一个**,不是每个 zone 一个:按默认副本数(gate 2 + scene 4)一个 zone 就是 6 个,
外加全局 battle 池的 1 个;`cpp-log-sidecar` 这个 ConfigMap 则每个 namespace 一份(每个 zone 一份 + infra 里 battle 一份)。
本机 kind 资源紧张时,要么 `-NoCppLogSidecar` 关掉,要么 `-LokiPushUrl` 指向外部 Loki,省掉集群里那套。

### 6.9 解析规则里几个刻意的选择

规则生成在 ConfigMap `cpp-log-sidecar` 的 `config.alloy` 里(事实源是 `k8s_deploy.ps1`,不要直接改集群里的副本)。改之前先读懂这几条:

- `local.file_match` 带 `ignore_older_than = "168h"`:muduo 滚动后不删旧文件,不甩掉它们的话每个滚过的文件都常驻一个 tailer
  (goroutine + fd + 缓冲),长跑的 scene 上只增不减。阈值取这么大是因为**文件掉出 target 列表会连读取位置一起丢**,
  它再被写入时整份(最多 8MiB)会重推一遍 —— 往小调是拿重复入库换内存,不要顺手调。
- `Dropped log messages at ...` 单列成一个多行合并的 `firstline` 分支,并用 `stage.match` 记成 `level=warn`。
  那是 muduo 后端写不过来时自己插进**日志文件**的丢弃告警(`AsyncLogging.cc` 里 `fputs` 到 stderr 的同时也 `output.append` 进 `LogFile`)。
  不单列的话它会被并进上一条、跟着那条的时间和级别走 —— 压测时"日志被丢了多少"这唯一的自证信号会被降级成 info 藏在别人的正文里。
  它那个时间走 `gmtime_r`、是 UTC,与正文 muduo 行的 +08:00 不同轴,所以**故意不接** `stage.timestamp`,用采集时间。
- **`Assertion failed: ` 那条分支已经删掉。** 它是 MSVC `_wassert` 写到控制台 stderr 的行型,只在 Windows 存在:
  Linux release 编译带 `-DNDEBUG`,`assert` 全被编掉;glibc 的断言文本也走容器 stderr、不进 muduo 文件。
  留在 k8s 这条链路上是条永远不会命中的死规则。本机那条链路(§2.2)仍然认它。
- `stage.timestamp` 的 `location` 是 `Asia/Hong_Kong`,与 `node.cpp:1163` 的
  `TimeZone::loadZoneFile("zoneinfo/Asia/Hong_Kong")` 同轴(和 `Asia/Shanghai` 偏移相同,只是不再两处写法不一致)。
- **Agones 注意:** GameServer 的 Pod 有多个容器时必须用 `spec.template.spec.container` 指名哪个是游戏容器,
  脚本在启用 sidecar 时会写这一行。sidecar 移进 `initContainers` 之后普通容器只剩 1 个、这行成了冗余,
  但保留着 —— 将来再加第二个普通容器时少了它 Fleet 会被 Agones 拒掉。

### 6.10 这套方案没覆盖的部分,别误以为都进去了

- **容器 stdout/stderr 上的东西 sidecar 读不到**(它只读共享卷里的文件):gate 的 `[gate_version]` 启动行、gRPC / abseil 和 librdkafka 的 stderr。
  要这些就另外上一个读 `/var/log/pods` 的 DaemonSet,Go 和 Java 的 pod 日志也走那条路(它们确实走 stdout,本文 §2.1 / §2.3 的解析段可以原样搬,标签改从 pod label 取)。
  **目前还没有这个 DaemonSet,k8s 上的 Go / Java 日志只在 `kubectl logs` 里。**
- **崩溃现场的文本同样采不到。** glibc 断言、`terminate called after throwing ...`、abort 的栈回溯都由 C 运行库直接写容器 stderr,
  不经过 muduo 的 `LogFile`。它们只存在于 `kubectl logs`,而且 **Pod 一重建就没了**(`kubectl logs --previous` 只留上一次)。
  对应地,**Loki 里的 `level="fatal"` 只覆盖代码里显式写了 `LOG_FATAL` 的分支 —— 查不到 fatal 不等于没崩。**
  崩没崩看 Pod 自己:

  ```powershell
  kubectl -n mmorpg-zone-1 get pod -o custom-columns="NAME:.metadata.name,RESTARTS:.status.containerStatuses[*].restartCount"
  kubectl -n mmorpg-zone-1 describe pod <pod>   # 看 Last State: Terminated 的 Reason / Exit Code
  ```
- `node-logs` 是 `emptyDir`,随 Pod 销毁即丢。**正常终止**这一路因为用了原生 sidecar 追得完(见 6.1);
  被强杀 / 驱逐 / 节点掉电时,还没被 sidecar 读走的尾部日志找不回来。
- 单副本 + 文件存储的 Loki 只够 dev / 小规模 staging。生产要换对象存储加多副本,
  `docs/ops/log-management.md` 的「Production (K8s)」一节是总方针。

相关设计:`docs/design/distributed-tracing.md`(日志行里带上 `trace_id` 后,可以在 Grafana 里从日志跳到链路)、
`docs/design/error-reporting.md`。

---

## 7. 验证记录

### 2026-09-14 首次全量验证

- **版本组合:** Loki 3.5.8、Alloy v1.10.0、Grafana 13.2.1。
- **全量重采:** 清空 Loki 和 Alloy 的数据卷后重新采集,Alloy 发出 7010 条,没有丢弃,没有报错。
- **逐文件对账:** 同一份配置在一次性容器里用 `loki.echo` 试跑,按"文件 × 级别"得到 45 行计数,和 Loki 实际入库逐行一致。
- **看板:** 看板共 7 个带查询的面板;其中 6 个统计 / 趋势面板的表达式经 Grafana `/api/ds/query` 执行成功,
  时间范围取 3 天时错误数 37、警告数 25,和对账数字一致。「日志流」面板当时未单独经 API 验证。
- **真实起服:** 用 `start_game.ps1`(不开客户端)起全套服务后,18 个正在写的日志文件里,能解析的最新一行都已进 Loki,最新一条约 20 秒前写入。
  gate / scene / battle 带 `zone=1`,网关日志按 JSON 解析出级别。
- **读取位置:** v1.10.0 正常停止后记录的位置等于文件大小(运行期间会滞后 10~20 秒);v1.19.2 在容器本地文件上同样出现位置回归。

### 2026-09-18 对抗式复核后的修正

一轮多维度复核 + 逐条对抗核实(25 条确认成立)之后,改了下面这些,并重新做了验证:

- **采集规则(`config.alloy`)**:etcd 客户端(zap)的 JSON 行改用 `ts` 字段解析(之前时间被 fudge 成上一条日志的时间,实测最多偏 23 小时);
  go-redis 文本行补上时间解析;librdkafka 的 `%N|epoch|` 行按 syslog 级别打 `level`、用自带时间(之前没有级别,错误面板统计不到);
  muduo 断言行打 `fatal`;上述两种行头加进多行合并的 `firstline`(之前会被并进前一条 gRPC 行);
  gRPC 行补年份改成"月日比今天晚就算去年"(之前跨年会得到未来时间被 Loki 400 拒收并丢弃);
  `sa_token.log` 的 Maven / JVM 行头加进 `firstline`(之前补读时会并进上一次运行的日志)。
- **验证方式:** 先用一份覆盖全部行型的样本目录做 `loki.echo` 试跑,逐行核对级别与时间;再对真实 `run/logs` 跑一次回归,
  29622 条、Alloy 零报错,Go 的 `warn` / `slow`、C++ 的 `error`(含 librdkafka)、Java 的 `warn` 都正确带上了级别。
- **看板:** 「错误最多的服务」条形图配色原来是反的(错误越多越绿),改成越多越红;去掉了 `severe` 这个 go-zero 永远不会输出的级别分支。
- **文档:** 本文修正了 C++ stdout 缓冲的机制与量级、`query_ingesters_within` 的说法、查不到结果的示例查询(zone 过滤、RPC 方法名)、
  手工起网关缺目录、启动方式与采集的对应关系、k8s 下 C++ 不写 stdout 等处。

### 2026-09-19 k8s C++ 日志 sidecar 审计修复(代码已落盘;kind 临时 namespace 端到端复测通过)

这一轮改的是 §6 那套:sidecar 形态、安全上下文、配置变更怎么生效、解析规则、日志保留,以及三个开关的可达性。

**已验证项(生成器侧):**

- **4 组 dry-run 退出码 0**(`infra-up` / `zone-up` / `zone-up -SceneOrchestrator agones` / `zone-up -NoCppLogSidecar`),
  生成的每个 manifest 块都经 `python yaml.safe_load_all` 解析通过(infra 12 块 / zone 6 / agones 7 / 关 sidecar 5)。
  修复前 `infra-up` 的 battle sidecar ConfigMap 块**是解析失败的**:battle 路径拿 `-` 当 zone 占位,渲染出 `mmorpg.io/zone: -`,
  YAML 把裸 `-` 当块序列项,直接语法错(加引号也过不了 k8s 的 label value 校验器)—— 这个 bug 会让 `infra-up` / `all-up` 整体中断。
  现在 zone 名为空就不写这行 label,非空时值加引号,battle 传空串;battle 的 sidecar 环境变量 `ZONE_NAME` 由 `-` 改成 `global`,
  所以 Loki 里它的 `zone` 标签值是 `global`。
- **生成物逐项核对:** `initContainers` 里的 `log-sidecar` 带 `restartPolicy: Always`、`securityContext`、`alloy-http` 端口;
  pod 模板带 12 位 config-hash 注解;`node-logs` 是 `emptyDir { sizeLimit: 2Gi }`;
  battle 的 ConfigMap 不带 `mmorpg.io/zone`,zone 的带且值有引号。
- **真跑了一遍 sidecar:** `grafana/alloy:v1.10.0` 以 `--read-only`、`--user 10001:10001`、日志目录只读挂载启动,
  吃**生成出来的**那份 `config.alloy`。结果:`Dropped log messages at ...` 独立成条且 `level=warn`;
  muduo 行拿到 `info` / `error` 与 `+08:00` 的时间戳;`filename` 出现在结构化元数据、不在标签里;
  该镜像里有 `Asia/Hong_Kong` 时区;只读根文件系统 + uid 10001 正常工作。
- **清理循环在 `ubuntu:24.04`(`deploy/k8s/Dockerfile.cpp` 的运行时基镜像)里实跑:** 12 个文件 → 只剩最近 8 个;`xargs -r` 存在。

**已验证项(kind 集群端到端复测,本轮改动之后跑的,已通过):**

做法先说清楚,否则下面的结论会被高估:临时 namespace `cpp-log-e2e`(测完已删),把 `zone-up -DryRun` **真实产出**的
`cpp-log-sidecar` ConfigMap 与 `gate` Deployment 原样 apply,只把业务容器镜像换成 `ubuntu:24.04`、
启动命令尾部的 `./gate` 换成一个按 muduo 格式写日志的循环(`args` 里 `mkdir -p` 与清理循环那段**逐字未改**);
Loki 用 `deploy/k8s/manifests/infra/loki.yaml` 原样部署在同一个 namespace。实测到的:

1. **Pod `2/2 Running`** —— 原生 sidecar 在 `initContainers` 里,同样计入 Pod Ready。
2. **真 Loki 里查得到**:`level=info` / `warn` / `error` 三类条目都在,muduo 行的时间戳按 `+08:00` 正确解析;
   `Dropped log messages at ...` 独立成条且 `level=warn`(6.9 那条刻意选择成立)。
3. **流标签集就是 6.4 那张表**:`/loki/api/v1/series` 实测为
   `{env=k8s, job=cpp_nodes, lang=cpp, level, namespace, pod, service=scene, zone=e2e}`(外加 Loki 自己派生的 `service_name`);
   **`filename` 与 `pid` 都不在流标签里**,降基数的那一步确实生效了。
4. **原生 sidecar 的语义直接实测过**:同一份 pod 模板改成裸 Pod + `restartPolicy: Never`(= Agones GameServer Pod 的形态),
   `initContainers` 里带 `restartPolicy: Always` 的容器退出后被 kubelet 反复拉起(restartCount 0→1→2→3,Pod 仍 Running);
   对照组是同一个 Never Pod 里的**普通**容器,退出后 `restarts=0`、永远停在 Terminated。这正是 6.1 说的、本轮要堵的那个洞。
5. **清理循环在集群里也成立**:业务容器里造 12 个 `.log`,5 分钟内降到 8 个;同一时刻 sidecar 的
   `loki_source_file_files_active_total` = 8 —— 旧文件的 tailer 随文件消失被释放,不会越积越多。
6. **sidecar 的 `/metrics` 从 Pod 外可达**(`0.0.0.0:12345` + containerPort `alloy-http`)。
   实测指标名是 `loki_source_file_files_active_total` / `loki_write_sent_entries_total`,**没有 `alloy_` 前缀**
   (6.6 已按实测改过);当时 `loki_write_sent_entries_total` = 66。

复测中顺带踩到一个坑,已写进 6.6:假节点一开始用 UTC 写时间,被按 `Asia/Hong_Kong` 解析成"8 小时之后",
Loki 静默丢弃 —— 默认查询窗口里查不到,sidecar 自己的日志里也没有任何报错。

**未验证项(不要当成已验证):**

- **没有用真实 C++ 镜像跑过** —— 上面的端到端复测是拿 `ubuntu:24.04` 当"假节点"写 muduo 格式的行验的。
- **没有在真实 zone / infra namespace 上 apply 过**,只在临时 namespace 和 dry-run 上做的。
- **Agones Fleet 没经过真实 Agones 准入校验**(本机 kind 没装 Agones controller,也就没有这道校验)。
- **契约测试本轮没有运行。** `tools/scripts/tests/k8s_deploy_contract.tests.ps1` 新增了 5 条用例
  (原生 sidecar 形态 + config-hash 注解、裸 `-` 的 label 值、battle 无 zone 标签 / zone 有引号、
  `-NoCppLogSidecar` 产物洁净、日志清理循环 + sizeLimit),另更新了**三条** `args` 断言
  (battle、gate / scene 的循环、Agones Fleet);Agones 那条的 DryRun 已经提成文件头的共享基线,
  Fleet 与 Deployment 两套模板的断言都挂在同一次运行上。按 `AGENTS.md` §10.1 由 Codex 跑。
- **6.3 那条查看路径里"在本机 Grafana 里加第二个数据源"那一半本轮没走过**(复测是直接打 Loki 的 HTTP API 验的),
  也没在真实集群上走过。
