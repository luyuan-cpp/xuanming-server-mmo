package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// 账号目录独立池上限(AccountDirectory.MaxOpenConn)。改成可配置之前它写死在 store 构造点为 8;
// 缺省必须仍是 8,否则没写这个键的 ConfigMap 会在升级时悄悄换池大小、连接总账对不上。

// TestAccountDirectoryPoolDefaultsWhenAbsent:整段缺失、或段在但键缺失,都取缺省 8。
func TestAccountDirectoryPoolDefaultsWhenAbsent(t *testing.T) {
	for name, body := range map[string]string{
		"段缺失":   minimalYaml,
		"段在键缺失": minimalYaml + "\nAccountDirectory:\n  MaxPlayersPerAccount: 5\n",
	} {
		c := loadYaml(t, body)
		if c.AccountDirectory.MaxOpenConn != 0 {
			t.Fatalf("%s: absent MaxOpenConn must stay zero-valued, got %d", name, c.AccountDirectory.MaxOpenConn)
		}
		got, err := c.AccountDirectory.PoolMaxOpenConn()
		if DefaultAccountDirectoryMaxOpenConn != 8 {
			t.Fatalf("DefaultAccountDirectoryMaxOpenConn = %d, want 8", DefaultAccountDirectoryMaxOpenConn)
		}
		if err != nil || got != DefaultAccountDirectoryMaxOpenConn {
			t.Fatalf("%s: PoolMaxOpenConn() = %d, %v; want 8, nil", name, got, err)
		}
	}
}

// TestAccountDirectoryPoolExplicitValueHonoured:显式值原样生效。
func TestAccountDirectoryPoolExplicitValueHonoured(t *testing.T) {
	c := loadYaml(t, minimalYaml+"\nAccountDirectory:\n  MaxOpenConn: 32\n")
	got, err := c.AccountDirectory.PoolMaxOpenConn()
	if err != nil || got != 32 {
		t.Fatalf("PoolMaxOpenConn() = %d, %v; want 32, nil", got, err)
	}
}

// TestAccountDirectoryPoolBounds:闭区间 [1, 128] 两端都允许;负数与超过 authoritysql 单池硬上限的值拒绝而不纠正
// (纠正会让 yaml 与实际池大小不一致,连接总账按 yaml 算就错了)。
func TestAccountDirectoryPoolBounds(t *testing.T) {
	for _, ok := range []int{1, MaxAccountDirectoryMaxOpenConn} {
		got, err := AccountDirectoryConfig{MaxOpenConn: ok}.PoolMaxOpenConn()
		if err != nil || got != ok {
			t.Fatalf("MaxOpenConn=%d must be accepted as is, got %d, %v", ok, got, err)
		}
	}
	if MaxAccountDirectoryMaxOpenConn != 128 {
		t.Fatalf("MaxAccountDirectoryMaxOpenConn = %d, want 128 (shared/authoritysql.Open hard cap)", MaxAccountDirectoryMaxOpenConn)
	}
	for _, bad := range []int{-1, MaxAccountDirectoryMaxOpenConn + 1, 1024} {
		if got, err := (AccountDirectoryConfig{MaxOpenConn: bad}).PoolMaxOpenConn(); err == nil {
			t.Fatalf("MaxOpenConn=%d must be rejected, got %d", bad, got)
		}
	}
}

// TestShippedYamlAccountDirectoryPoolIsExplicit:仓库 yaml 显式写 AccountDirectory.MaxOpenConn(部署脚本的连接总账从这个键读),
// 值等于缺省且合法。
func TestShippedYamlAccountDirectoryPoolIsExplicit(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	if c.AccountDirectory.MaxOpenConn != DefaultAccountDirectoryMaxOpenConn {
		t.Fatalf("etc/data_service.yaml AccountDirectory.MaxOpenConn = %d, want explicit %d",
			c.AccountDirectory.MaxOpenConn, DefaultAccountDirectoryMaxOpenConn)
	}
	if _, err := c.AccountDirectory.PoolMaxOpenConn(); err != nil {
		t.Fatalf("shipped AccountDirectory.MaxOpenConn must validate: %v", err)
	}
}
