#requires -Version 7
<#
.SYNOPSIS
    Disaster-recovery rollback for a single zone — orchestrates
    `k8s-zone-down` → MySQL PITR (manual) → inventory → Kafka offsets to the log end (-ToLatest)
    → discard Redis state written after TargetTime (zone_rollback_discard) → `k8s-zone-up`.

.DESCRIPTION
    This script is the executable form of the SOP in
    docs/design/zone_data_rollback.md §3 "整 Zone 灾难恢复级回档".

    回档语义(负责人 2026-10-17 裁决:丢弃 TargetTime 之后的写):回档 = 回到 TargetTime 时刻 MySQL 里已经落库的状态。
    T 之后的写一律丢弃 —— 包括 T 之前产生、T 时还在 Kafka 里没落库的存盘(玩家看到的回退点比 T 早一个消费延迟),
    代价已接受:T 之后的合法存盘一并丢失。所以:
      - Step 5 把 zone 组拨到日志末端(-ToLatest),不再拨回 TargetTime:拨回 T 会让 [T, 停服] 之间从未落库的积压照常写进 PITR 之后的 MySQL;
      - Step 4 丢弃 Redis 里 T 之后派生的状态:本 topic 的重试收据与死信改名进隔离区,受影响玩家的缓存与提交屏障按 PITR 之后的库处置;
      - go/db 的已落库游标 consumer:applied* 与归属计数器 owner_epoch 保留不动:拨到末端之后新消息的 seq(= offset + 1)恒大于旧游标,
        不会被误判陈旧(Step 5b 由工具复核 offset 单调这一前提)。
    回档里不再有 FLUSHDB:K8s 与本机的 go/db 游标、重试收据、归属映射、账号目录与各 zone 的玩家缓存同在一个 Redis DB,
    FLUSHDB 会清掉所有 zone 的这些键、游标没了之后重放全部重新落库,等于事故(-ConfirmFlushSharedRedis 已改为入口拒绝)。

    It will NOT do the destructive steps without -Apply. Default is dry-run:dry-run 照样执行入口校验,其余每一步只打印将要执行的命令
    与作用范围,不调 kubectl / Kafka CLI / 回档工具,不读写 Redis / MySQL。

    读 MySQL、写 Redis 的动作全部在独立工具 zone_rollback_discard 里(go/db/cmd/zone_rollback_discard,与 go/db 共用键名函数;
    -RollbackDiscardTool 给与线上 go/db 同一提交构建的可执行文件,-Apply 必填)。**该工具尚未落地**,契约见下方「zone_rollback_discard 契约」;
    本脚本只编排 kubectl、Kafka CLI 与工具调用,不自己拼任何 Redis 键名。

    What it does (in order):
      入口校验(dry-run 也执行,任何改动之前):
        E1 -TargetTime 必须是 UTC 的 yyyy-MM-ddTHH:mm:ss[.fff]Z,且早于当前 UTC(改成 -ToLatest 之后 Kafka 不再顺带校验它);
           -PitrRestoredTo 同一格式,给了就必须等于 -TargetTime。
        E2 -ConfirmFlushSharedRedis 与 -RedisPassword 传了即拒绝:回档不再 FLUSHDB;口令只经 -RedisPasswordEnv 指名的环境变量,不上命令行。
        E3 -Apply 时必须显式给 -RedisHost(go/db 的共享 Redis,不按 db.yaml 缺省的 127.0.0.1 补)、-MySqlDsnEnv(该环境变量非空)、
           -RollbackDiscardTool(存在的文件)与 -BarrierProtocol;-SkipMySqlPause 必须配 -PitrRestoredTo。dry-run 缺这些时打印占位符。
        E4 原有校验(不可变镜像、Ingress 参数成对、db.yaml 的组名 / 分区数 / topic 世代、zone 组 ≠ legacy 组)全部保留;
           生成 rollback-id(z<id>-<T 紧凑格式>-<当前 UTC 紧凑格式>,-RollbackId 可显式给),报告目录 run/logs/zone-rollback/<rid>/。
        待负责人裁决的路径一律在入口拒绝:-BarrierProtocol strict(裁决 #3)、-AcceptBlockedPlayers(裁决 #4)。
      0. 预检(只读,先于停服;dry-run 只预告,不执行),a / b / d / e / f 都过了才进 Step 1(c 只告警):
         a. 集群现状:以下四项凡是留空的,都要与集群现状核对,一次 kubectl 读 zone namespace 里的
            gate(StatefulSet 或 Deployment,两种形态都认)、Ingress gateway 与 scene Deployment(第四项用;
            scene Fleet 是 CRD,集群装了 Agones 时另读):
              - -GateRouterMode 留空:gate 模板当前的 GATE_CLIENT_RPC_ROUTER 须与 k8s_deploy.ps1 的 -GateRouterMode 默认值一致;
              - -ClientEntryMode 留空:gate 当前形态(StatefulSet = external,Deployment = podip)须与 k8s_deploy.ps1 的
                -ClientEntryMode 默认值一致;
              - -GatewayIngressHost 与 -ConfirmNoGatewayIngress 都没给:namespace 里不能有 Ingress gateway
                (Step 1 会连它一起删,Step 6 留空不重建),且必须读得到 —— 一个 gate 都没有时(如上一次回滚中途放弃、
                namespace 已被 Step 1 删掉)判为读不到,不当成"确定没有"。
              - -NodeOpenFileLimitMode 留空(B0-4,2026-10-22):同一次 kubectl 另读 scene Deployment(scene / scene-world / scene-instance),
                集群装了 Agones 时再读同名 scene Fleet;gate 与 scene 模板里 NODE_OPEN_FILE_LIMIT_MODE 折算出的模式须与 k8s_deploy.ps1
                -NodeOpenFileLimitMode 默认值(空 = 不写,进程按 enforce)一致 —— 任一个带着 warn 就拒绝,报错给出照传的 -NodeOpenFileLimitMode warn;
                没有 gate、Agones 探测或 Fleet 读取失败同样判为读不到,报错要求确认后显式传 warn 或 enforce(不替操作者选值)。
            不一致或判不出即在停服之前拒绝,要求显式传参;显式传了的项不核对。
            login 第三方登录(微信 / QQ,2026-10-18):没给 -ConfirmLoginAuthProviderChange 时,再读 zone namespace 里的 login ConfigMap
            (AuthProviders 段)与 Secret login-oauth 是否存在,按 k8s_deploy.ps1 防误关闸的同一份判据(lib Get-LoginAuthProviderDrift)
            核对本次的六个 -Login* 参数:线上开着的会被关掉或改掉(漏传 AppId / Endpoint / AllowOpenId、换了 AppId)、读不到、认不出,
            或 namespace 里已没有 login ConfigMap 而存档里也没有现状,都在停服之前拒绝,报错给出照传现状的参数串。
            入口决定存档(2026-10-18,回档复核 [10]):第 0 步全部通过、Step 1 之前,把本次 Step 6 的入口决定(留空项取核对过的集群现状,
            显式项取参数值:路由模式、入口形态、Ingress host / class / tls / trusted proxies、地址参数、-RequireClientEndpoint、
            第三方登录的 -Login* 与 -ConfirmLoginAuthProviderChange)连同读到的集群现状与 login 第三方登录现状
            写进 <报告目录>/entry-state.json(Schema 3;第三方登录接入之前写的 Schema 1 存档照常读,只是第三方登录不按存档比较,
            namespace 已删时由上面的核对要求照传并加 -ConfirmLoginAuthProviderChange。Schema 3 起集群现状另记 nofile 模式,
            它不进入口决定、不按存档比较;更早的存档没有它,namespace 已删时 nofile 判为读不到,要求显式传 -NodeOpenFileLimitMode)。同一 rid 重跑时:namespace 里一个 gate 都没有(已被 Step 1 删掉)就改用存档的集群现状做上面的核对,
            不再要操作者凭记忆重传;本次的入口决定与存档逐项不同即在停服之前拒绝,重跑不能静默换掉入口。
         b. 静态预检:把 Step 6 将要传给 dev_tools.ps1 k8s-zone-up 的同一份参数加 -DryRun 执行一遍
            (k8s_deploy.ps1 -DryRun 只在本地渲染清单、不调 kubectl),非 0 即拒绝。参数组合、档位与密钥、发布门禁的
            规则只在 k8s_deploy.ps1 一处,这里不复制,拼错或矛盾的组合在停服之前就暴露。
         c. Kafka LAG 只读快照(2026-10-15):对 zone 组与 legacy 组各 describe 一次,只打印本 topic 的分区、位点与 LAG 合计,
            供操作者核对;任何失败只告警,绝不拒绝(取代原 Step 2 的排空等待,见下)。这些 LAG 将被 Step 5 -ToLatest 丢弃。
         d. Step 5 预演(2026-10-15;2026-10-18 改为 -ToLatest):zone 组在本 topic 上已有数值位点就跳过;否则(组不存在 / 没有位点)用
            kafka_offset_reset.ps1 的 dry-run 对 zone 组按 -ToLatest 生成重置计划,计划不完整或失败即在停服之前拒绝 ——
            Kafka 不能为 zone 组建出完整计划时,拖到 Step 5 才发现,zone 已停服、MySQL 已回档。
            describe 本身失败同样拒绝(判不出就无法证明 Step 5 走得通)。
         e. Step 6 的 Kafka 读法(2026-10-17,R7 复核 [2]):Step 6 的 k8s_deploy.ps1 经 kubectl exec 进 infra namespace 的 kafka-0
            读两组位点(lib Get-DbGroupDescribeExecArgs,同一组 -KubeContext / -KubeConfig;infra namespace 取 dev_tools.ps1 的默认值,
            Step 6 就用它),这里先用同一条路径读一遍:exec 失败(缺 pods/exec、kafka-0 未就绪)或判不出即拒绝;再逐组与本机 CLI
            (-KafkaBootstrap)比对「本 topic 上有没有已提交位点」,不一致(两条路径指向不同的 Kafka)同样拒绝。
         f. 部署事实与回档工具预检(2026-10-18):一次只读 kubectl 取 zone namespace 里的 go-svc-db-config、go-svc-data-service-config
            与 db / login Deployment,只折出非机密的拓扑与世代(go/db RedisClient 的实例与 DB、OwnerEpochRedis / Placement.Redis、
            data-service MappingRedis、Fencing.Mode、db 消费组形状 / 世代 / 分区数、ZoneId、镜像),口令一概不读。逐项核对:
            ZoneId、TopicGeneration、PartitionCnt 等于本机 go/db/etc/db.yaml(topic 就是按它推导的);db 消费组按 lib ConvertFrom-LiveDbConsumerGroup
            (k8s_deploy.ps1 判组用的同一份规则)归到四种形状,facts 记 Shape / Group / HandoffFrom,只放行 final(L)、final(Z)、handoff(Z←L)、
            handoff(L←Z)(L = 本机 db.yaml 的 GroupID 基名,Z = L_z<ZoneId>;本脚本 Step 6 渲染的就是 final(Z),回档过一次的 zone 都是它,
            回档复核 [0]);-RedisDB 等于 RedisClient.DB、
            OwnerEpochRedis / Placement.Redis / MappingRedis 与 RedisClient 是同一实例同一 DB(工具的每玩家 Lua 跨这些键原子执行)、
            -BarrierProtocol legacy 时 Fencing.Mode 必须是 legacy。存为 <报告目录>/deploy-facts.json;同一 rid 重跑而 namespace 已删时复用它,
            namespace 还在时与存档逐项相同才放行。随后两项身份核对(2026-10-18,回档复核 [4][5]):
              - Redis 实例:用同一组 -KubeContext / -KubeConfig kubectl exec 进 infra namespace 的 redis-0(口令取 Pod 自己的 REDIS_PASSWORD,
                不上本机命令行),对 deploy-facts 里 RedisClient 的地址读 INFO server 的 run_id,作为每次工具调用的 -expect-run-id;工具每个模式
                先读 -redis 的 run_id,不一致即以 3 拒绝 —— -RedisHost(可以是端口转发)与 -KubeContext 由此绑到 go/db 用的同一个实例;
              - 工具版本:<tool> -version 报出的构建提交必须以线上 db 镜像的 tag(git sha)开头,且两边都不是脏树构建;db 镜像以 digest 部署、
                tag 不是 git sha 时无从核对,同样拒绝。
            然后 <tool> -mode preflight。任一不过即在停服之前拒绝。
      1. `k8s-zone-down -ZoneName <zone>` — stop all writers(删掉整个 zone namespace),随后用 kubectl 确认 namespace 已不存在
         (dev_tools 的 Remove-Zone 允许失败、不报错,不能靠它的退出码判断停服成功)。
      1b. 停服后核对(2026-10-18,只读,先于 PITR;顺序是「先证明没有写者,再取基线」,回档复核 [1]):
          (i)   zone 组 --state 为 Empty / Dead 或组不存在;legacy 组在本 topic 的行里 CONSUMER-ID 全是 '-';
          (ii)  隔 -QuiesceSeconds(≥ 300s,不低于 C++ 生产者 librdkafka message.timeout.ms)两次读本 topic 的日志末端,必须完全相同;
          (iii) <tool> -mode stopped:Redis TIME 下本 topic 没有未过期的 go/db 重试租约(单调时钟截止,缺省 90s)、
                本 zone 归属玩家没有人仍在别的 zone 在线(陈旧 location 按 merge_zone 的判据剔除,handoff 另等 300s);都过了之后,
                最后一步写行指纹基线 <报告目录>/fingerprint-baseline.json(phase=stopped)。这时 namespace 已确认删除、两组在本 topic 上
                没有活跃成员、日志末端不动、租约已过期,zone_<id>_db 不再有写入;以前基线在第 0 步 f(停服前)取,go/db 一直在落存盘,
                只要 zone 上有玩家活动,停服时的行就与基线不同 —— 没做 PITR 或连错库也会被 4a 当成「有变化」放行;
          (iv)  打印两组的 LAG,按「zone 组在本 topic 上有位点就用 zone 组,否则用 legacy 组」给出本次将被丢弃的积压条数。
          任一不过即拒绝:zone 已停服,尚未 PITR,Redis / Kafka 尚未改动;错误文本给出用同一 rid 重跑或放弃(不带 -DbConsumerGroup 的
          zone-up,见 4a 的中止路径)的做法。
      2. (已删除,2026-10-15)原来是 zone-down 之后等 Kafka LAG 排空。zone-down 之后本 zone 的 db 已停,LAG 不可能排空,
         以前必然等满超时再「proceeding anyway」;而 Step 5 把位点拨到日志末端(丢弃积压),排空没有意义。
         改为第 0 步 c 的只读快照与 Step 1b (iv)。编号 3–7 不变。
      3. ⚠️ MySQL PITR is NOT automated — print runbook reference and pause
         for confirmation. Operator runs PITR manually, hits ENTER to continue.
         只回退 zone_<id>_db(及工具预检报告里列出的落点库),不回退全局库(data_service 的 id_segment / player_name / 路由表、guild、
         mail、收件箱、trade;回退 id_segment 会重复发号);先恢复到影子库再替换,回档前的原库改名保留到 Step 6 冒烟通过。
         -Apply 带 -SkipMySqlPause 时必须同时给 -PitrRestoredTo(= TargetTime),声明写进报告目录。
      4a. 盘点(只读):<tool> -mode inventory,与 Step 1b 的停服基线比较行指纹(没有任何玩家变化 = 没做 PITR / 恢复到影子库没换回 /
          连错库,拒绝)。拒绝(在线、落点在别处、合服标记、隔离区冲突、读失败)或盘点出 blocked 玩家时停在这里:
          Kafka 与 Redis 都还没动,中止路径 = 换回原库 → 不跑 Step 5 / 4b → k8s-zone-up **不带 -DbConsumerGroup**(keep:全新 namespace
          按 Kafka 位点落回回档前的 final(L) 或 final(Z),两组都有位点时 k8s_deploy.ps1 拒绝并给出收尾命令)。不用 Step 6 的
          -DbConsumerGroup zone:回档前还在 legacy 组上的 zone 会被渲染成交接形状 handoff(Z←L),db 镜像没有交接闸时 go/db 在
          conf.MustLoad 拒启,「退路」反而起不来,还顺带改了消费组归属(回档复核 [9])。
          4a 通过之后、Step 5 之前再复核一次停服状态(只读,回档复核 [8]):namespace 仍不存在(没被 GitOps / CD 重建)、zone 组仍不活跃、
          legacy 组在本 topic 上仍没有成员。PITR 往往要几个小时,这段时间里写者可能回来;不过即按上面的中止路径拒绝(Kafka 与 Redis 都还没动)。
      5. Kafka offsets on db_task_zone_<id>[_g<gen>]:zone 组(db_rpc_consumer_group_z<id>,-KafkaGroup 留空即按 go/db/etc/db.yaml 的 GroupID
         基名推导)拨到日志末端(kafka_offset_reset.ps1 -ToLatest),逐分区复核之后删除 legacy 组(-KafkaLegacyGroup,留空即该基名)在本 topic 上的位点。
         回档之后该 zone 一律以 zone 组为权威,仍在 legacy 组上的 zone 借此迁到 zone 组(docs/design/db-consumer-group-per-zone.md §6)。
         legacy 组即使还有别的 zone 的成员,只要没有成员订阅本 topic,broker 就允许删除;仍有订阅者时这一步失败。
         随后读 zone 组位点作为切点 O(每个分区 CURRENT == LOG-END)存入 <报告目录>/kafka-cut.json。
         从这一步起没有干净的中止路径(积压已丢弃,原库缺这批写),只能向前:失败时按打印的单步命令用同一 rid 重跑。
         拨到末端之后为什么不清游标:seq 是 go/db 消费时算的 offset + 1,切点之后的消息 offset ≥ O,旧游标 ≤ O,新写恒大于旧游标;
         保留游标反而在以后误投递时还能吸收重放。以后再做 -ToDatetime / -ToEarliest 重置不得越过本次的切点(kafka-cut.json),
         越过即把停服时那批积压重新落库 —— 目前只能靠文档禁止。
      4b. 丢弃 Redis 里 T 之后的状态:<tool> -mode purge -confirm,判定参数与 4a 同一份(-target-time、-fingerprint-baseline,给了
          -AcknowledgeNoPostTWrites 时连同 -acknowledge-no-post-t-writes;回档复核 [2]),工具内部先原样重跑 4a 的检查,任一不过什么都不写。
          放在 Step 5 之后(回档丢弃语义对抗复核 [4]):Step 5 记下切点之后,残留写者(他区 scene 保留的实体、晚到的本地队列消息)写进
          Kafka 的消息越过切点,写进 Redis 的状态由 5b 的 verify 逐个复核已处理玩家的键并重跑盘点归类(回档复核 [6]),都在 Step 6 之前被发现;
          verify 覆盖不到的写法见 docs/design/zone_data_rollback.md「2026-10-18 修订」的残余风险。同一 rid 续跑的规则见契约。
      5b. 复核:zone 组每个分区 CURRENT == LOG-END == O(切点之后本 topic 没有新消息),再 <tool> -mode verify(只读)。
      6. `k8s-zone-up -ZoneName <zone> -ZoneId <id>` — restart zone
         显式传 -DbConsumerGroup zone:Step 5 之后 zone 组有完整位点、legacy 组在本 topic 上没有位点,k8s_deploy.ps1 的决策表
         因此落到最终形状(db ConfigMap 写 `GroupID: "<zone 组>"`,不带 GroupHandoffFrom),任何镜像都能起。
         ⚠️ gate 路由模式不粘滞:k8s_deploy.ps1 每次部署都把 -GateRouterMode 原样重写进 gate env
         (默认 "1",turn-based §22 D75),不从集群读回旧值。该 zone 若以 "0" 回退运行,回滚时
         **必须传 -GateRouterMode 0**,否则本步把它静默切回 "1";回退的原因(如路由服不可用)
         若仍在,gate 卡在依赖门,登录 / 匹配全部 no_target。留空 = 不覆盖,沿用 k8s_deploy.ps1 默认值;
         -Apply 下留空要先过第 0 步预检,预检拦住的正是这种静默翻转。
         ⚠️ 集群外入口参数同样不粘滞(集群外入口 D80 / D88 / D91):以 external 运行的 zone 回滚时,须照传上一次部署的
         -ClientEntryMode external、-GateServiceType、-GateNodePortBase、-ClientPublicHost / -GateClientHostTemplate、
         -GateExternalTrafficPolicy,经 Ingress 对外的再加 -GatewayIngressHost / -GatewayTrustedProxies(及 ClassName / TlsSecret),
         逐个 zone 切 external 的窗口里还有 -RequireClientEndpoint。Step 1 已删掉整个 namespace,k8s_deploy.ps1 自己的
         集群现状预检(换形态 / 地址漂移 / 已有 Ingress)在这一步面对的是空 namespace,一条都触发不了 —— 所以核对前移到第 0 步。
         -AllowDisruptiveSwitch 只透传到本步,仅在 namespace 没删净、上述预检真的触发时起作用,不豁免第 0 步。
         ⚠️ 发布档位与开发口令认证同样不粘滞:staging / prod zone 必须传 -ReleaseProfile(留空按 dev_tools.ps1 默认 dev 档部署,
         login 以 dev 模式、密钥回落占位值拉起);dev zone 依赖开发口令登录时照传 -LoginDevPasswordAuth
         (共享口令仍走环境变量 MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET),漏传则 login 没有任何认证配置、Step 7 冒烟登录失败。
         ⚠️ 第三方登录(微信 / QQ)同样不粘滞:开着的 zone 回滚时照传 -LoginWeChatAppId / -LoginQQAppId(及 -Login*Endpoint、
         -Login*AllowOpenId),微信 AppSecret 仍走环境变量 MMORPG_LOGIN_WECHAT_APP_SECRET;漏传由第 0 步在停服之前拒绝。
         确要关闭或改动加 -ConfirmLoginAuthProviderChange(同时透传到本步)。
         ⚠️ B0-4 nofile 豁免同样不粘滞:以 NODE_OPEN_FILE_LIMIT_MODE=warn 运行的 zone 回滚时照传 -NodeOpenFileLimitMode warn,
         否则本步按 enforce 重建,节点池硬限仍不够时 gate / scene 拒启;漏传由第 0 步在停服之前拒绝。
      7. Print verification checklist

    Why MySQL is manual:
      Picking the right binlog cut point and validating against shadow DB is
      a judgement call no script can safely make. See
      docs/ops/mysql-backup-pitr-runbook.md §4.1 for the procedure.

    zone_rollback_discard 契约(go/db/cmd/zone_rollback_discard,尚未落地;落地时以本段为准,改一边必须同步另一边):
      公共参数(Get-DiscardToolArgs 拼,顺序固定):-deploy-facts <报告目录>/deploy-facts.json -zone <id> -topic-generation <gen>
        -redis <host:port> -redis-db <n> [-redis-password-env <环境变量名>] -expect-run-id <40 位 hex> -mysql-dsn-env <环境变量名>
        -rollback-id <rid> -barrier-protocol legacy -report-dir <报告目录> -mode <preflight|stopped|inventory|purge|verify>。
        口令与 DSN 只从这两个环境变量读,不上命令行、不进报告;topic 名按 go/db 的同一函数由 zone + 世代推导。
        键名一律由 go/db/internal/kafka 的导出函数生成(工具落地时把 retry_routing.go / retry_ownership.go / key_ordered_consumer.go 里
        现为包内私有的键名函数与 escapeRedisGlob 导出给工具用,工具里不另拼键名字面量)。
        Redis 实例身份(回档复核 [4]):每个模式开始时先读 -redis 的 INFO server,cluster_enabled 必须为 0,run_id 必须等于 -expect-run-id,
        否则以 3 拒绝、什么都不写。-expect-run-id 由本脚本第 0 步 f 经 kubectl exec 对线上 go/db RedisClient 的地址读出,
        把 -RedisHost(可以是端口转发)与 -KubeContext 指向的集群绑到同一个实例;deploy-facts 只说明 go/db 用哪个地址,证明不了 -redis 就是它。
      构建身份(回档复核 [5]):只带 -version 时打印恰好两行 commit=<40 位小写 hex> 与 dirty=<true|false> 后以 0 退出,commit 由构建时
        -ldflags "-X main.buildCommit=…" 嵌入。本脚本第 0 步 f 据此与线上 db 镜像的 tag 比对(Test-DiscardToolMatchesDbImage);
        preflight 另以同一规则复核 -deploy-facts 的 DbImage,不一致以 3 拒绝(键名与 Lua 契约只对同一提交的 go/db 成立)。
      退出码:0 通过;1 出错(可能只做了一部分,修好后同一 rid 重跑,幂等);3 拒绝(什么都没写);
        4 目标看起来没在用(只 preflight:本 topic 一个 consumer:applied:<topic>:* 都没有,-allow-empty-target 放行);
        6 有 blocked 玩家(inventory 即拒绝)。其它值一律按出错处理。
      判定参数(inventory / purge / verify 共用,本脚本只拼一份 $judgeArgs,回档复核 [2]):-target-time <T> -fingerprint-baseline
        <报告目录>/fingerprint-baseline.json [-acknowledge-no-post-t-writes]。三个模式对同一组参数做同一份判定,不各自取舍开关。
      续跑规则(preflight、purge、verify 与整脚本重跑同一条,回档复核 [3]):进度清单 rollback:quarantine:<zone>:<rid>:manifest 记 rid / zone /
        topic / 世代,以及每一轮 pass 的序号、起止时刻与分族计数。清单的 rid / zone / topic / 世代与本次一致时,
        rollback:quarantine:<zone>:<rid>:* 里的内容属于本次续跑,不算冲突;清单缺失却有隔离键,或清单与本次不一致,才算隔离区冲突(3)。
        隔离键名带轮次:rollback:quarantine:<zone>:<rid>:<pass>:<原键>。每次 purge 在清单里开新一轮(pass 从 1 递增),本轮只改名源键仍在的条目,
        目标键都带本轮 pass —— 第 2 页失败后同 rid 重跑,已改名的键不在源上、不会重复;5b 发现切点之后又有写入、单独重跑 Step 5 再跑 4b 时,
        重新长出来的同名源键进下一轮,不撞上一轮的目标。目标键已存在(清单与隔离区不一致)即停,以 1 退出。
      preflight(停服前,只读):Redis 连得上且确实是 go/db 的库(run_id 见上);本 topic 各族键 TYPE 正确;merge:in_progress / merged_into:<zone>
        不存在、merge:merged_into:* 里 dst = 本 zone 的一律拒绝(T 之后合入本 zone 的玩家会被当成 T 之后建角而清掉,负责人裁决 #6 之前
        fail-closed);隔离区按续跑规则判定;zone_<id>_db 连得上、SELECT DATABASE() 正确、两张玩家表可读;只靠 Redis 就能判出的拒绝
        (落点在别处、协议世代与 Redis 内容矛盾、他区在线、隔离区冲突)在这里先判一次。preflight 不取行指纹基线(停服前 go/db 仍在落库,
        那时的基线没有意义,回档复核 [1])。zone 维护围栏(负责人裁决 #1)落地之前必须以 3 拒绝。
      stopped(停服后,-lease-wait / -handoff-wait / -fingerprint-baseline):scene_nodes:zone:<zone>:load 为空;本 topic 的 kafka:retry:instances
        没有未过期租约(-lease-wait,单调时钟截止);player:<id>:location 按 tools/merge_zone/scene_hot_state.go 的判据区分陈旧与他区在线,
        只看 home = 本 zone 的成员,他区在线即拒绝;handoff 另按 -handoff-wait(≥ HandoffTTL 300s)等待。以上都过了之后,最后一步把受影响玩家
        两表的行指纹基线写进 -fingerprint-baseline,文件里记 phase=stopped、rid、zone、取样时刻(Redis TIME 与 UTC)。本脚本保证调用它时
        namespace 已确认删除、两组在本 topic 上没有活跃成员、日志末端两次读数相同。同一 rid 已有 phase=stopped 的基线时不覆盖
        (PITR 之后整脚本重跑仍要与停服时的基线比较);已有文件但 phase 不是 stopped、rid / zone 不同或格式不对,以 3 拒绝。
      inventory(PITR 之后,只读,判定参数 + -report):读 -fingerprint-baseline(缺失、phase 不是 stopped 即 3),重算两表行指纹,
        与停服基线相比没有任何玩家变化即拒绝(没做 PITR、恢复到影子库没换回或连错库),除非 -acknowledge-no-post-t-writes;
        归类表穷举,兜底项 unclassified 按 blocked 处理;post_t_candidate / home_elsewhere 占比超过阈值即拒绝。
      purge(判定参数 + -confirm / -report,回档复核 [7]):先原样重跑 inventory 的全部前置检查与判定(同一组判定参数),任一不过即 3、什么都不写。
        删除范围只限本 topic、本 zone:
          - SCAN 的 MATCH 一律是 escapeRedisGlob(<含 topic 的键前缀>) + 边界 + '*',边界紧跟 topic:按实例 / 按键的族是 ':',按分区 ready 是 ':p'
            (go/db retry_routing.go 的 retryPartitionReadyKeyPrefix 同一口径);只认前缀逐字相同、topic 后紧跟边界的键,
            db_task_zone_1 不得命中 db_task_zone_11 / db_task_zone_1_g2,topic 里的 glob 元字符按字面匹配;SCAN 循环到 cursor = 0;
          - 本 topic 的重试收据(共享 ready、按分区 ready、processing、实例登记)与死信 RENAMENX 进 rollback:quarantine:<zone>:<rid>:<pass>:<原键>
            (改名与 PEXPIRE 同一段 Lua),guard 键 UNLINK;
          - 每玩家一段 Lua(白名单:home = 本 zone 且落点 = zone_<id>_db,KEYS 带映射与落点并原子复核)删除父快照、子表缓存、sync_digest、
            资产提交证据与屏障,ZREM 本人的补投索引;-barrier-protocol legacy 时绝不写 save_barrier;绝不碰 consumer:applied*、映射、account:*;
          - 任何一页 SCAN、任何一段 Lua 出错即停止,以 1 退出;停止时与正常结束时都把分族计数(每族 quarantined / unlinked,玩家 processed /
            skipped)打印到 stdout 并写进 -report(purge.json)。purge.json 与清单累计全部轮次,已处理玩家名单是 verify 的输入。
      verify(判定参数 + -kafka-cut / -purge-report / -report,只读,回档复核 [6]):
          - 本 topic 各族为 0(同一 MATCH 规则)、64 个补投桶为空、没有存活租约;
          - 逐个复核 -purge-report(全部轮次累计,须与清单一致)列为已处理的玩家:父快照、子表缓存、sync_digest、资产提交证据仍不存在,
            legacy 协议下 save_barrier 同样不存在,补投桶里没有其成员;
          - 用同一组判定参数对本 zone 归属玩家重跑 inventory 的归类:出现任何仍需丢弃的 T 之后状态或 blocked 玩家;
          - 分页 SCAN consumer:applied:<topic>:*(同一 MATCH 规则),各分区最大 seq 不大于切点(否则 offset 回退过,新存盘会被判陈旧,须换世代)。
        任一项不过即 3,本脚本在 Step 6 之前拒绝。
      工具自己的测试至少覆盖:db_task_zone_1 与 db_task_zone_11、db_task_zone_1_g2 的键并存;topic 含 glob 元字符;SCAN 第 2 页失败
        (退出 1、分族计数正确、同 rid 重跑结果一致);第一次 purge 完成后源键又出现(同 rid 再跑 purge 成功,新条目进下一轮 pass);
        -expect-run-id 不一致;-version 的输出格式;stopped 已有非 stopped 阶段的基线。

.EXAMPLE
    # Preview what would happen
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -NodeImage ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha>

.EXAMPLE
    # Run the destructive flow(-RedisHost 是 go/db 的共享 Redis;口令与 DSN 只放在环境变量里,参数只给变量名)
    $env:MMORPG_ROLLBACK_MYSQL_DSN = '<zone_101_db 的 DSN>'
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -NodeImage ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha> `
        -RedisHost 127.0.0.1 -RedisPort 6379 -RedisDB 0 -RedisPasswordEnv MMORPG_ROLLBACK_REDIS_PASSWORD `
        -MySqlDsnEnv MMORPG_ROLLBACK_MYSQL_DSN `
        -RollbackDiscardTool <与线上 go/db 同一提交构建的 zone_rollback_discard> `
        -BarrierProtocol legacy `
        -KafkaBootstrap kafka:9092 `
        -Apply

.EXAMPLE
    # zone 以 "0"(gate 直连模式)回退运行时的回滚:必须显式传 -GateRouterMode 0。
    # 回退态不粘滞,Step 6 留空会落回 k8s_deploy.ps1 默认的 "1"(口径见 k8s_deploy.ps1 -GateRouterMode 参数注释);
    # -Apply 留空时第 0 步预检读到 gate 当前是 "0",在停服之前拒绝。
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -NodeImage ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha> `
        -RedisHost 127.0.0.1 -MySqlDsnEnv MMORPG_ROLLBACK_MYSQL_DSN `
        -RollbackDiscardTool <zone_rollback_discard> -BarrierProtocol legacy `
        -KafkaBootstrap kafka:9092 `
        -GateRouterMode 0 `
        -Apply

.EXAMPLE
    # 以 external 运行、经 Ingress 对外的 staging / prod zone:照传上一次部署时的档位、入口与地址参数(值以该 zone 的部署记录为准;
    # 第 0 步预检会把集群里 gate StatefulSet 现有的地址 env 与 Ingress host 打出来供核对)。
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -NodeImage ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha> `
        -ReleaseProfile prod `
        -KubeContext <目标集群 context> `
        -ClientEntryMode external -GateServiceType NodePort -GateNodePortBase 30100 -ClientPublicHost 203.0.113.10 `
        -GatewayIngressHost gateway.test1.example.com -GatewayTrustedProxies 10.244.0.0/16 `
        -RedisHost 127.0.0.1 -RedisPasswordEnv MMORPG_ROLLBACK_REDIS_PASSWORD -MySqlDsnEnv MMORPG_ROLLBACK_MYSQL_DSN `
        -RollbackDiscardTool <zone_rollback_discard> -BarrierProtocol legacy `
        -Apply
#>
param(
    [Parameter(Mandatory = $true)]
    [string]$ZoneName,

    [Parameter(Mandatory = $true)]
    [int]$ZoneId,

    # UTC,yyyy-MM-ddTHH:mm:ss[.fff]Z,如 "2026-05-15T14:23:00Z";必须早于当前 UTC(入口校验 E1)。
    [Parameter(Mandatory = $true)]
    [string]$TargetTime,

    # go/db 的共享 Redis(go/db 的 ServerConfig.RedisClient;K8s 即 infra redis 的 DB 0,本机即唯一的那个 Redis)。
    # 回档工具在这里盘点并丢弃 T 之后的状态,不做 FLUSHDB。-Apply 必填,不按 db.yaml 缺省的 127.0.0.1 补;
    # 可以是端口转发地址:实例身份由第 0 步 f 经 kubectl exec 读出线上 go/db RedisClient 的 run_id,工具每个模式按 -expect-run-id 核对
    # (不一致即拒,回档复核 [4]);-RedisDB 由第 0 步 f 与线上 RedisClient.DB 核对。
    [string]$RedisHost = "",
    [string]$RedisPort = "6379",
    # Redis 口令所在的环境变量名(不是口令本身):原样交给工具的 -redis-password-env,本脚本不读口令值、不上命令行、不打印。
    # 给了就要求该变量非空;Redis 没开认证时留空。
    [string]$RedisPasswordEnv = "",
    # 已废弃(2026-10-18,回档丢弃语义对抗复核 [12]):口令经命令行会进进程列表与 shell 历史。传了即在入口拒绝,改用 -RedisPasswordEnv。
    [string]$RedisPassword = "",
    [int]$RedisDB = 0,

    # Kafka cluster bootstrap.
    [string]$KafkaBootstrap = "kafka:9092",
    # 留空 = 按 -ZoneId 与 go/db/etc/db.yaml 的 ServerConfig.Kafka.TopicGeneration 推导(lib Get-DbTaskTopicName:
    # 世代 ≤ 1 为 db_task_zone_<id>,否则 db_task_zone_<id>_g<gen>;与 go/db DbTaskTopicForGeneration、go/login 同规则,
    # 也与 Step 6 的 k8s_deploy.ps1 Initialize-DbConsumerGroupPlans 读的是同一个键)。
    # 旧默认值 "db_task_topic" 是个**全仓不存在的 topic** —— offset reset 会「成功」地
    # 重置一个空 topic，回滚后 go/db 照样重放目标时间点之后的写入。
    # 显式给了却与推导值不同即在入口拒绝(2026-10-17,R7 复核 [6]):Step 6 只按 db.yaml 的世代号读 topic,
    # 第 0 步 d 与 Step 5 改的若是另一个 topic,真实 topic 上的位点根本没动,而全程没有任何报错。
    [string]$KafkaTopic = "",
    # go/db 存盘消费组(docs/design/db-consumer-group-per-zone.md)。留空 = 按 go/db/etc/db.yaml 的
    # ServerConfig.Kafka.GroupID 基名推导:-KafkaGroup 为 <基名>_z<ZoneId>(回档后的权威组,Step 5 重置它),
    # -KafkaLegacyGroup 为 <基名>(K8s 上全 zone 共用的旧组,Step 5 删除它在本 topic 上的位点)。两者相同时入口即拒。
    # 以前缺省是 legacy 组:只要别的 zone 在线,它就不是 Empty,Step 5 的重置一定被 Kafka 拒绝。
    [string]$KafkaGroup = "",
    [string]$KafkaLegacyGroup = "",

    # Zone restart args.
    #
    # 必填且必须是不可变 tag。以前这里默认 "…:latest" —— 灾难回滚脚本自己
    # 默认了一个可变 tag,等于"回档到某个时间点"时把 zone 拉回了**当前**
    # 镜像,而不是那个时间点在跑的版本。回滚必须由操作者显式指定回到哪一版。
    [Parameter(Mandatory = $true)]
    [string]$NodeImage,
    [string]$NamespacePrefix = "mmorpg-zone",
    # 发布档位,经 Step 6 的 dev_tools.ps1 k8s-zone-up 透传给 k8s_deploy.ps1 -ReleaseProfile。
    # 留空 = 不覆盖,落到 dev_tools.ps1 的默认 dev 档;staging / prod zone 回滚必须显式传,否则 Step 6 以 dev 档重建:
    # login 以 Mode dev 拉起、各密钥回落占位值(安全方向的 fail-open)。只影响 Step 6(以及第 0 步静态预检),
    # Step 1 的 zone-down 与档位无关,不传。与 dev_tools.ps1 同一 ValidateSet,拼错在本脚本入口就拒。
    [ValidateSet("", "dev", "staging", "prod")]
    [string]$ReleaseProfile = "",

    # gate 客户端 RPC 路由模式,经 Step 6 的 dev_tools.ps1 k8s-zone-up 透传给 k8s_deploy.ps1 -GateRouterMode。
    # 默认留空 = 不覆盖下游默认值(k8s_deploy.ps1 默认 "1",turn-based §22 D75):默认值只允许存在于
    # k8s_deploy.ps1 一处,写法同 dev_tools.ps1 / k8s_image.ps1 的透传,非空才透传。
    # 回退态不粘滞(口径见 k8s_deploy.ps1 -GateRouterMode 参数注释):那边每次部署都把本值原样重写进 gate env,
    # 不从集群读回旧值。以 "0" 回退运行的 zone,回滚时必须显式传 -GateRouterMode 0,否则 Step 6 静默落回 "1";
    # 回退的原因(如路由服不可用)若仍在,gate 卡在依赖门,登录 / 匹配全部 no_target。
    # 留空时 -Apply 由预检(Assert-ZoneEntryKeptOnRollback)兜底:集群当前模式与默认值不一致或读不到即拒绝,
    # 不再只靠文字提醒;显式给值即视为操作者的决定(包括有意从 "0" 切回 "1"),不核对。
    # 与 dev_tools.ps1 同一 ValidateSet:拼错在本脚本入口就拒 —— 若放到 Step 6 才由下游拒,
    # 那时 zone 已停服、数据已回档,回滚卡在半路。
    [ValidateSet("", "0", "1")]
    [string]$GateRouterMode = "",

    # B0-4 运维豁免开关(NODE_OPEN_FILE_LIMIT_MODE),经 Step 6 的 dev_tools.ps1 k8s-zone-up 透传给 k8s_deploy.ps1 -NodeOpenFileLimitMode,
    # 取值语义与 warn 的代价见那边参数注释。写法同 -GateRouterMode:默认留空 = 不透传(k8s_deploy.ps1 留空即不写 env、进程按 enforce),
    # 默认值只留在 k8s_deploy.ps1 一处,非空才透传。Step 6 只跑 zone-up,只影响本 zone 的 gate / scene,不碰 battle 全局池。
    # ⚠ 不粘滞,回档场景后果更重:Step 1 删掉整个 zone namespace,Step 6 只按本次参数重建。该 zone 若因节点池 nofile 硬限不够而以 warn
    # 运行,回滚时漏传就落回 enforce,gate / scene 在 Step 6 拒启(CrashLoop,-WaitReady 失败并打出 "FATAL open file limit" 行)——
    # 那时 zone 已停服、MySQL 已回档、Step 5 已丢弃积压,没有干净的中止路径,只能在失败提示给出的 Step 6 单步命令里补上
    # -NodeOpenFileLimitMode warn 单独重跑。所以留空时 -Apply 由第 0 步 a 兜底(2026-10-22):同一次 kubectl 读 gate 与 scene 的模板
    # (集群装了 Agones 时另读 scene Fleet),任一个的 NODE_OPEN_FILE_LIMIT_MODE 折算成 warn、或判不出(没有 gate、kubectl / Agones 探测失败),
    # 都在 Step 1 停服之前拒绝:带 warn 时报错给出照传的 -NodeOpenFileLimitMode warn,判不出时要求确认后显式传 warn 或 enforce;
    # 口径同 -GateRouterMode,放行条件跟着 k8s_deploy.ps1 的默认值走。
    # 显式给值即操作者的决定,不核对:照传 warn,或确认硬限已修好、显式传 enforce 回到 enforce。
    # 读到的现状随集群现状存进 entry-state.json,同一 rid 重跑、namespace 已删时据此核对;显式值不进入口决定(不按存档比较),重跑可以改。
    # 与 dev_tools.ps1 同一 ValidateSet 且区分大小写:大写或拼错在本脚本入口就拒 —— 若放到 Step 6 才由下游拒,回滚卡在半路(理由同上)。
    [ValidateSet("", "enforce", "warn", IgnoreCase = $false)]
    [string]$NodeOpenFileLimitMode = "",

    # ── 集群外客户端入口(k8s_deploy.ps1 同名参数,集群外入口 D76–D93),经 Step 6 的 dev_tools.ps1 k8s-zone-up 透传 ──
    # 写法同 -GateRouterMode:一律默认留空 = 不覆盖下游默认值(默认值只留在 k8s_deploy.ps1 一处),非空才透传;
    # 枚举型与 dev_tools.ps1 同一 ValidateSet,拼错在本脚本入口就拒。
    # 全部不粘滞:Step 1 删掉整个 zone namespace(gate StatefulSet、每序号 Service、Ingress 一并删除),Step 6 只按本次参数重建,
    # 漏传就落回默认值 —— gate 被重建成 podip、对外地址换掉或 Ingress 丢失,集群外玩家全部连不上。以 external 运行的 zone
    # 回滚时必须照传上一次部署的同一组值。-Apply 下由第 0 步预检兜底两类漏传:-ClientEntryMode 留空而 gate 当前形态与默认值不同,
    # 以及 -GatewayIngressHost 留空而 namespace 里有 Ingress gateway,都在停服之前拒绝。地址参数(-GateServiceType、
    # -GateNodePortBase、-ClientPublicHost、-GateClientHostTemplate、-GateExternalTrafficPolicy)不与集群逐项核对:显式给了
    # -ClientEntryMode 即视为操作者的决定;预检把 gate StatefulSet 现有的地址 env 打出来供核对。参数自身的组合错误
    # (LoadBalancer 缺模板、nodePort 越界、host 带 scheme …)由第 0 步静态预检交给 k8s_deploy.ps1 判定,同样在停服之前拒绝。
    [ValidateSet("", "podip", "external")]
    [string]$ClientEntryMode = "",
    [string]$ClientPublicHost = "",
    [string]$GateClientHostTemplate = "",
    # -1 = 未指定(不透传,由 k8s_deploy.ps1 默认值决定);其余值原样透传,由下游校验(同 dev_tools.ps1)。
    [int]$GateNodePortBase = -1,
    [ValidateSet("", "Local", "Cluster")]
    [string]$GateExternalTrafficPolicy = "",
    # external 下决定每序号 Service 的类型,同属地址参数:漏传落回 NodePort,LoadBalancer 的 zone 对外地址整体换掉。
    [ValidateSet("", "ClusterIP", "NodePort", "LoadBalancer")]
    [string]$GateServiceType = "",
    # login / scene_manager ConfigMap 的 RequireClientEndpoint。逐个 zone 切 external 的窗口里(ingress_final §6 第 2 批)
    # 已切 zone 以 false 运行,回滚时须照传 -RequireClientEndpoint false,否则 auto 按 external 取 true,跨 zone 跳转到未切 zone 失败。
    # 现状在 ConfigMap 里,预检不核对。
    [ValidateSet("", "auto", "true", "false")]
    [string]$RequireClientEndpoint = "",
    # 集群内 login 的开发口令认证(k8s_deploy.ps1 同名开关,只允许 dev 档,由它的 preflight 把关),经 Step 6 透传;
    # switch 只在被指定时透传,共享口令仍走环境变量 MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET、不经参数。
    # 不粘滞:dev zone 依赖开发口令登录时照传,漏传则 Step 6 生成的 login 没有任何认证配置(fail-closed),登录全部失败。
    [switch]$LoginDevPasswordAuth,
    # 集群内 login 的第三方登录(微信 / QQ,k8s_deploy.ps1 同名参数,语义、形状校验与微信密钥 MMORPG_LOGIN_WECHAT_APP_SECRET 都在那边),
    # 经 Step 6 透传;字符串非空才透传,switch 只在被指定时透传。不粘滞,而 Step 1 会连 login ConfigMap 与 Secret login-oauth 一起删掉,
    # k8s_deploy.ps1 自己的防误关闸在 Step 6 面对的是空 namespace —— 所以 -Apply 下第 0 步在停服之前读线上现状,按同一份判据
    # (lib Get-LoginAuthProviderDrift)核对:线上开着的第三方登录会被本次参数关掉或改掉(漏传 AppId / Endpoint / AllowOpenId、
    # 换了 AppId)、线上读不到或认不出,都拒绝,报错给出照传现状的参数串。给了 -LoginWeChatAppId 时第 0 步的静态预检同样要求
    # 部署机已设置 MMORPG_LOGIN_WECHAT_APP_SECRET,缺了在停服之前拒绝。
    [string]$LoginWeChatAppId = "",
    [string]$LoginWeChatEndpoint = "",
    [switch]$LoginWeChatAllowOpenId,
    [string]$LoginQQAppId = "",
    [string]$LoginQQEndpoint = "",
    [switch]$LoginQQAllowOpenId,
    # 显式确认本次回滚后 login 的第三方登录只按本次的 -Login* 参数重建(关掉或改动线上配置也接受):给了就不读、不核对线上
    # login ConfigMap 与 Secret,并透传给 Step 6(namespace 没删净时 k8s_deploy.ps1 的防误关闸同样放行)。地位同 -ConfirmNoGatewayIngress。
    [switch]$ConfirmLoginAuthProviderChange,
    # gateway Ingress(D91,由 zone-up 随 gateway 生成)。给了 -GatewayIngressHost 就必须同时给 -GatewayTrustedProxies:
    # 与 k8s_deploy.ps1 preflight(lib Test-ClientEntryPreflight)同一规则,在本脚本入口先拒(dry-run 也报),不拖到 Step 6。
    [string]$GatewayIngressHost = "",
    [string]$GatewayIngressClassName = "",
    [string]$GatewayIngressTlsSecret = "",
    # 逗号分隔的 CIDR,原样透传。
    [string]$GatewayTrustedProxies = "",
    # 显式声明本次回滚后该 zone 不要 Ingress gateway:Ingress 这一项的"显式决定",地位同显式给 -GateRouterMode / -ClientEntryMode。
    # 给了就不核对 Ingress(namespace 里现有的 Ingress gateway 随 Step 1 删除,Step 6 不重建);不给且 -GatewayIngressHost 留空时,
    # 第 0 步要读到"确定没有"才放行 —— 集群读不到、或 namespace 里一个 gate 都没有(已被删,无从判断原先有没有)都拒绝。
    # 与 -GatewayIngressHost 互斥,同时给在入口即拒。
    [switch]$ConfirmNoGatewayIngress,
    # 确认本次就是要切换 gate 入口形态或改客户端地址来源(k8s_deploy.ps1 同名开关),经 Step 6 透传;switch 只在被指定时透传。
    # 通常不起作用:Step 1 删掉整个 zone namespace 后,k8s_deploy.ps1 的集群现状闸(换形态 / 地址漂移)在 Step 6 面对的是空 namespace。
    # 它只在 namespace 没删净时起作用(zone-down 删 namespace 允许失败):那时 Step 6 按集群现状判定,形态或地址与本次参数不同
    # 而没给它,就在写操作之前拒绝,回滚停在 Step 6(zone 已停服、数据已回档)—— 本次回滚确要切换形态或地址时预先加上。
    # 不豁免第 0 步:那里拦的是"留空落回默认值"的静默翻转,要换就显式传目标 -GateRouterMode / -ClientEntryMode,
    # Ingress 显式传 -GatewayIngressHost 或 -ConfirmNoGatewayIngress;"已有 Ingress 却缺 trusted proxies"在 k8s_deploy.ps1 那边同样不受它豁免。
    [switch]$AllowDisruptiveSwitch,
    # 共享 Redis 按用途拆实例(G4a 第 1a 批,k8s_deploy.ps1 同名参数):Step 6 的 zone-up 按它渲染 login 的 Node.QueueRedis 与 gateway 的
    # 限流 Redis 地址(组号仍取 zones 配置的 redisQueueGroup)。留空(缺省)= 不转发,Step 6 按 k8s_deploy.ps1 的 auto 渲染。
    # **以 queue 运行的 zone 回档必须显式给 -RedisSplit queue**:auto 靠读本 zone 线上 login ConfigMap 跟随现状,而 Step 1 已删掉整个
    # zone namespace,Step 6 读不到任何现状,auto 只能落成 none —— 回档后的 login / gateway 会静默连回核心 Redis(排队键与限流桶换了实例,
    # 排队开着时在途排位全部丢失),k8s_deploy.ps1 的切换闸面对空 namespace 也拦不住。本脚本不在停服前核对这一项,由操作者照传上次部署的值。
    [ValidateSet("", "none", "queue")]
    [string]$RedisSplit = "",

    # kubectl 目标集群,语义同 k8s_deploy.ps1 的 -KubeContext / -KubeConfig(非空才追加 --context / --kubeconfig)。
    # 第 0 步预检的只读 kubectl 与 Step 1 / Step 6(经 dev_tools.ps1 透传给 k8s_deploy.ps1)用同一组值,保证核对与改动落在同一集群。
    # 留空 = kubectl 当前 context;本机默认 context 可能属于别的项目,回滚前务必确认或显式指定。
    [string]$KubeContext = "",
    [string]$KubeConfig = "",

    # Skip the manual MySQL prompt (use only when PITR was done OOB earlier).
    # -Apply 下必须同时给 -PitrRestoredTo:跳过暂停而 PITR 其实没做(或恢复到了影子库没换回来)时,工具会把 T 之后的库当成回档后的库处置,
    # 积压却已在 Step 5 丢弃(回档丢弃语义对抗复核 [5])。
    [switch]$SkipMySqlPause,
    # 操作者声明的 PITR 恢复时刻,格式同 -TargetTime,给了就必须等于 -TargetTime;写进报告目录的运行记录。
    [string]$PitrRestoredTo = "",

    # 已废弃(2026-10-18):回档不再 FLUSHDB(go/db 游标、重试收据、映射、账号目录与各 zone 缓存同在一个 Redis DB,FLUSHDB 等于事故)。
    # 传了即在入口拒绝;整套环境重建走 AGENTS.md §6.2。
    [switch]$ConfirmFlushSharedRedis,

    # 回档工具:go/db/cmd/zone_rollback_discard 的预编译可执行文件,必须与线上 go/db 同一提交构建(回档丢弃语义对抗复核 [9]:
    # 不现场 go build —— 本地工作区未必是线上镜像的提交,键名与 Lua 契约可能对不上,运维机上也未必有 Go 工具链)。
    # 同一提交由第 0 步 f 机械核对(回档复核 [5]):<tool> -version 的构建提交必须以线上 db 镜像的 tag 开头、两边都不是脏树构建,否则停服之前拒绝。
    # -Apply 必填;工具契约见文件头。工具尚未落地,在它落地之前 -Apply 停在入口校验。
    [string]$RollbackDiscardTool = "",
    # zone_<id>_db 的 MySQL DSN 所在的环境变量名(不是 DSN 本身)。-Apply 必填且该变量必须非空;原样交给工具的 -mysql-dsn-env。
    [string]$MySqlDsnEnv = "",
    # 存盘提交屏障的协议世代。-Apply 必填。legacy:线上 go/db 的 Fencing.Mode 为 legacy(不写两表 SQL ACK、不补投),工具绝不写 save_barrier;
    # 第 0 步 f 核对它与线上 go-svc-db-config 的 Fencing.Mode 一致。legacy 只与尚未升级到严格提交屏障的旧 scene / login 同世代:
    # 现行 scene / login 只说严格屏障,go/db 缺省 dual,且 legacy go/db 在本 zone 有世代标记 db:save_protocol:zone:<id> 或补投桶非空时拒启
    # (2026-10-18 存盘协议世代门禁)。strict 待负责人裁决(#3:回档工具对该世代的屏障重建未定),传了即在入口拒绝;
    # 所以按现行代码部署的 zone 在裁决之前不能执行 -Apply。
    [ValidateSet("", "legacy", "strict")]
    [string]$BarrierProtocol = "",
    # 本次回档的 id:进隔离区键名 rollback:quarantine:<zone>:<rid>:* 与报告目录 run/logs/zone-rollback/<rid>/。
    # 留空 = z<ZoneId>-<TargetTime 紧凑格式>-<当前 UTC 紧凑格式>;整脚本重跑或单步重跑必须传上一次打印的同一个值。只允许 [A-Za-z0-9_-]。
    [string]$RollbackId = "",
    # Step 1b 两次读日志末端的间隔(秒)。下限 300 = C++ 生产者 librdkafka message.timeout.ms 的缺省值(本仓未覆盖,
    # 见 cpp/libs/engine/infra/messaging/kafka/kafka_producer.cpp):停服前已进本地队列的消息最晚在这之后才到 broker。
    [ValidateRange(300, 86400)]
    [int]$QuiesceSeconds = 300,
    # 带 blocked 玩家(torn / owner_behind / post_t_candidate / legacy_owned / unclassified)继续回档:这些玩家的缓存与屏障是隔离保留还是删除、
    # T 之后建的角色能否以空骨架继续用,待负责人裁决(#4)。传了即在入口拒绝;4a 盘点出 blocked 时回档停在 Step 5 之前。
    [ValidateRange(0, 2147483647)]
    [int]$AcceptBlockedPlayers = 0,
    # 交给工具 preflight 的 -allow-empty-target:目标 Redis 里本 topic 一个已落库游标都没有(工具退出码 4)时仍放行。
    # 只用于从未落过库的新 zone;平时它说明 -RedisHost / -RedisDB 指错了实例。
    [switch]$AllowEmptyTarget,
    # 交给工具的 -acknowledge-no-post-t-writes:Step 1b 停服基线与 PITR 之后的行指纹没有任何玩家变化(工具判为没做 PITR 或连错库)时仍放行。
    # 只用于 T 之后确实没有任何存盘的 zone。它是判定参数的一部分:inventory、purge(重跑 inventory 的检查)与 verify 一并带上(回档复核 [2])。
    [switch]$AcknowledgeNoPostTWrites,

    [switch]$Apply
)

$ErrorActionPreference = "Stop"

# topic 名必须与 go/db 实际消费的一致,否则第 5 步会「成功」重置一个不存在的 topic;推导在入口读 db.yaml 之后(见 TopicGeneration 那段)。
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")

. (Join-Path $ScriptDir "lib\release_common.ps1")
# 只为 Get-GateAddressHint 取 Get-ClientEntryContract(gate 生成器内部 env 名与端口模式取值的唯一来源);
# 该库只定义函数、不读脚本级变量、不设 StrictMode,dot-source 无副作用。
. (Join-Path $ScriptDir "lib\k8s_client_entry.ps1")
# 第 0 步 c / d 解析 kafka-consumer-groups --describe 用(与 kafka_offset_reset.ps1、k8s_deploy.ps1 同一份规则);
# 纯函数库,dot-source 无副作用。
. (Join-Path $ScriptDir "lib" "kafka_consumer_groups.ps1")

<#
.SYNOPSIS
    读部署脚本(k8s_deploy.ps1;第 0 步 e 另读 dev_tools.ps1 的 -InfraNamespace)参数块里若干参数的默认值,
    返回 @{ 参数名 = 默认值 | $null };某项不是字符串字面常量或读不到时为 $null。

.DESCRIPTION
    部署层默认值只允许存在于 k8s_deploy.ps1 一处(-GateRouterMode:turn-based §22 D75;-ClientEntryMode:集群外入口 D80;
    -NodeOpenFileLimitMode:B0-4,空串 = 不写 env):
    这里按语法树读它们,不在本脚本抄一份 "1" / "podip"。那边哪天改了默认值,预检自动跟着变,
    不会出现「按旧默认放行、Step 6 实际翻到新默认」。某项为 $null 时由调用方 fail-closed(要求显式传该参数),不猜。
    文件只解析一次。
#>
function Get-DeployParamDefaults {
    param(
        [Parameter(Mandatory = $true)][string]$DeployScriptPath,
        [Parameter(Mandatory = $true)][string[]]$Names
    )

    $defaults = @{}
    foreach ($name in $Names) { $defaults[$name] = $null }
    if (-not (Test-Path -LiteralPath $DeployScriptPath)) { return $defaults }
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile($DeployScriptPath, [ref]$null, [ref]$parseErrors)
    if ($parseErrors.Count -gt 0 -or $null -eq $ast.ParamBlock) { return $defaults }
    foreach ($name in $Names) {
        $parameters = @($ast.ParamBlock.Parameters | Where-Object { $_.Name.VariablePath.UserPath -eq $name })
        if ($parameters.Count -eq 1 -and $parameters[0].DefaultValue -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
            $defaults[$name] = $parameters[0].DefaultValue.Value
        }
    }
    return $defaults
}

<#
.SYNOPSIS
    gate 工作负载(StatefulSet 或 Deployment)Pod 模板里的 GATE_CLIENT_RPC_ROUTER,折算成 gate 实际生效的模式 "1" / "0"。纯函数。

.DESCRIPTION
    返回 [pscustomobject]@{ Mode = "1" | "0" | $null; Detail = 说明 };Mode 为 $null 表示判不出,由调用方 fail-closed。
    折算口径与 gate 进程一致(cpp/nodes/gate/gate_router_mode.h 的 ParseRouterModeFlag / ResolveRouterModeFromEnv):
    去掉首尾 ASCII 空白、转小写后为 "1" / "true" / "on" 才是路由模式,其它值以及**变量未设置**一律是直连 "0"
    —— 早期 k8s_deploy.ps1 不注入这个变量,那样部署的 gate 跑的就是直连。
    以下情况判不出:名为 gate 的容器不是恰好一个、该变量重复出现(不猜哪条生效)、用 valueFrom 注入(读不到字面值)。
    两种形态都由 k8s_deploy.ps1 写同一个字面值 env(external 的 StatefulSet 见 lib New-GateStatefulSetYaml)。
    StatefulSet 是 OnDelete,模板是最近一次部署的意图,个别 Pod 可能还没重建;回滚 Step 1 会删掉全部 Pod,要比的正是模板。
#>
function Get-GateRouterModeFromPodSpec {
    param(
        [AllowNull()]$PodSpec,
        # 只用于报错文本,例如 "StatefulSet"。
        [Parameter(Mandatory = $true)][string]$Kind
    )

    $containers = @($PodSpec.containers | Where-Object { $_.name -eq 'gate' })
    if ($containers.Count -ne 1) {
        return [pscustomobject]@{ Mode = $null; Detail = "gate $Kind 里名为 gate 的容器有 $($containers.Count) 个(k8s_deploy.ps1 只生成一个)" }
    }
    $entries = @($containers[0].env | Where-Object { $_.name -eq 'GATE_CLIENT_RPC_ROUTER' })
    if ($entries.Count -eq 0) {
        return [pscustomobject]@{ Mode = '0'; Detail = 'gate 未设置 GATE_CLIENT_RPC_ROUTER,进程默认即直连' }
    }
    if ($entries.Count -gt 1) {
        return [pscustomobject]@{ Mode = $null; Detail = "gate 的 GATE_CLIENT_RPC_ROUTER 出现 $($entries.Count) 次,不猜哪条生效" }
    }
    if ($null -ne $entries[0].valueFrom) {
        return [pscustomobject]@{ Mode = $null; Detail = 'gate 的 GATE_CLIENT_RPC_ROUTER 用 valueFrom 注入,读不到字面值' }
    }
    $raw = [string]$entries[0].value
    # 只去 ASCII 空白,与 C++ 侧 std::isspace(C locale)同一集合。
    $normalized = $raw.Trim([char[]]@(' ', "`t", "`n", "`v", "`f", "`r")).ToLowerInvariant()
    $mode = if ($normalized -in @('1', 'true', 'on')) { '1' } else { '0' }
    return [pscustomobject]@{ Mode = $mode; Detail = "gate env GATE_CLIENT_RPC_ROUTER=`"$raw`"" }
}

# scene 工作负载名(k8s_deploy.ps1 Resolve-SceneDeploymentPlan:legacy 单池 scene,或拆分后的 scene-world / scene-instance;
# Deployment 与 Agones Fleet 同名,容器名即工作负载名)。第 0 步 a 按名读;与 k8s_deploy.ps1 的一致性由契约测试按语法树核对。
function Get-ZoneSceneWorkloadNames {
    return @('scene', 'scene-world', 'scene-instance')
}

<#
.SYNOPSIS
    一个 C++ 工作负载 Pod 模板里的 NODE_OPEN_FILE_LIMIT_MODE(B0-4),折算成进程实际生效的模式 "warn" / "enforce"。纯函数。

.DESCRIPTION
    返回 [pscustomobject]@{ Mode = "warn" | "enforce" | $null; Detail = 说明 };Mode 为 $null 表示判不出,由调用方 fail-closed。
    折算口径与进程一致(cpp/libs/engine/core/node/system/node/open_file_limit.h 的 ParseMode):未设置或空串是 enforce,
    只有逐字 "warn" 是豁免,"enforce" 与其它值(大写、拼错)一律按 enforce —— 进程里拼错的开关不放行,所以它不算 warn。
    以下情况判不出(同 Get-GateRouterModeFromPodSpec):名为 -ContainerName 的容器不是恰好一个、该变量重复出现、用 valueFrom 注入。
    env 名取 lib Get-ClientEntryContract 的 NodeOpenFileLimitModeEnv(k8s_deploy.ps1 两个模板与 lib 三个生成器写的同一个名字)。
    只看模板(最近一次部署的意图,也包括 runbook §3.1 用 kubectl set env 手设的值);Step 1 会删掉全部 Pod,Step 6 按本次参数重建。
#>
function Get-OpenFileLimitModeFromPodSpec {
    param(
        [AllowNull()]$PodSpec,
        [Parameter(Mandatory = $true)][string]$ContainerName,
        # 只用于说明文本,例如 "Deployment scene"。
        [Parameter(Mandatory = $true)][string]$Label
    )

    $envName = (Get-ClientEntryContract).NodeOpenFileLimitModeEnv
    $containers = @($PodSpec.containers | Where-Object { $_.name -eq $ContainerName })
    if ($containers.Count -ne 1) {
        return [pscustomobject]@{ Mode = $null; Detail = "$Label 里名为 $ContainerName 的容器有 $($containers.Count) 个(k8s_deploy.ps1 只生成一个)" }
    }
    $entries = @($containers[0].env | Where-Object { $_.name -ceq $envName })
    if ($entries.Count -eq 0) {
        return [pscustomobject]@{ Mode = 'enforce'; Detail = "$Label 未设置 $envName(进程按 enforce)" }
    }
    if ($entries.Count -gt 1) {
        return [pscustomobject]@{ Mode = $null; Detail = "$Label 的 $envName 出现 $($entries.Count) 次,不猜哪条生效" }
    }
    if ($null -ne $entries[0].valueFrom) {
        return [pscustomobject]@{ Mode = $null; Detail = "$Label 的 $envName 用 valueFrom 注入,读不到字面值" }
    }
    $raw = [string]$entries[0].value
    $mode = if ($raw -ceq 'warn') { 'warn' } else { 'enforce' }
    return [pscustomobject]@{ Mode = $mode; Detail = "$Label $envName=`"$raw`"" }
}

<#
.SYNOPSIS
    zone namespace 里 C++ 工作负载(gate / scene)的 nofile 模式汇总(纯函数):任一个是 warn 即 "warn";
    有 gate、全部判得出且都不是 warn 才 "enforce";其余判不出($null)。返回 [pscustomobject]@{ Mode; Detail }。

.DESCRIPTION
    -Workloads:kubectl 读到并已按名筛过的 gate StatefulSet / Deployment、scene Deployment 与 scene Fleet 对象。
    Fleet 的 Pod 模板在 spec.template.spec.template.spec(Fleet → GameServer → Pod,New-SceneFleetYaml),其余在 spec.template.spec;
    容器名即工作负载名。一个 gate 都没有时判不出(namespace 多半已被删,与路由模式、入口形态同一口径),
    "现在只剩 scene"证明不了 gate 回滚前的取值。-FleetReadError 非空(Agones 探测或 Fleet 读取失败)时,没有 warn 即判不出:
    读不到 Fleet 不等于没有 Fleet。warn 优先于判不出:已知至少一个需要豁免,照传 warn 就是保持现状。
#>
function Resolve-ZoneOpenFileLimitMode {
    param(
        [AllowNull()][AllowEmptyCollection()][object[]]$Workloads = @(),
        [AllowNull()][AllowEmptyString()][string]$FleetReadError = ''
    )

    $results = @(foreach ($workload in @($Workloads | Where-Object { $null -ne $_ })) {
            $kind = [string]$workload.kind
            $name = [string]$workload.metadata.name
            $podSpec = if ($kind -eq 'Fleet') { $workload.spec.template.spec.template.spec } else { $workload.spec.template.spec }
            Get-OpenFileLimitModeFromPodSpec -PodSpec $podSpec -ContainerName $name -Label "$kind $name"
        })
    $details = @($results | ForEach-Object { $_.Detail })
    if (-not [string]::IsNullOrEmpty($FleetReadError)) { $details += "读不到 scene Fleet:$FleetReadError" }
    $hasGate = @($Workloads | Where-Object { $null -ne $_ -and $_.metadata.name -eq 'gate' }).Count -gt 0
    if (-not $hasGate) { $details = @('namespace 里没有 gate(可能已被上一次中途放弃的回滚删掉),判不出回滚前 gate 的取值') + $details }
    $detail = $details -join ';'

    if (@($results | Where-Object { $_.Mode -ceq 'warn' }).Count -gt 0) { return [pscustomobject]@{ Mode = 'warn'; Detail = $detail } }
    if (-not $hasGate -or -not [string]::IsNullOrEmpty($FleetReadError) -or @($results | Where-Object { $null -eq $_.Mode }).Count -gt 0) {
        return [pscustomobject]@{ Mode = $null; Detail = $detail }
    }
    return [pscustomobject]@{ Mode = 'enforce'; Detail = $detail }
}

<#
.SYNOPSIS
    gate StatefulSet 模板里的地址 env → 一段"照传哪些参数"的提示文本。纯函数。

.DESCRIPTION
    env 名、端口模式取值与每序号 Service 标签一律取自 lib/k8s_client_entry.ps1 的 Get-ClientEntryContract
    (New-GateStatefulSetYaml 写入、k8s_deploy.ps1 Get-GateAddressDrift 比对的同一份;生成器内部 env,C++ 不读),本脚本不另写字面量:
    GateClientPortModeEnv 为 GateClientPortModeNodePort 对应 -GateServiceType NodePort + -GateNodePortBase <GateNodePortBaseEnv 的值>,
    为 GateClientPortModeService 对应 -GateServiceType LoadBalancer;GateClientHostTemplateEnv 对应 -GateClientHostTemplate
    ({zone} 已在生成期渲染,照传渲染后的值即可);GateClientPublicHostEnv 对应 -ClientPublicHost。
    externalTrafficPolicy 写在每序号 Service 上、不在模板里,只指路(按 GateOrdinalLabelKey 标签列出)。
    只给提示、不做比对:地址计划的推导(OpsProfile 改写、zones 配置覆盖、模板渲染)只在 k8s_deploy.ps1 一处。
#>
function Get-GateAddressHint {
    param([AllowNull()]$PodSpec)

    $contract = Get-ClientEntryContract
    $portModeEnv = $contract.GateClientPortModeEnv
    $container = @($PodSpec.containers | Where-Object { $_.name -eq 'gate' }) | Select-Object -First 1
    $parts = @()
    foreach ($name in @($portModeEnv, $contract.GateNodePortBaseEnv, $contract.GateClientPortEnv,
            $contract.GateClientHostTemplateEnv, $contract.GateClientPublicHostEnv)) {
        $entry = @($container.env | Where-Object { $_.name -ceq $name }) | Select-Object -First 1
        if ($null -ne $entry) { $parts += "$name='$([string]$entry.value)'" }
    }
    $envText = if ($parts.Count -gt 0) { "模板地址 env:" + ($parts -join ' ') } else { '模板里没有地址 env' }
    return ($envText +
        "($portModeEnv=$($contract.GateClientPortModeNodePort) 对应 -GateServiceType NodePort -GateNodePortBase <$($contract.GateNodePortBaseEnv)>," +
        "$portModeEnv=$($contract.GateClientPortModeService) 对应 -GateServiceType LoadBalancer;" +
        "$($contract.GateClientHostTemplateEnv) 对应 -GateClientHostTemplate,$($contract.GateClientPublicHostEnv) 对应 -ClientPublicHost;" +
        "externalTrafficPolicy 在每序号 Service 上,见 kubectl get svc -l $($contract.GateOrdinalLabelKey))")
}

# kubectl 失败或输出不可解析时的现状:各项(含 nofile 模式)都判不出,由 Assert-ZoneEntryKeptOnRollback 按需拒绝。
function New-UnknownZoneEntryState {
    param([Parameter(Mandatory = $true)][string]$Detail)

    return [pscustomobject]@{
        RouterMode     = $null
        RouterDetail   = $Detail
        EntryMode      = $null
        EntryDetail    = $Detail
        IngressKnown   = $false
        Ingress        = $null
        IngressDetail  = $Detail
        OpenFileLimitMode   = $null
        OpenFileLimitDetail = $Detail
        # 读不到不等于 namespace 空了:只有 kubectl 读成功、一个 gate / Ingress 都没有时才为 $true(同一 rid 重跑据此改用 entry-state.json)。
        NamespaceEmpty = $false
    }
}

<#
.SYNOPSIS
    第 0 步预检的 kubectl 参数(只读):一次取回 zone namespace 里的 gate StatefulSet / gate Deployment / Ingress gateway,
    以及 scene Deployment(Get-ZoneSceneWorkloadNames,nofile 模式核对用)。dry-run 打印的就是这一份,-Apply 时原样执行。

.DESCRIPTION
    多个具名资源加 --ignore-not-found:不存在的静默跳过(退出码 0),找到的合成一个 List 输出,一个都没有时无输出。
    scene 的 Agones Fleet 是 CRD,没装 Agones 的集群里资源类型不存在会让整条失败,所以不并进这一次,另由 Get-ClusterSceneFleets 先探测再读。
    namespace 不存在时按名 get 同样是 NotFound、被一并忽略,由调用方把「gate 一个都没有」判为读不到。
    -KubeContext / -KubeConfig 非空才追加 --context / --kubeconfig,与 Step 1 / Step 6 透传给 k8s_deploy.ps1 的是同一组值。
#>
function Get-ZoneEntryKubectlArgs {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = '',
        [int]$RequestTimeoutSec = 15
    )

    $sceneDeployments = @(Get-ZoneSceneWorkloadNames | ForEach-Object { "deployment/$_" })
    $kubectlArgs = @('get', 'statefulset/gate', 'deployment/gate', 'ingress/gateway') + $sceneDeployments +
        @('-n', $Namespace, '-o', 'json', '--ignore-not-found', "--request-timeout=${RequestTimeoutSec}s")
    if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $kubectlArgs += @('--context', $KubeContext) }
    if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $kubectlArgs += @('--kubeconfig', $KubeConfig) }
    return , $kubectlArgs
}

# 第 0 步 a 读 scene Fleet 的 kubectl 参数(只读,不含 --context / --kubeconfig:经 lib Invoke-ClientEntryKubectl 执行时由它前置)。
# 资源名取 lib 契约的 AgonesFleetResource(k8s_deploy.ps1 Get-LiveSceneWorkloads 读 Fleet 用的同一个),按名读 + --ignore-not-found。
function Get-SceneFleetKubectlArgs {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [int]$RequestTimeoutSec = 15
    )

    return , (@('get', (Get-ClientEntryContract).AgonesFleetResource) + @(Get-ZoneSceneWorkloadNames) +
        @('-n', $Namespace, '-o', 'json', '--ignore-not-found', "--request-timeout=${RequestTimeoutSec}s"))
}

<#
.SYNOPSIS
    第 0 步 a(-NodeOpenFileLimitMode 留空时):读 zone namespace 里 Agones 编排的 scene Fleet,
    返回 [pscustomobject]@{ Items = Fleet 对象数组; Error = $null | 读不到的原因 }。

.DESCRIPTION
    先按 lib Test-AgonesFleetCrdPresent(k8s_deploy.ps1 Get-LiveSceneWorkloads 同一份探测,只读 agones.dev 组的发现文档)确认集群装没装 Agones:
    确定没装即没有 Fleet;装了再执行 Get-SceneFleetKubectlArgs。探测或读取失败、输出不是 JSON 时 Error 非空,
    调用方(Resolve-ZoneOpenFileLimitMode)判为读不到(fail-closed),不当成"没有 Fleet"。只读、不重试。
#>
function Get-ClusterSceneFleets {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = ''
    )

    try {
        if (-not (Test-AgonesFleetCrdPresent -KubeContext $KubeContext -KubeConfig $KubeConfig)) {
            return [pscustomobject]@{ Items = @(); Error = $null }
        }
        $fleetArgs = Get-SceneFleetKubectlArgs -Namespace $Namespace
        $result = Invoke-ClientEntryKubectl -KubectlArgs $fleetArgs -KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
    }
    catch {
        return [pscustomobject]@{ Items = @(); Error = $_.Exception.Message }
    }
    if ($result.ExitCode -ne 0) {
        return [pscustomobject]@{ Items = @(); Error = "kubectl $($fleetArgs -join ' ') 失败(退出码 $($result.ExitCode)):$($result.Stderr)" }
    }
    if ([string]::IsNullOrWhiteSpace($result.Stdout)) { return [pscustomobject]@{ Items = @(); Error = $null } }
    try {
        $document = $result.Stdout | ConvertFrom-Json -ErrorAction Stop
    }
    catch {
        return [pscustomobject]@{ Items = @(); Error = "kubectl 输出不是合法的 JSON:$($_.Exception.Message)" }
    }
    $items = if ($document.kind -eq 'List') { @($document.items) } else { @($document) }
    return [pscustomobject]@{ Items = @($items | Where-Object { $null -ne $_ -and $_.kind -eq 'Fleet' }); Error = $null }
}

<#
.SYNOPSIS
    执行一次 Get-ZoneEntryKubectlArgs 给出的只读 kubectl,返回 zone 入口现状:
    [pscustomobject]@{ RouterMode; RouterDetail; EntryMode; EntryDetail; IngressKnown; Ingress; IngressDetail;
    OpenFileLimitMode; OpenFileLimitDetail; NamespaceEmpty }。
    NamespaceEmpty:kubectl 读成功且一个 gate、一个 Ingress gateway 都没有(namespace 多半已被 Step 1 删掉);同一 rid 重跑据此改用 entry-state.json。

.DESCRIPTION
    - RouterMode:"1" | "0" | $null(判不出),口径见 Get-GateRouterModeFromPodSpec;StatefulSet 与 Deployment 两种 gate 都认。
    - EntryMode:gate 形态折算的 -ClientEntryMode(k8s_deploy.ps1 模式矩阵,集群外入口 D80):StatefulSet = "external",
      Deployment = "podip";$null = 判不出 —— 一个 gate 都没有(如 namespace 已被上一次中途放弃的回滚删掉),
      或两种形态同时存在(上一次形态切换没做完,不猜哪个在服务)。external 时 EntryDetail 附带模板里的地址 env(Get-GateAddressHint)。
    - IngressKnown / Ingress:Ingress 为 $null(不存在)或 { Host; ClassName; TlsSecret }。kubectl 读成功且 namespace 里至少有一个 gate,
      或读到了 Ingress gateway,才 IngressKnown = $true;一个 gate 都没有也没有 Ingress 时判为读不到(namespace 多半已被删,
      "现在没有"证明不了"回滚前没有")。
    - OpenFileLimitMode:"warn" | "enforce" | $null(判不出),gate 与 scene Deployment(同一次读)加 -SceneFleets 的 Fleet 汇总,
      口径见 Resolve-ZoneOpenFileLimitMode。没给 -SceneFleets($null,本次显式给了 -NodeOpenFileLimitMode、没读 Fleet)时
      记为判不出("未核对"):这份现状照样存进 entry-state.json,同一 rid 重跑留空时不能拿没读全的现状放行。
    kubectl 不可用或失败(不可达、Forbidden)、输出不是 JSON 时各项都判不出。只读一次、不重试。
    --request-timeout 只约束 HTTP 请求,约束不了凭证插件等请求之外的等待;此时尚未产生任何副作用,
    卡住时操作者 CTRL+C 即可,不为此引入进程级截止。
#>
function Get-ClusterZoneEntryState {
    param(
        [Parameter(Mandatory = $true)][string[]]$KubectlArgs,
        [Parameter(Mandatory = $true)][string]$Namespace,
        # Get-ClusterSceneFleets 的返回值;$null = 没读 Fleet(nofile 模式不核对)。
        [AllowNull()]$SceneFleets = $null
    )

    try {
        $lines = @(& kubectl @KubectlArgs 2>&1)
    }
    catch {
        return (New-UnknownZoneEntryState -Detail "kubectl 调用失败:$($_.Exception.Message)")
    }
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        $message = (($lines | ForEach-Object { "$_" }) -join ' ').Trim()
        return (New-UnknownZoneEntryState -Detail "kubectl $($KubectlArgs -join ' ') 失败(退出码 $exitCode):$message")
    }
    # stderr 上的告警行(ErrorRecord)不是 JSON 的一部分。
    $json = ($lines | Where-Object { $_ -isnot [System.Management.Automation.ErrorRecord] } | ForEach-Object { "$_" }) -join "`n"
    $items = @()
    if (-not [string]::IsNullOrWhiteSpace($json)) {
        try {
            $document = $json | ConvertFrom-Json -ErrorAction Stop
        }
        catch {
            return (New-UnknownZoneEntryState -Detail "kubectl 输出不是合法的 JSON:$($_.Exception.Message)")
        }
        # 多个具名资源时 kubectl 输出 List;单个对象的写法也认,不依赖 kubectl 版本的包装细节。
        $items = if ($document.kind -eq 'List') { @($document.items) } else { @($document) }
    }

    $gates = @($items | Where-Object { $null -ne $_ -and $_.kind -in @('StatefulSet', 'Deployment') -and $_.metadata.name -eq 'gate' })
    $routerMode = $null
    $entryMode = $null
    if ($gates.Count -eq 0) {
        $routerDetail = "namespace $Namespace 里既没有 gate StatefulSet 也没有 gate Deployment(可能已被上一次中途放弃的回滚删掉)"
        $entryDetail = $routerDetail
    }
    elseif ($gates.Count -gt 1) {
        $routerDetail = "namespace $Namespace 里 gate StatefulSet 与 gate Deployment 同时存在(上一次形态切换没做完),不猜哪个在服务"
        $entryDetail = $routerDetail
    }
    else {
        $gate = $gates[0]
        $kind = [string]$gate.kind
        $router = Get-GateRouterModeFromPodSpec -PodSpec $gate.spec.template.spec -Kind $kind
        $routerMode = $router.Mode
        $routerDetail = "$kind 模板:$($router.Detail)"
        if ($kind -eq 'StatefulSet') {
            $entryMode = 'external'
            $entryDetail = "gate 为 StatefulSet;" + (Get-GateAddressHint -PodSpec $gate.spec.template.spec)
        }
        else {
            $entryMode = 'podip'
            $entryDetail = 'gate 为 Deployment'
        }
    }

    $ingresses = @($items | Where-Object { $null -ne $_ -and $_.kind -eq 'Ingress' -and $_.metadata.name -eq 'gateway' })
    $ingress = $null
    $ingressKnown = $true
    $ingressDetail = "namespace $Namespace 里没有 Ingress gateway"
    if ($ingresses.Count -eq 0 -and $gates.Count -eq 0) {
        # 一个 gate 都没有 = namespace 多半已被删(上一次回滚中途放弃),Ingress 当时已随 namespace 一起没了:
        # "现在没有"证明不了"回滚前没有",与路由模式、入口形态同一口径判为读不到(fail-closed)。
        $ingressKnown = $false
        $ingressDetail = "namespace $Namespace 里一个 gate 都没有(namespace 可能已被上一次中途放弃的回滚删掉),无从判断回滚前有没有 Ingress gateway"
    }
    elseif ($ingresses.Count -gt 0) {
        # rules / tls 缺席时 @($null)[0] 取到 $null,不抛"对空数组取下标"。
        $ingress = [pscustomobject]@{
            Host      = [string](@($ingresses[0].spec.rules)[0].host)
            ClassName = [string]$ingresses[0].spec.ingressClassName
            TlsSecret = [string](@($ingresses[0].spec.tls)[0].secretName)
        }
        $ingressDetail = "namespace $Namespace 里有 Ingress gateway(host='$($ingress.Host)' class='$($ingress.ClassName)' tls='$($ingress.TlsSecret)')"
    }

    # 这份现状会存进 entry-state.json,同一 rid 重跑时读回,所以说明写「读这份现状的那次运行」,不写「本次」。
    $openFileLimit = [pscustomobject]@{ Mode = $null; Detail = '未核对(读这份现状的那次运行显式给了 -NodeOpenFileLimitMode,没读 scene Fleet)' }
    if ($null -ne $SceneFleets) {
        $sceneNames = Get-ZoneSceneWorkloadNames
        $sceneDeployments = @($items | Where-Object { $null -ne $_ -and $_.kind -eq 'Deployment' -and $_.metadata.name -in $sceneNames })
        $sceneFleetItems = @($SceneFleets.Items | Where-Object { $null -ne $_ -and $_.metadata.name -in $sceneNames })
        $openFileLimit = Resolve-ZoneOpenFileLimitMode -Workloads (@($gates) + $sceneDeployments + $sceneFleetItems) -FleetReadError ([string]$SceneFleets.Error)
    }

    return [pscustomobject]@{
        RouterMode     = $routerMode
        RouterDetail   = $routerDetail
        EntryMode      = $entryMode
        EntryDetail    = $entryDetail
        IngressKnown   = $ingressKnown
        Ingress        = $ingress
        IngressDetail  = $ingressDetail
        OpenFileLimitMode   = $openFileLimit.Mode
        OpenFileLimitDetail = $openFileLimit.Detail
        NamespaceEmpty = ($gates.Count -eq 0 -and $ingresses.Count -eq 0)
    }
}

<#
.SYNOPSIS
    第 0 步预检(-Apply):留空的 -GateRouterMode / -ClientEntryMode / -GatewayIngressHost / -NodeOpenFileLimitMode 逐项与集群现状核对,
    任一项不一致或判不出就在停服之前抛出,所有问题一次列全。

.DESCRIPTION
    k8s_deploy.ps1 每次部署都按本次参数重写 gate / Ingress、不从集群读回旧值(不粘滞);而 Step 1 删掉整个 zone namespace 之后,
    k8s_deploy.ps1 自己的集群现状预检(换形态、地址漂移、已有 Ingress)在 Step 6 面对的是空 namespace,一条都触发不了。
    所以留空的项只在「读得到、且与 Step 6 将要生成的一致」时放行;判不出同样拒绝 —— 判不出就无法证明不会翻转(AGENTS §11.3 fail-closed):
      - 路由模式:留空 = 落回默认值,与 gate 当前模式不同就是静默翻转(以 "0" 回退运行的 zone 被切回 "1",
        回退原因若仍在,gate 卡在依赖门,登录 / 匹配全部 no_target);
      - 入口形态:留空 = 落回默认值,external 的 zone 被重建成 podip,集群外玩家全部连不上;
      - Ingress:留空 = Step 6 不生成 Ingress,而 Step 1 已连同 namespace 删掉它,集群外玩家拿不到 gate 分配。
        读得到且确实没有才放行;namespace 里一个 gate 都没有时判不出(见 Get-ClusterZoneEntryState)。
      - nofile 模式(B0-4,2026-10-22):留空 = Step 6 按 k8s_deploy.ps1 -NodeOpenFileLimitMode 默认值重建 gate / scene
        (空串 = 不写 env,进程按 enforce)。以 warn 运行的 zone 落回 enforce 后,节点池 nofile 硬限仍不够时 gate / scene 在 Step 6 拒启,
        那时已停服、已回档、Step 5 已丢弃积压。与默认值折算出的模式一致才放行,判不出同样拒绝。
    拒绝发生在 Step 1 之前,此时没有任何改动;操作者确认后显式传参重跑即可(Ingress 的显式决定是 -GatewayIngressHost 或
    -ConfirmNoGatewayIngress)。显式给了的项不传对应开关,不在这里核对。
#>
function Assert-ZoneEntryKeptOnRollback {
    param(
        # Get-ClusterZoneEntryState 的返回值。
        [Parameter(Mandatory = $true)]$State,
        # Get-DeployParamDefaults 的返回值,须含 GateRouterMode / ClientEntryMode / NodeOpenFileLimitMode。
        [Parameter(Mandatory = $true)][hashtable]$DeployDefaults,
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$DeployScriptPath,
        [switch]$CheckRouterMode,
        [switch]$CheckEntryMode,
        [switch]$CheckIngress,
        [switch]$CheckOpenFileLimit
    )

    $problems = @()
    if ($CheckRouterMode) {
        $deployDefault = $DeployDefaults['GateRouterMode']
        if ($null -ne $deployDefault -and $State.RouterMode -eq $deployDefault) {
            Write-Host "  Current gate mode `"$($State.RouterMode)`" ($($State.RouterDetail)) matches the k8s_deploy.ps1 default; leaving it unset will preserve the mode" -ForegroundColor Green
        }
        else {
            $defaultText = if ($null -ne $deployDefault) { "`"$deployDefault`"" } else { "(读不到:$DeployScriptPath 的 -GateRouterMode 默认值不是字面常量或文件无法解析)" }
            $clusterText = if ($null -ne $State.RouterMode) { "`"$($State.RouterMode)`"" } else { '(读不到)' }
            $problems += ("gate 路由模式:未传 -GateRouterMode 时 Step 6 按 k8s_deploy.ps1 默认值 $defaultText 重新部署 gate," +
                "而 gate 当前模式为 $clusterText($($State.RouterDetail))。两者不一致或判不出时留空可能把回退态静默翻转" +
                "(以 `"0`" 回退运行的 zone 被切回 `"1`",回退原因若仍在,登录 / 匹配全部 no_target)。" +
                "请确认该 zone 应以哪种模式拉起,显式传 -GateRouterMode 0 或 -GateRouterMode 1。")
        }
    }
    if ($CheckEntryMode) {
        $deployDefault = $DeployDefaults['ClientEntryMode']
        if ($null -ne $deployDefault -and $State.EntryMode -eq $deployDefault) {
            Write-Host "  Current gate entry mode `"$($State.EntryMode)`" ($($State.EntryDetail)) matches the k8s_deploy.ps1 default; leaving it unset will preserve the mode" -ForegroundColor Green
        }
        else {
            $defaultText = if ($null -ne $deployDefault) { "`"$deployDefault`"" } else { "(读不到:$DeployScriptPath 的 -ClientEntryMode 默认值不是字面常量或文件无法解析)" }
            $clusterText = if ($null -ne $State.EntryMode) { "`"$($State.EntryMode)`"" } else { '(读不到)' }
            $problems += ("客户端入口形态:未传 -ClientEntryMode 时 Step 6 按 k8s_deploy.ps1 默认值 $defaultText 重建 gate," +
                "而 gate 当前入口形态为 $clusterText($($State.EntryDetail))。两者不一致或判不出时留空可能把入口静默翻转" +
                "(external 的 zone 被重建成 podip,集群外玩家全部连不上)。" +
                "请确认后显式传 -ClientEntryMode podip 或 -ClientEntryMode external;external 须同时照传上一次部署的地址参数" +
                "(-GateServiceType、-GateNodePortBase、-ClientPublicHost / -GateClientHostTemplate、-GateExternalTrafficPolicy)。")
        }
    }
    if ($CheckIngress) {
        if (-not $State.IngressKnown) {
            $problems += ("gateway Ingress:判不出回滚前 namespace $Namespace 里有没有 Ingress gateway($($State.IngressDetail))。" +
                "Step 1 删 namespace 会连它一起删,未传 -GatewayIngressHost 时 Step 6 不会重建。请按该 zone 的部署记录照传 " +
                "-GatewayIngressHost / -GatewayTrustedProxies(及 ClassName / TlsSecret);确认该 zone 不经 Ingress 对外时加 -ConfirmNoGatewayIngress。")
        }
        elseif ($null -ne $State.Ingress) {
            $keep = "-GatewayIngressHost $($State.Ingress.Host) -GatewayTrustedProxies <上一次部署的 CIDR>"
            if ($State.Ingress.ClassName) { $keep += " -GatewayIngressClassName $($State.Ingress.ClassName)" }
            if ($State.Ingress.TlsSecret) { $keep += " -GatewayIngressTlsSecret $($State.Ingress.TlsSecret)" }
            $problems += ("gateway Ingress:$($State.IngressDetail)。Step 1 删 namespace 会连它一起删,未传 -GatewayIngressHost 时 Step 6 不会重建," +
                "集群外玩家拿不到 gate 分配。请照传 $keep;确要撤掉 Ingress,加 -ConfirmNoGatewayIngress。")
        }
        else {
            Write-Host "  $($State.IngressDetail); leaving it unset will preserve Ingress" -ForegroundColor Green
        }
    }
    if ($CheckOpenFileLimit) {
        $deployDefault = $DeployDefaults['NodeOpenFileLimitMode']
        # 留空 = Step 6 不传,k8s_deploy.ps1 按它的默认值写 env(空串即不写);折算口径同 Get-OpenFileLimitModeFromPodSpec:只有逐字 warn 是豁免。
        $step6Mode = if ($null -eq $deployDefault) { $null } elseif ($deployDefault -ceq 'warn') { 'warn' } else { 'enforce' }
        # 旧存档(Schema 3 之前)的集群现状没有这个字段,读出来是 $null,同样判不出。
        $clusterMode = $State.OpenFileLimitMode
        if ($null -ne $step6Mode -and $clusterMode -ceq $step6Mode) {
            Write-Host "  Current gate / scene nofile mode `"$clusterMode`" ($($State.OpenFileLimitDetail)) matches the k8s_deploy.ps1 default; leaving -NodeOpenFileLimitMode unset will preserve the mode" -ForegroundColor Green
        }
        else {
            $defaultText = if ($null -ne $deployDefault) { "`"$deployDefault`"(gate / scene 按 $step6Mode 运行)" } else { "(读不到:$DeployScriptPath 的 -NodeOpenFileLimitMode 默认值不是字面常量或文件无法解析)" }
            $clusterText = if ($null -ne $clusterMode) { "`"$clusterMode`"" } else { '(读不到)' }
            $keep = if ($clusterMode -ceq 'warn') {
                '请照传 -NodeOpenFileLimitMode warn;确认节点池 nofile 硬限已修好、要回到 enforce,显式传 -NodeOpenFileLimitMode enforce。'
            }
            else { '请确认该 zone 应以哪种模式拉起,显式传 -NodeOpenFileLimitMode warn 或 -NodeOpenFileLimitMode enforce。' }
            $problems += ("nofile 豁免:未传 -NodeOpenFileLimitMode 时 Step 6 按 k8s_deploy.ps1 默认值 $defaultText 重建 gate / scene," +
                "而它们当前为 $clusterText($($State.OpenFileLimitDetail))。两者不一致或判不出时留空可能把 nofile 模式静默翻转" +
                "(以 warn 运行的 zone 落回 enforce,节点池 nofile 硬限仍不够时 gate / scene 在 Step 6 拒启,那时已停服、已回档、积压已丢弃," +
                "只能补上参数单独重跑 Step 6)。" + $keep)
        }
    }
    if ($problems.Count -eq 0) { return }
    throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:Step 1 会删掉 namespace $Namespace,Step 6 只按本次参数重建," +
        "留空的参数落回 k8s_deploy.ps1 默认值、不从集群读回旧值。以下 $($problems.Count) 项与集群现状不一致或判不出:`n  - " +
        ($problems -join "`n  - ") +
        "`n经包装入口调用而它不透传所需参数时,直接调用 tools/scripts/k8s_zone_rollback.ps1。")
}

<#
.SYNOPSIS
    Step 6 的入口决定(纯函数,回档复核 [10]):留空的路由模式 / 入口形态 / Ingress 取第 0 步 a 核对过的集群现状,显式项取参数值。
    返回 [ordered] 表,值一律是字符串(没给的显式参数为空串)。第 0 步存进 entry-state.json,同一 rid 重跑逐项比较。

.DESCRIPTION
    只在 Assert-ZoneEntryKeptOnRollback 放行之后调用:那时留空项已证明「集群现状 = k8s_deploy.ps1 默认值」,记集群现状就是记 Step 6
    实际生效的值,所以「首次留空、重跑显式传同值」判为相同。GatewayIngress:显式给了 host 记 host;-ConfirmNoGatewayIngress,
    或读到确实没有 Ingress,记 '(none)'。Ingress 的 class / tls / trusted proxies、地址参数与 -RequireClientEndpoint 只记显式值
    (不粘滞,留空即 k8s_deploy.ps1 默认值,集群现状核对不了它们)。
.PARAMETER Explicit
    本次的显式参数:键为参数名,未给为空串(GateNodePortBase 未给传空串而不是 -1),ConfirmNoGatewayIngress、
    LoginWeChatAllowOpenId、LoginQQAllowOpenId、ConfirmLoginAuthProviderChange 为布尔。
#>
function Get-ZoneEntryDecision {
    param(
        [AllowNull()]$State,
        [Parameter(Mandatory = $true)][System.Collections.IDictionary]$Explicit
    )

    $asText = { param($Value) if ($null -eq $Value) { '' } else { [string]$Value } }
    $routerMode = & $asText $Explicit['GateRouterMode']
    if ($routerMode -eq '' -and $null -ne $State) { $routerMode = & $asText $State.RouterMode }
    $entryMode = & $asText $Explicit['ClientEntryMode']
    if ($entryMode -eq '' -and $null -ne $State) { $entryMode = & $asText $State.EntryMode }
    $ingress = & $asText $Explicit['GatewayIngressHost']
    if ($ingress -eq '') {
        if ([bool]$Explicit['ConfirmNoGatewayIngress']) { $ingress = '(none)' }
        elseif ($null -ne $State -and [bool]$State.IngressKnown -and $null -eq $State.Ingress) { $ingress = '(none)' }
    }
    $decision = [ordered]@{ GateRouterMode = $routerMode; ClientEntryMode = $entryMode; GatewayIngress = $ingress }
    foreach ($name in @('GatewayIngressClassName', 'GatewayIngressTlsSecret', 'GatewayTrustedProxies', 'ClientPublicHost', 'GateClientHostTemplate',
            'GateNodePortBase', 'GateExternalTrafficPolicy', 'GateServiceType', 'RequireClientEndpoint')) {
        $decision[$name] = & $asText $Explicit[$name]
    }
    # login 第三方登录(微信 / QQ):只记显式值,追加在末尾。留空即「不开」,第 0 步 a 的防误关核对已证明线上与之一致(或已确认关闭),
    # 所以显式值就是 Step 6 实际生效的值。switch 记 'true' / 空串。接入之前写的存档(Schema 1,没有 LoginAuthState)没有这些键,
    # 读出来是空串:它们没记过第三方登录的决定,主流程比较时把这七个键剔掉,不拿空串去比(否则照传或确认都会被判为与存档不同)。
    foreach ($name in @('LoginWeChatAppId', 'LoginWeChatEndpoint', 'LoginWeChatAllowOpenId', 'LoginQQAppId', 'LoginQQEndpoint', 'LoginQQAllowOpenId',
            'ConfirmLoginAuthProviderChange')) {
        if ($name -in @('LoginWeChatAllowOpenId', 'LoginQQAllowOpenId', 'ConfirmLoginAuthProviderChange')) {
            $decision[$name] = if ([bool]$Explicit[$name]) { 'true' } else { '' }
        }
        else { $decision[$name] = & $asText $Explicit[$name] }
    }
    return $decision
}

# 取入口决定里的一项(存档经 JSON 往返后是 pscustomobject,测试里可能是 hashtable),缺省为空串。纯函数。
function Get-ZoneEntryDecisionValue {
    param([AllowNull()]$Decision, [Parameter(Mandatory = $true)][string]$Name)

    if ($null -eq $Decision) { return '' }
    if ($Decision -is [System.Collections.IDictionary]) {
        if ($Decision.Contains($Name) -and $null -ne $Decision[$Name]) { return [string]$Decision[$Name] }
        return ''
    }
    $property = $Decision.PSObject.Properties[$Name]
    if ($null -ne $property -and $null -ne $property.Value) { return [string]$property.Value }
    return ''
}

<#
.SYNOPSIS
    本次入口决定与同一 rid 存档逐项比较(纯函数),返回差异列表 [string[]];空表示相同。比较区分大小写。
#>
function Compare-ZoneEntryDecision {
    param(
        [AllowNull()]$Saved,
        [Parameter(Mandatory = $true)][System.Collections.IDictionary]$Current
    )

    $diffs = [System.Collections.Generic.List[string]]::new()
    foreach ($name in $Current.Keys) {
        $savedValue = Get-ZoneEntryDecisionValue -Decision $Saved -Name $name
        $currentValue = [string]$Current[$name]
        if ($savedValue -cne $currentValue) { $diffs.Add("${name}:存档 '$savedValue',本次 '$currentValue'") }
    }
    return , $diffs.ToArray()
}

<#
.SYNOPSIS
    入口决定 → 照传用的参数文本(纯函数),如 "-GateRouterMode 1 -ClientEntryMode podip -ConfirmNoGatewayIngress";用于重跑提示与拒绝文本。
#>
function Format-ZoneEntryDecisionArgs {
    param([AllowNull()]$Decision)

    $parts = [System.Collections.Generic.List[string]]::new()
    foreach ($name in @('GateRouterMode', 'ClientEntryMode')) {
        $value = Get-ZoneEntryDecisionValue -Decision $Decision -Name $name
        if ($value -ne '') { $parts.Add("-$name $value") }
    }
    $ingress = Get-ZoneEntryDecisionValue -Decision $Decision -Name 'GatewayIngress'
    if ($ingress -ceq '(none)') { $parts.Add('-ConfirmNoGatewayIngress') }
    elseif ($ingress -ne '') { $parts.Add("-GatewayIngressHost $ingress") }
    foreach ($name in @('GatewayIngressClassName', 'GatewayIngressTlsSecret', 'GatewayTrustedProxies', 'ClientPublicHost', 'GateClientHostTemplate',
            'GateNodePortBase', 'GateExternalTrafficPolicy', 'GateServiceType', 'RequireClientEndpoint')) {
        $value = Get-ZoneEntryDecisionValue -Decision $Decision -Name $name
        if ($value -ne '') { $parts.Add("-$name $value") }
    }
    # 第三方登录按 provider 分组(与 lib Get-LoginAuthProviderDrift 的 KeepArgs 同一顺序),switch 记的是 'true' / 空串。
    foreach ($name in @('LoginWeChatAppId', 'LoginWeChatEndpoint', 'LoginWeChatAllowOpenId', 'LoginQQAppId', 'LoginQQEndpoint', 'LoginQQAllowOpenId',
            'ConfirmLoginAuthProviderChange')) {
        $value = Get-ZoneEntryDecisionValue -Decision $Decision -Name $name
        if ($value -eq '') { continue }
        if ($name -in @('LoginWeChatAllowOpenId', 'LoginQQAllowOpenId', 'ConfirmLoginAuthProviderChange')) {
            if ($value -ceq 'true') { $parts.Add("-$name") }
        }
        else { $parts.Add("-$name $value") }
    }
    return ($parts -join ' ')
}

<#
.SYNOPSIS
    第 0 步静态预检(-Apply):把 Step 6 将要传给 dev_tools.ps1 k8s-zone-up 的同一份参数加上 -DryRun 执行一遍,
    失败即在停服之前抛出;成功只回显其中的告警。

.DESCRIPTION
    参数组合的规则(入口形态与服务类型、模板与副本数、nodePort 段、ClientPublicHost / Ingress host / CIDR 的格式、
    -RequireClientEndpoint 与入口形态的矛盾、-LoginDevPasswordAuth 只许 dev 档、第三方登录 -Login* 的形状与
    -LoginWeChatAppId 要的 MMORPG_LOGIN_WECHAT_APP_SECRET、档位对应的密钥与发布门禁 …)只在
    k8s_deploy.ps1 一处(lib Test-ClientEntryPreflight 与各 Assert-*)。本脚本不复制规则,而是让那边对 Step 6 的真实参数
    完整渲染一遍 zone-up:拖到 Step 6 才被拒时 zone 已停服、数据已回档,回滚卡在半路。
    k8s_deploy.ps1 -DryRun 不调 kubectl(集群现状预检、服务端预演都跳过),只在本地渲染清单并打印,停服前执行没有副作用;
    渲染路径里的生成器缺陷(如某条分支参数写错)也会在这里暴露,而不是在 Step 6。
    失败有两种形态都认:throw(k8s_deploy.ps1 的惯例)与非 0 退出码。
    输出整段捕获:dry-run 会打印全部清单(上千行),成功时只回显告警,失败时回显末尾 -TailLines 行与原因。

.PARAMETER UpArgs
    Step 6 的参数表,原样使用(本函数只在副本上加 DryRun),保证预检的就是 Step 6 将要执行的那一份。
#>
function Invoke-ZoneUpStaticPreflight {
    param(
        [Parameter(Mandatory = $true)][string]$DevToolsPath,
        [Parameter(Mandatory = $true)][hashtable]$UpArgs,
        [int]$TailLines = 40
    )

    $dryRunArgs = $UpArgs.Clone()
    $dryRunArgs.DryRun = $true
    $captured = [System.Collections.Generic.List[object]]::new()
    $failure = $null
    # 被调脚本不以 exit 结束时 $LASTEXITCODE 保留更早的值,先清零,才能把它的非 0 判成本次失败。
    $global:LASTEXITCODE = 0
    try {
        & $DevToolsPath @dryRunArgs *>&1 | ForEach-Object { $captured.Add($_) }
        if ($LASTEXITCODE -ne 0) { $failure = "dev_tools.ps1 以退出码 $LASTEXITCODE 结束" }
    }
    catch {
        $failure = $_.Exception.Message
    }

    if ($null -eq $failure) {
        foreach ($record in @($captured | Where-Object { $_ -is [System.Management.Automation.WarningRecord] })) {
            Write-Host "  Warning (k8s-zone-up -DryRun): $($record.Message)" -ForegroundColor Yellow
        }
        Write-Host "  k8s-zone-up -DryRun passed: Step 6 arguments passed static preflight in k8s_deploy.ps1; manifests rendered successfully" -ForegroundColor Green
        return
    }

    $lines = @($captured | ForEach-Object { "$_" })
    if ($lines.Count -gt 0) {
        $shown = [Math]::Min($TailLines, $lines.Count)
        Write-Host "  Last $shown lines of k8s-zone-up -DryRun output:" -ForegroundColor DarkGray
        $lines | Select-Object -Last $TailLines | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
    }
    throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:Step 6 将要执行的 dev_tools.ps1 k8s-zone-up 没通过 -DryRun 静态预检" +
        "(参数组合、档位与密钥、发布门禁都在 k8s_deploy.ps1 这一步判定;拖到 Step 6 才失败时 zone 已停服、数据已回档)。" +
        "`n原因:$failure`n修正参数后重跑。")
}

<#
.SYNOPSIS
    把 -TargetTime / -PitrRestoredTo 解析成 UTC [DateTime];不是 yyyy-MM-ddTHH:mm:ss[.f{1,3}]Z 即 throw(入口校验 E1)。纯函数。

.DESCRIPTION
    Step 5 改成 -ToLatest 之后,Kafka 不再顺带校验 TargetTime(以前 --to-datetime 解析不了会报错),而 Step 3 的 PITR 命令、
    工具 inventory 与回档报告都以它为准。只收带 Z 的 UTC 写法:空格分隔、时区偏移(+08:00)这类写法在 PITR 时最容易换算错,一律拒绝。
    ParseExact 用 InvariantCulture 与 AssumeUniversal | AdjustToUniversal,结果 Kind = Utc。
#>
function ConvertFrom-RollbackUtcTime {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text,
        [Parameter(Mandatory = $true)][string]$Name
    )

    $refusal = "k8s-zone-rollback 拒绝执行,尚未产生任何改动:"
    if ($Text -cnotmatch '^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,3})?Z$') {
        throw ($refusal + "-$Name '$Text' 不是 UTC 的 yyyy-MM-ddTHH:mm:ssZ(可带 1–3 位毫秒,如 2026-05-15T14:23:00Z、2026-05-15T14:23:00.250Z);" +
            "不收空格分隔与时区偏移,PITR 与回档报告都按这个时刻换算。")
    }
    $formats = [string[]]@("yyyy-MM-dd'T'HH:mm:ss'Z'", "yyyy-MM-dd'T'HH:mm:ss.f'Z'", "yyyy-MM-dd'T'HH:mm:ss.ff'Z'", "yyyy-MM-dd'T'HH:mm:ss.fff'Z'")
    $styles = [System.Globalization.DateTimeStyles]::AssumeUniversal -bor [System.Globalization.DateTimeStyles]::AdjustToUniversal
    $parsed = [datetime]::MinValue
    if (-not [datetime]::TryParseExact($Text, $formats, [System.Globalization.CultureInfo]::InvariantCulture, $styles, [ref]$parsed)) {
        throw ($refusal + "-$Name '$Text' 不是合法的日期时间(月、日或时分秒越界)。")
    }
    return $parsed
}

<#
.SYNOPSIS
    rollback-id 的缺省值 z<ZoneId>-<TargetTime 紧凑 UTC>-<当前紧凑 UTC>,如 z101-20260515T142300Z-20261018T031500Z。纯函数。
#>
function New-RollbackId {
    param(
        [Parameter(Mandatory = $true)][int]$ZoneId,
        [Parameter(Mandatory = $true)][datetime]$TargetUtc,
        [Parameter(Mandatory = $true)][datetime]$NowUtc
    )

    $format = "yyyyMMdd'T'HHmmss'Z'"
    $culture = [System.Globalization.CultureInfo]::InvariantCulture
    return "z$ZoneId-$($TargetUtc.ToString($format, $culture))-$($NowUtc.ToString($format, $culture))"
}

<#
.SYNOPSIS
    组装一次 zone_rollback_discard 调用的参数(纯函数:只拼参数,不执行)。dry-run 打印的就是这一份,-Apply 时原样执行。

.PARAMETER Common
    公共参数(有序,键为不带 "-" 的参数名),见文件头「zone_rollback_discard 契约」;值为 $null 或空串的项不输出
    (如 Redis 没开认证时的 redis-password-env)。
.PARAMETER Extra
    模式自己的参数,原样追加在 -mode <模式> 之后。
#>
function Get-DiscardToolArgs {
    param(
        [Parameter(Mandatory = $true)][ValidateSet('preflight', 'stopped', 'inventory', 'purge', 'verify')][string]$Mode,
        [Parameter(Mandatory = $true)][System.Collections.Specialized.OrderedDictionary]$Common,
        [string[]]$Extra = @()
    )

    $argv = [System.Collections.Generic.List[string]]::new()
    foreach ($key in $Common.Keys) {
        $value = $Common[$key]
        if ($null -eq $value -or "$value" -eq '') { continue }
        $argv.Add("-$key")
        $argv.Add("$value")
    }
    $argv.Add('-mode')
    $argv.Add($Mode)
    foreach ($item in $Extra) { $argv.Add($item) }
    return , $argv.ToArray()
}

# 打印用的命令行:含空白或为空的参数加单引号(只用于显示,执行时按数组原样传)。
function Format-CommandLine {
    param(
        [Parameter(Mandatory = $true)][string]$Program,
        [string[]]$Arguments = @()
    )

    $parts = foreach ($item in (@($Program) + @($Arguments))) {
        $text = "$item"
        if ($text -eq '' -or $text -match '\s') { "'" + $text.Replace("'", "''") + "'" } else { $text }
    }
    return ($parts -join ' ')
}

# 回档工具退出码的含义(契约见文件头),只用于错误文本。
function Get-DiscardToolExitMeaning {
    param([Parameter(Mandatory = $true)][int]$ExitCode)

    switch ($ExitCode) {
        0 { return '通过' }
        1 { return '出错(可能只做了一部分;修好后用同一 rid 重跑,幂等)' }
        3 { return '拒绝(什么都没写)' }
        4 { return '目标看起来没在用(本 topic 一个已落库游标都没有;-RedisHost / -RedisDB 多半指错了实例)' }
        6 { return '有 blocked 玩家' }
        default { return '契约之外的退出码,按出错处理' }
    }
}

<#
.SYNOPSIS
    执行一次 zone_rollback_discard,输出逐行缩进回显,返回退出码(不判成败,由调用方按契约解释)。

.DESCRIPTION
    局部放宽 ErrorActionPreference:工具往 stderr 打的诊断不能被当成终止错误,成败只看退出码。先清零 LASTEXITCODE,
    不把更早一次原生调用的退出码当成本次的。工具本身起不来(文件不存在、不可执行、脚本抛出)按退出码 1(出错)报告并回显原因。
    口令与 DSN 不经这里:工具从 -redis-password-env / -mysql-dsn-env 指名的环境变量自己读。
#>
function Invoke-DiscardTool {
    param(
        [Parameter(Mandatory = $true)][string]$ToolPath,
        [Parameter(Mandatory = $true)][string[]]$Arguments
    )

    $prevErrorAction = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $exitCode = 1
    try {
        $global:LASTEXITCODE = 0
        & $ToolPath @Arguments 2>&1 | ForEach-Object { Write-Host "    $_" }
        $exitCode = [int]$LASTEXITCODE
    }
    catch {
        Write-Host "    zone_rollback_discard failed to run: $($_.Exception.Message)" -ForegroundColor Red
        $exitCode = 1
    }
    finally {
        $ErrorActionPreference = $prevErrorAction
    }
    return $exitCode
}

<#
.SYNOPSIS
    第 0 步 f 的 kubectl 参数(只读):zone namespace 里 go/db 与 data-service 的 ConfigMap、db 与 login 的 Deployment。dry-run 打印的就是这一份。

.DESCRIPTION
    ConfigMap 名与数据键同 k8s_deploy.ps1 的 $GoSvcCatalogue(db → go-svc-db-config / db.yaml,data-service → go-svc-data-service-config /
    data_service.yaml),Deployment 与容器名同 deploy/k8s/manifests/go-svc/db.yaml、login.yaml;那边改名时契约测试会报红。
    --ignore-not-found:namespace 已被删(同一 rid 重跑)时无输出、退出码 0,由调用方改用报告目录里的存档。
#>
function Get-DeploySnapshotKubectlArgs {
    param([Parameter(Mandatory = $true)][string]$Namespace)

    return , @('get', 'configmap/go-svc-db-config', 'configmap/go-svc-data-service-config', 'deployment/db', 'deployment/login',
        '-n', $Namespace, '-o', 'json', '--ignore-not-found', '--request-timeout=15s')
}

<#
.SYNOPSIS
    把第 0 步 f 那次 kubectl 的 JSON 输出折成非机密的部署事实 [pscustomobject](纯函数);输出为空(namespace 已删)返回 $null,
    必需项读不出即 throw。

.DESCRIPTION
    只取白名单里的键,口令(RedisClient.Password、MySQL DSN 等)一律不读,结果可以落盘进报告目录、交给回档工具(-deploy-facts)。
      - go-svc-db-config 的 data.db.yaml:ZoneId、ServerConfig.RedisClient.Hosts / DB(DB 缺省 0)、ServerConfig.OwnerEpochRedis.Hosts / DB、
        Placement.Redis.Hosts / DB(两段缺省 = 复用 RedisClient,记为 $null)、ServerConfig.Fencing.Mode(整段缺省 = dual,与 go/db 的
        FencingConfig.EffectiveMode 同口径;2026-10-18 之前缺省 legacy)、
        ServerConfig.Kafka.TopicGeneration / PartitionCnt,以及 db 消费组的形状 KafkaGroup = { Shape; Group; HandoffFrom; Detail }:
        按 lib ConvertFrom-LiveDbConsumerGroup(k8s_deploy.ps1 判组的同一份规则)归到 final(G) / handoff(X←Y),-LegacyGroup / -ZoneGroup 即 L / Z。
        归不进四种形状时不 throw,记 Shape = 'unrecognized' 与原因(Detail),由 Get-DeployFactsProblems 拒绝 —— 线上 GroupID 只在 final(L) 下等于基名,
        final(Z) 写的是 L_z<id>,交接形状根本不写 GroupID,以前拿 GroupID 原值比基名,切到 zone 组的 zone(包括回档过一次的)一律被拒(回档复核 [0]);
      - go-svc-data-service-config 的 data.data_service.yaml:MappingRedis.Host(go-zero 的 redis 配置,固定 DB 0);
      - db Deployment 里名为 db 的容器镜像(必需,第 0 步 f 据它核对回档工具的构建提交);login Deployment 里名为 login 的容器镜像(只记录,可缺)。
    有输出但两份 ConfigMap 缺任一、缺数据键、缺 db 镜像,都 throw:namespace 还在却读不全,说明不是本仓生成的形状,不猜。
    Schema 2(2026-10-18):KafkaGroupBase 换成 KafkaGroup;Schema 1 的存档与现读必然不同,同一 rid 重跑时按「部署事实与存档不同」拒绝。
#>
function ConvertFrom-ZoneDeploySnapshot {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Json,
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$LegacyGroup,
        [Parameter(Mandatory = $true)][string]$ZoneGroup
    )

    if ([string]::IsNullOrWhiteSpace($Json)) { return $null }
    try {
        $document = $Json | ConvertFrom-Json -ErrorAction Stop
    }
    catch {
        throw "第 0 步 f 的 kubectl 输出不是合法的 JSON(namespace $Namespace):$($_.Exception.Message)"
    }
    $items = if ($document.kind -eq 'List') { @($document.items) } else { @($document) }
    $items = @($items | Where-Object { $null -ne $_ })

    $readConfigMapFile = {
        param([string]$Name, [string]$Key)
        $configMap = @($items | Where-Object { $_.kind -eq 'ConfigMap' -and $_.metadata.name -eq $Name }) | Select-Object -First 1
        if ($null -eq $configMap) { throw "namespace $Namespace 里读不到 ConfigMap $Name(namespace 还在,却不是本仓生成的形状)" }
        $dataProperty = $configMap.PSObject.Properties['data']
        $fileProperty = if ($null -ne $dataProperty -and $null -ne $dataProperty.Value) { $dataProperty.Value.PSObject.Properties[$Key] } else { $null }
        if ($null -eq $fileProperty -or [string]::IsNullOrWhiteSpace([string]$fileProperty.Value)) {
            throw "namespace $Namespace 的 ConfigMap $Name 没有 data.$Key"
        }
        $fileText = [string]$fileProperty.Value
        return [pscustomobject]@{ Text = $fileText; Scalars = (ConvertFrom-YamlToFlatMap -Text $fileText).Scalars }
    }
    $readImage = {
        param([string]$Deployment, [switch]$Required)
        $workload = @($items | Where-Object { $_.kind -eq 'Deployment' -and $_.metadata.name -eq $Deployment }) | Select-Object -First 1
        $container = if ($null -ne $workload) { @($workload.spec.template.spec.containers | Where-Object { $_.name -eq $Deployment }) | Select-Object -First 1 } else { $null }
        $image = if ($null -ne $container) { [string]$container.image } else { '' }
        if ($Required -and [string]::IsNullOrWhiteSpace($image)) {
            throw "namespace $Namespace 里读不到 Deployment $Deployment 的 $Deployment 容器镜像,无法核对回档工具与线上 go/db 是否同一提交"
        }
        if ([string]::IsNullOrWhiteSpace($image)) { return $null }
        return $image
    }
    $scalar = {
        param($Map, [string]$Key, $Default = $null)
        if ($Map.Contains($Key) -and -not [string]::IsNullOrWhiteSpace([string]$Map[$Key])) { return ([string]$Map[$Key]).Trim() }
        return $Default
    }
    $redisSection = {
        param($Map, [string]$Prefix)
        $hosts = & $scalar $Map "$Prefix.Hosts"
        if ($null -eq $hosts) { return $null }
        return [pscustomobject][ordered]@{ Hosts = $hosts; DB = (& $scalar $Map "$Prefix.DB" '0') }
    }

    $dbFile = & $readConfigMapFile 'go-svc-db-config' 'db.yaml'
    $db = $dbFile.Scalars
    $dataService = (& $readConfigMapFile 'go-svc-data-service-config' 'data_service.yaml').Scalars
    try {
        $liveGroup = ConvertFrom-LiveDbConsumerGroup -DbYaml $dbFile.Text -LegacyGroup $LegacyGroup -ZoneGroup $ZoneGroup
        $kafkaGroup = [pscustomobject][ordered]@{ Shape = $liveGroup.Shape; Group = $liveGroup.Group; HandoffFrom = $liveGroup.HandoffFrom; Detail = '' }
    }
    catch {
        $kafkaGroup = [pscustomobject][ordered]@{ Shape = 'unrecognized'; Group = ''; HandoffFrom = ''; Detail = $_.Exception.Message }
    }
    return [pscustomobject][ordered]@{
        Schema               = 2
        Namespace            = $Namespace
        ZoneId               = (& $scalar $db 'ZoneId')
        FencingMode          = (& $scalar $db 'ServerConfig.Fencing.Mode' 'dual')
        Redis                = (& $redisSection $db 'ServerConfig.RedisClient')
        OwnerEpochRedis      = (& $redisSection $db 'ServerConfig.OwnerEpochRedis')
        PlacementRedis       = (& $redisSection $db 'Placement.Redis')
        MappingRedisHost     = (& $scalar $dataService 'MappingRedis.Host')
        KafkaGroup           = $kafkaGroup
        KafkaTopicGeneration = (& $scalar $db 'ServerConfig.Kafka.TopicGeneration')
        KafkaPartitionCnt    = (& $scalar $db 'ServerConfig.Kafka.PartitionCnt')
        DbImage              = (& $readImage 'db' -Required)
        LoginImage           = (& $readImage 'login')
    }
}

<#
.SYNOPSIS
    部署事实与本次参数、本机 go/db/etc/db.yaml 的逐项核对(纯函数),返回问题列表 [string[]];空表示放行。

.DESCRIPTION
      - ZoneId 必须等于 -ZoneId;Kafka 世代 / 分区数必须等于本机 db.yaml(topic 是按本机 db.yaml 推导的,
        线上与之不同时第 0 步 c–e 与 Step 5 改的不是线上在用的 topic);
      - db 消费组形状(回档复核 [0]):L = -LocalGroupBase(本机 db.yaml 的 GroupID 基名),Z = L_z<ZoneId>。只放行 final(L)、final(Z)、
        handoff(Z←L)、handoff(L←Z) —— 与 k8s_deploy.ps1 生成与认定的四种形状相同;Shape 为 unrecognized(ConvertFrom-ZoneDeploySnapshot
        归不进四种形状)或组名不属于 {L, Z} 都拒绝。存档经 JSON 往返后在这里按本机 L / Z 再判一次,不信存档里的结论;
      - RedisClient.Hosts 必须是单个 host:port(工具的每玩家 Lua 跨键原子执行,不支持 Cluster),DB 必须等于 -RedisDB;
        主机名不在这里比对:-RedisHost 可以是端口转发地址,实例身份由 run_id 核对(Read-GoDbRedisRunId → 工具的 -expect-run-id,回档复核 [4]);
      - OwnerEpochRedis / Placement.Redis 要么缺省(复用 RedisClient),要么与 RedisClient 同 Hosts 同 DB;
        data-service 的 MappingRedis.Host 必须等于 RedisClient.Hosts 且 RedisClient.DB 为 0(MappingRedis 固定 DB 0)——
        映射、落点、归属计数器与缓存不在同一实例同一 DB 时,工具做不到「校验与删除在同一段 Lua 里」,只能拒绝(对抗复核 [1][9]);
      - -BarrierProtocol legacy 时 Fencing.Mode 必须是 legacy(大小写不敏感):dual 下 go/db 写提交屏障 ACK、saveoutbox 在跑,
        那是严格屏障世代,回档工具对它的处置待负责人裁决 #3。
#>
function Get-DeployFactsProblems {
    param(
        [Parameter(Mandatory = $true)]$Facts,
        [Parameter(Mandatory = $true)][int]$ZoneId,
        [Parameter(Mandatory = $true)][int]$RedisDB,
        [Parameter(Mandatory = $true)][string]$BarrierProtocol,
        [Parameter(Mandatory = $true)][string]$LocalGroupBase,
        [Parameter(Mandatory = $true)][int]$LocalTopicGeneration,
        [Parameter(Mandatory = $true)][int]$LocalPartitionCnt
    )

    $problems = [System.Collections.Generic.List[string]]::new()
    if ([string]$Facts.ZoneId -ne "$ZoneId") {
        $problems.Add("线上 go-svc-db-config 的 ZoneId 是 '$($Facts.ZoneId)',不是 -ZoneId $ZoneId(namespace 指错了 zone?)")
    }
    foreach ($pair in @(
            @{ Name = 'ServerConfig.Kafka.TopicGeneration'; Live = [string]$Facts.KafkaTopicGeneration; Local = "$LocalTopicGeneration" },
            @{ Name = 'ServerConfig.Kafka.PartitionCnt'; Live = [string]$Facts.KafkaPartitionCnt; Local = "$LocalPartitionCnt" })) {
        if ($pair.Live -cne $pair.Local) {
            $problems.Add("线上 $($pair.Name) 是 '$($pair.Live)',本机 go/db/etc/db.yaml 是 '$($pair.Local)':topic 按本机推导,改的就不是线上在用的那一个;先把工作树的 db.yaml 对齐到该 zone 实际部署的值")
        }
    }
    # db 消费组:按本机 L / Z 判线上形状属于四种之一(不拿 GroupID 原值比基名:final(Z) 写 L_z<id>,交接形状不写 GroupID)。
    $legacyGroup = $LocalGroupBase
    $zoneGroup = "${LocalGroupBase}_z$ZoneId"
    $known = "{$legacyGroup, $zoneGroup}"
    $group = $Facts.KafkaGroup
    $shape = if ($null -ne $group) { [string]$group.Shape } else { '' }
    $groupName = if ($null -ne $group) { [string]$group.Group } else { '' }
    $handoffFrom = if ($null -ne $group) { [string]$group.HandoffFrom } else { '' }
    $groupProblem = $null
    if ($shape -ceq 'final') {
        if (@($legacyGroup, $zoneGroup) -cnotcontains $groupName) { $groupProblem = "最终形状 GroupID=$groupName" }
    }
    elseif ($shape -ceq 'handoff') {
        $isZoneFromLegacy = ($groupName -ceq $zoneGroup -and $handoffFrom -ceq $legacyGroup)
        $isLegacyFromZone = ($groupName -ceq $legacyGroup -and $handoffFrom -ceq $zoneGroup)
        if (-not ($isZoneFromLegacy -or $isLegacyFromZone)) { $groupProblem = "交接形状 ConsumerGroup=$groupName GroupHandoffFrom=$handoffFrom" }
    }
    else {
        $detail = if ($null -ne $group -and -not [string]::IsNullOrWhiteSpace([string]$group.Detail)) { [string]$group.Detail } else { '部署事实里没有可识别的 db 消费组形状' }
        $groupProblem = "形状无法识别($detail)"
    }
    if ($null -ne $groupProblem) {
        $problems.Add("线上 go-svc-db-config 的 db 消费组形状不属于 ${known}:$groupProblem。只认 final(L)、final(Z)、handoff(Z←L)、handoff(L←Z)" +
            "(L 为本机 go/db/etc/db.yaml 的 GroupID 基名,Z = L_z$ZoneId);线上用的是别的组名时,Step 5 重置与退役的不是该 zone 在用的消费组。" +
            "查清该 zone 实际在用的消费组与部署来源后再回档")
    }
    $redis = $Facts.Redis
    if ($null -eq $redis -or [string]::IsNullOrWhiteSpace([string]$redis.Hosts)) {
        $problems.Add('线上 go-svc-db-config 读不到 ServerConfig.RedisClient.Hosts')
    }
    else {
        if ([string]$redis.Hosts -match '[,\s]') {
            $problems.Add("线上 RedisClient.Hosts 是 '$($redis.Hosts)'(多个地址):回档工具的每玩家 Lua 跨键原子执行,只支持单实例")
        }
        if ([string]$redis.DB -ne "$RedisDB") {
            $problems.Add("线上 RedisClient.DB 是 '$($redis.DB)',不是 -RedisDB ${RedisDB}:工具会在别的 DB 里盘点与丢弃")
        }
        foreach ($section in @(@{ Name = 'ServerConfig.OwnerEpochRedis'; Value = $Facts.OwnerEpochRedis }, @{ Name = 'Placement.Redis'; Value = $Facts.PlacementRedis })) {
            if ($null -eq $section.Value) { continue }
            if ([string]$section.Value.Hosts -ne [string]$redis.Hosts -or [string]$section.Value.DB -ne [string]$redis.DB) {
                $problems.Add("线上 $($section.Name) 是 $($section.Value.Hosts)/db$($section.Value.DB),与 RedisClient $($redis.Hosts)/db$($redis.DB) 不是同一实例同一 DB:回档工具做不到跨实例原子复核,拒绝(Redis 拆分后的回档另行设计)")
            }
        }
        if ([string]::IsNullOrWhiteSpace([string]$Facts.MappingRedisHost)) {
            $problems.Add('线上 go-svc-data-service-config 读不到 MappingRedis.Host')
        }
        elseif ([string]$Facts.MappingRedisHost -ne [string]$redis.Hosts -or [string]$redis.DB -ne '0') {
            $problems.Add("data-service 的 MappingRedis 是 $($Facts.MappingRedisHost)/db0,与 go/db RedisClient $($redis.Hosts)/db$($redis.DB) 不是同一实例同一 DB:归属映射与玩家键不在一处,回档工具做不到原子复核归属,拒绝")
        }
    }
    if ($BarrierProtocol -ceq 'legacy' -and [string]$Facts.FencingMode -ne 'legacy') {
        $problems.Add("-BarrierProtocol legacy 与线上 go/db 的 Fencing.Mode '$($Facts.FencingMode)' 矛盾:dual 下 go/db 写提交屏障 ACK、saveoutbox 在跑,属于严格屏障世代,回档工具对它的处置待负责人裁决 #3")
    }
    return , $problems.ToArray()
}

<#
.SYNOPSIS
    部署事实的规范化文本(去掉不参与比较的字段),用于判断线上快照与同一 rid 的存档是否相同。纯函数。
#>
function ConvertTo-DeployFactsKey {
    param([Parameter(Mandatory = $true)]$Facts)

    return ($Facts | Select-Object -Property * -ExcludeProperty CapturedAtUtc | ConvertTo-Json -Compress -Depth 6)
}

<#
.SYNOPSIS
    第 0 步 f(只在 -Apply 下、Step 1 之前):读部署事实、与存档和本次参数核对、落盘 deploy-facts.json;任何问题都在停服之前抛出。返回事实。

.DESCRIPTION
    namespace 还在:按 Get-DeploySnapshotKubectlArgs 现读;报告目录里已有同一 rid 的存档时必须与现读逐项相同(不同说明换了部署或 rid 用错了)。
    namespace 已删(同一 rid 在 Step 1 之后重跑):kubectl 无输出,改用存档;没有存档就拒绝 —— 读不到不等于没有。
    kubectl 失败(不可达、Forbidden)一律拒绝。
#>
function Resolve-ZoneDeployFacts {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$FactsPath,
        # db 消费组的 L / Z(本机 db.yaml 的 GroupID 基名与 <基名>_z<ZoneId>),交给 ConvertFrom-ZoneDeploySnapshot 判线上形状。
        [Parameter(Mandatory = $true)][string]$LegacyGroup,
        [Parameter(Mandatory = $true)][string]$ZoneGroup,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = ''
    )

    $refusal = "k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:"
    $result = Invoke-ClientEntryKubectl -KubectlArgs (Get-DeploySnapshotKubectlArgs -Namespace $Namespace) `
        -KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
    if ($result.ExitCode -ne 0) {
        throw ($refusal + "读不到 namespace $Namespace 的部署事实(kubectl 退出码 $($result.ExitCode)):$($result.Stderr)。读不到不等于没有,无法核对 Redis 拓扑与屏障协议。")
    }
    try {
        $live = ConvertFrom-ZoneDeploySnapshot -Json ([string]$result.Stdout) -Namespace $Namespace -LegacyGroup $LegacyGroup -ZoneGroup $ZoneGroup
    }
    catch {
        throw ($refusal + $_.Exception.Message)
    }
    $saved = $null
    if (Test-Path -LiteralPath $FactsPath) {
        try {
            $saved = Get-Content -LiteralPath $FactsPath -Raw -Encoding utf8 | ConvertFrom-Json -ErrorAction Stop
        }
        catch {
            throw ($refusal + "报告目录里的 $FactsPath 不是合法的 JSON:$($_.Exception.Message)")
        }
    }
    if ($null -eq $live) {
        if ($null -eq $saved) {
            throw ($refusal + "namespace $Namespace 里一个部署对象都没有(可能已被删),报告目录里也没有同一 rollback-id 的 $FactsPath。" +
                "namespace 删掉之后无从核对 Redis 拓扑与屏障协议;Step 1 之后重跑必须带上一次打印的 -RollbackId。")
        }
        Write-Host "  namespace $Namespace is gone; reusing deploy facts saved by an earlier run with the same -RollbackId ($FactsPath)" -ForegroundColor Yellow
        return $saved
    }
    $facts = $live | Select-Object -Property *, @{ Name = 'CapturedAtUtc'; Expression = { [datetime]::UtcNow.ToString('o') } }
    if ($null -ne $saved) {
        if ((ConvertTo-DeployFactsKey -Facts $saved) -cne (ConvertTo-DeployFactsKey -Facts $facts)) {
            throw ($refusal + "namespace $Namespace 的部署事实与同一 rollback-id 存档 $FactsPath 不同:要么部署在两次运行之间变了,要么 -RollbackId 用错了。" +
                "`n  存档:$(ConvertTo-DeployFactsKey -Facts $saved)`n  现读:$(ConvertTo-DeployFactsKey -Facts $facts)")
        }
        return $saved
    }
    Set-Content -LiteralPath $FactsPath -Encoding utf8 -Value ($facts | ConvertTo-Json -Depth 6)
    return $facts
}

<#
.SYNOPSIS
    第 0 步 f 读 go/db 所用 Redis 实例 run_id 的 kubectl 参数(纯函数:只拼参数,不执行;回档复核 [4])。

.DESCRIPTION
    exec 进 infra namespace 的 redis-0(redis 容器),对 deploy-facts 里 go/db RedisClient.Hosts 的地址执行 redis-cli INFO server ——
    走的是 go/db 自己连的那个地址(Service 名),HA 形态下由代理落到当前主库,与 go/db 看到的是同一个实例。
    口令取 Pod 自己的 REDIS_PASSWORD(Secret redis-auth),经 REDISCLI_AUTH 交给 redis-cli,不经本机命令行、不进日志;为空时不设
    (写法同 k8s_deploy.ps1 Test-RedisLiveMaxClients)。地址要拼进 sh -c,只收 <[A-Za-z0-9.-]+>:<端口>(单实例、无 IPv6),不合格即 throw。
    返回值交给 Invoke-ClientEntryKubectl 执行(带同一组 -KubeContext / -KubeConfig)。
#>
function Get-GoDbRedisRunIdExecArgs {
    param(
        [Parameter(Mandatory = $true)][string]$InfraNamespace,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$RedisHosts
    )

    $address = [regex]::Match($RedisHosts.Trim(), '^(?<host>[A-Za-z0-9.-]+):(?<port>\d{1,5})$')
    if (-not $address.Success) {
        throw "go/db 的 RedisClient.Hosts '$RedisHosts' 不是单个 <主机名>:<端口>,不能拼进 redis-0 里的 sh -c 读 run_id。"
    }
    $shell = 'if [ -n "$REDIS_PASSWORD" ]; then REDISCLI_AUTH=$REDIS_PASSWORD; export REDISCLI_AUTH; fi; exec redis-cli --raw -h ' +
        $address.Groups['host'].Value + ' -p ' + $address.Groups['port'].Value + ' INFO server'
    return , @('exec', '-n', $InfraNamespace, 'pod/redis-0', '-c', 'redis', '--', 'sh', '-c', $shell)
}

<#
.SYNOPSIS
    从 redis-cli INFO server 的输出里取 run_id(40 位小写十六进制),不是恰好一行即 throw。纯函数。
#>
function ConvertFrom-RedisInfoRunId {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Text)

    $matched = @([regex]::Matches($Text, '(?m)^run_id:([0-9a-f]{40})\r?$'))
    if ($matched.Count -ne 1) {
        throw "INFO server 的输出里 run_id 行有 $($matched.Count) 行(期望恰好 1 行 run_id:<40 位十六进制>),判不出实例身份。"
    }
    return $matched[0].Groups[1].Value
}

<#
.SYNOPSIS
    第 0 步 f(只在 -Apply 下、Step 1 之前):按 Get-GoDbRedisRunIdExecArgs 读出 go/db 所用 Redis 实例的 run_id;exec 失败或判不出即 throw。
#>
function Read-GoDbRedisRunId {
    param(
        [Parameter(Mandatory = $true)][string]$InfraNamespace,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$RedisHosts,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = ''
    )

    $refusal = "k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:"
    try {
        $execArgs = Get-GoDbRedisRunIdExecArgs -InfraNamespace $InfraNamespace -RedisHosts $RedisHosts
        $result = Invoke-ClientEntryKubectl -KubectlArgs $execArgs -KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
        if ($result.ExitCode -ne 0) {
            throw "kubectl exec 以退出码 $($result.ExitCode) 结束:$($result.Stderr)"
        }
        return (ConvertFrom-RedisInfoRunId -Text ([string]$result.Stdout))
    }
    catch {
        throw ($refusal + "读不到 go/db 所用 Redis($RedisHosts)的 run_id(kubectl exec 进 $InfraNamespace 的 redis-0 执行 INFO server)," +
            "无法证明 -RedisHost 与线上 go/db 是同一个实例:另一个环境里同 ZoneId 的 Redis 同样有这批游标,工具会在那边丢弃、目标环境原封不动。" +
            "检查部署账号对 $InfraNamespace 的 pods/exec 权限、redis-0 是否就绪、-KubeContext / -KubeConfig 是否指向目标集群后重跑。`n原因:$($_.Exception.Message)")
    }
}

<#
.SYNOPSIS
    回档工具 -version 的输出 → [pscustomobject]@{ Commit; Dirty };不是契约格式(恰好一行 commit=<40 位小写 hex>、一行 dirty=<true|false>)即 throw。纯函数。
#>
function ConvertFrom-DiscardToolVersion {
    param([AllowEmptyCollection()][AllowNull()][string[]]$Lines)

    $allLines = @($Lines | ForEach-Object { "$_".Trim() })
    $commits = @($allLines | ForEach-Object { [regex]::Match($_, '^commit=([0-9a-f]{40})$') } | Where-Object { $_.Success })
    $dirties = @($allLines | ForEach-Object { [regex]::Match($_, '^dirty=(true|false)$') } | Where-Object { $_.Success })
    if ($commits.Count -ne 1 -or $dirties.Count -ne 1) {
        $preview = (@($allLines | Where-Object { $_ -ne '' } | Select-Object -First 5) -join ' | ')
        if ($preview -eq '') { $preview = '(无输出)' }
        throw "回档工具 -version 的输出不是约定格式(恰好一行 commit=<40 位小写十六进制>、一行 dirty=<true|false>):$preview"
    }
    return [pscustomobject]@{ Commit = $commits[0].Groups[1].Value; Dirty = ($dirties[0].Groups[1].Value -ceq 'true') }
}

<#
.SYNOPSIS
    回档工具的构建提交与线上 db 镜像是否同一提交(纯函数,回档复核 [5]);返回问题文本,$null 表示一致。

.DESCRIPTION
    镜像 tag 是构建时的 git 短 sha(release_common.ps1 Get-ReleaseImageTag 取 12 位),工具的 commit 是完整 40 位:tag 必须是 commit 的前缀。
    以下一律判为无法核对(fail-closed):db 镜像以 digest 部署(引用里没有提交号)、tag 不是 7–40 位十六进制(含 -dirty 后缀的脏树构建)、
    工具自己是脏树构建(提交号代表不了它的代码)。
#>
function Test-DiscardToolMatchesDbImage {
    param(
        [Parameter(Mandatory = $true)]$Version,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$DbImage
    )

    if ($DbImage.Contains('@')) {
        return "线上 db 镜像 '$DbImage' 以 digest 部署,镜像引用里没有提交号,无法核对回档工具与线上 go/db 是否同一提交"
    }
    $tag = (Get-ImageTagFromRef -ImageRef $DbImage).ToLowerInvariant()
    if ($tag -cnotmatch '^[0-9a-f]{7,40}$') {
        return "线上 db 镜像 '$DbImage' 的 tag 不是 git sha(7–40 位十六进制;-dirty 后缀的脏树构建同样不收),无法核对回档工具与线上 go/db 是否同一提交"
    }
    if ($Version.Dirty) {
        return "回档工具是脏树构建(-version 报 dirty=true),提交号 $($Version.Commit) 代表不了它的代码"
    }
    if (-not ([string]$Version.Commit).StartsWith($tag, [System.StringComparison]::Ordinal)) {
        return "回档工具的构建提交 $($Version.Commit) 与线上 db 镜像 '$DbImage' 的 tag $tag 不是同一提交:键名与 Lua 契约只对同一提交的 go/db 成立"
    }
    return $null
}

<#
.SYNOPSIS
    执行一次 <tool> -version,返回 ConvertFrom-DiscardToolVersion 的结果;起不来、退出码非 0、输出不合约定都 throw。
#>
function Get-DiscardToolVersion {
    param([Parameter(Mandatory = $true)][string]$ToolPath)

    $prevErrorAction = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $global:LASTEXITCODE = 0
        $lines = @(& $ToolPath -version 2>&1 | ForEach-Object { "$_" })
        $exitCode = [int]$LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $prevErrorAction
    }
    if ($exitCode -ne 0) {
        throw "回档工具 -version 以退出码 $exitCode 结束:$((@($lines | Select-Object -Last 5)) -join ' | ')"
    }
    return (ConvertFrom-DiscardToolVersion -Lines $lines)
}

<#
.SYNOPSIS
    确认 zone namespace 已不存在;仍在(Terminating / 被重建)或读不到都 throw。Step 1 之后与 Step 5 之前各调一次。

.DESCRIPTION
    dev_tools.ps1 k8s-zone-down 的 Remove-Zone 以 -AllowFailure 执行 kubectl delete namespace:删除失败也不报错、退出码照样为 0,
    不能拿它判断「本 zone 的写者都停了」(回档丢弃语义对抗复核 [0])。Step 5 之前再调一次(回档复核 [8]):中间隔着人工 PITR,
    namespace 可能被 GitOps / CD 重建。-Refusal / -Hint 是这一阶段的拒绝前缀与后续做法,缺省为 Step 1 之后那一次的文本。
#>
function Assert-ZoneNamespaceGone {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = '',
        [string]$Refusal = 'k8s-zone-rollback 在 Step 1 之后拒绝继续:k8s-zone-down 已执行,',
        [string]$Hint = '尚未 PITR,Redis / Kafka 尚未改动;等 namespace 删净(kubectl get namespace 返回 NotFound)后用同一 -RollbackId 重跑。'
    )

    $result = Invoke-ClientEntryKubectl -KubectlArgs @('get', 'namespace', $Namespace, '-o', 'name', '--ignore-not-found', '--request-timeout=15s') `
        -KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
    if ($result.ExitCode -ne 0) {
        throw ($Refusal + "读不到 namespace $Namespace 是否已删除(kubectl 退出码 $($result.ExitCode)):$($result.Stderr)。" + $Hint)
    }
    if (-not [string]::IsNullOrWhiteSpace([string]$result.Stdout)) {
        throw ($Refusal + "namespace $Namespace 仍在($(([string]$result.Stdout).Trim()),多半还在 Terminating、删除失败被 Remove-Zone 的 -AllowFailure 吞掉," +
            "或被 GitOps / CD 重建)。本 zone 的写者可能还活着。" + $Hint)
    }
    Write-Host "  namespace $Namespace no longer exists" -ForegroundColor Green
}

<#
.SYNOPSIS
    本机 kafka-get-offsets.sh --time -1 读本 topic 各分区的日志末端,返回 @{ [int]分区 = [long]LEO };失败、"Error:" 行、解析不了都 throw。
#>
function Read-KafkaTopicLogEnd {
    param(
        [Parameter(Mandatory = $true)][string]$Bootstrap,
        [Parameter(Mandatory = $true)][string]$Topic
    )

    $prevErrorAction = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $global:LASTEXITCODE = 0
        $lines = @(& kafka-get-offsets.sh --bootstrap-server $Bootstrap --topic $Topic --time -1 2>&1 | ForEach-Object { "$_" })
        $exitCode = $LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $prevErrorAction
    }
    $errorLines = @($lines | Where-Object { $_ -match '^\s*Error:' })
    if ($exitCode -ne 0 -or $errorLines.Count -gt 0) {
        $detail = if ($errorLines.Count -gt 0) { $errorLines -join ' | ' } else { (@($lines | Select-Object -Last 5) -join ' | ') }
        throw "kafka-get-offsets.sh --topic $Topic --time -1 失败(退出码 $exitCode):$detail"
    }
    return (ConvertFrom-KafkaGetOffsets -Lines $lines -Topic $Topic)
}

<#
.SYNOPSIS
    Step 1b (ii):停服之后确认没有消费者还挂在本 topic 上,返回问题列表;describe 判不出即 throw。

.DESCRIPTION
    zone 组:--state 必须是 Empty / Dead 或组不存在 —— zone-down 之后还有成员,说明别处还有 db 实例在消费本 topic,
    Step 5 的重置会被 Kafka 拒绝,即使拨成功了它也会继续推进位点、把切点之后的消息落库。
    legacy 组:别的 zone 在线时它本来就是 Stable,不看状态,只看本 topic 的行里 CONSUMER-ID 全是 '-'(没有成员被分配到本 topic 的分区)。
    解析规则只在 lib 一份(ConvertFrom-KafkaGroupStateDescribe / Get-KafkaTopicActiveConsumers),原文格式按推定 fail-closed。
#>
function Get-KafkaTopicConsumerProblems {
    param(
        [Parameter(Mandatory = $true)][string]$Bootstrap,
        [Parameter(Mandatory = $true)][string]$Topic,
        [Parameter(Mandatory = $true)][string]$ZoneGroup,
        [Parameter(Mandatory = $true)][string]$LegacyGroup
    )

    $problems = [System.Collections.Generic.List[string]]::new()
    $zoneDescribe = Invoke-KafkaGroupDescribe -Bootstrap $Bootstrap -Group $ZoneGroup -State
    $zoneState = ConvertFrom-KafkaGroupStateDescribe -Lines $zoneDescribe.Lines -ExitCode $zoneDescribe.ExitCode -Group $ZoneGroup
    Write-Host "  zone group ${ZoneGroup}: state=$($zoneState.State) members=$(if ($null -ne $zoneState.Members) { $zoneState.Members } else { '-' })"
    if (-not (Test-KafkaGroupStateInactive -State $zoneState.State)) {
        $problems.Add("zone 组 $ZoneGroup 的状态是 $($zoneState.State)(成员 $($zoneState.Members) 个),不是 Empty / Dead:别处还有 db 实例在这个组里消费本 topic")
    }
    $legacyDescribe = Invoke-KafkaGroupDescribe -Bootstrap $Bootstrap -Group $LegacyGroup
    $legacyState = Resolve-KafkaGroupTopicOffsets -Lines $legacyDescribe.Lines -ExitCode $legacyDescribe.ExitCode -Topic $Topic
    # 这些 lib 函数以 `, $数组` 返回:直接赋值拿到数组本身;外面再套 @(...) 会多包一层,空表也成了 Count 1。
    $active = Get-KafkaTopicActiveConsumers -Resolved $legacyState
    Write-Host "  legacy group ${LegacyGroup}: $($legacyState.State); members assigned to ${Topic}: $($active.Count)"
    if ($active.Count -gt 0) {
        $problems.Add("legacy 组 $LegacyGroup 在 $Topic 上仍有成员被分配到分区($($active -join ';')):还有旧组 db 实例在消费本 topic")
    }
    return , $problems.ToArray()
}

<#
.SYNOPSIS
    读 zone 组在本 topic 上的位点并按 lib Get-KafkaGroupCutProblems 核对,返回 @{ Cut = @{ 分区 = 位点 }; Problems };describe 判不出即 throw。
.PARAMETER ExpectedCut
    不给 = Step 5 刚拨完、记切点(只要求每个分区 CURRENT == LOG-END);给了 = Step 5b,两者还必须都等于切点。
#>
function Read-ZoneGroupCut {
    param(
        [Parameter(Mandatory = $true)][string]$Bootstrap,
        [Parameter(Mandatory = $true)][string]$Topic,
        [Parameter(Mandatory = $true)][string]$ZoneGroup,
        [Parameter(Mandatory = $true)][int]$Partitions,
        [AllowNull()]$ExpectedCut = $null
    )

    $describe = Invoke-KafkaGroupDescribe -Bootstrap $Bootstrap -Group $ZoneGroup
    $state = Resolve-KafkaGroupTopicOffsets -Lines $describe.Lines -ExitCode $describe.ExitCode -Topic $Topic
    foreach ($row in (@($state.Rows) | Sort-Object Partition)) {
        Write-Host "    $ZoneGroup $Topic partition=$($row.Partition) current=$(if ($null -ne $row.CurrentOffset) { $row.CurrentOffset } else { '-' }) end=$(if ($null -ne $row.LogEndOffset) { $row.LogEndOffset } else { '-' })"
    }
    $problems = Get-KafkaGroupCutProblems -Rows @($state.Rows) -Partitions $Partitions -ExpectedCut $ExpectedCut
    return [pscustomobject]@{ Cut = $state.Committed; Problems = $problems }
}

<#
.SYNOPSIS
    执行一次本机 kafka-consumer-groups.sh --describe --group <g>(-State 时追加 --state),合并收集 stdout / stderr 为字符串行,
    返回 @{ Lines; ExitCode }。不判成败。
.DESCRIPTION
    局部放宽 ErrorActionPreference(成败只看退出码与输出);先清零 LASTEXITCODE,不把更早一次调用的退出码当成本次的。
    命令不存在等异常原样抛给调用方。
#>
function Invoke-KafkaGroupDescribe {
    param(
        [Parameter(Mandatory = $true)][string]$Bootstrap,
        [Parameter(Mandatory = $true)][string]$Group,
        [switch]$State
    )

    $describeArgs = @('--bootstrap-server', $Bootstrap, '--describe', '--group', $Group)
    if ($State) { $describeArgs += '--state' }
    $prevErrorAction = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $global:LASTEXITCODE = 0
        $lines = @(& kafka-consumer-groups.sh @describeArgs 2>&1 | ForEach-Object { "$_" })
        $exitCode = $LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $prevErrorAction
    }
    return [pscustomobject]@{ Lines = $lines; ExitCode = $exitCode }
}

<#
.SYNOPSIS
    第 0 步 c 与 Step 1b (iv):Kafka LAG 只读快照。对每个组 describe 一次,只打印本 topic 的分区 / current / end / lag 与 LAG 合计。

.DESCRIPTION
    只告警、绝不抛出:它取代的是原 Step 2 的排空等待 —— zone-down 之后 LAG 不可能排空,Step 5 又会把位点拨到日志末端,
    这些 LAG 正是要丢弃的积压,排空本来就没有意义;快照只供操作者看一眼两组在本 topic 上的进度(切组前后哪个组在用)与将丢弃的条数。
    组不存在打印 (group does not exist);describe 失败或输出解析不了只 Write-Warning。别的 topic 的行一律不计入。
#>
function Show-KafkaTopicLagSnapshot {
    param(
        [Parameter(Mandatory = $true)][string]$Bootstrap,
        [Parameter(Mandatory = $true)][string]$Topic,
        [Parameter(Mandatory = $true)][string[]]$Groups
    )

    foreach ($group in $Groups) {
        try {
            $describe = Invoke-KafkaGroupDescribe -Bootstrap $Bootstrap -Group $group
            $state = Resolve-KafkaGroupTopicOffsets -Lines $describe.Lines -ExitCode $describe.ExitCode -Topic $Topic
            if ($state.State -eq 'missing') {
                Write-Host "  group ${group}: (group does not exist)"
                continue
            }
            $rows = @($state.Rows)
            if ($rows.Count -eq 0) {
                Write-Host "  group ${group}: no partitions of $Topic in the describe output (no committed offsets, no assignment)"
                continue
            }
            $totalLag = [long]0
            $unknownLag = 0
            foreach ($row in ($rows | Sort-Object Partition)) {
                $current = if ($null -ne $row.CurrentOffset) { $row.CurrentOffset } else { '-' }
                $end = if ($null -ne $row.LogEndOffset) { $row.LogEndOffset } else { '-' }
                $lag = '-'
                if ($null -ne $row.Lag) {
                    $totalLag += [long]$row.Lag
                    $lag = $row.Lag
                }
                else {
                    $unknownLag++
                }
                Write-Host "    $group $Topic partition=$($row.Partition) current=$current end=$end lag=$lag"
            }
            $unknownText = if ($unknownLag -gt 0) { " ($unknownLag partition(s) without committed offsets, lag unknown)" } else { '' }
            Write-Host "  group ${group}: $Topic partitions=$($rows.Count) total lag=$totalLag$unknownText"
        }
        catch {
            Write-Warning "Kafka LAG snapshot unavailable: group ${group}: $($_.Exception.Message)"
        }
    }
}

<#
.SYNOPSIS
    Step 1b (iv):估算 Step 5 -ToLatest 将丢弃的积压条数(回档复核 [11]),只打印、只告警,绝不抛出。

.DESCRIPTION
    口径:zone 组在本 topic 上有位点就按 zone 组算,否则按 legacy 组算 —— 回档前该 zone 实际在用哪个组,停服时的积压就是那个组的
    LOG-END - 已提交位点。从未切过组的 zone 上 zone 组不存在,以前只打印 zone 组,得到「(group does not exist)」,看不出要丢多少。
    两组都有位点(交接没收尾)时按 zone 组算。没有已提交位点的分区 LAG 记为未知、单独计数,不当成 0;两组都没有位点时说明估不出。
#>
function Show-KafkaDiscardEstimate {
    param(
        [Parameter(Mandatory = $true)][string]$Bootstrap,
        [Parameter(Mandatory = $true)][string]$Topic,
        [Parameter(Mandatory = $true)][string]$ZoneGroup,
        [Parameter(Mandatory = $true)][string]$LegacyGroup
    )

    try {
        $states = @{}
        foreach ($group in @($ZoneGroup, $LegacyGroup)) {
            $describe = Invoke-KafkaGroupDescribe -Bootstrap $Bootstrap -Group $group
            $states[$group] = Resolve-KafkaGroupTopicOffsets -Lines $describe.Lines -ExitCode $describe.ExitCode -Topic $Topic
        }
        $basis = if ($states[$ZoneGroup].State -eq 'offsets') { $ZoneGroup } elseif ($states[$LegacyGroup].State -eq 'offsets') { $LegacyGroup } else { $null }
        if ($null -eq $basis) {
            Write-Host "  backlog that Step 5 discards: unknown (neither $ZoneGroup nor $LegacyGroup holds committed offsets on $Topic)" -ForegroundColor Yellow
            return
        }
        $total = [long]0
        $unknown = 0
        foreach ($row in @($states[$basis].Rows | Where-Object { $null -ne $_ })) {
            if ($null -ne $row.Lag) { $total += [long]$row.Lag } else { $unknown++ }
        }
        $unknownText = if ($unknown -gt 0) { "; $unknown partition(s) of $basis without committed offsets, their backlog is unknown" } else { '' }
        Write-Host "  backlog that Step 5 discards: $total message(s) on $Topic (basis: $basis, the group this zone consumed with before the rollback$unknownText)" -ForegroundColor Yellow
    }
    catch {
        Write-Warning "Backlog estimate unavailable on ${Topic}: $($_.Exception.Message)"
    }
}

<#
.SYNOPSIS
    第 0 步 d:Step 5 预演(只在 -Apply 下、Step 1 之前调用)。

.DESCRIPTION
    zone 组在本 topic 上已有数值位点 → 组已存在,Step 5 的重置走的是成熟路径,跳过预演。
    否则(组不存在 / 没有位点,即从未切过组的 zone)以 kafka_offset_reset.ps1 的 dry-run(hashtable splatting,
    不带 -Apply 与 -RetireGroup)对 zone 组按 -ToLatest 生成重置计划(2026-10-18 起与 Step 5 一致,不再按 TargetTime);
    它要求计划覆盖 0..Partitions-1,失败、throw、退出码非 0 都在停服之前抛出。Kafka 能否为不存在的组建出位点未在钉版镜像上实测
    (设计文档 §11 V3(c)),这一步把「Step 5 走不通」提前到破坏性步骤之前暴露;Step 5 自己 -Apply 之后还有逐分区复核。
    describe 本身失败同样抛出:判不出就无法证明 Step 5 走得通。
#>
function Invoke-Step5Rehearsal {
    param(
        [Parameter(Mandatory = $true)][string]$ResetScriptPath,
        [Parameter(Mandatory = $true)][string]$Bootstrap,
        [Parameter(Mandatory = $true)][string]$Topic,
        [Parameter(Mandatory = $true)][string]$ZoneGroup,
        [Parameter(Mandatory = $true)][int]$Partitions
    )

    $refusal = "k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:"
    try {
        $describe = Invoke-KafkaGroupDescribe -Bootstrap $Bootstrap -Group $ZoneGroup
        $state = Resolve-KafkaGroupTopicOffsets -Lines $describe.Lines -ExitCode $describe.ExitCode -Topic $Topic
    }
    catch {
        throw ($refusal + "Step 5 预演读不到 zone 组 $ZoneGroup 在 $Topic 上的位点(kafka-consumer-groups.sh --describe 失败或输出无法解析)," +
            "判不出 Step 5 能否走通。`n原因:$($_.Exception.Message)`n确认本机 kafka-consumer-groups.sh 可用、-KafkaBootstrap 可达后重跑。")
    }
    if ($state.State -eq 'offsets') {
        Write-Host "  zone group already holds offsets on ${Topic}; Step 5 rehearsal skipped" -ForegroundColor Green
        return
    }

    Write-Host "  zone group $ZoneGroup has no committed offsets on $Topic ($($state.State)); rehearsing Step 5 with kafka_offset_reset.ps1 (dry-run)"
    $rehearsalArgs = @{
        BootstrapServer = $Bootstrap
        Topic           = $Topic
        Group           = $ZoneGroup
        ToLatest        = $true
        Partitions      = $Partitions
    }
    $failure = $null
    # 被调脚本不以 exit 结束时 $LASTEXITCODE 保留更早的值,先清零,才能把它的非 0 判成本次失败。
    $global:LASTEXITCODE = 0
    try {
        & $ResetScriptPath @rehearsalArgs
        if ($LASTEXITCODE -ne 0) { $failure = "kafka_offset_reset.ps1 以退出码 $LASTEXITCODE 结束" }
    }
    catch {
        $failure = $_.Exception.Message
    }
    if ($null -ne $failure) {
        throw ($refusal + "Step 5 预演失败,Kafka 无法为 zone 组 $ZoneGroup 在 $Topic 上生成完整的重置计划(-ToLatest,分区数 $Partitions)。" +
            "拖到 Step 5 才失败时 zone 已停服、数据已回档。见 docs/design/db-consumer-group-per-zone.md §11 V3(c)。`n原因:$failure")
    }
    Write-Host "  Step 5 rehearsal passed: Kafka produced a complete reset plan for $ZoneGroup on $Topic" -ForegroundColor Green
}

<#
.SYNOPSIS
    第 0 步 e(只在 -Apply 下、Step 1 之前):按 Step 6 的同一条访问路径读 zone 组与 legacy 组在本 topic 上的位点;
    读不到,或与本机 CLI(-KafkaBootstrap)的结论不一致,都在停服之前抛出。

.DESCRIPTION
    Step 6 的 k8s_deploy.ps1(显式 -DbConsumerGroup zone)在写路径入口经 kubectl exec 进 infra namespace 的 kafka-0 读位点
    (Read-DbGroupTopicFacts),读不到就拒绝部署。以前第 0 步覆盖不到这条路径:b 的静态预检带 -DryRun、不读集群,c / d 用的是本机 CLI 加
    -KafkaBootstrap。于是部署账号缺 pods/exec、kafka-0 正在重启、容器名不对时,各项预检全过,直到 Step 6 才失败 —— 那时 zone 已停服、
    MySQL 已回档、Redis 已处置,照提示重跑仍要同一个 exec(R7 复核 [2])。
    这里用 lib 的同一份命令(Get-DbGroupDescribeExecArgs)与分类(Resolve-DbGroupDescribeExecResult)、同一个 kubectl 出口
    (lib/k8s_client_entry.ps1 Invoke-ClientEntryKubectl)、同一组 -KubeContext / -KubeConfig 与 Step 6 将用的 infra namespace,
    对两组各 exec 一次;exec 失败或输出判不出即拒绝。
    再与本机 CLI 逐组比对「本 topic 上有没有已提交位点」:两条路径是两个访问入口(-KafkaBootstrap 可能经端口转发或 hosts 指向别的集群),
    第 0 步 d 与 Step 5 改的是前者、Step 6 读的是后者,两边结论不同即拒绝。只比有无、不比位点值:停服之前 db 仍在消费,位点随时前进。
    本机 describe 失败同样拒绝(第 0 步 d 本来就要求本机 CLI 可用)。
#>
function Assert-Step6KafkaFactsReadable {
    param(
        [Parameter(Mandatory = $true)][string]$InfraNamespace,
        [Parameter(Mandatory = $true)][string]$Bootstrap,
        [Parameter(Mandatory = $true)][string]$Topic,
        [Parameter(Mandatory = $true)][string]$ZoneGroup,
        [Parameter(Mandatory = $true)][string]$LegacyGroup,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = ''
    )

    $refusal = "k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:"
    $mismatches = @()
    foreach ($group in @($ZoneGroup, $LegacyGroup)) {
        try {
            $execResult = Invoke-ClientEntryKubectl -KubectlArgs (Get-DbGroupDescribeExecArgs -InfraNamespace $InfraNamespace -Group $group) `
                -KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
            $execState = Resolve-DbGroupDescribeExecResult -Result $execResult -Topic $Topic
        }
        catch {
            throw ($refusal + "Step 6 的 k8s_deploy.ps1 要 kubectl exec 进 $InfraNamespace 的 kafka-0 读消费组 $group 在 $Topic 上的位点,现在就读不到;" +
                "拖到 Step 6 才失败时 zone 已停服、数据已回档。检查部署账号的 pods/exec 权限、kafka-0 是否就绪(多 broker 档 kafka-0 重启时同样读不到)、" +
                "-KubeContext / -KubeConfig 是否指向目标集群后重跑。`n原因:$($_.Exception.Message)")
        }
        try {
            $localDescribe = Invoke-KafkaGroupDescribe -Bootstrap $Bootstrap -Group $group
            $localState = Resolve-KafkaGroupTopicOffsets -Lines $localDescribe.Lines -ExitCode $localDescribe.ExitCode -Topic $Topic
        }
        catch {
            throw ($refusal + "本机 kafka-consumer-groups.sh(-KafkaBootstrap $Bootstrap)读不到消费组 $group 在 $Topic 上的位点,无法与 Step 6 的读法核对。" +
                "`n原因:$($_.Exception.Message)")
        }
        Write-Host "  group ${group}: kubectl exec kafka-0 -> $($execState.State); local CLI (-KafkaBootstrap $Bootstrap) -> $($localState.State)"
        if (($execState.State -ceq 'offsets') -ne ($localState.State -ceq 'offsets')) {
            $mismatches += "消费组 $group 在 $Topic 上:kafka-0 里读到 $($execState.State),本机 CLI 读到 $($localState.State)"
        }
    }
    if ($mismatches.Count -gt 0) {
        throw ($refusal + "两条访问路径看到的 Kafka 不一致,-KafkaBootstrap 与 -KubeContext 指向的可能不是同一个 Kafka 集群:`n  - " + ($mismatches -join "`n  - ") +
            "`n第 0 步 d 与 Step 5 改的是 -KafkaBootstrap 那一个,Step 6 读的是 -KubeContext 集群 $InfraNamespace 里的 kafka-0;核对两者后重跑。")
    }
    Write-Host "  Step 6 Kafka facts readable via kubectl exec, consistent with the local CLI" -ForegroundColor Green
}

# ── 入口校验(dry-run 也执行,任何改动之前) ──
# E2:回档不再 FLUSHDB;口令不再经命令行。传了就拒绝,不静默忽略(AGENTS §11.3)。
if ($ConfirmFlushSharedRedis) {
    throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:回档已不做 FLUSHDB,-ConfirmFlushSharedRedis 不再有效。" +
        "go/db 的游标、重试收据、归属映射、账号目录与各 zone 的玩家缓存同在一个 Redis DB,FLUSHDB 会清掉所有 zone 的这些键,游标没了之后重放全部重新落库。" +
        "回档改为由 zone_rollback_discard 只丢弃 TargetTime 之后的状态;整套环境重建走 AGENTS.md §6.2。去掉该参数重跑。")
}
if (-not [string]::IsNullOrEmpty($RedisPassword)) {
    throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:-RedisPassword 已废弃,口令经命令行会进进程列表与 shell 历史。" +
        "把口令放进一个环境变量,改传 -RedisPasswordEnv <变量名>(回档工具自己读该变量,本脚本不读、不打印)。")
}
# 待负责人裁决的路径一律 fail-closed(拒绝并说明),不在这里自行实现。
if ($BarrierProtocol -eq 'strict') {
    throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:-BarrierProtocol strict 待负责人裁决(#3)。严格提交屏障已是现行 scene / login 的唯一协议" +
        "(2026-10-17 落地),go/db 缺省 Fencing.Mode=dual(2026-10-18 存盘协议世代门禁);回档工具会成为 save_barrier 的新写入方,屏障按 PITR 之后的库重建的字段形状、" +
        "以及「有 pending、无 ACK、SQL 围栏 (0,0)」这类玩家怎么处置都没有定论。在裁决之前回档只支持 -BarrierProtocol legacy,而 legacy 只对应仍跑着" +
        "不写严格屏障的旧 scene / login、线上 Fencing.Mode=legacy 的 zone;按现行代码部署的 zone 在裁决之前不能执行 -Apply。")
}
if ($AcceptBlockedPlayers -gt 0) {
    throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:-AcceptBlockedPlayers 待负责人裁决(#4)。blocked 玩家(torn / owner_behind / post_t_candidate / " +
        "legacy_owned / unclassified,以及 T 之后建角、合入或迁入本 zone 的玩家)的父快照、子表缓存、屏障与资产证据是隔离保留还是删除,T 之后建的角色" +
        "能否以空骨架继续使用,都没有定论。在裁决之前,Step 4a 盘点出 blocked 玩家时回档停在 Step 5 之前(Kafka 与 Redis 都没动),按中止路径恢复。")
}
foreach ($envParam in @(@{ Name = 'RedisPasswordEnv'; Value = $RedisPasswordEnv }, @{ Name = 'MySqlDsnEnv'; Value = $MySqlDsnEnv })) {
    if (-not [string]::IsNullOrEmpty($envParam.Value) -and $envParam.Value -cnotmatch '^[A-Za-z_][A-Za-z0-9_]*$') {
        # 不回显给的值:误把口令或 DSN 本身传进来时,它不能再进错误文本与日志。
        throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:-$($envParam.Name) 不是合法的环境变量名(只收 [A-Za-z_][A-Za-z0-9_]*,给的值不回显)。这里要的是变量名,不是口令或 DSN 本身。"
    }
}
# E1:TargetTime 的格式与时间。改成 -ToLatest 之后 Kafka 不再顺带校验它,而 Step 3 的 PITR 命令与回档报告都以它为准。
$targetUtc = ConvertFrom-RollbackUtcTime -Text $TargetTime -Name 'TargetTime'
$nowUtc = [datetime]::UtcNow
if ($targetUtc -ge $nowUtc) {
    throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:-TargetTime $TargetTime 不早于当前 UTC $($nowUtc.ToString("yyyy-MM-dd'T'HH:mm:ss'Z'", [System.Globalization.CultureInfo]::InvariantCulture)),回不到一个还没发生的时刻(时区写错了?只收 UTC)。"
}
if (-not [string]::IsNullOrWhiteSpace($PitrRestoredTo)) {
    $pitrUtc = ConvertFrom-RollbackUtcTime -Text $PitrRestoredTo -Name 'PitrRestoredTo'
    if ($pitrUtc -ne $targetUtc) {
        throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:-PitrRestoredTo $PitrRestoredTo 与 -TargetTime $TargetTime 不是同一时刻。回档的语义是回到 TargetTime;PITR 恢复到别的时刻时,先按实际恢复时刻改 -TargetTime 再重跑。"
    }
}

# 回滚目标必须是不可变 tag,否则"回到旧版本"根本没发生。
$rollbackTag = Get-ImageTagFromRef -ImageRef $NodeImage
$rollbackChk = Test-ImmutableImageTag -Tag $rollbackTag
if (-not $rollbackChk.Ok) {
    throw "灾难回滚拒绝该 -NodeImage '$NodeImage':$($rollbackChk.Reason)。请传回滚目标版本的 git sha tag 或 repo@sha256:… digest。"
}

# 与 k8s_deploy.ps1 preflight(lib Test-ClientEntryPreflight,D91)同一规则,提前到入口,dry-run 下也能报出来。
# 其余参数组合的校验只在 k8s_deploy.ps1 一处,-Apply 由第 0 步静态预检(Invoke-ZoneUpStaticPreflight)复用,这里不复制。
if (-not [string]::IsNullOrWhiteSpace($GatewayIngressHost) -and [string]::IsNullOrWhiteSpace($GatewayTrustedProxies)) {
    throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:给了 -GatewayIngressHost 却没有 -GatewayTrustedProxies,Step 6 会被 k8s_deploy.ps1 的 preflight 拒绝(gateway 会把 Ingress controller 的地址当成所有玩家的来源 IP,D91)。请照传上一次部署的 -GatewayTrustedProxies。"
}
# 两者是 Ingress 这一项的两种相反的显式决定,同时给说明操作者自己没想清楚,不替他挑一个。
if ($ConfirmNoGatewayIngress -and -not [string]::IsNullOrWhiteSpace($GatewayIngressHost)) {
    throw "k8s-zone-rollback 拒绝执行:-ConfirmNoGatewayIngress 与 -GatewayIngressHost 互斥,尚未产生任何改动(前者 = 回滚后不要 Ingress,后者 = 回滚后按 $GatewayIngressHost 重建 Ingress),请按该 zone 的部署记录二选一。"
}

# go/db 存盘消费组的基名与分区数从 go/db/etc/db.yaml 现读(K8s 的 db ConfigMap 由 k8s_deploy.ps1 从同一文件渲染),
# 本脚本不抄常量;读不到就拒绝(fail-closed):组名猜错,Step 5 会重置一个没人用的组、再删掉真正在用的组的位点。
# 分区数给 Step 5 与第 0 步 d 做逐分区的计划覆盖与 -Apply 后复核。
$dbYamlPath = Join-Path "$RepoRoot" "go" "db" "etc" "db.yaml"
$dbGroupBase = Get-YamlScalar -Path $dbYamlPath -KeyPath 'ServerConfig.Kafka.GroupID'
if (-not $dbGroupBase.Found -or [string]::IsNullOrWhiteSpace([string]$dbGroupBase.Value)) {
    throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:读不到 go/db/etc/db.yaml 的 ServerConfig.Kafka.GroupID($($dbGroupBase.Reason)),无法推导 db 消费组名。"
}
$dbPartitionCnt = Get-YamlScalar -Path $dbYamlPath -KeyPath 'ServerConfig.Kafka.PartitionCnt'
$kafkaPartitionCount = 0
if (-not $dbPartitionCnt.Found -or -not [int]::TryParse([string]$dbPartitionCnt.Value, [ref]$kafkaPartitionCount) -or $kafkaPartitionCount -le 0) {
    throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:读不到 go/db/etc/db.yaml 的 ServerConfig.Kafka.PartitionCnt,或不是正整数('$($dbPartitionCnt.Value)' $($dbPartitionCnt.Reason)),Step 5 无法逐分区复核位点。"
}
# db_task topic 的世代号:Step 6 的 k8s_deploy.ps1 按同一个键推导本 zone 的 topic(Initialize-DbConsumerGroupPlans),
# 第 0 步 c / d / e 与 Step 5 必须作用在同一个 topic 上。规则与 k8s_deploy.ps1 相同:非负整数,读不到即拒(fail-closed)。
$dbTopicGeneration = Get-YamlScalar -Path $dbYamlPath -KeyPath 'ServerConfig.Kafka.TopicGeneration'
$kafkaTopicGeneration = 0
if (-not $dbTopicGeneration.Found -or -not [int]::TryParse([string]$dbTopicGeneration.Value, [ref]$kafkaTopicGeneration) -or $kafkaTopicGeneration -lt 0) {
    throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:读不到 go/db/etc/db.yaml 的 ServerConfig.Kafka.TopicGeneration,或不是非负整数('$($dbTopicGeneration.Value)' $($dbTopicGeneration.Reason)),拼不出本 zone 的 db_task topic 名。"
}
$derivedKafkaTopic = Get-DbTaskTopicName -ZoneId $ZoneId -Generation $kafkaTopicGeneration
if ([string]::IsNullOrWhiteSpace($KafkaTopic)) {
    $KafkaTopic = $derivedKafkaTopic
}
elseif ($KafkaTopic -cne $derivedKafkaTopic) {
    throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:-KafkaTopic $KafkaTopic 与按 go/db/etc/db.yaml(TopicGeneration=$kafkaTopicGeneration)推导的 $derivedKafkaTopic 不一致。" +
        "Step 6 的 k8s_deploy.ps1 只按 db.yaml 的世代号读 topic:第 0 步与 Step 5 改的若是另一个 topic,真实 topic 上的位点不会动。" +
        "-KafkaTopic 留空即按推导值;要回档的确实是另一代 topic,先把工作树的 go/db/etc/db.yaml 对齐到该 zone 实际部署的世代号。")
}
if ([string]::IsNullOrWhiteSpace($KafkaGroup)) { $KafkaGroup = "$($dbGroupBase.Value)_z$ZoneId" }
if ([string]::IsNullOrWhiteSpace($KafkaLegacyGroup)) { $KafkaLegacyGroup = [string]$dbGroupBase.Value }
if ($KafkaGroup -ceq $KafkaLegacyGroup) {
    throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:-KafkaGroup 与 -KafkaLegacyGroup 都是 $KafkaGroup。" +
        "Step 5 会重置这个组、再删除它自己在 $KafkaTopic 上的位点,go/db 将从 Oldest 重放整个保留期。" +
        "回档后的权威组是 zone 组:-KafkaGroup 留空即按 go/db/etc/db.yaml 推导为 $($dbGroupBase.Value)_z$ZoneId。")
}

# E3:-Apply 时破坏性步骤要用到的目标必须显式给出。不按 db.yaml 缺省的 127.0.0.1 补:本机缺省值在 K8s 上指向的是运维机自己,
# 回档会打到错误的目标;口令与 DSN 只给环境变量名。dry-run 不要求,缺的项在命令里打印占位符。
if ($Apply) {
    $missing = @()
    if ([string]::IsNullOrWhiteSpace($RedisHost)) {
        $missing += '-RedisHost <go/db 的共享 Redis:K8s 即 infra redis 的 DB 0,可以是端口转发地址>'
    }
    if ([string]::IsNullOrWhiteSpace($MySqlDsnEnv)) {
        $missing += "-MySqlDsnEnv <存放 zone_${ZoneId}_db DSN 的环境变量名>"
    }
    elseif ([string]::IsNullOrEmpty([System.Environment]::GetEnvironmentVariable($MySqlDsnEnv))) {
        $missing += "环境变量 $MySqlDsnEnv 为空:-MySqlDsnEnv 指名的变量必须有值(zone_${ZoneId}_db 的 DSN)"
    }
    if (-not [string]::IsNullOrEmpty($RedisPasswordEnv) -and [string]::IsNullOrEmpty([System.Environment]::GetEnvironmentVariable($RedisPasswordEnv))) {
        $missing += "环境变量 $RedisPasswordEnv 为空:-RedisPasswordEnv 指名的变量必须有值;Redis 没开认证时不要传 -RedisPasswordEnv"
    }
    if ([string]::IsNullOrWhiteSpace($RollbackDiscardTool)) {
        $missing += '-RollbackDiscardTool <与线上 go/db 同一提交构建的 zone_rollback_discard 可执行文件>(工具尚未落地时 -Apply 到此为止)'
    }
    elseif (-not (Test-Path -LiteralPath $RollbackDiscardTool -PathType Leaf)) {
        $missing += "-RollbackDiscardTool $RollbackDiscardTool 不是存在的文件"
    }
    if ([string]::IsNullOrWhiteSpace($BarrierProtocol)) {
        $missing += '-BarrierProtocol legacy(线上 go/db 的 Fencing.Mode 为 legacy;strict 待负责人裁决 #3)'
    }
    if ($SkipMySqlPause -and [string]::IsNullOrWhiteSpace($PitrRestoredTo)) {
        $missing += '-PitrRestoredTo <PITR 实际恢复到的时刻,须等于 -TargetTime>:带 -SkipMySqlPause 跳过暂停时必须声明 PITR 已做到哪个时刻'
    }
    if ($missing.Count -gt 0) {
        throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:-Apply 缺少以下 $($missing.Count) 项:`n  - " + ($missing -join "`n  - ") +
            "`n经包装入口调用而它不透传所需参数时,直接调用 tools/scripts/k8s_zone_rollback.ps1。")
    }
}
$BarrierProtocol = $BarrierProtocol.ToLowerInvariant()

# E4:rollback-id 与报告目录。rid 进 Redis 隔离区键名与路径,只收 [A-Za-z0-9_-];整脚本或单步重跑必须带同一个 rid。
if ([string]::IsNullOrWhiteSpace($RollbackId)) {
    $rollbackId = New-RollbackId -ZoneId $ZoneId -TargetUtc $targetUtc -NowUtc $nowUtc
}
elseif ($RollbackId -cnotmatch '^[A-Za-z0-9_-]{1,96}$') {
    throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:-RollbackId '$RollbackId' 不合法:它会拼进 Redis 隔离区键名 rollback:quarantine:<zone>:<rid>:* 与报告目录,只收 1–96 个 [A-Za-z0-9_-]。"
}
else {
    $rollbackId = $RollbackId
}
$reportDir = Join-Path "$RepoRoot" 'run' 'logs' 'zone-rollback' $rollbackId
$factsPath = Join-Path $reportDir 'deploy-facts.json'
# 第 0 步 a 的入口决定存档(回档复核 [10])与 Step 1b 的停服行指纹基线(工具 stopped 写,inventory / purge / verify 读,回档复核 [1])。
$entryStatePath = Join-Path $reportDir 'entry-state.json'
$baselinePath = Join-Path $reportDir 'fingerprint-baseline.json'
$purgeReportPath = Join-Path $reportDir 'purge.json'
$zoneNamespace = "${NamespacePrefix}-${ZoneName}"

$verb = if ($Apply) { "APPLY" } else { "DRY-RUN" }
Write-Host "" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host " k8s-zone-rollback ($verb)" -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host "  zone        : $ZoneName (id=$ZoneId)"
Write-Host "  target time : $TargetTime (UTC; rollback discards every write after it — owner decision 2026-10-17)"
Write-Host "  rollback id : $rollbackId (report directory $reportDir; pass -RollbackId $rollbackId to re-run any step)"
Write-Host "  redis       : $(if ($RedisHost) { "$RedisHost`:$RedisPort/db$RedisDB (go/db shared Redis)" } else { '(not set; -Apply requires -RedisHost: the go/db shared Redis)' })$(if ($RedisPasswordEnv) { " password from env $RedisPasswordEnv" })"
Write-Host "  mysql       : zone_${ZoneId}_db, DSN from env $(if ($MySqlDsnEnv) { $MySqlDsnEnv } else { '<not set; -Apply requires -MySqlDsnEnv>' })"
Write-Host "  discard tool: $(if ($RollbackDiscardTool) { $RollbackDiscardTool } else { '(not set; -Apply requires -RollbackDiscardTool: a prebuilt zone_rollback_discard from the same commit as the live go/db)' }) barrier-protocol=$(if ($BarrierProtocol) { $BarrierProtocol } else { '<not set>' })"
Write-Host "  kafka       : $KafkaBootstrap topic=$KafkaTopic group=$KafkaGroup retire=$KafkaLegacyGroup partitions=$kafkaPartitionCount reset=-ToLatest quiesce=${QuiesceSeconds}s"
Write-Host "  pitr        : $(if ($PitrRestoredTo) { "-PitrRestoredTo $PitrRestoredTo (operator declaration)" } elseif ($SkipMySqlPause) { '(-SkipMySqlPause without -PitrRestoredTo; -Apply refuses)' } else { '(Step 3 pauses until the operator confirms PITR)' })"
# 回退态不粘滞,留空时在摘要里就提醒:操作者在 dry-run 阶段就能看出 Step 6 会以哪种路由模式拉起 gate。
Write-Host "  gate router : $(if ($GateRouterMode) { "-GateRouterMode $GateRouterMode" } else { '(unset; using the k8s_deploy.ps1 default. A zone running in fallback mode "0" must pass -GateRouterMode 0; -Apply checks before shutdown and rejects mismatches or unavailable state)' })"
# B0-4 豁免开关同样不粘滞,留空时在摘要里就提醒;-Apply 由第 0 步 a 核对(见参数注释)。
Write-Host "  nofile mode : $(if ($NodeOpenFileLimitMode) { "-NodeOpenFileLimitMode $NodeOpenFileLimitMode" } else { '(unset; Step 6 uses the k8s_deploy.ps1 default and writes no NODE_OPEN_FILE_LIMIT_MODE, so gate / scene enforce the nofile check. A zone running with warn must pass -NodeOpenFileLimitMode warn; -Apply checks gate / scene before shutdown and rejects warn or unavailable state)' })"
# 集群外入口参数同样不粘滞,同样在摘要里提醒;地址参数的实际透传值见 Step 6 那一行。
Write-Host "  client entry: $(if ($ClientEntryMode) { "-ClientEntryMode $ClientEntryMode" } else { '(unset; using the k8s_deploy.ps1 default. Zones running in external mode must pass -ClientEntryMode external and address arguments; -Apply checks first)' })"
Write-Host "  ingress     : $(if ($GatewayIngressHost) { "-GatewayIngressHost $GatewayIngressHost" } elseif ($ConfirmNoGatewayIngress) { '-ConfirmNoGatewayIngress (Step 6 does not create Ingress; existing Ingress gateway in the namespace is removed in Step 1 without checking)' } else { '(unset; Step 6 does not create Ingress; existing Ingress gateway in the namespace is removed in Step 1; -Apply checks first)' })"
Write-Host "  release     : $(if ($ReleaseProfile) { "-ReleaseProfile $ReleaseProfile" } else { '(unset; Step 6 uses the dev_tools.ps1 default dev profile; staging / prod zones must explicitly pass -ReleaseProfile)' })"
Write-Host "  login auth  : $(if ($LoginDevPasswordAuth) { '-LoginDevPasswordAuth (shared password comes from environment variable MMORPG_LOGIN_DEV_PASSWORD_SHARED_SECRET)' } else { '(no -LoginDevPasswordAuth; pass it when a dev zone requires development-password login, otherwise login generated in Step 6 has no authentication configuration)' })"
$loginThirdPartyBanner = @()
if ($LoginWeChatAppId) { $loginThirdPartyBanner += "WeChat -LoginWeChatAppId $LoginWeChatAppId$(if ($LoginWeChatEndpoint) { ' -LoginWeChatEndpoint <set>' })$(if ($LoginWeChatAllowOpenId) { ' -LoginWeChatAllowOpenId' }) (AppSecret from environment variable MMORPG_LOGIN_WECHAT_APP_SECRET)" }
if ($LoginQQAppId) { $loginThirdPartyBanner += "QQ -LoginQQAppId $LoginQQAppId$(if ($LoginQQEndpoint) { ' -LoginQQEndpoint <set>' })$(if ($LoginQQAllowOpenId) { ' -LoginQQAllowOpenId' })" }
$loginThirdPartyConfirm = if ($ConfirmLoginAuthProviderChange) { '; -ConfirmLoginAuthProviderChange (live login ConfigMap / Secret is not checked)' } else { '; -Apply checks the live login ConfigMap / Secret login-oauth before shutdown' }
Write-Host "  login 3rd   : $(if ($loginThirdPartyBanner.Count -gt 0) { $loginThirdPartyBanner -join '; ' } else { '(no -LoginWeChatAppId / -LoginQQAppId; Step 6 renders login without WeChat / QQ login)' })$loginThirdPartyConfirm"
Write-Host "  switch gate : $(if ($AllowDisruptiveSwitch) { '-AllowDisruptiveSwitch (forwarded only to Step 6; permits mode / address changes if the namespace was not fully removed; does not bypass Step 0 checks)' } else { '(unset; Step 6 normally starts with an empty namespace; k8s_deploy.ps1 checks existing cluster state if cleanup was incomplete)' })"
Write-Host "  redis split : $(if ($RedisSplit) { "-RedisSplit $RedisSplit (forwarded to Step 6)" } else { '(unset; Step 6 renders -RedisSplit auto, which cannot read the deleted namespace and falls back to none: a zone running with queue must pass -RedisSplit queue)' })"
Write-Host "  kube target : $(if ($KubeContext -or $KubeConfig) { "context='$KubeContext' kubeconfig='$KubeConfig'" } else { '(unset; using the current kubectl context; preflight, Step 1 and Step 6 target the same cluster)' })"
Write-Host ""

if (-not $Apply) {
    Write-Host "DRY-RUN mode. The actions below would be executed in order." -ForegroundColor Yellow
    Write-Host "Re-run with -Apply to actually perform the rollback." -ForegroundColor Yellow
    Write-Host ""
}

# ── Step 6 的参数:先于第 0 步组装,静态预检与 Step 6 用的是同一份 ──
# 必须 hashtable splatting,数组 splatting 会按位置绑定,"-Command" 这个字符串会直接撞上 dev_tools.ps1 的 ValidateSet。
$upArgs = @{
    Command         = "k8s-zone-up"
    ZoneName        = $ZoneName
    ZoneId          = $ZoneId
    NodeImage       = $NodeImage
    NamespacePrefix = $NamespacePrefix
    WaitReady       = $true
    # 回档后永远落到 zone 组(docs/design/db-consumer-group-per-zone.md §6):Step 5 收尾后 zone 组有完整位点、
    # legacy 组在本 topic 上已没有位点,k8s_deploy.ps1 的决策表真跑时落到最终形状 —— db ConfigMap 写
    # `GroupID: "<zone 组>"`、不带 GroupHandoffFrom,交接闸之前的旧镜像也能起。第 0 步静态预检(-DryRun 不读集群)
    # 渲染的是交接形状预览,只校验参数组合。
    DbConsumerGroup = 'zone'
}
# 留空不传,由下游默认值接管(k8s_deploy.ps1 / dev_tools.ps1 各自唯一的一处);只有显式给了才覆盖(同 dev_tools.ps1 / k8s_image.ps1 的透传)。
# 字符串非空才传,GateNodePortBase 非 -1 才传,switch 只在被指定时传(同 dev_tools.ps1 Invoke-K8sDeploy)。
# -KubeContext / -KubeConfig 与第 0 步集群现状预检、Step 1 同一组值。
if (-not [string]::IsNullOrWhiteSpace($GateRouterMode)) { $upArgs.GateRouterMode = $GateRouterMode }
if (-not [string]::IsNullOrWhiteSpace($NodeOpenFileLimitMode)) { $upArgs.NodeOpenFileLimitMode = $NodeOpenFileLimitMode }
if (-not [string]::IsNullOrWhiteSpace($ReleaseProfile)) { $upArgs.ReleaseProfile = $ReleaseProfile }
if (-not [string]::IsNullOrWhiteSpace($ClientEntryMode)) { $upArgs.ClientEntryMode = $ClientEntryMode }
if (-not [string]::IsNullOrWhiteSpace($ClientPublicHost)) { $upArgs.ClientPublicHost = $ClientPublicHost }
if (-not [string]::IsNullOrWhiteSpace($GateClientHostTemplate)) { $upArgs.GateClientHostTemplate = $GateClientHostTemplate }
if ($GateNodePortBase -ne -1) { $upArgs.GateNodePortBase = $GateNodePortBase }
if (-not [string]::IsNullOrWhiteSpace($GateExternalTrafficPolicy)) { $upArgs.GateExternalTrafficPolicy = $GateExternalTrafficPolicy }
if (-not [string]::IsNullOrWhiteSpace($GateServiceType)) { $upArgs.GateServiceType = $GateServiceType }
if (-not [string]::IsNullOrWhiteSpace($RequireClientEndpoint)) { $upArgs.RequireClientEndpoint = $RequireClientEndpoint }
if ($LoginDevPasswordAuth) { $upArgs.LoginDevPasswordAuth = $true }
if (-not [string]::IsNullOrWhiteSpace($LoginWeChatAppId)) { $upArgs.LoginWeChatAppId = $LoginWeChatAppId }
if (-not [string]::IsNullOrWhiteSpace($LoginWeChatEndpoint)) { $upArgs.LoginWeChatEndpoint = $LoginWeChatEndpoint }
if ($LoginWeChatAllowOpenId) { $upArgs.LoginWeChatAllowOpenId = $true }
if (-not [string]::IsNullOrWhiteSpace($LoginQQAppId)) { $upArgs.LoginQQAppId = $LoginQQAppId }
if (-not [string]::IsNullOrWhiteSpace($LoginQQEndpoint)) { $upArgs.LoginQQEndpoint = $LoginQQEndpoint }
if ($LoginQQAllowOpenId) { $upArgs.LoginQQAllowOpenId = $true }
if ($ConfirmLoginAuthProviderChange) { $upArgs.ConfirmLoginAuthProviderChange = $true }
if ($AllowDisruptiveSwitch) { $upArgs.AllowDisruptiveSwitch = $true }
# 按用途拆实例(G4a 第 1a 批):非空才转发;以 queue 运行的 zone 必须显式给(见参数注释:Step 6 读不到线上,auto 只能落成 none)。
if (-not [string]::IsNullOrWhiteSpace($RedisSplit)) { $upArgs.RedisSplit = $RedisSplit }
if (-not [string]::IsNullOrWhiteSpace($GatewayIngressHost)) { $upArgs.GatewayIngressHost = $GatewayIngressHost }
if (-not [string]::IsNullOrWhiteSpace($GatewayIngressClassName)) { $upArgs.GatewayIngressClassName = $GatewayIngressClassName }
if (-not [string]::IsNullOrWhiteSpace($GatewayIngressTlsSecret)) { $upArgs.GatewayIngressTlsSecret = $GatewayIngressTlsSecret }
if (-not [string]::IsNullOrWhiteSpace($GatewayTrustedProxies)) { $upArgs.GatewayTrustedProxies = $GatewayTrustedProxies }
if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $upArgs.KubeContext = $KubeContext }
if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $upArgs.KubeConfig = $KubeConfig }

# ── 预检: gate 路由模式 / 客户端入口形态 / gateway Ingress / nofile 模式 + Step 6 静态预检(只读,先于 Step 1 停服) ──
# 放在停服之前:拖到 Step 6 才发现,zone 已停服、数据已回档,回滚卡在半路;而 Step 1 删掉 namespace 之后,
# k8s_deploy.ps1 自己的集群现状预检在 Step 6 已无从判断。集群现状只核对留空的项,gate / scene / Ingress 一次 kubectl 读完
# (Assert-ZoneEntryKeptOnRollback);nofile 模式要核对时另探测 Agones、装了再读 scene Fleet(Get-ClusterSceneFleets,CRD 不能并进那一次);
# login 第三方登录另读 login ConfigMap 与 Secret login-oauth 是否存在(两次只读 kubectl,见下面「防误关」一段);
# 静态预检让 k8s_deploy.ps1 对 Step 6 的同一份参数 -DryRun 一遍(Invoke-ZoneUpStaticPreflight)。
# dry-run 两者都不执行(整个 dry-run 只打印命令、不调任何外部工具),只预告 -Apply 时会做什么。
Write-Host "-- Preflight: gate routing mode / client entry mode / gateway Ingress / nofile mode / login WeChat-QQ login / Step 6 static preflight (read-only, before Step 1 shutdown) --" -ForegroundColor Cyan
$checkRouterMode = [string]::IsNullOrWhiteSpace($GateRouterMode)
$checkEntryMode = [string]::IsNullOrWhiteSpace($ClientEntryMode)
$checkIngress = [string]::IsNullOrWhiteSpace($GatewayIngressHost) -and -not $ConfirmNoGatewayIngress
$checkOpenFileLimit = [string]::IsNullOrWhiteSpace($NodeOpenFileLimitMode)
if (-not $checkRouterMode) { Write-Host "  Explicit -GateRouterMode ${GateRouterMode}: Step 6 starts gate with this value without checking the current cluster value" }
if (-not $checkEntryMode) { Write-Host "  Explicit -ClientEntryMode ${ClientEntryMode}: Step 6 rebuilds gate with this mode without checking the current cluster mode; uses the supplied address arguments" }
if ($ConfirmNoGatewayIngress) { Write-Host "  Explicit -ConfirmNoGatewayIngress: Step 6 does not create Ingress or check for Ingress gateway in the cluster" }
elseif (-not $checkIngress) { Write-Host "  Explicit -GatewayIngressHost ${GatewayIngressHost}: Step 6 rebuilds Ingress gateway with gateway" }
if (-not $checkOpenFileLimit) { Write-Host "  Explicit -NodeOpenFileLimitMode ${NodeOpenFileLimitMode}: Step 6 writes NODE_OPEN_FILE_LIMIT_MODE=$NodeOpenFileLimitMode into gate / scene without checking the current cluster value" }
# 入口决定存档(回档复核 [10]):同一 rid 重跑时读回 —— namespace 已删就沿用存档的集群现状做核对,本次入口决定与存档不同即拒绝。
# 首次运行在第 0 步全部通过、Step 1 之前才写(见第 0 步 f 之后):第 0 步后面任一项拒绝时不留存档,修正参数后同一 rid 重跑不受它约束。
$savedEntryState = $null
$entryDecision = $null
$zoneEntryState = $null
if ($Apply -and (Test-Path -LiteralPath $entryStatePath)) {
    try {
        $savedEntryState = Get-Content -LiteralPath $entryStatePath -Raw -Encoding utf8 | ConvertFrom-Json -ErrorAction Stop
    }
    catch {
        throw "k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:报告目录里的 $entryStatePath 不是合法的 JSON:$($_.Exception.Message)"
    }
}
$savedEntryHint = if ($null -ne $savedEntryState) { "同一 rollback-id 的存档 $entryStatePath 记录的入口参数:$(Format-ZoneEntryDecisionArgs -Decision $savedEntryState.Decision)" } else { '' }
if ($savedEntryHint) { Write-Host "  $savedEntryHint" }
if ($checkRouterMode -or $checkEntryMode -or $checkIngress -or $checkOpenFileLimit) {
    $checkNames = @()
    if ($checkRouterMode) { $checkNames += 'GATE_CLIENT_RPC_ROUTER 须与 k8s_deploy.ps1 -GateRouterMode 默认值一致' }
    if ($checkEntryMode) { $checkNames += 'gate 形态须与 k8s_deploy.ps1 -ClientEntryMode 默认值一致' }
    if ($checkIngress) { $checkNames += '不能有 Ingress gateway' }
    if ($checkOpenFileLimit) { $checkNames += 'gate / scene 的 NODE_OPEN_FILE_LIMIT_MODE 须与 k8s_deploy.ps1 -NodeOpenFileLimitMode 默认值一致' }
    $preflightKubectlArgs = Get-ZoneEntryKubectlArgs -Namespace $zoneNamespace -KubeContext $KubeContext -KubeConfig $KubeConfig
    Write-Host "  kubectl $($preflightKubectlArgs -join ' ') → $($checkNames -join ';')"
    if ($checkOpenFileLimit) {
        # 只用于显示(参数与 Get-ClusterSceneFleets 经 lib Invoke-ClientEntryKubectl 实际执行的一致)。
        # 两个参数函数整体返回数组(return ,),先接收再拼接。
        $agonesBaseArgs = Get-ClientEntryKubectlBaseArgs -KubeContext $KubeContext -KubeConfig $KubeConfig
        $sceneFleetArgs = Get-SceneFleetKubectlArgs -Namespace $zoneNamespace
        $agonesContract = Get-ClientEntryContract
        $agonesProbeShown = Format-CommandLine -Program 'kubectl' -Arguments (@($agonesBaseArgs) + @('get', '--raw', "/apis/$($agonesContract.AgonesApiGroup)/$($agonesContract.AgonesApiVersion)"))
        $sceneFleetShown = Format-CommandLine -Program 'kubectl' -Arguments (@($agonesBaseArgs) + @($sceneFleetArgs))
        Write-Host "  $agonesProbeShown; only when Agones is installed: $sceneFleetShown → scene Fleet NODE_OPEN_FILE_LIMIT_MODE (same check)"
    }
    if ($Apply) {
        $deployScriptPath = Join-Path $ScriptDir "k8s_deploy.ps1"
        $sceneFleets = if ($checkOpenFileLimit) { Get-ClusterSceneFleets -Namespace $zoneNamespace -KubeContext $KubeContext -KubeConfig $KubeConfig } else { $null }
        $zoneEntryState = Get-ClusterZoneEntryState -KubectlArgs $preflightKubectlArgs -Namespace $zoneNamespace -SceneFleets $sceneFleets
        # namespace 里一个 gate、一个 Ingress 都没有(多半已被同一 rid 上一次的 Step 1 删掉):改用存档里首次运行读到的集群现状。
        # kubectl 读失败不算(NamespaceEmpty 为 $false),照旧判为读不到。
        if ($zoneEntryState.NamespaceEmpty -and $null -ne $savedEntryState -and $null -ne $savedEntryState.ClusterState) {
            Write-Host "  namespace $zoneNamespace has no gate (deleted by an earlier run); checking against the cluster state saved by the same -RollbackId ($entryStatePath)" -ForegroundColor Yellow
            $zoneEntryState = $savedEntryState.ClusterState
            # Schema 3 之前的存档没记 nofile 模式:补成判不出,核对时要求显式传 -NodeOpenFileLimitMode(fail-closed)。
            # 只改内存里这一份,同一 rid 已有存档时不重写文件。
            if ($null -eq $zoneEntryState.PSObject.Properties['OpenFileLimitMode']) {
                $zoneEntryState | Add-Member -NotePropertyName OpenFileLimitMode -NotePropertyValue $null
                $zoneEntryState | Add-Member -NotePropertyName OpenFileLimitDetail -NotePropertyValue ("同一 rid 的存档 $entryStatePath 早于 nofile 模式核对(Schema 3 之前)," +
                    "没记下回滚前 gate / scene 的 NODE_OPEN_FILE_LIMIT_MODE")
            }
        }
        # 显式给了 -ClientEntryMode 时不核对,但把集群现状打出来,供操作者核对照传的地址参数。
        if (-not $checkEntryMode) { Write-Host "  Current cluster state (for comparison only): $($zoneEntryState.EntryDetail)" }
        try {
            Assert-ZoneEntryKeptOnRollback -State $zoneEntryState `
                -DeployDefaults (Get-DeployParamDefaults -DeployScriptPath $deployScriptPath -Names @('GateRouterMode', 'ClientEntryMode', 'NodeOpenFileLimitMode')) `
                -Namespace $zoneNamespace -DeployScriptPath $deployScriptPath `
                -CheckRouterMode:$checkRouterMode -CheckEntryMode:$checkEntryMode -CheckIngress:$checkIngress -CheckOpenFileLimit:$checkOpenFileLimit
        }
        catch {
            if (-not $savedEntryHint) { throw }
            throw ($_.Exception.Message + "`n" + $savedEntryHint + "(namespace 已删、存档里又没有集群现状时,照传这组参数即可;nofile 模式不在其中,按上面的提示显式传 -NodeOpenFileLimitMode)。")
        }
    } else {
        Write-Host "  (dry-run does not read the cluster; -Apply rejects mismatches or unavailable state before shutdown; explicitly pass the corresponding arguments)" -ForegroundColor Yellow
    }
} else {
    Write-Host "  All three entry values and -NodeOpenFileLimitMode were supplied explicitly; skipping cluster reads for gate / scene / Ingress (the login WeChat / QQ check below still reads unless -ConfirmLoginAuthProviderChange)"
}
if ($Apply) {
    $entryExplicit = [ordered]@{
        GateRouterMode            = $GateRouterMode
        ClientEntryMode           = $ClientEntryMode
        GatewayIngressHost        = $GatewayIngressHost
        ConfirmNoGatewayIngress   = [bool]$ConfirmNoGatewayIngress
        GatewayIngressClassName   = $GatewayIngressClassName
        GatewayIngressTlsSecret   = $GatewayIngressTlsSecret
        GatewayTrustedProxies     = $GatewayTrustedProxies
        ClientPublicHost          = $ClientPublicHost
        GateClientHostTemplate    = $GateClientHostTemplate
        GateNodePortBase          = $(if ($GateNodePortBase -ne -1) { "$GateNodePortBase" } else { '' })
        GateExternalTrafficPolicy = $GateExternalTrafficPolicy
        GateServiceType           = $GateServiceType
        RequireClientEndpoint     = $RequireClientEndpoint
        LoginWeChatAppId          = $LoginWeChatAppId
        LoginWeChatEndpoint       = $LoginWeChatEndpoint
        LoginWeChatAllowOpenId    = [bool]$LoginWeChatAllowOpenId
        LoginQQAppId              = $LoginQQAppId
        LoginQQEndpoint           = $LoginQQEndpoint
        LoginQQAllowOpenId        = [bool]$LoginQQAllowOpenId
        ConfirmLoginAuthProviderChange = [bool]$ConfirmLoginAuthProviderChange
    }
    $entryDecision = Get-ZoneEntryDecision -State $zoneEntryState -Explicit $entryExplicit
    if ($null -ne $savedEntryState) {
        # 第三方登录接入之前写的存档(没有 LoginAuthState 字段)没记过 -Login* 的决定:比较时剔掉这七个键,由下面的防误关核对
        # (namespace 已删时它判不出,要求照传并加 -ConfirmLoginAuthProviderChange)决定,不拿存档里不存在的空串去比。
        $entryDecisionToCompare = $entryDecision
        if ($null -eq $savedEntryState.PSObject.Properties['LoginAuthState']) {
            $entryDecisionToCompare = [ordered]@{}
            foreach ($key in $entryDecision.Keys) {
                if ($key -cnotin @('LoginWeChatAppId', 'LoginWeChatEndpoint', 'LoginWeChatAllowOpenId', 'LoginQQAppId', 'LoginQQEndpoint', 'LoginQQAllowOpenId',
                        'ConfirmLoginAuthProviderChange')) { $entryDecisionToCompare[$key] = $entryDecision[$key] }
            }
            Write-Host "  $entryStatePath predates the login WeChat / QQ check (no LoginAuthState); comparing gate / Ingress / address entries only" -ForegroundColor Yellow
        }
        $entryDiffs = Compare-ZoneEntryDecision -Saved $savedEntryState.Decision -Current $entryDecisionToCompare
        if ($entryDiffs.Count -gt 0) {
            throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:本次 Step 6 的入口决定与同一 rollback-id 的存档 $entryStatePath 不同," +
                "$($entryDiffs.Count) 项:`n  - " + ($entryDiffs -join "`n  - ") + "`n" + $savedEntryHint +
                "。同一 rid 重跑不许换掉入口(namespace 删掉之后,存档是唯一的核对依据);确认存档本身记错了,先人工核对该 zone 的部署记录,再删除该文件重跑。")
        }
        Write-Host "  Step 6 entry decision matches the one saved by the same -RollbackId ($entryStatePath)" -ForegroundColor Green
    }
}
# login 第三方登录(微信 / QQ)的防误关(-Apply,停服之前)。六个 -Login* 参数不粘滞,而 Step 1 会连 login ConfigMap 与 Secret
# login-oauth 一起删掉,k8s_deploy.ps1 自己的防误关闸(Assert-LoginAuthProviderClusterState)在 Step 6 面对的是空 namespace。
# 判据与 k8s_deploy.ps1 同一份(lib Get-LoginAuthProviderDrift),本脚本只换了「线上现状」的来源:namespace 里已没有 login ConfigMap
# (多半已被同一 rid 上一次的 Step 1 删掉)时,改用存档里首次运行读到的现状;存档里也没有就判不出,拒绝。
# 读到的现状(只有公开的 AppId / Endpoint、键名与 Secret 在不在,没有任何密钥值)随入口决定一起存进 entry-state.json。
$loginAuthState = $null
if ($ConfirmLoginAuthProviderChange) {
    Write-Host "  Explicit -ConfirmLoginAuthProviderChange: Step 6 renders login WeChat / QQ login only from this run's -Login* arguments; the live login ConfigMap / Secret is not checked"
}
else {
    $loginAuthContract = Get-ClientEntryContract
    $loginAuthBaseArgs = Get-ClientEntryKubectlBaseArgs -KubeContext $KubeContext -KubeConfig $KubeConfig
    # 只用于显示(参数与 lib Get-LoginAuthProviderLiveState 实际执行的一致);Secret 只用 -o name 探测存在性,不读值。
    $loginAuthPreview = @(
        @('get', 'configmap', $loginAuthContract.LoginConfigMapName, '-n', $zoneNamespace, '--ignore-not-found', '-o', 'json'),
        @('get', 'secret', $loginAuthContract.LoginOAuthSecretName, '-n', $zoneNamespace, '--ignore-not-found', '-o', 'name')
    ) | ForEach-Object { Format-CommandLine -Program 'kubectl' -Arguments (@($loginAuthBaseArgs) + $_) }
    Write-Host "  $($loginAuthPreview -join '; ') → live login WeChat / QQ login must not be turned off or changed by this run's -Login* arguments"
    if ($Apply) {
        try {
            $loginAuthState = Get-LoginAuthProviderLiveState -Namespace $zoneNamespace -KubeContext $KubeContext -KubeConfig $KubeConfig
        }
        catch {
            throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:读不到 namespace $zoneNamespace 里 login 第三方登录的线上现状" +
                "($($_.Exception.Message)),判不出 Step 6 会不会关掉微信 / QQ 登录。修好集群访问后重跑;确认可以只按本次的 -Login* 参数重建," +
                "加 -ConfirmLoginAuthProviderChange。")
        }
        if (-not $loginAuthState.ConfigMapPresent) {
            $savedLoginAuthState = if ($null -ne $savedEntryState) { $savedEntryState.PSObject.Properties['LoginAuthState'] } else { $null }
            if ($null -ne $savedLoginAuthState -and $null -ne $savedLoginAuthState.Value) {
                Write-Host "  namespace $zoneNamespace has no configmap/$($loginAuthContract.LoginConfigMapName) (deleted by an earlier run); checking against the login state saved by the same -RollbackId ($entryStatePath)" -ForegroundColor Yellow
                $loginAuthState = $savedLoginAuthState.Value
            }
            else {
                throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:namespace $zoneNamespace 里没有 configmap/$($loginAuthContract.LoginConfigMapName)" +
                    "(多半已被同一 rid 上一次的 Step 1 删掉,或该 zone 从未部署 login),同一 rid 的存档里也没有 login 的线上现状,判不出回滚前有没有开微信 / QQ 登录;" +
                    "Step 6 只按本次的 -Login* 参数重建 login。照传回滚前的 -LoginWeChatAppId / -LoginQQAppId(及 Endpoint / AllowOpenId)并加 " +
                    "-ConfirmLoginAuthProviderChange。" + $(if ($savedEntryHint) { "`n$savedEntryHint" } else { '' }))
            }
        }
        $loginAuthDrift = Get-LoginAuthProviderDrift -Live $loginAuthState.Live -OAuthSecretPresent ([bool]$loginAuthState.OAuthSecretPresent) `
            -WeChatAppId $LoginWeChatAppId -WeChatEndpoint $LoginWeChatEndpoint -WeChatAllowOpenId ([bool]$LoginWeChatAllowOpenId) `
            -QQAppId $LoginQQAppId -QQEndpoint $LoginQQEndpoint -QQAllowOpenId ([bool]$LoginQQAllowOpenId)
        if ($loginAuthDrift.Drifts.Count -gt 0) {
            $loginAuthKeep = if ($loginAuthDrift.KeepArgs) { "要保持现状请照传 $($loginAuthDrift.KeepArgs)" } else { '线上没有可照传的 provider' }
            throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:Step 1 会删掉 namespace $zoneNamespace(login ConfigMap 与 Secret " +
                "$($loginAuthContract.LoginOAuthSecretName) 一并删除),Step 6 只按本次的 -Login* 参数重建 login。以下 $($loginAuthDrift.Drifts.Count) 项" +
                "与线上 login 第三方登录不一致或判不出:`n  - " + ($loginAuthDrift.Drifts -join "`n  - ") +
                "`n$loginAuthKeep;确认要关闭或改动,加 -ConfirmLoginAuthProviderChange。经包装入口调用时用 dev_tools.ps1 的同名参数。")
        }
        Write-Host "  login WeChat / QQ login: live state matches this run's -Login* arguments$(if ($loginAuthDrift.KeepArgs) { " ($($loginAuthDrift.KeepArgs))" } else { ' (not enabled)' })" -ForegroundColor Green
    }
    else {
        Write-Host "  (dry-run does not read the cluster; -Apply rejects live WeChat / QQ login that this run would turn off or change, or unreadable state, before shutdown)" -ForegroundColor Yellow
    }
}
# 集群现状过了再做静态预检:前者只读一次 kubectl,后者要完整渲染一遍 zone-up,便宜的先拒。
Write-Host "  Static preflight: run dev_tools.ps1 k8s-zone-up with the same arguments as Step 6 (see the Step 6 command below) plus -DryRun; k8s_deploy.ps1 checks argument combinations, profile and secrets, and release gates (without calling kubectl)"
if ($Apply) {
    Invoke-ZoneUpStaticPreflight -DevToolsPath (Join-Path $ScriptDir "dev_tools.ps1") -UpArgs $upArgs
} else {
    Write-Host "  (not executed during dry-run; -Apply rejects a nonzero exit code before shutdown)" -ForegroundColor Yellow
}
# 第 0 步 c:Kafka LAG 只读快照(只告警,取代原 Step 2 的排空等待)。这些积压将被 Step 5 -ToLatest 丢弃。
Write-Host "  Kafka LAG snapshot on $KafkaTopic (read-only, warnings only; replaces the removed Step 2 drain wait; this backlog will be discarded by Step 5 -ToLatest):"
foreach ($snapshotGroup in @($KafkaGroup, $KafkaLegacyGroup)) {
    Write-Host "    kafka-consumer-groups.sh --bootstrap-server $KafkaBootstrap --describe --group $snapshotGroup"
}
if ($Apply) {
    Show-KafkaTopicLagSnapshot -Bootstrap $KafkaBootstrap -Topic $KafkaTopic -Groups @($KafkaGroup, $KafkaLegacyGroup)
} else {
    Write-Host "  (dry-run does not read Kafka)" -ForegroundColor Yellow
}
# 第 0 步 d:Step 5 预演。zone 组没有位点时,Kafka 能否为它按 -ToLatest 生成完整计划要在停服之前确认。
Write-Host ("  Step 5 rehearsal: describe $KafkaGroup; when it holds no committed offsets on $KafkaTopic, run kafka_offset_reset.ps1 " +
    "-BootstrapServer $KafkaBootstrap -Topic $KafkaTopic -Group $KafkaGroup -ToLatest -Partitions $kafkaPartitionCount (dry-run, no -RetireGroup)")
if ($Apply) {
    Invoke-Step5Rehearsal -ResetScriptPath (Join-Path $ScriptDir "kafka_offset_reset.ps1") -Bootstrap $KafkaBootstrap -Topic $KafkaTopic `
        -ZoneGroup $KafkaGroup -Partitions $kafkaPartitionCount
} else {
    Write-Host "  (not executed during dry-run; -Apply rejects a failed describe or an incomplete reset plan before shutdown)" -ForegroundColor Yellow
}
# 第 0 步 e:Step 6 的 k8s_deploy.ps1 读位点的那条路径(kubectl exec 进 infra namespace 的 kafka-0)现在就要读得通,并与本机 CLI 一致。
# infra namespace 取 Step 6 实际会用的值:Step 6 经 dev_tools.ps1 k8s-zone-up 部署、不传 -InfraNamespace,即 dev_tools.ps1 的参数默认值
# (按语法树读,不在本脚本抄一份;读不出即拒绝)。
$devToolsPath = Join-Path $ScriptDir "dev_tools.ps1"
$step6InfraNamespace = (Get-DeployParamDefaults -DeployScriptPath $devToolsPath -Names @('InfraNamespace'))['InfraNamespace']
$step6InfraText = if ([string]::IsNullOrWhiteSpace($step6InfraNamespace)) { '<unreadable>' } else { $step6InfraNamespace }
Write-Host "  Step 6 Kafka facts check (same path as k8s_deploy.ps1 Read-DbGroupTopicFacts; compared with the local CLI on $KafkaTopic):"
# 参数函数整体返回数组，先接收再拼接；统一格式化含空白的参数，保证预告命令可直接核对。
$kubectlDisplayBaseArgs = Get-ClientEntryKubectlBaseArgs -KubeContext $KubeContext -KubeConfig $KubeConfig
foreach ($factGroup in @($KafkaGroup, $KafkaLegacyGroup)) {
    # 只用于打印;-Apply 时由 Assert-Step6KafkaFactsReadable 经 Invoke-ClientEntryKubectl 执行同一份参数。
    $describeShownArgs = Get-DbGroupDescribeExecArgs -InfraNamespace $step6InfraText -Group $factGroup
    $factArgs = $kubectlDisplayBaseArgs + $describeShownArgs
    Write-Host ("    " + (Format-CommandLine -Program 'kubectl' -Arguments $factArgs))
}
if ($Apply) {
    if ([string]::IsNullOrWhiteSpace($step6InfraNamespace)) {
        throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:读不到 $devToolsPath 的 -InfraNamespace 默认值(不是字面常量或文件无法解析)," +
            "不知道 Step 6 会去哪个 infra namespace 的 kafka-0 读消费组位点。")
    }
    Assert-Step6KafkaFactsReadable -InfraNamespace $step6InfraNamespace -Bootstrap $KafkaBootstrap -Topic $KafkaTopic `
        -ZoneGroup $KafkaGroup -LegacyGroup $KafkaLegacyGroup -KubeContext $KubeContext -KubeConfig $KubeConfig
} else {
    Write-Host "  (not executed during dry-run; -Apply rejects an unreadable kubectl exec or a mismatch with the local CLI before shutdown)" -ForegroundColor Yellow
}

# 第 0 步 f(上半):部署事实、go/db 所用 Redis 的实例身份、回档工具的构建提交(只读,先于 Step 1)。部署对象只取非机密的拓扑与世代
# (Get-DeploySnapshotKubectlArgs / ConvertFrom-ZoneDeploySnapshot),与本次参数、本机 db.yaml 核对后落盘;namespace 删掉之后(同一 rid 重跑)复用存档。
# run_id 要进每次工具调用的公共参数(-expect-run-id),所以先于下面组装工具参数;工具 preflight 在组装之后(第 0 步 f 下半)。
$toolShown = if ($RollbackDiscardTool) { $RollbackDiscardTool } else { '<zone_rollback_discard>' }
$snapshotCommandArgs = Get-DeploySnapshotKubectlArgs -Namespace $zoneNamespace
$snapshotShown = Format-CommandLine -Program 'kubectl' -Arguments ($kubectlDisplayBaseArgs + $snapshotCommandArgs)
Write-Host "  Deploy facts (read-only, secrets are never read): $snapshotShown -> $factsPath"
$redisIdentityArgs = $kubectlDisplayBaseArgs + @('exec', '-n', $step6InfraText, 'pod/redis-0', '-c', 'redis', '--')
$redisIdentityShown = Format-CommandLine -Program 'kubectl' -Arguments $redisIdentityArgs
Write-Host ("  Redis identity (read-only): $redisIdentityShown sh -c '<REDISCLI_AUTH from the pod REDIS_PASSWORD>; " +
    "redis-cli --raw -h <go/db RedisClient host> -p <port> INFO server' -> run_id, passed to every discard tool call as -expect-run-id")
Write-Host "  Discard tool build: $toolShown -version (must print commit=<40 hex> and dirty=false; the commit must start with the tag of the live db image)"
$redisRunId = $null
$toolVersion = $null
if ($Apply) {
    New-Item -ItemType Directory -Path $reportDir -Force | Out-Null
    $deployFacts = Resolve-ZoneDeployFacts -Namespace $zoneNamespace -FactsPath $factsPath -LegacyGroup ([string]$dbGroupBase.Value) -ZoneGroup "$($dbGroupBase.Value)_z$ZoneId" `
        -KubeContext $KubeContext -KubeConfig $KubeConfig
    $liveGroupText = "$($deployFacts.KafkaGroup.Shape) $($deployFacts.KafkaGroup.Group)$(if ($deployFacts.KafkaGroup.HandoffFrom) { " <- $($deployFacts.KafkaGroup.HandoffFrom)" })"
    Write-Host ("  live go/db: RedisClient=$($deployFacts.Redis.Hosts)/db$($deployFacts.Redis.DB) Fencing.Mode=$($deployFacts.FencingMode) consumer group=$liveGroupText image=$($deployFacts.DbImage);" +
        " the tool connects to $($RedisHost):$($RedisPort)/db$RedisDB (a port-forward is fine; the run_id below binds it to this instance)")
    $factsProblems = Get-DeployFactsProblems -Facts $deployFacts -ZoneId $ZoneId -RedisDB $RedisDB -BarrierProtocol $BarrierProtocol `
        -LocalGroupBase ([string]$dbGroupBase.Value) -LocalTopicGeneration $kafkaTopicGeneration -LocalPartitionCnt $kafkaPartitionCount
    if ($factsProblems.Count -gt 0) {
        throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:部署事实($factsPath)与本次参数不一致,$($factsProblems.Count) 项:`n  - " + ($factsProblems -join "`n  - "))
    }
    # Redis 实例身份(回档复核 [4]):以前「目标就是 go/db 的库」只看本 topic 有没有游标,另一个环境里同 ZoneId 的 Redis 同样能过。
    $redisRunId = Read-GoDbRedisRunId -InfraNamespace $step6InfraNamespace -RedisHosts ([string]$deployFacts.Redis.Hosts) -KubeContext $KubeContext -KubeConfig $KubeConfig
    Write-Host "  go/db Redis $($deployFacts.Redis.Hosts): run_id=$redisRunId; every discard tool call requires the instance behind $($RedisHost):$($RedisPort) to report it (-expect-run-id)" -ForegroundColor Green
    # 工具构建提交(回档复核 [5]):键名与 Lua 契约只对同一提交的 go/db 成立,以前只写进 deploy-facts、从不核对。
    try {
        $toolVersion = Get-DiscardToolVersion -ToolPath $RollbackDiscardTool
    }
    catch {
        throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:读不到回档工具的构建提交($toolShown -version):$($_.Exception.Message)。" +
            "工具必须与线上 go/db 同一提交构建,核对不了就不能用它。")
    }
    $versionProblem = Test-DiscardToolMatchesDbImage -Version $toolVersion -DbImage ([string]$deployFacts.DbImage)
    if ($null -ne $versionProblem) {
        throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:$versionProblem。换成与线上 db 镜像同一提交、干净工作树构建的 zone_rollback_discard 后重跑。")
    }
    Write-Host "  discard tool commit $($toolVersion.Commit) matches the live db image $($deployFacts.DbImage)" -ForegroundColor Green
}

# ── 回档工具的公共参数与各模式的完整参数(Get-DiscardToolArgs,契约见文件头) ──
# dry-run 缺的项打印占位符;-Apply 时 E3 已保证它们都在、run_id 已在上面读出。口令与 DSN 只以环境变量名出现。
$toolCommon = [ordered]@{
    'deploy-facts'       = $factsPath
    'zone'               = "$ZoneId"
    'topic-generation'   = "$kafkaTopicGeneration"
    'redis'              = $(if ($RedisHost) { "${RedisHost}:${RedisPort}" } else { '<go/db shared redis host:port>' })
    'redis-db'           = "$RedisDB"
    'redis-password-env' = $RedisPasswordEnv
    'expect-run-id'      = $(if ($redisRunId) { $redisRunId } else { '<redis run_id read in step 0 f>' })
    'mysql-dsn-env'      = $(if ($MySqlDsnEnv) { $MySqlDsnEnv } else { '<mysql dsn env var>' })
    'rollback-id'        = $rollbackId
    'barrier-protocol'   = $(if ($BarrierProtocol) { $BarrierProtocol } else { '<legacy>' })
    'report-dir'         = $reportDir
}
# 判定参数(回档复核 [2]):inventory / purge / verify 共用这一份。purge 原样重跑 inventory 的判定,以前它不带 -target-time 与
# -acknowledge-no-post-t-writes,带了 -AcknowledgeNoPostTWrites 的路径上 inventory 能过、Step 5 跑完之后 purge 必然退出 3。
$judgeArgs = @('-target-time', $TargetTime, '-fingerprint-baseline', $baselinePath) + @(if ($AcknowledgeNoPostTWrites) { '-acknowledge-no-post-t-writes' })
# 租约等待覆盖 30s 租约加回收周期;handoff 等待不低于 go/shared/ownerepoch 的 HandoffTTL 300s,两者分别计时(对抗复核 [0][12])。
# 行指纹基线由 stopped 在停服之后写(回档复核 [1]),preflight 不再取基线。
$toolArgs = @{
    preflight = Get-DiscardToolArgs -Mode preflight -Common $toolCommon -Extra @(if ($AllowEmptyTarget) { '-allow-empty-target' })
    stopped   = Get-DiscardToolArgs -Mode stopped -Common $toolCommon -Extra @('-lease-wait', '90s', '-handoff-wait', '300s', '-fingerprint-baseline', $baselinePath)
    inventory = Get-DiscardToolArgs -Mode inventory -Common $toolCommon -Extra ($judgeArgs + @('-report', (Join-Path $reportDir 'inventory.json')))
    purge     = Get-DiscardToolArgs -Mode purge -Common $toolCommon -Extra ($judgeArgs + @('-confirm', '-report', $purgeReportPath))
}
# verify 的 -kafka-cut 要等 Step 5 记下切点:打印与 5b 都经这一个脚本块组装,参数只有一份。
$newVerifyArgs = {
    param([Parameter(Mandatory = $true)][string]$CutText)
    Get-DiscardToolArgs -Mode verify -Common $toolCommon -Extra ($judgeArgs + @('-kafka-cut', $CutText, '-purge-report', $purgeReportPath, '-report', (Join-Path $reportDir 'verify.json')))
}
# Step 5 的参数(hashtable splatting,理由见 Step 5)与失败时打印的单步命令先组装好。
$kArgs = @{
    BootstrapServer = $KafkaBootstrap
    Topic           = $KafkaTopic
    Group           = $KafkaGroup
    RetireGroup     = $KafkaLegacyGroup
    ToLatest        = $true
    Partitions      = $kafkaPartitionCount
}
if ($Apply) { $kArgs.Apply = $true }
$step5Text = "pwsh -File tools/scripts/kafka_offset_reset.ps1 -BootstrapServer $KafkaBootstrap -Topic $KafkaTopic -Group $KafkaGroup -RetireGroup $KafkaLegacyGroup -ToLatest -Partitions $kafkaPartitionCount -Apply"
$step6Text = "pwsh -File tools/scripts/dev_tools.ps1 " + (($upArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { if ($_.Value -is [bool]) { "-$($_.Key)" } else { "-$($_.Key) $($_.Value)" } }) -join ' ')
$cutDescribeText = "kafka-consumer-groups.sh --bootstrap-server $KafkaBootstrap --describe --group $KafkaGroup"
# 中止路径的 zone-up(回档复核 [9]):与 Step 6 同一份参数,只去掉 DbConsumerGroup(即 keep)。全新 namespace 下 keep 按 Kafka 位点
# 落到 final(L) 或 final(Z),与回档前一致;显式 zone 会把回档前还在 legacy 组上的 zone 渲染成交接形状,db 镜像没有交接闸时起不来。
$abortUpArgs = $upArgs.Clone()
$abortUpArgs.Remove('DbConsumerGroup')
$abortUpText = "pwsh -File tools/scripts/dev_tools.ps1 " + (($abortUpArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { if ($_.Value -is [bool]) { "-$($_.Key)" } else { "-$($_.Key) $($_.Value)" } }) -join ' ')
$entryArgsText = if ($null -ne $entryDecision) { Format-ZoneEntryDecisionArgs -Decision $entryDecision } else { '' }
# nofile 模式不进入口决定:显式给了就提示照传;留空时第 0 步 a 按存档里核对过的集群现状核对。
$nofileRerunText = if ($NodeOpenFileLimitMode) { ";-NodeOpenFileLimitMode 照传 $NodeOpenFileLimitMode(本次显式给值没有核对集群,namespace 已删时重跑留空会被判为读不到)" } else { '' }
$rerunWholeHint = if ($entryArgsText) {
    ("排除原因后用同一个 rid 整脚本重跑:加 -RollbackId $rollbackId,入口参数照传 $entryArgsText(namespace 已删时第 0 步 a 以报告目录里的 " +
        "entry-state.json 为准,显式值必须与它相同)$nofileRerunText;第 0 步 f 复用报告目录里的 deploy-facts.json。")
}
else {
    ("排除原因后用同一个 rid 整脚本重跑:加 -RollbackId $rollbackId;namespace 已删,须显式传 -GateRouterMode、-ClientEntryMode、-NodeOpenFileLimitMode," +
        "并对 Ingress 表态(-GatewayIngressHost … 或 -ConfirmNoGatewayIngress)、对第三方登录表态(照传 -Login* 并加 -ConfirmLoginAuthProviderChange)," +
        "第 0 步 f 复用报告目录里的 deploy-facts.json。")
}
$abortBeforeStep5 = ("中止路径(Step 5 之前都可用):换回 PITR 之前保留的原库 zone_${ZoneId}_db → 不跑 Step 5 与 4b → k8s-zone-up 不带 -DbConsumerGroup" +
    "(keep:全新 namespace 按 Kafka 位点落回回档前的 final(L) 或 final(Z),两组都有位点时 k8s_deploy.ps1 拒绝并给出收尾命令;" +
    "不要照搬 Step 6 那份显式指定 zone 组的参数,回档前还在 legacy 组上的 zone 会被渲染成交接形状):$abortUpText。")
$forwardHint = ("Step 5 已丢弃积压,原库缺这批写,不能再换回原库,只能向前。修好原因后用同一 rid 按顺序执行:" +
    "`n  - Step 5(只在它没成功时):$step5Text" +
    "`n  - 切点:$cutDescribeText(每个分区 CURRENT == LOG-END,记为切点,对照 $(Join-Path $reportDir 'kafka-cut.json'))" +
    "`n  - Step 4b:$(Format-CommandLine -Program $toolShown -Arguments $toolArgs.purge)" +
    "`n  - Step 5b:同一条 describe 必须仍等于切点,再 $(Format-CommandLine -Program $toolShown -Arguments (& $newVerifyArgs '<切点 p=offset,…>'))" +
    "`n  - Step 6:$step6Text")

# 第 0 步 f(下半):回档工具 preflight,通过之后写入口决定存档(回档复核 [10]),然后才进 Step 1。
Write-Host "  Discard tool preflight: $(Format-CommandLine -Program $toolShown -Arguments $toolArgs.preflight)"
if ($Apply) {
    # 运行记录:每次 -Apply 追加一行,只记非机密的参数(口令与 DSN 只记环境变量名)。
    $runRecord = [ordered]@{
        startedAtUtc     = $nowUtc.ToString('o')
        rollbackId       = $rollbackId
        zone             = $ZoneName
        zoneId           = $ZoneId
        targetTime       = $TargetTime
        pitrRestoredTo   = $PitrRestoredTo
        skipMySqlPause   = [bool]$SkipMySqlPause
        topic            = $KafkaTopic
        zoneGroup        = $KafkaGroup
        legacyGroup      = $KafkaLegacyGroup
        partitions       = $kafkaPartitionCount
        redis            = "$($toolCommon['redis'])/db$RedisDB"
        redisPasswordEnv = $RedisPasswordEnv
        mySqlDsnEnv      = $MySqlDsnEnv
        barrierProtocol  = $BarrierProtocol
        discardTool      = $RollbackDiscardTool
        discardToolCommit = $toolVersion.Commit
        redisRunId       = $redisRunId
        quiesceSeconds   = $QuiesceSeconds
        acknowledgeNoPostTWrites = [bool]$AcknowledgeNoPostTWrites
    }
    Add-Content -LiteralPath (Join-Path $reportDir 'run-log.jsonl') -Encoding utf8 -Value ($runRecord | ConvertTo-Json -Compress)
    $preflightCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $toolArgs.preflight
    if ($preflightCode -ne 0) {
        $emptyHint = if ($preflightCode -eq 4) { "确认 -RedisHost / -RedisDB 指向 go/db 实际在用的 Redis;只有从未落过库的新 zone 才加 -AllowEmptyTarget。" } else { "原因见上面工具的输出与 $reportDir(run_id 不一致说明 -RedisHost 连到的不是 go/db 用的那个实例)。" }
        throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:回档工具 preflight 以退出码 $preflightCode 结束($(Get-DiscardToolExitMeaning -ExitCode $preflightCode))。" + $emptyHint)
    }
    Write-Host "  Discard tool preflight passed" -ForegroundColor Green
    # 入口决定存档(回档复核 [10]):第 0 步全部通过才写;同一 rid 已有存档时上面已逐项比较过,不覆盖。
    if ($null -eq $savedEntryState) {
        # Schema 2(2026-10-18):加 LoginAuthState 与 Decision 的七个第三方登录键。Schema 1 的存档照常读,只是第三方登录不按存档比较
        # (见第 0 步 a 的入口决定比较)。
        # Schema 3(2026-10-22,B0-4):ClusterState 加 OpenFileLimitMode / OpenFileLimitDetail(本次显式给了 -NodeOpenFileLimitMode 时记为判不出)。
        # 它不进 Decision、不按存档比较(显式值重跑可以改);更早的存档没有这两个字段,namespace 已删时 nofile 判为读不到。
        $entryRecord = [ordered]@{
            Schema        = 3
            RollbackId    = $rollbackId
            Namespace     = $zoneNamespace
            CapturedAtUtc = [datetime]::UtcNow.ToString('o')
            ClusterState  = $zoneEntryState
            Decision      = $entryDecision
            # 第 0 步 a 核对过的 login 第三方登录线上现状(-ConfirmLoginAuthProviderChange 时没读,为 $null)。只有公开的 AppId / Endpoint、
            # 键名与 Secret 在不在,没有密钥值。同一 rid 重跑、namespace 里已没有 login ConfigMap 时以它为准;旧存档没有这个字段 = 判不出。
            LoginAuthState = $loginAuthState
        }
        Set-Content -LiteralPath $entryStatePath -Encoding utf8 -Value ($entryRecord | ConvertTo-Json -Depth 6)
        Write-Host "  entry decision saved to $entryStatePath ($entryArgsText)"
    }
} else {
    Write-Host ("  (not executed during dry-run; -Apply rejects unreadable or inconsistent deploy facts, an unreadable Redis run_id, a discard tool built " +
        "from another commit and a nonzero preflight exit code before shutdown)") -ForegroundColor Yellow
}
Write-Host ""

# ── Step 1: stop the zone ────────────────────────────────────────
Write-Host "── Step 1: k8s-zone-down ──" -ForegroundColor Cyan
# hashtable splatting(理由同 Step 5);-KubeContext / -KubeConfig 非空才透传,与第 0 步预检、Step 6 落在同一集群。
$downArgs = @{
    Command         = "k8s-zone-down"
    ZoneName        = $ZoneName
    NamespacePrefix = $NamespacePrefix
}
if (-not [string]::IsNullOrWhiteSpace($KubeContext)) { $downArgs.KubeContext = $KubeContext }
if (-not [string]::IsNullOrWhiteSpace($KubeConfig)) { $downArgs.KubeConfig = $KubeConfig }
Write-Host ("  dev_tools.ps1 " + (($downArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { "-$($_.Key) $($_.Value)" }) -join ' '))
$namespaceArgs = $kubectlDisplayBaseArgs + @('get', 'namespace', $zoneNamespace, '-o', 'name', '--ignore-not-found')
$namespaceShown = Format-CommandLine -Program 'kubectl' -Arguments $namespaceArgs
Write-Host "  then: $namespaceShown must print nothing (Remove-Zone tolerates delete failures)"
if ($Apply) {
    & "$ScriptDir/dev_tools.ps1" @downArgs
    if ($LASTEXITCODE -ne 0) { throw "k8s-zone-down failed(退出码 $LASTEXITCODE):尚未 PITR,Redis / Kafka 尚未改动。$rerunWholeHint" }
    Assert-ZoneNamespaceGone -Namespace $zoneNamespace -KubeContext $KubeContext -KubeConfig $KubeConfig
    Write-Host "  zone $ZoneName stopped" -ForegroundColor Green
}
Write-Host ""

# Step 2(zone-down 之后等 LAG 排空)已删除(2026-10-15):本 zone 的 db 已停,LAG 不可能排空,
# 而 Step 5 把位点拨到日志末端(丢弃积压)。停服前的只读快照见第 0 步 c,停服后的见 Step 1b (iv)。

# ── Step 1b: post-stop checks (read-only, before PITR) ──────────
# 停服之后、PITR 之前再确认「本 topic 不会再有写入、也不会再被消费」:这里拒绝时 zone 只是停了,数据一点没动,放弃回档直接 zone-up 即可;
# 拖到 PITR 之后才发现,MySQL 已回档、缓存还是 T 之后的,没有干净的退路(回档丢弃语义对抗复核 [6])。
Write-Host "── Step 1b: post-stop checks (read-only, before PITR) ──" -ForegroundColor Cyan
# 顺序是「先证明没有写者,再取基线」(回档复核 [1]):(i)(ii) 证明 Kafka 上没有消费者、也没有生产者,(iii) 的工具再确认重试租约已过期、
# 没有人在别处在线,最后一步才写停服行指纹基线 —— 那之后 zone_<id>_db 不再有写入,4a 拿 PITR 之后的指纹与它比较才有意义。
Write-Host "  (i)   kafka-consumer-groups.sh --bootstrap-server $KafkaBootstrap --describe --group $KafkaGroup --state (must be Empty / Dead, or the group must not exist)"
Write-Host "        kafka-consumer-groups.sh --bootstrap-server $KafkaBootstrap --describe --group $KafkaLegacyGroup (CONSUMER-ID must be '-' on every $KafkaTopic row)"
Write-Host "  (ii)  kafka-get-offsets.sh --bootstrap-server $KafkaBootstrap --topic $KafkaTopic --time -1, twice, ${QuiesceSeconds}s apart (the log end must not move)"
Write-Host "  (iii) $(Format-CommandLine -Program $toolShown -Arguments $toolArgs.stopped)"
Write-Host "        (leases, players online elsewhere; writes the stopped-phase fingerprint baseline $baselinePath last)"
Write-Host "  (iv)  LAG of both groups on $KafkaTopic; backlog that Step 5 -ToLatest discards = LAG of the zone group when it holds offsets on $KafkaTopic, else of the legacy group"
if ($Apply) {
    $stopRefusal = "k8s-zone-rollback 在 Step 1b 拒绝继续:zone 已停服,尚未 PITR,尚未改动 Redis / Kafka。"
    $stopHint = ("`n$rerunWholeHint`n放弃回档:数据都没动,k8s-zone-up 不带 -DbConsumerGroup(keep,按 Kafka 位点落回回档前的消费组形状)即可恢复:" +
        "$abortUpText。")
    try {
        $consumerProblems = Get-KafkaTopicConsumerProblems -Bootstrap $KafkaBootstrap -Topic $KafkaTopic -ZoneGroup $KafkaGroup -LegacyGroup $KafkaLegacyGroup
    }
    catch {
        throw ($stopRefusal + "判不出 $KafkaTopic 上还有没有消费者(kafka-consumer-groups.sh 失败或输出无法解析):$($_.Exception.Message)" + $stopHint)
    }
    if ($consumerProblems.Count -gt 0) {
        throw ($stopRefusal + "$KafkaTopic 上还有消费者:`n  - " + ($consumerProblems -join "`n  - ") + $stopHint)
    }
    try {
        $logEndBefore = Read-KafkaTopicLogEnd -Bootstrap $KafkaBootstrap -Topic $KafkaTopic
        Write-Host "  log end of ${KafkaTopic}: $(ConvertTo-KafkaCutText -Cut $logEndBefore); waiting ${QuiesceSeconds}s"
        $quiesceWatch = [System.Diagnostics.Stopwatch]::StartNew()
        Start-Sleep -Seconds $QuiesceSeconds
        $logEndAfter = Read-KafkaTopicLogEnd -Bootstrap $KafkaBootstrap -Topic $KafkaTopic
        Write-Host "  log end of ${KafkaTopic} after $([Math]::Round($quiesceWatch.Elapsed.TotalSeconds, 1))s: $(ConvertTo-KafkaCutText -Cut $logEndAfter)"
    }
    catch {
        throw ($stopRefusal + "读不到 $KafkaTopic 的日志末端:$($_.Exception.Message)" + $stopHint)
    }
    $quietProblems = Test-KafkaTopicQuiescent -First $logEndBefore -Second $logEndAfter -Partitions $kafkaPartitionCount
    if ($quietProblems.Count -gt 0) {
        throw ($stopRefusal + "停服之后 $KafkaTopic 仍有写入(别的 zone 的 scene 托管本 zone 归属玩家、晚到的本地队列消息或 saveoutbox 补投),现在拨位点、清 Redis 都不安全:`n  - " +
            ($quietProblems -join "`n  - ") + "`n查清生产者(他区在线的本 zone 归属玩家要先下线)。" + $stopHint)
    }
    Write-Host "  $KafkaTopic is quiet" -ForegroundColor Green
    # 工具 stopped 放在 Kafka 两项之后:它最后一步写停服行指纹基线,那一刻必须已经证明没有写者(回档复核 [1])。
    $stoppedCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $toolArgs.stopped
    if ($stoppedCode -ne 0) {
        throw ($stopRefusal + "回档工具 stopped 以退出码 $stoppedCode 结束($(Get-DiscardToolExitMeaning -ExitCode $stoppedCode)):" +
            "多半还有存活的 go/db 重试租约、本 zone 归属的玩家仍在别的 zone 在线,或同一 rid 已有不是停服阶段取的行指纹基线;详见上面工具的输出。" + $stopHint)
    }
    Write-Host "  stopped-phase fingerprint baseline: $baselinePath" -ForegroundColor Green
    Write-Host "  LAG on $KafkaTopic (informational):"
    Show-KafkaTopicLagSnapshot -Bootstrap $KafkaBootstrap -Topic $KafkaTopic -Groups @($KafkaGroup, $KafkaLegacyGroup)
    Show-KafkaDiscardEstimate -Bootstrap $KafkaBootstrap -Topic $KafkaTopic -ZoneGroup $KafkaGroup -LegacyGroup $KafkaLegacyGroup
} else {
    Write-Host "  (not executed during dry-run; -Apply refuses before the PITR prompt when any check fails)" -ForegroundColor Yellow
}
Write-Host ""

# ── Step 3: MySQL PITR (manual) ──────────────────────────────────
# 只回退本 zone 的库:data_service 的全局库(id_segment、player_name、路由表)一起回退会重复发出 player_id;guild / mail / 收件箱 / trade
# 各有自己的库,不随 zone 回退(跨服务状态不回退是已接受的代价)。旧文案里的库名 game 早已不存在。
Write-Host "── Step 3: MySQL Point-in-Time Recovery ──" -ForegroundColor Cyan
Write-Host "  ⚠️ This step is NOT automated." -ForegroundColor Yellow
Write-Host "  See docs/ops/mysql-backup-pitr-runbook.md §4.1 for the procedure."
Write-Host "  Target time: $TargetTime (UTC)"
Write-Host "  Restore ONLY zone_${ZoneId}_db, plus any placement-store databases listed by the discard tool in $reportDir."
Write-Host "  Do NOT roll back global databases: data_service (id_segment / player_name / route tables), guild, mail, player inbox, trade."
Write-Host "  Restore into a shadow database first, then swap it in; KEEP the pre-rollback zone_${ZoneId}_db (rename it) until the Step 6 smoke test passes."
Write-Host ""
Write-Host "  Typical command (verify before running; mysqlbinlog reads --stop-datetime in the local time zone of its host, so run it with TZ=UTC):"
Write-Host "    mysqlbinlog --database=zone_${ZoneId}_db --stop-datetime=`"$($targetUtc.ToString('yyyy-MM-dd HH:mm:ss', [System.Globalization.CultureInfo]::InvariantCulture))`" \\"
Write-Host "      /backup/binlog/mysql-bin.* | mysql -h shadow-host -u root -p zone_${ZoneId}_db_restore"
Write-Host "  Abort path until Step 5 runs: swap the original zone_${ZoneId}_db back, skip Step 5 and Step 4b, then run zone-up WITHOUT -DbConsumerGroup (keep):"
Write-Host "    $abortUpText"
Write-Host ""

if ($Apply -and -not $SkipMySqlPause) {
    Write-Host "  When MySQL PITR is complete and verified, press ENTER to continue."
    Write-Host "  Press CTRL+C to abort." -ForegroundColor Yellow
    [void](Read-Host)
}
elseif ($Apply) {
    # E3 已保证 -SkipMySqlPause 配了 -PitrRestoredTo(且等于 TargetTime);声明已写进运行记录。
    Write-Host "  -SkipMySqlPause: operator declared PITR of zone_${ZoneId}_db to $PitrRestoredTo (recorded in $(Join-Path $reportDir 'run-log.jsonl'))" -ForegroundColor Yellow
}
Write-Host ""

# ── Step 4a: inventory (read-only) ──────────────────────────────
# PITR 之后、任何写之前盘点。这里拒绝时 Kafka 与 Redis 都还没动,还能按中止路径换回原库;
# blocked 玩家怎么处置待负责人裁决 #4,裁决之前一律停在这里。盘点的行指纹与 Step 1b 的停服基线比较(回档复核 [1])。
# 盘点通过之后、Step 5 之前再复核一次停服状态(回档复核 [8]):中间隔着人工 PITR,namespace 可能被重建、消费者可能回来。
Write-Host "── Step 4a: inventory of post-TargetTime Redis state (read-only) ──" -ForegroundColor Cyan
Write-Host "  $(Format-CommandLine -Program $toolShown -Arguments $toolArgs.inventory)"
Write-Host "  then, before Step 5 (read-only): $namespaceShown must still print nothing; $KafkaGroup must still be Empty / Dead or missing; $KafkaLegacyGroup must still have no member on $KafkaTopic"
if ($Apply) {
    $inventoryRefusal = "k8s-zone-rollback 在 Step 4a 拒绝继续:zone 已停服、MySQL 已 PITR,Redis 与 Kafka 尚未改动。"
    $inventoryCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $toolArgs.inventory
    if ($inventoryCode -eq 6) {
        throw ($inventoryRefusal + "盘点出 blocked 玩家(torn / owner_behind / post_t_candidate / legacy_owned / unclassified,名单见 $(Join-Path $reportDir 'inventory.json'))。" +
            "带 blocked 玩家继续回档待负责人裁决(#4),裁决之前不放行。`n$abortBeforeStep5")
    }
    if ($inventoryCode -ne 0) {
        throw ($inventoryRefusal + "回档工具 inventory 以退出码 $inventoryCode 结束($(Get-DiscardToolExitMeaning -ExitCode $inventoryCode)),详见上面工具的输出与 $reportDir。" +
            "`n$abortBeforeStep5`nPITR 确已做完、排除原因后,也可以带 -RollbackId $rollbackId -SkipMySqlPause -PitrRestoredTo $TargetTime 整脚本重跑。")
    }
    Write-Host "  inventory passed: no blocked players" -ForegroundColor Green
    # Step 5 之前的停服复核(只读):Step 5 不可逆,拨位点之前最后一次确认「没有写者、没有消费者」。
    $recheckRefusal = "k8s-zone-rollback 在 Step 5 之前拒绝继续:zone 已停服、MySQL 已 PITR,Redis 与 Kafka 尚未改动。"
    $recheckHint = ("这段时间里本 zone 的写者可能已经回来,正把积压写进刚回档的库。查清原因(GitOps / CD 重建、手工拉起、别处的 db 实例)并停掉它们;" +
        "库已被写过时只能按中止路径恢复。`n$abortBeforeStep5")
    Assert-ZoneNamespaceGone -Namespace $zoneNamespace -KubeContext $KubeContext -KubeConfig $KubeConfig -Refusal $recheckRefusal -Hint $recheckHint
    try {
        $recheckProblems = Get-KafkaTopicConsumerProblems -Bootstrap $KafkaBootstrap -Topic $KafkaTopic -ZoneGroup $KafkaGroup -LegacyGroup $KafkaLegacyGroup
    }
    catch {
        throw ($recheckRefusal + "判不出 $KafkaTopic 上还有没有消费者(kafka-consumer-groups.sh 失败或输出无法解析):$($_.Exception.Message)`n" + $recheckHint)
    }
    if ($recheckProblems.Count -gt 0) {
        throw ($recheckRefusal + "PITR 期间 $KafkaTopic 上又出现了消费者(zone 组活跃时 Kafka 会拒绝 Step 5 的重置;legacy 组有订阅者时重置能成功、退役却失败," +
            "而那个消费者正在落库):`n  - " + ($recheckProblems -join "`n  - ") + "`n" + $recheckHint)
    }
    Write-Host "  still stopped: namespace gone, no consumer on $KafkaTopic" -ForegroundColor Green
}
Write-Host ""

# ── Step 5: Kafka offsets to the log end ─────────────────────────
Write-Host "── Step 5: Kafka offsets to the log end ($KafkaTopic, -ToLatest) ──" -ForegroundColor Cyan
# 必须用 hashtable splatting($kArgs 在第 0 步 f 之前组装)。之前用的是数组 splatting —— PowerShell 会把数组
# 元素当**位置参数**依次绑定,于是 "-BootstrapServer" 这个字符串本身被绑到
# BootstrapServer、"kafka:9092" 绑到 Group、…… 最后 "-Group" 撞上 [int]$Partitions
# 直接类型转换失败。结果是这个灾难恢复脚本连 dry-run 都跑不到第 6 步。
# zone 组拨到日志末端(丢弃 [T, 停服] 之间从未落库的积压:负责人 2026-10-17 裁决);kafka_offset_reset.ps1 -Apply 之后逐分区复核
# (CURRENT-OFFSET == 计划 NEW-OFFSET),复核通过才删除 legacy 组在本 topic 上的位点(-RetireGroup)。回档后该 zone 一律以 zone 组为权威,
# Step 6 据此落到最终形状。随后读 zone 组位点作为切点 O:之后 4b 与 5b 都以它为准,任何越过切点的写入都会在 5b 被发现。
Write-Host ("  kafka_offset_reset.ps1 " + (($kArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { "-$($_.Key) $($_.Value)" }) -join ' '))
Write-Host "  then: $cutDescribeText (every partition CURRENT == LOG-END; recorded as the cut in $(Join-Path $reportDir 'kafka-cut.json'))"
$kafkaCut = $null
if ($Apply) {
    # 先清零:被调脚本正常结束时 $LASTEXITCODE 保留的是更早一次原生命令的值。
    $global:LASTEXITCODE = 0
    $step5Failure = $null
    try {
        & "$ScriptDir/kafka_offset_reset.ps1" @kArgs
        if ($LASTEXITCODE -ne 0) { $step5Failure = "kafka_offset_reset.ps1 以退出码 $LASTEXITCODE 结束" }
    }
    catch {
        $step5Failure = $_.Exception.Message
    }
    if ($null -ne $step5Failure) {
        throw ("k8s-zone-rollback 在 Step 5 失败:MySQL 已 PITR,Redis 尚未改动;zone 组的位点可能已经拨到末端、积压可能已被丢弃。原因:$step5Failure`n" + $forwardHint)
    }
    try {
        $cutRead = Read-ZoneGroupCut -Bootstrap $KafkaBootstrap -Topic $KafkaTopic -ZoneGroup $KafkaGroup -Partitions $kafkaPartitionCount
    }
    catch {
        throw ("k8s-zone-rollback 在 Step 5 之后读不到 zone 组 $KafkaGroup 的位点,记不下切点:$($_.Exception.Message)`n" + $forwardHint)
    }
    if (@($cutRead.Problems).Count -gt 0) {
        throw ("k8s-zone-rollback 在 Step 5 之后拒绝继续:Step 5 之后又有消息写入 $KafkaTopic,或 zone 组的位点不在日志末端:`n  - " + (@($cutRead.Problems) -join "`n  - ") +
            "`n查清生产者后单独重跑 Step 5。`n" + $forwardHint)
    }
    $kafkaCut = $cutRead.Cut
    $cutRecord = [ordered]@{
        rollbackId    = $rollbackId
        topic         = $KafkaTopic
        group         = $KafkaGroup
        partitions    = $kafkaPartitionCount
        cut           = (ConvertTo-KafkaCutText -Cut $kafkaCut)
        capturedAtUtc = [datetime]::UtcNow.ToString('o')
        note          = '本次回档的丢弃水位:以后任何 -ToDatetime / -ToEarliest 重置都不得越过它,否则停服时那批积压会被重新落库。'
    }
    Set-Content -LiteralPath (Join-Path $reportDir 'kafka-cut.json') -Encoding utf8 -Value ($cutRecord | ConvertTo-Json)
    Write-Host "  cut recorded: $($cutRecord.cut)" -ForegroundColor Green
}
Write-Host ""

# ── Step 4b: discard post-TargetTime Redis state ─────────────────
# 放在 Step 5 之后(回档丢弃语义对抗复核 [4]):先在 Kafka 上记下切点,再清 Redis;两步之间残留写者写进 Kafka 的消息越过切点,
# 写进 Redis 的状态由 5b 的 verify 复核,都能被发现。工具内部先原样重跑 4a 的检查与判定(同一组判定参数 $judgeArgs,回档复核 [2]),
# 任一不过什么都不写;同一 rid 续跑按契约的续跑规则,每次 purge 开新一轮 pass(回档复核 [3])。
Write-Host "── Step 4b: discard post-TargetTime Redis state ──" -ForegroundColor Cyan
Write-Host "  $(Format-CommandLine -Program $toolShown -Arguments $toolArgs.purge)"
if ($Apply) {
    $purgeCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $toolArgs.purge
    if ($purgeCode -ne 0) {
        throw ("k8s-zone-rollback 在 Step 4b 失败:回档工具 purge 以退出码 $purgeCode 结束($(Get-DiscardToolExitMeaning -ExitCode $purgeCode)),分族计数见上面工具的输出与 $purgeReportPath" +
            "(同一 rid 重跑接着做:已改名的键不在源上,新出现的源键进下一轮 pass)。`n" + $forwardHint)
    }
    Write-Host "  purge finished" -ForegroundColor Green
}
Write-Host ""

# ── Step 5b: nothing written after the cut ───────────────────────
Write-Host "── Step 5b: verify nothing was written after the Step 5 cut ──" -ForegroundColor Cyan
$cutText = if ($null -ne $kafkaCut) { ConvertTo-KafkaCutText -Cut $kafkaCut } else { '<Step 5 cut p=offset,...>' }
# 判定参数与 4a / 4b 同一份;-purge-report 让 verify 逐个复核 purge 处理过的玩家(回档复核 [6])。
$verifyArgs = & $newVerifyArgs $cutText
Write-Host "  $cutDescribeText (every partition CURRENT == LOG-END == cut)"
Write-Host "  $(Format-CommandLine -Program $toolShown -Arguments $verifyArgs)"
if ($Apply) {
    try {
        $recheck = Read-ZoneGroupCut -Bootstrap $KafkaBootstrap -Topic $KafkaTopic -ZoneGroup $KafkaGroup -Partitions $kafkaPartitionCount -ExpectedCut $kafkaCut
    }
    catch {
        throw ("k8s-zone-rollback 在 Step 5b 读不到 zone 组 $KafkaGroup 的位点:$($_.Exception.Message)`n" + $forwardHint)
    }
    if (@($recheck.Problems).Count -gt 0) {
        throw ("k8s-zone-rollback 在 Step 5b 拒绝继续(Step 6 之前):Step 5 之后又有消息写入 $KafkaTopic,查清生产者后单独重跑 Step 5(新的切点),再用同一 rid 重跑 4b" +
            "(上一轮已隔离的键不受影响,重新出现的同名源键进下一轮 pass)与 5b:`n  - " +
            (@($recheck.Problems) -join "`n  - ") + "`n" + $forwardHint)
    }
    $verifyCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $verifyArgs
    if ($verifyCode -ne 0) {
        throw ("k8s-zone-rollback 在 Step 5b 拒绝继续(Step 6 之前):回档工具 verify 以退出码 $verifyCode 结束($(Get-DiscardToolExitMeaning -ExitCode $verifyCode)):" +
            "本 topic 的重试族或补投桶又出现了条目、仍有存活租约、purge 处理过的玩家又长出了父快照 / 子表缓存 / sync_digest / 资产提交证据 / 屏障," +
            "重跑盘点又归出了 T 之后的状态,或游标超过了切点(offset 回退过,须换世代)。查清残留写者后用同一 rid 重跑 4b 与 5b。`n" + $forwardHint)
    }
    Write-Host "  verified: $KafkaTopic is still at the cut and the discard tool found nothing left behind" -ForegroundColor Green
}
Write-Host ""

# ── Step 6: k8s-zone-up ──────────────────────────────────────────
Write-Host "── Step 6: k8s-zone-up ──" -ForegroundColor Cyan
# $upArgs 在第 0 步之前已组装好,静态预检用的就是这一份;dry-run 打出来的这一行就是 -Apply 时真正传下去的参数。
Write-Host ("  dev_tools.ps1 " + (($upArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { "-$($_.Key) $($_.Value)" }) -join ' '))
if ($Apply) {
    & "$ScriptDir/dev_tools.ps1" @upArgs
    if ($LASTEXITCODE -ne 0) {
        throw ("k8s-zone-up failed(退出码 $LASTEXITCODE):MySQL 已回档、Redis 已处置、Kafka 已拨到切点,zone 还没起来。" +
            "修好原因后单独重跑 Step 6:$step6Text")
    }
    Write-Host "  zone $ZoneName up" -ForegroundColor Green
}
Write-Host ""

# ── Step 7: verification checklist ───────────────────────────────
# 键名按现行实现写:父快照 PlayerAllData:{<pid>}(玩家域 hash tag,savebarrier.ParentKey),子表缓存 <表名>:<pid>(go/db buildCacheKey = {MsgType}:{key});
# 以前这里写的 player:{<pid>}:player_database 并不存在。
Write-Host "── Step 7: post-rollback verification ──" -ForegroundColor Cyan
Write-Host "  Run these manually:"
$verifyKubeArgs = "$(if ($KubeContext) { " --context $KubeContext" })$(if ($KubeConfig) { " --kubeconfig $KubeConfig" })"
$redisShown = if ($RedisHost) { "-h $RedisHost -p $RedisPort -n $RedisDB" } else { "-h <go/db shared redis> -p $RedisPort -n $RedisDB" }
$redisAuthHint = if ($RedisPasswordEnv) { " (authenticate with the password in env $RedisPasswordEnv; never on the command line)" } else { '' }
Write-Host "    1. kubectl get pods,sts,svc,ingress -n ${NamespacePrefix}-${ZoneName}$verifyKubeArgs (for external zones, confirm gate-<i> nodePort / Ingress host match their values before rollback)"
Write-Host "    2. Robot smoke login: pwsh -File tools/scripts/dev_tools.ps1 -Command dev-robot-zones -Zones $ZoneId"
Write-Host "    3. Before its first login, a player listed as processed in $purgeReportPath has no caches${redisAuthHint}:"
Write-Host "       redis-cli $redisShown EXISTS 'PlayerAllData:{<pid>}' 'player_database:<pid>' 'player_database_1:<pid>'   (expect 0)"
Write-Host "       then log that player in once (cold read from zone_${ZoneId}_db must succeed); if inventory.json lists blocked players, check one of them is refused with a clear message."
Write-Host "    4. Metrics on the zone's db: db_ordering_guard_total{outcome=`"stale`"} must stay 0 after the rollback (a nonzero value means offsets went backwards; bump the topic generation),"
Write-Host "       db_ordering_guard_total{outcome=`"apply_no_cursor`"} must not spike, db_save_outbox_total{result=`"published`"} should stay near 0."
# 第 5 条不再手拼 kafka:retry* 键名(以前只列了共享 ready 与死信,漏了 :p<n> 与 processing,回档复核 [11]):本 topic 的全部重试族、
# 补投桶与游标由 Step 5b 的 verify 按 go/db 的键名函数核对过。zone 拉起之后 go/db 已在用这些键(租约、重试),不能再拿 verify 复核。
Write-Host "    5. Retry state of ${KafkaTopic}: Step 5b verify checked every retry family (shared ready, :p<n>, processing, instances, dead letter), the 64 replay buckets and the cursors before zone-up;"
Write-Host "       keep $(Join-Path $reportDir 'verify.json'). Do not re-run verify now: the live go/db owns those keys again (leases, retries)."
Write-Host "    6. Keep on record: quarantine keys rollback:quarantine:${ZoneId}:${rollbackId}:* (never re-inject them into the live queues) and the report directory $reportDir."
Write-Host "    7. Tail scene/gate/db logs for replay errors (first 5 minutes); drop the renamed pre-rollback zone_${ZoneId}_db only after these checks pass."
Write-Host ""
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host " Rollback complete. " -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
