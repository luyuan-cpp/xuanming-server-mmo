# 数据一致性复查线:交 Gemini 验证(2026-11-26)

> 会话「服务器数据一致性问题」(2026-11-05 起)的收尾交接。**代码全部写完,没有待写的**;本线没有协议改动,不用跑生成。剩下的只有运行验证,按下面的顺序做。
> 逐条问题、处置与已有证据:[数据一致性保障地图](../design/data-consistency-guarantee-map-20261020.md) §6。搬库的操作与演练:[搬库手册](../ops/db-move-runbook.md) §10。

## 1. 现状

| 改动 | 落码 | 运行证据 |
|---|---|---|
| 流水随存盘提交、提交水位、人工终结统一判据、路由回滚看交接资格原件、本地换图旁路 | 11-05~06 | 11-06~08 已跑通,见保障地图 §6.4 |
| 身份不确认期间每 5 秒补存的单测 | 11-10 | 没跑 |
| 持有者存活租约(只和 etcd 失联的节点不被接管) | 11-12 | 没跑 |
| 人民币寄售:转人工后不能撤、同账号付款走退款 | 11-12 | 11-21 Gemini 全包跑过 `go/trade` 的 commerce(0 FAIL)与 `mall_game`(132/132);带 TiDB 的那几条当时是 SKIP |
| 搬库:cleanup 核复制流已撤、冻结窗口再比一次、校验和比表定义 | 11-12 | 没跑 |
| 搬库:cutover 核 TTL 已关、cleanup 先核 TiCDC 健康 | 11-19 | 没跑 |
| 搬库:TTL 任务还在跑也拒绝;audit 比对之前先核源库的 TTL;cleanup 对 TiCDC 的「没有」隔 3 秒确认两轮 | 11-26 | 没跑 |

提交状态(2026-11-26 晚核对):`2d20f6b68` 只带进了 `tools/dbmove` 11-26 改动的一部分(`mover.go`、`side.go` 的新接口方法),其余(`mover.go`、`main.go`、`replication.go` 与三个测试文件)已随 `00f92d49f` 入库;现在 `tools/dbmove` 在工作区没有未提交改动,HEAD 上是完整的一套。

## 2. 要跑的(按顺序;失败保留第一条错误原文,不改断言,不连续重试)

Go(各模块之间可并行):

1. `cd go/shared && gofmt -l ownerepoch dbplacement && go vet ./ownerepoch/... ./dbplacement/ && go test -count=1 ./ownerepoch/... ./dbplacement/`
2. `cd go/scene_manager && gofmt -l internal/logic internal/metrics internal/constants && go build ./... && go vet ./internal/logic/... ./internal/metrics/...`
3. `cd go/scene_manager && go test -count=1 ./internal/logic/ -run 'HolderLease|TestEnterScene_DeadOwner|TestEnterScene_ReplacedOwner|TestEnterScene_StaleZoneLocation|TestLeaveScene_OwnerDead|TestPlayerDomainSingleKeysShareThePlayerSlot' -v`:`holder_lease_test.go` 的 9 条全过;再 `go test -count=1 ./internal/logic/...` 全量不回归。
4. `cd go/trade && go test -count=1 -run 'TestRmbCancel' ./internal/commerce/`;真库(一次性 TiDB,`COMMERCE_TEST_DSN=<dsn>`、`COMMERCE_REQUIRE_TIDB=1`):`go test -count=1 -v -run 'TestTiDBRmb' ./internal/commerce/`,`TestTiDBRmbCancel*` 3 条与 `TestTiDBRmbDeliverRefundsSelfPurchaseInsteadOfStalling` 要 PASS,不能 SKIP。
5. `cd tools/dbmove && gofmt -l . && go build ./... && go vet ./... && go test -count=1 ./...`。点名核:`TestCutoverRefusesBeforeFreezing`(含 TTL 五条子用例)、`TestTicdcStillExists`(「没有」确认两轮)、`TestAuditRefusesWhileTheFrozenSourceIsStillChanged`、`TestCleanup*`。
6. 搬库真库用例,TiDB 与 MySQL 两档各一遍(两个实例,口径见手册 §10):`DBMOVE_IT_REQUIRE=1`,`go test -count=1 -run '^TestIT' -v ./...`,SKIP 算失败。`TestITTTLAttributeOnRealDatabases` 要写 `mysql.tidb_ttl_table_status`,**测试账号用一次性实例的 root**。

C++(MSBuild 串行 `/m:1`;只改库不重编会跑旧代码):

7. `msbuild cpp/libs/services/scene/scene.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64` → `cpp/nodes/scene/scene.vcxproj` → `cpp/tests/cross_zone_test/cross_zone_test.vcxproj` → `cpp/tests/currency_test/currency_test.vcxproj`。
8. `build\cpp\tests\cross_zone_test.exe --gtest_filter=PlayerStoreDirectEcs.*:ExitReleaseEcs.*`:11-10 的 3 条补存用例(`UnconfirmedIdentitySavesEachPlayerOnceEveryFiveSeconds` 等)与 11-12 的 3 条续租用例(`UnconfirmedIdentityRenewsTheHolderLeaseEveryThreeSeconds` 等)在列并通过;`build\cpp\tests\currency_test.exe --gtest_filter=CurrencyTest.PlayerDomainRedisKeysMatchGoContract`;再各跑全量。

其它:

9. 真 Redis,7.2 与 8.x 各一轮(隔离容器):`SET player:{1}:holder_lease x PX 15000` 之后 `PTTL` 在 (0, 15000] 内、`GET` 返回原值。
10. 有 promtool 时 `promtool check rules deploy/k8s/scene-manager-alerts.yaml`(先取出 PrometheusRule 的 spec);没有就报告未校验。
11. 真实 TiCDC:`curl -i <cdc>/api/v2/health` 健康时 200;`curl -i <cdc>/api/v2/changefeeds/no-such-feed` 是 400,响应体含 `CDC:ErrChangeFeedNotExists`;`curl <cdc>/api/v2/changefeeds/<真实 id>` 的顶层字段有 `state`、`checkpoint_ts`、`start_ts`、`create_time`。不符就把回答原样贴回,由 Claude 改 `tools/dbmove/replication.go`(Gemini 不改,见 §4)。
12. 演练:手册 §10 的表(含末尾两行:带 TTL 的表、TiCDC 升级中 cleanup);etcd 单边失联演练的口径在 `docs/ops/cross-zone-failure-test-runbook.md` 2026-11-12 的 Changelog。

## 3. 已经定案的,不要当成缺口再提

- 失去 etcd 租约的节点只重新注册,不冻结、不疏散(2026-09-08)。存活租约是在这个前提下收口的。
- 节点同时和 etcd、玩家域 Redis 失联时退回「只丢不盖,最多丢 5 秒」;进程被整体暂停等同崩溃。
- 搬库关的是表属性 `TTL_ENABLE`,不是全局开关;`cleanup` 核复制流没有跳过开关。
- MySQL 的定时事件工具不核:本仓的建表迁移不建 event,手册 §2 第 7 条列为前置条件。

## 4. 验证红了怎么办

保留第一条错误原文和失败用例单跑的完整输出,交回 Claude 的「服务器数据一致性问题」会话。本线的逻辑代码只由 Claude 改,Gemini 不改。Java 版(独立仓)只在 `PARITY.md` 登记了「资产流水跟着存盘提交走」待做,其余核对过是不适用或已有等价做法。
