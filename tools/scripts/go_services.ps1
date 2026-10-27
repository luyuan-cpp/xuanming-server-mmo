<#
.SYNOPSIS
    Unified Go micro-service launcher for local Windows development.

.DESCRIPTION
    Starts one, several, or all implemented go-zero services in the go/ directory.
    Each service runs as a separate background process; a stop command terminates them.

.PARAMETER Command
    start   – build & launch selected services (default: all)
    stop    – gracefully stop running services launched by this script
              (Ctrl+C to each instance's console, force-kill the process tree on timeout)
    status  – show which services are currently running
    list    – print the available service catalogue
    build   – compile native binaries for selected services -> bin\go_services\

.PARAMETER Services
    Comma-separated service names to start (e.g. "login,db").
    Defaults to all implemented services.

.PARAMETER ForceStop
    stop 时跳过 Ctrl+C,直接用 taskkill /T /F 强杀进程树(旧行为,对服务而言等同崩溃)。
    默认不加:先向每个实例的控制台投递 Ctrl+C 让服务走自己的优雅停机,超时再强杀进程树。

.PARAMETER StopTimeoutSeconds
    stop 时 Ctrl+C 之后等待实例自行退出的秒数。>0 时覆盖所有服务;
    默认 0 = 按服务取值(目录键 StopTimeoutSeconds,db 为 35,未写的服务为 25)。

.PARAMETER LocalMySQL
    本机 MySQL 兼容档。仅为 db 派生运行配置并关闭 RequireTiDB；只接受回环 SQL 地址。
    不改源配置、dual 围栏或其他服务，默认关闭。

.EXAMPLE
    # Start every implemented Go service
    pwsh -File tools/scripts/go_services.ps1 -Command start

    # Start only login and db
    pwsh -File tools/scripts/go_services.ps1 -Command start -Services login,db

    # Check what's running
    pwsh -File tools/scripts/go_services.ps1 -Command status

    # Stop all services launched by this script
    pwsh -File tools/scripts/go_services.ps1 -Command stop

    # 只停 db,Ctrl+C 后最多等 60 秒;或者跳过优雅停直接强杀
    pwsh -File tools/scripts/go_services.ps1 -Command stop -Services db -StopTimeoutSeconds 60
    pwsh -File tools/scripts/go_services.ps1 -Command stop -Services db -ForceStop
#>
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet("start", "start-exe", "stop", "stop-and-report", "signal-only", "status", "list", "build")]
    [string]$Command,

    [string[]]$Services = @(),

    # Per-service instance counts for local multi-open. Accepts either:
    #   * hashtable (in-session use):   -Counts @{ login = 2; scene_manager = 2 }
    #   * string list (pwsh -File):     -Counts login=2,scene_manager=2
    # Service name keys must match the catalogue. Count <= 1 means single instance
    # (uses the original etc/<svc>.yaml). Count > 1 generates derived yaml files
    # under run/etc/go_services/<svc>_<i>.yaml with ListenOn port = base + (i-1).
    # 'db' can run several instances too since 2026-10 (retry receipts are owned per instance, see go/db/internal/kafka/retry_ownership.go).
    [object]$Counts = @{},

    # Base port offset between instances. The N-th instance (1-based) listens on
    # base + (i-1) * PortStride. Default 1 keeps ports densely packed.
    [int]$PortStride = 1,

    # Disable tier-staged startup (parallel launch like before). Pass when you
    # know dependencies are already satisfied or you want maximum parallelism.
    [switch]$NoTier,

    # Per-tier readiness budget (seconds) when staged startup is enabled. Soft
    # gate: probe TCP LISTEN on each instance port; warn-and-continue on timeout.
    [int]$TierReadySeconds = 10,

    [switch]$LocalMySQL,

    # Local multi-zone launch. When > 0:
    #   * Instance keys are prefixed with 'z<Zone>_' so a second zone can coexist
    #     in the same PID file (e.g. z1_login_1, z2_login_1).
    #   * A derived yaml is materialized at run/etc/go_services/z<Zone>_<svc>[_<i>].yaml
    #     with `ZoneId: N` rewritten and `ListenOn` port shifted by (Zone-1)*ZonePortShift
    #     to avoid TCP port collisions.
    #   * `db` is launched once per zone by default since each
    #     zone owns its own Kafka topic + MySQL DB; pass -Counts db=N to run several per zone
    #     (they join the zone's consumer group and split its partitions).
    # Zone=0 (default) keeps legacy single-zone behaviour.
    [int]$Zone = 0,

    # Port offset between zones (Zone N port = base + (Zone-1) * ZonePortShift).
    # Default 1000 keeps zones cleanly separated even with PortStride=1.
    [int]$ZonePortShift = 1000,

    # stop:跳过 Ctrl+C 直接强杀进程树(旧行为)。见 Invoke-Stop 头注释。
    [switch]$ForceStop,

    # stop:Ctrl+C 后等待优雅退出的秒数,>0 时覆盖所有服务;0 = 按目录键 StopTimeoutSeconds(缺省 25)。
    [int]$StopTimeoutSeconds = 0,
    [string]$MaintenancePlanPath = "",
    [string]$StopReportPath = "",
    [int]$SignalProcessId = 0,
    [string]$MaintenanceDrainPlan = "",
    [string]$MaintenanceDrainDir = "",
    [string]$MaintenanceDrainSha256 = ""
)

$ErrorActionPreference = "Stop"
if ($LocalMySQL -and $Command -notin @('start', 'start-exe')) {
    throw '-LocalMySQL 只用于本地 start/start-exe，不用于构建或部署。'
}

$ScriptDir  = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot   = Resolve-Path (Join-Path $ScriptDir "..\..")
$GoRoot     = Join-Path $RepoRoot "go"
# Runtime artifacts (logs, pid files) live under run/, kept separate from
# build outputs in bin/. See docs/ops/log-management.md.
$RunDir     = Join-Path $RepoRoot "run"
$PidFile    = Join-Path $RunDir   "pids\go_services.pid.json"
$LogDir     = Join-Path $RunDir   "logs\go_services"
# Legacy fallback (older pid files written under bin/ before the run/ split).
$LegacyPidFile = Join-Path $RepoRoot "bin\go_services.pid.json"
# Built Go binaries still ship under bin/go_services\ alongside the C++ exes
# because that directory is the runtime working dir for local launches.
$GoBinDir   = Join-Path $RepoRoot "bin\go_services"

# 资产通道(docs/design/guild-phase2/04-asset-channel.md §4.32)的本机开发密钥来源。
# guild 用 MMORPG_ASSET_OP_SECRET_GUILD 签名、scene 用同名变量验签,值必须一致,否则每一次资产 RPC
# 都回 27008(AssetAuthFailed)。密钥值不写进仓库,见该文件的说明;用法见 Start-ServiceInstance。
. (Join-Path $ScriptDir "lib\assetop_dev_secret.ps1")
. (Join-Path $ScriptDir "lib\account_directory_dev_secret.ps1")
. (Join-Path $ScriptDir "lib\release_common.ps1")
# 邮件 MailAdmin admin 生产者签名密钥(docs/design/mail-system.md §2.3)的本机开发值来源:只注入给 mail,用法见 Start-ServiceInstance。
# 令牌值不写进仓库,见该文件的说明(run/secrets/mail-admin-producer-dev.env;robot / mailadmin CLI 在自己的 shell 里按同一文件取值)。
. (Join-Path $ScriptDir "lib\mail_admin_dev_token.ps1")

# ── Service catalogue ────────────────────────────────────────────────
# Order matters: infrastructure-layer services first, then domain services.
# Each entry:
#   Dir         relative path under go/
#   Entry       main .go file (used by `go run` and `go build`)
#   Port        base ListenOn port (instance #i listens on Port + (i-1)*PortStride)
#   Desc        human-readable description
#   ConfigFlag  command-line flag the entry uses to override the config path (e.g. -f, -loginService)
#   ConfigFile  default config file path relative to the service Dir
#   AllowMultiInstance  $false to forbid -Counts > 1 (no service needs it today; db became multi-instance safe in 2026-10)
#   Tier        startup ordering bucket. Lower tiers are launched and (softly)
#               wait for TCP LISTEN before the next tier. ONLY used by the local
#               dev launcher to avoid first-boot dial races. Runtime services do
#               not see this; they still discover endpoints via etcd (so rolling
#               upgrades / hot updates / canary releases are unaffected).
#               Tier 0 = infra-adjacent (db, data_service)
#               Tier 1 = scene_manager (depended by player_locator), client_rpc_router (gate 的唯一 gRPC 目标),
#                        chat (TiDB权威、登录Redis只读、etcd发现),以及 trade / friend / player_inbox / mail / rank 等全局服务
#                        (依赖 Tier 0 的 data_service 号段 / 归属区查询时都是 NonBlock 拨号)
#               Tier 2 = player_locator (depended by login)
#               Tier 3 = login (top of the dial chain)
#   StopTimeoutSeconds  可选。stop 时 Ctrl+C 之后等该服务自行退出的秒数,超时强杀进程树;不写取 25
#               (盖住 chat / trade / friend / guild lifecycle 的 24s 硬截止)。-StopTimeoutSeconds >0 时整体覆盖。
#               db 取 35:它的 Stop 先撤拉取、排空已分发任务并在会话释放时提交位点(上限 claimDrainTimeout 10s
#               + 5s 余量),再离组、等 worker 收尾,比其它服务慢;K8s 侧对应 45s 宽限减 5s preStop。
#               超时被强杀不丢数据(排序锁留到 TTL、重试收据租约过期后回收),只是失去优雅停的收益。
$ServiceCatalogue = [ordered]@{
    mail = @{ Dir = "mail"; Entry = "mail.go"; Port = 50900; Desc = "邮件（全局权威邮箱与附件到账）"; ConfigFlag = "-f"; ConfigFile = "etc/mail.yaml"; AllowMultiInstance = $true; Tier = 1 }
    rank = @{ Dir = "rank"; Entry = "rank.go"; Port = 51100; Desc = "排行榜（全局代次、封榜与奖励）"; ConfigFlag = "-f"; ConfigFile = "etc/rank.yaml"; AllowMultiInstance = $true; Tier = 1 }
    db              = @{ Dir = "db";              Entry = "db.go";                    Port = 6000;  Desc = "DB (Kafka consumer + MySQL)";       ConfigFlag = "-f";            ConfigFile = "etc/db.yaml";                    AllowMultiInstance = $true;  Tier = 0; StopTimeoutSeconds = 35 }
    data_service    = @{ Dir = "data_service";    Entry = "data_service.go";          Port = 9000;  Desc = "Data Service (multi-zone Redis)"; ConfigFlag = "-f";            ConfigFile = "etc/data_service.yaml";          AllowMultiInstance = $true;  Tier = 0 }
    # Base ports for the zRPC services were originally 50000/50200 but Windows
    # reserves dynamic-port ranges that frequently land on 50000-50171 and a
    # few apps (Manager.exe in dev environments) reuse 50200. Zone 1 (= base
    # + 0 shift) would otherwise fail with "An attempt was made to access a
    # socket in a way forbidden by its access permissions" or silent bind
    # failures. 53000/53200 sit safely above all observed Windows excluded
    # ranges (49728-49927, 50000-50171, 51573-51872) and below 60300 used by
    # scene_manager, so zones 1-4 each get a clean 1000-port slot.
    player_locator  = @{ Dir = "player_locator";  Entry = "player_locator.go";        Port = 53200; Desc = "Player Locator (Redis cache)";    ConfigFlag = "-f";            ConfigFile = "etc/player_locator.yaml";        AllowMultiInstance = $true;  Tier = 2 }
    login           = @{ Dir = "login";           Entry = "login.go";                 Port = 53000; Desc = "Login (gRPC + etcd)";            ConfigFlag = "-loginService"; ConfigFile = "etc/login.yaml";                 AllowMultiInstance = $true;  Tier = 3 }
    scene_manager   = @{ Dir = "scene_manager";   Entry = "scene_manager_service.go"; Port = 60300; Desc = "Scene Manager (Kafka + Redis)";    ConfigFlag = "-f";            ConfigFile = "etc/scene_manager_service.yaml"; AllowMultiInstance = $true;  Tier = 1 }
    # 回合制战斗匹配/切磋编排(docs/design/turn-based-battle-server.md §5.4)。
    # 50500 位于已观测的 Windows 保留区间(50000-50171)与 51573-51872 之间,安全;与 etc yaml ListenOn 保持一致。
    # 全球Match首次启动/协议增表须先用独立迁移账号显式运行：
    # MATCH_MIGRATION_USER / MATCH_MIGRATION_PASSWORD仅注入该前台子命令，执行完清掉环境变量；
    # bin/go_services/match.exe -f <GlobalMatch.Enabled=true的同库配置> -migrate
    # 常驻start/start-exe只核只读Plan，不自动DDL；运行账号不需要CREATE/ALTER权限。
    # 按包构建/运行,包含同目录 original_role_interceptor.go 等入口依赖。
    match           = @{ Dir = "match";           Entry = ".";                        Port = 50500; Desc = "Match (turn-battle matchmaking)";  ConfigFlag = "-f";            ConfigFile = "etc/match_service.yaml";         AllowMultiInstance = $true;  Tier = 3 }
    # 客户端 RPC 路由服(docs/design/client-rpc-router.md D29-D34):gate 在 GATE_CLIENT_RPC_ROUTER=1 模式下
    # 唯一的 gRPC 目标,无状态、全局池不分 zone、可多开。Tier 1:gate 一起来就要连到它,与 scene_manager
    # 同批先起;它自身不依赖别的 Go 服务(只按 etcd 发现转发)。
    # 端口 50600 与 match 的 50500 同一安全区间(已观测的 Windows 保留区 50000-50171 与 51573-51872 之间),
    # 与 go/client_rpc_router/etc/client_rpc_router.yaml 的 ListenOn 保持一致。
    client_rpc_router = @{ Dir = "client_rpc_router"; Entry = "client_rpc_router_service.go"; Port = 50600; Desc = "Client RPC Router (gate 唯一 gRPC 目标)"; ConfigFlag = "-f"; ConfigFile = "etc/client_rpc_router.yaml"; AllowMultiInstance = $true; Tier = 1 }
    # 全局聊天：多副本共用TiDB原件和原会话P锁，Redis仅核原登录连接，多实例只在路由服与
    # NodeInfo 视角等价;本地双 zone 仍按既有规则每 zone 起一份(-Zone 位移端口与 ZoneId),不另开特例。
    # **前提:gate 必须以路由服模式启动(GATE_CLIENT_RPC_ROUTER=1)**。chat 只承诺经 gate → client_rpc_router
    # 到达;直连模式下 gate 白名单(cpp/nodes/gate/main.cpp)里没有 Chat,任何 zone 的玩家都到不了它 ——
    # 进程照样起得来、端口照样 LISTEN,所以「服务在跑」不等于「玩家可达」(start_game.ps1 -GateRouterMode 默认开)。
    # Tier 1:它不拨任何 Go 服务(只连 etcd 注册 + Redis),没理由排在 login 之后;与路由服同批起,玩家登录后即可用。
    # 端口 50700 与 match 50500 / 路由服 50600 同一安全区间(已观测的 Windows 保留区 50000-50171 与 51573-51872 之间)。
    # 注意 -Zone 2 位移后是 51700,恰好落在已观测的 51573-51872 里:由 Resolve-BindablePort 自动上挪并走派生 yaml,
    # 路由服经 etcd NodeInfo 发现实际端口,不受影响(ZonePortShift 刻意不为它改)。
    # 必须与 go/chat/etc/chat.yaml 的 ListenOn、k8s_deploy.ps1 $GoSvcCatalogue 的 Port、manifests/go-svc/chat.yaml 保持一致。
    chat              = @{ Dir = "chat"; Entry = "chat.go"; Port = 50700; Desc = "Chat (全局聊天,路由服可达)"; ConfigFlag = "-f"; ConfigFile = "etc/chat.yaml"; AllowMultiInstance = $true; Tier = 1 }
    # 公会 guild:全局一份(port-decisions D-2),帮会按 zone_id **数据维度**隔离而不是按部署隔离 ——
    # 客户端请求属于哪个区由 guild 查 data_service 的玩家归属映射决定(docs/design/guild-zone-client-access.md)。
    # 与 chat 一样**只承诺路由服模式可达**(GATE_CLIENT_RPC_ROUTER=1):直连模式下 gate 白名单里没有 Guild,
    # 进程在跑、端口在听,玩家照样到不了它。
    # Tier 1:只依赖基础设施(MySQL / Redis / etcd)与 data_service(Tier 0;号段 + 归属 zone,NonBlock 拨号),不拨 login 等服务。
    # 多开安全:建帮 / 入会 / 公告都是 MySQL 事务,启动时的榜单重建有 Redis 维护锁(guild_rank:maintenance_lock)。
    # 端口 50300 与 go/guild/etc/guild.yaml 的 ListenOn 一致;-Zone 2 位移后 51300,不在已观测的保留区间内。
    guild             = @{ Dir = "guild"; Entry = "guild.go"; Port = 50300; Desc = "Guild (公会,按 zone 隔离,路由服可达)"; ConfigFlag = "-f"; ConfigFile = "etc/guild.yaml"; AllowMultiInstance = $true; Tier = 1 }
    # 聚宝斋 trade(docs/design/jubaozhai-market.md):全局一份(TradeNodeService),商品按 market_zone **数据维度**
    # 隔离,与 chat / guild 一样**只承诺路由服模式可达**(GATE_CLIENT_RPC_ROUTER=1)。
    # Tier 1:依赖 MySQL 独占库 mmorpg_trade(库由 deploy/mysql-init/00_init_zone_dbs.sql 预建,表由 trade 启动期
    # schemamigrate 或 `trade -f etc/trade.yaml -migrate` 建,port-decisions D-14)与 data_service(Tier 0;
    # listing_id 号段 biz_tag=trade_listing + BatchGetPlayerHomeZone,NonBlock 拨号),不拨 login 等服务。
    # 多开安全:P1 只有只读浏览 + 收藏(INSERT IGNORE / DELETE 幂等)+ 内部种子,无进程内共享状态。
    # 端口 50800 / metrics 9230 与 go/trade/etc/trade.yaml 一致;-Zone 2 位移后 51800 / 10230,51800 可能落入历史观测的
    # 保留区 51573-51872,由 Resolve-BindablePort 自动上挪(同 chat)。trade.yaml 形状满足本脚本 -Zone 改写:
    # 顶层单行 ListenOn / MetricsListenAddr、顶层 ZoneId 写在所有嵌套段之前、DataServiceRpc.Etcd.Key: dataservice.rpc 会被加 .z<N>。
    trade             = @{ Dir = "trade"; Entry = "trade.go"; Port = 50800; Desc = "Trade (聚宝斋,按 market_zone 隔离,路由服可达)"; ConfigFlag = "-f"; ConfigFile = "etc/trade.yaml"; AllowMultiInstance = $true; Tier = 1 }
    # 好友 friend(docs/design/friend-port-20260918.md):全局一份(FriendNodeService),好友关系天然跨 zone,
    # 业务代码禁止读 ZoneId 做分支;与 chat / guild / trade 一样**只承诺路由服模式可达**(GATE_CLIENT_RPC_ROUTER=1)。
    # 本目录**没有 Global 这个字段**(chat / guild / trade 同样不写,port-decisions D-2):"全服一份"是 K8s 侧的
    # 部署形态,由 k8s_deploy.ps1 $GoSvcCatalogue 的 Global = $true 表达(只在 infra namespace 部署一份、zone-up 跳过);
    # 本地双 zone 仍按既有规则每 zone 起一份进程。所以 friend 在那边标 Global、在这里不标,两处不矛盾。
    # Tier 1:只依赖基础设施(MySQL 独占库 mmorpg_friend + 共享 Redis + etcd + Kafka),不拨任何 Go 服务
    # (不领号段、不连 data_service —— 好友关系的主键就是两个 player_id)。
    # 多开安全:写路径全是 MySQL 事务,后台 sweep 是幂等的批量 DELETE,不引入 leader。
    # 端口 50400 / metrics 9180 与 go/friend/etc/friend.yaml 的 ListenOn / MetricsListenAddr、k8s_deploy.ps1 的
    # $GoSvcCatalogue 与 ConfigMap、manifests/go-svc/friend.yaml 五处逐字一致(契约 §7);-Zone 2 位移后 51400 / 10180,
    # 均不在已观测的 Windows 保留区间内。friend.yaml 形状满足本脚本 -Zone 改写:顶层单行 ListenOn / MetricsListenAddr、
    # 顶层 ZoneId 写在所有嵌套段之前;它自己的 Etcd.Key 是空串(D-13),不会被 .z<N> 后缀规则碰到。
    friend            = @{ Dir = "friend"; Entry = "friend.go"; Port = 50400; Desc = "Friend (全局好友,路由服可达)"; ConfigFlag = "-f"; ConfigFile = "etc/friend.yaml"; AllowMultiInstance = $true;  Tier = 1 }
    # 玩家持久收件箱 player_inbox(docs/design/player-inbox-service.md,D3 独立进程):全服一份(PlayerInboxNodeService),
    # 按全局唯一的 player_id 存;调用方是 C++ scene(按 etcd 前缀 PlayerInboxNodeService.rpc 直连 FetchItems / AckItems)
    # 与运维(PlayerInboxAdmin,x-admin-token),**不经路由服**,与 gate 的路由模式无关。
    # 与 friend 一样本目录不写 Global:K8s 侧由 k8s_deploy.ps1 $GoSvcCatalogue 的 Global = $true 表达,本地双 zone 仍每 zone 起一份。
    # Tier 1:依赖 MySQL 独占库 mmorpg_player_inbox(库由 deploy/mysql-init/00_init_zone_dbs.sql 预建,表由启动期
    # schemamigrate 或 `player_inbox -f etc/player_inbox.yaml -migrate` 建,D-14)、共享 Redis(owner_epoch 上界 / location)、
    # etcd、Kafka(在线提醒 scene-cmd)与 data_service(Tier 0;GetPlayerHomeZone / GetPlayerInboxLedger,NonBlock 拨号)。
    # 弱依赖:scene 不等它(不进 DependencyGate,IB-7),起晚了只是收件箱晚生效。
    # 多开安全:每个事务只碰一个 (player, domain) 的流行锁;清理 / 补发 / gauge / 对账四个后台循环只在领导者上跑。
    # 端口 50950 / metrics 9270(D14)与 go/player_inbox/etc/player_inbox.yaml 的 ListenOn / MetricsListenAddr、k8s_deploy.ps1 的
    # $GoSvcCatalogue 与 ConfigMap、manifests/go-svc/player-inbox.yaml 逐字一致;-Zone 2 位移后 51950 / 10270,均不在已观测的
    # Windows 保留区间内。player_inbox.yaml 形状须满足本脚本 -Zone 改写:顶层单行 ListenOn / MetricsListenAddr、顶层 ZoneId
    # 写在所有嵌套段之前;自己的 Etcd.Key 是空串(D-13),DataServiceRpc.Etcd.Key: dataservice.rpc 会被加 .z<N>(照 trade)。
    player_inbox      = @{ Dir = "player_inbox"; Entry = "player_inbox.go"; Port = 50950; Desc = "Player Inbox (玩家持久收件箱,scene 直连)"; ConfigFlag = "-f"; ConfigFile = "etc/player_inbox.yaml"; AllowMultiInstance = $true; Tier = 1 }
    # mail/rank 已按上面的现役条目登记：SQL/TiDB DSN、分用途生产者签名及全局语义；不恢复M1运行时。
}

# Derived per-instance config files live here so the source tree stays clean.
$DerivedEtcDir = Join-Path $RunDir "etc\go_services"

# Normalize $Counts (which may arrive as a hashtable, string, or string[]) to a
# plain hashtable<string,int>. pwsh -File serializes hashtable args to a string,
# so we accept the string form 'name=N,name=N' as a fallback.
. (Join-Path $PSScriptRoot "maintenance_drain.ps1")

function ConvertTo-CountsHashtable {
    param($Raw)
    $result = @{}
    if ($null -eq $Raw) { return $result }
    if ($Raw -is [hashtable]) {
        foreach ($k in $Raw.Keys) { $result[[string]$k] = [int]$Raw[$k] }
        return $result
    }
    $parts = @()
    if ($Raw -is [string]) {
        $parts = $Raw -split '[,;]' | Where-Object { $_ -ne '' }
    } elseif ($Raw -is [System.Collections.IEnumerable]) {
        foreach ($item in $Raw) {
            if ($item -is [string]) { $parts += ($item -split '[,;]' | Where-Object { $_ -ne '' }) }
        }
    } else {
        throw "Unsupported -Counts value type: $($Raw.GetType().FullName)"
    }
    foreach ($p in $parts) {
        $kv = $p -split '=', 2
        if ($kv.Count -ne 2) { throw "Invalid -Counts entry '$p'. Expected 'name=N'." }
        $result[$kv[0].Trim()] = [int]$kv[1].Trim()
    }
    return $result
}

$Counts = ConvertTo-CountsHashtable -Raw $Counts

# ── Helpers ──────────────────────────────────────────────────────────
function Resolve-ServiceList {
    param([string[]]$Requested)
    if ($Requested.Count -eq 0) {
        return @($ServiceCatalogue.Keys)
    }
    # Allow `-Services "a,b,c"` (single comma-joined string) in addition to
    # `-Services a,b,c` so pwsh -File invocations from .bat work the same way.
    $expanded = @()
    foreach ($s in $Requested) {
        $expanded += ($s -split '[,;]') | Where-Object { $_ -ne '' }
    }
    foreach ($s in $expanded) {
        if (-not $ServiceCatalogue.Contains($s)) {
            throw "Unknown service '$s'. Run with -Command list to see available services."
        }
    }
    return $expanded
}

# Returns the requested instance count for a service. Defaults to 1.
# Validates AllowMultiInstance and rejects negative / huge counts.
function Get-ServiceInstanceCount {
    param([string]$Name)
    if (-not $Counts.ContainsKey($Name)) { return 1 }
    $n = [int]$Counts[$Name]
    if ($n -lt 1) { return 1 }
    if ($n -gt 32) { throw "Refusing to launch $n instances of '$Name' (cap=32)." }
    if ($n -gt 1 -and -not $ServiceCatalogue[$Name].AllowMultiInstance) {
        throw "Service '$Name' does not support multi-instance (AllowMultiInstance=false). Counts entry: $Name=$n."
    }
    return $n
}

# Zone prefix used in instance keys / derived config filenames. Empty when
# Zone <= 0 to preserve legacy single-zone behaviour.
function Get-ZonePrefix {
    if ($Zone -le 0) { return "" }
    return "z${Zone}_"
}

# Builds the InstanceKey used everywhere (PID file, log file, derived yaml).
# Single instance preserves the legacy bare service name for backward compatibility,
# unless multi-zone is active (then keys are always zone-prefixed so multiple
# zones can coexist in the shared PID file).
function Get-InstanceKey {
    param([string]$Name, [int]$Index, [int]$Total)
    $zp = Get-ZonePrefix
    if ($Total -le 1) {
        if ($zp -eq "") { return $Name }
        return "${zp}${Name}"
    }
    return "${zp}${Name}_${Index}"
}

# For instance #1 of a single-instance launch with Zone=0, return the original
# etc path so behavior is unchanged. Otherwise materialize a derived yaml in
# run/etc/go_services with ListenOn (and ZoneId, when -Zone is set) rewritten.
# Windows(Hyper-V / WinNAT)每次开机会随机保留若干 TCP 端口区间(`netsh int ipv4 show excludedportrange protocol=tcp`),
# 固定端口落进去时 bind 报 "An attempt was made to access a socket in a way forbidden by its access permissions",
# go-zero 服务直接 panic(2026-08-22 player_locator 51200、2026-09-05 scene_manager 60300 都是这个坑)。
# 起服务前探一次:落在保留区间就往上挪到第一个不在任何区间里的端口,并强制走派生 yaml 写入;
# 对等方经 etcd 发现实际端口,不受影响。原始端口被占用仍报错;只有从保留区间挪出的候选
# 才继续避让本批计划和已有监听,避免 chat / trade 从同一保留区间挪到同一个出口。
$script:ExcludedPortRanges = $null
function Get-ExcludedPortRanges {
    if ($null -ne $script:ExcludedPortRanges) { return ,$script:ExcludedPortRanges }
    $ranges = @()
    try {
        $out = netsh interface ipv4 show excludedportrange protocol=tcp 2>$null
        foreach ($line in $out) {
            if ($line -match '^\s*(\d+)\s+(\d+)\s*\*?\s*$') { $ranges += ,@([int]$Matches[1], [int]$Matches[2]) }
        }
    } catch { }
    $script:ExcludedPortRanges = $ranges
    return ,$ranges
}
function Test-PortExcluded([int]$Port) {
    foreach ($r in (Get-ExcludedPortRanges)) { if ($Port -ge $r[0] -and $Port -le $r[1]) { return $true } }
    return $false
}
function Get-TcpListenerProcessIds([int]$Port) {
    # 不按 LocalPort 过滤 CIM 查询:没有匹配项时 cmdlet 会报错,不能把查询失败误当作端口空闲。
    return @(Get-NetTCPConnection -State Listen -ErrorAction Stop |
        Where-Object LocalPort -eq $Port | Select-Object -ExpandProperty OwningProcess -Unique)
}

function Get-InstancePort([hashtable]$Info, [int]$Index) {
    $zoneShift = if ($Zone -gt 0) { ($Zone - 1) * $ZonePortShift } else { 0 }
    return [int]$Info.Port + $zoneShift + ($Index - 1) * $PortStride
}

function Resolve-BindablePort {
    param([int]$Port, [string]$Label, [hashtable]$ReservedPorts = @{})
    if ($Port -lt 1 -or $Port -gt 65535) { throw "[port] $Label : 无效端口 $Port" }
    $relocate = Test-PortExcluded $Port
    for ($p = $Port; $p -le [Math]::Min(65535, $Port + 2000); $p++) {
        if (Test-PortExcluded $p) { continue }
        $planned = $ReservedPorts.ContainsKey($p) -and $ReservedPorts[$p] -ne $Label
        $owners = @(Get-TcpListenerProcessIds $p)
        if ($planned -or $owners.Count -gt 0) {
            if ($relocate) { continue }
            throw "[port] $Label : 原始端口 $Port 已占用 (计划=$($ReservedPorts[$p]), PID=$($owners -join ',')); 请核对实例或配置,不会终止占用进程。"
        }
        $ReservedPorts[$p] = $Label
        if ($p -ne $Port) {
            Write-Warning "[port] $Label : $Port is in a Windows reserved port range; using $p (derived yaml; peers discover via etcd and are unaffected)"
        }
        return $p
    }
    throw "[port] $Label : $Port 起 2000 个端口内没有可用候选。"
}

# 只接受仓库配置的明确层级/单值；不解释 YAML 别名或表达式，也不回显包含凭据的原文。
# 按字符位置只替换布尔值，保证 dual、围栏与其他配置字节原样保留。
function ConvertTo-LocalMySQLDbConfig([string]$Content) {
    $lines = [regex]::Matches($Content, '(?m)^[^\r\n]*(?:\r?\n|$)')
    $serverHeaders = @(for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i].Value.TrimEnd("`r", "`n") -match '^ServerConfig:[ \t]*(?:#.*)?$') { $i }
    })
    if ($serverHeaders.Count -ne 1) { throw 'LocalMySQL 要求唯一的 ServerConfig 配置段。' }
    $serverStart = $serverHeaders[0]
    $serverEnd = $lines.Count
    for ($i = $serverStart + 1; $i -lt $lines.Count; $i++) {
        if ($lines[$i].Value -match '^[^\s#]') { $serverEnd = $i; break }
    }
    $databaseHeaders = @(for ($i = $serverStart + 1; $i -lt $serverEnd; $i++) {
        if ($lines[$i].Value.TrimEnd("`r", "`n") -match '^  Database:[ \t]*(?:#.*)?$') { $i }
    })
    if ($databaseHeaders.Count -ne 1) { throw 'LocalMySQL 要求唯一的 ServerConfig.Database 配置段。' }
    $databaseStart = $databaseHeaders[0]
    $databaseEnd = $serverEnd
    for ($i = $databaseStart + 1; $i -lt $serverEnd; $i++) {
        if ($lines[$i].Value -match '^ {0,3}[^\s#]') { $databaseEnd = $i; break }
    }
    $hosts = @()
    $requirements = @()
    for ($i = $databaseStart + 1; $i -lt $databaseEnd; $i++) {
        $line = $lines[$i].Value.TrimEnd("`r", "`n")
        if ($line -match '^    Hosts:') { $hosts += $i }
        if ($line -match '^    RequireTiDB:') { $requirements += $i }
    }
    if ($hosts.Count -ne 1 -or $requirements.Count -ne 1) {
        throw 'LocalMySQL 要求 Database.Hosts 与 RequireTiDB 各有一个显式标量。'
    }
    $hostMatch = [regex]::Match($lines[$hosts[0]].Value.TrimEnd("`r", "`n"),
        '^    Hosts:[ \t]*(?:"(?<endpoint>[^"\r\n]+)"|''(?<endpoint>[^''\r\n]+)''|(?<endpoint>[^#\s"'']+))[ \t]*(?:#.*)?$')
    $endpointMatch = [regex]::Match($hostMatch.Groups['endpoint'].Value,
        '^(?<host>localhost|127\.[0-9]+\.[0-9]+\.[0-9]+|\[::1\]):(?<port>[0-9]+)$',
        [System.Text.RegularExpressions.RegexOptions]::IgnoreCase)
    $port = 0
    $localAddress = $null
    $hostName = $endpointMatch.Groups['host'].Value.Trim('[', ']')
    $loopback = $hostName -ieq 'localhost' -or
        ([Net.IPAddress]::TryParse($hostName, [ref]$localAddress) -and [Net.IPAddress]::IsLoopback($localAddress))
    if (-not $hostMatch.Success -or -not $endpointMatch.Success -or -not $loopback -or
        -not [int]::TryParse($endpointMatch.Groups['port'].Value, [ref]$port) -or $port -lt 1 -or $port -gt 65535) {
        throw 'LocalMySQL 只允许显式的回环 SQL 单端点，不会降低远端数据库的 TiDB 要求。'
    }
    $requirementLine = $lines[$requirements[0]]
    $requirementMatch = [regex]::Match($requirementLine.Value.TrimEnd("`r", "`n"),
        '^    RequireTiDB:[ \t]*(?<value>true|false)[ \t]*(?:#.*)?$')
    if (-not $requirementMatch.Success) { throw 'LocalMySQL 的 RequireTiDB 必须是明确的 true/false。' }
    $value = $requirementMatch.Groups['value']
    $offset = $requirementLine.Index + $value.Index
    return $Content.Remove($offset, $value.Length).Insert($offset, 'false')
}

function Get-LocalChatAuthoritySettings {
    param([string]$ConfigPath, [switch]$RequireDsn)
    $values = @{}
    foreach ($key in @('DSNEnv', 'RequireTiDB', 'GlobalLoginSessions', 'MaxOpenConns', 'AccountRoleSchema', 'LoginRedis.Host', 'LoginRedis.DB', 'LoginRedis.PasswordEnv', 'LoginRedis.PoolSize')) {
        $value = Get-YamlScalar -Path $ConfigPath -KeyPath $key
        if (-not $value.Found) { throw "Chat启动缺少显式权威配置字段 $key" }
        $values[$key] = $value.Value
    }
    if ($values.DSNEnv -cne 'CHAT_AUTHORITY_DSN' -or $values.RequireTiDB -cne 'true' -or $values.GlobalLoginSessions -cne 'true' -or $values['LoginRedis.PasswordEnv'] -cne 'LOGIN_REDIS_PASSWORD') {
        throw 'Chat必须使用TiDB及原登录态；禁止旧Redis历史/幂等权威回退。'
    }
    if ($values.MaxOpenConns -notmatch '^\d+$' -or [int]$values.MaxOpenConns -lt 2 -or [int]$values.MaxOpenConns -gt 128 -or $values['LoginRedis.PoolSize'] -notmatch '^\d+$' -or [int]$values['LoginRedis.PoolSize'] -lt 1 -or [int]$values['LoginRedis.PoolSize'] -gt 128) {
        throw 'Chat SQL和登录Redis池必须显式有界，不能采用随CPU变化的缺省池。'
    }
    $schema = Get-YamlScalar -Path (Join-Path $GoRoot 'data_service/etc/data_service.yaml') -KeyPath 'SnapshotMySQL.DBName'
    $redisHost = Get-YamlScalar -Path (Join-Path $GoRoot 'login/etc/login.yaml') -KeyPath 'Node.RedisClient.Host'
    $redisDb = Get-YamlScalar -Path (Join-Path $GoRoot 'login/etc/login.yaml') -KeyPath 'Node.RedisClient.DB'
    if (-not $schema.Found -or $schema.Value -notmatch '^[a-z][a-z0-9_]{0,63}$' -or -not $redisHost.Found -or $redisHost.Value -notmatch '^(?:[A-Za-z0-9][A-Za-z0-9.-]*|\[[0-9A-Fa-f:]+\]):[0-9]{1,5}$' -or -not $redisDb.Found -or $redisDb.Value -notmatch '^\d+$') {
        throw '本机Chat必须能读取同data_service全局角色库与同login Redis地址/DB，缺失时不猜测。'
    }
    if ($RequireDsn) {
        $dsn = [Environment]::GetEnvironmentVariable($values.DSNEnv, 'Process')
        if ([string]::IsNullOrWhiteSpace($dsn) -or $dsn.Contains("`r") -or $dsn.Contains("`n")) {
            throw 'Chat缺少合法CHAT_AUTHORITY_DSN环境值；请先以迁移账号显式-migrate，再提供无DDL权限的运行账号DSN。值不会写入配置或日志。'
        }
        if ($values.AccountRoleSchema -cne $schema.Value -or $values['LoginRedis.Host'] -cne $redisHost.Value -or $values['LoginRedis.DB'] -cne $redisDb.Value) {
            throw '派生Chat配置与当前全局角色库/login Redis原件不一致，拒绝启动。'
        }
    }
    return @{ Schema = $schema.Value; RedisHost = $redisHost.Value; RedisDb = $redisDb.Value }
}

function Resolve-InstanceConfig {
    param(
        [string]$Name,
        [hashtable]$Info,
        [int]$Index,
        [int]$Total,
        [hashtable]$ReservedPorts = @{}
    )
    $svcDir    = Join-Path $GoRoot $Info.Dir
    $baseYaml  = Join-Path $svcDir $Info.ConfigFile
    $useLocalMySQL = $LocalMySQL -and $Name -eq 'db'
    $zoneShift = if ($Zone -gt 0) { ($Zone - 1) * $ZonePortShift } else { 0 }
    $port      = Get-InstancePort -Info $Info -Index $Index
    $port      = Resolve-BindablePort $port (Get-InstanceKey -Name $Name -Index $Index -Total $Total) $ReservedPorts
	$globalTiDBOverride = [string][Environment]::GetEnvironmentVariable('MMORPG_GLOBAL_AUTHORITY_TIDB_HOST','Process')
	$rewriteGlobalTiDB = $Name -in @('data_service','db') -and -not [string]::IsNullOrWhiteSpace($globalTiDBOverride)

    # Legacy fast path: single-zone, single-instance -> original yaml as-is.
    if (-not $useLocalMySQL -and $Name -ne 'chat' -and -not $rewriteGlobalTiDB -and $Total -le 1 -and $Zone -le 0 -and $port -eq [int]$Info.Port) {
        return [pscustomobject]@{
            Path     = $Info.ConfigFile  # relative path; resolved by working dir
            FullPath = $baseYaml
            Port     = [int]$Info.Port
            Derived  = $false
        }
    }

    if (-not (Test-Path $baseYaml)) {
        throw "Base config not found for '$Name' instance #$Index : $baseYaml"
    }
    $content = Get-Content -LiteralPath $baseYaml -Raw
    if (-not (Test-Path $DerivedEtcDir)) {
        New-Item -ItemType Directory -Path $DerivedEtcDir -Force | Out-Null
    }

    $zp = Get-ZonePrefix
    $derivedName = if ($Total -le 1) { "${zp}${Name}.yaml" } else { "${zp}${Name}_${Index}.yaml" }
    $derivedPath = Join-Path $DerivedEtcDir $derivedName

    # Rewrite top-level ListenOn line. Keep the original host (e.g. 0.0.0.0 vs 127.0.0.1).
    # Use an instance Regex so the count-limited overload is available.
	if ($rewriteGlobalTiDB) {
		if ($globalTiDBOverride -notmatch '\A(?:[A-Za-z0-9][A-Za-z0-9.-]*|\[[0-9A-Fa-f:]+\]):([0-9]{1,5})\z' -or [int]$Matches[1] -lt 1 -or [int]$Matches[1] -gt 65535) { throw 'MMORPG_GLOBAL_AUTHORITY_TIDB_HOST必须是有效host:port，不接受凭据或配置注入。' }
		$pattern = if ($Name -eq 'data_service') { '(?m)^(SnapshotMySQL:\r?\n)(?:[ \t]+[^\r\n]*\r?\n)*' } else { '(?m)^  Database:\r?\n(?:[ ]{4}[^\r\n]*\r?\n|[ \t]*\r?\n)*' }
		$blocks = [regex]::Matches($content,$pattern)
		if ($blocks.Count -ne 1) { throw '正规全球SQL配置块不唯一，拒绝生成本机派生配置。' }
		$block = $blocks[0].Value
		$hostPattern = if ($Name -eq 'data_service') { '(?m)^  Host:.*$' } else { '(?m)^    Hosts:.*$' }
		if ([regex]::Matches($block,$hostPattern).Count -ne 1) { throw '正规全球SQL地址字段不唯一。' }
		$newLine = if ($Name -eq 'data_service') { '  Host: "'+$globalTiDBOverride+'"' } else { '    Hosts: "'+$globalTiDBOverride+'"' }
		$updated = [regex]::Replace($block,$hostPattern,$newLine)
		$content = $content.Substring(0,$blocks[0].Index)+$updated+$content.Substring($blocks[0].Index+$blocks[0].Length)
	}
    if ($useLocalMySQL) { $content = ConvertTo-LocalMySQLDbConfig $content }
    if ($Name -eq 'chat') {
        $chatAuthority = Get-LocalChatAuthoritySettings -ConfigPath $baseYaml
        $schemaPattern = [regex]::new('(?m)^AccountRoleSchema:\s*[^\r\n]+$')
        $loginPattern = [regex]::new('(?m)^LoginRedis:\r?\n(?:[ \t]+[^\r\n]*(?:\r?\n|$))+')
        if ($schemaPattern.Matches($content).Count -ne 1 -or $loginPattern.Matches($content).Count -ne 1) { throw 'Chat权威配置块缺失或重复，拒绝派生。' }
        $content = $schemaPattern.Replace($content, "AccountRoleSchema: $($chatAuthority.Schema)", 1)
        $loginBlock = "LoginRedis:`n  Host: $($chatAuthority.RedisHost)`n  PasswordEnv: LOGIN_REDIS_PASSWORD`n  DB: $($chatAuthority.RedisDb)`n  PoolSize: $((Get-YamlScalar -Path $baseYaml -KeyPath 'LoginRedis.PoolSize').Value)`n"
        $content = $loginPattern.Replace($content, $loginBlock, 1)
    }
    $regex   = [regex]::new('(?m)^(ListenOn:[ \t]*)([^:\s]+):(\d+)([ \t]*\r?)$')
    if (-not $regex.IsMatch($content)) {
        Write-Warning "Did not find a top-level 'ListenOn:' line in $baseYaml; instance #$Index may collide on port $($Info.Port)."
    }
    $content = $regex.Replace(
        $content,
        { param($m) "$($m.Groups[1].Value)$($m.Groups[2].Value):$port$($m.Groups[4].Value)" },
        1
    )

    # Rewrite top-level MetricsListenAddr (e.g. `MetricsListenAddr: ":9170"`) with the
    # SAME offset rule as ListenOn: base + (Zone-1)*ZonePortShift + (Index-1)*PortStride.
    # Without this every instance of scene_manager / db / match binds the same fixed
    # Prometheus port and the second instance (or second zone) dies on bind. Keeping
    # the rule identical to ListenOn means one mental model: zone 2 match #1 listens
    # on 51500 and exposes metrics on 10170. Services without the key are untouched.
    $metricsRegex = [regex]::new('(?m)^(?<lead>MetricsListenAddr:\s*"?)(?<host>[^:"\s]*):(?<port>\d+)(?<tail>"?\s*(#.*)?)$')
    if ($metricsRegex.IsMatch($content)) {
        $content = $metricsRegex.Replace(
            $content,
            { param($m)
                $metricsPort = [int]$m.Groups['port'].Value + $zoneShift + ($Index - 1) * $PortStride
                "$($m.Groups['lead'].Value)$($m.Groups['host'].Value):$metricsPort$($m.Groups['tail'].Value)"
            },
            1
        )
    }

    # Rewrite first ZoneId entry (handles both top-level 'ZoneId: N' and the
    # nested 'Node:\n  ZoneId: N' shape used by login/guild/player_locator).
    if ($Zone -gt 0) {
        $zoneRegex = [regex]::new('(?m)^(?<lead>\s*)ZoneId:\s*\d+\s*$')
        if ($zoneRegex.IsMatch($content)) {
            $content = $zoneRegex.Replace(
                $content,
                { param($m) "$($m.Groups['lead'].Value)ZoneId: $Zone" },
                1
            )
        } else {
            Write-Warning "No 'ZoneId:' found in $baseYaml; '$Name' will not honour -Zone $Zone."
        }

        # Suffix every etcd RPC discovery key with .z<Zone> so each zone's
        # zrpc clients/servers form their own isolated load-balancer set.
        # Matches both the server's own Etcd.Key (e.g. 'Key: scenemanagerservice.rpc')
        # and every RpcClient.Etcd.Key (e.g. 'SceneManagerRpc.Etcd.Key', 'LoginAdminRpc.Etcd.Key').
        # Deliberately scoped to identifiers ending in '.rpc' to avoid touching
        # Redis cache prefixes (e.g. scene_manager.yaml has 'Redis.Key: scenemanagerservice').
        $keyRegex = [regex]::new('(?m)^(?<lead>\s*Key:\s*)(?<id>[\w.-]+\.rpc)\s*$')
        $content = $keyRegex.Replace(
            $content,
            { param($m) "$($m.Groups['lead'].Value)$($m.Groups['id'].Value).z$Zone" }
        )

        # Suffix Kafka consumer group IDs so two zones do not race for the
        # same partitions when they happen to subscribe to overlapping topics
        # (defence in depth -- per-zone topics are already used in practice).
        $groupRegex = [regex]::new('(?m)^(?<lead>\s*GroupID:\s*")(?<id>[^"\r\n]+)(?<tail>"\s*(#.*)?)$')
        $content = $groupRegex.Replace(
            $content,
            { param($m) "$($m.Groups['lead'].Value)$($m.Groups['id'].Value)_z$Zone$($m.Groups['tail'].Value)" }
        )
    }

    Set-Content -Path $derivedPath -Value $content -Encoding UTF8 -NoNewline

    return [pscustomobject]@{
        Path     = $derivedPath
        FullPath = $derivedPath
        Port     = $port
        Derived  = $true
    }
}

# Normalize a PID-file entry to a hashtable { Pid; Port; Service }.
# Accepts legacy int values (pre multi-instance) for backward compatibility.
function Read-InstanceEntry {
    param($RawValue, [string]$FallbackService, [int]$FallbackPort)
    if ($null -eq $RawValue) { return $null }
    if ($RawValue -is [int] -or $RawValue -is [long]) {
        return @{ Pid = [int]$RawValue; Port = $FallbackPort; Service = $FallbackService }
    }
    $procId = if ($RawValue.PSObject.Properties.Name -contains 'Pid') { [int]$RawValue.Pid } else { 0 }
    $port   = if ($RawValue.PSObject.Properties.Name -contains 'Port') { [int]$RawValue.Port } else { $FallbackPort }
    $svc    = if ($RawValue.PSObject.Properties.Name -contains 'Service') { [string]$RawValue.Service } else { $FallbackService }
    return @{ Pid = $procId; Port = $port; Service = $svc }
}

# 分层就绪只认目标进程的监听;go run 的监听者是它启动的子进程。
# Soft TCP LISTEN probe used between tiers. We deliberately do NOT
# fail the launch on timeout: this is just a "give earlier tiers a head start"
# heuristic to avoid first-boot dial races. Runtime services keep using etcd
# discovery + gRPC reconnect, so rolling upgrades / canary releases are unaffected.
function Wait-TcpListenReady {
    param(
        [int]$Port,
        [int]$TimeoutSeconds = 10,
        [Parameter(Mandatory = $true)][int]$ProcessId,
        [switch]$AllowChildProcess
    )
    if ($Port -le 0 -or $ProcessId -le 0) { return $false }
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    do {
        $proc = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
        if (-not $proc -or $proc.HasExited) { return $false }
        $processIds = @($ProcessId)
        if ($AllowChildProcess) {
            for ($i = 0; $i -lt $processIds.Count; $i++) {
                $children = @(Get-CimInstance Win32_Process -Filter "ParentProcessId = $($processIds[$i])" -ErrorAction Stop)
                foreach ($child in $children) {
                    if ($processIds -notcontains [int]$child.ProcessId) { $processIds += [int]$child.ProcessId }
                }
            }
        }
        foreach ($owner in (Get-TcpListenerProcessIds $Port)) {
            if ($processIds -contains $owner) { return $true }
        }
        if ((Get-Date) -ge $deadline) { break }
        Start-Sleep -Milliseconds 200
    } while ((Get-Date) -lt $deadline)
    return $false
}

function Read-PidFile {
    if (Test-Path $PidFile) {
        $raw = Get-Content $PidFile -Raw | ConvertFrom-Json
        if ($null -eq $raw) { return [pscustomobject]@{} }
        return $raw
    }
    # One-time migration: pick up legacy pid file if present, then rewrite
    # under the new run/ location on next Write-PidFile.
    if (Test-Path $LegacyPidFile) {
        $raw = Get-Content $LegacyPidFile -Raw | ConvertFrom-Json
        if ($null -eq $raw) { return [pscustomobject]@{} }
        return $raw
    }
    return [pscustomobject]@{}
}

function Write-PidFile {
    param($Map)
    $dir = Split-Path -Parent $PidFile
    if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Path $dir -Force | Out-Null }
    $Map | ConvertTo-Json | Set-Content $PidFile -Encoding UTF8
}

# ── Helpers (startup monitoring) ──────────────────────────────────────
function Wait-ForStartupBanner {
    param(
        [string]$LogFile,
        [string]$ServiceName,
        [int]$TimeoutSeconds = 30
    )
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        try {
            if (Test-Path $LogFile) {
                $content = Get-Content $LogFile -Raw -ErrorAction SilentlyContinue
                if ($content -and ($content -match "STARTED SUCCESSFULLY")) {
                    $lines = $content -split "`n"
                    $inBanner = $false
                    foreach ($line in $lines) {
                        $trimmed = $line.Trim()
                        if ($trimmed -match '^={5,}') {
                            if ($inBanner) {
                                Write-Host "        $trimmed" -ForegroundColor Green
                                break
                            }
                            $inBanner = $true
                        }
                        if ($inBanner -and $trimmed.Length -gt 0) {
                            Write-Host "        $trimmed" -ForegroundColor Green
                        }
                    }
                    return
                }
            }
        } catch {
            # File might be locked briefly; retry on next iteration
        }
        Start-Sleep -Milliseconds 500
    }
    Write-Host "        [timeout] $ServiceName did not report startup within ${TimeoutSeconds}s - check logs" -ForegroundColor Yellow
}

# ── Commands ─────────────────────────────────────────────────────────
function Resolve-GoExecutablePath {
    param(
        [Parameter(Mandatory = $true)]
        [string]$ServiceName,
        [Parameter(Mandatory = $true)]
        [string]$ServiceDir
    )

    $preferred = Join-Path $GoBinDir "$ServiceName.exe"
    if (Test-Path $preferred) {
        return $preferred
    }

    $fallback = Join-Path $ServiceDir "$ServiceName.exe"
    if (Test-Path $fallback) {
        return $fallback
    }

    return $null
}

function Test-InstanceProcessIdentity {
    param([string]$Name, [hashtable]$Info, [string]$InstanceKey, [int]$Total, $Process)
    $svcDir = Join-Path $GoRoot $Info.Dir
    $executablePaths = @((Join-Path $GoBinDir "$Name.exe"), (Join-Path $svcDir "$Name.exe"))
    $commandLine = ([string]$Process.CommandLine).Replace('/', '\')
    if ($executablePaths -notcontains [string]$Process.ExecutablePath) {
        $goCommand = Get-Command go -ErrorAction SilentlyContinue
        if (-not $goCommand -or $Process.ExecutablePath -ne $goCommand.Source) { return $false }
        # go.exe 是全机共用的;只有绝对源码路径能证明它属于本仓库。旧的相对路径命令无法核实,
        # 保守拒绝复用,避免另一个仓库同名 chat.go 被当成本实例。
        $entryPattern = [regex]::Escape((Join-Path $svcDir $Info.Entry).Replace('/', '\'))
        if ($commandLine -notmatch "(?:^|\s)run\s+`"?$entryPattern`"?(?=\s|$)") { return $false }
    }
    # 兼容旧 PID 文件;身份必须同时匹配仓库内二进制和本实例配置,不能只凭 PID 存活。
    $configPaths = @((Join-Path $DerivedEtcDir "$InstanceKey.yaml"))
    if ($InstanceKey -eq $Name) {
        $configPaths += $Info.ConfigFile, (Join-Path $svcDir $Info.ConfigFile)
    }
    $flagPattern = [regex]::Escape($Info.ConfigFlag)
    foreach ($path in $configPaths) {
        $pathPattern = [regex]::Escape($path.Replace('/', '\'))
        if ($commandLine -match "(?:^|\s)$flagPattern\s+(?:`"$pathPattern`"|$pathPattern)(?=\s|$)") { return $true }
    }
    return $false
}

function Get-TrackedRunningInstance {
    param([string]$Name, [hashtable]$Info, [string]$InstanceKey, $RawValue, [int]$FallbackPort)
    $existing = Read-InstanceEntry -RawValue $RawValue -FallbackService $Name -FallbackPort $FallbackPort
    if (-not $existing -or $existing.Pid -le 0) { return $null }
    $existingProcess = Get-CimInstance Win32_Process -Filter "ProcessId = $($existing.Pid)" -ErrorAction Stop
    if (-not $existingProcess) { return $null }
    if ($existing.Service -ne $Name -or -not (Test-InstanceProcessIdentity -Name $Name -Info $Info -InstanceKey $InstanceKey -Process $existingProcess)) {
        throw "[pid] $InstanceKey : PID $($existing.Pid) 的可执行文件或配置不匹配,请核对 PID 记录;不会复用或终止该进程。"
    }
    return $existing
}

function Reserve-RunningInstancePorts {
    param([pscustomobject]$Pids, [hashtable]$ReservedPorts)
    $runningOwners = @{}
    foreach ($property in $Pids.PSObject.Properties) {
        $instanceKey = $property.Name
        $name = if ($ServiceCatalogue.Contains($instanceKey)) { $instanceKey }
                elseif ($instanceKey -match '^(?:z\d+_)?(?<svc>.+?)(?:_\d+)?$' -and $ServiceCatalogue.Contains($Matches.svc)) { $Matches.svc }
                else { continue }
        $info = $ServiceCatalogue[$name]
        $existing = Get-TrackedRunningInstance -Name $name -Info $info -InstanceKey $instanceKey -RawValue $property.Value -FallbackPort $info.Port
        if (-not $existing -or $existing.Port -le 0) { continue }
        if ($runningOwners.ContainsKey($existing.Port)) {
            throw "[port] $instanceKey 与 $($runningOwners[$existing.Port]) 的运行记录占用同一端口 $($existing.Port),请核对实例。"
        }
        $runningOwners[$existing.Port] = $instanceKey
        # 活实例可能仍在初始化,尚未 LISTEN。必须在任何新分配之前预留它的实际端口,
        # 并覆盖同端口的原始计划占位;后续新实例会避让或明确报错,不会抢占该端口。
        $ReservedPorts[$existing.Port] = $instanceKey
    }
}

function ConvertTo-ServiceProcessArgument {
    param([AllowEmptyString()][Parameter(Mandatory)][string]$Value)
    if($Value.Contains([char]0) -or $Value.Contains("`r") -or $Value.Contains("`n")){throw '启动参数不能包含NUL或换行'}
    # Start-Process会把数组重新拼成命令行，完整私有路径必须保留为一个argv。
    $escaped=[regex]::Replace($Value,'(\\*)"','${1}${1}\"')
    $escaped=[regex]::Replace($escaped,'(\\+)$','${1}${1}')
    return '"'+$escaped+'"'
}

function Start-ServiceInstance {
    param(
        [string]$Name,
        [hashtable]$Info,
        [int]$Index,
        [int]$Total,
        [pscustomobject]$Pids,
        [switch]$UseExe,
        [hashtable]$ReservedPorts = @{}
    )

    $svcDir = Join-Path $GoRoot $Info.Dir
    $instanceKey = Get-InstanceKey -Name $Name -Index $Index -Total $Total

    # 必须先确认能否复用再派生配置,否则重复 start 会改写仍在运行实例的 yaml。
    if ($Pids.PSObject.Properties.Name -contains $instanceKey) {
        $existing = Get-TrackedRunningInstance -Name $Name -Info $Info -InstanceKey $instanceKey -RawValue $Pids.$instanceKey -FallbackPort (Get-InstancePort -Info $Info -Index $Index)
        if ($existing) {
            $ReservedPorts[$existing.Port] = $instanceKey
            Write-Host "[skip]  $instanceKey (PID $($existing.Pid) already running on :$($existing.Port))" -ForegroundColor Yellow
            return $null
        }
    }
    $cfg = Resolve-InstanceConfig -Name $Name -Info $Info -Index $Index -Total $Total -ReservedPorts $ReservedPorts
    $writerEnvironment=$null
    $bootMaintenanceArgs=@()
    if ($Name -in @('db','data_service','login','chat','friend','guild','mail','rank','match','trade','player_inbox')) {
        $selectedConfig=''
        $writerEnvironment=Get-SqlWriterBootEnvironment -InstanceKey $instanceKey -RepoRoot $RepoRoot -ServiceName $Name -SelectedRuntimeConfigFile ([ref]$selectedConfig) -PreparedRuntimeArguments ([ref]$bootMaintenanceArgs) -PrepareOnlySupported:($Name -in @('db','data_service','login','chat','friend','guild','mail','rank','match','trade','player_inbox'))
        $listen=Get-YamlScalar -Path $selectedConfig -KeyPath 'ListenOn'
        if($listen -cnotmatch '^(?:\[[0-9A-Fa-f:]+\]|[A-Za-z0-9_.-]+):(?<port>[1-9][0-9]{0,4})$' -or [int]$Matches.port -ne $cfg.Port){throw '每boot完整配置的ListenOn与本次实例固定端口不符，不能写回或挪用别人的配置'}
        if($Zone -gt 0 -and [int](Get-YamlScalar -Path $selectedConfig -KeyPath 'ZoneId') -ne $Zone){throw '每boot完整配置ZoneId与本次实例不符'}
        $cfg.Path=$selectedConfig;$cfg.FullPath=$selectedConfig;$cfg.Derived=$false
    }else{Assert-PublicationStartAllowed -RepoRoot $RepoRoot}
    $logOut  = Join-Path $LogDir "$instanceKey.stdout.log"
    $logErr  = Join-Path $LogDir "$instanceKey.stderr.log"

    $configFlag = $Info.ConfigFlag
    $configArg  = $cfg.Path
    $maintenanceArgs=$bootMaintenanceArgs
    if($MaintenanceDrainPlan -or $MaintenanceDrainDir -or $MaintenanceDrainSha256){
        if($Name -notin @('db','login')){throw '该服务没有正式runtime drain入口，不能把未知flag当维护闭合'}
        if(-not [IO.Path]::IsPathRooted($MaintenanceDrainPlan) -or -not [IO.Path]::IsPathRooted($MaintenanceDrainDir) -or $MaintenanceDrainSha256 -cnotmatch '^[a-f0-9]{64}$' -or (Get-MaintenanceSha256 -Path $MaintenanceDrainPlan) -cne $MaintenanceDrainSha256){throw 'runtime drain Receipt/私有目录/rawSHA必须显式完整绑定'}
        if(-not [IO.Directory]::Exists($MaintenanceDrainDir) -or (Get-Item -LiteralPath $MaintenanceDrainDir).Attributes -band [IO.FileAttributes]::ReparsePoint){throw 'runtime drain目录缺失或为重解析点'}
        $explicit=@('-maintenance-drain-plan',[IO.Path]::GetFullPath($MaintenanceDrainPlan),'-maintenance-drain-dir',[IO.Path]::GetFullPath($MaintenanceDrainDir),'-maintenance-drain-sha256',$MaintenanceDrainSha256)
        if($bootMaintenanceArgs.Count -ne 0 -and ($bootMaintenanceArgs -join "`n") -cne ($explicit -join "`n")){throw '显式维护参数不能替换同新boot已固定prepare-only原件'}
        $maintenanceArgs=$explicit
    }


    # 资产通道签名密钥(规格 §4.32,90 清单 G-04 指派 B5b)。guild 的 etc/guild.yaml 打开了 AssetOp,
    # 启动时读不到 ≥32 字节的 MMORPG_ASSET_OP_SECRET_GUILD 即拒启;值取自 run/secrets/assetop-dev.env
    # (由 lib 首次随机生成,与 scene 同一把),**只在没设时兜底**、不回显值、finally 里还原。
    # **只对 guild 注入,且只给 guild 这一把**:lib 会把两把都补进环境,trade 那把在启动 guild 前摘掉,
    # 按最小权限给(AGENTS §11.3,同 cpp_nodes.ps1 只给 scene 注入)。trade 的注入不在本批范围。
    # 仅限本机 dev:预发 / 生产的密钥由部署侧注入(k8s_deploy.ps1 Resolve-InjectedSecret -MinLength 32)。
    $injectAssetOpSecret = ($Name -eq 'guild')
    $prevAssetOpSecrets = $null
    $previousAccountDirectorySecrets = $null
    # MailAdmin admin 生产者签名密钥(docs/design/mail-system.md §2.3):**只对 mail 注入、只给这一把**;值取自 run/secrets/mail-admin-producer-dev.env
    # (lib 首次随机生成,与本机 robot mail-smoke / cmd/mailadmin 读同一份),只在没设时兜底、不回显值、finally 里还原。
    # 仅限本机 dev:预发 / 生产由 k8s_deploy.ps1 经 Secret domain-service-secrets 的 mail-producer-admin 注入,部署链从不调用这里。
    # lib 函数只在 mail 分支里调用:go_services_ports.tests.ps1 按 AST 只加载本文件的函数(不执行顶层 dot-source)再起 chat / trade,
    # 无条件调用会让那些用例报「命令未找到」。
    $injectMailAdminToken = ($Name -eq 'mail')
    $prevMailAdminToken = $null
    $savedWriterEnvironment = $null
    $savedChatDsn = [Environment]::GetEnvironmentVariable('CHAT_AUTHORITY_DSN', 'Process')
	$savedMatchMigrationUser = [Environment]::GetEnvironmentVariable('MATCH_MIGRATION_USER', 'Process')
	$savedMatchMigrationPassword = [Environment]::GetEnvironmentVariable('MATCH_MIGRATION_PASSWORD', 'Process')
	$savedDatabaseAllowlist = [Environment]::GetEnvironmentVariable('DB_ALLOWED_DATABASES','Process')
    try {
        if($null -ne $writerEnvironment){$savedWriterEnvironment=Enter-SqlWriterBootEnvironment -Environment $writerEnvironment}
        if ($Name -eq 'chat') { $null = Get-LocalChatAuthoritySettings -ConfigPath $cfg.FullPath -RequireDsn }
		# 显式迁移CLI的凭据不能被任意常驻服务顺带继承。
		[Environment]::SetEnvironmentVariable('MATCH_MIGRATION_USER', $null, 'Process')
		[Environment]::SetEnvironmentVariable('MATCH_MIGRATION_PASSWORD', $null, 'Process')
		if ($Name -in @('data_service','db') -and [string]::IsNullOrWhiteSpace($savedDatabaseAllowlist)) {
			$declaredDatabases = [Environment]::GetEnvironmentVariable('MMORPG_DB_ALLOWED_DATABASES','Process')
			if ([string]::IsNullOrWhiteSpace($declaredDatabases)) { throw '全球DS/DB必须显式提供同一DB_ALLOWED_DATABASES（或MMORPG_DB_ALLOWED_DATABASES），不得从当前Zone推导玩家库白名单。' }
			[Environment]::SetEnvironmentVariable('DB_ALLOWED_DATABASES',$declaredDatabases,'Process')
		}
        # 聊天SQL账号仅交给chat；其它本机服务不得顺带继承此能力。
        if ($Name -ne 'chat') { [Environment]::SetEnvironmentVariable('CHAT_AUTHORITY_DSN', $null, 'Process') }
        $accountDirectoryService = $Name.Replace('_','-')
        if ($accountDirectoryService -in @('data-service','login','trade','match','scene-manager')) {
            $previousAccountDirectorySecrets = Backup-AccountDirectoryDevSecrets -Service $accountDirectoryService
            Initialize-AccountDirectoryDevSecrets -RepoRoot $RepoRoot -Service $accountDirectoryService
        }
        if ($injectAssetOpSecret) {
            # Backup 必须在 try 之内:Initialize 抛错时环境变量可能已被改了一半,放在外面就进不了 finally。
            $prevAssetOpSecrets = Backup-AssetOpDevSecrets
            Initialize-AssetOpDevSecrets -RepoRoot $RepoRoot | Out-Null
            Remove-Item Env:MMORPG_ASSET_OP_SECRET_TRADE -ErrorAction SilentlyContinue
        }
        if ($injectMailAdminToken) {
            $prevMailAdminToken = Backup-MailAdminDevToken
            Initialize-MailAdminDevToken -RepoRoot $RepoRoot | Out-Null
        }

        # 下面两处 Start-Process 必须保持每个实例一个独立(隐藏)控制台:-WindowStyle Hidden,且不得加 -NoNewWindow。
        # 优雅停(Invoke-Stop)是往目标的控制台投递 Ctrl+C,而 Ctrl+C 会送到附着在同一控制台上的全部进程;
        # 共用控制台时停一个服务会把同控制台的其它服务、甚至本脚本所在的终端一起打断。
        # tools/scripts/tests/go_services_stop.tests.ps1 有契约用例钉住这一点。
        if ($UseExe) {
            $exePath = Resolve-GoExecutablePath -ServiceName $Name -ServiceDir $svcDir
            if (-not $exePath) {
                Write-Warning "Skipping '$instanceKey': executable not found. Run -Command build first or place $Name.exe in bin\go_services\ or $svcDir."
                return $null
            }

            Write-Host "[start-exe] $instanceKey  :$($cfg.Port)  ($($Info.Desc))" -ForegroundColor Cyan

            $proc = Start-Process -FilePath $exePath `
                -ArgumentList (@(@($configFlag, $configArg)+$maintenanceArgs | ForEach-Object {ConvertTo-ServiceProcessArgument -Value $_})) `
                -WorkingDirectory $svcDir `
                -RedirectStandardOutput $logOut `
                -RedirectStandardError  $logErr `
                -PassThru `
                -WindowStyle Hidden
        }
        else {
            Write-Host "[start] $instanceKey  :$($cfg.Port)  ($($Info.Desc))" -ForegroundColor Cyan

            $proc = Start-Process -FilePath "go" `
                -ArgumentList (@(@("run", (Join-Path $svcDir $Info.Entry), $configFlag, $configArg)+$maintenanceArgs | ForEach-Object {ConvertTo-ServiceProcessArgument -Value $_})) `
                -WorkingDirectory $svcDir `
                -RedirectStandardOutput $logOut `
                -RedirectStandardError  $logErr `
                -PassThru `
                -WindowStyle Hidden
        }
    } finally {
        Restore-SqlWriterBootEnvironment -Saved $savedWriterEnvironment
		[Environment]::SetEnvironmentVariable('MATCH_MIGRATION_USER', $savedMatchMigrationUser, 'Process')
		[Environment]::SetEnvironmentVariable('MATCH_MIGRATION_PASSWORD', $savedMatchMigrationPassword, 'Process')
		[Environment]::SetEnvironmentVariable('DB_ALLOWED_DATABASES',$savedDatabaseAllowlist,'Process')
        [Environment]::SetEnvironmentVariable('CHAT_AUTHORITY_DSN', $savedChatDsn, 'Process')
        # 没注入过就没什么可还原的($prevAssetOpSecrets 仍是 $null);还原同时把摘掉的 trade 那把放回去。
        if ($injectAssetOpSecret) { Restore-AssetOpDevSecrets $prevAssetOpSecrets }
        Restore-AccountDirectoryDevSecrets $previousAccountDirectorySecrets
        if ($injectMailAdminToken) { Restore-MailAdminDevToken $prevMailAdminToken }
    }

    $entry = [pscustomobject]@{
        Pid     = $proc.Id
        Port    = $cfg.Port
        Service = $Name
        StartTimeUtcTicks = $proc.StartTime.ToUniversalTime().Ticks
        ExecutablePath = $proc.Path
        ExecutableSha256 = Get-MaintenanceSha256 -Path $proc.Path
        AllowChildProcess = -not $UseExe
    }
    $Pids | Add-Member -NotePropertyName $instanceKey -NotePropertyValue $entry -Force
    if ($cfg.Derived) {
        Write-Host "        PID $($proc.Id)  cfg -> $($cfg.FullPath)" -ForegroundColor DarkGray
    }
    Write-Host "        logs -> run\logs\go_services\$instanceKey.*.log" -ForegroundColor DarkGray

    return [pscustomobject]@{
        InstanceKey = $instanceKey
        LogFile     = $logOut
        Port        = $cfg.Port
        ProcessId   = $proc.Id
        AllowChildProcess = -not $UseExe
        Tier        = if ($Info.ContainsKey('Tier')) { [int]$Info.Tier } else { 99 }
    }
}

function Invoke-Start {
    param(
        [switch]$UseExe
    )

    $names = Resolve-ServiceList -Requested $Services
    $pids  = Read-PidFile
    $launchedServices = @()
    $reservedPorts = @{}

    if (-not (Test-Path $LogDir)) { New-Item -ItemType Directory -Path $LogDir -Force | Out-Null }

    # Build a flat plan of (name, index, total, info, tier) entries. Validation
    # (multi-instance allow, missing dirs) happens here so we can still group
    # by tier afterwards without re-scanning.
    $plan = @()
    foreach ($name in $names) {
        $info = $ServiceCatalogue[$name]
        $svcDir = Join-Path $GoRoot $info.Dir
        if (-not (Test-Path $svcDir)) {
            Write-Warning "Skipping '$name': directory $svcDir does not exist."
            continue
        }
        $count = Get-ServiceInstanceCount -Name $name
        $tier  = if ($info.ContainsKey('Tier')) { [int]$info.Tier } else { 99 }
        for ($i = 1; $i -le $count; $i++) {
            $instanceKey = Get-InstanceKey -Name $name -Index $i -Total $count
            $requestedPort = Get-InstancePort -Info $info -Index $i
            if ($reservedPorts.ContainsKey($requestedPort)) {
                throw "[port] $instanceKey 与 $($reservedPorts[$requestedPort]) 计划使用同一原始端口 $requestedPort,请核对配置。"
            }
            # 提前预留后续实例的原始端口,前面的保留区间避让不能抢占它。
            $reservedPorts[$requestedPort] = $instanceKey
            $plan += [pscustomobject]@{
                Name = $name; Info = $info; Index = $i; Total = $count; Tier = $tier
            }
        }
    }

    if ($plan.Count -eq 0) {
        Write-Host "No services to launch." -ForegroundColor Yellow
        return
    }
    # 包括未请求的已运行实例,分批启动同样不能抢走其初始化阶段的端口。
    Reserve-RunningInstancePorts -Pids $pids -ReservedPorts $reservedPorts

    if ($NoTier) {
        # Legacy parallel path: launch every instance back-to-back, then wait
        # for startup banners at the end. Useful when caller knows there is no
        # cross-service dial dependency to honor.
        foreach ($p in $plan) {
            $launched = Start-ServiceInstance -Name $p.Name -Info $p.Info -Index $p.Index -Total $p.Total -Pids $pids -UseExe:$UseExe -ReservedPorts $reservedPorts
            if ($launched) { $launchedServices += $launched }
        }
        Write-PidFile $pids
        if ($launchedServices.Count -gt 0) {
            Write-Host "`nWaiting for startup confirmation..." -ForegroundColor DarkGray
            foreach ($svc in $launchedServices) {
                Wait-ForStartupBanner -LogFile $svc.LogFile -ServiceName $svc.InstanceKey
            }
        }
        Write-Host "`nAll requested services launched. Use -Command status to check health." -ForegroundColor Green
        return
    }

    # Tier-staged path. Group plan entries by tier ascending. Within a tier we
    # launch all instances in parallel, then issue a TCP LISTEN probe for each
    # before moving on. The probe is a soft gate: timeout warns and continues
    # so a stuck service never blocks the whole batch.
    $tiers = $plan | Group-Object -Property Tier | Sort-Object { [int]$_.Name }
    foreach ($tierGroup in $tiers) {
        $tierName = $tierGroup.Name
        $entries  = $tierGroup.Group
        Write-Host "`n--- Tier $tierName ($($entries.Count) instance(s)) ---" -ForegroundColor Cyan

        $launchedThisTier = @()
        foreach ($p in $entries) {
            $launched = Start-ServiceInstance -Name $p.Name -Info $p.Info -Index $p.Index -Total $p.Total -Pids $pids -UseExe:$UseExe -ReservedPorts $reservedPorts
            if ($launched) {
                $launchedServices += $launched
                $launchedThisTier += $launched
            }
        }

        # Persist PIDs after each tier so a Ctrl+C between tiers does not lose state.
        Write-PidFile $pids

        if ($launchedThisTier.Count -gt 0) {
            Write-Host "  waiting up to ${TierReadySeconds}s for tier $tierName to LISTEN..." -ForegroundColor DarkGray
            foreach ($svc in $launchedThisTier) {
                $ok = Wait-TcpListenReady -Port $svc.Port -TimeoutSeconds $TierReadySeconds -ProcessId $svc.ProcessId -AllowChildProcess:$svc.AllowChildProcess
                if ($ok) {
                    Write-Host "  [ready]   $($svc.InstanceKey) :$($svc.Port)" -ForegroundColor Green
                } else {
                    Write-Host "  [warn]    $($svc.InstanceKey) :$($svc.Port) not LISTEN within ${TierReadySeconds}s; continuing anyway" -ForegroundColor Yellow
                }
            }
        }
    }

    if ($launchedServices.Count -gt 0) {
        Write-Host "`nWaiting for startup banners..." -ForegroundColor DarkGray
        foreach ($svc in $launchedServices) {
            Wait-ForStartupBanner -LogFile $svc.LogFile -ServiceName $svc.InstanceKey
        }
    }

    Write-Host "`nAll requested services launched. Use -Command status to check health." -ForegroundColor Green
}

# ── Helpers (graceful stop) ──────────────────────────────────────────
# 下面几个函数只读参数与本脚本的 param(-StopTimeoutSeconds),不依赖其它脚本级变量;
# 测试经 AST 只加载函数,并把 Send-ConsoleCtrlC / Wait-ProcessExit / Stop-ProcessTree / Get-StopClockSeconds 换成桩。

# 某个实例 Ctrl+C 之后等多久:-StopTimeoutSeconds >0 优先,其次目录键 StopTimeoutSeconds,默认 25。
# $Info 为 $null(PID 记录里的服务已不在目录中)时取默认值。
function Get-GracefulStopSeconds {
    param([hashtable]$Info)
    if ($StopTimeoutSeconds -gt 0) { return [int]$StopTimeoutSeconds }
    if ($null -ne $Info -and $Info.ContainsKey('StopTimeoutSeconds') -and [int]$Info.StopTimeoutSeconds -gt 0) {
        return [int]$Info.StopTimeoutSeconds
    }
    return 25
}

# 辅助进程执行的脚本文本:脱离自己的控制台 → 附着到目标进程的控制台 → 让自己忽略 Ctrl+C →
# 向该控制台的全部进程(进程组 0)投递 CTRL_C_EVENT。PID 以 [int] 形式嵌入文本,没有注入面。
# 退出码:0 = 已投递;2 = AttachConsole 失败(目标已退出 / 没有控制台 / 权限不足);
#        3 = GenerateConsoleCtrlEvent 失败;其它非 0(如 Add-Type 编译失败)同样视为没投递出去。
function Get-ConsoleCtrlCHelperScript {
    param([Parameter(Mandatory = $true)][int]$ProcessId)
    return @"
`$ErrorActionPreference = 'Stop'
Add-Type -Namespace MmorpgConsole -Name Native -MemberDefinition @'
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool FreeConsole();
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool AttachConsole(uint dwProcessId);
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool SetConsoleCtrlHandler(IntPtr handlerRoutine, bool add);
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool GenerateConsoleCtrlEvent(uint dwCtrlEvent, uint dwProcessGroupId);
'@
[void][MmorpgConsole.Native]::FreeConsole()
if (-not [MmorpgConsole.Native]::AttachConsole($ProcessId)) { exit 2 }
[void][MmorpgConsole.Native]::SetConsoleCtrlHandler([IntPtr]::Zero, `$true)
if (-not [MmorpgConsole.Native]::GenerateConsoleCtrlEvent(0, 0)) { exit 3 }
exit 0
"@
}

# 用当前宿主 exe(pwsh / powershell)起一个隐藏的短命辅助进程投递 Ctrl+C,返回是否投递成功。
# 不能在本进程里做:AttachConsole 要先 FreeConsole,会把本脚本从用户的终端上摘下来。
# 辅助进程正常 1~2 秒结束(主要是 Add-Type 编译);给 20 秒上限,超时杀掉辅助进程并按「没投递出去」处理,
# 调用方随即强杀目标,不会让 stop 无限挂住。
function Send-ConsoleCtrlC {
    param([Parameter(Mandatory = $true)][int]$ProcessId)
    $hostPath = (Get-Process -Id $PID).Path
    if ([string]::IsNullOrWhiteSpace($hostPath)) {
        Write-Warning "cannot resolve the current PowerShell executable; console interrupt for PID $ProcessId not sent"
        return $false
    }
    $helperScript = Get-ConsoleCtrlCHelperScript -ProcessId $ProcessId
    $encoded = [Convert]::ToBase64String([System.Text.Encoding]::Unicode.GetBytes($helperScript))
    try {
        $helper = Start-Process -FilePath $hostPath `
            -ArgumentList @('-NoProfile', '-NonInteractive', '-EncodedCommand', $encoded) `
            -WindowStyle Hidden `
            -PassThru
        # 先取一次句柄:进程退出后 Process 对象才能读到 ExitCode(Start-Process -PassThru 的已知坑)。
        $null = $helper.Handle
        if (-not $helper.WaitForExit(20000)) {
            Stop-Process -Id $helper.Id -Force -ErrorAction SilentlyContinue
            Write-Warning "console interrupt helper for PID $ProcessId did not finish within 20s"
            return $false
        }
        return ($helper.ExitCode -eq 0)
    } catch {
        Write-Warning "console interrupt helper for PID $ProcessId failed: $($_.Exception.Message)"
        return $false
    }
}

# 等进程退出,返回是否已退出。进程查不到(已退出)直接返回 $true。
function Wait-ProcessExit {
    param([Parameter(Mandatory = $true)][int]$ProcessId, [double]$TimeoutSeconds = 0)
    $proc = Get-Process -Id $ProcessId -ErrorAction SilentlyContinue
    if (-not $proc) { return $true }
    $timeoutMs = [int][Math]::Min([double][int]::MaxValue, [Math]::Max(0.0, $TimeoutSeconds * 1000.0))
    return [bool]$proc.WaitForExit($timeoutMs)
}

# 强杀整棵进程树。go run 模式下记录的 PID 是 go.exe,真正的服务是它的子进程,只杀 go.exe 不一定连带杀掉子进程。
# taskkill 失败(进程已不在 / 权限)时退回只杀该 PID,保持旧行为的下限。
function Stop-ProcessTree {
    param([Parameter(Mandatory = $true)][int]$ProcessId)
    $killed = $false
    try {
        $null = & taskkill.exe /PID $ProcessId /T /F 2>&1
        $killed = ($LASTEXITCODE -eq 0)
    } catch {
        $killed = $false
    }
    if (-not $killed) {
        Stop-Process -Id $ProcessId -Force -ErrorAction SilentlyContinue
    }
}

# 单调时钟(秒),只用于算等待剩余时间;测试里替换成桩。
function Get-StopClockSeconds {
    return [double][System.Diagnostics.Stopwatch]::GetTimestamp() / [double][System.Diagnostics.Stopwatch]::Frequency
}

# 停止本脚本启动的实例。默认优雅停,分三段(G9,docs/design/db-task-pipeline-bottlenecks.md §4.4):
#   1. 对全部存活目标投递 Ctrl+C(-ForceStop 时直接强杀);投递不出去的立刻强杀进程树。
#   2. 按各自的截止时间逐个等退出(Ctrl+C 是一起发的,所以等待是并行计时的);超时强杀进程树。
#   3. 已退出或查不到的实例照旧输出 [gone]。
# 为什么是 Ctrl+C:go-zero 的 proc 包在 Windows 上是空实现,服务端也没有停机端点;Go 运行时把 CTRL_C_EVENT
# 当作 os.Interrupt(= syscall.SIGINT)交给服务自己的 signal.Notify(如 go/db/db.go),这是唯一能走到
# 服务自身收尾逻辑的办法。旧行为 Stop-Process -Force 对服务而言等同崩溃。
# go run 模式下 go.exe 收到中断后不退出,等子进程(真正的服务)退出后才退出,所以等 go.exe 就是等服务。
# 依赖每个实例有独立控制台,见 Start-ServiceInstance 里 Start-Process 处的注释。
function Invoke-Stop {
    $pids = Read-PidFile
    if (@($pids.PSObject.Properties).Count -eq 0) {
        Write-Host "No tracked services to stop." -ForegroundColor Yellow
        return
    }

    # Build the target instance key list. -Services filters by base service name
    # and expands to every running instance of that service (e.g. login_1, login_2).
    if ($Services.Count -gt 0) {
        $targetServices = Resolve-ServiceList -Requested $Services
        $instanceKeys = @()
        foreach ($prop in $pids.PSObject.Properties) {
            $info = if ($ServiceCatalogue.Contains($prop.Name)) { $ServiceCatalogue[$prop.Name] } else { $null }
            $port = if ($info) { $info.Port } else { 0 }
            $entry = Read-InstanceEntry -RawValue $prop.Value -FallbackService $prop.Name -FallbackPort $port
            if ($entry -and ($targetServices -contains $entry.Service)) {
                $instanceKeys += $prop.Name
            }
        }
    } else {
        $instanceKeys = @($pids.PSObject.Properties.Name)
    }

    # 第一段:先对全部目标投递 Ctrl+C,再统一等待 —— 逐个「发、等」会让总时长变成各服务等待之和。
    $pending = @()
    foreach ($key in $instanceKeys) {
        if ($pids.PSObject.Properties.Name -notcontains $key) { continue }
        $entry = Read-InstanceEntry -RawValue $pids.$key -FallbackService $key -FallbackPort 0
        $procId = $entry.Pid
        $proc = $null
        try { $proc = Get-Process -Id $procId -ErrorAction Stop } catch { $proc = $null }
        if (-not $proc) {
            Write-Host "[gone]  $key (PID $procId not found)" -ForegroundColor DarkGray
        } elseif ($proc.HasExited) {
            Write-Host "[gone]  $key (PID $procId already exited)" -ForegroundColor DarkGray
        } elseif ($ForceStop) {
            Stop-ProcessTree -ProcessId $procId
            Write-Host "[kill]  $key (PID $procId): -ForceStop; force-killed process tree" -ForegroundColor Magenta
        } elseif (-not (Send-ConsoleCtrlC -ProcessId $procId)) {
            Stop-ProcessTree -ProcessId $procId
            Write-Host "[kill]  $key (PID $procId): console interrupt not deliverable; force-killed process tree" -ForegroundColor Red
        } else {
            # 等待时限按服务取:PID 记录里的 Service 优先;旧格式记录(只有 PID)从实例键里剥掉 z<N>_ 前缀与 _<i> 后缀。
            $svcName = [string]$entry.Service
            if (-not $ServiceCatalogue.Contains($svcName) -and $key -match '^(?:z\d+_)?(?<svc>.+?)(?:_\d+)?$' -and $ServiceCatalogue.Contains($Matches.svc)) {
                $svcName = $Matches.svc
            }
            $svcInfo = if ($ServiceCatalogue.Contains($svcName)) { $ServiceCatalogue[$svcName] } else { $null }
            $graceSeconds = Get-GracefulStopSeconds -Info $svcInfo
            $sentAt = Get-StopClockSeconds
            $pending += [pscustomobject]@{
                Key = $key; Pid = $procId; GraceSeconds = $graceSeconds; SentAt = $sentAt; Deadline = $sentAt + $graceSeconds
            }
        }
        # 只改内存里的记录;PID 文件在最后统一写回。中途被打断时文件不变,下次 stop 仍能找到这些实例。
        $pids.PSObject.Properties.Remove($key)
    }

    # 第二段:按各自截止时间的剩余量逐个等待,超时强杀进程树。
    foreach ($p in $pending) {
        $remaining = [Math]::Max(0.0, $p.Deadline - (Get-StopClockSeconds))
        if (Wait-ProcessExit -ProcessId $p.Pid -TimeoutSeconds $remaining) {
            $elapsed = [Math]::Max(0.0, (Get-StopClockSeconds) - $p.SentAt)
            $elapsedText = ([Math]::Round($elapsed, 1)).ToString([System.Globalization.CultureInfo]::InvariantCulture)
            Write-Host "[stop]  $($p.Key) (PID $($p.Pid), exited in ${elapsedText}s)" -ForegroundColor Magenta
        } else {
            Stop-ProcessTree -ProcessId $p.Pid
            Write-Host "[kill]  $($p.Key) (PID $($p.Pid)): no exit within $($p.GraceSeconds)s after Ctrl+C; force-killed process tree" -ForegroundColor Red
        }
    }

    Write-PidFile $pids
}

function Invoke-Status {
    $pids = Read-PidFile
    if (@($pids.PSObject.Properties).Count -eq 0) {
        Write-Host "No tracked services." -ForegroundColor Yellow
        return
    }
    Write-Host ("{0,-22} {1,-8} {2,-8} {3}" -f "INSTANCE", "PID", "PORT", "STATUS") -ForegroundColor White
    Write-Host ("{0,-22} {1,-8} {2,-8} {3}" -f "--------", "---", "----", "------")
    foreach ($prop in $pids.PSObject.Properties) {
        $key = $prop.Name
        # Derive base service name for catalogue lookup. Strips optional zone
        # prefix (z<N>_) and optional instance suffix (_<i>):
        #   login        -> login
        #   login_2      -> login
        #   z1_login     -> login
        #   z2_login_3   -> login
        $baseName = if ($ServiceCatalogue.Contains($key)) { $key }
                    elseif ($key -match '^(?:z\d+_)?(?<svc>.+?)(?:_\d+)?$' -and $ServiceCatalogue.Contains($Matches.svc)) { $Matches.svc }
                    else { $key }
        $basePort = if ($ServiceCatalogue.Contains($baseName)) { $ServiceCatalogue[$baseName].Port } else { 0 }
        $entry    = Read-InstanceEntry -RawValue $prop.Value -FallbackService $baseName -FallbackPort $basePort
        $procId   = $entry.Pid
        $port     = if ($entry.Port -gt 0) { $entry.Port } else { $basePort }
        $status = "UNKNOWN"
        try {
            $proc = Get-Process -Id $procId -ErrorAction Stop
            $status = if ($proc.HasExited) { "EXITED" } else { "RUNNING" }
        } catch {
            $status = "GONE"
        }
        $color = switch ($status) { "RUNNING" { "Green" } "EXITED" { "Red" } default { "DarkGray" } }
        Write-Host ("{0,-22} {1,-8} {2,-8} {3}" -f $key, $procId, $port, $status) -ForegroundColor $color
    }
}

function Invoke-List {
    Write-Host "`nAvailable Go services:`n" -ForegroundColor Cyan
    Write-Host ("{0,-18} {1,-8} {2}" -f "NAME", "PORT", "DESCRIPTION") -ForegroundColor White
    Write-Host ("{0,-18} {1,-8} {2}" -f "----", "----", "-----------")
    foreach ($kv in $ServiceCatalogue.GetEnumerator()) {
        Write-Host ("{0,-18} {1,-8} {2}" -f $kv.Key, $kv.Value.Port, $kv.Value.Desc)
    }
    Write-Host ""
}

# 本地 exe 注入与 deploy/k8s/Dockerfile.go-svc 同一组 go/shared/buildinfo 变量,服务启动首行
# `service starting ... version= commit=` 才能自报版本。-X 目标不存在时链接器静默忽略、构建照样成功,
# 所以 buildinfo 的变量名 / 包路径改动时这里与 Dockerfile.go-svc 必须同步,否则版本号悄悄变回 dev / unknown。
function Get-GoBuildInfoLdflags {
    # release_common.ps1 只定义函数与常量表,dot-source 无副作用;放在函数内,只有 build 命令才加载。
    . (Join-Path $ScriptDir "lib/release_common.ps1")

    $stamp = Get-GitReleaseStamp -RepoRoot $RepoRoot

    # 版本号只来自发布流程(MMORPG_RELEASE_VERSION),本地随手构建一律 dev,不从 git tag 猜。
    # 与 go_svc_image.ps1 同口径 fail-closed:挂发布版本号的产物必须能从一个干净 commit 还原,
    # 否则 exe 自报 v1.2.3、里面却是未提交的代码。环境变量多半是发布会话残留的,本地调试先清掉它。
    $version = "dev"
    if (-not [string]::IsNullOrWhiteSpace($env:MMORPG_RELEASE_VERSION)) {
        $releaseVersion = $env:MMORPG_RELEASE_VERSION
        $versionCheck = Test-ReleaseVersion -Version $releaseVersion
        if (-not $versionCheck.Ok) { throw "环境变量 MMORPG_RELEASE_VERSION 不合法:$($versionCheck.Reason)" }
        if (-not $stamp.Ok) {
            throw "发布版本 $releaseVersion 需要可追溯的 git commit:$($stamp.Reason)。本地调试构建请先 Remove-Item Env:MMORPG_RELEASE_VERSION。"
        }
        if ($stamp.Dirty) {
            throw "发布版本 $releaseVersion 不接受脏工作树(git status 非空):脏树产物无法从 commit $($stamp.Commit) 还原。请先提交或清理工作树;本地调试构建请先 Remove-Item Env:MMORPG_RELEASE_VERSION。"
        }
        $version = $releaseVersion
    }

    # 脏树带 -dirty:本地 exe 常从未提交的代码编出来,commit 不标脏会让人误以为跑的就是那个 commit。
    # 取不到 git 戳时注入 unknown:本脚本按文件构建(go build ./<entry>.go),Go 不给这种构建嵌 vcs.*,
    # buildinfo 没有可回落的来源,启动行如实显示 commit=unknown(见 go/shared/buildinfo 包注释)。
    $commit = "unknown"
    $buildTime = "unknown"
    if ($stamp.Ok) {
        $commit = if ($stamp.Dirty) { "$($stamp.Commit)-dirty" } else { $stamp.Commit }
        # BuildTime 取 HEAD 的提交时间(UTC),不取墙钟:-X 的值参与 go 的链接缓存键,墙钟每秒都变,
        # 会让没改代码的服务也全部重新链接、重写 exe —— 空构建多耗链接时间,服务正在跑、exe 被占用时必然 [FAIL]。
        # 口径同 SOURCE_DATE_EPOCH 惯例,也与 buildinfo 回落时拿 vcs.time(同为提交时间)当 BuildTime 一致。
        # 代价:脏树 exe 的 build_time 是 HEAD 提交时间而非实际编译时间,要看编译时间请看 exe 文件修改时间。
        $commitEpoch = (& git -C $RepoRoot show -s --format=%ct HEAD 2>$null)
        $epochSeconds = 0L
        if ($LASTEXITCODE -eq 0 -and [long]::TryParse(("$commitEpoch").Trim(), [ref]$epochSeconds)) {
            # InvariantCulture:自定义格式里的 ':' 是"时间分隔符"占位,部分区域性(如 fi-FI)会换成 '.'。
            $buildTime = [DateTimeOffset]::FromUnixTimeSeconds($epochSeconds).UtcDateTime.ToString(
                "yyyy-MM-ddTHH:mm:ssZ", [System.Globalization.CultureInfo]::InvariantCulture)
        }
    }

    return "-X shared/buildinfo.Version=$version -X shared/buildinfo.Commit=$commit -X shared/buildinfo.BuildTime=$buildTime"
}

function Invoke-Build {
    $names = Resolve-ServiceList -Requested $Services
    $outDir = Join-Path $RepoRoot "bin\go_services"
    if (-not (Test-Path $outDir)) { New-Item -ItemType Directory -Path $outDir -Force | Out-Null }

    # 同一批构建共用一份版本戳(git 只查一次,整批 ldflags 相同)。
    $ldflags = Get-GoBuildInfoLdflags
    Write-Host "[stamp] $ldflags" -ForegroundColor DarkGray

    $failed = @()
    foreach ($name in $names) {
        $info   = $ServiceCatalogue[$name]
        $svcDir = Join-Path $GoRoot $info.Dir

        if (-not (Test-Path $svcDir)) {
            Write-Warning "Skipping '$name': directory $svcDir does not exist."
            $failed += $name
            continue
        }

        $outExe = Join-Path $outDir "$name.exe"
        Write-Host "[build] $name -> bin\go_services\$name.exe" -ForegroundColor Cyan

        Push-Location $svcDir
        try {
            # -trimpath:exe 里不留本机绝对路径(与镜像构建同口径,panic 栈按模块路径显示)。
            go build -trimpath -ldflags $ldflags -o $outExe "./$($info.Entry)"
            if ($LASTEXITCODE -ne 0) {
                Write-Host "[FAIL]  $name" -ForegroundColor Red
                $failed += $name
            } else {
                Write-Host "[ok]    $name" -ForegroundColor Green
            }
        } finally {
            Pop-Location
        }
    }

    if ($failed.Count -gt 0) {
        throw "Build failed for: $($failed -join ', ')"
    }

    Write-Host "`nAll requested services built -> bin\go_services\" -ForegroundColor Green
}

function Invoke-StopAndReport {
    if ($ForceStop) { throw '严格StopAndReport禁止ForceStop；不能将强杀签为排空' }
    if ([string]::IsNullOrWhiteSpace($MaintenancePlanPath) -or [string]::IsNullOrWhiteSpace($StopReportPath)) { throw '严格停止必须有固定维护计划和永久收集输出' }
    Invoke-LocalStopAndCollect -PlanFile $MaintenancePlanPath -ReportFile $StopReportPath -Family go -RepoRoot $RepoRoot -Tracked (Read-PidFile) -Signal { param($processId) Send-ConsoleCtrlC -ProcessId $processId }
}

# ── Dispatch ─────────────────────────────────────────────────────────
switch ($Command) {
    "start"      { Invoke-Start }
    "start-exe"  { Invoke-Start -UseExe }
    "stop"   { Invoke-Stop }
    "stop-and-report" { Invoke-StopAndReport }
    "signal-only" { if($SignalProcessId -lt 1){throw "信号PID非法"};if(-not (Send-ConsoleCtrlC -ProcessId $SignalProcessId)){throw "优雅停止信号未投递"} }
    "status" { Invoke-Status }
    "list"   { Invoke-List }
    "build"  { Invoke-Build }
}
