# 换电脑接续：全部待做与旧电脑记忆要点（2026-11-27，做完删除本单）

> 负责人在新电脑上说「继续做完 handoff」时，Claude Code 从本单开始：先过 §1，再按 §3、§4 的顺序做。
> 旧电脑已回收，上面的 Claude 记忆没有搬过来，也不用导入任何东西：新电脑要知道的要点在本单 §7，记忆里记着、交接单里没写的待做在 §8。
> 本单是 11-27 的快照。别的交接单开头的状态行比本单新，两边不一致时以那份为准，并顺手改本单；`docs/handoff/` 里有本单没列的新单，按它自己的开头做，再补进本单。

**怎么收尾**：某份交接单的事全部做完（代码写完，Gemini 验证全绿并写进 PROGRESS），就删掉那份。本单列的都做完了，最后删本单。每次删完：

1. 去掉 `docs/README.md` 索引里那几行（做法见 §7.2「文档索引」）；
2. PROGRESS 追加一条，写明删了哪几份，原文用 `git show <删除前的提交>:docs/handoff/<文件>` 取。

## 1. 开工核对（新电脑，Claude Code）

旧电脑回收后，没推送到 GitHub 的东西都没了。下面缺一条就停下问负责人，不要凭印象补写。

1. 拉取 main：本仓、客户端仓 `mmorpg-client`、Java 仓 `xuanming-server-mmo`（按 AGENTS §9 的 GitHub 地址找，不写死盘符）。再跑 `git submodule update --init --recursive`：要不报错，`git submodule status` 每行开头是空格。
   - 11-27 核对时，子模块 `third_party/ue5navmesh` 在旧电脑上检出的是 `4c6b821`（11-11「保存导航网格构建兼容性改动」）。它不在任何分支上，也没推到子模块仓；本仓 HEAD 记的还是 `aacf2f82`。这一步报错，多半是本仓提交了新指针，子模块那个提交却没推上去。
2. `git log --oneline -1 -- docs/handoff/new-computer-start-here-handoff-20261127.md` 有输出（本单已入库）。
3. 各单自己的开工核对也要过：r2 剩余单 §1、合服副本单 §2 第 1 步。
4. 不随仓库走、也不用找的：旧电脑的 `build/verification` 运行证据、客户端 `Logs/`、Docker 卷里的节点日志、会话草稿目录。PROGRESS 写了「已验证」的以 PROGRESS 为准，要原件只能重跑。

## 2. 规矩（AGENTS.md 照常全文读，这里只列最容易做错的）

- 只在 main 上改，不开分支、不开 worktree；提交与推送由负责人做。
- 代码与文档由 Claude Code 写。Gemini 只跑生成、编译、测试、冒烟；跑出要改代码的地方，保留首错原文交回 Claude Code。Codex 只作图。
- 写代码和做分析按 AGENTS §10.3 用最高档模型。不是最高档时先问负责人一句，负责人明说用当前模型就照做（旧电脑上 11-26、11-27 负责人放行过 Opus 5.5）。
- Java 独立仓只在 `PARITY.md` 登记待做，不写 Java 代码。
- 每修一处，在所属设计稿写五项：问题、方案、为什么这样选、被否决的做法、代价与边界。负责人没想到的好方案一定写进去。PROGRESS 只追加；交接单的状态行同步改。
- 全部 Claude 模型的周额度过 80% 时，只做风险最高的，然后停下报告。
- 手上的事做完、没有别的要做时，明确告诉负责人「可以关窗口」。

## 3. 要写代码的（Claude Code，按这个顺序）

| 顺序 | 在哪 | 要做的 |
|---|---|---|
| 1 | `data-overwrite-audit-r2-remaining-handoff-20261127.md`（r2 剩余单） | r2 这条线的全部剩余：复核批 V/W/X、§7.4 与 §6.1 各条，r2-craft#1 批 C、批 B，guild 人工终结的条件写 SQL，各批顺带发现，文档，第 5 轮查找。照它 §1 做到 §8，做完按 §9 删 |
| 2 | 本单 §3.1 | 选角页回选上限的用例（客户端仓，只加用例）。r2 剩余单 §3.4 列的是同一件事，只做一次，两边都标状态。不依赖第 1 行的代码，在第 5 轮查找之前做完即可 |
| 3 | `admin-identity-dual-control-batches4-7-handoff-20261126.md` §5「批 7 剩余」 | 第 1 项（后台上 K8s）与第 3 项 11-27 已写完、未运行，交 Gemini，命令在 PROGRESS 标题带「双人复核·批 7 剩余」的条目。第 2 项（本机后台到 trade 的转发）原定前提不成立，等负责人定做法（§5），定了再写 |
| 4 | `merged-zone-etcd-copy-handoff-20261127.md` §2 | 代码已写完。先交 Gemini 跑 §3；全绿后做 §4（第二批的独立复核等） |
| 5 | `login-queue-verification-handoff-20261126.md` §4 | 开闸：Claude Code 改常量与契约用例。前提是第 2、5、7 步全绿并写进 PROGRESS，且负责人确认 |

### 3.1 选角页回选上限的用例（11-27 从 review-suggestions 交接单 §8 移过来）

负责人 11-27 定：换电脑由 Claude Code 写这条用例，Gemini 只编译、跑测。只加用例，不改产品代码。写完在本节标状态，并在 PROGRESS 追加一条。

- **要钉住的行为**（客户端仓 `Assets/Scripts/Game/GameClient.cs`，行号是 11-27 的，以代码为准）：
  - 选角循环在 `ConnectAndEnter` 里，约 1255–1305 行。EnterGame 受理之后服务端推进场失败（3023），`EnterGameAndWaitScene`（约 1452 行起）调 `onReselect(EnterRejectedReselectHint)` 回选角页。
  - 只有 `reselects < MaxEnterReselects` 时才给回选的回调（约 1302 行；`MaxEnterReselects = 3`，在 `GameClient.RoleSwitch.cs`）。三次用完之后再来 3023，照旧拆连接、回选服。
  - 3023 由推送带进来：`DispatchInbound` 里 `if (_awaitingSceneEntry && IsEnterFailureTip(tip.Id)) _enterFailedTipId = tip.Id;`，约 2865 行。先读这一段，弄清推送的消息号与包体。
- **用例**：新文件 `Assets/Tests/EditMode/Net/EnterGameReselectCapTests.cs`，配 `.meta`，GUID 新生成。
  1. 用反射调私有协程 `ConnectAndEnter(gen, zoneId, ip, port, payload, signature, …)`（约 1151 行，其余参数照现有调用点填），直接连本机假 gate，不走 HTTP。
  2. 假 gate：验票，令牌 Login 回一份带两个角色的列表；每次收到 EnterGame 都受理（回 `EnterGameResponse { PlayerId }`），紧接着推 3023。
  3. `PlayerChooser` 每次都选同一个角色，并记下被调用的次数和每次看到的 `EnterRejectHint`。
  4. 断言：
     - 一共选了 4 次（首次 1 次 + 回选 3 次），后 3 次的提示都是 `EnterRejectedReselectHint`；
     - 第 4 次 EnterGame 受理后再推 3023，管线收掉：onError 只调用一次，连接已拆，`InGame` 为假；
     - 没有第 5 次 EnterGame。
  5. 另加一条对照：重定向或短重连路径（`_redirecting` / `_reconnecting` 为真）下，第一次 3023 就拆连接，不回选角。
- **可抄的夹具**：
  - `Assets/Tests/EditMode/Net/GameClientReconnectTests.cs`：`SetUp` 里的客户端装配；`ServeEnteredGate`，即验票、Login、EnterGame 的假 gate；`PumpRecovery`，按生产协程语义展开嵌套协程并等 `AsyncOperation`；还有反射辅助函数。
  - `Assets/Tests/EditMode/Net/EnterGameRoleSwitchFlowTests.cs`：按脚本逐条应答的假 gate（`Serve`），以及带 10 秒护栏的 `Pump`。
- **跑（交 Gemini）**：EditMode 跑新文件、`EnterGameRoleSwitchFlowTests` 与 `GameClientReconnectTests`，全部通过；客户端仓根跑 `tools/client_compile_check.ps1`。

## 4. 只交 Gemini 跑的（代码已写完）

1. **先统一生成**，很多线生成之前编不过：按 `data/AGENTS.md` 导 `Tip.xlsx`；仓库根跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`；客户端仓依次跑 `tools/gen_proto.ps1`、`tools/gen_messageids.ps1`、`tools/client_compile_check.ps1`。
2. **再全量编译、跑一遍单测**：C++ 整个解决方案与测试工程（改过库要先编库，见 §7.2）、Go 每个模块 `go vet ./...` 与 `go test -count=1 ./...`、Unity EditMode、`tools/scripts/tests/` 下全部契约测试。11-19 以前落码、只写了「未编译」的条目很多（命令只在 `git show ccf2840b2:docs/PROGRESS.md`），这一步一次盖住，不用逐条翻。红了按首错交回 Claude Code。
3. **然后按各单自己的顺序跑真库、真 Kafka 与冒烟**：
   - `account-deletion-handoff-20261120.md`
   - `battle-script-review-retention-handoff-20261126.md`
   - `config-table-hot-reload-handoff-20261126.md`
   - `data-consistency-review-handoff-20261126.md`
   - `gate-online-identity-simplify-20261105/CODEX.md`
   - `minigame-websocket-handoff-20261120.md`
   - `passport-hardening-handoff-20261126.md`
   - `real-name-anti-addiction-handoff-20261126.md`
   - `redis-cluster-all-domains-handoff-20261126.md`
   - `scene-gate-instance-session-handoff-20261126.md`
   - `third-party-login-and-anti-addiction-handoff-20261126.md`
   - `login-queue-verification-handoff-20261126.md` 第 2 节
   - `review-suggestions-remaining-handoff-20261126.md` 第 6 节；第 1~5 节的命令在 PROGRESS 2026-11-26「交接单第 1 节与第 4 节第 2~4 条」与「两份新交接单的代码改由 Claude 写」两条
   - `payment-integration-handoff-20261120.md`「还剩」：Go `-race` 要在 Linux 或 Docker 里跑；整链路冒烟；kind 冒烟要先经负责人同意装工具
   - `data-overwrite-audit-r2-handoff-20261126.md`：r2 全线的总验证。等 §3 第 1、2 行做完、审计文档 §1「仍开着」清空，再整体跑一遍最省事
   - PROGRESS 文末 11-26、11-27 标「未编译」「待 Gemini」的各条（data_service 启动期故障隔离三条、gate 准入 P2-4、官方通行证加固复查等），命令在各条里
   - data_service 与 go/db 的熔断按后端隔离：PROGRESS 2026-11-26 三条——「go/db 与 data_service 两处『熔断按后端隔离』复核」「data_service 逻辑库熔断：调用方取消不再记成成功」「go/db 按存档库熔断：调用方取消不再记成成功」，命令与通过标准在各条里。代码都已写完、在 main，只剩跑
   - 顺序：data_service 启动期故障隔离的批 1、批 2、第二轮三条，要先于「合服映射的 etcd 副本第二批」跑（都改了 `go/data_service/internal/svc/servicecontext.go`），细节在 `merged-zone-etcd-copy-handoff-20261127.md` §3。这几条都不是改代码：红了保留首错原文交回 Claude Code

## 5. 等负责人定的

- r2 线待负责人定的，见 r2 剩余单 §8（11-27 时是 r2-craft#1 批 C 前提②：K8s 各档位要不要打开 trade 的 `AssetOp.Enabled`）。
- 登录排队什么时候开闸（§3 第 5 行）。
- 本机后台到 trade 的转发怎么做：本机 Docker 调试栈里没有 trade 容器，原定做法的前提不成立（admin-identity 单 §5、设计稿 `admin-identity-and-dual-control.md` §11.4 末节）。
- 支付：回调用的公网 HTTPS 域名与证书、各渠道商户号与私钥（`server-code-completion-20261120.md` §3.1）；Java 版什么时候移植（`payment-integration-handoff-20261120.md`）。
- 论剑榜按模式分榜的页签没有设计稿，要不要补（§8）。

## 6. 不是代码、或只留档的

- `spectate-tiered-fanout-followup-20261121.md`：公网上线前在目标物理机或云主机混跑，复测 Gate 扇出 p99。
- `team-follow-afk-hosting-handoff-20261120.md`：实网多节点冒烟；Codex 补美术。
- `server-gap-audit-20261101.md`：缺口总账，不是待办单。11-26 晚核对时，写着交 Gemini 的代码都已写完；各条「仍剩」多是真集群上的核对（如第 1 条 Agones 的三条假设）与 Java。本单没有逐条重核。
- `server-code-completion-20261120.md`、`battle-report-replay-client.md`：已没有待做（前者 §3.1 的支付域名已列进 §5，后者 11-21 已闭环验证），收尾时一起删。
- `data-overwrite-audit-r2-round4-20261126/`：第 4 轮查找的原始结果（三个 JSON），第 4 轮单引用，随第 4 轮单一起删。

## 7. 旧电脑 Claude 记忆里的要点（11-27 快照；与代码、文档冲突时以代码与文档为准）

### 7.1 跟负责人协作

- 负责人用语音输入，同音错字按读音还原（如「代做」=「待做」，「Emily做完」=「没做完」）。
- 「按最标准的方法做」= 待拍板项按有依据的标准做法直接定案改码。五条标准：不丢数据、强一致、性能高、可水平扩展、代码易懂。一批要改 30 个以上文件时，仍先拿负责人对这一批的明确同意。
- 过时的设计稿直接改正文，写清新方案与原因；只加「已被取代」标注不算整理。
- 上下文快满而活没做完时，自己开新会话交接并继续，不让负责人去开；没有新活不开。
- 「java 留到 todo」= 本轮不写 Java 代码，只登记待做；核对结论是「不适用」或「已有等价」时不往 `PARITY.md` 加行。
- 负责人另有独立 Go 仓 `xuanming-server`，会贴那边的讨论来问本仓有没有；那边的改动不归本仓。
- 文档省篇幅：读 PROGRESS 只按标题 grep、读文末；新条目不贴大段原始输出。

### 7.2 工作区、工具与环境

- 同一工作区常有多个会话并行。动手前用 `git diff HEAD` 分清哪些是自己的改动；交接里写的提交与验证状态可能过时，先查 git log 与 PROGRESS 文末。
- 负责人会把整个工作区一次快照提交（含别的会话写到一半的文件），偶尔会把正在写的文件截断着提交。快照带走了自己的文件时，用 `git show HEAD:<文件>` 核一遍（Go 文件再过 `gofmt -l`）。
- 判断某段工作有没有进 main，用 `git log -S '<关键行>'` 查历史。`refs/archive/*`、悬空 stash、11-02 快照里不在 main 的内容都是被取代的旧方案，别并回来；合并旧分支会把已删的代码带回来，编译照样过。
- 子代理很耗额度：一个只读查找代理约占 7% 周额度，并行多条 workflow 会打满额度并留下半截改动。别给正在跑的 workflow 代理发消息（会把它恢复成认不出自己改动的新实例）。子代理按提交读文件时会把 `git show` 输出放进临时目录，提示里禁止，结束后清理。
- 跨天要用的清单与代理结果落进 `docs/handoff/`，草稿目录跨天会被清空。
- Bash 工具：长 heredoc 可能解析失败，`awk -v` 会吞 Windows 路径的反斜杠。`.ps1` 双引号里变量紧跟冒号要写 `${name}:`，否则整份脚本解析失败；改完用 `[System.Management.Automation.Language.Parser]::ParseFile` 查语法。
- 文档索引：增删文档后先跑 `python tools/scripts/gen_docs_index.py --check`。绿了再跑生成，diff 只许有本次增删的行；本来就是红的（别的会话没登记）或 diff 里有别的行，就停下手工改自己那几行。脚本不收各目录的 `README.md`。
- 导表器只改模板时单独渲染核对（先用旧模板渲染，与现有产物逐字节相同再写回），别跑整套导表，会带进别的会话的表改动。
- C++：`CMakeLists.txt` 由 `.vcxproj` 生成，新增 `.cpp` 只登记 `.vcxproj` 与 `.filters`；`tools/scripts/run_cpp_tests.ps1 -Build` 不重编库，改了库先编库；任何指针类型的成员都会被门禁拦（含 `const char*` 与测试里的局部结构体），首错即停、整个工程不跑，改用 `string_view` 或引用。
- C++ 协程：Cobalt 取消之后再 `co_await` 会抛 `operation_aborted`。
- 文本契约测试（`tools/scripts/tests/` 下的 `*.tests.ps1` 与 `test_*.py`）按名字钉着 C++ 成员、函数与配置键，改名后 grep 这些测试同步改；新加 match 后台循环要在 `loop_liveness_alerts_contract.tests.ps1` 的点位名单与节拍表登记。
- `third_party/muduo-linux` 的 gitlink 在两个提交间翻转过多次，核对 Linux muduo 接口先 `git ls-tree HEAD third_party/muduo-linux` 看当前指针；muduo 网络源码有三份副本，要同步改。
- Docker 绑定挂载在 Windows 上大小写不敏感，会掩盖头文件大小写错；拿 Linux 构建当证据前先看构建目录在哪。
- 运行证据：Gemini 的原件在 `build/verification/<运行>/`，判断「跑过没有」按用例名核对原件，再看那之后源码没改。C++ 节点日志在 Docker 命名卷里（文件名 UTC、行内东八区），Go 只有容器输出，客户端 `Player.log` 只留两次，客户端 `Logs/` 下的证据会被清掉。
- 真 Redis 验证要跑两版：K8s 钉 7.2，本地 compose 与 Java 版是 8.10.x。
- 真库回归自起一次性 MySQL 容器（`cc-<用途>-<日期>`，端口错开），别借 `codex-*` 容器（会被中途撤掉）；`go test` 报 Access denied 时用 `-c` 编出测试二进制再跑。
- WebFetch 报 `Unable to verify if domain … is safe` 可能是瞬时的，隔一阵重试一次。
- 核对 Casdoor 要读通行证用的 v4.27.1 tag 源码（Bash 里 curl raw.githubusercontent 直接取），凭旧版记忆的结论多数不成立。

### 7.3 已定案、别改回去

- 玩家存档只存 TiDB，不进 Redis；Redis 不撤，协调用途照用。
- 万人同屏等实时 MMO 的需求，等真做成实时 MMORPG 再改，现在不动、不预留。
- 重连或连上某类节点时不做业务逻辑：连接层只做身份握手与 Ready；实例消失看 etcd 摘除；丢消息由业务有主协议兜底，不靠重连补发。
- ECS 按需组件在写入口懒建，读路径不建，持久数据不懒建。
- gRPC 不删库，只保证运行期不起 gRPC 线程。
- 玩家存档里的集合要有上界：旧周期数据清掉，完成状态用位图；上线前清档，不读旧格式。
- go/db 运行期冷却与 storeerr 已删，别接回（按驱动错误类型判不出断连）。
- go-zero 熔断按方法计失败、不分分片：一片失联可能连健康分片一起挡，新的 data_service 调用方要过 `dsclient.Conf`。
- 新加把物品移出人物背包的入口，必须先问 `CheckRemovable*`（战斗道具预留）。
- 给 scene 加客户端服务或方法时，要决定它进不进换场景节点时的重投白名单；单向消息顺序与旧场景消息闸已定案。
- 客户端请求幂等键的账本禁止整体清空。
- AOI 关注的权重表是静态的，「权重随状态变」是定下的反例。
- 角色换主人后本机待确认记录：修订号未知不作废、不按修订号分目录。
- 发号器：声明指针不严格化、丢码时 rebind 不放行、本地超上界拒绝。
- 写者名册缩容收尾：先读目录、后看 Pod，是硬约束。
- 发布轨道的金丝雀判定门槛（设计 §2.8）：改门槛要同步 §2.8、规则与契约测试。
- 数据一致性：「单边失联丢 5 秒」已定案，不是待定项。
- r2 批 S/T/U：§8.2 的世代预检定案不做。
- 战斗结束直连：时限判平仍走补签是有意留的。
- 改 C++ 客户端可见行为前，先查 Java 仓 `PARITY.md` 与 `docs/reference/mmorpg-client-contract-*.md`，按 Java 已定口径对齐。
- 配置表热更 `STATUS.json` 的字段名、清单数组、`sync_errors`、带 namespace 的状态键是多方约定，改一边要同步另一边。

## 8. 记忆里记着、交接单里没写的待做（11-27 快照，没有逐条核对）

接手前先按指向的设计稿或 PROGRESS 核对是否已经做了，做了的直接划掉。11-19 以前的 PROGRESS 条目只在 `git show ccf2840b2:docs/PROGRESS.md`。

- 全服匹配：已建房票巡检 `ReadyAfter` 的分片，`assetop-redelivery-sharding-20261110.md` 写着按 11-06 约定由数据覆写审计会话接，没查到做完的记录。
- 客户端整图加载：负责人 11-10 定等新图做完再改，方案在客户端仓 `Docs/QdaoMapDeliveries20261102.md` 末节，未落码。
- 论剑榜按模式分榜：页签没有设计稿（列进了 §5）。
- 帮会恢复结论沿用：网络组三条红的补丁写回过客户端仓，只剩网络组重跑与联网冒烟。
- 聊天世界频道三批：验证前要清 chat 库；TTL 的写法没有真库证据。
- 战斗直连心跳：Unity 必须与服务端同批升级；C++ battle 部分当时没编译。
- 聚合加载器 `all_table` 改名 `table_loader`：客户端 `.meta` 与 `robot/vendor` 待补。
- AOI 单向关注与物品封禁：查询的两条真库用例与两处英文文案待补跑。
- AOI 仇恨名单要等刷怪接上才生效。
- 观战分级扩容：7 条真 TiDB 用例与冒烟没跑。
- 控制面按 zone 化：部署契约与 kind 冒烟从没跑过。
- 客户端请求幂等键：三条 robot 冒烟没跑。
- snowflake 槽位清理：`snowflakealloc` 测试与整解没跑。
- 混版本持久字段保护：真实混版本演练没做。
- 无感重启（scene 计划排空等）：批 1~5 编译过，单测与冒烟没跑；批 6~8 没编译。
- 审计发件箱（R13）：Kafka 演练没跑。
- 分布式日志关联：三个阶段只有第 1 批验证过。
- Agones 名额按条目：上集群前核对设计稿 §13.8 的三条假设。
- owner_epoch 铸造持久化：现有用例可能受 EnterScene 第 3e 步影响，跑红时先看这里。
- 工作负载密钥改走 Secret：验证命令在 `docs/design/workload-secrets-via-k8s-secret.md` §7。
