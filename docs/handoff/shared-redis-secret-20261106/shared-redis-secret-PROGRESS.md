
## 2026-11-06 Claude:Go 服务与 gateway 的共享 Redis 口令改走 Secret redis-auth(未编译、未测试,待 Codex 验证)

- **做了什么**:C++ 11-05 已把共享 Redis 口令(`MMORPG_REDIS_PASSWORD`)从 ConfigMap 挪到 Secret `redis-auth`。本批把各 Go 服务与 Java gateway 也挪过去,口令离开所有 ConfigMap 与 `-DryRun` 输出。设计与理由见 `docs/handoff/handoff-global-singletons-p0-20261015.md` §9。
  - Go:ConfigMap 里共享 Redis 段的 `Pass` / `Password` 恒写空串,口令非空时末尾多一行顶层 `SharedRedisPasswordEnv: SHARED_REDIS_PASSWORD`。新包 `go/shared/redisauth` 在各服务迁移分支之后、建 Redis 客户端之前把变量值填进本服务所有共享 Redis 段;指名了变量却没有值、或字面量与变量同时给,都拒启。涉及 login、data-service、player-locator、scene-manager、match、trade、friend、player-inbox、guild,以及 guild 的 `cmd/assetopfix`。
  - gateway:`spring.data.redis.password` 只写 `"${SHARED_REDIS_PASSWORD}"`,不带缺省值,变量缺失时 `@Value` 解析失败、拒启。Java 代码不改。
  - 部署脚本:容器经 `secretKeyRef`(不写 optional)从本 namespace 的 `redis-auth` 拿 `SHARED_REDIS_PASSWORD`;Pod 模板带只哈希口令的 `mmorpg.io/shared-redis-auth-hash`,换口令即滚动(顺带修了「只改 Redis 口令时 login 不滚动」)。mail 早已走 Secret,只补指纹。新函数 `Add-PodTemplateAnnotationEntry`(往已有注解段末追加)、`Get-SharedRedisClientPassword`、`Add-SharedRedisAuthToManifest`。
  - dev 档没设口令时,所有生成物与改动前逐字相同。
- **取舍**:变量名用 `SHARED_REDIS_PASSWORD`,没用 `CORE_REDIS_PASSWORD`,因为同一口令也给 `redis-ctl-g<N>` 与 `redis-queue-g<N>` 用,而本仓库里 core 专指核心实例。Go 侧按仓库既有的 `*Env` 写法在配置里指名变量,不认固定变量名;不用 `conf.UseEnv()`。
- **上线**:项目未上线,首次部署整批起步。已有集群要先发 Go 镜像、再用新脚本部署,否则旧镜像按空口令连 Redis。
- **契约测试**:`k8s_deploy_contract.tests.ps1` 新增一节「Go 服务与 gateway 的共享 Redis 口令」(6 条,新增 1 个 DryRun),其中含「带口令的 prod DryRun 整份输出不含口令」。另改写 3b 节两条 gateway 用例(原先断言 ConfigMap 里有明文口令)。
- **验证**:Claude 只跑了 `gofmt -l` 与 PowerShell AST 解析(均无问题),没有编译、测试、DryRun。Codex 按交接 §9.5 执行:各模块 `go build` / `go vet` / `go test ./internal/config/`、`go/shared` 的 `go test ./redisauth/`、部署契约与 9 套回归,以及打补丁前后 dev DryRun 逐字比对。
- **没做**:集群上没跑过(Secret 注入、指纹滚动、Spring 嵌套占位)。同一批 ConfigMap 里 MySQL 口令、gateway 数据源口令、GateTokenSecret、InternalAuth、排队令牌仍是明文,属同类问题,建议另立任务。玩家域口令只换时 Go 服务不滚动(既有缺口)。
- **Java 版(AGENTS §12)**:核对 `D:\luyuan\xuanming-server-mmo`,`deploy/` 下没有 K8s 清单与 ConfigMap,不适用,不往 PARITY 加行。
- **文件**:`go/shared/redisauth/{redisauth.go,redisauth_test.go}`(新);`go/{login,data_service,player_locator,scene_manager,match,trade,friend,player_inbox,guild}/internal/config/{config.go,shared_redis_password_test.go(新)}`;`go/login/login.go`、`go/data_service/data_service.go`、`go/player_locator/player_locator.go`、`go/scene_manager/scene_manager_service.go`、`go/match/match_service.go`、`go/trade/trade.go`、`go/friend/friend.go`、`go/player_inbox/player_inbox.go`、`go/guild/guild.go`、`go/guild/cmd/assetopfix/main.go`;`tools/scripts/k8s_deploy.ps1`、`tools/scripts/tests/k8s_deploy_contract.tests.ps1`;`deploy/k8s/README.md`、`deploy/k8s/AGENTS.md`、`docs/design/gateway-k8s-deployment.md`、`docs/handoff/handoff-global-singletons-p0-20261015.md`、`docs/PROGRESS.md`(本条)。
