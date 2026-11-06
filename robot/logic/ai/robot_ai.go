package ai

import (
	"errors"
	"fmt"
	"math/rand"
	"time"

	"go.uber.org/zap"

	"proto/chat"
	"proto/common/component"
	"proto/scene"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"
	"robot/pkg"
)

// ErrMatchOutcomeUnknown 标记 MatchQueue.Join 的结局未知(超时、断线、路由层信封拒绝):请求可能已被 match 受理。
// RobotAI 按「已在队列里」处理,到时按请求号撤单 —— 服务端按 request_id 撤得掉结局未知的入队,并永久挡住它迟到入队。
var ErrMatchOutcomeUnknown = errors.New("match queue request outcome unknown")

// QueueTicket 是一次入队意图的撤单凭据。RequestID 是 JoinQueue 的请求号,Ticket 是受理(或「已在队列中」的拒绝)带回的票号;
// 撤单时两者都带,服务端认任一个。
type QueueTicket struct {
	RequestID string
	Ticket    string
}

// MatchQueue 是 join_queue 动作的匹配入口,由 main 注入:地域探测、按请求号认领回包与匹配协议都在 main 包
// (network_probe.go、smoke_rpc_call.go)。两个方法都在 RunLoop 的 goroutine 上同步调用,不得在 RecvLoop 上调用。
type MatchQueue interface {
	// Join 保证本会话持有尚未临近过期的探测结果,再发一次 JoinQueue。
	//   - 受理:返回凭据与 nil;
	//   - 结局未知:返回带请求号的凭据与包裹 ErrMatchOutcomeUnknown 的错误;
	//   - 业务拒绝:返回错误;「已在队列中」的拒绝带回票号时凭据里有 Ticket,调用方照样要撤掉它。
	Join() (QueueTicket, error)
	// Cancel 撤掉 t 这次入队意图。没有这张票(已成局、已过期)也不算错误;失败(含结局未知)返回错误,调用方下次再撤。
	Cancel(t QueueTicket) error
}

// RobotAI drives a single robot's in-game behavior after login+enter.
type RobotAI struct {
	client   *pkg.GameClient
	stats    *metrics.Stats
	profile  *Profile
	llm      *LLMAdvisor // nil = use profile weights only
	player   *gameobject.Player
	skillIDs []uint32 // fallback from config if GetSkillList not yet received
	interval time.Duration
	tick     int
	posX     float64
	posZ     float64

	// join_queue 动作的状态,只归 RunLoop 这一个 goroutine。
	queue      MatchQueue    // nil = 没注入,join_queue 只记失败
	queueDwell time.Duration // 排了这么久还没成局就撤单
	queued     QueueTicket   // 当前要撤的那次入队意图;RequestID 与 Ticket 都空 = 不在队列里
	queuedAt   time.Time
	now        func() time.Time // 单测注入;nil = time.Now
}

func NewRobotAI(client *pkg.GameClient, stats *metrics.Stats) *RobotAI {
	p := BuiltinProfiles["stress"] // backward-compatible default: skill-only
	return &RobotAI{
		client:   client,
		stats:    stats,
		profile:  &p,
		interval: 3 * time.Second,
	}
}

func (ai *RobotAI) SetSkillIDs(ids []uint32)       { ai.skillIDs = ids }
func (ai *RobotAI) SetInterval(d time.Duration)    { ai.interval = d }
func (ai *RobotAI) SetProfile(p *Profile)          { ai.profile = p }
func (ai *RobotAI) SetLLM(llm *LLMAdvisor)         { ai.llm = llm }
func (ai *RobotAI) SetPlayer(p *gameobject.Player) { ai.player = p }

// SetMatchQueue 给 join_queue 动作接上匹配入口;dwell 是排队多久还没成局就撤单(必须为正)。在 RunLoop 之前调用。
func (ai *RobotAI) SetMatchQueue(q MatchQueue, dwell time.Duration) {
	ai.queue = q
	ai.queueDwell = dwell
}

func (ai *RobotAI) clock() time.Time {
	if ai.now != nil {
		return ai.now()
	}
	return time.Now()
}

// RunLoop is a blocking loop that periodically performs actions.
func (ai *RobotAI) RunLoop(stop <-chan struct{}) {
	jitter := time.Duration(rand.Intn(2000)) * time.Millisecond
	time.Sleep(jitter)

	ticker := time.NewTicker(ai.interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			ai.tick++
			ai.doAction()
		}
	}
}

func (ai *RobotAI) doAction() {
	action := ai.pickAction()

	switch action {
	case ActionCastSkill:
		ai.castSkill()
	case ActionMove:
		ai.move()
	case ActionChat:
		ai.sendChat()
	case ActionSwitchScene:
		ai.switchScene()
	case ActionJoinQueue:
		ai.joinQueue()
	case ActionIdle:
		// do nothing
	}
}

// joinQueue 是 join_queue 动作:一次只推进一步,不在一拍里等成局。
//
//	在战斗里(已成局)       → 放下本地的入队意图(票随成局消费),记 in battle;
//	在队列里、没排满 dwell → 记 waiting;
//	在队列里、排满 dwell   → 撤单,失败就留着下次再撤;
//	不在队列里             → 入队。受理、结局未知、带回票号的拒绝都记成「在队列里」,到时撤掉。
//
// 收发计数由 MatchQueue 的实现记(它经 smokeCaller 发请求),这里只记行为。
func (ai *RobotAI) joinQueue() {
	start := ai.clock()
	if ai.queue == nil {
		ai.recordBehavior(ActionJoinQueue, start, false, "match queue not configured")
		return
	}
	if ai.player != nil && ai.player.InBattle() {
		ai.queued = QueueTicket{}
		ai.recordBehavior(ActionJoinQueue, start, true, "in battle")
		return
	}
	if ai.queued != (QueueTicket{}) {
		if waited := start.Sub(ai.queuedAt); waited < ai.queueDwell {
			ai.recordBehavior(ActionJoinQueue, start, true, fmt.Sprintf("waiting in queue %s", waited.Truncate(time.Second)))
			return
		}
		if err := ai.queue.Cancel(ai.queued); err != nil {
			ai.recordBehavior(ActionJoinQueue, start, false, "cancel: "+err.Error())
			return
		}
		ai.queued = QueueTicket{}
		ai.recordBehavior(ActionJoinQueue, start, true, fmt.Sprintf("cancelled after %s", ai.queueDwell))
		return
	}
	ticket, err := ai.queue.Join()
	if err == nil || errors.Is(err, ErrMatchOutcomeUnknown) || ticket.Ticket != "" {
		ai.queued, ai.queuedAt = ticket, start
	}
	if err != nil {
		ai.recordBehavior(ActionJoinQueue, start, false, "join: "+err.Error())
		return
	}
	ai.recordBehavior(ActionJoinQueue, start, true, "joined")
}

// pickAction decides what to do next — either via LLM or weighted random.
func (ai *RobotAI) pickAction() Action {
	if ai.llm != nil {
		state := GameState{
			PlayerId:  ai.client.PlayerId,
			PosX:      ai.posX,
			PosZ:      ai.posZ,
			SkillIDs:  ai.skillIDs,
			TickCount: ai.tick,
		}
		if ai.player != nil {
			state.SceneID = ai.player.GetSceneID()
			state.SceneConfigID = ai.player.GetSceneConfigID()
		}
		action, reason := ai.llm.DecideAction(state)
		zap.L().Debug("llm decision",
			zap.String("action", action.String()),
			zap.String("reason", reason),
		)
		return action
	}
	return ai.profile.Pick()
}

// --- Action implementations (flat, no deep call chain) ---

func (ai *RobotAI) recordBehavior(action Action, start time.Time, success bool, detail string) {
	if ai.stats == nil {
		return
	}
	var sceneID uint64
	if ai.player != nil {
		sceneID = ai.player.GetSceneID()
	}
	ai.stats.RecordBehavior(metrics.BehaviorRecord{
		Timestamp: time.Now(),
		Account:   ai.client.Account,
		Scenario:  "stress",
		Action:    action.String(),
		Success:   success,
		LatencyMs: time.Since(start).Milliseconds(),
		Detail:    detail,
		SceneID:   sceneID,
	})
}

func (ai *RobotAI) castSkill() {
	start := time.Now()
	// Use server-reported owned skills; fall back to config/table skills only
	// when no player object exists yet (pre-login phase).
	var skillIDs []uint32
	if ai.player != nil {
		skillIDs = ai.player.GetOwnedSkillIDs()
	}
	if len(skillIDs) == 0 && ai.player == nil {
		skillIDs = ai.skillIDs
	}
	if len(skillIDs) == 0 {
		ai.recordBehavior(ActionCastSkill, start, false, "no owned skills")
		return
	}

	skillID := skillIDs[rand.Intn(len(skillIDs))]
	x := ai.posX + rand.Float64()*20 - 10
	z := ai.posZ + rand.Float64()*20 - 10

	// Pick a target entity (self or nearby). Server requires target_id > 0.
	var targetID uint64
	if ai.player != nil {
		targetID = ai.player.GetRandomEntity()
	}
	if targetID == 0 {
		ai.recordBehavior(ActionCastSkill, start, false, "no visible target entity")
		return
	}

	err := ai.client.SendRequest(game.SceneSkillClientPlayerReleaseSkillMessageId, &scene.ReleaseSkillRequest{
		SkillTableId: skillID,
		TargetId:     targetID,
		Position:     &component.Vector3{X: x, Y: 0, Z: z},
	})
	if err != nil {
		zap.L().Warn("cast skill failed", zap.Error(err))
		ai.recordBehavior(ActionCastSkill, start, false, err.Error())
		return
	}
	ai.stats.SkillSent()
	ai.stats.MsgSent()
	ai.recordBehavior(ActionCastSkill, start, true, fmt.Sprintf("skill_id=%d target=%d", skillID, targetID))
}

func (ai *RobotAI) move() {
	start := time.Now()
	// Walk to a random nearby position.
	dx := rand.Float64()*30 - 15
	dz := rand.Float64()*30 - 15
	ai.posX += dx
	ai.posZ += dz

	// Use SceneInfoC2S to report new position (if the server supports it).
	err := ai.client.SendRequest(game.SceneSceneClientPlayerSceneInfoC2SMessageId, &scene.SceneInfoRequest{})
	if err != nil {
		zap.L().Warn("move failed", zap.Error(err))
		ai.recordBehavior(ActionMove, start, false, err.Error())
		return
	}
	ai.stats.MsgSent()
	ai.recordBehavior(ActionMove, start, true, fmt.Sprintf("pos=(%.1f,%.1f)", ai.posX, ai.posZ))
}

func (ai *RobotAI) switchScene() {
	start := time.Now()
	if ai.player == nil {
		ai.recordBehavior(ActionSwitchScene, start, false, "player state unavailable")
		return
	}
	sceneConfID := ai.player.GetSceneConfigID()
	if sceneConfID == 0 {
		ai.recordBehavior(ActionSwitchScene, start, false, "scene config unavailable")
		return
	}

	err := ai.client.SendRequest(game.SceneSceneClientPlayerEnterSceneMessageId, &scene.EnterSceneC2SRequest{
		SceneInfo: &scene.SceneInfoComp{SceneConfigId: sceneConfID},
	})
	if err != nil {
		zap.L().Warn("switch scene failed", zap.Error(err))
		ai.recordBehavior(ActionSwitchScene, start, false, err.Error())
		return
	}
	ai.stats.SceneSwitchSent()
	ai.stats.MsgSent()
	ai.recordBehavior(ActionSwitchScene, start, true, fmt.Sprintf("scene_conf_id=%d", sceneConfID))
}

func (ai *RobotAI) sendChat() {
	start := time.Now()
	messages := []string{
		"hello!", "anyone here?", "gg", "let's go", "lol",
		"need heal", "follow me", "good fight", "afk",
	}
	msg := messages[rand.Intn(len(messages))]

	err := ai.client.SendRequest(game.ClientPlayerChatSendChatMessageId, &chat.SendChatRequest{
		Message: &chat.ChatMessage{
			Content: fmt.Sprintf("[robot] %s", msg),
		},
	})
	if err != nil {
		zap.L().Warn("chat failed", zap.Error(err))
		ai.recordBehavior(ActionChat, start, false, err.Error())
		return
	}
	ai.stats.MsgSent()
	ai.recordBehavior(ActionChat, start, true, msg)
}
