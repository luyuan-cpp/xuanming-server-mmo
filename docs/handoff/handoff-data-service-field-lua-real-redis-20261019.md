# 交接:data_service 字段索引三段 Lua 的真 Redis 集成测试(2026-10-19)

> 一句话状态:**按负责人授权,`c35e1133d` 业务源码加在隔离副本补生成的一个 Go 绑定,本条字段 Lua 线验证已通过。** 构建、两组 vet、105 个顶层单测通过;Redis 7.2.16 / 8.10.2 各 7 个顶层用例与 8 个拒绝子测试全部 PASS、零 SKIP,两轮命令均退出 0。799 个运行输入哈希前后一致,容器已清理。完整证据与范围见第 10 节;第 7–9 节保留此前失败记录。原样提交仍缺绑定,最新工作区不在本次通过范围;未改原工作区业务源码、未执行提交。
> 接手前照 `AGENTS.md`「会话启动门禁」读一遍;Claude 不跑编译 / 测试(§10.1),本文第 3 节的命令交 Codex 执行。

## 0. 换账号接手

**代码位置**:本线全部改动(含本交接文档)已随负责人 2026-10-20 的保存提交 `c35e1133d` 入库,任何不早于该提交的 clone 都能接手,不再限定本机工作区。接手前先 `git log -1 --format=%h -- go/data_service/internal/logic/data_logic_integration_test.go`:不是 `c35e1133d` 就说明之后又有人改过,先看 diff。

**给 Codex 的原始开场提示**(复验前先读第 10 节,明确选用的提交与授权补生成绑定,不要混入最新工作区):

```text
接手 docs/handoff/handoff-data-service-field-lua-real-redis-20261019.md。先按 AGENTS.md 会话启动门禁读文档,
再按交接文档第 3 节,在 go/data_service 下先做静态检查与单测(3.2 第 3 步),再用两个一次性容器(不借 codex-* 容器)
分别以 redis:7.2 和 redis:8.10.2 各跑一轮 go test -tags=integration ./internal/logic/ -run RealRedis -count=1 -v。
每轮 7 个 TestRealRedis* 必须全部 PASS、不能有 SKIP,记下两行 redis_version。失败不改断言重跑,按交接文档第 2 节表格
判断并报告。跑完按第 3.4 节回填 docs/PROGRESS.md 与 docs/design/data_service_role_and_scope.md;停掉容器;不提交。
```

**给 Claude 的开场提示**(只在要改测试或核对回填时用;Claude 不跑测试):

```text
接手 docs/handoff/handoff-data-service-field-lua-real-redis-20261019.md。先按 AGENTS.md 会话启动门禁读文档,
再读交接文档全文与 go/data_service/internal/logic/data_logic_integration_test.go。若 docs/PROGRESS.md 末尾已有
Codex 的运行结果,按第 3.4 节核对回填是否正确;没有结果就不要声称已验证。用例失败时先按第 2 节表格归因,改代码前先问我。
```

## 1. 背景与范围

- `go/data_service/internal/logic/data_logic.go` 的玩家字段键族 `player:{id}:<field>` 有三段 Lua:`saveFieldsScript`(存盘)、`loadAllFieldsScript`(全量读)、`deletePlayerFieldsScript`(删除),配合字段索引 `player:{id}:__fields`。契约见 `docs/design/data_service_role_and_scope.md`「字段索引与保留名」。
- 已有单测全部只在 miniredis 上跑。miniredis 的 Lua 是 gopher-lua 仿真,在三处可能与真 Redis 不同,而三段脚本恰好依赖它们:TYPE 状态回复是否转成 `{ok=...}` 表、GET 缺失回 `false` 是否在返回数组里占位、INCR 是否拒绝 `"007"` 这类前导零串。
- 本线原本只补集成测试、不改实现;第二个会话按负责人「做完」的要求处理了第 4 节两个尾巴,其中版本守卫收紧(≤18 位 → ≤15 位)是本线**唯一的实现改动**,其余都是注释、测试与文档。

## 2. 已完成

| 项 | 位置 | 提交状态 |
|---|---|---|
| 单实例 6 个用例 | `go/data_service/internal/logic/data_logic_integration_test.go`(`//go:build integration`) | 已随 `53361d055` 入库 |
| Cluster 用例 `TestRealRedisClusterUndeclaredKeys`、SCAN 辅助函数抽成 `scanRealRedisPlayerKeys`、文件头改成「7.2 与 8.10.2 两轮」 | 同上文件 | 已随 `c35e1133d` 入库 |
| 设计文档「仍未处理」一条:标注「已补集成测试,待运行」,并更正「跨 slot 会报错」的说法 | `docs/design/data_service_role_and_scope.md` | 第一处随 `53361d055`、其余随 `c35e1133d` 入库 |
| 进度记录 | `docs/PROGRESS.md` 三条:「真 Redis 7.2 集成测试」「补 Redis Cluster 未声明键集成用例」「改为两版本验证并交接」 | 第一条随 `53361d055`、其余随 `c35e1133d` 入库 |
| 第 4 节尾巴 1:注释口径更正 | `data_logic.go` 的 `loadAllFieldsScript` 注释、`routing/router.go` 的 `getHomeZoneAndMergeFenceScript` 注释;设计文档那条更正标注「已改正」 | 已随 `c35e1133d` 入库 |
| 第 4 节尾巴 2:版本守卫 ≤18 位 → ≤15 位 | `data_logic.go` 的 `saveFieldsScript`(守卫字面量与注释)、`parseVersionValue` 注释;新单测 `player_fields_test.go: TestSave_VersionPrecisionCap`;集成测试用例 3 加上界段、用例 4 加两行坏版本;设计文档「版本格式」后加「版本精度上限」一条 | 已随 `c35e1133d` 入库 |
| 静态评审修正:Cluster 用例的就绪判断 | 集成测试用例 7:改为逐个种子节点等 `cluster_state:ok`(最多 30 秒)之后再建集群客户端,并断言 `CLUSTER SLOTS` 覆盖满 16384;第 3.2 节容器步骤改为三个节点都等到 ok | 已随 `c35e1133d` 入库 |
| 进度记录 | `docs/PROGRESS.md`「第 4 节两个尾巴」一条 | 已随 `c35e1133d` 入库 |

7 个用例各钉什么:

| 用例 | 钉住的行为 | 失败说明什么 |
|---|---|---|
| `TestRealRedisTypeReplyOkField` | 探针脚本 `TYPE(k).ok` 对索引 / 版本 / 字段 / `save_barrier` / 缺失键分别是 hash / string / string / hash / none;索引换成 string、版本换成 HASH 时读、写、删三个入口都回 1,错误文本带类型名,数据不动 | `.ok` 取不到时三段脚本的类型判断全部失灵,是实现缺陷 |
| `TestRealRedisLoadAllMissingValueKeepsPairs` | 索引有名字、字段键已删:RESP2 与 RESP3 客户端拿到的原始回复都不截断,缺失值是 null(不是布尔 false),后面的名字照常返回 | RESP3 下回布尔会让 `decodeLoadAllReply` 整体 fail-closed(生产客户端缺省 RESP3);错位会读错字段 |
| `TestRealRedisStampMatchesVersionBytes` | 戳与 `GET(__version)` 逐字节相等,含 15 位版本(Lua `%.14g` 会转成科学计数法);守卫上界 999999999999999 照常存盘,NewVersion 精确为 10^15、戳是 `"1000000000000000"` 不是 `"1e+15"` | 索引永远不可信,全量读退化成每次 SCAN;上界段失败说明 15 位上限不够保守,是实现问题 |
| `TestRealRedisSaveRefusalsHaveNoSideEffects` | 字段键是 HASH 回 -2;版本 `abc` / `007` / `00` / 19 位 / 2^53+1(带过期一格的 ExpectedVersion=2^53)/ 10^15、索引是 string 都 error_reply;字段未写、版本未变;并先断言真 Redis 的 INCR 确实拒绝 `abc` / `007` / `00` | 留下「字段已写、版本没涨」的半截写;2^53+1 那行失败说明 CAS 被舍入后的「相等」放行 |
| `TestRealRedisDeleteScriptModes` | 索引模式、回退模式、直接调脚本(错 token -1、存量玩家 -2 零副作用):只删 string,返回值等于前后快照差集 | 误删别的模块的 HASH 或计数不实 |
| `TestRealRedisLegacyPlayerEndToEnd` | 存量玩家全量读(SCAN)→ CAS 新写 → 删除(回退)→ 重新存盘建索引 → 按索引读与删;结束不留锁 | 端到端回归 |
| `TestRealRedisClusterUndeclaredKeys` | 三主集群上同 slot 未声明键放行;SCAN 回退按 hash tag 路由且 MGET 不 CROSSSLOT;同节点跨 slot 的未声明键**静默放行**、跨节点报 `non local key` | 前两项失败是实现问题;跨 slot 两枚探针失败说明 Redis 改了兼容模式规则,要改文档口径,不是实现缺陷 |

## 3. 验证步骤:两个版本各跑一轮(最新通过结果与输入范围见第 10 节)

### 3.1 版本口径

- K8s 清单 `deploy/k8s/manifests/infra/redis.yaml`、`redis-ha.yaml`、`redis-match-cluster.yaml`、`redis-rank.yaml` 都钉 `redis:7.2`(本机镜像 `REDIS_VERSION=7.2.16`)。
- 本地 `deploy/docker-compose.yml` 主 Redis 是 `redis:latest`,本机该镜像 `REDIS_VERSION=8.10.2`;Java 版 `deploy/compose/infra.yaml` 钉 `redis:8.10.2`。
- 两版都要全 PASS 才算覆盖。用例不判版本,只在 `-v` 输出里记 `redis_version`。
- 8.x 轮的镜像:本机若没有 `redis:8.10.2` 这个 tag,先 `docker image inspect redis:latest --format '{{range .Config.Env}}{{println .}}{{end}}'` 确认 `REDIS_VERSION=8.10.2`,确认后用 `redis:latest`;要拉镜像属于下载,先征得负责人同意。
- Redis 主干(8.x 线)源码已核对过:兼容模式 EVAL 仍带 `SCRIPT_ALLOW_CROSS_SLOT`、脚本侧 RESP2 下 `false` 仍转 null、状态回复仍转 `{ok=...}`,预期两轮结果一致。

### 3.2 每轮步骤(PowerShell,工作目录 `go/data_service`,`GOTOOLCHAIN=local`、`GOFLAGS=-p=1`;复验原样 c35e1133d 时先按第 10 节在隔离副本补齐缺失的 Go 绑定)

1. 选镜像:第一轮 `$img='redis:7.2'`,第二轮 `$img='redis:8.10.2'`(或按 3.1 确认过的 `redis:latest`)。
2. 起两个一次性容器(**不借 codex-* 容器**)。端口被占时(2026-10-22 订正):
   - 6390 被占:只换 `-p` 的宿主端口,同步改 `DATA_SERVICE_IT_REDIS_HOST`。
   - 7001–7003 被占:**不能只改 `-p` 映射**。集群节点按监听端口对外宣告,go-redis 会照宣告的 `127.0.0.1:700x` 连回去,只改映射会连到占着这几个端口的另一套集群上(`deploy/docker-compose.yml` 的 redis-cluster profile 就占 7000–7005)。要把下面命令里 `-p` 两侧、`for p in`、`--cluster create` 和就绪循环中的端口一起换成同一组新端口(2026-10-20 Codex 用的是 7101–7103),再同步改 `DATA_SERVICE_IT_REDIS_CLUSTER_ADDRS`。

   ```powershell
   docker run --rm -d -p 6390:6379 --name ds-it-fields $img --appendonly yes
   docker run --rm -d -p 7001-7003:7001-7003 --name ds-it-cluster $img sh -c 'for p in 7001 7002 7003; do redis-server --port $p --cluster-enabled yes --cluster-config-file nodes-$p.conf --cluster-announce-ip 127.0.0.1 --protected-mode no --daemonize yes; done; sleep 2; redis-cli --cluster create 127.0.0.1:7001 127.0.0.1:7002 127.0.0.1:7003 --cluster-replicas 0 --cluster-yes; tail -f /dev/null'
   $deadline = (Get-Date).AddSeconds(30)
   do {
     Start-Sleep -Milliseconds 500
     $ready = @(7001, 7002, 7003 | Where-Object { (docker exec ds-it-cluster redis-cli -p $_ cluster info) -match 'cluster_state:ok' }).Count
   } until ($ready -eq 3 -or (Get-Date) -gt $deadline)
   if ($ready -ne 3) { docker logs ds-it-cluster }
   ```

   `sh -c` 后面必须是单引号,`$p` 不能被 PowerShell 展开。三个节点都要报 `cluster_state:ok`;30 秒还不齐就看 `docker logs`,是环境问题。集群用例自己也会再等一遍(每个种子节点都 ok,最多 30 秒),并断言 `CLUSTER SLOTS` 覆盖满 16384 个 slot:go-redis 的集群客户端会把第一次拿到的 slot 表缓存 10 秒,集群没组好就建客户端,会把「同节点 / 别的节点」分错,造成跨 slot 探针的假失败。
3. 只在第一轮做静态检查与单测(不需要容器):
   - `gofmt -l internal/logic internal/routing`(期望无输出);
   - `go build ./...` → `go vet ./internal/logic/ ./internal/routing/` → `go vet -tags=integration ./internal/logic/`(同包 `id_segment_integration_test.go` 同 tag,会一起编译;它要 MySQL,但 `-run RealRedis` 不会跑它);
   - `go test -count=1 ./internal/logic/`(miniredis 单测,全绿;其中 `TestSave_VersionPrecisionCap` 是本线新增,必须 PASS)。`router.go` 只改了注释,编译与 vet 过即可,不单跑 routing 包测试;
   - `c35e1133d` 一并收进了别的线(D1 路由对账、debug_import 等)同样未编译的改动:`go build` / `go vet` 若报在本线文件(第 6 节)之外,记下文件与报错、归给对应那条线,不在本线里修。
4. 跑测:

   ```powershell
   $env:DATA_SERVICE_IT_REDIS_HOST='127.0.0.1:6390'
   $env:DATA_SERVICE_IT_REDIS_CLUSTER_ADDRS='127.0.0.1:7001,127.0.0.1:7002,127.0.0.1:7003'
   $env:DATA_SERVICE_IT_REQUIRE='1'
   go test -tags=integration ./internal/logic/ -run RealRedis -count=1 -v
   ```

   `DATA_SERVICE_IT_REQUIRE=1`(2026-10-22 起本文件的用例也认这个开关,见 `requireRealRedisIT`):任何 SKIP 都直接判 FAIL,不用再靠肉眼看 `-v` 输出。
   遇 `Access is denied`(杀毒扫描新生成的二进制):`go test -c -tags=integration -o $env:TEMP\logic_it.test.exe ./internal/logic/`,同一会话设好上面三个环境变量后执行 `& $env:TEMP\logic_it.test.exe -test.run RealRedis -test.count=1 -test.v`。
5. `docker stop ds-it-fields ds-it-cluster`(`--rm` 会删容器;ds-it-cluster 的 PID 1 是 sh,stop 要等 10 秒,正常)。

### 3.3 通过标准与失败处理

- 每轮 7 个 `TestRealRedis*` 全部 PASS,**不能有 SKIP**。SKIP 只有两种原因:环境变量没生效,或 9900000000001 起的玩家键空间不干净(新起的容器不会出现)。用例 4 的子测试应有 8 个:`field_key_is_hash`、`field_index_is_string` 与 6 个 `version_*`(含本线新增的 `version_9007199254740993`、`version_1000000000000000`)。
- `-v` 输出里两行版本证据:`real redis 127.0.0.1:6390: redis_version:...` 与 `real redis cluster ... (3 masters): redis_version:...`,两轮分别应是 7.2.x 与 8.10.x。
- 失败时:保留失败用例名、前后 200 行 `-v` 输出;集群用例卡在 `require.Eventually` 或「CLUSTER SLOTS 不是全覆盖」先贴 `docker logs ds-it-cluster` 与三个节点的 `cluster info`(环境问题,不是脚本结论)。**不改断言重跑**,按第 2 节表格判断是实现缺陷还是文档口径,报告后等决策。

### 3.4 回填(两轮都跑完后)

- `docs/PROGRESS.md` 末尾另起一条:两轮的版本号、通过数、证据位置。只追加,不改旧条目。
- 两轮都绿:把 `docs/design/data_service_role_and_scope.md`「仍未处理」那条改成「已在 7.2.x / 8.10.x 实测通过(日期、证据)」并移出「仍未处理」;Cluster 的更正说明保留;「版本精度上限」一条末尾的「真 Redis,待运行」改成实测结果。
- 任一轮红:那条保持「待运行 / 未覆盖」,写清楚哪个版本、哪个用例、什么现象。

## 4. 第 4 节两个尾巴(2026-10-19 已改,2026-10-20 编译与运行通过证据见第 10 节)

1. **注释口径更正(已改)**:`data_logic.go` 里 `loadAllFieldsScript` 的注释与 `routing/router.go` 里 `getHomeZoneAndMergeFenceScript` 的注释原来都说未声明键「跨 slot 会报错而不是静默」。改为:无 shebang 的 EVAL 是兼容模式(带 `SCRIPT_ALLOW_CROSS_SLOT`),同节点跨 slot 静默放行,只有跨节点才报 `non local key`;router 那段补了 Cluster 下围栏键两种情形的后果(本节点:读到的就是真围栏键;别的节点:pcall 失败分支按合服处理)。依据是 Redis 源码,原计划等 Cluster 用例实测后再改;提前改是为了与设计文档那条更正一致。**若用例 7 的两枚跨 slot 探针实测失败,两段注释与设计文档那条一起改口径**(不是实现缺陷)。
2. **版本精度(已改实现)**:脚本里版本经 `tonumber` 与 INCR 进入 Lua 都是 double,超过 2^53 后相邻整数舍入成同一个数:旧守卫放行到 18 位时,当前版本 2^53+1 与过期一格的 `ExpectedVersion=2^53` 会被判相等,CAS 误放行并写入。改法选了「守卫收紧到 ≤15 位」而不是「改字符串比较」:后者还要把四种返回值都改成字符串、Go 侧跟着改解析,改动面大;15 位上限下当前版本 < 10^15、INCR 后 ≤ 10^15,所有 Lua 算术都精确,单玩家存盘次数到不了这个量级。读路径(`parseVersionValue`)**不跟着设上限**:读不经 Lua 数字,`ParseUint` 精确;读到超限版本的调用方去 CAS 只会撞上写侧拒绝。项目尚未上线,没有存量 16 位以上的版本,不涉及兼容。
   - 回归:`TestSave_VersionPrecisionCap`(miniredis;2^53+1 那段在旧守卫下红,gopher-lua 的 `tonumber` 同样落到 float64)、集成用例 3 的上界段、用例 4 的两行新坏版本。

## 5. 约束

- 只用自己起的一次性容器,不借 codex-* 容器,不 FLUSHDB;用例只删 9900000000001 起自己建的键与 `lock:player:<id>`。
- 工作区与其他会话共用:只动本线文件,不提交、不切分支,提交等负责人发话。
- Java 版(AGENTS §12):对应 0.1.0-SNAPSHOT,不适用。Java 仓没有 data_service 玩家字段键族的对应实现;本线除测试外只收紧了这族键存盘脚本的版本守卫,同样没有 Java 对应物,不涉及客户端契约。按「Java 仓只记待做项」的约定,不往 Java `PARITY.md` 加行。

## 6. 相关文件

- `go/data_service/internal/logic/data_logic_integration_test.go`(本线全部集成测试)
- `go/data_service/internal/logic/data_logic.go`(被测脚本;本线只改了 `saveFieldsScript` 的守卫位数与几段注释)
- `go/data_service/internal/routing/router.go`(只改 `getHomeZoneAndMergeFenceScript` 注释)
- `go/data_service/internal/logic/player_fields_test.go`(新单测 `TestSave_VersionPrecisionCap`;另有复用的 `cmdRecorder`)、`data_logic_test.go`(复用的 `installHook`、`mustSave`)
- `docs/design/data_service_role_and_scope.md`「字段索引与保留名」「仍未处理」
- `docs/PROGRESS.md` 2026-10-19 的三条本线记录

## 7. 首轮 Codex 实际执行结果(2026-10-20,历史记录,最新通过结果见第 10 节)

- **输入**:启动时 HEAD `9a9390035`,包含 `c35e1133d`；本线四个 Go 文件最初无未提交改动,工作区已有其他会话改动(含 `internal/store/schema.go`)。清理结束时 HEAD 已由其他会话前移至 `49b1aa889`,本会话没有执行提交。Go 1.27.2、Windows/amd64、`GOTOOLCHAIN=local`、`GOFLAGS=-p=1`,工作目录 `go/data_service`。
- **静态与单测**:`gofmt -l internal/logic internal/routing` 无输出；`go build ./...`、`go vet ./internal/logic/ ./internal/routing/`、`go vet -tags=integration ./internal/logic/` 均退出 0；`go test -count=1 ./internal/logic/ -v` 退出 0,105 个顶层测试全部 PASS、零 SKIP,`TestSave_VersionPrecisionCap` PASS。上述仅代表各命令执行时输入。
- **容器**:只用本轮新建的四个一次性容器,没有下载镜像、没有借旧容器或 FLUSHDB。单实例 `127.0.0.1:6390`；旧集群占用 7001–7003,所以三主使用 `127.0.0.1:7101,127.0.0.1:7102,127.0.0.1:7103`,逐节点等就绪。8.x 使用已核实的本地 `redis:latest`(实际 8.10.2)。

| 实际 Redis 版本 | 顶层用例 | 拒绝子测试 | SKIP | `go test` 退出码 | 本轮结论 |
|---|---|---|---|---|---|
| 7.2.16(单实例及三主) | 7/7 PASS | 8/8 PASS | 0 | 0 | 该轮 Go 命令通过 |
| 8.10.2(单实例及三主) | 7/7 PASS | 8/8 PASS | 0 | 1 | 运行断言通过,配置包 vet 失败,整轮门禁未通过 |

- **阻塞原文**:8.10.2 日志开头为 `# data_service/internal/config`、`vet.exe: internal\config\config.go:421:10: undefined: errors`；逻辑包末尾为 `PASS` / `ok data_service/internal/logic` 后跟 `FAIL`。这是 D1 配置门禁改动范围,不是七个字段 Lua 用例的断言失败。当前配置文件已含 `errors` 导入；推测 vet 读到了并发编辑的中间状态,但没有重跑确认,不能声称当前源码仍缺导入或当前树已通过。
- **输入变化**:667 个运行输入的前后 SHA256 已保存；测试末对比有 4 个变化(入口、配置、路由、schema DDL 测试),清理结束时有 7 个变化(另含路由对账实现与测试)。三个本线 logic 文件未变化；`router.go` 的并发变化属于 D1 注释。完整清单见 `source-stability.json` 与 `source-stability-final.json`。本轮不能作为固定提交或当前最新工作区的整体验证证据。
- **执行器记录**:7.2 的原始 Go 命令退出 0；临时汇总脚本把日志中的调用行号计入版本证据去重而误报失败。只修了临时证据聚合,原日志、原 `failure.txt` 与 `aggregation-correction.json` 都保留,没有重跑 7.2 测试；随后仅执行尚未运行的 8.10.2 轮。未改测试断言。
- **清理与范围**:四个本轮容器均已停止并由 `--rm` 移除,`cleanup-final.json` 无残留；未改 Go 源码、未提交或推送。未跑其他包单测、MySQL 集成、race、全服务启动或压测；Java 版仍为第 5 节所述不适用(0.1.0-SNAPSHOT)。
- **证据位置**:本机仓库根下 `build/verification/data-service-field-lua-20261020-1792480770926/`；汇总 `summary.json`,静态与单测 `*-result.json` / `unit.log`,两轮 `redis72/integration.log`、`redis8102/integration.log` 及各自 `result.json`,镜像和版本、容器日志、输入哈希均在同目录。这是被 Git 忽略的本机运行证据,换机器接手须另拷该目录。
- **剩余工作**:按本文 §3.3 与 `AGENTS.md` §10.2,实际命令失败后保留日志并停止,不修其他线、不自动重跑。待负责人确认 D1 配置/路由等共享输入稳定后,在固定输入上补完复验；设计文档仍保留「整体验证待收口」,不能登记为两轮全绿。

## 8. 当前源码固定副本复验(2026-10-20,构建阻断)

- 负责人要求继续验证后,将 `go/data_service` / `go/shared` / `go/proto` / `go/schemamigrate` 同级复制到独立证据目录。锚定 HEAD `2066476114c577456ac3d825eb22c1e2c9543975` 加当时在途改动；674 个文件复制前、复制后及副本 SHA256 完全一致,构建后副本仍零变化,排除了本轮边读边编辑的问题。
- `gofmt -l internal/logic internal/routing` 无输出、退出 0；`go build ./...` 退出 1：`internal/svc/servicecontext.go:351:11: *routereconcile.RedisCache does not implement routereconcile.Cache (missing method ListFenceMirrors)`。
- 只读归因：D1 的 `reconciler.go` 接口和调用仍要求 `ListFenceMirrors`,`cache.go` 已改成 `ScanFenceMirrors` / `GetFenceMirrors`,尚未对齐。原文件与副本一致,目录清单完整,并非复制遗漏。本线三份 logic 文件仍无源码改动。
- 本轮没有执行后续两组 vet、单测或 Redis 集成,没有创建容器,没有修改源码或绕过检查。按失败规则停止,已询问负责人改验已提交的 `c35e1133d`,还是等待 D1 完成后复验最新工作区；未经后续确认不自行重跑。
- 本机证据：`build/verification/data-service-field-lua-recheck-20261020-1792482399231/summary.json`、`build.log`、`snapshot-result.json`、`snapshot-stability.json`；副本保留在该目录 `source/`。本节不改变第 7 节的历史运行结果,也不表示最新工作区已通过。

## 9. 只验证提交 c35e1133d(2026-10-20,原样提交缺 Go 绑定;授权补生成后的结果见第 10 节)

- 负责人明确选择「验证已提交的 c35e1133d,只验本条 Lua 线」后,用 `git archive` 导出四个 Go 模块,目标全长 `c35e1133d3e41bfaa2aee9f42110ffeb2a1bc954`。673 个文件经一次批量 `git hash-object --no-filters --stdin-paths` 与提交 blob 核对全部一致,没有混入最新工作区文件；构建后输入哈希仍零变化。
- `gofmt -l internal/logic internal/routing` 无输出、退出 0；`go build ./...` 退出 1：`internal/store/schema.go:323–327` 引用的 `dbpb.PlayerRoute`、`RouteMeta`、`RouteReconcile`、`ZoneMergeFence`、`ZoneMergedInto` 均 undefined。
- 只读核实：这五个 message 的定义已在该提交 `proto/common/database/rollback_database_table.proto:257–315` 中,但同提交 `go/proto/common/database/rollback_database_table.pb.go` 没有相应 Go 类型；后续 `10fa5d939` 才更新该绑定。该提交的 D1 `ListFenceMirrors` 接口和实现本身一致,此处不是第 8 节的接口错误。
- 该次原样构建失败后未执行两组 vet、单测、两版 Redis 集成,未创建容器,未修改原工作区源码或手改生成物。随后负责人已明确授权仅在隔离副本按该提交协议源生成 Go 绑定后继续,实际通过结果见第 10 节;原样提交的构建失败记录仍保留。
- 本机证据：`build/verification/data-service-field-lua-c35e1133d-20261020-1792483181584/initial-build-summary.json`、`build.log`、`export-audit.json`、`source-before.json`、`source-after-failed-build.json`；原样导出位于该目录 `source/`。

## 10. 授权补生成绑定后的固定副本验证通过(2026-10-20)

- **授权与范围**:负责人先选择仅验 `c35e1133d`,再明确授权「在副本生成绑定并继续验证」。本轮固定输入为全长 `c35e1133d3e41bfaa2aee9f42110ffeb2a1bc954` 的四个 Go 模块及原始 proto,仅补生成 `go/proto/common/database/rollback_database_table.pb.go`;原工作区业务源码、客户端与其他在途线均未修改。
- **输入闭合**:`git archive` 导出 799 个文件,与提交字节闭合。生成前后仅该绑定一项有变化;业务源码、单测与集成测试、原始 proto 均保持提交原样。生成后到所有测试完成,799 个输入 SHA256 零变化。只读并行核对另确认暂存四份协议只改 `go_package`,发布的绑定与 protoc 输出字节相同。
- **生成方式**:按窄范围授权,沿用该提交 `BuildUnifiedGoProto` 的「暂存 → `go_package=proto/{相对目录}` → protoc → 发布」规则,直接调用本机已有 protoc 35.1 / protoc-gen-go 1.36.10。暂存目标与三个直接依赖,保留标准 protobuf include,只把目标 proto 作为生成输入,实际只产出一个 Go 文件;没有手改生成物,没有进行全仓协议同步。参数、工具版本与生成日志均在证据目录。
- **执行环境**:Go 1.27.2、Windows/amd64;`GOTOOLCHAIN=local`、`GOFLAGS=-p=1`、`GOWORK=off`;`source/go/data_service` 为全部命令的工作目录。
- **静态与单测**:`gofmt -l internal/logic internal/routing` 无输出;`go build ./...`、`go vet ./internal/logic/ ./internal/routing/`、`go vet -tags=integration ./internal/logic/` 均退出 0;`go test -count=1 ./internal/logic/ -v` 退出 0,105 个顶层测试全部 PASS、零 FAIL、零 SKIP,含 `TestSave_VersionPrecisionCap`。

| 实际版本(单实例 / 三主) | 顶层用例 | 拒绝子测试 | SKIP | `go test` 退出码 |
|---|---|---|---|---|
| 7.2.16 / 7.2.16 | 7/7 PASS | 8/8 PASS | 0 | 0 |
| 8.10.2 / 8.10.2 | 7/7 PASS | 8/8 PASS | 0 | 0 |

- **版本与容器**:两轮均执行原命令 `go test -tags=integration ./internal/logic/ -run RealRedis -count=1 -v`,单实例与三主日志各有对应 `redis_version`。仅用本地 `redis:7.2` / 经核实为 8.10.2 的 `redis:latest`,未下载镜像;单实例端口 6390,三主端口 7101–7103。只用四个本轮新建的一次性容器,无 FLUSHDB、无借旧容器,四个容器均已停止并自动移除,`cleanup-final.json` 确认零残留。
- **证据**:本机 `build/verification/data-service-field-lua-c35-generated-20261020-1792484087475/summary.json` 汇总计算结果为 `scopeGatePassed=true`。原始日志 `unit.log`、`redis72/integration.log`、`redis8102/integration.log`;各阶段 `*-result.json`、`generation-result.json`、`derived-inputs.json`、`source-stability.json`、`cleanup-final.json` 及 `evidence-sha256.json` 同目录;生成与验证用副本保留在 `source/`。本机证据被 Git 忽略,跨机器接手须另拷。
- **交付边界**:本条 Lua 线在上述授权派生输入上已达到第 3.3 节标准。第 9 节的原样提交缺绑定问题并未通过改原仓库解决,不能写成「c35 原样可编译」；最新工作区/D1 也不属于本次通过范围。未运行其他包单测、MySQL 集成、race、全服务启动、压测或全仓协议同步。Java 版 0.1.0-SNAPSHOT 无对应字段键族,本条不适用,未改 Java 仓;未执行提交或推送。
- **回填**:设计文档已将这条字段 Lua 验证移出「仍未处理」并保留 Cluster 更正;PROGRESS 另起条目追加通过证据,第 7–9 节的历史失败不删除。
