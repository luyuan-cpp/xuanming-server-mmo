# 配置表热更：验证、补测与收尾交接（2026-11-26，交 Gemini）

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
- **缺测试**：C++ 这一侧一条单测都没有。缺的是：
  - `TableBundleManager`；
  - 清单与指针解析；
  - `ExcelExpression::Evaluate`；
  - battle 按指纹选表、房间持有旧表。
- **缺登记**：Java 仓 `PARITY.md` 还没有本线这一行。
- **授权**：负责人 11-20 原话「你出交接细节 我让gemini 去做……让他做完所有」。本单列的验证、补测、修红、登记都由 Gemini 做完，不用逐项再问；§2 列的情形除外。

## 1. 规矩

- 先读：
  - `AGENTS.md`，以及 `docs/rules/` 里的 `ai-workflow.md`、`cpp-runtime-and-build.md`、`proto-and-tables.md`；
  - 设计稿全文；
  - `data/AGENTS.md`。
- **模型档位**：编译、生成、跑测试、冒烟用 `Carbon Medium`；改代码（包括补单测、修红）切到 `Gemini 4 Neon Extra`，切之前把第一条错误原文存下来。
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
  - 留下第一条错误原文（文件:行）。
  - 修根因。不注释断言、不放宽断言、不跳测试、不连续自动重试。
  - 根因在别的线正在改的文件里时，先 `git diff` 分清是谁的改动；不是本线的就只记录、不修。

## 2. 什么时候停下来问负责人

- 修红要推翻设计稿里定下的事：
  - 指针只按轨道分；
  - 表缺省不可热更；
  - 战斗每局绑定一套表；
  - 由 sidecar 分发；
  - S3 不用第三方库。
- 要新增第三方依赖。
- 要改客户端仓，或者改客户端看得到的协议。
- 要往 kind 以外的集群部署。

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
- 测试全量 0 failed。确认 `configuration_table_test`、`turn_battle_engine_test`、`message_limiter_test`、`bag_test` 和 battle 节点的测试工程都在跑的名单里。

### 3.7 补 C++ 单测（改代码，切高档模型）

以下用例都要确定、可重复：

- 不 sleep，不依赖真实时间；
- 每条用例用自己的临时目录，用完删掉；
- 时间推进用 `TableBundleManager::PollOnce(nowMs)`；
- 起 `EventLoop` 的写法参照 `cpp/tests/rpc_controller_test/rpc_channel_loopback_test.cpp`。

**`cpp/tests/configuration_table_test/table_bundle_manager_test.cpp`**（新文件，登记进同目录的 `.vcxproj` 与 `.filters`）：

1. `VerifyTableDirManifest`：
   - 完好的目录能通过；
   - 改掉某个文件的一个字节就失败，错误里带文件名；
   - `schema_digest` 不同失败；
   - `schema_version = 1` 失败。
2. `ReadPointerLine`：
   - 文件里没有这条线：返回 true，结果为空；
   - JSON 写坏了：返回 false；
   - `current`、`next`、`activate_at_ms`、`immediate` 都读对。
3. `DiffChangedTables`：只列出 sha256 变了的表，结果排好序；json、binary 两种口径各一条。
4. 管理器，只改了可热更的表，并且带 `immediate`：`PollOnce` 之后把分片跑完，现役那一套表的 `content_digest` 换成新包，`STATUS.json` 写 loaded，`table_reload_total{result="committed"}` 加 1。
5. 改到了不可热更的表：不切换，`STATUS.json` 写 needs_restart。指针不变时再 `PollOnce`，不会重试。
6. `Validate` 钩子返回 false：不切换，状态是 rejected，原因写进 `STATUS.json`。
7. `activate_at` 在将来：用早于它的时刻 `PollOnce` 只到 prepared；用晚于它的时刻再调一次才提交。
8. 退役：
   - 提交之后，外部还持有的旧 `TableSet` 照样能读；
   - 没人持有、过了 10 分钟就被回收，`table_sets_retained` 回到 0。
   - 回收现在读的是真实的 `steady_clock`，这条测不了。做法：给回收加一个能注入的单调时钟（缺省 `steady_clock::now`，用 `std::function` 成员，不用裸指针），单测里注入假时钟，不要 sleep。

**表达式**（放进同一个测试工程）：同一个 `ExcelExpression` 用 const 引用调 `Evaluate`，传不同参数得到不同结果，而且不影响旧的 `SetParam` / `Value` 那条路。

**battle**（`cpp/nodes/battle/tests/` 下新建 `battle_table_provider_pool_test.cpp`，登记进 battle 测试工程）：

1. 两套 provider 按指纹登记（`BattleRoomManager::RegisterTableProvider`）：带旧指纹开局绑到旧那套，带新指纹开局绑到新那套。
2. 对局中途提交了新表：这一局继续用开局时那一套，打完才放手。
3. 找不到指纹：按 `FingerprintMode` 走，warn 用现役那套，enforce 拒绝开局。

`ResolveProviderForRequest` 是私有的，从公开的建房入口去测，不要为了测试把它改成 public。

写完先跑 `python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`，再整解、跑这两个测试工程。通过标准：新用例全过，老用例不回归。

### 3.8 预检 `--validate-tables`

```powershell
foreach ($n in 'scene','gate','battle') { & "bin/$n.exe" --validate-tables generated/tables; "$n exit=$LASTEXITCODE" }
```

Go 的 6 个读表服务（match、guild、login、scene_manager、mail、rank）：先编出二进制，在各自目录下运行 `<服务>.exe -validate-tables ../../generated/tables`。

通过标准：9 个进程的退出码全是 0。C++ 失败时 stderr 打 `table validation failed (manifest|parse|hook)`；Go 成功时打 `[TableBundle] validate-tables OK`，带上和 §3.2 一样的 `schema_digest`。

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
   - 在 manifest 里重算这个文件的 `size` / `sha256`。
   - 用 `tools/data_table_exporter/core/manifest.py` 的 `_content_digest` 重算 `content_digest`。这个一次性脚本放 `%TEMP%`，不进仓库。
   - 发出去：`pwsh -File tools/scripts/dev_tools.ps1 -Command tables-reload-local -TableSource <那个临时目录>`。

   通过标准：4 秒内 C++ 进程打出 `Table hot-reload committed`、Go 服务打出 `[TableBundle] reload committed`；`run/table_status/STATUS.json` 显示新摘要、状态 loaded；`table_reload_total{result="committed"}` 加 1；gate 的限流按新值生效。
4. 同样的办法再做两份包：
   - 改一张不可热更的表（例如 `world.json`）：期望打出 needs_restart，进程不切换。
   - 把 `MysteryMerchantRule` 改成校验不过的值：期望 rejected，原因写进 `STATUS.json`，进程不切换。
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
  - 标题：「2026-11-xx Gemini:配置表热更验证、补测与收尾」。
  - 逐节写出命令、通过数和失败原文摘要；没跑的写原因。
  - 不贴大段原始输出，日志留在 `build/verification/<运行名>/`。
- 把设计稿第 3 行的状态改成实际的验证结果，直接改正文。
- 在 Java 仓 `PARITY.md` 登记一行待做：本仓提交 `f067d01fc`，加上这次修红的提交（如果有）。只登记，不写 Java 代码。
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
