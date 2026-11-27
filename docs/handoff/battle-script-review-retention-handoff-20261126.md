# 剧本审核流水保留期清理与 match 保留期告警:全部验证交接(2026-11-26,交 Gemini)

## 0. 现状

- 本线做了两件事,**代码、测试、文档都已写完,Gemini 只负责生成、编译、测试**:
  1. **剧本审核流水 `match_battle_script_review` 保留 180 天后删**。
     - 配置 `BattleScript.ReviewRetentionDays`:缺省 180,0 = 缺省,非 0 越出 [30, 365] 拒启。
     - 索引 `(reviewed_ms, script_id, revision)`,缺了 match 拒绝启动。
     - 按 `(reviewed_ms, 主键)` 游标分批删,后台循环 `match.battle_script_review.purge`。
  2. **match 的两个保留期清理(剧本审核流水、观战意图)都补了失败计数与逾期告警**。
     - 一轮连续清页,至多 20 页或 10 秒。
     - 新告警组 `match.retention`:`MatchRetentionPurgeFailing`、`MatchRetentionBehind`。
     - 搬库冻结窗口里被写者闸门拒绝的轮次不计失败。
- 设计文档:
  - [battle-script.md](../design/battle-script.md) §6.4、§9;
  - [battle-spectate-tiered-fanout.md](../design/battle-spectate-tiered-fanout.md)「意图保留期」;
  - [account-deletion.md](../design/account-deletion.md) §6.2 match 行。
- 代码在哪:
  - **11-20 的两轮改动在 HEAD**,由批量提交 `b6a08148c` 收入。之后别的功能的提交改过其中 6 个文件,11-26 已核对没碰到本线部分。
  - **11-26 的一轮改动也已入库**(Claude 用 Fable 5.2 改的,随批量提交 `a1285cb8a` 进了 HEAD;同日晚逐个文件核对过 `git log -1` 与 `git status`,工作区没有本线的未提交改动),文件见 §2 带「11-26」标记的几项。内容:
    - 搬库冻结不算清理失败。两个清理循环一轮的结局改由共用的 `observeRetentionRound` 记(`go/match/internal/logic/retention_purge.go`)。
    - `schema.go` 一行 gofmt 空白修正。别的功能插进的 `MatchTrialChallengeCardRecord` 一行打乱了注释对齐,`git diff -w` 为空。
- **一条都没验证过**,没有生成、编译或测试记录。Claude 只做过静态核对:
  - `gofmt -l` 干净;
  - 两份 ps1 用 PowerShell 解析器解析,0 错误;
  - 契约用例的正则对着实际文件逐条匹配过;
  - 整仓 `python tools/scripts/check_log_text_english.py` 通过。
- 11-26 还核对过:
  - 没有别的代码读这张表;
  - `proto/persist_schema_gen.txt` 只管玩家存档,不含 match 的表;
  - match 当前的后台循环点位与契约登记的 15 个一致。
- 不用再拍板。保留期 180 天、上下限 [30, 365]、不按副本分片、不用 TiDB 表 TTL 都已定案,理由在 battle-script.md §6.4。
- PROGRESS 按标题开头 grep:
  - 「Claude:剧本审核流水保留期清理」(11-20);
  - 「Claude:补记 11-20 第二轮」(11-26);
  - 「Claude:剧本审核流水保留期清理复核」(11-26);
  - 「Claude:保留期清理搬库冻结不计失败」(11-26)。
- 旧条目里的用例名和日志文案有的已经改了,**命令以本单 §3 为准**。
- [账号注销交接单](account-deletion-handoff-20261120.md) 也列了本线。与它同批跑时,生成与清库合并成一次,本线的测试按本单做。

## 1. 规矩

- 与 [组队跟随交接单](team-follow-afk-hosting-handoff-20261120.md) §1 相同:
  - 读 `AGENTS.md` 全文;
  - 只在 main 上做,不开分支,不 push,不打 tag;
  - **只有负责人明确要求才 commit**,只提交本线的文件,不要 `git add -A`;
  - 不手改生成产物;
  - 日志文案用英文,文档与注释用中文。
- 分工(负责人 11-26 定):**逻辑代码由 Claude 改,Gemini 只负责生成、编译、测试、冒烟**,用低档模型 `Carbon Medium` 跑。
- 失败时:
  - 保留第一条失败原文(编译错误写到 文件:行),停下,交回 Claude 改;
  - 不注释断言、不跳测试、不放宽阈值、不连续自动重试;
  - 失败落在 §2 以外的文件,是别的线的:用 `git log` / `git diff` 分清,只记录;
  - go/match 因此整体编不过时,报告负责人。
- 结果:
  - 在 `docs/PROGRESS.md` 文末追加「Gemini:剧本审核流水保留期清理验证」,写清命令、通过数和失败原文摘要;
  - 日志放 `build/verification/<运行名>/`。
- 游戏未上线,match 库可以清空重建。
- Java 不写代码,Java 仓 `PARITY.md` 已登记待做。

## 2. 本线的文件

全部在 HEAD:带「11-26」的随 `a1285cb8a` 入库,其余随 `b6a08148c`。`go/match` 工作区里现有的未提交改动(`spectate*.go`、`config.go`、`servicecontext.go` 等)是别的线的,不属本单。

- 协议:`proto/match/match_authority.proto`(`MatchBattleScriptReviewRecord` 的注释与 `OptionIndex`)。
- `go/match/internal/authority/`:
  - `battle_script.go`、`battle_script_test.go`;
  - `schema.go`(审核流水索引那几行;11-26 另有一行 gofmt 空白修正)、`schema_test.go`;
  - `role_fence_purge.go`(只改注释)。
- `go/match/internal/config/`:`config.go`(`BattleScriptConf` 与 `Validate` 里的一段)、`battle_script_conf_test.go`。
- `go/match/internal/logic/`:
  - `retention_purge.go`(11-26)、`retention_purge_test.go`(11-26,新文件);
  - `battle_script_review_purge.go`(11-26)、`battle_script_review_purge_test.go`(11-26);
  - `global_spectator.go`(11-26;`globalSpectatorIntentPurge.round` 与两处常量注释);
  - `spectate_tier_test.go`(只有 `fakeIntentPurger` 那一段);
  - `matcher.go`(装配一行)。
- `go/match/internal/metrics/`:`battle_script.go`、`retention.go`(11-26)、`metrics.go`(登记一行)。
- 部署与契约:
  - `deploy/k8s/match-alerts.yaml`(11-26;`match.retention` 组与文件头);
  - `tools/scripts/tests/loop_liveness_alerts_contract.tests.ps1`(11-26);
  - `tools/scripts/tests/k8s_deploy_contract.tests.ps1`(只有 match-alerts 规则数那一行)。
- 文档(11-26 都有改动):
  - `docs/design/battle-script.md`、`battle-spectate-tiered-fanout.md`;
  - `docs/design/account-deletion.md`、`assetop-redelivery-sharding-20261110.md`(这两份只在 HEAD);
  - `deploy/k8s/README.md`(后台循环告警一节的一条);
  - PROGRESS 四条、本单、账号注销交接单里指向本单的两处。

## 3. 按这个顺序做完

1. **生成**(仓库根):
   ```
   pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
   ```
   - 本线不改客户端可见协议。同批有别的线要写客户端仓时,按那边的要求改用默认配置。
   - 期望:`go/proto/match/match_authority.pb.go` 里能 grep 到 `reviewed_ms,script_id,revision`。
2. **本地 match 库**,两种做法选一种:
   - 清库重建后跑一次迁移:
     - 本机 MySQL 档:`pwsh -File tools/scripts/go_services.ps1 -Command migrate -LocalMySQL -Services match`;
     - TiDB:用迁移账号在 `go/match` 下显式跑 `match -f etc/match_service.yaml -migrate`。
   - 不清库,手工补索引:
     ```sql
     ALTER TABLE match_battle_script_review ADD INDEX idx_match_battle_script_review_0 (reviewed_ms, script_id, revision);
     ```
   - 不做这一步,match 启动会被 `CheckCandidateIndexes` 拒绝,报错带 `match_battle_script_review`。这是预期行为。
3. **静态检查**(仓库根):
   - `gofmt -l` 对 §2 的全部 Go 文件,期望无输出;
   - `python tools/scripts/check_log_text_english.py`,期望干净。
4. **编译**:在 `go/match` 下跑 `go build ./... && go vet ./...`。
5. **纯单测**(不连库,在 `go/match` 下):
   ```
   go test ./internal/config/ -count=1 -v -run 'TestBattleScriptReviewRetention|TestMatchServiceYamlLoads|TestSpectateValidate|TestAuthorityConfigIsRequiredInEveryMode'
   go test ./internal/logic/ -count=1 -v -run 'TestBattleScriptReviewPurge|TestDrainRetentionPages|TestObserveRetentionRound|TestGlobalSpectatorIntentPurge|TestSpectateMaintenanceLoopPrunesIndexes'
   go test ./internal/authority/ -count=1 -v -run 'TestBattleScriptReviewPurgeRejectsNonPositiveRetention'
   go test ./internal/metrics/ -count=1
   ```
   输出里下面 9 个名字都要出现 `PASS`。少一个,就是名字又改了或用例没跑到:
   - `TestBattleScriptReviewRetention`
   - `TestBattleScriptReviewPurgeDrainsPagesAndKeepsCursorOnFailure`
   - `TestBattleScriptReviewPurgeCountsFailedOldestRead`
   - `TestBattleScriptReviewPurgeDoesNotCountFrozenWriter`
   - `TestDrainRetentionPagesStopsAtPageCapEndOrError`
   - `TestObserveRetentionRoundDoesNotCountFrozenWriter`
   - `TestGlobalSpectatorIntentPurgeDrainsPagesAndKeepsCursorOnFailure`
   - `TestSpectateMaintenanceLoopPrunesIndexes`
   - `TestBattleScriptReviewPurgeRejectsNonPositiveRetention`
6. **真 TiDB**:
   - 自己起一个一次性 TiDB,不要借别的会话的容器。版本与 `deploy/docker-compose.tidb.yml` 一致:
     ```
     docker run -d --rm --name match-it-tidb -p 127.0.0.1:14000:4000 pingcap/tidb:v8.5.5
     ```
   - 在同一个 PowerShell 进程里设环境变量,再跑测试:
     ```powershell
     . tools/scripts/sql_fixture_env.ps1 -Backend tidb -Endpoint 127.0.0.1:14000 -ReceiptDir <固定盘上的绝对路径>
     $env:MATCH_IT_TIDB = '127.0.0.1:14000'; $env:MATCH_IT_REQUIRE_TIDB = '1'
     cd go/match
     go test ./internal/authority/ -count=1 -v -run 'TestBattleScript|TestTiDBCandidateIndexes|TestTiDBAuthorityStartupUsesDMLAccountAndRequiresCurrentSchema|TestSpectatorTiDBIntentRetention|TestPurgeTiDB'
     ```
   - 期望 0 SKIP。本线新增的三条:
     - `TestBattleScriptReviewRetentionPurgesOnlyExpiredRows`:到期删、未到期与刚审的留、剧本行不动、两个副本游标交错、整轮重跑幂等、最老一行前移;
     - `TestBattleScriptReviewRetentionPagesByCursor`:同一时刻多行按主键翻页、不重不漏、扫完回到开头、空表回库当前时刻;
     - `TestTiDBCandidateIndexesRequireBattleScriptReviewPurgeIndex`:删掉索引后拒绝启动。
   - 同表、必须保持绿的旧用例:
     - `TestBattleScriptSubmitRevisionAndLimit`、`TestBattleScriptReviewLoadoutAndDelete`;
     - `TestPurgeTiDBRemovedRoleDeletesPersonalRowsAndReleasesReports`(删角后审核流水仍在);
     - `TestSpectatorTiDBIntentRetention*` 两条。
7. **回归**:同一进程里(带着第 6 步的环境变量),在 `go/match` 下跑 `go test ./... -count=1`。
8. **契约**(仓库根):
   - `pwsh -NoProfile -File tools/scripts/tests/loop_liveness_alerts_contract.tests.ps1`:期望 `fail=0`,其中包括「match 后台循环一族…」与「match 保留期清理…」两条。
   - `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`:「match-alerts.yaml:…」一条必须 PASS(规则 7 条)。别的用例红了,先按归属分清。
9. **告警语法**(仓库根,PowerShell)。先把 PrometheusRule 的 `spec` 取出来,作为普通规则文件,再用 promtool 检查:
   ```powershell
   $t = (Get-Content deploy/k8s/match-alerts.yaml -Raw) -replace "`r`n", "`n"
   $spec = $t.Substring($t.IndexOf("`nspec:`n") + 7)
   $rules = ($spec -split "`n" | ForEach-Object { if ($_.StartsWith('  ')) { $_.Substring(2) } else { $_ } }) -join "`n"
   $dir = Join-Path $env:TEMP 'match-alerts-promtool'; New-Item -ItemType Directory -Force $dir | Out-Null
   Set-Content -LiteralPath (Join-Path $dir 'rules.yaml') -Value $rules -NoNewline
   docker run --rm --network none -v "${dir}:/rules:ro" --entrypoint /bin/promtool prom/prometheus:v3.5.0 check rules /rules/rules.yaml
   ```
   - 期望 `SUCCESS`,规则数 7。
   - 结果里记下实际用的镜像 tag。这个 tag 拉不到就换一个固定版本,不要用 `latest`。
10. **冒烟**(可选,调试栈):
    - 起 match 后一分钟内,指标端口 9170 上应出现:
      - `match_retention_max_age_seconds{purge="battle_script_review"} 15552000`;
      - `match_retention_oldest_unixtime{purge="battle_script_review"}`(空表时约等于当前时刻)。
    - 停掉库几分钟:`match_retention_purge_failures_total` 两个 `purge` 都在涨,日志里有 `[retention] battle_script_review purge failed` 与 `[retention] spectator_intent purge failed`。
    - 库恢复后计数不再涨。
    - 搬库冻结的那一支(日志 `purge paused: writer gate frozen`、计数不涨)要真做一次 `cutover` 才看得到,不在这次冒烟里;由第 5 步的两条冻结用例覆盖。
11. **收尾**:按 §1 在 PROGRESS 文末追加结果。

## 4. 红了先看这里

- **match 起不来,报 `match_battle_script_review` 缺候选索引**:第 2 步没做。
- **第 5 步某个名字没出现**:以本单的名字为准。PROGRESS 旧条目里的旧名不要再用。
- **logic 用例里的指标断言**:这些指标是进程级全局量,用例先读基线再断言增量。不要在用例里重置全局指标,也不要给这几条加 `t.Parallel`。
- **`TestBattleScriptReviewRetention` 加载失败**:它读仓内 `etc/match_service.yaml`。yaml 本身有问题时 `TestMatchServiceYamlLoads` 会先红,那是 yaml 的问题,不是本线。
- **契约用例按代码原文匹配**,例如:
  - `observeRetentionRound\(metrics\.RetentionPurgeBattleScriptReview, `(两个清理各一句);
  - `case authoritysql\.IsWriterFrozen\(err\):` 之后的 `default:` 里才是 `metrics.ObserveRetentionPurgeFailure(purge)`,而且整个 logic 里只许出现这一处;
  - `metrics\.SetRetentionOldest\(metrics\.RetentionPurgeBattleScriptReview, oldestMs\)`;
  - `battleScriptReviewPurgeInterval\s*=\s*time\.Minute`、`retentionPurgeRoundBudget\s*=\s*10 \* time\.Second`。
- **点位名单**:loop_liveness 契约写死了 15 个后台循环点位。别的线新加 match 后台循环会让它红,那是新增方没登记,不是本线。
- **索引用例红**:`TestTiDBCandidateIndexesRequireBattleScriptReviewPurgeIndex` 用安装账号(`testInstall`)删索引,与已有的观战意图同款用例同一做法(11-21 两条一起改成这样)。两条一起红,是夹具的问题。

## 5. 已知风险,不在本交接范围

- 告警只做语法检查,没在有数据的 Prometheus 上求过值。
- 失败门槛 30 绑定一分钟一轮(契约钉住)。改清理间隔时,门槛与契约要同步改。
- 观战意图清理现在每分钟每副本最多读 20 页,这是有意增加的读量。
- Java 版不写代码。
