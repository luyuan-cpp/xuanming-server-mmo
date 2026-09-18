# 本地日志观测台:Grafana + Loki + Alloy(C++ / Go / Java 三语言统一接入)

> **状态:** 2026-09-13 接入;2026-09-14 修正 C++ 解析规则、钉死镜像版本并逐文件对账;
> 2026-09-18 按对抗式复核结果修正采集规则与本文多处说法(见 §7)。
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

## 6. 生产 / k8s

本文只覆盖本机文件采集。搬到 k8s 前要按语言分开看:

- **Go 和 Java 走容器 stdout/stderr**,用 Alloy DaemonSet 的 `loki.source.kubernetes`(或 `discovery.kubernetes` + `loki.source.file` 读 `/var/log/pods`)即可,
  `loki.process` 里这两种语言的解析段可以原样搬过去,标签改从 pod label 取。
- **C++ 节点在 Linux 下不写 stdout。** `Node::AsyncOutput` 里调用控制台输出的那一句包在 `#ifdef WIN32` 内,
  Linux/容器里 muduo 的业务日志(包括 `LOG_FATAL` 的崩溃原因)只写 `/app/bin/logs/cpp_nodes/*.log`,
  而 `k8s_deploy.ps1` 把这个目录挂成 `emptyDir`,目前没有任何 sidecar 或 DaemonSet 读它。
  照上面的做法采集,C++ 只能拿到 gate 的 `[gate_version]` 启动行和 gRPC/abseil 的 stderr,业务日志一条都采不到。
  两条出路,代价都要先想清楚:一是加 sidecar 或把目录换成 hostPath/共享卷让 DaemonSet 读(emptyDir 随 Pod 销毁即丢,
  sidecar 还得处理 muduo 的滚动文件名);二是改代码让 Linux 也输出到控制台(要走代码修改和编译流程)。
- Loki 也要从单机文件存储换成对象存储加多副本。`docs/ops/log-management.md` 的「Production (K8s)」一节是总方针。

相关设计:`docs/design/distributed-tracing.md`(日志行里带上 `trace_id` 后,可以在 Grafana 里从日志跳到链路)、
`docs/design/error-reporting.md`。

---

## 7. 本机验证记录

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
