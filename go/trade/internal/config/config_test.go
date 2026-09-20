package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"trade/internal/data"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"
)

const etcYaml = "../../etc/trade.yaml"

// TestEtcYamlContractValues 钉住 etc/trade.yaml 的关键值。段名 / 键名写错时 go-zero 会静默保持零值
// (例如 IdSegment 拼错 → 号段悄悄关掉),这里是编译进测试二进制的第二道网。
func TestEtcYamlContractValues(t *testing.T) {
	var c Config
	if err := conf.Load(etcYaml, &c); err != nil {
		t.Fatalf("load %s: %v", etcYaml, err)
	}
	if c.Name != "trade.rpc" {
		t.Errorf("Name = %q, want trade.rpc", c.Name)
	}
	if _, port, err := c.ListenHostPort(); err != nil || port != 50800 {
		t.Errorf("ListenOn = %q (port=%d err=%v), want port 50800", c.ListenOn, port, err)
	}
	if c.Etcd.Key != "" {
		t.Errorf("Etcd.Key = %q, want empty (D-13)", c.Etcd.Key)
	}
	if c.Timeout < MinRpcTimeoutMs || c.Timeout > MaxRpcTimeoutMs {
		t.Errorf("Timeout = %d, want [%d, %d]", c.Timeout, MinRpcTimeoutMs, MaxRpcTimeoutMs)
	}
	if c.MetricsListenAddr != ":9230" {
		t.Errorf("MetricsListenAddr = %q, want :9230", c.MetricsListenAddr)
	}
	if c.Mode != service.DevMode {
		t.Errorf("Mode = %q, want dev (go-zero 默认 pro 会让 SeedListing 在本地不可用)", c.Mode)
	}
	if c.ZoneId != 1 {
		t.Errorf("ZoneId = %d, want 1", c.ZoneId)
	}
	if c.DataServiceRpc.Etcd.Key != "dataservice.rpc" {
		t.Errorf("DataServiceRpc.Etcd.Key = %q, want dataservice.rpc", c.DataServiceRpc.Etcd.Key)
	}
	if !c.DataServiceRpc.NonBlock {
		t.Error("DataServiceRpc.NonBlock must be true: data_service 暂未起时不阻塞 trade 起服")
	}
	if !c.IdSegment.Enabled || c.IdSegment.FallbackToSnowflake {
		t.Errorf("IdSegment = %+v, want Enabled=true FallbackToSnowflake=false", c.IdSegment)
	}
	if c.Market.Scope != ScopeZone {
		t.Errorf("Market.Scope = %q, want zone", c.Market.Scope)
	}
	if c.Market.MaxPageSize != 20 || c.Market.DefaultPageSize != 20 || c.Market.MaxPage != 100 ||
		c.Market.MaxFavoritesPerPlayer != 100 {
		t.Errorf("Market = %+v, want 20/20/100/100", c.Market)
	}
	if c.MySQL.DBName != data.DatabaseName {
		t.Errorf("MySQL.DBName = %q, want %q", c.MySQL.DBName, data.DatabaseName)
	}
	if !c.ShouldAutoMigrate() {
		t.Error("本地 dev 应启动期自动建表(Schema.AutoMigrate: true)")
	}
	// 位置键 player:{id}:location 在共享 Redis 的 DB 0(写者是 C++ scene / scene_manager)。
	// 段名写错会被 go-zero 当未知键忽略 → Host 空 → Validate 拒绝起服,这里先在测试里点名。
	if c.SharedRedis.Host == "" {
		t.Error("SharedRedis.Host 为空:段名写错(必须逐字是 SharedRedis)或整段漏写")
	}
	if c.SharedRedis.DB != 0 {
		t.Errorf("SharedRedis.DB = %d, want 0(位置键所在库)", c.SharedRedis.DB)
	}
	// 资产通道默认关闭,本地这份也不例外 —— 这份 yaml 是最容易被整份抄去别的环境的一份。
	// 要在本机验托管时才临时改 true,并先让 start_game.ps1 注入两把开发密钥;
	// 改完记得改回来,否则这条断言会红(这就是它存在的目的:让"开着"必须是一次自觉的决定)。
	if c.AssetOp.Enabled {
		t.Error("AssetOp.Enabled 必须是 false:资产通道默认关闭,本地 yaml 被整份抄走也带不走一个打开的开关")
	}
	if c.AssetOp.SecretEnv != AssetOpSecretEnvPrefix+"TRADE" {
		t.Errorf("AssetOp.SecretEnv = %q, want %sTRADE(段名 / 键名写错会被 go-zero 当未知键忽略)",
			c.AssetOp.SecretEnv, AssetOpSecretEnvPrefix)
	}
	if c.AssetOp.Secret != "" {
		t.Error("AssetOp.Secret 必须留空:密钥只从环境变量读,绝不进仓库")
	}
}

// TestEtcYamlHasNoAssetOpSecretValue 机械守住"密钥不进仓库":yaml 里只许出现**变量名**,
// 不许出现 `Secret:` 这个键。Validate 会在运行期拒启,这里让它在测试期就红 ——
// 密钥一旦提交进 git 历史,删掉一行是补不回来的。
func TestEtcYamlHasNoAssetOpSecretValue(t *testing.T) {
	raw, err := os.ReadFile(etcYaml)
	if err != nil {
		t.Fatalf("read %s: %v", etcYaml, err)
	}
	if regexp.MustCompile(`(?m)^\s*Secret:`).MatchString(string(raw)) {
		t.Error("yaml 里出现了 `Secret:` 键:资产通道密钥只能由部署侧经环境变量注入(§4.32)")
	}
	if !regexp.MustCompile(`(?m)^AssetOp:`).MatchString(string(raw)) {
		t.Error("缺少顶层 `AssetOp:` 段:三处键名(yaml / config.go / k8s ConfigMap)必须逐字一致")
	}
}

// TestAssetOpSectionAbsentMeansDisabled 钉住"整段缺失 = 关闭"。
//
// 这是整个默认关闭机制的地基:go-zero 不会下钻一个整段 optional 且未出现的嵌套结构,
// 所以 AssetOp 段没写时 Enabled 就是零值 false。SchemaConf.AutoMigrate 正是被这条坑过
// (才改用 *bool),这里反过来依赖它 —— 依赖就要有测试钉住,否则哪天换了加载器,
// "没配置"会静默变成"开着"。
func TestAssetOpSectionAbsentMeansDisabled(t *testing.T) {
	raw, err := os.ReadFile(etcYaml)
	if err != nil {
		t.Fatalf("read %s: %v", etcYaml, err)
	}
	// 去掉 `AssetOp:` 与它下面的缩进行;段上方的注释是顶格 `#`,留着无害。
	stripped := regexp.MustCompile(`(?m)^AssetOp:\r?\n(?:[ \t]+.*\r?\n?)*`).ReplaceAllString(string(raw), "")
	if strings.Contains(stripped, "\nAssetOp:") || strings.HasPrefix(stripped, "AssetOp:") {
		t.Fatal("AssetOp 段没被去干净,本用例失去意义")
	}

	path := filepath.Join(t.TempDir(), "trade.yaml")
	if err := os.WriteFile(path, []byte(stripped), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	var c Config
	if err := conf.Load(path, &c); err != nil {
		t.Fatalf("缺 AssetOp 段的配置必须能加载(整段可缺失): %v", err)
	}
	if c.AssetOp.Enabled {
		t.Error("没写 AssetOp 段时 Enabled 必须是 false")
	}
}

// TestEtcYamlZoneRewriteAnchors 钉住 go_services.ps1 -Zone 改写依赖的 yaml 形状(契约 §7):
// 顶层单行 ListenOn / MetricsListenAddr,且文件里第一处(也是唯一一处)ZoneId 是顶层键。
// 形状一变,派生 yaml 就静默不位移端口 / 不改 zone。
func TestEtcYamlZoneRewriteAnchors(t *testing.T) {
	raw, err := os.ReadFile(etcYaml)
	if err != nil {
		t.Fatalf("read %s: %v", etcYaml, err)
	}
	text := string(raw)

	if !regexp.MustCompile(`(?m)^ListenOn:\s*[^:\s]+:\d+\s*$`).MatchString(text) {
		t.Error("缺少顶层单行 `ListenOn: host:port`")
	}
	if !regexp.MustCompile(`(?m)^MetricsListenAddr:\s*"?[^:"\s]*:9230"?\s*$`).MatchString(text) {
		t.Error("缺少顶层单行 `MetricsListenAddr: \":9230\"`")
	}
	// 缩进组只收空格 / 制表符:写成 \s* 会在多行模式下跨过前一个空行吞进换行符,把顶层键误判成嵌套键。
	zoneLines := regexp.MustCompile(`(?m)^([ \t]*)ZoneId:[ \t]*\d+[ \t]*\r?$`).FindAllStringSubmatch(text, -1)
	if len(zoneLines) != 1 {
		t.Fatalf("ZoneId 应只出现一次,实际 %d 次", len(zoneLines))
	}
	if zoneLines[0][1] != "" {
		t.Error("唯一的 ZoneId 必须是顶层键(-Zone 只改写第一处 ZoneId)")
	}
	// 顶层不许出现名为 Redis 的段:zrpc.RpcServerConf 已经有同名字段,conf.MustLoad 会直接报
	// "conflict key Redis" 起不来。共享 Redis 一律写成 SharedRedis(与 config.Config 同名)。
	if regexp.MustCompile(`(?m)^Redis:`).MatchString(text) {
		t.Error("顶层不能有 `Redis:` 段:与 zrpc.RpcServerConf 的同名字段冲突,加载期报 conflict key Redis")
	}
	if !regexp.MustCompile(`(?m)^SharedRedis:`).MatchString(text) {
		t.Error("缺少顶层 `SharedRedis:` 段:资产通道按 player:{id}:location 定位玩家所在 scene")
	}
}

// validConfig 是一份能通过 Validate 的最小配置,负向用例在它上面逐项改坏。
func validConfig() Config {
	c := Config{
		ZoneId:   1,
		LeaseTTL: 60,
		MySQL: MySQLConf{
			Host: "127.0.0.1:3306", User: "appuser", DBName: data.DatabaseName,
			MaxOpenConn: 20, MaxIdleConn: 5,
		},
		DataServiceRpc: zrpc.RpcClientConf{Target: "127.0.0.1:9000"},
		Market: MarketConf{
			Scope: ScopeZone, DefaultPageSize: 20, MaxPageSize: 20, MaxPage: 100, MaxFavoritesPerPlayer: 100,
		},
		// 共享 Redis 必填:资产通道靠 player:{id}:location 找玩家所在 scene。
		SharedRedis: RedisConf{Host: "127.0.0.1:6379"},
	}
	c.ListenOn = "127.0.0.1:50800"
	c.Timeout = 4000
	c.Etcd.Hosts = []string{"127.0.0.1:2379"}
	c.IdSegment.Enabled = true
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
		{"ZoneId=0", func(c *Config) { c.ZoneId = 0 }},
		{"ListenOn 为空", func(c *Config) { c.ListenOn = "" }},
		{"ListenOn 端口非法", func(c *Config) { c.ListenOn = "127.0.0.1:0" }},
		{"Timeout=0", func(c *Config) { c.Timeout = 0 }},
		{"Timeout<1000", func(c *Config) { c.Timeout = 999 }},
		{"Timeout>4000", func(c *Config) { c.Timeout = 4001 }},
		{"Etcd.Hosts 为空", func(c *Config) { c.Etcd.Hosts = nil }},
		{"Etcd.Key 非空", func(c *Config) { c.Etcd.Key = "trade.rpc" }},
		{"LeaseTTL=0", func(c *Config) { c.LeaseTTL = 0 }},
		{"MySQL.Host 为空", func(c *Config) { c.MySQL.Host = "" }},
		{"MySQL.User 为空", func(c *Config) { c.MySQL.User = "" }},
		{"MySQL.DBName 为空", func(c *Config) { c.MySQL.DBName = "" }},
		{"MySQL.DBName 指向别的库", func(c *Config) { c.MySQL.DBName = "mmorpg" }},
		{"MySQL.MaxOpenConn=0", func(c *Config) { c.MySQL.MaxOpenConn = 0 }},
		{"DataServiceRpc 无目标", func(c *Config) { c.DataServiceRpc = zrpc.RpcClientConf{} }},
		{"IdSegment 未启用", func(c *Config) { c.IdSegment.Enabled = false }},
		{"IdSegment 开了 snowflake 回退", func(c *Config) { c.IdSegment.FallbackToSnowflake = true }},
		{"Market.Scope 为空", func(c *Config) { c.Market.Scope = "" }},
		{"Market.Scope 未知", func(c *Config) { c.Market.Scope = "region" }},
		{"DefaultPageSize=0", func(c *Config) { c.Market.DefaultPageSize = 0 }},
		{"MaxPageSize=0", func(c *Config) { c.Market.MaxPageSize = 0 }},
		{"MaxPage=0", func(c *Config) { c.Market.MaxPage = 0 }},
		{"MaxFavoritesPerPlayer=0", func(c *Config) { c.Market.MaxFavoritesPerPlayer = 0 }},
		{"DefaultPageSize>MaxPageSize", func(c *Config) { c.Market.DefaultPageSize = 21 }},
		{"SharedRedis.Host 为空", func(c *Config) { c.SharedRedis.Host = "" }},
		// 密钥写进配置文件 = 泄漏,不看开关一律拒。
		{"AssetOp.Secret 非空(通道关着也拒)", func(c *Config) { c.AssetOp.Secret = "any-value-at-all" }},
		{"AssetOp.Secret 非空(通道开着)", func(c *Config) {
			c.AssetOp = AssetOpConf{Enabled: true, SecretEnv: AssetOpSecretEnvPrefix + "TRADE", Secret: "x"}
		}},
		// 开着却说不清密钥从哪来:scene 会一律回 27008,必须在启动期就点名。
		{"AssetOp 开着但 SecretEnv 为空", func(c *Config) { c.AssetOp.Enabled = true }},
		{"AssetOp 开着但 SecretEnv 不是资产密钥变量", func(c *Config) {
			c.AssetOp = AssetOpConf{Enabled: true, SecretEnv: "MMORPG_MYSQL_PASSWORD"}
		}},
		// 手滑把密钥值贴进 SecretEnv:形状过不了(真密钥含小写 / 连字符)。
		{"AssetOp 的 SecretEnv 被填成了密钥值", func(c *Config) {
			c.AssetOp = AssetOpConf{Enabled: true, SecretEnv: "some-lowercase-value-with-dashes-0000"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("应被 Validate 拒绝")
			}
		})
	}
}

// TestAssetOpEnabledAcceptsSecretEnv:开关打开 + 合法的密钥变量名 = 通过校验。
// 校验只管"来源说得清",密钥到底存不存在由装配层(svc.NewServiceContext)在启动期判:
// 配置对象读不到环境变量,把两件事混在一处会让单测被跑测机器的环境左右。
func TestAssetOpEnabledAcceptsSecretEnv(t *testing.T) {
	c := validConfig()
	c.AssetOp = AssetOpConf{Enabled: true, SecretEnv: AssetOpSecretEnvPrefix + "TRADE"}
	if err := c.Validate(); err != nil {
		t.Fatalf("开关打开 + 合法变量名应通过校验: %v", err)
	}
}

// TestValidateNeverEchoesAssetOpSecret:配置里误贴的密钥不得出现在错误文本里。
// 错误会被原样打进启动日志,回显等于把密钥从配置文件搬进日志(AGENTS §11.3)。
func TestValidateNeverEchoesAssetOpSecret(t *testing.T) {
	const pasted = "pasted-secret-must-not-appear-in-any-error-text"
	c := validConfig()
	c.AssetOp = AssetOpConf{Secret: pasted}
	err := c.Validate()
	if err == nil {
		t.Fatal("AssetOp.Secret 非空必须被拒")
	}
	if strings.Contains(err.Error(), pasted) {
		t.Error("错误文本里出现了密钥值")
	}
}

// TestRequestBudget 钉住整请求业务预算:严格小于服务端 Timeout(给 in-band 回包留余量),
// 且 Validate 放行的最小 Timeout 下仍为正。
func TestRequestBudget(t *testing.T) {
	c := validConfig()
	if got, want := c.RequestBudget(), 3500*time.Millisecond; got != want {
		t.Errorf("Timeout=4000 时 RequestBudget = %v, want %v", got, want)
	}

	c.Timeout = MinRpcTimeoutMs
	if err := c.Validate(); err != nil {
		t.Fatalf("Timeout=%d 应通过校验: %v", MinRpcTimeoutMs, err)
	}
	if got := c.RequestBudget(); got <= 0 || got >= time.Duration(c.Timeout)*time.Millisecond {
		t.Errorf("Timeout=%d 时 RequestBudget = %v, want (0, Timeout)", c.Timeout, got)
	}
}

func TestShouldAutoMigrate(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		v    *bool
		want bool
	}{
		{"没写 = 建表", nil, true},
		{"显式 true", &yes, true},
		{"显式 false", &no, false},
	}
	for _, tc := range cases {
		c := Config{Schema: SchemaConf{AutoMigrate: tc.v}}
		if got := c.ShouldAutoMigrate(); got != tc.want {
			t.Errorf("%s: ShouldAutoMigrate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsRelaxedMode(t *testing.T) {
	for mode, want := range map[string]bool{
		service.DevMode:  true,
		service.TestMode: true,
		service.RtMode:   false,
		service.PreMode:  false,
		service.ProMode:  false,
		"":               false,
	} {
		if got := IsRelaxedMode(mode); got != want {
			t.Errorf("IsRelaxedMode(%q) = %v, want %v", mode, got, want)
		}
	}
}

func TestMarketScopeEnum(t *testing.T) {
	if (MarketConf{Scope: ScopeZone}).ScopeEnum().String() != "MARKET_SCOPE_ZONE" {
		t.Error("zone 应映射为 MARKET_SCOPE_ZONE")
	}
	if (MarketConf{Scope: ScopeGlobal}).ScopeEnum().String() != "MARKET_SCOPE_GLOBAL" {
		t.Error("global 应映射为 MARKET_SCOPE_GLOBAL")
	}
}
