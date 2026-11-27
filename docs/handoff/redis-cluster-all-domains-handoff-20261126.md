# 全部 Redis 上 Cluster(缺口第 34 条)验证交接(2026-11-26,交 Gemini)

设计稿:[redis-cluster-all-domains-20261110.md](../design/redis-cluster-all-domains-20261110.md)。§10 表里第 1~7 批的代码与文档都已写完,只剩验证;本单只排顺序与口径,具体命令在各批的 PROGRESS 条目里(按标题 grep)。只在 main 上做,不建分支;项目未上线,Redis / TiDB 数据可以全清。

## 1. 顺序与通过标准

1. **协议生成**(第 3 批改了 `CreateSceneRequest.zone_id = 7`、`GameConfig.control_redis = 12`;`match_service.proto` 只改了注释):
   `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`,再跑 `pwsh -File tools/scripts/dev_tools.ps1 -Command go-svc-build`(不带 `-GoServices`,全部二进制)。
2. **Go 单测**(每个模块 `go vet ./... && go test ./... -count=1`,0 FAIL):
   - `go/shared`:`clusterredis`、`playerredis`(只剩别名)、`redisslot`、`assetopwake`、`leader`;
   - `go/scene_manager`(第 2a / 2b 批;公用夹具 `installSameTagGuard` 报红就是真跨槽,不许放宽守卫)、`go/login`(第 4 批)、`go/player_locator`、`go/player_inbox`、`go/data_service`、`go/friend`、`go/guild`、`go/trade`、`go/mail`、`go/match`(第 5 批与观战拆桶)。
   - 每批新增的用例名都写在对应 PROGRESS 条目里,核对它们出现在输出里且通过。
3. **C++**:`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64`(0 个 error C / LNK),再 `pwsh -File tools/scripts/run_cpp_tests.ps1 -Build`,至少 `-Filter rpc_controller_test`(`RedisSlotTest.*`、`RedisCommandRouteTest.*`、`RedisClusterClientTest.*`)、`currency_test`(`PlayerRedisRouteContract.*`)、`cross_zone_test`(`SceneOwnershipRules.*` 等)。交付前已有的只读预检照跑:`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`。
4. **部署契约**:第 6a / 6b-1 / 6b-2 批 PROGRESS 条目列出的 `tools/scripts/tests/*.tests.ps1` 全部通过;`k8s_deploy.ps1 -DryRun` 渲染核对(各服务只有 `ControlRedis` / `PlayerRedis` 两种集群段,C++ node-config 只有 `control_redis` / `player_redis`,gate / battle 没有 Redis 口令环境变量)。
5. **真集群集成**(本机 `deploy/compose.redis-cluster.yml`,8.10.2,全部句柄按 `tools/scripts/README.md` 同名一节指过去):
   - 登录排队 → 进场 → 换场景 → 离线;落地唤醒(SPUBLISH 64 片,每片只投各服务负责的副本);观战列表与随机观战;
   - 期间原子迁一段 slot(`docs/ops/redis-player-cluster-runbook.md` §4)、kill 一个主;
   - 通过标准:CROSSSLOT 为 0,`clusterredis` / C++ 守卫拒绝为 0,唤醒缺片告警 `AssetOpWakeShardsUnsubscribed` 不触发,迁槽后订阅自动重订。
6. **kind 冒烟**:`-PlayerRedisMasters 3 -ControlRedisMasters 3` 的 infra-up / all-up,建群 Job 核对 16384 个 slot,`k8s-redis-wipe` 逐主节点清干净;robot 跑一次进场与跨 zone 传送。
7. **容量实证**(最后做,按 AGENTS §6 压测纪律):3 主对 6 主施加同一份过量负载,`stress_summarize.ps1` 出 R1 / R2 逐主节点对比表;没有这张表不能说扩容有效。

## 2. 要先知道的

- 失败先看是不是别的会话的未提交改动引起的(`git status` 会有很多别的文件);本条的改动范围以各批 PROGRESS 条目的文件清单为准。
- 观战拆桶的生产代码 11-19 曾被整体提交 `9824368eb` 单独带进 HEAD,配套测试 `55eae6be3` 也已入库;若仍见 logic 包引用已删常量,说明拿的是那次提交的快照。
- 11-26 工作区里 `go/match/internal/logic/spectate.go`、`spectate_visibility.go` 的未提交改动是论剑 V7 高段排位公开观战(别的会话),不属本条;`go/match` 红在这两处时找那条线。
- 单 zone 的控制域与排队各在一个槽上是定案(设计 §6.2、§6.3):压测里单个主节点热不算缺陷,先看是不是同一 zone 的槽,�