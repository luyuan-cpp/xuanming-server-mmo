package config

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"player_inbox/internal/data"

	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/zrpc"
)

const etcYaml = "../../etc/player_inbox.yaml"

// TestEtcYamlContractValues 钉住 etc/player_inbox.yaml 的关键值(D14 端口、D-13 Key、D-14 库名、仅专用 Battle 默认启用)。
// 段名 / 键名写错时 go-zero 会静默保持零值,这里是编译进测试二进制的第二道网。
func TestEtcYamlContractValues(t *testing.T) {
	var c Config
	if err := conf.Load(etcYaml, &c); err != nil {
		t.Fatalf("load %s: %v", etcYaml, err)
	}
	if c.Name != "player_inbox.rpc" {
		t.Errorf("Name = %q, want player_inbox.rpc", c.Name)
	}
	if _, port, err := c.ListenHostPort(); err != nil || port != 50950 {
		t.Errorf("ListenOn = %q (port=%d err=%v), want port 50950(D14)", c.ListenOn, port, err)
	}
	if c.MetricsListenAddr != ":9270" {
		t.Errorf("MetricsListenAddr = %q, want :9270(D14)", c.MetricsListenAddr)
	}
	if c.Etcd.Key != "" {
		t.Errorf("Etcd.Key = %q, want empty(D-13)", c.Etcd.Key)
	}
	if c.Timeout < MinRpcTimeoutMs || c.Timeout > MaxRpcTimeoutMs {
		t.Errorf("Timeout = %d, want [%d, %d]", c.Timeout, MinRpcTimeoutMs, MaxRpcTimeoutMs)
	}
	if c.ZoneId != 1 {
		t.Errorf("ZoneId = %d, want 1", c.ZoneId)
	}
	if c.MySQL.DBName != data.DatabaseName {
		t.Errorf("MySQL.DBName = %q, want %q(D-14 独占库)", c.MySQL.DBName, data.DatabaseName)
	}
	if c.ShouldAutoMigrate() {
		t.Error("本地dev也必须显式迁移，常驻默认仅核对表结构")
	}
	if c.CoreRedis.Host == "" || c.CoreRedis.DB != 0 {
		t.Errorf("CoreRedis = %+v:段名必须逐字是 CoreRedis,DB 必须是 0(owner_epoch / location 所在库)", c.CoreRedis)
	}
	// 连接池上限要显式写正整数:k8s_deploy.ps1 把它镜像进 ConfigMap 并要求正整数,Redis 连接总账读同一个键。
	if c.CoreRedis.PoolSize <= 0 {
		t.Errorf("CoreRedis.PoolSize = %d, want > 0(共享 Redis 连接总账按它记账)", c.CoreRedis.PoolSize)
	}
	if len(c.Kafka.Brokers) == 0 {
		t.Error("Kafka.Brokers 为空:段名写错或整段漏写")
	}
	if c.DataServiceRpc.Etcd.Key != "dataservice.rpc" || !c.DataServiceRpc.NonBlock {
		t.Errorf("DataServiceRpc = %+v, want Key dataservice.rpc 且 NonBlock", c.DataServiceRpc.Etcd)
	}
	// 新终局默认仅开启 Match 专用 Battle 类型；GM 入箱与运维面仍停用，禁止借新版配置开放旧 GM 类型。
	if len(c.PlayerInbox.EnqueueTypes) != 1 || c.PlayerInbox.EnqueueTypes[0] != inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT.String() {
		t.Errorf("EnqueueTypes = %v:默认必须精确仅有 Match 专用 Battle 类型，GM 类型仍停用", c.PlayerInbox.EnqueueTypes)
	}
	if c.PlayerInbox.AdminToken != "" {
		t.Error("仓库里的 yaml 不得带 AdminToken(口令只经 k8s Secret 注入;本地临时写了也不要提交)")
	}
	// k8s_deploy.ps1 只在服务 yaml 声明了这个键时才注入口令(Assert-PlayerInboxAdminTokenEnvDeclared),
	// ConfigMap 写的变量名也是它:改名必须三处同改。
	if c.PlayerInbox.AdminTokenEnv != "PLAYER_INBOX_ADMIN_TOKEN" {
		t.Errorf("AdminTokenEnv = %q, want PLAYER_INBOX_ADMIN_TOKEN(与 k8s_deploy.ps1 的 PlayerInboxAdminTokenEnv 一致)",
			c.PlayerInbox.AdminTokenEnv)
	}
	// Stat 保留方法与慢调用记录；四个内部方法的玩家、结算和签名正文必须屏蔽，GM 运维接口仍保留审计线索。
	if !c.Middlewares.Stat || c.Middlewares.StatConf.SlowThreshold <= 0 {
		t.Errorf("Middlewares.Stat = %v SlowThreshold = %v:Stat 必须开着、慢调用阈值有效", c.Middlewares.Stat, c.Middlewares.StatConf.SlowThreshold)
	}
	ignored := map[string]bool{}
	for _, m := range c.Middlewares.StatConf.IgnoreContentMethods {
		ignored[m] = true
	}
	for _, m := range []string{inboxpb.PlayerInbox_FetchItems_FullMethodName, inboxpb.PlayerInbox_AckItems_FullMethodName, inboxpb.PlayerInbox_EnqueueBattleSettlement_FullMethodName, inboxpb.PlayerInbox_LookupBattleSettlement_FullMethodName} {
		if !ignored[m] {
			t.Errorf("IgnoreContentMethods 缺 %s", m)
		}
	}
	for _, m := range []string{
		inboxpb.PlayerInboxAdmin_GmSanctionPlayer_FullMethodName, inboxpb.PlayerInboxAdmin_VoidItem_FullMethodName,
		inboxpb.PlayerInboxAdmin_GetSummary_FullMethodName, inboxpb.PlayerInboxAdmin_ListItems_FullMethodName,
		inboxpb.PlayerInboxAdmin_LookupByRequest_FullMethodName, inboxpb.PlayerInboxAdmin_ReconcilePlayer_FullMethodName,
	} {
		if ignored[m] {
			t.Errorf("IgnoreContentMethods 不得含运维接口 %s:它的请求体是审计线索", m)
		}
	}
	p := c.PlayerInbox
	if p.TerminalRetentionDays != 30 || p.NotifyResendIntervalSeconds != 30 || p.ReconcileIntervalMinutes != 60 || p.ReconcileWindowHours != 24 {
		t.Errorf("PlayerInbox = %+v, want 保留 30 天 / 补发 30s / 对账 60min / 窗口 24h(D14、§6.3、§9.3)", p)
	}
}

// TestEtcYamlZoneRewriteAnchors 钉住 go_services.ps1 -Zone 改写依赖的形状(与 go/trade 同一口径)。
func TestEtcYamlZoneRewriteAnchors(t *testing.T) {
	raw, err := os.ReadFile(etcYaml)
	if err != nil {
		t.Fatalf("read %s: %v", etcYaml, err)
	}
	text := string(raw)
	if !regexp.MustCompile(`(?m)^ListenOn:\s*[^:\s]+:\d+\s*$`).MatchString(text) {
		t.Error("缺少顶层单行 `ListenOn: host:port`")
	}
	if !regexp.MustCompile(`(?m)^MetricsListenAddr:\s*"?[^:"\s]*:9270"?\s*$`).MatchString(text) {
		t.Error("缺少顶层单行 `MetricsListenAddr: \":9270\"`")
	}
	zoneLines := regexp.MustCompile(`(?m)^([ \t]*)ZoneId:[ \t]*\d+[ \t]*\r?$`).FindAllStringSubmatch(text, -1)
	if len(zoneLines) != 1 || zoneLines[0][1] != "" {
		t.Errorf("ZoneId 必须只出现一次且是顶层键(-Zone 只改写第一处 ZoneId),实际 %v", zoneLines)
	}
	if regexp.MustCompile(`(?m)^Redis:`).MatchString(text) {
		t.Error("顶层不能有 `Redis:` 段:与 zrpc.RpcServerConf 的同名字段冲突")
	}
	if regexp.MustCompile(`(?m)^\s*AdminToken:\s*"?[^"\s]+"?\s*$`).MatchString(text) {
		t.Error("仓库 yaml 不得写字面 AdminToken 的值(口令只经 AdminTokenEnv 指向的环境变量注入)")
	}
}

func validConfig() Config {
	c := Config{
		AccountRoleSchema: "testdb",
		ZoneId:            1,
		LeaseTTL:          60,
		MySQL: MySQLConf{
			Host: "127.0.0.1:3306", User: "appuser", DBName: data.DatabaseName, MaxOpenConn: 20, MaxIdleConn: 5,
		},
		CoreRedis:      RedisConf{Host: "127.0.0.1:6379", PoolSize: 20},
		Kafka:          KafkaConf{Brokers: []string{"127.0.0.1:9092"}, WriteTimeoutMs: 2000},
		DataServiceRpc: zrpc.RpcClientConf{Target: "127.0.0.1:9000"},
		PlayerInbox: PlayerInboxConf{
			TerminalRetentionDays:       30,
			SweepIntervalSeconds:        600,
			NotifyResendIntervalSeconds: 30,
			GaugeIntervalSeconds:        60,
			ReconcileIntervalMinutes:    60,
			ReconcileWindowHours:        24,
		},
	}
	c.ListenOn = "127.0.0.1:50950"
	c.Timeout = 3000
	c.Etcd.Hosts = []string{"127.0.0.1:2379"}
	return c
}

func TestValidate(t *testing.T) {
	base := validConfig()
	if err := base.Validate(); err != nil {
		t.Fatalf("基准配置应通过校验: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"ZoneId 为 0", func(c *Config) { c.ZoneId = 0 }},
		{"ListenOn 非法", func(c *Config) { c.ListenOn = "50950" }},
		{"Timeout 过小", func(c *Config) { c.Timeout = 999 }},
		{"Timeout 过大", func(c *Config) { c.Timeout = 5001 }},
		{"Etcd.Hosts 为空", func(c *Config) { c.Etcd.Hosts = nil }},
		{"Etcd.Key 非空(D-13)", func(c *Config) { c.Etcd.Key = "player_inbox.rpc" }},
		{"LeaseTTL 非正", func(c *Config) { c.LeaseTTL = 0 }},
		{"库名不是独占库", func(c *Config) { c.MySQL.DBName = "mmorpg_global" }},
		{"MySQL.Host 为空", func(c *Config) { c.MySQL.Host = "" }},
		{"连接池非正", func(c *Config) { c.MySQL.MaxOpenConn = 0 }},
		{"CoreRedis 为空", func(c *Config) { c.CoreRedis.Host = "" }},
		{"CoreRedis.PoolSize 为负", func(c *Config) { c.CoreRedis.PoolSize = -1 }},
		// 评审 L7:0 = go-redis 缺省 10 × GOMAXPROCS,随 CPU limit 漂移,Redis 连接总账就不是集群的账。
		{"CoreRedis.PoolSize 为 0(必须显式)", func(c *Config) { c.CoreRedis.PoolSize = 0 }},
		{"Kafka 为空", func(c *Config) { c.Kafka.Brokers = nil }},
		{"Kafka 超时非正", func(c *Config) { c.Kafka.WriteTimeoutMs = 0 }},
		{"DataServiceRpc 没有目标", func(c *Config) { c.DataServiceRpc = zrpc.RpcClientConf{} }},
		{"AdminToken 太短", func(c *Config) { c.PlayerInbox.AdminToken = "short" }},
		{"AdminToken 与 AdminTokenEnv 同时出现", func(c *Config) {
			c.PlayerInbox.AdminToken = strings.Repeat("k", MinAdminTokenBytes)
			c.PlayerInbox.AdminTokenEnv = "PLAYER_INBOX_ADMIN_TOKEN"
		}},
		{"AdminTokenEnv 不是变量名(疑似贴了口令)", func(c *Config) { c.PlayerInbox.AdminTokenEnv = "s3cr3t-token-value" }},
		{"EnqueueTypes 未知名", func(c *Config) { c.PlayerInbox.EnqueueTypes = []string{"PLAYER_INBOX_MSG_NOPE"} }},
		{"EnqueueTypes 含 UNSPECIFIED", func(c *Config) { c.PlayerInbox.EnqueueTypes = []string{"PLAYER_INBOX_MSG_UNSPECIFIED"} }},
		// 用户拍板 2026-10-16(交接 §6 M2 / M3):v1 不开挂欠款。
		{"EnqueueTypes 含 v1 未开放的 ATTACH_DEBT", func(c *Config) {
			c.PlayerInbox.EnqueueTypes = []string{"PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN", "PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT"}
		}},
		// 评审 L12:Stat 开着却没屏蔽 Fetch / Ack 的内容(ConfigMap 漏了这一段)= 拒启。
		{"Stat 开启但未屏蔽 Fetch / Ack 内容", func(c *Config) { c.Middlewares.Stat = true }},
		{"Stat 开启但只屏蔽了 Fetch", func(c *Config) {
			c.Middlewares.Stat = true
			c.Middlewares.StatConf.IgnoreContentMethods = []string{inboxpb.PlayerInbox_FetchItems_FullMethodName}
		}},
		{"保留期不大于签发窗", func(c *Config) { c.PlayerInbox.TerminalRetentionDays = 7 }},
		{"清理间隔越界", func(c *Config) { c.PlayerInbox.SweepIntervalSeconds = 1 }},
		{"补发间隔越界", func(c *Config) { c.PlayerInbox.NotifyResendIntervalSeconds = 0 }},
		{"gauge 间隔越界", func(c *Config) { c.PlayerInbox.GaugeIntervalSeconds = 0 }},
		{"对账间隔越界", func(c *Config) { c.PlayerInbox.ReconcileIntervalMinutes = 1 }},
		{"对账窗口越界", func(c *Config) { c.PlayerInbox.ReconcileWindowHours = 0 }},
		// 评审 L4:窗口必须覆盖至少两轮间隔(24h×60 = 1440 < 2×721)。
		{"对账窗口不足两轮间隔", func(c *Config) { c.PlayerInbox.ReconcileIntervalMinutes = 721 }},
		{"对账窗口短于一个间隔", func(c *Config) {
			c.PlayerInbox.ReconcileWindowHours, c.PlayerInbox.ReconcileIntervalMinutes = 1, 120
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("应被拒绝")
			}
		})
	}
	ok := validConfig()
	ok.PlayerInbox.ReconcileIntervalMinutes = 0
	ok.PlayerInbox.ReconcileWindowHours = 0
	if err := ok.Validate(); err != nil {
		t.Errorf("对账关闭时不校验窗口: %v", err)
	}
	ok = validConfig()
	ok.PlayerInbox.ReconcileIntervalMinutes = 720 // 边界:24h×60 = 2×720
	if err := ok.Validate(); err != nil {
		t.Errorf("窗口恰好覆盖两轮间隔应通过: %v", err)
	}
	ok = validConfig()
	ok.PlayerInbox.AdminToken = strings.Repeat("k", MinAdminTokenBytes)
	ok.PlayerInbox.EnqueueTypes = []string{"PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN", "PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN"}
	if err := ok.Validate(); err != nil {
		t.Errorf("合法口令与白名单(v1:封锁 / 解封)应通过: %v", err)
	}
	ok = validConfig()
	ok.Middlewares.Stat = true
	ok.Middlewares.StatConf.IgnoreContentMethods = []string{
		inboxpb.PlayerInbox_AckItems_FullMethodName, inboxpb.PlayerInbox_FetchItems_FullMethodName,
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("Stat 开启且已屏蔽 Fetch / Ack 内容应通过: %v", err)
	}
	ok = validConfig()
	ok.Middlewares.Stat = false
	if err := ok.Validate(); err != nil {
		t.Errorf("Stat 整体关闭时不要求屏蔽列表: %v", err)
	}
}

// TestValidateNeverEchoesAdminToken:口令太短被拒时错误文本不回显内容(AGENTS §11.3)。
func TestValidateNeverEchoesAdminToken(t *testing.T) {
	c := validConfig()
	c.PlayerInbox.AdminToken = "s3cr3t-tok"
	err := c.Validate()
	if err == nil {
		t.Fatal("短口令应被拒绝")
	}
	if strings.Contains(err.Error(), "s3cr3t-tok") {
		t.Fatalf("错误文本回显了口令: %v", err)
	}
}

func TestParseEnqueueTypes(t *testing.T) {
	got, err := ParseEnqueueTypes([]string{"PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN", "PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN"})
	if err != nil || len(got) != 2 {
		t.Fatalf("ParseEnqueueTypes = %v, %v", got, err)
	}
	if _, ok := got[inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_UNBLOCK_CURRENCY_GAIN]; !ok {
		t.Error("解析结果缺 UNBLOCK")
	}
	if empty, err := ParseEnqueueTypes(nil); err != nil || len(empty) != 0 {
		t.Errorf("空白名单 = 全部停用,不是错误: %v %v", empty, err)
	}
	if _, err := ParseEnqueueTypes([]string{"PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN", "PLAYER_INBOX_MSG_GM_BLOCK_CURRENCY_GAIN"}); err == nil {
		t.Error("重复列出应报错")
	}
	// v1 不开挂欠款(交接 §6 M2 / M3):装配层也经这个函数解析白名单,这里拒绝即两条启动路径都拒启。
	if _, err := ParseEnqueueTypes([]string{"PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT"}); err == nil ||
		!strings.Contains(err.Error(), "PLAYER_INBOX_MSG_GM_ATTACH_CURRENCY_DEBT") {
		t.Errorf("ATTACH_DEBT 在 v1 应被拒绝且点名类型: %v", err)
	}
}

func TestRequestBudgetAndAutoMigrate(t *testing.T) {
	c := validConfig()
	if got := c.RequestBudget(); got != 2500*time.Millisecond {
		t.Errorf("RequestBudget = %v, want 2.5s", got)
	}
	if c.ShouldAutoMigrate() {
		t.Error("没写Schema段默认只读核对")
	}
	no := false
	c.Schema.AutoMigrate = &no
	if c.ShouldAutoMigrate() {
		t.Error("显式 false 应只跑 plan")
	}
}

// TestResolveAdminToken 钉住口令解析:字面值优先;否则读环境变量(去首尾空白);都没有 = 停用;
// 环境变量里的口令太短 → 错误(调用方拒启),错误文本不回显口令。
func TestResolveAdminToken(t *testing.T) {
	long := strings.Repeat("t", MinAdminTokenBytes)
	const envName = "PLAYER_INBOX_ADMIN_TOKEN"
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	cases := []struct {
		name    string
		conf    PlayerInboxConf
		env     map[string]string
		want    string
		wantErr bool
	}{
		{"都没配:停用", PlayerInboxConf{}, nil, "", false},
		{"字面值", PlayerInboxConf{AdminToken: long}, nil, long, false},
		{"环境变量(去掉结尾换行)", PlayerInboxConf{AdminTokenEnv: envName}, map[string]string{envName: long + "\n"}, long, false},
		{"环境变量未设置:停用", PlayerInboxConf{AdminTokenEnv: envName}, nil, "", false},
		{"环境变量里的口令太短", PlayerInboxConf{AdminTokenEnv: envName}, map[string]string{envName: "tiny-secret"}, "", true},
	}
	for _, tc := range cases {
		got, err := tc.conf.ResolveAdminToken(env(tc.env))
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%s: ResolveAdminToken = (%q, %v), want (%q, err=%v)", tc.name, got, err, tc.want, tc.wantErr)
		}
		if err != nil && strings.Contains(err.Error(), "tiny-secret") {
			t.Errorf("%s: 错误文本回显了口令: %v", tc.name, err)
		}
	}
}
