#requires -Version 7
<#
.SYNOPSIS
    Disaster-recovery rollback for a single zone — orchestrates
    `k8s-zone-down` → MySQL PITR (manual) → inventory
    → discard Redis state written after TargetTime (zone_rollback_discard) → `k8s-zone-up`.

.DESCRIPTION
    This script is the executable form of the SOP in
    docs/design/zone_data_rollback.md §3 "整 Zone 灾难恢复级回档".

    回档语义(负责人 2026-10-17 裁决:丢弃 TargetTime 之后的写):回档 = 回到 TargetTime 时刻库里已经提交的状态。
    存盘已是 scene 经 gRPC 直接调 go/db 提交 player_store,没有在途队列,回退点就是 T;T 之后的写一律丢弃,代价已接受。所以:
      - Step 4 丢弃 Redis 里 T 之后派生的状态:受影响玩家的同步摘要与资产提交证据按 PITR 之后的库处置;
      - 归属计数器 owner_epoch 保留不动。
    回档里不再有 FLUSHDB:归属与位置键、交接标记、路由映射与账号目录同在一个 Redis DB,FLUSHDB 会清掉所有 zone 的这些键,
    等于事故(-ConfirmFlushSharedRedis 已改为入口拒绝)。

    It will NOT do the destructive steps without -Apply. Default is dry-run:dry-run 照样执行入口校验,其余每一步只打印将要执行的命令
    与作用范围,不调 kubectl / 回档工具,不读写 Redis / MySQL。

    读 MySQL、写 Redis 的动作全部在独立工具 zone_rollback_discard 里(go/db/cmd/zone_rollback_discard,与 go/db 共用键名函数;
    -RollbackDiscardTool 给与线上 go/db 同一提交构建的可执行文件,-Apply 必填)。**该工具尚未落地**,契约见下方「zone_rollback_discard 契约」;
    本脚本只编排 kubectl 与工具调用,不自己拼任何 Redis 键名。

    What it does (in order):
      入口校验(dry-run 也执行,任何改动之前):
        E1 -TargetTime 必须是 UTC 的 yyyy-MM-ddTHH:mm:ss[.fff]Z,且早于当前 UTC;
           -PitrRestoredTo 同一格式,给了就必须等于 -TargetTime。
        E2 -ConfirmFlushSharedRedis 与 -RedisPassword 传了即拒绝:回档不再 FLUSHDB;口令只经 -RedisPasswordEnv 指名的环境变量,不上命令行。
        E3 -Apply 时必须显式给 -RedisHost(核心 Redis,不按服务 yaml 缺省的 127.0.0.1 补)、-MySqlDsnEnv(该环境变量非空)
           与 -RollbackDiscardTool(存在的文件);-SkipMySqlPause 必须配 -PitrRestoredTo。dry-run 缺这些时打印占位符。
        E4 不可变镜像、Ingress 参数成对;
           生成 rollback-id(z<id>-<T 紧凑格式>-<当前 UTC 紧凑格式>,-RollbackId 可显式给),报告目录 run/logs/zone-rollback/<rid>/。
        待负责人裁决的路径一律在入口拒绝:-AcceptBlockedPlayers(裁决 #4)。
      0. 预检(只读,先于停服;dry-run 只预告,不执行),a / b / c 都过了才进 Step 1:
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
         c. 部署事实与回档工具预检(2026-10-18):一次只读 kubectl 取 zone namespace 里的 go-svc-db-config、go-svc-login-config
            与 db / login Deployment,只折出非机密的拓扑(核心 Redis 的实例与 DB,取 login 的 Node.RedisClient;go/db 不连 Redis。
            ZoneId、镜像),口令一概不读。逐项核对:ZoneId;-RedisDB 等于核心 Redis 的 DB。
            存为 <报告目录>/deploy-facts.json;同一 rid 重跑而 namespace 已删时复用它,
            namespace 还在时与存档逐项相同才放行。随后两项身份核对(2026-10-18,回档复核 [4][5]):
              - Redis 实例:用同一组 -KubeContext / -KubeConfig kubectl exec 进 infra namespace 的 redis-0(infra namespace 取 dev_tools.ps1 的默认值,
                Step 6 就用它;口令取 Pod 自己的 REDIS_PASSWORD,不上本机命令行),对 deploy-facts 里核心 Redis 的地址读 INFO server 的 run_id,
                作为每次工具调用的 -expect-run-id;工具每个模式先读 -redis 的 run_id,不一致即以 3 拒绝 ——
                -RedisHost(可以是端口转发)与 -KubeContext 由此绑到线上用的同一个实例;
              - 工具版本:<tool> -version 报出的构建提交必须以线上 db 镜像的 tag(git sha)开头,且两边都不是脏树构建;db 镜像以 digest 部署、
                tag 不是 git sha 时无从核对,同样拒绝。
            然后 <tool> -mode preflight。任一不过即在停服之前拒绝。
      1. `k8s-zone-down -ZoneName <zone>` — stop all writers(删掉整个 zone namespace),随后用 kubectl 确认 namespace 已不存在
         (dev_tools 的 Remove-Zone 允许失败、不报错,不能靠它的退出码判断停服成功)。
      1b. 停服后核对(2026-10-18,只读,先于 PITR;顺序是「先证明没有写者,再取基线」,回档复核 [1]):
          <tool> -mode stopped:本 zone 归属玩家没有人仍在别的 zone 在线(陈旧 location 不算,handoff 另等 300s);
          都过了之后,最后一步写行指纹基线 <报告目录>/fingerprint-baseline.json(phase=stopped)。这时 namespace 已确认删除,zone_<id>_db 不再有写入;
          以前基线在停服前取,go/db 一直在落存盘,只要 zone 上有玩家活动,停服时的行就与基线不同 —— 没做 PITR 或连错库也会被 4a 当成「有变化」放行。
          不过即拒绝:zone 已停服,尚未 PITR,Redis 尚未改动;错误文本给出用同一 rid 重跑或放弃(按 Step 6 的同一份参数 zone-up)的做法。
      2. (已删除,2026-10-15)原来是 zone-down 之后等 Kafka LAG 排空。存盘已不经 Kafka,没有积压可等。编号 3–7 不变。
      3. ⚠️ MySQL PITR is NOT automated — print runbook reference and pause
         for confirmation. Operator runs PITR manually, hits ENTER to continue.
         只回退 zone_<id>_db(及工具预检报告里列出的落点库),不回退全局库(data_service 的 player_name / 路由表、guild、
         mail、收件箱、trade);先恢复到影子库再替换,回档前的原库改名保留到 Step 6 冒烟通过。
         -Apply 带 -SkipMySqlPause 时必须同时给 -PitrRestoredTo(= TargetTime),声明写进报告目录。
      4a. 盘点(只读):<tool> -mode inventory,与 Step 1b 的停服基线比较行指纹(没有任何玩家变化 = 没做 PITR / 恢复到影子库没换回 /
          连错库,拒绝)。拒绝(在线、落点在别处、合服标记、读失败)或盘点出 blocked 玩家时停在这里:
          Redis 还没动,中止路径 = 换回原库 → 不跑 4b / Step 5 → 按 Step 6 的同一份参数 k8s-zone-up。
          4a 通过之后、4b 之前再复核一次停服状态(只读,回档复核 [8]):namespace 仍不存在(没被 GitOps / CD 重建)。
          PITR 往往要几个小时,这段时间里写者可能回来;不过即按上面的中止路径拒绝(Redis 还没动)。
      4b. 丢弃 Redis 里 T 之后的状态:<tool> -mode purge -confirm,判定参数与 4a 同一份(-target-time、-fingerprint-baseline,给了
          -AcknowledgeNoPostTWrites 时连同 -acknowledge-no-post-t-writes;回档复核 [2]),工具内部先原样重跑 4a 的检查,任一不过什么都不写。
          从这一步起没有干净的中止路径,只能向前:失败时按打印的单步命令用同一 rid 重跑。写进 Redis 的残留状态由 Step 5 的 verify 逐个复核
          已处理玩家的键并重跑盘点归类(回档复核 [6]),都在 Step 6 之前被发现。同一 rid 续跑的规则见契约。
      5. 复核:<tool> -mode verify(只读)。
      6. `k8s-zone-up -ZoneName <zone> -ZoneId <id>` — restart zone
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
      公共参数(Get-DiscardToolArgs 拼,顺序固定):-deploy-facts <报告目录>/deploy-facts.json -zone <id>
        -redis <host:port> -redis-db <n> [-redis-password-env <环境变量名>] -expect-run-id <40 位 hex> -mysql-dsn-env <环境变量名>
        -rollback-id <rid> -report-dir <报告目录> -mode <preflight|stopped|inventory|purge|verify>。
        口令与 DSN 只从这两个环境变量读,不上命令行、不进报告。
        键名一律由 go/shared 的导出函数生成(shared/playersync、shared/assetop、shared/handofffence、shared/ownerepoch 等),工具里不另拼键名字面量。
        玩家域(2026-10-31,docs/design/redis-player-store-cluster.md §5.2):sync_digest、handoff_fence、location 这些键都在玩家域。线上玩家域是 Redis Cluster 时
        本脚本多传 -player-redis <种子,逗号分隔> -player-redis-cluster true [-player-redis-password-env <环境变量名>],工具用 shared/playerredis 构造客户端:
        全键盘点走 ScanEachMaster(逐主节点,拓扑变化返回 ErrTopologyChanged,按 3 拒绝,不得据此断言键不存在),每玩家 Lua 只碰同一 {player_id} 的键。
        -redis 这时只指控制域(scene_nodes:zone:<zone>:load 等),没传 -player-redis 时玩家域与控制域同一个实例。集群不能经端口转发访问(MOVED 指向 Service IP),
        工具要在集群网络里跑。
        Redis 实例身份(回档复核 [4]):每个模式开始时先读 -redis 的 INFO server,cluster_enabled 必须为 0,run_id 必须等于 -expect-run-id,
        否则以 3 拒绝、什么都不写。-expect-run-id 由本脚本第 0 步 c 经 kubectl exec 对线上核心 Redis 的地址读出,
        把 -RedisHost(可以是端口转发)与 -KubeContext 指向的集群绑到同一个实例;deploy-facts 只说明线上用哪个地址,证明不了 -redis 就是它。
      构建身份(回档复核 [5]):只带 -version 时打印恰好两行 commit=<40 位小写 hex> 与 dirty=<true|false> 后以 0 退出,commit 由构建时
        -ldflags "-X main.buildCommit=…" 嵌入。本脚本第 0 步 c 据此与线上 db 镜像的 tag 比对(Test-DiscardToolMatchesDbImage);
        preflight 另以同一规则复核 -deploy-facts 的 DbImage,不一致以 3 拒绝(键名与 Lua 契约只对同一提交的 go/db 成立)。
      退出码:0 通过;1 出错(可能只做了一部分,修好后同一 rid 重跑,幂等);3 拒绝(什么都没写);
        4 目标看起来没在用(只 preflight:本 zone 归属玩家在 Redis 里一个协调键都没有,-allow-empty-target 放行);
        6 有 blocked 玩家(inventory 即拒绝)。其它值一律按出错处理。
      判定参数(inventory / purge / verify 共用,本脚本只拼一份 $judgeArgs,回档复核 [2]):-target-time <T> -fingerprint-baseline
        <报告目录>/fingerprint-baseline.json [-acknowledge-no-post-t-writes]。三个模式对同一组参数做同一份判定,不各自取舍开关。
      续跑规则(purge、verify 与整脚本重跑同一条,回档复核 [3]):purge 幂等,已处理玩家的键已不在,同一 rid 重跑接着做;
        purge.json 累计全部轮次,已处理玩家名单是 verify 的输入。
      preflight(停服前,只读):Redis 连得上且确实是线上的核心 Redis(run_id 见上);全局库 zone_merged_into
        里 src 或 dst = 本 zone 的一律拒绝(T 之后合入本 zone 的玩家会被当成 T 之后建角而清掉,负责人裁决 #6 之前
        fail-closed);zone_<id>_db 连得上、SELECT DATABASE() 正确、player_store 表可读;只靠 Redis 就能判出的拒绝
        (落点在别处、他区在线)在这里先判一次。preflight 不取行指纹基线(停服前 go/db 仍在落库,
        那时的基线没有意义,回档复核 [1])。zone 维护围栏(负责人裁决 #1)落地之前必须以 3 拒绝。
      stopped(停服后,-handoff-wait / -fingerprint-baseline):scene_nodes:zone:<zone>:load 为空;
        player:{<id>}:location 要区分陈旧与他区在线(判据随工具落地一起定),
        只看 home = 本 zone 的成员,他区在线即拒绝;handoff 另按 -handoff-wait(≥ HandoffTTL 300s)等待。以上都过了之后,最后一步把受影响玩家
        player_store 的行指纹基线写进 -fingerprint-baseline,文件里记 phase=stopped、rid、zone、取样时刻(Redis TIME 与 UTC)。本脚本保证调用它时
        namespace 已确认删除。同一 rid 已有 phase=stopped 的基线时不覆盖
        (PITR 之后整脚本重跑仍要与停服时的基线比较);已有文件但 phase 不是 stopped、rid / zone 不同或格式不对,以 3 拒绝。
      inventory(PITR 之后,只读,判定参数 + -report):读 -fingerprint-baseline(缺失、phase 不是 stopped 即 3),重算行指纹,
        与停服基线相比没有任何玩家变化即拒绝(没做 PITR、恢复到影子库没换回或连错库),除非 -acknowledge-no-post-t-writes;
        归类表穷举,兜底项 unclassified 按 blocked 处理;post_t_candidate / home_elsewhere 占比超过阈值即拒绝。
      purge(判定参数 + -confirm / -report,回档复核 [7]):先原样重跑 inventory 的全部前置检查与判定(同一组判定参数),任一不过即 3、什么都不写。
        删除范围只限本 zone:每玩家一段 Lua(白名单:home = 本 zone 且落点 = zone_<id>_db,KEYS 带映射与落点并原子复核)删除
        sync_digest 与资产提交证据;绝不写 player:{id}:handoff_fence(只有交接资格 handoff_fence),绝不碰 owner_epoch、映射、account:*;
        任何一段 Lua 出错即停止,以 1 退出;停止时与正常结束时都把计数(玩家 processed / skipped)打印到 stdout 并写进 -report(purge.json)。
      verify(判定参数 + -purge-report / -report,只读,回档复核 [6]):
          - 逐个复核 -purge-report(全部轮次累计)列为已处理的玩家:sync_digest、资产提交证据仍不存在;
          - 用同一组判定参数对本 zone 归属玩家重跑 inventory 的归类:出现任何仍需丢弃的 T 之后状态或 blocked 玩家。
        任一项不过即 3,本脚本在 Step 6 之前拒绝。
      工具自己的测试至少覆盖:某段 Lua 失败(退出 1、计数正确、同 rid 重跑结果一致);
        -expect-run-id 不一致;-version 的输出格式;stopped 已有非 stopped 阶段的基线。

.EXAMPLE
    # Preview what would happen
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -NodeImage ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha>

.EXAMPLE
    # Run the destructive flow(-RedisHost 是核心 Redis;口令与 DSN 只放在环境变量里,参数只给变量名)
    $env:MMORPG_ROLLBACK_MYSQL_DSN = '<zone_101_db 的 DSN>'
    pwsh -File tools/scripts/k8s_zone_rollback.ps1 `
        -ZoneName test1 -ZoneId 101 `
        -TargetTime "2026-05-15T14:23:00Z" `
        -NodeImage ghcr.io/luyuancpp/mmorpg-node:<回滚目标 sha> `
        -RedisHost 127.0.0.1 -RedisPort 6379 -RedisDB 0 -RedisPasswordEnv MMORPG_ROLLBACK_REDIS_PASSWORD `
        -MySqlDsnEnv MMORPG_ROLLBACK_MYSQL_DSN `
        -RollbackDiscardTool <与线上 go/db 同一提交构建的 zone_rollback_discard> `
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
        -RollbackDiscardTool <zone_rollback_discard> `
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
        -RollbackDiscardTool <zone_rollback_discard> `
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

    # 核心 Redis(login 的 Node.RedisClient;K8s 即 infra redis 的 DB 0,本机即唯一的那个 Redis)。go/db 不连 Redis。
    # 回档工具在这里盘点并丢弃 T 之后的状态,不做 FLUSHDB。-Apply 必填,不按 db.yaml 缺省的 127.0.0.1 补;
    # 可以是端口转发地址:实例身份由第 0 步 c 经 kubectl exec 读出线上核心 Redis 的 run_id,工具每个模式按 -expect-run-id 核对
    # (不一致即拒,回档复核 [4]);-RedisDB 由第 0 步 c 与线上核心 Redis 的 DB 核对。
    [string]$RedisHost = "",
    [string]$RedisPort = "6379",
    # Redis 口令所在的环境变量名(不是口令本身):原样交给工具的 -redis-password-env,本脚本不读口令值、不上命令行、不打印。
    # 给了就要求该变量非空;Redis 没开认证时留空。
    [string]$RedisPasswordEnv = "",
    # 已废弃(2026-10-18,回档丢弃语义对抗复核 [12]):口令经命令行会进进程列表与 shell 历史。传了即在入口拒绝,改用 -RedisPasswordEnv。
    [string]$RedisPassword = "",
    [int]$RedisDB = 0,
    # 玩家域 Redis Cluster(docs/design/redis-player-store-cluster.md)。线上 login ConfigMap 的 PlayerRedis.Cluster 为 true 时,第 0 步要求这几个都给,否则在停服之前拒绝:
    #   -PlayerRedisMasters / -PlayerRedisReplicasPerMaster:转给 Step 6 的 zone-up。不粘滞,漏传的话 Step 6 会把 zone 渲染回单实例(staging / prod 被静态预检拒绝)。
    #   -PlayerRedisSeeds:种子地址(host:port,逗号分隔),交给回档工具的 -player-redis。工具要在集群网络里跑,端口转发不行(MOVED 指向 Service IP)。
    #   -PlayerRedisPasswordEnv:口令所在的环境变量名,同 -RedisPasswordEnv;集群没开认证时留空。
    [ValidateRange(0, 512)]
    [int]$PlayerRedisMasters = 0,
    [ValidateRange(0, 2)]
    [int]$PlayerRedisReplicasPerMaster = 0,
    [string]$PlayerRedisSeeds = "",
    [string]$PlayerRedisPasswordEnv = "",

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
    # 那时 zone 已停服、MySQL 已回档、Step 4b 已处置 Redis,没有干净的中止路径,只能在失败提示给出的 Step 6 单步命令里补上
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
    # -Apply 下必须同时给 -PitrRestoredTo:跳过暂停而 PITR 其实没做(或恢复到了影子库没换回来)时,工具会把 T 之后的库当成回档后的库处置
    # (回档丢弃语义对抗复核 [5])。
    [switch]$SkipMySqlPause,
    # 操作者声明的 PITR 恢复时刻,格式同 -TargetTime,给了就必须等于 -TargetTime;写进报告目录的运行记录。
    [string]$PitrRestoredTo = "",

    # 已废弃(2026-10-18):回档不再 FLUSHDB(归属与位置键、交接标记、路由映射与账号目录同在一个 Redis DB,FLUSHDB 等于事故)。
    # 传了即在入口拒绝;整套环境重建走 AGENTS.md §6.2。
    [switch]$ConfirmFlushSharedRedis,

    # 回档工具:go/db/cmd/zone_rollback_discard 的预编译可执行文件,必须与线上 go/db 同一提交构建(回档丢弃语义对抗复核 [9]:
    # 不现场 go build —— 本地工作区未必是线上镜像的提交,键名与 Lua 契约可能对不上,运维机上也未必有 Go 工具链)。
    # 同一提交由第 0 步 c 机械核对(回档复核 [5]):<tool> -version 的构建提交必须以线上 db 镜像的 tag 开头、两边都不是脏树构建,否则停服之前拒绝。
    # -Apply 必填;工具契约见文件头。工具尚未落地,在它落地之前 -Apply 停在入口校验。
    [string]$RollbackDiscardTool = "",
    # zone_<id>_db 的 MySQL DSN 所在的环境变量名(不是 DSN 本身)。-Apply 必填且该变量必须非空;原样交给工具的 -mysql-dsn-env。
    [string]$MySqlDsnEnv = "",
    # 本次回档的 id:进报告目录 run/logs/zone-rollback/<rid>/,工具按它续跑。
    # 留空 = z<ZoneId>-<TargetTime 紧凑格式>-<当前 UTC 紧凑格式>;整脚本重跑或单步重跑必须传上一次打印的同一个值。只允许 [A-Za-z0-9_-]。
    [string]$RollbackId = "",
    # 带 blocked 玩家(torn / owner_behind / post_t_candidate / legacy_owned / unclassified)继续回档:这些玩家的同步摘要、交接资格与资产证据是保留还是删除、
    # T 之后建的角色能否以空骨架继续用,待负责人裁决(#4)。传了即在入口拒绝;4a 盘点出 blocked 时回档停在 Step 4b 之前。
    [ValidateRange(0, 2147483647)]
    [int]$AcceptBlockedPlayers = 0,
    # 交给工具 preflight 的 -allow-empty-target:目标 Redis 里本 zone 归属玩家一个协调键都没有(工具退出码 4)时仍放行。
    # 只用于从未落过库的新 zone;平时它说明 -RedisHost / -RedisDB 指错了实例。
    [switch]$AllowEmptyTarget,
    # 交给工具的 -acknowledge-no-post-t-writes:Step 1b 停服基线与 PITR 之后的行指纹没有任何玩家变化(工具判为没做 PITR 或连错库)时仍放行。
    # 只用于 T 之后确实没有任何存盘的 zone。它是判定参数的一部分:inventory、purge(重跑 inventory 的检查)与 verify 一并带上(回档复核 [2])。
    [switch]$AcknowledgeNoPostTWrites,

    [switch]$Apply
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir "..\..")

. (Join-Path $ScriptDir "lib\release_common.ps1")
# 只为 Get-GateAddressHint 取 Get-ClientEntryContract(gate 生成器内部 env 名与端口模式取值的唯一来源);
# 该库只定义函数、不读脚本级变量、不设 StrictMode,dot-source 无副作用。
. (Join-Path $ScriptDir "lib\k8s_client_entry.ps1")

<#
.SYNOPSIS
    读部署脚本(k8s_deploy.ps1;第 0 步 c 另读 dev_tools.ps1 的 -InfraNamespace)参数块里若干参数的默认值,
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
        那时已停服、已回档、Redis 已处置。与默认值折算出的模式一致才放行,判不出同样拒绝。
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
                "(以 warn 运行的 zone 落回 enforce,节点池 nofile 硬限仍不够时 gate / scene 在 Step 6 拒启,那时已停服、已回档、Redis 已处置," +
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
    Step 3 的 PITR 命令、工具 inventory 与回档报告都以 TargetTime 为准。只收带 Z 的 UTC 写法:空格分隔、时区偏移(+08:00)这类写法在 PITR 时最容易换算错,一律拒绝。
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
        4 { return '目标看起来没在用(本 zone 归属玩家一个协调键都没有;-RedisHost / -RedisDB 多半指错了实例)' }
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
    第 0 步 c 的 kubectl 参数(只读):zone namespace 里 go/db 与 login 的 ConfigMap、db 与 login 的 Deployment。dry-run 打印的就是这一份。

.DESCRIPTION
    ConfigMap 名与数据键同 k8s_deploy.ps1 的 $GoSvcCatalogue(db → go-svc-db-config / db.yaml,login → go-svc-login-config /
    login.yaml),Deployment 与容器名同 deploy/k8s/manifests/go-svc/db.yaml、login.yaml;那边改名时契约测试会报红。
    --ignore-not-found:namespace 已被删(同一 rid 重跑)时无输出、退出码 0,由调用方改用报告目录里的存档。
#>
function Get-DeploySnapshotKubectlArgs {
    param([Parameter(Mandatory = $true)][string]$Namespace)

    return , @('get', 'configmap/go-svc-db-config', 'configmap/go-svc-login-config', 'deployment/db', 'deployment/login',
        '-n', $Namespace, '-o', 'json', '--ignore-not-found', '--request-timeout=15s')
}

<#
.SYNOPSIS
    把第 0 步 c 那次 kubectl 的 JSON 输出折成非机密的部署事实 [pscustomobject](纯函数);输出为空(namespace 已删)返回 $null,
    必需项读不出即 throw。

.DESCRIPTION
    只取白名单里的键,口令(Redis 口令、MySQL DSN 等)一律不读,结果可以落盘进报告目录、交给回档工具(-deploy-facts)。
      - go-svc-db-config 的 data.db.yaml:ZoneId;
      - go-svc-login-config 的 data.login.yaml:Node.RedisClient.Host / DB(DB 缺省 0),这是核心 Redis。go/db 不连 Redis,映射在全局库;
        PlayerRedis.Cluster 与第一个种子(缺省 false / 空):线上玩家域是不是 Redis Cluster,第 0 步据此要求 -PlayerRedis* 参数;
      - db Deployment 里名为 db 的容器镜像(必需,第 0 步 c 据它核对回档工具的构建提交);login Deployment 里名为 login 的容器镜像(只记录,可缺)。
    有输出但两份 ConfigMap 缺任一、缺数据键、缺 db 镜像,都 throw:namespace 还在却读不全,说明不是本仓生成的形状,不猜。
    Schema 4(2026-10-30):存盘不再经 Kafka、go/db 没有 Fencing、OwnerEpochRedis 与 Redis 配置,映射不再有 Redis 副本,对应字段删除;
    更早的存档与现读必然不同,同一 rid 重跑时按「部署事实与存档不同」拒绝。
    Schema 5(2026-10-31):多了 PlayerRedis。
#>
function ConvertFrom-ZoneDeploySnapshot {
    param(
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$Json,
        [Parameter(Mandatory = $true)][string]$Namespace
    )

    if ([string]::IsNullOrWhiteSpace($Json)) { return $null }
    try {
        $document = $Json | ConvertFrom-Json -ErrorAction Stop
    }
    catch {
        throw "第 0 步 c 的 kubectl 输出不是合法的 JSON(namespace $Namespace):$($_.Exception.Message)"
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
    $db = (& $readConfigMapFile 'go-svc-db-config' 'db.yaml').Scalars
    $login = (& $readConfigMapFile 'go-svc-login-config' 'login.yaml').Scalars
    $redisHost = & $scalar $login 'Node.RedisClient.Host'
    $redis = if ($null -eq $redisHost) { $null } else { [pscustomobject][ordered]@{ Hosts = $redisHost; DB = (& $scalar $login 'Node.RedisClient.DB' '0') } }
    $playerRedis = [pscustomobject][ordered]@{ Cluster = (& $scalar $login 'PlayerRedis.Cluster' 'false'); FirstSeed = (& $scalar $login 'PlayerRedis.Addrs[0]' '') }
    return [pscustomobject][ordered]@{
        Schema     = 5
        Namespace  = $Namespace
        ZoneId     = (& $scalar $db 'ZoneId')
        Redis      = $redis
        PlayerRedis = $playerRedis
        DbImage    = (& $readImage 'db' -Required)
        LoginImage = (& $readImage 'login')
    }
}

<#
.SYNOPSIS
    部署事实与本次参数的逐项核对(纯函数),返回问题列表 [string[]];空表示放行。

.DESCRIPTION
      - ZoneId 必须等于 -ZoneId;
      - 核心 Redis 的地址必须是单个 host:port(工具的每玩家 Lua 跨键原子执行,不支持 Cluster),DB 必须等于 -RedisDB;
        主机名不在这里比对:-RedisHost 可以是端口转发地址,实例身份由 run_id 核对(Read-CoreRedisRunId → 工具的 -expect-run-id,回档复核 [4]);
      - 线上玩家域是 Redis Cluster 时必须给种子与主节点数:回档工具要丢弃的键都在那边,Step 6 的 zone-up 也要它(namespace 删掉之后读不到线上,不粘滞)。
#>
function Get-DeployFactsProblems {
    param(
        [Parameter(Mandatory = $true)]$Facts,
        [Parameter(Mandatory = $true)][int]$ZoneId,
        [Parameter(Mandatory = $true)][int]$RedisDB,
        [AllowEmptyString()][string]$PlayerRedisSeeds = '',
        [int]$PlayerRedisMasters = 0
    )

    $problems = [System.Collections.Generic.List[string]]::new()
    if ([string]$Facts.ZoneId -ne "$ZoneId") {
        $problems.Add("线上 go-svc-db-config 的 ZoneId 是 '$($Facts.ZoneId)',不是 -ZoneId $ZoneId(namespace 指错了 zone?)")
    }
    $redis = $Facts.Redis
    if ($null -eq $redis -or [string]::IsNullOrWhiteSpace([string]$redis.Hosts)) {
        $problems.Add('线上 go-svc-login-config 读不到 Node.RedisClient.Host')
    }
    else {
        if ([string]$redis.Hosts -match '[,\s]') {
            $problems.Add("线上核心 Redis 的地址是 '$($redis.Hosts)'(多个地址):回档工具的每玩家 Lua 跨键原子执行,只支持单实例")
        }
        if ([string]$redis.DB -ne "$RedisDB") {
            $problems.Add("线上核心 Redis 的 DB 是 '$($redis.DB)',不是 -RedisDB ${RedisDB}:工具会在别的 DB 里盘点与丢弃")
        }
    }
    $playerRedisProperty = $Facts.PSObject.Properties['PlayerRedis']
    if ($null -ne $playerRedisProperty -and $null -ne $playerRedisProperty.Value -and [string]$playerRedisProperty.Value.Cluster -eq 'true') {
        if ([string]::IsNullOrWhiteSpace($PlayerRedisSeeds)) {
            $problems.Add("线上玩家域是 Redis Cluster(种子 $($playerRedisProperty.Value.FirstSeed)),必须给 -PlayerRedisSeeds:回档工具要丢弃的键都在那边,只连 -RedisHost 会在控制域上盘点出空,然后什么都不丢")
        }
        if ($PlayerRedisMasters -lt 3) {
            $problems.Add('线上玩家域是 Redis Cluster,必须给 -PlayerRedisMasters(与 infra-up 同值):Step 6 的 zone-up 由它决定 player_redis,而 Step 1 删掉 namespace 之后读不到线上')
        }
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
    第 0 步 c(只在 -Apply 下、Step 1 之前):读部署事实、与存档和本次参数核对、落盘 deploy-facts.json;任何问题都在停服之前抛出。返回事实。

.DESCRIPTION
    namespace 还在:按 Get-DeploySnapshotKubectlArgs 现读;报告目录里已有同一 rid 的存档时必须与现读逐项相同(不同说明换了部署或 rid 用错了)。
    namespace 已删(同一 rid 在 Step 1 之后重跑):kubectl 无输出,改用存档;没有存档就拒绝 —— 读不到不等于没有。
    kubectl 失败(不可达、Forbidden)一律拒绝。
#>
function Resolve-ZoneDeployFacts {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$FactsPath,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = ''
    )

    $refusal = "k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:"
    $result = Invoke-ClientEntryKubectl -KubectlArgs (Get-DeploySnapshotKubectlArgs -Namespace $Namespace) `
        -KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
    if ($result.ExitCode -ne 0) {
        throw ($refusal + "读不到 namespace $Namespace 的部署事实(kubectl 退出码 $($result.ExitCode)):$($result.Stderr)。读不到不等于没有,无法核对 Redis 拓扑。")
    }
    try {
        $live = ConvertFrom-ZoneDeploySnapshot -Json ([string]$result.Stdout) -Namespace $Namespace
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
                "namespace 删掉之后无从核对 Redis 拓扑;Step 1 之后重跑必须带上一次打印的 -RollbackId。")
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
    第 0 步 c 读核心 Redis 实例 run_id 的 kubectl 参数(纯函数:只拼参数,不执行;回档复核 [4])。

.DESCRIPTION
    exec 进 infra namespace 的 redis-0(redis 容器),对 deploy-facts 里核心 Redis 的地址执行 redis-cli INFO server ——
    走的是服务自己连的那个地址(Service 名),HA 形态下由代理落到当前主库,与服务看到的是同一个实例。
    口令取 Pod 自己的 REDIS_PASSWORD(Secret redis-auth),经 REDISCLI_AUTH 交给 redis-cli,不经本机命令行、不进日志;为空时不设
    (写法同 k8s_deploy.ps1 Test-RedisLiveMaxClients)。地址要拼进 sh -c,只收 <[A-Za-z0-9.-]+>:<端口>(单实例、无 IPv6),不合格即 throw。
    返回值交给 Invoke-ClientEntryKubectl 执行(带同一组 -KubeContext / -KubeConfig)。
#>
function Get-CoreRedisRunIdExecArgs {
    param(
        [Parameter(Mandatory = $true)][string]$InfraNamespace,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$RedisHosts
    )

    $address = [regex]::Match($RedisHosts.Trim(), '^(?<host>[A-Za-z0-9.-]+):(?<port>\d{1,5})$')
    if (-not $address.Success) {
        throw "核心 Redis 的地址 '$RedisHosts' 不是单个 <主机名>:<端口>,不能拼进 redis-0 里的 sh -c 读 run_id。"
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
    第 0 步 c(只在 -Apply 下、Step 1 之前):按 Get-CoreRedisRunIdExecArgs 读出核心 Redis 实例的 run_id;exec 失败或判不出即 throw。
#>
function Read-CoreRedisRunId {
    param(
        [Parameter(Mandatory = $true)][string]$InfraNamespace,
        [Parameter(Mandatory = $true)][AllowEmptyString()][string]$RedisHosts,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = ''
    )

    $refusal = "k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:"
    try {
        $execArgs = Get-CoreRedisRunIdExecArgs -InfraNamespace $InfraNamespace -RedisHosts $RedisHosts
        $result = Invoke-ClientEntryKubectl -KubectlArgs $execArgs -KubeContext $KubeContext -KubeConfig $KubeConfig -CaptureOutput -AllowFailure
        if ($result.ExitCode -ne 0) {
            throw "kubectl exec 以退出码 $($result.ExitCode) 结束:$($result.Stderr)"
        }
        return (ConvertFrom-RedisInfoRunId -Text ([string]$result.Stdout))
    }
    catch {
        throw ($refusal + "读不到核心 Redis($RedisHosts)的 run_id(kubectl exec 进 $InfraNamespace 的 redis-0 执行 INFO server)," +
            "无法证明 -RedisHost 与线上是同一个实例:另一个环境里同 ZoneId 的 Redis 同样有这批键,工具会在那边丢弃、目标环境原封不动。" +
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
    确认 zone namespace 已不存在;仍在(Terminating / 被重建)或读不到都 throw。Step 1 之后与 Step 4b 之前各调一次。

.DESCRIPTION
    dev_tools.ps1 k8s-zone-down 的 Remove-Zone 以 -AllowFailure 执行 kubectl delete namespace:删除失败也不报错、退出码照样为 0,
    不能拿它判断「本 zone 的写者都停了」(回档丢弃语义对抗复核 [0])。Step 4b 之前再调一次(回档复核 [8]):中间隔着人工 PITR,
    namespace 可能被 GitOps / CD 重建。-Refusal / -Hint 是这一阶段的拒绝前缀与后续做法,缺省为 Step 1 之后那一次的文本。
#>
function Assert-ZoneNamespaceGone {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [AllowEmptyString()][string]$KubeContext = '',
        [AllowEmptyString()][string]$KubeConfig = '',
        [string]$Refusal = 'k8s-zone-rollback 在 Step 1 之后拒绝继续:k8s-zone-down 已执行,',
        [string]$Hint = '尚未 PITR,Redis 尚未改动;等 namespace 删净(kubectl get namespace 返回 NotFound)后用同一 -RollbackId 重跑。'
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

# ── 入口校验(dry-run 也执行,任何改动之前) ──
# E2:回档不再 FLUSHDB;口令不再经命令行。传了就拒绝,不静默忽略(AGENTS §11.3)。
if ($ConfirmFlushSharedRedis) {
    throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:回档已不做 FLUSHDB,-ConfirmFlushSharedRedis 不再有效。" +
        "归属与位置键、交接标记、路由映射与账号目录同在一个 Redis DB,FLUSHDB 会清掉所有 zone 的这些键。" +
        "回档改为由 zone_rollback_discard 只丢弃 TargetTime 之后的状态;整套环境重建走 AGENTS.md §6.2。去掉该参数重跑。")
}
if (-not [string]::IsNullOrEmpty($RedisPassword)) {
    throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:-RedisPassword 已废弃,口令经命令行会进进程列表与 shell 历史。" +
        "把口令放进一个环境变量,改传 -RedisPasswordEnv <变量名>(回档工具自己读该变量,本脚本不读、不打印)。")
}
# 待负责人裁决的路径一律 fail-closed(拒绝并说明),不在这里自行实现。
if ($AcceptBlockedPlayers -gt 0) {
    throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:-AcceptBlockedPlayers 待负责人裁决(#4)。blocked 玩家(torn / owner_behind / post_t_candidate / " +
        "legacy_owned / unclassified,以及 T 之后建角、合入或迁入本 zone 的玩家)的同步摘要、交接资格与资产证据是保留还是删除,T 之后建的角色" +
        "能否以空骨架继续使用,都没有定论。在裁决之前,Step 4a 盘点出 blocked 玩家时回档停在 Step 4b 之前(Redis 没动),按中止路径恢复。")
}
foreach ($envParam in @(@{ Name = 'RedisPasswordEnv'; Value = $RedisPasswordEnv }, @{ Name = 'PlayerRedisPasswordEnv'; Value = $PlayerRedisPasswordEnv }, @{ Name = 'MySqlDsnEnv'; Value = $MySqlDsnEnv })) {
    if (-not [string]::IsNullOrEmpty($envParam.Value) -and $envParam.Value -cnotmatch '^[A-Za-z_][A-Za-z0-9_]*$') {
        # 不回显给的值:误把口令或 DSN 本身传进来时,它不能再进错误文本与日志。
        throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:-$($envParam.Name) 不是合法的环境变量名(只收 [A-Za-z_][A-Za-z0-9_]*,给的值不回显)。这里要的是变量名,不是口令或 DSN 本身。"
    }
}
# E1:TargetTime 的格式与时间。Step 3 的 PITR 命令与回档报告都以它为准。
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

# E3:-Apply 时破坏性步骤要用到的目标必须显式给出。不按 db.yaml 缺省的 127.0.0.1 补:本机缺省值在 K8s 上指向的是运维机自己,
# 回档会打到错误的目标;口令与 DSN 只给环境变量名。dry-run 不要求,缺的项在命令里打印占位符。
if ($Apply) {
    $missing = @()
    if ([string]::IsNullOrWhiteSpace($RedisHost)) {
        $missing += '-RedisHost <核心 Redis:K8s 即 infra redis 的 DB 0,可以是端口转发地址>'
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
    if (-not [string]::IsNullOrEmpty($PlayerRedisPasswordEnv) -and [string]::IsNullOrEmpty([System.Environment]::GetEnvironmentVariable($PlayerRedisPasswordEnv))) {
        $missing += "环境变量 $PlayerRedisPasswordEnv 为空:-PlayerRedisPasswordEnv 指名的变量必须有值;玩家域集群没开认证时不要传 -PlayerRedisPasswordEnv"
    }
    if ([string]::IsNullOrWhiteSpace($RollbackDiscardTool)) {
        $missing += '-RollbackDiscardTool <与线上 go/db 同一提交构建的 zone_rollback_discard 可执行文件>(工具尚未落地时 -Apply 到此为止)'
    }
    elseif (-not (Test-Path -LiteralPath $RollbackDiscardTool -PathType Leaf)) {
        $missing += "-RollbackDiscardTool $RollbackDiscardTool 不是存在的文件"
    }
    if ($SkipMySqlPause -and [string]::IsNullOrWhiteSpace($PitrRestoredTo)) {
        $missing += '-PitrRestoredTo <PITR 实际恢复到的时刻,须等于 -TargetTime>:带 -SkipMySqlPause 跳过暂停时必须声明 PITR 已做到哪个时刻'
    }
    if ($missing.Count -gt 0) {
        throw ("k8s-zone-rollback 拒绝执行,尚未产生任何改动:-Apply 缺少以下 $($missing.Count) 项:`n  - " + ($missing -join "`n  - ") +
            "`n经包装入口调用而它不透传所需参数时,直接调用 tools/scripts/k8s_zone_rollback.ps1。")
    }
}

# E4:rollback-id 与报告目录。rid 进路径,只收 [A-Za-z0-9_-];整脚本或单步重跑必须带同一个 rid。
if ([string]::IsNullOrWhiteSpace($RollbackId)) {
    $rollbackId = New-RollbackId -ZoneId $ZoneId -TargetUtc $targetUtc -NowUtc $nowUtc
}
elseif ($RollbackId -cnotmatch '^[A-Za-z0-9_-]{1,96}$') {
    throw "k8s-zone-rollback 拒绝执行,尚未产生任何改动:-RollbackId '$RollbackId' 不合法:它会拼进报告目录路径并交给回档工具,只收 1–96 个 [A-Za-z0-9_-]。"
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
Write-Host "  redis       : $(if ($RedisHost) { "$RedisHost`:$RedisPort/db$RedisDB (core Redis)" } else { '(not set; -Apply requires -RedisHost: the core Redis)' })$(if ($RedisPasswordEnv) { " password from env $RedisPasswordEnv" })"
Write-Host "  mysql       : zone_${ZoneId}_db, DSN from env $(if ($MySqlDsnEnv) { $MySqlDsnEnv } else { '<not set; -Apply requires -MySqlDsnEnv>' })"
Write-Host "  discard tool: $(if ($RollbackDiscardTool) { $RollbackDiscardTool } else { '(not set; -Apply requires -RollbackDiscardTool: a prebuilt zone_rollback_discard from the same commit as the live go/db)' })"
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
# 玩家域 Redis Cluster:非 0 才转发;线上是集群时第 0 步已要求必给(Get-DeployFactsProblems)。
if ($PlayerRedisMasters -gt 0) { $upArgs.PlayerRedisMasters = $PlayerRedisMasters }
if ($PlayerRedisReplicasPerMaster -gt 0) { $upArgs.PlayerRedisReplicasPerMaster = $PlayerRedisReplicasPerMaster }
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
# 首次运行在第 0 步全部通过、Step 1 之前才写(见第 0 步 c 之后):第 0 步后面任一项拒绝时不留存档,修正参数后同一 rid 重跑不受它约束。
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
# 第 0 步 c 读 run_id 的 infra namespace 取 Step 6 实际会用的值:Step 6 经 dev_tools.ps1 k8s-zone-up 部署、不传 -InfraNamespace,
# 即 dev_tools.ps1 的参数默认值(按语法树读,不在本脚本抄一份;读不出即拒绝)。
$devToolsPath = Join-Path $ScriptDir "dev_tools.ps1"
$step6InfraNamespace = (Get-DeployParamDefaults -DeployScriptPath $devToolsPath -Names @('InfraNamespace'))['InfraNamespace']
$step6InfraText = if ([string]::IsNullOrWhiteSpace($step6InfraNamespace)) { '<unreadable>' } else { $step6InfraNamespace }
# 参数函数整体返回数组，先接收再拼接；统一格式化含空白的参数，保证预告命令可直接核对。
$kubectlDisplayBaseArgs = Get-ClientEntryKubectlBaseArgs -KubeContext $KubeContext -KubeConfig $KubeConfig

# 第 0 步 c(上半):部署事实、核心 Redis 的实例身份、回档工具的构建提交(只读,先于 Step 1)。部署对象只取非机密的拓扑与世代
# (Get-DeploySnapshotKubectlArgs / ConvertFrom-ZoneDeploySnapshot),与本次参数、本机 db.yaml 核对后落盘;namespace 删掉之后(同一 rid 重跑)复用存档。
# run_id 要进每次工具调用的公共参数(-expect-run-id),所以先于下面组装工具参数;工具 preflight 在组装之后(第 0 步 c 下半)。
$toolShown = if ($RollbackDiscardTool) { $RollbackDiscardTool } else { '<zone_rollback_discard>' }
$snapshotCommandArgs = Get-DeploySnapshotKubectlArgs -Namespace $zoneNamespace
$snapshotShown = Format-CommandLine -Program 'kubectl' -Arguments ($kubectlDisplayBaseArgs + $snapshotCommandArgs)
Write-Host "  Deploy facts (read-only, secrets are never read): $snapshotShown -> $factsPath"
$redisIdentityArgs = $kubectlDisplayBaseArgs + @('exec', '-n', $step6InfraText, 'pod/redis-0', '-c', 'redis', '--')
$redisIdentityShown = Format-CommandLine -Program 'kubectl' -Arguments $redisIdentityArgs
Write-Host ("  Redis identity (read-only): $redisIdentityShown sh -c '<REDISCLI_AUTH from the pod REDIS_PASSWORD>; " +
    "redis-cli --raw -h <core Redis host> -p <port> INFO server' -> run_id, passed to every discard tool call as -expect-run-id")
Write-Host "  Discard tool build: $toolShown -version (must print commit=<40 hex> and dirty=false; the commit must start with the tag of the live db image)"
$redisRunId = $null
$toolVersion = $null
if ($Apply) {
    if ([string]::IsNullOrWhiteSpace($step6InfraNamespace)) {
        throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:读不到 $devToolsPath 的 -InfraNamespace 默认值(不是字面常量或文件无法解析)," +
            "不知道该去哪个 infra namespace 的 redis-0 读核心 Redis 的 run_id。")
    }
    New-Item -ItemType Directory -Path $reportDir -Force | Out-Null
    $deployFacts = Resolve-ZoneDeployFacts -Namespace $zoneNamespace -FactsPath $factsPath -KubeContext $KubeContext -KubeConfig $KubeConfig
    Write-Host ("  live: core Redis=$($deployFacts.Redis.Hosts)/db$($deployFacts.Redis.DB) db image=$($deployFacts.DbImage);" +
        " the tool connects to $($RedisHost):$($RedisPort)/db$RedisDB (a port-forward is fine; the run_id below binds it to this instance)")
    $factsProblems = Get-DeployFactsProblems -Facts $deployFacts -ZoneId $ZoneId -RedisDB $RedisDB -PlayerRedisSeeds $PlayerRedisSeeds -PlayerRedisMasters $PlayerRedisMasters
    if ($factsProblems.Count -gt 0) {
        throw ("k8s-zone-rollback 在停服之前拒绝执行,尚未产生任何改动:部署事实($factsPath)与本次参数不一致,$($factsProblems.Count) 项:`n  - " + ($factsProblems -join "`n  - "))
    }
    # Redis 实例身份(回档复核 [4]):以前「目标就是线上的库」只看 Redis 里有没有本 zone 的键,另一个环境里同 ZoneId 的 Redis 同样能过。
    $redisRunId = Read-CoreRedisRunId -InfraNamespace $step6InfraNamespace -RedisHosts ([string]$deployFacts.Redis.Hosts) -KubeContext $KubeContext -KubeConfig $KubeConfig
    Write-Host "  core Redis $($deployFacts.Redis.Hosts): run_id=$redisRunId; every discard tool call requires the instance behind $($RedisHost):$($RedisPort) to report it (-expect-run-id)" -ForegroundColor Green
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
    'redis'              = $(if ($RedisHost) { "${RedisHost}:${RedisPort}" } else { '<core redis host:port>' })
    'redis-db'           = "$RedisDB"
    'redis-password-env' = $RedisPasswordEnv
    'expect-run-id'      = $(if ($redisRunId) { $redisRunId } else { '<redis run_id read in step 0 c>' })
    # 空值不输出(Get-DiscardToolArgs):玩家域没上集群时公共参数与以前逐字相同。
    'player-redis'              = $PlayerRedisSeeds
    'player-redis-cluster'      = $(if ($PlayerRedisSeeds) { 'true' } else { '' })
    'player-redis-password-env' = $PlayerRedisPasswordEnv
    'mysql-dsn-env'      = $(if ($MySqlDsnEnv) { $MySqlDsnEnv } else { '<mysql dsn env var>' })
    'rollback-id'        = $rollbackId
    'report-dir'         = $reportDir
}
# 判定参数(回档复核 [2]):inventory / purge / verify 共用这一份。purge 原样重跑 inventory 的判定,以前它不带 -target-time 与
# -acknowledge-no-post-t-writes,带了 -AcknowledgeNoPostTWrites 的路径上 inventory 能过、purge 必然退出 3。
$judgeArgs = @('-target-time', $TargetTime, '-fingerprint-baseline', $baselinePath) + @(if ($AcknowledgeNoPostTWrites) { '-acknowledge-no-post-t-writes' })
# handoff 等待不低于 go/shared/ownerepoch 的 HandoffTTL 300s(对抗复核 [0][12])。
# 行指纹基线由 stopped 在停服之后写(回档复核 [1]),preflight 不再取基线。
$toolArgs = @{
    preflight = Get-DiscardToolArgs -Mode preflight -Common $toolCommon -Extra @(if ($AllowEmptyTarget) { '-allow-empty-target' })
    stopped   = Get-DiscardToolArgs -Mode stopped -Common $toolCommon -Extra @('-handoff-wait', '300s', '-fingerprint-baseline', $baselinePath)
    inventory = Get-DiscardToolArgs -Mode inventory -Common $toolCommon -Extra ($judgeArgs + @('-report', (Join-Path $reportDir 'inventory.json')))
    purge     = Get-DiscardToolArgs -Mode purge -Common $toolCommon -Extra ($judgeArgs + @('-confirm', '-report', $purgeReportPath))
    verify    = Get-DiscardToolArgs -Mode verify -Common $toolCommon -Extra ($judgeArgs + @('-purge-report', $purgeReportPath, '-report', (Join-Path $reportDir 'verify.json')))
}
# 失败时打印的单步命令先组装好。
$step6Text = "pwsh -File tools/scripts/dev_tools.ps1 " + (($upArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { if ($_.Value -is [bool]) { "-$($_.Key)" } else { "-$($_.Key) $($_.Value)" } }) -join ' ')
$entryArgsText = if ($null -ne $entryDecision) { Format-ZoneEntryDecisionArgs -Decision $entryDecision } else { '' }
# nofile 模式不进入口决定:显式给了就提示照传;留空时第 0 步 a 按存档里核对过的集群现状核对。
$nofileRerunText = if ($NodeOpenFileLimitMode) { ";-NodeOpenFileLimitMode 照传 $NodeOpenFileLimitMode(本次显式给值没有核对集群,namespace 已删时重跑留空会被判为读不到)" } else { '' }
$rerunWholeHint = if ($entryArgsText) {
    ("排除原因后用同一个 rid 整脚本重跑:加 -RollbackId $rollbackId,入口参数照传 $entryArgsText(namespace 已删时第 0 步 a 以报告目录里的 " +
        "entry-state.json 为准,显式值必须与它相同)$nofileRerunText;第 0 步 c 复用报告目录里的 deploy-facts.json。")
}
else {
    ("排除原因后用同一个 rid 整脚本重跑:加 -RollbackId $rollbackId;namespace 已删,须显式传 -GateRouterMode、-ClientEntryMode、-NodeOpenFileLimitMode," +
        "并对 Ingress 表态(-GatewayIngressHost … 或 -ConfirmNoGatewayIngress)、对第三方登录表态(照传 -Login* 并加 -ConfirmLoginAuthProviderChange)," +
        "第 0 步 c 复用报告目录里的 deploy-facts.json。")
}
$abortBeforePurge = ("中止路径(Step 4b 之前都可用):换回 PITR 之前保留的原库 zone_${ZoneId}_db → 不跑 4b 与 Step 5 → " +
    "按 Step 6 的同一份参数 k8s-zone-up:$step6Text。")
$forwardHint = ("Step 4b 已开始处置 Redis,不能再换回原库,只能向前。修好原因后用同一 rid 按顺序执行:" +
    "`n  - Step 4b:$(Format-CommandLine -Program $toolShown -Arguments $toolArgs.purge)" +
    "`n  - Step 5:$(Format-CommandLine -Program $toolShown -Arguments $toolArgs.verify)" +
    "`n  - Step 6:$step6Text")

# 第 0 步 c(下半):回档工具 preflight,通过之后写入口决定存档(回档复核 [10]),然后才进 Step 1。
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
        redis            = "$($toolCommon['redis'])/db$RedisDB"
        redisPasswordEnv = $RedisPasswordEnv
        mySqlDsnEnv      = $MySqlDsnEnv
        discardTool      = $RollbackDiscardTool
        discardToolCommit = $toolVersion.Commit
        redisRunId       = $redisRunId
        acknowledgeNoPostTWrites = [bool]$AcknowledgeNoPostTWrites
    }
    Add-Content -LiteralPath (Join-Path $reportDir 'run-log.jsonl') -Encoding utf8 -Value ($runRecord | ConvertTo-Json -Compress)
    $preflightCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $toolArgs.preflight
    if ($preflightCode -ne 0) {
        $emptyHint = if ($preflightCode -eq 4) { "确认 -RedisHost / -RedisDB 指向线上实际在用的核心 Redis;只有从未落过库的新 zone 才加 -AllowEmptyTarget。" } else { "原因见上面工具的输出与 $reportDir(run_id 不一致说明 -RedisHost 连到的不是 go/db 用的那个实例)。" }
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
# hashtable splatting(理由见 $upArgs);-KubeContext / -KubeConfig 非空才透传,与第 0 步预检、Step 6 落在同一集群。
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
    if ($LASTEXITCODE -ne 0) { throw "k8s-zone-down failed(退出码 $LASTEXITCODE):尚未 PITR,Redis 尚未改动。$rerunWholeHint" }
    Assert-ZoneNamespaceGone -Namespace $zoneNamespace -KubeContext $KubeContext -KubeConfig $KubeConfig
    Write-Host "  zone $ZoneName stopped" -ForegroundColor Green
}
Write-Host ""

# Step 2(zone-down 之后等 LAG 排空)已删除(2026-10-15):存盘已不经 Kafka,没有积压可等。编号 3–7 不变。

# ── Step 1b: post-stop checks (read-only, before PITR) ──────────
# 停服之后、PITR 之前再确认「本 zone 不会再有写入」:这里拒绝时 zone 只是停了,数据一点没动,放弃回档直接 zone-up 即可;
# 拖到 PITR 之后才发现,MySQL 已回档、Redis 还是 T 之后的,没有干净的退路(回档丢弃语义对抗复核 [6])。
Write-Host "── Step 1b: post-stop checks (read-only, before PITR) ──" -ForegroundColor Cyan
# 顺序是「先证明没有写者,再取基线」(回档复核 [1]):工具 stopped 确认没有人在别处在线,最后一步才写停服行指纹基线 ——
# 那之后 zone_<id>_db 不再有写入,4a 拿 PITR 之后的指纹与它比较才有意义。
Write-Host "  $(Format-CommandLine -Program $toolShown -Arguments $toolArgs.stopped)"
Write-Host "  (players online elsewhere; writes the stopped-phase fingerprint baseline $baselinePath last)"
if ($Apply) {
    $stopRefusal = "k8s-zone-rollback 在 Step 1b 拒绝继续:zone 已停服,尚未 PITR,尚未改动 Redis。"
    $stopHint = ("`n$rerunWholeHint`n放弃回档:数据都没动,按 Step 6 的同一份参数 k8s-zone-up 即可恢复:$step6Text。")
    $stoppedCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $toolArgs.stopped
    if ($stoppedCode -ne 0) {
        throw ($stopRefusal + "回档工具 stopped 以退出码 $stoppedCode 结束($(Get-DiscardToolExitMeaning -ExitCode $stoppedCode)):" +
            "多半是本 zone 归属的玩家仍在别的 zone 在线,或同一 rid 已有不是停服阶段取的行指纹基线;详见上面工具的输出。" + $stopHint)
    }
    Write-Host "  stopped-phase fingerprint baseline: $baselinePath" -ForegroundColor Green
} else {
    Write-Host "  (not executed during dry-run; -Apply refuses before the PITR prompt when the check fails)" -ForegroundColor Yellow
}
Write-Host ""

# ── Step 3: MySQL PITR (manual) ──────────────────────────────────
# 只回退本 zone 的库:data_service 的全局库(player_name、路由表)一起回退会让名字登记和路由倒回去;guild / mail / 收件箱 / trade
# 各有自己的库,不随 zone 回退(跨服务状态不回退是已接受的代价)。旧文案里的库名 game 早已不存在。
Write-Host "── Step 3: MySQL Point-in-Time Recovery ──" -ForegroundColor Cyan
Write-Host "  ⚠️ This step is NOT automated." -ForegroundColor Yellow
Write-Host "  See docs/ops/mysql-backup-pitr-runbook.md §4.1 for the procedure."
Write-Host "  Target time: $TargetTime (UTC)"
Write-Host "  Restore ONLY zone_${ZoneId}_db, plus any placement-store databases listed by the discard tool in $reportDir."
Write-Host "  Do NOT roll back global databases: data_service (player_name / route tables), guild, mail, player inbox, trade."
Write-Host "  Restore into a shadow database first, then swap it in; KEEP the pre-rollback zone_${ZoneId}_db (rename it) until the Step 6 smoke test passes."
Write-Host ""
Write-Host "  Typical command (verify before running; mysqlbinlog reads --stop-datetime in the local time zone of its host, so run it with TZ=UTC):"
Write-Host "    mysqlbinlog --database=zone_${ZoneId}_db --stop-datetime=`"$($targetUtc.ToString('yyyy-MM-dd HH:mm:ss', [System.Globalization.CultureInfo]::InvariantCulture))`" \\"
Write-Host "      /backup/binlog/mysql-bin.* | mysql -h shadow-host -u root -p zone_${ZoneId}_db_restore"
Write-Host "  Abort path until Step 4b runs: swap the original zone_${ZoneId}_db back, skip Step 4b and Step 5, then run zone-up with the Step 6 arguments:"
Write-Host "    $step6Text"
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
# PITR 之后、任何写之前盘点。这里拒绝时 Redis 还没动,还能按中止路径换回原库;
# blocked 玩家怎么处置待负责人裁决 #4,裁决之前一律停在这里。盘点的行指纹与 Step 1b 的停服基线比较(回档复核 [1])。
# 盘点通过之后、4b 之前再复核一次停服状态(回档复核 [8]):中间隔着人工 PITR,namespace 可能被重建。
Write-Host "── Step 4a: inventory of post-TargetTime Redis state (read-only) ──" -ForegroundColor Cyan
Write-Host "  $(Format-CommandLine -Program $toolShown -Arguments $toolArgs.inventory)"
Write-Host "  then, before Step 4b (read-only): $namespaceShown must still print nothing"
if ($Apply) {
    $inventoryRefusal = "k8s-zone-rollback 在 Step 4a 拒绝继续:zone 已停服、MySQL 已 PITR,Redis 尚未改动。"
    $inventoryCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $toolArgs.inventory
    if ($inventoryCode -eq 6) {
        throw ($inventoryRefusal + "盘点出 blocked 玩家(torn / owner_behind / post_t_candidate / legacy_owned / unclassified,名单见 $(Join-Path $reportDir 'inventory.json'))。" +
            "带 blocked 玩家继续回档待负责人裁决(#4),裁决之前不放行。`n$abortBeforePurge")
    }
    if ($inventoryCode -ne 0) {
        throw ($inventoryRefusal + "回档工具 inventory 以退出码 $inventoryCode 结束($(Get-DiscardToolExitMeaning -ExitCode $inventoryCode)),详见上面工具的输出与 $reportDir。" +
            "`n$abortBeforePurge`nPITR 确已做完、排除原因后,也可以带 -RollbackId $rollbackId -SkipMySqlPause -PitrRestoredTo $TargetTime 整脚本重跑。")
    }
    Write-Host "  inventory passed: no blocked players" -ForegroundColor Green
    # Step 4b 之前的停服复核(只读):4b 之后就不能再换回原库,动 Redis 之前最后一次确认「没有写者」。
    $recheckRefusal = "k8s-zone-rollback 在 Step 4b 之前拒绝继续:zone 已停服、MySQL 已 PITR,Redis 尚未改动。"
    $recheckHint = ("这段时间里本 zone 的写者可能已经回来,正把存盘写进刚回档的库。查清原因(GitOps / CD 重建、手工拉起、别处的 db 实例)并停掉它们;" +
        "库已被写过时只能按中止路径恢复。`n$abortBeforePurge")
    Assert-ZoneNamespaceGone -Namespace $zoneNamespace -KubeContext $KubeContext -KubeConfig $KubeConfig -Refusal $recheckRefusal -Hint $recheckHint
    Write-Host "  still stopped: namespace gone" -ForegroundColor Green
}
Write-Host ""

# ── Step 4b: discard post-TargetTime Redis state ─────────────────
# 工具内部先原样重跑 4a 的检查与判定(同一组判定参数 $judgeArgs,回档复核 [2]),任一不过什么都不写;同一 rid 续跑按契约的续跑规则(回档复核 [3])。
Write-Host "── Step 4b: discard post-TargetTime Redis state ──" -ForegroundColor Cyan
Write-Host "  $(Format-CommandLine -Program $toolShown -Arguments $toolArgs.purge)"
if ($Apply) {
    $purgeCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $toolArgs.purge
    if ($purgeCode -ne 0) {
        throw ("k8s-zone-rollback 在 Step 4b 失败:回档工具 purge 以退出码 $purgeCode 结束($(Get-DiscardToolExitMeaning -ExitCode $purgeCode)),计数见上面工具的输出与 $purgeReportPath" +
            "(同一 rid 重跑接着做,幂等)。`n" + $forwardHint)
    }
    Write-Host "  purge finished" -ForegroundColor Green
}
Write-Host ""

# ── Step 5: nothing left behind ──────────────────────────────────
Write-Host "── Step 5: verify nothing was left behind ──" -ForegroundColor Cyan
# 判定参数与 4a / 4b 同一份;-purge-report 让 verify 逐个复核 purge 处理过的玩家(回档复核 [6])。
Write-Host "  $(Format-CommandLine -Program $toolShown -Arguments $toolArgs.verify)"
if ($Apply) {
    $verifyCode = Invoke-DiscardTool -ToolPath $RollbackDiscardTool -Arguments $toolArgs.verify
    if ($verifyCode -ne 0) {
        throw ("k8s-zone-rollback 在 Step 5 拒绝继续(Step 6 之前):回档工具 verify 以退出码 $verifyCode 结束($(Get-DiscardToolExitMeaning -ExitCode $verifyCode)):" +
            "purge 处理过的玩家又长出了同步摘要 / 资产提交证据,或重跑盘点又归出了 T 之后的状态。查清残留写者后用同一 rid 重跑 4b 与 Step 5。`n" + $forwardHint)
    }
    Write-Host "  verified: the discard tool found nothing left behind" -ForegroundColor Green
}
Write-Host ""

# ── Step 6: k8s-zone-up ──────────────────────────────────────────
Write-Host "── Step 6: k8s-zone-up ──" -ForegroundColor Cyan
# $upArgs 在第 0 步之前已组装好,静态预检用的就是这一份;dry-run 打出来的这一行就是 -Apply 时真正传下去的参数。
Write-Host ("  dev_tools.ps1 " + (($upArgs.GetEnumerator() | Sort-Object Name | ForEach-Object { "-$($_.Key) $($_.Value)" }) -join ' '))
if ($Apply) {
    & "$ScriptDir/dev_tools.ps1" @upArgs
    if ($LASTEXITCODE -ne 0) {
        throw ("k8s-zone-up failed(退出码 $LASTEXITCODE):MySQL 已回档、Redis 已处置,zone 还没起来。" +
            "修好原因后单独重跑 Step 6:$step6Text")
    }
    Write-Host "  zone $ZoneName up" -ForegroundColor Green
}
Write-Host ""

# ── Step 7: verification checklist ───────────────────────────────
# 键名按现行实现写:同步摘要 player:{<pid>}:sync_digest(shared/playersync.KeyFormat);玩家存档只在 player_store,Redis 里没有。
Write-Host "── Step 7: post-rollback verification ──" -ForegroundColor Cyan
Write-Host "  Run these manually:"
$verifyKubeArgs = "$(if ($KubeContext) { " --context $KubeContext" })$(if ($KubeConfig) { " --kubeconfig $KubeConfig" })"
$redisShown = if ($RedisHost) { "-h $RedisHost -p $RedisPort -n $RedisDB" } else { "-h <core redis> -p $RedisPort -n $RedisDB" }
$redisAuthHint = if ($RedisPasswordEnv) { " (authenticate with the password in env $RedisPasswordEnv; never on the command line)" } else { '' }
Write-Host "    1. kubectl get pods,sts,svc,ingress -n ${NamespacePrefix}-${ZoneName}$verifyKubeArgs (for external zones, confirm gate-<i> nodePort / Ingress host match their values before rollback)"
Write-Host "    2. Robot smoke login: pwsh -File tools/scripts/dev_tools.ps1 -Command dev-robot-zones -Zones $ZoneId"
Write-Host "    3. Before its first login, a player listed as processed in $purgeReportPath has no sync digest${redisAuthHint}:"
Write-Host "       redis-cli $redisShown EXISTS 'player:{<pid>}:sync_digest'   (expect 0)"
Write-Host "       then log that player in once (LoadAndClaim from player_store in zone_${ZoneId}_db must succeed); if inventory.json lists blocked players, check one of them is refused with a clear message."
Write-Host "    4. Metrics on the zone's db: db_player_store_requests_total{op,result} must show saves and claims succeeding, with no sustained failure result after the rollback."
Write-Host "    5. Keep on record: the report directory $reportDir, including $(Join-Path $reportDir 'verify.json')."
Write-Host "    6. Tail scene/gate/db logs for save errors (first 5 minutes); drop the renamed pre-rollback zone_${ZoneId}_db only after these checks pass."
Write-Host ""
Write-Host "============================================================" -ForegroundColor Cyan
Write-Host " Rollback complete. " -ForegroundColor Cyan
Write-Host "============================================================" -ForegroundColor Cyan
