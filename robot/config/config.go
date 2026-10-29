package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"robot/logic/ai"
	"shared/generated/table"
)

// Config holds the robot load-test configuration.
type Config struct {
	GatewayAddr    string   `yaml:"gateway_addr"`    // e.g. "http://127.0.0.1:8081" (Gateway HTTP API, for /api/assign-gate)
	ZoneID         uint32   `yaml:"zone_id"`         // target zone (0 = auto-select from server-list)
	RobotCount     int      `yaml:"robot_count"`     // number of concurrent robots
	AccountFmt     string   `yaml:"account_fmt"`     // printf pattern, e.g. "robot_%04d"
	Password       string   `yaml:"password"`        // shared password for all robots
	SkillIDs       []uint32 `yaml:"skill_ids"`       // skill_table IDs from Skill.xlsx to cycle (empty = auto-read from class table)
	ActionInterval int      `yaml:"action_interval"` // seconds between AI actions
	ReportInterval int      `yaml:"report_interval"` // seconds between stats reports
	TableDir       string   `yaml:"table_dir"`       // path to generated/tables dir for reading Skill/Class tables

	// Mode: "stress" (default) — mass concurrent bots; "login-test" — scenario
	// suite with login/skill/scene tests; "data-stress" — repeated
	// login → play → logout cycles with Redis-published expectations for the
	// db-side verifier (see DataStress block); "currency-crash-snapshot" —
	// 单次登录快照(见 CurrencyCrash 块); "battle-smoke" — 双机器人
	// 回合制战斗 + 观战端到端冒烟(见 battle_smoke_scenario.go;子模式
	// 见 BattleSmoke 块); "travel-smoke" — 单机器人跨 zone 场景传送往返
	// 冒烟(见 travel_smoke_scenario.go / TravelSmoke 块); "channel-smoke" —
	// 单机器人玩家主动切线冒烟(见 channel_smoke_scenario.go / ChannelSmoke 块);
	// "sync-smoke" — 落地恢复的按域版本摘要冒烟(见 sync_smoke_scenario.go / SyncSmoke 块)；
	// "asset-arrival-prepare" — 本机专用角色的正式 RPC 资产准备与有界保活。
	// "sync-smoke" — 落地恢复的按域版本摘要冒烟(见 sync_smoke_scenario.go / SyncSmoke 块);
	// "merchant-smoke" — 单机器人神秘商人冒烟(见 mystery_merchant_smoke_scenario.go / MerchantSmoke 块);
	// "equip-forge-topup" — 给一个开发账号补强化石 / 宝石 / 银两,好在真实客户端里点强化与镶嵌(见 EquipForgeTopup 块)。
	Mode string `yaml:"mode"`

	// FeaturesSmoke only uses an explicitly selected existing account/character.
	FeaturesSmoke FeaturesSmokeConfig `yaml:"features_smoke"`

	// AssetArrivalPrepare 只用于本机专用角色的正式 RPC 资产准备与有界保活。
	AssetArrivalPrepare AssetArrivalPrepareConfig `yaml:"asset_arrival_prepare"`

	// DataStress configures the data-consistency stress mode.
	DataStress DataStressConfig `yaml:"data_stress"`

	// BattleSmoke 是 "battle-smoke" 模式的子开关(见 BattleSmokeConfig)。
	// 缺省(cross_zone=false)跑原 PVE + 观战流程。
	BattleSmoke BattleSmokeConfig `yaml:"battle_smoke"`

	// ChatSmoke 是 "chat-smoke" 模式的子开关(见 ChatSmokeConfig / chat_smoke_scenario.go)。
	// 缺省(cross_zone=false)两个机器人都登 zone_id,做同 zone 弱验收。
	ChatSmoke ChatSmokeConfig `yaml:"chat_smoke"`

	// GuildSmoke 是 "guild-smoke" 模式的子开关(见 GuildSmokeConfig / guild_smoke_scenario.go)。
	GuildSmoke GuildSmokeConfig `yaml:"guild_smoke"`

	// TradeSmoke 是 "trade-smoke" 模式的子开关(见 TradeSmokeConfig / trade_smoke_scenario.go)。
	TradeSmoke TradeSmokeConfig `yaml:"trade_smoke"`

	// FriendSmoke 是 "friend-smoke" 模式的子开关(见 FriendSmokeConfig / friend_smoke_scenario.go)。
	FriendSmoke FriendSmokeConfig `yaml:"friend_smoke"`

	// MailSmoke 是 "mail-smoke" 模式的子开关(见 MailSmokeConfig / mail_smoke_scenario.go)。
	MailSmoke MailSmokeConfig `yaml:"mail_smoke"`

	// RankSmoke 是 "rank-smoke" 模式的子开关(见 RankSmokeConfig / rank_smoke_scenario.go)。
	RankSmoke RankSmokeConfig `yaml:"rank_smoke"`

	// TeamSmoke 是 "team-smoke" 模式的子开关(见 TeamSmokeConfig / team_smoke_scenario.go)。
	TeamSmoke TeamSmokeConfig `yaml:"team_smoke"`

	// TeamFollowSmoke 是 "team-follow-smoke" 模式的子开关(见 TeamFollowSmokeConfig / team_follow_smoke_scenario.go)。
	TeamFollowSmoke TeamFollowSmokeConfig `yaml:"team_follow_smoke"`

	// TeamBackfillSmoke 是 "team-backfill-smoke" 模式的子开关(见 TeamBackfillSmokeConfig / team_backfill_smoke_scenario.go)。
	TeamBackfillSmoke TeamBackfillSmokeConfig `yaml:"team_backfill_smoke"`

	// TravelSmoke 是 "travel-smoke" 模式的子开关(见 TravelSmokeConfig / travel_smoke_scenario.go)。
	TravelSmoke TravelSmokeConfig `yaml:"travel_smoke"`

	// ChannelSmoke 是 "channel-smoke" 模式的子开关(见 ChannelSmokeConfig / channel_smoke_scenario.go)。
	// 整块可缺省。
	ChannelSmoke ChannelSmokeConfig `yaml:"channel_smoke"`

	// SyncSmoke 是 "sync-smoke" 模式的子开关(见 SyncSmokeConfig / sync_smoke_scenario.go)。
	// 整块可缺省:缺省不做跨 zone 腿。
	SyncSmoke SyncSmokeConfig `yaml:"sync_smoke"`

	// MerchantSmoke 是 "merchant-smoke" 模式的子开关(见 MerchantSmokeConfig / mystery_merchant_smoke_scenario.go)。
	// 整块可缺省。
	MerchantSmoke MerchantSmokeConfig `yaml:"merchant_smoke"`

	// MallArrivalSmoke 是 "mall-arrival-smoke" 模式的子开关(见 MallArrivalSmokeConfig / mall_arrival_smoke_scenario.go)。
	MallArrivalSmoke MallArrivalSmokeConfig `yaml:"mall_arrival_smoke"`

	// EquipForgeTopup 是 "equip-forge-topup" 模式的子开关(见 EquipForgeTopupConfig / equip_forge_topup_scenario.go)。
	EquipForgeTopup EquipForgeTopupConfig `yaml:"equip_forge_topup"`

	// CurrencyCrash configures the "currency-crash-snapshot" mode used by
	// docs/notes/currency-crash-window-verification.md. Driven by an external
	// PowerShell script that runs the robot twice per case (pre/post kill) so
	// only one of the two legs needs to actually mutate the balance.
	CurrencyCrash CurrencyCrashConfig `yaml:"currency_crash"`

	// Auth type for login: "" or "password" (default), "satoken" (requires satoken_addr).
	AuthType    string `yaml:"auth_type"`
	SaTokenAddr string `yaml:"satoken_addr"` // SA-Token dev-login endpoint, e.g. "http://127.0.0.1:18080"

	// UseHttpLogin (default false) routes the *initial* login RPC through
	// the Java Gateway's POST /api/login instead of the legacy in-game
	// ClientPlayerLogin.Login over the gate TCP channel. The flow becomes:
	//
	//   POST /api/login          → access/refresh token, player list
	//   POST /api/assign-gate    → gate ip + HMAC token
	//   TCP connect gate         → ClientTokenVerify (HMAC, local)
	//   Login(auth_type=access_token) on gate TCP   ← lightweight, only to bind session
	//   EnterGame                → world
	//
	// Set true to validate the new path against the gateway. Off by default
	// so existing stress runs / regression suites are not affected.
	UseHttpLogin bool `yaml:"use_http_login"`

	// AI behavior profile: "stress", "behavioral", "fighter", "explorer", "chatter"
	// Or set custom_weights to define your own action distribution.
	Profile       string         `yaml:"profile"`
	CustomWeights map[string]int `yaml:"custom_weights"` // e.g. {cast_skill: 50, move: 30, idle: 15, chat: 5}

	// LLM settings (optional). When enabled, LLM decides actions instead of profile weights.
	// Best for 1-10 robots to model realistic player behavior. Not for mass load testing.
	LLM LLMConfig `yaml:"llm"`
}

// FeaturesSmokeConfig is opt-in: omitted write IDs and SortBag perform reads only.
// Account must be explicit; no guessed account, account creation or GM progression.
type FeaturesSmokeConfig struct {
	Account         string `yaml:"account"`
	PlayerID        uint64 `yaml:"player_id"`
	BagType         uint32 `yaml:"bag_type"`
	SortBag         bool   `yaml:"sort_bag"`
	Scope           uint32 `yaml:"scope"`
	AcceptMissionID uint32 `yaml:"accept_mission_id"`
	ClaimMissionID  uint32 `yaml:"claim_mission_id"`
	VerifyRelogin   bool   `yaml:"verify_relogin"`
	BattleConfigID  uint32 `yaml:"battle_config_id"` // 0=跳过；1=显式单人任务战斗验收
}

// AssetArrivalPrepareConfig 没有默认账号或无限保活；买家可显式关闭装备准备。
// GoldTarget 是准备完成后的最低金币余额，装备固定为一件 1101，不开放任意 GM 参数。
type AssetArrivalPrepareConfig struct {
	RunMode     string `yaml:"run_mode"`
	Dedicated   bool   `yaml:"dedicated"`
	Account     string `yaml:"account"`
	GoldTarget  uint64 `yaml:"gold_target"`
	Equipment   bool   `yaml:"equipment"`
	HoldSeconds int    `yaml:"hold_seconds"`
	OutputPath  string `yaml:"output_path"`
}

// ValidateAssetArrivalPrepare 必须在网络登录、建角和 GM 副作用前完成。
// 本机 dev 声明不替代 gate/scene 自身默认拒绝的 GM 权限闸。
func (c *Config) ValidateAssetArrivalPrepare() error {
	p := c.AssetArrivalPrepare
	if c.Mode != "asset-arrival-prepare" || p.RunMode != "dev" || !p.Dedicated {
		return fmt.Errorf("资产准备仅允许显式本机 dev 模式和专用角色")
	}
	if c.RobotCount != 1 || c.ZoneID == 0 || c.ZoneID > 999999 {
		return fmt.Errorf("资产准备要求单角色和显式 zone_id（1..999999）")
	}
	const prefix = "robot_assetarrival_"
	suffix := strings.TrimPrefix(p.Account, prefix)
	if !strings.HasPrefix(p.Account, prefix) || len(suffix) == 0 || len(suffix) > 48 {
		return fmt.Errorf("资产准备账号必须使用 robot_assetarrival_ 专用前缀和 1..48 位小写后缀")
	}
	for _, character := range suffix {
		if character != '_' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return fmt.Errorf("资产准备账号后缀只允许小写字母、数字和下划线")
		}
	}
	endpoint, err := url.Parse(c.GatewayAddr)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" {
		return fmt.Errorf("资产准备网关必须是无凭据的本机 HTTP 地址")
	}
	if ip := net.ParseIP(endpoint.Hostname()); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("资产准备网关只允许字面量回环 IP")
	}
	if (c.AuthType != "" && c.AuthType != "password") || c.UseHttpLogin || c.LLM.Enabled || strings.TrimSpace(c.Password) == "" {
		return fmt.Errorf("资产准备只允许显式本机开发口令认证，不运行 AI")
	}
	if p.GoldTarget < 17 || p.GoldTarget > 100000 || p.HoldSeconds < 120 || p.HoldSeconds > 1800 {
		return fmt.Errorf("资产准备 gold_target 必须为 17..100000，hold_seconds 必须为 120..1800")
	}
	if !filepath.IsAbs(p.OutputPath) {
		return fmt.Errorf("资产准备 output_path 必须是独立结果文件的绝对路径")
	}
	return nil
}

// DataStressConfig configures the "data-stress" mode that drives repeated
// login/logout cycles and publishes per-player expectations to Redis for the
// db-side verifier (`go/db/cmd/verifier`).
type DataStressConfig struct {
	// Rounds is the number of login → play → logout cycles each robot runs.
	Rounds int `yaml:"rounds"`

	// PlaySeconds is how long each robot stays in-game per round.
	PlaySeconds int `yaml:"play_seconds"`

	// VerifyRedisAddr is the Redis the verifier reads from. Usually the same
	// one the db service uses; the verifier needs both `verify:enrolled:*`
	// and `verify:expected:*` keys here.
	VerifyRedisAddr     string `yaml:"verify_redis_addr"`
	VerifyRedisPassword string `yaml:"verify_redis_password"`
	VerifyRedisDB       int    `yaml:"verify_redis_db"`
}

// CurrencyCrashConfig configures the "currency-crash-snapshot" mode.
//
// Each robot run is one *snapshot* (login → read pre balance → optionally
// add gold → read post balance → exit). The orchestration script
// (tools/scripts/currency_crash_window.ps1) runs the robot twice per
// case A/B/C/D and diffs the two snapshots' results from
// `robot/logs/currency_crash_window/<run>/snapshot.json`.
type CurrencyCrashConfig struct {
	// Account override. If empty, falls back to fmt.Sprintf(AccountFmt, 1).
	Account string `yaml:"account"`

	// AddAmount: gold to add via GmAddCurrency. 0 = read-only snapshot
	// (post-kill leg uses this).
	AddAmount int64 `yaml:"add_amount"`

	// OutputPath: file to write the snapshot JSON
	// ({pre_balance, post_balance, player_id, account, ts}).
	// Default: "logs/currency_crash_window/snapshot.json"
	OutputPath string `yaml:"output_path"`
}

// BattleSmokeConfig 配置 "battle-smoke" 模式的跨 zone 匹配子模式
// (docs/design/cross-zone-matchmaking.md §8 "robot")。
//
// cross_zone=true 时:机器人 A 登 zone_a、B 登 zone_b,双双 JoinQueue(mode),
// 断言匹配到同一 battle_id 并自动战斗到终局,输出 CROSS_ZONE_MATCH_OK。
// cross_zone=false 时本块其余字段无效,走原 PVE + 观战流程。
type BattleSmokeConfig struct {
	CrossZone bool `yaml:"cross_zone"`

	// 战斗直连没有开关:收缩后它是战斗唯一通路(turn-based §22 D73),原 skip_direct_connect
	// (经 gate 中继验证 D23 回落)已删。yaml 非严格解析,旧配置里残留该键不会报错,但不再生效。

	// ZoneA / ZoneB 是两个机器人各自登录的 zone;必须非 0 且不同
	// (同 zone 就不是跨 zone 冒烟,登录时 0 会被 server-list 自动选区顶掉)。
	ZoneA uint32 `yaml:"zone_a"`
	ZoneB uint32 `yaml:"zone_b"`

	// Mode 是排队模式,当前只支持 "1v1"(MATCH_MODE_1V1,两人凑对)。
	// 缺省 "1v1"。
	Mode string `yaml:"mode"`

	// FeaturesSmoke only uses an explicitly selected existing account/character.
	FeaturesSmoke FeaturesSmokeConfig `yaml:"features_smoke"`
}

// ChatSmokeConfig 配置 "chat-smoke" 模式(zone_contract_v1 §9 冒烟段)。
//
// cross_zone=true 时:机器人 A 登 zone_a、B 登 zone_b,并断言两人的 gate 地址不同 ——
// chat 是全局服务,只有真落在两个 zone 的 gate 上,"B 看到 A 的消息"才证明跨 zone 可达。
// cross_zone=false 时 zone_a / zone_b 无效,两人都登顶层 zone_id(同 zone 弱验收)。
type ChatSmokeConfig struct {
	CrossZone bool `yaml:"cross_zone"`

	// ZoneA / ZoneB 是两个机器人各自登录的 zone;cross_zone=true 时必须非 0 且不同
	// (0 会被 server-list 自动选区顶掉,两人可能落进同一 zone)。
	ZoneA uint32 `yaml:"zone_a"`
	ZoneB uint32 `yaml:"zone_b"`
}

// GuildSmokeConfig 配置 "guild-smoke" 模式(docs/design/guild-zone-client-access.md §6)。
//
// 机器人 A(帮主)与 B(成员)登 zone_a;cross_zone=true 时 C 登 zone_b,
// 断言别区的帮会对 C 表现为不存在、加不进去、本区榜上没有。
type GuildSmokeConfig struct {
	CrossZone bool `yaml:"cross_zone"`

	// ZoneA 必须非 0:冒烟要断言新建帮会的 zone_id 等于 A 的归属区,0 会被 server-list 自动选区顶掉。
	ZoneA uint32 `yaml:"zone_a"`
	// ZoneB 仅 cross_zone=true 时使用,必须非 0 且与 ZoneA 不同。
	ZoneB uint32 `yaml:"zone_b"`

	// Economy 打开经济段(帮会二期 B5c,robot/guild_economy_smoke.go):GUILD_SMOKE_OK 之后
	// 由 EconomyLeader(帮主)与 EconomyMember(成员)在 zone_a 另建帮会跑捐献 / 升级 / 兑换。
	// 两个账号必须首次在 zone_a 建角;缺省 robot_9214 / robot_9215(号段见 90-consistency Y-09)。
	Economy       bool   `yaml:"economy"`
	EconomyLeader string `yaml:"economy_leader"`
	EconomyMember string `yaml:"economy_member"`

	// Activities 打开活动段(帮会二期 B6a-cli / B6b-cli,robot/guild_activity_smoke.go):经济段之后由 ActivityAccounts
	// 的四个账号在 zone_a 另建帮会跑 元宵灯会 / 中秋团圆(docs/design/guild-phase2/06-activities.md §6.44 的 S1–S8);
	// 同道历练(S9–S15)另由 Trial 控制。
	Activities bool `yaml:"activities"`
	// ActivityAccounts 依次是帮主 A、成员 B / C / D,必须恰好四个且互不相同(默认配表的阈值按四个机器人能达成来取)。
	// 四个账号必须首次在 zone_a 建角;缺省 robot_9216–9219(号段见 90-consistency Y-09)。
	ActivityAccounts []string `yaml:"activity_accounts"`
	// Trial 打开同道历练段(§6.44 的 S9–S15:邀请房间 → 全员同意 → 开战 → 结算发奖 → 拒绝 / 离线 / 名单不合法),
	// 同时决定 S1 对历练卡片的期望:true = 开放,false = 未开放。只在 Activities 为 true 时有意义。
	// 它必须与被测 guild 的部署一致:配表行启用之外,历练还要 guild 配了 MatchRpc 与历练结果消费
	// (Activity.TrialResult.Enabled + Kafka.Brokers)才算开放;两边不一致时冒烟在 S1 失败并说明原因。
	Trial bool `yaml:"trial"`
}

// 经济段的缺省账号。与管理段(9201–9203、9211–9213)错开,两段同时跑时账号不会撞车。
const (
	defaultGuildEconomyLeader = "robot_9214"
	defaultGuildEconomyMember = "robot_9215"
)

// 活动段的账号数:帮主 + 三名成员。灯会阈值 3、团圆在线阈值 3 都要靠这四个人凑出来,
// 第四个人还要验"达阈值之后照常受理、资金不重复发",所以多一个少一个都跑不了。
const guildActivityAccountCount = 4

// 活动段的缺省账号(帮主在首位)。函数而不是包级切片:包级切片是可变的全局状态,
// 调用方拿到之后一改,下一次取到的"缺省值"就不是缺省值了。
func defaultGuildActivityAccounts() []string {
	return []string{"robot_9216", "robot_9217", "robot_9218", "robot_9219"}
}

func (c *GuildSmokeConfig) validate() error {
	if c.ZoneA == 0 {
		return fmt.Errorf("zone_a must be set (non-zero)")
	}
	if c.Economy {
		if c.EconomyLeader == "" {
			c.EconomyLeader = defaultGuildEconomyLeader
		}
		if c.EconomyMember == "" {
			c.EconomyMember = defaultGuildEconomyMember
		}
		if c.EconomyLeader == c.EconomyMember {
			return fmt.Errorf("economy_leader and economy_member must differ (got %q)", c.EconomyLeader)
		}
	}
	if c.Activities {
		if len(c.ActivityAccounts) == 0 {
			c.ActivityAccounts = defaultGuildActivityAccounts()
		}
		if len(c.ActivityAccounts) != guildActivityAccountCount {
			return fmt.Errorf("activity_accounts must list exactly %d accounts (leader first), got %d",
				guildActivityAccountCount, len(c.ActivityAccounts))
		}
		seen := make(map[string]struct{}, len(c.ActivityAccounts))
		for i, account := range c.ActivityAccounts {
			if account == "" {
				return fmt.Errorf("activity_accounts[%d] is empty", i)
			}
			if _, dup := seen[account]; dup {
				return fmt.Errorf("activity_accounts must be distinct (%q appears twice)", account)
			}
			seen[account] = struct{}{}
		}
	}
	// 历练段是活动段的后半程(同一个帮会、同四个账号),单开它没有东西可跑;明确拒绝好过悄悄不跑。
	if c.Trial && !c.Activities {
		return fmt.Errorf("trial requires activities: true")
	}
	if !c.CrossZone {
		return nil
	}
	if c.ZoneB == 0 {
		return fmt.Errorf("zone_b must be set (non-zero) when cross_zone is true")
	}
	if c.ZoneA == c.ZoneB {
		return fmt.Errorf("zone_a and zone_b must differ when cross_zone is true (got %d)", c.ZoneA)
	}
	return nil
}

// TradeSmokeConfig 配置 "trade-smoke" 模式(聚宝斋 P1,docs/design/jubaozhai-market.md §9)。
//
// 卖家 A 与买家 B 登 zone_a;cross_zone=true 时卖家兼买家 C 登 zone_b(否则也登 zone_a,跳过跨区断言)。
// 种子商品经 gRPC 直连 trade 的 TradeAdmin.SeedListing 造(不经 gate),其余请求全部经 gate → 路由服。
type TradeSmokeConfig struct {
	CrossZone bool `yaml:"cross_zone"`

	// ZoneA 必须非 0:冒烟要断言种子商品的 market_zone 等于 A 的归属区,0 会被 server-list 自动选区顶掉。
	ZoneA uint32 `yaml:"zone_a"`
	// ZoneB 仅 cross_zone=true 时使用,必须非 0 且与 ZoneA 不同。
	ZoneB uint32 `yaml:"zone_b"`

	// AdminAddr 是 trade 的 gRPC 监听地址(host:port),缺省 127.0.0.1:50800。
	// trade 是全局服务、只有一份,直接写地址,不走 etcd 发现、也不做 zone 端口位移。
	AdminAddr string `yaml:"admin_addr"`

	// ExpectScope 必须与 trade.yaml 的 Market.Scope 一致("zone" | "global"),
	// 用来断言浏览响应的 market_scope 并选择范围断言分支;不给缺省值,避免跑错档位还假绿。
	ExpectScope string `yaml:"expect_scope"`

	// RequestIntervalMs 是同一机器人相邻两个经 gate 的请求的最小间隔,缺省 1100:
	// 聚宝斋消息号不在 MessageLimiter 表里时吃 gate 默认档 3 次 / 窗口(推导见 chat_smoke_scenario.go)。
	RequestIntervalMs int `yaml:"request_interval_ms"`
}

// FriendSmokeConfig 配置 "friend-smoke" 模式(docs/design/friend-port-20260918.md)。
//
// 发起方 A 与拉黑方 C 登 zone_a;cross_zone=true 时接收方 B 登 zone_b(否则也登 zone_a)。
// 全部请求都经 gate → client_rpc_router:friend 只承诺路由服模式可达(D-12),没有 gate 直连白名单。
type FriendSmokeConfig struct {
	CrossZone bool `yaml:"cross_zone"`

	// ZoneA 必须非 0:0 会被 server-list 自动选区顶掉,三个机器人可能落进同一个 zone,
	// 跨区推送与跨区读会话的断言就都不成立了。
	ZoneA uint32 `yaml:"zone_a"`
	// ZoneB 仅 cross_zone=true 时使用,必须非 0 且与 ZoneA 不同。
	ZoneB uint32 `yaml:"zone_b"`

	// RequestIntervalMs 是同一机器人相邻两个经 gate 的请求的最小间隔,缺省 1100:
	// 好友消息号要等 proto-gen 定号之后才能进 MessageLimiter 表,在那之前吃 gate 默认档
	// 3 次 / 窗口(推导见 chat_smoke_scenario.go)。
	RequestIntervalMs int `yaml:"request_interval_ms"`

	// PushTimeoutMs 是等一条 NotifyFriendEvent 的预算,缺省 10000:
	// 推送比同步 RPC 多一跳 Kafka(friend → gate-cmd_g<N> → gate),所以给得更宽。
	PushTimeoutMs int `yaml:"push_timeout_ms"`
}

// MailSmokeConfig 配置 "mail-smoke" 模式(docs/design/mail-system.md §10 ⑦)。
//
// 旁观者 O 登 zone_a;cross_zone=true 时收件人 R 登 zone_b(否则也登 zone_a)。客户端请求全部经 gate → client_rpc_router
// (mail 只承诺路由服模式可达,D-12);发件经 gRPC 直连 MailAdmin(admin_addr),令牌取自环境变量 MMORPG_MAIL_ADMIN_TOKEN,不进 yaml。
type MailSmokeConfig struct {
	CrossZone bool `yaml:"cross_zone"`

	// ZoneA 必须非 0:0 会被 server-list 自动选区顶掉,两个机器人可能落进同一个 zone,跨区推送的结论就不成立了。
	ZoneA uint32 `yaml:"zone_a"`
	// ZoneB 仅 cross_zone=true 时使用,必须非 0 且与 ZoneA 不同。
	ZoneB uint32 `yaml:"zone_b"`

	// AdminAddr 是 mail 的 gRPC 监听地址(host:port),缺省 127.0.0.1:50900。
	// mail 是全局服务、只有一份,直接写地址,不走 etcd 发现、也不做 zone 端口位移(同 trade-smoke)。
	AdminAddr string `yaml:"admin_addr"`

	// RequestIntervalMs 是同一机器人相邻两个经 gate 的请求的最小间隔,缺省 1100(推导见 chat_smoke_scenario.go)。
	RequestIntervalMs int `yaml:"request_interval_ms"`

	// PushTimeoutMs 是等一条 NotifyMailEvent 的预算,缺省 10000。推送是**弱断言**:超时只打印 push_received=false,不算失败。
	PushTimeoutMs int `yaml:"push_timeout_ms"`
}

func (c *TradeSmokeConfig) validate() error {
	if c.ZoneA == 0 {
		return fmt.Errorf("zone_a must be set (non-zero)")
	}
	if c.AdminAddr == "" {
		return fmt.Errorf("admin_addr must be set")
	}
	switch c.ExpectScope {
	case "zone", "global":
		// valid
	default:
		return fmt.Errorf("expect_scope must be zone or global (got %q)", c.ExpectScope)
	}
	if c.RequestIntervalMs <= 0 {
		return fmt.Errorf("request_interval_ms must be > 0 (got %d)", c.RequestIntervalMs)
	}
	if !c.CrossZone {
		return nil
	}
	if c.ZoneB == 0 {
		return fmt.Errorf("zone_b must be set (non-zero) when cross_zone is true")
	}
	if c.ZoneA == c.ZoneB {
		return fmt.Errorf("zone_a and zone_b must differ when cross_zone is true (got %d)", c.ZoneA)
	}
	return nil
}

// RankSmokeConfig 配置 "rank-smoke" 模式(docs/design/leaderboard-system.md §8.2)。
//
// A、N 登 zone_a,B 登 zone_b(必须不同:跨区名字补全是本冒烟要验的一条)。A、B 跨区打一局 1V1,
// 读榜请求全部经 gate → client_rpc_router(rank 只承诺路由服模式可达,D-12)。
type RankSmokeConfig struct {
	// ZoneA / ZoneB 必须非 0 且不同:0 会被 server-list 自动选区顶掉,两个机器人可能落进同一个 zone。
	ZoneA uint32 `yaml:"zone_a"`
	ZoneB uint32 `yaml:"zone_b"`

	// RequestIntervalMs 是同一机器人相邻两次读榜的最小间隔,缺省 1100(gate 默认限流档 3 次 / 窗口,推导见 chat_smoke_scenario.go)。
	RequestIntervalMs int `yaml:"request_interval_ms"`

	// UpdateTimeoutMs 是打完一局之后等榜更新的预算,缺省 30000(match 发快照 → rank 消费都是异步的,含 Kafka 再均衡余量)。
	UpdateTimeoutMs int `yaml:"update_timeout_ms"`

	// RateLimitCheck 打开时,N 在一个窗口内连发 RateLimitPerSecond + 1 次读榜,断言最后一次被 gate 限流。
	// 只在 data/MessageLimiter.xlsx 按 rank 导过表(tools/scripts/rank_xlsx_patch.py)之后打开。
	RateLimitCheck bool `yaml:"rate_limit_check"`
	// RateLimitPerSecond 与 MessageLimiter 里 GetRankPage 那一行的每秒次数一致,缺省 5。
	RateLimitPerSecond int `yaml:"rate_limit_per_second"`
}

func (c *RankSmokeConfig) validate() error {
	if c.ZoneA == 0 || c.ZoneB == 0 {
		return fmt.Errorf("zone_a and zone_b must both be set (non-zero) (got %d / %d)", c.ZoneA, c.ZoneB)
	}
	if c.ZoneA == c.ZoneB {
		return fmt.Errorf("zone_a and zone_b must differ (got %d): cross-zone name lookup is part of the smoke", c.ZoneA)
	}
	if c.RequestIntervalMs <= 0 {
		return fmt.Errorf("request_interval_ms must be > 0 (got %d)", c.RequestIntervalMs)
	}
	if c.UpdateTimeoutMs <= 0 {
		return fmt.Errorf("update_timeout_ms must be > 0 (got %d)", c.UpdateTimeoutMs)
	}
	if c.RateLimitCheck && c.RateLimitPerSecond <= 0 {
		return fmt.Errorf("rate_limit_per_second must be > 0 when rate_limit_check is true (got %d)", c.RateLimitPerSecond)
	}
	return nil
}

func (c *MailSmokeConfig) validate() error {
	if c.ZoneA == 0 {
		return fmt.Errorf("zone_a must be set (non-zero)")
	}
	if c.AdminAddr == "" {
		return fmt.Errorf("admin_addr must be set")
	}
	if c.RequestIntervalMs <= 0 {
		return fmt.Errorf("request_interval_ms must be > 0 (got %d)", c.RequestIntervalMs)
	}
	if c.PushTimeoutMs <= 0 {
		return fmt.Errorf("push_timeout_ms must be > 0 (got %d)", c.PushTimeoutMs)
	}
	if !c.CrossZone {
		return nil
	}
	if c.ZoneB == 0 {
		return fmt.Errorf("zone_b must be set (non-zero) when cross_zone is true")
	}
	if c.ZoneA == c.ZoneB {
		return fmt.Errorf("zone_a and zone_b must differ when cross_zone is true (got %d)", c.ZoneA)
	}
	return nil
}

func (c *FriendSmokeConfig) validate() error {
	if c.ZoneA == 0 {
		return fmt.Errorf("zone_a must be set (non-zero)")
	}
	if c.RequestIntervalMs <= 0 {
		return fmt.Errorf("request_interval_ms must be > 0 (got %d)", c.RequestIntervalMs)
	}
	if c.PushTimeoutMs <= 0 {
		return fmt.Errorf("push_timeout_ms must be > 0 (got %d)", c.PushTimeoutMs)
	}
	if !c.CrossZone {
		return nil
	}
	if c.ZoneB == 0 {
		return fmt.Errorf("zone_b must be set (non-zero) when cross_zone is true")
	}
	if c.ZoneA == c.ZoneB {
		return fmt.Errorf("zone_a and zone_b must differ when cross_zone is true (got %d)", c.ZoneA)
	}
	return nil
}

// TeamSmokeConfig 配置 "team-smoke" 模式(docs/design/team-system.md §I.5)。
//
// 机器人 A(队长)、B、D 登 zone_a;cross_zone=true 时 C 登 zone_b,并按 expect_cross_zone_allowed
// 断言跨区组队被拒(X1)或放行(X2)。expect_cross_zone_allowed 必须与 match 的 Team.AllowCrossZone 一致。
type TeamSmokeConfig struct {
	CrossZone bool `yaml:"cross_zone"`
	// ExpectCrossZoneAllowed:false 跑 X1(TeamCrossZoneDenied),true 跑 X2(跨区入队 + 不跟随 + 跨 zone 开战)。
	// 只在 cross_zone=true 时有意义;cross_zone=false 却填 true 视为配置错误,避免"以为测了 X2 其实跳过了"。
	ExpectCrossZoneAllowed bool `yaml:"expect_cross_zone_allowed"`

	// ZoneA 必须非 0:冒烟要断言新队伍的 zone_id 等于 A 的 home zone,0 会被 server-list 自动选区顶掉。
	ZoneA uint32 `yaml:"zone_a"`
	// ZoneB 仅 cross_zone=true 时使用,必须非 0 且与 ZoneA 不同。
	ZoneB uint32 `yaml:"zone_b"`

	// BattleConfigId 是整队开战 / 单人 PVE 用的 DungeonTable id,必须非 0 且在 match 的 PveTeamSizeByConfigId 里配置。
	BattleConfigId uint32 `yaml:"battle_config_id"`

	// FollowSceneConfigIds 是换图候选(BaseScene id),至少 3 个互不相同的非 0 值:
	// S6 要选一张与 A、B 当前地图都不同的图,两人最多占掉 2 个。
	FollowSceneConfigIds []uint32 `yaml:"follow_scene_config_ids"`
}

// TeamBackfillSmokeConfig 配置 "team-backfill-smoke" 模式(docs/design/team-backfill-matching.md §7.3):
// 三个机器人 A(队长)、B(队员)、C(路人)都登 zone;A、B 组队选「凑满 3 人」,C 单排同一副本的 3 人档。
type TeamBackfillSmokeConfig struct {
	// Zone 必须非 0:三人要在同一个 zone(0 会被 server-list 自动选区顶掉,不保证三人同区)。
	Zone uint32 `yaml:"zone"`
	// BattleConfigId 是补位开战用的 DungeonTable id,必须非 0,且 match 的 PveTeamSizeByConfigId 里它的人数上限 ≥ 3。
	BattleConfigId uint32 `yaml:"battle_config_id"`
}

func (c *TeamBackfillSmokeConfig) validate() error {
	if c.Zone == 0 {
		return fmt.Errorf("zone must be set (non-zero)")
	}
	if c.BattleConfigId == 0 {
		return fmt.Errorf("battle_config_id must be set (non-zero)")
	}
	return nil
}

// teamSmokeMinFollowSceneConfigs 是换图候选的最少个数(见 FollowSceneConfigIds 注释)。
const teamSmokeMinFollowSceneConfigs = 3

func (c *TeamSmokeConfig) validate() error {
	if c.ZoneA == 0 {
		return fmt.Errorf("zone_a must be set (non-zero)")
	}
	if c.BattleConfigId == 0 {
		return fmt.Errorf("battle_config_id must be set (non-zero)")
	}
	seen := make(map[uint32]struct{}, len(c.FollowSceneConfigIds))
	for _, id := range c.FollowSceneConfigIds {
		if id == 0 {
			return fmt.Errorf("follow_scene_config_ids must not contain 0")
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("follow_scene_config_ids must not contain duplicates (got %d twice)", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) < teamSmokeMinFollowSceneConfigs {
		return fmt.Errorf("follow_scene_config_ids needs at least %d distinct scene config ids (got %d)",
			teamSmokeMinFollowSceneConfigs, len(seen))
	}
	if !c.CrossZone {
		if c.ExpectCrossZoneAllowed {
			return fmt.Errorf("expect_cross_zone_allowed=true requires cross_zone=true (the cross-zone steps would be skipped)")
		}
		return nil
	}
	if c.ZoneB == 0 {
		return fmt.Errorf("zone_b must be set (non-zero) when cross_zone is true")
	}
	if c.ZoneA == c.ZoneB {
		return fmt.Errorf("zone_a and zone_b must differ when cross_zone is true (got %d)", c.ZoneA)
	}
	return nil
}

// TeamFollowSmokeConfig 配置 "team-follow-smoke" 模式(docs/design/team-system.md「组队跟随 v1.1」)。
//
// 缺省(client_follower=false)两个机器人验服务端:入队即归队、可见性重建、双向移动广播。
// client_follower=true 改跑「真实客户端跟随」验收:机器人 B 入队后下线、把账号让给真实客户端(DevAutoPilot 登录同一账号),
// 机器人 A 当队长按脚本走路,从 B 的移动广播里量出它有没有跟在身后。
type TeamFollowSmokeConfig struct {
	ClientFollower bool `yaml:"client_follower"`
	// ClientWaitSeconds 是等真实客户端登录并出现在队长视野里的预算(秒);0 = 240(播放器冷启动较慢)。
	ClientWaitSeconds int `yaml:"client_wait_seconds"`
}

// TravelSmokeConfig 配置 "travel-smoke" 模式(docs/design/cross-zone-scene-travel.md §5 阶段 3)。
//
// 单机器人登 home_zone → TravelToZone{visit_zone} → 跟随 msg 124 落到访客区 → 停留 → TravelToZone{home_zone} 回家。
// 两个 zone 都必须显式给出:0 会被 server-list 自动选区顶掉,"从哪个区出发"就不确定了,
// 而本冒烟的每条断言(票据 zone、gate 地址变化、金币不回档)都以"出发区 == 账号的 home zone"为前提。
type TravelSmokeConfig struct {
	// Account 为空时沿用 robot_9601；显式账号仍须符合登录侧 robot_ 开发账号规则。
	Account string `yaml:"account"`

	// HomeZone 必须非 0,且是冒烟账号**首次建角**的 zone(home zone 在建角时登记,之后不随登录区变化)。
	HomeZone uint32 `yaml:"home_zone"`
	// VisitZone 是要去做客的 zone,必须非 0 且与 HomeZone 不同。
	VisitZone uint32 `yaml:"visit_zone"`

	// SceneConfigId 是 TravelToZoneRequest.scene_config_id(BaseScene id),去程与回程共用。
	// 0 = 由目标 zone 的 scene_manager 按世界频道表挑默认大世界(与 proto 注释同义),此时不断言落地地图;
	// 非 0 时断言落地后 NotifyEnterScene 的 scene_config_id 等于它,所以这张图必须两个 zone 都有。
	SceneConfigId uint32 `yaml:"scene_config_id"`

	// DwellSeconds 是到访客区后停留多久再回家,缺省 35:要熬过 home 区 login 的断线租约(30s),
	// 才能抓到"租约到期清理误伤了已经搬到访客区的会话"这一类问题。0 = 不停留(只验链路通不通)。
	DwellSeconds int `yaml:"dwell_seconds"`

	// RequireMoveAck=true 时访客区收不到 MoveAck 即失败。缺省 false:MoveAck 只在服务器纠偏时才回,
	// 场景没有导航网格时 fail-open 不回(player_movement_handler.cpp),不能当硬断言。
	RequireMoveAck bool `yaml:"require_move_ack"`

	// HopTimeoutSeconds 是一跳传送的总预算(TravelToZone 的认领窗口与等到达共用),缺省 60,推导见
	// travel_smoke_scenario.go 的 travelSmokeRedirectTimeout。要自动验收冻结硬上限(runbook 场景 H:服务端最迟 71s
	// 给结论,客户端预算 75s)时设成 ≥ 75,否则 robot 自己先超时,看不到上限分支。必须 > 0。
	HopTimeoutSeconds int `yaml:"hop_timeout_seconds"`
}

func (c *TravelSmokeConfig) validate() error {
	if c.Account == "" {
		c.Account = "robot_9601"
	}
	// 与 login 的 ValidatePasswordAccount 保持 UTF-8、首尾空白和 VARCHAR(191) 字符边界一致。
	const maxAccountRunes = 191
	if !utf8.ValidString(c.Account) || c.Account != strings.TrimSpace(c.Account) ||
		!strings.HasPrefix(c.Account, "robot_") || utf8.RuneCountInString(c.Account) > maxAccountRunes {
		return fmt.Errorf("account 必须使用 robot_ 前缀、有效 UTF-8、无首尾空白且不超过 %d 个字符", maxAccountRunes)
	}
	if c.HomeZone == 0 || c.VisitZone == 0 {
		return fmt.Errorf("home_zone and visit_zone must be set (non-zero)")
	}
	if c.HomeZone == c.VisitZone {
		return fmt.Errorf("home_zone and visit_zone must differ (got %d)", c.HomeZone)
	}
	if c.DwellSeconds < 0 {
		return fmt.Errorf("dwell_seconds must be >= 0 (got %d)", c.DwellSeconds)
	}
	if c.HopTimeoutSeconds <= 0 {
		return fmt.Errorf("hop_timeout_seconds must be > 0 (got %d)", c.HopTimeoutSeconds)
	}
	return nil
}

// ChannelSmokeConfig 配置 "channel-smoke" 模式(docs/design/world-channel-switch.md §10)。
//
// 单机器人登顶层 zone_id → 列线(SceneInfoC2S → NotifySceneInfo 带分线目录)→ 切到同图另一条线 → 再列线
// → 冷却验证 → 切到当前线的负例。本块两个字段都是等待上限,缺省值见 Load。
type ChannelSmokeConfig struct {
	// ArriveTimeoutSeconds 是发出切线请求后等 NotifyEnterScene(scene_id == 目标线)的上限,缺省 20。
	// 同节点切线在本地是毫秒级;目标线在别的 scene 节点时要走「冻结 → 存盘 → 写标记 → 重发」的交接链路,
	// 多 scene 节点环境按需调大。
	ArriveTimeoutSeconds int `yaml:"arrive_timeout_seconds"`

	// MaxCooldownWaitSeconds 是冷却验证愿意等的最长时间,缺省 60。
	// 目录里的 switch_cooldown_seconds + 1 超过它时,只验「冷却期内切回被拒」,不等冷却过期再切回
	// (日志打印 SKIP 说明):服务端配了长冷却的环境不该把冒烟跑成几分钟的挂起。
	MaxCooldownWaitSeconds int `yaml:"max_cooldown_wait_seconds"`
}

// channel-smoke 两个等待上限自身的上限:填错单位(比如把毫秒当秒)时在加载期就拒掉,
// 而不是让冒烟在失败路径上挂很久。
const (
	channelSmokeMaxArriveTimeoutSeconds = 300
	channelSmokeMaxCooldownWaitSeconds  = 600
)

func (c *ChannelSmokeConfig) validate() error {
	if c.ArriveTimeoutSeconds <= 0 || c.ArriveTimeoutSeconds > channelSmokeMaxArriveTimeoutSeconds {
		return fmt.Errorf("arrive_timeout_seconds must be in 1..%d (got %d)",
			channelSmokeMaxArriveTimeoutSeconds, c.ArriveTimeoutSeconds)
	}
	if c.MaxCooldownWaitSeconds <= 0 || c.MaxCooldownWaitSeconds > channelSmokeMaxCooldownWaitSeconds {
		return fmt.Errorf("max_cooldown_wait_seconds must be in 1..%d (got %d)",
			channelSmokeMaxCooldownWaitSeconds, c.MaxCooldownWaitSeconds)
	}
	return nil
}

// SyncSmokeConfig 配置 "sync-smoke" 模式(docs/design/landing-recovery-sync-digest.md §10 第 6 步)。
//
// 被观测方 A 与触发方 B 都登顶层 zone_id。本块只管可选的跨 zone 腿(场景步骤 S5),整块可缺省。
type SyncSmokeConfig struct {
	// CrossZone=true 时做 S5:A 从 zone_id 传送到 visit_zone、落地取摘要,再传送回 zone_id 再取一次。
	// 缺省 false:跨 zone 落地依赖的环境(两个 zone、共享 Redis、GateTokenSecret 等,见 etc/travel_smoke.yaml)
	// 不是每个开发机都有。
	CrossZone bool `yaml:"cross_zone"`
	// VisitZone 仅 cross_zone=true 时使用,必须非 0 且与 zone_id 不同。
	VisitZone uint32 `yaml:"visit_zone"`
	// SceneConfigId 是去程与回程两次 TravelToZoneRequest.scene_config_id;0 = 由目标 zone 的 scene_manager 挑默认大世界。
	SceneConfigId uint32 `yaml:"scene_config_id"`
}

// validate 只在 mode=sync-smoke 时调用。homeZone 是顶层 zone_id:跨 zone 腿从它出发、最后回到它,
// 所以必须显式给出 —— 0 会被 server-list 自动选区顶掉,"回家"就不确定了。
func (c *SyncSmokeConfig) validate(homeZone uint32) error {
	if !c.CrossZone {
		return nil
	}
	if homeZone == 0 {
		return fmt.Errorf("zone_id must be set (non-zero) when cross_zone is true")
	}
	if c.VisitZone == 0 {
		return fmt.Errorf("visit_zone must be set (non-zero) when cross_zone is true")
	}
	if c.VisitZone == homeZone {
		return fmt.Errorf("visit_zone must differ from zone_id when cross_zone is true (got %d)", homeZone)
	}
	return nil
}

// MerchantSmokeConfig 配置 "merchant-smoke" 模式(docs/design/mystery-merchant.md §9)。
//
// 单机器人登顶层 zone_id → 查询神秘商人 → 不在同一条线就切过去 → 走到站位旁 → 负例 → GM 加钱 → 购买 → 买到限购上限。
type MerchantSmokeConfig struct {
	// ArriveTimeoutSeconds 是切到商人所在线时等 NotifyEnterScene 的上限,缺省 20(含义同 ChannelSmokeConfig 的同名字段)。
	ArriveTimeoutSeconds int `yaml:"arrive_timeout_seconds"`
}

func (c *MerchantSmokeConfig) validate() error {
	if c.ArriveTimeoutSeconds <= 0 || c.ArriveTimeoutSeconds > channelSmokeMaxArriveTimeoutSeconds {
		return fmt.Errorf("arrive_timeout_seconds must be in 1..%d (got %d)",
			channelSmokeMaxArriveTimeoutSeconds, c.ArriveTimeoutSeconds)
	}
	return nil
}

// MallArrivalSmokeConfig 是官方商品到账冒烟(下单 → 模拟付款 → 发货进背包 → 订单已发货)的参数。
// 模拟付款器只许连本机商城的独立mock适配器，所以run_mode必须显式写dev；sku_id没有缺省值，
// 必须与 etc/mall_arrival_smoke_seed.sql 种进商城库的那条一致,item_config_id / count_per_unit 是它的发放内容
// (SKU 应答里不带发放内容,冒烟按这里的期望核对背包增量)。
type MallArrivalSmokeConfig struct {
	RunMode               string `yaml:"run_mode"`                // 必须写 "dev"
	MallURL               string `yaml:"mall_url"`                // 商城地址,只允许 http://<回环IP>:<端口>;缺省 http://127.0.0.1:18120
	PaymentSubject        string `yaml:"payment_subject"`         // 与商城 COMMERCE_PAYMENT_SUBJECT 一致;缺省 payment-adapter
	Channel               string `yaml:"channel"`                 // 必须mock；只声明适配器，不向请求体发送渠道
	SkuID                 uint64 `yaml:"sku_id"`                  // 官方商品号,必填
	Quantity              uint32 `yaml:"quantity"`                // 下单份数 1..16;缺省 1
	ItemConfigID          uint32 `yaml:"item_config_id"`          // 期望发放的物品;缺省 10(可叠加的回血药)
	CountPerUnit          uint32 `yaml:"count_per_unit"`          // 期望每份发放数量;缺省 2
	DeliverTimeoutSeconds int    `yaml:"deliver_timeout_seconds"` // 付款后等「已发货」的上限 10..600;缺省 90
	PollIntervalMs        int    `yaml:"poll_interval_ms"`        // 轮询订单状态的间隔 1100..10000;缺省 1500(gate 对未登记限流的消息默认 3 次/秒)
}

// Validate 必须在任何网络操作之前完成(由 Config.validate 在 Load 里调用)。
func (c *MallArrivalSmokeConfig) Validate() error {
	if c.RunMode != "dev" {
		return fmt.Errorf("run_mode must be explicitly \"dev\": the mock payer records payments and is only allowed against a local dev mall")
	}
	endpoint, err := url.Parse(c.MallURL)
	if err != nil || endpoint.Scheme != "http" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") || endpoint.Port() == "" {
		return fmt.Errorf("mall_url must be http://<loopback IP>:<port> without credentials, path or query")
	}
	if ip := net.ParseIP(endpoint.Hostname()); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("mall_url must use a literal loopback IP (dev only)")
	}
	if port, err := strconv.Atoi(endpoint.Port()); err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("mall_url必须显式指定1..65535端口")
	}
	if !mallSubjectPattern.MatchString(c.PaymentSubject) {
		return fmt.Errorf("payment_subject must match [a-z][a-z0-9-]{2,63} (same as the mall's COMMERCE_PAYMENT_SUBJECT)")
	}
	if c.Channel != "mock" {
		return fmt.Errorf("channel必须是mock，旧mock-dev及任意渠道回执均不能替代本地mock提供方原件")
	}
	if c.SkuID == 0 {
		return fmt.Errorf("sku_id must be set (the one seeded by etc/mall_arrival_smoke_seed.sql)")
	}
	if c.Quantity == 0 || c.Quantity > 16 {
		return fmt.Errorf("quantity must be in 1..16 (got %d)", c.Quantity)
	}
	if c.ItemConfigID == 0 || c.CountPerUnit == 0 || uint64(c.Quantity)*uint64(c.CountPerUnit) > 4096 {
		return fmt.Errorf("item_config_id must be nonzero and quantity × count_per_unit must be in 1..4096 (trade caps one delivery at 4096)")
	}
	if c.DeliverTimeoutSeconds < 10 || c.DeliverTimeoutSeconds > 600 {
		return fmt.Errorf("deliver_timeout_seconds must be in 10..600 (got %d)", c.DeliverTimeoutSeconds)
	}
	if c.PollIntervalMs < 1100 || c.PollIntervalMs > 10000 {
		return fmt.Errorf("poll_interval_ms must be in 1100..10000 (got %d)", c.PollIntervalMs)
	}
	return nil
}

// 与 java/mall_game HmacAuthentication / CommerceApi.Payment 的校验正则一致。
var (
	mallSubjectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
)

type LLMConfig struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"` // e.g. "http://localhost:11434/v1/chat/completions"
	APIKey   string `yaml:"api_key"`  // optional, for OpenAI/Azure
	Model    string `yaml:"model"`    // e.g. "gpt-4o-mini", "llama3", "qwen2"
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		RobotCount:     1,
		AccountFmt:     "robot_%04d",
		Password:       "123456",
		ActionInterval: 3,
		ReportInterval: 5,
		TableDir:       "../generated/tables",
		BattleSmoke:    BattleSmokeConfig{Mode: "1v1"},
		// trade-smoke 缺省:trade 本地 gRPC 端口 50800;间隔 1100ms 适配 gate 默认限流档。
		TradeSmoke:  TradeSmokeConfig{AdminAddr: "127.0.0.1:50800", RequestIntervalMs: 1100},
		FriendSmoke: FriendSmokeConfig{RequestIntervalMs: 1100, PushTimeoutMs: 10000},
		// mail-smoke 缺省:mail 本地 gRPC 端口 50900;间隔 / 推送预算同 friend-smoke。
		MailSmoke: MailSmokeConfig{AdminAddr: "127.0.0.1:50900", RequestIntervalMs: 1100, PushTimeoutMs: 10000},
		// rank-smoke 缺省:读榜间隔同 friend-smoke;等写榜 30s;限流断言默认关(MessageLimiter 按 rank 导表之后再开)。
		RankSmoke: RankSmokeConfig{RequestIntervalMs: 1100, UpdateTimeoutMs: 30000, RateLimitPerSecond: 5},
		// travel-smoke 缺省:停留 35s,大于 login 断线租约 30s(见 TravelSmokeConfig.DwellSeconds)。
		// 一跳预算缺省 60s(推导见 travel_smoke_scenario.go 的 travelSmokeRedirectTimeout)。
		TravelSmoke: TravelSmokeConfig{DwellSeconds: 35, HopTimeoutSeconds: 60},
		// channel-smoke 缺省:等到达 20s(同节点切线毫秒级,留足余量);冷却验证最多等 60s(服务端缺省冷却 10s)。
		ChannelSmoke: ChannelSmokeConfig{ArriveTimeoutSeconds: 20, MaxCooldownWaitSeconds: 60},
		// merchant-smoke 缺省:切到商人所在线时等到达 20s。
		MerchantSmoke: MerchantSmokeConfig{ArriveTimeoutSeconds: 20},
		// 本机商城18120、固定mock事实适配器、1份×2瓶回血药；渠道允许集合保持空。
		// run_mode 与 sku_id 刻意没有缺省值,必须在 yaml 里显式写出。
		MallArrivalSmoke: MallArrivalSmokeConfig{MallURL: "http://127.0.0.1:18120", PaymentSubject: "payment-adapter",
			Channel: "mock", Quantity: 1, ItemConfigID: 10, CountPerUnit: 2, DeliverTimeoutSeconds: 90, PollIntervalMs: 1500},
		// equip-forge-topup 缺省补给 equip-forge-smoke 用的那个账号。
		EquipForgeTopup: EquipForgeTopupConfig{Account: "robot_9104"},
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}
	return cfg, nil
}

// ResolveProfile returns the AI profile to use.
// Priority: custom_weights > builtin profile name > nil (use default).
func (c *Config) ResolveProfile() *ai.Profile {
	if len(c.CustomWeights) > 0 {
		weights := make(map[ai.Action]int)
		for name, w := range c.CustomWeights {
			if a, ok := ai.ParseAction(name); ok {
				weights[a] = w
			}
		}
		p := &ai.Profile{Name: "custom", Weights: weights}
		return p
	}
	if bp, ok := ai.BuiltinProfiles[c.Profile]; ok {
		return &bp
	}
	return nil
}

func (c *Config) validate() error {
	if c.GatewayAddr == "" {
		return fmt.Errorf("gateway_addr must be set")
	}
	if c.RobotCount <= 0 {
		return fmt.Errorf("robot_count must be > 0")
	}
	if c.AccountFmt == "" {
		return fmt.Errorf("account_fmt must be set")
	}
	switch c.Mode {
	case "", "stress", "login-test", "data-stress", "currency-crash-snapshot", "battle-smoke", "attribute-smoke", "pet-smoke", "equip-smoke", "equip-forge-smoke", "chat-smoke", "guild-smoke", "trade-smoke", "team-smoke", "team-follow-smoke", "team-backfill-smoke", "travel-smoke", "friend-smoke", "mail-smoke", "rank-smoke", "channel-smoke", "ordering-smoke", "sync-smoke", "features-smoke", "merchant-smoke", "mall-arrival-smoke", "equip-forge-topup", "asset-arrival-prepare":
		// valid
	default:
		return fmt.Errorf("unknown mode %q (expected stress, login-test, data-stress, currency-crash-snapshot, battle-smoke, attribute-smoke, pet-smoke, equip-smoke, equip-forge-smoke, chat-smoke, guild-smoke, trade-smoke, team-smoke, team-follow-smoke, team-backfill-smoke, travel-smoke, friend-smoke, mail-smoke, rank-smoke, channel-smoke, ordering-smoke, sync-smoke, features-smoke, merchant-smoke, mall-arrival-smoke, equip-forge-topup, or asset-arrival-prepare)", c.Mode)
	}
	if c.Mode == "asset-arrival-prepare" {
		return c.ValidateAssetArrivalPrepare()
	}
	if c.AuthType == "satoken" && c.SaTokenAddr == "" {
		return fmt.Errorf("satoken_addr must be set when auth_type is satoken")
	}
	if c.Mode == "battle-smoke" && c.BattleSmoke.CrossZone {
		if err := c.BattleSmoke.validate(); err != nil {
			return fmt.Errorf("battle_smoke: %w", err)
		}
	}
	if c.Mode == "chat-smoke" && c.ChatSmoke.CrossZone {
		if err := c.ChatSmoke.validate(); err != nil {
			return fmt.Errorf("chat_smoke: %w", err)
		}
	}
	if c.Mode == "guild-smoke" {
		if err := c.GuildSmoke.validate(); err != nil {
			return fmt.Errorf("guild_smoke: %w", err)
		}
	}
	if c.Mode == "trade-smoke" {
		if err := c.TradeSmoke.validate(); err != nil {
			return fmt.Errorf("trade_smoke: %w", err)
		}
	}
	if c.Mode == "friend-smoke" {
		if err := c.FriendSmoke.validate(); err != nil {
			return fmt.Errorf("friend_smoke: %w", err)
		}
	}
	if c.Mode == "mail-smoke" {
		if err := c.MailSmoke.validate(); err != nil {
			return fmt.Errorf("mail_smoke: %w", err)
		}
	}
	if c.Mode == "rank-smoke" {
		if err := c.RankSmoke.validate(); err != nil {
			return fmt.Errorf("rank_smoke: %w", err)
		}
	}
	if c.Mode == "team-backfill-smoke" {
		if err := c.TeamBackfillSmoke.validate(); err != nil {
			return fmt.Errorf("team_backfill_smoke: %w", err)
		}
	}
	if c.Mode == "team-smoke" {
		if err := c.TeamSmoke.validate(); err != nil {
			return fmt.Errorf("team_smoke: %w", err)
		}
	}
	if c.Mode == "travel-smoke" {
		if err := c.TravelSmoke.validate(); err != nil {
			return fmt.Errorf("travel_smoke: %w", err)
		}
	}
	if c.Mode == "channel-smoke" {
		if err := c.ChannelSmoke.validate(); err != nil {
			return fmt.Errorf("channel_smoke: %w", err)
		}
	}
	if c.Mode == "sync-smoke" {
		if err := c.SyncSmoke.validate(c.ZoneID); err != nil {
			return fmt.Errorf("sync_smoke: %w", err)
		}
	}
	if c.Mode == "merchant-smoke" {
		if err := c.MerchantSmoke.validate(); err != nil {
			return fmt.Errorf("merchant_smoke: %w", err)
		}
	}
	if c.Mode == "mall-arrival-smoke" {
		if c.RobotCount != 1 || (c.AuthType != "" && c.AuthType != "password") || c.UseHttpLogin || c.LLM.Enabled {
			return fmt.Errorf("mall_arrival_smoke: single robot with password auth only, no HTTP login, no AI")
		}
		if err := c.MallArrivalSmoke.Validate(); err != nil {
			return fmt.Errorf("mall_arrival_smoke: %w", err)
		}
	}
	if c.Mode == "equip-forge-topup" {
		if err := c.EquipForgeTopup.Validate(); err != nil {
			return fmt.Errorf("equip_forge_topup: %w", err)
		}
	}
	return nil
}

// validate 只在 cross_zone=true 时被调用:两个 zone 必须显式给出且不同,
// 否则冒烟结论(chat 跨 zone 可达)不成立。
func (c *ChatSmokeConfig) validate() error {
	if c.ZoneA == 0 || c.ZoneB == 0 {
		return fmt.Errorf("zone_a and zone_b must be set (non-zero) when cross_zone is true")
	}
	if c.ZoneA == c.ZoneB {
		return fmt.Errorf("zone_a and zone_b must differ when cross_zone is true (got %d)", c.ZoneA)
	}
	return nil
}

// validate 只在 cross_zone=true 时被调用:两个 zone 必须显式给出且不同,
// 否则冒烟结论(跨 zone 匹配)不成立。
func (b *BattleSmokeConfig) validate() error {
	if b.ZoneA == 0 || b.ZoneB == 0 {
		return fmt.Errorf("zone_a and zone_b must be set (non-zero) when cross_zone is true")
	}
	if b.ZoneA == b.ZoneB {
		return fmt.Errorf("zone_a and zone_b must differ when cross_zone is true (got %d)", b.ZoneA)
	}
	switch b.Mode {
	case "", "1v1":
		// valid;空串在 Load 里已被缺省值 "1v1" 覆盖,这里兜底
	default:
		return fmt.Errorf("unsupported mode %q (expected 1v1)", b.Mode)
	}
	return nil
}

// LoadTables loads all config tables from TableDir and auto-resolves SkillIDs if empty.
func (c *Config) LoadTables() {
	if c.TableDir == "" {
		zap.L().Info("table_dir not set, skipping table loading")
		return
	}

	// Load all tables (same as other Go services).
	table.LoadTables(c.TableDir, false)

	zap.L().Info("loaded tables",
		zap.String("table_dir", c.TableDir),
		zap.Int("skill_count", table.SkillTableManagerInstance.Count()),
		zap.Int("class_count", table.ClassTableManagerInstance.Count()),
	)

	// If skill_ids not configured, read all skill IDs from the skill table.
	if len(c.SkillIDs) == 0 {
		for _, row := range table.SkillTableManagerInstance.FindAll() {
			c.SkillIDs = append(c.SkillIDs, row.Id)
		}
		zap.L().Info("auto-resolved skill_ids from skill table", zap.Uint32s("skill_ids", c.SkillIDs))
	} else {
		// Validate configured skill_ids exist in the table.
		var valid []uint32
		for _, id := range c.SkillIDs {
			if table.SkillTableManagerInstance.Exists(id) {
				valid = append(valid, id)
			} else {
				zap.L().Warn("configured skill_id not found in Skill table, skipping", zap.Uint32("skill_id", id))
			}
		}
		c.SkillIDs = valid
	}
}

// EquipForgeTopupMaxCount 是一项物品单次能发的上限,与 GmGrantItem 的单次上限(equip-forge-smoke 的 equipForgeGrantMaxCount)一致。
const EquipForgeTopupMaxCount = 99

// EquipForgeTopupConfig 配置 "equip-forge-topup" 模式(docs/design/equipment-enhance-inlay.md §11.6):
// 用 GM 指令给一个开发账号补强化石 / 宝石 / 银两,然后退出。游戏里没有这些东西的产出,客户端也没有 GM 发物入口,
// 想在真实客户端里点一次强化 / 镶嵌,得先跑它。只发东西,不强化、不镶嵌、不改别的。
type EquipForgeTopupConfig struct {
	// Account 是要补货的账号,缺省 robot_9104(equip-forge-smoke 用的那个)。
	// 必须以 robot_ 开头:login 侧只对这个前缀走开发期密码认证。账号没有角色时自动建角。
	Account string `yaml:"account"`
	// Gold 是要加的银两(kCurrencyGold),0 = 不加。
	Gold int64 `yaml:"gold"`
	// Items 是要发的物品,每项 config_id + 数量(1..EquipForgeTopupMaxCount);同一个 config 可以写多项。
	Items []EquipForgeTopupItem `yaml:"items"`
}

// EquipForgeTopupItem 是 EquipForgeTopupConfig.Items 的一项。
type EquipForgeTopupItem struct {
	ConfigId uint32 `yaml:"config_id"`
	Count    uint32 `yaml:"count"`
}

// Validate 在发任何东西之前把配置错误拦下来(导出,好让 package main 的用例直接测)。
func (c *EquipForgeTopupConfig) Validate() error {
	if !strings.HasPrefix(c.Account, "robot_") {
		return fmt.Errorf("account must start with robot_ (got %q): login 只对这个前缀走开发期密码认证", c.Account)
	}
	if c.Gold < 0 {
		return fmt.Errorf("gold must be >= 0 (got %d)", c.Gold)
	}
	if c.Gold == 0 && len(c.Items) == 0 {
		return fmt.Errorf("nothing to grant: set gold and/or items")
	}
	for i, item := range c.Items {
		if item.ConfigId == 0 {
			return fmt.Errorf("items[%d].config_id must be set", i)
		}
		if item.Count == 0 || item.Count > EquipForgeTopupMaxCount {
			return fmt.Errorf("items[%d].count must be in 1..%d (got %d)", i, EquipForgeTopupMaxCount, item.Count)
		}
	}
	return nil
}
