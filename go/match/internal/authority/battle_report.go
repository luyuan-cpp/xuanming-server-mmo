package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"match/internal/metrics"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
)

// 战报回放的存取(docs/design/battle-report-replay.md §7)。
//
// 正文(match_battle_report)和每名玩家的列表(match_player_battle_report)只在终局事务里写:只有 COMPLETE 的局才有
// 战报,而且和战果同一个事务提交。回放外壳不合格只丢回放、不挡终局(BR-7);SQL 错误和事务里其他行一样,整笔回滚,
// battle 按原字节重放。每名玩家只留最近 BattleReportsKeptPerPlayer 条:同一事务把超出的最旧行删掉,正文的
// ref_count 随之减一,减到 0 删正文。读路径只读、不加锁。

// BattleReportsKeptPerPlayer 是每名玩家保留的 PVP 战报条数(负责人 2026-11-06 定)。Java 版同值。
const BattleReportsKeptPerPlayer = 100

// 回放外壳的版本与上限,与 battle 录制侧(cpp/libs/services/battle/system/battle_replay_recorder.h)同值(BR-5)。
const (
	battleReplayFormatVersion   = 1
	maxBattleReplayRawBytes     = 1 << 20
	maxBattleReplayPayloadBytes = 512 << 10
)

// battleReportEvictBatch 是一次终局事务里一名玩家最多挤出的旧行数。每局都挤,正常只超出一行;万一之前积下了
// (比如保留条数调小过),剩下的留给这名玩家的下一局,终局事务的工作量始终有界。
const battleReportEvictBatch = 16

// 回放被丢弃的原因,也是指标 label 的封闭集合。
const (
	BattleReportDropInvalidEnvelope = "invalid_envelope"
	BattleReportDropTooLarge        = "too_large"
	BattleReportDropNotRequested    = "not_requested"
)

// ErrBattleReportNotFound:本人列表里没有这一局,或这一局没存回放正文。两种情况对调用方相同(BR-9),用 errors.Is 判断。
var ErrBattleReportNotFound = errors.New("battle report not in the player's list or has no replay body")

// battleReportWrite 是终局事务里战报写入的结果,提交成功之后才记指标。
type battleReportWrite struct {
	stored  bool // 写了正文
	evicted int  // 挤出的列表行数
}

func (w battleReportWrite) observe() {
	if w.stored {
		metrics.ObserveBattleReportStored()
	}
	metrics.ObserveBattleReportEvicted(w.evicted)
}

// battleReplayDropReason 只看外壳本身,不碰 SQL;合格回空串。nil 外壳由调用方当成「本局没录」,不算丢弃。
func battleReplayDropReason(env *battlepb.BattleReplayEnvelope) string {
	switch {
	case env.GetFormatVersion() != battleReplayFormatVersion,
		env.GetCodec() != battlepb.EBattleReplayCodec_BATTLE_REPLAY_CODEC_GZIP,
		env.GetRawBytes() == 0, len(env.GetPayload()) == 0:
		return BattleReportDropInvalidEnvelope
	case env.GetRawBytes() > maxBattleReplayRawBytes, len(env.GetPayload()) > maxBattleReplayPayloadBytes:
		return BattleReportDropTooLarge
	}
	return ""
}

func dropBattleReplay(battleID uint64, reason string) {
	logx.Errorf("metric=match_battle_report_dropped reason=%s battle_id=%d; outcome still commits, this battle has no replay", reason, battleID)
	metrics.ObserveBattleReportDropped(reason)
}

// battleReportSummary 拼列表条目:结局取终局原件,参战者的名字、等级、职业取建房原件里的开战快照。
// members 按 (team_index, player_id) 升序,同一局在每个人列表里的条目逐字节相同。
func battleReportSummary(original *battlepb.CreateBattleRequest, event *kafkapb.BattleResultEvent, hasReplay bool) *pb.BattleReportSummary {
	members := make([]*pb.BattleReportMember, 0, len(original.GetPlayers()))
	for _, p := range original.GetPlayers() {
		members = append(members, &pb.BattleReportMember{
			PlayerId: p.GetPlayerId(), TeamIndex: p.GetTeamIndex(), Name: p.GetPlayerName(), Level: p.GetLevel(), ClassId: p.GetClassId(),
		})
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].TeamIndex != members[j].TeamIndex {
			return members[i].TeamIndex < members[j].TeamIndex
		}
		return members[i].PlayerId < members[j].PlayerId
	})
	return &pb.BattleReportSummary{
		BattleId:       event.GetBattleId(),
		Mode:           pb.MatchMode(event.GetMatchMode()),
		BattleConfigId: event.GetBattleConfigId(),
		FinishedAtMs:   event.GetFinishedAtMs(),
		Outcome:        event.GetOutcome(),
		TotalRounds:    event.GetTotalRounds(),
		Members:        members,
		HasReplay:      hasReplay,
	}
}

// writeBattleReportTx 在终局事务里写战报,调用方必须已持成局行锁并通过 validateTerminalResult。只有建房原件
// record_replay 为真的局才写:回放合格时写正文,再给每名参战者写一条列表行,并把各自超出保留条数的最旧行挤出去。
// 返回的错误只来自 SQL 与序列化,调用方整笔回滚;回放外壳的毛病不返回错误(BR-7)。
func writeBattleReportTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, event *kafkapb.BattleResultEvent, replay *battlepb.BattleReplayEnvelope, stamp uint64) (battleReportWrite, error) {
	original := &battlepb.CreateBattleRequest{}
	if err := proto.Unmarshal(f.CreateRequest, original); err != nil {
		// validateTerminalResult 已经解码核对过同一份原件,这里失败说明别处有缺陷;不为战报挡终局。
		logx.Errorf("metric=match_battle_report_original_undecodable battle_id=%d: %v", f.BattleId, err)
		return battleReportWrite{}, nil
	}
	if !original.GetRecordReplay() {
		if replay != nil {
			// battle 只在原件让录时才带回放;带了说明两边版本或实现不一致。
			dropBattleReplay(f.BattleId, BattleReportDropNotRequested)
		}
		return battleReportWrite{}, nil
	}
	hasReplay := false
	if replay != nil {
		if reason := battleReplayDropReason(replay); reason != "" {
			dropBattleReplay(f.BattleId, reason)
		} else {
			hasReplay = true
		}
	}
	// 参战者 = 个人结算原件的全集(validateTerminalResult 已核过与建房成员一一对应);升序写,锁序稳定。
	players := make([]uint64, 0, len(event.GetSettlements()))
	for _, s := range event.GetSettlements() {
		players = append(players, s.GetPlayerId())
	}
	sort.Slice(players, func(i, j int) bool { return players[i] < players[j] })
	summary, err := proto.MarshalOptions{Deterministic: true}.Marshal(battleReportSummary(original, event, hasReplay))
	if err != nil {
		return battleReportWrite{}, err
	}
	write := battleReportWrite{}
	if hasReplay {
		envelope, err := proto.MarshalOptions{Deterministic: true}.Marshal(replay)
		if err != nil {
			return battleReportWrite{}, err
		}
		digest := sha256.Sum256(envelope)
		if _, err = tx.ExecContext(ctx, "INSERT INTO match_battle_report(battle_id,created_ms,match_mode,battle_config_id,envelope,envelope_digest,ref_count) VALUES(?,?,?,?,?,?,?)",
			f.BattleId, stamp, event.GetMatchMode(), event.GetBattleConfigId(), envelope, digest[:], uint32(len(players))); err != nil {
			return battleReportWrite{}, err
		}
		write.stored = true
	}
	released := map[uint64]uint32{}
	for _, player := range players {
		if _, err = tx.ExecContext(ctx, "INSERT INTO match_player_battle_report(player_id,created_ms,battle_id,summary) VALUES(?,?,?,?)", player, stamp, f.BattleId, summary); err != nil {
			return battleReportWrite{}, err
		}
		evicted, err := evictBattleReportsTx(ctx, tx, player, released)
		if err != nil {
			return battleReportWrite{}, err
		}
		write.evicted += evicted
	}
	if err = releaseBattleReportsTx(ctx, tx, released); err != nil {
		return battleReportWrite{}, err
	}
	return write, nil
}

// evictBattleReportsTx 删掉这名玩家超出保留条数的最旧列表行(最多 battleReportEvictBatch 行),把被删行对应的正文记进
// released(battle_id → 要减的引用数),返回删掉的行数。只按真正删掉的行计数:同一行若被两个事务同时看到,
// 后一个的 DELETE 等锁之后影响 0 行,引用数不会多减。
func evictBattleReportsTx(ctx context.Context, tx *sql.Tx, player uint64, released map[uint64]uint32) (int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT created_ms,battle_id FROM match_player_battle_report WHERE player_id=? ORDER BY created_ms DESC,battle_id DESC LIMIT ? OFFSET ?",
		player, battleReportEvictBatch, BattleReportsKeptPerPlayer)
	if err != nil {
		return 0, err
	}
	type listKey struct{ createdMs, battleID uint64 }
	var victims []listKey
	for rows.Next() {
		var k listKey
		if err = rows.Scan(&k.createdMs, &k.battleID); err != nil {
			rows.Close()
			return 0, err
		}
		victims = append(victims, k)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	evicted := 0
	for _, v := range victims {
		result, err := tx.ExecContext(ctx, "DELETE FROM match_player_battle_report WHERE player_id=? AND created_ms=? AND battle_id=?", player, v.createdMs, v.battleID)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if n == 1 {
			released[v.battleID]++
			evicted++
		}
	}
	return evicted, nil
}

// releaseBattleReportsTx 给被挤出的正文减引用数,减到 0 就删掉正文。按 battle_id 升序加锁:并发的终局事务都按同一
// 顺序锁正文行,不会互相死锁。正文不存在(那一局没存回放)时两条语句都影响 0 行。
func releaseBattleReportsTx(ctx context.Context, tx *sql.Tx, released map[uint64]uint32) error {
	ids := make([]uint64, 0, len(released))
	for id := range released {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		n := released[id]
		if _, err := tx.ExecContext(ctx, "UPDATE match_battle_report SET ref_count=IF(ref_count>?,ref_count-?,0) WHERE battle_id=?", n, n, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM match_battle_report WHERE battle_id=? AND ref_count=0", id); err != nil {
			return err
		}
	}
	return nil
}

// checkBattleReportReplay 在终局重放(库里已是 COMPLETE)时,核对请求里的回放与库里那份是不是同一份。只记录,
// 不改库、不改回执(BR-7)。库里没有正文就不比:第一次提交时回放可能被合法丢弃,或者正文已经随列表被挤出删掉。
func checkBattleReportReplay(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, req *pb.CommitBattleOutcomeRequest) {
	replay := req.GetReplay()
	if replay == nil || battleReplayDropReason(replay) != "" {
		return
	}
	var stored []byte
	err := q.QueryRowContext(ctx, "SELECT envelope_digest FROM match_battle_report WHERE battle_id=?", req.GetBattleId()).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		logx.Errorf("[battle_report] Replay check read failed battle_id=%d: %v", req.GetBattleId(), err)
		return
	}
	envelope, err := proto.MarshalOptions{Deterministic: true}.Marshal(replay)
	if err != nil {
		return
	}
	digest := sha256.Sum256(envelope)
	if !bytes.Equal(stored, digest[:]) {
		logx.Errorf("metric=match_battle_report_replay_mismatch battle_id=%d; stored replay kept, reply unchanged", req.GetBattleId())
		metrics.ObserveBattleReportReplayMismatch()
	}
}

// BattleReports 读本人的战报列表:新的在前,最多 BattleReportsKeptPerPlayer 条。
func (s *Store) BattleReports(ctx context.Context, playerID uint64) ([]*pb.BattleReportSummary, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("battle report store is not configured")
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT summary FROM match_player_battle_report WHERE player_id=? ORDER BY created_ms DESC,battle_id DESC LIMIT ?", playerID, BattleReportsKeptPerPlayer)
	if err != nil {
		return nil, err
	}
	var reports []*pb.BattleReportSummary
	for rows.Next() {
		var body []byte
		if err = rows.Scan(&body); err != nil {
			rows.Close()
			return nil, err
		}
		summary := &pb.BattleReportSummary{}
		if err = proto.Unmarshal(body, summary); err != nil {
			rows.Close()
			return nil, fmt.Errorf("corrupt battle report list row: %w", err)
		}
		reports = append(reports, summary)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return reports, nil
}

// BattleReport 取一局的回放外壳。先确认本人列表里有这一局,再读正文;列表里没有、或这一局没存正文,都回
// ErrBattleReportNotFound。先鉴权再读正文:不在列表里的人读不到大行。
func (s *Store) BattleReport(ctx context.Context, playerID, battleID uint64) (*battlepb.BattleReplayEnvelope, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("battle report store is not configured")
	}
	var listed int
	err := s.DB.QueryRowContext(ctx, "SELECT 1 FROM match_player_battle_report WHERE player_id=? AND battle_id=? LIMIT 1", playerID, battleID).Scan(&listed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBattleReportNotFound
	}
	if err != nil {
		return nil, err
	}
	var body []byte
	err = s.DB.QueryRowContext(ctx, "SELECT envelope FROM match_battle_report WHERE battle_id=?", battleID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBattleReportNotFound
	}
	if err != nil {
		return nil, err
	}
	envelope := &battlepb.BattleReplayEnvelope{}
	if err = proto.Unmarshal(body, envelope); err != nil {
		return nil, fmt.Errorf("corrupt battle report body: %w", err)
	}
	return envelope, nil
}
