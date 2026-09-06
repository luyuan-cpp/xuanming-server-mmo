package config

import (
	"fmt"
	"os"

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
	// 见 BattleSmoke 块)。
	Mode string `yaml:"mode"`

	// DataStress configures the data-consistency stress mode.
	DataStress DataStressConfig `yaml:"data_stress"`

	// BattleSmoke 是 "battle-smoke" 模式的子开关(见 BattleSmokeConfig)。
	// 缺省(cross_zone=false)跑原 PVE + 观战流程。
	BattleSmoke BattleSmokeConfig `yaml:"battle_smoke"`

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

	// SkipDirectConnect=true 时机器人不走战斗直连(不连 BattleAssignedS2C 给的 battle
	// 节点地址),战斗消息全程经 gate 中继 —— 用来验证 D23 的回落路径仍然完整。
	// 缺省 false:收到 NotifyBattleAssigned 后凭票据直连 battle 节点,并断言回合结果
	// 与终局包确实从直连到达(docs/design/turn-based-battle-server.md §18)。
	SkipDirectConnect bool `yaml:"skip_direct_connect"`

	// ZoneA / ZoneB 是两个机器人各自登录的 zone;必须非 0 且不同
	// (同 zone 就不是跨 zone 冒烟,登录时 0 会被 server-list 自动选区顶掉)。
	ZoneA uint32 `yaml:"zone_a"`
	ZoneB uint32 `yaml:"zone_b"`

	// Mode 是排队模式,当前只支持 "1v1"(MATCH_MODE_1V1,两人凑对)。
	// 缺省 "1v1"。
	Mode string `yaml:"mode"`
}

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
	case "", "stress", "login-test", "data-stress", "currency-crash-snapshot", "battle-smoke", "attribute-smoke":
		// valid
	default:
		return fmt.Errorf("unknown mode %q (expected stress, login-test, data-stress, currency-crash-snapshot, battle-smoke, or attribute-smoke)", c.Mode)
	}
	if c.AuthType == "satoken" && c.SaTokenAddr == "" {
		return fmt.Errorf("satoken_addr must be set when auth_type is satoken")
	}
	if c.Mode == "battle-smoke" && c.BattleSmoke.CrossZone {
		if err := c.BattleSmoke.validate(); err != nil {
			return fmt.Errorf("battle_smoke: %w", err)
		}
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
