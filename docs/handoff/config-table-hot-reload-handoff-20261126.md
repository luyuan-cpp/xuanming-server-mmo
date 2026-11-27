# 配置表热更：验证与收尾交接（2026-11-26，交 Gemini）

## 0. 现状

- **代码五批都写完了**：11-20 落码，随 `f067d01fc` 入库。
  - 改了哪些文件看 `docs/PROGRESS.md` 里「2026-11-20 配置表热更全 5 批落码」这一条（按标题 grep）。
  - 方案以 [设计稿](../design/config-table-hot-reload.md) 为准：§16 已逐项核实，§17 是 13 张可热更表的审计台账。
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
- `tablebundle` 的 5 条用例要全过：`TestVerifyTableDirManifest*` 三条、`TestManagerStartupFallsBackToImageAndHotReloadsScheduled`、`TestManagerNeedsRestartAndRejectedDoNotRetry`。
- `table_sync` 的 4 条也要全过：`TestPackVerifyExtractAndPermanentIDRollback`、`TestVerifyAndExtractRejectsTraversalAndSymlinks`、`TestKeysAndPruneUnusedBundles`、`TestSigV4PutHeadGetAndSizeLimit`。
- `-race` 要 cgo，本机没有 gcc 就去掉 `-race` 跑一遍，并写明。

### 3.4 Java gateway_node

```powershell
mvn -f java/gateway_node/pom.xml test -Dtest=AdminTableBundleControllerTest,AdminKillSwitchControllerTest,AdminApiKeyFilterTest
mvn -f java/gateway_node/pom.xml test
```

通过标准：0 failures、0 errors。

### 3.5 部署脚本契约测试

```powershell
pwsh -NoProfile -File tools/scripts/tests/k8s_table_bundle_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_client_entry_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/loki_ha_deploy_contract.tests.ps1
```

通过标准：fail=0。后三份别的线也在改，红了先看失败的断言属于哪条线。

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
```

**`cpp/tests/configuration_table_test/table_bundle_manager_test.cpp`**（新文件，已登记进工程），共 13 条。前 12 条里除第 4 条外都属于 `TableBundleManagerTest`：

| 用例 | 验什么 |
|---|---|
| `ManifestVerificationAcceptsShippedTablesAndRejectsOtherSchema` | 仓里导出的表能过清单核对；`schema_digest` 不同被拒 |
| `ManifestVerificationRejectsTamperedFileAndOldManifestVersion` | 文件被改过被拒；第 1 版清单被拒 |
| `PointerLineIsReadPerSchemaLine` | 指针按 schema 线读；没有本线返回空；JSON 写坏报错 |
| `TableBundleManifestDiffTest.ListsChangedAddedAndRemovedTablesSortedForTheActiveFormat` | 逐表差异：改、增、删都列出并排好序，json / binary 各算各的 |
| `StartupLoadsTheBundleThePointerNames` | 启动时按指针加载那一包 |
| `HotReloadableChangeIsPreparedInSlicesThenCommittedWhole` | 准备期间现役表不动；提交后各表读到新包，旧的一套还能读 |
| `StructuralTableChangeNeedsRestartAndIsNotRetried` | 改到不可热更的表：needs_restart，不重试 |
| `ValidationRunsOnStagedTablesAndRejectionKeepsCurrentOnes` | 校验钩子读到的是暂存的新包；拒绝后现役表原样 |
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

通过标准：这 15 条全过，两个工程的老用例不回归。

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

### 3.9 本机热更冒烟（不需要 etcd 和对象存储）

1. 先发一次当前表。进程开了表包模式、却读不到 `POINTER.json`，会启动失败，所以这一步要在起进程之前：

   ```powershell
   pwsh -File tools/scripts/dev_tools.ps1 -Command tables-reload-local
   ```

2. 设 `MMORPG_TABLE_BUNDLE_ROOT=<仓库>/run/table_bundles` 和 `MMORPG_TABLE_STATUS_ROOT=<仓库>/run/table_status`，起 gate、scene、battle，再加一个 Go 读表服务（guild 或 match）。
   - 核对启动日志打出了表的 `version` 与 `content_digest`。
   - 核对 `/metrics` 里 `table_bundle_info{source="bundle"}` 是 1。
3. 做一份**只改了可热更表**的包，不碰仓库里的 xlsx：
   - 把 `generated/tables` 拷到 `%TEMP%`。
   - 改 `messagelimiter.json` 里一行的数值。节点按 `bin/etc/base_deploy_config.yaml` 的 `table_data_format` 加载，是 json 就改 json。
   - 在这个临时目录的 `manifest.json` 里，把这个文件的 `size` / `sha256` 改成现状，再把 `content_digest` 换成一个新的 64 位小写十六进制值（比如对旧值再做一次 sha256）。进程只拿它当这包表的名字，不会重算。改清单的一次性脚本也放 `%TEMP%`，不进仓库。
   - 发出去：`pwsh -File tools/scripts/dev_tools.ps1 -Command tables-reload-local -TableSource <那个临时目录>`。

   通过标准：4 秒内 C++ 进程打出 `Table hot-reload committed`、Go 服务打出 `[TableBundle] reload committed`；`run/table_status/STATUS.json` 显示新摘要、状态 loaded；`table_reload_total{result="committed"}` 加 1；gate 的限流按新值生效。
4. 同样的办法再做两份包：
   - 改一张不可热更的表（例如 `world.json`）：期望打出 needs_restart，进程不切换。
   - 把 `MysteryMerchantRule` 改成校验不过的值：期望 scene 打出 rejected，原因写进它的 `STATUS.json`，scene 不切换。gate、battle 不校验这张表，会照常切过去，这正是「到点后被拒」时部分进程新、部分进程旧的样子。验完再发一次好的包，让各进程回到同一版。
5. 有 robot 的话，另开一局回合制战斗，在打的过程中发一次第 3 步的包。核对这一局照常打完，结算没有指纹报错。

### 3.10 kind 上端到端（只在 dev 集群）

1. 用 `tools/scripts/go_svc_image.ps1` 打 `table-sync` 镜像，并载入 kind 节点。原生 sidecar 镜像拉不到时，Pod 会卡在 `Init:ErrImagePull`。
2. infra-up 带 `-TableSyncDevFixture`：会起 SeaweedFS，并建 `mmorpg-tables` 桶。zone-up 带同一个参数，sidecar 会挂进 gate、scene、battle（含 Agones Fleet）和 6 个 Go 读表服务。
3. 按运维手册 §1 的顺序跑：`table-bundle upload` → `verify` → `publish`（先 canary，再 stable）。

通过标准：

- `GET /admin/tables` 里全部实例都回报 loaded；
- `kubectl logs <pod> -c table-sync` 没有 403 或摘要校验失败；
- 两个摘要在各 Pod 的 `table_bundle_info` 上一致。

S3 签名是自己写的 SigV4，**第一次连真的 S3 兼容存储就在这一步**；报 403 先查签名，不要先怀疑凭据。有 promtool 的话，再加跑 `promtool check rules deploy/k8s/table-bundle-alerts.yaml`。本机没有 kind 就跳过这一节并写明原因，前面几节照样算数。

### 3.11 收尾

- 在 `docs/PROGRESS.md` **文末追加**一条，不改旧条目。
  - 标题：「2026-11-xx Gemini:配置表热更验证与收尾」。
  - 逐节写出命令、通过数和失败原文摘要；没跑的写原因。
  - 不贴大段原始输出，日志留在 `build/verification/<运行名>/`。
- 把设计稿第 3 行的状态改成实际的验证结果，直接改正文。
- 在 Java 仓 `PARITY.md` 登记一行待做。本仓提交写 `f067d01fc`（五批）和 `2d20f6b68`（表包管理器的可注入时钟），外加 §3.7 那批单测入库后的提交。只登记，不写 Java 代码。
- 改了或新增了文档，跑 `python tools/scripts/gen_docs_index.py`。
- 要提交就等负责人发话，只提交本线的文件。

## 4. 失败时先看这几处

| 现象 | 先看 |
|---|---|
| 启动直接退出 | 开了表包模式却没有 `POINTER.json`（§3.9 第 1 步）；或者本地包的 `schema_digest` 和二进制里的不一样 |
| 热更后某个功能拒绝了整包 | 后来接进 `Validate` 钩子的校验（挂机月卡、技能制书、灵石百宝阁、江湖恩怨、赛季等）有一个返回 false，整包就拒绝，这是预期行为。看 `STATUS.json` 里的原因是哪一个钩子给的 |
| `Validate` 期间的东西泄漏到了提交之后 | 校验函数只许读，不许装东西、不许写静态变量。`InstallItemInitializer`、导航网格这类只在启动时做，在 `OnLoaded` 里，不在 `Validate` 里 |
| Go 服务跨表判定读到半新半旧 | `Commit` 是逐张换的。一次判定要用多张同一版的表时，必须先 `ts := table.Current()` 再从 `ts` 读，查调用点有没有照这个做 |
| 战斗指纹不一致 | 先看 scene 提交后有没有刷新指纹（`OnCommitted`），再看 battle 的旧 provider 是不是已经过了 10 分钟被回收 |
| 主循环卡顿 | 看 `table_reload_slice_max_seconds`，它是解析最慢的那一张表花的时间 |
