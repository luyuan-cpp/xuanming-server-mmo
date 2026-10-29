# Codex 任务单:全局单点 P0 五项的实测核实(2026-10-14)

> **基线**:`2f897bd07`(完整哈希 `2f897bd0780c1cc78284548284f20d01e61d7ea8`),所有渲染都在检出到它的独立工作树里跑。来源是[全局单点交接](handoff-global-singletons-10m-ccu-20261013.md) §3 里的 P0。
> 写完本单时 HEAD 已前进到 `ebccaf04d`(其他会话的工作区快照)。它只在 `node.cpp:1179` 一带加了停机收尾,在 `k8s_deploy.ps1` 加了 126 行路由预算代码,没有改动这五项的逻辑;落码时 `k8s_deploy.ps1` 的相关行号要在本文基础上后移约 121 行(例如 `GroupID` 由 :2262 移到 :2383,`zoneredis.password` 由 :2301 移到 :2422)。
> **执行方**:Codex。Claude 不运行任何命令(AGENTS.md §10.1)。结果由负责人贴回。
> **本轮只做「现在就能跑」的实测**:部署渲染、一次性 docker 容器、只读采样。不编译 C++,不修构建环境,不下载(镜像缺失就停下问负责人),不做任何破坏性步骤。
> **逐条命令**在[步骤原文](global-singletons-p0-measure-steps-20261014.md)。本文 §2「公共约定」和 §3「覆盖条款」优先于步骤原文。
> **静态证据与修复设计**在[调查记录](global-singletons-p0-findings-20261014.md),给下一步落码用,Codex 不必读。

---

## 0. 范围

| 编号 | 缺陷 | 本轮实测回答什么 |
|---|---|---|
| P0-A | C++ 节点连 Redis 不发 AUTH | 部署产物里 C++ 是否确实拿不到口令;Redis 7.2 对未认证、错口令、无口令 AUTH 的回复 |
| P0-B | K8s 上 battle 的 Kafka 生产者拿不到 broker | 渲染出的 battle-node-config 与 node-config 的 `Kafka.GroupID` 是否非空 |
| P0-C | etcd 没有 MVCC 历史压缩 | 镜像默认是否压缩、默认配额、每 revision 字节数、env 是否生效、NOSPACE 下各操作的表现与解除顺序、压缩后 prev_kv 是否丢失 |
| P0-D | C++ etcd 双向流 `ok=false` 后不重建 | 本轮只能做存量日志排查;运行证据全部受阻(见 §5) |
| P0-E | 共享 Redis 没设 maxclients | redis:7.2 的缺省值、nofile 不足时的下调、到线语义;Go 客户端池参数(读模块缓存源码) |

不在本单:
- L-04(go/db 连接类错误不耗重试预算)和存盘重试入队补 WAITAOF:[go/db 交接](godb-pipeline-handoff-20261013.md) §2.3 认领或建议归它,归属待负责人定。
- 登录排队每 zone 一把锁(G1b):归[开服交接](handoff-launch-readiness-and-d1-20261013.md)。

为什么只跑一部分:C++ 节点在本机编不出来,C++ 运行期的红绿全部受阻。
- Boost 是模块化 include 布局,muduo 的 `EventLoop.h:18` 找不到 `boost/any.hpp`(C1083,docs/PROGRESS.md「2026-10-14 battle 控制面 handler 接入生成器」条目);
- `third_party/grpc/install_vs2026_dbg` 不存在;
- gRPC 的嵌套子模块(googletest、boringssl 等)没有检出。

解除这些属于 AGENTS.md §10.2 的停止条件(下载、装工具、改构建环境),要负责人授权。

---

## 1. 静态结论(Claude 侧,未运行)

方法:每项一个调查代理,两个对抗复核代理(一个专门反驳结论,一个专门核对命令与修复设计),一个定稿代理;之后又把全部结论在 `2f897bd07` 上重读、锚定一遍,并做了跨项审查。五项的静态证据链在 HEAD 上都仍然成立。

| 编号 | 静态结论 | 对交接原文的主要更正 |
|---|---|---|
| P0-A | 成立。建连和重连都不发 AUTH(`redis_manager.cpp` 的 `Connect` / `DoReconnect`);`zone_redis().password()` 在 cpp/ 下没有任何读取点;K8s 模板给 C++ 的 `zoneredis.password` 是字面量空串(`k8s_deploy.ps1:2301`);C++ 工作负载没有任何口令环境变量。 | gate 不发 Redis 命令。受影响的是 scene(冷启动进场 100% 失败)和 battle(结算降级)。触发条件是「部署注入了口令」,dev 档设了 `MMORPG_REDIS_PASSWORD` 也会触发。`bin/etc/game_config.yaml` 里有占位口令 `yourpassword`,C++ 一旦真发 AUTH,本机开发栈会全挂,修复时必须同批改。 |
| P0-B | 成立。`GroupID` 非空时 `KafkaManager::Init` 调用不带回调的 `Subscribe`,必然在 `kafka_manager.cpp:76-79` 返回 false,到不了 :90 的 `setBrokers`;`node.cpp:636` 丢掉返回值;battle 不注册任何消费者,所以生产者永远没有 broker。 | 不是静默:运行期每次发送失败都有 ERROR。topic 是 `match-results`,不是 match-rating。最重的后果是首次开局票据 `NotifyBattleAssigned` 发不出去,客户端拿不到 battle 地址;结算有 Redis 兜底。`DisableLegacyPerNodeTopic` 与「模板 GroupID 改空」耦合:改空后 gate / scene 换新消费组,会从 earliest 重放旧 topic。 |
| P0-C | 仓库侧成立:etcd.yaml 与 compose 都没有压缩配置,也没有任何调用方做压缩。会不会写满配额取决于镜像默认值,本轮实测定论。 | 「发号水位每秒写」在 Go 侧(snowflakealloc 与 login),C++ 自 2026-09-08 起不写水位。漏算了 login 每进程每秒 2 笔,以及过渡期的新旧键双写。「钉镜像版本」本身会滚动 etcd,必须排在 P0-D 修复之后。生成层有 Compact 接口,只是没人调。 |
| P0-D | 成立。生成的 `HandleCompletedQueueMessage` 遇 `ok=false` 只打一行 `RPC failed` 就 `return`(`grpc_init_client.cpp:407-411`),完成事件不分发;Watch 与 LeaseKeepAlive 两条流因此永久失效,约每 181 秒本地判失租并重注册一轮。 | 任何一次断连都会触发,不只是 etcd 滚动。后果更重:从第 2 轮起键会先被删再写回,gate 会摘掉该节点。每 2 秒的全量 Range 只补新增与更新,不补删除,下线的对端会永久留成幽灵节点。修复必须改生成器模板。2026-09-28 的 [历史设计 grpc-client-deadline-failure-callback.md](https://github.com/luyuan-cpp/xuanming-server-mmo/blob/2c07f2c28c627f2559f76b6699d82c4aa8aa05dd/docs/design/grpc-client-deadline-failure-callback.md) 已记过这个问题。 |
| P0-E | 成立,属于随 zone 数线性增长的配置缺口。四处 Redis 服务端都没有 maxclients。 | C++ 是每进程 1 条连接,不是每线程一条。Go 池上限 = 10 × GOMAXPROCS,而且连接数不回落。全仓客户端都没设连接名。到线后 Go 客户端多半报不出「max number of clients」,只有服务端的 `rejected_connections` 可靠。就绪探针也占名额,到线时探针失败会让 Service 摘掉 Redis。 |

---

## 2. 公共约定(覆盖步骤原文)

### 2.1 本轮绝对不做

- 编译任何 C++;修构建环境;初始化子模块;跑 `build_grpc.ps1` 或 `third-party-grpc-build`。
- 下载:`docker pull`;不带 `--pull never` 的 `docker run`;`go mod download`;Maven 依赖。缺什么就停下该项,问负责人。
- 重启或写入开发用的 etcd / redis / kafka 容器(只读采样除外)。
- 在主工作区 `D:\luyuan\xuanming-server-mmo-cpp`、Codex 的 `f452`、`793e` 三棵树里跑步骤或改文件。主工作区有其他会话的大量在途改动,这是常态,不是停止条件。
- `git stash` / `reset` / `checkout --`;新建 kind 集群;压测(AGENTS.md §6)。

### 2.2 S0 只读盘点(约 5 分钟)

PowerShell 7 会话:

```powershell
pwsh --version; $PSVersionTable.PSVersion
Get-Command pwsh,go,docker,kubectl,kind,protoc,python,py,cmake,ninja,winget -All -ErrorAction SilentlyContinue | Format-Table Name,Version,Source
go version; go env GOMODCACHE GOPROXY GOTOOLCHAIN GOFLAGS
docker context ls
docker --context desktop-linux info --format '{{.OSType}}|{{.ServerVersion}}'
docker --context desktop-linux image ls --format '{{.Repository}}:{{.Tag}} {{.ID}} {{.CreatedAt}}' | Select-String 'redis|etcd|mmorpg-node'
docker --context desktop-linux ps -a --format '{{.Names}}|{{.Image}}|{{.Status}}|{{.Ports}}'
$R = 'D:\luyuan\xuanming-server-mmo-cpp'
git -C $R rev-parse 2f897bd07
(git -C $R status --short | Measure-Object).Count
git -C $R worktree list
git -C $R submodule status
git -C $R\third_party\grpc submodule status
'third_party\boost\boost\any.hpp','third_party\grpc\install_vs2026_dbg','third_party\grpc\third_party\googletest\googletest\include\gtest\gtest.h','bin\gate.exe','bin\scene.exe','bin\battle.exe' | ForEach-Object { "$_=" + (Test-Path (Join-Path $R $_)) }
Get-ChildItem $R\lib -Filter *.lib -ErrorAction SilentlyContinue | Measure-Object
Test-Path "$env:TEMP\kind-mmorpg.kubeconfig"
```

- 期望:`rev-parse` 输出完整哈希 `2f897bd0780c1cc78284548284f20d01e61d7ea8`;status 的计数只记录,不作判据。
- 2026-10-14 只读观察到的现状(以实际输出为准):顶层子模块里未初始化的是 cppcodec、fmt、muduo-linux、recastnavigation、xxhash;gRPC 嵌套子模块除 abseil-cpp、protobuf 外都没有检出;6 个 `Test-Path` 都是 False;`lib\` 下没有 `.lib`。
- 停止条件:
  - pwsh 低于 7.3 或找不到:整体停下(安装要授权)。不要退回 Windows PowerShell 5.1:它会丢掉空字符串参数,P0-C 的 `etcdctl get "" --prefix` 会变形。
  - docker 不可用,或没有 `desktop-linux` context:第 2 段整体停下。
  - 沙箱拦截 127.0.0.1 回环:第 2 段暂停,先申请放行。
- 另外回报:
  - protoc 35.1 在哪(PROGRESS 10-13 有 Codex 用过的记录);
  - Python 是否可用;
  - 10-14 独立 GTest 用的 gtest 在哪;
  - 本机有没有 `mmorpg-node` 镜像及其创建时间。

### 2.3 测量工作树:全部渲染只用这一棵

```powershell
$W = Join-Path $env:TEMP 'p0-measure-2f897bd07'
git -C $R worktree add --detach $W 2f897bd07
git -C $W rev-parse HEAD; git -C $W status --short
```

- 期望:完整哈希;status 为空。`$W` 已存在就停下报告,不要覆盖。
- 步骤原文里各自取名的工作树,一律换成 `$W`:
  - P0-A 的 `C:/Users/luyua/.codex/worktrees/p0a-2f897bd07`(不要建在 Codex 的 worktrees 目录下);
  - P0-B 的 `%TEMP%\p0b-render-2f897bd07`;
  - P0-D 的 `%TEMP%\p0d-etcd`;
  - P0-E 的 `%TEMP%\p0e-maxclients-2f897bd07`。
- 本轮不往 `$W` 里拷任何文件。`$W` 保留到负责人确认结果后再 `git worktree remove`。

### 2.4 DryRun 会话卫生

新开一个 pwsh 7 会话,只用来跑 DryRun:

```powershell
Get-ChildItem Env:MMORPG_* | Remove-Item
Remove-Item Env:GAME_CONFIG_PATH,Env:RPC_PORT -ErrorAction SilentlyContinue
$env:KUBECONFIG = Join-Path $env:TEMP 'p0-no-cluster.kubeconfig'   # 故意指向不存在的文件
Set-Location $W
. ./tools/scripts/tests/lib/test_harness.ps1
. ./tools/scripts/tests/lib/deploy_capture.ps1
$out = Join-Path $env:TEMP ('p0-out-' + (Get-Date -Format yyyyMMdd-HHmmss))
New-Item -ItemType Directory $out | Out-Null; $out
```

- 清 `MMORPG_*`:`Invoke-CapturedPwsh` 只覆盖传入的变量,其余全部继承给子进程(`deploy_capture.ps1:52-62`);dev 档的口令变量非空就会被采用(`release_common.ps1:690-692`),会让判据串味,也会把真值写进保存的输出。
- 不存在的 `KUBECONFIG` 是纵深防御:DryRun 本身不调 kubectl,但本机默认 context 指向别的项目的集群。
- DryRun 输出里只会出现本单给的测试假值,可以留存。如果出现真实 kubectl 报错或连集群的迹象,立即停下报告。

### 2.5 docker 一次性容器

- 每条命令都带 `--context desktop-linux`;每条 `docker run` 都带 `--pull never`。
- Redis 的语义一律用 `redis:7.2`(K8s 用的版本,`redis.yaml:83`)。compose 里的 `redis:latest`(`docker-compose.yml:218`)只在只读采样里记录版本。
- 名字和端口互不冲突,不碰 6379、2379,也不碰 go/db 任务单用的 13307、14000:

| 项 | 容器名前缀 | 宿主端口 |
|---|---|---|
| P0-A | `p0a-*` | 127.0.0.1:6390、6391 |
| P0-C | `etcd-p0c-*` | 127.0.0.1:42379~42382 |
| P0-E | `p0e-*` | 不映射 |

- 收尾时各项删除自己的容器;P0-C 还要删卷 `etcd-p0c-vol`。

### 2.6 Go 步骤

```powershell
$env:GOPROXY = 'off'; $env:GOTOOLCHAIN = 'local'; $env:GOFLAGS = '-mod=readonly'
```

缺模块就记「没跑起来」,不下载。

### 2.7 判定与停止

- 每一步只写三种结论:成立 / 不成立 / 没跑起来。「没跑起来」既不算成立也不算不成立。
- 某项出现与期望不符:停下这一项,保留原始输出,不重试、不换写法;其它彼此独立的项照常继续。
- 只有环境类失败(§2.2 的停止条件)才整体停下。

---

## 3. 覆盖条款(对步骤原文的修正)

### P0-A

- **A1 不单独跑**:用 §4 第 1 段第 6 步的全量契约测试结果。
- **A2 必须补一个环境变量**:在 `$envs` 里加
  `MMORPG_LOGIN_QUEUE_TOKEN_SECRET = 'contract-test-queue-token-0123456789abcdef'`。
  原因:prod 档的登录排队默认开(`k8s_deploy.ps1:363` 的 `Resolve-PeakShavingSwitch`,非 dev 档的 auto 取开);zone-up 部署 login 时会要求这把密钥(:532-541),prod 档缺失就 throw(`release_common.ps1:696-698`)。不补的话 A2 退出码非 0,落进回退分支,prod 档的证据就丢了。这个值 ≥32 位,且与 GATE、INTERNAL_AUTH 两把不同。
- A2 的同一份输出里,顺带读 node-config 的 `Kafka.GroupID`,作为 P0-B 判据 A 的 zone 部分:
  ```powershell
  $nodeFlat = ConvertTo-FlatManifest -Block $node
  "P0-B: prod node-config Kafka.GroupID=[$(Get-FlatValue -Flat $nodeFlat -KeyPath 'data.base_deploy_config.yaml.Kafka.GroupID')]"
  ```
- **C 段**:
  - 每条 docker 命令加 `--context desktop-linux`,每条 `docker run` 加 `--pull never`;
  - C0 改为 `docker --context desktop-linux image inspect redis:7.2 --format '{{.Id}}'`,镜像固定用 `redis:7.2`;
  - 缺镜像就停下 P0-A 的 C 段,问负责人。
- **B0~B6 本轮不做**。

### P0-B

- 1-0 的工作树改用 `$W`;1-4 不删 `$W`。
- 1-3 不单独跑:用全量契约测试结果。
- 步骤 0(C++ 环境清单)并入 §2.2,不单独跑。
- 步骤 2、3 本轮不做。

### P0-C

- 照原文执行 M0、M2、M3、M4、清理、S1。
- M1 只在开发容器 `etcd` 在跑时做;S2 只在 `kind-mmorpg` 已存在时做。
- 缺 `bitnamilegacy/etcd:latest`:停下 P0-C,问负责人(原文的「同意 1」)。
- M5a、M5、M6 本轮不做。

### P0-D

- M0 并入 §2.2。原文「status 只有两行子模块标记,否则停下」改为只记录。
- M1 只在有现成的 C++ 节点文件日志时做,否则记「未取得」。
- M2 本轮不做:测试文件还没写,会在修复批里先写、先在干净工作树上跑红。
- M3 不单独跑:在全量契约测试结果里看原文点名的三条用例。
- M4 本轮不做:它会重启开发 etcd。
- A1、B0~B7、K1 不做。

### P0-E

- S0 里的工作树改用 `$W`;输出目录仍是原文的 `$W/run/logs/p0e-maxclients/`。
- S2 缺 `redis:7.2`:停下 P0-E 的 S2,问负责人。
- S3a 只在已有开发栈在跑时做;S6 只在 `kind-mmorpg` 已存在时做。
- R0 本轮不做:测试文件随修复批写。
- S3b、S4、S5 不做。

---

## 4. 执行顺序

### 第 0 段:只读盘点(约 5 分钟)

按 §2.2 执行。

### 第 1 段:渲染与静态核对(不连集群、不用 docker、不编译;约 30~60 分钟)

1. §2.3 建 `$W`;§2.4 准备会话。
2. P0-A A2(按 §3 修正)。
3. P0-A A3。
4. P0-B 1-1、1-2。
5. P0-C S1、P0-E S1:都是对 `2f897bd07` 的 `git show` / `git grep`,不需要 `$W`。
6. 全量契约测试,只跑一次,在 `$W` 里:
   ```powershell
   pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1 *> (Join-Path $out 'contract-baseline.txt')
   "contract exit=$LASTEXITCODE"
   ```
   - 按用例名判读。P0-D M3 只看原文点名的三条:NodeTTLSeconds、gate 就绪探针、scene 就绪探针。
   - 已知在 HEAD 上会红、与本单无关、只记录的两类:
     - `负向:zones.10zones.yaml 时打 MySQL连接总账超限 警告且不阻断`:用例断言旧的中文文案(`k8s_deploy_contract.tests.ps1:1656`),脚本已改为英文 `[MySQLConnectionLedger]OverLimit:`(`k8s_deploy.ps1:1238`);
     - prod 档相关用例(`prod 档位 + 注入环境变量时生成器可以正常出产物` 及依赖 `$prodRun` 的用例):`$ProdEnv`(:975-990)缺 `MMORPG_LOGIN_QUEUE_TOKEN_SECRET`,原因同 §3 的 A2。
   - 其它红照原文列出。
7. P0-E S2b(按 §2.6 设环境变量)。

### 第 2 段:docker 一次性容器(约 45 分钟;缺镜像的那一项停下等同意)

1. P0-C:M0 → M2 → M3(先探针 B、B2,等 420 秒,再探针 A;B 或 B2 不通过就不做 A)→ M4 → 清理。
2. P0-A:C0~C7(按 §3 修正)。
3. P0-E:S2.1~S2.4。

第 2、3 步可以放进 P0-C M3 的 420 秒等待里做;为了输出好读,也可以顺序跑。

### 第 3 段:条件只读采样(条件不满足记「未取得」)

1. P0-C M1:开发容器 `etcd` 在跑。
2. P0-E S3a:开发栈在跑。
3. P0-D M1:有现成的 C++ 节点文件日志。
4. P0-C S2、P0-E S6:`kind-mmorpg` 已存在;命令一律显式带 `--context kind-mmorpg` 和 `--kubeconfig`。

### 收尾

- 各项删除自己的临时容器与卷(§2.5)。
- `$W` 和各输出目录保留,回报路径。

---

## 5. 本轮不做的步骤

| 步骤 | 原因 | 解除条件 | 由谁解除 |
|---|---|---|---|
| P0-A B0~B6(scene 连带口令 Redis) | 没有 scene.exe,C++ 编不出 | 构建环境修好(§0),在专用构建树编出节点 | 负责人授权,Codex 修环境 |
| P0-B 步骤 2(C++ 红用例)、步骤 3(真实 battle) | 同上;另需 Claude 先写红用例 | 同上 | 同上 |
| P0-C M5a(Go 开发栈采样) | 要写开发 etcd,可能下载 Go 模块 | 负责人同意 | 负责人 |
| P0-C M5、M6 | 需整栈;M6 不可逆 | 构建环境修好;M6 另需同意,并与 P0-D 共用一次 etcd 重启窗口 | 负责人 |
| P0-D M2(Go 模板红测) | 测试文件未写 | 随修复批先写先红 | Claude 写,Codex 跑 |
| P0-D M4、A1、B0~B7、K1 | 重启开发 etcd;C++ 编不出;K1 要建 kind | 构建环境修好;重启另需同意;K1 修复合入后单独授权 | 负责人 |
| P0-E R0 | 测试文件随修复批写 | — | Claude 写,Codex 跑 |
| P0-E S3b、S4 | 没有节点 exe 与网关 jar | 构建环境修好 | 负责人授权 |
| P0-E S5(把共享 Redis 名额占满) | 有破坏性,期间本机登录与存盘失败 | 有在跑的栈且负责人同意 | 负责人 |

---

## 6. 回报

- 每一步:原始输出 + 三态结论。与期望不符的地方单独列出。
- 各输出目录的路径:`$out`(渲染)、P0-C 的 `$out`(`%TEMP%\etcd-p0c-<时间>`)、`$W/run/logs/p0e-maxclients/`。
- §2.2 的盘点结果,尤其是 pwsh 版本、protoc / Python / gtest 的位置、`mmorpg-node` 镜像。
- 未执行的步骤及原因(未获同意、条件不满足、受阻)。
- 附两条命令的输出,只记录现场:`git -C D:\luyuan\xuanming-server-mmo-cpp rev-parse HEAD`、`git -C D:\luyuan\xuanming-server-mmo-cpp status --short | Measure-Object`。

Claude 收到结果后写回三处:

1. `docs/PROGRESS.md` 追加一条「全局单点 P0 实测结果」;
2. 本文 §1 的表格后追加「实测结论」小节,逐项写成立 / 不成立 / 未取得;
3. [全局单点交接](handoff-global-singletons-10m-ccu-20261013.md)文末追加订正小节,把 §3 对应行的状态从「静态推断」改为实测结论。

---

## 7. 修复落码后的验证(P0-A / P0-B,2026-10-15 追加)

> P0-A(C++ zone Redis 认证)与 P0-B(battle Kafka 生产者 broker 登记)的 C++ 修复与回归用例已落码,**全部未编译、未测试**。P0-D 不在旧模板上修,已并入 gRPC→muduo 迁移第 2 批(见[调查记录](global-singletons-p0-findings-20261014.md) P0-D「去向」)。部署侧(K8s 模板给 C++ 口令、模板 `Kafka.GroupID` 改空)是第二批,等负责人拍板,本节不涉及。
> 改动大部分已随其他会话的快照提交 `ab0f27907` 入库,之后的收尾在主工作区未提交:`redis_manager.{h,cpp}`、`redis_manager_auth_test.cpp`、`kafka_manager.cpp`、`kafka_consumer.cpp`。所以本节**在主工作区跑**,不要新建工作树。

### 7.0 前置

- 2026-10-15 主会话只读观察:`third_party/boost/boost/any.hpp`、`third_party/grpc/install_vs2026_dbg`、gRPC 嵌套的 googletest 都已存在,子模块全部初始化,`lib/` 下有 `hiredisd.lib`、`rdkafka.lib`、`rdkafka++.lib`、`yaml-cppd.lib`,`bin/` 下有 `rdkafka.dll`,还没有任何节点 exe。§0 说的构建阻塞可能已部分解除,以实际编译为准。
- 主工作区有其他会话的在途改动(`node.cpp` 的停机与 RPC 段、`rpc_controller_test` 的 RpcChannel 改名、`kafka_command_test` 新增的 `kafka_command_whitelist_test.cpp` 等)。**第一条编译错误不在本项文件里时,记下文件名,视为其他会话的在途改动,不算本项失败**。本项文件:`redis_manager.{h,cpp}`、`config.cpp`、`node.cpp` 的 zone Redis 建连段 / `FatalStartupConfig` / `InitKafka`、`kafka_manager.{h,cpp}`、`kafka_producer.h`、`kafka_consumer.cpp`、两个新测试文件、`configuration_table_test.cpp`。
- `kafka_command_whitelist_test.cpp`(其他会话的)引用的 `rpc/service_metadata/kafka_command_event_whitelist.h` 目前不存在,要先按那批工作的说明跑 proto-gen 生成;否则整个 `kafka_command_test` 工程报 C1083。不生成就只做 7.2 的单文件编译。
- 清掉环境变量:`Remove-Item Env:ZONE_REDIS_PASSWORD -ErrorAction SilentlyContinue`。不需要真实 Redis / Kafka,不清任何数据。
- MSBuild 路径以 `tools/scripts/run_cpp_tests.ps1` 里 `Resolve-MSBuild` 找到的为准;一律 `/m:1` 串行。

### 7.1 库与节点

`RedisManager` 的成员布局变了,包含 `redis_manager.h` 的旧 obj 不能复用,要串行重编:

```powershell
$msb = '<Resolve-MSBuild 找到的 MSBuild.exe>'
& $msb cpp\libs\engine\config\config.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal
& $msb cpp\libs\engine\thread_context\thread_context.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal
& $msb cpp\libs\engine\infra\infra.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal
& $msb cpp\libs\engine\core\core.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal
& $msb game.sln /m:1 /p:Configuration=Debug /p:Platform=x64 /nologo /v:minimal
```

通过:本项文件里没有 `error C` / `LNK`;最好 `bin\` 下产出 gate / scene / battle 三个 exe。

### 7.2 单测

```powershell
pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter configuration_table_test
.\build\cpp\tests\configuration_table_test.exe --gtest_filter=GameConfigEnvOverride.*
pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter rpc_controller_test
.\build\cpp\tests\rpc_controller_test.exe --gtest_filter=RedisManagerAuth.* --gtest_repeat=20
pwsh -NoProfile -File tools/scripts/run_cpp_tests.ps1 -Build -Filter kafka_command
.\build\cpp\tests\kafka_command_test.exe --gtest_filter=KafkaManagerInit.*
dumpbin /dependents build\cpp\tests\kafka_command_test.exe | Select-String rdkafka   # VS 开发者命令行
```

- `rpc_controller_test` 整个工程在本项以外的文件编不过时,只编新文件:`& $msb cpp\tests\rpc_controller_test\rpc_controller_test.vcxproj /t:ClCompile /p:SelectedFiles=redis_manager_auth_test.cpp /p:Configuration=Debug /p:Platform=x64 /m:1 /nologo /v:minimal`,0 error 即可,用例留待工程能链接后再跑。`kafka_command_test` 同理(`SelectedFiles=kafka_manager_init_test.cpp`)。
- 不要加 `--gtest_catch_exceptions=0` 或 `--gtest_break_on_failure`:测试夹具的超时路径会 abort,Debug CRT 会弹框卡住无人值守的运行。
- exe 启动即退出、没有任何 gtest 输出(0xC0000135):先查 `rdkafka.dll` / `zlibd.dll` 是否已被脚本同步到 `build\cpp\tests\`,不算用例失败。

通过标准:

- `GameConfigEnvOverride` 全绿(含新增 `ZoneRedisPasswordFromEnvOverridesYaml`、`ZoneRedisPasswordWithoutEnvKeepsYaml`)。
- `RedisManagerAuth` 10 条重复 20 轮全绿,单轮约 2 秒以内。
- `KafkaManagerInit` 4 条全绿;`dumpbin` 只列出 `rdkafka.dll`,没有 `rdkafka++.dll`。
- 失败时保留:第一条 `error Cxxxx` / `LNKxxxx` 及前后 20 行;失败用例完整 gtest 输出(口令在输出里显示为 `<pw>`)。
- 已知可能的环境性失败(照实报告,不算本项逻辑问题):用例只在等 `Redis disconnected` 时失败且日志刷 `Channel::handle_event() POLLHUP`(Windows 上 Hiredis 的 Channel 没装 close 回调,适配层原有缺口);撞上 `EventLoop.cc` 的 `removeChannel` 断言(同批事件释放的既有残余风险,已记在 `redis_manager.h` 的生命周期注释里)。

### 7.3 修复前先红(可选,§11.4)

在 `6281e4d27`(修复前)的新工作树里只拷入测试与 `kafka_producer.h` 的只读访问器,不拷修复:

- P0-B:拷 `kafka_producer.h`、`kafka_manager_init_test.cpp`、`kafka_command_test.vcxproj`(删掉其中 whitelist 那一行),编测试工程后直接跑 `--gtest_filter=KafkaManagerInit.*`。期望恰好 `ProducerBrokersConfiguredWhenGroupIdIsSet`、`EmptyBrokersRejectedWhenGroupIdIsEmpty` 两条失败。
- P0-A:修复前 `RedisManager::Connect` 没有 `RedisConnectOptions` 参数,`RedisManagerAuth` 无法编译(§11.4 的「无法先写」情形)。红态改用配置测试:修复前 `ZoneRedisPasswordFromEnvOverridesYaml` 应失败(没有环境变量覆盖)。
- 新工作树的子模块与第三方库要另外准备,代价较高;不做就在回报里写「未取红态」。

### 7.4 节点黑盒(有可运行的 scene / battle 时)

- P0-A:沿用 §3 指向的步骤原文 B2~B6(一次性带口令 Redis 探针,口令经 `$env:ZONE_REDIS_PASSWORD` 传给 scene,不写进任何文件),分 6 组:
  - 0)不设口令连带口令探针:`Zone Redis auth: off`,两条 `SCRIPT LOAD failed for PlayerAllData (reply`,errorstats 出现 NOAUTH(等价于修复前的红);
  - 1)设对口令:`Zone Redis auth: on`、`Redis AUTH ok to 127.0.0.1:6390`、`Redis reconnected, retrying pending player loads`、两条 `SCRIPT LOAD ok`,NOAUTH 计数不再增长;
  - 2)口令设错:约每 3 秒一条 `Redis AUTH rejected by 127.0.0.1:6390` 且带 `metric=zone_redis_auth_failed`,不出现 `Redis reconnected` 与任何 `SCRIPT LOAD`;
  - 3)不设口令连无口令探针:与修复前一致,只多 `Zone Redis auth: off`;
  - 4)设口令连无口令探针:`Redis AUTH rejected`(服务端没开口令也按失败处理);
  - 5)第 1 组运行中 `docker --context desktop-linux restart p0a-redis-auth`:依次 `Redis disconnected`、`Redis connected to`、`Redis AUTH ok`、两条 `SCRIPT LOAD ok`。
  - 全部日志里搜两个口令字符串,必须 0 命中。
- P0-B:把本机 `bin/etc/base_deploy_config.yaml` 的 `Kafka.GroupID` 改成 `game-consumer-group` 后起 battle,本次启动日志应出现 `KafkaProducer: brokers stored for lazy init:`;把 `Kafka.Brokers` 清空后起 battle,stderr 日志应**区分大小写**命中 `FATAL Kafka config: Kafka.Brokers is empty`,进程非 0 退出。每项跑完用备份恢复,`git diff --quiet -- bin/etc/base_deploy_config.yaml` 退出码为 0。

回报写回:`docs/PROGRESS.md` 追加「全局单点 P0-A / P0-B 的 Codex 验证结果」,并在本节末尾补一行「验证结论」。

### 7.5 2026-10-20 Codex 验证回执（首轮结束，未收口）

本轮在主工作区按 §7.1 串行执行 C++ 构建，并补跑交接 §6.3 的四套部署契约及 PROGRESS 10-16 P0-E 第 3 批的 Go 构建/测试。**运行期间其他会话持续改动共享输入，且有真实失败；不能将本轮结果视为当前 HEAD 整体验收通过。** 失败项保留原件，不重试、不修复；独立项继续完成。没有协议生成、节点部署、共享 Redis/etcd 重启或数据清理。

| 范围 | 实际结果 | 证据边界 |
|---|---|---|
| C++ `config` / `thread_context` / `infra` | 三条 MSBuild 命令各退出 0；均 `/m:1 /nr:false /p:CL_MPCount=1` | 不是全量 Rebuild 或节点验收 |
| C++ `core` | 退出 1；检查器处理 `network/player_message_utils.cpp` 时文件已不存在，随后 MSB3073（检查器 exit 2） | 构建前后观测到 24 个既有 C++ 输入变化（含文件删除及工程改动），属于并行改名窗口的失败现场；不据此断言当前稳定树仍复现 |
| C++ 后续 | `game.sln`、三个指定 gtest 过滤器及 Redis AUTH repeat=20 均未启动 | 当前新构建下的 AUTH / Kafka 专测、节点黑盒仍未取得 |
| `etcd_maintenance_contract` | 原生 exit 0，9/9 | 本套运行前后输入无漂移；只证明静态契约 |
| `redis_maxclients_contract` | 原生 exit 0，7/7 | 当前已含 T6/T7 高可用用例，不再是旧交接的 5 条；本套无漂移 |
| `k8s_deploy_contract` | 原生 exit 1，272/284，12 失败 | 脚本、库、测试本身及部分配置在运行中变化，不收口、不稳定归因。首错为 MySQL ConfigMap 清单缺 guild；其余涉及 guild/rank、资产通道、gRPC 准入和 D1 告警 |
| `k8s_client_entry_contract` | 原生 exit 0，111/111 | 本套 158 个记录输入前后无漂移；是服务端部署入口契约，不涉及独立客户端仓 |
| Go 7 服务构建 | login、player_locator、trade、guild、player_inbox 退出 0；db、data_service 退出 1 | 全轮输入 1224→1227，31 项变化，HEAD 也变化；不能对当前整树收口 |
| Go 指定测试 | login、player_locator、trade、player_inbox 测试组退出 0；guild 测试组退出 1；db/data_service 测试因本模块构建首错未启动 | JSON 测试事件（含子测试）437 PASS / 1 FAIL / 1 SKIP，不称 439 个独立用例全验 |

Go 首错：

- db：`internal/kafka/route_ready_gate.go:308` 未定义 `routeReadyGateStateReadError`。
- data_service：`internal/routereconcile/cache.go:227` 的 `Control` 没有 `WatermarkWrongType`。
- guild：`TestGuildAuthorityDefaultsAndPoolBounds`，`config_test.go:193` 的 `MySQL.MaxOpenConn=0` 应拒启、实际返回 nil；这条不是 Redis PoolSize 校验。
- login 唯一 SKIP 为需显式开启真实 SQL 隔离夹具的配额用例，本轮未运行外部栈。
- `gofmt -l` 退出 0，但列出 5 个其他线的 login 文件；没有本批 config/svc 文件，不自动格式化。

P0-A/B、Redis 池/总账/重启闸、etcd CronJob 渲染等专题输出均观察到 PASS，逐名摘录在 `p0-relevant-cases.json`；**凡出自发生输入漂移的 k8s 套件，均不提升为稳定验收结论。**

历史证据订正：PROGRESS 10-19 已补记 Codex 10-17 的 RedisManagerAuth 10 条和 Liveness 5 条通过，但引用的 `run/merge-validation-20261017` 当前不存在，不能声称本轮复核了该原 XML。Java P0-E 第 3 批已有 10-18 验证回执：RedisPropertiesTest 10/10，五模块 3610 项含 639 条既有跳过、0 失败/错误；当前本线三个 Java 源码与该次输入 SHA 一致，可引用这份专项历史证据，不称本轮重验整个 Java 树。对应 Java 版本 0.1.0-SNAPSHOT，本轮不改 Java 或 PARITY。

P0-D 仍未取得「删除/重启一个 etcd 成员后无 fence 且零重注册」实跑证据，**P0-C 阶段 2 仍不得提前**。两版 Redis exporter 告警仍待定，Java compose 的 maxclients/nofile 取值仍待负责人决定；本轮没有替负责人确认旧交接 §2 的部署取值。

原始证据（仓库根相对路径）：

- 总索引：`build/verification/global-singletons-p0-20261020-summary.json`。
- C++：`build/verification/global-singletons-p0-20261020-cpp-20261020T072029Z/result.json`，首错上下文 `first-error-context.txt`。
- 部署：`build/verification/global-singletons-p0-20261020-deploy-20261020T072114Z/results.json`，失败原文 `k8s-deploy-failure-excerpts.txt`。
- Go：`build/verification/global-singletons-p0-20261020-go-20261020T072028Z/verification-summary-refined.json`（原始汇总、native 日志与 SHA 均保留）。

按 AGENTS.md §10.2 保留失败并等待决策。后续先取得其他会话停手后的稳定输入窗口，再决定重跑及仍存在的失败由哪条线修复；不能把混合工作区失败直接修成新的跨线改动。
