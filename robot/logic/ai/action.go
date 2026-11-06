package ai

import "math/rand"

// Action represents a type of behavior a robot can perform.
type Action int

const (
	ActionIdle        Action = iota // do nothing this tick
	ActionMove                      // walk to a random nearby position
	ActionCastSkill                 // release a skill at a random target/position
	ActionSwitchScene               // request a scene/channel switch
	ActionChat                      // send a chat message
	// ActionJoinQueue 进匹配队列:不在队列里就入队(先地域探测),排满 match_queue.dwell_seconds 还没成局就撤单,
	// 成局后等这局结束再排。依赖 main 注入的 MatchQueue(RobotAI.SetMatchQueue),没注入时只记失败。
	ActionJoinQueue
)

var actionNames = map[Action]string{
	ActionIdle:        "idle",
	ActionMove:        "move",
	ActionCastSkill:   "cast_skill",
	ActionSwitchScene: "switch_scene",
	ActionChat:        "chat",
	ActionJoinQueue:   "join_queue",
}

func (a Action) String() string {
	if s, ok := actionNames[a]; ok {
		return s
	}
	return "unknown"
}

// ParseAction converts a string (from LLM output or config) to an Action.
func ParseAction(s string) (Action, bool) {
	for a, name := range actionNames {
		if name == s {
			return a, true
		}
	}
	return ActionIdle, false
}

// Profile defines behavior weights for each action type.
// Higher weight = robot picks that action more often.
type Profile struct {
	Name    string
	Weights map[Action]int
}

// Pick selects a random action according to the weight distribution.
func (p *Profile) Pick() Action {
	total := 0
	for _, w := range p.Weights {
		total += w
	}
	if total == 0 {
		return ActionIdle
	}

	r := rand.Intn(total)
	for action, w := range p.Weights {
		r -= w
		if r < 0 {
			return action
		}
	}
	return ActionIdle
}

// Built-in profiles. Users can also define weights in robot.yaml.
var BuiltinProfiles = map[string]Profile{
	"fighter": {
		Name: "fighter",
		Weights: map[Action]int{
			ActionCastSkill:   60,
			ActionMove:        20,
			ActionSwitchScene: 10,
			ActionIdle:        5,
			ActionChat:        5,
		},
	},
	"explorer": {
		Name: "explorer",
		Weights: map[Action]int{
			ActionMove:        40,
			ActionSwitchScene: 25,
			ActionIdle:        15,
			ActionCastSkill:   15,
			ActionChat:        5,
		},
	},
	"chatter": {
		Name: "chatter",
		Weights: map[Action]int{
			ActionChat:        35,
			ActionIdle:        25,
			ActionMove:        20,
			ActionCastSkill:   10,
			ActionSwitchScene: 10,
		},
	},
	"behavioral": {
		Name: "behavioral",
		Weights: map[Action]int{
			ActionCastSkill:   45,
			ActionMove:        25,
			ActionSwitchScene: 20,
			ActionIdle:        5,
			ActionChat:        5,
		},
	},
	// stress: combat-heavy spam with occasional scene switching for pressure tests.
	"stress": {
		Name: "stress",
		Weights: map[Action]int{
			ActionCastSkill:   85,
			ActionSwitchScene: 15,
		},
	},
	// match-stress:匹配压测(global-match-hierarchical-pools.md §11 R0)。在 stress 的场景负载上加入队 / 撤单,
	// 给按池撮合造排队深度与成局。stress 档的权重不动:历史压测按它对比(AGENTS.md §6.2)。
	// 入队参数见 robot.yaml 的 match_queue 段。
	"match-stress": {
		Name: "match-stress",
		Weights: map[Action]int{
			ActionCastSkill:   50,
			ActionJoinQueue:   40,
			ActionSwitchScene: 10,
		},
	},
}

// Weight 返回 a 在本档里的权重(没有这项为 0)。
func (p *Profile) Weight(a Action) int {
	if p == nil {
		return 0
	}
	return p.Weights[a]
}
