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
NameCacheRedis:
  Host: 127.0.0.1:6379
  Type: node
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

// 块缺失就是生产安全值:不自动初始化,而且没有配两套 etcd 就发不了号。
func TestIdSegmentDefaultsWhenBlockAbsent(t *testing.T) {
	c := loadYaml(t, minimalYaml)
	if c.IdSegment.AllowAutoSeed {
		t.Fatal("an absent IdSegment block must NOT allow auto init (production semantics)")
	}
	if got, want := c.IdSegment.EffectiveTags(), DefaultIdSegmentTags; !equalStrings(got, want) {
		t.Fatalf("tags without a block = %v, want the default set %v", got, want)
	}
	if _, _, err := c.IdSegment.IssuerEndpoints([]string{"etcd:2379"}); err == nil {
		t.Fatal("production must not fall back to the service's own etcd")
	}
}

// 只写 `IdSegment:` 却不写键,语义与缺失一致。
func TestIdSegmentBlockPresentWithoutKeys(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
IdSegment: {}
`)
	if c.IdSegment.AllowAutoSeed {
		t.Fatal("an empty IdSegment block must keep AllowAutoSeed=false")
	}
	if got, want := c.IdSegment.EffectiveTags(), DefaultIdSegmentTags; !equalStrings(got, want) {
		t.Fatalf("tags with an empty block = %v, want %v", got, want)
	}
}

// 显式清单覆盖默认;返回的是副本,改它不能污染默认清单。
func TestIdSegmentTagsOverride(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
IdSegment:
  Tags: [player, pet]
`)
	got := c.IdSegment.EffectiveTags()
	if !equalStrings(got, []string{"player", "pet"}) {
		t.Fatalf("tags = %v, want [player pet]", got)
	}
	defaults := IdSegmentConfig{}.EffectiveTags()
	defaults[0] = "mutated"
	if DefaultIdSegmentTags[0] != "player" {
		t.Fatal("EffectiveTags must return a copy, not the package-level default slice")
	}
}

func TestIdSegmentIssuerBlockLoaded(t *testing.T) {
	c := loadYaml(t, minimalYaml+`
IdSegment:
  Issuer:
    Region: 5
    PrimaryEtcd: ["a:2379", "b:2379"]
    SecondaryEtcd: ["c:2379"]
`)
	if c.IdSegment.Issuer.Region != 5 {
		t.Fatalf("Region = %d", c.IdSegment.Issuer.Region)
	}
	primary, secondary, err := c.IdSegment.IssuerEndpoints(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(primary, []string{"a:2379", "b:2379"}) || !equalStrings(secondary, []string{"c:2379"}) {
		t.Fatalf("primary=%v secondary=%v", primary, secondary)
	}
}

func TestIdSegmentIssuerEndpoints(t *testing.T) {
	own := []string{"own:2379"}
	cases := []struct {
		name          string
		cfg           IdSegmentConfig
		wantPrimary   []string
		wantSecondary []string
		wantErr       bool
	}{
		{name: "production needs both", wantErr: true},
		{name: "production missing secondary", cfg: IdSegmentConfig{Issuer: IdIssuerConfig{PrimaryEtcd: []string{"a:2379"}}}, wantErr: true},
		{name: "production missing primary", cfg: IdSegmentConfig{Issuer: IdIssuerConfig{SecondaryEtcd: []string{"a:2379"}}}, wantErr: true},
		{
			name:    "production shared endpoint",
			cfg:     IdSegmentConfig{Issuer: IdIssuerConfig{PrimaryEtcd: []string{"a:2379", "b:2379"}, SecondaryEtcd: []string{"c:2379", " B:2379 "}}},
			wantErr: true,
		},
		{
			name:          "production independent clusters",
			cfg:           IdSegmentConfig{Issuer: IdIssuerConfig{PrimaryEtcd: []string{"a:2379"}, SecondaryEtcd: []string{"b:2379"}}},
			wantPrimary:   []string{"a:2379"},
			wantSecondary: []string{"b:2379"},
		},
		{name: "dev falls back to own etcd", cfg: IdSegmentConfig{AllowAutoSeed: true}, wantPrimary: own, wantSecondary: own},
		{
			name:          "dev may share one cluster",
			cfg:           IdSegmentConfig{AllowAutoSeed: true, Issuer: IdIssuerConfig{PrimaryEtcd: []string{"a:2379"}}},
			wantPrimary:   []string{"a:2379"},
			wantSecondary: []string{"a:2379"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			primary, secondary, err := tc.cfg.IssuerEndpoints(own)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !equalStrings(primary, tc.wantPrimary) || !equalStrings(secondary, tc.wantSecondary) {
				t.Fatalf("primary=%v secondary=%v", primary, secondary)
			}
		})
	}
	if _, _, err := (IdSegmentConfig{AllowAutoSeed: true}).IssuerEndpoints(nil); err == nil {
		t.Fatal("dev without any etcd must be rejected")
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

// ── AdminToken(运维 RPC 鉴权:Rollback* 与 ReleasePlayerName)────────────

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
// 这条不是形式主义:go-zero 对多余的键一律静默忽略,同一段里写错键名只有读回来才看得见。
// 顺带钉住 AdminToken 在这份 dev 配置里保持关闭 —— 本地栈不该默认开着运维 RPC
// (今天是 Rollback* 与 ReleasePlayerName 的运维路径;remap 已停用,与 token 无关)。
func TestShippedYamlLoads(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	if c.NameCacheRedis.Host == "" {
		t.Fatal("etc/data_service.yaml: NameCacheRedis.Host 不能为空")
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

// ── RouteStore(player_route 连接池与合服映射缓存,docs/design/player-route-mapping-durability-proposal.md §15)──────────

// assertRouteStoreDefaults 断言 got 是 Normalize 之后的默认形态(20/5、合服映射 5 秒)。
func assertRouteStoreDefaults(t *testing.T, got RouteStoreConfig) {
	t.Helper()
	if got.MaxOpenConn != 20 || got.MaxIdleConn != 5 || got.MergedZoneRefreshSec != 5 {
		t.Fatalf("RouteStore = %+v, want 20,5 / 5", got)
	}
}

// TestRouteStoreDefaultsWhenBlockAbsent:yaml 里没有 `RouteStore:` 段时规范化之后的默认值合法。
// go-zero 不下钻未出现的 optional 结构,所以默认值只能由 Normalize 给。
func TestRouteStoreDefaultsWhenBlockAbsent(t *testing.T) {
	c := loadYaml(t, minimalYaml)
	if c.RouteStore != (RouteStoreConfig{}) {
		t.Fatalf("an absent RouteStore block must stay zero-valued, got %+v", c.RouteStore)
	}
	rs := c.RouteStore.Normalize()
	assertRouteStoreDefaults(t, rs)
	if err := rs.Validate(); err != nil {
		t.Fatalf("normalized defaults must validate: %v", err)
	}

	// 只写 `RouteStore:` 不写键,语义与缺失一致。
	c = loadYaml(t, minimalYaml+`
RouteStore: {}
`)
	assertRouteStoreDefaults(t, c.RouteStore.Normalize())
}

// TestRouteStoreValidateRejects:配错**拒绝**而不是纠正;闭区间的边界本身是允许的,免得有人照着报错信息改成边界值却仍被拒。
func TestRouteStoreValidateRejects(t *testing.T) {
	base := RouteStoreConfig{}.Normalize()
	if err := base.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(r *RouteStoreConfig)
	}{
		{"MergedZoneRefreshSec 为负", func(r *RouteStoreConfig) { r.MergedZoneRefreshSec = -1 }},
		{"MergedZoneRefreshSec 超过 60", func(r *RouteStoreConfig) { r.MergedZoneRefreshSec = 61 }},
		{"MaxOpenConn 为负", func(r *RouteStoreConfig) { r.MaxOpenConn = -1 }},
		{"MaxIdleConn 为负", func(r *RouteStoreConfig) { r.MaxIdleConn = -1 }},
		{"MaxIdleConn 超过 MaxOpenConn", func(r *RouteStoreConfig) { r.MaxIdleConn = r.MaxOpenConn + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("%+v must be rejected", r)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		mutate func(r *RouteStoreConfig)
	}{
		{"MergedZoneRefreshSec=1", func(r *RouteStoreConfig) { r.MergedZoneRefreshSec = 1 }},
		{"MergedZoneRefreshSec=60", func(r *RouteStoreConfig) { r.MergedZoneRefreshSec = 60 }},
		{"MaxIdleConn=MaxOpenConn", func(r *RouteStoreConfig) { r.MaxIdleConn = r.MaxOpenConn }},
	} {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if err := r.Validate(); err != nil {
				t.Fatalf("%+v must be accepted: %v", r, err)
			}
		})
	}
}

// 随仓库配置写的就是组件默认值,这一段不偷偷改参数。
func TestShippedYamlRouteStoreMatchesDefaults(t *testing.T) {
	var c Config
	if err := conf.Load("../../etc/data_service.yaml", &c); err != nil {
		t.Fatalf("load etc/data_service.yaml: %v", err)
	}
	assertRouteStoreDefaults(t, c.RouteStore)
	if err := c.RouteStore.Validate(); err != nil {
		t.Fatalf("shipped RouteStore block must validate: %v", err)
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
