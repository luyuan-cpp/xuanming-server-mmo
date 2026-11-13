package inbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"player_inbox/internal/constants"
	"player_inbox/internal/metrics"

	componentpb "proto/common/component"
	inboxpb "proto/player_inbox"

	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// 对账(设计 §9.3):收件箱的终结记录 vs 玩家**已落盘**的消费账本(player_database.inbox_ledger)。
//
// 账本经 data_service.GetPlayerInboxLedger 读(它读 scene 存盘写的那份 blob,不回退读 MySQL,
// 语义同 GetPlayerAssetOpLedger)。读不到存档(found=false)计为 unknown,不告警;读失败计 error;
// 账本结构不合法(validLedger,与 C++ ValidateLedger 同口径)计 invalid —— 结论是「无法核对」,不是「无差异」。
// 两个入口共用同一个判定谓词 finalizedLedgerGap(decide.go):比水位,也比最近结局环(评审 M6)。

// LedgerReader 读一名玩家已落盘的收件箱账本。found=false = zone Redis 里没有这名玩家的 blob;
// found=true 时账本非 nil(玩家从未消费过收件箱时是空消息)。实现必须可并发调用:定时对账以有界并发读账本。
type LedgerReader interface {
	ReadInboxLedger(ctx context.Context, playerID uint64) (ledger *componentpb.PlayerInboxLedgerComp, found bool, err error)
}

// DecidedThrough 返回账本里某个域的水位;账本里没有该域时为 0。按 domain 字段查找,不靠下标(AGENTS §11.6)。
func DecidedThrough(ledger *componentpb.PlayerInboxLedgerComp, domain inboxpb.PlayerInboxDomain) uint64 {
	for _, c := range ledger.GetCursors() {
		if c.GetDomain() == uint32(domain) {
			return c.GetDecidedThroughSeq()
		}
	}
	return 0
}

// ReconcilePlayer 是运维手工对账入口:按单个玩家比对收件箱与已落盘账本,报告两类差异
// (FINAL_NOT_IN_LEDGER / PENDING_BUT_DECIDED,见 reconcileDiff)。只读,不改任何状态。
// 账本结构不合法时返回 error(适配层折成 kPlayerInboxStoreUnavailable 并记 ERROR):那份账本当不了证据,
// 回一个空的 diffs 会被读成「无差异」。
func (s *Store) ReconcilePlayer(ctx context.Context, req *inboxpb.ReconcilePlayerInboxRequest, ledgers LedgerReader) (*inboxpb.ReconcilePlayerInboxResponse, error) {
	resp := &inboxpb.ReconcilePlayerInboxResponse{}
	domain := req.GetDomain()
	if req.GetPlayerId() == 0 {
		resp.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return resp, nil
	}
	if domain != inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_UNSPECIFIED {
		if _, ok := s.registry.Domain(domain); !ok {
			resp.ErrorMessage = tipOf(constants.ErrInvalidParameter)
			return resp, nil
		}
	}
	if ledgers == nil {
		return nil, fmt.Errorf("inbox: ReconcilePlayer has no ledger reader")
	}
	readCtx, cancel := context.WithTimeout(ctx, constants.LedgerReadTimeout)
	ledger, found, err := ledgers.ReadInboxLedger(readCtx, req.GetPlayerId())
	cancel()
	if err != nil {
		return nil, fmt.Errorf("inbox: read persisted ledger player=%d: %w", req.GetPlayerId(), err)
	}
	resp.LedgerFound = found
	if !found {
		return resp, nil // unknown:读不到存档,不比对
	}
	if !validLedger(ledger) {
		return nil, fmt.Errorf("inbox: persisted inbox ledger of player=%d is structurally invalid "+
			"(duplicate/zero domain, ring not ascending or beyond the watermark, unrecordable outcome, zero request_id, "+
			"or more than %d ring entries); reconcile result is unknown", req.GetPlayerId(), ledgerRecentCapacity)
	}

	// 一页一页比对,只留有差异的行:读完这名玩家的全部条目行,读不全就回错误(forEachPlayerItemsPage)。
	var diffs []*inboxpb.PlayerInboxReconcileDiff
	err = s.forEachPlayerItemsPage(ctx, req.GetPlayerId(), domain, playerItemsPageSize, func(rows []*inboxpb.PlayerInboxItemRow) error {
		diffs = appendReconcileDiffs(diffs, ledger, rows)
		return nil
	})
	if err != nil {
		return nil, err
	}

	shard := RowShard(req.GetPlayerId())
	domains := []inboxpb.PlayerInboxDomain{domain}
	if domain == inboxpb.PlayerInboxDomain_PLAYER_INBOX_DOMAIN_UNSPECIFIED {
		domains = s.registry.Domains()
	}
	streams := make(map[inboxpb.PlayerInboxDomain]*inboxpb.PlayerInboxStream, len(domains))
	for _, d := range domains {
		st, err := readStream(ctx, s.db, shard, req.GetPlayerId(), d)
		if err != nil {
			return nil, err
		}
		if st != nil {
			streams[d] = st
		}
	}
	resp.Cursors, resp.Diffs = reconcileCursors(domains, streams, ledger), diffs
	return resp, nil
}

// 定时对账一轮的工作量上限(GM 域量很小,设计 §9.3;评审 M5)。
const (
	// reconcilePageSize:单片一次读多少行(键集翻页,见 RecentlyFinalized)。
	reconcilePageSize = 100
	// reconcilePerShardLimit:单片一轮最多核对多少行。读满仍有剩余 = 本片截断(先核对最新的,截掉最老的),
	// 继续下一片 —— 一个热点分片不该吃光整轮预算。
	reconcilePerShardLimit = 500
	// reconcileMaxPlayers:一轮最多读多少名玩家的账本(每名一次 data_service 调用)。数到上限 = 本轮截断,
	// 停在当前分片;下一轮的起始分片按墙钟轮转(reconcileStartShard,步长 reconcileStartStride)。
	reconcileMaxPlayers = 2000
	// reconcileLedgerReadConcurrency:账本读的并发路数。串行读 2000 名玩家(每次上限 LedgerReadTimeout = 2s)
	// 会撞穿 2 分钟的轮预算;8 路时 2000 次 × 典型 10ms ≈ 2.5s,同时给 data_service 的压力有上界。
	reconcileLedgerReadConcurrency = 8

	// reconcileStartStride:相邻两轮的起始分片相差几片(评审 R1,reconcileStartShard 用)。
	// 取 reconcileMaxPlayers / reconcilePerShardLimit = 4:一片至多收 reconcilePerShardLimit 行、也就至多这么多名
	// 新玩家,所以不论玩家数在哪一片触顶,从起点起的前 4 片一定读完(至多单片上限那么多行)—— 这是每轮覆盖的下界。
	// 起点每轮前移 4 片,相邻轮次的必读段首尾相接,连续 reconcileFullCycleRounds 轮必然把 64 片各读完一遍。
	// 原来每轮只前移 1 片:截断持续时一轮只读到 [start, start+c),一行终结时若落在这一段之后 c+W−1 片以外,
	// 直到滑出 W 轮的窗口都核对不到(默认 W = 24、c ≈ 32 时约 9/64 的行)。
	reconcileStartStride = reconcileMaxPlayers / reconcilePerShardLimit
	// reconcileFullCycleRounds = ⌈64 / reconcileStartStride⌉ = 16:截断持续时,每片至多隔这么多轮被读完一次。
	// 对账窗口(ReconcileWindow)不足这么多个间隔时,截断期间有的分片可能整个窗口都轮不到(reconcileRound 打 ERROR)。
	reconcileFullCycleRounds = (ShardCount + reconcileStartStride - 1) / reconcileStartStride
)

// 编译期断言:步长至少为 1(单片上限不得超过玩家数上限,否则「前 stride 片必读完」的下界不成立)。
const _ uint = reconcileStartStride - 1

// reconcileLimits 是一轮定时对账的工作量上限;生产取 defaultReconcileLimits,单测注入小值。
type reconcileLimits struct {
	pageSize        int
	perShard        int
	maxPlayers      int
	readConcurrency int
}

var defaultReconcileLimits = reconcileLimits{
	pageSize:        reconcilePageSize,
	perShard:        reconcilePerShardLimit,
	maxPlayers:      reconcileMaxPlayers,
	readConcurrency: reconcileLedgerReadConcurrency,
}

// ReconcileRoundResult 是一轮定时对账的结果,供后台循环记 gauge 与轮次计数、打汇总日志。
type ReconcileRoundResult struct {
	// Inconsistent 是本轮发现的不一致行数。它是「当前」读数:同一处不一致只要还在窗口里,每轮都会再报一次,
	// 所以只能进 gauge,不能累加进计数器(评审 L11)。截断的轮次只覆盖一部分分片,gauge 按 Shards 逐片记(reconcileTally)。
	Inconsistent int
	// PlayersPlanned 是本轮收集到、要读账本的玩家数;PlayersChecked 是实际判定完的(含读失败 / 读不到 / 账本不合法)。
	PlayersPlanned int
	PlayersChecked int
	// Truncated 为真:本轮没有核对完窗口内的全部行(单片读满上限,或玩家数到上限)。
	Truncated bool
	// Shards 是本轮读过的每一片的读数,按扫描顺序;没读到的分片不在里面。收集分片时就失败的轮次为空。
	Shards []ShardTally
}

// ShardTally 是一轮对账在一个分片上的读数(评审 R1)。一名玩家的行全在 RowShard(player_id) 那一片,
// 所以每名玩家的判定只归属一片。
type ShardTally struct {
	Shard uint32
	// Inconsistent 是本轮在这一片发现的不一致行数。
	Inconsistent int
	// Complete 为真:这一片窗口内的行本轮全部核对过 —— 没被单片上限截掉、不是玩家数触顶时停下的那一片、
	// 每名玩家都有确定的判定(读不到存档 / 账本不合法也算确定:它们不会因为下一轮重读而变)。
	// 为假(有截断、有账本读失败、轮次中途结束)时 Inconsistent 只是下界,0 不能证明这一片已经一致。
	Complete bool
}

// ReconcileShards 是定时对账作业的一轮(设计 §9.3)。每个 stable 副本只对账自己负责的分片(归属见
// internal/shardowner),调用方把这些分片按本轮的起点轮转好,作为 order 传进来:
//
//  1. 按 order 的顺序扫描这些分片(下一轮的起点由调用方按墙钟轮转,见 reconcileStartStride),逐片按键集翻页
//     读 sinceMs 之后由消费方终结的行,直到扫完或触到上限。触到上限计 player_inbox_reconcile_truncated_total
//     并打 ERROR,写明停在哪片、剩几片没扫到。
//  2. 按玩家以有界并发(reconcileLedgerReadConcurrency 路)读已落盘账本。
//  3. 逐个玩家按 ledgerMismatches 判定(与 ReconcilePlayer 同一个谓词,比水位也比环),每处不一致写一行
//     带 player_id 的 ERROR(player_id 只进日志);同时按分片记读数(ReconcileRoundResult.Shards)。
//
// order 里的分片必须在 [0, 64) 内且不重复,否则直接返回错误、一行不读。
// 返回本轮结果;ctx 结束(超时 / 关停)时返回已判定部分与 ctx.Err()。不改任何状态。
func (s *Store) ReconcileShards(ctx context.Context, sinceMs uint64, order []uint32, ledgers LedgerReader) (ReconcileRoundResult, error) {
	if ledgers == nil {
		return ReconcileRoundResult{}, fmt.Errorf("inbox: ReconcileShards has no ledger reader")
	}
	read := func(ctx context.Context, shard uint32, before *FinalizedRef, limit int) ([]FinalizedRef, error) {
		return s.RecentlyFinalized(ctx, shard, sinceMs, before, limit)
	}
	return reconcileShards(ctx, order, defaultReconcileLimits, read, ledgers)
}

// ringOrder 是从 start 起环形排满 64 片的扫描顺序((start+i) % 64)。本副本负责全部分片时就是这个顺序。
func ringOrder(start uint32) []uint32 {
	order := make([]uint32, ShardCount)
	for i := range order {
		order[i] = (start + uint32(i)) % ShardCount
	}
	return order
}

// checkShardOrder 要求 order 里的分片都在 [0, 64) 内且不重复:越界会读错分片,重复会把同一片读两遍、读数记两次。
func checkShardOrder(order []uint32) error {
	var seen [ShardCount]bool
	for _, shard := range order {
		if shard >= ShardCount {
			return fmt.Errorf("inbox: reconcile shard %d out of range [0, %d)", shard, ShardCount)
		}
		if seen[shard] {
			return fmt.Errorf("inbox: reconcile shard %d listed twice", shard)
		}
		seen[shard] = true
	}
	return nil
}

// reconcileShards 是 ReconcileShards 去掉库之后的部分:分片读取经 read 注入,单测用内存假实现。
func reconcileShards(ctx context.Context, order []uint32, limits reconcileLimits, read finalizedPageReader,
	ledgers LedgerReader) (ReconcileRoundResult, error) {
	log := logx.WithContext(ctx)
	if err := checkShardOrder(order); err != nil {
		return ReconcileRoundResult{}, err
	}
	scan, err := collectFinalized(ctx, order, limits, read)
	res := ReconcileRoundResult{PlayersPlanned: len(scan.players)}
	if err != nil {
		return res, err
	}
	if scan.truncated() {
		res.Truncated = true
		metrics.IncReconcileTruncated()
		// 覆盖保证(评审 R1):每轮至少读完从起点起的 reconcileStartStride 片,起点每轮在本副本负责的分片里前移这么多片,
		// 所以截断持续时每片至多隔 reconcileFullCycleRounds 轮读完一次;被单片上限截掉的是一片里最老的行,
		// 只要这一片持续有更新的行顶着,它们就一直核对不到。
		log.Errorf("[player_inbox] Reconcile round truncated (start_shard=%d, scanned %d/%d owned shards): %s; "+
			"the start shard advances %d shards per interval, so while this persists each shard is fully read at least once "+
			"every %d rounds (shards not reached keep their previous gauge readings until then), but rows cut by the "+
			"per-shard limit (oldest first) stay unchecked while newer rows keep filling that shard; "+
			"shorten ReconcileWindowHours, raise the limits, or add replicas (each replica reconciles only its own shards)",
			order[0], scan.scanned, len(order), scan.truncationSummary(limits), reconcileStartStride, reconcileFullCycleRounds)
	}

	reads := readLedgersBounded(ctx, ledgers, scan.players, limits.readConcurrency)
	shards, shardIndex := scan.shardTallies()
	for i, playerID := range scan.players {
		r := reads[i]
		tally := &shards[shardIndex[scan.shardOf[playerID]]]
		if ctx.Err() != nil && (!r.done || r.err != nil) {
			tally.Complete = false
			continue // 轮次已结束(超时 / 易主 / 关停):没读或读到一半被打断的不算读失败,留给之后的轮次
		}
		res.PlayersChecked++
		switch {
		case !r.done:
			tally.Complete = false
			metrics.ObserveReconcileRead("error")
			log.Errorf("[player_inbox] Reconcile ledger read did not complete player=%d (reader panicked, see %s)",
				playerID, safego.EventGoroutinePanic)
		case r.err != nil:
			tally.Complete = false
			metrics.ObserveReconcileRead("error")
			log.Errorf("[player_inbox] Reconcile could not read persisted ledger player=%d: %v", playerID, r.err)
		case !r.found:
			metrics.ObserveReconcileRead("absent")
		case !validLedger(r.ledger):
			metrics.ObserveReconcileRead("invalid")
			log.Errorf("[player_inbox] Reconcile skipped player=%d: persisted inbox ledger is structurally invalid "+
				"(duplicate/zero domain, ring not ascending or beyond the watermark, unrecordable outcome, zero request_id, "+
				"or more than %d ring entries); result is unknown, not 'no difference'", playerID, ledgerRecentCapacity)
		default:
			metrics.ObserveReconcileRead("found")
			for _, m := range ledgerMismatches(scan.byPlayer[playerID], r.ledger) {
				res.Inconsistent++
				tally.Inconsistent++
				cursor := findCursor(r.ledger, uint32(m.ref.Domain))
				log.Errorf("[player_inbox] Reconcile mismatch player=%d domain=%d seq=%d state=%d request_id=%d gap=%s "+
					"ledger_decided_through=%d ring_len=%d: inbox finalized the item but the persisted ledger does not hold "+
					"its outcome (rollback or Redis data loss); manual re-issue may be needed",
					playerID, m.ref.Domain, m.ref.Seq, m.ref.State, m.ref.RequestID, m.gap,
					cursor.GetDecidedThroughSeq(), len(cursor.GetRecent()))
			}
		}
	}
	res.Shards = shards
	return res, ctx.Err()
}

// finalizedPageReader 读一个分片里排在 before 之后(更旧)的一页消费方终结行;before 为 nil 从最新的一行读起。
// 生产为 Store.RecentlyFinalized(窗口起点由闭包带入)。
type finalizedPageReader func(ctx context.Context, shard uint32, before *FinalizedRef, limit int) ([]FinalizedRef, error)

// finalizedScan 是一轮对账收集到的待核对行与分片覆盖情况。
type finalizedScan struct {
	byPlayer map[uint64][]FinalizedRef
	// players 是玩家首次出现的顺序:读账本、判定与日志都按它走,结果确定(不随 map 遍历变)。
	players []uint64
	// shardOf 是每名玩家的行是从哪一片读出来的(按读出时的分片记,不按 player_id 重算)。
	shardOf map[uint64]uint32
	// order 是本轮的扫描顺序;读过的是它的前 scanned 片。
	order []uint32
	// scanned 是读过的分片数(含玩家数到上限时只读了一部分的那一片)。
	scanned int
	// capped 是读满单片上限、仍有剩余的分片,按扫描顺序。
	capped []uint32
	// playerCapHit 为真:玩家数到上限,停在 stoppedShard(该片只读了一部分)。
	playerCapHit bool
	stoppedShard uint32
}

func (s finalizedScan) truncated() bool { return len(s.capped) > 0 || s.playerCapHit }

// unreached 是本轮该扫、却一行都没读到的分片数。
func (s finalizedScan) unreached() int { return len(s.order) - s.scanned }

// shardTallies 按扫描顺序给出本轮读过的每一片的初始读数(不一致数 0),以及分片 → 下标。
// 收集阶段就截断了的分片(读满单片上限仍有剩余、玩家数触顶时停下的那一片)从一开始就不 Complete;
// 其余先记 Complete,判定阶段再按各玩家的账本读结果下调(reconcileRecent)。
func (s finalizedScan) shardTallies() ([]ShardTally, map[uint32]int) {
	capped := make(map[uint32]bool, len(s.capped))
	for _, c := range s.capped {
		capped[c] = true
	}
	out := make([]ShardTally, 0, s.scanned)
	index := make(map[uint32]int, s.scanned)
	for _, shard := range s.order[:s.scanned] {
		stopped := s.playerCapHit && shard == s.stoppedShard
		index[shard] = len(out)
		out = append(out, ShardTally{Shard: shard, Complete: !capped[shard] && !stopped})
	}
	return out, index
}

// truncationSummary 是截断日志里「停在哪、剩多少」的那一段。
func (s finalizedScan) truncationSummary(limits reconcileLimits) string {
	var parts []string
	if len(s.capped) > 0 {
		parts = append(parts, fmt.Sprintf("%d shard(s) %v hit the per-shard limit of %d rows (newest checked, oldest skipped)",
			len(s.capped), s.capped, limits.perShard))
	}
	if s.playerCapHit {
		parts = append(parts, fmt.Sprintf("stopped at shard=%d after reaching %d players, %d shard(s) not reached",
			s.stoppedShard, limits.maxPlayers, s.unreached()))
	}
	return strings.Join(parts, "; ")
}

// collectFinalized 按 order 的顺序扫描分片,收集待核对的行(评审 M5):
//   - 每片按键集翻页,一页至多 limits.pageSize 行;一片至多 limits.perShard 行,读满后探一行确认还有剩余才算截断
//     (恰好读满不算),然后继续下一片;
//   - 新出现的玩家超过 limits.maxPlayers 时整轮停在当前分片(已收进来的玩家照常核对)。
//
// 覆盖下界(评审 R1):一片至多收 limits.perShard 名新玩家,所以 order 的前 limits.maxPlayers / limits.perShard 片
// 一定读完 —— 玩家数最早也只能在下一片触顶。生产取默认上限时这就是 reconcileStartStride。
//
// 读失败或 ctx 结束返回 error,此时收集结果不完整,调用方不得拿去判定「无差异」。
func collectFinalized(ctx context.Context, order []uint32, limits reconcileLimits, read finalizedPageReader) (finalizedScan, error) {
	scan := finalizedScan{byPlayer: map[uint64][]FinalizedRef{}, shardOf: map[uint64]uint32{}, order: order}
	for _, shard := range order {
		if err := ctx.Err(); err != nil {
			return scan, err
		}
		scan.scanned++
		capHit, err := scan.collectShard(ctx, shard, limits, read)
		if err != nil {
			return scan, err
		}
		if capHit {
			scan.playerCapHit, scan.stoppedShard = true, shard
			break
		}
	}
	return scan, nil
}

// collectShard 读一个分片;返回 true = 玩家数到上限,整轮该停了。
func (s *finalizedScan) collectShard(ctx context.Context, shard uint32, limits reconcileLimits, read finalizedPageReader) (bool, error) {
	var before *FinalizedRef
	taken := 0
	for {
		limit := min(limits.pageSize, limits.perShard-taken)
		if limit <= 0 {
			more, err := read(ctx, shard, before, 1)
			if err != nil {
				return false, err
			}
			if len(more) > 0 {
				s.capped = append(s.capped, shard)
			}
			return false, nil
		}
		page, err := read(ctx, shard, before, limit)
		if err != nil {
			return false, err
		}
		for _, f := range page {
			if _, seen := s.byPlayer[f.PlayerID]; !seen {
				if len(s.players) >= limits.maxPlayers {
					return true, nil
				}
				s.players = append(s.players, f.PlayerID)
				s.shardOf[f.PlayerID] = shard
			}
			s.byPlayer[f.PlayerID] = append(s.byPlayer[f.PlayerID], f)
			taken++
		}
		if len(page) < limit {
			return false, nil // 本片读完了
		}
		last := page[len(page)-1]
		before = &last
	}
}

// ledgerRead 是一名玩家的账本读结果。done=false:没有读完(轮次已结束、没有发起,或读的 goroutine panic 了)。
type ledgerRead struct {
	ledger *componentpb.PlayerInboxLedgerComp
	found  bool
	err    error
	done   bool
}

// readLedgersBounded 以至多 concurrency 路并发读 players 的已落盘账本,结果按 players 的下标放回。
// 每路用原子计数领下标、只写自己领到的那一格,不共享别的可变状态;wg.Wait 之后调用方才读结果。
// ctx 结束后不再发起新的读;每次读各带 LedgerReadTimeout。单次读 panic 由 safego.Run 兜住(记 goroutine_panic),
// 只丢那一名玩家(下标留 done=false),这一路接着领下一个。
func readLedgersBounded(ctx context.Context, ledgers LedgerReader, players []uint64, concurrency int) []ledgerRead {
	const point = "player_inbox.reconcile_ledger_read"
	out := make([]ledgerRead, len(players))
	readOne := func(i int) {
		readCtx, cancel := context.WithTimeout(ctx, constants.LedgerReadTimeout)
		defer cancel()
		ledger, found, err := ledgers.ReadInboxLedger(readCtx, players[i])
		out[i] = ledgerRead{ledger: ledger, found: found, err: err, done: true}
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < min(max(concurrency, 1), len(players)); w++ {
		wg.Add(1)
		safego.Go(point, func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(players) || ctx.Err() != nil {
					return
				}
				safego.Run(point, func() { readOne(i) })
			}
		})
	}
	wg.Wait()
	return out
}
