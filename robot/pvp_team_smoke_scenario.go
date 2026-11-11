package main

// pvp-team-smoke 场景:人数档真人 PVP(PVP_TEAM,1V1～5V5)计分组排的端到端冒烟,覆盖
// docs/design/global-match-hierarchical-pools.md 开头「人数档真人 PVP 与计分组排」第 1～10 条的全部情况(§12.5 第 5 步)。
//
// 两个阶段(pvp_team_smoke.phase):
//
//	prepare:只登录要写评分的账号(首次登录即建角),把「账号 → player_id → 本模式评分」写进 fixture_path 后退出;
//	         之后由 go/match/cmd/ratingfixture 把夹具写进 match_rating(评分按 (player_id, mode) 存,R5)。
//	run:    登录全部账号,读夹具核对 player_id 与评分没变,清掉上一轮残留的队伍与排队,按人数档分组并发跑全部情况。
//	         人数档不同就是不同的匹配池,互不干扰;同一人数档里的情况顺序跑,前一种全员开战之后池里不剩人。
//
// 情况(每种情况用自己的账号,账号之间不共用;评分夹具每轮都重写,所以看评分的情况可以反复跑):
//
//	1v1                        单人对单人(每侧 1 人档也走 PVP_TEAM)
//	1v1-rating-window          1500 对 1700:起始容差 ±100 不成局,锚点等满一档、容差放宽到 ±200 后成局
//	2v2-duo-plus-solos         双排 + 两个单人:双排在同一侧;打完后全员在 PVP_TEAM 的榜上按 Elo 结算,1V1 的榜上没有他们
//	2v2-duo-vs-duo             双排对双排
//	2v2-party-too-big          3 人队开 2V2:TeamSizeExceeded,没人进队列
//	2v2-rating-gap             队员评分差超过上限:TeamRatingGapTooLarge;差恰好等于上限的另一组可以排队,排队中队员离队整队撤单
//	2v2-rated-balance          四个单人 1500 / 1600 / 1400 / 1500:容差放宽后成局,两个 1500 同侧(两侧总分相等)
//	3v3-duo-solo-vs-solos      双排 + 四个单人:车队差 1,立即成局
//	3v3-trio-waits-for-solos   3 人队对 3 个单人:车队差 2,单人等满一个车队差步长才成局
//	4v4-four-stack             4 人队开 5V5 被拒(TeamPvpPartySizeNotAllowed,队伍不进入匹配);同一队开 4V4 可以排队,
//	                           对 4 个单人车队差 3,单人等满两个步长才成局
//	5v5-trio-duo-vs-trio-duo   两个 3 人队 + 两支双排:每侧一个 3 人队加一支双排
//	5v5-duos-split             两支双排 + 六个单人:两支双排分在两侧(车队差 0 优先于 1)
//	5v5-stack-vs-stack         5 人队对 5 人队,立即成局
//	5v5-stack-waits-for-solos  5 人队对 5 个单人:车队差 4,单人等满三个步长才成局
//
// 成局的情况都核对:全员进同一局;每支队伍整体在同一侧;两侧各 N 人;再全员直连自动战斗打到终局。
// 「规则放宽之前不该成局」按规则阈值减 pvpSmokeQuietMargin 观察,之后再给 pvpSmokeAfterThresholdTimeout 等开战。
//
// 结果约定(供外层脚本消费):
//
//	prepare 完成 → 日志一行 `PVP_TEAM_SMOKE_PREPARED fixture=… ratings=…`,退出码 0;
//	run 全过     → 日志一行 `PVP_TEAM_SMOKE_OK cases=… battles=…`,退出码 0;
//	任一失败     → `PVP_TEAM_SMOKE_FAIL case=… reason=…`,退出码 1。
//
// 复用 team_smoke_scenario.go 的 teamSmokeBot(登录进场、按请求号认领回包、推送留底、战斗直连与自动战斗)与
// team_backfill_smoke_scenario.go 的入队、撤队、排队状态断言。

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"proto/battle"
	"proto/match"
	rankpb "proto/rank"
	teampb "proto/team"
	"robot/config"
	"robot/generated/pb/game"
	"robot/logic/gameobject"
	"robot/metrics"

	tiptable "shared/generated/pb/table"
)

const (
	// 账号前缀必须是 robot_ 才走开发用密码认证;87xx 段归本场景(其它冒烟占用的段见 team_smoke_scenario.go 的账号注释)。
	pvpSmokeAccountFmt = "robot_%04d"

	// 立即可成的情况等开战的预算:撮合每 500ms 一轮,加备战建房,与 teamSmokeBattleStartTimeout 同值。
	pvpSmokeImmediateTimeout = teamSmokeBattleStartTimeout
	// 等规则放宽的情况:过了阈值之后再给这么久开战。
	pvpSmokeAfterThresholdTimeout = 25 * time.Second
	// 「阈值之前不该成局」的观察截止比阈值早这么多,吸收撮合轮间隔、入队往返与两端时钟差。
	pvpSmokeQuietMargin = 2 * time.Second
	// 登录与清场的并发上限:一次几十个账号时不压垮 gateway / login。
	pvpSmokeParallelism = 8
	// 等写榜的预算与读榜间隔:评分入账 → 投榜 outbox → rank 写榜都是异步的;gate 对读榜默认限流 3 次 / 窗口。
	pvpSmokeRankTimeout      = 30 * time.Second
	pvpSmokeRankPollInterval = 1500 * time.Millisecond
	// 评分榜的 board_type(authority.rankBoardTypes,R5):1V1 是 1,PVP_TEAM 是 3。其余键固定为全服、"all"、第 1 代。
	pvpSmokeBoardOneVOne = 1
	pvpSmokeBoardPvpTeam = 3
	// 没有评分记录的玩家按 1500.00(ratingrule.DefaultCenti)。
	pvpSmokeDefaultRating int64 = 150_000
	// 评分差那组的队长评分;另两人按组队评分差上限往上加。
	pvpSmokeGapBaseRating int64 = 120_000
)

// tip id 一律读导表器生成的枚举,不写字面量(AGENTS.md §7 不变量 5)。
var (
	tipTeamSizeExceeded      = uint32(tiptable.TeamError_kTeamSizeExceeded)
	tipTeamPvpPartySize      = uint32(tiptable.TeamError_kTeamPvpPartySizeNotAllowed)
	tipTeamRatingGapTooLarge = uint32(tiptable.TeamError_kTeamRatingGapTooLarge)
	tipTeamNoTeam            = uint32(tiptable.TeamError_kTeamHasNotTeamId)
)

// pvpSmokeWait 描述一种情况要等哪条规则放宽:两项最多一项非 0。
type pvpSmokeWait struct {
	// gapSteps:车队差要吃亏一侧等满这么多个车队差步长(从最后一个单位入队起算,吃亏的单人排在最后)。
	gapSteps uint32
	// toleranceSteps:评分差要锚点等满这么多档容差(从第一个单位入队起算,锚点排在最前)。
	toleranceSteps uint32
}

// pvpSmokeCase 是一种情况。units 按入队顺序列出撮合单位,每个单位是账号编号(robot_%04d 里的数字)列表,
// 第一个是队长,单人是长度 1。
type pvpSmokeCase struct {
	name    string
	perSide uint32
	units   [][]int
	// formsBattle:这种情况最后成一局(否则只断言拒绝,没人进局)。
	formsBattle bool
	wait        pvpSmokeWait
	// together / opposite:必须同侧 / 必须分在两侧的单位下标对(队伍整体同侧、两侧各 N 人总是核对)。
	together [][2]int
	opposite [][2]int
	// settleRatings:打完后核对 PVP_TEAM 榜上的 Elo 结算,并确认 1V1 的榜上没有这些人。
	settleRatings bool
	// ratings 返回要写进 match_rating 的评分(账号编号 → ×100),按配置算;nil = 都用默认 1500。
	ratings func(cfg *config.PvpTeamSmokeConfig) map[int]int64
	// run 为 nil 时走标准流程(pvpSmokeCaseRun.standard)。
	run func(*pvpSmokeCaseRun) error
}

// pvpSmokeCases 是全部情况。账号段:1 人档 8701-8704,2 人档 8711-8728,3 人档 8731-8742,4 人档 8751-8758,5 人档 8760-8799。
func pvpSmokeCases() []*pvpSmokeCase {
	return []*pvpSmokeCase{
		{name: "1v1", perSide: 1, units: [][]int{{8701}, {8702}}, formsBattle: true},
		{name: "1v1-rating-window", perSide: 1, units: [][]int{{8703}, {8704}}, formsBattle: true,
			wait: pvpSmokeWait{toleranceSteps: 1},
			ratings: func(cfg *config.PvpTeamSmokeConfig) map[int]int64 {
				// 分差 = 起始容差 + 一档:起始容差内配不上,锚点等满一档就配上。
				return map[int]int64{8703: pvpSmokeDefaultRating, 8704: pvpSmokeDefaultRating + pvpSmokeCenti(cfg.ToleranceBase()+cfg.ToleranceStepDelta())}
			}},
		{name: "2v2-duo-plus-solos", perSide: 2, units: [][]int{{8711, 8712}, {8713}, {8714}}, formsBattle: true, settleRatings: true},
		{name: "2v2-duo-vs-duo", perSide: 2, units: [][]int{{8715, 8716}, {8717, 8718}}, formsBattle: true},
		{name: "2v2-party-too-big", perSide: 2, units: [][]int{{8719, 8720, 8721}}, run: (*pvpSmokeCaseRun).partyTooBig},
		{name: "2v2-rating-gap", perSide: 2, units: [][]int{{8722, 8723}, {8724}}, run: (*pvpSmokeCaseRun).ratingGap,
			ratings: func(cfg *config.PvpTeamSmokeConfig) map[int]int64 {
				limit := pvpSmokeCenti(cfg.RatingGapMax())
				return map[int]int64{8722: pvpSmokeGapBaseRating, 8723: pvpSmokeGapBaseRating + limit + pvpSmokeCenti(100), 8724: pvpSmokeGapBaseRating + limit}
			}},
		{name: "2v2-rated-balance", perSide: 2, units: [][]int{{8725}, {8726}, {8727}, {8728}}, formsBattle: true,
			wait:     pvpSmokeWait{toleranceSteps: 1},
			together: [][2]int{{0, 3}, {1, 2}},
			ratings: func(cfg *config.PvpTeamSmokeConfig) map[int]int64 {
				half := pvpSmokeCenti((cfg.ToleranceBase() + cfg.ToleranceStepDelta()) / 2)
				return map[int]int64{8725: pvpSmokeDefaultRating, 8726: pvpSmokeDefaultRating + half, 8727: pvpSmokeDefaultRating - half, 8728: pvpSmokeDefaultRating}
			}},
		{name: "3v3-duo-solo-vs-solos", perSide: 3, units: [][]int{{8731, 8732}, {8733}, {8734}, {8735}, {8736}}, formsBattle: true},
		{name: "3v3-trio-waits-for-solos", perSide: 3, units: [][]int{{8737, 8738, 8739}, {8740}, {8741}, {8742}}, formsBattle: true,
			wait: pvpSmokeWait{gapSteps: 1}},
		{name: "4v4-four-stack", perSide: 4, units: [][]int{{8751, 8752, 8753, 8754}, {8755}, {8756}, {8757}, {8758}}, formsBattle: true,
			wait: pvpSmokeWait{gapSteps: 2}, run: (*pvpSmokeCaseRun).fourStack},
		{name: "5v5-trio-duo-vs-trio-duo", perSide: 5, units: [][]int{{8760, 8761, 8762}, {8763, 8764}, {8765, 8766, 8767}, {8768, 8769}}, formsBattle: true,
			opposite: [][2]int{{0, 2}, {1, 3}}},
		{name: "5v5-duos-split", perSide: 5, units: [][]int{{8770, 8771}, {8772, 8773}, {8774}, {8775}, {8776}, {8777}, {8778}, {8779}}, formsBattle: true,
			opposite: [][2]int{{0, 1}}},
		{name: "5v5-stack-vs-stack", perSide: 5, units: [][]int{{8780, 8781, 8782, 8783, 8784}, {8785, 8786, 8787, 8788, 8789}}, formsBattle: true},
		{name: "5v5-stack-waits-for-solos", perSide: 5, units: [][]int{{8790, 8791, 8792, 8793, 8794}, {8795}, {8796}, {8797}, {8798}, {8799}}, formsBattle: true,
			wait: pvpSmokeWait{gapSteps: 3}},
	}
}

// pvpSmokeCenti 把评分点换成 ×100。
func pvpSmokeCenti(points uint32) int64 { return int64(points) * 100 }

func pvpSmokeAccount(number int) string { return fmt.Sprintf(pvpSmokeAccountFmt, number) }

// pvpSmokeSelectCases 按名字挑出要跑的情况,保持定义顺序;names 为空 = 全部。不认识的名字直接报错。
func pvpSmokeSelectCases(all []*pvpSmokeCase, names []string) ([]*pvpSmokeCase, error) {
	if len(names) == 0 {
		return all, nil
	}
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	var selected []*pvpSmokeCase
	for _, c := range all {
		if wanted[c.name] {
			selected = append(selected, c)
			delete(wanted, c.name)
		}
	}
	if len(wanted) > 0 {
		var unknown []string
		for name := range wanted {
			unknown = append(unknown, name)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown pvp_team_smoke.cases %v", unknown)
	}
	return selected, nil
}

// pvpSmokeSeeds 汇总所选情况要写的评分(账号编号 → ×100)。
func pvpSmokeSeeds(cases []*pvpSmokeCase, cfg *config.PvpTeamSmokeConfig) map[int]int64 {
	seeds := map[int]int64{}
	for _, c := range cases {
		if c.ratings == nil {
			continue
		}
		for number, rating := range c.ratings(cfg) {
			seeds[number] = rating
		}
	}
	return seeds
}

// pvpSmokeCheckRuleConfig 确认配置能把看评分的情况区分开:分差要大于起始容差(否则立即成局),
// 又不超过一档之后的容差(否则一档之后还配不上)。
func pvpSmokeCheckRuleConfig(cfg *config.PvpTeamSmokeConfig) error {
	base, delta := cfg.ToleranceBase(), cfg.ToleranceStepDelta()
	if (base+delta)/2*2 <= base {
		return fmt.Errorf("rating_tolerance_step_delta=%d is too small next to rating_tolerance_base=%d to tell the rated cases apart", delta, base)
	}
	return nil
}

func pvpSmokeAccounts(cases []*pvpSmokeCase) []int {
	var numbers []int
	for _, c := range cases {
		for _, unit := range c.units {
			numbers = append(numbers, unit...)
		}
	}
	return numbers
}

func pvpSmokeSortedNumbers(seeds map[int]int64) []int {
	numbers := make([]int, 0, len(seeds))
	for number := range seeds {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	return numbers
}

// RunPvpTeamSmoke 是 main.go `mode: pvp-team-smoke` 的入口。任一步失败以退出码 1 结束进程;全部通过正常返回(退出码 0)。
func RunPvpTeamSmoke(cfg *config.Config) {
	loginTestCfg = cfg // loginAndEnterScenario 从这个包级变量读认证方式

	stats := robotStatsRef
	if stats == nil {
		stats = metrics.NewStats()
	}
	session := newPvpSmokeSession(stats)
	sc := &cfg.PvpTeamSmoke
	cases, err := pvpSmokeSelectCases(pvpSmokeCases(), sc.Cases)
	if err != nil {
		session.fail("select-cases", err)
	}
	if err := pvpSmokeCheckRuleConfig(sc); err != nil {
		session.fail("config", err)
	}
	seeds := pvpSmokeSeeds(cases, sc)
	if sc.Phase == config.PvpTeamSmokePhasePrepare {
		session.prepare(cfg, sc, seeds)
		return
	}
	session.runCases(cfg, sc, cases, seeds)
}

// ---- 会话:登录、清场、失败退出 ----

// pvpSmokeSession 持有本进程登录的全部机器人。登录完成后 bots 只读。
type pvpSmokeSession struct {
	stats    *metrics.Stats
	mu       sync.Mutex
	bots     map[int]*teamSmokeBot
	failOnce sync.Once
}

func newPvpSmokeSession(stats *metrics.Stats) *pvpSmokeSession {
	return &pvpSmokeSession{stats: stats, bots: map[int]*teamSmokeBot{}}
}

func (s *pvpSmokeSession) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, bot := range s.bots {
		_ = leaveGame(bot.gc, s.stats)
		gameobject.PlayerList.Delete(bot.gc.PlayerId)
		bot.gc.Close()
	}
	s.bots = map[int]*teamSmokeBot{}
}

// fail 打出结果行、登出全部机器人并以退出码 1 结束进程。各人数档的情况并发跑,只有第一个失败生效。
func (s *pvpSmokeSession) fail(caseName string, err error) {
	s.failOnce.Do(func() {
		zap.L().Error(fmt.Sprintf("PVP_TEAM_SMOKE_FAIL case=%s reason=%v", caseName, err))
		s.cleanup()
		_ = zap.L().Sync()
		os.Exit(1)
	})
	select {} // 另一个 goroutine 正在退出进程
}

// loginAll 并发(有上限)登录 numbers 里的账号,全部进同一个 zone。
func (s *pvpSmokeSession) loginAll(cfg *config.Config, zone uint32, numbers []int) error {
	errs := make([]error, len(numbers))
	pvpSmokeForEach(len(numbers), func(i int) {
		zoneCfg := *cfg
		zoneCfg.ZoneID = zone
		account := pvpSmokeAccount(numbers[i])
		bot, err := teamSmokeLogin(&zoneCfg, account, account, s.stats)
		if err != nil {
			errs[i] = fmt.Errorf("login %s zone=%d: %w", account, zone, err)
			return
		}
		s.mu.Lock()
		s.bots[numbers[i]] = bot
		s.mu.Unlock()
	})
	return errors.Join(errs...)
}

// reset 把机器人带回「不在任何队伍、不在任何队列」:上一轮失败可能留下队伍、成员关系或排队。
// 同一支残留队伍的队员可能被并发清场:队长解散之后队员再离队会回「你还没有队伍」,按已离队处理。
func (s *pvpSmokeSession) reset(numbers []int) error {
	errs := make([]error, len(numbers))
	pvpSmokeForEach(len(numbers), func(i int) {
		bot := s.bots[numbers[i]]
		if err := pvpSmokeLeaveTeam(bot); err != nil {
			errs[i] = err
			return
		}
		if err := backfillCancelQueue(bot); err != nil {
			errs[i] = err
			return
		}
		if err := backfillExpectQueueState(bot, match.QueueState_QUEUE_STATE_NOT_QUEUED, teamSmokeSettleTimeout); err != nil {
			errs[i] = fmt.Errorf("%w (a previous run may have stopped mid-battle; rerun after its battle ends)", err)
		}
	})
	return errors.Join(errs...)
}

func pvpSmokeLeaveTeam(bot *teamSmokeBot) error {
	for attempt := 0; attempt < 3; attempt++ {
		view, err := bot.myTeam()
		if err != nil {
			return err
		}
		tid := view.GetTeamId()
		if tid == 0 {
			return nil
		}
		resp := &teampb.TeamResponse{}
		var tip uint32
		if view.GetLeaderId() == bot.gc.PlayerId {
			tip, err = bot.rpc(game.ClientPlayerTeamDisbandTeamMessageId, &teampb.DisbandTeamRequest{ExpectedTeamId: tid}, resp)
		} else {
			tip, err = bot.rpc(game.ClientPlayerTeamLeaveTeamMessageId, &teampb.LeaveTeamRequest{ExpectedTeamId: tid}, resp)
		}
		if err != nil {
			return fmt.Errorf("%s leave leftover team %d: %w", bot.name, tid, err)
		}
		switch tip {
		case 0, tipTeamNoTeam:
			continue
		case tipTeamInMatch:
			return fmt.Errorf("%s leftover team %d still holds a match lock (a previous run died mid-start; the lock expires within about 101s)", bot.name, tid)
		default:
			return fmt.Errorf("%s leave leftover team %d: tip=%d parameters=%v", bot.name, tid, tip, resp.GetErrorMessage().GetParameters())
		}
	}
	return fmt.Errorf("%s is still in a team after 3 leave attempts", bot.name)
}

// pvpSmokeForEach 以 pvpSmokeParallelism 为并发上限对 0..n-1 调 fn,全部返回后才返回。
func pvpSmokeForEach(n int, fn func(int)) {
	slots := make(chan struct{}, pvpSmokeParallelism)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		slots <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-slots }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// ---- 评分夹具 ----

// pvpSmokeFixture 是评分夹具文件的格式,与 go/match/cmd/ratingfixture 读的一致。
type pvpSmokeFixture struct {
	Mode    uint32                 `json:"mode"`
	Ratings []pvpSmokeFixtureEntry `json:"ratings"`
}

type pvpSmokeFixtureEntry struct {
	Account     string `json:"account"`
	PlayerID    uint64 `json:"player_id"`
	RatingCenti int64  `json:"rating_centi"`
}

func pvpSmokeWriteFixture(path string, fixture pvpSmokeFixture) error {
	body, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o644)
}

func pvpSmokeReadFixture(path string) (pvpSmokeFixture, error) {
	var fixture pvpSmokeFixture
	body, err := os.ReadFile(path)
	if err != nil {
		return fixture, err
	}
	err = json.Unmarshal(body, &fixture)
	return fixture, err
}

// prepare 登录要写评分的账号,写出评分夹具。
func (s *pvpSmokeSession) prepare(cfg *config.Config, sc *config.PvpTeamSmokeConfig, seeds map[int]int64) {
	numbers := pvpSmokeSortedNumbers(seeds)
	if err := s.loginAll(cfg, sc.Zone, numbers); err != nil {
		s.fail("prepare-login", err)
	}
	fixture := pvpSmokeFixture{Mode: uint32(match.MatchMode_MATCH_MODE_PVP_TEAM)}
	for _, number := range numbers {
		bot := s.bots[number]
		fixture.Ratings = append(fixture.Ratings, pvpSmokeFixtureEntry{Account: bot.account, PlayerID: bot.gc.PlayerId, RatingCenti: seeds[number]})
	}
	if err := pvpSmokeWriteFixture(sc.FixturePath, fixture); err != nil {
		s.fail("prepare-write-fixture", fmt.Errorf("write %s: %w", sc.FixturePath, err))
	}
	zap.L().Info(fmt.Sprintf("PVP_TEAM_SMOKE_PREPARED fixture=%s ratings=%d", sc.FixturePath, len(fixture.Ratings)))
	s.cleanup()
	_ = zap.L().Sync()
}

// verifyFixture 核对夹具文件覆盖了本轮要的每个评分,且 player_id 与评分都没变(换库或改了配置都要重跑 prepare)。
// 夹具是否已经写进 match_rating 从客户端看不到;没写的话看评分的情况会失败,失败原因里带提示。
func (s *pvpSmokeSession) verifyFixture(path string, seeds map[int]int64) error {
	if len(seeds) == 0 {
		return nil
	}
	fixture, err := pvpSmokeReadFixture(path)
	if err != nil {
		return fmt.Errorf("read rating fixture %s (run phase=prepare and go/match/cmd/ratingfixture first): %w", path, err)
	}
	if fixture.Mode != uint32(match.MatchMode_MATCH_MODE_PVP_TEAM) {
		return fmt.Errorf("rating fixture %s has mode=%d, expected PVP_TEAM=%d", path, fixture.Mode, match.MatchMode_MATCH_MODE_PVP_TEAM)
	}
	byAccount := map[string]pvpSmokeFixtureEntry{}
	for _, entry := range fixture.Ratings {
		byAccount[entry.Account] = entry
	}
	for _, number := range pvpSmokeSortedNumbers(seeds) {
		bot := s.bots[number]
		entry, ok := byAccount[bot.account]
		if !ok || entry.PlayerID != bot.gc.PlayerId || entry.RatingCenti != seeds[number] {
			return fmt.Errorf("rating fixture %s is stale for %s (fixture player_id=%d rating=%d, expected player_id=%d rating=%d): "+
				"rerun phase=prepare and go/match/cmd/ratingfixture", path, bot.account, entry.PlayerID, entry.RatingCenti, bot.gc.PlayerId, seeds[number])
		}
	}
	return nil
}

// runCases 登录、核对夹具、清场,然后按人数档分组并发跑。
func (s *pvpSmokeSession) runCases(cfg *config.Config, sc *config.PvpTeamSmokeConfig, cases []*pvpSmokeCase, seeds map[int]int64) {
	numbers := pvpSmokeAccounts(cases)
	if err := s.loginAll(cfg, sc.Zone, numbers); err != nil {
		s.fail("login", err)
	}
	if err := s.verifyFixture(sc.FixturePath, seeds); err != nil {
		s.fail("fixture", err)
	}
	if err := s.reset(numbers); err != nil {
		s.fail("reset", err)
	}
	zap.L().Info("[pvp-team-smoke] robots ready", zap.Int("robots", len(numbers)), zap.Int("cases", len(cases)),
		zap.Uint32("zone", sc.Zone), zap.Uint32("battle_config_id", sc.BattleConfigId))

	groups := map[uint32][]*pvpSmokeCase{}
	var order []uint32
	for _, c := range cases {
		if _, ok := groups[c.perSide]; !ok {
			order = append(order, c.perSide)
		}
		groups[c.perSide] = append(groups[c.perSide], c)
	}
	var battles atomic.Int64
	var wg sync.WaitGroup
	for _, perSide := range order {
		wg.Add(1)
		go func(group []*pvpSmokeCase) {
			defer wg.Done()
			for _, c := range group {
				started := time.Now()
				run := &pvpSmokeCaseRun{cfg: sc, c: c, session: s, tids: make([]uint64, len(c.units)), marks: map[uint64]int{}}
				body := c.run
				if body == nil {
					body = (*pvpSmokeCaseRun).standard
				}
				if err := body(run); err != nil {
					s.fail(c.name, err)
				}
				if run.battleId != 0 {
					battles.Add(1)
				}
				zap.L().Info("[pvp-team-smoke] case passed", zap.String("case", c.name), zap.Uint64("battle_id", run.battleId),
					zap.Duration("elapsed", time.Since(started)))
			}
		}(groups[perSide])
	}
	wg.Wait()
	zap.L().Info(fmt.Sprintf("PVP_TEAM_SMOKE_OK cases=%d battles=%d", len(cases), battles.Load()))
	s.cleanup()
	_ = zap.L().Sync()
}

// ---- 单种情况 ----

// pvpSmokeCaseRun 是一种情况的执行状态,只由跑这种情况的那个 goroutine 读写(战斗阶段每名机器人各一个 goroutine,
// 只读这里的字段)。
type pvpSmokeCaseRun struct {
	cfg      *config.PvpTeamSmokeConfig
	c        *pvpSmokeCase
	session  *pvpSmokeSession
	tids     []uint64       // 每个单位的队伍 id;单人为 0
	marks    map[uint64]int // player_id → 入队之前的推送留底位置
	battleId uint64
}

func (r *pvpSmokeCaseRun) unit(i int) []*teamSmokeBot {
	bots := make([]*teamSmokeBot, len(r.c.units[i]))
	for j, number := range r.c.units[i] {
		bots[j] = r.session.bots[number]
	}
	return bots
}

func (r *pvpSmokeCaseRun) allBots() []*teamSmokeBot {
	var bots []*teamSmokeBot
	for i := range r.c.units {
		bots = append(bots, r.unit(i)...)
	}
	return bots
}

// standard:组队 → 入队 →(要等规则放宽的)确认阈值之前没成局 → 全员进同一局 → 核对分边 → 打到终局 →(需要的)核对评分结算。
func (r *pvpSmokeCaseRun) standard() error {
	if err := r.formParties(); err != nil {
		return err
	}
	before, err := r.ratingsBefore()
	if err != nil {
		return err
	}
	firstAt, lastAt, err := r.enqueue()
	if err != nil {
		return err
	}
	timeout := pvpSmokeImmediateTimeout
	if deadline, why, ok := r.quietDeadline(firstAt, lastAt); ok {
		if err := r.expectQuiet(deadline, why); err != nil {
			return err
		}
		timeout = pvpSmokeQuietMargin + pvpSmokeAfterThresholdTimeout
	}
	sides, err := r.awaitBattle(timeout)
	if err != nil {
		return err
	}
	if err := r.checkSides(sides); err != nil {
		return err
	}
	outcome, err := r.fight()
	if err != nil {
		return err
	}
	if r.c.settleRatings {
		return r.checkSettlement(before, sides, outcome)
	}
	return nil
}

// partyTooBig:3 人队开每侧 2 人的 PVP_TEAM,回 TeamSizeExceeded,没人进队列。
func (r *pvpSmokeCaseRun) partyTooBig() error {
	if err := r.formParties(); err != nil {
		return err
	}
	return pvpSmokeStartRejected(r.unit(0), r.tids[0], r.cfg.BattleConfigId, r.c.perSide, tipTeamSizeExceeded)
}

// ratingGap:X + Y 评分差超过上限被拒(TeamRatingGapTooLarge);Y 离队、Z 入队,X + Z 差恰好等于上限,可以排队;
// 排队中 Z 离队,X 收到整队撤单(TeamStateChanged),两人都不在排。
func (r *pvpSmokeCaseRun) ratingGap() error {
	if err := r.formParties(); err != nil {
		return err
	}
	pair := r.unit(0)
	x, y, z := pair[0], pair[1], r.unit(1)[0]
	tid := r.tids[0]
	if err := pvpSmokeStartRejected(pair, tid, r.cfg.BattleConfigId, r.c.perSide, tipTeamRatingGapTooLarge); err != nil {
		return fmt.Errorf("%w (was the rating fixture applied with go/match/cmd/ratingfixture?)", err)
	}
	if err := y.expect(game.ClientPlayerTeamLeaveTeamMessageId, &teampb.LeaveTeamRequest{ExpectedTeamId: tid}, &teampb.TeamResponse{}, 0); err != nil {
		return fmt.Errorf("%s leave: %w", y.name, err)
	}
	if _, err := backfillJoin(x, z, tid); err != nil {
		return err
	}
	if err := pvpSmokeStartTeam([]*teamSmokeBot{x, z}, tid, r.cfg.BattleConfigId, r.c.perSide); err != nil {
		return fmt.Errorf("a party whose rating gap equals the limit must be able to queue: %w", err)
	}
	xMark := x.mark()
	if err := z.expect(game.ClientPlayerTeamLeaveTeamMessageId, &teampb.LeaveTeamRequest{ExpectedTeamId: tid}, &teampb.TeamResponse{}, 0); err != nil {
		return fmt.Errorf("%s leave while queued: %w", z.name, err)
	}
	canceled, err := x.waitSnapshot(xMark, teamSmokePushTimeout, "MATCH_QUEUE_CANCELED after roster change", func(s *teampb.TeamSnapshotS2C) bool {
		return s.GetReason() == teampb.TeamChangeReason_TEAM_CHANGE_REASON_MATCH_QUEUE_CANCELED && s.GetTeam().GetTeamId() == tid
	})
	if err != nil {
		return err
	}
	if got := canceled.GetTip().GetId(); got != tipTeamStateChanged {
		return fmt.Errorf("MATCH_QUEUE_CANCELED tip=%d, expected TeamStateChanged=%d", got, tipTeamStateChanged)
	}
	for _, bot := range []*teamSmokeBot{x, z} {
		if err := backfillExpectQueueState(bot, match.QueueState_QUEUE_STATE_NOT_QUEUED, backfillSmokeAsyncCancelTimeout); err != nil {
			return err
		}
	}
	return nil
}

// fourStack:4 人队先开每侧 5 人被拒(TeamPvpPartySizeNotAllowed),队伍不进入匹配;再开每侧 4 人走标准流程。
func (r *pvpSmokeCaseRun) fourStack() error {
	if err := r.formParties(); err != nil {
		return err
	}
	if err := pvpSmokeStartRejected(r.unit(0), r.tids[0], r.cfg.BattleConfigId, 5, tipTeamPvpPartySize); err != nil {
		return err
	}
	return r.standard()
}

// formParties 把多人单位组成队伍(第一个账号建队,其余申请、队长同意)。已组好的不重复组。
func (r *pvpSmokeCaseRun) formParties() error {
	for i := range r.c.units {
		if len(r.c.units[i]) == 1 || r.tids[i] != 0 {
			continue
		}
		bots := r.unit(i)
		leader := bots[0]
		resp := &teampb.TeamResponse{}
		if err := leader.expect(game.ClientPlayerTeamCreateTeamMessageId, &teampb.CreateTeamRequest{}, resp, 0); err != nil {
			return fmt.Errorf("%s create team: %w", leader.name, err)
		}
		tid := resp.GetTeam().GetTeamId()
		if tid == 0 {
			return fmt.Errorf("%s created a team but the view has team_id=0", leader.name)
		}
		for _, member := range bots[1:] {
			if _, err := backfillJoin(leader, member, tid); err != nil {
				return err
			}
		}
		r.tids[i] = tid
	}
	return nil
}

// enqueue 先让全员持有地域探测结果,再按 units 的顺序逐个入队(队伍经 StartTeamMatch,单人经 JoinQueue),
// 入队之前记下每人的推送留底位置。返回第一个与最后一个单位发出入队请求的时刻(都早于服务端记的入队时刻)。
func (r *pvpSmokeCaseRun) enqueue() (firstAt, lastAt time.Time, err error) {
	bots := r.allBots()
	if err := teamSmokeEnsureNetworkProbes(bots...); err != nil {
		return firstAt, lastAt, err
	}
	for _, bot := range bots {
		r.marks[bot.gc.PlayerId] = bot.mark()
	}
	for i := range r.c.units {
		lastAt = time.Now()
		if i == 0 {
			firstAt = lastAt
		}
		unit := r.unit(i)
		if len(unit) == 1 {
			err = pvpSmokeJoinSolo(unit[0], r.cfg.BattleConfigId, r.c.perSide, r.cfg.Zone)
		} else {
			err = pvpSmokeStartTeam(unit, r.tids[i], r.cfg.BattleConfigId, r.c.perSide)
		}
		if err != nil {
			return firstAt, lastAt, err
		}
	}
	return firstAt, lastAt, nil
}

// quietDeadline 返回「这之前不该成局」的截止时刻:车队差从最后一个单位入队起算,评分差从锚点(第一个单位)入队起算。
func (r *pvpSmokeCaseRun) quietDeadline(firstAt, lastAt time.Time) (time.Time, string, bool) {
	switch {
	case r.c.wait.gapSteps > 0:
		threshold := time.Duration(r.c.wait.gapSteps*r.cfg.GapStepSeconds()) * time.Second
		return lastAt.Add(threshold - pvpSmokeQuietMargin), fmt.Sprintf("the weaker side waited %s for the premade gap", threshold), true
	case r.c.wait.toleranceSteps > 0:
		threshold := time.Duration(r.c.wait.toleranceSteps*r.cfg.ToleranceStepSeconds()) * time.Second
		return firstAt.Add(threshold - pvpSmokeQuietMargin), fmt.Sprintf("the anchor waited %s for the rating tolerance to widen "+
			"(was the rating fixture applied with go/match/cmd/ratingfixture?)", threshold), true
	}
	return time.Time{}, "", false
}

// expectQuiet 等到 deadline,确认这期间没有人收到开战推送。
func (r *pvpSmokeCaseRun) expectQuiet(deadline time.Time, why string) error {
	time.Sleep(time.Until(deadline))
	for _, bot := range r.allBots() {
		_, err := teamSmokeWaitPush(bot, r.marks[bot.gc.PlayerId], game.BattleClientPlayerNotifyBattleStartMessageId, 0,
			func() *battle.BattleStartS2C { return &battle.BattleStartS2C{} },
			func(s *battle.BattleStartS2C) bool { return s.GetBattleId() != 0 })
		if err == nil {
			return fmt.Errorf("%s got BattleStart before %s", bot.name, why)
		}
		if !errors.Is(err, errTeamSmokeTimeout) {
			return err
		}
	}
	return nil
}

// awaitBattle 等全员收到同一 battle_id 的开战推送,返回其中一份战斗状态里每个单位的 team_index(按 actor_id)。
func (r *pvpSmokeCaseRun) awaitBattle(timeout time.Duration) (map[uint64]uint32, error) {
	bots := r.allBots()
	starts := make([]*battle.BattleStartS2C, len(bots))
	errs := make([]error, len(bots))
	var wg sync.WaitGroup
	for i, bot := range bots {
		wg.Add(1)
		go func(i int, bot *teamSmokeBot) {
			defer wg.Done()
			starts[i], errs[i] = teamSmokeWaitPush(bot, r.marks[bot.gc.PlayerId], game.BattleClientPlayerNotifyBattleStartMessageId, timeout,
				func() *battle.BattleStartS2C { return &battle.BattleStartS2C{} },
				func(s *battle.BattleStartS2C) bool { return s.GetBattleId() != 0 })
		}(i, bot)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("wait for BattleStart: %w", err)
	}
	r.battleId = starts[0].GetBattleId()
	for i, start := range starts {
		if start.GetBattleId() != r.battleId {
			return nil, fmt.Errorf("%s entered battle_id=%d but %s entered battle_id=%d: the queued units did not form one battle",
				bots[i].name, start.GetBattleId(), bots[0].name, r.battleId)
		}
	}
	sides := map[uint64]uint32{}
	for _, actor := range starts[0].GetState().GetActors() {
		sides[actor.GetActorId()] = actor.GetTeamIndex()
	}
	return sides, nil
}

// checkSides 核对分边:每名玩家都在战斗状态里;每支队伍整体在同一侧;两侧各 perSide 人;together / opposite 成立。
func (r *pvpSmokeCaseRun) checkSides(sides map[uint64]uint32) error {
	unitSide := make([]uint32, len(r.c.units))
	var people [2]uint32
	for i := range r.c.units {
		for j, bot := range r.unit(i) {
			side, ok := sides[bot.gc.PlayerId]
			if !ok {
				return fmt.Errorf("%s is missing from the battle state of battle_id=%d", bot.name, r.battleId)
			}
			if side > 1 {
				return fmt.Errorf("%s has team_index=%d in battle_id=%d, expected 0 or 1", bot.name, side, r.battleId)
			}
			if j == 0 {
				unitSide[i] = side
			} else if side != unitSide[i] {
				return fmt.Errorf("unit %d was split across sides: %s is on side %d but its leader is on side %d", i, bot.name, side, unitSide[i])
			}
			people[side]++
		}
	}
	if people[0] != r.c.perSide || people[1] != r.c.perSide {
		return fmt.Errorf("battle_id=%d has %d and %d players on the two sides, expected %d each", r.battleId, people[0], people[1], r.c.perSide)
	}
	for _, pair := range r.c.opposite {
		if unitSide[pair[0]] == unitSide[pair[1]] {
			return fmt.Errorf("units %d and %d are both on side %d, expected opposite sides", pair[0], pair[1], unitSide[pair[0]])
		}
	}
	for _, pair := range r.c.together {
		if unitSide[pair[0]] != unitSide[pair[1]] {
			return fmt.Errorf("units %d and %d are on sides %d and %d, expected the same side", pair[0], pair[1], unitSide[pair[0]], unitSide[pair[1]])
		}
	}
	return nil
}

// fight 让全员直连、开自动战斗打到终局,返回终局胜负(全员看到的必须一致)。
func (r *pvpSmokeCaseRun) fight() (battle.EBattleOutcome, error) {
	bots := r.allBots()
	outcomes := make([]battle.EBattleOutcome, len(bots))
	errs := make([]error, len(bots))
	var wg sync.WaitGroup
	for i, bot := range bots {
		wg.Add(1)
		go func(i int, bot *teamSmokeBot) {
			defer wg.Done()
			mark := r.marks[bot.gc.PlayerId]
			direct, err := bot.connectBattle(mark, r.battleId)
			if err != nil {
				errs[i] = err
				return
			}
			defer direct.Close()
			if _, err := bot.finishBattleOn(direct, mark, r.battleId); err != nil {
				errs[i] = err
				return
			}
			end, err := teamSmokeWaitPush(bot, mark, game.BattleClientPlayerNotifyBattleEndMessageId, 0,
				func() *battle.BattleEndS2C { return &battle.BattleEndS2C{} },
				func(e *battle.BattleEndS2C) bool { return e.GetBattleId() == r.battleId })
			if err != nil {
				errs[i] = err
				return
			}
			outcomes[i] = end.GetOutcome()
		}(i, bot)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return battle.EBattleOutcome_BATTLE_OUTCOME_ONGOING, fmt.Errorf("fight battle_id=%d: %w", r.battleId, err)
	}
	for i, outcome := range outcomes {
		if outcome != outcomes[0] {
			return outcome, fmt.Errorf("%s saw outcome %s but %s saw %s in battle_id=%d", bots[i].name, outcome, bots[0].name, outcomes[0], r.battleId)
		}
	}
	return outcomes[0], nil
}

// ---- 评分结算 ----

// ratingsBefore 在入队之前读一次 PVP_TEAM 榜上的 self(只在要核对结算的情况读):没上榜按默认 1500。
func (r *pvpSmokeCaseRun) ratingsBefore() (map[uint64]*rankpb.RankEntry, error) {
	if !r.c.settleRatings {
		return nil, nil
	}
	before := map[uint64]*rankpb.RankEntry{}
	for _, bot := range r.allBots() {
		entry, err := pvpSmokeRankSelf(bot, pvpSmokeBoardPvpTeam)
		if err != nil {
			return nil, err
		}
		before[bot.gc.PlayerId] = entry
	}
	return before, nil
}

// checkSettlement 核对按模式结算:全员在 PVP_TEAM 的榜上新记了一局,分数等于按两队平均分算的 Elo(K = 32);
// 1V1 的榜上没有他们。battle 判的胜负 match 可能按回合上限改判平局(R5 的 ratedScoreA),所以分出胜负的局
// 也接受「整局按平局结算」,但全员必须按同一种结果结算。
func (r *pvpSmokeCaseRun) checkSettlement(before map[uint64]*rankpb.RankEntry, sides map[uint64]uint32, outcome battle.EBattleOutcome) error {
	var scoreA float64
	switch outcome {
	case battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN:
		scoreA = 1
	case battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_B_WIN:
		scoreA = 0
	case battle.EBattleOutcome_BATTLE_OUTCOME_DRAW:
		scoreA = 0.5
	default:
		return fmt.Errorf("battle_id=%d ended with outcome %s, which match does not rate", r.battleId, outcome)
	}
	ratings := map[uint64]int64{}
	players := map[uint64]uint32{}
	for _, bot := range r.allBots() {
		player := bot.gc.PlayerId
		players[player] = sides[player]
		ratings[player] = pvpSmokeDefaultRating
		if entry := before[player]; entry.GetRank() != 0 {
			ratings[player] = entry.GetScore()
		}
	}
	decided, draw := pvpSmokeExpectedRatings(ratings, players, scoreA), pvpSmokeExpectedRatings(ratings, players, 0.5)
	matchesDecided, matchesDraw := true, true
	for _, bot := range r.allBots() {
		player := bot.gc.PlayerId
		// 已在榜上、按平局结算又不改分的人,入账后分数与 achieved_ms 都不变,榜上看不出新记了一局。
		unchangedAllowed := before[player].GetRank() != 0 && draw[player] == ratings[player]
		after, err := pvpSmokeWaitRanked(bot, before[player], unchangedAllowed)
		if err != nil {
			return err
		}
		matchesDecided = matchesDecided && after.GetScore() == decided[player]
		matchesDraw = matchesDraw && after.GetScore() == draw[player]
		oneVOne, err := pvpSmokeRankSelf(bot, pvpSmokeBoardOneVOne)
		if err != nil {
			return err
		}
		if oneVOne.GetRank() != 0 {
			return fmt.Errorf("%s appeared on the 1V1 board after a PVP_TEAM battle: ratings must be kept per mode", bot.name)
		}
	}
	if !matchesDecided && !matchesDraw {
		return fmt.Errorf("PVP_TEAM board scores after battle_id=%d (outcome %s) match neither the Elo result %v nor a draw %v", r.battleId, outcome, decided, draw)
	}
	return nil
}

// pvpSmokeExpectedRatings 是 Elo 的测试预言,与 match 的 authority.ApplyRating 同一公式、各自实现:队伍用成员平均分算期望,
// K = 32,变化量四舍五入到 0.01 分,下限 0。ratings 是结算前的评分(×100),sides 是 team_index,scoreA 是 0 侧的得分。
func pvpSmokeExpectedRatings(ratings map[uint64]int64, sides map[uint64]uint32, scoreA float64) map[uint64]int64 {
	var sum [2]float64
	var count [2]float64
	for player, rating := range ratings {
		sum[sides[player]] += float64(rating) / 100
		count[sides[player]]++
	}
	averageA, averageB := sum[0]/count[0], sum[1]/count[1]
	delta := int64(math.Round(3200 * (scoreA - 1/(1+math.Pow(10, (averageB-averageA)/400)))))
	expected := map[uint64]int64{}
	for player, rating := range ratings {
		change := delta
		if sides[player] == 1 {
			change = -delta
		}
		expected[player] = max(0, rating+change)
	}
	return expected
}

// pvpSmokeWaitRanked 轮询 PVP_TEAM 榜上的 self,直到比 before 新记了一局(写榜是异步的)。
// unchangedAllowed:这个人这局可能按不改分结算(榜上看不出来),超时时分数没变就按已结算返回,由调用方核对分数。
func pvpSmokeWaitRanked(bot *teamSmokeBot, before *rankpb.RankEntry, unchangedAllowed bool) (*rankpb.RankEntry, error) {
	deadline := time.Now().Add(pvpSmokeRankTimeout)
	for {
		after, err := pvpSmokeRankSelf(bot, pvpSmokeBoardPvpTeam)
		if err != nil {
			return nil, err
		}
		if rankSmokePlayedOneMore(before, after) {
			return after, nil
		}
		if !time.Now().Before(deadline) && unchangedAllowed && after.GetRank() != 0 && after.GetScore() == before.GetScore() {
			return after, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%s did not get a new PVP_TEAM board entry within %s (rank=%d achieved_ms=%d, before rank=%d achieved_ms=%d)",
				bot.name, pvpSmokeRankTimeout, after.GetRank(), after.GetAchievedMs(), before.GetRank(), before.GetAchievedMs())
		}
		time.Sleep(pvpSmokeRankPollInterval)
	}
}

// pvpSmokeRankSelf 读一张评分榜的第一页,返回自己的条目(未上榜时 rank = 0)。
func pvpSmokeRankSelf(bot *teamSmokeBot, boardType uint32) (*rankpb.RankEntry, error) {
	request := &rankpb.GetRankPageRequest{
		Board: &rankpb.RankBoardKey{BoardType: boardType, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: "all", Generation: 1},
		Page:  1, PageSize: 10,
	}
	resp := &rankpb.GetRankPageResponse{}
	envelopeTip, err := bot.call(game.ClientPlayerRankGetRankPageMessageId, request, resp)
	if err != nil {
		return nil, fmt.Errorf("%s GetRankPage(board_type=%d): %w", bot.name, boardType, err)
	}
	if envelopeTip != 0 {
		return nil, fmt.Errorf("%s GetRankPage(board_type=%d) envelope rejected tip=%d (no rank instance behind the router, or gate rate limiting)",
			bot.name, boardType, envelopeTip)
	}
	if tip := resp.GetErrorMessage().GetId(); tip != 0 {
		return nil, fmt.Errorf("%s GetRankPage(board_type=%d) tip=%d", bot.name, boardType, tip)
	}
	self := resp.GetSelf()
	if self == nil {
		self = &rankpb.RankEntry{}
	}
	if self.GetRank() != 0 && self.GetEntityId() != bot.gc.PlayerId {
		return nil, fmt.Errorf("%s self entry belongs to player %d", bot.name, self.GetEntityId())
	}
	return self, nil
}

// ---- 入队 ----

func pvpSmokeStartRequest(tid uint64, battleConfigId, perSide uint32) *teampb.StartTeamMatchRequest {
	return &teampb.StartTeamMatchRequest{BattleConfigId: battleConfigId, ExpectedTeamId: tid, TargetTeamSize: perSide,
		MatchMode: match.MatchMode_MATCH_MODE_PVP_TEAM, RequestId: chatSmokeRequestId()}
}

// pvpSmokeStartTeam 队长按 PVP_TEAM 每侧 perSide 人发起整队排队,断言回包是 QUEUED。
// 上一场结算落地前的过渡态(队员仍持 battle:lock、ready 票据未清)在 teamSmokeSettleTimeout 内重试,重试复用同一份请求。
func pvpSmokeStartTeam(bots []*teamSmokeBot, tid uint64, battleConfigId, perSide uint32) error {
	if err := teamSmokeEnsureNetworkProbes(bots...); err != nil {
		return err
	}
	leader := bots[0]
	request := pvpSmokeStartRequest(tid, battleConfigId, perSide)
	deadline := time.Now().Add(teamSmokeSettleTimeout)
	for {
		resp := &teampb.TeamResponse{}
		tip, err := leader.rpc(game.ClientPlayerTeamStartTeamMatchMessageId, request, resp)
		if err != nil {
			return err
		}
		if tip == 0 {
			view := resp.GetTeam()
			if view.GetMatchState() != teampb.TeamMatchState_TEAM_MATCH_STATE_QUEUED || view.GetMatchMode() != match.MatchMode_MATCH_MODE_PVP_TEAM || view.GetMatchTargetSize() != perSide {
				return fmt.Errorf("%s StartTeamMatch(PVP_TEAM, per_side=%d) accepted but the view has match_state=%s mode=%s target=%d",
					leader.name, perSide, view.GetMatchState(), view.GetMatchMode(), view.GetMatchTargetSize())
			}
			return nil
		}
		transient := tip == tipTeamMemberInBattle || tip == tipTeamMemberNotReady
		if !transient || !time.Now().Before(deadline) {
			return fmt.Errorf("%s StartTeamMatch(PVP_TEAM, per_side=%d) rejected tip=%d parameters=%v",
				leader.name, perSide, tip, resp.GetErrorMessage().GetParameters())
		}
		time.Sleep(teamSmokeSettleRetryInterval)
	}
}

// pvpSmokeStartRejected 断言队长按每侧 perSide 人发起被以 want 拒绝,而且全员都不在排、队伍没有进入匹配
// (拒绝时名册和开战锁都不动)。
func pvpSmokeStartRejected(bots []*teamSmokeBot, tid uint64, battleConfigId, perSide, want uint32) error {
	if err := teamSmokeEnsureNetworkProbes(bots...); err != nil {
		return err
	}
	leader := bots[0]
	resp := &teampb.TeamResponse{}
	tip, err := leader.rpc(game.ClientPlayerTeamStartTeamMatchMessageId, pvpSmokeStartRequest(tid, battleConfigId, perSide), resp)
	if err != nil {
		return err
	}
	if tip != want {
		return fmt.Errorf("%s StartTeamMatch(PVP_TEAM, per_side=%d) returned tip=%d, expected %d", leader.name, perSide, tip, want)
	}
	for _, bot := range bots {
		if err := backfillExpectQueueState(bot, match.QueueState_QUEUE_STATE_NOT_QUEUED, 0); err != nil {
			return err
		}
	}
	view, err := leader.myTeam()
	if err != nil {
		return err
	}
	if view.GetMatchState() != teampb.TeamMatchState_TEAM_MATCH_STATE_IDLE {
		return fmt.Errorf("after the rejection team %d has match_state=%s, expected IDLE", tid, view.GetMatchState())
	}
	return nil
}

// pvpSmokeJoinSolo 单人按 PVP_TEAM 每侧 perSide 人入队。过渡态的拒绝在 teamSmokeSettleTimeout 内重试,复用同一份请求。
func pvpSmokeJoinSolo(bot *teamSmokeBot, battleConfigId, perSide, zone uint32) error {
	planId, err := bot.networkProbe()
	if err != nil {
		return fmt.Errorf("%s: %w", bot.name, err)
	}
	request := &match.JoinQueueRequest{
		PlayerId:       bot.gc.PlayerId,
		Mode:           match.MatchMode_MATCH_MODE_PVP_TEAM,
		BattleConfigId: battleConfigId,
		TeamSize:       perSide,
		ZoneId:         zone,
		RequestId:      chatSmokeRequestId(),
		NetworkPlanId:  planId,
	}
	deadline := time.Now().Add(teamSmokeSettleTimeout)
	for {
		resp := &match.JoinQueueResponse{}
		if err := bot.matchCall(game.MatchServiceJoinQueueMessageId, request, resp); err != nil {
			return fmt.Errorf("%s JoinQueue(PVP_TEAM, per_side=%d): %w", bot.name, perSide, err)
		}
		if resp.GetErrorCode() == 0 && resp.GetErrorMessage().GetId() == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s JoinQueue(PVP_TEAM, per_side=%d) rejected error_code=%d tip=%d",
				bot.name, perSide, resp.GetErrorCode(), resp.GetErrorMessage().GetId())
		}
		time.Sleep(teamSmokeSettleRetryInterval)
	}
}
