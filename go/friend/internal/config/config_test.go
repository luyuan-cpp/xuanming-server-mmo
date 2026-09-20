package config

import (
	"os"
	"regexp"
	"testing"
	"time"

	"friend/internal/data"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
)

// etcYaml 用相对路径而不是绝对路径:`go test ./...` 的工作目录是**被测包目录**,
// 所以这条相对路径在任何机器 / 任何 checkout 位置都成立(与 trade 同名测试同一写法)。
const etcYaml = "../../etc/friend.yaml"

// TestEtcYamlContractValues 钉住 etc/friend.yaml 的关键值。
//
// 为什么值得一条测试:段名 / 键名写错时 go-zero 会把字段静默保持零值(例如 Friend 段拼成
// Friends → 所有阈值变 0、Sweep.Mode 变空串),配置加载不报任何错。这里是编译进测试二进制的
// 第二道网 —— 顺带把 yaml 本身的可解析性也覆盖了。
// conf.Load 内部会调 Validate(go-zero v1.10.0 core/conf/config.go 的 validate(v)),
// 所以这条测试同时证明"etc/friend.yaml 能通过 conf.Load + Validate"。
func TestEtcYamlContractValues(t *testing.T) {
	var c Config
	if err := conf.Load(etcYaml, &c); err != nil {
		t.Fatalf("load %s: %v", etcYaml, err)
	}
	// 再显式调一次:万一将来 go-zero 改了自动校验的触发条件,这条断言也不会失效。
	if err := c.Validate(); err != nil {
		t.Fatalf("%s 必须能通过 Validate: %v", etcYaml, err)
	}

	if c.Name != "friend.rpc" {
		t.Errorf("Name = %q, want friend.rpc", c.Name)
	}
	if _, port, err := c.ListenHostPort(); err != nil || port != 50400 {
		t.Errorf("ListenOn = %q (port=%d err=%v), want port 50400(契约 §7 给 friend 留的号)",
			c.ListenOn, port, err)
	}
	if c.Timeout < MinRpcTimeoutMs || c.Timeout > MaxRpcTimeoutMs {
		t.Errorf("Timeout = %d, want [%d, %d]", c.Timeout, MinRpcTimeoutMs, MaxRpcTimeoutMs)
	}
	if c.Mode != service.DevMode {
		t.Errorf("Mode = %q, want dev(go-zero 默认 pro,本地日志与调试入口都依赖 dev)", c.Mode)
	}
	if c.ZoneId != 1 {
		t.Errorf("ZoneId = %d, want 1", c.ZoneId)
	}
	if c.LeaseTTL != 60 {
		t.Errorf("LeaseTTL = %d, want 60(崩溃场景的切换窗口,契约 §2)", c.LeaseTTL)
	}
	if c.Etcd.Key != "" {
		t.Errorf("Etcd.Key = %q, want 空串(全局服务不注册 go-zero key,契约 D-13)", c.Etcd.Key)
	}
	if len(c.Etcd.Hosts) == 0 {
		t.Error("Etcd.Hosts 不能为空")
	}
	if c.MetricsListenAddr != ":9180" {
		t.Errorf("MetricsListenAddr = %q, want :9180(契约 §7 端口分工)", c.MetricsListenAddr)
	}
	if c.KillSwitchPrefix != "" {
		t.Errorf("KillSwitchPrefix = %q, want 空串(用默认前缀 /mmorpg/killswitch/)", c.KillSwitchPrefix)
	}

	// 共享库必须配:它是 FriendRedis 的回落目标,也是读契约 key player:session:{id} 的唯一句柄。
	if c.Redis.Host == "" {
		t.Error("Redis.Host(共享库)不能为空")
	}
	if c.Redis.Type != "node" {
		t.Errorf("Redis.Type = %q, want node(契约 key 的写者含 C++,hiredis 没有集群客户端)", c.Redis.Type)
	}
	// 本地默认是单库形态:FriendRedis 整段注释掉,私有 key 回落共享库。
	// 这条断言不是"必须回落",而是钉住"本地默认形态没被人顺手改成指向一个不存在的集群"。
	if c.FriendRedis.Host != "" {
		t.Errorf("FriendRedis.Host = %q, want 空(本地默认单库形态,整段注释掉)", c.FriendRedis.Host)
	}

	if c.MySQL.DBName != data.DatabaseName {
		t.Errorf("MySQL.DBName = %q, want %q(friend 独占逻辑库,D-14)", c.MySQL.DBName, data.DatabaseName)
	}
	if c.MySQL.MaxOpenConn <= 0 || c.MySQL.MaxIdleConn <= 0 {
		t.Errorf("MySQL 连接池上限 = %d/%d, 必须为正(MaxOpenConns 零值是不限连接数)",
			c.MySQL.MaxOpenConn, c.MySQL.MaxIdleConn)
	}
	if !c.ShouldAutoMigrate() {
		t.Error("本地 dev 应启动期自动建表(Schema.AutoMigrate: true)")
	}

	// 阈值逐项钉死:它们是硬上限与限流,被人"顺手调大"就等于取消了这层保护。
	wantFriend := FriendConf{
		MaxFriends:            200,
		MaxPendingRequests:    50,
		MaxIncomingRequests:   200,
		MaxBlocks:             200,
		RecommendDefaultLimit: 10,
		RecommendMaxLimit:     20,
		RecommendMaxExclude:   64,
		RequestQuotaPerMinute: 10,
		ListReadHardLimit:     1000,
		CacheTTL:              30 * time.Minute,
		Sweep: SweepConf{
			Mode:          SweepModeReportOnly,
			Interval:      5 * time.Minute,
			RetentionDays: 7,
			BatchLimit:    1000,
		},
	}
	if c.Friend != wantFriend {
		t.Errorf("Friend = %+v\nwant %+v", c.Friend, wantFriend)
	}
}

// TestEtcYamlZoneRewriteAnchors 钉住 go_services.ps1 -Zone 改写依赖的 yaml **文本形状**(契约 §7)。
// 形状一变,派生 yaml 就静默不位移端口 / 不改 zone —— 双 zone 联调时表现为两个 zone 抢同一个端口,
// 或者 zone 2 的 friend 注册进 zone 1 的路径。这些都不会报错。
func TestEtcYamlZoneRewriteAnchors(t *testing.T) {
	raw, err := os.ReadFile(etcYaml)
	if err != nil {
		t.Fatalf("read %s: %v", etcYaml, err)
	}
	text := string(raw)

	// 顶层单行 `ListenOn: host:port`,行尾不能带注释(脚本的正则要求 `\s*$`)。
	if !regexp.MustCompile(`(?m)^ListenOn:\s*[^:\s]+:\d+\s*$`).MatchString(text) {
		t.Error("缺少顶层单行 `ListenOn: host:port`")
	}
	// 顶层单行 `MetricsListenAddr: ":9180"`。原来的嵌套 Prometheus 段不被脚本位移。
	if !regexp.MustCompile(`(?m)^MetricsListenAddr:\s*"?[^:"\s]*:9180"?\s*$`).MatchString(text) {
		t.Error("缺少顶层单行 `MetricsListenAddr: \":9180\"`")
	}
	if regexp.MustCompile(`(?m)^Prometheus:`).MatchString(text) {
		t.Error("不应再有嵌套 Prometheus 段:它的端口不会被 -Zone 位移,多副本 / 双 zone 会撞端口")
	}

	// 缩进组只收空格 / 制表符:写成 \s* 会在多行模式下跨过前一个空行吞进换行符,
	// 把顶层键误判成嵌套键。
	zoneRe := regexp.MustCompile(`(?m)^([ \t]*)ZoneId:[ \t]*\d+[ \t]*\r?$`)
	zoneLines := zoneRe.FindAllStringSubmatch(text, -1)
	if len(zoneLines) != 1 {
		t.Fatalf("ZoneId 应只出现一次,实际 %d 次(-Zone 只改写第一处)", len(zoneLines))
	}
	if zoneLines[0][1] != "" {
		t.Error("唯一的 ZoneId 必须是顶层键")
	}

	// 顶层 ZoneId 必须排在所有嵌套段之前。现在只有一处 ZoneId,顺序看似无所谓;
	// 真正的风险是将来有人往某个嵌套段里加了 ZoneId —— 那时 -Zone 会改写排在前面的那一处。
	// 把"顶层 ZoneId 在最前"固化下来,这个风险就永远只影响嵌套的那一处。
	zoneIdx := zoneRe.FindStringIndex(text)
	// 第一条缩进的非注释行 = 第一个嵌套段的内容([^#\s] 排除掉 `  # ...` 这类缩进注释)。
	nestedIdx := regexp.MustCompile(`(?m)^[ \t]+[^#\s]`).FindStringIndex(text)
	if nestedIdx == nil {
		t.Fatal("yaml 里找不到任何嵌套段,配置结构已被改动")
	}
	if zoneIdx[0] > nestedIdx[0] {
		t.Error("顶层 ZoneId 必须写在所有嵌套段之前")
	}

	// 共享 Redis 段必须显式写 Key: ""(整行省略会让 conf 加载期报 Redis.Key is not set)。
	if !regexp.MustCompile(`(?m)^Redis:\r?\n(?:[ \t]+.*\r?\n)*?[ \t]+Key:\s*""`).MatchString(text) {
		t.Error("Redis 段必须显式写 `Key: \"\"`")
	}
	// Etcd 段同理(契约 D-13)。
	if !regexp.MustCompile(`(?m)^Etcd:\r?\n(?:[ \t]+.*\r?\n)*?[ \t]+Key:\s*""`).MatchString(text) {
		t.Error("Etcd 段必须显式写 `Key: \"\"`")
	}
	// go-zero 的 redis.RedisConf 根本没有 DB 字段,写了也是静默忽略;而契约 §4 要求
	// 契约 key 一律落默认 DB 0(多运行时共写)。原配置写过 `DB: 3`,这条挡住它回来。
	if regexp.MustCompile(`(?m)^[ \t]*DB:`).MatchString(text) {
		t.Error("不应出现 DB: 键(契约 key 由多运行时共写,DB 号必须全仓一致 = 默认 0)")
	}
	// chat 必须屏蔽 SendChat 的请求体;friend 的请求体只有数字 id,刻意不屏蔽(见 yaml 末尾注释)。
	// 只匹配真正的键行(行首缩进后直接跟键名),yaml 末尾解释这个判断的注释不算。
	if regexp.MustCompile(`(?m)^[ \t]*IgnoreContentMethods:`).MatchString(text) {
		t.Error("friend 不需要 StatConf.IgnoreContentMethods:请求体没有隐私正文,全量日志是排障资产")
	}
}

// validConfig 是一份能通过 Validate 的最小配置,负向用例在它上面逐项改坏。
// 刻意不从 yaml 读:yaml 里任何一处被改坏,负向用例就会因为"基准本来就不合法"而集体误报。
func validConfig() Config {
	c := Config{
		ZoneId:            1,
		LeaseTTL:          60,
		MetricsListenAddr: ":9180",
		MySQL: MySQLConf{
			Host: "127.0.0.1:3306", User: "appuser", DBName: data.DatabaseName,
			MaxOpenConn: 20, MaxIdleConn: 5,
		},
		Friend: FriendConf{
			MaxFriends:            200,
			MaxPendingRequests:    50,
			MaxIncomingRequests:   200,
			MaxBlocks:             200,
			RecommendDefaultLimit: 10,
			RecommendMaxLimit:     20,
			RecommendMaxExclude:   64,
			RequestQuotaPerMinute: 10,
			ListReadHardLimit:     1000,
			CacheTTL:              30 * time.Minute,
			Sweep: SweepConf{
				Mode:          SweepModeReportOnly,
				Interval:      5 * time.Minute,
				RetentionDays: 7,
				BatchLimit:    1000,
			},
		},
	}
	c.ListenOn = "127.0.0.1:50400"
	c.Timeout = 4000
	c.Etcd.Hosts = []string{"127.0.0.1:2379"}
	c.Redis.Host = "127.0.0.1:6379"
	c.Redis.Type = "node"
	return c
}

func TestValidateAcceptsBaseline(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatalf("基准配置应通过校验: %v", err)
	}
}

// TestValidateAcceptsOptionalSections 证明"整段可缺失"的几段真的可以缺:
// Schema 缺失 = 自动建表;FriendRedis 缺失 = 回落共享库;Kafka.Brokers 缺失 = 不推送。
// 这三条都是本地 / 冒烟环境的常态,不能被 Validate 拒。
func TestValidateAcceptsOptionalSections(t *testing.T) {
	c := validConfig()
	c.Schema = SchemaConf{}
	c.FriendRedis.Host = ""
	c.Kafka = KafkaConf{}
	if err := c.Validate(); err != nil {
		t.Fatalf("Schema / FriendRedis / Kafka 整段缺失应通过校验: %v", err)
	}
	if !c.ShouldAutoMigrate() {
		t.Error("Schema 段缺失时应按自动建表处理")
	}

	// FriendRedis 配上独立实例(甚至 cluster)同样合法:私有 key 全是单 key 操作。
	c.FriendRedis.Host = "127.0.0.1:7000,127.0.0.1:7001"
	c.FriendRedis.Type = "cluster"
	if err := c.Validate(); err != nil {
		t.Fatalf("FriendRedis 配 cluster 应通过校验: %v", err)
	}
}

// TestValidateRejects 覆盖 Validate 的每一条拒绝。每条拒绝对应的都是"进程能起来、
// 但行为静默出错"的配置,理由写在 config.go 的对应分支旁。
func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"ZoneId=0", func(c *Config) { c.ZoneId = 0 }},
		{"ListenOn 为空", func(c *Config) { c.ListenOn = "" }},
		{"ListenOn 不是 host:port", func(c *Config) { c.ListenOn = "50400" }},
		{"ListenOn 端口为 0", func(c *Config) { c.ListenOn = "127.0.0.1:0" }},
		{"ListenOn 端口非数字", func(c *Config) { c.ListenOn = "127.0.0.1:abc" }},
		{"Timeout=0(go-zero 里表示不限时)", func(c *Config) { c.Timeout = 0 }},
		{"Timeout<1000", func(c *Config) { c.Timeout = 999 }},
		{"Timeout>4000", func(c *Config) { c.Timeout = 4001 }},
		{"Etcd.Hosts 为空", func(c *Config) { c.Etcd.Hosts = nil }},
		{"Etcd.Key 非空", func(c *Config) { c.Etcd.Key = "friend.rpc" }},
		{"LeaseTTL=0", func(c *Config) { c.LeaseTTL = 0 }},
		{"LeaseTTL 为负", func(c *Config) { c.LeaseTTL = -1 }},
		{"MySQL.Host 为空", func(c *Config) { c.MySQL.Host = "" }},
		{"MySQL.User 为空", func(c *Config) { c.MySQL.User = "" }},
		{"MySQL.DBName 为空", func(c *Config) { c.MySQL.DBName = "" }},
		{"MySQL.DBName 指向别的库", func(c *Config) { c.MySQL.DBName = "mmorpg" }},
		{"MySQL.MaxOpenConn=0", func(c *Config) { c.MySQL.MaxOpenConn = 0 }},
		{"MySQL.MaxIdleConn=0", func(c *Config) { c.MySQL.MaxIdleConn = 0 }},
		{"Redis.Host 为空(共享库)", func(c *Config) { c.Redis.Host = "" }},

		{"Friend.MaxFriends=0", func(c *Config) { c.Friend.MaxFriends = 0 }},
		{"Friend.MaxPendingRequests=0", func(c *Config) { c.Friend.MaxPendingRequests = 0 }},
		{"Friend.MaxIncomingRequests=0", func(c *Config) { c.Friend.MaxIncomingRequests = 0 }},
		{"Friend.MaxBlocks=0", func(c *Config) { c.Friend.MaxBlocks = 0 }},
		{"Friend.RecommendDefaultLimit=0", func(c *Config) { c.Friend.RecommendDefaultLimit = 0 }},
		{"Friend.RecommendMaxLimit=0", func(c *Config) { c.Friend.RecommendMaxLimit = 0 }},
		{"Friend.RecommendMaxExclude=0", func(c *Config) { c.Friend.RecommendMaxExclude = 0 }},
		{"Friend.RequestQuotaPerMinute=0", func(c *Config) { c.Friend.RequestQuotaPerMinute = 0 }},
		{"Friend.ListReadHardLimit=0", func(c *Config) { c.Friend.ListReadHardLimit = 0 }},
		{"整段 Friend 缺失", func(c *Config) { c.Friend = FriendConf{} }},

		{"RecommendDefaultLimit>RecommendMaxLimit", func(c *Config) { c.Friend.RecommendDefaultLimit = 21 }},
		{"RecommendMaxLimit 超过硬天花板 20", func(c *Config) { c.Friend.RecommendMaxLimit = 21 }},
		{"Friend.CacheTTL=0(等于永不过期)", func(c *Config) { c.Friend.CacheTTL = 0 }},
		{"Friend.CacheTTL 为负", func(c *Config) { c.Friend.CacheTTL = -time.Second }},

		{"Sweep.Mode 为空串(整段没写、default 未回填)", func(c *Config) { c.Friend.Sweep.Mode = "" }},
		{"Sweep.Mode 未知取值", func(c *Config) { c.Friend.Sweep.Mode = "archive" }},
		{"Sweep.Interval=0", func(c *Config) { c.Friend.Sweep.Interval = 0 }},
		{"Sweep.Interval 为负", func(c *Config) { c.Friend.Sweep.Interval = -time.Minute }},
		{"Sweep.RetentionDays=0", func(c *Config) { c.Friend.Sweep.RetentionDays = 0 }},
		{"Sweep.RetentionDays 为负", func(c *Config) { c.Friend.Sweep.RetentionDays = -1 }},
		{"Sweep.BatchLimit=0", func(c *Config) { c.Friend.Sweep.BatchLimit = 0 }},
		{"Sweep.BatchLimit 为负", func(c *Config) { c.Friend.Sweep.BatchLimit = -1 }},
		{"整段 Sweep 缺失", func(c *Config) { c.Friend.Sweep = SweepConf{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("应被 Validate 拒绝")
			}
		})
	}
}

// TestValidateAcceptsBothSweepModes 两个枚举值都必须放行 —— 否则 delete 模式永远上不了线。
func TestValidateAcceptsBothSweepModes(t *testing.T) {
	for _, mode := range []string{SweepModeReportOnly, SweepModeDelete} {
		c := validConfig()
		c.Friend.Sweep.Mode = mode
		if err := c.Validate(); err != nil {
			t.Errorf("Sweep.Mode=%q 应通过校验: %v", mode, err)
		}
	}
}

// TestRequestBudget 钉住整请求业务预算:严格小于服务端 Timeout(给 in-band 回包留余量),
// 且在 Validate 放行的最小 Timeout 下仍为正 —— 预算为非正意味着所有 I/O 一进 handler 就过期。
func TestRequestBudget(t *testing.T) {
	c := validConfig()
	if got, want := c.RequestBudget(), 3500*time.Millisecond; got != want {
		t.Errorf("Timeout=4000 时 RequestBudget = %v, want %v", got, want)
	}

	c.Timeout = MinRpcTimeoutMs
	if err := c.Validate(); err != nil {
		t.Fatalf("Timeout=%d 应通过校验: %v", MinRpcTimeoutMs, err)
	}
	if got := c.RequestBudget(); got <= 0 || got >= time.Duration(c.Timeout)*time.Millisecond {
		t.Errorf("Timeout=%d 时 RequestBudget = %v, want (0, Timeout)", c.Timeout, got)
	}
}

func TestShouldAutoMigrate(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		v    *bool
		want bool
	}{
		{"没写 = 建表", nil, true},
		{"显式 true", &yes, true},
		{"显式 false", &no, false},
	}
	for _, tc := range cases {
		c := Config{Schema: SchemaConf{AutoMigrate: tc.v}}
		if got := c.ShouldAutoMigrate(); got != tc.want {
			t.Errorf("%s: ShouldAutoMigrate = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsRelaxedMode(t *testing.T) {
	for mode, want := range map[string]bool{
		service.DevMode:  true,
		service.TestMode: true,
		service.RtMode:   false,
		service.PreMode:  false,
		service.ProMode:  false,
		"":               false,
	} {
		if got := IsRelaxedMode(mode); got != want {
			t.Errorf("IsRelaxedMode(%q) = %v, want %v", mode, got, want)
		}
	}
}

// TestListenHostPort 钉住 friend.go 的 advertisedHost 依赖的解析行为:
// 通配地址必须能被解析出空 / :: 的 host(那时才轮到 POD_IP / InternalIp 兜底),
// 而不是解析失败导致注册时把 "0.0.0.0:50400" 原样通告给路由服(路由服拨不通)。
func TestListenHostPort(t *testing.T) {
	cases := []struct {
		listenOn string
		wantHost string
		wantPort uint32
	}{
		{"127.0.0.1:50400", "127.0.0.1", 50400},
		{"0.0.0.0:50400", "0.0.0.0", 50400},
		{":50400", "", 50400},
		{"[::]:50400", "::", 50400},
	}
	for _, tc := range cases {
		c := Config{}
		c.ListenOn = tc.listenOn
		host, port, err := c.ListenHostPort()
		if err != nil {
			t.Errorf("ListenHostPort(%q) 返回错误: %v", tc.listenOn, err)
			continue
		}
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("ListenHostPort(%q) = (%q, %d), want (%q, %d)",
				tc.listenOn, host, port, tc.wantHost, tc.wantPort)
		}
	}
}
