# 交接:客户端请求按会话保序(路由服水平扩展下的乱序修复,2026-10-15)

> 接手人先按 AGENTS.md「会话启动门禁」读完 `docs/PROGRESS.md` 等,再读本文与设计文档
> [client-request-ordering.md](../design/client-request-ordering.md)(权威设计,O1–O14 决策、§16 实施进度逐行记录)。
> 本文只回答三件事:**做到哪了、还剩什么、接手时要注意什么**。所有改动**都没有编译、没有跑测试**。
>
> **2026-10-16 续做状态**:§3.1(Java 后端截止)与 §3.2(保序冒烟三组 + guild 冒烟第 11 步)已改完,仍未编译、未测试;顺带的 Go friend / trade 自动提交单语句收口另起一条。结果、待拍板项、残留与 Codex 命令见 `docs/PROGRESS.md` 2026-10-16 条与设计 §16 同日行。§3.3 的验证顺序不变,在第 3 步、第 5 步、第 6 步各加上那条里的命令。
>
> **2026-11-11 收口状态**:六个区域(C++ gate lane、Go login 与建角截止分摊、Go friend / trade 锁等待与商城截止、Go match 权威 SQL 截止与整队开战、robot 通用分发、Java 截止传播与重定向回 1003)已改完,全部未编译、未测试;§3.4 的「login CreatePlayer 子预算」已定案、「本仓 Go robot 通用分发」已修。盘点结论、改动、两版定案、残留、需负责人知悉的行为变化与按顺序的 Codex 命令见 `docs/PROGRESS.md` 2026-11-11「客户端请求保序收口」条与设计 §16 同日行。

## 1. 问题与方案(一段话)

gate 每条请求随机选路由副本,路由服每条请求随机选业务实例,同一玩家「加入匹配 → 取消匹配」可能执行成「取消 → 加入」。
粘性路由、单副本、加锁都保证不了顺序(grpc-go 每条流一个 goroutine,go-zero 超时拦截器到期后 handler 继续跑)。
方案是业界标准的「每个 key 单一属主、串行处理,状态层再加版本 / 栅栏 / 幂等」,三层:

1. **gate 按会话的路由 lane**(C++):同一会话的路由类请求同时只有 1 条在途,排队 ≤ 32、等待 ≤ 5s,用 gate 自己发的 `gate_call_seq` + 会话代次 `session_incarnation`(SessionDetails 7 / 8 号字段,路由服原样回传)认领回包;节点摘除快速失败、断线 / 重定向收尾、主动过期扫描。只管经路由服的 gRPC 消息,scene 的 TCP 路径本来就有序。
2. **预算链**:目标服务 zrpc Timeout + 1000 ≤ 路由服 ForwardTimeoutMs(5000)< 路由服 Timeout(6000);路由服 Timeout + 2000 ≤ gate deadline(8000);gate deadline + 5000 + 1000 ≤ 客户端超时(15000)。业务在提交点用请求 ctx,截止后不再写。
3. **业务栅栏**:login 会话栅栏(按 gate 实例 + 会话代次的双边握手,防断线 / 离开游戏与 EnterGame 后台链交错)、match 跨会话复核、guild `expected_guild_id`(E2)。

Java 版(`D:\luyuan\xuanming-server-mmo`)同一语义:会话内后端请求合成一条 lane,client 路径写去掉 Redisson 重发,跨会话复核,E2 同批。

## 2. 已完成(全部未编译、未测试)

逐批的文件清单与要点见设计文档 **§16**(只追加的进度表,按时间顺序),这里只列结论:

| 批次 | 内容 | 状态 |
|---|---|---|
| A 预算链 | login / match Timeout 4000、部署门禁 `Assert-RouterForwardBudget` + Pester 用例 | 已改 |
| B match ctx | 建票 / 入队 / 删票 / 挑战 / 观战提交点用请求 ctx;AddObserver 结果未知时补发 RemoveObserver | 已改 |
| C gate lane | `router_lane.h`、`client_request_lane.{h,cpp}`、接线、回包桥、节点摘除、过期扫描、启动日志;多轮评审修复 | 已改 |
| D login 栅栏 | `go/login/internal/logic/pkg/sessionfence`、Login / Disconnect / LeaveGame / EnterGame 接线、双玩家收尾、入口续期 | 已改 |
| E1 match 会话复核 | JoinQueue 拒绝被取代的旧会话 | 已改 |
| E2 guild expected_guild_id | 两版服务端;proto 已改且生成物已有人重生成;**客户端仓未改** | 已改 |
| F Java | 会话后端 lane、lane 满 1008、排队超时 1003、login 失败信封、JoinQueue 复核、client 写 `retryAttempts(0)` | 已改 |
| R 冒烟 | `robot/ordering_smoke_scenario.go`:**只有 JoinQueue→CancelQueue 一组** | 部分(见 §3.2) |
| §6.1 逐服务证据 | team / friend / chat / trade / guild / login 截止后不提交,各补回归测试(Go 侧) | 已改 |
| O6 gate 级失败带 id | GM / battle / scene 未就绪 / RpcClient 未挂 / 直连解析失败 / 直连找不到节点 → 带请求 id 的信封(两版);Java 链路不受理也回信封 | 已改 |

两个仓的提交状态:本方案的不少文件已被别的会话的「工作区快照」提交顺带入库(mmorpg `ebccaf04d`、`54c9d775d`、`ce523171a` 等;Java `88b9e65`、`18c48b8`、`d57d11f`),其余仍是未提交改动。**提交前用 `git log -- <文件>` 与 `git diff` 分清归属**,工作区里同时有大量别的会话的改动(密码失败配额、敏感词、帮会申请配额、商城、muduo 传输、RPC0 等),不要一起带走或回退。

## 3. 剩余工作

### 3.1 Java 后端「截止后不提交」与误拒窗口(未开始,工作区无半截改动)

可直接交给一个实现代理,只改 Java 仓 game-friend、game-chat、game-trade、game-team、game-match、game-login(game-discovery 只读在线目录字段),**不含 game-guild、game-gate**:

1. gate 的会话后端 lane 在后端调用超时(game-gate 的 Dubbo 消费端超时,5000)之后就放行下一条;后端 handler 若仍在跑并在之后提交就会乱序(C++ 版 §6.1 同一问题)。PARITY 说「各后端自身预算已小于 gate 的 5 s(match 4.5 s、friend / trade 3.5 s)」,要逐个后端核实:预算是否覆盖整个 handler、提交点之前是否检查预算 / 截止、Redisson / JDBC 写是否会在预算之后发出(含 Redisson 超时重发)。缺的修,每个后端至少一条「截止已过 → 不提交」的测试。参考已做好的:game-guild 的 `GuildTx` 子预算检查(`GuildTxTest#请求预算在提交前用完_不提交_按依赖故障抛`)、game-match 的 `RedissonTicketStore.CLIENT_WRITE_SCRIPT`(`retryAttempts(0)`)。
2. game-login:`CreatePlayerHandler` 在 gate 截止之后是否还会提交;`LoginTokens.refresh` 的作废与签发是否原子(mmorpg 侧已改为一段 Lua `rotateRefreshScript`);EnterGame 后台链是否有与 C++ 同类的「已受理、断线后才绑定 → 玩家永久在线」竞态(设计 §14 第 3 条)。
3. game-match 在线目录残留条目误拒新会话:`QueueService` 的跨会话栅栏只在「条目存在且会话号或 gate 实例不同」时回 16020;旧会话条目残留(旧 gate 崩溃 / 新 gate `putAsync` 失败,TTL 60 s 内)时会误拒合法新会话。改为只有在线目录条目的 owner_epoch(或等价代次字段,先读 game-discovery `PlayerPresenceDirectory` 与位置记录的字段)不小于位置记录的 owner_epoch 时才判「被取代」,否则按缺席放行;补「残留旧条目 + 新会话 → 放行」「新条目 + 旧会话 → 拒绝」两条测试。

完成后在 PARITY.md 第 158 行(「客户端请求按会话保序」)末尾追加结果。

### 3.2 保序冒烟补齐(未开始,工作区无半截改动)

只改 `robot/ordering_smoke_scenario.go`、`robot/etc/ordering_smoke.yaml`(必要时 `robot/main.go`、`robot/config/config.go` 的已有分派与校验)。设计 §12 批次 R 要求流水线连发四组:JoinQueue→CancelQueue(已有)、ApplyJoinGuild→CancelGuildApplication、AcceptFriend→RemoveFriend、Login→EnterGame。复用 `robot/guild_smoke_scenario.go`、`robot/friend_smoke_scenario.go`、`robot/login.go` 的消息号、请求构造与读状态函数;账号避开现有冒烟号段(看各场景文件头注释),好友一组需要两个账号。每组:不等回包连发 → 等两条回包 → 查权威状态 → 与「后发的那条最后执行」比较;乱序时记失败并收尾。Login→EnterGame 的期望是 EnterGame 不被判「未登录」。遵守每号每秒 3 条的 gate 限频。可选:guild 冒烟加一步「带过期 `expected_guild_id` 的退帮被拒(kGuildNotInGuild)、新帮不变」(`robot/vendor` 的 guild.pb.go 已有该字段)。

### 3.3 验证(用户 / Codex 执行;Claude 不跑构建与测试)

按顺序执行,任何一步失败就停,保留输出:

1. **proto 重生成**(SessionDetails 7 / 8 号字段):仓库根目录 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`(生成器源码有变或没有工具时先 `proto-gen-build`)。确认 `cpp/generated/proto/common/base/session.pb.h`、`go/proto/common/base/session.pb.go`、`robot/vendor/proto/common/base/session.pb.go` 都出现 `session_incarnation` / `gate_call_seq`。guild 的 `expected_guild_id` 生成物已在工作区。
2. **C++**:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(必须串行 `/m:1`),0 error。`router_lane_test` 按 `cpp/nodes/gate/tests/router_lane_test.cpp` 文件头的独立编译命令(含 `-I cpp/libs/engine/core`)运行,12 例全绿。
3. **Go**:在 `go/login`、`go/match`、`go/guild`、`go/chat`、`go/friend`、`go/trade`、`robot` 各执行 `go build ./... && go vet ./... && go test ./... -count=1`。guild 真库与集成用例:设 `GUILD_TEST_MYSQL_DSN` 后 `go test ./internal/data -run 'TestInTxNoCommitAfter|TestDisbandGuildRefusal|TestDisbandGuild_|TestKickMember|TestLeaveGuild' -count=1 -v` 与 `go test -tags integration ./internal/logic -run 'ExpectedGuild|ManagementFlow' -count=1 -v`,必须是 PASS 不是 SKIP。
4. **部署门禁**:`pwsh tools/scripts/tests/k8s_deploy_contract.tests.ps1`,含 3 条 `Assert-RouterForwardBudget` 用例。
5. **Java 仓**:`java tools/ContractSync.java --mmorpg D:\luyuan\xuanming-server-mmo-cpp --check` 先看差异 —— **它会把 mmorpg 工作区里所有客户端契约改动一起带过去,包括别的会话未提交的,需要人确认** —— 确认后去掉 `--check` 同步;然后 `./mvnw -B clean install -DskipTests`、`./mvnw -B -pl game-gate,game-match,game-guild,game-robot -am test`;guild 真库用例加 `-Dgame.it.mysql=jdbc:mysql://127.0.0.1:3306 -Dtest='JdbcGuildStoreMysqlTest,GuildLockOrderMysqlTest' -Dsurefire.failIfNoSpecifiedTests=false`。
6. **冒烟**(按 AGENTS §6.2 清空 Redis / MySQL / etcd 与 Kafka offset,`GATE_CLIENT_RPC_ROUTER=1`):在 `robot/` 下依次跑现有 team / friend / guild / chat / battle / trade / channel / travel 冒烟,再跑 `.\robot.exe -c etc/ordering_smoke.yaml`,每轮必须通过。失败保留 robot 输出、gate 的 `[lane]` 汇总行、路由服 / login / match 日志。

### 3.4 待人拍板 / 需要授权

- **login CreatePlayer 子预算**:名义之和(发号 3s + 名字 3s + player:zone 3s + 种子 1s)超过 Timeout 4000。实际被 handler ctx 截在 4s、截止后不提交;收紧会牵动同名重试门槛(`DefaultReserveTimeout`),数值由 login 负责人定(设计 §5.3、§6.1 订正注里有一套参考分法)。
- **客户端仓 `../mmorpg-client`(未授权,未读未改)**:① 五个帮会写请求带 `expected_guild_id`,处理 14002;② gate 级失败一律是带请求 id 的信封,含 scene 未就绪时应答为 Empty 的移动号(MoveStart / MoveSync / MoveStop);③ Q4:正式客户端是否会不等回包连发有依赖的请求(设计 §11)。
- **直连模式非 lane 的 gRPC 失败桥**(`rpc_reply_bridge.cpp`)仍推 23:拿不到客户端请求 id,随 O7 / D61 删直连路径处理;要提前改需要接一路 `x-gate-client-request-id` metadata(接线方式见设计 §4.1 订正注)。
- **本仓 Go robot 通用分发** `robot/logic/handler/message_body_handler.go` 不看 `error_message`,错误信封的空 body 会交给类型化 handler(如 ListSkills 清空技能表);travel_smoke 的 GM 流程又依赖 handler 被调用来关闭等待通道,改它要连带调整。既有缺陷,未改。
- F29:pending 状态下析构 `grpc::CompletionQueue` 是否崩溃,未验证(阶段 0g)。

### 3.5 已另开的独立任务(设计 §7.2 顺带发现的缺陷,用户已在各自会话 / worktree 启动)

login_session 键按 uint32 撞号;gate 在 LeaveGame 后不清 playerId;帮会捐献 / 商店购买幂等键;RefreshToken 回包丢失后的安全重试;gRPC CQ 轮询遇 `!ok` 跳过剩余事件;镜像建场景在途标记与整理背包冻结。**注意重叠**:RefreshToken 任务与本方案在 `go/login/internal/logic/pkg/token/token.go` 上有交集(本方案已把轮换改为一段原子 Lua),gate playerId 任务会碰 `client_request_lane.cpp` 的入队身份快照,帮会幂等键任务会碰 `proto/guild/guild.proto`(字段号不要与 E2 的 2 / 3 号撞)。

## 4. 接手注意事项

- **共享工作区**:同一工作区有多个会话并行(muduo HTTP/2 传输、RPC0 异步回调、全局单例、存盘管线、收件箱等)。改任何文件前先 `git diff -- <文件>`,只做最小改动,绝不回退别人的改动。「C++ gRPC 迁移至 muduo HTTP/2」会话拥有 `SendRawViaRouter` 的 muduo 分支、`rpc_reply_bridge`、`node.cpp` 关机顺序;「C++ RPC 异步回调链式调用设计」会话已收尾 lane 相关文件(它把 `SendEnvelope` / `FindLiveSession` 导出给 scene TCP 回包路径共用,`[lane]` 汇总的 `incarnation_mismatch` 因此也包含那条路径)。
- **额度**:并行多个改文件的代理会撞 5 小时 / 周额度并留下半截改动。改文件的代理按批跑(每批约 3 个),提示里带「先 git diff、在半截改动上补完、不重复添加」的续做说明;代理失败后先 `grep -o '"name":"\(Edit\|Write\)"[^}]*"file_path":"[^"]*"' <workflow 目录>/agent-*.jsonl` 列出它实际改过的文件。
- **只追加**:`docs/PROGRESS.md` 与设计文档 §16 只追加新行,不改旧行(补充旧条目另起一条);设计正文可以加带日期的订正注。
- **规范**:Claude 不跑任何构建 / 测试 / 生成命令(AGENTS §10.1),交付写「未编译,待验证」并给命令;不手改生成产物;错误码只用导表生成的常量;两版同批(AGENTS §12),Java 侧结果登记 PARITY.md 第 158 行。
- 读旧版本文件时不要 `git show` 重定向到 `%TEMP%`。
