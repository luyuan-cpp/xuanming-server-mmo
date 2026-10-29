package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestAccountDirectoryRoleTradeSQLProofRequiresExplicitConfig(t *testing.T) {
	absent := loadYaml(t, minimalYaml)
	if absent.AccountDirectory.RoleTradeSQLProof.Enabled || len(absent.AccountDirectory.RoleTradeSQLProof.AllowedStorageIDs) != 0 || absent.AccountDirectory.MaxRoles() != 5 {
		t.Fatal("缺配置不能隐式启用跨库冻结证明，目录上限仍为全局默认")
	}
	explicit := loadYaml(t, minimalYaml+"\nAccountDirectory:\n  MaxPlayersPerAccount: 7\n  RoleTradeSQLProof:\n    Enabled: true\n    AllowedStorageIDs: [1, 1000000]\n")
	proof := explicit.AccountDirectory.RoleTradeSQLProof
	if !proof.Enabled || len(proof.AllowedStorageIDs) != 2 || proof.AllowedStorageIDs[0] != 1 || proof.AllowedStorageIDs[1] != 1000000 || explicit.AccountDirectory.MaxRoles() != 7 {
		t.Fatal("显式SQL证明白名单/角色上限未被配置解析器识别")
	}
}

func TestAccountDirectoryPendingSchemasAreExplicitAndMatchIsParsed(t *testing.T) {
	absent := loadYaml(t, minimalYaml).AccountDirectory
	if absent.RoleTradeTradeSchema != "" || absent.RoleTradeGuildSchema != "" || absent.RoleTradeMailSchema != "" || absent.RoleTradeMatchSchema != "" || absent.RoleTradeInboxSchema != "" {
		t.Fatal("跨域挂起schema缺失不能从DSN或其它服务配置补齐")
	}
	explicit := loadYaml(t, minimalYaml+"\nAccountDirectory:\n  RoleTradeTradeSchema: peer_trade\n  RoleTradeGuildSchema: peer_guild\n  RoleTradeMailSchema: peer_mail\n  RoleTradeMatchSchema: peer_match\n  RoleTradeInboxSchema: peer_inbox\n").AccountDirectory
	if explicit.RoleTradeTradeSchema != "peer_trade" || explicit.RoleTradeGuildSchema != "peer_guild" || explicit.RoleTradeMailSchema != "peer_mail" || explicit.RoleTradeMatchSchema != "peer_match" || explicit.RoleTradeInboxSchema != "peer_inbox" {
		t.Fatal("显式trade/guild/mail/match权威schema必须完整保留")
	}
	var shipped Config
	if err := conf.Load("../../etc/data_service.yaml", &shipped); err != nil {
		t.Fatal(err)
	}
	if shipped.AccountDirectory.RoleTradeMatchSchema != "mmorpg_match" || shipped.AccountDirectory.RoleTradeInboxSchema != "mmorpg_player_inbox" {
		t.Fatal("仓库配置必须显式登记匹配挂起权威schema")
	}
}

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

// ── AdminToken(运维 RPC 鉴权:Rollback* 与 ReleasePlayerName;RemapHomeZoneForMerge 已停用)────────────

// TestAdminTokenAbsentMeansDisabled 钉住默认形态:yaml 里没有 AdminToken 时它是空串,
// 而空串在 server 侧的语义是**该 RPC 停用**(不是免鉴权)。
// data_service 在集群内网无鉴权可达,一个默认敞开的运维接口等于把"回档 / 强制释放名字"
// 这类没有反向操作的能力交给任何能拨到它的进程。这条理由最初是为 remap 写的;remap 自 2026-10-12
// 起不论 token 一律停用(proposal §4.3),token 现在只守三个 Rollback* 与 ReleasePlayerName 的运维路径。
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
// 顺带钉住 AdminToken 在这份 dev 配置里保持关闭 —— 本地栈不该默认开着运维 RPC
// (今天是 Rollback* 与 ReleasePlayerName 的运维路径;remap 已停用,与 token 无关)。
func TestShippedYamlLoads(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	if c.MappingRedis.Host == "" {
		t.Fatal("etc/data_service.yaml: MappingRedis.Host 不能为空")
	}
	if c.AdminToken != "" {
		t.Fatalf("etc/data_service.yaml 不该内置 AdminToken(会让本地栈默认开着运维 RPC),got %q", c.AdminToken)
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

// ── RouteStore(映射提交点,docs/design/player-route-mapping-durability-proposal.md §4、§11.2)──────────

func intPtr(v int) *int { return &v }

// assertRouteStoreDefaults 断言 got 是 Normalize 之后的默认形态(redis、8、20/5、3、1/0/1500)。
func assertRouteStoreDefaults(t *testing.T, got RouteStoreConfig) {
	t.Helper()
	if got.Mode != RouteStoreModeRedis || got.FenceShards != 8 || got.MaxOpenConn != 20 || got.MaxIdleConn != 5 ||
		got.PublishAttempts != 3 || got.NumLocal() != 1 || got.NumReplicas() != 0 || got.WaitAOFTimeoutMs != 1500 {
		t.Fatalf("RouteStore = %+v (numlocal=%d numreplicas=%d), want redis / 8 / 20,5 / 3 / 1,0,1500",
			got, got.NumLocal(), got.NumReplicas())
	}
	if got.WaitAOFNumLocal == nil || got.WaitAOFNumReplicas == nil {
		t.Fatal("Normalize must fill both WaitAOF pointers")
	}
}

// TestRouteStoreDefaultsWhenBlockAbsent:yaml 里没有 `RouteStore:` 段 = redis 模式,与 2026-10-12 之前逐字相同;
// 规范化之后的默认值合法。go-zero 不下钻未出现的 optional 结构,所以默认值只能由 Normalize 给。
func TestRouteStoreDefaultsWhenBlockAbsent(t *testing.T) {
	c := loadYaml(t, minimalYaml)
	if c.RouteStore.Mode != "" || c.RouteStore.WaitAOFNumLocal != nil || c.RouteStore.WaitAOFNumReplicas != nil {
		t.Fatalf("an absent RouteStore block must stay zero-valued, got %+v", c.RouteStore)
	}
	rs := c.RouteStore.Normalize()
	assertRouteStoreDefaults(t, rs)
	if err := rs.Validate(); err != nil {
		t.Fatalf("normalized defaults must validate: %v", err)
	}
	if rs.DBFirst() {
		t.Fatal("the default mode must be redis, not db_first")
	}
	if !rs.WaitsForAOF() {
		t.Fatal("the default must wait for local AOF (numlocal=1)")
	}

	// 只写 `RouteStore:` 不写键,语义与缺失一致。
	c = loadYaml(t, minimalYaml+`
RouteStore: {}
`)
	assertRouteStoreDefaults(t, c.RouteStore.Normalize())
}

// TestRouteStoreValidateRejects 覆盖规格 d 节的每一条拒绝规则:配错**拒绝**而不是纠正。
// 最要紧的是 Mode 拼错:`DB_FIRST` / `db-first` 被悄悄当成 redis,运维以为切了提交点而实际没切。
func TestRouteStoreValidateRejects(t *testing.T) {
	base := RouteStoreConfig{}.Normalize()
	if err := base.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(r *RouteStoreConfig)
	}{
		{"Mode 大写", func(r *RouteStoreConfig) { r.Mode = "DB_FIRST" }},
		{"Mode 连字符", func(r *RouteStoreConfig) { r.Mode = "db-first" }},
		{"Mode 未知值", func(r *RouteStoreConfig) { r.Mode = "mysql" }},
		{"Mode 首字母大写", func(r *RouteStoreConfig) { r.Mode = "Redis" }},
		{"FenceShards 为负", func(r *RouteStoreConfig) { r.FenceShards = -1 }},
		{"FenceShards 超过 64", func(r *RouteStoreConfig) { r.FenceShards = 65 }},
		{"MaxOpenConn 为负", func(r *RouteStoreConfig) { r.MaxOpenConn = -1 }},
		{"MaxIdleConn 为负", func(r *RouteStoreConfig) { r.MaxIdleConn = -1 }},
		{"MaxIdleConn 超过 MaxOpenConn", func(r *RouteStoreConfig) { r.MaxIdleConn = r.MaxOpenConn + 1 }},
		{"PublishAttempts 为负", func(r *RouteStoreConfig) { r.PublishAttempts = -1 }},
		{"PublishAttempts 超过 5", func(r *RouteStoreConfig) { r.PublishAttempts = 6 }},
		{"WaitAOFNumLocal 为 2", func(r *RouteStoreConfig) { r.WaitAOFNumLocal = intPtr(2) }},
		{"WaitAOFNumLocal 为负", func(r *RouteStoreConfig) { r.WaitAOFNumLocal = intPtr(-1) }},
		{"WaitAOFNumReplicas 为负", func(r *RouteStoreConfig) { r.WaitAOFNumReplicas = intPtr(-1) }},
		{"WaitAOFNumReplicas 超过 16", func(r *RouteStoreConfig) { r.WaitAOFNumReplicas = intPtr(17) }},
		{"WaitAOFTimeoutMs 为负", func(r *RouteStoreConfig) { r.WaitAOFTimeoutMs = -1 }},
		{"WaitAOFTimeoutMs 超过 1500", func(r *RouteStoreConfig) { r.WaitAOFTimeoutMs = 1501 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("%+v must be rejected", r)
			}
			// db_first 下同样拒绝:非法值与 Mode 无关。
			if r.Mode == RouteStoreModeRedis {
				r.Mode = RouteStoreModeDBFirst
				if err := r.Validate(); err == nil {
					t.Fatalf("%+v must be rejected under db_first too", r)
				}
			}
		})
	}

	// 闭区间的边界本身是允许的,免得有人照着报错信息改成边界值却仍被拒。
	accepted := []struct {
		name   string
		mutate func(r *RouteStoreConfig)
	}{
		{"db_first", func(r *RouteStoreConfig) { r.Mode = RouteStoreModeDBFirst }},
		{"FenceShards=1", func(r *RouteStoreConfig) { r.FenceShards = 1 }},
		{"FenceShards=64", func(r *RouteStoreConfig) { r.FenceShards = 64 }},
		{"MaxIdleConn=MaxOpenConn", func(r *RouteStoreConfig) { r.MaxIdleConn = r.MaxOpenConn }},
		{"PublishAttempts=1", func(r *RouteStoreConfig) { r.PublishAttempts = 1 }},
		{"PublishAttempts=5", func(r *RouteStoreConfig) { r.PublishAttempts = 5 }},
		{"WaitAOFNumReplicas=16", func(r *RouteStoreConfig) { r.WaitAOFNumReplicas = intPtr(16) }},
		{"WaitAOFTimeoutMs=1", func(r *RouteStoreConfig) { r.WaitAOFTimeoutMs = 1 }},
		{"WaitAOFTimeoutMs=1500", func(r *RouteStoreConfig) { r.WaitAOFTimeoutMs = 1500 }},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if err := r.Validate(); err != nil {
				t.Fatalf("%+v must be accepted: %v", r, err)
			}
		})
	}
}

// TestRouteStoreExplicitZeroWaitDisables:两项都**显式**写 0 = 不发 WAITAOF(只给没开 AOF 的环境 / miniredis 单测),
// 且 Normalize 不得把显式的 0 当成"没写"改回默认 1 —— 这正是两项用 *int 的原因。
func TestRouteStoreExplicitZeroWaitDisables(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
RouteStore:
  WaitAOFNumLocal: 0
  WaitAOFNumReplicas: 0
`)
	if c.RouteStore.WaitAOFNumLocal == nil || c.RouteStore.WaitAOFNumReplicas == nil {
		t.Fatalf("explicit zeros must be loaded as non-nil pointers, got %+v", c.RouteStore)
	}
	rs := c.RouteStore.Normalize()
	if rs.NumLocal() != 0 || rs.NumReplicas() != 0 {
		t.Fatalf("Normalize must keep explicit zeros, got numlocal=%d numreplicas=%d", rs.NumLocal(), rs.NumReplicas())
	}
	if rs.WaitsForAOF() {
		t.Fatal("WaitAOFNumLocal=0 + WaitAOFNumReplicas=0 must disable WAITAOF")
	}
	if err := rs.Validate(); err != nil {
		t.Fatalf("explicit zeros must validate: %v", err)
	}

	// 只要求副本(redis-ha 档的写法)仍然要发 WAITAOF。
	rs.WaitAOFNumReplicas = intPtr(1)
	if !rs.WaitsForAOF() {
		t.Fatal("WaitAOFNumReplicas=1 must still wait for AOF")
	}
}

// 随仓库配置采用全球 TiDB 提交点和 64 围栏分片；其余池与发布参数沿用组件默认值。
func TestShippedYamlRouteStoreIsDBFirstAndValid(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	if c.RouteStore.Mode != RouteStoreModeDBFirst {
		t.Fatalf("etc/data_service.yaml RouteStore.Mode = %q, want an explicit %q", c.RouteStore.Mode, RouteStoreModeDBFirst)
	}
	if c.RouteStore.WaitAOFNumLocal == nil || c.RouteStore.WaitAOFNumReplicas == nil {
		t.Fatal("etc/data_service.yaml must spell out both WaitAOF keys (their safe default is not the zero value)")
	}
	if c.RouteStore.FenceShards != 64 {
		t.Fatalf("etc/data_service.yaml RouteStore.FenceShards = %d, want 64 (the global TiDB tier)",
			c.RouteStore.FenceShards)
	}
	// 已单独断言全球覆盖值；下面只比较未覆盖的组件默认参数。
	rest := c.RouteStore
	rest.Mode = RouteStoreModeRedis
	rest.FenceShards = 8
	assertRouteStoreDefaults(t, rest)
	if err := c.RouteStore.Validate(); err != nil {
		t.Fatalf("shipped RouteStore block must validate: %v", err)
	}
}

// TestRouteStoreReadRequiresReady(第 3 批读方门禁,规格 §5.2):组件原始缺省 false(整段缺失、段在键不在都一样,
// Normalize 不改它);true 只许 db_first —— redis 模式下没有可回源的真源,回退顺序写反时必须拒启而不是静默跑;
// 随仓库的全球应用配置显式开启 ready 门禁。
func TestRouteStoreReadRequiresReady(t *testing.T) {
	for _, y := range []string{minimalYaml, minimalYaml + `
RouteStore: {}
`, minimalYaml + `
RouteStore:
  Mode: db_first
`} {
		if c := loadYaml(t, y); c.RouteStore.ReadRequiresReady || c.RouteStore.Normalize().ReadRequiresReady {
			t.Fatalf("ReadRequiresReady must default to false, got true for yaml:\n%s", y)
		}
	}

	c := loadYaml(t, minimalYaml+`
RouteStore:
  Mode: db_first
  ReadRequiresReady: true
`)
	rs := c.RouteStore.Normalize()
	if !rs.ReadRequiresReady {
		t.Fatal("ReadRequiresReady: true must be loaded and kept by Normalize")
	}
	if err := rs.Validate(); err != nil {
		t.Fatalf("ReadRequiresReady=true under db_first must validate: %v", err)
	}

	for _, mode := range []string{RouteStoreModeRedis, ""} {
		bad := RouteStoreConfig{Mode: mode, ReadRequiresReady: true}.Normalize()
		err := bad.Validate()
		if err == nil {
			t.Fatalf("ReadRequiresReady=true with Mode=%q (normalized %q) must be rejected", mode, bad.Mode)
		}
		if !strings.Contains(err.Error(), "ReadRequiresReady") {
			t.Fatalf("the rejection must name the key, got %v", err)
		}
	}

	var shipped Config
	if err := conf.Load("../../etc/data_service.yaml", &shipped); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	if !shipped.RouteStore.ReadRequiresReady {
		t.Fatal("etc/data_service.yaml must enable RouteStore.ReadRequiresReady for global db_first reads")
	}
}

// TestRouteStoreReadRequiresReadyNeedsReconcileLeader(A1-6 / A2-2 / M1-1 / LR-6):读方门禁开着时不许急停对账 leader
// (Reconcile.Enabled=false)—— 没有 leader 就没人撤 / 维护就绪标记,读方却继续判 Trusted。
//   - 读方开 + Enabled=false:不论 ArmReadyMarker 是否已改成 false 都拒启,报错点名两个键(结构体与 yaml 两条入口);
//   - 先关读方再急停、或 leader 开着(没写 = true / 显式 true):照常通过;
//   - 随仓库的全球默认组合(db_first + 读方门禁 + leader 开)通过;在它上面只把 Enabled 改成 false(Arm 一并改 false,
//     只留这一处违例)就被拒 —— 守卫确实罩住现役配置。
func TestRouteStoreReadRequiresReadyNeedsReconcileLeader(t *testing.T) {
	off, on := false, true

	for _, arm := range []bool{false, true} {
		bad := RouteStoreConfig{Mode: RouteStoreModeDBFirst, ReadRequiresReady: true}
		bad.Reconcile.Enabled = &off
		bad.Reconcile.ArmReadyMarker = arm
		err := bad.Normalize().Validate()
		if err == nil {
			t.Fatalf("ReadRequiresReady=true with Reconcile.Enabled=false (ArmReadyMarker=%t) must be rejected", arm)
		}
		if !arm && (!strings.Contains(err.Error(), "ReadRequiresReady") || !strings.Contains(err.Error(), "Reconcile.Enabled")) {
			t.Fatalf("the rejection must name both keys, got %v", err)
		}
	}

	c := loadYaml(t, minimalYaml+`
RouteStore:
  Mode: db_first
  ReadRequiresReady: true
  Reconcile:
    Enabled: false
`)
	if err := c.RouteStore.Normalize().Validate(); err == nil || !strings.Contains(err.Error(), "Reconcile.Enabled") {
		t.Fatalf("the same combination written in yaml must be rejected naming Reconcile.Enabled, got %v", err)
	}

	stop := RouteStoreConfig{Mode: RouteStoreModeDBFirst}
	stop.Reconcile.Enabled = &off
	if err := stop.Normalize().Validate(); err != nil {
		t.Fatalf("an emergency stop after the readers are off must validate: %v", err)
	}
	for _, enabled := range []*bool{nil, &on} {
		r := RouteStoreConfig{Mode: RouteStoreModeDBFirst, ReadRequiresReady: true}
		r.Reconcile.Enabled = enabled
		if err := r.Normalize().Validate(); err != nil {
			t.Fatalf("ReadRequiresReady=true with the leader on (Enabled=%v) must validate: %v", enabled, err)
		}
	}

	var shipped Config
	if err := conf.Load("../../etc/data_service.yaml", &shipped); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	rs := shipped.RouteStore.Normalize()
	if !rs.DBFirst() || !rs.ReadRequiresReady || !rs.Reconcile.On() {
		t.Fatalf("this case assumes the shipped global default (db_first + ReadRequiresReady + leader on), got Mode=%q ReadRequiresReady=%t Enabled=%t",
			rs.Mode, rs.ReadRequiresReady, rs.Reconcile.On())
	}
	if err := rs.Validate(); err != nil {
		t.Fatalf("the shipped global default combination must validate: %v", err)
	}
	rs.Reconcile.Enabled = &off
	rs.Reconcile.ArmReadyMarker = false
	if err := rs.Validate(); err == nil || !strings.Contains(err.Error(), "Reconcile.Enabled") {
		t.Fatalf("stopping the leader on the shipped global default must be rejected naming Reconcile.Enabled, got %v", err)
	}
}

// TestRouteStorePublishRedisPoolSize(A2-1 / LR-4):db_first 发布客户端的池上限。没配(整段缺失、段在键不在)= 缺省 20;
// [1, 128] 之外拒启(0 由 Normalize 换成缺省,所以拒绝的是负数与超上限),与 Mode 无关;边界值本身允许;
// 随仓库的 yaml 写的就是缺省值。
func TestRouteStorePublishRedisPoolSize(t *testing.T) {
	for _, y := range []string{minimalYaml, minimalYaml + `
RouteStore: {}
`} {
		if got := loadYaml(t, y).RouteStore.Normalize().PublishRedisPoolSize; got != DefaultRouteStorePublishRedisPoolSize {
			t.Fatalf("PublishRedisPoolSize must default to %d, got %d for yaml:\n%s", DefaultRouteStorePublishRedisPoolSize, got, y)
		}
	}
	for _, mode := range []string{RouteStoreModeRedis, RouteStoreModeDBFirst} {
		for _, size := range []int{-1, MaxRouteStorePublishRedisPoolSize + 1} {
			err := RouteStoreConfig{Mode: mode, PublishRedisPoolSize: size}.Normalize().Validate()
			if err == nil || !strings.Contains(err.Error(), "PublishRedisPoolSize") {
				t.Fatalf("Mode=%s PublishRedisPoolSize=%d must be rejected naming the key, got %v", mode, size, err)
			}
		}
		for _, size := range []int{1, MaxRouteStorePublishRedisPoolSize} {
			if err := (RouteStoreConfig{Mode: mode, PublishRedisPoolSize: size}).Normalize().Validate(); err != nil {
				t.Fatalf("Mode=%s PublishRedisPoolSize=%d must be accepted: %v", mode, size, err)
			}
		}
	}

	var shipped Config
	if err := conf.Load("../../etc/data_service.yaml", &shipped); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	if shipped.RouteStore.PublishRedisPoolSize != DefaultRouteStorePublishRedisPoolSize {
		t.Fatalf("etc/data_service.yaml RouteStore.PublishRedisPoolSize = %d, want the default %d spelled out",
			shipped.RouteStore.PublishRedisPoolSize, DefaultRouteStorePublishRedisPoolSize)
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

// ── Kafka 消费者旋钮(G2a 快照审计管道扩容)──────────────────────────────────

// TestKafkaConsumerKnobsDefaultToRowByRow:yaml 不写五个新键(整段 Kafka 缺失、或段在而键不在)时,
// Normalize 之后必须是 2026-10-12 之前的行为:逐条(1)、4MiB 软上限(逐条下不起作用)、不等待、并发 1、不探 lag。
// 默认值一旦漂成"批量打开",一份旧 ConfigMap 就会在没人拍板的情况下把审计落库换成另一条路径。
func TestKafkaConsumerKnobsDefaultToRowByRow(t *testing.T) {
	want := KafkaConfig{
		SnapshotBatchMaxRows:       1,
		SnapshotBatchMaxBytes:      DefaultSnapshotBatchMaxBytes,
		SnapshotFlushIntervalMs:    0,
		SnapshotMaxInflightFlushes: 1,
		LagProbeIntervalMs:         0,
	}
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{"整段 Kafka 缺失", minimalYaml},
		{"Kafka 段在、新键不在", minimalYaml + `
Kafka:
  Brokers:
    - 127.0.0.1:9092
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := loadYaml(t, tc.yaml).Kafka.Normalize()
			if got.SnapshotBatchMaxRows != want.SnapshotBatchMaxRows ||
				got.SnapshotBatchMaxBytes != want.SnapshotBatchMaxBytes ||
				got.SnapshotFlushIntervalMs != want.SnapshotFlushIntervalMs ||
				got.SnapshotMaxInflightFlushes != want.SnapshotMaxInflightFlushes ||
				got.LagProbeIntervalMs != want.LagProbeIntervalMs {
				t.Fatalf("defaults drifted: got rows=%d bytes=%d interval=%d inflight=%d lag=%d, want rows=1 bytes=%d interval=0 inflight=1 lag=0",
					got.SnapshotBatchMaxRows, got.SnapshotBatchMaxBytes, got.SnapshotFlushIntervalMs,
					got.SnapshotMaxInflightFlushes, got.LagProbeIntervalMs, DefaultSnapshotBatchMaxBytes)
			}
			// 快照池按 yaml 默认 5(缺段时 0 → 按 5 算)都必须合法。
			for _, pool := range []int{0, 5} {
				if err := got.ValidateConsumers(pool); err != nil {
					t.Fatalf("normalized defaults must validate with pool=%d: %v", pool, err)
				}
			}
		})
	}
}

// TestKafkaConsumerKnobsValidate:越界一律拒绝(拒启)而不是纠正;闭区间的边界本身允许,
// 免得有人照着报错信息改成边界值却仍被拒。并发上限跟着快照池走,池未配(0)按实际回落值 5 算。
func TestKafkaConsumerKnobsValidate(t *testing.T) {
	base := KafkaConfig{}.Normalize()
	for _, tc := range []struct {
		name   string
		pool   int
		mutate func(k *KafkaConfig)
		ok     bool
	}{
		{"rows=-1", 5, func(k *KafkaConfig) { k.SnapshotBatchMaxRows = -1 }, false},
		{"rows=257", 5, func(k *KafkaConfig) { k.SnapshotBatchMaxRows = 257 }, false},
		{"rows=256", 5, func(k *KafkaConfig) { k.SnapshotBatchMaxRows = 256 }, true},
		{"bytes=1", 5, func(k *KafkaConfig) { k.SnapshotBatchMaxBytes = 1 }, false},
		{"bytes=64MiB", 5, func(k *KafkaConfig) { k.SnapshotBatchMaxBytes = 64 << 20 }, false},
		{"bytes=64KiB", 5, func(k *KafkaConfig) { k.SnapshotBatchMaxBytes = 64 << 10 }, true},
		{"bytes=32MiB", 5, func(k *KafkaConfig) { k.SnapshotBatchMaxBytes = 32 << 20 }, true},
		{"interval=-1", 5, func(k *KafkaConfig) { k.SnapshotFlushIntervalMs = -1 }, false},
		{"interval=5001", 5, func(k *KafkaConfig) { k.SnapshotFlushIntervalMs = 5001 }, false},
		{"interval=5000", 5, func(k *KafkaConfig) { k.SnapshotFlushIntervalMs = 5000 }, true},
		{"inflight=-1", 5, func(k *KafkaConfig) { k.SnapshotMaxInflightFlushes = -1 }, false},
		{"inflight=5 @pool5", 5, func(k *KafkaConfig) { k.SnapshotMaxInflightFlushes = 5 }, false},
		{"inflight=4 @pool5", 5, func(k *KafkaConfig) { k.SnapshotMaxInflightFlushes = 4 }, true},
		{"inflight=1 @pool1", 1, func(k *KafkaConfig) { k.SnapshotMaxInflightFlushes = 1 }, true},
		{"inflight=2 @pool1", 1, func(k *KafkaConfig) { k.SnapshotMaxInflightFlushes = 2 }, false},
		{"inflight=4 @pool0(按 5)", 0, func(k *KafkaConfig) { k.SnapshotMaxInflightFlushes = 4 }, true},
		{"inflight=5 @pool0(按 5)", 0, func(k *KafkaConfig) { k.SnapshotMaxInflightFlushes = 5 }, false},
		{"lag=1", 5, func(k *KafkaConfig) { k.LagProbeIntervalMs = 1 }, false},
		{"lag=4999", 5, func(k *KafkaConfig) { k.LagProbeIntervalMs = 4999 }, false},
		{"lag=600001", 5, func(k *KafkaConfig) { k.LagProbeIntervalMs = 600001 }, false},
		{"lag=-1", 5, func(k *KafkaConfig) { k.LagProbeIntervalMs = -1 }, false},
		{"lag=0(关)", 5, func(k *KafkaConfig) { k.LagProbeIntervalMs = 0 }, true},
		{"lag=30000", 5, func(k *KafkaConfig) { k.LagProbeIntervalMs = 30000 }, true},
		{"lag=600000", 5, func(k *KafkaConfig) { k.LagProbeIntervalMs = 600000 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := base
			tc.mutate(&k)
			err := k.ValidateConsumers(tc.pool)
			if tc.ok && err != nil {
				t.Fatalf("%+v must be accepted with pool=%d: %v", k, tc.pool, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("%+v must be rejected with pool=%d", k, tc.pool)
			}
		})
	}

	// Normalize 不纠正负数:负数留给 ValidateConsumers 拒绝,而不是被悄悄改成默认值。
	neg := KafkaConfig{SnapshotBatchMaxRows: -3, SnapshotMaxInflightFlushes: -1}.Normalize()
	if neg.SnapshotBatchMaxRows != -3 || neg.SnapshotMaxInflightFlushes != -1 {
		t.Fatalf("Normalize must not correct negative values, got %+v", neg)
	}
}

// TestShippedYamlKafkaConsumerKnobsAreRowByRowAndValid:仓库里那份 yaml 显式写出五个键,值与代码默认一致(逐条、并发 1),
// 只有 lag 探针打开(30000ms,规格 D-7:只读 broker 元数据,没有它积压 / 保留期告警全哑)。把批量打开是 Q-7 的显式决定,
// 前置是压测对比表(AGENTS §6),不能随仓库配置悄悄带出去。
func TestShippedYamlKafkaConsumerKnobsAreRowByRowAndValid(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	k := c.Kafka
	if k.SnapshotBatchMaxRows != 1 || k.SnapshotBatchMaxBytes != DefaultSnapshotBatchMaxBytes ||
		k.SnapshotFlushIntervalMs != 0 || k.SnapshotMaxInflightFlushes != 1 {
		t.Fatalf("etc/data_service.yaml must spell out the row-by-row defaults (1 / %d / 0 / 1), got %d / %d / %d / %d",
			DefaultSnapshotBatchMaxBytes, k.SnapshotBatchMaxRows, k.SnapshotBatchMaxBytes, k.SnapshotFlushIntervalMs, k.SnapshotMaxInflightFlushes)
	}
	if k.LagProbeIntervalMs != 30000 {
		t.Fatalf("etc/data_service.yaml LagProbeIntervalMs = %d, want 30000", k.LagProbeIntervalMs)
	}
	if err := k.Normalize().ValidateConsumers(c.SnapshotMySQL.MaxOpenConn); err != nil {
		t.Fatalf("shipped Kafka consumer knobs must validate: %v", err)
	}
}

// TestValidateRegions:Regions 生效时(没配 DevRedis)zone → 集群表必须一对一、地址与 zone 都不能为空。
// 同一 zone 出现在两个 Region 时,旧实现后写覆盖先写、debug_fetch 取先写,两边报的集群不一样。
func TestValidateRegions(t *testing.T) {
	region := func(id uint32, zones ...uint32) RegionConfig {
		return RegionConfig{Id: id, Zones: zones, Redis: RedisClusterConfig{Addrs: []string{"10.0.0.1:6379"}}}
	}
	if err := ValidateRegions([]RegionConfig{region(1, 1, 2), region(2, 3)}); err != nil {
		t.Fatalf("disjoint regions must validate: %v", err)
	}

	noAddrs := region(1, 1)
	noAddrs.Redis.Addrs = nil
	for name, tc := range map[string]struct {
		regions []RegionConfig
		want    string
	}{
		"empty":          {nil, "Regions is empty"},
		"no addrs":       {[]RegionConfig{noAddrs}, "Redis.Addrs is empty"},
		"no zones":       {[]RegionConfig{region(1)}, "Zones is empty"},
		"duplicate zone": {[]RegionConfig{region(1, 1, 2), region(2, 2)}, "zone 2 is listed in both"},
		"dup in one":     {[]RegionConfig{region(1, 5, 5)}, "zone 5 is listed in both"},
	} {
		err := ValidateRegions(tc.regions)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
}

// ── RouteStore.Reconcile(对账 leader,第 1b-2 批;规格 spec_D1_batch1b2_2_3.md §4.5、scout 1b-2 config 一项)──────

// assertReconcileDefaults 断言 got 是 Normalize 之后的默认形态。
func assertReconcileDefaults(t *testing.T, got RouteReconcileConfig) {
	t.Helper()
	if got.Enabled == nil || !*got.Enabled || got.ArmReadyMarker || got.AssumeNoEviction {
		t.Fatalf("Reconcile switches = Enabled %v ArmReadyMarker %t AssumeNoEviction %t, want true / false / false",
			got.Enabled, got.ArmReadyMarker, got.AssumeNoEviction)
	}
	if got.RollbackToleranceMs == nil || *got.RollbackToleranceMs != 0 || got.ToleranceMs() != 0 {
		t.Fatalf("RollbackToleranceMs = %v, want an explicit 0 (owner decision 10-17)", got.RollbackToleranceMs)
	}
	want := map[string][2]int{
		"LeaseMs":                  {got.LeaseMs, 10000},
		"RenewIntervalMs":          {got.RenewIntervalMs, 3000},
		"TickIntervalMs":           {got.TickIntervalMs, 1000},
		"IncrementalIntervalMs":    {got.IncrementalIntervalMs, 30000},
		"IncrementalOverlapMs":     {got.IncrementalOverlapMs, 15000},
		"RepairLookbackMs":         {got.RepairLookbackMs, 60000},
		"PushBatch":                {got.PushBatch, 500},
		"PushMaxRowsPerSec":        {got.PushMaxRowsPerSec, 20000},
		"RedisPoolSize":            {got.RedisPoolSize, 4},
		"FreezeTimeoutMs":          {got.FreezeTimeoutMs, 5000},
		"RedisFailureReleaseMs":    {got.RedisFailureReleaseMs, 10000},
		"ArmingBlockedReportLimit": {got.ArmingBlockedReportLimit, 1000},
		"LeaseSafetyMs":            {got.LeaseSafetyMs(), 2000},
	}
	for name, v := range want {
		if v[0] != v[1] {
			t.Errorf("Reconcile.%s = %d, want %d", name, v[0], v[1])
		}
	}
}

// TestRouteReconcileDefaults:整段缺失、`Reconcile: {}`、只写 Mode 时都得到默认值且合法;leader 只在 db_first 下构造
// (ReconcileEnabled),RedisFailureReleaseMs 缺省跟着 LeaseMs 走。
func TestRouteReconcileDefaults(t *testing.T) {
	for _, y := range []string{minimalYaml, minimalYaml + `
RouteStore: {}
`, minimalYaml + `
RouteStore:
  Reconcile: {}
`} {
		c := loadYaml(t, y)
		if c.RouteStore.Reconcile.Enabled != nil || c.RouteStore.Reconcile.LeaseMs != 0 {
			t.Fatalf("an absent Reconcile block must stay zero-valued, got %+v", c.RouteStore.Reconcile)
		}
		rs := c.RouteStore.Normalize()
		assertReconcileDefaults(t, rs.Reconcile)
		if err := rs.Validate(); err != nil {
			t.Fatalf("normalized defaults must validate: %v", err)
		}
		if rs.ReconcileEnabled() {
			t.Fatal("redis mode must not construct the reconcile leader")
		}
		rs.Mode = RouteStoreModeDBFirst
		if !rs.ReconcileEnabled() {
			t.Fatal("db_first with Reconcile.Enabled unset must construct the reconcile leader (nil = true)")
		}
	}

	// RedisFailureReleaseMs 没写时跟着 LeaseMs 走,写了就按写的。
	rc := RouteReconcileConfig{LeaseMs: 20000}.Normalize()
	if rc.RedisFailureReleaseMs != 20000 {
		t.Fatalf("RedisFailureReleaseMs must default to LeaseMs, got %d", rc.RedisFailureReleaseMs)
	}
	rc = RouteReconcileConfig{LeaseMs: 20000, RedisFailureReleaseMs: 3000}.Normalize()
	if rc.RedisFailureReleaseMs != 3000 {
		t.Fatalf("an explicit RedisFailureReleaseMs must be kept, got %d", rc.RedisFailureReleaseMs)
	}

	// 急停:Enabled=false 在 db_first 下也不构造。
	c := loadYaml(t, minimalYaml+`
RouteStore:
  Mode: db_first
  Reconcile:
    Enabled: false
`)
	rs := c.RouteStore.Normalize()
	if rs.Reconcile.On() || rs.ReconcileEnabled() {
		t.Fatal("Reconcile.Enabled: false must be honoured (emergency stop)")
	}
	if err := rs.Validate(); err != nil {
		t.Fatalf("db_first + Enabled=false must validate: %v", err)
	}
}

// TestRouteReconcileRejects:每条范围 / 跨字段规则各一格,非法值不论 Mode 都拒启;ArmReadyMarker=true 只许 db_first 且 Enabled≠false。
func TestRouteReconcileRejects(t *testing.T) {
	base := RouteStoreConfig{}.Normalize()
	if err := base.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	off := false
	cases := []struct {
		name   string
		mutate func(r *RouteReconcileConfig)
	}{
		{"LeaseMs 低于 3000", func(r *RouteReconcileConfig) { r.LeaseMs = 2999 }},
		{"LeaseMs 超过 60000", func(r *RouteReconcileConfig) { r.LeaseMs = 60001 }},
		{"LeaseMs 8000 撑不住默认冻结时限", func(r *RouteReconcileConfig) { r.LeaseMs = 8000 }},
		{"TickIntervalMs 低于 200", func(r *RouteReconcileConfig) { r.TickIntervalMs = 199 }},
		{"TickIntervalMs 超过 10000", func(r *RouteReconcileConfig) { r.TickIntervalMs = 10001 }},
		{"RenewIntervalMs 太大", func(r *RouteReconcileConfig) { r.RenewIntervalMs = 4001 }},
		{"RenewIntervalMs 小于拍间隔", func(r *RouteReconcileConfig) { r.RenewIntervalMs = 900 }},
		{"IncrementalIntervalMs 低于 1000", func(r *RouteReconcileConfig) { r.IncrementalIntervalMs = 999 }},
		{"IncrementalOverlapMs 低于 5000", func(r *RouteReconcileConfig) { r.IncrementalOverlapMs = 4999 }},
		{"RepairLookbackMs 低于 15000", func(r *RouteReconcileConfig) { r.RepairLookbackMs = 14999 }},
		{"RepairLookbackMs 小于重叠量", func(r *RouteReconcileConfig) { r.IncrementalOverlapMs, r.RepairLookbackMs = 20000, 15000 }},
		{"RollbackToleranceMs 为负", func(r *RouteReconcileConfig) { r.RollbackToleranceMs = intPtr(-1) }},
		{"RollbackToleranceMs 超过 60000", func(r *RouteReconcileConfig) { r.RollbackToleranceMs = intPtr(60001) }},
		{"PushBatch 为负", func(r *RouteReconcileConfig) { r.PushBatch = -1 }},
		{"PushBatch 超过 1000", func(r *RouteReconcileConfig) { r.PushBatch = 1001 }},
		{"PushMaxRowsPerSec 低于 100", func(r *RouteReconcileConfig) { r.PushMaxRowsPerSec = 99 }},
		{"RedisPoolSize 超过 32", func(r *RouteReconcileConfig) { r.RedisPoolSize = 33 }},
		{"FreezeTimeoutMs 低于 5000", func(r *RouteReconcileConfig) { r.FreezeTimeoutMs = 4999 }},
		{"FreezeTimeoutMs 超过 60000", func(r *RouteReconcileConfig) { r.LeaseMs, r.FreezeTimeoutMs = 60000, 60001 }},
		{"RedisFailureReleaseMs 小于拍间隔", func(r *RouteReconcileConfig) { r.RedisFailureReleaseMs = 100 }},
		{"RedisFailureReleaseMs 超过 600000", func(r *RouteReconcileConfig) { r.RedisFailureReleaseMs = 600001 }},
		{"ArmingBlockedReportLimit 为负", func(r *RouteReconcileConfig) { r.ArmingBlockedReportLimit = -1 }},
		{"ArmingBlockedReportLimit 超过 100000", func(r *RouteReconcileConfig) { r.ArmingBlockedReportLimit = 100001 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{RouteStoreModeRedis, RouteStoreModeDBFirst} {
				r := base
				r.Mode = mode
				tc.mutate(&r.Reconcile)
				err := r.Validate()
				if err == nil {
					t.Fatalf("Mode=%s Reconcile=%+v must be rejected", mode, r.Reconcile)
				}
				if !strings.Contains(err.Error(), "RouteStore.Reconcile.") {
					t.Fatalf("the rejection must name the key, got %v", err)
				}
			}
		})
	}

	// ArmReadyMarker=true:redis 模式、或 db_first 但 Enabled=false,一律拒绝(回退时先改 Arm=false)。
	for _, tc := range []struct {
		name    string
		mode    string
		enabled *bool
	}{
		{"redis 模式", RouteStoreModeRedis, nil},
		{"db_first 但急停", RouteStoreModeDBFirst, &off},
	} {
		r := base
		r.Mode = tc.mode
		r.Reconcile.ArmReadyMarker = true
		r.Reconcile.Enabled = tc.enabled
		if err := r.Validate(); err == nil || !strings.Contains(err.Error(), "ArmReadyMarker") {
			t.Fatalf("%s: ArmReadyMarker=true must be rejected naming the key, got %v", tc.name, err)
		}
	}

	accepted := []struct {
		name   string
		mutate func(r *RouteStoreConfig)
	}{
		{"db_first + ArmReadyMarker", func(r *RouteStoreConfig) { r.Mode, r.Reconcile.ArmReadyMarker = RouteStoreModeDBFirst, true }},
		{"redis + Enabled=false", func(r *RouteStoreConfig) { r.Reconcile.Enabled = &off }},
		{"RollbackToleranceMs=60000", func(r *RouteStoreConfig) { r.Reconcile.RollbackToleranceMs = intPtr(60000) }},
		{"PushBatch=1000", func(r *RouteStoreConfig) { r.Reconcile.PushBatch = 1000 }},
		{"RenewIntervalMs=4000", func(r *RouteStoreConfig) { r.Reconcile.RenewIntervalMs = 4000 }},
		{"LeaseMs=60000", func(r *RouteStoreConfig) { r.Reconcile.LeaseMs = 60000 }},
		{"FreezeTimeoutMs=6000", func(r *RouteStoreConfig) { r.Reconcile.FreezeTimeoutMs = 6000 }},
		{"RedisFailureReleaseMs=TickIntervalMs", func(r *RouteStoreConfig) { r.Reconcile.RedisFailureReleaseMs = r.Reconcile.TickIntervalMs }},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if err := r.Validate(); err != nil {
				t.Fatalf("%+v must be accepted: %v", r.Reconcile, err)
			}
		})
	}
}

// 全球配置运行对账并开启 ready 装配；实际发布仍须通过 leader 的历史导入与回档检查。
func TestShippedYamlReconcileEnablesGlobalReady(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	rc := c.RouteStore.Reconcile
	if !rc.ArmReadyMarker {
		t.Fatal("etc/data_service.yaml must enable RouteStore.Reconcile.ArmReadyMarker for global readiness")
	}
	if rc.Enabled == nil || rc.RollbackToleranceMs == nil {
		t.Fatal("etc/data_service.yaml must spell out Reconcile.Enabled and Reconcile.RollbackToleranceMs (their safe default is not the zero value)")
	}
	if !c.RouteStore.Normalize().ReconcileEnabled() {
		t.Fatal("the shipped global db_first yaml must construct the reconcile leader")
	}
	// Arm 已单独断言，其他对账参数继续保持组件默认值。
	rest := rc.Normalize()
	rest.ArmReadyMarker = false
	assertReconcileDefaults(t, rest)
	if err := c.RouteStore.Normalize().Validate(); err != nil {
		t.Fatalf("shipped RouteStore block (with Reconcile) must validate: %v", err)
	}
}
