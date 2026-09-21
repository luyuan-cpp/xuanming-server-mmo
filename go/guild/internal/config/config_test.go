package config

import (
	"strings"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
)

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
	if err := conf.Load("../../etc/guild.yaml", &c); err != nil {
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

// ── 合服闸门配置(MergeMarkerRedis)─────────────────────────────

// TestMergeMarkerRedisAbsentMeansFenceInactive 钉住"整段缺失 = 闸门不生效"这条契约。
// 这是刻意的降级形态:guild 与 data_service 目前没有别的连线,强制它连一个新 Redis
// 会让所有还没配这段的环境直接起不来;闸门只是合服窗口里的加固。
func TestMergeMarkerRedisAbsentMeansFenceInactive(t *testing.T) {
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(minimalGuildYaml), &c); err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}
	if c.MergeMarkerRedis.Enabled() {
		t.Fatalf("没写 MergeMarkerRedis 时闸门必须是关的,got %+v", c.MergeMarkerRedis)
	}
}

// TestEtcYamlWiresMergeMarkerRedis:仓库里那份 dev 配置要把闸门接上,
// 否则本地根本演练不到合服流程。
//
// DB 必须是 0:data_service 的 mapping Redis 恒在 DB 0
// (go-zero 的 redis.RedisConf 没有 DB 字段,etc/data_service.yaml 里写 DB 会被静默忽略)。
// 填成别的库 = 闸门永远看不到 merge:in_progress:{zone},合服窗口内照样能建帮。
func TestEtcYamlWiresMergeMarkerRedis(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/guild.yaml", &c); err != nil {
		t.Fatalf("加载 etc/guild.yaml 失败: %v", err)
	}
	if !c.MergeMarkerRedis.Enabled() {
		t.Fatal("etc/guild.yaml 应当接上 MergeMarkerRedis,否则本地演练不到合服闸门")
	}
	if c.MergeMarkerRedis.DB != 0 {
		t.Errorf("MergeMarkerRedis.DB = %d;data_service 的 mapping Redis 恒在 DB 0,填别的库闸门必然失效",
			c.MergeMarkerRedis.DB)
	}
}

// minimalGuildYaml 是一份能过 go-zero 必填校验的最小配置(不含 MergeMarkerRedis)。
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

func TestEtcYamlPassesValidate(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/guild.yaml", &c); err != nil {
		t.Fatalf("加载 etc/guild.yaml 失败: %v", err)
	}
	if c.Timeout != 4000 {
		t.Errorf("Timeout = %d,期望 4000", c.Timeout)
	}
	if c.DataServiceRpc.Timeout != 2000 {
		t.Errorf("DataServiceRpc.Timeout = %d,期望 2000", c.DataServiceRpc.Timeout)
	}
	if !c.ShouldAutoMigrate() {
		t.Error("dev 档 etc/guild.yaml 应当开启 Schema.AutoMigrate")
	}
	if got := c.RequestBudget(); got != 3500*time.Millisecond {
		t.Errorf("RequestBudget = %v,期望 3500ms", got)
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
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("AssetOp.%s = %d,Y-06 默认值是 %d", ck.name, ck.got, ck.want)
		}
	}
	if a.CleanupEnabled {
		t.Error("CleanupEnabled 没写时应为 false(只有显式打开才清理)")
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
		{"预算上界", func(a *AssetOpConf) { a.OpBudgetMs, a.LeaseMs = 10000, 12000 }, ""},
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
	if err := conf.Load("../../etc/guild.yaml", &c); err != nil {
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
}

func TestShouldAutoMigrateDefaults(t *testing.T) {
	var c Config
	if !c.ShouldAutoMigrate() {
		t.Error("没写 Schema 段时应当建表(dev 默认)")
	}
	no := false
	c.Schema.AutoMigrate = &no
	if c.ShouldAutoMigrate() {
		t.Error("显式 false 时只做只读核对")
	}
}
