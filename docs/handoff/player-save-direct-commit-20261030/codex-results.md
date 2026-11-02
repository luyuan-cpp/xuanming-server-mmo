# 第二轮验证回执

本轮验证及收尾均已结束。仅使用本轮r2证据，不沿用第一轮结果。落回执前快照UTC `2026-11-02T08:25:39.7237817Z`，HEAD `179fca76a387e8bafa6b5c9a9090351c209e68ac`，分支`main`，status=4项；元数据见 [99-source-before-receipt.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/99-source-before-receipt.json)。初次失败、修后新结果及没跑项目分别保留。

执行根为`D:/luyuan/xuanming-server-mmo-cpp`；日志根为`D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/`。本任务未新建worktree、未切分支，未在主仓执行git commit、stash、reset或checkout。期间的外部提交与本任务作者边界分开记录。表中相对日志名与该绝对日志根拼接即完整路径；每个ID的完整cwd/exe/args/exit/起止时间/绝对日志均在 [commands.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/commands.jsonl)，长锚定正则和同模板命令以ID精确索引，不截断原始参数。

开跑HEAD=`2b8184428a43b4b543ac2bf0d346cb9307b55a81`；开跑状态668项（429修改、188删除、4子模块dirty、47未跟踪），见 [00-status-before.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/00-status-before.txt)。最新落回执前源码状态以上述99元数据为准。验证期间源文件实际发生外部变化，不归因其作者；各次构建/生成必须按时点看，不能将不同源码时点拼成固定源码下的全绿。没有开跑源树内容hash/diff，不能精确归因全部既有dirty与自动生成增量。

用户后来追加授权允许修纯编译/构建问题，业务逻辑问题先交Claude。追加阶段人工只改3文件：Directory.Build.targets的两处Regex外加MSBuild.Unescape并补1条中文注释；router测试3处退役符号换为同类现存内部RPC夹具；authority_jdbc POM投影正式proto输入并保留proto/规范名。业务逻辑、断言语义、正式协议契约没有修改。其他工作区变化不归因于本次人工修复或生成器。三文件diff及时间/hash：[build-fix-manual-three-files.diff](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/build-fix-manual-three-files.diff)、[build-fix-head-snapshot-3files.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/build-fix-head-snapshot-3files.json)。即使外部提交已经收走这三文件的差异，这三处追加修复仍由本任务root/子代理编写；当前git差异数不等同作者改动范围。修后使用新命令ID，原失败记录保留，不改写成最初通过。

版本：Go `go1.27.2 windows/amd64`，MSBuild `18.10.1.42706`，PowerShell `7.6.5`；SQL源MySQL `26.7.0`、dbmove独立目标MySQL `8.4.13`，TiDB `v8.5.2`；真实Redis两档 `7.2.16`/`8.10.2`，§12集群 `8.10.2`。来源为 [Go](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/00-go-version.log) / [MSBuild](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/00-msbuild-version.log) / [pwsh](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/00-pwsh-version.log) / [MySQL源](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-real-mysql-source-ready.log) / [目标](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-real-mysql-target-ready.log) / [TiDB](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/04-tidb-startup-log.log) / [Redis7.2](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/04-redis72-info.log) / [Redis8](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/04-redis8-info.log)。

所有实际SKIP按没跑；包级no-test-files、编译/调度wrapper退出0不当作业务用例通过。普通“只跑”阶段不重试失败；追加授权后仅修改过的纯构建问题以新ID验证，未改业务失败不重跑。所有原始失败名/断言、跳过与未RUN明细保留在各节和外部原件；没有性能结论。

## 1 协议生成:通过

本节两条命令均exit0，使用`proto_gen.no-unity.yaml`，未操作独立客户端。生成后只读验收：C++ protobuf宏为7035001；两份建表清单SHA256均为`5B80A29CEB5018B8F7B2FC810BCE8DA27DD7D8BA7E08F54024A523749B554702`，含player_store、不含player_database；player_store/role_fence绑定存在；指定旧产物与工程引用不存在；6条退役台账存在；指定旧消息/RPC与RemapHomeZoneForMerge无命中；login字段3/4 reserved。此次01生成无清退动作，不能宣称本节删过27文件/15条目。证据：[01-static-checks.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/01-static-checks.txt)。

| ID | 退出码 | 工作目录（相对执行根） | 已执行命令模板 | 日志文件（相对日志根） |
|---|---:|---|---|---|
| `01-proto-build` | 0 | `.` | `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-build` | `01-proto-build.log` |
| `01-proto-run` | 0 | `.` | `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml` | `01-proto-run.log` |

## 2 Go依赖:通过

交接列出的19个模块各tidy一次，再执行robot vendor，本节20条均exit0。tools/dbmove另在§13执行。成功退出不证明具体go.mod/go.sum内容增量；没有开跑前字节快照。

| ID | 退出码 | 工作目录（相对执行根） | 已执行命令模板 | 日志文件（相对日志根） |
|---|---:|---|---|---|
| `02-tidy-go-shared` | 0 | `go/shared` | `go mod tidy` | `02-tidy-go-shared.log` |
| `02-tidy-go-schemamigrate` | 0 | `go/schemamigrate` | `go mod tidy` | `02-tidy-go-schemamigrate.log` |
| `02-tidy-go-data_service` | 0 | `go/data_service` | `go mod tidy` | `02-tidy-go-data_service.log` |
| `02-tidy-go-db` | 0 | `go/db` | `go mod tidy` | `02-tidy-go-db.log` |
| `02-tidy-go-login` | 0 | `go/login` | `go mod tidy` | `02-tidy-go-login.log` |
| `02-tidy-go-guild` | 0 | `go/guild` | `go mod tidy` | `02-tidy-go-guild.log` |
| `02-tidy-go-trade` | 0 | `go/trade` | `go mod tidy` | `02-tidy-go-trade.log` |
| `02-tidy-go-mail` | 0 | `go/mail` | `go mod tidy` | `02-tidy-go-mail.log` |
| `02-tidy-go-rank` | 0 | `go/rank` | `go mod tidy` | `02-tidy-go-rank.log` |
| `02-tidy-go-friend` | 0 | `go/friend` | `go mod tidy` | `02-tidy-go-friend.log` |
| `02-tidy-go-match` | 0 | `go/match` | `go mod tidy` | `02-tidy-go-match.log` |
| `02-tidy-go-chat` | 0 | `go/chat` | `go mod tidy` | `02-tidy-go-chat.log` |
| `02-tidy-go-player_inbox` | 0 | `go/player_inbox` | `go mod tidy` | `02-tidy-go-player_inbox.log` |
| `02-tidy-go-player_locator` | 0 | `go/player_locator` | `go mod tidy` | `02-tidy-go-player_locator.log` |
| `02-tidy-go-scene_manager` | 0 | `go/scene_manager` | `go mod tidy` | `02-tidy-go-scene_manager.log` |
| `02-tidy-go-client_rpc_router` | 0 | `go/client_rpc_router` | `go mod tidy` | `02-tidy-go-client_rpc_router.log` |
| `02-tidy-tools-merge_zone` | 0 | `tools/merge_zone` | `go mod tidy` | `02-tidy-tools-merge_zone.log` |
| `02-tidy-tools-data_consistency_check` | 0 | `tools/data_consistency_check` | `go mod tidy` | `02-tidy-tools-data_consistency_check.log` |
| `02-tidy-robot` | 0 | `robot` | `go mod tidy` | `02-tidy-robot.log` |
| `02-vendor-robot` | 0 | `robot` | `go mod vendor` | `02-vendor-robot.log` |

## 3 Go编译、vet、单测:失败

58条完成命令，3条非零；19个普通模块build均exit0。client_rpc_router的vet/test在测试源码编译失败；go/test.ps1另有1个普通文本断言失败。本节未全绿。JSON事件含子测试9346 PASS、0 FAIL、727 SKIP；顶层4378 PASS、0 FAIL、583 SKIP；另89个包级skip。JSON计数不包括go/test.ps1普通文本失败，727个SKIP均没跑。

追加构建修复后：`03-router-buildfix-build`、`03-router-buildfix-vet`、`03-router-buildfix-test`均exit0，41个顶层PASS/0FAIL/0SKIP；只替换该测试3处退役符号，断言语义不变。新日志 [build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/03-router-buildfix-build.log) / [vet](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/03-router-buildfix-vet.log) / [test](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/03-router-buildfix-test.log)。这不抹掉下面首次编译失败，也不解决L1的shared/placement断言；不得把本节改称全绿。

全部独立编译错误原文：

```text
03-vet-go-client_rpc_router：
vet.exe: internal\logic\forwardlogic_test.go:231:37: undefined: game.DataServiceLoadPlayerDataMessageId
03-test-go-client_rpc_router：
internal\logic\forwardlogic_test.go:231:37: undefined: game.DataServiceLoadPlayerDataMessageId
internal\logic\forwardlogic_test.go:237:27: undefined: game.DataServiceLoadPlayerDataMessageId
internal\logic\forwardlogic_test.go:239:40: undefined: game.DataServiceLoadPlayerDataMessageId
```

go/test.ps1在db段通过后，于shared段失败；脚本内后续login/tools/dbmove段没跑，login普通模块另有本轮03独立命令。失败断言原文：

```text
--- FAIL: TestMergedZoneCacheRefreshReplacesAndKeepsOldOnFailure (0.00s)
    merged_zone_test.go:357: a successful refresh reaches OnRefresh with nil: [<nil> <nil> read merged zones: db down read merged zones: db down zone_merged_into is broken: placement: merged zone chain cycles, exceeds the hop limit or targets zone 0: zone 5 stuck at 5 after 16 hops (next 6) zone_merged_into is broken: placement: merged zone chain cycles, exceeds the hop limit or targets zone 0: zone 5 stuck at 5 after 16 hops (next 6) <nil> <nil>]
FAIL
FAIL	shared/placement	0.013s
ok  	shared/kafkautil	0.043s
ok  	shared/dbplacement	0.047s
FAIL
L1_TESTS_FAIL exit=1
```

data_service/internal/idissuer与chat/internal/authority -run TestActorVerify的-race均没跑：PATH无gcc，未安装工具；普通版由本轮全量测试覆盖，不能替代race证据。见[03-race-skipped.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/03-race-skipped.txt)。

完整失败原文：[aggregate-03-failed-output.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-03-failed-output.jsonl)；失败与编译错误清单：[aggregate-03-analysis.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-03-analysis.txt)；全部SKIP按没跑，明细：[aggregate-03-skipped.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-03-skipped.txt)。

| ID | 退出码 | 工作目录（相对执行根） | 已执行命令模板 | 日志文件（相对日志根） |
|---|---:|---|---|---|
| `03-build-go-shared` | 0 | `go/shared` | `go build -gcflags=-e ./...` | `03-build-go-shared.log` |
| `03-vet-go-shared` | 0 | `go/shared` | `go vet ./...` | `03-vet-go-shared.log` |
| `03-test-go-shared` | 0 | `go/shared` | `go test -json -count=1 ./...` | `03-test-go-shared.log` |
| `03-build-go-schemamigrate` | 0 | `go/schemamigrate` | `go build -gcflags=-e ./...` | `03-build-go-schemamigrate.log` |
| `03-vet-go-schemamigrate` | 0 | `go/schemamigrate` | `go vet ./...` | `03-vet-go-schemamigrate.log` |
| `03-test-go-schemamigrate` | 0 | `go/schemamigrate` | `go test -json -count=1 ./...` | `03-test-go-schemamigrate.log` |
| `03-build-go-data_service` | 0 | `go/data_service` | `go build -gcflags=-e ./...` | `03-build-go-data_service.log` |
| `03-vet-go-data_service` | 0 | `go/data_service` | `go vet ./...` | `03-vet-go-data_service.log` |
| `03-test-go-data_service` | 0 | `go/data_service` | `go test -json -count=1 ./...` | `03-test-go-data_service.log` |
| `03-build-go-db` | 0 | `go/db` | `go build -gcflags=-e ./...` | `03-build-go-db.log` |
| `03-vet-go-db` | 0 | `go/db` | `go vet ./...` | `03-vet-go-db.log` |
| `03-test-go-db` | 0 | `go/db` | `go test -json -count=1 ./...` | `03-test-go-db.log` |
| `03-build-go-login` | 0 | `go/login` | `go build -gcflags=-e ./...` | `03-build-go-login.log` |
| `03-vet-go-login` | 0 | `go/login` | `go vet ./...` | `03-vet-go-login.log` |
| `03-test-go-login` | 0 | `go/login` | `go test -json -count=1 ./...` | `03-test-go-login.log` |
| `03-build-go-guild` | 0 | `go/guild` | `go build -gcflags=-e ./...` | `03-build-go-guild.log` |
| `03-vet-go-guild` | 0 | `go/guild` | `go vet ./...` | `03-vet-go-guild.log` |
| `03-test-go-guild` | 0 | `go/guild` | `go test -json -count=1 ./...` | `03-test-go-guild.log` |
| `03-build-go-trade` | 0 | `go/trade` | `go build -gcflags=-e ./...` | `03-build-go-trade.log` |
| `03-vet-go-trade` | 0 | `go/trade` | `go vet ./...` | `03-vet-go-trade.log` |
| `03-test-go-trade` | 0 | `go/trade` | `go test -json -count=1 ./...` | `03-test-go-trade.log` |
| `03-build-go-mail` | 0 | `go/mail` | `go build -gcflags=-e ./...` | `03-build-go-mail.log` |
| `03-vet-go-mail` | 0 | `go/mail` | `go vet ./...` | `03-vet-go-mail.log` |
| `03-test-go-mail` | 0 | `go/mail` | `go test -json -count=1 ./...` | `03-test-go-mail.log` |
| `03-build-go-rank` | 0 | `go/rank` | `go build -gcflags=-e ./...` | `03-build-go-rank.log` |
| `03-vet-go-rank` | 0 | `go/rank` | `go vet ./...` | `03-vet-go-rank.log` |
| `03-test-go-rank` | 0 | `go/rank` | `go test -json -count=1 ./...` | `03-test-go-rank.log` |
| `03-build-go-friend` | 0 | `go/friend` | `go build -gcflags=-e ./...` | `03-build-go-friend.log` |
| `03-vet-go-friend` | 0 | `go/friend` | `go vet ./...` | `03-vet-go-friend.log` |
| `03-test-go-friend` | 0 | `go/friend` | `go test -json -count=1 ./...` | `03-test-go-friend.log` |
| `03-build-go-match` | 0 | `go/match` | `go build -gcflags=-e ./...` | `03-build-go-match.log` |
| `03-vet-go-match` | 0 | `go/match` | `go vet ./...` | `03-vet-go-match.log` |
| `03-test-go-match` | 0 | `go/match` | `go test -json -count=1 ./...` | `03-test-go-match.log` |
| `03-build-go-chat` | 0 | `go/chat` | `go build -gcflags=-e ./...` | `03-build-go-chat.log` |
| `03-vet-go-chat` | 0 | `go/chat` | `go vet ./...` | `03-vet-go-chat.log` |
| `03-test-go-chat` | 0 | `go/chat` | `go test -json -count=1 ./...` | `03-test-go-chat.log` |
| `03-build-go-player_inbox` | 0 | `go/player_inbox` | `go build -gcflags=-e ./...` | `03-build-go-player_inbox.log` |
| `03-vet-go-player_inbox` | 0 | `go/player_inbox` | `go vet ./...` | `03-vet-go-player_inbox.log` |
| `03-test-go-player_inbox` | 0 | `go/player_inbox` | `go test -json -count=1 ./...` | `03-test-go-player_inbox.log` |
| `03-build-go-player_locator` | 0 | `go/player_locator` | `go build -gcflags=-e ./...` | `03-build-go-player_locator.log` |
| `03-vet-go-player_locator` | 0 | `go/player_locator` | `go vet ./...` | `03-vet-go-player_locator.log` |
| `03-test-go-player_locator` | 0 | `go/player_locator` | `go test -json -count=1 ./...` | `03-test-go-player_locator.log` |
| `03-build-go-scene_manager` | 0 | `go/scene_manager` | `go build -gcflags=-e ./...` | `03-build-go-scene_manager.log` |
| `03-vet-go-scene_manager` | 0 | `go/scene_manager` | `go vet ./...` | `03-vet-go-scene_manager.log` |
| `03-test-go-scene_manager` | 0 | `go/scene_manager` | `go test -json -count=1 ./...` | `03-test-go-scene_manager.log` |
| `03-build-go-client_rpc_router` | 0 | `go/client_rpc_router` | `go build -gcflags=-e ./...` | `03-build-go-client_rpc_router.log` |
| `03-vet-go-client_rpc_router` | 1 | `go/client_rpc_router` | `go vet ./...` | `03-vet-go-client_rpc_router.log` |
| `03-test-go-client_rpc_router` | 1 | `go/client_rpc_router` | `go test -json -count=1 ./...` | `03-test-go-client_rpc_router.log` |
| `03-build-tools-merge_zone` | 0 | `tools/merge_zone` | `go build -gcflags=-e ./...` | `03-build-tools-merge_zone.log` |
| `03-vet-tools-merge_zone` | 0 | `tools/merge_zone` | `go vet ./...` | `03-vet-tools-merge_zone.log` |
| `03-test-tools-merge_zone` | 0 | `tools/merge_zone` | `go test -json -count=1 ./...` | `03-test-tools-merge_zone.log` |
| `03-build-tools-data_consistency_check` | 0 | `tools/data_consistency_check` | `go build -gcflags=-e ./...` | `03-build-tools-data_consistency_check.log` |
| `03-vet-tools-data_consistency_check` | 0 | `tools/data_consistency_check` | `go vet ./...` | `03-vet-tools-data_consistency_check.log` |
| `03-test-tools-data_consistency_check` | 0 | `tools/data_consistency_check` | `go test -json -count=1 ./...` | `03-test-tools-data_consistency_check.log` |
| `03-build-robot` | 0 | `robot` | `go build -gcflags=-e ./...` | `03-build-robot.log` |
| `03-vet-robot` | 0 | `robot` | `go vet ./...` | `03-vet-robot.log` |
| `03-test-robot` | 0 | `robot` | `go test -json -count=1 ./...` | `03-test-robot.log` |
| `03-go-test-script` | 1 | `.` | `pwsh -NoProfile -File go/test.ps1` | `03-go-test-script.log` |

## 4 真库:失败

自建本轮player-store-r2-* MySQL/TiDB/Redis，未借codex资源。30条完成命令，12条非零。两条环境SQL初始化在容器SQL ready前连接失败，不重试；依赖预建guild_test/trade_it的两项没跑。随机库夹具等独立项继续。夹具资格与业务提交失败均记失败，不能称整条业务链验证完成。

后续只读核实源TiKV在02:53:16.959Z已OOM退出；各命令起止与环境失效边界见§13及 [13-tikv-oom-finished.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-tikv-oom-finished.log)。保留下面真实初始结果，不将失效后端上的运行当健康真库验收。

JSON事件含子测试1486 PASS、278 FAIL、3 SKIP；顶层755 PASS、240 FAIL、3 SKIP；另9个包级skip、6个已RUN无终态与6个包超时。计数含集成命令内普通单测与重复范围，不是去重后的真库业务场景数。MySQL与TiDB playerstore命令各37个顶层PASS，其中各12个TestIT_ PASS。

各测试命令计数及超时最后用例：

```text
计数为本轮各命令测试事件，含集成命令内普通单测与重复范围，不能当成去重后的真库业务场景数。失败发生在夹具初始化时也计失败，不声称已验证业务。未RUN的用例数量需完整同tag-list另算。

命令 | 退出 | 顶层PASS | 顶层FAIL | 顶层SKIP | 已RUN无终态 | 超时包数
04-mysql-playerstore | 0 | 37 | 0 | 0 | 0 | 0
04-mysql-data-service | 1 | 293 | 149 | 2 | 0 | 0
04-mysql-guild-economy | 0 | 25 | 0 | 1 | 0 | 0
04-mysql-inbox | 1 | 99 | 37 | 0 | 0 | 0
04-tidb-playerstore | 0 | 37 | 0 | 0 | 0 | 0
04-tidb-data-directory | 1 | 10 | 3 | 0 | 1 | 1
04-tidb-data-service | 1 | 177 | 4 | 0 | 1 | 1
04-tidb-chat | 1 | 13 | 2 | 0 | 0 | 0
04-tidb-match | 1 | 64 | 13 | 0 | 2 | 2
04-tidb-rank-mail-chain | 1 | 0 | 1 | 0 | 0 | 0
04-tidb-rank | 1 | 0 | 7 | 0 | 0 | 0
04-tidb-mail | 1 | 0 | 13 | 0 | 1 | 1
04-tidb-guild-trial | 1 | 0 | 11 | 0 | 1 | 1

SQL playerstore 的 TestIT_ 顶层用例：
04-mysql-playerstore {"pass": 12}
04-tidb-playerstore {"pass": 12}

超时/未完成原文：
04-tidb-data-directory | data_service/internal/store | panic: test timed out after 10m0s | 最后RUN=('data_service/internal/store', 'TestTiDBAccountDirectoryReadTakesNoLockOnHit') | 无终态=[('data_service/internal/store', 'TestTiDBAccountDirectoryReadTakesNoLockOnHit')]
04-tidb-data-service | data_service/internal/store | panic: test timed out after 10m0s | 最后RUN=('data_service/internal/store', 'TestTiDBShardedRouteStore') | 无终态=[('data_service/internal/store', 'TestTiDBShardedRouteStore')]
04-tidb-match | match/internal/authority | panic: test timed out after 10m0s | 最后RUN=('match/internal/authority', 'TestDirectTiDBFixedPendingHighWaterDoesNotIncludeLaterInputs') | 无终态=[('match/internal/authority', 'TestDirectTiDBFixedPendingHighWaterDoesNotIncludeLaterInputs')]
04-tidb-match | match/internal/team | panic: test timed out after 10m0s | 最后RUN=('match/internal/team', 'TestTeamTiDBRosterZoneCheckResolvesMergedZones') | 无终态=[('match/internal/team', 'TestTeamTiDBRosterZoneCheckResolvesMergedZones')]
04-tidb-mail | mail/internal/data | panic: test timed out after 10m0s | 最后RUN=('mail/internal/data', 'TestTiDBMailPendingAssetOpStatesBlockFreeze') | 无终态=[('mail/internal/data', 'TestTiDBMailPendingAssetOpStatesBlockFreeze')]
04-tidb-guild-trial | guild/internal/data | panic: test timed out after 10m0s | 最后RUN=('guild/internal/data', 'TestTiDBTrialOppositeRosterConcurrentUsesUnsignedP') | 无终态=[('guild/internal/data', 'TestTiDBTrialOppositeRosterConcurrentUsesUnsignedP')]
```

明确没跑：MySQL guild-lock、MySQL trade-seq（预建库初始化失败）；全部-race（无gcc）；3条SKIP及超时后尚未进入的用例。所有SKIP/无终态列表见[aggregate-04-skipped.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-04-skipped.txt)。两版Redis容器启动不等于两版真实Redis业务用例通过；相关C++用例因§6失败没跑，§16后续有实际token结果时另记。

MySQL data_service口令错误的只读依据：`go/data_service/internal/store/storetest/mysql.go:212～220`仅非空EnvPassword覆盖YAML，环境空值回落源YAML口令。未改配置/口令重跑。

以下按原错误正文归组保留全部失败用例，父测试无独立断言单列；各命令完整堆栈和上下文保存在日志及失败JSONL：

```text
失败断言组=12；按错误正文归组；去除文件行号、testify 的 Error Trace/Test 定位元数据，并仅在分组键规范化 *_it_<随机后缀> 测试库名和系统临时路径。下列至少保留一条未规范化断言样例，全部原文见失败输出 JSONL。

错误组 1（72 个失败事件，原文变体=1）
断言原文样例：

Error:      	Received unexpected error:
权威SQL启动失败: 权威SQL物理会话或boot身份核验失败
用例：
- 04-mysql-data-service | data_service/internal/store | TestTiDBShardedCreateAndReadAcrossShards；位置=account_directory_shard_tidb_test.go:192
- 04-mysql-data-service | data_service/internal/store | TestTiDBShardedCapHoldsUnderConcurrency；位置=account_directory_shard_tidb_test.go:219
- 04-mysql-data-service | data_service/internal/store | TestTiDBShardedTransferAcrossShards；位置=account_directory_shard_tidb_test.go:249
- 04-mysql-data-service | data_service/internal/store | TestTiDBShardedStaleCreateRefIsSweptAndTombstoned；位置=account_directory_shard_tidb_test.go:288
- 04-mysql-data-service | data_service/internal/store | TestTiDBWritePendingRefSurvivesConcurrentDrop；位置=account_directory_shard_tidb_test.go:322
- 04-mysql-data-service | data_service/internal/store | TestTiDBReserveRefOverExistingRef；位置=account_directory_shard_tidb_test.go:353
- 04-mysql-data-service | data_service/internal/store | TestTiDBActivateRefLeavesAnotherTransferAlone；位置=account_directory_shard_tidb_test.go:386
- 04-mysql-data-service | data_service/internal/store | TestTiDBShardedPlayerNames；位置=account_directory_shard_tidb_test.go:431
- 04-mysql-data-service | data_service/internal/store | TestTiDBShardedRouteStore；位置=account_directory_shard_tidb_test.go:480
- 04-mysql-data-service | data_service/internal/store | TestTiDBLedgerIsItsOwnDatabase；位置=account_directory_shard_tidb_test.go:514
- 04-mysql-data-service | data_service/internal/store | TestTiDBWrongLogicalShardsRefusesToStart；位置=account_directory_shard_tidb_test.go:535
- 04-mysql-data-service | data_service/internal/store | TestTiDBAccountDirectoryCapReplayRepairAndRemoval；位置=account_directory_tidb_test.go:125
- 04-mysql-data-service | data_service/internal/store | TestTiDBAccountDirectoryReadTakesNoLockOnHit；位置=account_directory_tidb_test.go:211
- 04-mysql-data-service | data_service/internal/store | TestTiDBAccountDirectoryConcurrentFirstLoginCreatesOneRow；位置=account_directory_tidb_test.go:249
- 04-mysql-data-service | data_service/internal/store | TestTiDBAccountDirectoryPreservesExactAccountIdentity；位置=account_directory_tidb_test.go:278
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleAdmissionVerifyFollowsGenerationAndClose；位置=account_role_admission_tidb_test.go:39
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleAdmissionFixedHolderCancelAndLateGenerations；位置=account_role_admission_tidb_test.go:72
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleAdmissionStrongSQLProofRejectsUnsyncedRowsAndKeepsReceipt；位置=account_role_admission_tidb_test.go:194
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleAdmissionSQLProofNoAdmissionAndHistoricalFloor；位置=account_role_admission_tidb_test.go:239
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleAdmissionRegisterAndBeginShareCommitLock；位置=account_role_admission_tidb_test.go:280
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleAdmissionSQLProofRejectsRouteAndSchemaFailures；位置=account_role_admission_tidb_test.go:308
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleAdmissionOpenRejectsRemovalAndFloorSurvivesRemoval；位置=account_role_admission_tidb_test.go:350
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleTradeDeadHolderIsFencedWithoutAck；位置=account_role_admission_tidb_test.go:373
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleAdmissionCloseAfterOwnerChangedIsMoot；位置=account_role_admission_tidb_test.go:408
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleTradeReleasedCurrentHolderStillWaitsForAck；位置=account_role_admission_tidb_test.go:437
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleTradeCurrentHolderBlocksUntilReleased；位置=account_role_admission_tidb_test.go:464
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleCancelBeforeUnknownBeginPersistsPermanentTombstone；位置=account_role_cross_domain_tidb_test.go:137
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleBeginAllCleanFreezesParticipantsThenEntersFreezing；位置=account_role_cross_domain_tidb_test.go:167
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleBeginDryRunRejectionWritesNothing/有挂起；位置=account_role_cross_domain_tidb_test.go:202
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleBeginDryRunRejectionWritesNothing/问不齐；位置=account_role_cross_domain_tidb_test.go:202
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleBeginRejectedBeforeDryRun；位置=account_role_cross_domain_tidb_test.go:229
- 04-mysql-data-service | data_service/internal/store | TestTiDBRolePreparingDoesNotBlockEntryAndFreezingSnapshotsIt；位置=account_role_cross_domain_tidb_test.go:249
- 04-mysql-data-service | data_service/internal/store | TestTiDBRolePreparingRejectsOwnershipOperations；位置=account_role_cross_domain_tidb_test.go:286
- 04-mysql-data-service | data_service/internal/store | TestTiDBRolePreparingExpiredIsAbortedByReplay；位置=account_role_cross_domain_tidb_test.go:308
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleAbortStalePreparingInBackground；位置=account_role_cross_domain_tidb_test.go:348
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleCancelFromPreparingDoesNotTouchPlayerStore；位置=account_role_cross_domain_tidb_test.go:423
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleCancelOnlyTouchesPlayerStoreForPendingHolders；位置=account_role_cross_domain_tidb_test.go:438
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleBeginBlockedLeavesNoTrace；位置=account_role_cross_domain_tidb_test.go:468
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleBeginUnreachableKeepsPreparingAndReplayResumes；位置=account_role_cross_domain_tidb_test.go:489
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleCancelFromPreparingThawsAndKeepsTombstone；位置=account_role_cross_domain_tidb_test.go:522
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleFenceVersionBumpsOnEveryOwnershipChangeAndTransferPushes；位置=account_role_cross_domain_tidb_test.go:544
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleMissingCoordinatorRejectsOwnershipChanges；位置=account_role_cross_domain_tidb_test.go:580
- 04-mysql-data-service | data_service/internal/store | TestTiDBRolePrepareConclusionOfOlderRoundDoesNotCount；位置=account_role_cross_domain_tidb_test.go:605
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleBeginHolderConflictAtFinishLeavesNoTrace；位置=account_role_cross_domain_tidb_test.go:638
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleListingCannotTakeRemovalTokenRange；位置=account_role_cross_domain_tidb_test.go:655
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleRemovalBlockedClearsTokenAndSmallTable；位置=account_role_cross_domain_tidb_test.go:665
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleRemovalEntryDuringPrepareAbandons；位置=account_role_cross_domain_tidb_test.go:685
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleRemovalUnreachableKeepsTokenAndBackgroundFinishes；位置=account_role_cross_domain_tidb_test.go:713
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleRemovalBackgroundAbandonsWhenBlocked；位置=account_role_cross_domain_tidb_test.go:751
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleRemovalWalksRolesOneByOne；位置=account_role_cross_domain_tidb_test.go:768
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleRemovalFirstRoleRejectedStaysDefinite；位置=account_role_cross_domain_tidb_test.go:796
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleBeginFailureAfterFreezingCommitIsNotADefiniteRejection；位置=account_role_cross_domain_tidb_test.go:819
- 04-mysql-data-service | data_service/internal/store | TestTiDBCreateRoleCommitsDirectoryAndInitialRowsOnce；位置=account_role_initial_commit_tidb_test.go:45
- 04-mysql-data-service | data_service/internal/store | TestTiDBCreateRoleDoesNotQueueOnRouteMetaAndFrozenStillRejects；位置=account_role_initial_commit_tidb_test.go:71
- 04-mysql-data-service | data_service/internal/store | TestTiDBCreateRoleInitialRowCollisionLeavesNoRole；位置=account_role_initial_commit_tidb_test.go:108
- 04-mysql-data-service | data_service/internal/store | TestTiDBCreateRoleConcurrentInitialRowsAreWrittenOnce；位置=account_role_initial_commit_tidb_test.go:135
- 04-mysql-data-service | data_service/internal/store | TestTiDBCreateRoleMissingInitialTableLeavesNoRole；位置=account_role_initial_commit_tidb_test.go:172
- 04-mysql-data-service | data_service/internal/store | TestTiDBInitialCommitIsRequiredAndLegacyIsExplicit；位置=account_role_initial_commit_tidb_test.go:201
- 04-mysql-data-service | data_service/internal/store | TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor/initial；位置=account_role_initial_commit_tidb_test.go:228
- 04-mysql-data-service | data_service/internal/store | TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor/unmarked；位置=account_role_initial_commit_tidb_test.go:228
- 04-mysql-data-service | data_service/internal/store | TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor/saved-row；位置=account_role_initial_commit_tidb_test.go:228
- 04-mysql-data-service | data_service/internal/store | TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor/historical-floor；位置=account_role_initial_commit_tidb_test.go:228
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleTradeFreezeCancelDoesNotRefreezeOrClearNewListing；位置=account_role_trade_tidb_test.go:46
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleTransferCapConcurrentReplayAndOriginalReceipt；位置=account_role_trade_tidb_test.go:129
- 04-mysql-data-service | data_service/internal/store | TestTiDBRoleTransferOppositeAccountsUseOneByteLockOrder；位置=account_role_trade_tidb_test.go:221
- 04-mysql-data-service | data_service/internal/store | TestTiDBCreateRoleWritesPlayerStoreRows；位置=player_store_access_tidb_test.go:50
- 04-mysql-data-service | data_service/internal/store | TestTiDBReadPlayerStoreAsset；位置=player_store_access_tidb_test.go:97
- 04-mysql-data-service | data_service/internal/store | TestTiDBReleaseDeadPlayerStoreHolder；位置=player_store_access_tidb_test.go:133
- 04-mysql-data-service | data_service/internal/store | TestTiDBRaiseStoreEpochComparesClaimSeq；位置=player_store_access_tidb_test.go:169
- 04-mysql-data-service | data_service/internal/store | TestTiDBVerifyFrozenOnPlayerStore；位置=player_store_access_tidb_test.go:207
- 04-mysql-data-service | data_service/internal/store | TestTiDBReadPlayerDisplays；位置=player_store_access_tidb_test.go:246
- 04-mysql-data-service | data_service/internal/store | TestTiDBEmptyRouteBootstrapDoesNotTreatExistingPlayerHistoryAsFresh；位置=route_bootstrap_tidb_test.go:17

错误组 2（1 个失败事件，原文变体=1）
断言原文样例：

Error:      	Received unexpected error:
migrate account_directory_mig_a51ea0aabc9df045_00: open mysql: 权威SQL启动失败: 权威SQL物理会话或boot身份核验失败
用例：
- 04-mysql-data-service | data_service/internal/store | TestTiDBMigrateSchemaBuildsEveryShardAndLedger；位置=account_directory_shard_tidb_test.go:588

错误组 3（99 个失败事件，原文变体=1）
断言原文样例：
Error 1045 (28000): Access denied for user 'root'@'192.168.80.1' (using password: YES)
用例：
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_BootstrapDDLIsVarcharUniqueBin；位置=player_name_store_integration_test.go:57
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveIdempotentAndTaken；位置=player_name_store_integration_test.go:107
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ConcurrentSameName；位置=player_name_store_integration_test.go:174
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseConditional；位置=player_name_store_integration_test.go:233
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_BatchGetSkipsAbsentAndRejectsOversize；位置=player_name_store_integration_test.go:295
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseRacesTwoReservesConverges/未提交的Release正删被抢的名字/a_竞争者都大于原主人；位置=player_name_store_integration_test.go:870
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseRacesTwoReservesConverges/未提交的Release正删被抢的名字/b_竞争者都小于原主人；位置=player_name_store_integration_test.go:870
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseRacesTwoReservesConverges/未提交的Release正删被抢的名字/c_竞争者一大一小；位置=player_name_store_integration_test.go:870
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseRacesTwoReservesConverges/被抢名字已释放且未purge_未提交的Release正删后继名字/a_竞争者都大于原主人；位置=player_name_store_integration_test.go:870
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseRacesTwoReservesConverges/被抢名字已释放且未purge_未提交的Release正删后继名字/b_竞争者都小于原主人；位置=player_name_store_integration_test.go:870
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseRacesTwoReservesConverges/被抢名字已释放且未purge_未提交的Release正删后继名字/c_竞争者一大一小；位置=player_name_store_integration_test.go:870
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks/a_竞争者都大于原主人；位置=player_name_store_integration_test.go:915
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks/b_竞争者都小于原主人；位置=player_name_store_integration_test.go:915
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks/c_竞争者一大一小；位置=player_name_store_integration_test.go:915
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveBehindPendingReleaseOutcome/未提交的Release正删被抢的名字/a_竞争者都大于原主人；位置=player_name_store_integration_test.go:964
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveBehindPendingReleaseOutcome/未提交的Release正删被抢的名字/b_竞争者都小于原主人；位置=player_name_store_integration_test.go:964
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveBehindPendingReleaseOutcome/未提交的Release正删被抢的名字/c_竞争者一大一小；位置=player_name_store_integration_test.go:964
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveBehindPendingReleaseOutcome/被抢名字已释放且未purge_未提交的Release正删后继名字/a_竞争者都大于原主人；位置=player_name_store_integration_test.go:964
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveBehindPendingReleaseOutcome/被抢名字已释放且未purge_未提交的Release正删后继名字/b_竞争者都小于原主人；位置=player_name_store_integration_test.go:964
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveBehindPendingReleaseOutcome/被抢名字已释放且未purge_未提交的Release正删后继名字/c_竞争者一大一小；位置=player_name_store_integration_test.go:964
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseStatementLocksClusteredKeyFirst；位置=player_name_store_integration_test.go:1040
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_StoreRefusesUnknownPrimaryKey/player_name；位置=player_name_store_integration_test.go:1104
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_StoreRefusesUnknownPrimaryKey/player_name_owner；位置=player_name_store_integration_test.go:1104
- 04-mysql-data-service | data_service/internal/store | TestPlayerName_StoreRefusesWrongDirectoryShard；位置=player_name_store_integration_test.go:1118
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_LifecycleWritesLinesCountersAndAudit；位置=recall_store_integration_test.go:123
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_ShortfallCountsSeparately；位置=recall_store_integration_test.go:185
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_SourceCanBeConsumedOnce；位置=recall_store_integration_test.go:200
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_FrozenRoleDefersDispatch；位置=recall_store_integration_test.go:226
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_MissingRoleClosesTheRow；位置=recall_store_integration_test.go:249
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_CancelStopsWhatHasNotTakenEffect；位置=recall_store_integration_test.go:272
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_CancelRacingDispatchKeepsCountersRight；位置=recall_store_integration_test.go:315
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_SourcesAreReleasedOnlyWhenNothingWasRecalled；位置=recall_store_integration_test.go:344
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_RescheduleWithWrongTokenIsLeaseLost；位置=recall_store_integration_test.go:384
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_PendingForRoleBlocksUntilSettled；位置=recall_store_integration_test.go:393
- 04-mysql-data-service | data_service/internal/store | TestRecallStore_ResolveManually；位置=recall_store_integration_test.go:413
- 04-mysql-data-service | data_service/internal/store | TestRouteSchema_MigrationCreatesTablesSeedsMetaAndIsIdempotent；位置=route_store_integration_test.go:157
- 04-mysql-data-service | data_service/internal/store | TestRouteSchema_CheckConstraintsEnforced；位置=route_store_integration_test.go:194
- 04-mysql-data-service | data_service/internal/store | TestRouteSchema_ShapeGuardRejectsTamperedTable/丢列；位置=route_store_integration_test.go:228
- 04-mysql-data-service | data_service/internal/store | TestRouteSchema_ShapeGuardRejectsTamperedTable/换主键；位置=route_store_integration_test.go:238
- 04-mysql-data-service | data_service/internal/store | TestRouteSchema_ShapeGuardRejectsTamperedTable/player_route主键退回player_id；位置=route_store_integration_test.go:247
- 04-mysql-data-service | data_service/internal/store | TestRouteSchema_ShapeGuardRejectsTamperedTable/G1a之前的真实旧形状；位置=route_store_integration_test.go:259
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_RegisterHomeBranches；位置=route_store_integration_test.go:301
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_NullPlacementBindsNull；位置=route_store_integration_test.go:369
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_ConcurrentSamePlayer；位置=route_store_integration_test.go:422
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_ConcurrentManyPlayersSameZone；位置=route_store_integration_test.go:477
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_LockWaitBounded；位置=route_store_integration_test.go:495
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_MetaMissingAndNotWritableFailClosed；位置=route_store_integration_test.go:527
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_ProbeRejectsPoolWithoutSessionVariables；位置=route_store_integration_test.go:566
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_ProbeRejectsUnenforcedShardCheck；位置=route_store_integration_test.go:586
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_ProbeRejectsTableBuiltWithAnotherShardModulus；位置=route_store_integration_test.go:606
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_ProbeKeysHeldByRealPlayers；位置=route_store_integration_test.go:658
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_ProbesQueueOnMetaGuard；位置=route_store_integration_test.go:719
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_ShardMismatchRowIsInvisibleToPointReads；位置=route_store_integration_test.go:761
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_KeyedReadsUsePrimaryRange；位置=route_store_integration_test.go:842
- 04-mysql-data-service | data_service/internal/store | TestRouteStore_TiDBClusteredShardsAndPresplit；位置=route_store_integration_test.go:909
- 04-mysql-data-service | data_service/internal/store | TestMigrateSchema_CreatesGlobalTablesAndIsIdempotent；位置=schema_integration_test.go:28
- 04-mysql-data-service | data_service/internal/store | TestMigrateSchema_ConcurrentFirstBootIsSerialized；位置=schema_integration_test.go:122
- 04-mysql-data-service | data_service/internal/store | TestMigrateSchema_LockBusyReturnsSentinel；位置=schema_integration_test.go:145
- 04-mysql-data-service | data_service/internal/store | TestMigrateSchema_RejectsLegacyHandWrittenTables；位置=schema_legacy_integration_test.go:87
- 04-mysql-data-service | data_service/internal/store | TestSnapshotMigration_RefusesKeyPresentColumnBroken；位置=snapshot_guid_key_shape_integration_test.go:59
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_ProbeRejectsKeyPresentColumnBroken；位置=snapshot_guid_key_shape_integration_test.go:81
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_GuidDedupAndGmRowsCoexist；位置=snapshot_store_integration_test.go:68
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_GmReadersExcludeSceneRows；位置=snapshot_store_integration_test.go:115
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_ConcurrentWritersNeverSurfaceDeadlock；位置=snapshot_store_integration_test.go:373
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_LegacyGapDeadlockIsAbsorbedInPlace；位置=snapshot_store_integration_test.go:461
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_GuidUniqueKeyIsEnforcedByTheDatabase；位置=snapshot_store_integration_test.go:613
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_GuidLookupUsesUniqueKey；位置=snapshot_store_integration_test.go:657
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_SameGuidConcurrentWritersLandOneRow；位置=snapshot_store_integration_test.go:708
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_DistinctGuidDoesNotWaitOnUncommittedNeighbour；位置=snapshot_store_integration_test.go:768
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_DistinctGuidsNeverDeadlock；位置=snapshot_store_integration_test.go:821
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_DistinctGuidsNeverDeadlock_MixedRowIDs；位置=snapshot_store_integration_test.go:821
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_SameGuidQueueDeadlockIsAbsorbed；位置=snapshot_store_integration_test.go:890
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_BatchOutOfGuidOrderOverlapDoesNotDeadlock；位置=snapshot_store_integration_test.go:966
- 04-mysql-data-service | data_service/internal/store | TestSnapshotTableUniqueIndexesAreOnlyGuidKey；位置=snapshot_store_integration_test.go:1030
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_BatchOverlapDoesNotDeadlock；位置=snapshot_store_integration_test.go:1072
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_BatchOverlapDoesNotDeadlock_MixedRowIDs；位置=snapshot_store_integration_test.go:1072
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_LegacyWriterCoexistsWithUniqueKey；位置=snapshot_store_integration_test.go:1149
- 04-mysql-data-service | data_service/internal/store | TestSnapshotMigration_DedupesDuplicateGuidsAndIsRerunnable；位置=snapshot_store_integration_test.go:1204
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_FallsBackWhenUniqueKeyAbsent；位置=snapshot_store_integration_test.go:1265
- 04-mysql-data-service | data_service/internal/store | TestSnapshotMigration_RefusesSameNamedNonUniqueIndex；位置=snapshot_store_integration_test.go:1302
- 04-mysql-data-service | data_service/internal/store | TestSnapshotMigration_RefusesSameNamedPlainColumn；位置=snapshot_store_integration_test.go:1332
- 04-mysql-data-service | data_service/internal/store | TestSnapshotMigration_DedupePointDeleteUsesPrimaryKey；位置=snapshot_store_integration_test.go:1360
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_LookupMissIsAnError；位置=snapshot_store_integration_test.go:1398
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_BatchDedupsAgainstStoredAndWithinBatch；位置=snapshot_store_integration_test.go:1452
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_BatchReplayIsNoop；位置=snapshot_store_integration_test.go:1484
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_BatchIdsFollowGuidOrder；位置=snapshot_store_integration_test.go:1519
- 04-mysql-data-service | data_service/internal/store | TestSnapshotTablesHaveNoAutoIncrement；位置=snapshot_store_integration_test.go:1550
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_RowIDCollisionIsNotSwallowed；位置=snapshot_store_integration_test.go:1568
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_BatchFallsBackWhenUniqueKeyAbsent；位置=snapshot_store_integration_test.go:1595
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_OverlappingBatchesFromTwoWritersLandOneRowEach；位置=snapshot_store_integration_test.go:1629
- 04-mysql-data-service | data_service/internal/store | TestSnapshotStore_BatchOfLargeBlobsFitsPacket；位置=snapshot_store_integration_test.go:1679
- 04-mysql-data-service | data_service/internal/store | TestTransactionLogStore_InsertIgnoreReplayIsNoop；位置=transaction_log_store_integration_test.go:55
- 04-mysql-data-service | data_service/internal/store | TestTransactionLogStore_TimestampSecAndZoneLand；位置=transaction_log_store_integration_test.go:92
- 04-mysql-data-service | data_service/internal/kafka | TestSnapshotConsumer_RealStore_DedupsBySnapshotGuidAndStaysInvisibleToGm；位置=consumer_integration_test.go:73
- 04-mysql-data-service | data_service/internal/kafka | TestTxLogConsumer_RealStore_ReplayAfterCommitIsNoop；位置=consumer_integration_test.go:136
- 04-mysql-data-service | data_service/internal/kafka | TestSnapshotConsumer_RealStore_BatchModeDedupsAndCommits；位置=consumer_integration_test.go:181
- 04-tidb-data-service | data_service/internal/kafka | TestSnapshotConsumer_RealStore_DedupsBySnapshotGuidAndStaysInvisibleToGm；位置=consumer_integration_test.go:73
- 04-tidb-data-service | data_service/internal/kafka | TestTxLogConsumer_RealStore_ReplayAfterCommitIsNoop；位置=consumer_integration_test.go:136
- 04-tidb-data-service | data_service/internal/kafka | TestSnapshotConsumer_RealStore_BatchModeDedupsAndCommits；位置=consumer_integration_test.go:181

错误组 4（37 个失败事件，原文变体=1）
断言原文样例：
权威SQL启动失败: 权威SQL物理会话或boot身份核验失败
用例：
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxEnqueueSharesFenceLockWithFreeze；位置=role_authority_integration_test.go:43
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxPermanentRequestCannotRebindAfterTransfer；位置=role_authority_integration_test.go:102
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxRequiresExactRegisteredOpenAdmission；位置=role_authority_integration_test.go:136
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxAdmissionOfOldRevisionRejectedInTx；位置=role_authority_integration_test.go:164
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxEveryMutationHonorsFrozenFence；位置=role_authority_integration_test.go:175
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxNotifyBookkeepingSkipsRoleFence；位置=role_authority_integration_test.go:215
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxFrozenFenceRejectsEvenEmptyFetch；位置=role_authority_integration_test.go:257
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxLostPushHealsOnNextAccess；位置=role_authority_integration_test.go:270
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxRoleMissingFromDirectoryIsRejected；位置=role_authority_integration_test.go:290
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxPendingItemStatesBlockFreeze；位置=role_authority_integration_test.go:348
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxKnownItemTerminalsAllowFreeze；位置=role_authority_integration_test.go:361
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationInboxNullItemStateRemainsBlocking；位置=role_authority_integration_test.go:371
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationBattleSettlementKeepsPermanentOriginalUntilApplied；位置=store_integration_test.go:227
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationEnqueueCreateDuplicateConflict；位置=store_integration_test.go:511
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationEnqueueRejectionsWriteNothing；位置=store_integration_test.go:554
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationEnqueueFullKeepsDuplicates；位置=store_integration_test.go:584
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationRaceRetryGetsDuplicateNotFull；位置=store_integration_test.go:603
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationConcurrentEnqueueSeqContiguous；位置=store_integration_test.go:645
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationFetchNoStreamAndFastPathWritesNothing；位置=store_integration_test.go:678
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationFetchLockedPathDeliversAndFences；位置=store_integration_test.go:701
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationFetchFenceRejections；位置=store_integration_test.go:733
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationFetchCursorAheadRaisesNextSeq；位置=store_integration_test.go:768
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationAckResults；位置=store_integration_test.go:789
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationAckStaleAfterAdvance；位置=store_integration_test.go:850
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationAckRejectsUnregisteredAdmissionDespiteRedisFailure；位置=store_integration_test.go:874
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationCountScannedRows；位置=store_integration_test.go:906
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationVoid；位置=store_integration_test.go:937
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationAuditRecordsWhatWasDone；位置=store_integration_test.go:1035
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationForceVoid；位置=store_integration_test.go:1120
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationSweepPreservesPermanentRequestRows；位置=store_integration_test.go:1250
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationNotifyCASSameMillisecond；位置=store_integration_test.go:1287
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationScanGauges；位置=store_integration_test.go:1321
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationNotifyResendBackoff；位置=store_integration_test.go:1399
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationSummaryListLookup；位置=store_integration_test.go:1492
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationReconcile；位置=store_integration_test.go:1553
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationRecentlyFinalizedOrder；位置=store_integration_test.go:1617
- 04-mysql-inbox | player_inbox/internal/inbox | TestIntegrationCheckRowShards；位置=store_integration_test.go:1688

错误组 5（1 个失败事件，原文变体=1）
断言原文样例：

Error:      	Received unexpected error:
route store on account_directory_k4_e121e296d06ab465: directory shard account_directory_k4_e121e296d06ab465_00: player_route: CHECK constraint chk_route_shard is not enforced (player_id uniqueness depends on it; needs MySQL >= 8.0.16, or TiDB >= 7.2 with `SET GLOBAL tidb_enable_check_constraint=ON` before the table is created): read chk_route_shard metadata: 权威SQL结果未定（业务发送后），禁止重放
directory shard account_directory_k4_e121e296d06ab465_01: player_route: CHECK constraint chk_route_shard is not enforced (player_id uniqueness depends on it; needs MySQL >= 8.0.16, or TiDB >= 7.2 with `SET GLOBAL tidb_enable_check_constraint=ON` before the table is created): read chk_route_shard metadata: 权威SQL结果未定（业务发送后），禁止重放
directory shard account_directory_k4_e121e296d06ab465_02: player_route: CHECK constraint chk_route_shard is not enforced (player_id uniqueness depends on it; needs MySQL >= 8.0.16, or TiDB >= 7.2 with `SET GLOBAL tidb_enable_check_constraint=ON` before the table is created): read chk_route_shard metadata: 权威SQL结果未定（业务发送后），禁止重放
directory shard account_directory_k4_e121e296d06ab465_03: player_route: CHECK constraint chk_route_shard is not enforced (player_id uniqueness depends on it; needs MySQL >= 8.0.16, or TiDB >= 7.2 with `SET GLOBAL tidb_enable_check_constraint=ON` before the table is created): read chk_route_shard metadata: 权威SQL结果未定（业务发送后），禁止重放
用例：
- 04-tidb-data-directory | data_service/internal/store | TestTiDBShardedRouteStore；位置=account_directory_shard_tidb_test.go:482

错误组 6（1 个失败事件，原文变体=1）
断言原文样例：

Error:      	Received unexpected error:
player_route: CHECK constraint chk_route_shard is not enforced (player_id uniqueness depends on it; needs MySQL >= 8.0.16, or TiDB >= 7.2 with `SET GLOBAL tidb_enable_check_constraint=ON` before the table is created): TiDB CHECK switch is disabled
用例：
- 04-tidb-data-directory | data_service/internal/store | TestTiDBMigrateSchemaBuildsEveryShardAndLedger；位置=account_directory_shard_tidb_test.go:588

错误组 7（1 个失败事件，原文变体=1）
断言原文样例：

Error:      	Not equal:
expected: []uint8([]byte{0x98, 0x6, 0x7})
actual  : protoreflect.RawFields(protoreflect.RawFields{0x98, 0x6, 0x7})
用例：
- 04-tidb-data-directory | data_service/internal/store | TestTiDBAccountDirectoryCapReplayRepairAndRemoval；位置=account_directory_tidb_test.go:181

错误组 8（1 个失败事件，原文变体=1）
断言原文样例：

Error:      	Received unexpected error:
directory shard account_directory_k4_97338a39690d1e54_00: list columns of account_directory: context deadline exceeded
directory shard account_directory_k4_97338a39690d1e54_01: list primary key columns of account_directory: context deadline exceeded
directory shard account_directory_k4_97338a39690d1e54_02: list columns of account_directory: context deadline exceeded
directory shard account_directory_k4_97338a39690d1e54_03: list columns of account_directory: context deadline exceeded
用例：
- 04-tidb-data-service | data_service/internal/store | TestTiDBShardedCreateAndReadAcrossShards；位置=account_directory_shard_tidb_test.go:193

错误组 9（1 个失败事件，原文变体=1）
断言原文样例：

Error:      	Received unexpected error:
SQL writer启动grant缺失、漂移或已消费
用例：
- 04-tidb-chat | chat/internal/authority | TestChatTiDBLeastPrivilegeRuntimeContract；位置=least_privilege_tidb_test.go:58

错误组 10（28 个失败事件，原文变体=1）
断言原文样例：

Error:      	Received unexpected error:
权威SQL结果未定（业务发送后），禁止重放
用例：
- 04-tidb-chat | chat/internal/authority | TestChatTiDBConcurrentSameOriginalCommitsOnce；位置=store_tidb_test.go:159
- 04-tidb-match | match/internal/team | TestTeamTiDBSceneMembershipRequiresRegisteredAdmissionAndSignedReply；位置=authority_server_integration_test.go:39
- 04-tidb-match | match/internal/team | TestTeamTiDBOriginalUUIDReplayAfterLeaveAndRestart；位置=sql_store_integration_test.go:151
- 04-tidb-match | match/internal/team | TestTeamTiDBOldInvitationCannotAuthorizeNewOwner；位置=sql_store_integration_test.go:183
- 04-tidb-match | match/internal/team | TestTeamTiDBConcurrentAcceptHasOneMembership；位置=sql_store_integration_test.go:205
- 04-tidb-match | match/internal/team | TestTeamTiDBExpiredRequestDeadlineWritesNothing；位置=sql_store_integration_test.go:259
- 04-tidb-match | match/internal/team | TestTeamTiDBCheckParty；位置=sql_store_integration_test.go:273
- 04-tidb-match | match/internal/team | TestTeamTiDBFirstFullLaunchAndPendingSoloUseTheSamePCommitFence/solo；位置=sql_store_integration_test.go:360
- 04-tidb-match | match/internal/team | TestTeamTiDBFirstFullLaunchAndPendingSoloUseTheSamePCommitFence/team；位置=sql_store_integration_test.go:360
- 04-tidb-match | match/internal/team | TestTeamTiDBWholeUnitsCrossWaitUnknownAndRecovery；位置=sql_store_integration_test.go:405
- 04-tidb-match | match/internal/team | TestTeamTiDBFinalHomeFiltersWholeUnitsBeforeLimit；位置=sql_store_integration_test.go:450
- 04-tidb-match | match/internal/team | TestTeamTiDBFinalHomeFiltersSoloPeersBeforeLimit；位置=sql_store_integration_test.go:499
- 04-tidb-match | match/internal/team | TestTeamTiDBFailedFormationRequeuesWholeOriginalUnit；位置=sql_store_integration_test.go:535
- 04-tidb-match | match/internal/team | TestTeamTiDBHomeZoneResolvesMergedZone；位置=sql_store_integration_test.go:613
- 04-tidb-rank-mail-chain | match/internal/authority | TestTiDBRatingRankMailChainRecoversAfterCommittedReplyLoss；位置=rank_mail_chain_integration_test.go:68
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialStartedBattleReleasesOnlyOnSignedSQLAbort/帮会在；位置=trial_launch_abort_tidb_test.go:76
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialStartedBattleReleasesOnlyOnSignedSQLAbort/帮会已解散；位置=trial_launch_abort_tidb_test.go:76
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialSQLLobbyRedisLossPendingAndOriginalOwner；位置=trial_lobby_sql_test.go:54
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialSQLLastConsentAndUnknownLaunchNeverTTLRelease；位置=trial_lobby_sql_test.go:89
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialCancelBeforePrepareSurvivesCrashAndRejectsLateLaunch；位置=trial_lobby_sql_test.go:126
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialExpiredUnpreparedCleanupDoesNotRegrantChangedOwner；位置=trial_lobby_sql_test.go:159
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialPrepareReverseGuardAfterConsents；位置=trial_lobby_sql_test.go:205
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialDisbandRetainsUntilBoundTerminalAndOldResultCannotClearNewLobby/settle；位置=trial_lobby_sql_test.go:274
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialDisbandRetainsUntilBoundTerminalAndOldResultCannotClearNewLobby/poison；位置=trial_lobby_sql_test.go:274
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialMatchWinsBeforePrivateCreateAndNoPartialConsent；位置=trial_lobby_sql_test.go:345
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialOccupancyIsReleasedWhenLobbyIsNotCreatedOrEnds；位置=trial_lobby_sql_test.go:390
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialOccupancyIsReleasedWhenAcquireOutcomeIsUnknown；位置=trial_lobby_sql_test.go:464
- 04-tidb-guild-trial | guild/internal/data | TestTiDBTrialOccupancyIsReleasedWhenRenewalLosesToLobbyEnd；位置=trial_lobby_sql_test.go:481

错误组 11（1 个失败事件，原文变体=1）
断言原文样例：

Error:      	Received unexpected error:
权威SQL结果未定（事务结束），禁止重放
用例：
- 04-tidb-match | match/internal/authority | TestTerminalTiDBCancelledFormationDistinguishesUnissuedAndPersistedCreate/新协议原件；位置=completion_test.go:230

错误组 12（20 个失败事件，原文变体=1）
断言原文样例：
权威SQL结果未定（业务发送后），禁止重放
用例：
- 04-tidb-rank | rank/internal/data | TestTiDBRankMetricsRequireCommitAndRetainDueDelayOnReadFailure；位置=repo_integration_test.go:27
- 04-tidb-rank | rank/internal/data | TestTiDBRemovedEntryStaysRemovedAgainstLateBatch；位置=repo_integration_test.go:27
- 04-tidb-rank | rank/internal/data | TestTiDBOrderedScoresFreezeAndReliableRewardIntents；位置=repo_integration_test.go:27
- 04-tidb-rank | rank/internal/data | TestTiDBVersionedAchievedMsComesFromProducer；位置=repo_integration_test.go:27
- 04-tidb-rank | rank/internal/data | TestTiDBShardedSourceCursorsAndVersionsSurviveOutOfOrderDelivery；位置=repo_integration_test.go:27
- 04-tidb-rank | rank/internal/data | TestTiDBFreezeFailureRollsBackEverySnapshotAndIntent；位置=repo_integration_test.go:27
- 04-tidb-rank | rank/internal/data | TestTiDBRankIndependentPoolsReplayConflictFreezeAndRewardFence；位置=repo_integration_test.go:362
- 04-tidb-mail | mail | TestTiDBRealRPCAuthSendOfflineReadClaimAndDeleteProtection；位置=flow_integration_test.go:100
- 04-tidb-mail | mail/internal/data | TestTiDBMailOldSessionCannotReadOrMutateTransferredOrAbaRole；位置=repo_integration_test.go:194
- 04-tidb-mail | mail/internal/data | TestTiDBMailChecksOriginalSessionBeforePendingClaimReplay；位置=repo_integration_test.go:243
- 04-tidb-mail | mail/internal/data | TestTiDBUnknownClaimStateNeverDeletesEvenAfterExpiry；位置=repo_integration_test.go:269
- 04-tidb-mail | mail/internal/data | TestTiDBConcurrentClaimRequiresSQLCommitAndProtectsPending；位置=repo_integration_test.go:290
- 04-tidb-mail | mail/internal/data | TestTiDBConcurrentSystemSendIsDecidedByUniqueKey；位置=repo_integration_test.go:376
- 04-tidb-mail | mail/internal/data | TestTiDBConcurrentCampaignCreateIsDecidedByUniqueKey；位置=repo_integration_test.go:428
- 04-tidb-mail | mail/internal/data | TestTiDBManifestSealFreezesBroadcastRecipients；位置=repo_integration_test.go:479
- 04-tidb-mail | mail/internal/data | TestTiDBGuildTextFollowsCurrentGuildAndFrozenRewardsSurviveLeave；位置=repo_integration_test.go:533
- 04-tidb-mail | mail/internal/data | TestTiDBMailNewOwnerHealsStaleFrozenReplicaOldOwnerRejected；位置=role_fence_tidb_test.go:35
- 04-tidb-mail | mail/internal/data | TestTiDBMailFrozenRoleRejectsClaimWithoutSideEffects；位置=role_fence_tidb_test.go:58
- 04-tidb-mail | mail/internal/data | TestTiDBMailActorMissingFromDirectoryIsDenied；位置=role_fence_tidb_test.go:86
- 04-tidb-mail | mail/internal/data | TestTiDBGuildLookupFailureOnlyHidesGuildText；位置=role_fence_tidb_test.go:95

父测试随子测试失败且无独立断言=15（不重复子断言）
04-mysql-data-service | data_service/internal/store | TestTiDBRoleBeginDryRunRejectionWritesNothing
04-mysql-data-service | data_service/internal/store | TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor
04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseRacesTwoReservesConverges/未提交的Release正删被抢的名字
04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseRacesTwoReservesConverges/被抢名字已释放且未purge_未提交的Release正删后继名字
04-mysql-data-service | data_service/internal/store | TestPlayerName_ReleaseRacesTwoReservesConverges
04-mysql-data-service | data_service/internal/store | TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks
04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveBehindPendingReleaseOutcome/未提交的Release正删被抢的名字
04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveBehindPendingReleaseOutcome/被抢名字已释放且未purge_未提交的Release正删后继名字
04-mysql-data-service | data_service/internal/store | TestPlayerName_ReserveBehindPendingReleaseOutcome
04-mysql-data-service | data_service/internal/store | TestPlayerName_StoreRefusesUnknownPrimaryKey
04-mysql-data-service | data_service/internal/store | TestRouteSchema_ShapeGuardRejectsTamperedTable
04-tidb-match | match/internal/authority | TestTerminalTiDBCancelledFormationDistinguishesUnissuedAndPersistedCreate
04-tidb-match | match/internal/team | TestTeamTiDBFirstFullLaunchAndPendingSoloUseTheSamePCommitFence
04-tidb-guild-trial | guild/internal/data | TestTiDBTrialStartedBattleReleasesOnlyOnSignedSQLAbort
04-tidb-guild-trial | guild/internal/data | TestTiDBTrialDisbandRetainsUntilBoundTerminalAndOldResultCannotClearNewLobby

已 RUN 但没有终态=6；不能从退出 1 推断整包跑完。
04-tidb-data-directory | data_service/internal/store | TestTiDBAccountDirectoryReadTakesNoLockOnHit
04-tidb-data-service | data_service/internal/store | TestTiDBShardedRouteStore
04-tidb-match | match/internal/authority | TestDirectTiDBFixedPendingHighWaterDoesNotIncludeLaterInputs
04-tidb-match | match/internal/team | TestTeamTiDBRosterZoneCheckResolvesMergedZones
04-tidb-mail | mail/internal/data | TestTiDBMailPendingAssetOpStatesBlockFreeze
04-tidb-guild-trial | guild/internal/data | TestTiDBTrialOppositeRosterConcurrentUsesUnsignedP

超时：04-tidb-data-directory；Package=data_service/internal/store
panic: test timed out after 10m0s
该包最后 RUN（截至 panic）：data_service/internal/store | TestTiDBAccountDirectoryReadTakesNoLockOnHit
该包已 RUN 无终态：TestTiDBAccountDirectoryReadTakesNoLockOnHit
完整堆栈：D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2\04-tidb-data-directory.log

超时：04-tidb-data-service；Package=data_service/internal/store
panic: test timed out after 10m0s
该包最后 RUN（截至 panic）：data_service/internal/store | TestTiDBShardedRouteStore
该包已 RUN 无终态：TestTiDBShardedRouteStore
完整堆栈：D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2\04-tidb-data-service.log

超时：04-tidb-match；Package=match/internal/authority
panic: test timed out after 10m0s
该包最后 RUN（截至 panic）：match/internal/authority | TestDirectTiDBFixedPendingHighWaterDoesNotIncludeLaterInputs
该包已 RUN 无终态：TestDirectTiDBFixedPendingHighWaterDoesNotIncludeLaterInputs
完整堆栈：D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2\04-tidb-match.log

超时：04-tidb-match；Package=match/internal/team
panic: test timed out after 10m0s
该包最后 RUN（截至 panic）：match/internal/team | TestTeamTiDBRosterZoneCheckResolvesMergedZones
该包已 RUN 无终态：TestTeamTiDBRosterZoneCheckResolvesMergedZones
完整堆栈：D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2\04-tidb-match.log

超时：04-tidb-mail；Package=mail/internal/data
panic: test timed out after 10m0s
该包最后 RUN（截至 panic）：mail/internal/data | TestTiDBMailPendingAssetOpStatesBlockFreeze
该包已 RUN 无终态：TestTiDBMailPendingAssetOpStatesBlockFreeze
完整堆栈：D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2\04-tidb-mail.log

超时：04-tidb-guild-trial；Package=guild/internal/data
panic: test timed out after 10m0s
该包最后 RUN（截至 panic）：guild/internal/data | TestTiDBTrialOppositeRosterConcurrentUsesUnsignedP
该包已 RUN 无终态：TestTiDBTrialOppositeRosterConcurrentUsesUnsignedP
完整堆栈：D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2\04-tidb-guild-trial.log

普通文本失败输出：04-mysql-init
ERROR 2002 (HY000): Can't connect to local MySQL server through socket '/var/run/mysqld/mysqld.sock' (2)

普通文本失败输出：04-tidb-init
ERROR 2003 (HY000): Can't connect to MySQL server on 'player-store-r2-tidb:4000' (111)
```

完整失败原文：[aggregate-04-failed-output.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-04-failed-output.jsonl)；全部分组/用例：[aggregate-04-analysis.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-04-analysis.txt)；SKIP均没跑，见[aggregate-04-skipped.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-04-skipped.txt)。

| ID | 退出码 | 工作目录（相对执行根） | 已执行命令模板 | 日志文件（相对日志根） |
|---|---:|---|---|---|
| `04-network-create` | 0 | `.` | `docker --context desktop-linux network create player-store-r2-net` | `04-network-create.log` |
| `04-mysql-start` | 0 | `.` | `docker --context desktop-linux run -d --name player-store-r2-mysql --network player-store-r2-net --memory 1g -p 127.0.0.1:13306:3306 -e MYSQL_ALLOW_EMPTY_PASSWORD=yes -e MYSQL_ROOT_HOST=% mysql:latest` | `04-mysql-start.log` |
| `04-redis72-start` | 0 | `.` | `docker --context desktop-linux run -d --name player-store-r2-redis72 --memory 256m -p 127.0.0.1:16379:6379 redis:7.2 redis-server --save '' --appendonly no` | `04-redis72-start.log` |
| `04-redis8-start` | 0 | `.` | `docker --context desktop-linux run -d --name player-store-r2-redis8 --memory 256m -p 127.0.0.1:16380:6379 redis:latest redis-server --save '' --appendonly no` | `04-redis8-start.log` |
| `04-pd-start` | 0 | `.` | `docker --context desktop-linux run -d --name player-store-r2-pd --network player-store-r2-net --memory 2g pingcap/pd:v8.5.2 --name=player-store-r2-pd --client-urls=http://0.0.0.0:2379 --peer-urls=http://0.0.0.0:2380 --advertise-client-urls=http://player-store-r2-pd:2379 --advertise-peer-urls=http://player-store-r2-pd:2380 --initial-cluster=player-store-r2-pd=http://player-store-r2-pd:2380` | `04-pd-start.log` |
| `04-tikv-start` | 0 | `.` | `docker --context desktop-linux run -d --name player-store-r2-tikv --network player-store-r2-net --memory 2g pingcap/tikv:v8.5.2 --addr=0.0.0.0:20160 --advertise-addr=player-store-r2-tikv:20160 --pd=player-store-r2-pd:2379 --data-dir=/tmp/tikv` | `04-tikv-start.log` |
| `04-tidb-start` | 0 | `.` | `docker --context desktop-linux run -d --name player-store-r2-tidb --network player-store-r2-net --memory 2g -p 127.0.0.1:14000:4000 pingcap/tidb:v8.5.2 --store=tikv --path=player-store-r2-pd:2379 --host=0.0.0.0 --advertise-address=player-store-r2-tidb` | `04-tidb-start.log` |
| `04-mysql-init` | 1 | `.` | `docker --context desktop-linux exec player-store-r2-mysql mysql -uroot -e 'SELECT VERSION(); CREATE DATABASE guild_test; CREATE DATABASE trade_it; CREATE DATABASE player_inbox_it; CREATE DATABASE friend_it; CREATE DATABASE gateway_test;'` | `04-mysql-init.log` |
| `04-tidb-init` | 1 | `.` | `docker --context desktop-linux exec player-store-r2-mysql mysql -hplayer-store-r2-tidb -P4000 -uroot -e 'SET GLOBAL tidb_enable_check_constraint=ON; SELECT VERSION(); CREATE DATABASE guild_test; CREATE DATABASE trade_it; CREATE DATABASE player_inbox_it; CREATE DATABASE friend_it;'` | `04-tidb-init.log` |
| `04-redis72-info` | 0 | `.` | `docker --context desktop-linux exec player-store-r2-redis72 redis-cli INFO server` | `04-redis72-info.log` |
| `04-redis8-info` | 0 | `.` | `docker --context desktop-linux exec player-store-r2-redis8 redis-cli INFO server` | `04-redis8-info.log` |
| `04-mysql-startup-log` | 0 | `.` | `docker --context desktop-linux logs player-store-r2-mysql` | `04-mysql-startup-log.log` |
| `04-pd-startup-log` | 0 | `.` | `docker --context desktop-linux logs player-store-r2-pd` | `04-pd-startup-log.log` |
| `04-tikv-startup-log` | 0 | `.` | `docker --context desktop-linux logs player-store-r2-tikv` | `04-tikv-startup-log.log` |
| `04-tidb-startup-log` | 0 | `.` | `docker --context desktop-linux logs player-store-r2-tidb` | `04-tidb-startup-log.log` |
| `04-rank-chain-build` | 0 | `go/rank` | `go test -gcflags=-e -c -o D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2/bin/rank_chain.test.exe .` | `04-rank-chain-build.log` |
| `04-mail-chain-build` | 0 | `go/mail` | `go test -gcflags=-e -c -o D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2/bin/mail_chain.test.exe .` | `04-mail-chain-build.log` |
| `04-mysql-playerstore` | 0 | `go/db` | `go test -gcflags=-e -json -count=1 -p=1 -tags=integration ./internal/playerstore/...` | `04-mysql-playerstore.log` |
| `04-mysql-data-service` | 1 | `go/data_service` | `go test -gcflags=-e -json -count=1 -p=1 -tags=integration ./internal/store/... ./internal/logic/... ./internal/kafka/...` | `04-mysql-data-service.log` |
| `04-mysql-guild-economy` | 0 | `go/guild` | `go test -gcflags=-e -json -count=1 -p=1 -tags=integration ./internal/logic -run EconomyFlow` | `04-mysql-guild-economy.log` |
| `04-mysql-inbox` | 1 | `go/player_inbox` | `go test -gcflags=-e -json -count=1 -p=1 -tags=integration ./internal/inbox/...` | `04-mysql-inbox.log` |
| `04-tidb-playerstore` | 0 | `go/db` | `go test -gcflags=-e -json -count=1 -p=1 -tags=integration ./internal/playerstore/...` | `04-tidb-playerstore.log` |
| `04-tidb-data-directory` | 1 | `go/data_service` | `go test -gcflags=-e -json -count=1 -p=1 ./internal/store/... -run TestTiDB` | `04-tidb-data-directory.log` |
| `04-tidb-data-service` | 1 | `go/data_service` | `go test -gcflags=-e -json -count=1 -p=1 -tags=integration ./internal/store/... ./internal/logic/... ./internal/kafka/...` | `04-tidb-data-service.log` |
| `04-tidb-chat` | 1 | `go/chat` | `go test -gcflags=-e -json -count=1 -p=1 ./internal/authority -run TestChatTiDB` | `04-tidb-chat.log` |
| `04-tidb-match` | 1 | `go/match` | `go test -gcflags=-e -json -count=1 -p=1 ./internal/authority ./internal/team/...` | `04-tidb-match.log` |
| `04-tidb-rank-mail-chain` | 1 | `go/match` | `go test -gcflags=-e -json -count=1 -p=1 ./internal/authority -run TestTiDBRatingRankMailChainRecoversAfterCommittedReplyLoss` | `04-tidb-rank-mail-chain.log` |
| `04-tidb-rank` | 1 | `go/rank` | `go test -gcflags=-e -json -count=1 -p=1 ./... -run TestTiDB` | `04-tidb-rank.log` |
| `04-tidb-mail` | 1 | `go/mail` | `go test -gcflags=-e -json -count=1 -p=1 ./... -run TestTiDB` | `04-tidb-mail.log` |
| `04-tidb-guild-trial` | 1 | `go/guild` | `go test -gcflags=-e -json -count=1 -p=1 ./internal/data -run TestTiDBTrial` | `04-tidb-guild-trial.log` |

## 5 merge_zone:失败

普通单测已在§3本轮跑过。merge_integration两后端首次命令：MySQL exit0，TiDB exit1；JSON含子测试141 PASS/7 FAIL/0 SKIP，顶层43 PASS/7 FAIL/0 SKIP。TiDB七例均在夹具连接阶段报`context deadline exceeded`：

```text
TestIT_MergeWritesOneRowWithAuditColumns (merge_integration_test.go:232)
TestIT_MergeIsIdempotent (:251)
TestIT_MergeRefusals (:277)
TestIT_FrozenCatalogRefusesWrites (:313)
TestIT_Unmerge (:332)
TestIT_DryRunAndAuditWithReaderPermission (:370)
TestIT_ConcurrentMergesSerializeOnRouteMeta (:399)
```

在线双区合服、并发业务、在途结算、撤销、崩溃恢复等场景表没跑：§3未全绿且完整节点栈未建立，工具真库不替代在线场景。见[05-not-run.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/05-not-run.txt)。

完整失败原文：[aggregate-05-failed-output.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-05-failed-output.jsonl)；全部分组：[aggregate-05-analysis.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-05-analysis.txt)。

| ID | 退出码 | 工作目录（相对执行根） | 已执行命令模板 | 日志文件（相对日志根） |
|---|---:|---|---|---|
| `05-merge-mysql` | 0 | `tools/merge_zone` | `go test -gcflags=-e -json -count=1 -p=1 -timeout=60m -tags=merge_integration ./...` | `05-merge-mysql.log` |
| `05-merge-tidb` | 1 | `tools/merge_zone` | `go test -gcflags=-e -json -count=1 -p=1 -timeout=60m -tags=merge_integration ./...` | `05-merge-tidb.log` |

## 6 C++:失败

Windows整解串行构建exit1，耗时24m13s，stdout末尾显示10个警告、4个错误。全部四个不同错误：

追加纯构建修复：两处MSBuild属性函数恢复分号列表后，`06-msbuild-after-buildfix`串行整解实际exit1，原3个LNK1104全部消失；battle节点、turn_battle_engine_test、client_endpoint_test实际产出。唯一去重错误仍为scene的RecallPick::bag裸指针门禁，留Claude处理，没有放宽检查：

```text
asset_op_system.cpp:882:2: RecallPick 内 Bag* bag;，1 match
Directory.Build.targets(56,5): error MSB3073: NoRawPointerMemberCheck 已退出，代码为1 [scene.vcxproj]
```

新整解 [06-msbuild-after-buildfix.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-msbuild-after-buildfix.log)；errorsonly前50行及中文stdout原文 [build-fix-msbuild-errors-first50.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/build-fix-msbuild-errors-first50.txt)；原errorsonly [06-msbuild-after-buildfix-errors.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-msbuild-after-buildfix-errors.log)。`06-turn-battle-after-buildfix`首次实际运行exit0、300/300 PASS，目标依赖不含scene.lib；日志 [06-turn-battle-after-buildfix.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-turn-battle-after-buildfix.log) / [stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-turn-battle-after-buildfix/turn_battle_engine_test.stdout.log)，新产物mtime/SHA [06-turn-battle-buildfix-evidence.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-turn-battle-buildfix-evidence.json)。新事实不覆盖首次4错，也不宣布scene或整解通过。

```text
1. scene共享库NoRawPointerMemberCheck：asset_op_system.cpp:882:2，RecallPick::bag为Bag* bag;，1 match，随后Directory.Build.targets(56,5) MSB3073/exit1。
2. cpp/nodes/battle/battle.vcxproj：LINK : fatal error LNK1104: 无法打开文件“.obj”
3. cpp/tests/turn_battle_engine_test/turn_battle_engine_test.vcxproj：LINK : fatal error LNK1104: 无法打开文件“.obj”
4. cpp/tests/client_endpoint_test/client_endpoint_test.vcxproj：LINK : fatal error LNK1104: 无法打开文件“.obj”
```

errorsonly共4行，UTF-8转录保留全部，少于50行：[06-errors-first50.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-errors-first50.txt)；原生CP936：[06-msbuild-errors.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-msbuild-errors.log)；全部link命令上下文：[06-link-command-context.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-link-command-context.txt)；最终错误摘要：[06-cpp-error-summary.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-cpp-error-summary.txt)。没有运行旧exe/旧失败lib来宣称当前源码通过。

首次构建后上述8组测试均未执行。追加修复后turn_battle已首次实际300/300 PASS；目前仍没跑的7个scene依赖目标是cross_zone、currency、bag、kafka_command、rpc_controller、player_landing、aoi。battle_observer_contract_test编入rpc_controller，无独立目标，同样没跑。scene依赖目标的真实Redis/Cluster回归没跑；独立Hiredis结果另见§15。完整依赖映射：[06-test-dependencies.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-test-dependencies.txt)，没跑清单：[06-not-run.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-not-run.txt)。muduo_grpc独立目标若在§15首次执行，按后续节实际结果记载，不能补称本节整解全绿。

Linux镜像 `06-linux-docker-build` 已exit0，2026-11-02T04:03:53.9585875Z至05:47:35.7131444Z；`docker build --progress=plain --build-arg BUILD_JOBS=1 -f deploy/k8s/Dockerfile.cpp -t local/player-store-verify-r2 .`。完整日志 [06-linux-docker-build.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/06-linux-docker-build.log)。这只证明该次Docker上下文构建，不替代Windows整解、进程启动或全链路业务。

| ID | 退出码 | 工作目录（相对执行根） | 已执行命令模板 | 日志文件（相对日志根） |
|---|---:|---|---|---|
| `06-msbuild-game` | 1 | `.` | `msbuild game.sln /m:1 /nr:false /p:CL_MPCount=1 /p:Configuration=Debug /p:Platform=x64 '/flp1:logfile=D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2/06-msbuild-errors.log;errorsonly'` | `06-msbuild-game.log` |

## 7 部署合同:失败

完成43条命令，9条非零：8份合同脚本失败，另1条docs-index失败。共44个具名FAIL，另2份脚本在未命名前置检查中断。夹具/DryRun通过不等于真实部署通过。裸指针no_raw_pointer_check实际checker检查没跑（缺checker/LLVM，未安装升级）；no_raw_pointer_project已按MSBuildPath/EvidenceDir带参执行，但前置失败。

独立错误原文摘要：

```text
gateway env注入：服务 gateway 的 manifest 应当恰好有 1 个 env: 段(实际 0 个),无法注入 ADMIN_APIKEY。（k8s_deploy.ps1:11374）
Agones：工作负载必须有唯一普通containers段；init不参与runtime密钥注入
Kafka拓扑：全球匹配结果提交要求Kafka至少3副本、min.insync.replicas至少2；不能以单broker声明耐久提交。（k8s_deploy.ps1:8907）
Redis queue：断言失败: 有口令 / 无口令两条 exec 都要带 --enable-debug-command no (期望 '2',实际 '3')
logical DB：Cannot bind argument to parameter 'Because' because it is an empty string.
裸指针项目夹具：成功缓存与检查结果不一致。（no_raw_pointer_project.tests.ps1:36）
publish_images：Argument types do not match（publish_images.ps1:636）；缺夹具sha256sums.txt；另负例预期版本冲突但实际No such image。
TiDB catalog：断言失败: 没有列级锁写 (不该匹配 /lock_touch/,但匹配到了 'lock_touch')
docs-index：docs/README.md索引过期（原日志中文乱码）；交接注明该项既有基线红，本轮只--check，未重生成。
```

全部44个具名失败索引（同一用例只列一次，stdout行号在各原日志可定位，完整断言在失败JSONL中）：

- `07-k8s_client_entry_contract` 第65行：端到端 podip 默认(gate 2 副本):gate 仍是 Deployment,不带任何集群外入口 env、不占 hostPort,也没有 StatefulSet 一族
- `07-k8s_client_entry_contract` 第2586行：端到端 external + NodePort 2 副本:gate StatefulSet + gate-0 / gate-1 + headless + PDB,不生成 Deployment gate 与 gate-entry
- `07-k8s_client_entry_contract` 第5213行：端到端 external 警告:nodePort 段出推荐子段、部署 gateway 却没有 Ingress —— 只警告不拒绝,且端口原样生效
- `07-k8s_client_entry_contract` 第2583行：端到端 podip:login / scene_manager 的 RequireClientEndpoint = false,键名与服务自己的 etc 一致;不写 DevPasswordAuth,清理口令 Secret
- `07-k8s_client_entry_contract` 第5204行：端到端 external:先删 Deployment 形态(带确认提示)与 gate-entry,再按 headless → 每序号 Service → StatefulSet → PDB 的顺序 apply,就绪等 StatefulSet
- `07-k8s_client_entry_contract` 第5210行：端到端 external:login / scene_manager 的 RequireClientEndpoint = true;gateway 带 Ingress 与 trusted proxies
- `07-k8s_client_entry_contract` 第6126行：端到端 infra-up agones + external:battle 是 Fleet(client 端口 Dynamic、health 抬到 105、agones 来源、不写 PORT),RBAC 先于 Fleet,等 Fleet 就绪
- `07-k8s_client_entry_contract` 第6144行：端到端 infra-up agones + podip:Fleet 只写 CLIENT_ENDPOINT_SOURCE=none
- `07-k8s_client_entry_contract` 第6164行：全部 DryRun 输出:gRPC 口从不进 hostPort / nodePort / Agones ports,不开 hostNetwork;新生成器的工作负载不出现 D92 身份 env
- `07-k8s_redis_split_contract` 第3行：缺省 zone-up DryRun:login 没有 QueueRedis、gateway 指向核心 redis.<infra>,整份输出没有 redis-queue-g 字样
- `07-k8s_redis_split_contract` 第2526行：-RedisSplit queue:login 整段写 QueueRedis(Host = redis-queue-g0,其余与 RedisClient 逐项同值,PoolSize 镜像 login.yaml)
- `07-k8s_redis_split_contract` 第5064行：zones 配置 redisQueueGroup: 2 → login / gateway 都指向 redis-queue-g2
- `07-k8s_redis_split_contract` 第7624行：地址不随形态变:单实例与高可用下 login / gateway 的排队地址、核心地址、Redis split 与 queue 总账都逐字相同
- `07-logical_db_deploy_contract` 第10行：DryRun:data-service 的名册和这次的 K、存档库清单对不上,在 apply 之前拒绝
- `07-logical_db_deploy_contract` 第2532行：dev zone-up:K=1、地域 0,池就是服务 yaml 的值,入口打印本次形态
- `07-logical_db_deploy_contract` 第5054行：prod zone-up K=64:片数、每片池、地域码进 ConfigMap,上限换 TiDB 预算,存档库池不收窄
- `07-logical_db_deploy_contract` 第7672行：staging 可以用小一点的 K,同样进 ConfigMap
- `07-logical_db_deploy_contract` 第13202行：-SqlClusters 进全部写者的 DbPlacement 段
- `07-redis_player_cluster_contract` 第16行：dev 缺省 zone-up:产物没有 redis-player,node-config 没有 player_redis,Go 服务的 PlayerRedis 只写池上限
- `07-redis_player_cluster_contract` 第8936行：-PlayerRedisMasters 3 的 zone-up:node-config 写 player_redis,scene 有口令与配置指纹,gate 没有;zone 的 Go 服务写种子;WAITAOF 取 1
- `07-redis_player_cluster_contract` 第11525行：dev 档集群不注入口令:Go 不写 PasswordEnv,容器没有口令环境变量;集群无口令启动
- `07-redis_player_cluster_contract` 第22275行：负向:prod 开了集群缺 MMORPG_PLAYER_REDIS_PASSWORD,在任何写操作之前拒绝并点名变量;给了就能出产物
- `07-k8s_redis_split_contract` 第5060行：-RedisSplit queue:gateway 的 spring.data.redis.host = redis-queue-g0.<infra>,端口不变
- `07-k8s_redis_split_contract` 第7600行：infra-up -RedisSplit queue:渲染 redis-queue-g0(不带 app: redis、noeviction + maxmemory、MAXCLIENTS 一次、debug 关、exporter、无未替换占位),排在核心之后
- `07-logical_db_deploy_contract` 第10290行：mysql-init:K=1 建基名和流水库,K=64 建全部片库和流水库,基名不再是库
- `07-logical_db_deploy_contract` 第15746行：MMORPG_DB_ALLOWED_DATABASES 在写路径入口核:没给、不合规都在任何清单之前拒绝,dev 也一样
- `07-redis_player_cluster_contract` 第14081行：每主 2 副本:节点数 ×1.5,min-replicas-to-write 取 1
- `07-logical_db_deploy_contract` 第13200行：DbPlacement 段的位置和 Go 的配置结构一致
- `07-logical_db_deploy_contract` 第15728行：MMORPG_DB_PLACEMENT 进写者的 registrar init,只进 init;login 和 gateway 写区域 MySQL 上的 mmorpg,不进
- `07-logical_db_deploy_contract` 第15730行：chat、mail、db 的内部 RPC 配置:chat 和 db 走 etcd 发现目录,mail 直连 guild Service
- `07-logical_db_deploy_contract` 第15741行：写者的控制文件都在 emptyDir 挂载根下的私有子目录(挂载根过不了私有目录校验)
- `07-logical_db_deploy_contract` 第15743行：registrar 带 MMORPG_SQL_BACKEND:login、gateway 固定 mysql,其余跟 -DatabaseBackend,只进 init
- `07-logical_db_deploy_contract` 第19674行：回收 CronJob:zone 和 infra 各一份,先于写者,只有 pods 的三个动词,非 root,只认主容器的退出码 78
- `07-publish_images` 第11行：成功:快照轨全量发布,布局 / 清单 / build-info / 符号 / sha256sums / latest 指针正确
- `07-publish_images` 第15行：-SkipIfExists:同一提交已发布时 exit 0 且不再调用镜像脚本
- `07-publish_images` 第17行：成功:release 轨发布,版本号注入镜像脚本,目录与 tag 按版本号命名
- `07-publish_images` 第21行：构建路径:按 family 顺序先 preflight/构建,再 list-refs,且都带本次 tag
- `07-publish_images` 第13行：负向:同一提交再次发布必须被不可变规则拒绝,已发布内容不变
- `07-publish_images` 第19行：负向:-SkipIfExists 不能掩盖"同一版本号已由其他提交发布"
- `07-redis_player_cluster_contract` 第2538行：dev 缺省 infra-up:不渲染集群;读登录态的全局服务显式指向 login 的实例与 DB,口令走各自原来的路子
- `07-redis_player_cluster_contract` 第5269行：-PlayerRedisMasters 3 的 infra-up:清单、Secret、建群 Job 都渲染,排在全局 Go 服务之前,占位全部替换,口令不进产物
- `07-redis_player_cluster_contract` 第8934行：-PlayerRedisMasters 3 的 infra-up:全局服务的 PlayerRedis、LoginRedis 写种子与 PasswordEnv,容器有口令环境变量
- `07-redis_player_cluster_contract` 第17354行：加主节点不增加每节点连接:-PlayerRedisMasters 3 与 6 的 steady 相同
- `07-sql_boot_catalog_local` 第332行：TiDB 档:SELECT_ONLY、不带 -LocalMySQL、gateway 保留强核验

另两个前置中断：07-k8s_deploy_contract（tests.ps1:95，基线DryRun exit1后后续断言无意义，底层gateway env注入错误）；07-no_raw_pointer_project（tests.ps1:36，成功缓存与检查结果不一致）。07-docs-index没有具名test，单独计命令失败。

完整失败原文：[aggregate-07-failed-output.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-07-failed-output.jsonl)；完整断言/分组：[aggregate-07-analysis.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-07-analysis.txt)；没跑项：[aggregate-07-skipped.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-07-skipped.txt)。

| ID | 退出码 | 工作目录（相对执行根） | 已执行命令模板 | 日志文件（相对日志根） |
|---|---:|---|---|---|
| `07-artifacts_fetch_import_retention` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\artifacts_fetch_import_retention.tests.ps1` | `07-artifacts_fetch_import_retention.log` |
| `07-artifacts_lib` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\artifacts_lib.tests.ps1` | `07-artifacts_lib.log` |
| `07-chat_authority_deploy` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\chat_authority_deploy.tests.ps1` | `07-chat_authority_deploy.log` |
| `07-deploy_capture_contract` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\deploy_capture_contract.tests.ps1` | `07-deploy_capture_contract.log` |
| `07-dev_tools_db_move_contract` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\dev_tools_db_move_contract.tests.ps1` | `07-dev_tools_db_move_contract.log` |
| `07-dev_tools_kafka_offset_reset` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\dev_tools_kafka_offset_reset.tests.ps1` | `07-dev_tools_kafka_offset_reset.log` |
| `07-dev_tools_merge_zone_contract` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\dev_tools_merge_zone_contract.tests.ps1` | `07-dev_tools_merge_zone_contract.log` |
| `07-dev_tools_proto_gen_exit` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\dev_tools_proto_gen_exit.tests.ps1` | `07-dev_tools_proto_gen_exit.log` |
| `07-docker_debug_sql_boot` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\docker_debug_sql_boot.tests.ps1` | `07-docker_debug_sql_boot.log` |
| `07-etcd_maintenance_contract` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\etcd_maintenance_contract.tests.ps1` | `07-etcd_maintenance_contract.log` |
| `07-global_authority_deploy` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\global_authority_deploy.tests.ps1` | `07-global_authority_deploy.log` |
| `07-go_services_local_mysql` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\go_services_local_mysql.tests.ps1` | `07-go_services_local_mysql.log` |
| `07-go_services_ports` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\go_services_ports.tests.ps1` | `07-go_services_ports.log` |
| `07-go_services_stop` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\go_services_stop.tests.ps1` | `07-go_services_stop.log` |
| `07-go_svc_image_authority` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\go_svc_image_authority.tests.ps1` | `07-go_svc_image_authority.log` |
| `07-id_issuer_deploy_contract` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\id_issuer_deploy_contract.tests.ps1` | `07-id_issuer_deploy_contract.log` |
| `07-k8s_client_entry_contract` | 1 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\k8s_client_entry_contract.tests.ps1` | `07-k8s_client_entry_contract.log` |
| `07-k8s_deploy_contract` | 1 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\k8s_deploy_contract.tests.ps1` | `07-k8s_deploy_contract.log` |
| `07-k8s_gate_drain` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\k8s_gate_drain.tests.ps1` | `07-k8s_gate_drain.log` |
| `07-k8s_login_auth_provider_guard` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\k8s_login_auth_provider_guard.tests.ps1` | `07-k8s_login_auth_provider_guard.log` |
| `07-k8s_migrate_gate` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\k8s_migrate_gate.tests.ps1` | `07-k8s_migrate_gate.log` |
| `07-k8s_redis_split_contract` | 1 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\k8s_redis_split_contract.tests.ps1` | `07-k8s_redis_split_contract.log` |
| `07-k8s_route_tool` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\k8s_route_tool.tests.ps1` | `07-k8s_route_tool.log` |
| `07-kafka_offset_reset_output_guard` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\kafka_offset_reset_output_guard.tests.ps1` | `07-kafka_offset_reset_output_guard.log` |
| `07-lobby_authority_render` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\lobby_authority_render.tests.ps1` | `07-lobby_authority_render.log` |
| `07-logical_db_deploy_contract` | 1 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\logical_db_deploy_contract.tests.ps1` | `07-logical_db_deploy_contract.log` |
| `07-login_metrics_contract` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\login_metrics_contract.tests.ps1` | `07-login_metrics_contract.log` |
| `07-mail_admin_dev_token` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\mail_admin_dev_token.tests.ps1` | `07-mail_admin_dev_token.log` |
| `07-maintenance_drain` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\maintenance_drain.tests.ps1` | `07-maintenance_drain.log` |
| `07-make_release` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\make_release.tests.ps1` | `07-make_release.log` |
| `07-no_raw_pointer_memory` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\no_raw_pointer_memory.tests.ps1` | `07-no_raw_pointer_memory.log` |
| `07-no_raw_pointer_project` | 1 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\no_raw_pointer_project.tests.ps1 -MSBuildPath 'C:\Program Files\Microsoft Visual Studio\18\Enterprise\MSBuild\Current\Bin\MSBuild.exe' -EvidenceDir D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2/07-no-raw-pointer-project` | `07-no_raw_pointer_project.log` |
| `07-no_raw_pointer_setup` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\no_raw_pointer_setup.tests.ps1` | `07-no_raw_pointer_setup.log` |
| `07-publish_images` | 1 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\publish_images.tests.ps1` | `07-publish_images.log` |
| `07-redis_exporter_contract` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\redis_exporter_contract.tests.ps1` | `07-redis_exporter_contract.log` |
| `07-redis_maxclients_contract` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\redis_maxclients_contract.tests.ps1` | `07-redis_maxclients_contract.log` |
| `07-redis_player_cluster_contract` | 1 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\redis_player_cluster_contract.tests.ps1` | `07-redis_player_cluster_contract.log` |
| `07-release_common_version` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\release_common_version.tests.ps1` | `07-release_common_version.log` |
| `07-sql_boot_catalog_local` | 1 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\sql_boot_catalog_local.tests.ps1` | `07-sql_boot_catalog_local.log` |
| `07-start_game_command_contract` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\start_game_command_contract.tests.ps1` | `07-start_game_command_contract.log` |
| `07-stress_snap_redis` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\tests\stress_snap_redis.tests.ps1` | `07-stress_snap_redis.log` |
| `07-stress-summarize` | 0 | `.` | `pwsh -File tools/scripts/test_stress_summarize.ps1` | `07-stress-summarize.log` |
| `07-docs-index` | 1 | `.` | `python tools/scripts/gen_docs_index.py --check` | `07-docs-index.log` |

## 8 etcd运维:没跑

M0、M3、探针A、P0-D全部没跑：本机kubectl contexts为空，PATH无kind，没有本轮隔离K8s运维环境。M0要求改三处镜像pin也与只跑不改配置冲突，未执行镜像配置修改。§7的etcd_maintenance_contract exit0，不替代上述实跑。证据：[08-not-run.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/08-not-run.txt)、[00-kube-contexts.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/00-kube-contexts.log)、[00-kind.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/00-kind.txt)。

## 9 Java网关:失败

首次本仓reactor命令exit1。那一次失败发生在authority-jdbc的protobuf:5.1.10:generate (authority-descriptor)，当时尚未进入Java源码编译/测试。原文：

```text
MultipleFailuresException → UncheckedIOException → NoSuchFileException:
D:\luyuan\xuanming-server-mmo-cpp\build\friend-request-proto-3c959635\third_party\grpc
Reactor: authority-jdbc FAILURE; gateway-node SKIPPED
```

protoc35.1本地文件传输已完成；错误为生成插件扫描缺失目录，不能写成测试断言失败。该首次命令下五个目标类及MySQL锁序断言均没跑，gateway-node的SKIPPED按没跑；扫描阶段未产生本次JUnit结果。后来已有修后实际JUnit，见下文，不能仍说全Java未入JUnit。

| ID | 退出码 | 工作目录（相对执行根） | 已执行命令模板 | 日志文件（相对日志根） |
|---|---:|---|---|---|
| `09-java-reactor` | 1 | `java` | `mvn.cmd -pl gateway_node -am test -Dtest=RowIdsTest,GatewaySchemaContractTest,ZoneWhitelistRepositoryWiringTest,ZoneWhitelistUpsertLockOrderMySqlTest,AdminWhitelistControllerTest -Dsurefire.failIfNoSpecifiedTests=false -Dsurefire.reportsDirectory=D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2/09-java-reports -Dprotoc.spec=file:/D:/luyuan/xuanming-server-mmo-cpp/third_party/protobuf/install_vs2026_dbg/bin/protoc.exe` | `09-java-reactor.log` |

追加授权后只改 `java/authority_jdbc/pom.xml`：initialize阶段将正式proto逐字复制到本次target输入目录（filtering=false），保留proto/规范路径，限制插件扫描根；没有修改协议、业务逻辑、Java测试断言或删除他人build产物。`09-java-buildfix-compile`的`mvn.cmd -pl gateway_node -am test-compile -Dprotoc.spec=file:/D:/luyuan/xuanming-server-mmo-cpp/third_party/protobuf/install_vs2026_dbg/bin/protoc.exe`实际exit0。日志 [09-java-buildfix-compile.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/09-java-buildfix-compile.log)。

正式源根同一35.1 protoc独立基准 `09-java-buildfix-descriptor-reference` exit0；`09-buildfix-descriptor-compare`实际`fc.exe /b`退出0。基准及生成descriptor SHA均为 `CAC4E1BC76DDDCC37CCCC67FCF7066722E8DE6BD9284FA0893DEB4E1812CF7F5`，主入口及依赖5份正式proto/投影SHA逐份相同，见 [09-buildfix-contract-check.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/09-buildfix-contract-check.json)、[09-buildfix-descriptor-compare.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/09-buildfix-descriptor-compare.log)。这是原扫描阻塞已解除的修后证据。

同一次首次合并suite `16-java-buildfix-unit-suite`实际exit0，26套新XML=255PASS/0FAIL/0SKIP（authority_jdbc20、gateway_node235）。§9四个已执行专项类共32：RowIdsTest3、AdminWhitelistControllerTest10、ZoneWhitelistRepositoryWiringTest4、GatewaySchemaContractTest15（不含那条真TiDB方法）；AuthorityConnectionTest20、HotfixCheckServiceTest8也在这次真实运行中通过，不是另起测试重跑。

CLI `-Dsurefire.reportsDirectory`未绑定预期目录，实际新XML落在默认target/surefire-reports。按命令[start,end]时窗、Maven Running类集合及逐类计数一致后归档52份XML/TXT，未借历史target结果。完整 [16-java-buildfix-test-summary.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-buildfix-test-summary.json)、[归档新报告](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-buildfix-reports)、[16-java-buildfix-unit-suite.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-buildfix-unit-suite.log)。

14个真实外部前置方法仍没跑：Hikari1、两MySQL锁序类4、真TiDB方法1、LoginEndpointIntegrationTest8；它们不在255内，不能说269全跑。完整方法/源码行/原因/前置ENV键名见 [16-java-not-run.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-not-run.txt) / [16-java-not-run.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-not-run.json)。04建库失败、源SQL停机，未伪造writer grant/资格，H2或root不替代真实限制账号和锁语义；LoginEndpoint完整Spring上下文亦无test/H2免授权。

## 10 全链路冒烟:跳过

全部业务场景没跑：交接要求§3、§6全绿才能开展，本轮两节均未全绿。因此本地建角/进场/存盘/下线/重连/交接/交易冻结/判死恢复/结算/收件箱/发号/聊天/榜单/战斗/拉数导入/在线合服以及MySQL七项均未开展，没有用单元/夹具替代。K8s也没跑：无可用本轮kind/context，未登录远端。证据：[10-not-run.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/10-not-run.txt)。

## 11 压测:没跑

没跑，用户明确本轮排除。未执行登录加压、容量加压或给出性能对比结论；脚本DryRun、合同与快照不算压测。

## 12 Redis集群:没跑

总项业务、故障、迁移和容量链没跑；建群健康子项已实际执行且通过，不能用健康子项代替总项完成。已在本轮官方deploy/compose.redis-cluster.yml上，以新项目player-store-r2-redis和新项目卷首次建群，Redis8.10.2固定digest。为固定container_name冲突，先只读核同仓compose身份且仍停止，再仅rm七个精确停止容器（无force、不-v），保留旧卷，未动codex资源。compose up exit0，init wait命令exit0且打印容器exit0；六节点PING/INFO/CLUSTER/replication/dbsize、原生cluster check均exit0。健康摘要[12-result.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/12-result.json)为infra_healthy=true、conflicts空。12-not-run.txt首句是较早冲突快照，不能据此写最终建群没跑，应以完成记录为准。

原生集群3主3从，16384 slots覆盖通过；迁移status为空列表；stress_snap用Stages0、空HTTP endpoints采集真实Redis快照，命令exit0，无加压。这只说明基础设施，不是业务客户端/容量通过。login-test/travel-smoke、故障/迁移业务链没跑：§3/6红且业务栈未就绪；现成playerredis用例使用miniredis，不能冒充真实Cluster。实际容量/分片加压没跑，本轮仅capacity DryRun（exit0，明确nothing executed）；kind部署及真实exporter/告警指标核对没跑：无context/kind，未安装或改配置。

| ID | 退出码 | 工作目录（相对执行根） | 已执行命令模板 | 日志文件（相对日志根） |
|---|---:|---|---|---|
| `12-container-inventory` | 0 | `.` | `docker --context desktop-linux ps -a --format '{{.Names}}'` | `12-container-inventory.log` |
| `12-volume-inventory` | 0 | `.` | `docker --context desktop-linux volume ls --format '{{.Name}}'` | `12-volume-inventory.log` |
| `12-network-inventory` | 0 | `.` | `docker --context desktop-linux network ls --format '{{.Name}}'` | `12-network-inventory.log` |
| `12-image-inspect` | 0 | `.` | `docker --context desktop-linux image inspect redis:8.10.2@sha256:6f81e8915c60b065a524e6967e0ad1c639ba6efa84d669f823683ea04d9150ee --format '{{.Id}} {{json .RepoDigests}}'` | `12-image-inspect.log` |
| `12-capacity-dryrun` | 0 | `.` | `pwsh -NoProfile -File tools/scripts/redis_cluster_capacity.ps1 -Target docker-cluster:redis-player-cluster-0:7100 -DryRun -OutDir D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2/12-capacity-dryrun` | `12-capacity-dryrun.log` |
| `12-conflict-owners` | 0 | `.` | `docker --context desktop-linux inspect --format '{{.Name}}\|{{.State.Running}}\|{{.Created}}\|{{json .Config.Labels}}' redis-player-cluster-0 redis-player-cluster-1 redis-player-cluster-2 redis-player-cluster-3 redis-player-cluster-4 redis-player-cluster-5 redis-player-cluster-init mysql` | `12-conflict-owners.log` |
| `12-preclean-identities` | 0 | `.` | `docker --context desktop-linux inspect --format '{{.Id}}\|{{.Name}}\|{{.State.Running}}\|{{json .Config.Labels}}' redis-player-cluster-0 redis-player-cluster-1 redis-player-cluster-2 redis-player-cluster-3 redis-player-cluster-4 redis-player-cluster-5 redis-player-cluster-init` | `12-preclean-identities.log` |
| `12-preclean-stopped-containers` | 0 | `.` | `docker --context desktop-linux rm 25099eaa67fd484236da9a960498bcec81d9e519187ae5e60384ce5afb73e847 d52e69048ba7dd8cabf1bec30605cccdac52fe5ee37a8a6052ae9f961404c540 1b15fcf7aa33ef62fa7d08d671683a99c6b37a6b8f4554a605b2907e1f264e9c f98e0df400eabfefa5f13c232fdc51781f45ac4e560340f95d64551aba3660f0 25dda006d31822182d5288b1792b9833e9549cc8b0a3b55599ea6f6864bd4013 d0c8f7986cf8f8beaf1ead4d92bd105dc18c6c480c1f50a0a1961dfbbfb64287 0ee3153e9c6e2330505e4dff4798ec36fc92377d926f4e4feb87bd2c7ad316e8` | `12-preclean-stopped-containers.log` |
| `12-postclean-container-inventory` | 0 | `.` | `docker --context desktop-linux ps -a --format '{{.Names}}'` | `12-postclean-container-inventory.log` |
| `12-postclean-volume-inventory` | 0 | `.` | `docker --context desktop-linux volume ls --format '{{.Name}}'` | `12-postclean-volume-inventory.log` |
| `12-postclean-network-inventory` | 0 | `.` | `docker --context desktop-linux network ls --format '{{.Name}}'` | `12-postclean-network-inventory.log` |
| `12-compose-up` | 0 | `.` | `docker --context desktop-linux compose -p player-store-r2-redis -f deploy/compose.redis-cluster.yml up -d` | `12-compose-up.log` |
| `12-init-wait` | 0 | `.` | `docker --context desktop-linux wait redis-player-cluster-init` | `12-init-wait.log` |
| `12-init-log` | 0 | `.` | `docker --context desktop-linux logs redis-player-cluster-init` | `12-init-log.log` |
| `12-7100-ping` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7100 PING` | `12-7100-ping.log` |
| `12-7100-server` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7100 INFO server` | `12-7100-server.log` |
| `12-7100-cluster-info` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7100 CLUSTER INFO` | `12-7100-cluster-info.log` |
| `12-7100-nodes` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7100 CLUSTER NODES` | `12-7100-nodes.log` |
| `12-7100-replication` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7100 INFO replication` | `12-7100-replication.log` |
| `12-7100-dbsize` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7100 DBSIZE` | `12-7100-dbsize.log` |
| `12-7101-ping` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7101 PING` | `12-7101-ping.log` |
| `12-7101-server` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7101 INFO server` | `12-7101-server.log` |
| `12-7101-cluster-info` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7101 CLUSTER INFO` | `12-7101-cluster-info.log` |
| `12-7101-nodes` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7101 CLUSTER NODES` | `12-7101-nodes.log` |
| `12-7101-replication` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7101 INFO replication` | `12-7101-replication.log` |
| `12-7101-dbsize` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7101 DBSIZE` | `12-7101-dbsize.log` |
| `12-7102-ping` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7102 PING` | `12-7102-ping.log` |
| `12-7102-server` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7102 INFO server` | `12-7102-server.log` |
| `12-7102-cluster-info` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7102 CLUSTER INFO` | `12-7102-cluster-info.log` |
| `12-7102-nodes` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7102 CLUSTER NODES` | `12-7102-nodes.log` |
| `12-7102-replication` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7102 INFO replication` | `12-7102-replication.log` |
| `12-7102-dbsize` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7102 DBSIZE` | `12-7102-dbsize.log` |
| `12-7103-ping` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7103 PING` | `12-7103-ping.log` |
| `12-7103-server` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7103 INFO server` | `12-7103-server.log` |
| `12-7103-cluster-info` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7103 CLUSTER INFO` | `12-7103-cluster-info.log` |
| `12-7103-nodes` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7103 CLUSTER NODES` | `12-7103-nodes.log` |
| `12-7103-replication` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7103 INFO replication` | `12-7103-replication.log` |
| `12-7103-dbsize` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7103 DBSIZE` | `12-7103-dbsize.log` |
| `12-7104-ping` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7104 PING` | `12-7104-ping.log` |
| `12-7104-server` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7104 INFO server` | `12-7104-server.log` |
| `12-7104-cluster-info` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7104 CLUSTER INFO` | `12-7104-cluster-info.log` |
| `12-7104-nodes` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7104 CLUSTER NODES` | `12-7104-nodes.log` |
| `12-7104-replication` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7104 INFO replication` | `12-7104-replication.log` |
| `12-7104-dbsize` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7104 DBSIZE` | `12-7104-dbsize.log` |
| `12-7105-ping` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7105 PING` | `12-7105-ping.log` |
| `12-7105-server` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7105 INFO server` | `12-7105-server.log` |
| `12-7105-cluster-info` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7105 CLUSTER INFO` | `12-7105-cluster-info.log` |
| `12-7105-nodes` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7105 CLUSTER NODES` | `12-7105-nodes.log` |
| `12-7105-replication` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7105 INFO replication` | `12-7105-replication.log` |
| `12-7105-dbsize` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7105 DBSIZE` | `12-7105-dbsize.log` |
| `12-native-cluster-check` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli --cluster check 127.0.0.1:7100` | `12-native-cluster-check.log` |
| `12-migration-status` | 0 | `.` | `docker --context desktop-linux exec redis-player-cluster-0 redis-cli -p 7100 CLUSTER MIGRATION STATUS ALL` | `12-migration-status.log` |
| `12-real-snapshot` | 0 | `.` | `pwsh -NoProfile -File D:\luyuan\xuanming-server-mmo-cpp\build\player-store-verify-20261101-r2/12-snapshot.ps1` | `12-real-snapshot.log` |

## 13 第三阶段:失败

本节证据快照 UTC：2026-11-02T06:14:51.250003+00:00。仅据本轮已完成命令写入，原postgen及missing的真实完成记录合并；不能把调度条目 completed 当成实际验证通过。

全部精确命令（含超长锚定正则）、退出码、起止时间与完整日志路径见 [13-command-index.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-command-index.txt)，底层原始记录见 [commands.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/commands.jsonl)。失败完整输出见 [aggregate-13-failed-output.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-13-failed-output.jsonl)；4组断言和全部失败名如下，不把空匹配编译当单测通过。

**生成与源码时点。** 生成器三目录单测、proto-gen-build、两次 no-unity proto-gen-run 均退出0。实际生成单测另有1个Windows链接权限条件SKIP，按没跑；名单见后文跳过原件。命令为：

```powershell
# cwd: tools/proto_generator/protogen
go test -json -gcflags=-e -count=1 -p=1 -timeout=60m ./internal/prototools/... ./internal/generator/go/... ./internal/utils/...
# cwd: 仓库根目录
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command proto-gen-build
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml
# 上一命令实际执行两次，各exit0
```

日志：[13-generator-tests.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-generator-tests.log)、[13-proto-build.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-proto-build.log)、[13-proto-run-1.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-proto-run-1.log)、[13-proto-run-2.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-proto-run-2.log)。

13首跑实际清退9文件、5条工程清单项；第二次清退0。不能沿用事先约27文件/15条的推演值。两份建表清单SHA256均为 `5B80A29CEB5018B8F7B2FC810BCE8DA27DD7D8BA7E08F54024A523749B554702`；role_fence Go绑定存在；node.pb.h运行时版本保护为7035001。

验收原件：[13-generation-checks.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-generation-checks.json)；9文件/5工程项全名：[13-generation-coverage.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-generation-coverage.txt)。

验证期间 `proto/scene/player_rollback.proto` 实际发生外部删除：基线2b8184428的blob为f22bedcd9ec5a534b606aa4abd749038c1fcb721，开跑status未列dirty/D，c2d8e5b77相对该基线为D；Codex与子代理没有手改该源。证据 [13-source-change-evidence.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-source-change-evidence.txt)、[00-status-before.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/00-status-before.txt)。因此03/04/06与13的源码时点不同，不能拼接成固定源码下的全绿验证。message_id开跑已M且没有内容hash，“本轮无新增活行”无法判通过，详 [16-generation-audit.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-generation-audit.txt)；已知残留metadata头未手删。

**17模块的生成后证据。** 最后生成结束于2026-11-02T04:54:41.5432055Z。03的build/vet/普通单测发生在此之前，仍作为当时的实际结果保留；13后补的空匹配测试只验证当前生成API的测试编译，未重新运行失败case。共同命令：

```powershell
go build -gcflags=-e ./...
go vet ./...
go test -run '^$' -json -gcflags=-e -p=1 -count=1 -timeout=60m ./...
```

只有build0才做vet，只有vet0才做默认vet未关闭的空匹配编译。原postgen前9模块完成，后7曾因观察到其他Go而保护性未调度，不属于Go验证失败；原结果/错误保留，后续首次命令只从commands合并。

原调度 [13-postgen-results.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-results.json)；7模块首次补齐计划 [13-postgen-missing.ps1](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-missing.ps1)；如有补齐结果见 [13-postgen-missing-results.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-missing-results.json)。

| 模块 | 生成后production build | vet | 测试证据 |
|---|---|---|---|
| go/shared | [13-postgen-build-go-shared: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-shared.log) | [13-postgen-vet-go-shared: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-shared.log) | [13-postgen-test-compile-go-shared: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-shared.log)（仅编译） |
| go/data_service | [13-postgen-build-go-data_service: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-data_service.log) | [13-postgen-vet-go-data_service: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-data_service.log) | [13-postgen-test-compile-go-data_service: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-data_service.log)（仅编译） |
| go/db | [13-postgen-build-go-db: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-db.log) | [13-postgen-vet-go-db: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-db.log) | [13-postgen-test-compile-go-db: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-db.log)（仅编译） |
| go/login | [13-postgen-build-go-login: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-login.log) | [13-postgen-vet-go-login: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-login.log) | [13-postgen-test-compile-go-login: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-login.log)（仅编译） |
| go/guild | [13-postgen-build-go-guild: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-guild.log) | [13-postgen-vet-go-guild: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-guild.log) | [13-postgen-test-compile-go-guild: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-guild.log)（仅编译） |
| go/match | [13-postgen-build-go-match: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-match.log) | [13-postgen-vet-go-match: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-match.log) | [13-postgen-test-compile-go-match: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-match.log)（仅编译） |
| go/trade | [13-postgen-build-go-trade: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-trade.log) | [13-postgen-vet-go-trade: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-trade.log) | [13-postgen-test-compile-go-trade: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-trade.log)（仅编译） |
| go/mail | [13-postgen-build-go-mail: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-mail.log) | [13-postgen-vet-go-mail: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-mail.log) | [13-postgen-test-compile-go-mail: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-mail.log)（仅编译） |
| go/friend | [13-postgen-build-go-friend: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-friend.log) | [13-postgen-vet-go-friend: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-friend.log) | [13-postgen-test-compile-go-friend: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-friend.log)（仅编译） |
| go/player_inbox | [13-postgen-build-go-player_inbox: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-player_inbox.log) | [13-postgen-vet-go-player_inbox: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-player_inbox.log) | [13-postgen-test-compile-go-player_inbox: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-player_inbox.log)（仅编译） |
| go/chat | [13-postgen-build-go-chat: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-chat.log) | [13-postgen-vet-go-chat: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-chat.log) | [13-postgen-test-compile-go-chat: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-chat.log)（仅编译） |
| go/rank | [13-postgen-build-go-rank: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-rank.log) | [13-postgen-vet-go-rank: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-rank.log) | [13-postgen-test-compile-go-rank: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-rank.log)（仅编译） |
| go/scene_manager | [13-postgen-build-go-scene_manager: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-scene_manager.log) | [13-postgen-vet-go-scene_manager: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-scene_manager.log) | [13-postgen-test-compile-go-scene_manager: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-scene_manager.log)（仅编译） |
| tools/dbmove | [13-dbmove-build: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-build.log) | [13-dbmove-vet: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-vet.log) | [13-dbmove-tests: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-tests.log)（123顶层PASS/5条件SKIP） |
| tools/merge_zone | [13-postgen-build-tools-merge_zone: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-tools-merge_zone.log) | [13-postgen-vet-tools-merge_zone: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-tools-merge_zone.log) | [13-postgen-test-compile-tools-merge_zone: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-tools-merge_zone.log)（仅编译） |
| tools/data_consistency_check | [13-postgen-build-tools-data_consistency_check: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-tools-data_consistency_check.log) | [13-postgen-vet-tools-data_consistency_check: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-tools-data_consistency_check.log) | [13-postgen-test-compile-tools-data_consistency_check: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-tools-data_consistency_check.log)（仅编译） |
| go/schemamigrate | [13-postgen-build-go-schemamigrate: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-build-go-schemamigrate.log) | [13-postgen-vet-go-schemamigrate: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-vet-go-schemamigrate.log) | [13-postgen-test-compile-go-schemamigrate: 0](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-postgen-test-compile-go-schemamigrate.log)（仅编译） |

此快照实际完成：生成后production build=17/17，vet=17/17，空匹配测试编译=16/16；dbmove普通测试另计。空匹配命令实际case RUN总计=0；未完成项不能按计划计通过。原17模块普通单测及真实SQL覆盖仍受上述时点和各自失败/跳过限制。router与scene已有失败未重试，早于13的C++构建不冒充清退后的全量重编译。

**真实SQL和依赖中止。** 源TiKV从02:11:40Z运行至02:53:16.959Z，之后ExitCode=137/OOMKilled=true、RestartCount=0、容器内存限额2GiB。13 SQL开始时它已经退出；TiDB frontend仍显示Running不能代替TiKV后端就绪。后续失败只能按实际日志保留，不能称健康真库验收，也不能据此排除代码问题。

状态证据 [13-source-sql-state.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-source-sql-state.log)、[13-tikv-oom-finished.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-tikv-oom-finished.log)；完整TiKV stdout/stderr约71MB：[13-tikv-oom-log.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-tikv-oom-log.log)。确认失败前置后，没有重启或重试，主代理按核过的PID和开始时刻只终止本轮13 SQL驱动/当前match测试，详 [13-tidb-interruption.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-tidb-interruption.json)。随后按精确ID停止本轮PD/TiDB、两MySQL夹具，保留数据。

- data_service/store：`go test -json -gcflags=-e -count=1 -p=1 -timeout=60m -tags=integration -run <262项锚定regex> data_service/internal/store`，exit1；262个顶层均RUN，123PASS/137FAIL/2SKIP，0未终态，0剩余未RUN。原件 [13-tidb-ds-store-p01-b01.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-tidb-ds-store-p01-b01.log)；精确regex [13-ds-store-remaining.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-ds-store-remaining.json)。
- match：无tag同参数，`-run <authority包精确regex> match/internal/authority`；命令exit=-1/interrupted=true。138项选择集实际仅5个顶层RUN：1PASS、3FAIL、1中止无终态；133个从未RUN。最后未终态父用例 `TestEntryTiDBNewTeamRejectsActualPendingQueuedAndMatchedTickets`，最后子RUN为 `/3`。-1来自持句柄观测的强制退出，不是断言终态。原件 [13-tidb-match-p01-b01.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-tidb-match-p01-b01.log)、选择 [13-match-remaining.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-match-remaining.json)。
- 继续依赖该源的原13后续/additional/supplement完全未启动：shared真实入口9+3、独立match logic15/server1、guild剩余14、mail余2、commerce静态67、TiDB inbox37、guild强会话1、TiDB dbmove5；这些静态组有重叠，不相加为总测试数。friend13还因04专用库初始化失败没准备；Kafka1还硬要求固定19492/19493/19494三broker、RF3/ISR3/min.insync.replicas=2，未找到适用正式本地入口。没跑的全部名字及已RUN/无终态/未RUN边界见 [13-tidb-dependent-not-run.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-tidb-dependent-not-run.txt)（51KB）及 [13-tidb-dependent-not-run.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-tidb-dependent-not-run.json)。普通03条件SKIP不算真库，04已有失败不重试。

**MySQL dbmove。** 独立源13306、独立新目标13307；真实SQL就绪检查各只执行一次且成功。source实际26.7.0、缓存Oracle target实际8.4.13；镜像ID `sha256:68d16b54ba323db45375337dc8289b4e8508f5bef51a634c92b7b76bb393df52`。没有补权限或重试。

真实命令：cwd=`tools/dbmove`，`go test -json -gcflags=-e -count=1 -p=1 -timeout=60m -run "^(TestITCutoverOnRealDatabases|TestITOverBudgetRollsBackRealGate|TestITChecksumMatchesAndCatchesDifference|TestITAccountLockGrantsAndConnections|TestITMaintenanceProbeOnRealDatabases)$" ./...`；exit1，5RUN=4PASS/1FAIL/0SKIP，失败 `TestITCutoverOnRealDatabases`。原件 [13-dbmove-real-mysql-test.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-real-mysql-test.log)、[13-dbmove-real-mysql-verdict.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-real-mysql-verdict.json)；两侧版本/就绪 [13-dbmove-real-mysql-source-ready.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-real-mysql-source-ready.log)、[13-dbmove-real-mysql-target-ready.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-real-mysql-target-ready.log)。

该测试是真实SQL两实例上的闸门/权限/校验和验证，源码etcd和复制仍分别为fakeStore/itRepl；不能代替在线玩家搬库、TiCDC/BR/GTID或灾备全地域回档。普通13-dbmove-tests的5个条件SKIP是当时未设置真库DSN的事实，后来MySQL这5个已首次实际运行；聚合原件中的早期“没有独立目标实例”说明不能再作为MySQL最终状态。

资源观察原件 [13-dbmove-resources-before-mysql.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-resources-before-mysql.log)、[13-dbmove-resources-ready-mysql.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-resources-ready-mysql.log)、[13-dbmove-resources-after-mysql.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-dbmove-resources-after-mysql.log)，不作性能结论。

**全部失败断言分组。** 同一原文仅写一次，下面保留全部失败名；父测试只有继承失败时单独列名，不重复子断言。原始Trace/行号/上下文不删，见聚合完整原件。

错误组1：69 个失败事件，原文变体=1。

```text
Error:      	Received unexpected error:
权威SQL结果未定（业务发送后），禁止重放
```

`13-tidb-ds-store-p01-b01` / `data_service/internal/store`：

- `TestTiDBLedgerIsItsOwnDatabase`
- `TestTiDBWrongLogicalShardsRefusesToStart`
- `TestTiDBAccountDirectoryCapReplayRepairAndRemoval`
- `TestTiDBAccountDirectoryReadTakesNoLockOnHit`
- `TestTiDBAccountDirectoryConcurrentFirstLoginCreatesOneRow`
- `TestTiDBAccountDirectoryPreservesExactAccountIdentity`
- `TestTiDBRoleAdmissionVerifyFollowsGenerationAndClose`
- `TestTiDBRoleAdmissionFixedHolderCancelAndLateGenerations`
- `TestTiDBRoleAdmissionStrongSQLProofRejectsUnsyncedRowsAndKeepsReceipt`
- `TestTiDBRoleAdmissionSQLProofNoAdmissionAndHistoricalFloor`
- `TestTiDBRoleAdmissionRegisterAndBeginShareCommitLock`
- `TestTiDBRoleAdmissionSQLProofRejectsRouteAndSchemaFailures`
- `TestTiDBRoleAdmissionOpenRejectsRemovalAndFloorSurvivesRemoval`
- `TestTiDBRoleTradeDeadHolderIsFencedWithoutAck`
- `TestTiDBRoleAdmissionCloseAfterOwnerChangedIsMoot`
- `TestTiDBRoleTradeReleasedCurrentHolderStillWaitsForAck`
- `TestTiDBRoleTradeCurrentHolderBlocksUntilReleased`
- `TestTiDBRoleCancelBeforeUnknownBeginPersistsPermanentTombstone`
- `TestTiDBRoleBeginAllCleanFreezesParticipantsThenEntersFreezing`
- `TestTiDBRoleBeginDryRunRejectionWritesNothing/有挂起`
- `TestTiDBRoleBeginDryRunRejectionWritesNothing/问不齐`
- `TestTiDBRoleBeginRejectedBeforeDryRun`
- `TestTiDBRolePreparingDoesNotBlockEntryAndFreezingSnapshotsIt`
- `TestTiDBRolePreparingRejectsOwnershipOperations`
- `TestTiDBRolePreparingExpiredIsAbortedByReplay`
- `TestTiDBRoleAbortStalePreparingInBackground`
- `TestTiDBRoleCancelFromPreparingDoesNotTouchPlayerStore`
- `TestTiDBRoleCancelOnlyTouchesPlayerStoreForPendingHolders`
- `TestTiDBRoleBeginBlockedLeavesNoTrace`
- `TestTiDBRoleBeginUnreachableKeepsPreparingAndReplayResumes`
- `TestTiDBRoleCancelFromPreparingThawsAndKeepsTombstone`
- `TestTiDBRoleFenceVersionBumpsOnEveryOwnershipChangeAndTransferPushes`
- `TestTiDBRoleMissingCoordinatorRejectsOwnershipChanges`
- `TestTiDBRolePrepareConclusionOfOlderRoundDoesNotCount`
- `TestTiDBRoleBeginHolderConflictAtFinishLeavesNoTrace`
- `TestTiDBRoleListingCannotTakeRemovalTokenRange`
- `TestTiDBRoleRemovalBlockedClearsTokenAndSmallTable`
- `TestTiDBRoleRemovalEntryDuringPrepareAbandons`
- `TestTiDBRoleRemovalUnreachableKeepsTokenAndBackgroundFinishes`
- `TestTiDBRoleRemovalBackgroundAbandonsWhenBlocked`
- `TestTiDBRoleRemovalWalksRolesOneByOne`
- `TestTiDBRoleRemovalFirstRoleRejectedStaysDefinite`
- `TestTiDBRoleBeginFailureAfterFreezingCommitIsNotADefiniteRejection`
- `TestTiDBCreateRoleCommitsDirectoryAndInitialRowsOnce`
- `TestTiDBCreateRoleDoesNotQueueOnRouteMetaAndFrozenStillRejects`
- `TestTiDBCreateRoleInitialRowCollisionLeavesNoRole`
- `TestTiDBCreateRoleConcurrentInitialRowsAreWrittenOnce`
- `TestTiDBCreateRoleMissingInitialTableLeavesNoRole`
- `TestTiDBInitialCommitIsRequiredAndLegacyIsExplicit`
- `TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor/initial`
- `TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor/unmarked`
- `TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor/saved-row`
- `TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor/historical-floor`
- `TestTiDBRoleTradeFreezeCancelDoesNotRefreezeOrClearNewListing`
- `TestTiDBRoleTransferCapConcurrentReplayAndOriginalReceipt`
- `TestTiDBRoleTransferOppositeAccountsUseOneByteLockOrder`
- `TestTiDBCreateRoleWritesPlayerStoreRows`
- `TestTiDBReadPlayerStoreAsset`
- `TestTiDBReleaseDeadPlayerStoreHolder`
- `TestTiDBRaiseStoreEpochComparesClaimSeq`
- `TestTiDBVerifyFrozenOnPlayerStore`
- `TestTiDBReadPlayerDisplays`
- `TestTiDBEmptyRouteBootstrapDoesNotTreatExistingPlayerHistoryAsFresh`

`13-tidb-match-p01-b01` / `match/internal/authority`：

- `TestAuthorityTiDBActivityCancelTombstoneRejectsLateStartAndChangedBody`
- `TestAuthorityTiDBActivityCancelAndReserveHaveOneCommitDecision`
- `TestEntryTiDBTeamLaunchWithoutTicketWinsAgainstLateSoloDirectAndWatch`
- `TestEntryTiDBNewTeamRejectsActualPendingQueuedAndMatchedTickets/0`
- `TestEntryTiDBNewTeamRejectsActualPendingQueuedAndMatchedTickets/1`
- `TestEntryTiDBNewTeamRejectsActualPendingQueuedAndMatchedTickets/2`

错误组2：1 个失败事件，原文变体=1。

```text
Error:      	Received unexpected error:
Error 8027 (HY000): Information schema is out of date: schema failed to update in 1 lease, please make sure TiDB can connect to TiKV
```

`13-tidb-ds-store-p01-b01` / `data_service/internal/store`：

- `TestTiDBMigrateSchemaBuildsEveryShardAndLedger`

错误组3：93 个失败事件，原文变体=1。

```text
Error 1045 (28000): Access denied for user 'root'@'192.168.80.1' (using password: YES)
```

`13-tidb-ds-store-p01-b01` / `data_service/internal/store`：

- `TestPlayerName_BootstrapDDLIsVarcharUniqueBin`
- `TestPlayerName_ReserveIdempotentAndTaken`
- `TestPlayerName_ConcurrentSameName`
- `TestPlayerName_ReleaseConditional`
- `TestPlayerName_BatchGetSkipsAbsentAndRejectsOversize`
- `TestPlayerName_ReleaseRacesTwoReservesConverges/未提交的Release正删被抢的名字/a_竞争者都大于原主人`
- `TestPlayerName_ReleaseRacesTwoReservesConverges/未提交的Release正删被抢的名字/b_竞争者都小于原主人`
- `TestPlayerName_ReleaseRacesTwoReservesConverges/未提交的Release正删被抢的名字/c_竞争者一大一小`
- `TestPlayerName_ReleaseRacesTwoReservesConverges/被抢名字已释放且未purge_未提交的Release正删后继名字/a_竞争者都大于原主人`
- `TestPlayerName_ReleaseRacesTwoReservesConverges/被抢名字已释放且未purge_未提交的Release正删后继名字/b_竞争者都小于原主人`
- `TestPlayerName_ReleaseRacesTwoReservesConverges/被抢名字已释放且未purge_未提交的Release正删后继名字/c_竞争者一大一小`
- `TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks/a_竞争者都大于原主人`
- `TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks/b_竞争者都小于原主人`
- `TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks/c_竞争者一大一小`
- `TestPlayerName_ReserveBehindPendingReleaseOutcome/未提交的Release正删被抢的名字/a_竞争者都大于原主人`
- `TestPlayerName_ReserveBehindPendingReleaseOutcome/未提交的Release正删被抢的名字/b_竞争者都小于原主人`
- `TestPlayerName_ReserveBehindPendingReleaseOutcome/未提交的Release正删被抢的名字/c_竞争者一大一小`
- `TestPlayerName_ReserveBehindPendingReleaseOutcome/被抢名字已释放且未purge_未提交的Release正删后继名字/a_竞争者都大于原主人`
- `TestPlayerName_ReserveBehindPendingReleaseOutcome/被抢名字已释放且未purge_未提交的Release正删后继名字/b_竞争者都小于原主人`
- `TestPlayerName_ReserveBehindPendingReleaseOutcome/被抢名字已释放且未purge_未提交的Release正删后继名字/c_竞争者一大一小`
- `TestPlayerName_ReleaseStatementLocksClusteredKeyFirst`
- `TestPlayerName_StoreRefusesUnknownPrimaryKey/player_name`
- `TestPlayerName_StoreRefusesUnknownPrimaryKey/player_name_owner`
- `TestPlayerName_StoreRefusesWrongDirectoryShard`
- `TestRecallStore_LifecycleWritesLinesCountersAndAudit`
- `TestRecallStore_ShortfallCountsSeparately`
- `TestRecallStore_SourceCanBeConsumedOnce`
- `TestRecallStore_FrozenRoleDefersDispatch`
- `TestRecallStore_MissingRoleClosesTheRow`
- `TestRecallStore_CancelStopsWhatHasNotTakenEffect`
- `TestRecallStore_CancelRacingDispatchKeepsCountersRight`
- `TestRecallStore_SourcesAreReleasedOnlyWhenNothingWasRecalled`
- `TestRecallStore_RescheduleWithWrongTokenIsLeaseLost`
- `TestRecallStore_PendingForRoleBlocksUntilSettled`
- `TestRecallStore_ResolveManually`
- `TestRouteSchema_MigrationCreatesTablesSeedsMetaAndIsIdempotent`
- `TestRouteSchema_CheckConstraintsEnforced`
- `TestRouteSchema_ShapeGuardRejectsTamperedTable/丢列`
- `TestRouteSchema_ShapeGuardRejectsTamperedTable/换主键`
- `TestRouteSchema_ShapeGuardRejectsTamperedTable/player_route主键退回player_id`
- `TestRouteSchema_ShapeGuardRejectsTamperedTable/G1a之前的真实旧形状`
- `TestRouteStore_RegisterHomeBranches`
- `TestRouteStore_NullPlacementBindsNull`
- `TestRouteStore_ConcurrentSamePlayer`
- `TestRouteStore_ConcurrentManyPlayersSameZone`
- `TestRouteStore_LockWaitBounded`
- `TestRouteStore_MetaMissingAndNotWritableFailClosed`
- `TestRouteStore_ProbeRejectsPoolWithoutSessionVariables`
- `TestRouteStore_ProbeRejectsUnenforcedShardCheck`
- `TestRouteStore_ProbeRejectsTableBuiltWithAnotherShardModulus`
- `TestRouteStore_ProbeKeysHeldByRealPlayers`
- `TestRouteStore_ProbesQueueOnMetaGuard`
- `TestRouteStore_ShardMismatchRowIsInvisibleToPointReads`
- `TestRouteStore_KeyedReadsUsePrimaryRange`
- `TestRouteStore_TiDBClusteredShardsAndPresplit`
- `TestMigrateSchema_CreatesGlobalTablesAndIsIdempotent`
- `TestMigrateSchema_ConcurrentFirstBootIsSerialized`
- `TestMigrateSchema_LockBusyReturnsSentinel`
- `TestMigrateSchema_RejectsLegacyHandWrittenTables`
- `TestSnapshotMigration_RefusesKeyPresentColumnBroken`
- `TestSnapshotStore_ProbeRejectsKeyPresentColumnBroken`
- `TestSnapshotStore_GuidDedupAndGmRowsCoexist`
- `TestSnapshotStore_GmReadersExcludeSceneRows`
- `TestSnapshotStore_ConcurrentWritersNeverSurfaceDeadlock`
- `TestSnapshotStore_LegacyGapDeadlockIsAbsorbedInPlace`
- `TestSnapshotStore_GuidUniqueKeyIsEnforcedByTheDatabase`
- `TestSnapshotStore_GuidLookupUsesUniqueKey`
- `TestSnapshotStore_SameGuidConcurrentWritersLandOneRow`
- `TestSnapshotStore_DistinctGuidDoesNotWaitOnUncommittedNeighbour`
- `TestSnapshotStore_DistinctGuidsNeverDeadlock`
- `TestSnapshotStore_DistinctGuidsNeverDeadlock_MixedRowIDs`
- `TestSnapshotStore_SameGuidQueueDeadlockIsAbsorbed`
- `TestSnapshotStore_BatchOutOfGuidOrderOverlapDoesNotDeadlock`
- `TestSnapshotTableUniqueIndexesAreOnlyGuidKey`
- `TestSnapshotStore_BatchOverlapDoesNotDeadlock`
- `TestSnapshotStore_BatchOverlapDoesNotDeadlock_MixedRowIDs`
- `TestSnapshotStore_LegacyWriterCoexistsWithUniqueKey`
- `TestSnapshotMigration_DedupesDuplicateGuidsAndIsRerunnable`
- `TestSnapshotStore_FallsBackWhenUniqueKeyAbsent`
- `TestSnapshotMigration_RefusesSameNamedNonUniqueIndex`
- `TestSnapshotMigration_RefusesSameNamedPlainColumn`
- `TestSnapshotMigration_DedupePointDeleteUsesPrimaryKey`
- `TestSnapshotStore_LookupMissIsAnError`
- `TestSnapshotStore_BatchDedupsAgainstStoredAndWithinBatch`
- `TestSnapshotStore_BatchReplayIsNoop`
- `TestSnapshotStore_BatchIdsFollowGuidOrder`
- `TestSnapshotTablesHaveNoAutoIncrement`
- `TestSnapshotStore_RowIDCollisionIsNotSwallowed`
- `TestSnapshotStore_BatchFallsBackWhenUniqueKeyAbsent`
- `TestSnapshotStore_OverlappingBatchesFromTwoWritersLandOneRowEach`
- `TestSnapshotStore_BatchOfLargeBlobsFitsPacket`
- `TestTransactionLogStore_InsertIgnoreReplayIsNoop`
- `TestTransactionLogStore_TimestampSecAndZoneLand`

错误组4：1 个失败事件，原文变体=1。

```text
refused: the target is missing grants the source has, these accounts would fail after the flip and there is no way back. Nothing was changed: root@%: CREATE_SPATIAL_REFERENCE_SYSTEM on *.*, MANAGE_DATA_MASKING_POLICY on *.*
```

`13-dbmove-real-mysql-test` / `dbmove`：

- `TestITCutoverOnRealDatabases`

继承子测试失败、无独立断言的父/中间层事件11个：

- `TestTiDBRoleBeginDryRunRejectionWritesNothing`（13-tidb-ds-store-p01-b01）
- `TestTiDBInitialCommitProofNeedsInitialRowsAndNoHistoricalFloor`（13-tidb-ds-store-p01-b01）
- `TestPlayerName_ReleaseRacesTwoReservesConverges/未提交的Release正删被抢的名字`（13-tidb-ds-store-p01-b01）
- `TestPlayerName_ReleaseRacesTwoReservesConverges/被抢名字已释放且未purge_未提交的Release正删后继名字`（13-tidb-ds-store-p01-b01）
- `TestPlayerName_ReleaseRacesTwoReservesConverges`（13-tidb-ds-store-p01-b01）
- `TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks`（13-tidb-ds-store-p01-b01）
- `TestPlayerName_ReserveBehindPendingReleaseOutcome/未提交的Release正删被抢的名字`（13-tidb-ds-store-p01-b01）
- `TestPlayerName_ReserveBehindPendingReleaseOutcome/被抢名字已释放且未purge_未提交的Release正删后继名字`（13-tidb-ds-store-p01-b01）
- `TestPlayerName_ReserveBehindPendingReleaseOutcome`（13-tidb-ds-store-p01-b01）
- `TestPlayerName_StoreRefusesUnknownPrimaryKey`（13-tidb-ds-store-p01-b01）
- `TestRouteSchema_ShapeGuardRejectsTamperedTable`（13-tidb-ds-store-p01-b01）

本轮13实际case SKIP共8条（生成器平台1、dbmove普通轮5、store GUID门控2），按没跑；完整名字和原因见 [aggregate-13-skipped.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-13-skipped.txt)。match中止父/子各1条无终态另计，不伪造SKIP。空匹配/无测试包的包级事件不当作业务测试通过。

分组全原件 [aggregate-13-analysis.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-13-analysis.txt)、[aggregate-13-failed-output.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-13-failed-output.jsonl)。17模块当前已完成记录已合并；普通单测/真库覆盖边界仍按上述实际运行区分。

Java追加事实：同一次 `16-java-buildfix-unit-suite` 中AuthorityConnectionTest普通20个@Test实际通过（Mockito/内存语义，不是TiDB/物理ACL验收）；POM扫描前置已修复，不再沿用早期“AuthorityConnectionTest未入JUnit”。真实AuthorityHikariTiDbTest仍没跑，完整证据见§9与 `16-java-not-run.txt/json`。

## 14 GM批量回收与摘孤儿:失败

本节失败。`14-reference-audit`退出0只证明引用本轮03/04/13证据的审计脚本执行完成，没有新增真库通过证据；不能据此记§14通过。shared/data_service普通build、vet、test引用本轮03（均exit0，真实用例SKIP仍算没跑）；协议证据引用本轮01/13，未沿用第一轮。13后生成物的17模块production build/vet及16模块空匹配编译已实际完成（均0、实际case RUN=0），见§13；不能将其当新增普通单测或回收真库通过。

回收真库每后端11个用例全部已实际RUN并FAIL：MySQL来自`04-mysql-data-service.log`，TiDB补余来自`13-tidb-ds-store-p01-b01.log`。两组均为PASS=0/FAIL=11/SKIP=0/无终态=0，不能当作回收状态机业务已经验证。原始错误相同：

```text
recall_store_integration_test.go:123: Error 1045 (28000): Access denied for user 'root'@'192.168.80.1' (using password: YES)
```

11个失败用例名（以下每个在两后端各失败一次，完整断言定位保留在上述两份JSON日志）：

```text
TestRecallStore_LifecycleWritesLinesCountersAndAudit
TestRecallStore_ShortfallCountsSeparately
TestRecallStore_SourceCanBeConsumedOnce
TestRecallStore_FrozenRoleDefersDispatch
TestRecallStore_MissingRoleClosesTheRow
TestRecallStore_CancelStopsWhatHasNotTakenEffect
TestRecallStore_CancelRacingDispatchKeepsCountersRight
TestRecallStore_SourcesAreReleasedOnlyWhenNothingWasRecalled
TestRecallStore_RescheduleWithWrongTokenIsLeaseLost
TestRecallStore_PendingForRoleBlocksUntilSettled
TestRecallStore_ResolveManually
```

C++ currency/bag定向与全量、两版真实Redis用例均没跑：目标显式依赖本轮失败的scene.lib，不能靠旧lib或新链接exe宣称当前源码通过。预览→执行→GetRecallBatch及欠款、离线、shortfall、重放、取消重预览、过户阻断、LoginAdmin真实摘孤儿端到端没跑，完整业务栈前置未满足。原始失败保留，获新授权后若修纯构建问题，须另用新ID记追加验证，不能抹掉本次结果。

## 15 并回修复:失败

首次Windows验证失败；Linux两套SHA各122个实际用例通过。`15-reference-audit`退出0仅引用03的scene_manager build/vet/logic/metrics证据，不是新增全套测试通过；首次Windows driver退出1。

Windows独立`muduo_grpc -Build`首次退出1。14个翻译单元的裸指针门禁通过，随后链接报错，把整串分号分隔库名当作一个输入文件。原日志中文位置已是问号，保留原文：

```text
LINK : fatal error LNK1104: ??????"muduo_grpc.lib;muduo.lib;nghttp2.lib;hiredisd.lib;ws2_32.lib;kernel32.lib;user32.lib;gdi32.lib;winspool.lib;comdlg32.lib;advapi32.lib;shell32.lib;ole32.lib;oleaut32.lib;uuid.lib;odbc32.lib;odbccp32.lib;" [D:\luyuan\xuanming-server-mmo-cpp\cpp\tests\muduo_grpc_test\muduo_grpc_test.vcxproj]
```

首次完整构建日志保留在`15-hiredis-grpc/muduo_grpc_test.build.log`，共1个匹配error行，只读提取到`15-errors-first50.txt`；追加授权后另有修改后的新ID验证。首次失败时`15-grpc-artifact-metadata.json`记录：muduo.lib时间UTC为`2026-11-01T09:14:57.0002982Z`，muduo_grpc.lib为`2026-10-27T12:57:39.5898671Z`，目标`build/cpp/tests/muduo_grpc_test.exe`不存在；因此测试实际0例，不能满足EXE晚于muduo.lib的通过标准，也未运行旧EXE。

Windows rpc_controller全量、HiredisEventBatchWindows两条×20轮首次没跑（0/40），依赖失败scene.lib；muduo_grpc的HiredisPeerFin等测试首次因链接失败没跑。所有零case项目单列`15-skipped.txt`。这些Windows项目不能用Linux结果替代。获新授权后，新ID `15-hiredis-grpc-buildfix` 已实际完成0，独立muduo_grpc构建0，200PASS/0FAIL/5SKIP；不再是运行中。HiredisPeerFin实际PASS，新EXE晚于当时muduo.lib，原始失败保留。Windows rpc_controller/HiredisEventBatchWindows的40次仍没跑，未用新独立目标结果替代。

三份Hiredis.cc、三份Hiredis.h两两`fc.exe /b`六组全exit0，路径编号：0=`cpp/libs/engine/muduo_windows/src/contrib/hiredis`，1=`tools/archived/muduo_linux_overlay/contrib/hiredis`，2=`third_party/muduo/contrib/hiredis`。三份cc的SHA256均为`8C8AF27C6F03B7536CC18B7C6CAA87968D88CBA5B8606A224DDEE4D0D79B55F4`；三份h均为`B8BB1AC73AA86313A65CEB5B1C4687BC33CC24F57AAB203BDB64B201026E4F21`。对应结果和每对日志见`15-hiredis-copy-results.json`与下表。

Linux已完成当前gitlink `b55b0ac6a10d4ded1301c3172e779ec46ada2ab3`：HEAD/gitlink相同，`status --short`为空；使用linux/amd64不可变SDK imageID `sha256:fc6fad59c914fe383f64fb01471cd0bf8128c591a6e69e060ce634e00dbb2024`。工作树只读挂载、串行构建，CMake将本轮overlay复制到独立build目录；未切换子模块或拉镜像。

该SHA的configure/build/test各exit0。实际测试日志122条RUN/122条OK，XML为13个suite、tests=122、failure/error/disabled/skipped均0，耗时7.32秒。编译与链接参数实际含`-fsanitize=address,undefined`；运行设置泄漏检查和遇错即停，日志无ASan/UBSan报告。二进制`15-linux-b55b0ac/build/redis_lifecycle_test`时间UTC为`2026-11-02T06:20:33.1074391Z`，早于本次实际test开始`06:20:34.2823903Z`。证据为`15-linux-b55b0ac-test.log`、`15-linux-b55b0ac/redis-lifecycle.xml`、`15-linux-environment.json`与真实flags.make/link.txt，未用0case或旧XML替代。

另一版本完整SHA为`f1fc77e0c13b80e5086ff457362c8a86d1b609d4`，以git archive导入本轮独立目录后只读挂载。该套configure/build/test均exit0；实际test.log有122条RUN/122条OK、0FAILED/0SKIPPED，XML为122个status=run、failure/error/disabled均0、无skipped元素，耗时7.351秒；启用ASan/UBSan且实际日志无报告。二进制`15-linux-f1fc77e/build/redis_lifecycle_test`时间UTC为`2026-11-02T06:24:46.7583988Z`，早于本次test开始`06:24:47.3670014Z`。证据为`15-linux-f1fc77e-test.log`、`15-linux-f1fc77e/redis-lifecycle.xml`与该build下实际flags.make/link.txt。

`15-linux-driver`已exit0，两SHA各122/122（合计244次运行），均无SKIP或ASan/UBSan报告。该Linux成功不抹去首次Windows失败及仍没跑的Windows RPC40/互操作5。

本段统一日志根为`D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/`。下表文件均相对该绝对根；每条准确cwd/exe/args/exit/absolute log均可在[commands.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/commands.jsonl)按ID回查。

| ID | 退出码 | 完整日志文件 |
|---|---:|---|
| 14-reference-audit | 0 | 14-reference-audit.log |
| 15-reference-audit | 0 | 15-reference-audit.log |
| 15-windows-driver | 1 | 15-windows-driver.log |
| 15-hiredis-grpc | 1 | 15-hiredis-grpc.log，另见15-hiredis-grpc/muduo_grpc_test.build.log |
| 15-compare-Hiredis-cc-0-1 | 0 | 15-compare-Hiredis-cc-0-1.log |
| 15-compare-Hiredis-cc-0-2 | 0 | 15-compare-Hiredis-cc-0-2.log |
| 15-compare-Hiredis-cc-1-2 | 0 | 15-compare-Hiredis-cc-1-2.log |
| 15-compare-Hiredis-h-0-1 | 0 | 15-compare-Hiredis-h-0-1.log |
| 15-compare-Hiredis-h-0-2 | 0 | 15-compare-Hiredis-h-0-2.log |
| 15-compare-Hiredis-h-1-2 | 0 | 15-compare-Hiredis-h-1-2.log |
| 15-linux-sdk-image | 0 | 15-linux-sdk-image.log |
| 15-linux-current-muduo-sha | 0 | 15-linux-current-muduo-sha.log |
| 15-linux-current-gitlink | 0 | 15-linux-current-gitlink.log |
| 15-linux-current-muduo-status | 0 | 15-linux-current-muduo-status.log（空，clean） |
| 15-linux-b55b0ac-configure | 0 | 15-linux-b55b0ac-configure.log |
| 15-linux-b55b0ac-build | 0 | 15-linux-b55b0ac-build.log |
| 15-linux-b55b0ac-test | 0 | 15-linux-b55b0ac-test.log |
| 15-linux-f1-muduo-sha | 0 | 15-linux-f1-muduo-sha.log |
| 15-linux-f1-archive | 0 | 15-linux-f1-archive.log |
| 15-linux-f1-extract | 0 | 15-linux-f1-extract.log |
| 15-linux-f1fc77e-configure | 0 | 15-linux-f1fc77e-configure.log |
| 15-linux-f1fc77e-build | 0 | 15-linux-f1fc77e-build.log |
| 15-linux-f1fc77e-test | 0 | 15-linux-f1fc77e-test.log |
| 15-linux-driver | 0 | 15-linux-driver.log |
| 15-hiredis-grpc-buildfix | 0 | 15-hiredis-grpc-buildfix.log；新目录15-hiredis-grpc-after-buildfix |

修后Windows真实证据：[15-hiredis-grpc-buildfix.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/15-hiredis-grpc-buildfix.log)、[build](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/15-hiredis-grpc-after-buildfix/muduo_grpc_test.build.log)、[stdout](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/15-hiredis-grpc-after-buildfix/muduo_grpc_test.stdout.log)。`HiredisPeerFin.PureFinDisconnectsAndCompletesPendingCommandBeforeTimeout`为实际PASS，EXE mtime=2026-11-02T06:31:58.522273Z，muduo.lib=2026-11-01T09:14:57.000298Z；hash/时间 [build-fix-hiredis-evidence.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/build-fix-hiredis-evidence.json)。5个Go互操作SKIP按没跑，名字 [build-fix-hiredis-skipped.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/build-fix-hiredis-skipped.txt)；RPC40因scene.lib门禁没有执行，不能称本节全部验证通过。

## 16 遗留修复与实际验证:失败

快照UTC `2026-11-02T07:48:52.266946+00:00`。本轮9条完成命令中，`16-token-real-redis72`、`16-token-real-redis8`实际exit1；`16-extra-checks` wrapper退出0仅说明调度结束，不能据此判本节通过。

真实命令/退出码/日志汇总 [aggregate-16-analysis.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-16-analysis.txt)；失败全部原文 [aggregate-16-failed-output.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-16-failed-output.jsonl)。未改逻辑、断言或配置，两个失败均未重试，交Claude。

Go协议分类（cwd=`tools/proto_generator/protogen`）：

```powershell
go test -json -gcflags=-e -count=1 -p=1 -timeout=60m ./internal/... -run TestClientRpcKinds
```

`16-generator-client-rpc-kinds` exit0，10个顶层PASS/0FAIL/0SKIP，含子测试60个PASS；这是13三目录没有覆盖的首次独立入口，没有再次运行协议生成。日志 [16-generator-client-rpc-kinds.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-generator-client-rpc-kinds.log)；范围依据 [13-client-rpc-coverage.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/13-client-rpc-coverage.txt)。生成物静态核对及message_id归因限制仍见 [16-generation-audit.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-generation-audit.txt)，不把相对HEAD差异当本轮新增。

真实Redis token（cwd=`go/login`，分别绑定本轮Redis7.2/8，REQUIRE=1、DB9）：

```powershell
go test -gcflags=-e -json -count=1 -p=1 ./internal/logic/pkg/token/ -run ^TestTokenFamily_RealRedis$
```

| 命令ID | 实际结果 | 完整日志 |
|---|---|---|
| `16-token-redis72-identity` / `flush` | 各exit0；精确本轮实例核验及FLUSHALL | [identity](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-token-redis72-identity.log) / [flush](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-token-redis72-flush.log) |
| `16-token-real-redis72` | exit1，顶层FAIL+子测试FAIL | [16-token-real-redis72.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-token-real-redis72.log) |
| `16-token-redis8-identity` / `flush` | 各exit0；精确本轮实例核验及FLUSHALL | [identity](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-token-redis8-identity.log) / [flush](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-token-redis8-flush.log) |
| `16-token-real-redis8` | exit1，顶层FAIL+子测试FAIL | [16-token-real-redis8.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-token-real-redis8.log) |

两版本同一断言原文（`family_test.go:1564`）：

```text
真实拒绝不得改写本例任意原件 <nil>
```

失败名在两后端均为 `TestTokenFamily_RealRedis/persistent-head-refresh`；父 `TestTokenFamily_RealRedis` 随子测试失败，无另一条独立断言，不重复记错误。该处代码检查`err != nil || value != before[key]`；日志没有具体key/字节差异，不臆测原因。两版本合计4个失败事件（2顶层+2子），无未终态。

Java构建修复后的合并suite `16-java-buildfix-unit-suite` exit0：26套新XML共255个实际PASS/0FAIL/0SKIP（authority_jdbc20、gateway_node235，包含Hotfix测试）。另14个外部真实前置方法没有运行，按没跑，不能说269全跑。报告CLI属性未绑定预期目录，实际从命令时窗内默认target的新XML按Maven Running类集合/逐类计数一致交叉归档；摘要 [16-java-buildfix-test-summary.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-buildfix-test-summary.json)，归档 [16-java-buildfix-reports](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-buildfix-reports)，完整命令日志 [16-java-buildfix-unit-suite.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-buildfix-unit-suite.log)；14个方法全名及原因 [16-java-not-run.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-not-run.txt) / [16-java-not-run.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-java-not-run.json)。

实际Maven选择（cwd=`java`）：

```powershell
mvn.cmd -pl gateway_node -am test "-Dtest=*,!AuthorityHikariTiDbTest,!ZoneWhitelistUpsertLockOrderMySqlTest,!ZoneConfigUpsertLockOrderMySqlTest,!LoginEndpointIntegrationTest,!GatewaySchemaContractTest#formalSchemaValidatesOnRealTiDbAndRoundTripsUnsignedZoneRange" -Dsurefire.failIfNoSpecifiedTests=false -Dsurefire.reportsDirectory=<本轮16-java-buildfix-reports> -Dprotoc.spec=file:/D:/luyuan/xuanming-server-mmo-cpp/third_party/protobuf/install_vs2026_dbg/bin/protoc.exe
```

其他Go/部署合同仅引用本轮03/07和相应postgen证据，不重试其中业务失败。MySQL帮会请求幂等8个实际PASS；TiDB同8个入口因源失败没跑；C++ EnterFenceEcs/SessionDetailsBuilder因scene.lib门禁依赖没跑，不能把EXE变新当当前scene已通过。完整没跑边界 [16-scope-not-run.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-scope-not-run.txt)；wrapper引用详情 [16-extra-checks.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-extra-checks.log)。

主代理第一次只读查看失败清单发生PowerShellParserError，解析阶段没有读取清单或启动测试；原工具回执保留。之后只是聚合既有日志，没有重跑测试，见 [16-readonly-parser-error-note.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/16-readonly-parser-error-note.txt)。此辅助错误不计为Go业务FAIL。

## 17 SQL路径故障控制器:没跑

独立编译检查已实际完成：`17-faultdrill-vet`、`17-controller-test-build`、`17-robot-test-build`均exit0；`17-compile-checks` wrapper也exit0。命令：

```powershell
# GOTOOLCHAIN=local；GOPROXY=off；cwd=go/data_service
go vet ./internal/faultdrill
go test -gcflags=-e -c -o <本轮r2>/bin/17-controller.test.exe ./internal/faultdrill
# cwd=robot
go test -gcflags=-e -c -o <本轮r2>/bin/17-robot.test.exe .
```

完整argv/退出码/日志 [aggregate-17-analysis.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/aggregate-17-analysis.txt)；各日志 [vet](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/17-faultdrill-vet.log) / [controller-c](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/17-controller-test-build.log) / [robot-c](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/17-robot-test-build.log) / [wrapper](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/17-compile-checks.log)。两个新二进制均存在，mtime/SHA证据 [17-compile-artifacts.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/17-compile-artifacts.json)。`-c`只编译，没有运行任何测试case，不能记场景PASS。

真实故障runner和两个顶层入口没有运行：scene门禁未过，且本轮已验收双区栈/scene身份、两个专用新账号及PROCESS等前置未齐。没有暂停/恢复或故障注入。完整未运行入口及子场景 [17-scope-not-run.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/17-scope-not-run.txt)；不得由二进制编译成功推导真实SQL故障或性能结论。

## 18 双区写者启动接线:失败

`18-go-svc-build`exit0。首次`18-infra-prepare`及原`18-real-startup`均exit1：Kafka错误为`java.lang.OutOfMemoryError: Java heap space`，容器ExitCode=1、OOMKilled=false、RestartCount=0；没有重启/重试。该JVM堆失败与§13 TiKV容器OOMKilled=true不是同一事实。
原件 [18-infra-failed-kafka-log.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/18-infra-failed-kafka-log.log)、[18-kafka-first-failure-state.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/18-kafka-first-failure-state.log)、[18-real-startup.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/18-real-startup.log)。独立Redis PING/persistence、etcd endpoint health、私有Redis Cluster INFO/check五条命令全exit0，仅为各自健康子项。

SQL两负例/catalog迁移注册发布不依赖Kafka，因此首次继续执行原先未启动的独立SQL步骤，没有重跑GoSvcBuild/infra/Kafka。前置核bootRoot不存在、mysql名称/14306空闲、未启动ID/log不存在；DbPlacement只核变量名/是否非空，管理环境finally恢复，口令仅内存经env传入。

新MySQL首次`18-mysql-start`exit0。53次`18-mysql-ready-log-001`～`053`均是成功的只读日志观察（各0），等正式3306就绪才首次SQL，不是失败SQL重试。`18-negative-schema-before`和`18-negative-processes-before`均0。`18-negative-no-allowlist`exit1却不是预期的白名单拒绝，实际负例失败，原文：

```text
Exception: D:\luyuan\xuanming-server-mmo-cpp\tools\scripts\lib\sql_boot_catalog_local.ps1:180
Line |
 180 |  … e.Value)) { throw "Cannot read $KeyPath from $Path; refusing to guess …
     |                ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
     | Cannot read SnapshotMySQL.DBName from
     | D:\luyuan\xuanming-server-mmo-cpp\tools\go\data_service\etc\data_service.yaml; refusing to guess.
```

实际配置路径是`tools/go/data_service/etc/data_service.yaml`，没有通过白名单拒绝验收；bootRoot仍不存在。没有改启动逻辑/配置或重试。同偏移负例、正例catalog生成和Inspect依赖同一配置读取，均没跑；negativePortCollisionExit/catalogExit/inspectExit实际为null，success=false。
结果原件 [18-independent-sql-result.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/18-independent-sql-result.json)、[18-independent-sql.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/18-independent-sql.log)。28个Go实例整体因Kafka和catalog前置不足全部没跑；没有LISTEN/Main36业务通过结论。

本节全部准确命令、cwd/完整argv/exit/起止/绝对日志（含全部53次观察及infra身份检查）逐条见 [18-command-index.txt](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/18-command-index.txt)；底层 [commands.jsonl](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/commands.jsonl)。下表合并同模板：

| ID/范围 | 退出码 | 完整日志（相对本轮绝对日志根） |
|---|---:|---|
| `18-contract-references` | 0 | `18-contract-references.log` |
| `18-go-svc-build` | 0 | `18-go-svc-build.log` |
| `18-infra-compose-up` | 0 | `18-infra-compose-up.log` |
| `18-infra-prepare` | 1 | `18-infra-prepare.log` |
| `18-real-startup` | 1 | `18-real-startup.log` |
| `18-kafka-first-failure-state` | 0 | `18-kafka-first-failure-state.log` |
| `18-infra-failed-kafka-log` | 0 | `18-infra-failed-kafka-log.log` |
| `18-infra-redis-ping` | 0 | `18-infra-redis-ping.log` |
| `18-infra-redis-persistence` | 0 | `18-infra-redis-persistence.log` |
| `18-infra-etcd-health` | 0 | `18-infra-etcd-health.log` |
| `18-infra-match-cluster-info` | 0 | `18-infra-match-cluster-info.log` |
| `18-infra-match-cluster-check` | 0 | `18-infra-match-cluster-check.log` |
| `18-independent-container-names` | 0 | `18-independent-container-names.log` |
| `18-independent-processes-preflight` | 0 | `18-independent-processes-preflight.log` |
| `18-mysql-start` | 0 | `18-mysql-start.log` |
| `18-negative-schema-before` | 0 | `18-negative-schema-before.log` |
| `18-negative-processes-before` | 0 | `18-negative-processes-before.log` |
| `18-negative-no-allowlist` | 1 | `18-negative-no-allowlist.log` |
| `18-independent-sql` | 1 | `18-independent-sql.log` |
| `18-final-kafka-topic-init-log` | 0 | `18-final-kafka-topic-init-log.log` |
| `18-mysql-ready-log-001`～`053` | 各0 | 对应同名`.log`，完整53条见18-command-index.txt |
| 其余infra盘点/旧容器精确身份检查/前置清理/状态观察 | 逐条实际退出码见完整索引 | 全部命令及日志保留于18-command-index.txt |

收尾：`99-cleanup-owned-fixtures`exit0，selected=21、stoppedRunning=17、errors=[]；containersDeleted=false、volumesDeleted=false。只停止精确核过的本轮自有运行容器，保留容器/卷/失败证据，未动已退出TiKV/Kafka。MySQL停机后的ExitCode=137且OOMKilled=false，不覆盖先前3306就绪和SQL实际结果，也不改称容器OOM。
清理原件 [cleanup-r2-result.json](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/cleanup-r2-result.json)、[99-cleanup-owned-fixtures.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/99-cleanup-owned-fixtures.log)、[99-stop-owned-running-containers.log](D:/luyuan/xuanming-server-mmo-cpp/build/player-store-verify-20261101-r2/99-stop-owned-running-containers.log)。本任务未在主仓执行git commit；所有native验证会话已真实结束。

状态:本轮跑完
