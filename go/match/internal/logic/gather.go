package logic

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"fmt"
	"time"

	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"
	scenepb "proto/scene"
	smpb "proto/scene_manager"

	"github.com/zeromicro/go-zero/core/logx"
)

// gather 各跳 RPC 的超时。
const (
	prepareBattleTimeout = 3 * time.Second
	createBattleTimeout  = 5 * time.Second
	rollbackTimeout      = 3 * time.Second
)

// kMaxBattleTeamSize 队伍上限 5(设计决策 D14,双侧强制):引擎 Initialize
// 同步校验每队 ≤ 5;match 侧 PVE_TEAM 凑满人数按 min(配置值, 5) 收口 ——
// DungeonTable 存在 max_team_size=10 的历史行,代码收口比改表重导安全。
// 与引擎 constants/turn_battle_constants.h 的同名常量保持一致。
const kMaxBattleTeamSize = 5

// required5v5Players 5v5 凑满人数(设计决策 D15:FIFO 凑 10 人,
// 弹出序前 5 人 team 0、后 5 人 team 1)。
const required5v5Players = kMaxBattleTeamSize * 2

// TableFingerprintMode 的三个取值(config.TableFingerprintMode,设计决策 D14)。
const (
	tableFingerprintOff     = "off"
	tableFingerprintWarn    = "warn"
	tableFingerprintEnforce = "enforce"
)

// preparedMember 记录一个已冻结(PrepareBattle 成功)的参与者,补偿时逐个解冻。
type preparedMember struct {
	playerId      uint64
	sceneEndpoint string
	snapshot      *battlepb.BattlePlayerSnapshot
	// tableFingerprint 是出快照的 scene 节点回报的战斗配表指纹
	// (PrepareBattleResponse.table_fingerprint);旧版 scene 不填则为空。
	tableFingerprint string
}

// gather 对 scene / battle 节点的四个 gRPC 调用抽成包级函数变量:生产走
// *RPC 实现(DialEndpoint 连接缓存 + 各自超时),测试换成 fake —— 现有
// runGatherFn 只能绕开整条 gather,验证指纹比对 / 补偿路径要让 gather 真跑。
var (
	prepareBattleFn       = prepareBattleRPC
	cancelBattlePrepareFn = cancelBattlePrepareRPC
	createBattleFn        = createBattleRPC
	destroyBattleFn       = destroyBattleRPC
)

// RunGather 执行开局管线(设计文档 §3.1/§3.2):
//
//	battle_id = snowflake(match 节点专属生产,17-bit 布局)
//	→ 选 battle 节点(etcd watch 缓存,v1 随机)
//	→ 逐参与者:读 player:{id}:location 定位 scene 节点 → gRPC Scene.PrepareBattle 收快照
//	→ 全齐后 gRPC BattleNode.CreateBattle(seed 用 crypto/rand)
//
// 失败补偿(§3.2 补偿矩阵):
//   - 任一步失败 → 对已冻结者逐个 Scene.CancelBattlePrepare;
//   - CreateBattle 半成功 → BattleNode.DestroyBattle(尽力而为);
//   - requeueOnFail=true 时(队列凑单场景)幸存成员按原序回队首,
//     肇事成员删票出局;false 时(solo/切磋)统一删票收场。
//
// tickets 是弹组 / 入队时读到的各成员 ticket id(设计决策 D5):票据的 ready /
// 回队首 / 删票全部带它做 CAS,matched TTL 到期后玩家重排拿到的新票据不会被
// 迟到的旧 gather 写脏。withTickets=false 用于切磋:参与者从未入队,没有 ticket 可推进。
func RunGather(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, battleConfigId uint32,
	members []uint64, requeueOnFail bool, tickets map[uint64]string,
) {
	runGather(svcCtx, mode, battleConfigId, members, requeueOnFail, true, tickets)
}

// RunChallengeGather 是切磋入口的 gather:与队列匹配汇入同一条管线,
// 仅参与者没有排队票据(设计决策 D3:gather/补偿逻辑只存在一份)。
func RunChallengeGather(svcCtx *svc.ServiceContext, battleConfigId uint32, challengerId, responderId uint64) bool {
	return runGather(svcCtx, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, battleConfigId,
		[]uint64{challengerId, responderId}, false, false, nil)
}

func runGather(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, battleConfigId uint32,
	members []uint64, requeueOnFail bool, withTickets bool, tickets map[uint64]string,
) bool {
	start := time.Now()
	modeName := mode.String()

	fail := func(outcome string, failedPlayer uint64, prepared []preparedMember, battleId uint64) bool {
		if withTickets && requeueOnFail {
			// 进补偿前先给幸存者票据续期(CAS):matched TTL 只覆盖到这里为止的
			// 链路(matchedTicketTTLFor),逐人 CancelBattlePrepare 每人最长
			// rollbackTimeout,不续期 10 人组会在 requeueFront 之前全部过期、
			// 整组从队列消失且没有票据。
			extendMatchedTickets(svcCtx, members, tickets, compensationTicketTTLFor(len(prepared)))
		}
		cancelPrepared(svcCtx, prepared, battleId)
		if withTickets {
			if requeueOnFail {
				// 肇事成员出局删票,其余成员回队首(补偿矩阵 todo #7)。
				var survivors []uint64
				for _, pid := range members {
					if pid == failedPlayer {
						deleteTicketIfOwned(svcCtx, pid, tickets[pid])
						continue
					}
					survivors = append(survivors, pid)
				}
				requeueFront(svcCtx, matchQueueKey(int32(mode), battleConfigId), survivors, tickets)
			} else {
				for _, pid := range members {
					deleteTicketIfOwned(svcCtx, pid, tickets[pid])
				}
			}
		}
		metrics.ObserveGather(modeName, outcome, time.Since(start))
		return false
	}

	// 1. battle_id:只能由 match 节点生产(宪法 §7 SnowFlake 节点隔离)。
	//    ErrFenced/ErrBorrowLimitExceeded 一律 fail-closed,不得用 0 顶替。
	battleId, err := svcCtx.BattleIDGen.Generate()
	if err != nil {
		logx.Errorf("[gather] battle_id 生成失败 mode=%s members=%v: %v", modeName, members, err)
		return fail("internal", 0, nil, 0)
	}

	// 2. 选 battle 节点(全局池,v1 随机;负载上报二期)。
	battleNode, ok := svcCtx.BattleNodes.PickRandom()
	if !ok {
		logx.Errorf("[gather] 无可用 battle 节点 battle=%d mode=%s members=%v", battleId, modeName, members)
		return fail("no_battle_node", 0, nil, 0)
	}

	deadlineMs := nowMs() + uint64(svcCtx.Config.BattleMaxDurationSeconds)*1000
	// prepare_deadline_ms:备战作废期限,与本组票据的 matched TTL 对齐
	// (matchedTicketTTLFor 按组大小算的最坏 gather 耗时)。弹组实例在 gather
	// 中崩溃时,未冻结者靠票据过期自愈,已冻结者靠 scene 在 PREPARING 态按这个
	// 期限解冻并把 battle:lock 收窄到同一窗口 —— 两边同一时刻放行,玩家不再等
	// BattleMaxDurationSeconds+60s 的战斗锁(设计决策 D13;deadline_ms 语义不变,
	// CreateBattle 成功后仍是战斗本身的作废期限)。
	prepareDeadlineMs := nowMs() + uint64(matchedTicketTTLFor(svcCtx, uint32(len(members))))*1000

	// 2.5 观战互斥清退(设计决策 D11 / 不变量 8):进 gather 的玩家必须先从
	//     观战中摘除,杜绝观众绑定与随后的参战 BindBattleEvent 抢占 SessionInfo
	//     槽位的竞态;尽力而为,失败只记日志不阻断开局。
	for _, playerId := range members {
		stopWatchingIfAny(svcCtx, playerId, "enter_gather")
	}

	// 2.6 分队:5v5 按评分蛇形平衡(§11),其余模式按弹出序(teamIndexFor)。
	teams := teamAssignment(svcCtx, mode, battleId, members)

	// 3. 逐参与者冻结收快照。
	var prepared []preparedMember
	for i, playerId := range members {
		endpoint, resp, err := preparePlayer(svcCtx, playerId, battleId, battleNode.NodeId, deadlineMs, prepareDeadlineMs)
		if err != nil {
			logx.Errorf("[gather] PrepareBattle 失败 battle=%d player=%d(第 %d/%d 人): %v",
				battleId, playerId, i+1, len(members), err)
			outcome := "prepare_failed"
			if endpoint == "" {
				outcome = "no_location"
			}
			return fail(outcome, playerId, prepared, battleId)
		}
		snapshot := resp.GetSnapshot()
		snapshot.TeamIndex = teams[i]
		prepared = append(prepared, preparedMember{
			playerId:         playerId,
			sceneEndpoint:    endpoint,
			snapshot:         snapshot,
			tableFingerprint: resp.GetTableFingerprint(),
		})
	}

	// 3.5 组内 zone 组成(可观测性,设计决策 D8):匹配不看 zone,只在这里
	//     统计"跨 zone 对局占比"并把 zone 打进对局日志。
	observeZoneMix(modeName, battleId, prepared)

	// 3.6 配表指纹比对(设计决策 D14):全员非空且两两一致才透传给 battle;
	//     不一致按 TableFingerprintMode 处理,enforce 下与 prepare 失败同路径补偿,
	//     肇事者 = 与多数派不一致者(出局删票),幸存者回队首。
	tableFingerprint, offender, ok := checkTableFingerprints(svcCtx, modeName, battleId, prepared)
	if !ok {
		return fail("fingerprint_mismatch", offender, prepared, battleId)
	}

	// 4. CreateBattle。seed 用 crypto/rand 生成 uint64(引擎确定性 RNG 的种子)。
	seed, err := randomSeed()
	if err != nil {
		logx.Errorf("[gather] 生成随机种子失败 battle=%d: %v", battleId, err)
		return fail("internal", 0, prepared, battleId)
	}
	snapshots := make([]*battlepb.BattlePlayerSnapshot, 0, len(prepared))
	for _, p := range prepared {
		snapshots = append(snapshots, p.snapshot)
	}
	createdAtMs := nowMs()
	if err := createBattle(battleNode, &battlepb.CreateBattleRequest{
		BattleId:         battleId,
		BattleConfigId:   battleConfigId,
		Players:          snapshots,
		Seed:             seed,
		MatchMode:        uint32(mode),
		CreatedAtMs:      createdAtMs,
		DeadlineMs:       deadlineMs,
		TableFingerprint: tableFingerprint,
	}); err != nil {
		logx.Errorf("[gather] CreateBattle 失败 battle=%d node=%d(%s): %v",
			battleId, battleNode.NodeId, battleNode.Endpoint, err)
		// 半成功兜底:RPC 超时时战斗可能已建成,先尽力 DestroyBattle 再解冻。
		if !destroyBattle(battleNode, battleId, "gather_rollback") {
			// DestroyBattle 也失败 → 房间可能仍活着且已向 scene 发出 BattleConfirmedEvent。
			// 此时再逐人 CancelBattlePrepare 会出现"Cancel 先于 Confirm 到达、锁被删、随后
			// Confirm 因锁不在无法重建冻结"的交错(C++ 复审),留下"房间活着、玩家无冻结"。
			// 所以不解冻、不回队:scene 侧 FIGHTING 态由 Confirm 升级并按正式 deadline 收尾,
			// 房间按 deadline_ms 强制结束并回流结算;票据留 matched 由 TTL 自愈。
			logx.Errorf("[gather] CreateBattle 失败且 DestroyBattle 失败,房间可能仍活着,放弃解冻交给 scene/battle 期限收尾 battle=%d members=%v",
				battleId, members)
			metrics.ObserveGather(modeName, "create_failed_room_alive", time.Since(start))
			return false
		}
		return fail("create_failed", 0, prepared, battleId)
	}

	// 5. 成功收尾:ticket 推进 ready(短 TTL 自清),后续状态由
	//    battle 节点的 BattleStartS2C / 结算链路接管。
	if withTickets {
		for _, pid := range members {
			markTicketReady(svcCtx, pid, tickets[pid], battleId)
		}
	}

	// 登记观战索引(设计决策 D9:只有 match 知道战斗在哪个 battle 节点;
	// score/created_at 与 CreateBattleRequest 同一时刻取值,过期判定对齐 deadline)。
	playerNames := make([]string, 0, len(prepared))
	for _, p := range prepared {
		playerNames = append(playerNames, p.snapshot.GetPlayerName())
	}
	registerSpectateBattle(svcCtx, battleId, battleNode.NodeId, mode, battleConfigId, playerNames, createdAtMs)
	logx.Infof("[gather] 开局成功 battle=%d mode=%s config=%d node=%d members=%v 耗时=%s",
		battleId, modeName, battleConfigId, battleNode.NodeId, members, time.Since(start))
	metrics.ObserveGather(modeName, "success", time.Since(start))
	return true
}

// preparePlayer 定位玩家所在 scene 节点并调 PrepareBattle,返回校验过的响应
// (快照非空;调用方另取 table_fingerprint)。返回的 endpoint 为空表示还没定位
// 到对端(位置缺失/节点未注册),用于区分 no_location 与 prepare_failed 两类指标。
func preparePlayer(svcCtx *svc.ServiceContext, playerId, battleId uint64,
	battleNodeId uint32, deadlineMs, prepareDeadlineMs uint64,
) (string, *scenepb.PrepareBattleResponse, error) {
	// player:{id}:location 由 scene_manager 写(protobuf PlayerLocation),
	// 是玩家 scene 定位的共享契约(设计文档 §5.4 / go/scene_manager changesceneutil.go);
	// 带 zone_id,所以任意 zone 的玩家都能定位到自己 zone 的 scene 节点。
	loc, err := loadPlayerLocation(svcCtx, playerId)
	if err != nil {
		return "", nil, err
	}
	if loc == nil {
		return "", nil, fmt.Errorf("玩家位置不存在(不在线或未进场)")
	}

	endpoint, err := svcCtx.SceneNodes.EndpointOf(loc.ZoneId, loc.NodeId)
	if err != nil {
		return "", nil, fmt.Errorf("定位 scene 节点失败(zone=%d node=%s): %w", loc.ZoneId, loc.NodeId, err)
	}

	resp, err := prepareBattleFn(endpoint, &scenepb.PrepareBattleRequest{
		PlayerId:          playerId,
		BattleId:          battleId,
		BattleNodeId:      battleNodeId,
		DeadlineMs:        deadlineMs,
		PrepareDeadlineMs: prepareDeadlineMs,
	})
	if err != nil {
		return endpoint, nil, fmt.Errorf("PrepareBattle RPC 失败: %w", err)
	}
	if resp.GetErrorMessage().GetId() != 0 {
		return endpoint, nil, fmt.Errorf("scene 拒绝备战 tip_id=%d", resp.GetErrorMessage().GetId())
	}
	if resp.GetSnapshot() == nil {
		return endpoint, nil, fmt.Errorf("scene 返回空快照")
	}
	return endpoint, resp, nil
}

// prepareBattleRPC 调 scene 节点冻结玩家并抽快照(prepareBattleFn 的生产实现)。
// 走 gRPC 专用控制面 SceneNodeGrpc(scene.proto 的 Scene 服务是 cc_generic_services
// 的 muduo TCP 形态,C++ 侧无法以 gRPC 实现,先例见 CreateScene/DestroyScene)。
// 消息类型定义在 scene.proto(scenepb),SceneNodeGrpc 服务只引用不重复定义。
func prepareBattleRPC(endpoint string, req *scenepb.PrepareBattleRequest) (*scenepb.PrepareBattleResponse, error) {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("连接 scene 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepareBattleTimeout)
	defer cancel()
	return smpb.NewSceneNodeGrpcClient(conn).PrepareBattle(ctx, req)
}

// checkTableFingerprints 比对一组已冻结成员回报的配表指纹(设计决策 D14)。
// 返回 (透传给 battle 的指纹, 肇事者, 是否放行):
//   - 全员非空且两两一致 → (指纹, 0, true);
//   - off → 不比对不透传 ("", 0, true);
//   - 不一致或部分为空:warn → 记 Error 日志 + 指标后放行 ("", 0, true);
//     enforce → ("", 肇事者, false),调用方按 prepare 失败补偿。
//
// 肇事者 = 与多数派不一致者(弹出序里第一个):多数派只在非空指纹里选(空 =
// 该 scene 没回报,本身就是可疑方,不能靠"空"凑成多数);全员皆空时没有
// 多数派可言,记第一位成员 —— enforce 下这等于拒绝所有不回报指纹的 scene,
// 是开 enforce 的前提(灰度期用 warn)。
func checkTableFingerprints(svcCtx *svc.ServiceContext, modeName string, battleId uint64,
	prepared []preparedMember,
) (string, uint64, bool) {
	fpMode := svcCtx.Config.TableFingerprintMode
	if fpMode == "" {
		fpMode = tableFingerprintWarn
	}
	if fpMode == tableFingerprintOff || len(prepared) == 0 {
		return "", 0, true
	}

	// 多数派:非空指纹按出现次数取最多,平票取弹出序靠前者。
	counts := make(map[string]int, len(prepared))
	majority := ""
	for _, p := range prepared {
		fp := p.tableFingerprint
		if fp == "" {
			continue
		}
		counts[fp]++
		if majority == "" || counts[fp] > counts[majority] {
			majority = fp
		}
	}
	var offender uint64
	perMember := make([]string, 0, len(prepared))
	for _, p := range prepared {
		perMember = append(perMember, fmt.Sprintf("%d=%q", p.playerId, p.tableFingerprint))
		if offender == 0 && (majority == "" || p.tableFingerprint != majority) {
			offender = p.playerId
		}
	}
	if offender == 0 {
		return majority, 0, true
	}

	metrics.ObserveTableFingerprintMismatch(fpMode)
	if fpMode == tableFingerprintEnforce {
		logx.Errorf("[gather] 配表指纹不一致,拒绝开局(enforce) battle=%d mode=%s majority=%q offender=%d members=%v",
			battleId, modeName, majority, offender, perMember)
		return "", offender, false
	}
	logx.Errorf("[gather] 配表指纹不一致,照常开局(warn;不透传指纹) battle=%d mode=%s majority=%q offender=%d members=%v",
		battleId, modeName, majority, offender, perMember)
	return "", 0, true
}

// observeZoneMix 统计一组已冻结成员的 zone 去重数:1 个 zone 记 single,否则 cross
// (gather_zone_mix_total{mode,mix}),并把每人 zone 打进日志。zone 取自快照的
// BattleRouting.zone_id —— 它是 scene 在 PrepareBattle 时从自身配置填的,比
// 票据里入队时刻的快照更权威。
func observeZoneMix(modeName string, battleId uint64, prepared []preparedMember) {
	zones := make(map[uint32]struct{}, len(prepared))
	perMember := make([]string, 0, len(prepared))
	for _, p := range prepared {
		zone := p.snapshot.GetRouting().GetZoneId()
		zones[zone] = struct{}{}
		perMember = append(perMember, fmt.Sprintf("%d@z%d", p.playerId, zone))
	}
	mix := "single"
	if len(zones) > 1 {
		mix = "cross"
	}
	logx.Infof("[gather] 组内 zone 组成 battle=%d mode=%s zone_mix=%s zones=%d members=%v",
		battleId, modeName, mix, len(zones), perMember)
	metrics.ObserveGatherZoneMix(modeName, mix)
}

// cancelPrepared 对已冻结者逐个解冻(尽力而为;彻底失败由 scene 侧
// InBattleComp.deadline_ms 的 reaper 兜底,见补偿矩阵)。
func cancelPrepared(svcCtx *svc.ServiceContext, prepared []preparedMember, battleId uint64) {
	for _, p := range prepared {
		err := cancelBattlePrepareFn(p.sceneEndpoint, &scenepb.CancelBattlePrepareRequest{
			PlayerId: p.playerId,
			BattleId: battleId,
		})
		if err != nil {
			logx.Errorf("[gather] CancelBattlePrepare 失败(scene reaper 会按 deadline 兜底解冻) battle=%d player=%d endpoint=%s: %v",
				battleId, p.playerId, p.sceneEndpoint, err)
			continue
		}
		logx.Infof("[gather] 已解冻 battle=%d player=%d", battleId, p.playerId)
	}
}

// cancelBattlePrepareRPC 调 scene 节点解冻(cancelBattlePrepareFn 的生产实现)。
func cancelBattlePrepareRPC(endpoint string, req *scenepb.CancelBattlePrepareRequest) error {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return fmt.Errorf("连接 scene 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_, err = smpb.NewSceneNodeGrpcClient(conn).CancelBattlePrepare(ctx, req)
	return err
}

// createBattle 调 battle 节点建房。
func createBattle(node discovery.NodeEntry, req *battlepb.CreateBattleRequest) error {
	resp, err := createBattleFn(node.Endpoint, req)
	if err != nil {
		return fmt.Errorf("CreateBattle RPC 失败: %w", err)
	}
	if resp.GetErrorMessage().GetId() != 0 {
		return fmt.Errorf("battle 节点拒绝建房 tip_id=%d", resp.GetErrorMessage().GetId())
	}
	return nil
}

// createBattleRPC 是 createBattleFn 的生产实现。
func createBattleRPC(endpoint string, req *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createBattleTimeout)
	defer cancel()
	return battlepb.NewBattleNodeClient(conn).CreateBattle(ctx, req)
}

// destroyBattle 半成功回滚:CreateBattle 出错时战斗可能已建成,尽力销毁。
// 返回是否确认销毁成功;失败时调用方不得再解冻参战者(见 CreateBattle 失败分支)。
func destroyBattle(node discovery.NodeEntry, battleId uint64, reason string) bool {
	if err := destroyBattleFn(node.Endpoint, &battlepb.DestroyBattleRequest{
		BattleId: battleId,
		Reason:   reason,
	}); err != nil {
		// 销毁失败不致命:battle 房间收不到指令会按 deadline_ms 强制收尾。
		logx.Errorf("[gather] DestroyBattle 失败 battle=%d reason=%s: %v", battleId, reason, err)
		return false
	}
	return true
}

// destroyBattleRPC 是 destroyBattleFn 的生产实现。
func destroyBattleRPC(endpoint string, req *battlepb.DestroyBattleRequest) error {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	_, err = battlepb.NewBattleNodeClient(conn).DestroyBattle(ctx, req)
	return err
}

// teamAssignment 给一组成员分队,返回与 members 同下标的队号。5v5 读各成员
// 评分后按 assignBalancedTeams 蛇形平衡(§11;评分读失败者按默认分参与),
// 其余模式走 teamIndexFor 的弹出序语义。
func teamAssignment(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, battleId uint64, members []uint64) []uint32 {
	if mode == matchpb.MatchMode_MATCH_MODE_5V5 {
		ratings := loadRatings(svcCtx, members)
		teams := assignBalancedTeams(members, ratings)
		perMember := make([]string, 0, len(members))
		for i, pid := range members {
			perMember = append(perMember, fmt.Sprintf("%d=%s@t%d", pid, formatRating(ratings[pid]), teams[i]))
		}
		logx.Infof("[gather] 5v5 按评分蛇形分队 battle=%d members=%v", battleId, perMember)
		return teams
	}
	teams := make([]uint32, len(members))
	for i := range members {
		teams[i] = teamIndexFor(mode, i, len(members))
	}
	return teams
}

// teamIndexFor 决定参与者的队伍编号:1v1/切磋语义不变(memberIndex 即队号,
// 前后两人各一队);5v5 不走这里(teamAssignment 按评分蛇形分队,§11;
// 这里保留"前半 team 0、后半 team 1"作为无评分时的弹出序语义,D15);
// PVE 全员 0 队(怪物侧由 battle 节点按 DungeonTable 生成,恒为 1 队对手)。
func teamIndexFor(mode matchpb.MatchMode, memberIndex int, required int) uint32 {
	switch mode {
	case matchpb.MatchMode_MATCH_MODE_1V1, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE:
		return uint32(memberIndex)
	case matchpb.MatchMode_MATCH_MODE_5V5:
		if required > 0 && memberIndex >= required/2 {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// randomSeed 用 crypto/rand 生成引擎种子(设计文档 §5.4;引擎内所有随机
// 只走该种子驱动的确定性 RNG,宪法新增不变量 #5)。
func randomSeed() (uint64, error) {
	var buf [8]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}
