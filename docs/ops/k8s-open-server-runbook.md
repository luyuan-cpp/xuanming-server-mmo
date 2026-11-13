# K8s 开服操作手册

> 2026-10-15 按当前部署脚本修订。本文面向集群外玩家的生产部署;命令仅作操作说明,本次文档修订没有执行部署、推送或压测。中英文旧副本的操作步骤统一按本修订执行。

## 1. 前置条件

在仓库根目录操作,先确认目标集群、区服清单、镜像与入口规划。完整参数见 [部署目录说明](../../deploy/k8s/README.md),发布门禁见 [发布清单](release-checklist.md),入口与切换约束见 [集群外客户端入口](../design/k8s-client-entry.md)。

- 集群已具备 Ingress controller、DNS / TLS、可用的存储类,并已安装本例使用的 Agones。生产 battle 用 Agones 动态端口;普通 Deployment + hostPort 只作验证与回退。
- C++、全部启用的 Go 服务与 Java gateway 镜像都已构建、验证,并由有权限的发布者推到目标仓库。AI 不执行推送。Go 侧包括 `go_svc_image.ps1` 目录里的两个工具镜像:`mmorpg-writer-authority`(写者 Pod 的 init)、`mmorpg-db-migrate`(`db-migrate` Job)。
- 写者 Pod 回收 CronJob 用官方镜像 `registry.k8s.io/kubectl`,版本钉在 `-SqlWriterReaperImage` 的缺省值。集群拉不到外网时推到内部仓库,部署时给这个参数。
- 使用明确的 `ReleaseProfile=prod` 或 `staging`;默认 `dev` 不代表生产档。先完成该档位的 `release_preflight.ps1` 门禁。
- 发布环境通过受控方式注入密钥,不把值写进本文、Git 文件、命令历史或日志。部署脚本使用 `MMORPG_GATE_TOKEN_SECRET`、`MMORPG_INTERNAL_AUTH_SIGNING_KEY`(gate 会话身份签名私钥,64 个十六进制字符,与公钥列表 `MMORPG_INTERNAL_AUTH_PUBLIC_KEYS` 成对注入,在 `go/shared` 下执行 `go run ./cmd/identitykeygen` 一次生成一对,见 docs/design/gate-online-identity.md §4)、`MMORPG_BATTLE_TOKEN_SECRET`、`MMORPG_SPECTATE_TOKEN_SECRET`、`MMORPG_LOGIN_QUEUE_TOKEN_SECRET`、`MMORPG_GATEWAY_ADMIN_API_KEY`,以及 `MMORPG_MYSQL_USER`、`MMORPG_GATEWAY_DB_USER`(只是写进 ConfigMap 模板的用户名)、`MMORPG_REDIS_PASSWORD`。数据库口令不经部署脚本:每个槽位的账号与口令写进各自的 `runtimeConfigSecret` / `schemaConfigSecret`(见下文「运维预置的每 boot 配置」),route-tool 用 schema / 维护身份(见下文「route-tool 的身份」);2026-11-12 起部署脚本不再要求 `MMORPG_MYSQL_PASSWORD` 与 `MMORPG_GATEWAY_DB_PASSWORD`,设了也不读。HMAC 与管理密钥至少 32 字符,Redis 密码至少 12 字符(部署脚本校验),各槽位的数据库口令按不低于这个强度生成;用途不同的密钥不得复用。数据库账号必须已在目标实例正确创建并授权,各业务服务的其他专用凭据按其配置准备。
- `ZonesConfigPath` 要包含实际区服,不能把示例区服直接当成已审批规划;`ClusterId` 是部署级常量,已有环境不得随意改值。
- 写者闸门每个逻辑库一份([设计 §2](../design/horizontal-scale-logical-db.md))。`maintenance_drain.ps1 render-roles` 的计划按逻辑库各写一份:`authoritySchema` 和 `schemaDatabases` 都是这个库,`registrationRequestFile` 是这个库的 REGISTER;每个目录片的计划再加 `directorySchema: <全局库名>_00` 和 `directoryShard: true`(只有第 0 片有 `zone_merged_into`,合服工具的授权由 `directorySchema` 带出;`directoryShard` 带出 schema 账号种 `directory_shard` 的权限;K=1 时第 0 片就是 `<全局库名>`)。写者账号只要自己要写的库的闸门表权限(SELECT,LOCK_TOUCH 档再加 `writer_slot.lock_touch` 的 UPDATE),不再有跨库授权。唯一的例外是 data_service 在存档库的表:`player_store` 读写,`player_centre_database`、`user_accounts` 只读(空路由启动判据)。各服务的 `role_fence` 在本服务的库里,授权跟着本库的业务表走。流水库的计划要列全 `transaction_log`、`player_snapshot`、`rollback_audit_log`、`gm_recall_batch`、`gm_recall_op`、`gm_recall_seq`、`gm_recall_source`、`role_fence`,操作都是 `SELECT,INSERT,UPDATE,DELETE`。少了回收存储就起不来,`Store.Required=true` 时 data_service 拒启;`Recall.Enabled=false` 也一样,护栏要靠这个存储判挂起。
- **授权分两步,顺序不能反**。表级 `GRANT` 对不存在的表报 1146(MySQL、TiDB 都是),空库上只有 schema 账号的授权执行得了:
  1. 迁移之前,每个逻辑库跑一次 `maintenance_drain.ps1 -MaintenanceCommand render-schema-role -PlanPath <计划> -OutputPath <输出>`,把输出的 SQL 执行掉。计划只要 `authoritySchema`、`schemaPrincipal`、`schemaDatabases`,目录片再加 `directoryShard: true`。这时还没有 generation,也就没有 REGISTER 原件,所以不用 `render-roles`。`mmorpg` 同样要跑:2026-11-10 起它和其余逻辑库一样在全球权威入口上,表由 `gateway-schema` Job 建(§5);只有 dev 用集群内 MySQL 时由 `gateway_tables.sql` 建、不走这一步。
  2. 迁移之后(§5),`render-roles` 输出的 JSON 里 `operator`、`runtime` 两段再执行,它们都是表级授权。`schema` 段和第 1 步是同一份,不用再执行。
- 写者名册(`rosterFile`,格式 `SQL_WRITER_BOOT_ROSTER_V2`)的每条 entry 是一个**槽位**,下标就是 StatefulSet 序号(Pod 名 `<服务>-<序号>`,写者服务都是 StatefulSet):`requests` 里每个要写的逻辑库一份 REGISTER 模板,`authoritySchema` 是这个库,`protectedSchemas` 只有这个库,`writer` 里只写这个槽位的账号(`sqlPrincipal`)、`generation` 和 `state: ACTIVE`;`generation` 取这个库 `route_meta` 的值,由 `-migrate` 随机生成,所以要先跑迁移 Job 才能定名册,顺序见 §5。模板**不写身份**:`requestId`、`deploymentId`、`writerKey`、`instanceId`、`registrationDigest`、`registeredMs`、`bootSeq` 由 init 里的 `writer_authority` 每次启动现场生成,模板里写了就整份拒绝。也不带 `expectedEpoch`、`expectedCatalogRevision`:每次搬库或回滚 epoch 都变,收据记落地时的实际值。go/db 要写几个落点库就几份,`gateway` 写 `mmorpg`。注册账号的连接串指哪个库都行,`writer_authority` 按 REGISTER 的库名切换;所有库要在这个连接串的集群上。
- **每个槽位一个专用 SQL 账号**。同一名册的槽位之间、不同工作负载(含 `<svc>-canary`)之间、不同 zone 的名册之间都不许重复:同一账号上的两个 Pod 会互相换代,谁都写不稳。`k8s_deploy.ps1` 在 apply 之前核全部名册,有重复就拒绝。账号按上面的第 2 步授权,一个槽位授权一次,之后不随启动变。
- **槽位数就是副本数上限**。每次启动(滚动发布、写过之后崩溃被回收 CronJob 删掉、节点驱逐)都是这个槽位的下一代:新一代在每个库的注册事务里把上一代退休,名册不消耗,不用留余量([写者名册换代](../design/writer-roster-exhaustion-20261106.md) §5)。
  - `k8s_deploy.ps1` 要求槽位数 ≥ 副本数、≤ 1024,不够就在 apply 之前拒绝。
  - 扩容:先给名册加槽位(新账号按第 2 步授权,更新目录的 `rosterFile`、`rosterSha256`),再调副本数。名册 SHA 在 Pod 模板里,换名册会滚动这个服务。
  - 缩容:缩掉的序号没有下一代来换代,它们的最后一代按非 run 的 RETIRE 退休,不用人工。`k8s_deploy.ps1` apply 变小的副本数之后当场跑一轮收尾、等它报告 `pending=0` 才往下走(门禁同迁移 Job:staging/prod 恒等,dev 仅 `-WaitReady`),所以部署结束就可以开排空;部署打出「slot retirement ... did not finish」警告时才要等收尾 CronJob `sql-writer-slot-retirer-<StatefulSet>`(每 5 分钟一轮,兜底)。再扩回来的序号只注册新一代(§9「没有进程的写者」)。
- go/db 认领前调 data_service 的 `GetPlayerRoute` 选库,不连目录([设计 §7](../design/horizontal-scale-logical-db.md)):配置 `DataServiceRpc` 必填,两边都要有 `MMORPG_ROLE_FENCE_SECRET_DB`,缺了 go/db 拒绝启动。
- **TiDB 档先开 `tidb_enable_check_constraint`,再建库**:`SET GLOBAL tidb_enable_check_constraint = ON;`。`player_route` 的唯一性靠 CHECK,TiDB 默认不执行,建完表才打开不一定补上约束。`data_service -migrate` 建表之前查这个开关,没开就拒绝。开了就不要再关。MySQL 档不用管。
- 目录按 `Directory.LogicalShards` 分片,流水和快照在 `<全局库名>_ledger`([设计 §1、§5.1](../design/horizontal-scale-logical-db.md))。`-DirectoryLogicalShards`:prod 必须显式 64,staging 必须显式(64 的约数),dev 缺省 1。K 上线后不能改。这些库 `-migrate` 之前要建好,要是空库;TiDB 档由运维预建片库 `<全局库名>_00..63`、`<全局库名>_ledger` 和各业务库(`mmorpg` 也在其中,显式写排序规则)。MySQL 档在 staging、prod 指向外部 MySQL 主库,库同样由运维预建;`infra-up` 的 `mysql-init-sql` 只在 dev 用集群内单实例 MySQL 时部署,staging、prod 把入口写成它会被拒绝(部署目录说明「`mmorpg` 库在全球权威入口上」)。
- 玩家存档在本地域的 K 个存档库 `player_store_<1000000 + 地域码×1000 + 片号>_db`(片号 0..K-1,[总体方案 §7.2](../design/player-data-tidb-scaling-plan.md)),和区无关。`-PlayerStoreShards`:staging、prod 必须显式(1..1000,TiDB 档初值 64),dev 缺省 1,上线后不能改。这 K 个库和各区的本区主库 `zone_<n>_db` 一样由运维预建、空库,`MMORPG_DB_ALLOWED_DATABASES` 要恰好列出这 K 个存档库外加各区主库:少一个、多一个(地域码或 K 填错)部署脚本都在写之前拒绝。dev 的集群内 MySQL 由 `mysql-init-sql` 预建 `player_store_1000000_db`。项目上线前切到这套落点时清库,不迁移旧的 zone 库存档。
- data_service 的 boot 名册(`rosterFile`)每条 entry 的 `requests` 要恰好是片库、流水库和 `MMORPG_DB_ALLOWED_DATABASES` 列出的全部存档库,一共 K+1+存档库数份,多一份少一份都拒绝启动。`k8s_deploy.ps1` 在 apply 之前核一次,对不上就拒绝。
- `-Region` 是地域码,写进 ConfigMap 模板的 `IdSegment.Issuer.Region`,同时注入 data-service 主容器的 `MMORPG_ID_ISSUER_EXPECTED_REGION`。常驻进程读的是 Secret 里抄的配置,抄错了 data_service 比对这个变量,拒绝启动。staging、prod 必须显式给 1..63,0 只给 dev。一旦发号不能改,不能和别的地域重复。开新地域见 [开新地域手册](open-new-region-runbook.md)。
- `-IdRegistryEtcd` 是全局号源登记处的地址(`host:port`,全世界共用一套),写进 ConfigMap 模板的 `IdSegment.Issuer.RegistryEtcd`。staging、prod 必须给,不能是本集群的 `etcd` 或 `etcd-id-secondary`;托管登记处的那一个集群用 `-HostIdRegistry`(`infra-up` 部署 `etcd-id-registry`,地址取本集群的 Service)。Secret 里的配置要抄上同样的 `RegistryEtcd`。见[发号器运维手册](id-issuer-runbook.md) §1、§2。
- 合服映射只在目录第 0 片,各服务经 `GetMergedZones` 读,不连目录。合服工具的写权限在操作员角色 `deploy/sql/authority_operator_role.sql` 里,不进运行账号。
- **运维预置的每 boot 配置要带上新段**。常驻进程的配置来自 `runtimeConfigSecret`,schema Job 来自 `schemaConfigSecret`,`k8s_deploy.ps1` 渲染的 ConfigMap 是模板:把 ConfigMap 里的 `Directory`、`RoleFence`、`Recall`、`SharedRedis`、`PlayerRedis`、`MethodTimeouts`、`DbPlacement`、`IdSegment.Issuer`(`Region`、两套本地 etcd、`RegistryEtcd`)、`DataServiceRpc`、`GuildRpc`、`MatchRpc` 抄进去,再填账号。两边不一致时以 Secret 为准,配置缺了服务拒绝启动。match 另有一项 `GlobalMatch.BattlePodNamespace`(2026-11-11,战斗服所在的命名空间,原样抄、不要改):漏抄不会拒绝启动,match 启动时打一行 ERROR `battle pod lookup disabled`,Agones 下战斗服崩溃后的对局就只能等截止才作废,见 [人工 ABORT 手册](match-formation-manual-abort-runbook.md) §9。密钥按来源分两种填:
  - 全服同值的密钥照抄模板,只写变量名,值由部署注入 Pod:共享 Redis 口令(各段 `Password: ""` 加顶层 `SharedRedisPasswordEnv`,口令非空时模板才有这一行)、gate 令牌与 login 的几把 HMAC 密钥(`Secrets.*.Env`)、match 的 `Spectate.*Env`、gateway 的 `"${变量名}"`。
  - 数据库口令是这个槽位(schema Job 是 schema 主体)自己的,直接写进 Secret 里配置的口令字段:db `ServerConfig.Database.Passwd`、data-service `SnapshotMySQL.Password`、其余 Go 服务 `MySQL.Password`、gateway `spring.datasource.password`;模板里这些字段是空串,账号字段是模板用户名,抄的时候换成这个槽位的。部署脚本不给任何 Pod 与 Job 注入共享数据库口令。staging、prod 的 `schemaConfigSecret` 必须带 `DbPlacement`,没有时 `-migrate` 拒绝运行(`MMORPG_DB_PLACEMENT_REQUIRED`):它靠这一段看库是不是在搬。go/db 的 `db-migrate` Job 同样注入;手工跑 `cmd/migrate -command up` 时自己设这个环境变量,配置文件同样带这一段。gateway 的 `runtimeConfigSecret` 从 `java-svc-gateway-config` 抄:JDBC URL 指向全球权威入口,`mmorpg.sql.backend` 跟 `-DatabaseBackend`(tidb 或 mysql),`mmorpg.run-mode: prod`,tidb 档 `mmorpg.sql.require-tidb: true`;login 的同理按 TiDB 入口重做。
- **schema Job 的账号**。目录条目的 `schemaPermissionSecret` 有两个键,必须是同一个账号:`principal` 所有 Job 都读(`MMORPG_SQL_SCHEMA_PRINCIPAL`);`dsn` 由 guild、mail、rank、gateway 的 `gateway-schema` 和 data-service 的 route-tool Job 读,它们的连接串走环境变量(gateway 的 `dsn` 指向 `mmorpg`;data-service 的 `dsn` 指向全局库所在的集群,route-tool 只取其中的账号、口令与地址,库名仍取配置)。所以 data-service 条目的这份 Secret 也要带 `dsn`。两边对不上时 Job 报 `权威SQL受限主体不匹配`。其余服务的账号在 `schemaConfigSecret` 的配置里(chat 是 `chat-authority-secrets` 的 `migration-dsn`,match 是 `domain-service-secrets` 的 `match-migration-user`、`match-migration-password`),同样要等于这个 `principal`。`schemaConfigSecret` 的键名是配置文件名(`db.yaml`、`data_service.yaml` 等),内容从 `-DryRun` 渲染的 ConfigMap 抄。
- **route-tool 的身份**(2026-11-12)。`data_service -route-tool` 不用配置里的账号,只用子命令的控制身份([设计 §4.2](../design/workload-secrets-via-k8s-secret.md)):
  - `tidb-prepare` 在 K8s 上经 `tools/scripts/k8s_route_tool.ps1` 以 Job 跑,运维机先设 `MMORPG_SQL_K8S_BOOT_CATALOG`(就是上面那份部署目录)。脚本取 `<namespace>/data-service` 条目的 `schemaPermissionSecret`,建 Job 之前先核这份 Secret 在本 namespace、`dsn` 与 `principal` 两个键都在(只读键名,跑脚本的身份要能 `get` 它),Job 再注入这两个键;目录取不到、Secret 不在或缺键都以 2 退出、什么都不建。
  - `freeze` / `unfreeze` 不进集群:在运维机上和 `maintenance_drain.ps1` 的 freeze / release 阶段一样设 `MMORPG_SQL_MAINTENANCE_DSN`、`MMORPG_SQL_OPERATOR_PRINCIPAL`、`MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256`,再跑 `data_service -f <data_service.yaml> -route-tool freeze …`(参数见 `go/data_service/internal/routetoolcli/cli.go` 的 usage)。`k8s_route_tool.ps1` 对这两个子命令直接拒绝。
- **login 的口令登录**(可选,2026-11-12,`k8s_deploy.ps1 -LoginPasswordAuth`,未运行;设计见 [去单点总纲](../design/no-single-node-horizontal-scaling-20261001.md) §6.5)。不开就跳过这一条,login 只有第三方与 access-token 登录。要开时按顺序:
  1. 在 `mmorpg` 所在的 SQL 集群建一个只读账号,只授 `GRANT SELECT ON mmorpg.user_accounts TO '<账号>'@'%'`,别的权限都不给。全部 zone 的 login 共用它;它不是写者身份,不进写者名册。
  2. 在每个 zone namespace 预置一个 Secret,键 `dsn` 的值是 `<账号>:<口令>@tcp(<mmorpg 的 SQL 入口>)/mmorpg[?参数]`(go-sql-driver 写法,库名必须是 `mmorpg`,参数里的 `/` 写成 `%2F`)。名字自取,不要与这个 login 条目里别的 Secret 同名;用 `kubectl -n <zone-ns> create secret generic <名字> --from-file=dsn=<只有一行连接串的文件>` 建,文件末尾不要带换行,连接串不进命令行(会进命令历史)。部署脚本不读、不建、不删它。
  3. 在 SQL boot 目录(`MMORPG_SQL_K8S_BOOT_CATALOG`)的 `<zone-ns>/login` 条目里加 `"passwordAuthSecret": "<名字>"`。只有 login 条目认这个字段,写在别的写者条目上会被当成未知字段拒绝。
  4. 用 `-DryRun` 渲染一次(会打一行 `would check that Secret <名字> ... has a non-empty key dsn`),把 `go-svc-login-config` 里的整段 `PasswordAuth` 原样抄进 login 每个槽位的运行配置(`runtimeConfigSecret`)。`DSNEnv` 是变量名,照抄,不要换成连接串。没抄进去,login 起得来但口令登录是关的。
  5. 部署带上 `-LoginPasswordAuth`(包装入口 `dev_tools.ps1 -Command k8s-zone-up` / `k8s-all-up` 同样给)。真跑时部署脚本在任何写操作之前核对目录字段和这个 Secret(只看键名与长度),缺了就整条拒绝。以后每次部署都要照传:漏传会在写操作之前被拒绝。
  6. 账号用 `go/login/cmd/password_admin -create` 开通(用法见 `docs/design/auth-provider-framework.md`「开通口令账号与写入口令」)。玩家自助注册没有开放。
  - 换口令:先在 SQL 上给这个账号设新口令,改 Secret 的 `dsn`,再 `kubectl -n <zone-ns> rollout restart statefulset/login`,滚完再废掉旧口令。部署脚本读不到连接串,不会自动滚动。
  - 关掉:先从运行配置里删掉 `PasswordAuth` 段并滚动 login,再不带 `-LoginPasswordAuth`、加 `-ConfirmLoginAuthProviderChange` 部署;这个 Secret 是你的,部署脚本不删,不用了自己删。顺序反了,还在读 `PasswordAuth` 的 Pod 拿不到连接串会拒绝启动。
  - 开着时每个 login Pod 的内存请求与上限各多 `KDFConcurrency × 64Mi`(缺省 2 × 64Mi),节点容量按这个算。
- **预置 Secret `domain-service-secrets` 的角色护栏密钥**。值由运维生成,不进仓库、不进日志;每把至少 32 字节,互不相同(data_service 拒绝跨用途复用),同名 key 在各 namespace 同值。manifest 都是 `optional: false`,缺一个 key 对应的 Pod 卡在 `CreateContainerConfigError`:

  | key | 环境变量 | 放在哪 | 谁用 |
  |---|---|---|---|
  | `role-fence-coordinator` | `MMORPG_ROLE_FENCE_SECRET_COORDINATOR` | 每个 zone namespace、infra namespace | data-service 调参与者,六个参与者验 |
  | `asset-data-service` | `MMORPG_ASSET_OP_SECRET_DATA_SERVICE` | 每个 zone namespace | GM 批量回收的资产指令签名,data-service 签、scene 验,两边同值。`optional: true`:`-AssetOps off` 时用不到;`-AssetOps on` 时 data-service 缺它拒启 |
  | `login-admin-token` | `MMORPG_LOGIN_ADMIN_TOKEN` | 每个 zone namespace | `LoginAdmin` 的运维口令(`x-admin-token`),至少 16 字节。`optional: true`:没有这个 key 时运维面整体停用 |
  | `role-fence-guild` | `MMORPG_ROLE_FENCE_SECRET_GUILD` | 每个 zone namespace、infra namespace | guild 读权威状态,data-service 验 |
  | `role-fence-trade` | `MMORPG_ROLE_FENCE_SECRET_TRADE` | 同上 | trade |
  | `role-fence-mail` | `MMORPG_ROLE_FENCE_SECRET_MAIL` | 同上 | mail |
  | `role-fence-match` | `MMORPG_ROLE_FENCE_SECRET_MATCH` | 同上 | match |
  | `role-fence-player-inbox` | `MMORPG_ROLE_FENCE_SECRET_PLAYER_INBOX` | 同上 | player-inbox |
  | `role-fence-friend` | `MMORPG_ROLE_FENCE_SECRET_FRIEND` | 同上 | friend |
  | `role-fence-chat` | `MMORPG_ROLE_FENCE_SECRET_CHAT` | 同上 | chat 读权威状态(只读,不是参与者) |
  | `role-fence-db` | `MMORPG_ROLE_FENCE_SECRET_DB` | 每个 zone namespace | go/db 调 `GetPlayerRoute`,data-service 验 |
  | `match-internal-guild` | `MMORPG_MATCH_INTERNAL_SECRET_GUILD` | infra namespace | guild 和 match 同值:开局、占用、读终局都验它 |
  | `mail-producer-battle` | `MAIL_PRODUCER_SECRET_BATTLE` | infra namespace(mail)、每个 zone namespace(scene) | scene 签、mail 验,两边同值:两包都放不下的战斗掉落改投邮件([battle-overflow-mail.md](../design/battle-overflow-mail.md))。缺它 mail 与 scene 都起不来 |

  **2026-11-12 增补：首次发布之前必须补上 `mail-producer-battle`。** 它是两包都放不下的战斗掉落改投邮件（[battle-overflow-mail.md](../design/battle-overflow-mail.md)）的发件签名密钥，scene 签、mail 验。顺序如下：

  1. 在 infra namespace 与**每一个** zone namespace 的 `domain-service-secrets` 里各加一个 `mail-producer-battle`。各处值相同，至少 32 字节，由运维生成，不进仓库、不进日志。
  2. 再 apply mail（infra）与各 zone 的 scene。两边 manifest 都是 `optional: false`：key 缺失时对应 Pod 卡在 `CreateContainerConfigError`，起不来，不会带着空密钥跑。
  3. 核对 key 名（只列名字，不打印值）：`kubectl -n <namespace> get secret domain-service-secrets -o go-template='{{range $k, $_ := .data}}{{$k}}{{"\n"}}{{end}}'`。

  值不同，或两边时钟相差超过 5 分钟时，Pod 都能起来，但 mail 会拒绝验签：scene 侧是 ERROR `metric=mail_outbox_stuck reason=signature_rejected`（值班查询见 `deploy/k8s/asset-channel-alerts.yaml` 头注释），物品留在玩家存档的待寄队列里，不丢，但寄不出去。改成同一个值（或修好时钟）之后，按退避自动寄出。

  部署脚本（`k8s_deploy.ps1`）在任何写操作之前核对这一项：键缺失、不足 32 字节、或与 infra 不同，都会拒绝部署，一个都不写。`-DryRun` 只打一行说明。发布预检（`release_preflight.ps1`）只查配置与制品、不连集群，不查这一项。

- 参与者经 infra namespace 的 K8s Service 直连(`guild.<infra>:50300`、`trade.<infra>:50800`、`mail.<infra>:50900`、`match.<infra>:50500`、`player-inbox.<infra>:50950`、`friend.<infra>:50400`),`k8s_deploy.ps1` 按 `-InfraNamespace` 渲染。`-NetworkPolicies on` 时 guild、match 的放行名单已含 data-service。任何一个参与者不可达,过户和删角一律拒绝,其余功能不受影响。
- 告警装 `deploy/k8s/logical-db-alerts.yaml`,安装方式和抓取前提写在文件头。闸门冻结时长没有指标,要查库。

`-DryRun` 会输出部分 ConfigMap / Secret 的渲染内容;只用不含真实密钥的离线输入检查结构,不要把真实密钥的 dry-run 输出归档。

## 2. 准备 C++ 运行时产物

这些必须是 Linux 可执行文件,包括 gate、scene、battle,不能用 Windows `.exe`。暂存目录与构建路径见 [runtime 说明](../../deploy/k8s/runtime/README.md)。

```powershell
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-stage-runtime `
  -BinarySourceRoot "<Linux二进制目录>" `
  -ZoneInfoSource bin/zoneinfo -TableSource generated/tables
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-image-preflight
```

## 3. 镜像与版本

正式发布按 [发布清单](release-checklist.md) 出包并记录不可变 tag / digest。以下部署参数使用已推送、已验证的完整镜像引用。C++、Go、Java 的 tag 可以不同,但必须来自同一份已核对的发布清单。

`k8s-release-zone / k8s-release-all` 是 C++ 镜像构建 / 推送与节点发布包装,**不会部署 Go / Java 服务**。全栈发布使用下列 `k8s_deploy.ps1` 入口;不能只跑包装命令就宣称 login、router 与 gateway 已发布。

## 4. 明确本次部署参数

托管云示例使用每个 gate 序号各自的 LoadBalancer 与 DNS 名。替换全部占位后再运行:

```powershell
$ErrorActionPreference = "Stop"
$Deploy = @{
  KubeContext = "<目标 context>"
  KubeConfig = "<kubeconfig 绝对路径>"
  ReleaseProfile = "prod"
  OpsProfile = "managed-cloud"
  InfraNamespace = "mmorpg-infra"
  ZonesConfigPath = "deploy/k8s/zones.ops-recommended.yaml"
  ClusterId = 0
  # prod 必须是 64,上线后不能改。
  DirectoryLogicalShards = 64
  # 地域码 1..63,0 只给 dev。一旦发号不能改,不能和别的地域重复,见开新地域手册。
  Region = <地域码>
  # 本地域的存档库数 K(TiDB 档初值 64),上线后不能改。MMORPG_DB_ALLOWED_DATABASES 要恰好列出
  # player_store_<1000000 + 地域码×1000 + 0..K-1>_db 这 K 个库,别的库一个都不许有(zone 库已退役,写了直接拒绝)。
  PlayerStoreShards = 64
  # 全局号源登记处,全世界共用一套。托管它的集群改用 HostIdRegistry = $true。
  IdRegistryEtcd = "<登记处的 host:port>"
  NodeImage = "<镜像仓库>/mmorpg-node:<不可变 tag>"
  GoSvcRegistry = "<Go 镜像仓库前缀>"
  GoSvcTag = "<不可变 Go tag>"
  JavaSvcRegistry = "<Java 镜像仓库前缀>"
  JavaSvcTag = "<不可变 Java tag>"
  ClientEntryMode = "external"
  GateRouterMode = "1"
  BattleOrchestrator = "agones"
  GateClientHostTemplate = "gate-{ordinal}.{zone}.example.com"
  GatewayIngressHost = "gateway-{zone}.example.com"
  GatewayIngressClassName = "nginx"
  GatewayIngressTlsSecret = "<zone 内已创建的 TLS Secret 名>"
  GatewayTrustedProxies = "<Ingress controller 的可信 CIDR>"
  WaitReady = $true
}
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig get nodes
```

裸金属改为 `OpsProfile="bare-metal"`,gate 使用每序号 NodePort。每个 zone 的 `gateNodePortBase` 须在区服清单显式分配,范围不能重叠;DNS 模板必须让每个序号解析到实际可达的节点 / 独立 L4 入口,不能用随机分流到全部 gate 的单一入口代替。gate 票据绑定具体节点。

只设置 `OpsProfile` 或 `GateServiceType` 不会自动启用集群外入口。默认 `ClientEntryMode=podip` 只适合集群内 robot;podip 下 gate 副本数为 2 时连单一 `gate-entry` Service 都不会生成。生产示例必须显式保留 `external` 与每序号地址规划。

## 5. 先部署共享 infra 与全局服务

```powershell
& ./tools/scripts/k8s_deploy.ps1 -Command infra-up @Deploy
```

本步骤预建审计 / 命令 topic(dev 用集群内 MySQL 时还部署它和初始化 ConfigMap),并在 infra namespace 部署全局 Go 服务(包括 client-rpc-router)与 battle。必须给出 `GoSvcRegistry` 且不带 `SkipGoSvc`;留空会跳过 Go 服务,不是自动复用节点镜像。上例 `Gateway*` 参数在 infra-up 只会提示被忽略,它们在下一步 zone-up 随 Java gateway 生效。

全新集群上,全局服务里除 client-rpc-router 以外,本步骤只写 ConfigMap、跑迁移 Job,进程要等第一个 zone 的 data-service 就绪才能起(启动依赖见部署目录说明「全局服务的部署顺序」),输出里有告警 `[GlobalGoSvc]Deferred:`。第一个 zone 就绪之后再跑一次本节命令,它们才起来。

### 全新集群的顺序

名册要按迁移 Job 种下的 `route_meta.generation` 写,所以 Job 和进程分两遍跑。Job 重复跑是幂等的:已经齐全的库跳过,不换 generation。

1. 建库(TiDB 档先开 `tidb_enable_check_constraint`,见 §1)。按 §1 给 schema 账号授权(`render-schema-role`),预置 `schemaConfigSecret`、`schemaPermissionSecret`。
2. 写部署目录 `MMORPG_SQL_K8S_BOOT_CATALOG`。这一遍每个写者的条目只要 `schemaConfigSecret`、`schemaPermissionSecret` 两个字段;gateway 另要 `registrarImage`(按摘要固定):网关没有自己的迁移镜像,`gateway-schema` Job 跑它里面的 `writer_authority`。
3. 跑迁移 Job,不起写者进程:

   ```powershell
   & ./tools/scripts/k8s_deploy.ps1 -Command all-up -SqlSchemaOnly @Deploy
   ```

   它做的是:infra、全局服务的 ConfigMap 与迁移 Job,client-rpc-router 的进程,然后每个 zone 的 `db-migrate`、`data-service-migrate`,tidb 档再加 `gateway-schema`(建 `mmorpg` 的闸门表、区目录、公告、白名单和种子行;每个 zone 各跑一次,幂等)。`db-migrate` 迁 `MMORPG_DB_ALLOWED_DATABASES` 列出的全部存档库。这一步恒等 Job 结束,失败或超时就中断,看 `kubectl -n <namespace> logs job/<服务>-migrate`。只加一个 zone 时用 `zone-up -ZoneName <zone> -ZoneId <id> -SqlSchemaOnly`。
4. 逐库读 generation,目录各片、流水库、全部存档库、各业务库都要读,`mmorpg` 也要:

   ```sql
   SELECT generation FROM route_meta WHERE id = 1;
   ```

5. 写每个写者的 REGISTER 模板和槽位名册(§1,一个副本一个槽位、一个账号),`render-roles` 之后执行 `operator`、`runtime` 两段,把 boot 的 Secret 预置好,目录条目补全 `requestSecret`、`registrarSecret`、`registrarImage`、`physicalAuthoritySha256`、`runtimeConfigSecret`、`replicas`、`rosterFile`、`rosterSha256`(开口令登录的 zone,login 条目再加 `passwordAuthSecret`,见 §1「login 的口令登录」)。
6. 起进程,顺序由脚本排好(§6):

   ```powershell
   & ./tools/scripts/k8s_deploy.ps1 -Command all-up -SkipInfra @Deploy
   & ./tools/scripts/k8s_deploy.ps1 -Command infra-up @Deploy
   ```

   第一条跑一遍 Job(都是跳过),再起 zone 里的进程;第二条看到 data-service 就绪,起其余全局服务。也可以不分开,一条不带 `SkipInfra` 的 `all-up`。`@Deploy` 带 `WaitReady` 时第一条会等到 scene 就绪,而 scene 等发号器:等 `statefulset/data-service` 就绪之后另开一个终端做第 7 步的初始化,或者这一遍去掉 `WaitReady`。
7. 初始化发号器(§6.1),验收(§7),然后才放流量。

目录里缺名册时,不带 `SqlSchemaOnly` 的 `zone-up` 在写任何东西之前拒绝,报错点名这个服务。

先确认共享存储、topic 预建 Job、全局 router 与 battle 就绪,再发布 zone。gate 路由模式 1 依赖 router;只重跑 zone-up 不会补齐缺失的全局服务。存量环境的 etcd / Redis / Kafka 形态切换有单独迁移要求,先按部署目录说明审查,不得把第一次部署命令当成无影响升级。

## 6. 发布单个或全部区服

单区示例(名称、ID 必须与区服清单一致):

```powershell
& ./tools/scripts/k8s_deploy.ps1 -Command zone-up `
  -ZoneName yesterday -ZoneId 101 @Deploy
```

全部区服:已完成上一节 infra 时加 `SkipInfra`,避免无意再次更新共享资源。这两条命令都不起全局服务;全新集群上它们会告警 `[GlobalGoSvc]NotStarted:` 并点名没起的服务,这时再跑一次上一节的 `infra-up`。

```powershell
& ./tools/scripts/k8s_deploy.ps1 -Command all-up -SkipInfra @Deploy
```

`GoSvcRegistry / JavaSvcRegistry` 分别控制 zone 内 Go 服务与 Java gateway 是否部署;缺任一个都会跳过相应服务。生产档默认打开限流与登录排队,所需 queue token 密钥必须已注入。不要为绕过门禁改用 dev 档或 `SkipPreflight`。

### 6.1 初始化发号器

只在全新地域做一次,在第一个 zone 的 data-service 就绪之后、放流量之前。这个地域码还没在全局号源登记处认领时发号器拒绝发号,这是刻意的。整个世界第一次部署时,先按[发号器运维手册](id-issuer-runbook.md) §2.1 托管并开启登记处(`-id-registry-open -id-registry-next 1`)。

```powershell
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig -n <zone-namespace> `
  exec statefulset/data-service -- /app/service -f /app/etc/private/data_service.yaml -id-issuer-init
```

输出 `region code claimed in the id registry and both reservation records created` 就完成了,重复执行是安全的。码被别的环境认领过、已退役时拒绝,不写任何东西;已经发过号的地域丢了记录走 `-id-issuer-recover`,不要重新初始化。mail、rank 不因为它卡启动,它们的发号器预领失败只记日志。没初始化时建角和发号报错,scene 的号段门禁不放行,所以 zone 就绪不了。初始化之后不用重启任何服务,客户端按退避自动重试。详见[发号器运维手册](id-issuer-runbook.md) §2。

## 7. 部署后验收

```powershell
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-status `
  -ZoneName yesterday -KubeContext $Deploy.KubeContext -KubeConfig $Deploy.KubeConfig
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig `
  -n mmorpg-infra get pods
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig `
  -n mmorpg-zone-yesterday get statefulset,service,ingress
```

核对写者 Pod 实际读到的地域码。ConfigMap 只是模板,常驻进程读 Secret 里抄的配置,下面两个数都要等于这次的 `-Region`:

```powershell
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig -n mmorpg-zone-yesterday `
  exec statefulset/data-service -- grep -n 'Region:' /app/etc/private/data_service.yaml
kubectl --context $Deploy.KubeContext --kubeconfig $Deploy.KubeConfig -n mmorpg-zone-yesterday `
  exec statefulset/data-service -- printenv MMORPG_ID_ISSUER_EXPECTED_REGION
```

核对 `kubectl -n <namespace> get cronjob sql-writer-pod-reaper` 在每个 zone namespace 和 infra namespace 都有;`kubectl -n <namespace> get cronjob -l app=sql-writer-slot-retirer` 每个写者 StatefulSet(含 `<svc>-canary` 与 gateway)一份,名字 `sql-writer-slot-retirer-<StatefulSet>`。

核对 gate-0、gate-1 的独立 Service 与 DNS,以及 gateway Ingress / TLS;用授权测试账号验证 server-list、assign-gate、gate 长连和 battle 直连。确认下发的是客户端可达地址而非 PodIP。管理接口不经公开 Ingress,验收从集群内进行。具体场景与运行证据按对应冒烟文档记录,`WaitReady` 只证明 Kubernetes 就绪条件,不替代业务验收。

## 8. 回滚与停止服务

回滚要从上一份发布清单取出 C++ / Go / Java 三类镜像身份,先审查数据库迁移与协议兼容。保持本次所有 context、namespace、`external`、DNS、Ingress、可信代理与路由模式参数,修改 `$Deploy` 中相应镜像后,按上述顺序部署;只回滚 zone 不更新 infra 中的全局 router / battle。

这些参数不粘滞。漏传可能退回 podip,脚本会拒绝会踢人的形态或地址切换;只有确认维护窗口与迁移方案后才使用 `AllowDisruptiveSwitch`。

紧急关服使用明确的目标 context 与配置:

```powershell
pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-down -ZoneName yesterday `
  -KubeContext $Deploy.KubeContext -KubeConfig $Deploy.KubeConfig
```

zone-down 删除整个 zone namespace。全区关闭见 `all-down`;默认还会删除 infra namespace 及其 PVC,不能把它当成仅停业务进程的命令。只关闭 zone 时使用明确的 `SkipInfra` 与已核对的区服清单。

## 9. Troubleshooting

Common checks:

```powershell
kubectl get ns
kubectl get pods -n mmorpg-zone-yesterday -o wide
kubectl describe pod <pod-name> -n mmorpg-zone-yesterday
kubectl logs <pod-name> -n mmorpg-zone-yesterday --tail=200
kubectl get svc -n mmorpg-zone-yesterday
```

写者 Pod(db、data-service、login、gateway 和 infra namespace 的全局服务):

- **`Init:Error` / `Init:CrashLoopBackOff`**:看 `kubectl logs <pod> -c sql-writer-registrar --previous`。`pod <名字> is slot N but the roster has M slots` 是副本数比名册槽位多,先加槽位(§1)。`cannot register boot N of this slot yet` 后面跟 denied:这个槽位已经有同号或更新的一代,多半是节点失联之后强删过 Pod、旧节点上的 Pod 还在(见下面「节点失联」);确认旧 Pod 没了,删掉这个 Pod,StatefulSet 重建时取更大的号。`the boot record of this pod does not belong to this roster slot and pod` 是启动记录和名册对不上,同样删 Pod 重建。闸门表缺失是迁移 Job 没跑或没覆盖这个库,`generation` 不符是名册按错的库写的。这种 Pod 回收 CronJob 不删,告警是 `SqlWriterRegistrarFailing`。闸门冻结(排空、搬库)时 init 什么也不写,解冻后自己过去。
- **主容器崩溃、被 OOMKill**:写资格一次性,写过之后容器原地重启读到消费标记就拒启,入口脚本以退出码 78 退出,这个 Pod 自己恢复不了。`sql-writer-pod-reaper` 每分钟删一次主容器上一次以 78 退出的写者 Pod,StatefulSet 按同一序号重建,新 Pod 在同一槽位上换代、把上一代退休,名册不消耗。不用人工删。还没写过就退出的进程没有消费标记,原地重启,不删。看它删了什么:`kubectl -n <namespace> logs job/<job> -c select-restarted-writers`(删之前)、`-c delete-restarted-writers`(删之后),两边都写明 namespace 和 Pod 名。
- **没有进程的写者**:被删 Pod 的那一代由同序号的新 Pod 在换代时退休,不用人工。缩容被缩掉的序号没有下一代,由收尾 CronJob `sql-writer-slot-retirer-<StatefulSet>` 结束([写者名册换代](../design/writer-roster-exhaustion-20261106.md) §5.9):只结束「序号 ≥ 副本数、这个名字的 Pod 对象已经没了」的槽位的在役一代,非 run 的 RETIRE,用这个写者 registrar 的注册账号。看它做了什么:`kubectl -n <namespace> logs job/<job> -c retire-removed-slots`,每个候选一行(`slot`、`logical_db`、`boot_seq`、`result`),最后一行是汇总,`pending` 是被缩掉、这一轮还没能结束的在役一代;`result` 是 `pod-present`(还在优雅退出)、`gate-frozen`、`drain-run-open`、`registered-elsewhere` 的算 pending,`scaled-back-up` 不算(新 Pod 会换代)。
  - 归属:收尾只结束本 namespace 的工作负载登记的一代。注册器登记时把自己的 ServiceAccount 连同 UID 记进 REGISTER(`registeredBy`,形如 `k8s-sa:<namespace>/<ServiceAccount>/<UID>`),收尾读回来和本 namespace 现在同名 ServiceAccount 的 UID 比,对不上就不动它,记 `result=registered-elsewhere`,这一轮 Job 失败。看到它先查原因,不要直接人工退休:
    - 另一个集群或环境在用同一份名册和注册账号(备份恢复出来的副本、备用集群、误共用目录)。本集群若不该连这套库,停掉这里的 `sql-writer-slot-retirer-*` CronJob 并撤掉注册账号 Secret;反过来是对方不该连,就去对方那边停。
    - 这个 namespace 或它的 ServiceAccount 被删掉重建过(UID 变了),或者这一代是集群外登记的(本机脚本、调试栈,`registeredBy` 为空):确认登记它的进程确实没了,再按下面的人工 RETIRE 退休。
  - 缩容:`k8s_deploy.ps1` apply 变小的副本数之后按这份 CronJob 的模板当场起 Job(`sql-writer-slot-retirer-<StatefulSet>-d<秒>`),Pod 还在优雅退出就隔 10 秒再起一轮,直到汇总行 `pending=0`,预算 max(`-WaitTimeoutSeconds`, 180 秒),门禁同迁移 Job(staging/prod 恒等,dev 仅 `-WaitReady`;不等时只起一轮)。部署结束、没有警告,就可以直接开排空。
  - 部署打出「slot retirement ... did not finish」警告(闸门冻结、排空 run 开着、收尾失败)时,先别开排空:等 CronJob(每 5 分钟一轮,兜底)某一轮汇总行 `pending=0`,或者 `writer_registry` 里这些账号最新一行已是 REVOKED。被缩掉的末代没结束就开排空,P0 会把它们算进 ACTIVE 全集,还要按 run 人工退休。
  - 收尾一直不成功(告警 `SqlWriterSlotRetirerFailing`,30 分钟没有一次成功)时,按日志修好;修好之前要开排空,就人工按非 run 的 RETIRE 退休:原件的 `writer` 原样复制这一代 REGISTER 收据里的 `writer`(含 `bootSeq`),缺 `bootSeq` 的请求会被拒绝(同上 §5.2)。
  - 金丝雀收尾:`-CanaryServices` 不再列出某个写者时,部署脚本删掉 `<svc>-canary`,当场跑一轮它的收尾(不看门禁,要等结果),`pending=0` 之后自己删 `sql-writer-slot-retirer-<svc>-canary` 的 CronJob、ServiceAccount、Role、RoleBinding;没做完就留着它,CronJob 接着做,下一次部署再来一遍。删金丝雀的目录条目与 Secret 之前,确认这份 CronJob 已经不在(`kubectl -n <namespace> get cronjob sql-writer-slot-retirer-<svc>-canary` 找不到),否则它引用的 Secret 没了,每一轮都起不来。
  - 部署脚本手工起的 Job 不算进 CronJob 的历史上限,结束一天后由 `ttlSecondsAfterFinished` 自动删。
- **节点失联**:StatefulSet 不会在别的节点自动重建同序号的 Pod,要等节点对象被删,或者人工 `kubectl -n <namespace> delete pod <pod> --force --grace-period=0`。强删是安全的:新 Pod 换代之后,旧节点上的进程就算还活着,下一笔写也会被闸门拒绝(写不进去,不会写坏数据)。
- **Job 或 CronJob 起不来**:`sql-writer-pod-reaper` 的镜像拉不到时只是这一分钟没回收,不影响别的 Pod。`sql-writer-slot-retirer-<StatefulSet>` 用 registrar 的镜像,拉不到时这一轮不结束,下一轮再来。看 `kubectl -n <namespace> describe job <job>`。

C++ node business logs are **not** on stdout. Apart from gRPC / abseil / librdkafka's own
stderr, the only thing `kubectl logs` shows on a C++ Pod is gate's `[gate_version] ...` startup
line — and that line is **gate-specific**: scene and battle print no startup line at all, so their
stdout carries nothing but that stderr. muduo writes the business logs to files under
`/app/bin/logs/cpp_nodes/` on the shared `node-logs` volume. Two ways to read them:

1. Query Loki. An Alloy sidecar ships those files to the Loki in the infra namespace;
   port-forward instructions and label conventions are in
   [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §6.
2. Read the files in the Pod directly:

```powershell
# zone nodes (gate and the scene pools) live in the zone namespace
kubectl exec <pod-name> -n mmorpg-zone-yesterday -c <node-container> -- ls /app/bin/logs/cpp_nodes
# battle is a global pool deployed in the infra namespace, not in any zone namespace
kubectl exec <pod-name> -n mmorpg-infra -c battle -- ls /app/bin/logs/cpp_nodes
```

`<node-container>` equals the node name (`gate` / `scene`) only in the single scene-pool layout.
Under the scene role split the pools are `scene-world` / `scene-instance` and **the container name
is the pool name**, so `-c scene` fails with `container not found`. When in doubt, list them first:
`kubectl get pod <pod-name> -n <namespace> -o jsonpath='{.spec.containers[*].name}'`.
battle is deployed to the infra namespace (`mmorpg-infra` by default) and belongs to no zone, so in
Loki its `zone` label value is `global` rather than a zone name.

Only the 8 most recent rolled files are kept — an in-container cleanup loop deletes the rest,
because muduo rolls every 8MiB and never removes old files. Anything older than those 8 files
exists only in Loki.

Every C++ Pod also runs a **native sidecar** named `log-sidecar` (declared under `initContainers`
with `restartPolicy: Always`). It counts toward Pod readiness, so `kubectl get pod` reports one
container more than it used to (a gate Pod reads `2/2`). **If that image cannot be pulled, the
business container never starts at all**: the Pod STATUS is `Init:ErrImagePull` or
`Init:ImagePullBackOff` (search for those exact strings), the error appears under the
**Init Containers** section of `kubectl describe pod`, and the gate / scene / battle process has not
run even once. On kind, `grafana/alloy:v1.10.0` must be loaded into the node first. To rule the
sidecar out, redeploy with `-NoCppLogSidecar`: that removes only the sidecar container, its two
volumes (`cpp-log-sidecar-config` / `cpp-log-sidecar-state`) and the pod-template annotation
`mmorpg.io/cpp-log-sidecar-config-hash` (plus the `container: <pool-name>` line on the scene Agones
Fleet). The in-container log cleanup loop and the `node-logs` `sizeLimit: 2Gi` are rendered
**unconditionally** and stay exactly as they are. (C++ pods no longer carry a `snowflake-cache`
volume since 2026-11-12: the C++ snowflake slot client is gone.)

> Status (2026-09-19): this shape is **committed and passed an end-to-end run in a temporary
> namespace on a local kind cluster** (fake C++ node writing muduo-format lines + real Loki), but it
> has **never been applied to a real zone / infra namespace**. What was and was not verified:
> [grafana-loki-local-logs.md](grafana-loki-local-logs.md) §7.

If `WaitReady` fails:

1. Check image pull errors and registry credentials.
2. Check ConfigMap data and mounted runtime paths.
3. Check gate service exposure mode against cluster type.
4. Verify infra pods (`etcd`, `redis`, `kafka`) are healthy.

## 10. Change Record Template

Keep a short release note per operation:

1. Date/time and operator.
2. Command executed.
3. Image tag.
4. Zones affected.
5. Validation result.
6. Rollback decision and final status.
