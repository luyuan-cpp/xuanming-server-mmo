// Package config 是 player_inbox 服务的配置结构与启动期校验(docs/design/player-inbox-service.md §1.4、§3.3、§10)。
//
// 键名与 etc/player_inbox.yaml 逐字一致;第 4 批的 k8s_deploy.ps1 ConfigMap 渲染也必须逐字一致,
// 否则 ConfigMap 里的值会被 go-zero 当未知键静默忽略。
package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"player_inbox/internal/constants"
	"player_inbox/internal/data"

	inboxpb "proto/player_inbox"
	"shared/authoritysql"

	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
)

// MaxRpcTimeoutMs / MinRpcTimeoutMs 是 zrpc 服务端整体超时的区间(毫秒)。
//
// 调用方是 C++ scene 与运维工具,不经路由服。scene 侧的 GrpcClient.CallDeadlineMs.PlayerInboxNodeService
// 必须 ≥ Timeout + 2000(部署门禁,第 4 批 Assert-GrpcClientDeadlineBudget 核对),所以上限给 5000;
// 下限保证扣掉 InBandReplyReserve 后仍有 500ms I/O 预算。
const (
	MaxRpcTimeoutMs int64 = 5000
	MinRpcTimeoutMs int64 = 1000
)

// InBandReplyReserve 是整请求业务预算相对 zrpc 服务端 Timeout 预留的余量(同 trade P1-9):
// go-zero 的超时拦截器到点直接回 DeadlineExceeded,handler 之后产出的 in-band kPlayerInboxStoreUnavailable
// 会被丢掉,所以一次请求里的全部 I/O 共用 Timeout − 本余量 的截止时间。
const InBandReplyReserve = 500 * time.Millisecond

// MinAdminTokenBytes 是 AdminToken 非空时的最短字节数。GM 处置是常态业务、口令常驻(设计 §1.6),
// 太短的口令等于没有;空串仍表示「运维面停用」。
const MinAdminTokenBytes = 16

// adminTokenEnvPattern 约束 AdminTokenEnv 只能是「一个环境变量名」,顺带挡住「把口令值贴进 AdminTokenEnv」的手滑
// (真口令一般含小写或连字符,过不了这个形状)。同 go/trade 的 AssetOp.SecretEnv 判据。
var adminTokenEnvPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// 后台循环参数的合法区间。
const (
	minRetentionDays         = 8 // 必须大于签发窗 7 天(设计 §6.2 生产方契约、§9.1)
	maxRetentionDays         = 365
	minSweepIntervalSeconds  = 10
	maxSweepIntervalSeconds  = 86400
	minGaugeIntervalSeconds  = 5
	maxGaugeIntervalSeconds  = 3600
	minNotifyResendSeconds   = 5
	maxNotifyResendSeconds   = 600
	minReconcileIntervalMins = 5
	maxReconcileIntervalMins = 7 * 24 * 60
	minReconcileWindowHours  = 1
	maxReconcileWindowHours  = 7 * 24
)

// Config 是 player_inbox 服务的全部配置。
//
// 不声明名为 Redis 的字段:嵌入的 zrpc.RpcServerConf 已带 `Redis`,同名会在加载期报 "conflict key Redis"。
// 核心 Redis 因此叫 CoreRedis。
type Config struct {
	zrpc.RpcServerConf

	// ZoneId:只影响 etcd 注册路径 <Prefix>/zone/<z>/...。必填且拒 0;业务代码禁止读它做分支
	// (收件箱按全局唯一的 player_id 存,与 zone 无关)。
	ZoneId uint32 `json:",optional"`

	// LeaseTTL:etcd 节点注册租约 TTL(秒),即崩溃场景的切换窗口。
	LeaseTTL int64 `json:",default=60"`

	// MetricsListenAddr:Prometheus /metrics 监听地址,留空关闭。player_inbox 用 :9270(D14)。
	MetricsListenAddr string `json:",optional"`

	// KillSwitchPrefix:RPC 级热关停规则在 etcd 里的前缀,留空用 /mmorpg/killswitch/。
	KillSwitchPrefix string `json:",optional"`

	// MySQL:独占库 mmorpg_player_inbox(D-14)。连接池即设计 §10 的「独立连接池」:
	// 审计与入箱同库同事务；逐物理连接由shared/authoritysql按 MySQL.Backend 档位固定当前读。
	MySQL MySQLConf `json:"MySQL"`

	// 同一SQL入口中的角色目录schema；入箱、消费、运维、后台均在业务事务首锁原角色P。
	AccountRoleSchema string `json:",optional"`

	// Schema:建表策略,见 SchemaConf.AutoMigrate。整段可缺失。
	Schema SchemaConf `json:",optional"`

	// CoreRedis:**核心** Redis(与 C++ scene / scene_manager 同一实例,DB 0),只读三类东西:
	//   - player:{id}:owner_epoch:仅STALE分类诊断，不是消费授权上界；
	//   - player:{id}:location:在线提醒定位玩家所在 scene 节点;
	//   - 维护选主锁 player_inbox:maintenance:leader(清理 / 补发提醒 / gauge / 对账只在领导者上跑)。
	// 指错实例的表现是 R 永远是 0(所有 Fetch 判 EPOCH_AHEAD)、提醒永远发不出,所以 Validate 拒绝空 Host。
	CoreRedis RedisConf `json:"CoreRedis"`

	// Kafka:在线提醒写 scene-cmd_g<N>(kafkacmd 寻址)。只是提醒,丢了由落地拉取与在线兜底补上。
	Kafka KafkaConf `json:"Kafka"`

	// DataServiceRpc:玩家存在校验(GetPlayerHomeZone)与对账读已落盘账本(GetPlayerInboxLedger)。
	// 写 Key: dataservice.rpc 不违反 D-13(data_service 按 zone 部署,先例 go/trade/etc/trade.yaml)。
	DataServiceRpc zrpc.RpcClientConf `json:"DataServiceRpc"`

	// PlayerInbox:收件箱业务参数(D14)。
	PlayerInbox PlayerInboxConf `json:"PlayerInbox"`
}

// PlayerInboxConf 是收件箱业务参数。
type PlayerInboxConf struct {
	// AdminToken:运维接口 PlayerInboxAdmin 的共享口令(字面值),调用方放在 gRPC metadata `x-admin-token` 里。
	// **口令解析后为空 = 运维面整体停用**(fail-closed,设计 §1.6)。与 data_service 守回档 RPC 的 AdminToken 不是同一把:
	// GM 处置是常态业务,口令常驻。与 AdminTokenEnv 二选一;仓库里的 yaml 永远不写它(config_test 钉住)。
	AdminToken string `json:",optional"`

	// AdminTokenEnv:运维口令所在的**环境变量名**(不是口令值),非空时从该变量取口令(见 ResolveAdminToken)。
	// K8s 由 Secret player-inbox-admin-token 注入容器变量 PLAYER_INBOX_ADMIN_TOKEN,ConfigMap 只写这个变量名
	// (docs/ops/player-inbox-runbook.md §1;tools/scripts/k8s_deploy.ps1 在要注入口令时核对服务 yaml 声明了本键)。
	// 变量未设置或为空 = 运维面停用。与字面 AdminToken 二者都非空时拒启:两个来源说不清以哪个为准。
	AdminTokenEnv string `json:",optional"`

	// EnqueueTypes:允许入箱的消息类型白名单,写 PlayerInboxMessageType 的枚举名
	// (如 PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN)。**默认空 = 全部停用**,是入箱开关的唯一来源(设计 §3.3);
	// 不在白名单里的类型回 kPlayerInboxDisabled。上线顺序见设计 §11.7:scene 全部打开消费之后才填它。
	// v1 只放封锁 / 解封两种,v1DisabledEnqueueTypes 里的类型(挂欠款)列进来即拒启。
	EnqueueTypes []string `json:",optional"`

	// TerminalRetentionDays保留旧配置兼容；全球item现为永久原请求收据，Sweep不再物删。
	TerminalRetentionDays int `json:",default=30"`
	// SweepIntervalSeconds:两轮清理之间的间隔;每轮最多删 500 行(设计 §9.1)。
	SweepIntervalSeconds int `json:",default=600"`
	// NotifyResendIntervalSeconds:提醒补发循环的间隔(设计 §6.3:30 秒一轮)。
	NotifyResendIntervalSeconds int `json:",default=30"`
	// GaugeIntervalSeconds:扫描型 gauge 的刷新间隔(设计 §9.4,只由领导者产出)。
	GaugeIntervalSeconds int `json:",default=60"`
	// ReconcileIntervalMinutes:对账作业间隔(设计 §9.3:每小时一轮)。0 = 不跑。
	// 当选领导者后第一轮只等 1–5 分钟(随机),之后按本间隔跑。
	ReconcileIntervalMinutes int `json:",default=60"`
	// ReconcileWindowHours:对账只看最近这么多小时内由消费方终结的行(设计 §9.3:24 小时)。
	// 必须覆盖至少两轮间隔(ReconcileWindowHours×60 ≥ 2×ReconcileIntervalMinutes),见 Validate。
	// 轮次持续被截断时,每片至多隔 16 轮才读完一次(inbox.reconcileFullCycleRounds,评审 R1):窗口不足 16 个间隔时
	// 有的分片可能整个窗口都轮不到(此时每轮截断都打 ERROR)。默认 24 小时 / 60 分钟 = 24 轮。
	ReconcileWindowHours int `json:",default=24"`
}

// RedisConf 是 go-redis 客户端的连接参数。刻意不用 go-zero 的 redis.RedisConf:那份结构没有 DB 字段
// (填了会被静默忽略),也设不了连接池上限。形状与 go/trade 的 SharedRedis 逐字一致。
type RedisConf struct {
	Host     string `json:"Host"`
	Password string `json:"Password,optional"`
	DB       int    `json:"DB,optional"`
	// PoolSize 是 go-redis 连接池上限(共享 Redis 连接总账,P0-E 第 3 批)。**必须显式写正整数**(评审 L7,Validate 拒绝 ≤ 0):
	// 0 在 go-redis 里是缺省 10 × GOMAXPROCS,随 CPU limit 漂移,总账就不是集群的账。K8s 上 tools/scripts/k8s_deploy.ps1
	// 从 etc/player_inbox.yaml 镜像 CoreRedis.PoolSize 并要求正整数,Redis 连接总账读同一个键。
	// 围栏上界、在线提醒与选主锁共用这一个池。
	PoolSize int `json:"PoolSize,optional"`
}

// KafkaConf 是在线提醒用的 Kafka 生产者参数。
type KafkaConf struct {
	Brokers []string `json:"Brokers"`
	// WriteTimeoutMs:writer 侧单次 produce 的超时(毫秒)。提醒只是提示,超时就留给补发循环。
	WriteTimeoutMs int `json:"WriteTimeoutMs,default=2000"`
}

// MySQLConf 是 mmorpg_player_inbox 的连接参数(形状与 go/trade 相同)。
type MySQLConf struct {
	Host string `json:"Host"`
	User string `json:"User"`
	// Password 可以为空(本地免密账号);值只进 DSN,绝不打日志、不进横幅。
	Password    string `json:"Password,optional"`
	DBName      string `json:"DBName"`
	MaxOpenConn int    `json:"MaxOpenConn,default=20"`
	MaxIdleConn int    `json:"MaxIdleConn,default=5"`
	// Backend:tidb | mysql,不写就是 tidb(docs/design/player-data-tidb-scaling-plan.md §9);非法值拒启。
	Backend string `json:",default=tidb"`
}

// SchemaConf 建表策略(D-14 第 4 条)。AutoMigrate 为 nil(没写)或 false 时常驻启动仅核对；显式迁移才跑 schemamigrate.Up;
// false:启动时只跑只读 Plan,有待执行语句或需人工项即拒绝启动。用 *bool 的理由同 go/trade。
type SchemaConf struct {
	AutoMigrate *bool `json:",optional"`
}

// RequestBudget 返回一次请求内全部 I/O 共用的业务预算。Validate 保证结果恒为正。
func (c Config) RequestBudget() time.Duration {
	return time.Duration(c.Timeout)*time.Millisecond - InBandReplyReserve
}

// ShouldAutoMigrate 识别显式自动迁移请求；常驻入口会拒绝true，没写默认只读核对。
func (c Config) ShouldAutoMigrate() bool {
	return c.Schema.AutoMigrate != nil && *c.Schema.AutoMigrate
}

// ResolveAdminToken 解析生效的运维口令:字面 AdminToken 优先(Validate 已保证与 AdminTokenEnv 不同时出现);
// 否则从 AdminTokenEnv 指定的环境变量读(去首尾空白:Secret 文件常带结尾换行,带着它比对只会一律拒绝)。
// 返回空串 = 运维面停用。环境变量里的口令不足 MinAdminTokenBytes 字节返回错误(调用方拒启),错误文本不回显口令。
// getenv 显式注入(生产为 os.Getenv),单测不碰真实环境变量。
func (p PlayerInboxConf) ResolveAdminToken(getenv func(string) string) (string, error) {
	if p.AdminToken != "" {
		return p.AdminToken, nil
	}
	if p.AdminTokenEnv == "" || getenv == nil {
		return "", nil
	}
	token := strings.TrimSpace(getenv(p.AdminTokenEnv))
	if token == "" {
		return "", nil
	}
	if len(token) < MinAdminTokenBytes {
		return "", fmt.Errorf("环境变量 %s 里的运维口令至少 %d 字节(得到长度 %d,此处刻意不回显内容);不打算开运维面就不要设置该变量",
			p.AdminTokenEnv, MinAdminTokenBytes, len(token))
	}
	return token, nil
}

// IsRelaxedMode 报告 go-zero Mode 是否为 dev / test(只用于决定是否注册 gRPC reflection)。
func IsRelaxedMode(mode string) bool {
	return mode == service.DevMode || mode == service.TestMode
}

// ListenHostPort 把 ListenOn 拆成 host 与端口号。用 net.SplitHostPort 以正确处理 "[::]:50950"。
func (c *Config) ListenHostPort() (string, uint32, error) {
	host, portStr, err := net.SplitHostPort(c.ListenOn)
	if err != nil {
		return "", 0, fmt.Errorf("ListenOn 必须是 host:port,得到 %q: %w", c.ListenOn, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return "", 0, fmt.Errorf("ListenOn 端口非法: %q", c.ListenOn)
	}
	return host, uint32(port), nil
}

// v1DisabledEnqueueTypes 是 v1 不许进白名单的类型及原因(用户拍板 2026-10-16,交接 §6 M2 / M3)。
//
// GM_ATTACH_CURRENCY_DEBT:scene 侧挂欠款会把同币种的新单合并进旧欠款,并用后来那单覆盖到期时间 / 原因 / 操作人
// (一张小单就能让大额欠款提前失效,过期欠款的剩余部分也会被复活);挂错之后也没有任何撤销类型。合并语义、
// 撤销类型(按金额减少)与单笔上限一起做完之前,白名单里出现它就拒启。GmSanctionPlayer 对它照常回
// kPlayerInboxDisabled(不在白名单里);类型注册与 scene 执行器不动,解禁时删掉这一项即可。
var v1DisabledEnqueueTypes = map[inboxpb.PlayerInboxMessageType]string{
	inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT: "欠款合并语义与撤销路径未就绪(交接 §6 M2 / M3)",
}

// ParseEnqueueTypes 把白名单里的枚举名解析成类型集合。未知名、UNSPECIFIED、重复、v1 未开放的类型都报错:
// 写错的名字若被静默忽略,运维会以为打开了某个类型,实际一直回 Disabled。Validate 与装配层(svc)都经它解析,
// 所以任何一条启动路径都拒绝 v1 未开放的类型。
func ParseEnqueueTypes(names []string) (map[inboxpb.PlayerInboxMessageType]struct{}, error) {
	out := make(map[inboxpb.PlayerInboxMessageType]struct{}, len(names))
	for _, name := range names {
		v, ok := inboxpb.PlayerInboxMessageType_value[name]
		if !ok {
			return nil, fmt.Errorf("PlayerInbox.EnqueueTypes 含未知类型名 %q(须是 PlayerInboxMessageType 的枚举名)", name)
		}
		t := inboxpb.PlayerInboxMessageType(v)
		if t == inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_UNSPECIFIED {
			return nil, errors.New("PlayerInbox.EnqueueTypes 不能含 PLAYER_INBOX_MSG_UNSPECIFIED")
		}
		if why, disabled := v1DisabledEnqueueTypes[t]; disabled {
			return nil, fmt.Errorf("PlayerInbox.EnqueueTypes 不能含 %s:v1 不开放,%s;v1 只放封锁 / 解封两种", name, why)
		}
		if _, dup := out[t]; dup {
			return nil, fmt.Errorf("PlayerInbox.EnqueueTypes 重复列出 %q", name)
		}
		out[t] = struct{}{}
	}
	return out, nil
}

// Validate 做启动期整体校验(fail-fast)。conf.MustLoad / conf.Load 会自动调用它(go-zero v1.10.0)。
// 每一条拒绝的都是"进程能起来、但行为静默出错"的配置。
func (c *Config) Validate() error {
	if c.ZoneId == 0 {
		return errors.New("ZoneId 必须非 0(K8s 全局服务用 ConfigMap 注入的 CurrentZoneId)")
	}
	if c.ListenOn == "" {
		return errors.New("ListenOn 不能为空")
	}
	if _, _, err := c.ListenHostPort(); err != nil {
		return err
	}
	if c.Timeout < MinRpcTimeoutMs || c.Timeout > MaxRpcTimeoutMs {
		return fmt.Errorf("Timeout(%d ms)必须在 [%d, %d] 内:scene 的 CallDeadlineMs 按 Timeout+2000 核对,"+
			"下限保证扣掉 %v 回包余量后仍有 I/O 预算", c.Timeout, MinRpcTimeoutMs, MaxRpcTimeoutMs, InBandReplyReserve)
	}
	if len(c.Etcd.Hosts) == 0 {
		return errors.New("Etcd.Hosts 不能为空(节点注册、scene 节点镜像与 killswitch 都依赖 etcd)")
	}
	if c.Etcd.Key != "" {
		return fmt.Errorf("Etcd.Key 必须留空(得到 %q):全局服务不注册 go-zero key,契约 D-13", c.Etcd.Key)
	}
	if c.LeaseTTL <= 0 {
		return fmt.Errorf("LeaseTTL 必须为正数,得到 %d", c.LeaseTTL)
	}
	if err := c.validateStatContentSuppressed(); err != nil {
		return err
	}

	if c.MySQL.Host == "" || c.MySQL.User == "" || c.MySQL.DBName == "" {
		return errors.New("MySQL.Host / MySQL.User / MySQL.DBName 都不能为空")
	}
	if _, err := authoritysql.ParseBackend(c.MySQL.Backend); err != nil {
		return fmt.Errorf("MySQL.Backend: %w", err)
	}
	if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`).MatchString(c.AccountRoleSchema) {
		return errors.New("AccountRoleSchema必须显式配置同事务域角色目录schema")
	}
	if c.MySQL.DBName != data.DatabaseName {
		return fmt.Errorf("MySQL.DBName 必须是 %q(得到 %q):收件箱独占该库,表不许建进别的库(D-14)",
			data.DatabaseName, c.MySQL.DBName)
	}
	if c.MySQL.MaxOpenConn <= 0 || c.MySQL.MaxIdleConn <= 0 {
		return fmt.Errorf("MySQL.MaxOpenConn(%d)与 MySQL.MaxIdleConn(%d)必须为正数",
			c.MySQL.MaxOpenConn, c.MySQL.MaxIdleConn)
	}

	if c.CoreRedis.Host == "" {
		return errors.New("CoreRedis.Host 不能为空:围栏上界读 player:{id}:owner_epoch、在线提醒读 player:{id}:location," +
			"两者都在与 C++ scene 相同的核心 Redis 上")
	}
	if c.CoreRedis.PoolSize <= 0 {
		return fmt.Errorf("CoreRedis.PoolSize(%d)必须显式写正整数:0 在 go-redis 里是缺省 10 × GOMAXPROCS,"+
			"随 CPU limit 漂移,Redis 连接总账(k8s_deploy.ps1 镜像同一个键)就对不上", c.CoreRedis.PoolSize)
	}
	if len(c.Kafka.Brokers) == 0 {
		return errors.New("Kafka.Brokers 不能为空:在线提醒经 scene-cmd 写 Kafka")
	}
	if c.Kafka.WriteTimeoutMs <= 0 {
		return fmt.Errorf("Kafka.WriteTimeoutMs 必须为正数,得到 %d", c.Kafka.WriteTimeoutMs)
	}
	if !HasRpcTarget(c.DataServiceRpc) {
		return errors.New("DataServiceRpc 必须配置 Etcd.Key / Endpoints / Target 之一:玩家存在校验与对账都依赖 data_service")
	}

	p := c.PlayerInbox
	if p.AdminToken != "" && p.AdminTokenEnv != "" {
		return errors.New("PlayerInbox.AdminToken 与 PlayerInbox.AdminTokenEnv 只能二选一:两个口令来源说不清以哪个为准" +
			"(K8s 只用 AdminTokenEnv,字面 AdminToken 只给不经部署脚本的临时环境)")
	}
	if p.AdminTokenEnv != "" && !adminTokenEnvPattern.MatchString(p.AdminTokenEnv) {
		// 不回显:这条分支最常见的成因是把口令本身贴进了 AdminTokenEnv(AGENTS §11.3)。
		return fmt.Errorf("PlayerInbox.AdminTokenEnv 必须是形如 PLAYER_INBOX_ADMIN_TOKEN 的环境变量名"+
			"(得到一个长度 %d 的值,形状不符;此处刻意不回显内容)", len(p.AdminTokenEnv))
	}
	if p.AdminToken != "" && len(p.AdminToken) < MinAdminTokenBytes {
		// 不回显口令内容(AGENTS §11.3)。
		return fmt.Errorf("PlayerInbox.AdminToken 非空时至少 %d 字节(得到长度 %d,此处刻意不回显内容);"+
			"不打算开运维面就留空", MinAdminTokenBytes, len(p.AdminToken))
	}
	if _, err := ParseEnqueueTypes(p.EnqueueTypes); err != nil {
		return err
	}
	if p.TerminalRetentionDays < minRetentionDays || p.TerminalRetentionDays > maxRetentionDays {
		return fmt.Errorf("PlayerInbox.TerminalRetentionDays(%d)必须在 [%d, %d] 内:必须大于签发窗 %v,"+
			"否则遵守契约的重试可能在行被删后重复入箱", p.TerminalRetentionDays, minRetentionDays, maxRetentionDays,
			constants.IssuedAtMaxPast)
	}
	if time.Duration(p.TerminalRetentionDays)*24*time.Hour <= constants.IssuedAtMaxPast {
		return fmt.Errorf("PlayerInbox.TerminalRetentionDays(%d 天)必须大于签发窗 %v", p.TerminalRetentionDays, constants.IssuedAtMaxPast)
	}
	if p.SweepIntervalSeconds < minSweepIntervalSeconds || p.SweepIntervalSeconds > maxSweepIntervalSeconds {
		return fmt.Errorf("PlayerInbox.SweepIntervalSeconds(%d)必须在 [%d, %d] 内",
			p.SweepIntervalSeconds, minSweepIntervalSeconds, maxSweepIntervalSeconds)
	}
	if p.NotifyResendIntervalSeconds < minNotifyResendSeconds || p.NotifyResendIntervalSeconds > maxNotifyResendSeconds {
		return fmt.Errorf("PlayerInbox.NotifyResendIntervalSeconds(%d)必须在 [%d, %d] 内",
			p.NotifyResendIntervalSeconds, minNotifyResendSeconds, maxNotifyResendSeconds)
	}
	if p.GaugeIntervalSeconds < minGaugeIntervalSeconds || p.GaugeIntervalSeconds > maxGaugeIntervalSeconds {
		return fmt.Errorf("PlayerInbox.GaugeIntervalSeconds(%d)必须在 [%d, %d] 内",
			p.GaugeIntervalSeconds, minGaugeIntervalSeconds, maxGaugeIntervalSeconds)
	}
	if p.ReconcileIntervalMinutes != 0 {
		if p.ReconcileIntervalMinutes < minReconcileIntervalMins || p.ReconcileIntervalMinutes > maxReconcileIntervalMins {
			return fmt.Errorf("PlayerInbox.ReconcileIntervalMinutes(%d)必须为 0(不跑)或在 [%d, %d] 内",
				p.ReconcileIntervalMinutes, minReconcileIntervalMins, maxReconcileIntervalMins)
		}
		if p.ReconcileWindowHours < minReconcileWindowHours || p.ReconcileWindowHours > maxReconcileWindowHours {
			return fmt.Errorf("PlayerInbox.ReconcileWindowHours(%d)必须在 [%d, %d] 内",
				p.ReconcileWindowHours, minReconcileWindowHours, maxReconcileWindowHours)
		}
		if p.ReconcileWindowHours*60 < 2*p.ReconcileIntervalMinutes {
			// 窗口短于一个间隔时,两轮之间终结的行可能一轮都没被核对就滑出窗口;要求两倍,
			// 是给一轮失败 / 超时 / 易主留余量 —— 每一行至少有两次被核对的机会(评审 L4)。
			return fmt.Errorf("PlayerInbox.ReconcileWindowHours(%d 小时)必须至少覆盖两轮对账间隔"+
				"(%d×60 ≥ 2×%d 分钟):否则一轮失败或易主就会有终结的行一次都没被核对就滑出窗口",
				p.ReconcileWindowHours, p.ReconcileWindowHours, p.ReconcileIntervalMinutes)
		}
	}
	return nil
}

// statContentSuppressedMethods 是必须列进 Middlewares.StatConf.IgnoreContentMethods 的方法(评审 L12)。
//
// go-zero 的 Stat 拦截器(Middlewares.Stat 缺省 true)对每个非慢调用把请求体整包 JSON 按 INFO 打日志。
// FetchItems / AckItems 每次落地、每次确认各一条,量随在线人数线性涨,请求体对排障没有增量(异常结局另有带
// player_id 的 ERROR 日志),所以只屏蔽它们的**内容**:方法名、对端、耗时照常记,慢调用照样打 slowcall。
// PlayerInboxAdmin(GM 处置、作废等)**不**在这里:它们的请求体是审计线索。
var statContentSuppressedMethods = []string{
	inboxpb.PlayerInbox_FetchItems_FullMethodName,
	inboxpb.PlayerInbox_AckItems_FullMethodName,
}

// validateStatContentSuppressed 确认 Fetch / Ack 的请求内容不会被 Stat 拦截器打进日志:要么整体关掉 Stat,
// 要么把它们全部列进 IgnoreContentMethods(照 go/client_rpc_router)。K8s ConfigMap 漏了这一段时拒启,
// 而不是安静地把每次落地的请求体灌进 Loki。
func (c *Config) validateStatContentSuppressed() error {
	if !c.Middlewares.Stat {
		return nil
	}
	listed := make(map[string]bool, len(c.Middlewares.StatConf.IgnoreContentMethods))
	for _, m := range c.Middlewares.StatConf.IgnoreContentMethods {
		listed[m] = true
	}
	for _, m := range statContentSuppressedMethods {
		if !listed[m] {
			return fmt.Errorf("Middlewares.StatConf.IgnoreContentMethods 必须包含 %q(全部应列:%v):否则 go-zero Stat 拦截器"+
				"会把每次落地 / 确认的请求体整包按 INFO 打进日志;或显式设 Middlewares.Stat=false", m, statContentSuppressedMethods)
		}
	}
	return nil
}

// HasRpcTarget 判断 zrpc 客户端配置是否指向了任何目标(etcd key / 直连端点 / target),与 go/trade 同一判据。
func HasRpcTarget(c zrpc.RpcClientConf) bool {
	return c.Etcd.Key != "" || len(c.Endpoints) > 0 || c.Target != ""
}
