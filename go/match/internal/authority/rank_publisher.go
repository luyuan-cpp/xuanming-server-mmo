package authority

import (
	"context"
	"database/sql"
	"errors"
	"shared/drainproof"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	rankpb "proto/rank"
	"shared/serviceauth"
)

// RunRankPublisher 只从已提交 outbox 顺发；RPC超时不推进本地游标，重试同业务ID/载荷仅更新认证时间。
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
		call, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
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
					if resultErr != nil {
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
func (s *Store) PublishRankOnce(ctx context.Context, client rankpb.RankAdminClient, secret string) error {
	var produced, committed, board uint64
	e := s.DB.QueryRowContext(ctx, "SELECT produced_cursor,committed_cursor,board_id FROM match_rank_source WHERE source_epoch=1").Scan(&produced, &committed, &board)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if board == 0 {
		request := &rankpb.OpenRankGenerationRequest{Board: &rankpb.RankBoardKey{BoardType: 1, Scope: rankpb.RankScope_RANK_SCOPE_GLOBAL, Period: "all", Generation: 1}, Producer: "match", SourceEpoch: 1, RequestId: 1}
		if e = serviceauth.Sign(rankpb.RankAdmin_OpenRankGeneration_FullMethodName, request, "match", secret, uint64(time.Now().UnixMilli())); e != nil {
			return e
		}
		response, e := client.OpenRankGeneration(ctx, request)
		if e != nil {
			return e
		}
		if response.GetErrorMessage().GetId() != 0 || response.BoardId == 0 {
			return errors.New("评级榜代次打开被拒")
		}
		board = response.BoardId
		if _, e = s.DB.ExecContext(ctx, "UPDATE match_rank_source SET board_id=? WHERE source_epoch=1 AND board_id=0", board); e != nil {
			return e
		}
	}
	for sent := 0; committed < produced && sent < 16; sent++ {
		var body []byte
		if e = s.DB.QueryRowContext(ctx, "SELECT entries FROM match_rank_outbox WHERE source_epoch=1 AND source_cursor=?", committed+1).Scan(&body); e != nil {
			return e
		}
		request := &rankpb.SubmitRankScoresRequest{}
		if e = proto.Unmarshal(body, request); e != nil {
			return e
		}
		request.BoardId = board
		if e = serviceauth.Sign(rankpb.RankAdmin_SubmitRankScores_FullMethodName, request, "match", secret, uint64(time.Now().UnixMilli())); e != nil {
			return e
		}
		response, e := client.SubmitRankScores(ctx, request)
		if e != nil {
			return e
		}
		if response.GetErrorMessage().GetId() != 0 || response.CommittedCursor != request.SourceCursor {
			return errors.New("评级投榜未确认连续游标")
		}
		if _, e = s.DB.ExecContext(ctx, "UPDATE match_rank_source SET committed_cursor=? WHERE source_epoch=1 AND committed_cursor=?", request.SourceCursor, committed); e != nil {
			return e
		}
		committed = request.SourceCursor
	}
	return nil
}
