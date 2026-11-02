package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
)

// 本文件钉住 TokenConfig 块「缺省即默认值」的语义(docs/design/dual-token-authentication.md「令牌家族、重用检测与家族吊销」)。
//
// 为什么要钉:K8s 的 login ConfigMap 由 k8s_deploy.ps1 另写模板、不含 TokenConfig 块。块缺失时如果拿到零值,
// TTL=0 会让启动校验拒绝启动(login 整体不可用),RefreshReuseRevoke=false 会让重用检测静默退成只观测。

var wantTokenDefaults = TokenConf{
	AccessTokenTTL:     2 * time.Hour,
	RefreshTokenTTL:    720 * time.Hour,
	RefreshReuseGrace:  30 * time.Second,
	RefreshReuseRevoke: true,
}

// Config.TokenConfig 的 tag 不能带 optional:go-zero 对整块缺失的 optional 结构体不填内层 default。
func TestConfigTokenConfigTagIsNotOptional(t *testing.T) {
	f, ok := reflect.TypeOf(Config{}).FieldByName("TokenConfig")
	if !ok {
		t.Fatal("Config.TokenConfig is missing")
	}
	if got := f.Tag.Get("json"); got != "TokenConfig" {
		t.Fatalf("Config.TokenConfig json tag = %q, want %q (optional would zero the whole block when it is absent)",
			got, "TokenConfig")
	}
}

type tokenConfigProbe struct {
	TokenConfig TokenConf `json:"TokenConfig"`
}

func TestTokenConfigBlockMissingMeansDefaults(t *testing.T) {
	var p tokenConfigProbe
	if err := conf.LoadFromYamlBytes([]byte("Name: login.rpc\n"), &p); err != nil {
		t.Fatalf("a missing TokenConfig block must load with defaults, got error: %v", err)
	}
	if p.TokenConfig != wantTokenDefaults {
		t.Fatalf("TokenConfig on a missing block = %+v, want %+v", p.TokenConfig, wantTokenDefaults)
	}
}

// 块写了但只写一部分:没写的字段同样取默认值;显式写 false 的开关保留 false。
func TestTokenConfigPartialBlockFillsDefaults(t *testing.T) {
	var p tokenConfigProbe
	if err := conf.LoadFromYamlBytes([]byte("TokenConfig:\n  RefreshReuseRevoke: false\n"), &p); err != nil {
		t.Fatalf("load partial TokenConfig block: %v", err)
	}
	want := wantTokenDefaults
	want.RefreshReuseRevoke = false
	if p.TokenConfig != want {
		t.Fatalf("TokenConfig on a partial block = %+v, want %+v", p.TokenConfig, want)
	}
}

// 仓内 etc/login.yaml 显式写出的值与默认值一致:本地开发与集群(靠默认值)行为相同。
func TestEtcYamlTokenConfigBlock(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/login.yaml", &c); err != nil {
		t.Fatalf("load etc/login.yaml: %v", err)
	}
	if c.TokenConfig != wantTokenDefaults {
		t.Fatalf("etc/login.yaml TokenConfig = %+v, want %+v", c.TokenConfig, wantTokenDefaults)
	}
}
