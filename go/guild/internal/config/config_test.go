package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"

	"shared/assetopwake"
	"shared/playerredis"
)

// 直接加载当前正式样例，避免测试注入掩盖全局会话权威的配置回归。
func loadEtcWithAuthorityForTest(t *testing.T, c *Config) error {
	t.Helper()
	return conf.Load("../../etc/guild.yaml", c)
}

func TestEtcTemplateRequiresExplicitGlobalSessions(t *testing.T) {
	var c Config
	if err := loadEtcWithAuthorityForTest(t, &c); err != nil {
		t.Fatalf("当前样例显式全局权威配置应可加载: %v", err)
	}
	if c.AccountRoleSchema == "" || c.MySQL.Backend != "tidb" || c.MatchAuthoritySchema == "" || !c.GlobalLoginSessions || !c.LoginRedis.HasAddr() {
		t.Fatal("生产样例必须显式声明同事务域权威库、Match权威库和全局LoginRedis")
	}
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"未声明全局会话", func(c *Config) { c.GlobalLoginSessions = false }},
		{"登录实例缺失", func(c *Config) { c.LoginRedis.Addrs = nil }},
		{"非法登录DB", func(c *Config) { c.LoginRedis.DB = -1 }},
		{"非法登录连接池", func(c *Config) { c.LoginRedis.PoolSizePerNode = -1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := c
			test.change(&invalid)
			if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "LoginRedis") {
				t.Fatalf("缺失或非法全局会话权威必须拒启: %v", err)
			}
		})
	}
}

// TestEtcYamlEnablesPrometheus 钉死 etc/guild.yaml 里的 Prometheus 段确实能被解析、
// 并且真的会启用 /metrics。
//
// 这条是「公会服务完全没有 /metrics」那个可观测性缺口的回归护栏:
// go-zero 的 prometheus.StartAgent 判的是 `len(c.Host) == 0 → 直接 return`,
// 所以 Host 空 = 端点根本不存在,而配置解析不会报任何错 —— 光看启动日志发现不了。
// 顺带把 yaml 本身的可解析性也覆盖了:段名写错(比如写成 Prometheus 的复数形式)
// 时字段会静默保持零值,同样只有这里能抓到。
func TestEtcYamlEnablesPrometheus(t *testing.T) {
	var c Config
	if err := loadEtcWithAuthorityForTest(t, &c); err != nil {
		t.Fatalf("加载 etc/guild.yaml 失败: %v", err)
	}

	if c.Prometheus.Host == "" {
		t.Fatal("Prometheus.Host 为空,go-zero 的 StartAgent 会直接 return,/metrics 不存在")
	}
	if c.Prometheus.Port == 0 {
		t.Fatal("Prometheus.Port 为 0")
	}
	if c.Prometheus.Path != "/metrics" {
		t.Errorf("Prometheus.Path = %q,期望 /metrics", c.Prometheus.Path)
	}
	// 端口分工:login 9101 / scene_manager 9150 / db 9160 / match 9170 /
	// friend 9180 / player_locator 9190 / guild 9220。撞端口会让两个服务里后起的那个
	// 静默拿不到 /metrics(ListenAndServe 报错只落一行日志)。
	if c.Prometheus.Port != 9220 {
		t.Errorf("Prometheus.Port = %d,公会服务约定用 9220", c.Prometheus.Port)
	}
}

// minimalGuildYaml 是一份能过 go-zero 必填校验的最小配置。
const minimalGuildYaml = `
Name: guild.rpc
ListenOn: 127.0.0.1:50300
Timeout: 4000
RedisClient:
  Host: 127.0.0.1:6379
  Password: ""
  DB: 2
PlayerLocatorRedis:
  Host: 127.0.0.1:6379
  Password: ""
  DB: 0
MySQL:
  DataSource: "u:p@tcp(127.0.0.1:3306)/mmorpg_guild"
DataServiceRpc:
  Timeout: 2000
Node:
  ZoneId: 1
  LeaseTTL: 500
Registry:
  Etcd:
    Hosts:
      - 127.0.0.1:2379
    DialTimeout: 5s
Cache:
  DefaultTTL: 30m
  MaxMembers: 50
Kafka:
  Brokers:
    - 127.0.0.1:9092
`

// loadMinimal 反序列化最小配置(Validate 由 conf 自动执行);用于在其上改单个字段再直接调 Validate。
func loadMinimal(t *testing.T) Config {
	t.Helper()
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(minimalGuildYaml), &c); err != nil {
		t.Fatalf("最小配置必须能通过校验: %v", err)
	}
	return c
}

// 门禁只看档位是否合法,不看 Mode:两档都能上生产(player-data-tidb-scaling-plan.md §9)。
func TestGuildBackendMustBeLegalInEveryMode(t *testing.T) {
	for _, mode := range []string{"pro", "pre", "test", "dev"} {
		for backend, legal := range map[string]bool{"tidb": true, "mysql": true, "oracle": false} {
			t.Run(mode+"/"+backend, func(t *testing.T) {
				input := "Mode: " + mode + "\n" + strings.Replace(minimalGuildYaml, "MySQL:\n", "MySQL:\n  Backend: "+backend+"\n", 1)
				var c Config
				err := conf.LoadFromYamlBytes([]byte(input), &c)
				if legal && err != nil {
					t.Fatalf("合法档位应放行: %v", err)
				}
				if !legal && (err == nil || !strings.Contains(err.Error(), "MySQL.Backend")) {
					t.Fatalf("非法档位必须拒启,得到 %v", err)
				}
			})
		}
	}
}

func TestGuildAuthorityDefaultsAndPoolBounds(t *testing.T) {
	c := loadMinimal(t)
	if c.MySQL.Backend != "tidb" || c.MySQL.MaxOpenConn != 20 {
		t.Fatal("未配置时必须是tidb档且固定连接池默认20")
	}
	c.MySQL.MaxOpenConn = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("兼容零值必须使用有限默认连接池: %v", err)
	}
	if open, _ := c.MySQL.EffectivePool(); open != DefaultMySQLMaxOpenConn {
		t.Fatalf("零值不能变成无限连接池, 生效上限=%d", open)
	}
	for _, size := range []int{-1, 129} {
		c.MySQL.MaxOpenConn = size
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "MaxOpenConn") {
			t.Fatalf("非法连接池上限%d必须拒启,得到%v", size, err)
		}
	}
}

func TestTrialMatchAuthoritySchemaMustBeExplicit(t *testing.T) {
	c := loadMinimal(t)
	c.AccountRoleSchema = "global_roles"
	c.GlobalLoginSessions = true
	c.LoginRedis.Addrs = []string{"127.0.0.1:26379"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "MatchAuthoritySchema") {
		t.Fatalf("不能从DSN猜Match权威库: %v", err)
	}
	c.MatchAuthoritySchema = "mmorpg_match"
	if err := c.Validate(); err != nil {
		t.Fatalf("显式同集群schema配置应可继续装配核表: %v", err)
	}
}

func TestGuildSQLDataSourceEnvIsAuthoritative(t *testing.T) {
	const env = "GUILD_UNIT_TEST_SQL_DSN"
	c := loadMinimal(t)
	c.MySQL.DataSourceEnv = env
	t.Setenv(env, "")
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "DataSourceEnv") {
		t.Fatalf("显式环境来源缺失不能回退样例DSN: %v", err)
	}
	t.Setenv(env, "u:must-not-leak@tcp(127.0.0.1:3306)/mmorpg_guild")
	if err := c.Validate(); err != nil {
		t.Fatalf("合法环境来源应通过: %v", err)
	}
	if c.MySQL.DataSource != os.Getenv(env) {
		t.Fatal("环境来源没有覆盖样例DSN")
	}
	// K8s不提供DataSource字面量，只注入Secret指定的环境来源。
	input := strings.Replace(minimalGuildYaml,
		`  DataSource: "u:p@tcp(127.0.0.1:3306)/mmorpg_guild"`,
		"  DataSourceEnv: "+env, 1)
	var fromEnvOnly Config
	if err := conf.LoadFromYamlBytes([]byte(input), &fromEnvOnly); err != nil {
		t.Fatalf("仅环境来源的部署配置应可加载: %v", err)
	}
	if fromEnvOnly.MySQL.DataSource != os.Getenv(env) {
		t.Fatal("部署配置未解析Secret环境来源")
	}
	t.Setenv(env, "u:must-not-leak@tcp(::broken")
	if err := c.Validate(); err == nil || strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("非法环境DSN必须拒启且不泄露口令: %v", err)
	}
}

// TestValidateTimeoutBudget:Timeout 是路由服 5s 转发预算减去 1s;预算还必须放得下
// 一次 data_service 调用(≤3s)+ 归属区查询(1.5s)+ 回包余量(0.5s),否则建帮会在
// 服务端超时,客户端看到的是 DeadlineExceeded 而不是业务 tip。
func TestValidateTimeoutBudget(t *testing.T) {
	cases := []struct {
		timeout, dataService int64
		wantErr              string
	}{
		{4000, 2000, ""},
		{3500, 1500, ""},
		{4000, 500, ""},
		{4001, 2000, "Timeout"},
		{10000, 2000, "Timeout"},
		{0, 2000, "Timeout"},
		{3999, 2000, "超时预算不足"},
		{4000, 3000, "超时预算不足"},
		{4000, 0, "DataServiceRpc.Timeout"},
		{4000, 499, "DataServiceRpc.Timeout"},
		{4000, 3001, "DataServiceRpc.Timeout"},
	}
	for _, tc := range cases {
		c := loadMinimal(t)
		c.Timeout = tc.timeout
		c.DataServiceRpc.Timeout = tc.dataService
		err := c.Validate()
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("Timeout=%d DataServiceRpc=%d 应通过,得到 %v", tc.timeout, tc.dataService, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("Timeout=%d DataServiceRpc=%d 错误 = %v,期望包含 %q", tc.timeout, tc.dataService, err, tc.wantErr)
		}
	}
}

// TestValidateCallsEmbeddedRpcServerConfValidate:本包的 Validate 遮蔽了嵌入的
// zrpc.RpcServerConf.Validate,忘了显式调用就会丢掉 go-zero 自己的必填校验。
func TestValidateCallsEmbeddedRpcServerConfValidate(t *testing.T) {
	c := loadMinimal(t)
	c.Auth = true
	if err := c.Validate(); err == nil {
		t.Fatal("Auth=true 且未配 Redis 时,嵌入的 RpcServerConf.Validate 必须报错")
	}
	c.Auth = false
	if err := c.Validate(); err != nil {
		t.Fatalf("Auth=false 不该因 Redis 报错: %v", err)
	}
}

// TestValidateRequiresGuildDatabase:帮会表只建在独占库(D-14);DSN 指错库必须拒启,
// 且错误文案不得带出 DSN 里的口令。
func TestValidateRequiresGuildDatabase(t *testing.T) {
	c := loadMinimal(t)
	c.MySQL.DataSource = "u:p@tcp(127.0.0.1:3306)/mmorpg"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "mmorpg_guild") {
		t.Fatalf("库名不是 mmorpg_guild 时必须报错,得到 %v", err)
	}

	c.MySQL.DataSource = "u:p@tcp(127.0.0.1:3306)/mmorpg_guild"
	if err := c.Validate(); err != nil {
		t.Fatalf("独占库应通过: %v", err)
	}

	c.MySQL.DataSource = "u:p4ss@tcp(::bad"
	err = c.Validate()
	if err == nil {
		t.Fatal("DSN 无法解析时必须报错")
	}
	if strings.Contains(err.Error(), "p4ss") {
		t.Fatalf("错误文案泄露了口令: %v", err)
	}
}

// TestValidateRejectsClientFoundRows(06-activities.md §6.11.2、契约偏差 14、90 Y-20):
// 活动与经济的计数 upsert 以 RowsAffected=0 判定"已达上限",clientFoundRows=true 会让它恒为非 0,
// 超限的参与被当成成功。显式打开必须拒启,错误文案不得带出口令;显式写 false 与不写都放行。
func TestValidateRejectsClientFoundRows(t *testing.T) {
	c := loadMinimal(t)
	c.MySQL.DataSource = "u:p4ss@tcp(127.0.0.1:3306)/mmorpg_guild?clientFoundRows=true"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "clientFoundRows") {
		t.Fatalf("clientFoundRows=true 必须拒启,得到 %v", err)
	}
	if strings.Contains(err.Error(), "p4ss") {
		t.Fatalf("错误文案泄露了口令: %v", err)
	}

	c.MySQL.DataSource = "u:p4ss@tcp(127.0.0.1:3306)/mmorpg_guild?clientFoundRows=false"
	if err := c.Validate(); err != nil {
		t.Fatalf("clientFoundRows=false 应通过: %v", err)
	}
}

// TestValidateMySQLPool(K8s 上线批 BK8s):连接池上限不写取缺省值;写了必须为正、空闲不超过上限。
// 以前不设上限 = database/sql 不限,MySQL 连接总账无从记账。
func TestValidateMySQLPool(t *testing.T) {
	c := loadMinimal(t)
	c.MySQL.MaxOpenConn, c.MySQL.MaxIdleConn = 0, 0
	if err := c.Validate(); err != nil {
		t.Fatalf("不写池参数应通过(取缺省值): %v", err)
	}
	if open, idle := c.MySQL.EffectivePool(); open != DefaultMySQLMaxOpenConn || idle != DefaultMySQLMaxIdleConn {
		t.Fatalf("缺省池参数 = %d / %d,期望 %d / %d", open, idle, DefaultMySQLMaxOpenConn, DefaultMySQLMaxIdleConn)
	}

	c.MySQL.MaxOpenConn, c.MySQL.MaxIdleConn = 30, 10
	if err := c.Validate(); err != nil {
		t.Fatalf("30 / 10 应通过: %v", err)
	}
	if open, idle := c.MySQL.EffectivePool(); open != 30 || idle != 10 {
		t.Fatalf("显式池参数 = %d / %d,期望 30 / 10", open, idle)
	}

	for _, tc := range []struct{ open, idle int }{{-1, 0}, {0, -1}, {4, 5}, {3, 0}} {
		c.MySQL.MaxOpenConn, c.MySQL.MaxIdleConn = tc.open, tc.idle
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "MySQL.Max") {
			t.Fatalf("MaxOpenConn=%d MaxIdleConn=%d 必须拒启,得到 %v", tc.open, tc.idle, err)
		}
	}
}

func TestEtcYamlPassesValidate(t *testing.T) {
	var c Config
	if err := loadEtcWithAuthorityForTest(t, &c); err != nil {
		t.Fatalf("加载 etc/guild.yaml 失败: %v", err)
	}
	if c.Timeout != 4000 {
		t.Errorf("Timeout = %d,期望 4000", c.Timeout)
	}
	if c.DataServiceRpc.Timeout != 2000 {
		t.Errorf("DataServiceRpc.Timeout = %d,期望 2000", c.DataServiceRpc.Timeout)
	}
	if c.ShouldAutoMigrate() {
		t.Error("dev档etc/guild.yaml也应关闭Schema.AutoMigrate")
	}
	if got := c.RequestBudget(); got != 3500*time.Millisecond {
		t.Errorf("RequestBudget = %v,期望 3500ms", got)
	}
	if c.MySQL.MaxOpenConn != 20 || c.MySQL.MaxIdleConn != 5 {
		t.Errorf("MySQL 池参数 = %d / %d,期望 20 / 5(K8s ConfigMap 与 MySQL 连接总账都镜像这两个键)", c.MySQL.MaxOpenConn, c.MySQL.MaxIdleConn)
	}
}

// ── 资产通道(AssetOp,B5b)────────────────────────────────────

// TestAssetOpAbsentMeansDisabled 钉住"整段缺失 = 关"(裁决 D,fail-closed):
// 一份配置抄到别的环境时少抄这一段,结果必须是通道关着;而且关着时零值的循环 / 清理参数
// 不许让 Validate 拒启(那会让所有没配这段的环境直接起不来)。
func TestAssetOpAbsentMeansDisabled(t *testing.T) {
	c := loadMinimal(t)
	if c.AssetOp.Enabled {
		t.Fatal("没写 AssetOp 段时资产通道必须是关的")
	}
	if c.AssetOp.CleanupEnabled {
		t.Fatal("没写 AssetOp 段时清理必须是关的")
	}
	if c.AssetOp.Workers != 0 || c.AssetOp.LeaseMs != 0 {
		t.Fatalf("整段缺失时 go-zero 不回填 default,字段应为零值,得到 %+v", c.AssetOp)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("通道关闭时零值参数不该被校验: %v", err)
	}
}

// assetOpReadyYaml 在最小配置上补齐资产通道的两项前置:号段开启 + data_service 有目标。
// 用替换而不是再追加一个 DataServiceRpc 段:yaml 里同名键出现两次会直接加载失败。
func assetOpReadyYaml(t *testing.T) string {
	t.Helper()
	const from = "DataServiceRpc:\n  Timeout: 2000\n"
	const to = "DataServiceRpc:\n  Endpoints:\n    - 127.0.0.1:9000\n  Timeout: 2000\nIdSegment:\n  Enabled: true\n"
	if !strings.Contains(minimalGuildYaml, from) {
		t.Fatal("minimalGuildYaml 的 DataServiceRpc 段形状变了,同步改 assetOpReadyYaml")
	}
	return strings.Replace(minimalGuildYaml, from, to, 1)
}

// loadAssetOpEnabled 加载"只写 Enabled: true"的配置(加载即校验)。
func loadAssetOpEnabled(t *testing.T) Config {
	t.Helper()
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(assetOpReadyYaml(t)+"AssetOp:\n  Enabled: true\n"), &c); err != nil {
		t.Fatalf("只写 Enabled: true 的配置必须能通过校验: %v", err)
	}
	return c
}

// TestAssetOpEnabledFillsY06Defaults:段一出现,go-zero 就按 default 回填没写的键,
// 得到的必须是 90-consistency Y-06 的那组值。段名或 default 写错时字段会静默保持 0,
// 只有这里能抓到。
func TestAssetOpEnabledFillsY06Defaults(t *testing.T) {
	a := loadAssetOpEnabled(t).AssetOp
	if !a.Enabled {
		t.Fatal("Enabled 应为 true")
	}
	checks := []struct {
		name      string
		got, want int
	}{
		{"ReconcileIntervalMs", a.ReconcileIntervalMs, 2000},
		{"ReconcileBatch", a.ReconcileBatch, 100},
		{"Workers", a.Workers, 8},
		{"LeaseMs", a.LeaseMs, 10000},
		{"OpBudgetMs", a.OpBudgetMs, 2500},
		{"MaxBackoffMs", a.MaxBackoffMs, 60000},
		{"PoisonDelayMs", a.PoisonDelayMs, 3600000},
		{"LedgerReadMinAttempts", a.LedgerReadMinAttempts, 3},
		{"CleanupIntervalMinutes", a.CleanupIntervalMinutes, 10},
		{"TerminalRetentionDays", a.TerminalRetentionDays, 30},
		{"CounterRetentionDays", a.CounterRetentionDays, 30},
		// 落地恢复(docs/design/landing-recovery-sync-digest.md §6):与 assetop.DefaultLoopConfig 同值。
		{"OfflineAfterAttempts", a.OfflineAfterAttempts, 5},
		{"OfflineBackoffMs", a.OfflineBackoffMs, 1800000},
		{"AbandonAfterDays", a.AbandonAfterDays, 180},
		{"AbandonSafetyGapMs", a.AbandonSafetyGapMs, 3600000},
		{"AbandonBatch", a.AbandonBatch, 200},
		{"AbandonIntervalMs", a.AbandonIntervalMs, 3600000},
		{"AuditIntervalMs", a.AuditIntervalMs, 86400000},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("AssetOp.%s = %d,Y-06 默认值是 %d", ck.name, ck.got, ck.want)
		}
	}
	// Wake 段没写:go-zero 不下钻整段 optional 的嵌套结构,字段全零;默认值由读取方法给出(AssetOpWakeConf 的说明)。
	if a.Wake.Enabled != nil {
		t.Errorf("Wake 段没写时 Enabled 应为 nil(= 开),得到 %v", *a.Wake.Enabled)
	}
	if !a.Wake.On() {
		t.Error("Wake 段没写时落地唤醒应当是开的")
	}
	if got := a.Wake.EffectiveConcurrency(); got != assetopwake.DefaultConcurrency {
		t.Errorf("Wake.EffectiveConcurrency = %d,默认应为 assetopwake.DefaultConcurrency(%d)", got, assetopwake.DefaultConcurrency)
	}
	if got := a.Wake.EffectiveTimeout(); got != assetopwake.DefaultWakeTimeout {
		t.Errorf("Wake.EffectiveTimeout = %v,默认应为 assetopwake.DefaultWakeTimeout(%v)", got, assetopwake.DefaultWakeTimeout)
	}
	if a.CleanupEnabled {
		t.Error("CleanupEnabled 没写时应为 false(只有显式打开才清理)")
	}
	// 常量与 struct tag 是同一个默认值的两处写法:段缺失时回档检查按常量回落(svc.RollbackProofRetention),
	// 两处分叉会让"段缺失的副本"与"段出现但没写这个键的副本"对同一次回档给出不同结论。
	if a.TerminalRetentionDays != DefaultTerminalRetentionDays {
		t.Errorf("TerminalRetentionDays 的 tag 默认值 %d 与 DefaultTerminalRetentionDays %d 不一致",
			a.TerminalRetentionDays, DefaultTerminalRetentionDays)
	}
}

// TestAssetOpLoopBounds:循环参数越界各一例,以及每条边界的合法端点。
// 边界与 assetop.NewLoop 的校验同形(Workers ≤ 64、Batch ≥ Workers、Lease ≥ OpBudget + 2000),
// 这里放过的配置不该在 NewLoop 里才炸(svc 包的 LoopConfigFrom 测试守住另一半)。
func TestAssetOpLoopBounds(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(a *AssetOpConf)
		wantErr string // 空 = 应通过
	}{
		{"间隔下界", func(a *AssetOpConf) { a.ReconcileIntervalMs = 200 }, ""},
		{"间隔上界", func(a *AssetOpConf) { a.ReconcileIntervalMs = 60000 }, ""},
		{"间隔过小", func(a *AssetOpConf) { a.ReconcileIntervalMs = 199 }, "ReconcileIntervalMs"},
		{"间隔过大", func(a *AssetOpConf) { a.ReconcileIntervalMs = 60001 }, "ReconcileIntervalMs"},
		{"单 worker 单行", func(a *AssetOpConf) { a.Workers, a.ReconcileBatch = 1, 1 }, ""},
		{"worker 上界", func(a *AssetOpConf) { a.Workers, a.ReconcileBatch = 64, 64 }, ""},
		{"worker 为 0", func(a *AssetOpConf) { a.Workers = 0 }, "Workers"},
		{"worker 超 64", func(a *AssetOpConf) { a.Workers, a.ReconcileBatch = 65, 100 }, "Workers"},
		{"批小于 worker", func(a *AssetOpConf) { a.Workers, a.ReconcileBatch = 8, 7 }, "ReconcileBatch"},
		{"批上界", func(a *AssetOpConf) { a.ReconcileBatch = 1000 }, ""},
		{"批过大", func(a *AssetOpConf) { a.ReconcileBatch = 1001 }, "ReconcileBatch"},
		{"预算下界", func(a *AssetOpConf) { a.OpBudgetMs = 1000 }, ""},
		{"预算过小", func(a *AssetOpConf) { a.OpBudgetMs = 999 }, "OpBudgetMs"},
		// 预算顶到上界时,落地唤醒的生效超时(默认 10000)不足 2 × OpBudgetMs,必须一起调大(validateLanding)。
		{"预算上界", func(a *AssetOpConf) { a.OpBudgetMs, a.LeaseMs, a.Wake.TimeoutMs = 10000, 12000, 20000 }, ""},
		{"预算上界而唤醒超时仍取默认", func(a *AssetOpConf) { a.OpBudgetMs, a.LeaseMs = 10000, 12000 }, "Wake.TimeoutMs"},
		{"预算过大", func(a *AssetOpConf) { a.OpBudgetMs, a.LeaseMs = 10001, 20000 }, "OpBudgetMs"},
		{"租约恰好等于预算加余量", func(a *AssetOpConf) { a.OpBudgetMs, a.LeaseMs = 2500, 4500 }, ""},
		{"租约短于预算加余量", func(a *AssetOpConf) { a.OpBudgetMs, a.LeaseMs = 2500, 4499 }, "LeaseMs"},
		{"租约上界", func(a *AssetOpConf) { a.LeaseMs = 600000 }, ""},
		{"租约过大", func(a *AssetOpConf) { a.LeaseMs = 600001 }, "LeaseMs"},
		{"退避封顶下界", func(a *AssetOpConf) { a.MaxBackoffMs = 1000 }, ""},
		{"退避封顶过小", func(a *AssetOpConf) { a.MaxBackoffMs = 999 }, "MaxBackoffMs"},
		{"退避封顶过大", func(a *AssetOpConf) { a.MaxBackoffMs = 600001 }, "MaxBackoffMs"},
		{"毒行延迟下界", func(a *AssetOpConf) { a.PoisonDelayMs = 60000 }, ""},
		{"毒行延迟上界", func(a *AssetOpConf) { a.PoisonDelayMs = 86400000 }, ""},
		{"毒行延迟过小", func(a *AssetOpConf) { a.PoisonDelayMs = 59999 }, "PoisonDelayMs"},
		{"毒行延迟过大", func(a *AssetOpConf) { a.PoisonDelayMs = 86400001 }, "PoisonDelayMs"},
		{"账本门槛为 0", func(a *AssetOpConf) { a.LedgerReadMinAttempts = 0 }, "LedgerReadMinAttempts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadAssetOpEnabled(t)
			tc.mutate(&c.AssetOp)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过,得到 %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v,期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

// TestAssetOpLoopParamsIgnoredWhenDisabled:关着通道时循环参数不校验 —— 否则"改一行 Enabled: false
// 止血"会因为别的键不合法而起不来,止血开关本身失效。
func TestAssetOpLoopParamsIgnoredWhenDisabled(t *testing.T) {
	c := loadAssetOpEnabled(t)
	c.AssetOp.Enabled = false
	c.AssetOp.Workers = 0
	c.AssetOp.LeaseMs = 1
	c.IdSegment.Enabled = false
	if err := c.Validate(); err != nil {
		t.Fatalf("Enabled=false 时循环参数与号段前置都不该被校验: %v", err)
	}
}

// TestAssetOpRequiresIdSegmentAndDataService:开着通道却发不出 op_id,
// 表现是进程正常、每一次捐献 / 兑换都回发号失败 —— 必须炸在启动期。
func TestAssetOpRequiresIdSegmentAndDataService(t *testing.T) {
	c := loadAssetOpEnabled(t)
	c.IdSegment.Enabled = false
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "IdSegment") {
		t.Fatalf("Enabled=true 且号段关闭必须拒启,得到 %v", err)
	}

	c = loadAssetOpEnabled(t)
	c.DataServiceRpc.Endpoints = nil
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "DataServiceRpc") {
		t.Fatalf("Enabled=true 且 data_service 没有目标必须拒启,得到 %v", err)
	}
}

// TestAssetOpCleanupBounds:清理参数只在 CleanupEnabled 时校验,越界各一例 + 合法端点。
func TestAssetOpCleanupBounds(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(a *AssetOpConf)
		wantErr string
	}{
		{"间隔下界", func(a *AssetOpConf) { a.CleanupIntervalMinutes = 1 }, ""},
		{"间隔上界", func(a *AssetOpConf) { a.CleanupIntervalMinutes = 1440 }, ""},
		{"间隔为 0", func(a *AssetOpConf) { a.CleanupIntervalMinutes = 0 }, "CleanupIntervalMinutes"},
		{"间隔过大", func(a *AssetOpConf) { a.CleanupIntervalMinutes = 1441 }, "CleanupIntervalMinutes"},
		{"终态保留下界", func(a *AssetOpConf) { a.TerminalRetentionDays = 7 }, ""},
		{"终态保留上界", func(a *AssetOpConf) { a.TerminalRetentionDays = 365 }, ""},
		{"终态保留过短", func(a *AssetOpConf) { a.TerminalRetentionDays = 6 }, "TerminalRetentionDays"},
		{"终态保留过长", func(a *AssetOpConf) { a.TerminalRetentionDays = 366 }, "TerminalRetentionDays"},
		{"计数保留过短", func(a *AssetOpConf) { a.CounterRetentionDays = 6 }, "CounterRetentionDays"},
		{"计数保留过长", func(a *AssetOpConf) { a.CounterRetentionDays = 366 }, "CounterRetentionDays"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadAssetOpEnabled(t)
			c.AssetOp.CleanupEnabled = true
			tc.mutate(&c.AssetOp)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过,得到 %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v,期望包含 %q", err, tc.wantErr)
			}
		})
	}

	// 清理关着时,越界的清理参数不校验。
	c := loadAssetOpEnabled(t)
	c.AssetOp.CleanupEnabled = false
	c.AssetOp.CleanupIntervalMinutes = 0
	c.AssetOp.TerminalRetentionDays = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("CleanupEnabled=false 时清理参数不该被校验: %v", err)
	}
}

// TestEtcYamlAssetOpBlock:仓库里的 dev 配置把资产通道与清理都显式打开,
// 且每个键都显式写出(与 Y-06 默认值一致)—— 段名拼错时 Enabled 会静默变成 false,
// 本地就再也演练不到捐献 / 兑换。
func TestEtcYamlAssetOpBlock(t *testing.T) {
	var c Config
	if err := loadEtcWithAuthorityForTest(t, &c); err != nil {
		t.Fatalf("加载 etc/guild.yaml 失败: %v", err)
	}
	a := c.AssetOp
	if !a.Enabled {
		t.Fatal("etc/guild.yaml 应显式打开 AssetOp.Enabled(本地演练捐献 / 兑换)")
	}
	if !a.CleanupEnabled {
		t.Fatal("etc/guild.yaml 应显式打开 AssetOp.CleanupEnabled")
	}
	if a.Workers != 8 || a.LeaseMs != 10000 || a.OpBudgetMs != 2500 || a.ReconcileBatch != 100 ||
		a.ReconcileIntervalMs != 2000 || a.MaxBackoffMs != 60000 || a.PoisonDelayMs != 3600000 ||
		a.LedgerReadMinAttempts != 3 {
		t.Errorf("etc/guild.yaml 的循环参数偏离 Y-06 默认值: %+v", a)
	}
	if a.CleanupIntervalMinutes != 10 || a.TerminalRetentionDays != 30 || a.CounterRetentionDays != 30 {
		t.Errorf("etc/guild.yaml 的清理参数偏离默认值: %+v", a)
	}
	// 落地恢复(设计 §5.3 / §6):每个键显式写出且与默认值一致;键名拼错时字段会静默回落到 default,只有这里看得出来
	// 「写了却没生效」。本项目尚未上线,dev 配置直接开启自动放弃(滚动升级提示见 yaml 注释)。
	if a.OfflineAfterAttempts != 5 || a.OfflineBackoffMs != 1800000 {
		t.Errorf("etc/guild.yaml 的停放档参数偏离默认值: %+v", a)
	}
	if a.AbandonAfterDays != 180 || a.AbandonSafetyGapMs != 3600000 || a.AbandonBatch != 200 || a.AbandonIntervalMs != 3600000 {
		t.Errorf("etc/guild.yaml 的自动放弃参数偏离默认值: %+v", a)
	}
	if a.AuditIntervalMs != 86400000 {
		t.Errorf("etc/guild.yaml 的 AuditIntervalMs = %d,期望 86400000(每天一次)", a.AuditIntervalMs)
	}
	if a.Wake.Enabled == nil || !*a.Wake.Enabled {
		t.Error("etc/guild.yaml 应显式写 AssetOp.Wake.Enabled: true(本地演练落地唤醒)")
	}
	if a.Wake.Concurrency != assetopwake.DefaultConcurrency || a.Wake.EffectiveTimeout() != assetopwake.DefaultWakeTimeout {
		t.Errorf("etc/guild.yaml 的唤醒参数偏离默认值: %+v", a.Wake)
	}
}

// TestAssetOpLandingBounds:落地恢复四组参数(停放档 / 自动放弃 / 日对账 / 唤醒)越界各一例,以及每条边界的合法端点。
// 边界与 assetop.LoopConfig.validate / validateAbandon、assetopwake 的约束同形(停放间隔 ∈ [MaxBackoff, 24h]、
// 放弃门槛 0 或 ≥ 30 天、安全间隔 ≥ 10 分钟且 ≥ 租约、批 [1, 1000]、唤醒超时 ≥ 2 × OpBudget),这里放过的配置不该在
// assetop.NewLoop 里才炸。每组都是"0 = 关闭"时从属参数不校验 —— 否则"改一个 0 止血"会因为别的键越界而起不来。
func TestAssetOpLandingBounds(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(a *AssetOpConf)
		wantErr string // 空 = 应通过
	}{
		// 停放档
		{"停放档关闭", func(a *AssetOpConf) { a.OfflineAfterAttempts = 0 }, ""},
		{"停放档关闭时间隔不校验", func(a *AssetOpConf) { a.OfflineAfterAttempts, a.OfflineBackoffMs = 0, 1 }, ""},
		{"停放次数为负", func(a *AssetOpConf) { a.OfflineAfterAttempts = -1 }, "OfflineAfterAttempts"},
		{"停放次数上界", func(a *AssetOpConf) { a.OfflineAfterAttempts = 1000 }, ""},
		{"停放次数过大", func(a *AssetOpConf) { a.OfflineAfterAttempts = 1001 }, "OfflineAfterAttempts"},
		{"停放间隔恰好等于退避封顶", func(a *AssetOpConf) { a.OfflineBackoffMs = a.MaxBackoffMs }, ""},
		{"停放间隔短于退避封顶", func(a *AssetOpConf) { a.OfflineBackoffMs = a.MaxBackoffMs - 1 }, "OfflineBackoffMs"},
		{"停放间隔上界 24 小时", func(a *AssetOpConf) { a.OfflineBackoffMs = 86400000 }, ""},
		{"停放间隔超过 24 小时", func(a *AssetOpConf) { a.OfflineBackoffMs = 86400001 }, "OfflineBackoffMs"},

		// 自动放弃
		{"放弃关闭", func(a *AssetOpConf) { a.AbandonAfterDays = 0 }, ""},
		{"放弃关闭时从属参数不校验", func(a *AssetOpConf) {
			a.AbandonAfterDays, a.AbandonSafetyGapMs, a.AbandonBatch, a.AbandonIntervalMs = 0, 0, 0, 0
		}, ""},
		{"放弃门槛为负", func(a *AssetOpConf) { a.AbandonAfterDays = -1 }, "AbandonAfterDays"},
		{"放弃门槛下界 30 天", func(a *AssetOpConf) { a.AbandonAfterDays = 30 }, ""},
		{"放弃门槛不足 30 天", func(a *AssetOpConf) { a.AbandonAfterDays = 29 }, "AbandonAfterDays"},
		{"放弃门槛上界", func(a *AssetOpConf) { a.AbandonAfterDays = 3650 }, ""},
		{"放弃门槛过大", func(a *AssetOpConf) { a.AbandonAfterDays = 3651 }, "AbandonAfterDays"},
		{"安全间隔下界 10 分钟", func(a *AssetOpConf) { a.AbandonSafetyGapMs = 600000 }, ""},
		{"安全间隔不足 10 分钟", func(a *AssetOpConf) { a.AbandonSafetyGapMs = 599999 }, "AbandonSafetyGapMs"},
		{"安全间隔恰好等于租约", func(a *AssetOpConf) { a.LeaseMs, a.AbandonSafetyGapMs = 600000, 600000 }, ""},
		{"安全间隔短于租约", func(a *AssetOpConf) { a.LeaseMs, a.AbandonSafetyGapMs = 600000, 599999 }, "AbandonSafetyGapMs"},
		{"放弃批下界", func(a *AssetOpConf) { a.AbandonBatch = 1 }, ""},
		{"放弃批为 0", func(a *AssetOpConf) { a.AbandonBatch = 0 }, "AbandonBatch"},
		{"放弃批上界", func(a *AssetOpConf) { a.AbandonBatch = 1000 }, ""},
		{"放弃批过大", func(a *AssetOpConf) { a.AbandonBatch = 1001 }, "AbandonBatch"},
		{"放弃间隔下界", func(a *AssetOpConf) { a.AbandonIntervalMs = 60000 }, ""},
		{"放弃间隔过小", func(a *AssetOpConf) { a.AbandonIntervalMs = 59999 }, "AbandonIntervalMs"},
		{"放弃间隔上界", func(a *AssetOpConf) { a.AbandonIntervalMs = 86400000 }, ""},
		{"放弃间隔过大", func(a *AssetOpConf) { a.AbandonIntervalMs = 86400001 }, "AbandonIntervalMs"},

		// 日对账
		{"对账关闭", func(a *AssetOpConf) { a.AuditIntervalMs = 0 }, ""},
		{"对账间隔下界", func(a *AssetOpConf) { a.AuditIntervalMs = 60000 }, ""},
		{"对账间隔过小", func(a *AssetOpConf) { a.AuditIntervalMs = 59999 }, "AuditIntervalMs"},
		{"对账间隔为负", func(a *AssetOpConf) { a.AuditIntervalMs = -1 }, "AuditIntervalMs"},
		{"对账间隔上界 7 天", func(a *AssetOpConf) { a.AuditIntervalMs = 7 * 86400000 }, ""},
		{"对账间隔超过 7 天", func(a *AssetOpConf) { a.AuditIntervalMs = 7*86400000 + 1 }, "AuditIntervalMs"},

		// 落地唤醒
		{"唤醒并发上界", func(a *AssetOpConf) { a.Wake.Concurrency = 64 }, ""},
		{"唤醒并发过大", func(a *AssetOpConf) { a.Wake.Concurrency = 65 }, "Wake.Concurrency"},
		{"唤醒并发为负", func(a *AssetOpConf) { a.Wake.Concurrency = -1 }, "Wake.Concurrency"},
		// 唤醒超时下界 = 2 × OpBudgetMs:先列行(Store 子预算 1000ms),剩余预算还要够一整个 OpBudget 才领行。
		// 只比 OpBudget 多 1ms 的旧合法值必须拒绝 —— 列完行就不够一个 OpBudget,每次唤醒一行不领、静默退化。
		{"唤醒超时恰好等于单行预算", func(a *AssetOpConf) { a.Wake.TimeoutMs = a.OpBudgetMs }, "Wake.TimeoutMs"},
		{"唤醒超时比单行预算多 1ms", func(a *AssetOpConf) { a.Wake.TimeoutMs = a.OpBudgetMs + 1 }, "Wake.TimeoutMs"},
		{"唤醒超时比两倍单行预算少 1ms", func(a *AssetOpConf) { a.Wake.TimeoutMs = 2*a.OpBudgetMs - 1 }, "Wake.TimeoutMs"},
		{"唤醒超时恰好两倍单行预算", func(a *AssetOpConf) { a.Wake.TimeoutMs = 2 * a.OpBudgetMs }, ""},
		{"预算下界时唤醒超时取两倍", func(a *AssetOpConf) { a.OpBudgetMs, a.Wake.TimeoutMs = 1000, 2000 }, ""},
		{"预算下界时唤醒超时不足两倍", func(a *AssetOpConf) { a.OpBudgetMs, a.Wake.TimeoutMs = 1000, 1999 }, "Wake.TimeoutMs"},
		{"预算上界时唤醒超时取两倍", func(a *AssetOpConf) { a.OpBudgetMs, a.LeaseMs, a.Wake.TimeoutMs = 10000, 12000, 20000 }, ""},
		{"预算上界时唤醒超时不足两倍", func(a *AssetOpConf) { a.OpBudgetMs, a.LeaseMs, a.Wake.TimeoutMs = 10000, 12000, 19999 }, "Wake.TimeoutMs"},
		{"唤醒超时上界", func(a *AssetOpConf) { a.Wake.TimeoutMs = 60000 }, ""},
		{"唤醒超时过大", func(a *AssetOpConf) { a.Wake.TimeoutMs = 60001 }, "Wake.TimeoutMs"},
		{"唤醒超时为负", func(a *AssetOpConf) { a.Wake.TimeoutMs = -1 }, "Wake.TimeoutMs"},
		{"唤醒显式关闭时参数不校验", func(a *AssetOpConf) {
			off := false
			a.Wake.Enabled, a.Wake.Concurrency, a.Wake.TimeoutMs = &off, -1, -1
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadAssetOpEnabled(t)
			tc.mutate(&c.AssetOp)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过,得到 %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v,期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

// TestAssetOpLandingParamsIgnoredWhenDisabled:通道关着时落地恢复参数同样不校验(同 TestAssetOpLoopParamsIgnoredWhenDisabled:
// "改一行 Enabled: false 止血"不能因为别的键越界而起不来)。
func TestAssetOpLandingParamsIgnoredWhenDisabled(t *testing.T) {
	c := loadAssetOpEnabled(t)
	c.AssetOp.Enabled = false
	c.AssetOp.AbandonAfterDays = 1
	c.AssetOp.OfflineBackoffMs = 1
	c.AssetOp.AuditIntervalMs = 1
	c.AssetOp.Wake.TimeoutMs = 1
	if err := c.Validate(); err != nil {
		t.Fatalf("Enabled=false 时落地恢复参数不该被校验: %v", err)
	}
}

// TestAssetOpWakeConfAccessors:显式写的值原样生效;显式 false 关闭;没写取 assetopwake 的默认值(唯一来源在那边)。
func TestAssetOpWakeConfAccessors(t *testing.T) {
	on, off := true, false
	cases := []struct {
		name        string
		w           AssetOpWakeConf
		wantOn      bool
		wantConc    int
		wantTimeout time.Duration
	}{
		{"没写", AssetOpWakeConf{}, true, assetopwake.DefaultConcurrency, assetopwake.DefaultWakeTimeout},
		{"显式开", AssetOpWakeConf{Enabled: &on, Concurrency: 2, TimeoutMs: 5000}, true, 2, 5 * time.Second},
		{"显式关", AssetOpWakeConf{Enabled: &off}, false, assetopwake.DefaultConcurrency, assetopwake.DefaultWakeTimeout},
	}
	for _, tc := range cases {
		if got := tc.w.On(); got != tc.wantOn {
			t.Errorf("%s: On() = %t,期望 %t", tc.name, got, tc.wantOn)
		}
		if got := tc.w.EffectiveConcurrency(); got != tc.wantConc {
			t.Errorf("%s: EffectiveConcurrency() = %d,期望 %d", tc.name, got, tc.wantConc)
		}
		if got := tc.w.EffectiveTimeout(); got != tc.wantTimeout {
			t.Errorf("%s: EffectiveTimeout() = %v,期望 %v", tc.name, got, tc.wantTimeout)
		}
	}

	// yaml 里显式写 false 必须解析成非 nil 的 false(*bool 才分得清"没写"与"关"),整段配置照样能过校验。
	var c Config
	yaml := assetOpReadyYaml(t) + "AssetOp:\n  Enabled: true\n  Wake:\n    Enabled: false\n"
	if err := conf.LoadFromYamlBytes([]byte(yaml), &c); err != nil {
		t.Fatalf("Wake.Enabled: false 的配置必须能通过校验: %v", err)
	}
	if c.AssetOp.Wake.Enabled == nil || *c.AssetOp.Wake.Enabled || c.AssetOp.Wake.On() {
		t.Fatalf("显式写 Wake.Enabled: false 必须关闭唤醒,得到 %+v", c.AssetOp.Wake)
	}
}

// ── 同道历练(MatchRpc / Activity,B6b)──────────────────────────

// TestActivityAbsentUsesDefaults 钉住"整段缺失"的两条相反语义(06 §6.22,见 ActivityConf 的说明):
// 四个节律回落到默认值(待入队循环与巡检器在历练关着时也要跑完存量),而结果消费是关的(= 历练未开放)。
// 少抄一段配置的环境必须起得来,且不会在没有结算的情况下放玩家开战。
func TestActivityAbsentUsesDefaults(t *testing.T) {
	c := loadMinimal(t)
	a := c.Activity
	if a.TrialResultOverdueSeconds != 0 || a.TrialAbandonSeconds != 0 || a.TrialSweepIntervalSeconds != 0 || a.OwedLoopIntervalSeconds != 0 {
		t.Fatalf("整段缺失时 go-zero 不回填 default,字段应为零值,得到 %+v", a)
	}
	checks := []struct {
		name      string
		got, want time.Duration
	}{
		{"TrialResultOverdue", a.TrialResultOverdue(), 420 * time.Second},
		{"TrialAbandon", a.TrialAbandon(), 3600 * time.Second},
		{"TrialSweepInterval", a.TrialSweepInterval(), 60 * time.Second},
		{"OwedLoopInterval", a.OwedLoopInterval(), 10 * time.Second},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("Activity.%s() = %v,整段缺失时应回落到默认 %v", ck.name, ck.got, ck.want)
		}
	}
	if a.TrialResult.Enabled || c.TrialResultActive() {
		t.Fatal("没写 Activity.TrialResult 时结果消费必须是关的(没有结算就不许开战)")
	}
	if c.MatchRpcConfigured() {
		t.Fatal("没写 MatchRpc 时不应判为已配置")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("整段缺失不该被拒启: %v", err)
	}
}

// loadTrialEnabled 在最小配置上只写 `Activity.TrialResult.Enabled: true`(加载即校验)。
func loadTrialEnabled(t *testing.T) Config {
	t.Helper()
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(minimalGuildYaml+"Activity:\n  TrialResult:\n    Enabled: true\n"), &c); err != nil {
		t.Fatalf("只写 TrialResult.Enabled: true 的配置必须能通过校验: %v", err)
	}
	return c
}

// TestActivityConfDefaultsMatchTags:段一出现,go-zero 按 struct tag 的 default 回填;回填值必须与
// XxxOrDefault 方法用的常量相同 —— 两处各写一份,改了一处忘了另一处时,"写了段"与"没写段"会得到两套节律。
func TestActivityConfDefaultsMatchTags(t *testing.T) {
	c := loadTrialEnabled(t)
	a := c.Activity
	nums := []struct {
		name      string
		got, want uint32
	}{
		{"TrialResultOverdueSeconds", a.TrialResultOverdueSeconds, defaultTrialResultOverdueSeconds},
		{"TrialAbandonSeconds", a.TrialAbandonSeconds, defaultTrialAbandonSeconds},
		{"TrialSweepIntervalSeconds", a.TrialSweepIntervalSeconds, defaultTrialSweepIntervalSeconds},
		{"OwedLoopIntervalSeconds", a.OwedLoopIntervalSeconds, defaultOwedLoopIntervalSeconds},
	}
	for _, ck := range nums {
		if ck.got != ck.want {
			t.Errorf("Activity.%s 的 tag default = %d,与代码里的默认常量 %d 不一致", ck.name, ck.got, ck.want)
		}
	}
	r := a.TrialResult
	if !r.Enabled || r.Topic != "match-results" || r.Partitions != 3 || r.GroupID != "guild-trial" {
		t.Errorf("TrialResult 默认值应为 match-results / 3 / guild-trial(与 match 的 ResultTopic、ResultTopicPartitions 对齐),得到 %+v", r)
	}
	if !c.TrialResultActive() {
		t.Error("Enabled 且配了 Brokers 时 TrialResultActive 应为 true")
	}
}

// TestTrialResultActiveNeedsBrokers:Brokers 为空不拒启(本地不起 Kafka 是正常形态),但历练必须视为未开放。
func TestTrialResultActiveNeedsBrokers(t *testing.T) {
	c := loadTrialEnabled(t)
	c.Kafka.Brokers = nil
	if c.TrialResultActive() {
		t.Fatal("没有 Brokers 时结果消费跑不起来,TrialResultActive 必须为 false")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Enabled 而没有 Brokers 不该拒启(由 guild.go 打告警并关闭历练): %v", err)
	}
}

// TestActivityBounds:节律越界各一例 + 合法端点;结果消费参数只在 Enabled 时校验。
func TestActivityBounds(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(a *ActivityConf)
		wantErr string // 空 = 应通过
	}{
		{"超时判定下界", func(a *ActivityConf) { a.TrialResultOverdueSeconds = 60 }, ""},
		{"超时判定过短", func(a *ActivityConf) { a.TrialResultOverdueSeconds = 59 }, "TrialResultOverdueSeconds"},
		{"超时判定写 0 取默认", func(a *ActivityConf) { a.TrialResultOverdueSeconds = 0 }, ""},
		{"放弃时长恰好等于超时加余量", func(a *ActivityConf) { a.TrialResultOverdueSeconds, a.TrialAbandonSeconds = 420, 1020 }, ""},
		{"放弃时长短于超时加余量", func(a *ActivityConf) { a.TrialResultOverdueSeconds, a.TrialAbandonSeconds = 420, 1019 }, "TrialAbandonSeconds"},
		{"放弃时长上界 7 天", func(a *ActivityConf) { a.TrialAbandonSeconds = 604800 }, ""},
		{"放弃时长超过保留期", func(a *ActivityConf) { a.TrialAbandonSeconds = 604801 }, "TrialAbandonSeconds"},
		{"超时判定大到挤掉默认放弃时长", func(a *ActivityConf) { a.TrialResultOverdueSeconds, a.TrialAbandonSeconds = 3600, 0 }, "TrialAbandonSeconds"},
		{"巡检间隔下界", func(a *ActivityConf) { a.TrialSweepIntervalSeconds = 10 }, ""},
		{"巡检间隔上界", func(a *ActivityConf) { a.TrialSweepIntervalSeconds = 600 }, ""},
		{"巡检间隔过短", func(a *ActivityConf) { a.TrialSweepIntervalSeconds = 9 }, "TrialSweepIntervalSeconds"},
		{"巡检间隔过长", func(a *ActivityConf) { a.TrialSweepIntervalSeconds = 601 }, "TrialSweepIntervalSeconds"},
		{"待入队间隔下界", func(a *ActivityConf) { a.OwedLoopIntervalSeconds = 1 }, ""},
		{"待入队间隔上界", func(a *ActivityConf) { a.OwedLoopIntervalSeconds = 60 }, ""},
		{"待入队间隔过长", func(a *ActivityConf) { a.OwedLoopIntervalSeconds = 61 }, "OwedLoopIntervalSeconds"},
		{"消费开着但 Topic 为空", func(a *ActivityConf) { a.TrialResult.Topic = "" }, "Topic"},
		{"消费开着但 GroupID 为空", func(a *ActivityConf) { a.TrialResult.GroupID = "" }, "GroupID"},
		{"消费开着但分区数为 0", func(a *ActivityConf) { a.TrialResult.Partitions = 0 }, "Partitions"},
		{"消费关着时不校验它的参数", func(a *ActivityConf) {
			a.TrialResult = TrialResultConf{}
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadTrialEnabled(t)
			tc.mutate(&c.Activity)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过,得到 %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 = %v,期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

// TestMatchRpcTimeoutBounds:MatchRpc 只在配了目标时校验(整段缺失时 Timeout 是 0,不能因此拒启);
// 配了目标时合法区间是 [300, 2000]:
//   - 超过 2000ms 拒启 —— 开战那次请求的预算里放不下更久的等待;
//   - 低于 300ms 拒启(含 0 = 不设客户端超时)—— logic 给 match 的预算不足 300ms 就不调它,Timeout 配得比这还小,
//     历练显示开放而每次开战必回"服务繁忙"。下限常量镜像 logic 的 minMatchBudget(logic 侧有用例守两者相等)。
func TestMatchRpcTimeoutBounds(t *testing.T) {
	c := loadMinimal(t)
	c.MatchRpc.Timeout = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("没配 MatchRpc 目标时不校验它的 Timeout: %v", err)
	}
	if MinMatchRpcTimeoutMs != 300 || MaxMatchRpcTimeoutMs != 2000 {
		t.Fatalf("MatchRpc.Timeout 的合法区间变了([%d, %d]):同步改下面的边界用例、etc/guild.yaml 的注释与 06 §6.22",
			MinMatchRpcTimeoutMs, MaxMatchRpcTimeoutMs)
	}

	cases := []struct {
		timeout int64
		wantErr bool
	}{
		{300, false}, {1500, false}, {2000, false},
		{0, true}, {-1, true}, {1, true}, {299, true}, {2001, true},
	}
	for _, tc := range cases {
		c := loadMinimal(t)
		c.MatchRpc.Endpoints = []string{"127.0.0.1:50500"}
		c.MatchRpc.Timeout = tc.timeout
		if !c.MatchRpcConfigured() {
			t.Fatal("配了 Endpoints 应判为已配置")
		}
		err := c.Validate()
		if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "MatchRpc.Timeout")) {
			t.Errorf("MatchRpc.Timeout=%d 必须拒启,得到 %v", tc.timeout, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("MatchRpc.Timeout=%d 应通过,得到 %v", tc.timeout, err)
		}
	}
}

// TestEtcYamlTrialBlock:仓库里的 dev 配置把同道历练接全(match 客户端 + 结果消费),段名拼错时这些字段会静默
// 保持零值、历练在本地永远显示"未开放"。同时钉住两条写在 yaml 注释里、解析结果看不出来的约定:
//   - MatchRpc 显式 NonBlock(match 没起时 guild 照常起服;svc 建客户端时也会强制);
//   - GroupID 不加引号(go_services.ps1 -Zone 只给带引号的 GroupID 加 _zN,加了就变成每个 zone 各结算一遍)。
func TestEtcYamlTrialBlock(t *testing.T) {
	const path = "../../etc/guild.yaml"
	var c Config
	if err := loadEtcWithAuthorityForTest(t, &c); err != nil {
		t.Fatalf("加载 etc/guild.yaml 失败: %v", err)
	}
	if !c.MatchRpcConfigured() || c.MatchRpc.Etcd.Key != "matchservice.rpc" {
		t.Errorf("etc/guild.yaml 应把 MatchRpc 指向 matchservice.rpc,得到 Etcd.Key=%q", c.MatchRpc.Etcd.Key)
	}
	if !c.MatchRpc.NonBlock {
		t.Error("MatchRpc 应显式 NonBlock: true(match 是弱依赖)")
	}
	if c.MatchRpc.Timeout != 1500 {
		t.Errorf("MatchRpc.Timeout = %d,期望 1500", c.MatchRpc.Timeout)
	}
	a := c.Activity
	if a.TrialResultOverdueSeconds != 420 || a.TrialAbandonSeconds != 3600 || a.TrialSweepIntervalSeconds != 60 || a.OwedLoopIntervalSeconds != 10 {
		t.Errorf("etc/guild.yaml 的历练节律偏离默认值: %+v", a)
	}
	r := a.TrialResult
	if !r.Enabled || r.Topic != "match-results" || r.Partitions != 3 || r.GroupID != "guild-trial" {
		t.Errorf("etc/guild.yaml 的结果消费配置应为 Enabled / match-results / 3 / guild-trial,得到 %+v", r)
	}
	if !c.TrialResultActive() {
		t.Error("dev 配置里历练结果消费应当是开的")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 etc/guild.yaml 失败: %v", err)
	}
	if !regexp.MustCompile(`(?m)^\s*GroupID:\s*guild-trial\s*(#.*)?$`).Match(raw) {
		t.Error("etc/guild.yaml 的 GroupID 必须写成不带引号的 `GroupID: guild-trial`:带引号会被 go_services.ps1 -Zone 加上 _zN 后缀")
	}
}

func TestShouldAutoMigrateDefaults(t *testing.T) {
	var c Config
	if c.ShouldAutoMigrate() {
		t.Error("没写Schema段时应默认只读核对")
	}
	no := false
	c.Schema.AutoMigrate = &no
	if c.ShouldAutoMigrate() {
		t.Error("显式 false 时只做只读核对")
	}
}

func TestApplicationQuotaConfig(t *testing.T) {
	c := loadMinimal(t)
	if c.ApplicationQuotaPerMinute != 10 {
		t.Fatalf("省略配额配置时应默认 10，得到 %d", c.ApplicationQuotaPerMinute)
	}
	for _, limit := range []uint32{0, 1, 600, 601} {
		c.ApplicationQuotaPerMinute = limit
		err := c.Validate()
		valid := limit >= 1 && limit <= MaxApplicationQuotaPerMinute
		if valid != (err == nil) {
			t.Errorf("阈值 %d，合法=%v，校验错误=%v", limit, valid, err)
		}
	}
	var checked Config
	if err := loadEtcWithAuthorityForTest(t, &checked); err != nil {
		t.Fatal(err)
	}
	if checked.ApplicationQuotaPerMinute != 10 {
		t.Fatalf("主配置应启用 10 次配额，得到 %d", checked.ApplicationQuotaPerMinute)
	}
	for _, raw := range []string{"0", "601", "-1"} {
		var invalid Config
		if err := conf.LoadFromYamlBytes([]byte(minimalGuildYaml+"\nApplicationQuotaPerMinute: "+raw+"\n"), &invalid); err == nil {
			t.Errorf("显式配置阈值 %s 不得被默认值掩盖", raw)
		}
	}
}

// 没写 PlayerRedis.Addrs:玩家域指向 PlayerLocatorRedis 那一个实例,它没写 Host 时回落 RedisClient;写了就完全按它来。
func TestEffectivePlayerRedis(t *testing.T) {
	c := Config{
		RedisClient:        RedisConf{Host: "global:6379", Password: "global-secret"},
		PlayerLocatorRedis: RedisConf{Host: "locator:6379", Password: "locator-secret", PoolSize: 9},
	}
	got := c.EffectivePlayerRedis()
	if len(got.Addrs) != 1 || got.Addrs[0] != "locator:6379" || got.Password != "locator-secret" || got.PoolSizePerNode != 9 {
		t.Fatalf("应指向 PlayerLocatorRedis: %+v", got)
	}

	c.PlayerLocatorRedis = RedisConf{}
	got = c.EffectivePlayerRedis()
	if len(got.Addrs) != 1 || got.Addrs[0] != "global:6379" || got.Password != "global-secret" || got.PoolSizePerNode != playerRedisFallbackPoolSize {
		t.Fatalf("PlayerLocatorRedis 没写 Host 时应回落 RedisClient: %+v", got)
	}

	c.PlayerRedis = playerredis.Config{Addrs: []string{"a:7000", "b:7000"}, Cluster: true}
	got = c.EffectivePlayerRedis()
	if len(got.Addrs) != 2 || !got.Cluster || got.Password != "" {
		t.Fatalf("写了 Addrs 应完全按它来: %+v", got)
	}
}
