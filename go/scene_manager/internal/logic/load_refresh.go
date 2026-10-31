package logic

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/breaker"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
)

// 负载分刷新:领导者按 zone 批量刷新(2026-10-16)。入口是 load_reporter.go 的 refreshLoadScores
// (每拍 / fullSync)与 updateNodeLoad(watch PUT),两者都只在领导者上走到这里。
//
// 为什么只有领导者刷新:负载集与类型镜像的写只归领导者(单写者,理由见 updateNodeLoad 的注释)。
// 以前跟随者照样每拍逐个 GET 全服所有节点的两个计数、只是不写,读出来只用来发每节点指标 ——
// 开服规模下这是 0.8·Z·N_s GET/s 的白读,每个副本每拍还要串行 2·N_s 次往返(容量模型 §2.2,公式推算,
// 未实测)。现在跟随者一条 Redis 命令都不发,每节点指标也只由领导者导出。
//
// 为什么不是字面的「每个副本只刷新本 zone」:那需要每个 zone 各有一个写者,破坏单写者不变量,还要为
// 滚动升级另加兜底。每 zone 一个领导者、watch 收窄归 A1(容量模型 §3);这里的「按 zone 分批」正是
// 那时可以直接复用的形状。
//
// 一次刷新的形状(refreshZoneLoadSets):
//  1. 一条 SADD 把本次涉及的全部 zone 登记进 scene_manager:zones(尽力而为,见 zone_registry.go);
//  2. 读:pipeline 分批,每个节点一条 GET(scene_count)、一条 SCARD(成员集合);
//  3. 写之前再判一次领导权;pipeline 分批写:每个节点一条 SET 类型镜像,每个 zone 一条 ZADD 写读成功的
//     节点,再加一条 ZADD NX 把读失败的节点以哨兵分加进去(已在负载集里的不受影响);
//  4. 计数读成功、且所在 zone 的分数 ZADD 确认写成功的节点发每节点指标(导出的分数必须是 Redis 里生效的那个)。
//
// 读失败语义(fail-closed):
//   - 不按 0 写:按 0 写会把一个满载节点伪装成最空闲的,下一批进场全压上去;
//   - 已在负载集里的成员保留旧分;
//   - 不在负载集里的节点用 ZADD NX 以哨兵分(nodeLoadScoreUnknown)加入。**不能因为读失败扣下成员资格**:
//     负载集成员资格就是存活信号 —— IsNodeAlive 把「不在负载集」当作唯一的死亡证据,
//     playerLocationOwnerGone 把 ZCARD==0 当作整 zone 已死的条件之一(另须 etcd 注册表里该 zone 已无节点),world_rebalance 用 ZSCORE
//     判存活。Redis 丢了成员(或节点刚注册)时若因计数读失败就不加回去,活节点会被判死,在老属主仍在存盘时
//     放行改派 / 接管,即双写 / 回档。哨兵分让它在选点里排到最后,下一拍读成功后自然纠正。
//
// pipeline 结果要按批判定(2026-10-16 对照模块缓存里 go-zero v1.10.0 与 go-redis v9.17.3 的源码核实):
//   - go-zero 的熔断挂在 ProcessPipelineHook 上(core/stores/redis/breakerhook.go):熔断打开时直接返回
//     breaker.ErrServiceUnavailable,不调用下一层 —— 队列里的命令一条都没执行,各自的 err 也不回填,
//     StringCmd.Result() 是 ("", nil);
//   - 取连接 / 网络出错时 go-redis 把错误回填到每条命令(redis.go generalProcessPipeline 的 setCmdsErr);
//   - 正常执行时返回值是第一条带错命令的错误(cmdsFirstErr),redis.Nil 也算。
//
// 所以 PipelinedCtx 的返回值既不能直接当判据(redis.Nil 也会出现在这里),也不能忽略(熔断拒绝只体现在
// 这里)。判定见 pipelineBatchRejected 与 classifyCounterReply。
//
// outcome 上 read_failed 只记传输层失败(批被拒,或有命令带着非 Redis 回复的错误:取连接 / 网络 / 超时),
// 判定见 pipelineHitTransportError;个别键的逐条回复错误(WRONGTYPE)与坏值(空串 / 非数字)不论批多大
// 都只记 partial。按「整批全错」判会让同一个坏键在独占尾批、或部署里只有 1 个节点时记 read_failed,
// 与别的节点同批时记 partial,告警随节点增减时响时不响。

const (
	// loadRefreshPipelineMaxCmds:单个 pipeline 批次的命令数上限。批太大会拉长 Redis 单次处理时间、抬高
	// 其它客户端的尾延迟;太小则往返次数多。1024 是起步值,对其它客户端尾延迟的影响未压测(AGENTS §6)。
	loadRefreshPipelineMaxCmds = 1024
	// loadRefreshNodesPerReadBatch:读批里每个节点两条命令。
	loadRefreshNodesPerReadBatch = loadRefreshPipelineMaxCmds / 2

	// nodeLoadScoreUnknown:计数读不到、又不在负载集里的节点以这个分数加入负载集(ZADD NX)。
	// 选点按分数升序取(node_selection.go getNodesForPurpose 保持 ZRANGE 的升序),所以它最后才会被选中;
	// 真实负载分是 α·scene_count + β·player_count,碰不到 1e12。它只写进 Redis:读失败的节点本次不调
	// ObserveNode,scene_manager_node_load_score 不会出现这个值,看板与告警不会把它当负载画出来。
	nodeLoadScoreUnknown int64 = 1_000_000_000_000
)

// nodesByRoleKey 是 metrics.SetNodesByRole 的计数键。类型别名,与其签名里的匿名结构体是同一个类型。
type nodesByRoleKey = struct {
	ZoneID uint32
	Role   uint32
}

// loadRefreshNode 是一次刷新里的一个 (zone,node) 身份(已按 nodeID 去重),以及它的计数读取结果。
type loadRefreshNode struct {
	zoneID      uint32
	nodeID      string
	role        uint32
	sceneCount  int64
	playerCount int64
	readOK      bool
}

// loadRefreshReport 汇总一次刷新,outcome() 折成 metrics.LoadRefreshOutcome* 之一。
type loadRefreshReport struct {
	nodeReadErrors      int  // 计数没读到的节点数(含传输层失败与逐条回复错误 / 坏值)
	readTransportFailed bool // 至少一批读遇到传输层失败(熔断拒绝 / 取连接 / 网络 / 超时),见 pipelineHitTransportError
	registryFailed      bool
	writeFailed         bool
	lostLeadership      bool
}

// outcome 在多种情况同时出现时只取一个:write_failed > read_failed > registry_failed > lost_leadership
// > partial > ok。前三种是告警口径(SceneManagerLoadRefreshFailing),排在前面才不会被别的盖掉。
func (r loadRefreshReport) outcome() string {
	switch {
	case r.writeFailed:
		return metrics.LoadRefreshOutcomeWriteFailed
	case r.readTransportFailed:
		return metrics.LoadRefreshOutcomeReadFailed
	case r.registryFailed:
		return metrics.LoadRefreshOutcomeRegistryFailed
	case r.lostLeadership:
		return metrics.LoadRefreshOutcomeLostLeadership
	case r.nodeReadErrors > 0:
		return metrics.LoadRefreshOutcomePartial
	default:
		return metrics.LoadRefreshOutcomeOK
	}
}

// snapshotLoadRefreshNodes 在 knownNodesMu 读锁内拷出本次要刷新的节点与 nodes_by_role 计数,出锁后再做
// Redis I/O。以前持读锁逐个 GET,锁会被占住 2·N_s 次往返。knownNodes 的写方(fullSync 与 watch 事件)
// 和刷新都在 load reporter 这一条 goroutine 上,拷贝之后才做的 I/O 不会与它们交错。
//
// 每个 zone 内按 nodeID 去重:同一身份有多条注册时,取 etcd key 字典序最小那条的类型。这种身份会被
// getNodesForPurpose / IsNodeAlive 按 isKnownNodeIdentityAmbiguous 拒绝,取哪条不影响选点;计数键只按
// (zone,node) 分,一个身份读写一次就够。nodes_by_role 仍按注册条数计,与旧口径一致。
func snapshotLoadRefreshNodes() (map[uint32][]loadRefreshNode, map[nodesByRoleKey]int) {
	knownNodesMu.RLock()
	defer knownNodesMu.RUnlock()

	keys := make([]string, 0, len(knownNodes))
	for k := range knownNodes {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	type identity struct {
		zoneID uint32
		nodeID string
	}
	seen := make(map[identity]struct{}, len(keys))
	byZone := make(map[uint32][]loadRefreshNode)
	counts := make(map[nodesByRoleKey]int)
	for _, k := range keys {
		entry := knownNodes[k]
		counts[nodesByRoleKey{ZoneID: entry.reg.ZoneId, Role: entry.reg.SceneNodeType}]++
		id := identity{zoneID: entry.reg.ZoneId, nodeID: entry.nodeID}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		byZone[id.zoneID] = append(byZone[id.zoneID], loadRefreshNode{
			zoneID: id.zoneID,
			nodeID: id.nodeID,
			role:   entry.reg.SceneNodeType,
		})
	}
	return byZone, counts
}

// refreshZoneLoadSets 按 zone 批量刷新 byZone 里全部节点的负载分。**只在领导者上调用**(调用方判领导权);
// 写之前与写批之间还会再判,中途降级即停。byZone 里每个 zone 的节点须已按 nodeID 去重。
// 失败只打英文日志、记进返回的 report,不返回 error:下一拍会整体重来。
func refreshZoneLoadSets(svcCtx *svc.ServiceContext, byZone map[uint32][]loadRefreshNode) loadRefreshReport {
	var report loadRefreshReport
	zones := make([]uint32, 0, len(byZone))
	for z, nodes := range byZone {
		if len(nodes) > 0 {
			zones = append(zones, z)
		}
	}
	if len(zones) == 0 {
		return report
	}
	slices.Sort(zones)
	ctx := context.Background()

	// ① 登记先于负载集写入。失败不扣成员资格,ZADD 照常写、不加 XX(理由见 zone_registry.go)。
	if err := registerSceneManagerZones(svcCtx, zones...); err != nil {
		report.registryFailed = true
		logx.Errorf("[LoadRefresh] zone registry write failed; load sets are still written and registration retries next tick: %v", err)
	}

	// ② 读。nodes 指向 byZone 各切片里的元素,读结果直接回填。
	nodes := make([]*loadRefreshNode, 0)
	for _, z := range zones {
		zoneNodes := byZone[z]
		for i := range zoneNodes {
			nodes = append(nodes, &zoneNodes[i])
		}
	}
	readLoadRefreshCounters(ctx, svcCtx, nodes, &report)

	// ③ 读可能花了几百毫秒,写之前再判一次领导权:降级后的写正是单写者不变量要防的那种写。
	if !isLeader() {
		report.lostLeadership = true
		logx.Infof("[LoadRefresh] lost leadership before writing load scores; successor refreshes on its next tick")
		return report
	}

	// ④ 写。
	unwrittenScoreZones := writeLoadRefreshScores(ctx, svcCtx, buildLoadRefreshWrites(svcCtx, zones, byZone), &report)
	if report.lostLeadership {
		return report
	}

	// ⑤ 每节点指标只发「计数读成功、且所在 zone 的分数 ZADD 确认写成功」的节点:读失败的节点没有可信的计数,
	// 也不能把哨兵分发出去;写没落地时导出的分数与 Redis 里实际生效的不一致。
	// 没发的节点序列停在上一次成功的值,不 ForgetNode:单拍抖动会让序列闪断,把 WorldNodeSaturated /
	// LoadScoreDispersionLow 的 for 计时清零。代价是持续读不到的节点在看板与这两条告警里一直是旧值(非成员还一直
	// 挂着哨兵分、选点排最后),由告警 SceneManagerLoadRefreshNodeErrors(node_errors_total 持续增长)兜住;
	// 写失败由 SceneManagerLoadRefreshFailing 兜住。
	for _, n := range nodes {
		if !n.readOK {
			continue
		}
		if _, unwritten := unwrittenScoreZones[n.zoneID]; unwritten {
			continue
		}
		metrics.ObserveNode(n.nodeID, n.zoneID, n.role, n.sceneCount, n.playerCount,
			computeNodeLoadScore(svcCtx, n.sceneCount, n.playerCount))
	}
	return report
}

// counterReply 是 pipeline 里一条 GET 的结果(*redis.StringCmd 满足它;用接口是为了不直接 import go-redis)。
type counterReply interface {
	Result() (string, error)
}

// errReply 是 pipeline 里一条写命令的结果(*redis.Cmd 满足它)。
type errReply interface {
	Err() error
}

// memberCountReply 是 pipeline 里一条 SCARD 的结果(*redis.IntCmd 满足它)。
type memberCountReply interface {
	Result() (int64, error)
}

// readLoadRefreshCounters 分批 pipeline 读每个节点的 scene_count 与成员人数,结果回填到 nodes。
func readLoadRefreshCounters(ctx context.Context, svcCtx *svc.ServiceContext, nodes []*loadRefreshNode, report *loadRefreshReport) {
	for start := 0; start < len(nodes); start += loadRefreshNodesPerReadBatch {
		batch := nodes[start:min(start+loadRefreshNodesPerReadBatch, len(nodes))]
		sceneReplies := make([]counterReply, len(batch))
		playerReplies := make([]memberCountReply, len(batch))
		// 取连接失败时回调不会被调用,两个切片里全是 nil,replyResult 按「未执行」处理。
		batchErr := svcCtx.Redis.PipelinedCtx(ctx, func(p redis.Pipeliner) error {
			for i, n := range batch {
				sceneReplies[i] = p.Get(ctx, nodeSceneCountKey(n.zoneID, n.nodeID))
				playerReplies[i] = p.SCard(ctx, nodeMembersKey(n.zoneID, n.nodeID))
			}
			return nil
		})

		cmdErrs := make([]error, 0, 2*len(batch))
		for i := range batch {
			_, sceneErr := replyResult(sceneReplies[i])
			_, playerErr := memberCountResult(playerReplies[i])
			cmdErrs = append(cmdErrs, sceneErr, playerErr)
		}
		rejected := pipelineBatchRejected(batchErr, cmdErrs)
		// read_failed 只记传输层失败;个别键的回复错误 / 坏值只记 partial,理由见文件头。
		transportFailed := rejected || pipelineHitTransportError(cmdErrs)

		readOK := 0
		var firstFailure string
		for i, n := range batch {
			if rejected {
				continue
			}
			sceneVal, sceneErr := replyResult(sceneReplies[i])
			playerCount, playerErr := memberCountResult(playerReplies[i])
			sceneCount, sceneOK := classifyCounterReply(sceneVal, sceneErr, batchErr)
			playerOK := classifyMemberCountReply(playerReplies[i], playerErr, batchErr)
			if sceneOK && playerOK {
				n.sceneCount, n.playerCount, n.readOK = sceneCount, playerCount, true
				readOK++
				continue
			}
			if firstFailure == "" {
				firstFailure = fmt.Sprintf("zone=%d node=%s scene_count=(%q, %v) members=(%d, %v)",
					n.zoneID, n.nodeID, sceneVal, sceneErr, playerCount, playerErr)
			}
		}

		failed := len(batch) - readOK
		report.nodeReadErrors += failed
		switch {
		case transportFailed:
			report.readTransportFailed = true
			logx.Errorf("[LoadRefresh] counter read pipeline hit a transport-level failure (breaker/connection/timeout): %d/%d nodes keep their scores (non-members join with the unknown-load sentinel): rejected=%t batch_err=%v first=%s",
				failed, len(batch), rejected, batchErr, firstFailure)
		case failed > 0:
			logx.Errorf("[LoadRefresh] %d/%d node counter reads failed; their scores are kept (non-members join with the unknown-load sentinel): first %s",
				failed, len(batch), firstFailure)
		}
	}
}

// replyResult 取一条 GET 的结果;回调没被调用(r 为 nil)时按「未执行」返回 ("", nil),由
// pipelineBatchRejected / classifyCounterReply 判成失败。
func replyResult(r counterReply) (string, error) {
	if r == nil {
		return "", nil
	}
	return r.Result()
}

// memberCountResult 取一条 SCARD 的结果;回调没被调用(r 为 nil)时按「未执行」返回 (0, nil),
// 由 pipelineBatchRejected / classifyMemberCountReply 判成失败。
func memberCountResult(r memberCountReply) (int64, error) {
	if r == nil {
		return 0, nil
	}
	return r.Result()
}

// pipelineBatchRejected 判断整批 pipeline 是否应按「没有执行」处理。batchErr 是 PipelinedCtx 的返回值,
// cmdErrs 是每条命令自身的 err(回调没被调用时全是 nil)。
//
//   - batchErr 为 nil 或 redis.Nil:批已执行,逐条判;
//   - batchErr 是熔断拒绝(breaker.ErrServiceUnavailable):整批没执行;
//   - 其它 batchErr,且没有任何一条命令自己带错:错误没落到任何命令上(回调未被调用、钩子拒绝),整批没执行;
//   - 其它 batchErr,且有命令自己带错:批已执行(或网络错误已回填到每条命令),逐条判。
func pipelineBatchRejected(batchErr error, cmdErrs []error) bool {
	if batchErr == nil || errors.Is(batchErr, redis.Nil) {
		return false
	}
	if errors.Is(batchErr, breaker.ErrServiceUnavailable) {
		return true
	}
	for _, err := range cmdErrs {
		if err != nil {
			return false
		}
	}
	return true
}

// pipelineHitTransportError 判断一批已执行的命令里有没有传输层失败:某条命令自身带错,且不是 Redis 的逐条
// 回复错误。go-redis 遇到取连接 / 网络 / 超时 / 协议解析错误时把它回填到当前及后续命令(v9.17.3 redis.go
// pipelineReadCmds 与 generalProcessPipeline 的 setCmdsErr)。
//
// 只看类型,不拿它与 batchErr 比较(errors.Is):重试用尽时 generalProcessPipeline 直接返回最后一轮的错误,
// 命令身上留的可能是更早一轮的错误实例(比如第一轮读到 EOF、之后几轮拨号被拒)。也不要求「每条都带错」:
// 连接在批中途断开时前面的命令已经成功,按「每条都带错」判又会让结果取决于批在哪里切开。
func pipelineHitTransportError(cmdErrs []error) bool {
	for _, err := range cmdErrs {
		if err != nil && !isRedisReplyError(err) {
			return true
		}
	}
	return false
}

// isRedisReplyError 判断 err 是不是 Redis 服务端对这一条命令的回复错误(-ERR / -WRONGTYPE / redis.Nil 等)。
// 与 go-redis 的 isRedisError(v9.17.3 error.go)同义:它的回复错误类型都带 RedisError() 方法;用接口判,
// 不直接 import go-redis。
func isRedisReplyError(err error) bool {
	var replyErr interface{ RedisError() }
	return errors.As(err, &replyErr)
}

// classifyCounterReply 把 pipeline 里一条计数 GET 的结果判成 (计数, 是否可信)。val / err 是这条命令自己的
// 结果,batchErr 是整批 PipelinedCtx 的返回值。
//
//   - 熔断拒绝(batchErr 是 breaker.ErrServiceUnavailable):命令没执行,不可信;
//   - 命令自身 redis.Nil:键不存在,计数就是 0(节点刚注册、还没有任何 INCR);
//   - 命令自身其它错误(网络 / WRONGTYPE):不可信;
//   - ("", nil):命令没执行(熔断 / 回调未被调用),或值被写成了空串 —— 两种都不可信。go-zero 的
//     非 pipeline Get 会把 redis.Nil 吞成 ("", nil),pipeline 不会,这里不能沿用 readInt64 的「空串当 0」;
//   - 不是十进制整数:不可信。
//
// 只有 batchErr 是熔断拒绝才压过单条结果:别的 batchErr 只是「第一条带错命令的错误」,与本条无关。
func classifyCounterReply(val string, err error, batchErr error) (int64, bool) {
	if batchErr != nil && errors.Is(batchErr, breaker.ErrServiceUnavailable) {
		return 0, false
	}
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, true
		}
		return 0, false
	}
	if val == "" {
		return 0, false
	}
	n, perr := strconv.ParseInt(val, 10, 64)
	if perr != nil {
		return 0, false
	}
	return n, true
}

// classifyMemberCountReply 判一条 SCARD 的结果可不可信。键不存在时 SCARD 回 0,是可信的「没有人」,但熔断拒绝时
// IntCmd 的零值同样是 (0, nil),不能读成 0 人:
//
//   - 回调没被调用(r 为 nil)或熔断拒绝:命令没执行,不可信;
//   - 命令自身带错(网络 / WRONGTYPE):不可信;
//   - 其余可信。
func classifyMemberCountReply(r memberCountReply, err error, batchErr error) bool {
	if r == nil || (batchErr != nil && errors.Is(batchErr, breaker.ErrServiceUnavailable)) {
		return false
	}
	return err == nil
}

// loadRefreshWriteCmd 是写批里的一条命令。zoneID 用于出错时的日志与按 zone 判定指标能否导出;
// scores 标记「读成功节点的分数 ZADD」:它没落地时,该 zone 本拍不发每节点指标。
type loadRefreshWriteCmd struct {
	zoneID uint32
	scores bool
	args   []any
}

// buildLoadRefreshWrites 按 zone 生成写命令:每个节点一条 SET 类型镜像;读成功的节点合成一条 ZADD;
// 读失败的节点合成一条 ZADD NX(哨兵分,已在负载集里的成员不受影响)。
//
// 类型镜像排在 ZADD 前面,而且不论读成败都写(类型来自 etcd 注册,与计数无关):新成员进负载集时已经带
// 类型,缩短 getNodesForPurpose 把「没有类型镜像」的节点当作未分类、放进所有池子的窗口。只有镜像没有
// 成员是无害的(getNodesForPurpose 只遍历负载集)。
//
// 分数 = int64(computeNodeLoadScore(...)),与旧的 go-zero Zadd(int64) 截断一致。
func buildLoadRefreshWrites(svcCtx *svc.ServiceContext, zones []uint32, byZone map[uint32][]loadRefreshNode) []loadRefreshWriteCmd {
	var cmds []loadRefreshWriteCmd
	for _, z := range zones {
		loadKey := nodeLoadKey(z)
		scored := []any{"ZADD", loadKey}
		unknown := []any{"ZADD", loadKey, "NX"}
		for _, n := range byZone[z] {
			cmds = append(cmds, loadRefreshWriteCmd{zoneID: z, args: []any{
				"SET", nodeSceneNodeTypeKey(z, n.nodeID), strconv.FormatUint(uint64(n.role), 10),
			}})
			if n.readOK {
				scored = append(scored, int64(computeNodeLoadScore(svcCtx, n.sceneCount, n.playerCount)), n.nodeID)
			} else {
				unknown = append(unknown, nodeLoadScoreUnknown, n.nodeID)
			}
		}
		if len(scored) > 2 {
			cmds = append(cmds, loadRefreshWriteCmd{zoneID: z, scores: true, args: scored})
		}
		if len(unknown) > 3 {
			cmds = append(cmds, loadRefreshWriteCmd{zoneID: z, args: unknown})
		}
	}
	return cmds
}

// writeLoadRefreshScores 分批 pipeline 执行写命令。批与批之间判领导权,降级即停。
// 某一批被整体拒绝(熔断)时继续后面的批:熔断是本地按比例丢请求,后面的批可能放行。
//
// 返回分数 ZADD 没有确认写成功的 zone(该条命令失败,或所在批被整体拒绝),调用方据此不导出这些 zone 的
// 每节点指标。降级中途停下时不统计剩余批次:调用方此时整拍放弃指标。
// 失败按批聚合成一行日志:网络错误被回填到整批命令时,逐条打会在 Redis 故障期间每拍放大成 N_s+Z 行。
func writeLoadRefreshScores(ctx context.Context, svcCtx *svc.ServiceContext, cmds []loadRefreshWriteCmd, report *loadRefreshReport) map[uint32]struct{} {
	unwrittenScoreZones := make(map[uint32]struct{})
	for start := 0; start < len(cmds); start += loadRefreshPipelineMaxCmds {
		if start > 0 && !isLeader() {
			report.lostLeadership = true
			logx.Infof("[LoadRefresh] lost leadership mid-write; remaining %d commands dropped, successor refreshes on its next tick",
				len(cmds)-start)
			return unwrittenScoreZones
		}
		batch := cmds[start:min(start+loadRefreshPipelineMaxCmds, len(cmds))]
		replies := make([]errReply, len(batch))
		batchErr := svcCtx.Redis.PipelinedCtx(ctx, func(p redis.Pipeliner) error {
			for i, c := range batch {
				replies[i] = p.Do(ctx, c.args...)
			}
			return nil
		})

		cmdErrs := make([]error, len(batch))
		for i, r := range replies {
			if r != nil {
				cmdErrs[i] = r.Err()
			}
		}
		rejected := pipelineBatchRejected(batchErr, cmdErrs)

		failedCmds := 0
		var firstFailure string
		var minZone, maxZone uint32
		for i, c := range batch {
			if !rejected && cmdErrs[i] == nil {
				continue
			}
			if c.scores {
				unwrittenScoreZones[c.zoneID] = struct{}{}
			}
			if failedCmds == 0 {
				minZone, maxZone = c.zoneID, c.zoneID
				firstFailure = fmt.Sprintf("%v %v: %v", c.args[0], c.args[1], cmdErrs[i])
			}
			minZone, maxZone = min(minZone, c.zoneID), max(maxZone, c.zoneID)
			failedCmds++
		}
		if failedCmds == 0 {
			continue
		}
		report.writeFailed = true
		if rejected {
			logx.Errorf("[LoadRefresh] write pipeline of %d commands (zones %d..%d) was not executed: %v",
				len(batch), minZone, maxZone, batchErr)
			continue
		}
		logx.Errorf("[LoadRefresh] %d/%d write commands failed (zones %d..%d); per-node metrics of zones whose score ZADD failed are not exported this tick: first %s",
			failedCmds, len(batch), minZone, maxZone, firstFailure)
	}
	return unwrittenScoreZones
}
