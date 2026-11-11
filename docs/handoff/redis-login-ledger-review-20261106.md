# 登录 Redis 清退后的连接总账：交 Claude Code 独立审核

日期：2026-11-06（America/New_York）。用户要求：先修复，再交给 Claude Code，按 Claude 自己的思路审核。

## 给审核者

请独立判断本次修改是否正确、完整，审核入口、方法和深度由你决定。下面的修复解释与测试结果是证据，不是预设结论；可以质疑计算口径，并沿相关代码寻找遗漏。请报告有证据的问题、影响和位置；没有发现问题也请说明实际审核边界。遵循根 `AGENTS.md`：需要补充编译或测试时，给出由 Codex 执行的命令。

工作目录：`D:\luyuan\xuanming-server-mmo-cpp`。起始核对 HEAD 为 `1194c4d46`，期间其他会话继续提交。工作区有多个会话并行修改，甚至同一个脚本也有其他修改；下面明确区分本次改动，不要把整份 `git diff` 都归到这次任务。本会话未执行提交或推送。

## 已有事实与本次修复

六个在线服务 chat、friend、guild、match、mail、trade 的登录 Redis 配置清退已经由身份会话完成；本次核对时 `Get-RedisPoolMirror`、`Get-RedisPlayerPoolMirror`、ConfigMap 与登录域总账已同步，没有再次删除这些配置。背景见 [身份清退执行清单](gate-online-identity-single-source-20261105/CODEX.md) 与 [身份设计](../design/gate-online-identity.md)。

发现的残留是 match 的控制域默认池：`go/match/internal/svc/servicecontext.go` 的 `NewRedisHandles` 只返回 MatchRedis；K8s 为它配置专用 `redis-match-cluster`，不会同时创建旧 `sharedRds`。但核心 Redis 总账仍记 `DefaultPools=1; GoMaxProcs=2`，每个 match 副本多计 20 条连接，滚动峰值也相应多计。

本次改动：

- `tools/scripts/k8s_deploy.ps1`：仅 `Get-RedisConnectionLedgerInputs` 中 match 行的 `DefaultPools`、`GoMaxProcs` 改为 0，并修正该行与零连接过滤的注释。未拆玩家域时仍计 PlayerRedis；拆到玩家域集群后，既有过滤会移除 match 的零连接控制域行。
- `tools/scripts/tests/k8s_deploy_contract.tests.ps1`：仅 match 的默认池期望改为 0。
- `tools/scripts/tests/redis_player_cluster_contract.tests.ps1`：原 mail 的迁移断言扩为 mail、match，核对未拆时仅有玩家域池、拆后无控制域行，并保持 chat 不连 Redis 的断言。
- `tools/scripts/tests/k8s_redis_split_contract.tests.ps1`：同步控制域实例接入后过时的两条断言。缺省 infra-up 只排除共享模板的整行说明注释，实际输出仍禁止 queue 实例；redis-wipe 预览必须同时列出 `redis-ctl-g<N>`，原有准入与不执行操作的断言保留。

同期出现的发号器预检、MySQL 告警断言等差异属于其他会话，不是本次修复。

## 验证证据

日志目录：`C:\Users\luyua\AppData\Local\Temp\redis-ledger-review-20261106`。

- `match-ledger-red.log`：先改回归断言，生产脚本未修时 2 项均失败，实际值分别为默认池 1 和 `1/2/0/0`。
- `match-ledger-green.log`：修复后同两项 2/2 PASS，退出 0。
- `match-ledger-regression.ps1`：通过 AST 提取两个仓库原始 Test-Case 与正式生产函数，保留可复现的定向入口；没有复制另一套断言。
- `redis_maxclients_contract.log`：完整 7/7 PASS，退出 0。
- `redis-player-cluster-full.log`：完整 34/34 PASS，退出 0，包含普通/集群形态下六服务不生成登录 Redis 配置的检查。
- `k8s_redis_split_contract.log`：首次完整执行 41 PASS、2 FAIL。失败仅为上述过时断言：缺省 infra-up 的两个 `redis-queue-g` 命中都是模板整行注释，非注释命中为 0；实际清库选择器与预览均已包含控制域实例。
- `k8s_redis_split_contract.after.log`：修改后完整 43/43 PASS，退出 0，包含高可用形态、连接/代理总账、形态切换闸、状态检查和清库替身。
- 四个修改过的 PowerShell 文件解析无错误（`powershell-parse.log`）；针对本次文件的 `git diff --check` 无空白错误。

完整 `k8s_deploy_contract.tests.ps1` 的基线运行在修生产代码前发现三项发布轨道失败，已停止，不能当作整套通过。日志 `k8s_deploy_contract.before.log`：

1. canary zone 的 Deployment 用例调用未定义的 `Select-CppWorkloadBlock`。
2. canary zone 的 Agones Fleet / external gate 用例同样调用未定义函数。
3. 全局 canary 用例以 `-canary\b` 搜索稳定版整个块，命中后失败。

本轮没有修改以上发布轨道用例，没有修改服务业务代码，没有运行 Go/C++ 构建、真实 Kubernetes 部署或故障切换。排队 HA 的契约测试结果与真实集群验收应分别表述。

Java：本次修复属于本仓 PowerShell 部署总账，Java 版没有同一实现，代码修复不适用；六服务身份清退与 Redis 拆分的 Java 状态仍以 `D:\luyuan\xuanming-server-mmo\PARITY.md` 为准（0.1.0-SNAPSHOT 的既有待办），本次未改 Java。

## 交接状态

已找到本机 Claude Code CLI，但 `auth status --text` 返回未登录。Claude 桌面 Code 页面已登录并选中本仓库，但显示 `Weekly limit reached`。本轮没有成功提交审核请求，尚无 Claude 审核结论；未变更账号、套餐或额度设置。

恢复可用后，交给 Claude Code 的请求可直接使用：

> 请在 `D:\luyuan\xuanming-server-mmo-cpp` 独立审核本次 Redis 连接总账修复，先读 `AGENTS.md` 和 `docs/handoff/redis-login-ledger-review-20261106.md`。审核方法和判断由你决定，不预设结论，可以质疑修复依据并检查相关遗漏。请给出发现、影响、证据与审核边界；需要额外运行验证时，按仓库分工给 Codex 具体命令。当前同文件存在其他会话改动，请核对归属。

## 2026-11-10 Claude 独立审核结论

只读审核：没有改代码，没有编译，没有运行任何测试或程序。

### 审核边界

- **代码版本**：HEAD `e0790ad40` 加 11-10 的工作区。本次修复随 `de6891478` 进入主干；工作区里 match 那一行和三份测试的相关断言与 `de6891478` 相同。`k8s_deploy.ps1`、`go/guild/guild.go`、`deploy/k8s/README.md` 审核期间仍有其他会话在改，下文按函数名、变量名引用，行号以当时为准。
- **读了什么**：
  - go/match：`internal/svc/servicecontext.go` 的 `NewRedisHandles`，`internal/config/config.go` 的 `EffectivePlayerRedis`，`match_service.go` 的口令解析。
  - go-zero v1.10.0 模块缓存：`core/stores/redis` 的 `NonBlock` 缺省值与 `NewRedis`，`zrpc/server.go` 的 `setupAuthInterceptors`。
  - `k8s_deploy.ps1`：match 的 ConfigMap 模板、`Apply-Infra` 的基础设施清单、四份 Redis 总账的输入函数、`Write-RedisConnectionLedger`。
  - 三份测试在 `1194c4d46..de6891478` 之间的差异。
  - 登录 Redis 清退涉及的六个服务（chat、friend、guild、match、mail、trade）里全部 Redis 客户端的构造点。
- **没能核对的**：交接引用的 `C:\Users\luyua\AppData\Local\Temp\redis-ledger-review-20261106` 在 11-10 已不存在，红/绿日志与 AST 复现脚本都无法重读；`build/verification` 下也没有 11-06 之后这几份契约测试的运行记录。所以下面的「修复正确」是静态推演，运行证据要按文末命令重跑。

### 结论：match 行的修复正确

1. `NewRedisHandles` 只建一个 go-zero 句柄。只有 `MatchRedis.Host` 为空时，它才回落到 `Redis` 段，也就是核心库。
2. K8s 上不会走到这个回落：
   - match 的 ConfigMap 恒写 `MatchRedis.Host`，值是六个 `redis-match-cluster-N` Pod 的 headless DNS，`Type: cluster`。
   - `Apply-Infra` 无条件部署 `redis-match-cluster.yaml`。
3. match 读 `Redis` 段的地方只剩 `EffectivePlayerRedis`，也就是玩家域没写 `Addrs` 时的回落。这部分由行上的 `PlayerMirror` 计入：
   - `PoolSizePerNode` 是 20，ConfigMap 镜像同一个键；
   - `playerredis.Config.OrDefault` 保留段里写的池上限，不会被回落值覆盖。
4. zrpc 服务端只在 `Auth: true` 时连 Redis（go-zero v1.10.0 的 `setupAuthInterceptors`），match 没有开 `Auth`。
5. 拆出玩家域集群后，PlayerRedis 搬走，match 行四项都是 0，由既有过滤删掉。`Write-RedisConnectionLedger` 按 `Get-PlayerRedisTopology` 传 `-PlayerDomainSplit`，所以部署时走的就是这条过滤。
6. 数值：每副本从 40 条改为 20 条（未拆）。拆了之后，原来每副本留 20 条，现在整行移除。按 2 副本、滚动余量 R=2 计，规划档峰值减少 80，与交接里「每副本多计 20 条」一致。
7. 附带一点：多计并不是登录 Redis 清退造成的。`NewRedisHandles` 至少从 `7c6ecb885` 起就只建一个句柄，旧注释里的 `sharedRds` 早就过时了。这不影响修复本身，只是把过去的规划峰值多算了 80（偏保守）。

**三份测试的改动没有放宽断言**：

- `k8s_deploy_contract`：只改了期望常数。
- `redis_player_cluster_contract`：比原来更严，mail、match 都逐项核对未拆时的池数与 `0/0/0/0`，拆后不留行。
- `k8s_redis_split_contract` 的 queue 判据：现在只排除整行注释。
  - 依据：`redis-purpose.yaml` 第 1、4 行的模板头注释会随每次都 apply 的 `redis-ctl-g<N>` 一起渲染出来。
  - 行尾注释、真实资源行、操作行照样会命中。
- 清库预览串：改成与 `Invoke-RedisWipe` 的实际文案一致，要求多出 `redis-ctl-g<N>`，比原来更严。

### 发现

**1. guild 的申请配额客户端没进核心 Redis 总账**（低：账目漏项，不是当前容量风险；11-10 已修，见下文「修复三处发现」）

- **漏的是什么**：`go/guild/guild.go` 主流程装配 `NewApplicationQuota` 之前，有一句 `quotaRedis := redis.NewClient(...)`。
  - 地址是 `RedisClient.Host`，`PoolSize: 4`、`MaxActiveConns: 4`。
  - `ApplicationQuotaPerMinute` 的校验要求取值在 [1, 600]（缺省 10），所以配额恒开。
  - K8s 的 guild ConfigMap 把 `RedisClient.Host` 写成 `redis.<infra>:6379`，所以不论拆没拆玩家域，这个客户端都连核心库。
- **总账现状**：guild 行记了四项，没有配额池：
  - 两个显式池（`RedisClient`、`PlayerLocatorRedis`）；
  - 唤醒订阅 1 条；
  - `PlayerFixed = 4`（摘要 bump 池，在玩家域）。
  README 公式 `2P + 1 + 4` 里的 4 也是摘要池。
- **来历**：配额池在 `de4960bee`（10-15）加入，第二天 `d61932f13` 把 guild 接进 K8s 总账时就漏了。
- **影响**：2 副本时稳态少计 8 条、滚动余量（R=2）少计 8 条，规划档峰值共少计 16 条。离 `maxclients` 40000 很远，但总账「每行有代码出处、逐项可核」的性质被破坏了。
- **建议改法**：
  - guild 行 `Fixed = 4`，拆不拆都留在核心总账。
  - Go 侧把字面量提成具名常量（如 `applicationQuotaRedisPoolSize`），契约测试照 `ownerEpochMintPoolSize` / `playerSyncRedisPoolSize` 的写法从源码读出来比对。
  - 同步改的地方：
    - `redis_player_cluster_contract` 里 guild 拆分后的期望 `"$guildControl/0"` 改为 `/4`；
    - README 的 guild 公式；
    - `Get-RedisConnectionLedgerInputs` 注释里 `Fixed` 的说明：原文只写「非 Go 客户端」，实际也装代码里写死的 Go 小池。
  - `go/guild/guild.go` 正被其他会话修改，动手前先对齐。

**2. 修复依赖的新前提没有测试钉住**（低：测试缺口；11-10 已修，见下文「修复三处发现」）

- **前提**：`DefaultPools = 0` 成立，全靠 ConfigMap 恒写 `MatchRedis.Host`。
- **为什么需要测试**：一旦哪天 K8s 不写这一段（例如给 `redis-match-cluster` 加了可选开关），`NewRedisHandles` 会静默回落到核心库。后果有两个：
  - 每副本多一个 go-zero 缺省池（10 × GOMAXPROCS = 20 条），总账少计；
  - 观战键写进核心库。
- **现状**：原来的 `DefaultPools = 1` 把这种情况一并按保守值算进去了；改成 0 之后，这层兜底没有了。`tools/scripts/tests` 下没有任何用例断言 match ConfigMap 的 `MatchRedis` 段。
- **建议**：在 `k8s_deploy_contract` 加一条 DryRun 断言：`go-svc-match-config` 的 `MatchRedis.Host` 等于六个 `redis-match-cluster-N.redis-match-cluster.<infra>.svc.cluster.local:6379`，`MatchRedis.Type` 是 `cluster`。Because 写明这条断言是总账 match 行为 0 的前提。

**3. 文档与注释过时**（低；11-10 已改正注释，见下文「修复三处发现」；chat 的两个死句柄 11-11 已删，见文末「删除 chat 的两个死句柄」）

- `deploy/k8s/README.md`「Redis 连接总账」一节：
  - 「设不了的」名单还列着 match；
  - 公式写的是 `match / friend 每副本 = 10G`。

  match 应当与 mail 同列，写成「每副本 = P，只有玩家域句柄；建了玩家域集群这一行就搬走」。README 虽然写了「条数以代码表为准」，但逐服务公式是明确陈述，会误导下一次手算。
- `k8s_deploy.ps1` 的 `Get-RedisPoolMirror` 注释，仍把 match 列在 go-zero 缺省池客户端里。
- chat 的两处说法与代码不符：
  - 总账「不计入」名单与 README 都写「chat 的 ChatRedis：K8s 上指向 redis-match-cluster」。实际 chat 的 ConfigMap 从不写 `ChatRedis`。
  - chat ConfigMap 模板的注释写「正式SQL聊天不创建此句柄」。实际 `go/chat/internal/svc/servicecontext.go` 的 `NewRedisHandles` 无条件 `redis.MustNewRedis(c.Redis.RedisConf)`；K8s 上没配 `ChatRedis`，两个句柄都是这一个。
- chat 今天确实不产生连接，「chat 不连 Redis」实际成立，原因有两条：
  - `NonBlock` 缺省 true，构造时不 ping；
  - `ChatRedis` / `SharedRedis` 两个字段没有运行时读者，只有启动横幅打印 `ChatRedisTarget`。
- chat 的风险：以后有人按注释以为 `svcCtx.ChatRedis` 在 `redis-match-cluster` 上而开始用它，压力会落到核心库，而且走的是没记账的缺省池。
- 建议：改正这几处注释；更彻底的做法是删掉 chat 里这两个死句柄。删句柄是 go/chat 改动，要 Codex 编译测试。

### 发布轨道三项失败的现状

- **HEAD `e0790ad40` 仍然存在**（静态阅读）：
  - 第一、二项：测试在文件前部调用 `Select-CppWorkloadBlock`，这个函数在文件后部才定义。PowerShell 按顺序执行脚本，调用时还没有定义。
  - 第三项：稳定版块的断言用 `-canary\b`，而夹具的 infra namespace 是 `contract-canary-infra`，稳定版清单处处引用它。
- **工作区已有修正**（发布轨道会话 11-10 所改，未提交，未运行）：
  - 改法：
    - 先定义自带的 `Select-ReleaseTrackWorkload` 再调用；
    - namespace 改成 `contract-track-infra`；
    - 稳定版断言收窄为 `\b<svc>-canary\b`。
  - 评价：
    - 三处都对准了根因。
    - 收窄后的判据仍能抓到金丝雀自己的资源名与 Secret（如 `chat-canary-request`）。
  - 验证：归发布轨道会话的 Codex 清单（`docs/PROGRESS.md` 11-10「发布轨道」条第 4 步），不在本修复范围。

### 交 Codex 复验

**本修复只改 PowerShell，不需要 Go / C++ 构建。**

执行约定：

- 在仓库根目录、同一个 pwsh 会话里执行，先 `Get-ChildItem Env:MMORPG_* | Remove-Item`。
- 先记下 `git rev-parse HEAD` 和下面几份文件的 `git diff --stat`。其他会话仍在改 `k8s_deploy.ps1` 与测试，结果要能对上源码版本。
- 红了保留第一条失败的完整输出，不改断言，不连续重试。

命令：

1. `pwsh -NoProfile -File tools/scripts/tests/redis_player_cluster_contract.tests.ps1`：全部 PASS。其中必须有「控制域总账:没建集群时这些池记在控制域;建了集群就搬走,只剩玩家域句柄的 mail / match 不留行;chat 不连 Redis,两份都没有它」。
2. `pwsh -NoProfile -File tools/scripts/tests/k8s_redis_split_contract.tests.ps1`：全部 PASS。其中有「缺省 infra-up DryRun:不渲染或操作 queue 实例,共享模板的整行说明注释不算部署」和「redis-wipe DryRun:…」两条。
3. `pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1`：全部 PASS。
4. `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`：
   - 以下三条必须 PASS：「Redis 总账输入:…」「Redis 连接总账:规划档峰值 ≤ 80%…」「G3b Redis 连接总账:scene-manager 行…」；
   - 以「发布轨道:」开头的各条，按发布轨道会话的清单判定；
   - 其余失败逐条点名，不笼统写「有既有失败」。

发现 1～3 修完后，重跑第 1、4 条。如果按建议在 guild 提了具名常量，再在 `go/guild` 下跑 `go vet ./...` 与 `go test -count=1 ./...`（`GOTOOLCHAIN=local`、`GOPROXY=off`）。

### Java

- **本修复**：不适用。Java 版的 Redis 总账是 Java 仓 `docs/design/architecture.md` §6.1 的手算账，每个进程只有一个 Redisson 客户端，没有 go-zero 回落句柄。
- **发现 1**：同理不适用。这是按该节的描述判断的，没有逐个核对 Java guild 的代码。
- 不新增 `PARITY.md` 条目。

## 2026-11-10 Claude 修复三处发现

负责人 11-10 回复「修这三处」。已落码，未编译，未测试，未提交。

### 改了什么

**发现 1：guild 申请配额池记入总账**

- `go/guild/internal/logic/application_quota.go`：新增具名常量 `ApplicationQuotaRedisPoolSize = 4`。注释写明：它连核心库，总账按它记，契约测试从这一行读值。
- `go/guild/guild.go`：配额客户端 `quotaRedis` 的 `PoolSize`、`MaxActiveConns` 改为引用这个常量，值不变，仍是 4。只改了这两个字段所在的一行。
- `go/guild/internal/logic/application_quota_test.go`：真 Redis 用例 `TestApplicationQuotaRealRedis` 自建的客户端也改用这个常量。
- `tools/scripts/k8s_deploy.ps1`：
  - `Get-RedisConnectionLedgerInputs` 的 guild 行，`Fixed` 由 0 改为 4。行上注释写明出处，以及「拆不拆玩家域都留在这一份」。
  - 同一函数头注释里 `Fixed` 的定义改为：不按池配置键记账的固定连接数，包括非 Go 客户端（Java gateway）和代码里写死池上限的 Go 小池（guild 的申请配额池）。
- 测试：
  - `k8s_deploy_contract.tests.ps1` 新增一条用例「Redis 连接总账:guild 行 Fixed 含 go/guild/internal/logic/application_quota.go 的 ApplicationQuotaRedisPoolSize(…)」，接在 G3b 那条 scene-manager 用例之后。它核对：
    - 常量从源码读出；
    - `quotaRedis` 的装配恰好一处，连 `RedisClient.Host`，两个池字段都引用常量；
    - 未拆时 `Fixed` = 配额池 + 摘要池，拆后 `Fixed` = 配额池。
  - `redis_player_cluster_contract.tests.ps1`：拆分后 guild 的期望由 `"$guildControl/0"` 改为 `"$guildControl/<配额池>"`，配额池从源码读。
  - `lobby_authority_render.tests.ps1`：guild 行 `Fixed` 的期望由「摘要池」改为「摘要池 + 配额池」。`Fixed` 改成 8 之后原断言会红，所以同批改。
- **数值影响**：每副本核心连接都 +4，未拆时 `Fixed` 4→8，拆后 0→4。2 副本、R=2 时，规划档峰值 +16。

**发现 2：给 MatchRedis 前提补契约测试**

- `k8s_deploy_contract.tests.ps1` 新增一条用例「Redis 连接总账:match 行没有缺省池的前提 —— match ConfigMap 恒写 MatchRedis,指向 redis-match-cluster 的每个 Pod,Type cluster」。
  - 位置：接在「match 不再发评分快照」之后，复用同一份 `$guildInfraRun`（infra-up DryRun，namespace `contract-battle-infra`）。
  - 核对 `MatchRedis.Type` 是 `cluster`。
  - 期望的 `MatchRedis.Host` 从 `redis-match-cluster.yaml` 的 `replicas`（现为 6）推出来，不照抄生成器里的 `0..5`。清单改了副本数而生成器没跟上，这条也会红。
  - 最后核对总账 match 行的 `DefaultPools/GoMaxProcs` 为 `0/0`，让前提和账目写在同一条用例里。
- `k8s_deploy.ps1` 总账的「不计入」名单和 README 同步写明：这一段不写时，match 会静默回落到核心库，match 行就不能再记 0。

**发现 3：注释与文档**

- `k8s_deploy.ps1`：
  - `Get-RedisPoolMirror` 注释：缺省池客户端名单去掉 match，补一句：match 的句柄在 K8s 上恒指向 redis-match-cluster。
  - 总账「不计入」名单：
    - chat 一条改为：K8s 的 ConfigMap 不写 ChatRedis；`NewRedisHandles` 仍按 Redis 段建一个 go-zero 句柄，但不预连、没有调用方发命令，所以不建连接。
    - match 一条去掉 chat 的 ChatRedis。
  - chat ConfigMap 模板里的注释改为与代码一致。这行注释会渲染进 chat 的 ConfigMap，但只是 YAML 注释，不影响配置值。
- `deploy/k8s/README.md`「Redis 连接总账」一节：
  - 「设不了的」名单去掉 match。
  - 公式：
    - friend 单列 `10G`；
    - match 并入 mail 一行，写成 `match / mail 每副本 = P`；
    - guild 改为 `2P + 1 + 4 + 4`，写明配额池与摘要池各 4。
  - 「不计入」：chat 一条补「K8s 不写 ChatRedis」，match 一条去掉 ChatRedis。
  - 「契约测试钉住」：补上新增的两条。

### 有意没做的

- **chat 的两个死句柄**：这一轮没删，只改正了注释。负责人随后要求删除，11-11 已删，见文末「删除 chat 的两个死句柄」。
- **guild.go 配额客户端上方的一行旧注释没动**。注释说 `svcCtx.PlayerSyncBumper` 是「指向核心 Redis DB 0 的专用客户端」，实际是玩家域客户端，没建集群时才回落到核心库。这一行不在这次的发现里，也不在我改的行里。
- **chat 的一条中文启动日志没改**。`NewRedisHandles` 里打的是 `[chat] 旧缓存句柄沿共享配置建立…`，按 AGENTS §2 应改英文，归「全仓服务日志改英文」会话。

### 静态检查（Claude 实际做了的）

- 三个 Go 文件 `gofmt -l` 无输出。
- 改过的四个 ps1（`k8s_deploy.ps1`、`k8s_deploy_contract`、`redis_player_cluster_contract`、`lobby_authority_render`）PowerShell 解析 0 错误。
- 八个文件 `git diff --check` 无空白错误；换行符与各文件原有的一致：两份契约测试是 CRLF，其余是 LF。
- 用新用例里的同一批正则直接匹配源码，结果都对上：
  - 常量恰好一处，值是 4；
  - `quotaRedis` 装配恰好一处，连 `RedisClient.Host`，两个池字段都引用常量；
  - `playerSyncRedisPoolSize` 恰好一处；
  - `redis-match-cluster.yaml` 的 `replicas` 恰好一处，值是 6。

  这只是核对正则写法，不算运行测试。

### 交 Codex 验证

执行约定：

- 同一个 pwsh 会话里先 `Get-ChildItem Env:MMORPG_* | Remove-Item`。
- 先记下 `git rev-parse HEAD` 和下列文件的 `git diff --stat`。
- go/guild 和 `k8s_deploy.ps1` 里还有其他会话未提交的改动。红了先看失败用例是不是本批的；保留第一条失败的完整输出，不改断言，不连续重试。

步骤：

1. **Go**：工作目录 `go/guild`，设 `GOTOOLCHAIN=local`、`GOPROXY=off`，依次跑 `go vet ./...`、`go build ./...`、`go test -count=1 ./...`。
   - 本批只把字面量 4 换成同值常量，期望全部通过。
   - 没设 `GUILD_QUOTA_TEST_REDIS` 时 `TestApplicationQuotaRealRedis` 是 SKIP，属正常，写明即可。
2. **脚本契约**（仓库根目录）：
   - `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`：
     - 两条新用例必须 PASS：「Redis 连接总账:guild 行 Fixed 含 …ApplicationQuotaRedisPoolSize…」和「Redis 连接总账:match 行没有缺省池的前提 …」。
     - 以下三条也要 PASS：「Redis 总账输入:…」「Redis 连接总账:规划档峰值 ≤ 80%…」「G3b Redis 连接总账:scene-manager 行…」。
     - 以「发布轨道:」开头的各条，按发布轨道会话的清单判定。
   - `pwsh -NoProfile -File tools/scripts/tests/redis_player_cluster_contract.tests.ps1`：全部 PASS，其中有「控制域总账:…只剩玩家域句柄的 mail / match 不留行…」。
   - `pwsh -NoProfile -File tools/scripts/tests/lobby_authority_render.tests.ps1`：退出码 0。
   - 下面两份本批没改，作回归用，全部 PASS：
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_redis_split_contract.tests.ps1`
     - `pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1`
3. **红/绿**（可选，建议做）：
   - 用 11-06 那样的 AST 定向方式，拿新增的 guild 用例分别跑两版 `k8s_deploy.ps1`：
     - HEAD 版：guild 行还是 `Fixed = 0`，应当红；
     - 工作区版：应当绿。
   - 不要为此改动工作区里的正式文件。

### Java

三处只涉及本仓的部署脚本、契约测试、README 和 go/guild 的一个常量。Java 版每个进程只有一个 Redisson 客户端，没有独立小池，所以不适用，不新增 `PARITY.md` 条目。

## 2026-11-11 Claude 删除 chat 的两个死句柄

负责人回复「删掉死句柄」。已落码，未编译，未测试，未提交。

### 改了什么

**go/chat**

- `internal/svc/servicecontext.go`：
  - 删掉 `ChatRedis`、`SharedRedis`、`ChatRedisTarget` 三个字段和 `NewRedisHandles`；
  - `fmt` 与 go-zero `stores/redis` 两个 import 随之删掉；
  - 包注释改为「chat 不连 Redis」。
- `chat.go`：启动横幅删掉 `chat_redis:` 一行。go、tools、robot 下没有任何脚本解析这一行。
- `internal/config/config.go`：
  - 删掉 `ChatRedis` 字段和 `stores/redis` import；
  - `Validate` 删掉「`Redis.Host` 不能为空」这条校验及其说明；
  - 类型注释改写为：chat 不连 Redis；`RpcServerConf` 的 `Redis` 段是 optional，只在 `Auth=true` 时用。
- `internal/config/sensitive_words_test.go`：删掉 `cfg.Redis.Host` 这一行，它只为通过上面那条校验而写。
- `etc/chat.yaml`：删掉 `Redis` 和 `ChatRedis` 两段，换成两行说明。

**为什么能整段删**

- go-zero v1.10.0 里 `RpcServerConf.Redis` 是 `json:",optional"`。
- go/db、client_rpc_router 同样嵌入 `RpcServerConf`，yaml 里都没有 `Redis` 段。
- chat 不开 `Auth`，zrpc 不会连 Redis。

**部署与脚本**

- `k8s_deploy.ps1`：
  - chat ConfigMap 模板删掉 `Redis` 段，换成一行注释说明为什么不写；
  - 总账「不计入」名单的 chat 一条改为：go/chat 不建任何 Redis 句柄，ConfigMap 也不写 Redis / ChatRedis 段。
- chat 不在 `SharedRedisAuthGoServices` 里，本来就没有口令注入和指纹注解，manifest 不用改。
- `start_game.ps1`：两行注释原来写 chat「只读写 Redis 历史」「只依赖 etcd + Redis」，已改正。

**测试**

- `k8s_deploy_contract.tests.ps1` 的「chat ConfigMap:带世界频道推送的 Kafka brokers…」一条：加一个断言，渲染出的 chat ConfigMap 里没有 `Redis:` / `ChatRedis:` 段。
- `chat_authority_deploy.tests.ps1`：
  - 模板分支的反向断言加上 `Redis:`；
  - 新增一个断言：`go/chat/etc/chat.yaml` 也没有这两段。

**注释与文档**

- `robot/chat_smoke_scenario.go` 的说明注释：原来写历史在 ChatRedis、幂等靠 SET NX，改为历史在 SQL 原件库、幂等靠 SQL 收据。
- `deploy/k8s/README.md`：「不计入」里的 chat 一条。
- `docs/design/shared-redis-split-by-purpose.md`：句柄盘点表的 go/chat 一行。

### 没动的

- **几份历史文档仍写着 ChatRedis**（下一节收尾时又逐个复核过，结论以这里为准）：
  - `docs/design/disconnect-reconnect-audit-20261020.md` 是 10-20 的审计快照，当时 chat 还在 Redis 上，不改。
  - `docs/design/data-consistency-guarantee-map-20261020.md` 第 58 行在 §1 里。文件头已写明 §0~§5 是 10-20 架构的快照，当时这一行是对的，不改。现状在 §6.1，表里已经含「队伍」（库内事务强一致）。此前说「这一行早就过时」不准确。
  - `docs/design/microservice-zone-contract-20260914.md` 的 §19（10-21）是 chat 的现行契约，其中两句与现状不符，11-11 已改正，见下一节。§9 等旧节由 §19 声明取代，不改。
  - `docs/design/turn-based-battle-server.md` 09-30 补充的风险前提已经不成立，11-11 加了带日期的更正，见下一节。
- **其他会话在 go/chat 里的在途改动没碰**：日志改英文、`least_privilege_tidb_test.go`、`chat_logic.go`。对方刚把 `NewRedisHandles` 里的一条日志改成英文，那条日志随函数一起删掉了。

### 顺带发现的疑点（11-11 已改）

- **现象**：`tools/scripts/start_game.ps1` 的 `Test-UsesKafkaCommandContract` 名单里没有 chat。
- **为什么是问题**：
  - chat 的世界频道推送写 `chat-world-bcast_g<N>`，代号同样取自启动器注入的 `KAFKA_COMMAND_TOPIC_GENERATION`。`shared/kafkacmd` 用 `sync.Once` 在进程内只读一次这个变量，主题在启动时就定了。
  - 这份名单的用途，是在复用已在跑的进程之前核对它的代号。
- **后果**：换代后复用旧的 chat 进程，世界频道推送会写进旧代号的主题，gate 收不到，玩家只能靠拉历史看到新消息。
- **范围**：只影响本机启动器。K8s 不受影响：`Add-GoSvcCommandTopicEnv` 对每个 Go 服务（含 chat）都注入代号，每次滚动都重新注入。
- **处理**：负责人回复「加进名单」，已改：
  - `start_game.ps1`：名单末尾加上 `chat`，上方注释写明原因。chat 由 `Start-LocalGoServices` 拉起，exe 名就是 `chat`，所以起服后的记录（`Save-LocalCommandContract`）和复用前的核对（`Assert-RunningCommandContract`）都会覆盖它。
  - `tools/scripts/tests/start_game_command_contract.tests.ps1`：chat 从「没有命令主题生产链的服务」那组移到生产者那组，跑同样三类用例——缺记录拒绝复用、保存后可复用、四个字段任一不匹配都拒绝。
  - `docs/design/chat-world-channel-fanout.md` 第 4 节「分区」一条补了一句。
- **一次性影响**：第一次用新脚本时，如果还有旧脚本拉起的 chat 在跑，启动器会拒绝复用，提示先有序停止旧进程。名单里其他服务也是同样的行为。

### 静态检查（Claude 实际做了的）

- chat 的四个 Go 文件和 robot 的一个文件，`gofmt -l` 无输出。
- 删掉 `fmt`、`stores/redis` 两个 import 之后，文件里已无引用；`errors` 在 config.go 里仍有 5 处使用。
- 四个 ps1（`k8s_deploy.ps1`、`start_game.ps1`、`k8s_deploy_contract`、`chat_authority_deploy`）PowerShell 解析 0 错误。
- `chat.yaml` 用 PyYAML 解析通过，顶层没有 `Redis` / `ChatRedis`。
- `git diff --check` 干净；换行符与各文件原有的一致。

### 交 Codex 验证

执行约定同上一节。go/chat 里还有其他会话未提交的改动，红了先看失败用例是不是本批的。

1. **Go**：工作目录 `go/chat`，设 `GOTOOLCHAIN=local`、`GOPROXY=off`，依次跑 `go vet ./...`、`go build ./...`、`go test -count=1 ./...`。
   - 真库用例没设环境变量时是 SKIP，属正常，写明即可。
   - robot 只改了注释，不用单独编。
2. **脚本契约**（仓库根目录）：
   - `pwsh -NoProfile -File tools/scripts/tests/chat_authority_deploy.tests.ps1`：全部 PASS。
   - `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`：「chat ConfigMap:带世界频道推送的 Kafka brokers,WorldHistoryCacheMs 逐字镜像服务 yaml」PASS；和上一节的两条新用例在同一次运行里看。
   - 下面三份读 chat ConfigMap 的其他键，作回归用，全部 PASS：
     - `pwsh -NoProfile -File tools/scripts/tests/redis_player_cluster_contract.tests.ps1`
     - `pwsh -NoProfile -File tools/scripts/tests/logical_db_deploy_contract.tests.ps1`
     - `pwsh -NoProfile -File tools/scripts/tests/k8s_migrate_gate.tests.ps1`
   - `pwsh -NoProfile -File tools/scripts/tests/start_game_command_contract.tests.ps1`：全部 PASS。其中 chat 的用例必须都在：
     - 「chat 缺少可信启动记录时拒绝复用」；
     - 「chat 保存契约后可按 PID/启动时间/generation/分区复用」；
     - 「chat 拒绝不匹配的 Generation / Partitions / ProcessId / StartTimeUtcTicks」四条。

     「没有命令主题生产链的服务不检查或写契约」这条也要 PASS，它的名单里已经去掉了 chat。
3. **本机冒烟**（可选）：
   - `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-start` 起 chat；
   - 启动不报配置错，尤其不能出现 `Redis.Key is not set` 这类加载错误；横幅里没有 `chat_redis` 一行；
   - 再跑一次 robot 的 chat 冒烟，输出 `CHAT_SMOKE_OK`。

### Java

这是本仓 go/chat 的死代码清理，不是功能改动，Java 版 chat 是自己的实现，不适用，不新增 `PARITY.md` 条目。

## 2026-11-11 Claude 收尾：match 集群补连接上限与总账，改正剩余过时说明

负责人要求：查出的问题现在修，按最标准的做法定，不等压测。已落码，未运行，未提交。

### 改了什么

**1. redis-match-cluster 补齐其他 Redis 实例都有的三件事**

核心、queue / ctl 实例、玩家域集群都有显式 maxclients、连接总账和部署后核对，只有 match 私有集群一样都没有：连接上限停在 Redis 缺省的 10000，还会被容器 nofile 悄悄压低，而且没有人记账。

- `deploy/k8s/manifests/infra/redis-match-cluster.yaml`：redis 容器改用启动薄壳，与玩家域集群同一写法：
  - `MAXCLIENTS=40000` 只定义一次，与核心、玩家域集群同值；
  - 打印 `redis fd limit:` 一行，硬限不够只打 ERROR、不拒启；
  - `exec redis-server ... --maxclients "$MAXCLIENTS"`。
  - **注意**：改的是 Pod 模板，下次 `infra-up` 会滚动重启六个节点。StatefulSet 缺省按序滚动，`nodes.conf` 在 PVC 上，已建好的集群会原样重新入群。项目未上线，不另设重启闸。
- `tools/scripts/k8s_deploy.ps1`：
  - 新增 `Get-RedisMatchConnectionLedgerInputs`。只有 match 一行：每副本对每个主节点一个 go-zero 缺省池（10 × GOMAXPROCS，G 按 2 记），maxclients 读集群清单。
  - 新增 `Write-RedisMatchConnectionLedger`，挂在 `Write-RedisConnectionLedger` 末尾，写路径每次都打一行 `Redis match connection ledger: per_master match_replicas=… steady=… peak=… maxclients=… warn_at=… headroom=…`；超过 80% 水位打 `[RedisMatchConnectionLedger]OverWarnRatio:`，只告警。
  - 新增 `Wait-RedisMatchClusterReady`。`Apply-Infra` 记下这次 apply 的清单内容，`-WaitReady` 时先等建群 Job `redis-match-cluster-init` 完成，再逐节点核对生效的 maxclients（复用 `Test-RedisLiveMaxClients`）。节点数读清单的 `replicas`。这一步排在全局 Go 服务之前。
  - 两处注释指向新的那份总账：核心总账「不计入」里 MatchRedis 那条、`Get-RedisPoolMirror` 注释里 match 那句。
- **数值**（按公式手算，以部署打印为准）：每主节点稳态 = 2 副本 × 20 = 40，滚动余量 = 2 × 20 = 40，加运维余量 50，峰值约 130；maxclients 40000，告警水位 32000。
- 测试：
  - `redis_maxclients_contract.tests.ps1` 新增 T8：`MAXCLIENTS` 恰好一处、与 `redis.yaml` 同值；唯一一条 `exec redis-server` 带 `--maxclients "$MAXCLIENTS"`；薄壳打印 fd 限值，硬限不足只打 ERROR、不 exit、不改 nofile。
  - `k8s_deploy_contract.tests.ps1`：
    - AST 抽取名单加 `Get-RedisMatchConnectionLedgerInputs`；
    - 「Redis 总账输入:…」的副本数 / CPU limit 钉子把 match 集群那份输入也纳进来；
    - 新增「Redis 连接总账:redis-match-cluster 单独一份…」：核对输入形状，再在 `$battleInfraRun`（带 `-WaitReady` 的 infra-up DryRun）里核对打印的那一行与纯函数算出的一致、规划档峰值不超过告警水位，以及等建群 Job、逐节点核对 maxclients 的两行 DryRun 输出。
- `deploy/k8s/README.md`「Redis 连接总账」一节：新增 match 集群一条；「契约测试钉住」补上 CPU limit 也覆盖 match 集群那份，以及 T8。

**2. 剩余的过时说明**

- `go/guild/guild.go`：配额客户端上方的注释原写 `PlayerSyncBumper`「指向核心 Redis DB 0」，改为「玩家域的专用小池客户端 newPlayerSyncRedis，没建玩家域集群时回落到核心库」。只改注释。
- `docs/design/microservice-zone-contract-20260914.md` §19 两句：
  - 「原账号由只读全局 login Redis 捕获」改为「取自 gate 签名的会话身份原件（`roleidentity.Original`），chat 不读 login 的 Redis」。已对照 `go/chat/internal/authority/actor_verify.go` 核实。
  - 「Redis 句柄仅是既有可丢缓存装配」改为「chat 不建任何 Redis 句柄」。
  - §19 其余句子这次没有逐条复核。
- `docs/design/turn-based-battle-server.md` 09-30 补充：加带日期的更正。这个集群 10-23 起已是 noeviction，chat 不再连它，静默淘汰的风险已不存在。
- `docs/design/redis-cluster-all-domains-20261110.md`（「全部 Redis 上 Cluster」那条线的设计稿）：
  - §4.8 与第 5 批注明「chat 删无用句柄」11-11 已做；
  - 第 6 批注明 match 集群的 maxclients、总账与 `-WaitReady` 核对已补，通用化时并入。

### 静态检查（Claude 实际做了的）

- 三个 ps1（`k8s_deploy.ps1`、`k8s_deploy_contract`、`redis_maxclients_contract`）PowerShell 解析 0 错误，新增的三个函数各只定义一次。
- `redis-match-cluster.yaml` 用 PyYAML 解析通过，四个文档都在，渲染出的启动薄壳与预期逐行一致。
- 部署脚本读 `MAXCLIENTS` 的正则、T8 的各条正则对清单逐个匹配，都对上。
- `gofmt -l go/guild/guild.go` 无输出。
- `git diff --check` 干净；换行符与各文件原有的一致：`k8s_deploy_contract` 全 CRLF，其余 LF。

### 交 Codex 验证

执行约定同前几节：同一 pwsh 会话先清 `Env:MMORPG_*`，记 HEAD 与文件 diff stat，红了保留第一条失败的完整输出。

1. `pwsh -NoProfile -File tools/scripts/tests/redis_maxclients_contract.tests.ps1`：T1~T8 全部 PASS。
2. `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`：
   - 必须 PASS：「Redis 连接总账:redis-match-cluster 单独一份…」「Redis 总账输入:…」，以及前几节点名的各条；
   - 以「发布轨道:」开头的各条，按发布轨道会话的清单判定。
3. Go：guild 只改了注释，与前面「修复三处发现」一节的 `go/guild` 命令一起跑。
4. **集群冒烟**（可选，dev 集群）：
   - 跑 `infra-up -WaitReady`，match 集群六个 Pod 滚动重启后回到 `cluster_state:ok`；
   - 输出里应有 `Redis match connection ledger: per_master match_replicas=2 …`；
   - 还应有 `Redis maxclients check OK: live=40000 manifest MAXCLIENTS=40000 nodes=6 statefulset=redis-match-cluster`；
   - 出现 `[RedisMaxClients]Mismatch` 时，看对应 Pod 日志里的 `redis fd limit:` 一行。

### Java

部署脚本、清单与契约测试只在本仓，Java 版没有 K8s 清单。不适用，不新增 `PARITY.md` 条目。
