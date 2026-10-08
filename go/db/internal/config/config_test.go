package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// placementHolder 只含 Placement 一个字段,标签与 Config 上的完全相同,用来单独钉住 go-zero 对这一段的解析语义,
// 不必为了单测凑齐 Config 的其余必填段。
type placementHolder struct {
	Placement PlacementConfig `json:"Placement,optional"`
}

// 整段缺省必须等价于 AllowStoreFamilies=true、Required=false(player-storage-placement.md §6.1 / §6.2):
// go-zero 对整段 optional 且未出现的嵌套结构不回填内层 default,零值本身必须就是这个语义。
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

// etc/db.yaml 的示例段必须能被解析,且写出的就是缺省语义(段名 / 键名写错会静默保持零值)。
func TestEtcYamlPlacementBlockMatchesDefaults(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/db.yaml", &c); err != nil {
		t.Fatalf("load etc/db.yaml: %v", err)
	}
	c.Normalize()
	if c.Placement.AllowStoreFamilies == nil {
		t.Fatal("etc/db.yaml must spell out AllowStoreFamilies so the example documents the key")
	}
	if !c.Placement.StoreFamiliesAllowed() || c.Placement.Required {
		t.Fatalf("etc/db.yaml must ship the default semantics, got families=%v required=%v",
			c.Placement.StoreFamiliesAllowed(), c.Placement.Required)
	}
	if c.Placement.ExtraStoreMaxOpenConn != DefaultPlacementExtraStoreMaxOpenConn ||
		c.Placement.ExtraStoreMaxIdleConn != DefaultPlacementExtraStoreMaxIdleConn {
		t.Fatalf("etc/db.yaml pool = %d/%d", c.Placement.ExtraStoreMaxOpenConn, c.Placement.ExtraStoreMaxIdleConn)
	}
	if c.Placement.Redis != nil {
		t.Fatal("etc/db.yaml must keep Placement.Redis commented out (reuse RedisClient) for local dev")
	}
}
