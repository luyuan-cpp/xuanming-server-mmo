package config

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/service"
)

// 本文件钉住 HMAC 密钥门禁(secrets.go):生产模式拒绝启动,dev / test 只告警。
// 同一处缺陷在两种模式下说的是同一句话 —— 宽松模式的告警(进 SECURITY WARNING 日志)与生产模式的启动错误
// 是同一份文案,而且是英文(AGENTS §2)。

// 测试用密钥:够长、不是占位串,一眼看得出是假值。
var (
	testGateSecret  = strings.Repeat("g", MinSecretLen)
	testQueueSecret = strings.Repeat("q", MinSecretLen)
)

// validSecretsConfig 是一份两把密钥都合格、开了登录排队的配置。
func validSecretsConfig(mode string) *Config {
	cfg := &Config{}
	cfg.Mode = mode
	cfg.Secrets.GateToken.Value = testGateSecret
	cfg.Secrets.QueueToken.Value = testQueueSecret
	cfg.Queue.Enabled = true
	return cfg
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func TestResolveSecretsAcceptsStrongDistinctSecrets(t *testing.T) {
	for _, mode := range []string{service.ProMode, service.DevMode, ""} {
		resolved, warnings, err := ResolveSecrets(validSecretsConfig(mode), envLookup(nil))
		if err != nil || len(warnings) != 0 {
			t.Fatalf("mode=%q: 合格的密钥不应报错或告警: warnings=%v err=%v", mode, warnings, err)
		}
		if string(resolved.GateToken.Primary()) != testGateSecret || string(resolved.QueueToken.Primary()) != testQueueSecret {
			t.Fatalf("mode=%q: 解析出的主密钥不对", mode)
		}
	}
}

// 每处缺陷:生产模式(含空 Mode)拒绝启动;dev 模式照常解析,但告警里有同一句话。
func TestResolveSecretsWeakSecretsFailInProductionAndWarnInDev(t *testing.T) {
	defects := []struct {
		name     string
		weaken   func(*Config)
		wantText []string // 文案必须点名的内容:哪把密钥、什么问题
	}{
		{"GateToken 未配置", func(c *Config) { c.Secrets.GateToken.Value = "" },
			[]string{"GateToken", "not configured"}},
		{"主密钥是占位串", func(c *Config) { c.Secrets.GateToken.Value = LegacyPlaceholderSecret },
			[]string{"GateToken", "primary key", "placeholder"}},
		{"主密钥太短", func(c *Config) { c.Secrets.GateToken.Value = "too-short" },
			[]string{"GateToken", "primary key", "9 bytes"}},
		{"只验不签的旧密钥太短", func(c *Config) { c.Secrets.QueueToken.Additional = []string{"old-short"} },
			[]string{"QueueToken", "additional key #1", "9 bytes"}},
		{"两把主密钥相同", func(c *Config) { c.Secrets.QueueToken.Value = testGateSecret },
			[]string{"GateToken", "QueueToken", "same primary key"}},
	}
	for _, tc := range defects {
		t.Run(tc.name, func(t *testing.T) {
			var strictText string
			for _, mode := range []string{service.ProMode, ""} {
				cfg := validSecretsConfig(mode)
				tc.weaken(cfg)
				resolved, _, err := ResolveSecrets(cfg, envLookup(nil))
				if err == nil || resolved != nil {
					t.Fatalf("mode=%q: 生产模式必须拒绝启动(空 Mode 也按生产),resolved=%v err=%v", mode, resolved, err)
				}
				strictText = err.Error()
			}
			for _, want := range tc.wantText {
				if !strings.Contains(strictText, want) {
					t.Errorf("错误应点名 %q,实际: %s", want, strictText)
				}
			}
			if !isASCII(strictText) {
				t.Errorf("文案必须是英文(它在宽松模式下进日志): %s", strictText)
			}

			cfg := validSecretsConfig(service.DevMode)
			tc.weaken(cfg)
			resolved, warnings, err := ResolveSecrets(cfg, envLookup(nil))
			if err != nil || resolved == nil {
				t.Fatalf("dev 模式只告警、不拒绝启动: err=%v", err)
			}
			if !slices.Contains(warnings, strictText) {
				t.Errorf("dev 模式的告警应与生产模式的错误是同一句话 %q,实际告警: %v", strictText, warnings)
			}
		})
	}
}

// QueueToken 只有开了登录排队才必需;没配时 dev 回落到 GateToken 的密钥并告警,生产不回落。
func TestResolveSecretsQueueTokenRequiredOnlyWhenQueueEnabled(t *testing.T) {
	cfg := validSecretsConfig(service.ProMode)
	cfg.Secrets.QueueToken.Value = ""
	if _, _, err := ResolveSecrets(cfg, envLookup(nil)); err == nil || !strings.Contains(err.Error(), "QueueToken") {
		t.Fatalf("生产模式开了排队却没配 QueueToken 必须拒绝启动,err=%v", err)
	}

	cfg.Queue.Enabled = false
	if _, warnings, err := ResolveSecrets(cfg, envLookup(nil)); err != nil || len(warnings) != 0 {
		t.Fatalf("没开排队时不需要 QueueToken: warnings=%v err=%v", warnings, err)
	}

	dev := validSecretsConfig(service.DevMode)
	dev.Secrets.QueueToken.Value = ""
	resolved, warnings, err := ResolveSecrets(dev, envLookup(nil))
	if err != nil || string(resolved.QueueToken.Primary()) != testGateSecret {
		t.Fatalf("dev 模式应回落到 GateToken 的密钥: err=%v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("回落必须告警:生产不允许两个用途共用一把密钥")
	}
	for _, w := range warnings {
		if !isASCII(w) {
			t.Errorf("告警进日志,必须是英文: %s", w)
		}
	}
}
