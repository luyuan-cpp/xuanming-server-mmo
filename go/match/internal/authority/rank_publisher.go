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
	rankpb "proto/rank"
	"shared/serviceauth"
)

// RunRankPublisher 只从已提交 outbox 按片顺发；RPC超时不推进本地游标，重试同业务ID/载荷仅更新认证时间。
func RunRankPublisher(ctx context.Context, s *Store, endpoint, secret string, admissions ...*drainproof.Session) {
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
	conn, e := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if e != nil {
		logx.Error("评级投榜连接初始化失败")
		return
	}
	defer func() {
		if err := conn.Close(); err != nil && relay != nil {
			relay.Hold()
		}
	}()
	client := rankpb.NewRankAdminClient(conn)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		call, cancel := context.WithTimeout(context.WithoutCancel(ctx), rankPublishRoundTimeout)
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
			return s.PublishRankOnce(call, client, secret)
		}()
		cancel()
		if e != nil && ctx.Err() == nil {
			logx.Errorf("评级投榜等待恢复: %v", e)
		}
		select {
		case <-ctx.Done():
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
)

func rankPublishBudgetLeft(ctx context.Context) bool {
	deadline, limited := ctx.Deadline()
	return !limited || time.Until(deadline) > rankPublishBatchReserve
}

// rankShardProgress 是 match_rank_source 的一行:一个计分模式的一片。
type rankShardProgress struct {
	mode, shard                uint32
	produced, committed, board uint64
}

func (p rankShardProgress) backlog() uint64 {
	if p.committed >= p.produced {
		return 0
	}
	return p.produced - p.committed
}

// orderRankShardsByBacklog 让积压多的片先投(各模式的片放在一起比)。预算不够时没轮到的片积压继续涨、被投的片在减,
// 下一轮就轮到它们,高号片、冷门模式的片不会一直排在后面饿死;积压相同按读出的 (模式, 片号) 序。
func orderRankShardsByBacklog(shards []rankShardProgress) {
	sort.SliceStable(shards, func(i, j int) bool { return shards[i].backlog() > shards[j].backlog() })
}

func (s *Store) readRankShards(ctx context.Context) ([]rankShardProgress, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT mode,shard,produced_cursor,committed_cursor,board_id FROM match_rank_source WHERE source_epoch=1 ORDER BY mode,shard")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var shards []rankShardProgress
	for rows.Next() {
		var p rankShardProgress
		if e = rows.Scan(&p.mode, &p.shard, &p.produced, &p.committed, &p.board); e != nil {
			return nil, e
		}
		shards = append(shards, p)
	}
	return shards, rows.Err()
}

// PublishRankOnce 投一轮:每个计分模式一张榜(rankBoardTypes),每片按自己的游标顺序投到本模式的榜;
// 片之间、模式之间没有先后(版本防旧值覆盖,见 ApplyRating)。每个副本只投自己负责的 (模式, 来源片)
// (work_owner.go 的 rankWorkKey):加副本就分摊投榜,不再每个副本把全部片各投一遍;两个副本偶尔同投一片也安全,
// rank 按 (榜, 片, 游标) 幂等,本地游标只前进不后退。
// 一片失败、或一个模式的榜这一轮绑不上,都不拦别的片和别的模式,下一轮从原游标接着投;返回遇到的第一个错误。
func (s *Store) PublishRankOnce(ctx context.Context, client rankpb.RankAdminClient, secret string) error {
	shards, e := s.readRankShards(ctx)
	if e != nil {
		return e
	}
	var first error
	boards := map[uint32]uint64{}
	for _, p := range shards {
		if _, seen := boards[p.mode]; seen {
			continue
		}
		board, e := s.bindRankBoard(ctx, client, secret, p.mode, shards)
		if e != nil {
			if first == nil {
				first = e
			}
			board = 0 // 这个模式本轮不投;记 0 只是为了不再重复绑定
		}
		boards[p.mode] = board
	}
	orderRankShardsByBacklog(shards)
	for _, p := range shards {
		if !rankPublishBudgetLeft(ctx) {
			break
		}
		board := boards[p.mode]
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
	return first
}

// bindRankBoard 找出一个计分模式的榜(还没有就向 rank 打开),并给本模式里还没记榜号的片补上。
// 同一模式各片记录的榜必须一致;不同模式各是各的榜。打开请求的业务号取 board_type:每张榜一个固定请求,
// 重放(包括别的副本同时打开)得到同一个 board_id,rank 按榜键幂等。1V1 的请求与拆分前逐字节相同。
func (s *Store) bindRankBoard(ctx context.Context, client rankpb.RankAdminClient, secret string, mode uint32, shards []rankShardProgress) (uint64, error) {
	boardType, rated := RankBoardType(mode)
	if !rated {
		return 0, fmt.Errorf("评级来源里有不计分模式 %d 的行", mode)
	}
	var board uint64
	unbound := false
	for _, p := range shards {
		if p.mode != mode {
			continue
		}
		switch {
		case p.board == 0:
			unbound = true
		case board == 0:
			board = p.board
		case board != p.board:
			return 0, fmt.Errorf("模式 %d 的评级来源各片记录的榜不一致", mode)
		}
	}
	if board == 0 {
		request := &rankpb.OpenRankGenerationRequest{Board: &rankpb.RankBoardKey{BoardType: boardType, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: "all", Generation: 1}, Producer: "match", SourceEpoch: 1, RequestId: uint64(boardType)}
		if e := serviceauth.Sign(rankpb.RankAdmin_OpenRankGeneration_FullMethodName, request, "match", secret, uint64(time.Now().UnixMilli())); e != nil {
			return 0, e
		}
		response, e := client.OpenRankGeneration(ctx, request)
		if e != nil {
			return 0, e
		}
		if response.GetErrorMessage().GetId() != 0 || response.BoardId == 0 {
			return 0, fmt.Errorf("模式 %d 的评级榜代次打开被拒", mode)
		}
		board = response.BoardId
	}
	// 新片的行是后来才建的,board_id 还是 0,这里补上。
	if unbound {
		if _, e := s.DB.ExecContext(ctx, "UPDATE match_rank_source SET board_id=? WHERE source_epoch=1 AND mode=? AND board_id=0", board, mode); e != nil {
			return 0, e
		}
	}
	return board, nil
}

func (s *Store) publishRankShard(ctx context.Context, client rankpb.RankAdminClient, secret string, board uint64, p rankShardProgress) error {
	committed := p.committed
	for sent := 0; committed < p.produced && sent < rankPublishPerShard && rankPublishBudgetLeft(ctx); sent++ {
		var body []byte
		if e := s.DB.QueryRowContext(ctx, "SELECT entries FROM match_rank_outbox WHERE source_epoch=1 AND mode=? AND shard=? AND source_cursor=?", p.mode, p.shard, committed+1).Scan(&body); e != nil {
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
		// 发布者没有选主,别的副本可能已经投得更远:榜回「重复」和更大的游标,这批的结局已定,不算故障,本地游标推到榜确认的位置。
		reached := response.CommittedCursor
		if response.GetErrorMessage().GetId() != 0 || reached < request.SourceCursor || (reached > request.SourceCursor && !response.Duplicated) {
			return errors.New("评级投榜未确认连续游标")
		}
		if _, e = s.DB.ExecContext(ctx, "UPDATE match_rank_source SET committed_cursor=? WHERE source_epoch=1 AND mode=? AND shard=? AND committed_cursor<?", reached, p.mode, p.shard, reached); e != nil {
			return e
		}
		committed = reached
	}
	return nil
}
