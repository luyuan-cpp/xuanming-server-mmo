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

// 整段缺省:落点库家族放行,连接池由 Normalize 回填默认值。
func TestPlacementBlockMissingMeansDefaults(t *testing.T) {
	var h placementHolder
	if err := conf.LoadFromYamlBytes([]byte("Name: db.rpc\n"), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !h.Placement.StoreFamiliesAllowed() {
		t.Fatal("a missing Placement block must allow store families")
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
	if err := conf.LoadFromYamlBytes([]byte("Placement:\n  ZoneStoreMaxOpenConn: 12\n"), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !h.Placement.StoreFamiliesAllowed() {
		t.Fatal("an omitted AllowStoreFamilies must stay true inside a present block")
	}
	if h.Placement.ZoneStoreMaxOpenConn != 12 {
		t.Fatalf("ZoneStoreMaxOpenConn = %d, want 12", h.Placement.ZoneStoreMaxOpenConn)
	}
}

func TestPlacementExplicitValues(t *testing.T) {
	var h placementHolder
	yaml := "Placement:\n" +
		"  AllowStoreFamilies: false\n" +
		"  ExtraStoreMaxOpenConn: 16\n"
	if err := conf.LoadFromYamlBytes([]byte(yaml), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	if h.Placement.StoreFamiliesAllowed() {
		t.Fatal("an explicit AllowStoreFamilies=false must disable store families")
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
	if !c.Placement.StoreFamiliesAllowed() {
		t.Fatal("etc/db.yaml must ship the default semantics: store families allowed")
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

// go/db 不再连 Redis、选库读全局库:样例不能留着已经没有含义的键,读的人会以为它们还起作用。
func TestEtcYamlDropsRetiredKeys(t *testing.T) {
	raw, err := os.ReadFile("../../etc/db.yaml")
	if err != nil {
		t.Fatalf("read etc/db.yaml: %v", err)
	}
	text := string(raw)
	for _, key := range []string{"RedisClient:", "Required:", "RequireRouteReady:", "LegacyNonGlobal:", "Redis:"} {
		if strings.Contains(text, key) {
			t.Fatalf("etc/db.yaml still carries the retired key %q", key)
		}
	}
}

// 配置加载器忽略不认识的键:yaml 里遗留的旧键(Kafka、RedisClient、Placement.Redis 等)不会让新二进制起不来,
// 部署模板晚一批同步也能启动。
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
	text = strings.Replace(text, anchor, anchor+"  Kafka:\n    PartitionCnt: 10\n  RedisClient:\n    Hosts: \"127.0.0.1:6379\"\n    PoolSize: 20\n", 1)
	const placementAnchor = "\nPlacement:\n"
	if strings.Count(text, placementAnchor) != 1 {
		t.Fatalf("etc/db.yaml must contain the anchor %q exactly once", placementAnchor)
	}
	text = strings.Replace(text, placementAnchor, placementAnchor+"  Required: true\n  RequireRouteReady: true\n  Redis:\n    Hosts: \"127.0.0.1:6379\"\n", 1)
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(text), &c); err != nil {
		t.Fatalf("unknown keys must be ignored by the config loader: %v", err)
	}
	if c.ServerConfig.Database.Hosts == "" || c.Placement.ZoneStoreMaxOpenConn <= 0 {
		t.Fatal("the rest of etc/db.yaml must still load")
	}
}
