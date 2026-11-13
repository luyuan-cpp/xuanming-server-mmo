# 工作负载密钥只放 K8s Secret(2026-11-10)

> 状态:按批落码中,**未编译、未测试**,验证交 Codex(本文 §7)。负责人 11-10 要求「按最标准的方式修复」交接
> `docs/handoff/handoff-global-singletons-p0-20261015.md` §9.6 记下的剩余明文密钥;共享 Redis 口令已在 11-06 那批改走 Secret `redis-auth`(同文 §9),
> 本文把其余密钥按同一套做法收口。Java 版(独立仓)不适用:它没有 K8s 清单,不往 PARITY 加行。

## 1. 目标与不变量

- 任何密钥都不出现在 ConfigMap 与 `k8s_deploy.ps1 -DryRun` 的输出里。密钥只放 K8s Secret,经 `secretKeyRef`(不写 optional)注入容器环境变量。
- 换密钥即滚动:每个读这把密钥的 Pod 模板带一条只哈希这把密钥的指纹注解(Helm 的 checksum 注解做法)。不哈希整份 ConfigMap,无关改动不滚动。
- fail-closed:配置写明从某个变量取、变量却没有值 → 进程拒启;Secret 缺失 → Pod 停在 `CreateContainerConfigError`。
- 最小权限:只给真正读这把密钥的进程注入。
- **写者不拿共享数据库口令**。db、data-service、login、gateway 与全局 Go 服务都是 SQL 写者:常驻进程读运维预置的 `runtimeConfigSecret`,迁移 Job
  读 `schemaConfigSecret`(`Add-SqlWriterBootManifest`、`Add-SchemaPermissionManifest` 把配置卷换成这两份 Secret),每个槽位一个专用账号,口令就写在
  那两份 Secret 的配置里(`docs/ops/k8s-open-server-runbook.md` §1)。部署脚本渲染的 ConfigMap 对写者只是运维抄写的模板,口令一律写空串;
  部署脚本不往写者 Pod 与迁移 Job 注入任何共享的数据库口令。全服同值的密钥(Redis 口令、HMAC 密钥)照常注入写者 Pod,模板里写变量名,抄进 Secret 时照抄。
- 各档位一条路径:dev 档的回落值也经 Secret 注入(与 `admin.api-key` 的既有做法一致)。dev 档生成物因此会变(ConfigMap 不再有这些值,工作负载多出 env 与注解),这是有意的;项目未上线,没有存量集群要兼容。

## 2. 盘点(11-10,`tools/scripts/k8s_deploy.ps1`;11-12 复核数据库口令的读者)

| 密钥 | 部署输入变量(= 容器环境变量名) | 以前明文写在哪 | 读它的进程 | namespace |
|---|---|---|---|---|
| gate 连接令牌 | `MMORPG_GATE_TOKEN_SECRET` | node-config 与 battle-node-config 的 `GateTokenSecret`;login 顶层 `GateTokenSecret`;scene-manager `GateTokenSecret`;gateway `gate.token-secret` | C++ gate(验)、C++ battle(只用来核「与 battle 令牌不同」)、login(签)、scene-manager(跨 zone 重定向时签)、gateway(CDN 签名) | zone;battle 在 infra |
| 观战凭证 | `MMORPG_SPECTATE_TOKEN_SECRET` | node-config `SpectateTokenSecret`;match `Spectate.TicketSecret` | C++ gate(验)、match(签) | zone;match 在 infra |
| battle 直连票据 | `MMORPG_BATTLE_TOKEN_SECRET` | battle-node-config `BattleTokenSecret` | C++ battle | infra |
| match 运营入口 | `MMORPG_MATCH_OPS_CALLER_SECRET`(可不给 = 入口关闭) | match `Spectate.OpsCallerSecret` | match | infra |
| 会话身份签名私钥(2026-11-11 起取代共用的 HMAC 验签密钥 `MMORPG_INTERNAL_AUTH_SECRET`,见 gate-online-identity.md §10.1) | `MMORPG_INTERNAL_AUTH_SIGNING_KEY` | 无(不进任何 ConfigMap) | 只有 gate,Secret `gate-identity-auth`(键 `signing-key`);login 与在线服务只拿公钥列表 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS`(不是密钥,明文 env) | zone |
| 登录排队令牌 | `MMORPG_LOGIN_QUEUE_TOKEN_SECRET`(排队开才有) | login `Secrets.QueueToken.Value` | login | zone |
| MySQL 部署口令 | `MMORPG_MYSQL_PASSWORD` | db `Database.Passwd`;data-service `SnapshotMySQL.Password`;trade / friend / player-inbox `MySQL.Password` | 只有 data-service 的 route-tool Job:它是唯一直接挂部署脚本 ConfigMap 的 Job。五个服务都是写者,常驻进程与迁移 Job 用 Secret 里的槽位 / schema 账号 | zone |
| gateway 数据源口令 | 11-12 起不再是部署输入(以前是 `MMORPG_GATEWAY_DB_PASSWORD`) | gateway `spring.datasource.password` | 没有读者:gateway 是写者,数据源账号与口令在 `runtimeConfigSecret` 里 | — |

C++ scene 读 node-config 但不用这几把令牌,不注入。用户名(`MMORPG_MYSQL_USER`、`MMORPG_GATEWAY_DB_USER`)不是密钥,仍写在 ConfigMap 模板里。

11-10 盘点时把五个服务的 MySQL 口令和 gateway 数据源口令也当成「工作负载要读的密钥」,第 4 批因此给写者 Pod 与迁移 Job 注入了 `mysql-auth` / `gateway-db-auth`。
11-12 复核写者的启动路径时发现它们根本不读这份 ConfigMap,注入的共享口令没有用处,还多给了一份不该有的凭据,第 7 批(§6)撤掉,只留 route-tool 这一个读者。

## 3. 统一做法

- **Secret 对象**:一把密钥一个单键 Opaque Secret,每个有消费者的 namespace 各一份同名副本(Secret 不能跨 namespace 引用),由部署脚本先于工作负载建好,
  经 `Apply-InjectedSecret` 写入(DryRun 只打一行隐去值的意图)。名字:`gate-token`、`spectate-token`、`battle-token`、`match-ops-caller`、`login-queue-token`、
  `mysql-auth`(只给 route-tool Job)。gate 的会话身份签名私钥用的是已有的 `gate-identity-auth`(只给 gate,见 §2 表与 §5.1),不在本表里。
- **容器环境变量名**与部署输入变量名相同:一把密钥全仓只有一个名字,运维看到 Pod 的 env 就知道它对应哪个输入。
- **ConfigMap**:原来写明文的位置写空串;Go 服务另写一个指名变量的键(见 §4.2);gateway 写 `"${变量名}"`。写者的数据库口令字段只写空串、不指名变量。
- **指纹**:`mmorpg.io/<Secret 名>-hash`,值是 `Get-InjectedSecretFingerprint`(用途串 `<Secret 名>/workload`)。
- **规则收在一处**:部署脚本里一张目录表列出每把密钥的取值、Secret 名与键、变量名、指纹注解和消费者;建 Secret、注入 env、写指纹都按这张表走,不在各服务的分支里各写一遍。

## 4. 各语言怎么读

### 4.1 C++

`cpp/libs/engine/config/config.cpp` 的 `readBaseDeployConfig` 对三把令牌加环境变量覆盖:变量非空就覆盖 yaml 的值,与 `ZONE_REDIS_PASSWORD` 同一模式,日志只记「已覆盖」不记值。
变量名是固定的(C++ 侧其余密钥也都是固定名,如 `MMORPG_INTERNAL_AUTH_SIGNING_KEY`)。缺变量时取 yaml(K8s 上为空串),生产档由各节点既有的空密钥校验拒启
(gate `ValidateGateTokenSecretOrDie`、battle `ClassifyTokenSecret`、gate 的观战密钥校验)。

### 4.2 Go

- 新包 `go/shared/secretenv`(由 11-06 的 `go/shared/redisauth` 泛化而来,`redisauth` 改为委托给它):`Fill(envKey, envName, lookup, targets...)`。
  配置里指名了变量才生效;变量缺席或为空、或目标字段同时写了字面量,都返回错误;错误文本只点键名与变量名。共享 Redis 口令的九个服务改用它,行为不变。
- 各服务新增的指名键(本机 `etc/*.yaml` 不写,行为不变):

  | 服务 | 新键 | 填哪些字段 | 解析时机 |
  |---|---|---|---|
  | data-service | `MySQLPasswordEnv` | `SnapshotMySQL.Password` | `data_service.go` 读配置后、分流到 `-migrate` 与各运维入口之前;`-route-tool` 自己读配置,在 `routetoolcli` 的 `conf.Load` 之后 |
  | scene-manager | `GateTokenSecretEnv` | `GateTokenSecret` | 加载配置后 |
  | match | `Spectate.TicketSecretEnv`、`Spectate.OpsCallerSecretEnv` | `Spectate.TicketSecret`、`Spectate.OpsCallerSecret` | 加载配置后、`Spectate` 校验之前 |

  `MySQLPasswordEnv` 只有 data-service 有:读它的是直接挂 `go-svc-data-service-config` 的 route-tool Job。写者常驻进程与迁移 Job 读 Secret 里抄的配置,
  口令写在配置里、不指名变量,解析调用对它们是空操作。11-10 第 2 批给 db、trade、friend、player-inbox 也加过同名键,没有任何读者,11-12 第 7 批删掉。
  `-route-tool` 在 `data_service.go` 的分流之前就转进 `routetoolcli.Run`,第 2 批只在主路径填了口令,route-tool 读到的是空口令;第 7 批在 `routetoolcli` 读配置之后补上。
- login 不改代码:`Secrets.*.Env` 早已支持(`go/login/internal/config/secrets.go`,指名了变量却没有值任何模式都拒启)。部署改写成
  `Secrets.GateToken.Env`(排队开时另有 `QueueToken.Env`),不再写顶层 `GateTokenSecret`(那是只读一个值、不能轮换的兼容入口)。
  11-11 起 login 的 `Secrets` 里不再有 `InternalAuth`:内部身份声明改由 gate 的 Ed25519 签名,login 只拿公钥列表(gate-online-identity.md §10.1)。

### 4.3 gateway(本仓 `java/gateway_node`)

ConfigMap 模板写 `gate.token-secret: "${MMORPG_GATE_TOKEN_SECRET}"`,由 Spring 解析占位;`spring.datasource.password` 写空串:gateway 是写者,运行配置是
`runtimeConfigSecret` 里抄的那份,数据源账号与口令填它这个槽位的,部署脚本不解析、不注入数据源口令。有两处 Spring 的行为要单独兜住:

- **宽松绑定**:`GateProperties` 是 `@ConfigurationProperties` 绑定,Spring Boot 对解析不了的占位**保留原文**,不像 `@Value` 那样拒启。
  变量缺失时令牌密钥会是字面量 `${MMORPG_GATE_TOKEN_SECRET}`;jar 内的缺省值又是公开占位串 `change-me…`。两种都等于拿公开常量签 CDN 链接。
  K8s 上 `secretKeyRef` 不写 optional,Secret 缺失 Pod 起不来,但绕过部署脚本时仍会发生,所以 gateway 自己在启动时校验(批 6,11-11):
  `GateTokenSecretGuard` 在生产档(`mmorpg.run-mode` 不是 dev / test)遇到空值、未解析的 `${...}`、公开占位串、短于 32 字节即拒启,dev / test 只告警;
  判定口径(占位前缀、32 字节下限)与 login 的 `go/login/internal/config/secrets.go` 一致,改一边要同步另一边。test profile
  (`src/test/resources/application-test.yaml`)写明 `mmorpg.run-mode: test`。数据源口令不另加校验:填错了 Hikari 启动时就连不上库。
- **递归解析**:Spring 解析出变量的值之后,会把这个值再当占位解析一遍(`PropertyPlaceholderHelper`)。值里有 `${x}` 就会被换成别的属性、
  或者解析失败,密钥被静默改掉。部署侧 `Assert-SpringPlaceholderSafe`(批 6,11-11)在部署 gateway 时、任何写操作之前检查 gate 令牌、
  共享 Redis 口令与管理面口令,值里含 `${` 即拒绝;单独的 `$` 不受影响。

## 5. 部署侧(批 4,`tools/scripts/k8s_deploy.ps1`;批 7 收窄数据库口令)

批 4 必须整批落地:ConfigMap 改成空串 / 变量名、工作负载注入 env、建 Secret、迁移清单与契约测试改写,缺一样部署就是坏的。

### 5.1 目录表与函数

- `$script:WorkloadSecretCatalog`([ordered],键 = Secret 名):`Env`(变量名)、`SecretKey`、`Value`(脚本块,写路径上 `Initialize-InjectedSecrets` 之后求值;
  返回空串 = 本次不注入,如排队关、运营入口没开)、`Consumers`(Go / Java 服务名,C++ 写 `cpp:gate` / `cpp:battle`,不由本脚本 apply 的 Job 写 Job 名)。
  条目:`gate-token`、`spectate-token`、`battle-token`(取值抽成 `Resolve-BattleTokenSecret`,与 node-config 生成器共用校验)、`match-ops-caller`、
  `login-queue-token`、`mysql-auth`(消费者只有 `data-service-route-tool`)。
- 写者的数据库口令不进表(见 §1):`Initialize-InjectedSecrets` 只解析部署账号 `MMORPG_MYSQL_USER/PASSWORD` 与 gateway 模板用户名 `MMORPG_GATEWAY_DB_USER`,
  不再解析 `MMORPG_GATEWAY_DB_PASSWORD`。
- 会话身份签名不进表(2026-11-11 起,gate-online-identity.md §10.1):
  - 私钥 `MMORPG_INTERNAL_AUTH_SIGNING_KEY` 只有 gate 引用,Secret `gate-identity-auth` 由 Apply-Zone 在 gate 之前建;
  - login 与 chat、friend、guild、trade、match、mail、rank 只拿公钥列表 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS`,由 `Apply-OneGoSvc` 明文注入;
  - login 的 ConfigMap 不再有 `Secrets.InternalAuth`。
- `Sync-WorkloadSecrets -Namespace -Consumer`:对这个消费者用到、值非空的条目 `Apply-InjectedSecret`;同一次运行里同一个 namespace 的同一把只建一次。
- `Add-WorkloadSecretsToManifest -SvcName -ManifestContent`:`Add-GoSvcSecretEnv` 注入 env,`Add-PodTemplateAnnotationEntry` 追加指纹。
- `Get-CppNodeWorkloadSecretEnvEntries -Role` / `Get-CppNodeWorkloadSecretAnnotations -Role`:C++ 三种工作负载形态共用(见 5.2)。
- 指纹键 `mmorpg.io/<Secret 名>-hash`,值 `Get-InjectedSecretFingerprint -SecretName '<Secret 名>/workload'`。

### 5.2 调用点

- Go 服务(`Apply-OneGoSvc`):渲染阶段在 `Add-SharedRedisAuthToManifest` 之后注入;集群写阶段在 ConfigMap 之前 `Sync-WorkloadSecrets`,各 Stage 都做(幂等)。
  部署 data-service 时另同步消费者 `data-service-route-tool`:route-tool Job 由 `k8s_route_tool.ps1` 另行起,挂 data-service 的 ConfigMap,Secret 跟着
  data-service 建在同一个 zone namespace。条件写成 `if ($script:WorkloadSecretCatalog)`:`k8s_migrate_gate.tests.ps1` 按 AST 单独抽这个函数跑,
  那里不定义目录表,条件恒假。
- gateway(`Apply-JavaSvcManifests`):同上,只有 `gate-token`。
- C++:`New-NodeDeploymentYaml` 按 `$NodeName` 取 env 行;`New-CppPodAnnotationBlock`、`New-CppNodePodCommon` 加 `-Role`(external gate StatefulSet、battle Fleet、
  battle hostPort Deployment 经 PodCommon 拿到同样的 env 与指纹);`Apply-Zone` 在 gate 之前同步 `cpp:gate`,`Apply-Infra` 在 battle 之前同步 `cpp:battle`。
  注解段的先后:sidecar 配置指纹、zone Redis 口令指纹、本角色的工作负载密钥指纹(目录表顺序)、scene 的资产与玩家域指纹、Prometheus 抓取注解。
- route-tool:`data-service-route-tool.yaml` 的主容器 env 静态写 `secretKeyRef {name: mysql-auth, key: password, optional: false}`(Job 每次重建,不需要指纹)。
  迁移 Job(`*-migrate.yaml`)不引用它:配置卷在部署时换成 `schemaConfigSecret`,账号是 schema 主体自己的。

### 5.3 ConfigMap

- node-config / battle-node-config:三把令牌写空串,生成期校验照旧(长度、分域、占位)。
- login:不再写顶层 `GateTokenSecret`;写 `Secrets.GateToken.Env`,排队开时 `Secrets.QueueToken.Env`(`Secrets.InternalAuth` 11-11 起取消,见 §4.2)。
- scene-manager:`GateTokenSecretEnv`。match:`Spectate.TicketSecretEnv`,运营入口开时 `Spectate.OpsCallerSecretEnv`。
- data-service:`SnapshotMySQL.Password` 写显式空串(不写会落回缺省值),顶层 `MySQLPasswordEnv: MMORPG_MYSQL_PASSWORD`(给 route-tool Job)。
- db / trade / friend / player-inbox:口令字段写空串,不指名变量。
- gateway:`spring.datasource.password: ""`、`gate.token-secret: "${MMORPG_GATE_TOKEN_SECRET}"`。

运维把这些模板抄进 `runtimeConfigSecret` / `schemaConfigSecret` 时:全服同值的密钥照抄变量名;数据库口令填槽位 / schema 账号自己的;data-service 的
`MySQLPasswordEnv` 删掉(规则写在开服手册 §1)。

### 5.4 契约测试(`tools/scripts/tests/k8s_deploy_contract.tests.ps1`)

- 改写断言明文值的用例(11-10 盘点约 20 条):scene-manager 与 node-config 的 `GateTokenSecret`、login `Secrets.InternalAuth`、prod db 口令、三处 GateTokenSecret 同值、
  `Secrets.QueueToken`(含「并进已有 Secrets: 段」那条)、`BattleTokenSecret`、`SpectateTokenSecret` 与 match `Spectate.TicketSecret` / `OpsCallerSecret`。
  改成断言「ConfigMap 里是空串或变量名、工作负载有 secretKeyRef 与指纹」。
- 新增:staging / prod 的 zone-up 与 infra-up 带全部密钥(每把一个独特值)的 DryRun 整份输出不含任何一个值;route-tool 清单的静态 env 与目录表一致;
  `Get-GlobalGoSvcNames` 输出有序、两处 Java 遍历排序(批 1 的回归)。
- 批 7:写者(db、data-service、gateway、trade、friend、player-inbox)与迁移 Job 一律不引用共享数据库口令;`mysql-auth` 在 zone namespace 只建一次、infra 不建;
  gate 的注解段逐行核对时允许本角色的工作负载密钥指纹(dev 档也有,回落值同样经 Secret 注入)。

## 6. 分批与状态

| 批 | 内容 | 状态 |
|---|---|---|
| 1 | 部署输出先后固定:`Get-GlobalGoSvcNames`、`Wait-ForZoneReady` 与 `Apply-JavaSvcManifests` 的 Java 循环按名字排序 | 11-10 已写回 main,未运行 |
| 2 | Go:`go/shared/secretenv`(`redisauth` 改为委托给它)与各服务指名键(§4.2),含单测 | 11-10 已写回 main,未编译 |
| 3 | C++:三把令牌的环境变量覆盖(§4.1),`configuration_table_test` 两条用例 | 11-10 已写回 main,未编译 |
| 4 | 部署脚本切换、迁移清单、契约测试(§5);顺带补玩家域 Redis 口令的 Go 服务指纹 `mmorpg.io/player-redis-auth-hash` | 11-11 已写回 main,未运行 |
| 5 | 部署文档(`deploy/k8s/README.md` 新节「工作负载密钥只放 Secret」)、交接 §9.6、PROGRESS | 11-11 已写回 main |
| 6 | §8 里原列为后续的两项现在补上:gateway 启动期校验 `gate.token-secret`(`GateTokenSecretGuard` + 单测,test profile 写明 test 档);部署侧拒绝含 `${` 的 Spring 密钥(`Assert-SpringPlaceholderSafe` + 契约测试) | 11-11 已写回 main,未编译、未运行 |
| 7 | 写者不拿共享数据库口令(§1、§2 末段):目录表删 `gateway-db-auth`、`mysql-auth` 只给 route-tool Job;五份迁移清单撤掉 `mysql-auth`;gateway 数据源口令写空串、不再解析 `MMORPG_GATEWAY_DB_PASSWORD`;Go 删 db / trade / friend / player-inbox 的 `MySQLPasswordEnv`;route-tool 读配置后补填口令(含两条单测);契约测试改写,并修第 4 批打红的 gate 注解段断言与 `k8s_route_tool.tests.ps1` 的口令断言;文档与开服手册的抄写规则 | 11-12 已写回 main,未编译、未运行 |

上线顺序:先发带新读法的镜像(批 2、3 都向后兼容:不指名变量时行为不变),再用新部署脚本(批 4、7)。反过来会让旧镜像读到空密钥。

## 7. 验证(Codex 执行)

批 1~3、7 的代码部分可以先验(不依赖部署脚本):

1. 格式:仓库根 `gofmt -l go/shared/secretenv go/shared/redisauth go/db go/data_service go/trade go/friend go/player_inbox go/scene_manager go/match` 应无输出。
2. Go(各模块目录分别执行,任一失败停下报告;与本批无关的红按文件归属单独报):
   - `go/shared`:`go vet ./secretenv/ ./redisauth/`,`go test ./secretenv/ ./redisauth/ -count=1 -v`(secretenv 5 条、redisauth 原有 7 条全 PASS)。
   - `go/data_service`:`go build ./...`,`go vet ./internal/config/ ./internal/routetoolcli/`;
     `go test ./internal/config/ -count=1 -run 'TestResolveMySQLPassword|TestExplicitEmptySnapshotPasswordStaysEmptyAfterLoad' -v`(4 条);
     `go test ./internal/routetoolcli/ -count=1 -run 'TestRunFillsMySQLPasswordFromTheNamedVariable|TestRunNamedMySQLPasswordVariableEmptyExitsTwo' -v`(第 7 批新增 2 条),
     再跑这两个包的全部用例,不应新增失败。
   - `go/db`、`go/trade`、`go/friend`、`go/player_inbox`:`go build ./...`,`go vet ./...`,`go test ./internal/config/ -count=1`(第 7 批删了 `MySQLPasswordEnv`
     与各自的 `mysql_password_test.go`,编译能过就说明没有残留引用)。`go/db` 另跑 `go build ./cmd/migrate`;`go/trade` 另跑 `go build ./cmd/assetopfix ./cmd/commercefix`。
   - `go/scene_manager`:`go build ./...`,`go test ./internal/config/ -count=1 -run 'TestResolveGateTokenSecret' -v`(3 条)。
   - `go/match`:`go build ./...`,`go test ./internal/config/ -count=1 -run 'TestValidateSpectateDefers|TestResolveSpectateSecrets' -v`(5 条),整包不应新增失败。
   - 九个用 `redisauth` 的服务各跑一次 `go test ./internal/config/ -count=1 -run 'TestResolveSharedRedisPassword'`:`redisauth` 改为委托,行为应不变。
3. C++:`cpp/libs/engine/config/config.cpp` 改了,先串行 `msbuild /m:1` 重编依赖它的库,再编 `configuration_table_test`(测试工程链接预编好的库,只编测试会跑旧代码),
   运行 `--gtest_filter=BaseDeployConfigEnvOverride.*:GameConfigEnvOverride.*`,新增 2 条与原有用例全 PASS。gate / battle / scene 只需随整解增量编译通过。

批 4、7(仓库根,PowerShell 7;先清掉 `MMORPG_*` 环境变量,dev 基线用例假定都没设):

4. 部署契约:`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`。本批相关用例全部 `[PASS]`:
   - 节「工作负载密钥只放 K8s Secret」:目录表与契约表一致(`mysql-auth` 只给 route-tool、`MySQLPasswordEnv` 只有 data-service);迁移清单不引用共享口令、
     route-tool 清单静态引用 `mysql-auth`;prod zone-up 与 dev infra-up 两条「整份输出不含值、ConfigMap 只写变量名、消费者注入并带指纹、写者不拿共享数据库口令、
     Secret 先于引用且只写一次」;玩家域 Redis 口令指纹;部署输出排序(批 1 的回归);纯函数 `Assert-SpringPlaceholderSafe`(三把密钥)。
   - 改写过的单项用例:scene-manager `GateTokenSecretEnv`、node-config `GateTokenSecret`、login `Secrets.*.Env`、prod db 不拿共享口令、四处 gate 令牌同一变量与指纹、
     `Secrets.QueueToken.Env`、battle 配置、node-config `SpectateTokenSecret`、`Resolve-SpectateTokenSecret` 的 dev 回落、match 的 Spectate 段与纯函数 `New-MatchSpectateYaml`。
   - 第 7 批修的两条 zone Redis 用例:「zone Redis 口令(prod)」与「zone Redis 口令(dev 基线,不注入)」逐行核对 gate 注解段时含 gate 的两条工作负载密钥指纹。
   - 本节有 1 个 DryRun 子进程(带独特 MySQL 口令的 dev infra-up)。
5. 回归,不应新增失败:`k8s_route_tool.tests.ps1`(第 7 批把「输出不含 password」改成「不含口令值、只经 secretKeyRef 引用」)、`k8s_migrate_gate.tests.ps1`
   (按 AST 抽 `Apply-OneGoSvc` 跑,目录表不定义,新分支应恒假)、`lobby_authority_render.tests.ps1`、`redis_player_cluster_contract.tests.ps1`、
   `k8s_client_entry_contract.tests.ps1`、`logical_db_deploy_contract.tests.ps1`、`k8s_gate_drain.tests.ps1`、`k8s_login_auth_provider_guard.tests.ps1`、
   `gate_identity_environment.tests.ps1`、`id_issuer_deploy_contract.tests.ps1`、`sql_writer_slot_retirer_contract.tests.ps1`(后两条读开服手册)。
6. 可选,要拉镜像,先征得负责人同意:kind 上带全部 `MMORPG_*` 密钥跑全新 `infra-up` 与一个 `zone-up`,确认 `kubectl get cm -A -o yaml` 里搜不到任何一个值
   (值从环境变量取,不敲在命令行上),各服务与 C++ 节点正常起来;再只换一把密钥重跑,确认它的消费者滚动、其余不动。

批 6:

7. gateway(仓库根,按本仓 Java 工程的既有口径):`mvn -f java/gateway_node/pom.xml -Dtest=GateTokenSecretGuardTest test`(5 条全 PASS),
   再跑整套 `mvn -f java/gateway_node/pom.xml test`,不应新增失败。重点看 `LoginEndpointIntegrationTest`、`GatewayTracingIntegrationTest`:
   它们用 test profile 起上下文,profile 里的 `gate.token-secret: test-secret` 是占位值,靠 `mmorpg.run-mode: test` 只告警;红了先查 profile 是否生效。
8. 部署契约:第 4 步那套里的「纯函数 Assert-SpringPlaceholderSafe」,应 `[PASS]`。

## 8. 后续与风险

- gateway 启动期校验令牌密钥:11-11 已补(批 6,§4.3)。
- 经 Spring 占位交给 gateway 的密钥里含 `${`:11-11 起部署时拒绝(批 6,§4.3)。
- 内部调用方验签的滚动:11-11 起已不是问题。验签方只拿 gate 的公钥列表,它是明文 env,写在各服务的 Pod 模板里;换密钥对时公钥一变,
  模板就变,各服务随之滚动。gate 自己的模板里同样有这份公钥列表,私钥换了也会滚动(gate-online-identity.md §10.1)。
- 玩家域 Redis 口令的 Go 服务指纹:批 4 已补(`mmorpg.io/player-redis-auth-hash`)。
- route-tool 的真执行(`tidb-prepare -dry-run=false`、`freeze`、`unfreeze`)按设计要 schema / operator 主体(`MMORPG_SQL_SCHEMA_PRINCIPAL`、
  `MMORPG_SQL_OPERATOR_PRINCIPAL`,freeze / unfreeze 还要 `MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256`),`data-service-route-tool.yaml` 不注入它们,账号也是 ConfigMap 里的
  部署账号。照现在的清单,K8s 上的 route-tool Job 只跑得了 dry-run。维护流程(`tools/scripts/maintenance_drain.ps1`)用环境变量里的
  `MMORPG_SQL_MAINTENANCE_DSN` / `MMORPG_SQL_SCHEMA_DSN` 与主体连库,`routetoolcli` 却从配置的 `SnapshotMySQL` 拼账号,两边口径不一,补齐要改
  `routetoolcli` 的连库方式与 Job 的凭据来源。这是路由映射那条线的遗留,不是本文的改动,11-12 记为待办,等负责人定在哪里做;做完 route-tool 不再用部署账号的话,
  `mysql-auth` 整个可以撤掉。
- 都只按代码与文档核对过,没有在集群上跑过(§7 第 6 步)。
