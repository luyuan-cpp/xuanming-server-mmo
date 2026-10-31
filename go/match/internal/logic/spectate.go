package logic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"match/internal/authority"
	"match/internal/catalog"
	"match/internal/constants"
	"match/internal/svc"

	battlepb "proto/battle"
	matchpb "proto/match"

	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 观战索引(设计文档 §10.4;spectate:* 只有 match 读写,不变量 7)。权威落点与观战占用都在 SQL
// (match_formation / match_spectator),这里只是协调用途的展示索引,丢了不影响开局与补签:
//
//	spectate:battles:active       zset    member=battle_id 十进制,score=created_at_ms;只收公开局
//
// 观战档位与 L1 / L2 准入用的键见 spectate_tier.go。一场战斗的观战记录不进 Redis,读的时候由 SQL 成局现算(loadSpectateBattle)。
//
// 私密局(可见性非 PUBLIC,见 spectate_visibility.go)不进活跃集合。ZSET 成员没有 TTL,靠这几条路清理:
// 战斗终局提交时当场摘除(releaseSpectateOnOutcome)、battle 报房间不存在时懒剔除、读到过期 score 时现场剔除、
// 定期 ZREMRANGEBYSCORE 兜底(cleanupExpiredSpectateIndex)。
const spectateBattlesActiveKey = "spectate:battles:active"

// battle 节点观战 RPC 超时(与 gather 的 prepare/rollback 同量级)。removeObserverTimeout 进备战预算(matchedTicketTTLFor)。
const (
	addObserverTimeout    = 3 * time.Second
	removeObserverTimeout = 3 * time.Second
)

// battle 侧 AddObserver 的房间不存在信号 = table.CommonError_kEntityIsNull
// (与 battle_room_manager.cpp AddObserver 契约对齐,match 据此懒剔除索引);
// 观众席位满=MatchError_kMatchSpectateFull、墓碑表满=kRateLimitExceeded、观众是参战者=kInvalidParameter、
// 签不出票=kServiceUnavailable(turn-based §22 D70)不触发剔除 —— 这些情况战斗仍在打,索引是好的。
// 拒绝码到客户端结论的映射见 spectateRejectVerdict。

// spectateTTLSeconds 是观战键 TTL 与活跃集合的过期分界。成局的期限 = 提交时刻 + 备战预算(authority.WorkLeaseMs)+ 模板战斗时长,
// 模板时长上限是 catalog.MaxBattleSeconds,所以按上限算,再留 60s 吃掉 deadline 强制收尾与终局提交的时间差(与终局宽限同量)。
const spectateTTLSeconds = catalog.MaxBattleSeconds + int(authority.WorkLeaseMs/1000) + 60

// spectateRedisOpTimeout 每条观战索引 Redis 命令的 ctx 截止。go-zero 没开 go-redis 的 ContextTimeoutEnabled,这个截止只约束取连接、
// 拨号和内部重试之间的退避,socket 读仍可能等满 go-redis 默认 ReadTimeout(3s);这些命令都在开局关键路径之外,不进备战预算。
const spectateRedisOpTimeout = time.Second

// addSpectateActive 开局成功后把这场战斗加入活跃集合 spectate:battles:active(观战列表与随机选场)。best-effort,失败只记日志,
// 只是该场不进观战列表。活跃集合只收公开局:这里是唯一的 ZADD,在这里拒收非公开局,任何调用方都没法把私密局放进观战列表与随机选场。
// visibility 必须取建房原件冻结的 CreateBattleRequest.spectate_visibility,不得按模式另算一份:与 battle 房间同源,
// 执法点之间才不会各说各话。createdAtMs 与 CreateBattleRequest.created_at_ms 同一时刻取值,过期判定对齐 deadline。
func addSpectateActive(svcCtx *svc.ServiceContext, battleId, createdAtMs uint64, visibility battlepb.ESpectateVisibility) {
	if !isSpectatePublic(visibility) {
		logx.Infof("[spectate] Battle is not public; not listed for spectating battle=%d visibility=%s", battleId, visibility.String())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), spectateRedisOpTimeout)
	defer cancel()
	if _, err := svcCtx.MatchRedis.ZaddCtx(ctx, spectateBattlesActiveKey, int64(createdAtMs), strconv.FormatUint(battleId, 10)); err != nil {
		logx.Errorf("[spectate] Spectator index ZADD failed (replacement tickets unaffected; battle excluded from spectator list) battle=%d: %v",
			battleId, err)
		return
	}
	logx.Infof("[spectate] Battle registered for spectating battle=%d", battleId)
}

// loadSpectateBattle 由 SQL 成局组一场战斗的观战记录;不存在或已中止返回 (nil, nil)。
func loadSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64) (*matchpb.SpectateBattleRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f, e := svcCtx.Authority.Formation(ctx, battleId)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if f.State == 5 {
		return nil, nil
	}
	return spectateRecordFromFormation(f)
}

// spectateRecordFromFormation 由成局组观战记录(设计决策 D9)。可见性取持久原件里冻结的值(与 battle 房间同源),不按 f.Mode 重算;
// 实例与期限也取成局原件:L2 观众不调 battle RPC,观战凭证的这两个字段全靠这里(§15.6)。
func spectateRecordFromFormation(f *matchpb.MatchFormationRecord) (*matchpb.SpectateBattleRecord, error) {
	if len(f.CreateRequest) == 0 {
		return nil, errors.New("成局备战尚未完成")
	}
	create := &battlepb.CreateBattleRequest{}
	if err := proto.Unmarshal(f.CreateRequest, create); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(create.Players))
	for _, p := range create.Players {
		names = append(names, p.PlayerName)
	}
	return &matchpb.SpectateBattleRecord{
		Summary: &matchpb.BattleWatchSummary{
			BattleId:       f.BattleId,
			Mode:           matchpb.MatchMode(f.Mode),
			BattleConfigId: f.ConfigId,
			PlayerNames:    names,
			CreatedAtMs:    f.CreatedMs,
		},
		BattleNodeId:     f.TargetNodeId,
		Visibility:       create.GetSpectateVisibility(),
		BattleInstanceId: f.TargetInstanceId,
		DeadlineMs:       f.DeadlineMs,
	}, nil
}

// removeSpectateBattle 清掉一场已结束战斗的全部观战数据,幂等:battle 报房间不存在、成局缺失或已中止时懒剔除,
// 终局提交时也走这里。活跃集合成员、L2 配额与赛事局索引成员一并还掉:它们是全服共用的集合,
// 成员不随本场的键过期,等期限到才清会白占名额。档位、中继 gate 集合与赛事局标记同样删掉:它们带 TTL 兜底,
// 但留着会让这场结束之后到达的观众按旧档位签出收不到帧的凭证。
func removeSpectateBattle(svcCtx *svc.ServiceContext, battleId uint64) {
	removeSpectateActiveMember(svcCtx, battleId)
	member := strconv.FormatUint(battleId, 10)
	if _, err := svcCtx.MatchRedis.Zrem(spectateBroadcastActiveKey, member); err != nil {
		logx.Errorf("[spectate] Failed to release broadcast quota battle=%d: %v", battleId, err)
	}
	if _, err := svcCtx.MatchRedis.Zrem(spectateFeaturedActiveKey, member); err != nil {
		logx.Errorf("[spectate] Failed to remove featured index member battle=%d: %v", battleId, err)
	}
	// 这三个键共用 hash tag {id},落同一 slot,可以一条 DEL。
	if _, err := svcCtx.MatchRedis.Del(spectateTierKey(battleId), spectateRelayGatesKey(battleId), spectateFeaturedKey(battleId)); err != nil {
		logx.Errorf("[spectate] Failed to delete battle tier keys (TTL will clear them) battle=%d: %v", battleId, err)
	}
}

// releaseSpectateOnOutcome 在战斗终局提交之后尽力清掉这场的观战数据:结束的场立刻离开
// 观战列表与随机选场,L2 配额立刻还给别的场,否则要白占到战斗期限(最长约 1 小时),全服配额很快用尽。
// 放进独立任务:Redis 抖动不能拖慢终局回执,battle 收不到回执就会重投。任务发不出(本实例正在排空)只留日志,
// 由键的 TTL 与定期清理兜底。
func releaseSpectateOnOutcome(svcCtx *svc.ServiceContext, battleId uint64) {
	if !svcCtx.LaunchRelayTask("match.global.spectate.release", func() { removeSpectateBattle(svcCtx, battleId) }) {
		logx.Infof("[spectate] Spectate cleanup skipped on outcome (TTL and periodic cleanup will clear it) battle=%d", battleId)
	}
}

// removeSpectateActiveMember 只从活跃集合摘掉一场、不碰其余观战键:不该出现在集合里的私密成员走这里。
func removeSpectateActiveMember(svcCtx *svc.ServiceContext, battleId uint64) {
	if _, err := svcCtx.MatchRedis.Zrem(spectateBattlesActiveKey, strconv.FormatUint(battleId, 10)); err != nil {
		logx.Errorf("[spectate] Spectator index ZREM failed battle=%d: %v", battleId, err)
	}
}

// spectateStaleBeforeMs 返回过期分界:score(created_at_ms)早于该值的战斗
// 必已按 deadline 收尾,索引条目是残留。
func spectateStaleBeforeMs() uint64 {
	return nowMs() - uint64(spectateTTLSeconds)*1000
}

// pickRandomBattle 随机挑一场可观战战斗:ZCARD 取规模 + 随机下标 ZRANGE 单点读,
// 避免全量拉 ZSET。挑到过期/非法成员现场懒剔除后换一个,最多尝试 3 次;
// 挑不到返回 (0, nil)。随机用 math/rand 即可:选场不要求确定性
// (种子 RNG 纪律只约束 battle 引擎内部)。
func pickRandomBattle(svcCtx *svc.ServiceContext) (uint64, error) {
	staleBefore := spectateStaleBeforeMs()
	for attempt := 0; attempt < 3; attempt++ {
		count, err := svcCtx.MatchRedis.Zcard(spectateBattlesActiveKey)
		if err != nil {
			return 0, fmt.Errorf("观战索引 ZCARD 失败: %w", err)
		}
		if count <= 0 {
			return 0, nil
		}
		idx := int64(rand.Intn(count))
		pairs, err := svcCtx.MatchRedis.ZrangeWithScores(spectateBattlesActiveKey, idx, idx)
		if err != nil {
			return 0, fmt.Errorf("观战索引 ZRANGE 失败: %w", err)
		}
		if len(pairs) == 0 {
			// 并发剔除导致下标越界,重试。
			continue
		}
		battleId, err := strconv.ParseUint(pairs[0].Key, 10, 64)
		if err != nil {
			logx.Errorf("[spectate] Invalid member %q in spectator index; removing", pairs[0].Key)
			if _, err := svcCtx.MatchRedis.Zrem(spectateBattlesActiveKey, pairs[0].Key); err != nil {
				logx.Errorf("[spectate] Failed to remove invalid member: %v", err)
			}
			continue
		}
		if uint64(pairs[0].Score) < staleBefore {
			removeSpectateBattle(svcCtx, battleId)
			continue
		}
		return battleId, nil
	}
	return 0, nil
}

// observerCancelProven 判定 RemoveObserver 回包是不是取消证明。battle 对不合账本契约的 Remove(实例不符、op 非法、
// 截止与原件不同)回空包、不摘人,所以只有带回原 op 与实例的 cancelled 才算。
func observerCancelProven(req *battlepb.RemoveObserverRequest, reply *battlepb.RemoveObserverResponse) bool {
	return reply.GetCancelled() && reply.GetSpectatorOperationId() == req.GetSpectatorOperationId() &&
		reply.GetTargetInstanceId() == req.GetTargetInstanceId()
}

// spectateRejectVerdict 把 battle AddObserver 的拒绝码映射为客户端结论:
//   - kEntityIsNull(房间不存在,战斗已收尾)→ ErrBattleNotWatchable / not_found,roomMissing=true:调用方据此剔除索引;
//   - kMatchSpectateFull(直连观众席位已满)→ ErrSpectateFull / full:战斗仍在打,索引不动;
//   - 其它(墓碑表满 kRateLimitExceeded、观众是参战者 kInvalidParameter、签不出票 kServiceUnavailable)
//     → ErrBattleNotWatchable / rejected,索引不动。
func spectateRejectVerdict(battleTip uint32) (clientTip uint32, outcome string, roomMissing bool) {
	switch battleTip {
	case uint32(table.CommonError_kEntityIsNull):
		return constants.ErrBattleNotWatchable, "not_found", true
	case uint32(table.MatchError_kMatchSpectateFull):
		return constants.ErrSpectateFull, "full", false
	default:
		return constants.ErrBattleNotWatchable, "rejected", false
	}
}

// 观战记录与 RPC 对端都读 SQL 成局,这两条缝让列表、运营入口与 L1 / L2 准入的判定不起 TiDB 也能测(与 attachRelayGateFn 同一模式)。
var (
	loadSpectateBattleFn = loadSpectateBattle
	spectateTargetFn     = spectateTarget
)

// spectateTarget 定位观战 RPC 的对端:gRPC 地址与 battle 实例(观战账本的 target_instance_id),都取成局记录上固定的落点。
// 实例为空时 fail-closed:battle 按实例拒绝,空实例的请求必然被拒。
func spectateTarget(ctx context.Context, s *svc.ServiceContext, node uint32, battle uint64) (endpoint, instance string, err error) {
	f, e := s.Authority.Formation(ctx, battle)
	if e != nil {
		return "", "", e
	}
	if f.State != 4 {
		return "", "", errors.New("成局尚未确认或已经中止")
	}
	if f.TargetInstanceId == "" {
		return "", "", fmt.Errorf("battle 节点未登记实例 uuid(node=%d)", node)
	}
	return f.TargetEndpoint, f.TargetInstanceId, nil
}

// battleInstanceReplaced 判定注册表里这个 node_id 已经是别的实例:原实例的进程已被替换,房间随它消失。只读内存镜像,不发 RPC。
// 查不到不算:etcd 抖动、租约短暂失效、多条注册歧义时都查不到,不代表实例已死。
func battleInstanceReplaced(svcCtx *svc.ServiceContext, node uint32, instance string) bool {
	if svcCtx.BattleNodes == nil {
		return false
	}
	entry, err := svcCtx.BattleNodes.EntryOfNode(node)
	return err == nil && entry.NodeUuid != "" && entry.NodeUuid != instance
}

// cleanupExpiredSpectateIndex 清理观战索引里 score 早于 TTL 窗口的残留成员
// (ZSET 成员没有 TTL,只能靠懒剔除 + 这里的定期兜底)。O(log N + M) 轻量,多实例重复执行幂等,
// 由 startGlobalSpectateMaintenance 的低频循环执行。
func cleanupExpiredSpectateIndex(svcCtx *svc.ServiceContext) {
	staleBefore := int64(spectateStaleBeforeMs())
	if staleBefore <= 0 {
		return
	}
	removed, err := svcCtx.MatchRedis.Zremrangebyscore(spectateBattlesActiveKey, 0, staleBefore)
	if err != nil {
		logx.Errorf("[spectate] Failed to clean expired spectator index entries: %v", err)
		return
	}
	if removed > 0 {
		logx.Infof("[spectate] Cleaned %d expired spectator index entries", removed)
	}
	// 赛事局索引成员的 score 是到期毫秒,同一轮顺手清(L2 配额的过期项由抢配额的 Lua 自己清)。
	if _, err := svcCtx.MatchRedis.Zremrangebyscore(spectateFeaturedActiveKey, 0, int64(nowMs())); err != nil {
		logx.Errorf("[spectate] Failed to clean expired featured index entries: %v", err)
	}
}
