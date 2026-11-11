package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
	inboxpb "proto/player_inbox"
	"shared/inboxcontract"
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

// ErrCompletionOriginalConflict:同一 battle_id 已归档的完整结算来源与这次的原字节不同。永久错误:原件已在库里,
// 重试永远不会成功。消费方据此记录后跳过,不能原地重试卡住整个分区(docs/design/infra-reconnect-overview.md §4 R11)。
var ErrCompletionOriginalConflict = errors.New("同对局完整结算来源原字节冲突")

func terminalAbortDue(deadline, stamp uint64) bool {
	return deadline != 0 && stamp >= deadline && stamp-deadline >= terminalOutcomeGraceMs
}

// FormationOutcomeDecided 判定成局的终局已经持久落定:state 5(安全中止 / 截止 ABORT),或 state 4 且 retry_at_ms 已是
// terminalOutcomeNoRetryMs。后者只有两个写入点,都要求同一原件的终局行已在:COMPLETE 提交(Store.CommitBattleOutcome)与
// retireTerminalOutcomeSchedules。COMPLETE 之后成局仍是 state 4,只看 state 认不出这场已结束(观战第二轮评审 R2-GO 严重-1)。
// 终局落定之后 battle 上的房间已收尾或正在收尾,这场不再接纳观众。
func FormationOutcomeDecided(formation *pb.MatchFormationRecord) bool {
	return formation.GetState() == 5 || formation.GetState() == 4 && formation.GetRetryAtMs() == terminalOutcomeNoRetryMs
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
			return ErrCompletionOriginalConflict
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

// supportedOutcomeOriginal 是截止 ABORT 与 COMPLETE 共读的原件判据(身份、版本、时刻):先核成局行上的能力证据
// (originalCapabilityCheck),再解码持久 Create 逐项核对(decodeOutcomeOriginal)。错误里带第一项不通过的原因码。
// 2026-11-05 起逐项判据拆成上面两个函数,人工处置工具(manual_abort.go)读同一份规则报告「哪一项不通过」;
// 放行与拒绝的条件、两类错误的分界与拆分前逐字相同。
func supportedOutcomeOriginal(f *pb.MatchFormationRecord) (*battlepb.CreateBattleRequest, error) {
	if check := originalCapabilityCheck(f); check != "" {
		return nil, fmt.Errorf("旧局或原能力证据不完整，终局保持UNKNOWN(%s)", check)
	}
	r, check := decodeOutcomeOriginal(f)
	if check != "" {
		return nil, fmt.Errorf("持久创建原件与终局身份冲突(%s)", check)
	}
	return r, nil
}

// originalCapabilityCheck 核成局行上的协议版本与发房前能力证据,返回第一项不通过的原因码(ASCII,低基数),全部通过回空串。
func originalCapabilityCheck(f *pb.MatchFormationRecord) string {
	switch {
	case f == nil:
		return "formation_missing"
	case f.OutcomeProtocolVersion != 1:
		return "outcome_protocol"
	case f.PreflightSupportedVersion != 1:
		return "preflight_version"
	case f.TargetInstanceId == "":
		return "target_instance_missing"
	case f.PreflightInstanceId != f.TargetInstanceId:
		return "preflight_instance_mismatch"
	case f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion != 1:
		return "adopted_legacy" // battle 明确回执采用了旧协议
	case !f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion != 0:
		return "adopted_invalid"
	}
	return ""
}

// decodeOutcomeOriginal 解码持久 Create 原件并逐项与成局行核对,返回原件(解码失败为 nil)与第一项不通过的原因码
// (全部通过回空串)。只看原件正文,不看能力证据;核对项不通过时也返回已解码的原件,人工处置要用它的截止与准入期限。
func decodeOutcomeOriginal(f *pb.MatchFormationRecord) (*battlepb.CreateBattleRequest, string) {
	if f == nil {
		return nil, "formation_missing"
	}
	r := &battlepb.CreateBattleRequest{}
	if proto.Unmarshal(f.CreateRequest, r) != nil {
		return nil, "create_undecodable"
	}
	switch {
	case r.OutcomeProtocolVersion != 1:
		return r, "create_protocol"
	case r.BattleId != f.BattleId:
		return r, "battle_id_mismatch"
	case r.PlacementOperationId != f.OperationId:
		return r, "operation_mismatch"
	case r.TargetInstanceId != f.TargetInstanceId:
		return r, "target_instance_mismatch"
	case r.BattleConfigId != f.ConfigId:
		return r, "config_mismatch"
	case r.MatchMode != f.Mode:
		return r, "mode_mismatch"
	case r.DeadlineMs == 0:
		return r, "deadline_missing"
	case r.DeadlineMs != f.DeadlineMs:
		return r, "deadline_mismatch"
	case r.CreatedAtMs < f.CreatedMs:
		return r, "created_before_formation"
	case r.DeadlineMs <= r.CreatedAtMs:
		return r, "created_after_deadline"
	case len(r.Players) != len(f.Members):
		return r, "players_count_mismatch"
	}
	return r, ""
}

// originalMemberTeams 核对 Create 原件的参战名单与成局成员是同一个集合:r.Players 无空号、无重复,与 f.Members 一一对应
// (f.Members 自身也不得重复)。返回 玩家→队号,COMPLETE 据它核个人结算的成员与队伍。
// 截止 ABORT 不读它:释放按 f.Members 走,不依赖原件名单,名单不符的原件照样能被作废。
func originalMemberTeams(f *pb.MatchFormationRecord, r *battlepb.CreateBattleRequest) (map[uint64]uint32, error) {
	teams := make(map[uint64]uint32, len(r.Players))
	for _, p := range r.Players {
		if p == nil || p.PlayerId == 0 {
			return nil, errors.New("创建成员为空")
		}
		if _, exists := teams[p.PlayerId]; exists {
			return nil, errors.New("创建成员重复")
		}
		teams[p.PlayerId] = p.TeamIndex
	}
	if len(f.Members) != len(teams) {
		return nil, errors.New("创建成员与原成局成员人数不符")
	}
	seen := make(map[uint64]bool, len(f.Members))
	for _, m := range f.Members {
		if _, ok := teams[m.PlayerId]; !ok || seen[m.PlayerId] {
			return nil, errors.New("终局缺原成局成员")
		}
		seen[m.PlayerId] = true
	}
	return teams, nil
}

// completableOriginal 是「这份持久原件能被 COMPLETE 裁决」只看原件的全部判据:supportedOutcomeOriginal(身份、版本、时刻,
// 截止 ABORT 也读它)加 originalMemberTeams(参战名单)。validateTerminalResult 与落库前的 validateSupportedIntent 都只调它,
// 判据只写这一处;以后给 COMPLETE 加只看原件的检查,就加在这里(2026-11-05 修复)。
func completableOriginal(f *pb.MatchFormationRecord) (*battlepb.CreateBattleRequest, map[uint64]uint32, error) {
	r, err := supportedOutcomeOriginal(f)
	if err != nil {
		return nil, nil, err
	}
	teams, err := originalMemberTeams(f, r)
	if err != nil {
		return nil, nil, err
	}
	return r, teams, nil
}

// ErrUnterminableOriginal:这份 v1 Create 原件一旦持久,COMPLETE 就裁决不了它(completableOriginal 不过;其中身份、版本、
// 时刻不过时到期 ABORT 也裁决不了),全员只能白打一局或永久卡住,所以拒绝落库、也不发 Create;gather 按发房前失败
// 干净收尾、全员回队,并单独计 match_gather_total{outcome="original_unterminable"}(2026-11-05 修复)。用 errors.Is 判断。
var ErrUnterminableOriginal = errors.New("Create原件落库后终局无法裁决，拒绝持久")

// validateSupportedIntent 在 saveIntent 的 formation 行锁内、落库之前调用:用锁内原行加上即将写入的四个字段
// (create_request、outcome_protocol_version、preflight_supported_version、preflight_instance_id)拼出落库后的成局行,
// 跑一遍 COMPLETE 只看原件的全部判据 completableOriginal(它包含截止 ABORT 读的 supportedOutcomeOriginal)。
// 凡是持久了的 v1 原件,COMPLETE 与 ABORT 就都裁决得了,落库之后终局失败只可能来自 battle 回的结果本身;
// 不合格的原件不落库,也就不会发出一份「能开打、却永远收不了尾」或「打完只能作废」的 Create。
// 修复前的触发:成局 created_ms 取 TiDB 时刻,原件 CreatedAtMs 取 match 本机墙钟,本机慢于 TiDB 时 r.CreatedAtMs < f.CreatedMs,
// 原件照常落库、battle 照常开打,之后 COMPLETE 与 ABORT 都永久失败,全员票与帮会占用永不释放。
// 名单不符(scene 回的快照玩家号错或为 0)时修复前同样照常落库开打,每次 COMPLETE 都被拒,只能截止后作废。
func validateSupportedIntent(f *pb.MatchFormationRecord, request []byte, instance string) error {
	candidate := proto.Clone(f).(*pb.MatchFormationRecord)
	candidate.CreateRequest = request
	candidate.OutcomeProtocolVersion, candidate.PreflightSupportedVersion, candidate.PreflightInstanceId = 1, 1, instance
	if _, _, err := completableOriginal(candidate); err != nil {
		logx.Errorf("global formation create original rejected before persist: no terminal path could ever adjudicate it (check clock skew between match and TiDB, or a prepare snapshot whose player does not match the formation) battle_id=%d: %v", f.GetBattleId(), err)
		return fmt.Errorf("%w: %v", ErrUnterminableOriginal, err)
	}
	return nil
}

func validateTerminalPayload(event *kafkapb.BattleResultEvent, raw []byte) error {
	if err := validateCompletion(event, raw); err != nil {
		return err
	}
	if event.SettlementFormatVersion != 1 || completionHasUnknown(event.ProtoReflect()) {
		return errors.New("终局执行要求完整已知语义的版本1原件")
	}
	// 个人原件要整份入 Inbox:上限只读 proto 类型规格(shared/inboxcontract,与收件箱校验同一来源),读不到按未知拒绝。
	limit, limited := inboxcontract.MaxPayloadBytes(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT)
	if !limited {
		return errors.New("Inbox载荷上限未知，整局不能提交COMPLETE")
	}
	for _, s := range event.Settlements {
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(s)
		if err != nil {
			return err
		}
		if len(body) > limit {
			return fmt.Errorf("个人结算%d字节超过Inbox载荷上限%d字节，整局不能提交COMPLETE", len(body), limit)
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
	r, members, err := completableOriginal(f)
	if err != nil {
		return err
	}
	// 结束时刻只要求非 0,不与原件 CreatedAtMs 比先后(2026-11-05 修复):FinishedAtMs 取 battle 主机时钟,CreatedAtMs 取
	// match 侧时刻,两台主机比先后,battle 时钟稍慢时几秒就分出胜负的局(逃跑、秒杀)会被永久拒收、截止后整局作废。
	// 它只给 scene 的近期战绩与帮会补登记作展示时刻、给帮会结算延迟作指标,不参与任何裁决;终局属于哪一局由
	// placement_operation_id + target_instance_id 与持久原件逐项核对,不靠时间先后。帮会侧同样只要求非 0。
	if event.BattleId != r.BattleId || event.BattleConfigId != r.BattleConfigId || event.MatchMode != r.MatchMode || event.FinishedAtMs == 0 || !proto.Equal(event.ActivityContext, r.ActivityContext) {
		return errors.New("终局配置、活动不符或缺结束时刻")
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
func (s *Store) terminalFormation(ctx context.Context, id uint64) (tx *sql.Tx, locked *pb.MatchFormationRecord, originals []*pb.MatchAuthorityTicketRecord, err error) {
	err = s.Fenced(ctx, func() (e error) { tx, locked, originals, e = s.terminalFormationOnce(ctx, id); return e })
	return tx, locked, originals, err
}

func (s *Store) terminalFormationOnce(ctx context.Context, id uint64) (*sql.Tx, *pb.MatchFormationRecord, []*pb.MatchAuthorityTicketRecord, error) {
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

// decodeTerminalRequest 做不碰 SQL 的终局原件校验(大小、未知字段、解码、完整已知语义与 Inbox 上限)。
// 返回的错误都已包成 ErrBattleOutcomeOriginal:这些毛病与库里状态无关,重放同一份原件结果不变。
func decodeTerminalRequest(req *pb.CommitBattleOutcomeRequest) (*kafkapb.BattleResultEvent, error) {
	if len(req.RawResultPayload) == 0 || len(req.RawResultPayload) > maxCompletionBytes || completionHasUnknown(req.ProtoReflect()) {
		return nil, fmt.Errorf("%w: 终局请求原件不完整", ErrBattleOutcomeOriginal)
	}
	event := &kafkapb.BattleResultEvent{}
	if err := proto.Unmarshal(req.RawResultPayload, event); err != nil {
		return nil, fmt.Errorf("%w: 终局原字节不能解码", ErrBattleOutcomeOriginal)
	}
	if err := validateTerminalPayload(event, req.RawResultPayload); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBattleOutcomeOriginal, err)
	}
	return event, nil
}

// CommitBattleOutcome 在任何个人副作用前持久唯一决定和两个outbox。Commit未知绝不返回成功。
//
// 2026-11-05 修复:先复读既有终局,再判原件冲突。另一副本可能刚在截止后提交了 ABORT(同一事务把票置 4、formation 置 5);
// 原件即使永久不合格(个人结算超 Inbox 上限、版本错位带未知字段),battle 也要拿到这份 ABORT 才能收尾删房,
// 不能永远收到 FailedPrecondition、把房间占到 30 小时本地清理。既有 COMPLETE 照旧逐字节比对,不会因此放过别的原件。
// 2026-11-11:原执行实例的原件内容不合格、且已过权威截止加宽限时,这次重放直接在锁内写 ABORT 并回 ABORT
// (rejected_original_abort.go),不再等截止作废取证到硬上限;截止之前与版本错位类拒收照旧回原件冲突。
func (s *Store) CommitBattleOutcome(ctx context.Context, req *pb.CommitBattleOutcomeRequest) (pb.BattleOutcomeDecision, error) {
	if s == nil || s.DB == nil {
		return 0, errors.New("终局权威未装配")
	}
	// 身份不全就查不到属于哪一局,不碰 SQL,直接按原件冲突拒绝。
	if req == nil || req.BattleId == 0 || req.PlacementOperationId == "" || req.TargetInstanceId == "" {
		return 0, fmt.Errorf("%w: 终局请求原件不完整", ErrBattleOutcomeOriginal)
	}
	event, originalErr := decodeTerminalRequest(req)
	old, err := readTerminal(ctx, s.DB, req.BattleId)
	if err != nil {
		if originalErr != nil {
			// 原件本身永久不合格,与库是否可读无关;仍按原件冲突分类,读失败的原因一并带上。
			return 0, errors.Join(originalErr, err)
		}
		return 0, err
	}
	if old != nil && old.Decision != 0 {
		decision, replayErr := replayTerminal(old, req)
		if replayErr == nil && decision == pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE {
			// 只核对、只记录,不影响回执(battle_report.go)。
			checkBattleReportReplay(ctx, s.DB, req)
		}
		return decision, replayErr
	}
	if originalErr != nil {
		return s.settleRejectedOriginal(ctx, req, originalErr)
	}
	return s.commitBattleOutcomeLocked(ctx, req, event)
}

// commitBattleOutcomeLocked 是 COMPLETE 的锁内提交点:原 P→formation→原票加锁,锁内先复读终局,再核原件,最后同一事务写
// COMPLETE、全员个人来源、formation 4 与票 2→3。锁外复读没看到终局、进锁之前另一副本刚提交 ABORT 的竞态落在这里,
// 单独成函数后可以确定性复现(测试跳过锁外复读直接调它)。
func (s *Store) commitBattleOutcomeLocked(ctx context.Context, req *pb.CommitBattleOutcomeRequest, event *kafkapb.BattleResultEvent) (pb.BattleOutcomeDecision, error) {
	tx, f, roles, err := s.terminalFormation(ctx, req.BattleId)
	if err != nil {
		// 另一入口可能刚提交了终局:截止 ABORT 在同一事务里把原票置 4(这里读到 ErrOriginalPlayer),任何提交都会让锁内
		// 成局与锁外观察不一致。有终局就回放;没有才按原错误处理,原票失效才是原件冲突。
		old, readErr := readTerminal(ctx, s.DB, req.BattleId)
		if readErr != nil {
			return 0, errors.Join(err, readErr)
		}
		if old != nil && old.Decision != 0 {
			return replayTerminal(old, req)
		}
		if errors.Is(err, ErrOriginalPlayer) {
			return 0, fmt.Errorf("%w: 原成员或归属失效", ErrBattleOutcomeOriginal)
		}
		return 0, err
	}
	defer tx.Rollback()
	// 锁内也先复读终局:已有决定就按它回放(state 5 的局回 ABORT),不走下面的冲突判定。
	old, err := readTerminal(ctx, tx, f.BattleId)
	if err != nil {
		return 0, err
	}
	if old != nil {
		return replayTerminal(old, req)
	}
	if f.OperationId != req.PlacementOperationId || f.TargetInstanceId != req.TargetInstanceId || f.State == 5 {
		return 0, fmt.Errorf("%w: 终局原操作、实例或成局状态不符", ErrBattleOutcomeOriginal)
	}
	if err = validateTerminalResult(f, event, req.RawResultPayload); err != nil {
		// 原执行实例的原件内容不合格:过了截止加宽限就在这把锁里作废(rejected_original_abort.go),否则照旧回原件冲突。
		return s.abortRejectedOriginalLocked(ctx, tx, f, req, fmt.Errorf("%w: %v", ErrBattleOutcomeOriginal, err))
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
	// 战报回放(battle-report-replay.md BR-4):与终局同一事务写;回放外壳不合格只丢回放,SQL 错误照常整笔回滚。
	reportWrite, err := writeBattleReportTx(ctx, tx, f, event, req.GetReplay(), stamp)
	if err != nil {
		return 0, err
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
	reportWrite.observe()
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE, nil
}

// abortTerminalTx 是截止裁决,只给已持全部原P与formation锁的入口(到期恢复、帮会取消)使用；事务里不调用运行时。
// evidence 是调用方在事务之前向原执行实例取到的截止作废证据(deadlineEvidence / probeDeadlinePage),时间判据只在
// judgeDeadlineAbort(2026-11-06)。已有终局回它的决定(verdict.abort=false);自动路径不裁决的局(autoTerminalOriginal 为假:
// 不在途,或原件不可裁决)回 UNKNOWN;judgeDeadlineAbort 判可以作废时在本事务里经 commitAbortTx 写 ABORT(verdict.abort=true),
// 由调用方提交,提交成功后 observeDeadlineAbort 计数;判要等时 verdict.nextProbeMs 是到期恢复下次取证的排期。
// 缺终局的 state 5 不再补 ABORT(2026-11-06):新代码写 state 5 时同一事务写终局或根本没有 Create 原件,
// 缺终局的 state 5 只来自旧代码,项目未上线不兼容(与到期恢复同一口径,scene 查询回不会落定)。
func (s *Store) abortTerminalTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, evidence DeadlineAbortEvidence) (pb.BattleOutcomeDecision, deadlineAbortVerdict, error) {
	old, err := readTerminal(ctx, tx, f.BattleId)
	if err != nil {
		return 0, deadlineAbortVerdict{}, err
	}
	if old != nil {
		return old.Decision, deadlineAbortVerdict{}, nil
	}
	r, ok := autoTerminalOriginal(f)
	if !ok {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, deadlineAbortVerdict{}, nil
	}
	stamp, err := now(ctx, tx)
	if err != nil {
		return 0, deadlineAbortVerdict{}, err
	}
	verdict := judgeDeadlineAbort(r.DeadlineMs, stamp, f.RetryAtMs, evidence)
	if !verdict.abort {
		return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_UNKNOWN, verdict, nil
	}
	// 截止裁决的 ABORT 没有 Battle 原字节：format_version=0、来源记为 MATCH_DEADLINE_ABORT，不冒充 BATTLE_RPC。
	if err = commitAbortTx(ctx, tx, f, stamp, pb.MatchBattleCompletionSourceKind_MATCH_BATTLE_COMPLETION_SOURCE_MATCH_DEADLINE_ABORT); err != nil {
		return 0, deadlineAbortVerdict{}, err
	}
	return pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT, verdict, nil
}

// PublishBattleOutcomesOnce 按默认参数(DefaultOutcomeOutboxOptions:结果 8 行、首投 4 行、等待 4 行、逐行串行,
// 与 2026-10-30 之前写死的行为一致)投递一轮终局发件箱,不计数。生产循环改走带 GlobalMatch.OutcomeOutbox 配置的
// PublishBattleOutcomes(outcome_outbox.go,实现与语义都在那里);这里留给只要默认参数的调用方与用例。
func (s *Store) PublishBattleOutcomesOnce(ctx context.Context, client func(context.Context) (inboxpb.PlayerInboxClient, error), secret string, publish func(context.Context, uint64, []byte) error) error {
	return s.PublishBattleOutcomes(ctx, client, secret, publish, DefaultOutcomeOutboxOptions())
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
	// 收件箱写进条目的同一份内容摘要(布局只在 shared/inboxcontract 写一份)。
	content := inboxcontract.ContentSHA256(uint32(inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_BATTLE_SETTLEMENT), req.IssuedAtMs, req.SettlementPayload)
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
