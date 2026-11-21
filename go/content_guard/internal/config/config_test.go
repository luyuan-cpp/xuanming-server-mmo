package config

import (
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
)

// loadShipped 读随服务提交的本机配置:它必须能加载并通过校验,否则 go_services.ps1 起不来。
func loadShipped(t *testing.T) Config {
	t.Helper()
	var c Config
	if err := conf.Load("../../etc/content_guard.yaml", &c); err != nil {
		t.Fatalf("load etc/content_guard.yaml: %v", err)
	}
	return c
}

func TestShippedConfigIsValid(t *testing.T) {
	c := loadShipped(t)
	if err := c.Validate(); err != nil {
		t.Fatalf("etc/content_guard.yaml does not validate: %v", err)
	}
	// 与 tools/scripts/go_services.ps1 的目录、设计文档一致的几个值。
	if c.ListenOn != "127.0.0.1:50250" || c.MetricsListenAddr != ":9290" || c.Etcd.Key != "contentguard.rpc" {
		t.Fatalf("ports or discovery key drifted: ListenOn=%q MetricsListenAddr=%q Etcd.Key=%q", c.ListenOn, c.MetricsListenAddr, c.Etcd.Key)
	}
	if c.Mode != service.DevMode || c.RequireLexicon {
		t.Fatalf("the local config must be dev mode without RequireLexicon: Mode=%q RequireLexicon=%v", c.Mode, c.RequireLexicon)
	}
	if got := c.EffectiveLexiconPrefix(); got != "/mmorpg/content_guard/lexicon/" {
		t.Fatalf("EffectiveLexiconPrefix = %q", got)
	}
}

func TestValidateRejections(t *testing.T) {
	cases := map[string]func(*Config){
		"no etcd hosts":             func(c *Config) { c.Etcd.Hosts = nil },
		"zone zero":                 func(c *Config) { c.ZoneId = 0 },
		"lease zero":                func(c *Config) { c.LeaseTTL = 0 },
		"text limit too small":      func(c *Config) { c.MaxTextBytes = 63 },
		"text limit too large":      func(c *Config) { c.MaxTextBytes = 64*1024 + 1 },
		"prefix without slash":      func(c *Config) { c.LexiconPrefix = "/mmorpg/content_guard/lexicon" },
		"pro without lexicon":       func(c *Config) { c.Mode = service.ProMode },
		"pre without lexicon":       func(c *Config) { c.Mode = service.PreMode },
		"stat content not ignored":  func(c *Config) { c.Middlewares.StatConf.IgnoreContentMethods = nil },
		"platform method unignored": func(c *Config) { c.Middlewares.StatConf.IgnoreContentMethods = []string{CheckTextFullMethod} },
		"invalid async timeout":     func(c *Config) { c.Platform.AsyncTimeoutMs = 10001 },
	}
	for name, mutate := range cases {
		c := loadShipped(t)
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate accepted the config", name)
		}
	}
}

func TestValidateAcceptedVariants(t *testing.T) {
	cases := map[string]func(*Config){
		"pro with lexicon":      func(c *Config) { c.Mode = service.ProMode; c.RequireLexicon = true },
		"custom prefix":         func(c *Config) { c.LexiconPrefix = "/staging/content_guard/lexicon/" },
		"stat disabled":         func(c *Config) { c.Middlewares.Stat = false; c.Middlewares.StatConf.IgnoreContentMethods = nil },
		"metrics port disabled": func(c *Config) { c.MetricsListenAddr = "" },
	}
	for name, mutate := range cases {
		c := loadShipped(t)
		mutate(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("%s: Validate rejected the config: %v", name, err)
		}
	}
	c := loadShipped(t)
	c.LexiconPrefix = "/staging/content_guard/lexicon/"
	if got := c.EffectiveLexiconPrefix(); !strings.HasPrefix(got, "/staging/") {
		t.Fatalf("EffectiveLexiconPrefix ignored the configured prefix: %q", got)
	}
}
