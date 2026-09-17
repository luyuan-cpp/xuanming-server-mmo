package config

import (
	"os"
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
	if strings.Contains(text, "\nRedis:") {
		t.Error("trade P1 不应声明共享 Redis 段(P1-10)")
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
