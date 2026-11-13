# Development Scripts

PowerShell and shell scripts for common development tasks.

### R双区Mall/Gateway四副本（2026-11-02，待实际验收）

本节属于本仓R的Go/C++业务栈。四个全局Java角色为`mall`、`mall-replica`、`gateway`、`gateway-replica`，分别使用R的`java/mall_game`和`java/gateway_node`。两个Mall副本共用同一个商城逻辑库，两个Gateway副本共用同一个网关逻辑库；每个进程独立NORMAL主体、boot身份、grant和消费目录。Gateway按zone固定本轮两区Go login的gRPC地址，不借独立Java仓的Dubbo网关。

在已有受控目录写公开JSON计划，格式`LOCAL_R_JAVA_FOUR_INSTANCES_V1`，顶层字段仅`format`、`javaExecutable`、`javaSha256`、`instances`。`javaExecutable`固定本机JDK23的绝对路径和SHA，不能拿旧进程代替。`instances`恰好四项，每项字段为：

- `instanceKey`：上面的四个固定角色名；`service`为`mall`或`gateway`。
- `port`：互不重复的显式本机HTTP端口；`database`为该服务的全局逻辑库名，两副本必须相同，Mall与Gateway两库不同。
- `jarSha256`：本轮R构建的运行包SHA。固定路径为`java/mall_game/target/mall-game-0.1.0-SNAPSHOT.jar`及`java/gateway_node/target/gateway-node-0.0.1-SNAPSHOT.jar`。
- Gateway额外必填`loginGrpcEndpoints`（如`990017=127.0.0.1:53000,990018=127.0.0.1:54000`，必须取本轮真实Go计划）、`redisEndpoint`、`etcdEndpoint`和`mallAdminUrl`；后两者仅显式`http://127.0.0.1:<端口>`根地址，商城端口必须属于同计划Mall。计划不写口令、DSN或签名密钥。

先由Codex在资源槽中构建`go/data_service/cmd/writer_authority`，并在合法Authority测试夹具准备齐备后执行R reactor `mvn -f java/pom.xml -pl gateway_node,mall_game -am clean package`；Gateway需要JDK23及显式已核protoc。保留默认测试，不用`skipTests`。`AuthorityHikariTiDbTest`缺真实NORMAL夹具即失败；Mall真TiDB随机库用例还须逐库NORMAL授权，不能用一份外部grant或root写连接冒充。

一次生成全部Go及四Java的目录，不能对每个zone/Java角色循环调用producer：

```powershell
# 这些变量是本轮已核原件/计划的路径和公开SHA；不在命令或跟踪文件里填写凭据。
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command sql-boot-catalog-local -Zones 990017,990018 -ZonePortOffsets 990017=0,990018=1000 -JavaInstancePlanPath $javaPlan -JavaInstancePlanSha256 $javaPlanSha -PhysicalAuthorityFile $physical -PhysicalAuthoritySha256 $physicalSha -LockQualificationFile $i3 -LockQualificationSha256 $i3Sha
```

调用前照常显式提供两区玩家库白名单、loopback SQL端点及原本机Schema身份。公开物理authority与真实I3原件须由执行者先实核同一物理epoch；这两个参数是原件绑定，不能替代本轮独立物理检查。四Java模式拒绝退回local自报锁声明。Mall先由独立Schema子进程调用`writer_authority -command prepare-schema -schema <商城库>`，复用共享闸门DDL；NORMAL没有DDL或目录控制权限。每boot数据源凭据只在受控私有配置落盘；公开计划不带凭据。此模式保留既有账号、旧boot和UNKNOWN原件，不自动删除。

Go/C++由同一轮各自正规入口启动后，用producer本轮`last-run.json`摘要启动四Java，JavaOnly分流不启动Docker或Go/C++，不重做目录：

```powershell
pwsh -NoProfile -File tools/scripts/start_game.ps1 -JavaOnly -JavaInstancePlanPath $javaPlan -JavaInstancePlanSha256 $javaPlanSha -JavaZones 990017,990018 -SqlBootSummaryPath $bootSummary
```

每实例先经`Get-SqlWriterBootEnvironment`，只给自身资格和固定私密文件helper，剥离Schema/registrar/maintenance控制环境，再以完整私有配置启动。每boot记录实际PID、创建FILETIME、JAR/Java SHA与端口；启动失败仅结束本批原句柄并保留尝试，不自动重试。监听就绪不是业务通过，之后仍须同轮双区业务与真实SQL故障恢复断言。玩家持久化实现没有因此修改，仍走SQL `player_store`，Redis只保留既有协调用途。

受影响验证命令由Codex在源码稳定后执行：`tools/scripts/tests/sql_boot_catalog_local.tests.ps1`、`sql_boot_catalog_java.tests.ps1`、`start_game_java.tests.ps1`；Go在`go/data_service`执行`go test -count=1 ./cmd/writer_authority`。新增脚本以无功能假产物和启动替身检查合同，不执行JVM、SQL或容器。本文是接线与待执行口径，不宣称这些命令已经通过。

## Directory Rules

- 仓库根保留 `dev.bat` 开发菜单，以及转调本目录的 `start-server.cmd` / `start-game.cmd` 薄启动器；维护中的独立工程脚本放在本目录，不把菜单本身误称为无逻辑的薄包装。
- [tools/scripts](.) is the canonical home for maintained engineering scripts.
- One-off or retired scripts go to [tools/archived](../archived) instead of staying here.
- [tools/scripts/third_party](third_party) stores third-party build and maintenance scripts, 包括独立 Protobuf、BoringSSL、GoogleTest 与 Redis 构建。
- If a top-level convenience command is needed, keep it as a thin wrapper that forwards into [tools/scripts](.) instead of copying logic.
- Do not place project-maintained scripts under vendored directories such as [third_party](../../third_party); treat those trees as upstream-owned whenever possible.

## Available Scripts

### prepare_docker_debug.py（独立 Docker 断点调试环境）

在仓库根执行 `python tools/scripts/prepare_docker_debug.py`，从当前源配置派生容器配置、初始化 SQL 与 topic 契约，并复制写者注册步骤用的脚本和角色模板，输出到 Git 忽略的 `build/docker-debug-current/`。需要本机 Python、PyYAML 和运行中的 Docker；脚本只读检查现有 Docker 子网，不构建、不启动容器、不删除数据。首次生成随机密钥，后续保留原值，不把密钥写进受管文件或输出。

HTTP 默认映射宿主 8081 与 18081。已有其他本机服务器占用时，分别通过 `--gateway-client-port`、`--gateway-port` 选择空闲端口，保留原有服务。两项参数连同 Gate、Battle 端口一起校验互异，调试客户端使用实际选定的 HTTP 地址。

预先用当前源码构建镜像后，运行 `docker compose --env-file build/docker-debug-current/.env -f deploy/debug/compose.game.yml -f deploy/debug/compose.debug.yml up -d`。镜像构建沿用 `deploy/k8s/Dockerfile.cpp`、`Dockerfile.go-svc`（上下文 `go/`、命名上下文 `tables=generated/tables`）及 `Dockerfile.java-svc`；C++ 调试层见 `deploy/debug/Dockerfile`。Compose 禁止自动拉取，初始化任务成功后才启动 16 个常驻容器。

写者注册步骤用单独的镜像（pwsh、mysql 客户端、`writer_authority`），首次要构建。基础镜像只构建一次，`writer_authority` 变了只跑第二行：

```powershell
docker --context desktop-linux build -f deploy/debug/Dockerfile.sql-boot -t local/mmorpg-sql-boot:debug-20261031 deploy/debug
python tools/scripts/rebuild_docker_debug.py go --services sql-boot
```

`db-migrate` 使用独立的 `go/db/cmd/migrate` 入口。数据库迁移工具变更后执行 `python tools/scripts/rebuild_docker_debug.py go --services db-migrate`，更新 `DB_MIGRATE_IMAGE`。这是一次性任务，重编只登记新镜像，下一次完整启动时再执行迁移。

`player-inbox` 的运行镜像同样首次要构建一次，之后重编只跑第二行（迁移任务用同一个镜像）。已有 `build/docker-debug-current` 时先重跑一次 `prepare_docker_debug.py`，补上 `player_inbox` 的配置和 `PLAYER_INBOX_IP`：

```powershell
docker --context desktop-linux build -f deploy/k8s/Dockerfile.go-svc --build-arg SERVICE=player_inbox --build-arg ENTRY=player_inbox.go --build-context tables=generated/tables -t local/mmorpg-player-inbox:debug-20261031 go
python tools/scripts/rebuild_docker_debug.py go --services player-inbox
```

写者授权与 K8s 的 registrar init 同形状，门禁与生产相同，没有 dev 旁路：

- `sql-boot` 每次 `up` 先跑：补库，生成注册和维护账号的口令、物理 authority 原件、锁资格声明（命名卷 `sql-boot-private`，口令随机，不进被跟踪文件），判定全局库要不要跑 `data_service -migrate`（它只接受空库）。全局库是半成品时直接报错，不替人删数据，用 `down -v` 重来；已有数据卷的权威表不全同样如此。
- `db-migrate`、`data-migrate`、`match-migrate`、`player-inbox-migrate` 用 root 建表。`data-migrate` 建目录 `testdb` 和流水库 `testdb_ledger`。
- `db`、`data-service`、`login`、`match`、`player-inbox`、`gateway` 前各有一个一次性的 `<服务>-sql-boot`（`service_completed_successfully`）：用 `lib/sql_boot_catalog_local.ps1` 的授权清单和 `deploy/sql` 的角色模板建这一 boot 的账号 `lw_<boot>_<服务>`，调 `writer_authority -command register`，把单次授权、运行配置和 SQL 变量写进命名卷 `<服务>-sql-writer`。常驻容器只读挂载它，由 `sql-writer-entry.sh` 取出摘要和变量再启动业务进程；消费标记写在 `<服务>-sql-consume`。
- 上一轮的账号没有连接就删，还有连接（服务还在跑）就留到下一轮。`writer_registry` 里的旧行不删，只是停用账号的历史记录。
- login 进程不连 SQL、不消费授权，这一步只为和 `go_services.ps1`、K8s 的写者清单同形状。chat、friend、guild、mail、rank、trade 不在这个栈里，其中 guild、trade、mail、friend 是角色护栏的参与者，协调者连不上它们，这个栈里过户和删角一律拒绝；要加时照 `match-sql-boot` 写一段，并在 `prepare_docker_debug.py` 的 `GO_CONFIGS` 加配置。
- `player-inbox` 收全球匹配成局的个人结算：match 用 `MMORPG_PLAYER_INBOX_SECRET_MATCH` 签名入箱，scene 用 `MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE` 签名拉取。这个栈的 `game_config.yaml` 由 `prepare_docker_debug.py` 打开 `player_inbox.consumer_enabled`（显式写，不跟着源配置变），否则结算只进箱、到不了玩家。调试端口 `127.0.0.1:2352`。

单次授权启动即消费，**`docker compose restart <写者>` 会被拒启**（`SQL writer启动grant缺失、漂移或已消费`）。要重启就连注册步骤一起重建，`rebuild_docker_debug.py` 更新写者时也是这样做：

```powershell
docker --context desktop-linux compose --env-file build/docker-debug-current/.env -f deploy/debug/compose.game.yml -f deploy/debug/compose.debug.yml up -d --no-build --pull never --no-deps --force-recreate data-service-sql-boot data-service
```

首次建栈仍使用上述 Compose 初始化流程。已完成初始化的原栈，日常停止后使用下节启动入口，避免重放初始化和迁移任务。

### start_docker_debug.py（已初始化 Docker 调试栈的日常启动）

先打开 Docker Desktop，在本仓库根目录执行：

```powershell
# 只读预检，列出将检查或启动的服务。
python -X utf8 -B tools/scripts/start_docker_debug.py

# 每次手动开服使用这一条；同一次调用先预检，再按依赖顺序恢复原容器。
python -X utf8 -B tools/scripts/start_docker_debug.py --execute
```

入口仅操作 `desktop-linux` 上的 `xuanming-debug-current` 原容器，要求基础初始化和写者注册任务已成功完成。16 个常驻服务及五个写者注册容器须与当前 `.env` 指定的镜像、挂载、端口及显式环境变量一致；不会重跑的历史初始化、迁移和 login 注册容器只核对成功状态与实际身份、配置及挂载基线，不要求追溯更新成最新业务镜像。正常运行的服务只检查，不重复启动或注册；停止的 `db`、`data-service`、`match`、`player-inbox`、`gateway` 先重新执行各自原注册容器，取得新授权，再启动服务。login 不消费 SQL 授权，不重跑它的注册任务。所有服务已经正常运行时，重复执行只做检查。

现有容器使用手绘节庆地图时，日常启动会从容器的 Compose 标签识别部署批次的 `compose.authored.json`。预检逐项核对六个服务的开发表只读挂载和三个 C++ 节点的导航只读挂载，并将这些文件与覆盖配置的摘要纳入启动基线。该部署批次的配置和表目录需要保留。

每次调用独立保存 `build/verification/docker-start-<UTC>/`，包含启动计划、旧退出状态和结果。Compose 显式卷核对原卷名与读写属性；镜像声明的默认匿名卷核对原身份、目标、匿名标签、驱动及存在状态，所有实际挂载卷均纳入前后不变的基线，不要求匿名卷带 Compose 项目标签。脚本不构建、不拉取镜像、不重建容器、不清卷、不重放初始化或迁移；任一步失败立即停止并显示脱敏原因，保留已启动的服务及失败证据，不自动重试。成功输出 `Server startup completed.`，表示运行就绪检查完成；源码一致性仍由 VS Code 附加前检查执行，不能据此断言旧崩溃原因已修复。

默认客户端入口为 `http://127.0.0.1:8081`，同一网关另保留 `http://127.0.0.1:18081`；gate 为 `127.0.0.1:28000`，battle 为 `127.0.0.1:28010`。可用 `--gateway-port` 调整附加 HTTP 端口、用 `--gate-port` / `--battle-port` 调整对应入口；`8081` 保留给客户端默认网关。VS Code 附加到固定名称的三个调试容器。此栈只启用登录、建角、主世界进场、战斗与全球匹配结算所需服务，未启动域的请求按现有路由契约失败。校验配置时使用 `docker compose ... config --quiet`，不要把展开后的配置与 `.env` 打印或上传。

### compose.redis-cluster.yml(玩家域 Redis Cluster 本机开发版)

3 主 3 从,8.10.x,端口 7100-7105,与 K8s 清单同一个镜像摘要。真集群验证用(设计 `docs/design/redis-player-store-cluster.md` §14.2)。开发时不需要它:不写 `PlayerRedis` / `player_redis` 就是单实例。

```powershell
docker --context desktop-linux compose -f deploy/compose.redis-cluster.yml up -d
docker --context desktop-linux wait redis-player-cluster-init    # 打印 0 才算建好
docker --context desktop-linux logs redis-player-cluster-init    # 最后一行 redis player cluster verified: 16384 slots covered
docker --context desktop-linux compose -f deploy/compose.redis-cluster.yml down -v
```

不要用 `up --wait`:一次性容器退出会被它判成失败。节点共用一个网络命名空间,宣告 `127.0.0.1:710N`,所以只有宿主机上的进程能连,`deploy/debug` 的调试栈连不上它。

**把本机服务指过去**,只改跑在宿主机上的那几份配置,改完重启服务:

```yaml
# go/<服务>/etc/*.yaml 的 PlayerRedis(login、player_locator、scene_manager、match、friend、guild、trade、player_inbox、mail)
PlayerRedis:
  Addrs: [127.0.0.1:7100, 127.0.0.1:7101, 127.0.0.1:7102]
  Cluster: true
  PoolSizePerNode: 20
# go/login/etc/login.yaml 的 LoginRedis 同样结构(只有 login 有这一段:其余服务只认 gate 签名的会话身份原件,不读登录态)。DB 必须是 0 或不写。
LoginRedis:
  Addrs: [127.0.0.1:7100, 127.0.0.1:7101, 127.0.0.1:7102]
  Cluster: true
```

```yaml
# bin/etc/game_config.yaml:scene 连玩家域。开发集群没有口令。
player_redis:
  mode: cluster
  seeds: ["127.0.0.1:7100", "127.0.0.1:7101", "127.0.0.1:7102"]
```

这些改动不要提交。连上之后 login 启动横幅有 `player redis: addrs=127.0.0.1:7100,... cluster=true`,scene 日志有 `Player Redis: mode=cluster`。

清库:`pwsh -File tools/scripts/dev_tools.ps1 -Command redis-wipe-local -ConfirmWipeAllRedis`,单实例加开发集群的每个主节点(`lib/redis_local_flush.ps1`)。`FLUSHALL` 在 Cluster 里只清本节点,不要只对一个节点执行。压测快照的 target 写 `docker-cluster:redis-player-cluster-0:7100`(`stress_snap.ps1`)。

压测抓本机 C++ 节点的 `/metrics`(2026-11-12 起):C++ 进程设了 `MMORPG_METRICS_PORT` 才开出口,本机缺省不开。起节点前设 `$env:MMORPG_METRICS_BASE_PORT = '19400'`(或 `cpp_nodes.ps1 -MetricsBasePort 19400`),每个实例按 `lib/cpp_metrics_ports.ps1` 拿一个端口(gate 19401…、scene 19501…、battle 19601…,`-Zone N` 再加 1000 × N);`stress_snap.ps1 -CppMetricsBasePort 19400` 按 `run/pids/cpp_nodes.pid.json` 逐个抓成 `t<N>m_cpp_<实例键>.txt`,`stress_summarize.ps1` 的 N1 表读它们。契约测试 `tests/stress_cpp_metrics.tests.ps1`。

### redis_cluster_capacity.ps1(Redis Cluster 容量基准:玩家域 redis-player、控制 redis-control)

逐档加压,按 `INFO` 求差出每个主节点的 ops 和主线程 CPU,判定有没有真的压到饱和(设计 §14.3)。副本数、代理数、连接数都不是证据。

```powershell
# 只看将要执行什么,不连任何东西
pwsh -File tools/scripts/redis_cluster_capacity.ps1 -Target k8s-cluster:mmorpg-stress-a/redis-player -DryRun
# K8s 压测环境。压测端是单独的 Pod,跑完删除
pwsh -File tools/scripts/redis_cluster_capacity.ps1 -Target k8s-cluster:mmorpg-stress-a/redis-player -ClientSteps 100,200,400,800,1600 -StageSeconds 120
# 本机开发集群,只验证脚本:压测端和节点同机,判定不会算作容量证据
pwsh -File tools/scripts/redis_cluster_capacity.ps1 -Target docker-cluster:redis-player-cluster-0:7100 -ClientSteps 50,200 -StageSeconds 30
```

- 退出码:0 = 已饱和,可作证据;2 = 没饱和或压测端与节点同机,不能作证据;1 = 失败(拓扑中途变化、节点重启、压测端提前退出)。
- 三条都成立才算饱和:最后一档每个主的主线程 CPU ≥ `-SaturatedCpuPercent`(缺省 90)、总 ops 比上一档增长 < `-PlateauGrowthPercent`(缺省 5)、错误回复不超过千分之一。没到就加 `-ClientSteps` 的上限、`-Threads`、`-Pipeline`,或换更大的压测端。
- 结果在 `-OutDir`(缺省 `build/redis-capacity-<时间>`):`capacity.txt`、`capacity.json`、每档每节点的 `INFO` 原文。
- K8s 的 namespace 必须匹配 `^mmorpg-stress-`:它往集群里写键、把主线程压满。键前缀 `capbench:`,全部带 600 秒 TTL。
- 负载是一段 Lua(写 location、写会话 hash、铸代次),只是玩家域写路径的近似;压控制集群(`-Target k8s-cluster:<ns>/redis-control`)用同一段负载,得到的是每主节点的同口径基线。端到端结论走 AGENTS.md §6 的压测流程。
- K8s 目标的口令 Secret 缺省取 `<statefulset>-auth`(`redis-player-auth`、`redis-control-auth`),要别的用 `-AuthSecret`;压测端与 `app=<statefulset>` 的 Pod 反亲和。

### start_game.ps1（本机游戏一键启动）

电脑重启后双击仓库根目录 `start-server.cmd`；双击 `start-game.cmd` 可同时打开客户端。
脚本复用现有 `dev_tools.ps1` 和编译产物，依次启动本机 Docker、数据库依赖、已配置并具备产物的 Go 服务（含 client_rpc_router；可选服务按前置检查决定）、Java 网关、一区 gate / scene / battle，并等待就绪。关闭启动窗口后服务继续运行；再次双击会复用已有实例。

- 网关：`http://127.0.0.1:8081`，一区；默认客户端：同级工作目录 `tmp/showcase_player/mmorpg.exe`。
- 指定客户端：`pwsh -File tools/scripts/start_game.ps1 -OpenClient -ClientPath "E:/其他目录/mmorpg.exe"`。
- 只检查运行文件：`pwsh -File tools/scripts/start_game.ps1 -CheckOnly`。
- 前置：PowerShell 7、Java 23+、本机 Docker Desktop、已缓存镜像与游戏编译产物；日常启动不执行构建或镜像下载。
- 写者授权：第 3 步每次都重新生成（见下一节）。第 1 步要求 `bin/go_services/` 下有 `writer_authority.exe`、`db_migrate.exe`、`private_control_file.exe`，由不带 `-GoServices` 的 `dev_tools.ps1 -Command go-svc-build` 一并编出。
- 日志：`run/logs/game-launcher/<时间>/`；服务日志仍位于 `run/logs/go_services` 和 `run/logs/cpp_nodes`。
- 旧 PID 会核对程序完整路径，失效记录先备份。Docker 停止时，已知遗留通信目录与独立 socket 会重命名备份；不删除数据卷、账号或角色。
- 失败时显示原因与日志位置；已经启动的服务保留，解决原因后可重新双击。

#### 本机写者启动目录（`sql-boot-catalog-local`）

常驻 SQL 服务（db、data_service、login、chat、friend、guild、mail、rank、match、trade、player_inbox）和 Java 网关只认写者启动目录 `MMORPG_SQL_BOOT_CATALOG`：每个实例一份单次启动授权，启动即消费，账号也是每次一换（`writer_registry.sql_principal` 永久唯一）。这条命令在本机生成它，只接受回环数据库（`127.0.0.1`、`localhost`、`[::1]`），其他地址一律拒绝。Go、C++、Java 都没有 dev 旁路，`Get-SqlWriterBootEnvironment` 的校验没有放宽。C++ 节点不碰 SQL，不需要授权。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command sql-boot-catalog-local            # 全部写者
pwsh -File tools/scripts/dev_tools.ps1 -Command sql-boot-catalog-local -GoServices db,data_service
```

`start_game.ps1` 每次启动自动跑，不用手动。直接用 `go-svc-start` / `go-svc-start-exe` 时先跑它，再按最后一行输出设 `MMORPG_SQL_BOOT_CATALOG` 和 `MMORPG_DB_ALLOWED_DATABASES`；服务每启动一次都要重新生成。

前置：

- 编译产物：`proto-gen-run` 之后的 `go-svc-build`（不带 `-GoServices`），网关要用当前源码打的 jar。
- 数据库：默认用 compose 的 `mysql` 容器，SQL 走 `docker exec`，主机不用装客户端。容器发布的端口要和端点一致。
- root 账号：默认取 `go/db/etc/db.yaml` 的 `ServerConfig.Database.User` / `Passwd`（与 compose 的 `MYSQL_ROOT_PASSWORD` 同值）；可用 `MMORPG_LOCAL_SQL_ROOT_USER`、`MMORPG_LOCAL_SQL_ROOT_PASSWORD` 覆盖。容器外连入时主体默认是 `<user>@%`，不一样就设 `MMORPG_SQL_SCHEMA_PRINCIPAL`。
- 本机 TiDB 或其他端口：`$env:MMORPG_GLOBAL_AUTHORITY_TIDB_HOST='127.0.0.1:4000'`，PATH 上要有 `mysql.exe`，root 要有口令（match 迁移拒绝空口令）。档位自动探测：MySQL 用 `LOCK_TOUCH`，TiDB 用 `SELECT_ONLY`。
- 仓库在固定 NTFS 盘：授权文件要落私有 ACL。

步骤：建库 → 建表（go/db `cmd/migrate`、data_service `-migrate` 仅空库、其余服务 `-migrate`；账号都是 root，常驻账号没有 DDL）→ 按 `deploy/sql` 模板给每个实例建账号 `lw_<boot>_<实例>`，口令本机随机 → 每实例调一次 `writer_authority -command register`（单条目 roster，实例要写的每个逻辑库一份 Request，闸门跟着库走）→ 写目录，用 `Get-SqlWriterBootEnvironment` 自检。可选服务（chat、friend、mail、rank、trade、player_inbox）建表失败只跳过它们，必需服务失败就中止。

产物都在 `run/secrets/sql-boot-local/`（`.gitignore` 的 `run/*` 覆盖，目录 ACL 只留当前账号、SYSTEM、Administrators），不打印口令：

| 路径 | 内容 |
|---|---|
| `catalog.json` | `MMORPG_SQL_BOOT_CATALOG` 指向它。已在运行的实例不发新授权（go_services 会 skip） |
| `last-run.json` | 本轮摘要：注册 / 运行中 / 跳过的实例，网关用的 `private_control_file` 路径与 SHA |
| `boots/<boot>/<实例>/` | `grant/`（单次授权）、`runtime/`（`-f` 用的私有配置，含账号）、`env/`（白名单 SQL 变量）、`consume/`（消费标记） |
| `operators.json` | 注册与维护账号（`mmorpg_local_registrar`、`mmorpg_local_maintenance`）及口令 |
| `runtime-accounts.json` | 各实例账号的记录，下一轮据此清理 |

另有 `%LOCALAPPDATA%\mmorpg\sql-boot-local\private_control_file.exe`：Java 在 Windows 上经它读写私有文件，并要求 exe 的整条路径不能被非受信主体替换，仓库目录一般过不了。

重来：

- 只换授权：重跑命令。已停实例的账号和旧 `boots/<boot>` 会清掉；运行中实例的账号保留，`writer_registry` 里的旧行不删（只是停用账号的历史记录，不影响新实例）。
- 报 `has tables but lacks the authority set`：`mmorpg_global`、`mmorpg_global_ledger` 是旧布局或半成品（一个是空的、一个有表也算），`data_service -migrate` 只跑空库，脚本不替人删数据。项目未上线，先停服（`dev_tools.ps1 -Command dev-stop`，网关另停），再 `docker --context desktop-linux compose -f deploy/docker-compose.yml --profile redis-cluster down -v` 清掉本机卷（含 Redis、etcd、Kafka），删掉 `run/secrets/sql-boot-local`，重新启动。

已知限制：

- mail、rank 的生产者签名密钥由 `go_services.ps1` 注入本机开发值（`run/secrets/producer-dev.env`、`mail-admin-producer-dev.env`、`assetop-dev.env`），调用方已设置的值优先。
- 角色护栏的九把密钥（`MMORPG_ROLE_FENCE_SECRET_*`）同样由 `go_services.ps1` 注入本机开发值（`run/secrets/role-fence-dev.env`，`lib/role_fence_dev_secret.ps1`），data_service 拿全部，参与者拿自己和协调者那把，chat、go/db 只拿自己那把。
- 本机目录 K=1（`go/data_service/etc/data_service.yaml` 的 `Directory.LogicalShards`），`MMORPG_DB_ALLOWED_DATABASES` 里的每个存档库都用 `db_migrate` 迁移，data_service 的写资格覆盖全部存档库。
- 不带 `-Zones` 时只管单区、每服务一个实例（`start_game.ps1` 的形态）。多区用法见下面。
- 目录只发 Go 写者和单区网关。Java 版（`D:\luyuan\xuanming-server-mmo`）的 4 个写者角色由 Java 仓自己的 producer 生成，这个工具不生成，多区模式也不发网关条目。
- Windows 保留端口让服务挪了端口时，生成目录和启动之间端口变了会明确拒启（`ListenOn` 对不上），重跑即可。
- 这条命令只管本机进程。Docker 调试栈在容器里注册，见 prepare_docker_debug.py 一节。

多区一次生成（只能一次给全部区：循环调用会替换目录，只剩最后一区）：

```powershell
# 存档库按 go/login 的 Placement 推出(dev 地域 0、K=1,只有 player_store_1000000_db),多区共用;zone 库已退役(A17c)。
# 白名单可以不给;给了就必须恰好是这几个库,生成和启动用同一个值
$env:MMORPG_DB_ALLOWED_DATABASES='player_store_1000000_db'
# 在 pwsh 会话里用 & 调用：-Zones 是整数数组
& tools/scripts/dev_tools.ps1 -Command sql-boot-catalog-local -Zones 990017,990018 -ZonePortOffsets '990017=0,990018=1000'
# 按最后一行输出设 MMORPG_SQL_BOOT_CATALOG，再每区启动一次，Zone、偏移、GoCounts、GoPortStride 都和生成时相同
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-start-exe -Zone 990017 -ZonePortOffset 0 -LocalMySQL
pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-start-exe -Zone 990018 -ZonePortOffset 1000 -LocalMySQL
```

- 区号小（如 1、2）时不用 `-ZonePortOffsets`，端口仍按 `(Zone-1)*ZonePortShift` 算。990017 这类语义区号代入公式会越界，必须显式给每个区的偏移，区号本身不变。
- 建库前先校验：实例键唯一、配置在 `run/etc/go_services` 下、`ListenOn` 等于计划端口、配置区号等于目标区、全部监听和 metrics 端口不冲突、账号名不碰撞。任何一项不符就拒绝，不建库。
- 不像单区那样跳过可选服务：任何一区规划失败、任何服务建表失败或注册失败，都不发布目录。旧目录在开始时已删除，需要修好后重跑。
- 协调者 data_service 的 `RoleFence` 直连端口按同区计划的实际端口回填。缺参与者或参与者多副本时拒绝，多副本要改用 Etcd 形式。
- 启动时区号或端口和目录对不上会拒启，不改已签配置。login、guild 的区号写在 `Node.ZoneId`，计划和启动用同一个读法。

### dev_tools.ps1

Main entry point for tool commands on Windows.

Supported commands include:

- `help`
- `proto-gen-build` (alias: `pbgen-build`)
- `proto-gen-run` (alias: `pbgen-run`)
- `tree`
- `naming-audit`
- `naming-apply`
- `third-party-lock-check`（默认只读校验，`-ApplyPatches` 恢复登记补丁）
- `third-party-zlib-build`（隔离构建 Debug/MDd zlib 并执行上游测试）
- `third-party-protobuf-build`
- `no-raw-pointer-setup`
- `iwyu-run`
- `sql-boot-catalog-local`（本机写者启动目录，见 start_game.ps1 一节）
- `db-move`（不停服搬逻辑库，见 `docs/ops/db-move-runbook.md`）
- `k8s-*`

Proto generator naming references:

- Current naming-state snapshot: `../docs/proto_gen_naming_audit.md`
- Compatibility boundary and migration rules: `../docs/proto_gen_naming_migration.md`

### no-raw-pointer-setup（裸指针成员检查器）

在仓库根目录执行：

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command no-raw-pointer-setup
```

自动下载固定版本 LLVM/Clang 23.1.1 Windows x64 **开发包**，校验固定 SHA256，
解压到 `build/deps/llvm-23.1.1`，然后用本机 Visual Studio 和 CMake 串行构建
`cpp/plugin/build/Release/no_raw_ptr_check.exe`。现有 MSBuild 钩子会自动发现该文件。
开发包约 860 MiB，首次下载和解压需要时间与数 GB 磁盘空间；重跑复用开发库并增量编译。
下载中断保留 `.part` 文件，下次续传。校验失败会明确报错，不运行未校验的开发包。
下载及构建产物均在已忽略的构建目录，不安装系统软件、不修改全局 PATH。
LLVM 官方静态库还需要 zlib、zstd、libxml2；入口同样按固定版本和 SHA256 准备它们，
编译到 `build/deps/llvm-23.1.1-support`，已有完整产物则复用。DIA SDK 自动取自本机 Visual Studio。
编译后自动验证合法成员、裸指针拒绝、解析失败拒绝和第三方源码排除边界。
同时验证真实构建钩子的含空格路径、成功缓存和失败时清除旧缓存。

第三方源码按完整目录排除：`third_party/` 与 `cpp/libs/engine/muduo_windows/`，
包括被业务源码包含的库头文件。Windows 路径兼容大小写与两种分隔符；
`third_party_adapter` 等近似目录仍检查。排除依据是成员声明所在源码，
项目类中指向 `muduo::EventLoop`、`protobuf::Service` 等类型的裸指针仍会报告。
独立工具与备用 clang-query 使用相同边界；检查规则更新会让成功缓存失效。
工程检查还会保留 CMake `/external:I` 外部头目录，并拒绝 clang-query 的解析错误，
即使它同时输出 `0 matches`、退出码为 0。

- 预览路径与下载地址：增加 `-DryRun`。
- 只准备开发库：增加 `-DownloadOnly`。
- 已有完整开发库：增加 `-LlvmRoot 'E:/llvm-sdk'`，跳过下载。
  未指定时也会检查 `LLVM_ROOT` 环境变量、`Program Files/LLVM` 和原工程的 `D:/game/llvm`。
- 前置：PowerShell 7、Windows x64、Visual Studio C++ 工具、CMake、系统 `curl.exe` 与 `tar.exe`。
  CMake 不在 PATH 时自动查找 Visual Studio 内置版本。
- 普通 LLVM 安装器不能替代此开发包。官方包说明见
  [LLVM 23.1.1 Release](https://github.com/llvm/llvm-project/releases/tag/llvmorg-23.1.1)。
- 构建检查器成功不等于项目静态检查通过；项目扫描结果以之后的 MSBuild 检查输出为准。

### iwyu_run.ps1 / iwyu_run.sh

Cross-platform include hygiene entrypoint.

- Generates `compile_commands.json` under `<node>/build_iwyu`
- Uses `include-what-you-use` when available
- Falls back to `clang-tidy` with `misc-include-cleaner`
- Supports dry-run and fix mode
- Supports fast mode with changed files only (`-ChangedOnly`)
- Supports batch node scan (`-NodePath` accepts multiple values)
- In `-ChangedOnly` mode, if no C/C++ files changed under a node, that node is skipped without full-scan fallback

Windows:

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene
pwsh -File tools/scripts/dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/gate -IwyuTool clang-tidy -FixIncludes
pwsh -File tools/scripts/dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene,cpp/nodes/gate -ChangedOnly
```

Linux/macOS:

```bash
tools/scripts/iwyu_run.sh -NodePath cpp/nodes/scene -Tool auto
tools/scripts/iwyu_run.sh -NodePath cpp/nodes/gate -Tool clang-tidy -Fix
tools/scripts/iwyu_run.sh -NodePath cpp/nodes/scene,cpp/nodes/gate -ChangedOnly
```

Dependencies:

- `pwsh`
- `cmake`
- `clang-tidy` (required)
- `include-what-you-use` (optional but preferred when available)

### check_log_text_english.py（日志文案英文守卫）

AGENTS.md §2 规定服务和脚本的日志文案用英文。本脚本静态扫描 go / cpp / java / robot / tools / deploy，
发现直接写在日志、控制台输出、panic / sys.exit 调用里的中文字面量就以退出码 1 失败；CI 由
`.github/workflows/log-text-english.yml` 在每次提交上运行。覆盖范围、不在范围的内容（返回的错误、
throw、注释、测试、生成代码等）与例外登记方式只写在脚本头注释里。

```bash
python3 tools/scripts/check_log_text_english.py
python3 -m unittest discover -s tools/scripts/tests -p 'test_log_text_english*' -v
```

确实要保留中文的例外登记在 `tools/scripts/log_text_english_allowlist.txt`（路径、原文片段、原因三列），
修好之后要删掉对应行，否则检查会报「stale allowlist entry」。

### 第三方版本固定与补丁恢复

版本表及补丁清单在 [third_party/releases.lock.json](../../third_party/releases.lock.json)。
当前固定 20 个顶层子模块：17 个正式 release，3 个没有正式 release 的依赖保留当前提交。
父仓的 gitlink 才是真正的版本锁；`.gitmodules` 的分支配置不能代替提交锁。
不要使用 `git submodule update --remote` 或逐库 `git pull`，否则会越过固定提交。

首次检出或按父仓更新依赖后，先运行同步脚本，再恢复已登记的兼容补丁：

```powershell
pwsh -File tools/archived/sync_submodules.ps1
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-lock-check -ApplyPatches

# 日常只读检查，不修改子模块或补丁。
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-lock-check
```

Linux 同步入口为 `bash tools/archived/sync_submodules.sh`，之后执行
`python3 tools/scripts/third_party/verify_release_lock.py --apply-patches`。
校验器检查清单路径、实际提交与已登记补丁；包装入口保留 Python 的原生退出码。
默认校验发现缺少补丁也会失败，不会静默应用。升级依赖须同时复核并更新 gitlink、固定清单和兼容补丁。

Redis 8.10.2 的登记补丁保留 hiredis DNS 测试的错误码与文本检查，Windows 预期文本改由
`gai_strerror(EAI_NONAME)` 生成，适配系统语言；其他平台保留原有断言。
sol2 v3.3.0 的引用 `optional::emplace` 补丁回补上游已有的引用重绑定修复，使用
`std::addressof` 支持重载取地址运算符，并拒绝临时对象，避免 const 引用悬垂。
librdkafka v2.16.0 的补丁同时保留 Windows 工程调整与重入组变参修正：先完成格式化，
再以 `"%s"` 传递原因，避免读取未初始化的 `va_list` 或将它误传给普通变参函数。

Linux 的 curl 与 librdkafka 统一使用独立固定提交的 BoringSSL 头文件及静态归档；C++ 不依赖官方 gRPC SDK。
不要将这些头文件与系统 OpenSSL 库混用。librdkafka 的 BoringSSL 补丁适配密码学随机数、
HMAC 声明及 PKCS#12 证书、私钥和证书链加载；构建将缺失函数声明视为错误。
独立 `third_party/openssl` 固定源码版本，本构建流程不安装或使用它。

版本宏与运行字符串须按上游实际记录，版本来源以清单中的官方 tag 和精确 SHA 为准。
curl 的 `curl-8_23_0` 源码仍标为 `8.23.0-DEV`（数字 `0x081700`）；官方发布脚本在生成
tarball 时才准备发布版本头。直接从该 tag 构建时保留真实字符串，校验数字版本与源码宏一致，
不改头文件伪装输出。sol2 的 `v3.3.0` 源码版本宏仍为 `3.2.3`，同样不能据此改写锁定源码或冒称另一版本。

### third_party/refresh_boost_headers.py（复制聚合头）

Boost 1.92 的头文件分布在直属模块的 `include/boost/`，单独初始化顶层 gitlink 不会生成
工程引用的 `third_party/boost/boost/`。先按 Boost 父提交初始化全部内部子模块，再运行普通复制：

```powershell
git -C third_party/boost submodule update --init --recursive
$boostArchive = "build/verification/boost-copy-$(Get-Date -Format yyyyMMdd-HHmmss)"
python tools/scripts/third_party/refresh_boost_headers.py --archive $boostArchive
```

Linux 对应使用 `python3`，`--archive` 同样必须指定本轮全新目录。
归档目录必须位于本仓库内、Boost 源码树外且与目标在同一文件系统；不能复用已有目录。
工具按 `.gitmodules` 与父 gitlink 校验模块 HEAD，只复制直属 `include/boost/` 中的普通文件，
不扫描测试或示例，也不创建符号链接、目录联接或硬链接。
完整文件集合、SHA256 与 `BOOST_VERSION=109200` 核查通过后，旧聚合树才移入归档的
`previous-boost/`，再发布新树；失败保留现场，不删除旧树。
归档同时保存 `modules.json`、`files.sha256.json` 和成功或失败回执。
干净 CI 必须在子模块初始化后、Docker 的 Boost 聚合头 `COPY` 前完成这一步。
标准 `tools/scripts/build_linux.sh` 在未指定 `--skip-deps` 时，会在初始化全部依赖后自动执行
固定清单校验、登记补丁恢复和本轮 Boost 聚合头复制；`--dry-run` 只显示计划，不创建归档目录。
Docker builder 保持 `--skip-deps`，复用 CI 已准备的源码和 deps 阶段产物，不读取 `.git`。

### third_party/build_zlib.ps1

只使用已安装的 Visual Studio/MSVC、CMake 与 Ninja，不安装工具。
固定源码构建 Debug/MDd，串行编译，全部上游 ctest 通过后才安装。
`-BuildRoot` 必须是仓库内的全新目录；默认 `build/third_party/zlib-v1.3.2-dbg`，
产物分别位于其 `build/` 与 `install/`，安装目录含 `lib/zlibd.lib`、`lib/zlibstaticd.lib` 与 `bin/zlibd.dll`。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-zlib-build -BuildRoot build/third_party/zlib-v1.3.2-dbg -Jobs 1
```

### 独立 C++ 依赖构建

Protobuf 35.1、Abseil、GoogleTest 1.17.0、BoringSSL 都是独立子模块，提交固定于
`third_party/releases.lock.json`。Protobuf 自带 utf8_range；不再构建官方 gRPC、插件或官方桩。
构建入口只使用已有 MSVC 14.51、CMake 和 VS / Ninja，串行执行，不安装或升级工具。
MSVC 缺少英文诊断包时自动选择已有 VS 生成器，避免 Ninja 的本地化依赖前缀转码失真；
VS / Ninja 使用不同构建缓存目录，安装前缀保持相同。

```powershell
# zlib 安装目录需与目标配置、架构一致；下面复用已有的 Debug 安装。
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-protobuf-build -Configuration Debug -ZlibInstallRoot build/third_party/zlib-v1.3.2-dbg/install
pwsh -File tools/scripts/dev_tools.ps1 -Command third-party-boringssl-build -Configuration Debug
pwsh -File tools/scripts/third_party/build_gtest.ps1 -Configuration Debug
```

默认 `x64 Debug` 使用 `/MDd`，安装前缀分别是各子模块内的 `install_vs2026_dbg`；
`-Configuration Release` 使用 `/MD` 与 `install_vs2026_rel`；`-Architecture x86` 使用
`install_vs2026_x86_dbg/rel`。不能跨配置或架构混用库。Protobuf 安装包含同批 Abseil 和 utf8 库。
`-BuildRoot` 可将构建及安装放进仓内 ignored 路径；它不会自动发布到默认前缀。

Linux 由 `tools/archived/setup_dependencies.sh` 先准备 zlib，再调用
`third_party/build_linux_sdk.sh` 构建独立序列化与 TLS 依赖；Docker 使用主仓子模块源码，
不再 clone 官方 gRPC 仓库。SDK Debug 来源清单升级为 schema 2，字段 `dependencySource`
必须完整包含独立 Protobuf、Abseil、BoringSSL 的提交、gitlink 和文件摘要；旧 schema 1 不接受。

#### Commands

```powershell
# Show command help
pwsh -File dev_tools.ps1 -Command help

# Build proto-gen
pwsh -File dev_tools.ps1 -Command proto-gen-build

# Run proto-gen with default config
pwsh -File dev_tools.ps1 -Command proto-gen-run

# 构建独立 Protobuf
pwsh -File dev_tools.ps1 -Command third-party-protobuf-build

# Run include cleaner (auto tool selection)
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene

# Force clang-tidy mode and auto-fix include issues
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene -IwyuTool clang-tidy -FixIncludes

# Fast mode: only check changed files in current diff range
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene -ChangedOnly

# Batch scan multiple nodes
pwsh -File dev_tools.ps1 -Command iwyu-run -NodePath cpp/nodes/scene,cpp/nodes/gate -ChangedOnly

# 在 VS Code 中构建独立 Protobuf
# Run Task -> third_party: protobuf build

# Run proto-gen with custom config
pwsh -File dev_tools.ps1 -Command proto-gen-run -ConfigPath "<path-to-config>"

# Generate project tree report
pwsh -File dev_tools.ps1 -Command tree

# Audit recursive naming issues (default snake_case)
pwsh -File dev_tools.ps1 -Command naming-audit

# Apply recursive naming normalization
pwsh -File dev_tools.ps1 -Command naming-apply

# Use kebab-case style with a capped batch size
pwsh -File dev_tools.ps1 -Command naming-audit -Style kebab -MaxChanges 200

# Open one k8s zone
pwsh -File dev_tools.ps1 -Command k8s-zone-up -ZoneName yesterday -ZoneId 101 -OpsProfile managed-cloud -NodeImage "<image>" -WaitReady

# Open all zones from JSON/YAML config
pwsh -File dev_tools.ps1 -Command k8s-all-up -ZonesConfigPath deploy/k8s/zones.ops-recommended.yaml -OpsProfile managed-cloud -NodeImage "<image>" -WaitReady

# Check/close k8s zones
pwsh -File dev_tools.ps1 -Command k8s-zone-status -ZoneName yesterday
pwsh -File dev_tools.ps1 -Command k8s-zone-down -ZoneName yesterday
pwsh -File dev_tools.ps1 -Command k8s-all-status -ZonesConfigPath deploy/k8s/zones.yaml
pwsh -File dev_tools.ps1 -Command k8s-all-down -ZonesConfigPath deploy/k8s/zones.yaml

# 独立 Release 构建需匹配的 Release zlib
pwsh -File dev_tools.ps1 -Command third-party-protobuf-build -Configuration Release -Clean -Jobs 1 -ZlibInstallRoot "<Release zlib prefix>"

# Preflight/build/push/release runtime image for k8s
pwsh -File dev_tools.ps1 -Command k8s-stage-runtime -BinarySourceRoot D:/linux-build/bin -ZoneInfoSource bin/zoneinfo -TableSource generated/tables
pwsh -File dev_tools.ps1 -Command k8s-image-preflight
pwsh -File dev_tools.ps1 -Command k8s-build-image -ImageRepository ghcr.io/luyuancpp/mmorpg-node -ImageTag v1
pwsh -File dev_tools.ps1 -Command k8s-push-image -ImageRepository ghcr.io/luyuancpp/mmorpg-node -ImageTag v1
pwsh -File dev_tools.ps1 -Command k8s-release-zone -ZoneName yesterday -ZoneId 101 -ImageRepository ghcr.io/luyuancpp/mmorpg-node -ImageTag v1 -WaitReady
```

### normalize_names.ps1

Recursively normalizes file and directory names to `snake_case` or `kebab-case`.

- Excludes `third_party` by default
- Also skips tooling/build artifact roots by default: `.git`, `.vs`, `.idea`, `.vscode`, `_copilot_session_transfer`, `x64`, `bin`, `generated`, `cpp/generated`, `go/generated`, `cpp/bin`
- Skips embedded muduo vendor trees under `cpp/libs/*/muduo_windows`
- By default only scans source roots: `cpp`, `go`, `java`, `proto`, `docs`, `tools`, `scripts`, `data`, `deploy`, `robot`, `test`, `etc`
- Skips dotfiles and dot-directories to avoid renaming repository metadata files
- Skips high-risk generated/IDE files: `*.vcxproj*`, `*.sln*`, `*.tlog`, `*.recipe`, `*.pb.{h,cc,go}`
- Supports conflict detection (duplicate targets / existing target path)
- Uses deep-first rename order to avoid parent path conflicts

Recommended workflow:

```powershell
# 1) Dry run first
pwsh -File tools/scripts/dev_tools.ps1 -Command naming-audit

# 2) Apply in small batches for safer rollout
pwsh -File tools/scripts/dev_tools.ps1 -Command naming-apply -MaxChanges 100

# 3) Build and run tests after each batch

# Optional: run against all non-excluded paths (advanced)
pwsh -File tools/scripts/normalize_names.ps1 -Mode audit -IncludeRelativePaths @()
```

### tree.ps1

Generates a tree structure report of the project directory.

```powershell
pwsh -File tree.ps1
```

### k8s_deploy.ps1

Kubernetes-only deployment entrypoint used by `dev_tools.ps1`.

- Supports single-zone and multi-zone one-click open-server flows.
- 按 infra / zone 职责部署基础设施、Go 服务、Java 网关和 C++ gate/scene；battle 为全局池，Centre 已退役。Go/Java 部署需显式提供各自镜像仓库参数。
- 集群外入口显式使用 `-ClientEntryMode external`，为 gate 创建 StatefulSet 与每序号 Service；默认 podip 只面向集群内测试，不能用生产暴露 profile 代替入口模式。生产还须显式设置 `-ReleaseProfile prod`。
- Reads multi-zone definitions from `deploy/k8s/zones.json` or `deploy/k8s/zones.yaml` (or custom path).
- Do not assume `LoadBalancer` across all environments: managed cloud K8s usually prefers `LoadBalancer`, while self-hosted / bare metal K8s should usually prefer `NodePort` plus an external L4 load balancer.
- Prefer passing `-OpsProfile managed-cloud` or `-OpsProfile bare-metal` so the exposure choice is explicit in commands and change records.
- In `custom` mode, the default `-GateServiceType` baseline is `NodePort` (safer for clusters without mature LB support).
- 逻辑库参数：`-DirectoryLogicalShards`（prod 必须 64，staging 必须显式，dev 缺省 1）、`-Region`（地域码，staging、prod 必须显式给 1..63，0 只给 dev，见 `docs/ops/open-new-region-runbook.md`）、`-SqlClusters` 和 `-SqlDefaultCluster`（搬库时的物理集群清单，见 `docs/ops/db-move-runbook.md`）。都不粘滞，`dev_tools.ps1` 同名转发。

Quick exposure preflight (dry-run + assertions):

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-exposure-preflight
```

See `deploy/k8s/README.md` for full usage and behavior details.
Ops runbook: `docs/ops/k8s-open-server-runbook.md`.

### k8s_image.ps1

Kubernetes runtime image and release entrypoint used by `dev_tools.ps1`.

- Verifies staged Linux runtime files before image build.
- Builds and pushes the dedicated K8s runtime image from `deploy/k8s/Dockerfile.runtime`.
- Supports one-command release flows: build + push + deploy.

### k8s_stage_runtime.ps1

Stages Linux node binaries, `zoneinfo`, and generated tables into `deploy/k8s/runtime/linux` for the dedicated K8s runtime image.

## Usage Examples

### From Project Root

```powershell
# Generate all proto code
pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run

# Generate project tree
pwsh -File tools/scripts/dev_tools.ps1 -Command tree
```

### From tools/scripts Directory

```powershell
# Build proto-gen
pwsh -File dev_tools.ps1 -Command proto-gen-build

# Run proto-gen
pwsh -File dev_tools.ps1 -Command proto-gen-run -ConfigPath ../proto_generator/protogen/etc/proto_gen.yaml
```

## Notes

### 本机 Docker 修改后重编与断点

修改 C++、Go 或 Java 后，在仓库根执行统一入口：

```powershell
python -X utf8 -B tools/scripts/rebuild_all_docker_debug.py
```

如果此前的统一批次在配置刷新和容器更新前停止，可显式加 `--resume-builds-from <旧批次目录>`。例如在仓库根执行：

```powershell
python -X utf8 -B tools/scripts/rebuild_all_docker_debug.py --resume-builds-from build/verification/docker-rebuild-all-20261112T040700.029055Z
```

入口为新批次重新预检、采集当前运行状态，并核对旧批次已成功的连续 C++ / Go 阶段。候选的完整来源、运行资源、SDK、基础镜像、Debug 构建参数和本机镜像 ID 都须与证据相符；任一核对失败即停止。新回执用 `reused_from` 标出每个复用阶段的来源。可先用同一参数加 `--check` 只读验证资格。Java 阶段按本次配置重新创建或核验 fixture，从头运行构建及测试。省略复用参数时，入口按通常流程重新构建所选语言。

默认按 C++ → Go → Java 串行调用底层构建器：C++ 三节点 `gate`、`scene`、`battle`；Go 八服务 `login`、`db`、`data-service`、`scene-manager`、`player-locator`、`client-rpc-router`、`match`、`player-inbox`，以及 `sql-boot` 注册工具和 `db-migrate` 存档库迁移工具；Java `gateway`。`DB_MIGRATE_IMAGE` 随构建结果发布到原批次的配置，供下次显式迁移使用。全部构建成功且来源一致后，才执行 `prepare --refresh-existing`，保留 canonical `build/docker-debug-current/.env` 指向的原批次目录、端口、密码和卷前缀，再调用一次 `update_images`。全量更新只重建 12 个运行服务及 6 个写者注册步骤（`<服务>-sql-boot`），不重跑 init、migrate 或基础设施；最后核对健康、源码来源和镜像。写者账号每次重建都换。

已有调试栈在 Docker Desktop 重启后可直接执行同一条命令。完成所选语言、SDK、fixture 和运行栈预检后，入口持项目启动锁，按 MySQL → Redis → etcd → Kafka 的顺序恢复已退出的原容器。启动前核对容器完整 ID、配置、镜像、挂载卷、端口及项目运行状态；每项只发一次 `docker start`，最多等待 360 秒达到健康，再继续下一项。已健康的服务直接通过。已有服务运行但不健康、检测到身份或配置变化、启动失败或超时均停止编译，已成功恢复的基础服务及本轮启动回执保留在现场。恢复阶段仅涉及这四个基础服务。

加 `--check` 可只读预检，终端和 `preflight.json` 中的 `infraReady` 和 `pendingResume` 分别显示实际就绪状态和执行模式将恢复的服务。`--check` 仅写本次审计文件。加 `--languages cpp,go` 等可选择语言子集，执行顺序仍为 C++ → Go → Java。配置变动、身份迁移或缺少生成器版本记录时，子集更新会受到保守检查，应按报错要求执行全量更新。

Java 默认自动创建本次调用独占的三容器 TiDB 8.5.8 测试 fixture，经正式 producer 生成新的 `NORMAL` writer grant，再执行原有完整测试。无需手动配置十项 fixture 变量。统一入口会先检查本地工具、辅助程序、固定镜像及端口等前提；缺失时在 C++/Go 编译前停止。`--check` 只做只读检查，不创建 fixture。实际环境仅在轮到 Java 构建时创建，Java 构建和来源验证全部成功后才退休 writer 并清理本次 fixture，清理成功后才刷新配置和更新服务。创建、构建、验证或清理失败均停止并保留证据与尚存的 fixture，不自动重试，不跳过测试或复用旧 JAR。

如需自定义外部 fixture，八项核心变量中任一已设置就选择外部模式，必须完整提供 `MMORPG_AUTHORITY_IT_JDBC_URL`、`MMORPG_SQL_USERNAME`、`MMORPG_SQL_PASSWORD`、`MMORPG_SQL_WRITER_GRANT_FILE`、`MMORPG_SQL_WRITER_GRANT_SHA256`、`MMORPG_SQL_DEPLOYMENT_ID`、`MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256`、`MMORPG_SQL_WRITER_CONSUME_DIR`，以及 `MMORPG_SQL_PRIVATE_FILE_HELPER` 和 `MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256`。JDBC URL 仅接受 `jdbc:mysql://127.0.0.1:<port>/<schema>` 或 `jdbc:mysql://localhost:<port>/<schema>`，必须显式指定端口和单个 schema，不接受 URL 参数、用户信息或多主机形式。仅配置 private-file helper 两项仍可使用自动模式。外部 fixture 不由统一入口退休或删除；两种模式传给 Java 的子环境均移除 `COMMERCE_IT_*` 和 SQL schema/registrar/maintenance 管理 DSN。公开 receipt 只记录 fixture 模式和阶段，不记录凭据或私有 context。离线单元测试用于验证编排契约，不代表已完成真实编译或部署。

任一步骤失败即停止，不自动重试；日志保存在 `build/verification/docker-rebuild-all-<UTC>/`。容器更新阶段失败会保留已完成的更新状态，不提供全事务回滚。配置未变化时仍可使用原单语言入口 `python tools/scripts/rebuild_docker_debug.py cpp`、`python tools/scripts/rebuild_docker_debug.py go --services login` 或 `python tools/scripts/rebuild_docker_debug.py java`；配置或来源检查报错时使用上面的统一入口。旧入口的 `--build-only` 只构建，不更新配置或容器，`--services sql-boot` 只换工具镜像。

`build_docker_cpp_debug.py` 使用本机已验证且有效的 SDK 和 `Debug/-O0`，SDK 来源及依赖守卫位于忽略目录 `build/docker-debug-current/cpp-toolchain.json`；依赖源码变化时停止，不复用不匹配 SDK。`build_docker_go_debug.py` 使用当前 Go 1.27.2、`all=-N -l` 与固定 Delve v1.27.2，逐服务串行构建，并可能按原流程构建固定版本调试器，不能假设全流程完全离线。`build_docker_java_debug.py` 使用已有 JDK 23/Maven 和固定版本 protoc，按原有测试前置离线单线程执行测试、打包并核验行号表。三个底层构建器只输出本地镜像及 `--output/result.json`，不自行更新运行栈。

`docker_cpp_sdk_guard.py` 收集 SDK 及独立依赖的实际文件集合；`docker_debug_manifest.py` 在容器构建中核验冻结源码、维护按 SHA 变化失效的对象缓存，并登记最终编译参数、节点与调试符号摘要。

VS Code 的“终端 → 运行任务”提供对应的“Docker：重编并更新”任务。“运行和调试”提供 C++ 三节点、Go 服务及 Java Gateway 的附加配置，每项附加前执行源码检查。也可执行 `python tools/scripts/rebuild_docker_debug.py check --services scene` 单独检查。Go 普通服务不编译的 `*_test.go` 不参与运行源码一致性判定。完整操作与端口见 `docs/design/docker_k8s_build_deploy.md`。

- 维护中的 PowerShell 入口使用 PowerShell 7（`pwsh`）；不要假设 Windows PowerShell 5.1 兼容。
- Use `-Verbose` flag for detailed output
- Scripts are designed to be cross-platform compatible where possible
- For large repositories, prefer iterative rename batches with `-MaxChanges` to keep refactors reviewable and reduce breakage risk
