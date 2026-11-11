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
	if c.Placement.StoreMaxOpenConn != DefaultPlacementStoreMaxOpenConn ||
		c.Placement.StoreMaxIdleConn != DefaultPlacementStoreMaxIdleConn ||
		c.Placement.StoreConnMaxIdleSeconds != DefaultPlacementStoreConnMaxIdleSeconds {
		t.Fatalf("pool defaults = %d/%d/%ds, want %d/%d/%ds", c.Placement.StoreMaxOpenConn, c.Placement.StoreMaxIdleConn,
			c.Placement.StoreConnMaxIdleSeconds, DefaultPlacementStoreMaxOpenConn, DefaultPlacementStoreMaxIdleConn,
			DefaultPlacementStoreConnMaxIdleSeconds)
	}

	// 可重复调用(Normalize 的契约):第二次不得改变任何值。
	before := c.Placement
	c.Normalize()
	if c.Placement != before {
		t.Fatalf("Normalize must be idempotent, Placement changed from %+v to %+v", before, c.Placement)
	}
}

// 写了段但没写 AllowStoreFamilies:同样按 true(*bool 的 nil),不能因为「写了半段」换一套默认值。
func TestPlacementPartialBlockKeepsFamilyDefault(t *testing.T) {
	var h placementHolder
	if err := conf.LoadFromYamlBytes([]byte("Placement:\n  StoreMaxOpenConn: 12\n"), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !h.Placement.StoreFamiliesAllowed() {
		t.Fatal("an omitted AllowStoreFamilies must stay true inside a present block")
	}
	if h.Placement.StoreMaxOpenConn != 12 {
		t.Fatalf("StoreMaxOpenConn = %d, want 12", h.Placement.StoreMaxOpenConn)
	}
}

func TestPlacementExplicitValues(t *testing.T) {
	var h placementHolder
	yaml := "Placement:\n" +
		"  AllowStoreFamilies: false\n" +
		"  StoreMaxOpenConn: 16\n" +
		"  StoreConnMaxIdleSeconds: 30\n"
	if err := conf.LoadFromYamlBytes([]byte(yaml), &h); err != nil {
		t.Fatalf("load: %v", err)
	}
	if h.Placement.StoreFamiliesAllowed() {
		t.Fatal("an explicit AllowStoreFamilies=false must disable store families")
	}

	c := Config{Placement: h.Placement}
	c.Normalize()
	if c.Placement.StoreMaxOpenConn != 16 || c.Placement.StoreConnMaxIdleSeconds != 30 {
		t.Fatalf("explicit pool values must survive Normalize, got %d / %ds", c.Placement.StoreMaxOpenConn, c.Placement.StoreConnMaxIdleSeconds)
	}
	if c.Placement.StoreMaxIdleConn != DefaultPlacementStoreMaxIdleConn {
		t.Fatalf("an omitted idle size falls back to the default, got %d", c.Placement.StoreMaxIdleConn)
	}
}

// etc/db.yaml 必须能解析且显式写出存档库池的三个键,不能靠 Normalize 掩盖漏键。
func TestEtcYamlPlacementBlockSpellsOutStorePool(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/db.yaml", &c); err != nil {
		t.Fatalf("load etc/db.yaml: %v", err)
	}
	// Normalize 之前:键名写错会静默保持零值,Normalize 之后又被默认值盖住,只看 Normalize 之后的值发现不了。
	// K8s ConfigMap 从这几个键镜像,总账也读它们。
	raw := c.Placement
	if raw.StoreMaxOpenConn <= 0 || raw.StoreMaxIdleConn <= 0 || raw.StoreConnMaxIdleSeconds <= 0 {
		t.Fatalf("etc/db.yaml must spell out the store pool keys, got %d/%d idleSeconds=%d",
			raw.StoreMaxOpenConn, raw.StoreMaxIdleConn, raw.StoreConnMaxIdleSeconds)
	}
	c.Normalize()
	if c.Placement.AllowStoreFamilies == nil {
		t.Fatal("etc/db.yaml must spell out AllowStoreFamilies so the example documents the key")
	}
	if !c.Placement.StoreFamiliesAllowed() {
		t.Fatal("etc/db.yaml must ship the default semantics: store families allowed")
	}
	// 存档库池写的是「本地域只有一个库」时的预算(docs/design/player-data-tidb-scaling-plan.md §7.2):go/db 没有本区库,
	// 全部存盘都走它,所以是 60 / 20,不是默认的 8 / 2。K8s 的 ConfigMap 按存档库数摊到每个库(k8s_deploy.ps1 Get-PlayerStorePoolLimit)。
	const wantOpen, wantIdle = 60, 20
	if c.Placement.StoreMaxOpenConn != wantOpen || c.Placement.StoreMaxIdleConn != wantIdle {
		t.Fatalf("etc/db.yaml store pool = %d/%d, want the single-store budget %d/%d",
			c.Placement.StoreMaxOpenConn, c.Placement.StoreMaxIdleConn, wantOpen, wantIdle)
	}
	if c.Placement.StoreConnMaxIdleSeconds != DefaultPlacementStoreConnMaxIdleSeconds {
		t.Fatalf("etc/db.yaml StoreConnMaxIdleSeconds = %d, want %d",
			c.Placement.StoreConnMaxIdleSeconds, DefaultPlacementStoreConnMaxIdleSeconds)
	}
}

// go/db 不再连 Redis、选库读全局库,也没有本区库和 zone 家族库(A17c):样例不能留着已经没有含义的键,
// 读的人会以为它们还起作用(加载器忽略不认识的键,写了也不报错)。
func TestEtcYamlDropsRetiredKeys(t *testing.T) {
	raw, err := os.ReadFile("../../etc/db.yaml")
	if err != nil {
		t.Fatalf("read etc/db.yaml: %v", err)
	}
	text := string(raw)
	for _, key := range []string{"RedisClient:", "Required:", "RequireRouteReady:", "LegacyNonGlobal:", "Redis:",
		"ZoneStoreMaxOpenConn:", "ZoneStoreMaxIdleConn:", "NonZoneStoreMaxOpenConn:", "NonZoneStoreMaxIdleConn:",
		"ExtraStoreMaxOpenConn:", "ExtraStoreMaxIdleConn:", "ExtraStoreConnMaxIdleSeconds:", "\n    MaxOpenConn:", "\n    MaxIdleConn:"} {
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
	if c.ServerConfig.Database.Hosts == "" || c.Placement.StoreMaxOpenConn <= 0 {
		t.Fatal("the rest of etc/db.yaml must still load")
	}
}
