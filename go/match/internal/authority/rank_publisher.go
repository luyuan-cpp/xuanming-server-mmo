package authority

import (
	"context"
	"errors"
	"fmt"
	"shared/authoritysql"
	"shared/drainproof"
	"sort"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"match/internal/metrics"
	pb "proto/match"
	rankpb "proto/rank"
	"shared/generated/pb/table"
	"shared/grpckeepalive"
	"shared/serviceauth"
	"shared/tracing"
)

// rankPublishPoint 是投榜每一轮的点位名:safego_panic_total 与 match_loop_last_run_unixtime 的 point label,必须是常量。
const rankPublishPoint = "match.rank_publish"

// RunRankPublisher 只从已提交 outbox 按片顺发；RPC超时不推进本地游标，重试同业务ID/载荷仅更新认证时间。
//
// 每一轮各自 recover(2026-11-12):一轮 panic 打 HOLD(结局未知),只丢这一轮,循环接着跑。原来 panic 一路抛出去把进程打死:
// 投榜按 (模式, 来源片) 分给各副本,一条解释不了的行会先打死负责它那一片的副本,片换主之后再打死下一个。
// 兜住的 panic 计 safego_panic_total{point},告警 GoBackgroundPanicRecovered 按点位报(deploy/k8s/go-background-alerts.yaml)。
// 游标在库里、只在确认之后推进,丢一轮下一轮从同一位置重投。
//
// 活性(metrics.LoopLive 里 match.rank_publish 那一条;缺口清单第 25 条第六批,口径在 shared/looplive 的包注释):一进来就写一次,
// 下面任何一处意料之外的 return(参数不对、建不了连接)都把序列停在这一刻,由停摆告警 MatchBackgroundLoopStalled 报出来;
// 每一轮正常返回(成败都算)就写这一刻,panic 的一轮不写;只有 ctx 结束才删序列。
func RunRankPublisher(ctx context.Context, s *Store, endpoint, secret string, admissions ...*drainproof.Session) {
	live := metrics.LoopLive.For(rankPublishPoint)
	live.Mark(time.Now())
	var relay *drainproof.Session
	if len(admissions) > 1 {
		return
	}
	if len(admissions) == 1 {
		relay = admissions[0]
		if relay == nil {
			return
		}
	}
	// 裸客户端不经 zrpc,traceparent 由 shared/tracing 的拦截器写(docs/design/distributed-tracing.md §6.2)。
	// K8s 上 endpoint 是 rank 的 Service 地址,整条连接落在一个 Pod 上:keepalive 让对端整机消失后的半开连接
	// 在一次调用后 20 秒内断开重连,而不是等内核重传约 15 分钟(shared/grpckeepalive)。
	conn, e := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(tracing.UnaryClientInterceptor()), grpckeepalive.DialOption())
	if e != nil {
		logx.Error("rating rank publisher connection initialization failed")
		return
	}
	defer func() {
		if err := conn.Close(); err != nil && relay != nil {
			relay.Hold()
		}
	}()
	client := rankpb.NewRankAdminClient(conn)
	rankReader := grpcRankBoardReader{client: rankpb.NewClientPlayerRankClient(conn)}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			live.Stop()
			return
		}
		call, cancel := context.WithTimeout(context.WithoutCancel(ctx), rankPublishRoundTimeout)
		e = nil
		live.Round(rankPublishPoint, func() {
			e = func() (resultErr error) {
				if relay != nil {
					finish, err := relay.Enter()
					if err != nil {
						return err
					}
					defer finish()
					defer func() {
						if recovered := recover(); recovered != nil {
							relay.Hold()
							panic(recovered)
						}
						if resultErr != nil && !authoritysql.IsWriterFrozen(resultErr) {
							relay.Hold()
						}
					}()
				}
				if err := s.PublishRankOnce(call, client, secret); err != nil {
					return err
				}
				if s.ownsWork("ranked-rollover:0") {
					// 换季状态机给予独立 20 秒单步预算（不与 5 秒投榜预算争抢），并接入真实封榜核对与榜首查名。
					rolloverCall, rolloverCancel := context.WithTimeout(context.WithoutCancel(ctx), rankRolloverStepTimeout)
					defer rolloverCancel()
					return s.AdvanceRankedSeasonRollover(rolloverCall, client, rankReader, secret, s.rankedNameLookup)
				}
				return nil
			}()
		})
		cancel()
		if e != nil && ctx.Err() == nil {
			logx.Errorf("rating rank publishing awaiting recovery: %v", e)
		}
		select {
		case <-ctx.Done():
			live.Stop()
			return
		case <-ticker.C:
		}
	}
}

// 一轮的节奏。每片配额 4 批,64 片最多 256 批;积压时 5 秒投不完是常态(一批要几十条 SQL,榜上还按 board 行锁串行),
// 所以预算只剩一批的余量就收手,下一轮接着投。预算用完不是失败:回错误会让 relay.Hold 永久挡住这个进程的排空。
// 只有已经发出去的那一批超时才是结局未知。
const (
	rankPublishPerShard     = 4
	rankPublishRoundTimeout = 5 * time.Second
	rankPublishBatchReserve = time.Second
	rankRolloverStepTimeout = 20 * time.Second
)

type grpcRankBoardReader struct {
	client rankpb.ClientPlayerRankClient
}

func (r grpcRankBoardReader) GetRankPage(ctx context.Context, in *rankpb.GetRankPageRequest) (*rankpb.GetRankPageResponse, error) {
	return r.client.GetRankPage(ctx, in)
}

func rankPublishBudgetLeft(ctx context.Context) bool {
	deadline, limited := ctx.Deadline()
	return !limited || time.Until(deadline) > rankPublishBatchReserve
}

// rankShardProgress 是 match_rank_source 的一行:一个计分模式在某个来源世代(source_epoch)的一片。
type rankShardProgress struct {
	epoch                      uint64
	mode, shard                uint32
	produced, committed, board uint64
}

type rankBoardKey struct {
	epoch uint64
	mode  uint32
}

func (p rankShardProgress) backlog() uint64 {
	if p.committed >= p.produced {
		return 0
	}
	return p.produced - p.committed
}

// orderRankShardsByBacklog 让积压多的片先投(各模式的片放在一起比)。预算不够时没轮到的片积压继续涨、被投的片在减,
// 下一轮就轮到它们,高号片、冷门模式的片不会一直排在后面饿死;积压相同按读出的 (世代, 模式, 片号) 序。
func orderRankShardsByBacklog(shards []rankShardProgress) {
	sort.SliceStable(shards, func(i, j int) bool { return shards[i].backlog() > shards[j].backlog() })
}

func (s *Store) readRankShards(ctx context.Context) ([]rankShardProgress, error) {
	pvpTeam := uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)
	rows, e := s.DB.QueryContext(ctx,
		"SELECT source_epoch,mode,shard,produced_cursor,committed_cursor,board_id FROM match_rank_source WHERE (mode<>? AND source_epoch=1) OR (mode=? AND source_epoch>=1) ORDER BY source_epoch,mode,shard",
		pvpTeam, pvpTeam)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var shards []rankShardProgress
	for rows.Next() {
		var p rankShardProgress
		if e = rows.Scan(&p.epoch, &p.mode, &p.shard, &p.produced, &p.committed, &p.board); e != nil {
			return nil, e
		}
		shards = append(shards, p)
	}
	return shards, rows.Err()
}

// PublishRankOnce 投一轮:每个计分模式(PVP_TEAM 按赛季 epoch)一张榜,每片按自己的游标顺序投到对应榜。
func (s *Store) PublishRankOnce(ctx context.Context, client rankpb.RankAdminClient, secret string) error {
	shards, e := s.readRankShards(ctx)
	if e != nil {
		return e
	}
	var first error
	boards := map[rankBoardKey]uint64{}
	latestBoardByMode := map[uint32]rankBoardKey{}
	for _, p := range shards {
		key := rankBoardKey{epoch: p.epoch, mode: p.mode}
		if prev, ok := latestBoardByMode[p.mode]; !ok || p.epoch >= prev.epoch {
			latestBoardByMode[p.mode] = key
		}
		if _, seen := boards[key]; seen {
			continue
		}
		board, e := s.bindRankBoard(ctx, client, secret, p.epoch, p.mode, shards)
		if e != nil {
			if first == nil {
				first = e
			}
			board = 0 // 这个世代本轮不投;记 0 只是为了不再重复绑定
		}
		boards[key] = board
	}
	orderRankShardsByBacklog(shards)
	for _, p := range shards {
		if !rankPublishBudgetLeft(ctx) {
			break
		}
		board := boards[rankBoardKey{epoch: p.epoch, mode: p.mode}]
		if board == 0 || !s.ownsWork(rankWorkKey(p.mode, p.shard)) {
			continue
		}
		if e = s.publishRankShard(ctx, client, secret, board, p); e != nil {
			if first == nil {
				first = e
			}
			if ctx.Err() != nil {
				break // 这一轮的预算用完了,后面的片也发不出去
			}
		}
	}
	activeBoards := make(map[uint32]uint64, len(latestBoardByMode))
	activeGen := make(map[uint32]uint64, len(latestBoardByMode))
	for mode, key := range latestBoardByMode {
		activeBoards[mode] = boards[key]
		activeGen[mode] = key.epoch
	}
	if e = s.publishRankRemovals(ctx, client, secret, activeBoards, activeGen); e != nil && first == nil {
		first = e
	}
	return first
}

// rankRemovalPage 是一轮最多处理的待摘榜行数。
const rankRemovalPage = 16

func (s *Store) publishRankRemovals(ctx context.Context, client rankpb.RankAdminClient, secret string, boards, generations map[uint32]uint64) error {
	rows, e := s.DB.QueryContext(ctx, "SELECT mode,player_id FROM match_rank_removal ORDER BY mode,player_id LIMIT ?", rankRemovalPage)
	if e != nil {
		return e
	}
	type removal struct {
		mode   uint32
		player uint64
	}
	var pending []removal
	for rows.Next() {
		var r removal
		if e = rows.Scan(&r.mode, &r.player); e != nil {
			rows.Close()
			return e
		}
		pending = append(pending, r)
	}
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return e
	}
	var first error
	for _, r := range pending {
		if !rankPublishBudgetLeft(ctx) {
			break
		}
		board := boards[r.mode]
		if board == 0 {
			continue
		}
		gen := generations[r.mode]
		if gen == 0 {
			gen = 1
		}
		if e = s.removeFromRank(ctx, client, secret, board, gen, r.mode, r.player); e != nil && first == nil {
			first = e
		}
	}
	return first
}

func (s *Store) removeFromRank(ctx context.Context, client rankpb.RankAdminClient, secret string, board, generation uint64, mode uint32, player uint64) error {
	request := &rankpb.RemoveRankEntryRequest{BoardId: board, Generation: generation, EntityId: player, RequestId: player}
	if e := serviceauth.Sign(rankpb.RankAdmin_RemoveRankEntry_FullMethodName, request, "match", secret, uint64(time.Now().UnixMilli())); e != nil {
		return e
	}
	response, e := client.RemoveRankEntry(ctx, request)
	if e != nil {
		return e
	}
	switch tip := response.GetErrorMessage().GetId(); tip {
	case 0, uint32(table.RankError_kRankGenerationClosed), uint32(table.RankError_kRankBoardNotFound):
	default:
		return fmt.Errorf("rank refused to remove a deleted player from the board of mode %d: tip=%d", mode, tip)
	}
	_, e = s.DB.ExecContext(ctx, "DELETE FROM match_rank_removal WHERE mode=? AND player_id=?", mode, player)
	return e
}

// bindRankBoard 找出一个计分模式在指定 epoch 下的榜(还没有就向 rank 打开),并补齐 board_id。
// 1V1 / 5V5 保持 period="all", generation=1, request_id=boardType 与旧契约逐字节一致;
// PVP_TEAM 按赛季表解析 period=season_key, generation=rank_generation(若无赛季行回退 period="all", generation=1)。
func (s *Store) bindRankBoard(ctx context.Context, client rankpb.RankAdminClient, secret string, epoch uint64, mode uint32, shards []rankShardProgress) (uint64, error) {
	boardType, rated := RankBoardType(mode)
	if !rated {
		return 0, fmt.Errorf("评级来源里有不计分模式 %d 的行", mode)
	}
	var board uint64
	unbound := false
	for _, p := range shards {
		if p.mode != mode || p.epoch != epoch {
			continue
		}
		switch {
		case p.board == 0:
			unbound = true
		case board == 0:
			board = p.board
		case board != p.board:
			return 0, fmt.Errorf("模式 %d 世代 %d 的评级来源各片记录的榜不一致", mode, epoch)
		}
	}
	period := "all"
	generation := uint64(1)
	requestID := uint64(boardType)
	isSeasonBoard := false
	if mode == uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM) {
		var seasonKey string
		var rankGen, seasonBoard uint64
		if err := s.DB.QueryRowContext(ctx, "SELECT season_key,rank_generation,rank_board_id FROM match_ranked_season WHERE source_epoch=? LIMIT 1", epoch).Scan(&seasonKey, &rankGen, &seasonBoard); err == nil && seasonKey != "" {
			period = seasonKey
			generation = rankGen
			if generation == 0 {
				generation = epoch
			}
			requestID = uint64(boardType)*100000 + epoch
			isSeasonBoard = true
			if board == 0 && seasonBoard != 0 {
				board = seasonBoard
			}
		}
	}
	if board == 0 {
		request := &rankpb.OpenRankGenerationRequest{
			Board:       &rankpb.RankBoardKey{BoardType: boardType, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: period, Generation: generation},
			Producer:    "match",
			SourceEpoch: epoch,
			RequestId:   requestID,
		}
		if e := serviceauth.Sign(rankpb.RankAdmin_OpenRankGeneration_FullMethodName, request, "match", secret, uint64(time.Now().UnixMilli())); e != nil {
			return 0, e
		}
		response, e := client.OpenRankGeneration(ctx, request)
		if e != nil {
			return 0, e
		}
		if response.GetErrorMessage().GetId() != 0 || response.BoardId == 0 {
			return 0, fmt.Errorf("模式 %d 世代 %d 的评级榜代次打开被拒", mode, epoch)
		}
		board = response.BoardId
	}
	if unbound {
		if _, e := s.DB.ExecContext(ctx, "UPDATE match_rank_source SET board_id=? WHERE source_epoch=? AND mode=? AND board_id=0", board, epoch, mode); e != nil {
			return 0, e
		}
	}
	if isSeasonBoard {
		_, _ = s.DB.ExecContext(ctx, "UPDATE match_ranked_season SET rank_board_id=? WHERE source_epoch=? AND rank_board_id=0", board, epoch)
	}
	return board, nil
}

func (s *Store) publishRankShard(ctx context.Context, client rankpb.RankAdminClient, secret string, board uint64, p rankShardProgress) error {
	committed := p.committed
	for sent := 0; committed < p.produced && sent < rankPublishPerShard && rankPublishBudgetLeft(ctx); sent++ {
		var body []byte
		if e := s.DB.QueryRowContext(ctx, "SELECT entries FROM match_rank_outbox WHERE source_epoch=? AND mode=? AND shard=? AND source_cursor=?", p.epoch, p.mode, p.shard, committed+1).Scan(&body); e != nil {
			return e
		}
		request := &rankpb.SubmitRankScoresRequest{}
		if e := proto.Unmarshal(body, request); e != nil {
			return e
		}
		request.BoardId = board
		if e := serviceauth.Sign(rankpb.RankAdmin_SubmitRankScores_FullMethodName, request, "match", secret, uint64(time.Now().UnixMilli())); e != nil {
			return e
		}
		response, e := client.SubmitRankScores(ctx, request)
		if e != nil {
			return e
		}
		reached := response.CommittedCursor
		if response.GetErrorMessage().GetId() != 0 || reached < request.SourceCursor || (reached > request.SourceCursor && !response.Duplicated) {
			return errors.New("评级投榜未确认连续游标")
		}
		if _, e = s.DB.ExecContext(ctx, "UPDATE match_rank_source SET committed_cursor=? WHERE source_epoch=? AND mode=? AND shard=? AND committed_cursor<?", reached, p.epoch, p.mode, p.shard, reached); e != nil {
			return e
		}
		committed = reached
	}
	return nil
}
