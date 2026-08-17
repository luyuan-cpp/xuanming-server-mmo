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
	"google.golang.org/protobuf/proto"
)

// gather 各跳 RPC 的超时。
const (
	prepareBattleTimeout = 3 * time.Second
	createBattleTimeout  = 5 * time.Second
	rollbackTimeout      = 3 * time.Second
)

// preparedMember 记录一个已冻结(PrepareBattle 成功)的参与者,补偿时逐个解冻。
type preparedMember struct {
	playerId      uint64
	sceneEndpoint string
	snapshot      *battlepb.BattlePlayerSnapshot
}

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
// withTickets=false 用于切磋:参与者从未入队,没有 ticket 可推进。
func RunGather(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, battleConfigId uint32,
	members []uint64, requeueOnFail bool,
) {
	runGather(svcCtx, mode, battleConfigId, members, requeueOnFail, true)
}

// RunChallengeGather 是切磋入口的 gather:与队列匹配汇入同一条管线,
// 仅参与者没有排队票据(设计决策 D3:gather/补偿逻辑只存在一份)。
func RunChallengeGather(svcCtx *svc.ServiceContext, battleConfigId uint32, challengerId, responderId uint64) bool {
	return runGather(svcCtx, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE, battleConfigId,
		[]uint64{challengerId, responderId}, false, false)
}

func runGather(svcCtx *svc.ServiceContext, mode matchpb.MatchMode, battleConfigId uint32,
	members []uint64, requeueOnFail bool, withTickets bool,
) bool {
	start := time.Now()
	modeName := mode.String()

	fail := func(outcome string, failedPlayer uint64, prepared []preparedMember, battleId uint64) bool {
		cancelPrepared(svcCtx, prepared, battleId)
		if withTickets {
			if requeueOnFail {
				// 肇事成员出局删票,其余成员回队首(补偿矩阵 todo #7)。
				var survivors []uint64
				for _, pid := range members {
					if pid == failedPlayer {
						deleteTicket(svcCtx, pid)
						continue
					}
					survivors = append(survivors, pid)
				}
				requeueFront(svcCtx, int32(mode), battleConfigId, survivors)
			} else {
				for _, pid := range members {
					deleteTicket(svcCtx, pid)
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

	// 3. 逐参与者冻结收快照。
	var prepared []preparedMember
	for i, playerId := range members {
		endpoint, snapshot, err := preparePlayer(svcCtx, playerId, battleId, battleNode.NodeId, deadlineMs)
		if err != nil {
			logx.Errorf("[gather] PrepareBattle 失败 battle=%d player=%d(第 %d/%d 人): %v",
				battleId, playerId, i+1, len(members), err)
			outcome := "prepare_failed"
			if endpoint == "" {
				outcome = "no_location"
			}
			return fail(outcome, playerId, prepared, battleId)
		}
		snapshot.TeamIndex = teamIndexFor(mode, i)
		prepared = append(prepared, preparedMember{
			playerId:      playerId,
			sceneEndpoint: endpoint,
			snapshot:      snapshot,
		})
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
	if err := createBattle(battleNode, &battlepb.CreateBattleRequest{
		BattleId:       battleId,
		BattleConfigId: battleConfigId,
		Players:        snapshots,
		Seed:           seed,
		MatchMode:      uint32(mode),
		CreatedAtMs:    nowMs(),
		DeadlineMs:     deadlineMs,
	}); err != nil {
		logx.Errorf("[gather] CreateBattle 失败 battle=%d node=%d(%s): %v",
			battleId, battleNode.NodeId, battleNode.Endpoint, err)
		// 半成功兜底:RPC 超时时战斗可能已建成,先尽力 DestroyBattle 再解冻。
		destroyBattle(battleNode, battleId, "gather_rollback")
		return fail("create_failed", 0, prepared, battleId)
	}

	// 5. 成功收尾:ticket 推进 ready(短 TTL 自清),后续状态由
	//    battle 节点的 BattleStartS2C / 结算链路接管。
	if withTickets {
		for _, pid := range members {
			markTicketReady(svcCtx, pid, battleId)
		}
	}
	logx.Infof("[gather] 开局成功 battle=%d mode=%s config=%d node=%d members=%v 耗时=%s",
		battleId, modeName, battleConfigId, battleNode.NodeId, members, time.Since(start))
	metrics.ObserveGather(modeName, "success", time.Since(start))
	return true
}

// preparePlayer 定位玩家所在 scene 节点并调 PrepareBattle。
// 返回的 endpoint 为空表示还没定位到对端(位置缺失/节点未注册),
// 用于区分 no_location 与 prepare_failed 两类指标。
func preparePlayer(svcCtx *svc.ServiceContext, playerId, battleId uint64,
	battleNodeId uint32, deadlineMs uint64,
) (string, *battlepb.BattlePlayerSnapshot, error) {
	// player:{id}:location 由 scene_manager 写(protobuf PlayerLocation),
	// 是玩家 scene 定位的共享契约(设计文档 §5.4 / go/scene_manager changesceneutil.go)。
	raw, err := svcCtx.Redis.Get(getPlayerLocationKey(playerId))
	if err != nil {
		return "", nil, fmt.Errorf("读玩家位置失败: %w", err)
	}
	if raw == "" {
		return "", nil, fmt.Errorf("玩家位置不存在(不在线或未进场)")
	}
	loc := &smpb.PlayerLocation{}
	if err := proto.Unmarshal([]byte(raw), loc); err != nil {
		return "", nil, fmt.Errorf("玩家位置反序列化失败: %w", err)
	}

	endpoint, err := svcCtx.SceneNodes.EndpointOf(loc.ZoneId, loc.NodeId)
	if err != nil {
		return "", nil, fmt.Errorf("定位 scene 节点失败(zone=%d node=%s): %w", loc.ZoneId, loc.NodeId, err)
	}

	conn, err := discovery.DialEndpoint(endpoint)
	if err != nil {
		return endpoint, nil, fmt.Errorf("连接 scene 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepareBattleTimeout)
	defer cancel()
	// 走 gRPC 专用控制面 SceneNodeGrpc(scene.proto 的 Scene 服务是 cc_generic_services
	// 的 muduo TCP 形态,C++ 侧无法以 gRPC 实现,先例见 CreateScene/DestroyScene)。
	// 消息类型定义在 scene.proto(scenepb),SceneNodeGrpc 服务只引用不重复定义。
	resp, err := smpb.NewSceneNodeGrpcClient(conn).PrepareBattle(ctx, &scenepb.PrepareBattleRequest{
		PlayerId:     playerId,
		BattleId:     battleId,
		BattleNodeId: battleNodeId,
		DeadlineMs:   deadlineMs,
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
	return endpoint, resp.GetSnapshot(), nil
}

// cancelPrepared 对已冻结者逐个解冻(尽力而为;彻底失败由 scene 侧
// InBattleComp.deadline_ms 的 reaper 兜底,见补偿矩阵)。
func cancelPrepared(svcCtx *svc.ServiceContext, prepared []preparedMember, battleId uint64) {
	for _, p := range prepared {
		conn, err := discovery.DialEndpoint(p.sceneEndpoint)
		if err != nil {
			logx.Errorf("[gather] 解冻时连接 scene 失败 battle=%d player=%d endpoint=%s: %v",
				battleId, p.playerId, p.sceneEndpoint, err)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
		_, err = smpb.NewSceneNodeGrpcClient(conn).CancelBattlePrepare(ctx, &scenepb.CancelBattlePrepareRequest{
			PlayerId: p.playerId,
			BattleId: battleId,
		})
		cancel()
		if err != nil {
			logx.Errorf("[gather] CancelBattlePrepare 失败(scene reaper 会按 deadline 兜底解冻) battle=%d player=%d: %v",
				battleId, p.playerId, err)
			continue
		}
		logx.Infof("[gather] 已解冻 battle=%d player=%d", battleId, p.playerId)
	}
}

// createBattle 调 battle 节点建房。
func createBattle(node discovery.NodeEntry, req *battlepb.CreateBattleRequest) error {
	conn, err := discovery.DialEndpoint(node.Endpoint)
	if err != nil {
		return fmt.Errorf("连接 battle 节点失败: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), createBattleTimeout)
	defer cancel()
	resp, err := battlepb.NewBattleNodeClient(conn).CreateBattle(ctx, req)
	if err != nil {
		return fmt.Errorf("CreateBattle RPC 失败: %w", err)
	}
	if resp.GetErrorMessage().GetId() != 0 {
		return fmt.Errorf("battle 节点拒绝建房 tip_id=%d", resp.GetErrorMessage().GetId())
	}
	return nil
}

// destroyBattle 半成功回滚:CreateBattle 出错时战斗可能已建成,尽力销毁。
func destroyBattle(node discovery.NodeEntry, battleId uint64, reason string) {
	conn, err := discovery.DialEndpoint(node.Endpoint)
	if err != nil {
		logx.Errorf("[gather] DestroyBattle 时连接 battle 节点失败 battle=%d: %v", battleId, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	if _, err := battlepb.NewBattleNodeClient(conn).DestroyBattle(ctx, &battlepb.DestroyBattleRequest{
		BattleId: battleId,
		Reason:   reason,
	}); err != nil {
		// 销毁失败不致命:battle 房间收不到指令会按 deadline_ms 强制收尾。
		logx.Errorf("[gather] DestroyBattle 失败 battle=%d reason=%s: %v", battleId, reason, err)
	}
}

// teamIndexFor 决定参与者的队伍编号:PVP(1v1/切磋)前后两人各一队,
// PVE 全员 0 队(怪物侧由 battle 节点按 DungeonTable 生成,恒为 1 队对手)。
func teamIndexFor(mode matchpb.MatchMode, memberIndex int) uint32 {
	switch mode {
	case matchpb.MatchMode_MATCH_MODE_1V1, matchpb.MatchMode_MATCH_MODE_PVP_CHALLENGE:
		return uint32(memberIndex)
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
