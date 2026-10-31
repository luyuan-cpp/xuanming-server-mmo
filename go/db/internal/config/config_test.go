package config

import (
	"os"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// placementHolder 只含 Placement 一个字段,标签与 Config 上的完全相同,用来单独钉住 go-zero 对这一段的解析语义,
// 不必为了单测凑齐 Config 的其余必填段。
type placementHolder struct {
	Placement PlacementConfig `json:"Placement,optional"`
}

// 字段零值保留旧解码形状，全球Config启动另由ValidateGlobalAuthority拒绝缺失门禁；
// 不用default/Normalize把显式false静默变true，历史形状仅在显式LegacyNonGlobal中可启动。
func TestPlacementBlockMissingMeansDefaults(t *testing.T) {
	var h placementHolder
	if err := conf.LoadFromYamlBytes([]byte("Name: db.rpc\n"), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !h.Placement.StoreFamiliesAllowed() {
		t.Fatal("a missing Placement block must allow store families")
	}
	if h.Placement.Required {
		t.Fatal("a missing Placement block must not require placement records")
	}
	if h.Placement.Redis != nil {
		t.Fatal("a missing Placement block must reuse RedisClient")
	}

	c := Config{Placement: h.Placement}
	c.Normalize()
	if c.Placement.ExtraStoreMaxOpenConn != DefaultPlacementExtraStoreMaxOpenConn ||
		c.Placement.ExtraStoreMaxIdleConn != DefaultPlacementExtraStoreMaxIdleConn {
		t.Fatalf("pool defaults = %d/%d, want %d/%d", c.Placement.ExtraStoreMaxOpenConn, c.Placement.ExtraStoreMaxIdleConn,
			DefaultPlacementExtraStoreMaxOpenConn, DefaultPlacementExtraStoreMaxIdleConn)
	}
}

// 写了段但没写 AllowStoreFamilies:同样按 true(*bool 的 nil),不能因为「写了半段」换一套默认值。
func TestPlacementPartialBlockKeepsFamilyDefault(t *testing.T) {
	var h placementHolder
	if err := conf.LoadFromYamlBytes([]byte("Placement:\n  Required: true\n"), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !h.Placement.StoreFamiliesAllowed() {
		t.Fatal("an omitted AllowStoreFamilies must stay true inside a present block")
	}
	if !h.Placement.Required {
		t.Fatal("Required=true must be honored")
	}
}

func TestPlacementExplicitValues(t *testing.T) {
	var h placementHolder
	yaml := "Placement:\n" +
		"  AllowStoreFamilies: false\n" +
		"  ExtraStoreMaxOpenConn: 16\n" +
		"  Redis:\n" +
		"    Hosts: \"10.0.0.1:6379\"\n" +
		"    DB: 3\n"
	if err := conf.LoadFromYamlBytes([]byte(yaml), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	if h.Placement.StoreFamiliesAllowed() {
		t.Fatal("an explicit AllowStoreFamilies=false must disable store families")
	}
	if h.Placement.Redis == nil || h.Placement.Redis.Hosts != "10.0.0.1:6379" || h.Placement.Redis.DB != 3 {
		t.Fatalf("Placement.Redis = %+v", h.Placement.Redis)
	}

	c := Config{Placement: h.Placement}
	c.Normalize()
	if c.Placement.ExtraStoreMaxOpenConn != 16 {
		t.Fatalf("an explicit pool size must survive Normalize, got %d", c.Placement.ExtraStoreMaxOpenConn)
	}
	if c.Placement.ExtraStoreMaxIdleConn != DefaultPlacementExtraStoreMaxIdleConn {
		t.Fatalf("an omitted idle size falls back to the default, got %d", c.Placement.ExtraStoreMaxIdleConn)
	}
}

// Placement.Redis 段出现就必须带 Hosts:空的 Redis 段静默落到默认地址,读到的就不是 mapping Redis。
func TestPlacementRedisBlockRequiresHosts(t *testing.T) {
	var h placementHolder
	if err := conf.LoadFromYamlBytes([]byte("Placement:\n  Redis:\n    DB: 0\n"), &h); err == nil {
		t.Fatal("a Placement.Redis block without Hosts must be rejected")
	}
}

// etc/db.yaml必须能解析且显式写出全球默认，不能靠Normalize掩盖漏键。
func TestEtcYamlPlacementBlockMatchesDefaults(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/db.yaml", &c); err != nil {
		t.Fatalf("load etc/db.yaml: %v", err)
	}
	// Normalize 之前:示例 yaml 必须把两类池的四个新键与空闲秒数都写出来(键名写错会静默保持零值,
	// Normalize 之后又被回落值盖住,只看 Normalize 之后的值发现不了)。K8s ConfigMap 从这几个键镜像,总账也读它们。
	raw := c.Placement
	if raw.ZoneStoreMaxOpenConn <= 0 || raw.ZoneStoreMaxIdleConn <= 0 ||
		raw.NonZoneStoreMaxOpenConn <= 0 || raw.NonZoneStoreMaxIdleConn <= 0 || raw.ExtraStoreConnMaxIdleSeconds <= 0 {
		t.Fatalf("etc/db.yaml must spell out the split pool keys, got zone=%d/%d nonZone=%d/%d idleSeconds=%d",
			raw.ZoneStoreMaxOpenConn, raw.ZoneStoreMaxIdleConn, raw.NonZoneStoreMaxOpenConn, raw.NonZoneStoreMaxIdleConn,
			raw.ExtraStoreConnMaxIdleSeconds)
	}
	c.Normalize()
	if c.Placement.AllowStoreFamilies == nil {
		t.Fatal("etc/db.yaml must spell out AllowStoreFamilies so the example documents the key")
	}
	if !c.Placement.StoreFamiliesAllowed() || !c.Placement.Required {
		t.Fatalf("etc/db.yaml must ship the default semantics, got families=%v required=%v",
			c.Placement.StoreFamiliesAllowed(), c.Placement.Required)
	}
	if c.Placement.ZoneStoreMaxOpenConn != DefaultPlacementExtraStoreMaxOpenConn ||
		c.Placement.ZoneStoreMaxIdleConn != DefaultPlacementExtraStoreMaxIdleConn ||
		c.Placement.NonZoneStoreMaxOpenConn != DefaultPlacementExtraStoreMaxOpenConn ||
		c.Placement.NonZoneStoreMaxIdleConn != DefaultPlacementExtraStoreMaxIdleConn {
		t.Fatalf("etc/db.yaml pools = zone %d/%d nonZone %d/%d, want both %d/%d",
			c.Placement.ZoneStoreMaxOpenConn, c.Placement.ZoneStoreMaxIdleConn,
			c.Placement.NonZoneStoreMaxOpenConn, c.Placement.NonZoneStoreMaxIdleConn,
			DefaultPlacementExtraStoreMaxOpenConn, DefaultPlacementExtraStoreMaxIdleConn)
	}
	if c.Placement.ExtraStoreConnMaxIdleSeconds != DefaultPlacementExtraStoreConnMaxIdleSeconds {
		t.Fatalf("etc/db.yaml ExtraStoreConnMaxIdleSeconds = %d, want %d",
			c.Placement.ExtraStoreConnMaxIdleSeconds, DefaultPlacementExtraStoreConnMaxIdleSeconds)
	}
	if c.Placement.Redis != nil {
		t.Fatal("etc/db.yaml must keep Placement.Redis commented out (reuse RedisClient) for local dev")
	}
}

// 两类额外落点库池(G2b 批 C)整段缺省时:都取默认 8 / 2,空闲回收 60s。
func TestPlacementSplitPoolsDefault(t *testing.T) {
	var h placementHolder
	if err := conf.LoadFromYamlBytes([]byte("Name: db.rpc\n"), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	c := Config{Placement: h.Placement}
	c.Normalize()
	p := c.Placement
	if p.ZoneStoreMaxOpenConn != DefaultPlacementExtraStoreMaxOpenConn || p.ZoneStoreMaxIdleConn != DefaultPlacementExtraStoreMaxIdleConn {
		t.Fatalf("zone store pool = %d/%d, want %d/%d", p.ZoneStoreMaxOpenConn, p.ZoneStoreMaxIdleConn,
			DefaultPlacementExtraStoreMaxOpenConn, DefaultPlacementExtraStoreMaxIdleConn)
	}
	if p.NonZoneStoreMaxOpenConn != DefaultPlacementExtraStoreMaxOpenConn || p.NonZoneStoreMaxIdleConn != DefaultPlacementExtraStoreMaxIdleConn {
		t.Fatalf("non-zone store pool = %d/%d, want %d/%d", p.NonZoneStoreMaxOpenConn, p.NonZoneStoreMaxIdleConn,
			DefaultPlacementExtraStoreMaxOpenConn, DefaultPlacementExtraStoreMaxIdleConn)
	}
	if p.ExtraStoreConnMaxIdleSeconds != DefaultPlacementExtraStoreConnMaxIdleSeconds {
		t.Fatalf("idle seconds = %d, want %d", p.ExtraStoreConnMaxIdleSeconds, DefaultPlacementExtraStoreConnMaxIdleSeconds)
	}

	// 可重复调用(Normalize 的契约):第二次不得改变任何值。
	before := c.Placement
	c.Normalize()
	if c.Placement != before {
		t.Fatalf("Normalize must be idempotent, Placement changed from %+v to %+v", before, c.Placement)
	}
}

// 只写旧键的旧部署:两类库都回落到旧键的值,维持原来的池大小。
func TestPlacementSplitPoolsFallBackToLegacyKeys(t *testing.T) {
	var h placementHolder
	yaml := "Placement:\n" +
		"  ExtraStoreMaxOpenConn: 16\n" +
		"  ExtraStoreMaxIdleConn: 4\n"
	if err := conf.LoadFromYamlBytes([]byte(yaml), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	c := Config{Placement: h.Placement}
	c.Normalize()
	p := c.Placement
	if p.ZoneStoreMaxOpenConn != 16 || p.ZoneStoreMaxIdleConn != 4 {
		t.Fatalf("zone store pool = %d/%d, want legacy 16/4", p.ZoneStoreMaxOpenConn, p.ZoneStoreMaxIdleConn)
	}
	if p.NonZoneStoreMaxOpenConn != 16 || p.NonZoneStoreMaxIdleConn != 4 {
		t.Fatalf("non-zone store pool = %d/%d, want legacy 16/4", p.NonZoneStoreMaxOpenConn, p.NonZoneStoreMaxIdleConn)
	}
}

// 新键优先于旧键:只调大非 zone 池时,zone 家族池仍按旧键。
func TestPlacementSplitPoolsNewKeysWin(t *testing.T) {
	var h placementHolder
	yaml := "Placement:\n" +
		"  ExtraStoreMaxOpenConn: 16\n" +
		"  NonZoneStoreMaxOpenConn: 32\n" +
		"  ExtraStoreConnMaxIdleSeconds: 30\n"
	if err := conf.LoadFromYamlBytes([]byte(yaml), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	c := Config{Placement: h.Placement}
	c.Normalize()
	p := c.Placement
	if p.ZoneStoreMaxOpenConn != 16 {
		t.Fatalf("zone store max open = %d, want the legacy fallback 16", p.ZoneStoreMaxOpenConn)
	}
	if p.NonZoneStoreMaxOpenConn != 32 {
		t.Fatalf("non-zone store max open = %d, want the explicit 32", p.NonZoneStoreMaxOpenConn)
	}
	if p.ZoneStoreMaxIdleConn != DefaultPlacementExtraStoreMaxIdleConn || p.NonZoneStoreMaxIdleConn != DefaultPlacementExtraStoreMaxIdleConn {
		t.Fatalf("idle pools = %d/%d, want the default %d", p.ZoneStoreMaxIdleConn, p.NonZoneStoreMaxIdleConn,
			DefaultPlacementExtraStoreMaxIdleConn)
	}
	if p.ExtraStoreConnMaxIdleSeconds != 30 {
		t.Fatalf("idle seconds = %d, want the explicit 30", p.ExtraStoreConnMaxIdleSeconds)
	}

	// 新旧键混写的起点上同样可重复调用。
	before := c.Placement
	c.Normalize()
	if c.Placement != before {
		t.Fatal("Normalize must be idempotent when legacy and new keys are mixed")
	}
}

// etc/db.yaml必须显式写ready门禁开；键名写错不能静默退回旧MGET。
func TestEtcYamlPlacementRouteReadyKeys(t *testing.T) {
	raw, err := os.ReadFile("../../etc/db.yaml")
	if err != nil {
		t.Fatalf("read etc/db.yaml: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.Contains(text, "\n  RequireRouteReady: true\n") {
		t.Fatal("全球etc/db.yaml必须显式Placement.RequireRouteReady: true")
	}
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(text), &c); err != nil {
		t.Fatalf("load etc/db.yaml: %v", err)
	}
	if !c.Placement.RequireRouteReady {
		t.Fatal("全球etc/db.yaml必须保持Placement.RequireRouteReady=true")
	}
}

// 整段缺省或只写了别的键:RequireRouteReady 保持 false,不能被 Normalize 悄悄打开或关闭。
func TestPlacementRouteReadyOmittedStaysFalse(t *testing.T) {
	for _, yaml := range []string{"Name: db.rpc\n", "Placement:\n  Required: true\n"} {
		var h placementHolder
		if err := conf.LoadFromYamlBytes([]byte(yaml), &h); err != nil {
			t.Fatalf("load %q: %v", yaml, err)
		}
		c := Config{Placement: h.Placement}
		c.Normalize()
		if c.Placement.RequireRouteReady {
			t.Fatalf("%q: an omitted RequireRouteReady must stay false", yaml)
		}
	}
}

// 配置加载器忽略不认识的键:yaml 里遗留的旧键(Kafka、Fencing 等)不会让新二进制起不来。
func TestConfigLoaderIgnoresUnknownKeys(t *testing.T) {
	raw, err := os.ReadFile("../../etc/db.yaml")
	if err != nil {
		t.Fatalf("read etc/db.yaml: %v", err)
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	const anchor = "\nServerConfig:\n"
	if strings.Count(text, anchor) != 1 {
		t.Fatalf("etc/db.yaml must contain the anchor %q exactly once", anchor)
	}
	text = strings.Replace(text, anchor, anchor+"  Kafka:\n    PartitionCnt: 10\n", 1)
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(text), &c); err != nil {
		t.Fatalf("unknown keys must be ignored by the config loader: %v", err)
	}
	if c.ServerConfig.RedisClient.Hosts == "" {
		t.Fatal("the rest of etc/db.yaml must still load")
	}
}
