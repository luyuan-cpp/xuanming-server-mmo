package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
	inboxpb "proto/player_inbox"
	"shared/serviceauth"
)

// 留出Kafka默认消息封装余量；两版生产者使用同一上限，消费者保持1MiB读取预算。
const maxCompletionBytes = 960 * 1024

// 只用于formation调度元数据，非业务时间戳；取有符号64位上界以保持双版SQL读者一致。
// COMPLETE已永久决定，不再参与AcquireWork/到期恢复；双outbox有各自独立的next_retry_ms。
const terminalOutcomeNoRetryMs uint64 = 1<<63 - 1

// Battle在deadline强制结束后仍需提交原终局；给重投与时钟偏差留固定60秒。
// 工作租约仍在原deadline停止续租，宽限只延后ABORT，不推迟COMPLETE。
const terminalOutcomeGraceMs uint64 = 60 * 1000

// 只标记原请求或永久原件冲突；SQL/传输错误及提交未知不能据错误文本推成永久拒绝。
var ErrBattleOutcomeOriginal = errors.New("战斗终局原件冲突")

func terminalAbortDue(deadline, stamp uint64) bool {
	return deadline != 0 && stamp >= deadline && stamp-deadline >= terminalOutcomeGraceMs
}

// ArchiveCompletion 只保全完整结算来源，不投递资产、不确认玩家到账，也不销账。
// 同一事务保存 Kafka 原字节及全部个人原件；重复局只接受完全相同的原字节。
// 原提交结果未知时按原消息重放，不得重算另一份结算。来源位点记录第一次成功归档的位置。
func (s *Store) ArchiveCompletion(ctx context.Context, event *kafkapb.BattleResultEvent, raw []byte, topic string, partition int, offset int64) error {
	if err := validateCompletion(event, raw); err != nil {
		return err
	}
	if event.SettlementFormatVersion == 0 {
		return nil
	}
	if topic == "" || partition < 0 || int64(partition) > int64(1<<31-1) || offset < 0 {
		return errors.New("完整结算来源缺少有效Kafka位点")
	}
	if s == nil || s.DB == nil {
		return errors.New("完整结算来源数据库未配置")
	}
	// 与既有SQL评级相同的单次8秒预算；消费者停机取消仍沿父ctx传播。
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	digest := sha256.Sum256(raw)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp, err := now(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO match_battle_completion(battle_id,raw_payload,payload_digest,format_version,archived_ms,source_topic,source_partition,source_offset,source_kind) VALUES(?,?,?,?,?,?,?,?,?)",
		event.BattleId, raw, digest[:], event.SettlementFormatVersion, stamp, topic, partition, offset, uint32(pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_KAFKA))
	if err != nil {
		var duplicate *mysql.MySQLError
		if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
			return err
		}
		var previous, previousDigest []byte
		var version uint32
		if err := tx.QueryRowContext(ctx, "SELECT raw_payload,payload_digest,format_version FROM match_battle_completion WHERE battle_id=?", event.BattleId).Scan(&previous, &previousDigest, &version); err != nil {
			return err
		}
		if version != event.SettlementFormatVersion || !bytes.Equal(previous, raw) || !bytes.Equal(previousDigest, digest[:]) {
			return errors.New("同对局完整结算来源原字节冲突")
		}
		// 新RPC终局的第一次真实Kafka观察单独落实际位点，不伪造其原始来源种类。
		if _, err = tx.ExecContext(ctx, "UPDATE match_battle_completion SET source_topic=?,source_partition=?,source_offset=? WHERE battle_id=? AND source_topic=''", topic, partition, offset, event.BattleId); err != nil {
			return err
		}
		return tx.Commit()
	}
	settlements := append([]*battlepb.BattleSettlementData(nil), event.Settlements...)
	sort.Slice(settlements, func(i, j int) bool { return settlements[i].PlayerId < settlements[j].PlayerId })
	for _, settlement := range settlements {
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(settlement)
		if err != nil {
			return err
		}
		personalDigest := sha256.Sum256(body)
		if _, err := tx.ExecContext(ctx, "INSERT INTO match_battle_settlement_source(battle_id,player_id,settlement_proto,payload_digest,archived_ms) VALUES(?,?,?,?,?)",
			event.BattleId, settlement.PlayerId, body, personalDigest[:], stamp); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("完整结算来源提交结果未知，必须按原消息重放: %w", err)
	}
	return nil
}

func validateCompletion(event *kafkapb.BattleResultEvent, raw []byte) error {
	if len(raw) > maxCompletionBytes {
		return errors.New("完整结算来源超过960KiB")
	}
	if event == nil {
		return errors.New("对局结果缺失")
	}
	if len(raw) == 0 {
		return errors.New("对局结果原字节缺失")
	}
	decoded := &kafkapb.BattleResultEvent{}
	if err := proto.Unmarshal(raw, decoded); err != nil || !proto.Equal(decoded, event) {
		return errors.New("结算来源原字节与解析结果不符")
	}
	switch event.SettlementFormatVersion {
	case 0:
		if len(event.Settlements) != 0 {
			return errors.New("旧版结果不能携带未声明完整性的个人结算")
		}
		return nil
	case 1:
	default:
		return errors.New("不支持的完整结算来源版本")
	}
	if event.BattleId == 0 || event.Outcome < battlepb.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN || event.Outcome > battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW {
		return errors.New("完整结算来源缺少有效对局或终局")
	}
	members := make(map[uint64]uint32)
	teams := make(map[uint32]bool)
	for _, team := range event.Teams {
		if team == nil || teams[team.TeamIndex] || len(team.PlayerIds) == 0 {
			return errors.New("完整结算来源队伍为空或重复")
		}
		teams[team.TeamIndex] = true
		for _, player := range team.PlayerIds {
			if _, exists := members[player]; player == 0 || exists {
				return errors.New("完整结算来源玩家为空或重复")
			}
			members[player] = team.TeamIndex
		}
	}
	if len(members) == 0 || len(event.Settlements) != len(members) {
		return errors.New("完整结算来源缺少全部参战玩家")
	}
	seen := make(map[uint64]bool, len(members))
	for _, settlement := range event.Settlements {
		if settlement == nil || settlement.PlayerId == 0 || seen[settlement.PlayerId] {
			return errors.New("个人结算缺失或玩家重复")
		}
		team, exists := members[settlement.PlayerId]
		if !exists || settlement.BattleId != event.BattleId || settlement.Outcome != event.Outcome || settlement.PlayerTeamIndex != team || settlement.TotalRounds != event.TotalRounds {
			return errors.New("个人结算与对局、终局、队伍或总回合数不符")
		}
		seen[settlement.PlayerId] = true
	}
	return nil
}

// 新终局只解释已声明的完整语义；旧Kafka归档仍原样保未知字段，不升级为执行资格。
func completionHasUnknown(m protoreflect.Message) bool {
	if len(m.GetUnknown()) != 0 {
		return true
	}
	found := false
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case f.IsMap() && f.MapValue().Message() != nil:
			v.Map().Range(func(_ protoreflect.MapKey, x protoreflect.Value) bool {
				found = completionHasUnknown(x.Message())
				return !found
			})
		case f.IsList() && f.Message() != nil:
			for i := 0; i < v.List().Len(); i++ {
				if completionHasUnknown(v.List().Get(i).Message()) {
					found = true
					break
				}
			}
		case f.Message() != nil && !f.IsMap() && !f.IsList():
			found = completionHasUnknown(v.Message())
		}
		return !found
	})
	return found
}

func supportedOutcomeOriginal(f *pb.MatchFormationRecord) (*battlepb.CreateBattleRequest, error) {
	if f == nil || f.OutcomeProtocolVersion != 1 || f.PreflightSupportedVersion != 1 || f.TargetInstanceId == "" || f.PreflightInstanceId != f.TargetInstanceId || (f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion != 1) || (!f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion != 0) {
		return nil, errors.New("旧局或原能力证据不完整，终局保持UNKNOWN")
	}
	r := &battlepb.CreateBattleRequest{}
	if proto.Unmarshal(f.CreateRequest, r) != nil || r.OutcomeProtocolVersion != 1 || r.BattleId != f.BattleId || r.PlacementOperationId != f.OperationId || r.TargetInstanceId != f.TargetInstanceId || r.BattleConfigId != f.ConfigId || r.MatchMode != f.Mode || r.DeadlineMs == 0 || r.DeadlineMs != f.DeadlineMs || r.CreatedAtMs < f.CreatedMs || r.DeadlineMs <= r.CreatedAtMs || len(r.Players) != len(f.Members) {
		return nil, errors.New("持久创建原件与终局身份冲突")
	}
	return r, nil
}

func validateTerminalPayload(event *kafkapb.BattleResultEvent, raw []byte) error {
	if err := validateCompletion(event, raw); err != nil {
		return err
	}
	if event.SettlementFormatVersion != 1 || completionHasUnknown(event.ProtoReflect()) {
		return errors.New("终局执行要求完整已知语义的版本1原件")
	}
	for _, s := range event.Settlements {
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(s)
		if err != nil {
			return err
		}
		if len(body) > 16384 {
			return errors.New("个人结算超过Inbox16KiB，整局不能提交COMPLETE")
		}
	}
	// Guild等全局消费者读取这两份索引，必须与同一原个人结果一致。
	for _, kind := range []struct {
		ids      []uint64
		selected func(*battlepb.BattleSettlementData) bool
	}{
		{event.FledPlayerIds, func(s *battlepb.BattleSettlementData) bool { return s.Fled }},
		{event.DeadPlayerIds, func(s *battlepb.BattleSettlementData) bool { return s.IsDead }},
	} {
		want := map[uint64]bool{}
		for _, s := range event.Settlements {
			if kind.selected(s) {
				want[s.PlayerId] = true
			}
		}
		seen := map[uint64]bool{}
		for _, id := range kind.ids {
			if !want[id] || seen[id] {
				return errors.New("终局逃跑或阵亡索引与原个人事实不符")
			}
			seen[id] = true
		}
		if len(seen) != len(want) {
			return errors.New("终局缺逃跑或阵亡原成员")
		}
	}
	return nil
}

func validateTerminalResult(f *pb.MatchFormationRecord, event *kafkapb.BattleResultEvent, raw []byte) error {
	// 锁前已做纯校验；提交时仍复核同一完整语义和当前持久创建/成员原件。
	if err := validateTerminalPayload(event, raw); err != nil {
		return err
	}
	r, err := supportedOutcomeOriginal(f)
	if err != nil {
		return err
	}
	if event.BattleId != r.BattleId || event.BattleConfigId != r.BattleConfigId || event.MatchMode != r.MatchMode || event.FinishedAtMs < r.CreatedAtMs || !proto.Equal(event.ActivityContext, r.ActivityContext) {
		return errors.New("终局配置、活动或原时间不符")
	}
	members := map[uint64]uint32{}
	for _, p := range r.Players {
		if p == nil || p.PlayerId == 0 {
			return errors.New("创建成员为空")
		}
		if _, exists := members[p.PlayerId]; exists {
			return errors.New("创建成员重复")
		}
		members[p.PlayerId] = p.TeamIndex
	}
	for _, m := range f.Members {
		if _, ok := members[m.PlayerId]; !ok {
			return errors.New("终局缺原成局成员")
		}
	}
	if len(event.Settlements) != len(members) {
		return errors.New("终局个人原件全集与持久创建成员不符")
	}
	for _, s := range event.Settlements {
		team, ok := members[s.PlayerId]
		if !ok || team != s.PlayerTeamIndex {
			return errors.New("终局成员或队伍与创建原件不符")
		}
	}
	return nil
}

// 原票的owner不能从当前目录补造。P首锁后再锁formation，COMPLETE与ABORT争同一行。
func (s *Store) terminalFormation(ctx context.Context, id uint64) (*sql.Tx, *pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord, error) {
	f, err := s.Formation(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	var originals []*pb.MatchAuthorityTicketRecord
	for _, m := range f.Members {
		t, err := scanTicket(s.DB.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=?", m.PlayerId))
		if err != nil {
			return nil, nil, nil, err
		}
		if t.Ticket != m.Ticket || t.FormationId != id || (t.State != 2 && t.State != 3) || t.OwnerAccount == "" || t.OwnerRevision == 0 {
			return nil, nil, nil, ErrOriginalPlayer
		}
		originals = append(originals, t)
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, nil, nil, err
	}
	ok := false
	defer func() {
		if !ok {
			tx.Rollback()
		}
	}()
	if err = s.lockOriginalPlayers(ctx, tx, originals); err != nil {
		return nil, nil, nil, err
	}
	locked, err := scanFormation(tx.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=? FOR UPDATE", id))
	if err != nil {
		return nil, nil, nil, err
	}
	if !proto.Equal(locked, f) {
		return nil, nil, nil, errors.New("终局原成局并发变化，需重放原请求")
	}
	for _, t := range originals {
		actual, err := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", t.PlayerId))
		if err != nil {
			return nil, nil, nil, err
		}
		if actual.Ticket != t.Ticket || actual.FormationId != id || actual.OwnerAccount != t.OwnerAccount || actual.OwnerRevision != t.OwnerRevision || (actual.State != 2 && actual.State != 3) {
			return nil, nil, nil, ErrOriginalPlayer
		}
	}
	ok = true
	return tx, locked, originals, nil
}

func readTerminal(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id uint64) (*pb.MatchBattleCompletionRecord, error) {
	r := &pb.MatchBattleCompletionRecord{}
	err := q.QueryRowContext(ctx, "SELECT battle_id,raw_payload,payload_digest,decision,placement_operation_id,target_instance_id,outcome_protocol_version FROM match_battle_completion WHERE battle_id=?", id).Scan(&r.BattleId, &r.RawPayload, &r.PayloadDigest, &r.Decision, &r.PlacementOperationId, &r.TargetInstanceId, &r.OutcomeProtocolVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func replayTerminal(r *pb.MatchBattleCompletionRecord, req *pb.CommitBattleOutcomeRequest) (pb.BattleOutcomeDecision, error) {
	if r == nil || r.Decision == pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, nil
	}
	if r.OutcomeProtocolVersion != 1 || r.PlacementOperationId != req.PlacementOperationId || r.TargetInstanceId != req.TargetInstanceId {
		return 0, fmt.Errorf("%w: 原终局身份冲突", ErrBattleOutcomeOriginal)
	}
	if r.Decision == pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE {
		d := sha256.Sum256(req.RawResultPayload)
		if !bytes.Equal(r.RawPayload, req.RawResultPayload) || !bytes.Equal(r.PayloadDigest, d[:]) {
			return 0, fmt.Errorf("%w: 同局COMPLETE原字节冲突", ErrBattleOutcomeOriginal)
		}
	} else if r.Decision != pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT {
		return 0, fmt.Errorf("%w: 非法持久终局", ErrBattleOutcomeOriginal)
	}
	return r.Decision, nil
}

// CommitBattleOutcome 在任何个人副作用前持久唯一决定和两个outbox。Commit未知绝不返回成功。
func (s *Store) CommitBattleOutcome(ctx context.Context, req *pb.CommitBattleOutcomeRequest) (pb.BattleOutcomeDecision, error) {
	if s == nil || s.DB == nil {
		return 0, errors.New("终局权威未装配")
	}
	if req == nil || req.BattleId == 0 || req.PlacementOperationId == "" || req.TargetInstanceId == "" || len(req.RawResultPayload) == 0 || len(req.RawResultPayload) > maxCompletionBytes || completionHasUnknown(req.ProtoReflect()) {
		return 0, fmt.Errorf("%w: 终局请求原件不完整", ErrBattleOutcomeOriginal)
	}
	event := &kafkapb.BattleResultEvent{}
	if err := proto.Unmarshal(req.RawResultPayload, event); err != nil {
		return 0, fmt.Errorf("%w: 终局原字节不能解码", ErrBattleOutcomeOriginal)
	}
	if err := validateTerminalPayload(event, req.RawResultPayload); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBattleOutcomeOriginal, err)
	}
	old, err := readTerminal(ctx, s.DB, req.BattleId)
	if err != nil {
		return 0, err
	}
	if old != nil && old.Decision != 0 {
		return replayTerminal(old, req)
	}
	tx, f, roles, err := s.terminalFormation(ctx, req.BattleId)
	if err != nil {
		if errors.Is(err, ErrOriginalPlayer) {
			return 0, fmt.Errorf("%w: 原成员或归属失效", ErrBattleOutcomeOriginal)
		}
		return 0, err
	}
	defer tx.Rollback()
	if f.OperationId != req.PlacementOperationId || f.TargetInstanceId != req.TargetInstanceId || f.State == 5 {
		return 0, fmt.Errorf("%w: 终局原操作、实例或成局状态不符", ErrBattleOutcomeOriginal)
	}
	if err = validateTerminalResult(f, event, req.RawResultPayload); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrBattleOutcomeOriginal, err)
	}
	old, err = readTerminal(ctx, tx, f.BattleId)
	if err != nil {
		return 0, err
	}
	if old != nil {
		return replayTerminal(old, req)
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return 0, err
	}
	digest := sha256.Sum256(req.RawResultPayload)
	_, err = tx.ExecContext(ctx, "INSERT INTO match_battle_completion(battle_id,raw_payload,payload_digest,format_version,archived_ms,source_topic,source_partition,source_offset,decision,source_kind,placement_operation_id,target_instance_id,outcome_protocol_version,result_published,next_retry_ms) VALUES(?,?,?,?,?,'',0,0,?,?,?,?,1,0,?)", f.BattleId, req.RawResultPayload, digest[:], uint32(1), stamp, uint32(pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE), uint32(pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_BATTLE_RPC), f.OperationId, f.TargetInstanceId, stamp)
	if err != nil {
		return 0, err
	}
	owners := map[uint64]*pb.MatchAuthorityTicketRecord{}
	for _, r := range roles {
		owners[r.PlayerId] = r
	}
	for _, settlement := range event.Settlements {
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(settlement)
		if err != nil {
			return 0, err
		}
		d := sha256.Sum256(body)
		owner := owners[settlement.PlayerId]
		_, err = tx.ExecContext(ctx, "INSERT INTO match_battle_settlement_source(battle_id,player_id,settlement_proto,payload_digest,archived_ms,owner_account,owner_revision,issued_at_ms,inbox_seq,inbox_state,dispatch_state,next_retry_ms) VALUES(?,?,?,?,?,?,?,?,0,0,1,?)", f.BattleId, settlement.PlayerId, body, d[:], stamp, owner.OwnerAccount, owner.OwnerRevision, stamp, stamp)
		if err != nil {
			return 0, err
		}
	}
	// Create回执可能丢失；COMPLETE本身是采用新协议的持久证据，但不伪造adopted回执字段。
	if _, err = tx.ExecContext(ctx, "UPDATE match_formation SET state=4,retry_at_ms=? WHERE battle_id=?", terminalOutcomeNoRetryMs, f.BattleId); err != nil {
		return 0, err
	}
	for _, r := range roles {
		if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=3 WHERE player_id=? AND ticket=? AND formation_id=? AND state=2", r.PlayerId, r.Ticket, f.BattleId); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("终局提交未知，必须重放原字节: %w", err)
	}
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, nil
}

// abortTerminalTx 只给已持全部原P与formation锁的取消入口使用；绝不调用运行时猜终局。
func (s *Store) abortTerminalTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord) (pb.BattleOutcomeDecision, error) {
	old, err := readTerminal(ctx, tx, f.BattleId)
	if err != nil {
		return 0, err
	}
	if old != nil {
		return old.Decision, nil
	}
	r, err := supportedOutcomeOriginal(f)
	if err != nil {
		return 0, nil
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return 0, err
	}
	if !terminalAbortDue(r.DeadlineMs, stamp) {
		return 0, nil
	}
	// 旧恢复流程可能先将运行时ABORTED收为state5；它自身不是终局证明。
	// 仍须本函数前面的原cap/version/UUID/body与deadline全部成立，才能补SQL ABORT。
	if f.State != 2 && f.State != 3 && f.State != 4 && f.State != 5 {
		return 0, nil
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO match_battle_completion(battle_id,raw_payload,payload_digest,format_version,archived_ms,source_topic,source_partition,source_offset,decision,source_kind,placement_operation_id,target_instance_id,outcome_protocol_version,result_published,next_retry_ms) VALUES(?,?,?,1,?,'',0,0,2,2,?,?,1,1,0)", f.BattleId, []byte{}, []byte{}, stamp, f.OperationId, f.TargetInstanceId)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_formation SET state=5 WHERE battle_id=?", f.BattleId); err != nil {
		return 0, err
	}
	for _, m := range f.Members {
		if _, err = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND ticket=? AND formation_id=? AND state IN(2,3)", m.PlayerId, m.Ticket, f.BattleId); err != nil {
			return 0, err
		}
	}
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, nil
}

// 两类outbox逐recipient以SQL截止租约claim；失败项推后，下一页不会永远被前8项占住。
// 回执丢失只重签认证并重放原正文。SQL写序列/原owner从不在投递时重算。
func (s *Store) PublishBattleOutcomesOnce(ctx context.Context, client func(context.Context) (inboxpb.PlayerInboxClient, error), secret string, publish func(context.Context, uint64, []byte) error) error {
	if client == nil || publish == nil {
		return errors.New("终局投递端口未装配")
	}
	stamp, err := s.Now(ctx)
	if err != nil {
		return err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT battle_id,raw_payload,payload_digest,next_retry_ms FROM match_battle_completion WHERE decision=1 AND result_published=0 AND next_retry_ms<=? ORDER BY next_retry_ms,battle_id LIMIT 8", stamp)
	if err != nil {
		return err
	}
	var results []*pb.MatchBattleCompletionRecord
	for rows.Next() {
		r := &pb.MatchBattleCompletionRecord{}
		if err = rows.Scan(&r.BattleId, &r.RawPayload, &r.PayloadDigest, &r.NextRetryMs); err != nil {
			rows.Close()
			return err
		}
		results = append(results, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, r := range results {
		lease := stamp + 30000
		changed, err := s.DB.ExecContext(ctx, "UPDATE match_battle_completion SET next_retry_ms=? WHERE battle_id=? AND decision=1 AND result_published=0 AND next_retry_ms=?", lease, r.BattleId, r.NextRetryMs)
		if err != nil {
			return err
		}
		n, err := changed.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		d := sha256.Sum256(r.RawPayload)
		if !bytes.Equal(d[:], r.PayloadDigest) {
			failures = append(failures, errors.New("结果outbox摘要损坏"))
			continue
		}
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = publish(call, r.BattleId, r.RawPayload)
		cancel()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if _, err = s.DB.ExecContext(ctx, "UPDATE match_battle_completion SET result_published=1 WHERE battle_id=? AND decision=1 AND result_published=0 AND next_retry_ms=?", r.BattleId, lease); err != nil {
			return err
		}
	}
	var people []*pb.MatchBattleSettlementSourceRecord
	// 首投与等待APPLIED各保四个名额，总预算仍八人；离线积压不能占满首投页。
	for _, seqCondition := range []string{"=0", ">0"} {
		rows, err = s.DB.QueryContext(ctx, "SELECT battle_id,player_id,settlement_proto,payload_digest,owner_account,owner_revision,issued_at_ms,inbox_seq,next_retry_ms FROM match_battle_settlement_source WHERE dispatch_state=1 AND inbox_seq"+seqCondition+" AND next_retry_ms<=? ORDER BY next_retry_ms,battle_id,player_id LIMIT 4", stamp)
		if err != nil {
			return err
		}
		for rows.Next() {
			r := &pb.MatchBattleSettlementSourceRecord{}
			if err = rows.Scan(&r.BattleId, &r.PlayerId, &r.SettlementProto, &r.PayloadDigest, &r.OwnerAccount, &r.OwnerRevision, &r.IssuedAtMs, &r.InboxSeq, &r.NextRetryMs); err != nil {
				rows.Close()
				return err
			}
			people = append(people, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	for _, r := range people {
		lease := stamp + 30000
		changed, err := s.DB.ExecContext(ctx, "UPDATE match_battle_settlement_source SET next_retry_ms=? WHERE battle_id=? AND player_id=? AND dispatch_state=1 AND next_retry_ms=?", lease, r.BattleId, r.PlayerId, r.NextRetryMs)
		if err != nil {
			return err
		}
		n, err := changed.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		d := sha256.Sum256(r.SettlementProto)
		if !bytes.Equal(d[:], r.PayloadDigest) {
			failures = append(failures, errors.New("个人outbox摘要损坏"))
			continue
		}
		call, cancel := context.WithTimeout(ctx, 2*time.Second)
		seq, state, err := deliverBattleOriginal(call, r, client, secret)
		cancel()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		dispatch := uint32(1)
		if state == inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED {
			dispatch = 2
		}
		next := stamp + pendingSettlementDelayMs(r.IssuedAtMs, stamp)
		if dispatch == 2 {
			next = 0
		}
		if _, err = s.DB.ExecContext(ctx, "UPDATE match_battle_settlement_source SET inbox_seq=?,inbox_state=?,dispatch_state=?,next_retry_ms=? WHERE battle_id=? AND player_id=? AND dispatch_state=1 AND next_retry_ms=? AND (inbox_seq=0 OR inbox_seq=?)", seq, uint32(state), dispatch, next, r.BattleId, r.PlayerId, lease, seq); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

// 由原签发时间和SQL时钟决定5秒至5分钟的有界退避，不改原件或新增重试账本。
func pendingSettlementDelayMs(issued, stamp uint64) uint64 {
	const maximum uint64 = 5 * 60 * 1000
	delay := uint64(5000)
	if stamp <= issued {
		return delay
	}
	age := stamp - issued
	for delay < maximum && age >= delay {
		age -= delay
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

// 查询发生在任何SQL/P锁之外；随后只按同一不可变来源CAS，再由ReleaseCompletedReady
// 重新持原P及票据校验。网络回包从不直接释放票，也不以入箱/水位猜APPLIED。
func (s *Store) RefreshBattleSettlementReceipt(ctx context.Context, ticket *pb.MatchAuthorityTicketRecord, resolve func(context.Context) (inboxpb.PlayerInboxClient, error), secret string) error {
	if ticket == nil || ticket.State != 3 || ticket.FormationId == 0 || ticket.OwnerAccount == "" || ticket.OwnerRevision == 0 {
		return nil
	}
	r := &pb.MatchBattleSettlementSourceRecord{}
	err := s.DB.QueryRowContext(ctx, "SELECT battle_id,player_id,settlement_proto,payload_digest,owner_account,owner_revision,issued_at_ms,inbox_seq FROM match_battle_settlement_source WHERE battle_id=? AND player_id=? AND owner_account=? AND owner_revision=? AND dispatch_state=1 AND inbox_seq>0", ticket.FormationId, ticket.PlayerId, ticket.OwnerAccount, ticket.OwnerRevision).Scan(&r.BattleId, &r.PlayerId, &r.SettlementProto, &r.PayloadDigest, &r.OwnerAccount, &r.OwnerRevision, &r.IssuedAtMs, &r.InboxSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	digest := sha256.Sum256(r.SettlementProto)
	if !bytes.Equal(digest[:], r.PayloadDigest) {
		return errors.New("个人outbox原摘要损坏")
	}
	call, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	seq, state, err := deliverBattleOriginal(call, r, resolve, secret)
	if err != nil || state != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED {
		return err
	}
	_, err = s.DB.ExecContext(ctx, "UPDATE match_battle_settlement_source SET inbox_state=2,dispatch_state=2,next_retry_ms=0 WHERE battle_id=? AND player_id=? AND owner_account=? AND owner_revision=? AND inbox_seq=? AND payload_digest=? AND dispatch_state=1", r.BattleId, r.PlayerId, r.OwnerAccount, r.OwnerRevision, seq, r.PayloadDigest)
	return err
}

func deliverBattleOriginal(ctx context.Context, r *pb.MatchBattleSettlementSourceRecord, resolve func(context.Context) (inboxpb.PlayerInboxClient, error), secret string) (uint64, inboxpb.PlayerInboxItemState, error) {
	client, err := resolve(ctx)
	if err != nil {
		return 0, 0, err
	}
	req := &inboxpb.BattleSettlementInboxRequest{PlayerId: r.PlayerId, BattleId: r.BattleId, ExpectedOwnerAccount: r.OwnerAccount, ExpectedOwnerRevision: r.OwnerRevision, IssuedAtMs: r.IssuedAtMs, SettlementPayload: append([]byte(nil), r.SettlementProto...)}
	v, err := serviceauth.New(map[string]string{"inbox": secret})
	if err != nil {
		return 0, 0, err
	}
	digest, err := serviceauth.Digest(req)
	if err != nil {
		return 0, 0, err
	}
	var head [12]byte
	binary.BigEndian.PutUint32(head[:4], uint32(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT))
	binary.BigEndian.PutUint64(head[4:], req.IssuedAtMs)
	h := sha256.New()
	h.Write(head[:])
	h.Write(req.SettlementPayload)
	content := h.Sum(nil)
	validate := func(method string, resp *inboxpb.BattleSettlementInboxResponse) error {
		if resp == nil {
			return errors.New("Inbox原收据缺失")
		}
		caller, err := v.Verify(ctx, method+"/reply", resp)
		if err != nil || caller != "inbox" || completionHasUnknown(resp.ProtoReflect()) || resp.RequestDigest != digest || resp.PlayerId != r.PlayerId || resp.BattleId != r.BattleId || resp.GetErrorMessage().GetId() != 0 || resp.Seq == 0 || (r.InboxSeq != 0 && r.InboxSeq != resp.Seq) || !bytes.Equal(content, resp.ContentSha256) || (resp.State != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING && resp.State != inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED) {
			return errors.New("Inbox签名原收据冲突或未得合法结局")
		}
		return nil
	}
	method := inboxpb.PlayerInbox_LookupBattleSettlement_FullMethodName
	if r.InboxSeq == 0 {
		method = inboxpb.PlayerInbox_EnqueueBattleSettlement_FullMethodName
	}
	if err = serviceauth.Sign(method, req, "match", secret, uint64(time.Now().UnixMilli())); err != nil {
		return 0, 0, err
	}
	var resp *inboxpb.BattleSettlementInboxResponse
	if r.InboxSeq == 0 {
		resp, err = client.EnqueueBattleSettlement(ctx, req)
	} else {
		resp, err = client.LookupBattleSettlement(ctx, req)
	}
	if err != nil {
		return 0, 0, err
	}
	if err = validate(method, resp); err != nil {
		return 0, 0, err
	}
	return resp.Seq, resp.State, nil
}
