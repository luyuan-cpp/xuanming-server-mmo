package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/IBM/sarama"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"

	"shared/accountdirectory"
	"shared/idsegment"
	"shared/serviceauth"
)

// Config is the top-level configuration.
type Config struct {
	zrpc.RpcServerConf // Embedded go-zero RPC config (ListenOn, Etcd, etc.)
	// LegacyNonGlobal仅供显式dev/test历史夹具；缺省和普通dev配置仍须满足全球SQL契约。
	LegacyNonGlobal  bool               `json:"LegacyNonGlobal,optional"`
	Node             NodeConfig         `json:"Node"`
	Snowflake        SnowflakeConf      `json:"Snowflake"`
	Locker           LockerConf         `json:"Locker"`
	Account          AccountConf        `json:"Account"`
	Registry         RegistryConf       `json:"Registry"`
	Timeouts         TimeoutConf        `json:"Timeouts"`
	Kafka            KafkaConfig        `json:"Kafka"`
	PlayerLocatorRpc zrpc.RpcClientConf `json:"PlayerLocatorRpc"` // player_locator gRPC client
	SceneManagerRpc  zrpc.RpcClientConf `json:"SceneManagerRpc"`  // scene_manager gRPC client
	// DataServiceRpc 是 data_service 的 gRPC 客户端(etcd 发现,Key=dataservice.rpc)。
	// 全局账号目录、PlayerId号段、home_zone与名字登记共用。保留 optional 便于旧配置
	// 报明确的域不可用；未配置时账号目录 fail-closed，不使用 Redis 权威替身。
	DataServiceRpc zrpc.RpcClientConf `json:"DataServiceRpc,optional"`
	// IdSegment 控制 PlayerId 的号段发号(docs/design/node-id-overhaul-plan-20260908.md §6)。
	// 字段语义见 shared/idsegment.Conf。**没写这块 = 号段关闭 = 纯 snowflake 老路径**,
	// 这就是设计稿 §6.4 说的「保留一个版本做回滚开关」。
	IdSegment idsegment.Conf `json:"IdSegment,optional"`
	// GateTokenSecret 已废弃:单个 string 无法不停服轮换,而且 gate token 与
	// 排队 token 共用同一把密钥,信任域没拆。保留字段只为让 deploy/*.yaml 与
	// tools/scripts/k8s_deploy.ps1 这些**仓外**部署产物在过渡期仍能起服
	// (ResolveSecrets 会按兼容路径读它并打 WARN)。新配置请写 Secrets 块。
	GateTokenSecret string `json:"GateTokenSecret,optional"`
	// Secrets 是按用途隔离的 HMAC 密钥集合,支持三段式不停服轮换。
	// 详见 secrets.go 的包注释。
	Secrets SecretsConf `json:"Secrets,optional"`
	// InternalAuth 是内部调用方身份声明(x-session-detail-bin)的验签策略。
	InternalAuth InternalAuthConf `json:"InternalAuth,optional"`
	// KillSwitch 是 RPC 级热关停闸门(shared/killswitch)的配置。
	KillSwitch    KillSwitchConf `json:"KillSwitch,optional"`
	TableDir      string         `json:",default=../../generated/tables"`
	AuthProviders AuthConfig     `json:"AuthProviders,optional"` // Third-party auth provider config
	// ClusterId 是部署级集群号:PlayerId(bwmarrin 13 位 node 段)切成 [cluster3][slot10]
	// 的 cluster 部分(docs/design/node-id-overhaul-plan-20260908.md §5)。**运维按集群
	// 一次性设定,策划不碰**;默认 0 = 单集群 / 存量 PlayerId 布局。etcd 槽位前缀带
	// c<cluster>,两个集群共用一个 etcd 也不会撞号。必须 < 8。
	ClusterId uint32 `json:"ClusterId,default=0"`
	// SnowflakeCacheDir 是 PlayerId 槽位的本地缓存目录(shared/snowflakealloc,设计稿 §3.5):
	// 启动时 etcd 不可达、且缓存里的上次水位确认在 F(2h)内,就用缓存的槽起服并后台重试
	// 注册。留空关闭。默认相对服务工作目录(go/login/)指向仓库 run/(已 gitignore)。
	SnowflakeCacheDir string `json:"SnowflakeCacheDir,default=../../run/snowflake"`
	// DevSkipAuth 是已废弃的不安全开关。保留字段只为让旧配置在启动时
	// 明确失败,不能因为结构体删字段而静默忽略、让开发者误以为仍生效。
	DevSkipAuth     bool                `json:"DevSkipAuth,optional"`
	PasswordAuth    PasswordAuthConf    `json:"PasswordAuth,optional"`
	DevPasswordAuth DevPasswordAuthConf `json:"DevPasswordAuth,optional"`
	TokenConfig     TokenConf           `json:"TokenConfig,optional"` // Access/refresh token TTL settings
	PreloadPool     PreloadPoolConf     `json:"PreloadPool,optional"` // Background goroutine pool for player data preload

	// LegacyGateLoginEnabled controls whether the deprecated path
	// "Client → cpp gate (TCP) → ClientPlayerLogin.Login with password /
	// third-party credentials" is still served.
	//
	// Background: ARCH §12 T+2 step. The new path (Java Gateway POST
	// /api/login → gRPC login) has been the recommended route since
	// 2026-05; the legacy path is kept as backwards compatibility with
	// throttled deprecation telemetry (legacy_login_count counter).
	// Once that counter trends below the agreed threshold per T+1 exit
	// criteria, ops flips this flag to false and the legacy branch
	// returns kLoginUnknownError, forcing the few remaining old
	// clients to fall through to /api/login (or their own retry path,
	// which typically re-resolves via /api/assign-gate).
	//
	// 判定与弃用遥测同源(clientplayerlogin.classifyLoginPath):会话头带非零 SessionId
	// = 经 cpp gate HandleGrpcNodeMessage 转发。其中 auth_type="access_token" 是新流程
	// (/api/login → assign-gate → gate 上 Login{access_token})绑定会话的必经一步,
	// 是 login_session 与设备集的唯一写入点,**不受本开关管**——拦了它,CreatePlayer /
	// EnterGame 全部回 kLoginSessionNotFound,所有客户端都进不了游戏。只有在 gate 上用
	// 口令 / 第三方凭据首认证(含空 auth_type)才是被关掉的旧路径。
	//
	// 置 false 后旧路径在认证之前拒绝(2026-10-18 起;此前在认证与账号锁之后):不调用认证器
	// (第三方 code 不被消费)、不抢账号锁、不建账号目录、不写 login_session / 设备集,
	// 翻回 true 只是改配置,无需清理。凭据真假都回 kLoginUnknownError,不再先回 kLoginAccountNotFound。
	//
	// Default true preserves current behavior. Operators flip via
	// login.yaml; no restart-blocking dependency on other services.
	LegacyGateLoginEnabled bool `json:"LegacyGateLoginEnabled,default=true"`

	// Queue holds configuration for the AssignGate login queue. Disabled by
	// default so existing deployments keep the legacy "always assign" behavior;
	// flip Enabled=true once the queue path has been validated end-to-end. See
	// docs/design/login-queue-2026-05.md (added with this feature).
	Queue QueueConf `json:"Queue,optional"`

	// HomeZone 控制「按 data_service 的 player:zone 映射修正 zone」的两条链
	// (internal/logic/pkg/homezone 包注释)。整块可缺省,缺省 = 角色列表刷新开、
	// 进游戏重定向关(见 HomeZoneConf 各字段)。
	HomeZone HomeZoneConf `json:"HomeZone,optional"`

	// Placement 控制建角时是否顺手钉「存储落点」(docs/design/player-storage-placement.md §8.3)。
	// 全球服务必须显式PinOnCreate=true；整块缺省保留零值，启动门禁拒绝而不静默补权。
	Placement PlacementConf `json:"Placement,optional"`

	// NewPlayerSeed 控制建角时是否写「新号父键种子」,让新号首次 EnterGame 不再冷读数据库
	// (docs/design/new-player-first-enter-no-cold-read.md)。整块可缺省,缺省 = 关 = 与本功能之前逐字节相同。
	NewPlayerSeed NewPlayerSeedConf `json:"NewPlayerSeed,optional"`
	// ParentLoad 控制 Redis 实例身份变化后的 SQL 核验；块缺省也必须启用内部默认值。
	ParentLoad ParentLoadConf `json:"ParentLoad"`

	// PersistSchemaRegressionMode 是 EnterGame 父快照快路径上持久 schema 世代倒退判定(C4)的模式
	// (docs/design/mixed-version-persist-field-protection.md §5.1)。取值 warn / enforce;
	// 缺键或空串按 warn,其余取值启动期拒绝(ParsePersistSchemaRegressionMode,login.go)。
	// 运行期只读归一化之后的结果,不要直接比较这个字符串。
	PersistSchemaRegressionMode string `json:"PersistSchemaRegressionMode,optional"`

	// RequireClientEndpoint 控制下发 gate 地址时是否**必须**用 gate 自报的客户端可达地址
	// (NodeInfo.client_endpoint,集群外入口 D78/D79,规则见 shared/clientendpoint.Select)。
	//
	//   - false(默认,podip 模式与滚动过渡期):gate 没自报就回落 endpoint(PodIP),与改动前一致;
	//   - true(external 模式):没自报客户端地址的 gate 直接跳过,不再把集群外连不上的 PodIP 发给玩家。
	//
	// 这是消费方的纵深防御,不是主闸:生产方(gate 进程)在 external 模式下缺地址会直接起不来。
	// 打开顺序:确认 etcd 里本 zone 所有 gate 都带 clientEndpoint 之后再置 true;回退时先把它置回 false。
	// 置 true 而 gate 全都缺地址 = 本 zone 无 gate 可分,登录整体失败(mmorpg_client_endpoint_select_total
	// {result="rejected"} 会持续增长)。K8s 下由 k8s_deploy.ps1 按 -ClientEntryMode 写进 ConfigMap。
	RequireClientEndpoint bool `json:"RequireClientEndpoint,default=false"`

	// GateDrain 是 gate 排空判定循环(loginqueue.StartGateDrainMonitor)的参数:已标
	// gate:{id}:draining 的 gate 在线掉到阈值或等到期限后,由它写上 gate:{id}:drained,
	// k8s_gate_drain.ps1 看到标记才删 Pod(集群外入口 D87)。
	//
	// **刻意不标 optional**:go-zero 对整块缺失的 optional 结构体不填内层 default(见 KillSwitchConf),
	// 而 K8s 的 login ConfigMap 由 k8s_deploy.ps1 另写模板、不含这一块。标了 optional,集群上就会拿到
	// 零值 Interval = 监控关闭,drained 永远等不到。不标 optional 且内层字段全带 default 时,go-zero
	// 判定该结构体「非必填」,整块缺失 = 全部取默认值(gate_drain_conf_test.go 钉住这一点)。
	GateDrain GateDrainConf `json:"GateDrain"`
}

// 整块不标 optional，保证省略块的部署模板也取得 true；false 仅用于冷读风暴止血。
// ACK 超前与历史缺口属于独立的回退证据，不受此开关影响。
type ParentLoadConf struct {
	VerifyOnRedisIdentityChange bool `json:"VerifyOnRedisIdentityChange,default=true"`
}

// GateDrainConf 是 gate 排空判定的参数,判定逻辑与标记语义见 loginqueue/gatedrain_monitor.go。
//
// **每个字段都必须带 default**:任一字段变成必填,整块缺失的旧配置(包括 K8s ConfigMap)会起服失败;
// 变成 optional 不带 default,缺失时拿零值。两种都会让 Config.GateDrain 的「缺省即默认值」失效。
type GateDrainConf struct {
	// Interval 是判定周期。<=0 关闭监控(启动时打 ERROR):drained 永远不出现,排空脚本只能等到超时。
	// 每轮一次 etcd 前缀读 + 每台 gate 一次 Redis GET,5s 对 etcd / Redis 都可忽略。
	Interval time.Duration `json:"Interval,default=5s"`
	// DrainedBelowPlayers:在线数 <= 它即判定排空。默认 0 = 必须一个人都不剩。
	DrainedBelowPlayers uint32 `json:"DrainedBelowPlayers,default=0"`
	// Deadline:从标 draining 起等这么久,无论还剩多少人都判定可缩容(理由记 deadline,打 ERROR);
	// 0 = 永不因超时放行,只认人走干净。判定只产出信号,删不删 Pod 仍由运维带不带 -DeletePod 决定。
	//
	// 默认 25m 必须**小于** draining 标记的 TTL(k8s_gate_drain.ps1 -DrainTtlSeconds,默认 1800s)
	// 与脚本的等待上限(-WaitTimeoutSeconds,默认 1800s):期限不早于 TTL 时 draining 先过期,
	// 这台 gate 重新接客,drained 永远等不到。调小脚本的 TTL 时同步调小这里。
	Deadline time.Duration `json:"Deadline,default=25m"`
}

// PlacementConf 是建角钉落点的开关(docs/design/player-storage-placement.md §8.3 / §13)。
//
// go-zero对缺失optional块不填内层default，因此保留字段零值，不猜测调用者是否想降级。
// 全球启动门禁要求显式PinOnCreate=true；旧关闭形状只供LegacyNonGlobal且Mode=dev/test的历史夹具。
// 正式顺序是迁移/路由初始化→DS强TiDB/db_first及对账arm→可信ready→DB/Login。
// 旧Redis目录须先正规import/audit，不能通过关闭PinOnCreate接管空SQL；新号storage0只取正规Node.ZoneId。
type PlacementConf struct {
	// PinOnCreate=true:建角时在同一次 RegisterPlayerZone 里原子钉 player:placement = "{storage}:1"。
	PinOnCreate bool `json:"PinOnCreate,optional"`
	// NewPlayerStorageId 是新角色钉到的落点库编号(§4.2):0 = 本 login 所在 zone(Node.ZoneId);
	// 1..999999 = zone_{id}_db;>=1000000 = player_store_{id}_db(Phase 2 全局库填 1000000)。
	// 只在 PinOnCreate=true 时生效。
	NewPlayerStorageId uint32 `json:"NewPlayerStorageId,optional"`
}

// StorageIDForNewPlayer 返回建角时要传给 RegisterPlayerZone 的 storage_id。
// 返回 0 表示「不钉」(开关关着)—— 这是 data_service 侧约定的缺省语义,不是错误。
// loginZone 为本 login 的 Node.ZoneId,NewPlayerStorageId 为 0 时用它兜底,
// 与「建角 home = login 所在 zone」的既有约定一致,钉下去的有效落点与不钉时相同。
func (c PlacementConf) StorageIDForNewPlayer(loginZone uint32) uint32 {
	if !c.PinOnCreate {
		return 0
	}
	if c.NewPlayerStorageId != 0 {
		return c.NewPlayerStorageId
	}
	return loginZone
}

// ValidateGlobalAuthority在任何外部连接之前校验全球建角与SQL目录调用前提。
// Login不另造TiDB开关：真实强SQL/已核ready由DS正式门禁负责，Login必须指向该DS而不能静默降级。
func (c *Config) ValidateGlobalAuthority(getenv func(string) string) error {
	if c.LegacyNonGlobal {
		if c.Mode != service.DevMode && c.Mode != service.TestMode {
			return fmt.Errorf("LegacyNonGlobal仅允许显式Mode=dev/test的历史非全球夹具")
		}
		return nil
	}
	if !c.Placement.PinOnCreate {
		return fmt.Errorf("全球Login必须配置Placement.PinOnCreate=true")
	}
	if c.Node.ZoneId == 0 {
		return fmt.Errorf("全球Login必须配置非零Node.ZoneId，NewPlayerStorageId=0才可取正规本区落点")
	}
	rpc := c.DataServiceRpc
	target := strings.TrimSpace(rpc.Target) != ""
	for _, endpoint := range rpc.Endpoints {
		target = target || strings.TrimSpace(endpoint) != ""
	}
	if strings.TrimSpace(rpc.Etcd.Key) != "" {
		for _, host := range rpc.Etcd.Hosts {
			target = target || strings.TrimSpace(host) != ""
		}
	}
	if !target {
		return fmt.Errorf("全球Login必须配置DataServiceRpc明确目标，不能回落旧Redis账号或归属目录")
	}
	if getenv == nil {
		return fmt.Errorf("全球Login缺少账号目录密钥环境读取器")
	}
	if _, err := serviceauth.New(map[string]string{"login": getenv(accountdirectory.SecretEnv)}); err != nil {
		return fmt.Errorf("全球Login的%s无效: %w", accountdirectory.SecretEnv, err)
	}
	return nil
}

// NewPlayerSeedConf 是「新号首次入场免冷读」的开关(docs/design/new-player-first-enter-no-cold-read.md)。
//
// 正向命名,零值 = 关:go-zero 不给整块缺失的 optional 结构体递归填内层 default(见 HomeZoneConf 的说明),
// 所以默认值只能靠零值表达。
type NewPlayerSeedConf struct {
	// Enabled=true:CreatePlayer 在落账号 blob 之前写 PlayerAllData:{id} 种子(两张子表只带 player_id,
	// TTL 与冷读组装的父键相同),新号首次 EnterGame 命中缓存直接进场。种子没写成不影响建角(退回冷读)。
	// 打开前置:go/db 的 db_placement_guard_total{outcome="missing_required"} 与
	// db_placement_store_open_total{result=~"error|rejected"} 两类告警已就位(设计 §9 第 2 条)。
	Enabled bool `json:"Enabled,optional"`
}

// HomeZoneConf 是合服后 zone 归属修正的开关。
//
// 两个开关的默认方向**刻意不同**:
//
//   - 角色列表刷新(RefreshRoleListDisabled)沿用 KillSwitchConf 的纪律:go-zero 对整块
//     缺失的 optional 结构体**不会**递归填内层 default,所以反向命名,零值 = 开着。
//     它是合服后的**主修正机制**:客户端按刷新后的 zone_id 选区,直接连到归属 zone 的
//     gate,根本不需要重定向。
//   - 进游戏重定向(RedirectOnEnterEnabled)正向命名,零值 = 关。它依赖客户端实现
//     SceneClientPlayerCommonRedirectToGate(msg 124:断开当前 gate、连 target_ip:port、
//     首包 ClientTokenVerifyRequest、再走 Login + EnterGame)。EnterGame RPC 在触发重定向
//     时已经回了成功并清掉登录会话,客户端若只是把 124 打个日志(Unity 端目前就是这样),
//     玩家就卡在源 zone 的 gate 上既进不了场景也无法重试。**必须等客户端实现了 124 才能
//     置 true**;参考实现见 robot/pkg/redirect.go(FollowRedirect)与
//     robot/logic/handler/scene_client_player_common_redirect_to_gate.go。
//
// 关掉角色列表刷新只在 homezone 自身出问题需要止血时用;关掉后 login 退回修复前行为
// (角色列表报建角 zone),不会拒绝任何请求。
type HomeZoneConf struct {
	// RefreshRoleListDisabled=true:Login 返回的角色列表不再用映射覆盖 zone_id。
	RefreshRoleListDisabled bool `json:"RefreshRoleListDisabled,default=false"`
	// RedirectOnEnterEnabled=true:EnterGame 按归属 zone 触发 scene_manager 的跨区重定向。
	// 默认 false —— 客户端实现 msg 124 之前不许打开(见类型注释)。即使打开,也只对
	// 「首次登录且 player_locator 里没有在场 scene」的请求生效:重连 / 顶号必须回到
	// 原来的 scene,不能被归属 zone 覆盖(entergamelogic.go homeZoneOverrideAllowed)。
	RedirectOnEnterEnabled bool `json:"RedirectOnEnterEnabled,default=false"`
	// RoleListLookupTimeout 是 Login 角色列表那一次 BatchGetPlayerHomeZone 的预算;
	// 0 = homezone.DefaultRoleListLookupTimeout(500ms)。登录链路上串行的一跳,
	// 超时按失败处理(保留建角 zone),宁可偶尔给旧 zone 也不拖慢所有登录。
	// 同一个值也用于角色列表缺名回源 BatchGetPlayerName(loginlogic.go fillMissingRoleNames):
	// 两次查询串行、各自计时(不是共享一份预算)。全部有名的账号仍只有一跳;
	// 缺名账号(B3a 之前建的老角色)最坏 2×该值,调大时按双倍估算登录延迟。
	RoleListLookupTimeout time.Duration `json:"RoleListLookupTimeout,optional"`
	// EnterLookupTimeout 是 EnterGame 那一次 GetPlayerHomeZone 的预算;
	// 0 = homezone.DefaultEnterLookupTimeout(1.5s)。EnterGame 链路本身是异步的
	// (5 分钟预算),可以比角色列表宽松。
	EnterLookupTimeout time.Duration `json:"EnterLookupTimeout,optional"`
	// RegisterTimeout 是 CreatePlayer 写 player:zone 映射(RegisterPlayerZone)的预算;
	// 0 = homezone.DefaultRegisterTimeout(3s)。这条写失败建角**整体失败**
	// (createplayerlogic.go),所以给得比查询宽。
	RegisterTimeout time.Duration `json:"RegisterTimeout,optional"`
}

// QueueConf controls the login queue (Redis ZSET-backed) that throttles
// AssignGate when a zone's gates are at capacity.
//
// Knobs are deliberately conservative:
//   - DispatchInterval too low burns Redis QPS for no benefit (gate
//     PlayerCount only refreshes every 10s via etcd anyway).
//   - AdmitTTL 是放行槽等客户端来取的时限,也是没人来取时在途占位的寿命(默认 60s)。
//     取走之后、以及快速通道当场交票之后,占位只再留 20s:那是代码常量
//     loginqueue.defaultHandoffGrace,不是配置,依据见该常量的注释(G4b-G)。
//     占位键的整键 TTL 只增不减(剩余不足本副本的 max(AdmitTTL, 20s) + 1min 才续),排队开着时
//     也可以滚动调整它;调小之后,已续上的较大整键 TTL 要自然走完才收敛(见 loginqueue.admittedKeyTTL)。
//   - SoftCapMultiplier (e.g. 1.5) is the headroom factor applied to the
//     largest observed PlayerCount when no explicit ZoneCapacityOverride is
//     set, so freshly-deployed zones aren't capped at "current load".
//   - DispatcherLockKey 是出队 leader 锁键的**前缀**,实际锁键是
//     <DispatcherLockKey>:zone:<Node.ZoneId>(loginqueue.ZoneDispatcherLockKey),
//     每个 zone 一个 leader。dispatcher 只看得见本 zone 的 gate,锁的范围必须与之相同。
//     2026-10-12 以前它就是锁键本身、全服一把,多 zone 共用 Redis 时只有 leader 所在
//     zone 出队(G1b)。默认值没变,旧配置无需改动;Queue.Enabled=true 时它不能为空、
//     Node.ZoneId 不能为 0,否则起服失败。同一 zone 的所有 login 副本必须配同一个值,
//     配得不一样就是两把互不相干的锁,该 zone 长期双 leader。
//   - Queue.Enabled=true 时 DispatchInterval、DispatcherLockTTL 必须为正(loginqueue.NewDispatcher
//     校验),否则起服失败。default=1s / 30s 只在键缺省时生效,显式写 0s 不会回落到默认值。
//     (2026-10-15 补注)QueueEntryTTL 同样必须为正,AdmitTTL 不小于 1ms(放行脚本的 SET PX 取整到毫秒;
//     loginqueue.ValidateTTLs,initLoginQueue 校验),否则起服失败;default=60s / 1h 同样只在键缺省时生效。
//   - (2026-10-18 补注)QueueEntryTTL 改为空闲 TTL:meta、token 索引与队列键自最后一次成功的排队中查询(或入队)
//     起算,至少保留 QueueEntryTTL、至多 1.25 倍,持续轮询的人不会因为等满 1h 被弹回队尾。排队令牌改为固定 24h
//     签名过期(loginqueue.queueTokenMaxAge),不再等于 QueueEntryTTL。QueueEntryTTL 取值须在 [1s, 24h],否则起服
//     失败。本版本以及从它回滚,都必须在 Enabled=false 下滚完:排队开着混跑时,旧版 leader 弹出、尚未写完的条目会被
//     新版判为 queue_gone、打回队尾。
type QueueConf struct {
	Enabled             bool          `json:"Enabled,default=false"`
	DispatchInterval    time.Duration `json:"DispatchInterval,default=1s"`
	AdmitTTL            time.Duration `json:"AdmitTTL,default=60s"`
	QueueEntryTTL       time.Duration `json:"QueueEntryTTL,default=1h"`
	SoftCapMultiplier   float64       `json:"SoftCapMultiplier,default=1.5"`
	DefaultRetryAfterMs uint32        `json:"DefaultRetryAfterMs,default=2000"`
	DispatcherLockTTL   time.Duration `json:"DispatcherLockTTL,default=30s"`
	DispatcherLockKey   string        `json:"DispatcherLockKey,default=dispatcher:lock:login_queue"`
	// ZoneCapacityOverride lets ops pin the per-zone admission ceiling
	// without trusting gate-side soft caps. Key is zone_id (string for YAML),
	// value is the absolute number of concurrent online players permitted.
	ZoneCapacityOverride map[string]uint32 `json:"ZoneCapacityOverride,optional"`
}

// InternalAuthConf 控制内部调用方身份声明的验签闸门(见
// internal/logic/pkg/callerauth)。
//
// **生产模式恒强制验签,这里没有任何开关能关掉它** —— 唯一能放宽的是
// go-zero Mode=dev/test,而那是编排层的决定,不是业务配置。
type InternalAuthConf struct {
	// ForceEnforce 让 dev/test 也走强制档。上游(cpp gate / Java gateway)
	// 接完签名后,先在本地把它打开做联调,再上生产。
	ForceEnforce bool `json:"ForceEnforce,default=false"`

	// MaxClockSkew 是允许的时间戳偏移窗口(双向)。签名里带毫秒时间戳,
	// 超出这个窗口一律拒绝 —— 这是防重放的第一道闸,nonce 是第二道。
	//
	// 30s 的取法:内网 NTP 偏差通常在毫秒级,30s 已经给足容错;
	// 再放大只会线性放大重放窗口和 nonce 表的内存占用。
	MaxClockSkew time.Duration `json:"MaxClockSkew,default=30s"`

	// MaxNonceEntries 是 nonce 表的硬上限。超出后强制翻代(丢掉最旧的一代),
	// 重放窗口会临时缩短但**绝不会**放行验签失败的请求。
	// 默认 500000 ≈ 4000 QPS × 2×MaxClockSkew 的用量。
	MaxNonceEntries int `json:"MaxNonceEntries,default=500000"`

	// AllowedCallers 是调用方标识白名单(如 ["gate","gateway"])。
	// 留空表示不限制调用方名字,但签名仍然必须验过。
	AllowedCallers []string `json:"AllowedCallers,optional"`
}

// KillSwitchConf 配置 RPC 级热关停闸门(见 shared/killswitch 包注释)。
//
// **整块字段的零值就是期望行为**,这不是偷懒而是刻意的:go-zero 的
// conf.MustLoad 走的是 mapping.UnmarshalJsonMap(没开 WithDefault),
// 一个标了 optional 的结构体字段整块缺失时**不会**递归填内层 default
// (见 core/mapping/unmarshaler.go:processNamedFieldWithoutValue)。
// 于是任何一份没写 KillSwitch 块的 yaml —— 包括仓外的 deploy/*.yaml ——
// 都会拿到零值。所以这里绝不能用 `Enabled bool default=true`:那种写法
// 会让老配置静默地把止血阀关掉,而且要等到线上出事才发现。
//
// 零值 = 开着 watch + 库默认前缀/时限。这不会拒绝任何请求:killswitch
// 铁律 fail-open,etcd 连不上、前缀下没 key、值写坏了一律放行,只有运维
// 显式往 etcd 写 deny 才会关停。反过来默认不开才是危险的 —— 真出事时
// 没有止血阀可用。
type KillSwitchConf struct {
	// Disabled 完全不起 etcd watch,也不把拦截器挂进链(全部放行)。
	// 刻意用反向命名,理由见上:缺配置必须等于"开着"。
	// 只有在热关停机制自身出问题时才需要打开这个逃生阀。
	Disabled bool `json:"Disabled,default=false"`

	// Prefix 是规则在 etcd 里的前缀,留空用 killswitch.DefaultPrefix
	// ("/mmorpg/killswitch/")。改它等于换一套规则命名空间,必须与运维
	// 下发规则的路径一致,否则规则永远命不中(而且是静默命不中)。
	Prefix string `json:"Prefix,optional"`

	// StaleAfter 与 etcd 失联多久后主动作废本地规则快照、退回全放行。
	// 0 = 用库默认(1 分钟);负数 = 永不作废。
	StaleAfter time.Duration `json:"StaleAfter,optional"`

	// ResyncBackoff 全量同步失败后的重试间隔,<=0 用库默认(3s)。
	ResyncBackoff time.Duration `json:"ResyncBackoff,optional"`
}

// PreloadPoolConf controls the bounded goroutine pool used to fan out
// fire-and-forget player data preload tasks (Kafka -> DB -> Redis warm).
// Defaults are tuned for ~256 concurrent in-flight preloads, which is far
// more than the Kafka SyncProducer can drain (it is mutex-serialized), so
// the pool acts as a bounded queue + backpressure / overload shield.
type PreloadPoolConf struct {
	Size          int           `json:"Size,default=256"`          // Worker count
	StatsInterval time.Duration `json:"StatsInterval,default=30s"` // Periodic snapshot log interval (0 = disabled)
}

// AuthConfig holds third-party auth provider settings.
type AuthConfig struct {
	SaToken *SaTokenAuthConf `json:"SaToken,optional"`
	WeChat  *WeChatAuthConf  `json:"WeChat,optional"`
	QQ      *QQAuthConf      `json:"QQ,optional"`
	NetEase *NeteaseAuthConf `json:"NetEase,optional"`
}

// PasswordAuthConf 配置生产口令认证。MySQL DSN 含凭证，只允许通过命名的
// 环境变量注入，不能把 DSN/密码直接写进 YAML；运行时账号只需
// user_accounts 的 SELECT 权限。password 必须已由离线工具迁移为 Argon2id
// PHC 字符串；空值和旧明文都会 fail-closed。
type PasswordAuthConf struct {
	Enabled      bool   `json:"Enabled,default=false"`
	DSNEnv       string `json:"DSNEnv,optional"`
	MaxOpenConns int    `json:"MaxOpenConns,default=20"`
	MaxIdleConns int    `json:"MaxIdleConns,default=10"`
	// 单次 Argon2id 默认使用 64 MiB；并发 2 约占 128 MiB。代码硬上限为 8。
	KDFConcurrency int `json:"KDFConcurrency,default=2"`
	// 只约束等待 semaphore 的时间；调用方 context 更早取消时立即失败。
	KDFWaitTimeout time.Duration `json:"KDFWaitTimeout,default=500ms"`
	FailureLimit   int           `json:"FailureLimit,default=5"`
	FailureWindow  time.Duration `json:"FailureWindow,default=15m"`
	FailureLock    time.Duration `json:"FailureLock,default=5m"`
}

// ValidateFailureQuota 禁止关闭或截断配额参数；副本间参数由 Redis 指纹复核。
func (c PasswordAuthConf) ValidateFailureQuota() error {
	if c.FailureLimit < 1 || c.FailureLimit > 100 || c.FailureWindow < time.Second || c.FailureWindow > time.Hour || c.FailureLock < time.Second || c.FailureLock > time.Hour || c.FailureWindow%time.Millisecond != 0 || c.FailureLock%time.Millisecond != 0 {
		return fmt.Errorf("PasswordAuth requires FailureLimit 1..100 and whole-millisecond FailureWindow/FailureLock within 1s..1h")
	}
	return nil
}

// DevPasswordAuthConf 是唯一允许“不接账号库口令体系”的开发登录路径。
// 默认关闭；只有 go-zero Mode=dev/test 才允许开启，并同时要求共享密钥
// 和账号前缀白名单，避免重新引入
// “报上任意 account 即登录”的生产漏洞。共享密钥必须从环境变量注入，
// 不得写入受管配置文件。
type DevPasswordAuthConf struct {
	Enabled bool `json:"Enabled,default=false"`
	// SharedSecretEnv 只能填写环境变量名；共享密钥本身不得进入受管配置。
	SharedSecretEnv        string   `json:"SharedSecretEnv,optional"`
	AllowedAccountPrefixes []string `json:"AllowedAccountPrefixes,optional"`
}

// SaTokenAuthConf holds SA-Token Redis lookup settings.
type SaTokenAuthConf struct {
	Redis     RedisConf `json:"Redis"`
	TokenName string    `json:"TokenName,default=satoken"`
	LoginType string    `json:"LoginType,default=login"`
}

// WeChatAuthConf holds WeChat OAuth settings.
//
// AppSecret 是调用微信开放平台的真实凭据，与 PasswordAuth.DSNEnv、DevPasswordAuth.SharedSecretEnv
// 同一套纪律：生产只写 AppSecretEnv（环境变量名），密钥本身由 K8s Secret 注入。go-zero 的
// conf.MustLoad 不展开 ${VAR}，所以旧写法 `AppSecret: "${WECHAT_APP_SECRET}"` 会把字面量
// 当成密钥 —— 任何字段里出现未展开的 ${...} 都由 ResolveSecrets 在启动期拒绝（auth_providers.go）。
type WeChatAuthConf struct {
	AppId string `json:"AppId"`
	// AppSecretEnv 是环境变量名（不是密钥本身）。非空时优先于 AppSecret。
	AppSecretEnv string `json:"AppSecretEnv,optional"`
	// AppSecret 是内联明文，只允许 Mode=dev/test（本地沙箱）；其余模式拒绝启动。
	AppSecret string `json:"AppSecret,optional"`
	// RequireUnionId 缺省 true：微信没返回 unionid（应用未绑定开放平台主体）时拒绝登录并回故障码，
	// 不回落 openid。openid 按应用区分，回落后同一玩家在第二个应用里会变成另一个账号，且无法事后合并。
	// 只有确认永远只有一个应用、永不接开放平台时才显式写 false。
	RequireUnionId bool `json:"RequireUnionId,default=true"`
	// Endpoint optionally overrides the api.weixin.qq.com base URL.
	// Set to a local sandbox mock (e.g. http://127.0.0.1:18090) when
	// real Open Platform credentials / network egress aren't available.
	// Production leaves this empty so the provider hits the real host.
	// 生产模式只接受 https://；http:// 只在 Mode=dev/test 下放行。
	Endpoint string `json:"Endpoint,optional"`
}

// QQAuthConf holds QQ OAuth settings.
//
// QQ 走客户端 SDK 拿到的 access_token，服务端只调 /oauth2.0/me 并校验 client_id == AppId，
// 不需要 AppKey；AppKeyEnv / AppKey 保留给将来服务端 code 换 token 的流程，写了就按与
// WeChatAuthConf.AppSecret 相同的规则解析。
type QQAuthConf struct {
	AppId     string `json:"AppId"`
	AppKeyEnv string `json:"AppKeyEnv,optional"`
	AppKey    string `json:"AppKey,optional"`
	// RequestUnionId 缺省 true：请求 /oauth2.0/me 时带 unionid=1（应用须已申请 unionid 权限）。
	RequestUnionId bool `json:"RequestUnionId,default=true"`
	// RequireUnionId 缺省 true，语义同 WeChatAuthConf.RequireUnionId。RequestUnionId=false 时
	// 永远拿不到 unionid，必须同时显式写 false，否则启动期拒绝。
	RequireUnionId bool `json:"RequireUnionId,default=true"`
	// Endpoint optionally overrides the graph.qq.com base URL.
	// Same semantics as WeChatAuthConf.Endpoint.
	Endpoint string `json:"Endpoint,optional"`
}

// NeteaseAuthConf holds NetEase auth settings.
type NeteaseAuthConf struct {
	AppKey    string `json:"AppKey"`
	AppSecret string `json:"AppSecret"`
}

// TokenConf holds access/refresh token TTL settings.
type TokenConf struct {
	AccessTokenTTL  time.Duration `json:"AccessTokenTTL,default=2h"`    // Access token lifetime (default 2 hours)
	RefreshTokenTTL time.Duration `json:"RefreshTokenTTL,default=720h"` // Refresh token lifetime (default 30 days)
}

// NodeConfig holds node-level settings including login duration limits.
type NodeConfig struct {
	ZoneId           uint32        `json:"ZoneId"`
	SessionExpireMin uint32        `json:"SessionExpireMin"` // Session idle timeout in minutes
	MaxLoginDevices  uint32        `json:"MaxLoginDevices"`
	RedisClient      RedisConf     `json:"RedisClient"`
	// QueueRedis 是排队、准入与放行锁所在的 Redis(shared-redis-split-by-purpose.md §3.2.1、§25.3 第 10 条:
	// 按 zone 组分实例 redis-queue-g<N>)。整段不写 = 复用 RedisClient,而且是同一个 *redis.Client(不多开连接池),
	// 行为与拆分前逐字相同。gate 排空标记(gate:<id>:draining / drained)**不在**这里,仍在 RedisClient:
	// k8s_gate_drain.ps1 与 scene_manager 的跨 zone 重定向都读核心上的那一份。
	// 用指针 + optional:go-zero 不会为没出现的 optional 嵌套结构回填内层字段;段一旦出现,内层字段按 RedisConf
	// 的规则全部必填,K8s ConfigMap 必须整段写出(含 PoolSize,连接总账按它记账)。
	QueueRedis       *RedisConf    `json:"QueueRedis,optional"`
	LeaseTTL         int64         `json:"LeaseTTL"` // Lease TTL in seconds
	QueueShardCount  uint64        `json:"QueueShardCount"`
	MaxLoginDuration time.Duration `json:"MaxLoginDuration"` // Max online duration per login (e.g. 24h, force logout on expiry)
	LogoutGraceTime  time.Duration `json:"LogoutGraceTime"`  // Grace period before forced logout (e.g. 5m, for player warning)
}

// RedisConf holds Redis client settings.
type RedisConf struct {
	Host         string        `json:"Host"`
	Password     string        `json:"Password"`
	DB           uint32        `json:"DB"`
	DefaultTTL   time.Duration `json:"DefaultTTL"`
	DialTimeout  time.Duration `json:"DialTimeout"`
	ReadTimeout  time.Duration `json:"ReadTimeout"`
	WriteTimeout time.Duration `json:"WriteTimeout"`
	// PoolSize 是这个 go-redis 客户端的连接池上限(共享 Redis 连接总账,P0-E 第 3 批)。
	// 0 / 不写 = go-redis 缺省 10 × GOMAXPROCS,随 CPU limit 漂移;负数起服即失败(ValidatePoolSize)。
	// K8s 上 tools/scripts/k8s_deploy.ps1 从 etc/login.yaml 镜像 Node.RedisClient.PoolSize 并要求正整数,
	// Redis 连接总账读同一个键。Pub/Sub 专用连接不占池名额;PasswordAuth 的失败配额池固定 4 条,不读本字段。
	PoolSize int `json:"PoolSize,optional"`
}

// ValidatePoolSize 拒绝负的 PoolSize:负数没有合法含义,go-redis 只把 0 当缺省
// (v9.16.0 建池时 make(chan) 遇到负容量直接 panic)。
func (c RedisConf) ValidatePoolSize() error {
	if c.PoolSize < 0 {
		return fmt.Errorf("PoolSize=%d must be >= 0 (0 = go-redis default 10 x GOMAXPROCS)", c.PoolSize)
	}
	return nil
}

// KafkaConfig holds Kafka producer and consumer settings.
type KafkaConfig struct {
	Brokers []string `json:"Brokers"`
	// GroupID 已废弃:login 只生产不消费,代码不读它,也不是 go/db 的消费组(go/db 按 zone 拆组与它无关)。
	// 保留为可选只是为了让带这个键的旧 ConfigMap / login.yaml 仍能加载;先放宽、全部 login 跑上本版本之后再删,
	// 三处配置(go/login/etc/login.yaml、k8s_deploy.ps1 的 login 模板、deploy/login-stack.linux/login.yaml)的
	// 删除顺序见 docs/design/db-consumer-group-per-zone.md §8 Step 7。
	GroupID string `json:"GroupID,optional"`
	Topic   string `json:"Topic,optional"` // Derived from Node.ZoneId at startup
	// TopicGeneration is part of the immutable routing identity. Keep 1 for
	// db_task_zone_{ZoneId}; a partition-count change requires an offline drain
	// and a higher generation, which creates a new topic namespace.
	TopicGeneration  uint32                  `json:"TopicGeneration,default=1"`
	PartitionCnt     int32                   `json:"PartitionCnt"`
	InitialPartition int                     `json:"InitialPartition"` // Should match PartitionCnt
	DialTimeout      time.Duration           `json:"DialTimeout"`
	ReadTimeout      time.Duration           `json:"ReadTimeout"`
	WriteTimeout     time.Duration           `json:"WriteTimeout"`
	RetryMax         int                     `json:"RetryMax"`
	RetryBackoff     time.Duration           `json:"RetryBackoff"`
	ChannelBuffer    int                     `json:"ChannelBuffer"`
	SyncInterval     time.Duration           `json:"SyncInterval"`
	StatsInterval    time.Duration           `json:"StatsInterval"`
	CompressionType  sarama.CompressionCodec `json:"CompressionType"` // none/gzip/snappy
	Idempotent       bool                    `json:"Idempotent"`
	MaxOpenRequests  int                     `json:"MaxOpenRequests"`              // Must be 1 when idempotent
	RetentionMs      int64                   `json:"RetentionMs,default=86400000"` // Topic retention in ms (default 24h)
	// P1 数据安全加固 2026-06-03: 旧值 300000 (5min)
	// 太短,db service 卡住会丢数据
	// RetentionBytes 是 db_task topic 每分区的 retention.bytes。K8s 由 k8s_deploy.ps1 按 Kafka 盘预算渲染,
	// 与 db 同值;本机不设 = 沿用 broker 缺省(0 = 不管理,<0 启动拒绝)。
	// 见 docs/design/db-task-delivery-and-retention.md §B。
	RetentionBytes int64 `json:"RetentionBytes,optional"`
}

// SnowflakeConf holds snowflake ID generator settings.
type SnowflakeConf struct {
	Epoch    int64  `json:"Epoch"` // Epoch timestamp in milliseconds
	NodeBits uint32 `json:"NodeBits"`
	StepBits uint32 `json:"StepBits"`
}

// LockerConf holds distributed lock settings.
type LockerConf struct {
	AccountLockTTL uint32 `json:"AccountLockTTL"` // Account lock TTL in seconds
	PlayerLockTTL  uint32 `json:"PlayerLockTTL"`  // Player lock TTL in seconds
}

// AccountConf holds account-related settings.
type AccountConf struct {
	MaxDevicesPerAccount int64         `json:"MaxDevicesPerAccount"`
	MaxPlayersPerAccount int           `json:"MaxPlayersPerAccount,default=5"` // 已弃用的zone副本；真正上限由data_service.AccountDirectory决定。
	CacheExpire          time.Duration `json:"CacheExpire,optional"`           // 已弃用，仅兼容旧配置；账号目录不设 TTL。
}

// RegistryConf holds service registry/discovery settings.
type RegistryConf struct {
	Etcd EtcdRegistryConf `json:"Etcd"`
}

// EtcdRegistryConf holds etcd registry settings.
type EtcdRegistryConf struct {
	Hosts       []string      `json:"Hosts"`
	Key         string        `json:"Key"`
	DialTimeout time.Duration `json:"DialTimeout"`
}

// TimeoutConf holds various timeout settings.
type TimeoutConf struct {
	EtcdDialTimeout         time.Duration `json:"EtcdDialTimeout"`
	ServiceDiscoveryTimeout time.Duration `json:"ServiceDiscoveryTimeout"`
	TaskWaitTimeout         time.Duration `json:"TaskWaitTimeout"`
	LoginTotalTimeout       time.Duration `json:"LoginTotalTimeout"` // Total login timeout including Redis/DB
	RoleCacheExpire         time.Duration `json:"RoleCacheExpire"`
}

var AppConfig Config

// DbTaskTopic returns the zone-specific Kafka topic for DB tasks.
func DbTaskTopic(zoneId uint32) string {
	return fmt.Sprintf("db_task_zone_%d", zoneId)
}

func DbTaskTopicForGeneration(zoneId, generation uint32) string {
	base := DbTaskTopic(zoneId)
	if generation <= 1 {
		return base
	}
	return fmt.Sprintf("%s_g%d", base, generation)
}
