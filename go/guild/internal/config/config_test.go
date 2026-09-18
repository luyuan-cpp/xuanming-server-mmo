package config

import (
	"strings"
	"testing"
	"time"

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
	// 端口分工:login 9101 / scene_manager 9150 / db 9160 / match 9170 /
	// friend 9180 / player_locator 9190 / guild 9220。撞端口会让两个服务里后起的那个
	// 静默拿不到 /metrics(ListenAndServe 报错只落一行日志)。
	if c.Prometheus.Port != 9220 {
		t.Errorf("Prometheus.Port = %d,公会服务约定用 9220", c.Prometheus.Port)
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
Timeout: 4000
RedisClient:
  Host: 127.0.0.1:6379
  Password: ""
  DB: 2
PlayerLocatorRedis:
  Host: 127.0.0.1:6379
  Password: ""
  DB: 0
MySQL:
  DataSource: "u:p@tcp(127.0.0.1:3306)/mmorpg_guild"
DataServiceRpc:
  Timeout: 2000
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

// loadMinimal 反序列化最小配置(Validate 由 conf 自动执行);用于在其上改单个字段再直接调 Validate。
func loadMinimal(t *testing.T) Config {
	t.Helper()
	var c Config
	if err := conf.LoadFromYamlBytes([]byte(minimalGuildYaml), &c); err != nil {
		t.Fatalf("最小配置必须能通过校验: %v", err)
	}
	return c
}

// TestValidateTimeoutBudget:Timeout 是路由服 5s 转发预算减去 1s;预算还必须放得下
// 一次 data_service 调用(≤3s)+ 归属区查询(1.5s)+ 回包余量(0.5s),否则建帮会在
// 服务端超时,客户端看到的是 DeadlineExceeded 而不是业务 tip。
func TestValidateTimeoutBudget(t *testing.T) {
	cases := []struct {
		timeout, dataService int64
		wantErr              string
	}{
		{4000, 2000, ""},
		{3500, 1500, ""},
		{4000, 500, ""},
		{4001, 2000, "Timeout"},
		{10000, 2000, "Timeout"},
		{0, 2000, "Timeout"},
		{3999, 2000, "超时预算不足"},
		{4000, 3000, "超时预算不足"},
		{4000, 0, "DataServiceRpc.Timeout"},
		{4000, 499, "DataServiceRpc.Timeout"},
		{4000, 3001, "DataServiceRpc.Timeout"},
	}
	for _, tc := range cases {
		c := loadMinimal(t)
		c.Timeout = tc.timeout
		c.DataServiceRpc.Timeout = tc.dataService
		err := c.Validate()
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("Timeout=%d DataServiceRpc=%d 应通过,得到 %v", tc.timeout, tc.dataService, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("Timeout=%d DataServiceRpc=%d 错误 = %v,期望包含 %q", tc.timeout, tc.dataService, err, tc.wantErr)
		}
	}
}

// TestValidateCallsEmbeddedRpcServerConfValidate:本包的 Validate 遮蔽了嵌入的
// zrpc.RpcServerConf.Validate,忘了显式调用就会丢掉 go-zero 自己的必填校验。
func TestValidateCallsEmbeddedRpcServerConfValidate(t *testing.T) {
	c := loadMinimal(t)
	c.Auth = true
	if err := c.Validate(); err == nil {
		t.Fatal("Auth=true 且未配 Redis 时,嵌入的 RpcServerConf.Validate 必须报错")
	}
	c.Auth = false
	if err := c.Validate(); err != nil {
		t.Fatalf("Auth=false 不该因 Redis 报错: %v", err)
	}
}

// TestValidateRequiresGuildDatabase:帮会表只建在独占库(D-14);DSN 指错库必须拒启,
// 且错误文案不得带出 DSN 里的口令。
func TestValidateRequiresGuildDatabase(t *testing.T) {
	c := loadMinimal(t)
	c.MySQL.DataSource = "u:p@tcp(127.0.0.1:3306)/mmorpg"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "mmorpg_guild") {
		t.Fatalf("库名不是 mmorpg_guild 时必须报错,得到 %v", err)
	}

	c.MySQL.DataSource = "u:p@tcp(127.0.0.1:3306)/mmorpg_guild"
	if err := c.Validate(); err != nil {
		t.Fatalf("独占库应通过: %v", err)
	}

	c.MySQL.DataSource = "u:p4ss@tcp(::bad"
	err = c.Validate()
	if err == nil {
		t.Fatal("DSN 无法解析时必须报错")
	}
	if strings.Contains(err.Error(), "p4ss") {
		t.Fatalf("错误文案泄露了口令: %v", err)
	}
}

func TestEtcYamlPassesValidate(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/guild.yaml", &c); err != nil {
		t.Fatalf("加载 etc/guild.yaml 失败: %v", err)
	}
	if c.Timeout != 4000 {
		t.Errorf("Timeout = %d,期望 4000", c.Timeout)
	}
	if c.DataServiceRpc.Timeout != 2000 {
		t.Errorf("DataServiceRpc.Timeout = %d,期望 2000", c.DataServiceRpc.Timeout)
	}
	if !c.ShouldAutoMigrate() {
		t.Error("dev 档 etc/guild.yaml 应当开启 Schema.AutoMigrate")
	}
	if got := c.RequestBudget(); got != 3500*time.Millisecond {
		t.Errorf("RequestBudget = %v,期望 3500ms", got)
	}
}

func TestShouldAutoMigrateDefaults(t *testing.T) {
	var c Config
	if !c.ShouldAutoMigrate() {
		t.Error("没写 Schema 段时应当建表(dev 默认)")
	}
	no := false
	c.Schema.AutoMigrate = &no
	if c.ShouldAutoMigrate() {
		t.Error("显式 false 时只做只读核对")
	}
}
