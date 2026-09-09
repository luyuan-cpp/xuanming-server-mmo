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

// ── 合服闸门配置(MergeMarkerRedis)─────────────────────────────

// TestMergeMarkerRedisAbsentMeansFenceInactive 钉住"整段缺失 = 闸门不生效"这条契约。
// 这是刻意的降级形态:guild 与 data_service 目前没有别的连线,强制它连一个新 Redis
// 会让所有还没配这段的环境直接起不来;闸门只是合服窗口里的加固。
func TestMergeMarkerRedisAbsentMeansFenceInactive(t *testing.T) {
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(minimalGuildYaml), &c); err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}
	if c.MergeMarkerRedis.Enabled() {
		t.Fatalf("没写 MergeMarkerRedis 时闸门必须是关的,got %+v", c.MergeMarkerRedis)
	}
}

// TestEtcYamlWiresMergeMarkerRedis:仓库里那份 dev 配置要把闸门接上,
// 否则本地根本演练不到合服流程。
//
// DB 必须是 0:data_service 的 mapping Redis 恒在 DB 0
// (go-zero 的 redis.RedisConf 没有 DB 字段,etc/data_service.yaml 里写 DB 会被静默忽略)。
// 填成别的库 = 闸门永远看不到 merge:in_progress:{zone},合服窗口内照样能建帮。
func TestEtcYamlWiresMergeMarkerRedis(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/guild.yaml", &c); err != nil {
		t.Fatalf("加载 etc/guild.yaml 失败: %v", err)
	}
	if !c.MergeMarkerRedis.Enabled() {
		t.Fatal("etc/guild.yaml 应当接上 MergeMarkerRedis,否则本地演练不到合服闸门")
	}
	if c.MergeMarkerRedis.DB != 0 {
		t.Errorf("MergeMarkerRedis.DB = %d;data_service 的 mapping Redis 恒在 DB 0,填别的库闸门必然失效",
			c.MergeMarkerRedis.DB)
	}
}

// minimalGuildYaml 是一份能过 go-zero 必填校验的最小配置(不含 MergeMarkerRedis)。
const minimalGuildYaml = `
Name: guild.rpc
ListenOn: 127.0.0.1:50300
RedisClient:
  Host: 127.0.0.1:6379
  Password: ""
  DB: 2
PlayerLocatorRedis:
  Host: 127.0.0.1:6379
  Password: ""
  DB: 0
MySQL:
  DataSource: "u:p@tcp(127.0.0.1:3306)/mmorpg"
Node:
  ZoneId: 1
  LeaseTTL: 500
Registry:
  Etcd:
    Hosts:
      - 127.0.0.1:2379
    DialTimeout: 5s
Cache:
  DefaultTTL: 30m
  MaxMembers: 50
Kafka:
  Brokers:
    - 127.0.0.1:9092
`
