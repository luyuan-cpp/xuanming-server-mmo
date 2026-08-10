package config

import (
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
)

// TestEtcYamlEnablesPrometheus 钉死 etc/guild.yaml 里的 Prometheus 段确实能被解析、
// 并且真的会启用 /metrics。
//
// 这条是「公会服务完全没有 /metrics」那个可观测性缺口的回归护栏:
// go-zero 的 prometheus.StartAgent 判的是 `len(c.Host) == 0 → 直接 return`,
// 所以 Host 空 = 端点根本不存在,而配置解析不会报任何错 —— 光看启动日志发现不了。
// 顺带把 yaml 本身的可解析性也覆盖了:段名写错(比如写成 Prometheus 的复数形式)
// 时字段会静默保持零值,同样只有这里能抓到。
func TestEtcYamlEnablesPrometheus(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/guild.yaml", &c); err != nil {
		t.Fatalf("加载 etc/guild.yaml 失败: %v", err)
	}

	if c.Prometheus.Host == "" {
		t.Fatal("Prometheus.Host 为空,go-zero 的 StartAgent 会直接 return,/metrics 不存在")
	}
	if c.Prometheus.Port == 0 {
		t.Fatal("Prometheus.Port 为 0")
	}
	if c.Prometheus.Path != "/metrics" {
		t.Errorf("Prometheus.Path = %q,期望 /metrics", c.Prometheus.Path)
	}
	// 端口分工:login 9101 / scene_manager 9150 / db 9160 / guild 9170 /
	// friend 9180 / player_locator 9190。撞端口会让两个服务里后起的那个
	// 静默拿不到 /metrics(ListenAndServe 报错只落一行日志)。
	if c.Prometheus.Port != 9170 {
		t.Errorf("Prometheus.Port = %d,公会服务约定用 9170", c.Prometheus.Port)
	}
}
