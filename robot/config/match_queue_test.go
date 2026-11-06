package config

import (
	"strings"
	"testing"
)

// 缺省入队参数本身合法;只开放会入队的模式;各数值越界都拒绝。
func TestMatchQueueConfigValidate(t *testing.T) {
	ok := MatchQueueConfig{Mode: "MATCH_MODE_1V1", BattleConfigID: 1, DwellSeconds: 30}
	if err := ok.Validate(); err != nil {
		t.Fatalf("default-shaped config rejected: %v", err)
	}
	for _, mode := range []string{"MATCH_MODE_PVE_SOLO", "MATCH_MODE_PVE_TEAM", "MATCH_MODE_PVP_TEAM"} {
		c := ok
		c.Mode = mode
		if err := c.Validate(); err != nil {
			t.Errorf("mode %s rejected: %v", mode, err)
		}
	}
	cases := []struct {
		name string
		edit func(*MatchQueueConfig)
		want string
	}{
		{"unknown mode", func(c *MatchQueueConfig) { c.Mode = "1v1" }, "not a queueing mode"},
		{"challenge does not queue", func(c *MatchQueueConfig) { c.Mode = "MATCH_MODE_PVP_CHALLENGE" }, "not a queueing mode"},
		{"5v5 not open", func(c *MatchQueueConfig) { c.Mode = "MATCH_MODE_5V5" }, "not a queueing mode"},
		{"zero battle config", func(c *MatchQueueConfig) { c.BattleConfigID = 0 }, "battle_config_id"},
		{"team size too big", func(c *MatchQueueConfig) { c.TeamSize = 6 }, "team_size"},
		{"zero dwell", func(c *MatchQueueConfig) { c.DwellSeconds = 0 }, "dwell_seconds"},
		{"dwell too long", func(c *MatchQueueConfig) { c.DwellSeconds = 601 }, "dwell_seconds"},
	}
	for _, tc := range cases {
		c := ok
		tc.edit(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want an error mentioning %q", tc.name, err, tc.want)
		}
	}
}

// 只有档位真会入队时才校验 match_queue:没用 join_queue 的压测不因这一段写错而起不来。
func TestMatchQueueValidatedOnlyWhenProfileJoinsQueue(t *testing.T) {
	bad := MatchQueueConfig{Mode: "nope"}
	stress := &Config{GatewayAddr: "http://127.0.0.1:8081", RobotCount: 1, AccountFmt: "robot_%04d", Profile: "stress", MatchQueue: bad}
	if err := stress.validate(); err != nil {
		t.Fatalf("stress profile must not validate match_queue: %v", err)
	}
	matchStress := *stress
	matchStress.Profile = "match-stress"
	if err := matchStress.validate(); err == nil || !strings.Contains(err.Error(), "match_queue") {
		t.Fatalf("match-stress profile with a bad match_queue: validate() = %v, want a match_queue error", err)
	}
	custom := *stress
	custom.Profile = ""
	custom.CustomWeights = map[string]int{"join_queue": 1}
	if err := custom.validate(); err == nil || !strings.Contains(err.Error(), "match_queue") {
		t.Fatalf("custom weights with join_queue and a bad match_queue: validate() = %v, want a match_queue error", err)
	}
}
