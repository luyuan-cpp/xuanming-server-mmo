package config

import (
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
)

// TestEtcYamlEnablesPrometheus 钉死 etc/player_locator.yaml 里的 Prometheus 段
// 确实能被解析、并且真的会启用 /metrics。
//
// 这条是「player_locator 完全没有 /metrics」那个可观测性缺口的回归护栏:
// go-zero 的 prometheus.StartAgent 判的是 `len(c.Host) == 0 → 直接 return`,
// 所以 Host 空 = 端点根本不存在,而配置解析不会报任何错 —— 光看启动日志发现不了。
// 顺带把 yaml 本身的可解析性也覆盖了:段名写错时字段会静默保持零值,
// 同样只有这里能抓到。
func TestEtcYamlEnablesPrometheus(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/player_locator.yaml", &c); err != nil {
		t.Fatalf("加载 etc/player_locator.yaml 失败: %v", err)
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
	if c.Prometheus.Port != 9190 {
		t.Errorf("Prometheus.Port = %d,player_locator 约定用 9190", c.Prometheus.Port)
	}
}

// Reconcile 各项零值取默认值,负数按注释语义保留:默认值是唯一事实源(dev yaml 与 K8s ConfigMap 都不写值)。
func TestReconcileConfNormalized(t *testing.T) {
	got := ReconcileConf{}.Normalized()
	want := ReconcileConf{
		AuditIntervalSeconds:    DefaultReconcileAuditIntervalSeconds,
		ScanCount:               DefaultReconcileScanCount,
		ScanPagesPerSecond:      DefaultReconcileScanPagesPerSecond,
		MaxTransitionsPerSecond: DefaultReconcileMaxTransitionsPerSecond,
		DeadGateMembersPerRound: DefaultReconcileDeadGateMembersPerRound,
	}
	if got != want {
		t.Fatalf("零值 Normalized = %+v, want %+v", got, want)
	}

	off := ReconcileConf{AuditIntervalSeconds: -1, ScanPagesPerSecond: -1, MaxTransitionsPerSecond: -1}.Normalized()
	if off.AuditIntervalSeconds != -1 {
		t.Errorf("AuditIntervalSeconds=-1 必须保留为关闭,得到 %d", off.AuditIntervalSeconds)
	}
	if off.ScanPagesPerSecond != -1 || off.MaxTransitionsPerSecond != -1 {
		t.Errorf("负数限速必须保留为不限速,得到 scan=%d transitions=%d", off.ScanPagesPerSecond, off.MaxTransitionsPerSecond)
	}
	if off.ScanCount != DefaultReconcileScanCount || off.DeadGateMembersPerRound != DefaultReconcileDeadGateMembersPerRound {
		t.Errorf("未设置的项仍取默认值,得到 %+v", off)
	}

	custom := ReconcileConf{AuditIntervalSeconds: 3600, ScanCount: 50, ScanPagesPerSecond: 7, MaxTransitionsPerSecond: 9, DeadGateMembersPerRound: 11}
	if custom.Normalized() != custom {
		t.Errorf("显式正值不得被改写,得到 %+v", custom.Normalized())
	}
}

func TestLeaseScanRound(t *testing.T) {
	if got := (LeaseConf{}).ScanRound(); got != DefaultLeaseScanRoundSeconds*time.Second {
		t.Errorf("零值 ScanRound = %v", got)
	}
	if got := (LeaseConf{ScanRoundSeconds: -3}).ScanRound(); got != DefaultLeaseScanRoundSeconds*time.Second {
		t.Errorf("负数按默认值处理,得到 %v", got)
	}
	if got := (LeaseConf{ScanRoundSeconds: 5}).ScanRound(); got != 5*time.Second {
		t.Errorf("显式值不得被改写,得到 %v", got)
	}
}

// PlayerRedis 段不写时由服务指向 RedisClient(svc.playerRedisConf):开发环境不用改配置就能跑。
func TestEtcYamlLeavesPlayerRedisToFallback(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/player_locator.yaml", &c); err != nil {
		t.Fatalf("加载 etc/player_locator.yaml 失败: %v", err)
	}
	if len(c.PlayerRedis.Addrs) != 0 {
		t.Fatalf("etc/player_locator.yaml 的 PlayerRedis.Addrs = %v;dev 环境靠缺省指向 RedisClient", c.PlayerRedis.Addrs)
	}
}

// dev yaml 只用注释说明 Reconcile 各项,不写值:这些项全服必须一致,K8s ConfigMap 也不镜像,
// 两处各写一份迟早漂移,事实源只留在代码默认值。有人往 yaml 里加值时这里红,提醒同时决定 K8s 怎么镜像。
func TestEtcYamlLeavesReconcileToCodeDefaults(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/player_locator.yaml", &c); err != nil {
		t.Fatalf("加载 etc/player_locator.yaml 失败: %v", err)
	}
	if c.Lease.Reconcile != (ReconcileConf{}) {
		t.Fatalf("etc/player_locator.yaml 的 Lease.Reconcile 写了值 %+v;事实源是代码默认值,要改默认值请改 config.go", c.Lease.Reconcile)
	}
	if c.Lease.ReconcileIntervalSeconds != 0 {
		t.Fatalf("etc/player_locator.yaml 的 Lease.ReconcileIntervalSeconds = %d;全服必须一致,不在 dev yaml 里单独设", c.Lease.ReconcileIntervalSeconds)
	}
	if c.Lease.ScanRoundSeconds != 0 {
		t.Fatalf("etc/player_locator.yaml 的 Lease.ScanRoundSeconds = %d;事实源是代码默认值,要改默认值请改 config.go", c.Lease.ScanRoundSeconds)
	}
}
