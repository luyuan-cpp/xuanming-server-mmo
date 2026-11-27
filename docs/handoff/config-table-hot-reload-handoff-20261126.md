# 配置表热更：验证与收尾交接（2026-11-26，交 Gemini）

## 0. 现状

- **代码五批都写完了**：11-20 落码，随 `f067d01fc` 入库。
  - 改了哪些文件看 `docs/PROGRESS.md` 里「2026-11-20 配置表热更全 5 批落码」这一条（按标题 grep）。
  - 方案以 [设计稿](../design/config-table-hot-reload.md) 为准：§16 已逐项核实，§17 是可热更表的审计台账（原 13 张，加上 11-26 复核的后来放开的 20 张）。
  - 操作步骤看 [运维手册](../ops/table-hot-reload-runbook.md)。
- **别的线顺带验证过的**（不用重做，出现回归再管）：
  - 导表器已经全量跑过多次。产物已经是 manifest 第 2 版：`schema_digest = 88750c2d961d8710`，83 张表。C++ 的 `kTableSchemaDigest` 和 Go 的 `SchemaDigest` 都是这个值。
  - `scene.exe`、`battle.exe`、`bag_test.exe`、`turn_battle_engine_test.exe` 编过，相关用例过了。
  - `bin/scene.exe --validate-tables generated/tables` 退出码 0。
  - Java 仓的 `ContractSync` 已在第 2 版清单上同步过，`--check` 通过。
  - 后来加的功能（挂机月卡、技能制书、灵石百宝阁、江湖恩怨、赛季）都把自己的表校验挂进了 `Validate` / `OnCommitted` 钩子。
- **下面这些没有任何运行记录**，PROGRESS 和 `build/verification/` 里都找不到：
  - 导表器两份新单测：`test_manifest.py`、`test_schema_proto.py`。
  - `go/shared/tablebundle`。
  - `go/table_sync` 的三套单测与两个命令。
  - gateway 的 `AdminTableBundleControllerTest`。
  - 部署脚本的四份契约测试。
  - gate、battle 与 6 个 Go 服务的 `--validate-tables`。
  - 本机热更冒烟。
  - 集群端到端。
- **C++ 单测 11-26 已由 Claude 补上，还没编译**（§3.7）：
  - 覆盖表包管理器、清单与指针解析、表达式只读求值、battle 按指纹选表与旧表保留。
  - 为了能测，表包管理器和 battle 的旧表保留期都换成了可注入的时钟，缺省仍是系统时钟。
- **11-26 晚 Claude 逐行审查五批代码，修了 7 处逻辑问题，连同配套单测都还没编译**（明细看 `docs/PROGRESS.md`「2026-11-26 配置表热更逐行审查」这一条）：
  - sidecar 与发布工具按「表名 → 条目」的对象读清单，而导表器写的是数组，真实的包一个都过不了核对；
  - C++ 节点写的 `STATUS.json` 字段名和 sidecar、管理接口读的不是一套；
  - 表包没同步下来时，重启的进程会静默退回镜像里的旧表，现在改成拒绝启动；
  - 被拒绝 / 需要重启的结论在指针撤走后不清；
  - 管理接口把上一包的拒绝算到新包头上；
  - scene 的装备表、神秘商人、游历手册三项校验没接进热更的拒绝闸门；
  - C++ 启动时不核对清单摘要与指针是否一致。
  - 两轮的代码与测试都已在 main：`8a6697af0`、`3eb57a09a`、`d77a458ee`（负责人 11-26 21:24 / 22:13 / 22:16 的批量提交）。
- **同晚第二轮审部署脚本与可观测性，又修了 8 处、加了 1 条告警**，未运行（明细看 PROGRESS「2026-11-26 Claude:配置表热更第二轮审查」，根因与改法在设计稿 §19 第 8~15 条）：
  - 两个 zone 的同名 Pod 互相覆盖 etcd 状态键：键改成 `<轨道>/<namespace>/<pod>`，sidecar 多一个 `POD_NAMESPACE` 环境变量；
  - 外接对象存储时，部署前核对凭据 Secret（`Assert-TableSyncS3SecretProvisioned`）；
  - sidecar 的指标没人抓：新增 `deploy/k8s/table-sync-podmonitor.yaml`；
  - 收敛告警把 stable 与 canary 混着比：两种语言的 `table_bundle_info` 加 `release_track` 标签；
  - 「需要重启」告警改读新的状态量 `table_bundle_needs_restart`；
  - 单包上限从 256 MiB 降到 64 MiB（与卷的 1Gi 一起算），清理时没人用的包最多留 2 个，过期的临时文件也清；
  - 解包目录显式改成 0755；
  - sidecar 只在状态变化时写 etcd 状态键，平时只续租；
  - 新告警 `TableSyncFetchFailing`。
- **缺登记**：Java 仓 `PARITY.md` 还没有本线这一行。
- **分工**（负责人 11-26 定：「逻辑代码方面你去改，Gemini 只负责编译」）：
  - 本单由 Gemini 跑生成、编译、测试、冒烟，并登记结果。
  - 跑出来要改代码的，留下第一条错误原文停下，交回 Claude 改。只有负责人明说让 Gemini 接手写代码时例外。

## 1. 规矩

- 先读：
  - `AGENTS.md`，以及 `docs/rules/` 里的 `ai-workflow.md`、`cpp-runtime-and-build.md`、`proto-and-tables.md`；
  - 设计稿全文；
  - `data/AGENTS.md`。
- **模型档位**：编译、生成、跑测试、冒烟用 `Carbon Medium`。本单不需要 Gemini 改代码。
- **工作区**：只在 main 工作区做，不开分支，不 `git push`、不 `git tag`。只有负责人要求时才 commit，而且只提交本线的改动。工作区里有大量别的线没提交的文件：不要 `git add -A`，不要 `git reset` / `git checkout --`。
- **导表**（本线最容易误伤别人的地方）：
  - **产物已经是第 2 版，本线不需要再全量导表。** 11-26 核对时，别的线还有一批没提交的 xlsx 和产物：`GuildActivity`、`GuildDonate`、`GuildLevel`、`GuildShop`、`GuildWarMilestone`、`GuildWarRule`、`RewardedAdSlot`、`TournamentRule`、`Tip`，外加两份 schema。全量导表会把它们一起带进产物和 manifest。
  - 要核对产物和模板对不对得上，用只读的 `sandbox_export.py`（§3.2）。
  - **不写客户端仓。** 默认配置会把 C# 部署到 `../mmorpg-client`（见 `exporter_config.yaml` 的 csharp.deploy）。确实要真导表时，用 `python tools/data_table_exporter/run.py <临时配置>`，临时配置放 `%TEMP%`，里面把 `languages.csharp.enabled` 设成 false。
  - **不改 manifest 的 `_content_digest` 算法。** Java 仓 `tools/ContractSync.java` 的 `manifestContentDigest` 按同一个算法重算。
  - 改了模板，产物要同批更新，而且多份副本都要更新：
    - C++：`generated/code/cpp` 与 `cpp/generated/table/code`；
    - Go：`go/shared/generated/table`；
    - Java：`generated/code/java` 与 `java/config_node/.../com/game/table`。
  - 产物改了名要删旧文件。
- **C++**：
  - 类 / 结构体不放裸指针成员；
  - 不新起线程，守卫清单不加项；
  - MSBuild 一律 `/m:1`；
  - `run_cpp_tests.ps1 -Build` 不重编库，改了库先整解；
  - 新 `.cpp` 只登记 `.vcxproj` / `.filters`，不手改 `CMakeLists.txt`。
- 日志英文；注释、文档、提交说明用中文。
- **失败时**：
  - 留下第一条错误原文（文件:行），或者失败用例的完整输出，这一步就停在这里。
  - 不自己改代码：把原文和复现命令写进 PROGRESS，交回 Claude。不注释断言、不放宽断言、不跳测试、不连续自动重试。
  - 先 `git diff` 分清出错的文件是谁在改；属于别的线的，同样只记录。
  - 和这一步无关的后续步骤可以接着跑，比如 Go 红了不妨碍跑 Java 和契约测试。结果里写明哪些是在红的基础上跑的。

## 2. 什么时候停下来问负责人

- 要往 kind 以外的集群部署。
- 要动客户端仓。
- 要全量导表，或者要改 `generated/` 下的产物。
- 交回 Claude 的问题里，有需要推翻设计稿定案的，一并报给负责人。定案是这五条：
  - 指针只按轨道分；
  - 表缺省不可热更；
  - 战斗每局绑定一套表；
  - 由 sidecar 分发；
  - S3 不用第三方库。

## 3. 按顺序做

运行名统一用 `table-hot-reload-<UTC 时间>`，日志都放 `build/verification/<运行名>/`。开工先记下 `git rev-parse HEAD` 和 `git status --short`，收尾时用来分清哪些改动是自己的。下面的命令都在仓库根目录执行，单独写了目录的除外。

### 3.1 静态守卫（几秒）

```powershell
python tools/scripts/tests/test_cpp_no_raw_pointer_member.py
python tools/scripts/tests/test_cpp_thread_sources.py
python tools/scripts/tests/test_battle_no_direct_table_singleton.py
python tools/scripts/check_log_text_english.py --root .
```

通过标准：全部 OK，0 violations。

### 3.2 导表器

```powershell
Push-Location tools/data_table_exporter; py -m pytest -q; Pop-Location
py tools/data_table_exporter/tools/sandbox_export.py --out "$env:TEMP\table-sandbox-<时间>" --compare
(Get-Content generated/tables/manifest.json -Raw | ConvertFrom-Json).schema_digest
Select-String -Path cpp/generated/table/code/table_loader.h -Pattern 'kTableSchemaDigest ='
Select-String -Path go/shared/generated/table/table_loader.go -Pattern 'const SchemaDigest'
```

通过标准：

- pytest 0 failed。
- `--compare` 里，模板生成的代码与仓内一致。数据文件和 manifest 的差异只能来自 §1 列的那些别的线的 xlsx，逐个列出来，不修。
- 三处摘要相同。

### 3.3 Go

```powershell
$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'
Push-Location go/shared; go vet ./tablebundle/...; go test ./tablebundle/... -race -count=1; Pop-Location
Push-Location go/table_sync; go build ./...; go vet ./...; go test ./... -race -count=1; Pop-Location
foreach ($m in 'match','guild','login','scene_manager','mail','rank','player_locator') {
  Push-Location "go/$m"; go build ./...; go vet ./...; go test ./... -count=1; Pop-Location
}
```

通过标准：

- 0 失败。
- `tablebundle` 的 6 条用例要全过：`TestVerifyTableDirManifest*` 三条、`TestManagerStartupFallsBackToImageAndHotReloadsScheduled`、`TestManagerNeedsRestartAndRejectedDoNotRetry`，以及 11-26 新增的 `TestStartupRefusesImageFallbackWhenOwnLineFailedToSync`（本线表包没同步下来时拒绝启动）。
- `tablebundle` 的 `TestManagerNeedsRestartAndRejectedDoNotRetry` 第二轮加了两处断言：停在 needs_restart 时 `table_bundle_needs_restart` 为 1，指针换走后为 0。
- `table_sync` 的 9 条也要全过。第二轮新增两条：`TestPruneCapsRecentUnreferencedBundlesAndRemovesOrphanedTempFiles`（没人用的包最多留 2 个、过期临时文件要清）、`cmd/table-sync` 的 `TestStatusIdentityJoinsNamespaceSoSameNamedPodsInDifferentZonesStayApart`；`TestPackVerifyExtractAndPermanentIDRollback` 加了「解包目录是 0755」的断言，Windows 上跳过，Linux 证据要在 Linux 上跑一遍。其余是 `TestPackVerifyExtractAndPermanentIDRollback`、`TestVerifyAndExtractRejectsTraversalAndSymlinks`、`TestKeysAndPruneUnusedBundles`、`TestSigV4PutHeadGetAndSizeLimit`，以及第一轮新增的三条：
  - `TestManifestJSONUsesExporterArrayShape`：清单的 `tables` 是数组，缺名、重名被拒；
  - `TestVerifyDirManifestAcceptsRealExporterOutput`：仓里 `generated/tables` 能过 sidecar 的清单核对。这一条红了说明 sidecar 读不了真实的包，线上一个包都下不来；
  - `TestDocumentRoundTripCarriesSyncErrors`：`POINTER.json` 的 `sync_errors` 读写往返。
- `-race` 要 cgo，本机没有 gcc 就去掉 `-race` 跑一遍，并写明。

### 3.4 Java gateway_node

```powershell
mvn -f java/gateway_node/pom.xml test -Dtest=AdminTableBundleControllerTest,AdminKillSwitchControllerTest,AdminApiKeyFilterTest
mvn -f java/gateway_node/pom.xml test
```

通过标准：0 failures、0 errors。`AdminTableBundleControllerTest` 现在是 5 条，11-26 新增两条：`listDoesNotBlameTheNewBundleForWhatPodsSaidAboutAnOlderOne`（实例回报还停在上一包时，不把上一包的拒绝算到新包头上）、`listCountsSameNamedPodsOfDifferentZonesSeparately`（两个 zone 的 `scene-0` 各算一个，一个的拒绝不被另一个盖掉）。

### 3.5 部署脚本契约测试

```powershell
pwsh -NoProfile -File tools/scripts/tests/k8s_table_bundle_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/loki_ha_deploy_contract.tests.ps1
```

通过标准：fail=0。后三份别的线也在改，红了先看失败的断言属于哪条线。`k8s_table_bundle_contract.tests.ps1` 第二轮改成 4 个用例：5 条告警的指标、标签、失败结果名与代码核对，PodMonitor 端口名与 sidecar 一致；sidecar 带 `POD_NAMESPACE`；凭据 Secret 核对（kubectl 用替身，不连集群）；镜像登记与 Fleet 的 container 行。

### 3.6 C++ 整解与已有用例

```powershell
msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64
pwsh -File tools/scripts/run_cpp_tests.ps1 -LogDir build/verification/<运行名>/cpp-tests
```

通过标准：

- 整解 0 error，`gate.exe`、`scene.exe`、`battle.exe` 与全部测试工程都编出来。
- 测试全量 0 failed。确认 `configuration_table_test`、`turn_battle_engine_test`、`message_limiter_test`、`bag_test`、`rpc_controller_test` 都在跑的名单里。battle 节点的用例编在 `rpc_controller_test` 里，没有单独的工程。

### 3.7 新补的 C++ 单测（代码已写，只跑）

11-26 由 Claude 写好，未编译。§3.6 的整解会把它们编进去；这里单独点名，方便核对新用例确实跑了。

```powershell
pwsh -File tools/scripts/run_cpp_tests.ps1 -Filter configuration_table -LogDir build/verification/<运行名>/cpp-tests
pwsh -File tools/scripts/run_cpp_tests.ps1 -Filter rpc_controller -LogDir build/verification/<运行名>/cpp-tests
pwsh -File tools/scripts/run_cpp_tests.ps1 -Filter bag_test -LogDir build/verification/<运行名>/cpp-tests
```

**`cpp/tests/configuration_table_test/table_bundle_manager_test.cpp`**（已登记进工程），共 14 条。除「逐表差异」和最后一条外都属于 `TableBundleManagerTest`：

| 用例 | 验什么 |
|---|---|
| `ManifestVerificationAcceptsShippedTablesAndRejectsOtherSchema` | 仓里导出的表能过清单核对；`schema_digest` 不同被拒 |
| `ManifestVerificationRejectsTamperedFileAndOldManifestVersion` | 文件被改过被拒；第 1 版清单被拒 |
| `PointerLineIsReadPerSchemaLine` | 指针按 schema 线读；没有本线返回空；JSON 写坏报错 |
| `SyncErrorIsReadPerSchemaLine`（11-26 晚新增） | `sync_errors` 按 schema 线读：有本线返回原因，别的线没同步下来不影响本线，JSON 写坏返回没有 |
| `TableBundleManifestDiffTest.ListsChangedAddedAndRemovedTablesSortedForTheActiveFormat` | 逐表差异：改、增、删都列出并排好序，json / binary 各算各的 |
| `StartupLoadsTheBundleThePointerNames`（11-26 晚加断言） | 启动时按指针加载那一包；`STATUS.json` 用的是约定的字段名（`service`、`loaded_digest`、`loaded_at_ms`、`pending_digest` 等） |
| `HotReloadableChangeIsPreparedInSlicesThenCommittedWhole` | 准备期间现役表不动；提交后各表读到新包，旧的一套还能读 |
| `StructuralTableChangeNeedsRestartAndIsNotRetried`（11-26 晚加一段） | 改到不可热更的表：needs_restart，不重试；整条线从指针文件里消失后，这个结论被清掉 |
| `ValidationRunsOnStagedTablesAndRejectionKeepsCurrentOnes`（11-26 晚加一段） | 校验钩子读到的是暂存的新包；拒绝后现役表原样；撤掉 next 后拒绝结论被清掉；同一包再发一次会重新校验（拒绝计数到 2） |
| `UnparsableTableRejectsTheBundle` | 有一张表解析失败：整包拒绝 |
| `ScheduledBundleWaitsPreparedUntilActivateAt` | 没到点只准备；差 1 毫秒不提交；到点提交 |
| `WithdrawnNextDropsThePreparedBundle` | 到点前撤掉 next：到点也不切 |
| `RetiredSetIsReleasedAfterTheRetentionWindow` | 旧表 10 分钟后管理器放手；外面还拿着的照样能读 |
| `TableExpressionEvaluateTest.ConstEvaluateTakesParamsPerCallAndLeavesTheStatefulPathAlone` | 表达式只读求值按次传参，不影响旧的两步路径 |

**`cpp/nodes/battle/tests/battle_observer_contract_test.cpp`**（编在 `rpc_controller_test` 里），新增 2 条，都属于 `BattleObserverContract`：

| 用例 | 验什么 |
|---|---|
| `TableProviderIsChosenByTheFingerprintTheRequestCarries` | 按请求或玩家快照带的指纹选那一套表；没带或找不到就用现役的 |
| `RetiredTableProviderLeavesThePoolAfterRetentionButItsRoomKeepsIt` | 旧的一套 10 分钟后移出保留表；已开的房间仍拿着它，清房才放 |

**`cpp/tests/bag_test/player_mystery_merchant_test.cpp`**，11-26 晚新增 1 条，属于 `PlayerMysteryMerchantTest`：

| 用例 | 验什么 |
|---|---|
| `TableValidationFailsOnUnsellableShelvedGoodsAndOutOfRangeDistance` | 神秘商人表校验现在有返回值：导出的表过；上架却卖不了的商品、购买距离为 0 不过；下架（weight = 0）的行不算坏行 |

通过标准：这 17 条全过，三个工程的老用例不回归。第二轮在两条已有用例里加了指标断言：`StartupLoadsTheBundleThePointerNames` 核对 `table_bundle_info{release_track="stable",` 与 `table_bundle_needs_restart 0`，`StructuralTableChangeNeedsRestartAndIsNotRetried` 核对停住时为 1、整条线消失后为 0。跑测试的环境里要是设了 `MMORPG_RELEASE_TRACK=canary`，第一处会红，这是环境问题，去掉再跑。

没覆盖的：battle 的 warn / enforce 判定模式每个进程只读一次配置，单测里切不了。

这批用例依赖两件事，红了先看是不是它们变了：

- `MessageLimiter` 在可热更名单里，`World` 不在。
- 仓里 `generated/tables` 的清单和文件对得上，而且它的 `schema_digest` 和编进 `table.lib` 的一致。别的线刚改了表结构、导了表却没重编 `table.lib` 时，夹具的 SetUp 会直接失败。

### 3.8 预检 `--validate-tables`

```powershell
foreach ($n in 'scene','gate','battle') { & "bin/$n.exe" --validate-tables generated/tables; "$n exit=$LASTEXITCODE" }
```

Go 的 6 个读表服务（match、guild、login、scene_manager、mail、rank）：先编出二进制，在各自目录下运行 `<服务>.exe -validate-tables ../../generated/tables`。

通过标准：9 个进程的退出码全是 0，输出里的 `schema_digest` 和 §3.2 的一样。

- C++ 成功打 `table validation passed`，失败在 stderr 打 `table validation failed (manifest|parse|hook)`。
- Go 成功打 `[TableBundle] validate-tables OK`。
- scene 的预检从 11-26 晚起多跑三项（装备表、神秘商人、游历手册）。仓里现有的表按单测应当全过；如果 scene 退出码非 0，把 stderr 里 `hook` 后面那句原因和它上面的 `[PlayerEquip]` / `[MysteryMerchant]` / `[TravelPass]` 报错行记下来交回 Claude，不要改表、不要把校验摘掉。

### 3.9 本机热更冒烟（不需要 etcd 和对象存储）

1. 先发一次当前表。进程开了表包模式、却读不到 `POINTER.json`，会启动失败，所以这一步要在起进程之前：

   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command tables-reload-local
   ```

2. 设 `MMORPG_TABLE_BUNDLE_ROOT=<仓库>/run/table_bundles`，起 gate、scene、battle，再加一个 Go 读表服务（guild 或 match）。`MMORPG_TABLE_STATUS_ROOT` **每个进程各用一个目录**（`<仓库>/run/table_status/<进程名>`）：几个进程共用一个目录会互相覆盖同一个 `STATUS.json`，第 4 步就看不出是谁拒绝的。
   - 核对启动日志打出了表的 `version` 与 `content_digest`。
   - 核对 `/metrics` 里 `table_bundle_info{source="bundle"}` 是 1。
   - 核对每个进程的 `STATUS.json` 都有 `service`、`loaded_digest`、`loaded_at_ms` 这几个字段，C++ 与 Go 写出来的字段名一样。
3. 做一份**只改了可热更表**的包，不碰仓库里的 xlsx：
   - 把 `generated/tables` 拷到 `%TEMP%`。
   - 改 `messagelimiter.json` 里一行的数值。节点按 `bin/etc/base_deploy_config.yaml` 的 `table_data_format` 加载，是 json 就改 json。
   - 在这个临时目录的 `manifest.json` 里，把这个文件的 `size` / `sha256` 改成现状，再把 `content_digest` 换成一个新的 64 位小写十六进制值（比如对旧值再做一次 sha256）。进程只拿它当这包表的名字，不会重算。改清单的一次性脚本也放 `%TEMP%`，不进仓库。
   - 发出去：`pwsh -File tools/scripts/dev_tools.ps1 -Command tables-reload-local -TableSource <那个临时目录>`。

   通过标准：4 秒内 C++ 进程打出 `Table hot-reload committed`、Go 服务打出 `[TableBundle] reload committed`；各进程 `STATUS.json` 的 `loaded_digest` 是新摘要、`pending_state` 为空；`table_reload_total{result="committed"}` 加 1；gate 的限流按新值生效。
4. 同样的办法再做两份包：
   - 改一张不可热更的表（例如 `world.json`）：期望打出 needs_restart，进程不切换，`STATUS.json` 里 `pending_state` 是 `needs_restart`、`pending_digest` 是这一包。
   - 把 `mysterymerchantrule.json` 里的 `interact_range` 改成 0：期望 scene 打出 `Table hot-reload rejected`，它的 `STATUS.json` 里 `pending_state` 是 `rejected`、`pending_reason` 带 `MysteryMerchantRule`，scene 不切换。gate、battle 不校验这张表，会照常切过去，这正是「到点后被拒」时部分进程新、部分进程旧的样子。
   - 验完再发一次第 3 步那份好的包：各进程回到同一版，scene 的 `STATUS.json` 里 `pending_state`、`pending_digest`、`pending_reason` 都清空。
5. 表包没到位时拒绝启动（不需要真的 sidecar）：停掉 scene 和那个 Go 服务，手工改 `run/table_bundles/POINTER.json`，把 `lines` 里本线（键是 §3.2 的 `schema_digest`）那一项删掉，并在顶层加 `"sync_errors": {"<schema_digest>": "smoke test"}`，再起这两个进程。
   - 期望都起不来，日志里有 `Startup refused: table bundle for schema line`（C++）/ `startup refused: table bundle for schema line`（Go）。
   - 把 `sync_errors` 也删掉（`lines` 里仍没有本线）再起：期望正常启动，`table_bundle_info{source="image"}` 是 1。
   - 验完重跑第 1 步的 `tables-reload-local` 恢复指针。
6. 有 robot 的话，另开一局回合制战斗，在打的过程中发一次第 3 步的包。核对这一局照常打完，结算没有指纹报错。

### 3.10 kind 上端到端（只在 dev 集群）

1. 用 `tools/scripts/go_svc_image.ps1` 打 `table-sync` 镜像，并载入 kind 节点。原生 sidecar 镜像拉不到时，Pod 会卡在 `Init:ErrImagePull`。
2. infra-up 带 `-TableSyncDevFixture`：会起 SeaweedFS，并建 `mmorpg-tables` 桶。zone-up 带同一个参数，sidecar 会挂进 gate、scene、battle（含 Agones Fleet）和 6 个 Go 读表服务。
3. 按运维手册 §1 的顺序跑：`table-bundle upload` → `verify` → `publish`（先 canary，再 stable）。

通过标准：

- `GET /admin/tables` 里全部实例都回报 loaded；
- `kubectl logs <pod> -c table-sync` 没有 403 或摘要校验失败；
- 两个摘要在各 Pod 的 `table_bundle_info` 上一致，而且都带 `release_track`（canary 的 Pod 是 `canary`）。

第二轮改动在这里一并核对（都只读，不改集群里任何东西）：

4. **状态键带 namespace**：`etcdctl get --prefix --keys-only /mmorpg/tables/status/` 列出的键形如 `/mmorpg/tables/status/stable/mmorpg-zone-1/guild-0`。起两个 zone 时，两个 zone 的同名 Pod 各有一个键。
5. **稳态不写键**：发布完成、各实例都回报 loaded 之后，`etcdctl watch --prefix /mmorpg/tables/status/` 看 1 分钟，应当没有事件（只续租）。再发一包，每个 Pod 应当有两三次写入，然后又安静下来。
6. **sidecar 指标抓得到**：装上 `deploy/k8s/table-sync-podmonitor.yaml`（运维手册 §3），在 Prometheus 里查 `table_sync_etcd_last_read_timestamp_seconds`，每个带 sidecar 的 Pod 都要有一条。kind 上没装 Prometheus Operator 就跳过并写明。
7. **凭据核对**：kind 上用的是 dev 夹具，凭据由部署脚本自己写入，这道核对不会触发。只用 `-DryRun`（不连集群、不写任何东西）带 `-TableSyncS3Endpoint https://s3.example.invalid` 跑一次 zone-up，确认输出里有 `would check that Secret table-sync-s3 in namespace … has non-empty keys access-key-id and secret-access-key`。缺 Secret 时真跑的行为由 §3.5 的契约测试覆盖，不要在集群上故意删 Secret 去试。

S3 签名是自己写的 SigV4，**第一次连真的 S3 兼容存储就在这一步**；报 403 先查签名，不要先怀疑凭据。有 promtool 的话再校验一次规则语法。promtool 只认顶层是 `groups:` 的规则文件，而这份是 PrometheusRule，要先取出它的 `spec`（例如 `yq '.spec' deploy/k8s/table-bundle-alerts.yaml > $env:TEMP\table-bundle-rules.yaml`），再 `promtool check rules $env:TEMP\table-bundle-rules.yaml`，临时文件不进仓库。本机没有 kind 就跳过这一节并写明原因，前面几节照样算数。

### 3.11 收尾

- 在 `docs/PROGRESS.md` **文末追加**一条，不改旧条目。
  - 标题：「2026-11-xx Gemini:配置表热更验证与收尾」。
  - 逐节写出命令、通过数和失败原文摘要；没跑的写原因。
  - 不贴大段原始输出，日志留在 `build/verification/<运行名>/`。
- 把设计稿第 3 行的状态改成实际的验证结果，直接改正文。
- 在 Java 仓 `PARITY.md` 登记一行待做。本仓提交写 `f067d01fc`（五批）、`2d20f6b68`（表包管理器的可注入时钟）、`8a6697af0` / `3eb57a09a` / `d77a458ee`（11-26 晚两轮审查的修复与单测）。只登记，不写 Java 代码。登记里写明 Java 版要对齐的约定：清单 `tables` 是数组；`STATUS.json` 的字段名；`POINTER.json` 的 `sync_errors` 与「表包没到位拒绝启动」；etcd 状态键是 `<轨道>/<namespace>/<pod>`；`table_bundle_info` 带 `release_track`；状态量 `table_bundle_needs_restart`。
- 改了或新增了文档，跑 `python tools/scripts/gen_docs_index.py`。
- 要提交就等负责人发话，只提交本线的文件。

## 4. 失败时先看这几处

| 现象 | 先看 |
|---|---|
| 启动直接退出 | 开了表包模式却没有 `POINTER.json`（§3.9 第 1 步）；或者本地包的 `schema_digest` 和二进制里的不一样 |
| 启动报 `Startup refused: table bundle for schema line` | sidecar 没把本线的表包同步下来。看 `POINTER.json` 的 `sync_errors` 和 sidecar 日志（对象存储不通、包不存在、摘要对不上）。这是有意的拒绝，不是代码问题；同步好之后进程自己能起来 |
| zone-up / infra-up 停在 `Secret table-sync-s3 does not exist in namespace …` | 外接对象存储时要先在该 namespace 建只读凭据 Secret（运维手册 §3）。这是有意的前置核对，不是代码问题 |
| Prometheus 里查不到 `table_sync_*` 指标 | 没装 `deploy/k8s/table-sync-podmonitor.yaml`，或 Prometheus 的 podMonitorSelector 选不到它。业务进程的指标查得到也不说明 sidecar 的抓到了：两者走的不是同一条抓取路径 |
| 发布工具刚 `publish` 就报被拒绝 | 看 `GET /admin/tables` 里被拒绝实例的 `digest` 是不是这次发的包。不是的话是管理接口的汇总出了问题，记下响应原文交回 Claude |
| 管理接口里看不到 C++ 节点、或它的 `loaded` 一直是 0 | 看那个 Pod 的 `STATUS.json` 字段名是不是 `loaded_digest` / `pending_digest` 这一套；旧二进制写的是 `content_digest` / `pending_content_digest`，要重编 |
| 热更后某个功能拒绝了整包 | 后来接进 `Validate` 钩子的校验（挂机月卡、技能制书、灵石百宝阁、江湖恩怨、赛季等）有一个返回 false，整包就拒绝，这是预期行为。看 `STATUS.json` 里的原因是哪一个钩子给的 |
| `Validate` 期间的东西泄漏到了提交之后 | 校验函数只许读，不许装东西、不许写静态变量。`InstallItemInitializer`、导航网格这类只在启动时做，在 `OnLoaded` 里，不在 `Validate` 里 |
| Go 服务跨表判定读到半新半旧 | `Commit` 是逐张换的。一次判定要用多张同一版的表时，必须先 `ts := table.Current()` 再从 `ts` 读，查调用点有没有照这个做 |
| 战斗指纹不一致 | 先看 scene 提交后有没有刷新指纹（`OnCommitted`），再看 battle 的旧 provider 是不是已经过了 10 分钟被回收 |
| 主循环卡顿 | 看 `table_reload_slice_max_seconds`，它是解析最慢的那一张表花的时间 |
