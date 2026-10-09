package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeromicro/go-zero/core/conf"
)

// minimalYaml 是一份**没有 Schema / Kafka 段**的配置:线上 ConfigMap 与本地脚本都可能
// 只写自己关心的键。整段缺失时的零值必须直接就是安全语义,这正是本文件锁死的东西。
const minimalYaml = `
Name: dataservice.rpc
ListenOn: 0.0.0.0:9000
MappingRedis:
  Host: 127.0.0.1:6379
  Type: node
Regions:
  - Id: 1
    Zones: [1]
    Redis:
      Addrs:
        - 127.0.0.1:7001
`

func loadYaml(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data_service.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	var c Config
	if err := conf.Load(path, &c); err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return c
}

// TestSchemaDefaultsWhenBlockAbsent 锁死 G2:yaml 里没有 `Schema:` 段时,建表仍然要跑。
//
// go-zero 的 mapping 不会下钻一个"整段 optional 且未出现"的嵌套结构,所以那里面的
// `json:",default=true"` 一个都不会被回填 —— 裸 bool 会得到 false,启动路径一条 DDL 都
// 不跑,三个 store 打在一个没有表的库上,rollback / txlog / AllocateIdSegment 每一次调用
// 都在运行期炸(收口手写 DDL 之前,store 自己的 CREATE TABLE IF NOT EXISTS 还兜着)。
// 所以 AutoMigrate 是 *bool:nil(没写)= 安全默认 = 建表。
func TestSchemaDefaultsWhenBlockAbsent(t *testing.T) {
	c := loadYaml(t, minimalYaml)
	if c.Schema.AutoMigrate != nil {
		t.Fatalf("an absent Schema block must leave AutoMigrate unset, got %v", *c.Schema.AutoMigrate)
	}
	if !c.ShouldAutoMigrate() {
		t.Fatal("yaml without a Schema block must still auto-migrate (safe default)")
	}
}

// TestSchemaAutoMigrateFalseHonoured 显式关闭(staging/prod 的 ConfigMap 形态)必须生效。
func TestSchemaAutoMigrateFalseHonoured(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
Schema:
  AutoMigrate: false
`)
	if c.ShouldAutoMigrate() {
		t.Fatal("Schema.AutoMigrate=false must disable the startup DDL path")
	}
}

// TestSchemaAutoMigrateTrueHonoured 显式打开(dev/本地 yaml 形态)。
func TestSchemaAutoMigrateTrueHonoured(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
Schema:
  AutoMigrate: true
`)
	if !c.ShouldAutoMigrate() {
		t.Fatal("Schema.AutoMigrate=true must enable the startup DDL path")
	}
}

// TestSchemaBlockPresentWithoutKey 只写 `Schema:` 却不写键,语义仍是"建表"。
func TestSchemaBlockPresentWithoutKey(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
Schema: {}
`)
	if !c.ShouldAutoMigrate() {
		t.Fatal("an empty Schema block must keep the safe default (auto-migrate on)")
	}
}

// TestIdSegmentDefaultsWhenBlockAbsent 锁死设计 §7.5 第 7 条的生产安全值:yaml 里没有
// `IdSegment:` 段时,运行期**不得**自动补种号段行(AllowAutoSeed=false),而迁移仍然要
// 预建默认清单里每种永久身份的行。这里 AllowAutoSeed 用的是裸 bool:go-zero 不回填未出现结构里的
// default,但本键的安全值恰好就是零值,所以块缺失得到的正是生产语义。
func TestIdSegmentDefaultsWhenBlockAbsent(t *testing.T) {
	c := loadYaml(t, minimalYaml)
	if c.IdSegment.AllowAutoSeed {
		t.Fatal("an absent IdSegment block must NOT allow runtime auto-seeding (production semantics)")
	}
	if got, want := c.IdSegment.EffectiveBootstrapTags(), DefaultIdSegmentBootstrapTags; !equalStrings(got, want) {
		t.Fatalf("bootstrap tags without a block = %v, want the default set %v", got, want)
	}
}

// TestIdSegmentBlockPresentWithoutKeys 只写 `IdSegment:` 却不写键,语义与缺失一致。
func TestIdSegmentBlockPresentWithoutKeys(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
IdSegment: {}
`)
	if c.IdSegment.AllowAutoSeed {
		t.Fatal("an empty IdSegment block must keep AllowAutoSeed=false")
	}
	if got, want := c.IdSegment.EffectiveBootstrapTags(), DefaultIdSegmentBootstrapTags; !equalStrings(got, want) {
		t.Fatalf("bootstrap tags with an empty block = %v, want %v", got, want)
	}
}

// TestIdSegmentAllowAutoSeedHonoured dev yaml 显式打开补种(仅 dev)。
func TestIdSegmentAllowAutoSeedHonoured(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
IdSegment:
  AllowAutoSeed: true
`)
	if !c.IdSegment.AllowAutoSeed {
		t.Fatal("IdSegment.AllowAutoSeed=true must be honoured")
	}
	// 只写 AllowAutoSeed 不写 BootstrapTags:清单仍是默认清单,不是空。
	if got, want := c.IdSegment.EffectiveBootstrapTags(), DefaultIdSegmentBootstrapTags; !equalStrings(got, want) {
		t.Fatalf("bootstrap tags = %v, want %v", got, want)
	}
}

// TestIdSegmentBootstrapTagsOverride 显式清单覆盖默认;返回的是副本,改它不能污染包级默认值。
func TestIdSegmentBootstrapTagsOverride(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
IdSegment:
  BootstrapTags: [player, pet]
`)
	got := c.IdSegment.EffectiveBootstrapTags()
	if !equalStrings(got, []string{"player", "pet"}) {
		t.Fatalf("bootstrap tags = %v, want [player pet]", got)
	}
	defaults := IdSegmentConfig{}.EffectiveBootstrapTags()
	defaults[0] = "mutated"
	if DefaultIdSegmentBootstrapTags[0] != "player" {
		t.Fatal("EffectiveBootstrapTags must return a copy, not the package-level default slice")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestKafkaTopicGenerationDefaults 锁死 G1 的有效 topic 名:代号恒定出现在名字里,
// 连 generation=1 也不例外(base 名已经被 broker 自动建成 1 分区,不能再用)。
func TestKafkaTopicGenerationDefaults(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
Kafka:
  Brokers:
    - 127.0.0.1:9092
`)
	if got, want := c.Kafka.EffectiveTransactionLogTopic(), "transaction_log_topic_g1"; got != want {
		t.Fatalf("transaction log topic = %q, want %q", got, want)
	}
	if got, want := c.Kafka.EffectiveSnapshotTopic(), "player_snapshot_topic_g1"; got != want {
		t.Fatalf("snapshot topic = %q, want %q", got, want)
	}
}

// TestKafkaTopicGenerationBump 分区契约变更 = 换代号 = 换名字,老 topic 原地不动。
func TestKafkaTopicGenerationBump(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
Kafka:
  Brokers:
    - 127.0.0.1:9092
  TopicGeneration: 3
  TransactionLogPartitions: 12
`)
	if got, want := c.Kafka.EffectiveTransactionLogTopic(), "transaction_log_topic_g3"; got != want {
		t.Fatalf("transaction log topic = %q, want %q", got, want)
	}
	if got, want := c.Kafka.EffectiveSnapshotTopic(), "player_snapshot_topic_g3"; got != want {
		t.Fatalf("snapshot topic = %q, want %q", got, want)
	}
}

// TestKafkaBaseTopicOverride base 名仍可配(C++ 端改名时两边要能一起改)。
func TestKafkaBaseTopicOverride(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
Kafka:
  Brokers:
    - 127.0.0.1:9092
  TransactionLogTopic: tx_other
  SnapshotTopic: snap_other
  TopicGeneration: 2
`)
	if got, want := c.Kafka.EffectiveTransactionLogTopic(), "tx_other_g2"; got != want {
		t.Fatalf("transaction log topic = %q, want %q", got, want)
	}
	if got, want := c.Kafka.EffectiveSnapshotTopic(), "snap_other_g2"; got != want {
		t.Fatalf("snapshot topic = %q, want %q", got, want)
	}
}

// ── AdminToken(运维 RPC 鉴权,RemapHomeZoneForMerge)────────────

// TestAdminTokenAbsentMeansDisabled 钉住默认形态:yaml 里没有 AdminToken 时它是空串,
// 而空串在 server 侧的语义是**该 RPC 停用**(不是免鉴权)。
// data_service 在集群内网无鉴权可达,一个默认敞开的 remap 接口等于把"改写全服玩家
// 归属 zone"的能力交给任何能拨到它的进程,而这个操作没有反向操作。
func TestAdminTokenAbsentMeansDisabled(t *testing.T) {
	c := loadYaml(t, minimalYaml)
	if c.AdminToken != "" {
		t.Fatalf("AdminToken must default to empty (= RPC disabled), got %q", c.AdminToken)
	}
}

// TestAdminTokenHonoured 显式配置时要读得到。
func TestAdminTokenHonoured(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
AdminToken: merge-window-token
`)
	if c.AdminToken != "merge-window-token" {
		t.Fatalf("AdminToken = %q, want merge-window-token", c.AdminToken)
	}
}

// TestShippedYamlLoads 保证仓库里那份 etc/data_service.yaml 始终能被 go-zero 读进来。
//
// 这条不是形式主义:MappingRedis 段刚删掉一个**被静默忽略**的 `DB: 15`
// (go-zero 的 redis.RedisConf 没有 DB 字段,映射实际一直在 DB 0)。
// 顺带钉住 AdminToken 在这份 dev 配置里保持关闭 —— 本地栈不该默认开着 remap。
func TestShippedYamlLoads(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	if c.MappingRedis.Host == "" {
		t.Fatal("etc/data_service.yaml: MappingRedis.Host 不能为空")
	}
	if c.AdminToken != "" {
		t.Fatalf("etc/data_service.yaml 不该内置 AdminToken(会让本地栈默认开着 remap),got %q", c.AdminToken)
	}
}

// ── PlayerName(玩家名字注册表,设计 03-names.md §3.6a)──────────

// wantPlayerNameDefaults 把默认值集中写一份,几个用例共用。
func wantPlayerNameDefaults() PlayerNameConfig {
	return PlayerNameConfig{
		MaxOpenConn:      20,
		MaxIdleConn:      5,
		ReleaseWindow:    10 * time.Minute,
		CacheTTL:         24 * time.Hour,
		NegativeCacheTTL: 60 * time.Second,
	}
}

func assertPlayerName(t *testing.T, got, want PlayerNameConfig) {
	t.Helper()
	if got != want {
		t.Fatalf("PlayerName = %+v, want %+v", got, want)
	}
}

// TestPlayerNameDefaultsWhenBlockAbsent 锁死本段最危险的性质:**零值全是危险值**。
// go-zero 不下钻一个未出现的 optional 结构,所以这里不能靠 `default=` 标签;
// 默认值由 Normalize() 在代码里填(装配点在 svc.NewServiceContext)。
// 如果哪天有人把 Normalize 从装配路径里拿掉,线上就会得到 CacheTTL=0(每次缓存回填
// 都报错)和 ReleaseWindow=0(建角失败后的条件释放什么都删不掉,全变孤儿)。
func TestPlayerNameDefaultsWhenBlockAbsent(t *testing.T) {
	c := loadYaml(t, minimalYaml)
	if (c.PlayerName != PlayerNameConfig{}) {
		t.Fatalf("an absent PlayerName block must stay zero-valued, got %+v", c.PlayerName)
	}
	assertPlayerName(t, c.PlayerName.Normalize(), wantPlayerNameDefaults())
	if err := c.PlayerName.Normalize().Validate(); err != nil {
		t.Fatalf("normalized defaults must validate: %v", err)
	}
}

// TestPlayerNameBlockPresentWithoutKeys 只写 `PlayerName:` 却不写键,语义与缺失一致。
func TestPlayerNameBlockPresentWithoutKeys(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
PlayerName: {}
`)
	assertPlayerName(t, c.PlayerName.Normalize(), wantPlayerNameDefaults())
}

// TestPlayerNameExplicitValuesHonoured 顺带钉住 go-zero 能把 "30m" 这种字面量读成
// time.Duration:读不成的话整份 yaml 会在启动期报错,而不是悄悄走默认值。
func TestPlayerNameExplicitValuesHonoured(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
PlayerName:
  MaxOpenConn: 40
  MaxIdleConn: 8
  ReleaseWindow: 30m
  CacheTTL: 6h
  NegativeCacheTTL: 5s
`)
	assertPlayerName(t, c.PlayerName.Normalize(), PlayerNameConfig{
		MaxOpenConn:      40,
		MaxIdleConn:      8,
		ReleaseWindow:    30 * time.Minute,
		CacheTTL:         6 * time.Hour,
		NegativeCacheTTL: 5 * time.Second,
	})
	if err := c.PlayerName.Normalize().Validate(); err != nil {
		t.Fatalf("explicit values must validate: %v", err)
	}
}

// TestPlayerNamePartialBlockKeepsOtherDefaults 只覆盖一个键时,其余仍取默认值
// —— 压测时常见的"只调连接池"形态。
func TestPlayerNamePartialBlockKeepsOtherDefaults(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
PlayerName:
  MaxOpenConn: 64
`)
	want := wantPlayerNameDefaults()
	want.MaxOpenConn = 64
	assertPlayerName(t, c.PlayerName.Normalize(), want)
}

// TestPlayerNameValidateRejectsBadValues 配错必须**拒绝**而不是纠正:
// 纠正会让 yaml 与进程实际行为不一致,而这几个键里有一个是安全边界(ReleaseWindow)。
// 拒绝的代价是名字 store 不装配 → 建角当场全线拒绝,症状显眼;纠正的代价是一个
// 被悄悄放宽的释放窗口在事故发生前谁都不知道。
func TestPlayerNameValidateRejectsBadValues(t *testing.T) {
	base := wantPlayerNameDefaults()
	cases := []struct {
		name   string
		mutate func(p *PlayerNameConfig)
	}{
		{"MaxOpenConn 为负", func(p *PlayerNameConfig) { p.MaxOpenConn = -1 }},
		{"MaxIdleConn 为负", func(p *PlayerNameConfig) { p.MaxIdleConn = -1 }},
		{"MaxIdleConn 超过 MaxOpenConn", func(p *PlayerNameConfig) { p.MaxIdleConn = p.MaxOpenConn + 1 }},
		{"ReleaseWindow 为负", func(p *PlayerNameConfig) { p.ReleaseWindow = -time.Second }},
		{"ReleaseWindow 超过硬上限", func(p *PlayerNameConfig) { p.ReleaseWindow = MaxPlayerNameReleaseWindow + time.Second }},
		{"CacheTTL 为负", func(p *PlayerNameConfig) { p.CacheTTL = -time.Second }},
		{"NegativeCacheTTL 为负", func(p *PlayerNameConfig) { p.NegativeCacheTTL = -time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatalf("%+v must be rejected", p)
			}
		})
	}
}

// TestPlayerNameValidateAcceptsCapBoundary 硬上限本身是**允许**的(闭区间),
// 免得有人照着报错信息把值改成上限却仍被拒。
func TestPlayerNameValidateAcceptsCapBoundary(t *testing.T) {
	p := wantPlayerNameDefaults()
	p.ReleaseWindow = MaxPlayerNameReleaseWindow
	if err := p.Validate(); err != nil {
		t.Fatalf("ReleaseWindow == cap must be accepted: %v", err)
	}
}

// TestShippedYamlPlayerNameMatchesDefaults:仓库里那份 yaml 写的就是默认值。
// 两处必须一致,否则"文档说 10 分钟、线上跑 30 分钟"这种偏差没人会发现 ——
// 它既不会报错,也不会有任何日志。
func TestShippedYamlPlayerNameMatchesDefaults(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	assertPlayerName(t, c.PlayerName, wantPlayerNameDefaults())
	if err := c.PlayerName.Validate(); err != nil {
		t.Fatalf("shipped PlayerName block must validate: %v", err)
	}
}

// ── 回档帮会闸(docs/design/guild-phase2/07-rollback-fail-closed.md §7.5.5,测试 C1)──────────

// TestGuildCheckDefaultsWhenKeysAbsent:三项都不写时,余量与预算取文档默认值且合法;
// GuildInternalRpc 缺席 = 未配置(回档闸不装配 → 回档一律被拒),而不是"跳过检查"。
func TestGuildCheckDefaultsWhenKeysAbsent(t *testing.T) {
	c := loadYaml(t, minimalYaml)
	if c.GuildClockSkewMarginMs != 300000 {
		t.Fatalf("GuildClockSkewMarginMs default = %d, want 300000", c.GuildClockSkewMarginMs)
	}
	if c.GuildCheckBudgetSeconds != 120 {
		t.Fatalf("GuildCheckBudgetSeconds default = %d, want 120", c.GuildCheckBudgetSeconds)
	}
	if c.HasGuildInternalRpc() {
		t.Fatal("an absent GuildInternalRpc block must read as not configured")
	}
	if err := c.ValidateGuildCheck(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
}

// TestGuildClockSkewMarginBounds 是 C1 的前半:余量 0 / 4999 拒绝,5000 过。
// 下界 = OpBudget × 2:guild 记下的终结时刻是 ProcessOne 的开始时刻,可早于落盘一个 OpBudget;
// 余量为 0 时"快照不含这笔扣款、guild 却记在快照之前"的行会漏掉 = 回档复制资产。
func TestGuildClockSkewMarginBounds(t *testing.T) {
	cases := []struct {
		margin  int64
		wantErr bool
	}{
		{-1, true}, {0, true}, {4999, true}, {5000, false}, {300000, false}, {3600000, false}, {3600001, true},
	}
	for _, tc := range cases {
		c := loadYaml(t, minimalYaml)
		c.GuildClockSkewMarginMs = tc.margin
		if err := c.ValidateGuildCheck(); (err != nil) != tc.wantErr {
			t.Fatalf("GuildClockSkewMarginMs=%d: err=%v, wantErr=%t", tc.margin, err, tc.wantErr)
		}
	}
}

// TestGuildInternalRpcTimeoutBounds 是 C1 的后半:已配 GuildInternalRpc 时 Timeout 499 / 3501 拒绝,
// 500 / 3500 过;块里不写 Timeout 时取 go-zero 默认 2000,同样合法。
func TestGuildInternalRpcTimeoutBounds(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
GuildInternalRpc:
  Endpoints:
    - 127.0.0.1:50300
`)
	if !c.HasGuildInternalRpc() {
		t.Fatal("Endpoints must count as configured")
	}
	if err := c.ValidateGuildCheck(); err != nil {
		t.Fatalf("default Timeout (%d) must validate: %v", c.GuildInternalRpc.Timeout, err)
	}
	cases := []struct {
		timeout int64
		wantErr bool
	}{
		{0, true}, {499, true}, {500, false}, {3000, false}, {3500, false}, {3501, true},
	}
	for _, tc := range cases {
		cc := c
		cc.GuildInternalRpc.Timeout = tc.timeout
		if err := cc.ValidateGuildCheck(); (err != nil) != tc.wantErr {
			t.Fatalf("GuildInternalRpc.Timeout=%d: err=%v, wantErr=%t", tc.timeout, err, tc.wantErr)
		}
	}
}

// TestGuildInternalRpcEtcdOnlyIsNotConfigured:只写了 Etcd(或什么目标都没写)的块按"没配"处理。
// RpcClientConf.Etcd 带 inherit,块里不写 Etcd 时会继承顶层 Etcd 的 Key=dataservice.rpc,拨到自己身上;
// 而 guild 本来就不注册 go-zero key(D-13)。两种都不能被当成"配好了"。
func TestGuildInternalRpcEtcdOnlyIsNotConfigured(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
Etcd:
  Hosts:
    - 127.0.0.1:2379
  Key: dataservice.rpc
GuildInternalRpc:
  Timeout: 3000
`)
	if c.HasGuildInternalRpc() {
		t.Fatalf("a GuildInternalRpc block without Endpoints/Target must not count as configured (etcd=%+v)", c.GuildInternalRpc.Etcd)
	}

	c = loadYaml(t, minimalYaml+`
GuildInternalRpc:
  Etcd:
    Hosts:
      - 127.0.0.1:2379
    Key: guild.rpc
`)
	if c.HasGuildInternalRpc() {
		t.Fatal("etcd discovery of guild is not supported (guild registers no go-zero key); it must read as not configured")
	}
}

// TestGuildCheckBudgetBounds:检查预算 [1, 3600] 秒。
func TestGuildCheckBudgetBounds(t *testing.T) {
	cases := []struct {
		budget  int64
		wantErr bool
	}{
		{-1, true}, {0, true}, {1, false}, {120, false}, {3600, false}, {3601, true},
	}
	for _, tc := range cases {
		c := loadYaml(t, minimalYaml)
		c.GuildCheckBudgetSeconds = tc.budget
		if err := c.ValidateGuildCheck(); (err != nil) != tc.wantErr {
			t.Fatalf("GuildCheckBudgetSeconds=%d: err=%v, wantErr=%t", tc.budget, err, tc.wantErr)
		}
	}
}

// TestGuildRecheckBudgetFollowsCheckBudget:写后复查预算 = max(120s, 检查预算)。复查重走检查的分块翻页,
// 检查预算调大而复查停在 120s 时,数据写完后复查必然超时(每次都误报紧急告警)。
func TestGuildRecheckBudgetFollowsCheckBudget(t *testing.T) {
	cases := []struct {
		checkSeconds int64
		want         time.Duration
	}{
		{1, 120 * time.Second}, {119, 120 * time.Second}, {120, 120 * time.Second},
		{121, 121 * time.Second}, {600, 600 * time.Second}, {MaxGuildCheckBudgetSeconds, time.Hour},
	}
	for _, tc := range cases {
		c := loadYaml(t, minimalYaml)
		c.GuildCheckBudgetSeconds = tc.checkSeconds
		if got := c.GuildRecheckBudget(); got != tc.want {
			t.Fatalf("GuildCheckBudgetSeconds=%d: GuildRecheckBudget() = %v, want %v", tc.checkSeconds, got, tc.want)
		}
	}
}

// TestShippedYamlGuildCheck 钉住仓库里那份 dev yaml 的回档闸配置(07 §7.5.5、§7.5.3-3、§7.9.1):
// 直连 guild 的 ListenOn、单次超时合法、NonBlock、Breaker 关;MetricsListenAddr 已开;三个 Rollback* 配了
// MethodTimeouts,且 RollbackPlayer 的值盖得住"沉降 30 + 检查预算 + 复查等待 10 + 复查预算"
// (沉降 30s 与复查等待 10s 两个常量在 internal/logic/rollback_logic.go;本包不能 import logic,这里照抄数值;
// 复查预算取 Config.GuildRecheckBudget())。
func TestShippedYamlGuildCheck(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	if !c.HasGuildInternalRpc() {
		t.Fatal("etc/data_service.yaml must configure GuildInternalRpc (otherwise every local rollback is rejected)")
	}
	if got := c.GuildInternalRpc.Endpoints; len(got) != 1 || got[0] != "127.0.0.1:50300" {
		t.Fatalf("GuildInternalRpc.Endpoints = %v, want [127.0.0.1:50300] (= go/guild/etc/guild.yaml ListenOn)", got)
	}
	if !c.GuildInternalRpc.NonBlock {
		t.Fatal("GuildInternalRpc.NonBlock must be true: guild being down must not block data_service startup")
	}
	if c.GuildInternalRpc.Middlewares.Breaker {
		t.Fatal("GuildInternalRpc.Middlewares.Breaker must be false: the check never retries, a breaker only muddies the error")
	}
	if err := c.ValidateGuildCheck(); err != nil {
		t.Fatalf("shipped guild check config must validate: %v", err)
	}
	if c.MetricsListenAddr == "" {
		t.Fatal("MetricsListenAddr must be set: the rollback guild alerts need /metrics")
	}

	want := map[string]time.Duration{
		"/data_service.DataService/RollbackPlayer": 0,
		"/data_service.DataService/RollbackZone":   0,
		"/data_service.DataService/RollbackAll":    0,
	}
	for _, mt := range c.MethodTimeouts {
		if _, ok := want[mt.FullMethod]; ok {
			want[mt.FullMethod] = mt.Timeout
		}
	}
	for method, got := range want {
		if got <= 0 {
			t.Fatalf("MethodTimeouts has no entry for %s (go-zero's 2000ms default would cut every rollback short)", method)
		}
	}
	minPlayer := 30*time.Second + time.Duration(c.GuildCheckBudgetSeconds)*time.Second + 10*time.Second + c.GuildRecheckBudget()
	if got := want["/data_service.DataService/RollbackPlayer"]; got < minPlayer {
		t.Fatalf("RollbackPlayer MethodTimeout %v < settle + check budget + recheck delay + recheck budget = %v", got, minPlayer)
	}
}

// Store.Required:本地 yaml 不写(整段缺失)必须保持旧的降级语义;K8s ConfigMap 显式写 true 必须生效。
// 零值就是旧行为,所以这里用裸 bool 是安全的(对照 Schema.AutoMigrate 必须用 *bool 的理由)。
func TestStoreRequiredDefaultsToFalseAndHonoursExplicitTrue(t *testing.T) {
	if c := loadYaml(t, minimalYaml); c.Store.Required {
		t.Fatal("yaml without a Store block must keep the legacy degrade-and-run behaviour")
	}
	if c := loadYaml(t, minimalYaml+`
Store: {}
`); c.Store.Required {
		t.Fatal("an empty Store block must not turn Required on")
	}
	if c := loadYaml(t, minimalYaml+`
Store:
  Required: true
`); !c.Store.Required {
		t.Fatal("Store.Required=true (the K8s ConfigMap form) must be honoured")
	}
}
