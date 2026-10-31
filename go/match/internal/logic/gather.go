package logic

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"match/internal/discovery"
	"match/internal/metrics"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"
	scenepb "proto/scene"
	smpb "proto/scene_manager"
	"shared/generated/tip"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// gather 各跳 RPC 的超时。
const (
	prepareBattleTimeout = 3 * time.Second
	createBattleTimeout  = 5 * time.Second
	rollbackTimeout      = 3 * time.Second
)

// kMaxBattleTeamSize 队伍上限 5(设计决策 D14,双侧强制):引擎 Initialize
// 同步校验每队 ≤ 5;match 侧组队人数上限按 min(配置值, 5) 收口 ——
// DungeonTable 存在 max_team_size=10 的历史行,代码收口比改表重导安全。
// 与引擎 constants/turn_battle_constants.h 的同名常量保持一致。
const kMaxBattleTeamSize = 5

// required5v5Players 5v5 凑满人数(设计决策 D15)。
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

// battleNotAllocatableMessage 是 battle 节点 CreateBattle 被节点级准入拒绝时回的 gRPC 状态消息:
// 建房准入闸未开 / 已关(节点启动未完成 / 停机中),或拿不到分配许可(Agones 未就绪 / 排空中 /
// 许可被拒)。它与 codes.Unavailable 的**组合**是跨语言字符串契约(k8s-client-entry D82,C++ 侧
// cpp/nodes/battle/handler/grpc/battle_node.cpp 的 kBattleNotAllocatableMessage):battle 在 gRPC
// 线程上、runInLoop 之前查准入闸并取许可,loop 内插表前再复核一次准入闸,任一处拒绝都保证
// 没有任何副作用(没插表、没发确认事件)。
const battleNotAllocatableMessage = "battle_not_allocatable"

// errBattleNotAllocatable 由 createBattle 包装返回,标记"节点级准入拒绝、房间必未建成"
// (调用方用 errors.Is 判定,据此短时熔断这个实例)。
var errBattleNotAllocatable = errors.New("battle 节点暂不可分配(" + battleNotAllocatableMessage + ")")

// errPrepareRejected 标记确定由玩家自身状态导致的备战失败:不在线或未进场、scene 以业务码(非故障码)拒绝。
// 成局只凭它给玩家定罪(终止其单排票);读位置失败、RPC 超时或断连、scene 内部故障码等结果未知的
// 失败不定罪(docs/design/global-match-hierarchical-pools.md §5.5)。
var errPrepareRejected = errors.New("玩家当前不能备战")

// gather 对 scene / battle 节点的 gRPC 调用抽成包级函数变量:生产走
// *RPC 实现(DialEndpoint 连接缓存 + 各自超时),测试换成 fake。
var (
	prepareBattleFn       = prepareBattleRPC
	cancelBattlePrepareFn = cancelBattlePrepareRPC
	createBattleFn        = createBattleRPC
)

// runFormationGather 推进一局已在 SQL 提交的成局,执行开局管线(设计文档 §3.1/§3.2):
//
//	逐参与者:读 player:{id}:location 定位 scene 节点 → 持久化端点 → gRPC Scene.PrepareBattle 收快照
//	→ 全齐后证明目标实例支持 SQL 终局协议、把建房原件写进成局(SaveSupportedIntent)
//	→ gRPC BattleNode.CreateBattle(seed 用 crypto/rand)→ 记采用版本并提交 state 4
//
// battle_id、机房、目标实例在成局事务里已固定,这里不换节点、不重选。失败时:
//   - 发房之前失败且能证明原 operation 不会建房(globalDestroy)→ globalAbortPrepared 解冻并收尾,
//     肇事者(掉线、不在场景、配表不一致)的单排票终止,其余成员保留原入队时刻回队;
//   - 证明不了,或 Create 结果未知 → 标记 Unknown,由工作租约到期后的恢复与唯一 SQL 终局收尾。
//
// 同步执行,返回是否开局成功;调用方负责放进后台任务并持有工作租约。
func runFormationGather(svcCtx *svc.ServiceContext, f *matchpb.MatchFormationRecord, activityContext *battlepb.BattleActivityContext) bool {
	start := time.Now()
	mode := matchpb.MatchMode(f.Mode)
	modeName := mode.String()
	battleId := f.BattleId
	members := make([]uint64, 0, len(f.Members))
	for _, member := range f.Members {
		members = append(members, member.PlayerId)
	}

	fail := func(outcome string, failedPlayer uint64) bool {
		if globalDestroy(svcCtx.Authority, f) {
			globalAbortPrepared(svcCtx, f, failedPlayer)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = svcCtx.Authority.Unknown(ctx, f)
			cancel()
		}
		metrics.ObserveGather(modeName, outcome, time.Since(start))
		return false
	}

	battleNode := discovery.NodeEntry{NodeId: f.TargetNodeId, NodeUuid: f.TargetInstanceId, Endpoint: f.TargetEndpoint, PhysicalRegion: f.PhysicalRegion}
	teams, err := gatherTeamAssignment(svcCtx, f, members)
	if err != nil {
		logx.Errorf("[gather] Invalid explicit team assignment battle=%d: %v", battleId, err)
		return fail("internal", 0)
	}

	// prepare_deadline_ms:备战作废期限(matchedTicketTTLFor 按组大小算的最坏 gather 耗时),且不晚于工作租约。
	// 进程在 gather 中崩溃时,已冻结者靠 scene 在 PREPARING 态按这个期限解冻并把 battle:lock 收窄到同一窗口
	// (设计决策 D13;deadline_ms 语义不变,CreateBattle 成功后仍是战斗本身的作废期限)。
	deadlineMs := f.DeadlineMs
	prepareDeadlineMs := min(f.CreatedMs+uint64(matchedTicketTTLFor(svcCtx, uint32(len(members))))*1000, f.RetryAtMs-1000)

	// 逐参与者冻结收快照。
	var prepared []preparedMember
	for i, playerId := range members {
		endpoint, resp, err := preparePlayerFenced(svcCtx, playerId, battleId, battleNode.NodeId, deadlineMs, prepareDeadlineMs, func(endpoint string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return svcCtx.Authority.Prepared(ctx, f, playerId, endpoint)
		})
		if err != nil {
			logx.Errorf("[gather] PrepareBattle failed battle=%d player=%d (member %d/%d): %v",
				battleId, playerId, i+1, len(members), err)
			outcome := "prepare_failed"
			if endpoint == "" {
				outcome = "no_location"
			}
			culprit := uint64(0) // 结果未知的失败不定罪,全员保留原入队时刻回队
			if errors.Is(err, errPrepareRejected) {
				culprit = playerId
			}
			return fail(outcome, culprit)
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

	// 组内 zone 组成(可观测性,设计决策 D8)。
	observeZoneMix(modeName, battleId, prepared)

	// 配表指纹比对(设计决策 D14):全员非空且两两一致才透传给 battle;
	// 不一致按 TableFingerprintMode 处理,enforce 下肇事者 = 与多数派不一致者,其余成员回队。
	tableFingerprint, offender, ok := checkTableFingerprints(svcCtx, modeName, battleId, prepared)
	if !ok {
		return fail("fingerprint_mismatch", offender)
	}

	// seed 用 crypto/rand 生成 uint64(引擎确定性 RNG 的种子)。
	seed, err := randomSeed()
	if err != nil {
		logx.Errorf("[gather] Failed to generate random seed battle=%d: %v", battleId, err)
		return fail("internal", 0)
	}
	snapshots := make([]*battlepb.BattlePlayerSnapshot, 0, len(prepared))
	for _, p := range prepared {
		snapshots = append(snapshots, p.snapshot)
	}
	createdAtMs := nowMs()
	createReq := &battlepb.CreateBattleRequest{
		BattleId:               battleId,
		BattleConfigId:         f.ConfigId,
		Players:                snapshots,
		Seed:                   seed,
		MatchMode:              uint32(mode),
		CreatedAtMs:            createdAtMs,
		DeadlineMs:             deadlineMs,
		TableFingerprint:       tableFingerprint,
		ActivityContext:        activityContext,
		SpectateVisibility:     spectateVisibilityFor(mode, activityContext),
		RequiredPhysicalRegion: f.PhysicalRegion,
		PlacementOperationId:   f.OperationId,
		TargetInstanceId:       f.TargetInstanceId,
		AdmitBeforeMs:          prepareDeadlineMs,
		OutcomeProtocolVersion: 1,
	}
	// 先证明原UUID支持SQL终局门禁；不支持的节点会忽略Create新字段，因此不能直接试发。
	capctx, capcancel := context.WithTimeout(context.Background(), 3*time.Second)
	capability, cerr := queryOutcomeCapability(capctx, battleNode, createReq)
	capcancel()
	if cerr != nil {
		return fail("outcome_capability_missing", 0)
	}
	payload, merr := proto.MarshalOptions{Deterministic: true}.Marshal(createReq)
	if merr != nil {
		return fail("intent_failed", 0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	canonical, merr := svcCtx.Authority.SaveSupportedIntent(ctx, f, payload, capability)
	cancel()
	if merr != nil {
		return fail("intent_failed", 0)
	}
	if merr = proto.Unmarshal(canonical, createReq); merr != nil {
		return fail("intent_failed", 0)
	}

	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	workErr := svcCtx.Authority.CheckWork(ctx, f)
	cancel()
	if workErr != nil {
		return false
	}
	err = createBattle(battleNode, createReq)
	if err != nil {
		if errors.Is(err, errBattleNotAllocatable) && svcCtx.Placement != nil {
			// 节点级准入拒绝:本副本短时熔断这个实例,后续成局在同机房换节点。
			// 原件仍按下面的 UNKNOWN 走唯一 SQL 终局,不因"房间必未建成"提前释放。
			svcCtx.Placement.MarkUnavailable(f.TargetInstanceId)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		var adoption *outcomeAdoptionError
		if errors.As(err, &adoption) {
			if observationErr := svcCtx.Authority.RecordBattleAdopted(ctx, f, adoption.instance, adoption.version); observationErr != nil {
				logx.Error("[gather] Adopted outcome protocol version not persisted yet; keeping original for recovery")
			}
		}
		_ = svcCtx.Authority.Unknown(ctx, f)
		cancel()
		return false
	}

	// 成功收尾:提交 state 4,后续状态由 battle 节点的 BattleStartS2C / 结算链路接管。
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
	e := svcCtx.Authority.RecordBattleAdopted(ctx, f, createReq.TargetInstanceId, 1)
	if e == nil {
		e = svcCtx.Authority.Finish(ctx, f, true)
	}
	cancel()
	if e != nil {
		logx.Error("[gather] Battle created but authority commit unconfirmed; keeping original intent for recovery")
		return false
	}

	// 公开这场战斗:入活跃集合(best-effort;补签与按 battle_id 观战都读成局原件,不靠这一步)。
	addSpectateActive(svcCtx, battleId, createdAtMs, createReq.GetSpectateVisibility())
	logx.Infof("[gather] Battle started successfully battle=%d mode=%s config=%d node=%d members=%v elapsed=%s",
		battleId, modeName, f.ConfigId, battleNode.NodeId, members, time.Since(start))
	metrics.ObserveGather(modeName, "success", time.Since(start))
	return true
}

// gatherTeamAssignment 给成局成员分队。PVP_TEAM 的分边在提交点已写进成局原件,不重新计算;其余模式按 teamAssignment。
func gatherTeamAssignment(s *svc.ServiceContext, f *matchpb.MatchFormationRecord, members []uint64) ([]uint32, error) {
	mode := matchpb.MatchMode(f.Mode)
	if mode != matchpb.MatchMode_MATCH_MODE_PVP_TEAM {
		return teamAssignment(s, mode, f.BattleId, members), nil
	}
	assignments := make(map[uint64]uint32, len(f.Members))
	for _, member := range f.Members {
		if _, duplicate := assignments[member.PlayerId]; duplicate {
			return nil, fmt.Errorf("成局原件成员重复")
		}
		assignments[member.PlayerId] = member.TeamIndex
	}
	if len(assignments) != len(members) {
		return nil, fmt.Errorf("PVP分边名单不完整")
	}
	counts := [2]uint32{}
	teams := make([]uint32, len(members))
	seen := make(map[uint64]bool, len(members))
	for i, player := range members {
		side, ok := assignments[player]
		if !ok || side > 1 || player == 0 || seen[player] {
			return nil, fmt.Errorf("PVP分边成员或队号无效")
		}
		seen[player] = true
		teams[i] = side
		counts[side]++
	}
	if counts[0] == 0 || counts[0] != counts[1] || counts[0] > kMaxBattleTeamSize {
		return nil, fmt.Errorf("PVP双方人数须相同且各为1到5人")
	}
	return teams, nil
}

// preparePlayerFenced 定位玩家所在 scene 节点并调 PrepareBattle,返回校验过的响应
// (快照非空;调用方另取 table_fingerprint)。返回的 endpoint 为空表示还没定位
// 到对端(位置缺失/节点未注册),用于区分 no_location 与 prepare_failed 两类指标。
// before 在冻结 RPC 之前持久化精确端点，并校验本代工作租约。
func preparePlayerFenced(svcCtx *svc.ServiceContext, playerId, battleId uint64, battleNodeId uint32, deadlineMs, prepareDeadlineMs uint64, before func(string) error) (string, *scenepb.PrepareBattleResponse, error) {
	// player:{id}:location 由 scene_manager 写(protobuf PlayerLocation),
	// 是玩家 scene 定位的共享契约(设计文档 §5.4 / go/scene_manager changesceneutil.go);
	// 带 zone_id,所以任意 zone 的玩家都能定位到自己 zone 的 scene 节点。
	loc, err := loadPlayerLocation(svcCtx, playerId)
	if err != nil {
		return "", nil, err
	}
	if loc == nil {
		return "", nil, fmt.Errorf("玩家位置不存在(不在线或未进场): %w", errPrepareRejected)
	}

	endpoint, err := svcCtx.SceneNodes.EndpointOf(loc.ZoneId, loc.NodeId)
	if err != nil {
		return "", nil, fmt.Errorf("定位 scene 节点失败(zone=%d node=%s): %w", loc.ZoneId, loc.NodeId, err)
	}

	if before != nil {
		if err = before(endpoint); err != nil {
			return endpoint, nil, err
		}
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
	if id := resp.GetErrorMessage().GetId(); id != 0 {
		if tip.IsFault(id) {
			return endpoint, nil, fmt.Errorf("scene 备战内部故障 tip_id=%d", id)
		}
		return endpoint, nil, fmt.Errorf("scene 拒绝备战 tip_id=%d: %w", id, errPrepareRejected)
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
// 肇事者 = 与多数派不一致者(成员序里第一个):多数派只在非空指纹里选(空 =
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

	// 多数派:非空指纹按出现次数取最多,平票取成员序靠前者。
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
		logx.Errorf("[gather] Configuration fingerprints differ; rejecting battle start (enforce) battle=%d mode=%s majority=%q offender=%d members=%v",
			battleId, modeName, majority, offender, perMember)
		return "", offender, false
	}
	logx.Errorf("[gather] Configuration fingerprints differ; starting battle normally (warn; fingerprint not forwarded) battle=%d mode=%s majority=%q offender=%d members=%v",
		battleId, modeName, majority, offender, perMember)
	return "", 0, true
}

// observeZoneMix 统计一组已冻结成员的 zone 去重数:1 个 zone 记 single,否则 cross
// (gather_zone_mix_total{mode,mix}),并把每人 zone 打进日志。zone 取自快照的
// BattleRouting.zone_id —— 它是 scene 在 PrepareBattle 时从自身配置填的。
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
	logx.Infof("[gather] Group zone composition battle=%d mode=%s zone_mix=%s zones=%d members=%v",
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
			logx.Errorf("[gather] CancelBattlePrepare failed (scene reaper will unfreeze at deadline) battle=%d player=%d endpoint=%s: %v",
				battleId, p.playerId, p.sceneEndpoint, err)
			continue
		}
		logx.Infof("[gather] Player unfrozen battle=%d player=%d", battleId, p.playerId)
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

// createBattle 调 battle 节点建房。返回的错误分三类:
//   - 包装了 errBattleNotAllocatable(errors.Is 可判):节点级准入拒绝,battle 侧无副作用;
//   - 其余 RPC 失败:请求可能已送达、房间可能已建成,结果未知;
//   - battle 业务拒绝(tip≠0)或回执不符 / 明确未采用协议 1(outcomeAdoptionError)。
func createBattle(node discovery.NodeEntry, req *battlepb.CreateBattleRequest) error {
	resp, err := createBattleFn(node.Endpoint, req)
	if err != nil {
		if isBattleNotAllocatable(err) {
			return fmt.Errorf("battle 节点拒绝分配 node=%d: %w", node.NodeId, errBattleNotAllocatable)
		}
		return fmt.Errorf("CreateBattle RPC 失败: %w", err)
	}
	if resp.GetErrorMessage().GetId() != 0 {
		return fmt.Errorf("battle 节点拒绝建房 tip_id=%d", resp.GetErrorMessage().GetId())
	}
	if resp.GetBattleId() != req.BattleId || resp.GetTargetInstanceId() != req.TargetInstanceId {
		return errors.New("Battle创建回执原局或原UUID不符")
	}
	if resp.GetAdoptedOutcomeProtocolVersion() != 1 {
		return &outcomeAdoptionError{instance: resp.TargetInstanceId, version: resp.AdoptedOutcomeProtocolVersion}
	}
	return nil
}

type outcomeAdoptionError struct {
	instance string
	version  uint32
}

func (e *outcomeAdoptionError) Error() string {
	return "原Battle创建明确未采用终局协议1，禁止追认"
}

func queryOutcomeCapability(ctx context.Context, node discovery.NodeEntry, req *battlepb.CreateBattleRequest) (*battlepb.QueryBattleCreationResponse, error) {
	conn, err := discovery.DialEndpoint(node.Endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := battlepb.NewBattleNodeClient(conn).QueryBattleCreation(ctx, &battlepb.QueryBattleCreationRequest{BattleId: req.BattleId, PlacementOperationId: req.PlacementOperationId, TargetInstanceId: req.TargetInstanceId})
	if err != nil {
		return nil, err
	}
	if resp.GetTargetInstanceId() != req.TargetInstanceId || resp.GetSupportedOutcomeProtocolVersion() != 1 || resp.GetState() != battlepb.BattleCreationState_BATTLE_CREATION_ABSENT {
		return nil, errors.New("原节点能力未知或原创建并非ABSENT，禁止发Create")
	}
	return resp, nil
}

// isBattleNotAllocatable 判定 CreateBattle 的 gRPC 错误是否为节点级准入拒绝(k8s-client-entry D82):
// 状态码 Unavailable 与消息 battle_not_allocatable 必须**同时精确**匹配。只看 Unavailable 不够 ——
// 连接断开、对端重启同样回 Unavailable,那时请求可能已送达、房间可能已建成。
func isBattleNotAllocatable(err error) bool {
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.Unavailable && st.Message() == battleNotAllocatableMessage
}

// createBattleRPC 是 createBattleFn 的生产实现。gRPC 状态错误必须原样返回、不在这里包装:
// 包装后 status.FromError 取到的消息是整段错误文本,isBattleNotAllocatable 判不中。
func createBattleRPC(endpoint string, req *battlepb.CreateBattleRequest) (*battlepb.CreateBattleResponse, error) {
	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createBattleTimeout)
	defer cancel()
	return battlepb.NewBattleNodeClient(conn).CreateBattle(ctx, req)
}

// teamAssignment 给一组成员分队,返回与 members 同下标的队号。5v5 读各成员
// 评分后按 assignBalancedTeams 蛇形平衡(§11;评分读失败者按默认分参与),
// 其余模式走 teamIndexFor 的成员序语义;1v1 再按 battle_id 决定两人是否对调。
func teamAssignment(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, battleId uint64, members []uint64) []uint32 {
	if mode == matchpb.MatchMode_MATCH_MODE_5V5 {
		ratings := loadRatings(svcCtx, members)
		teams := assignBalancedTeams(members, ratings)
		perMember := make([]string, 0, len(members))
		for i, pid := range members {
			perMember = append(perMember, fmt.Sprintf("%d=%s@t%d", pid, formatRating(ratings[pid]), teams[i]))
		}
		logx.Infof("[gather] 5v5 teams assigned by rating in snake order battle=%d members=%v", battleId, perMember)
		return teams
	}
	teams := make([]uint32, len(members))
	for i := range members {
		teams[i] = teamIndexFor(mode, i, len(members))
	}
	if mode == matchpb.MatchMode_MATCH_MODE_1V1 && len(members) == 2 {
		// 0 队不再恒归排在前面的锚点(等待最久者):按 battle_id 决定是否对调,同一局重放结果不变
		// (docs/design/global-match-hierarchical-pools.md §8)。切磋保持发起者在 0 队。
		flip := sideFlip(battleId)
		teams[0], teams[1] = teams[0]^flip, teams[1]^flip
	}
	return teams
}

// sideFlip 是 battle_id 经 splitmix64 终混后的最低位(0 或 1)。battle_id 现在已经打散,低位本身也均匀;
// 终混是 snowflake 时期留下的(当时低位是秒内序号,低并发时几乎恒为 0),保留它不依赖 id 的布局。
func sideFlip(battleId uint64) uint32 {
	x := battleId + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return uint32((x ^ (x >> 31)) & 1)
}

// teamIndexFor 决定参与者的队伍编号:1v1/切磋语义不变(memberIndex 即队号,
// 前后两人各一队);5v5 不走这里(teamAssignment 按评分蛇形分队,§11;
// 这里保留"前半 team 0、后半 team 1"作为无评分时的成员序语义,D15);
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
