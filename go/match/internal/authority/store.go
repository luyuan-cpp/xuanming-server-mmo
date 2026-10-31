package authority

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"shared/authoritysql"
	"sort"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"match/internal/catalog"
	"match/internal/config"
	"match/internal/scope"
	battlepb "proto/battle"
	pb "proto/match"
	"schemamigrate"
	"shared/roleidentity"
)

type Store struct {
	roleSchema       string
	entryGuildSchema string
	DB               *sql.DB
	Secret           []byte
	Config           config.GlobalMatchConf
	TeamConfig       config.TeamConf // 启动时由同一SQL策略指纹准入，运行中不热改。
	roleGuard        func(context.Context, *sql.Tx, []*pb.MatchAuthorityTicketRecord) error
	entryGuard       func(context.Context, *sql.Tx, []*pb.MatchAuthorityTicketRecord, *matchEntryOriginal) error
	// catalog 与 schedule 由 ConfigureMatchCatalog 启动时装配，之后只读（见 match_rules.go）。
	catalog  *catalog.Catalog
	schedule scope.Schedule
}

const ticketColumns = "player_id,ticket,request_id,mode,config_id,team_size,state,enqueued_ms,not_before_ms,zone_id,rating_centi,formation_id,team_id,network_profile,expires_ms,selected_region,preferred_region,owner_account,owner_revision"
const formationColumns = "battle_id,operation_id,state,physical_region,target_node_id,target_instance_id,target_endpoint,mode,config_id,members,create_request,created_ms,deadline_ms,retry_at_ms,activity_context,requeue_on_failure,prepared,outcome_protocol_version,preflight_supported_version,preflight_instance_id,adopted_outcome_protocol_version,adopted_outcome_observed"

var ErrJoinCancelled = errors.New("匹配请求已经撤销")

func Open(ctx context.Context, c config.GlobalMatchConf) (*Store, error) {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return nil, err
	}
	return OpenWithPermission(ctx, c, authoritysql.Writer(writer))
}

// OpenWithPermission 保留全部既有校验，由进程显式传同一 boot 的能力。
func OpenWithPermission(ctx context.Context, c config.GlobalMatchConf, permission authoritysql.Permission) (*Store, error) {
	secret := []byte(os.Getenv(c.ProbeSecretEnv))
	if len(secret) < 32 {
		return nil, errors.New("全球匹配探测密钥至少32字节，必须从环境注入")
	}
	if c.CrossRegionWaitSeconds == 0 || c.HardLatencyMs < 20 || c.HardLatencyMs > 1000 || c.ProbeTTLSeconds < 30 || c.ProbeTTLSeconds > 600 {
		return nil, errors.New("全球匹配数据库或地域配置无效")
	}
	db, err := openAuthorityDatabaseWithPermission(ctx, c, permission)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	if err = requireCurrentSchema(ctx, db, c.Database, schemamigrate.Plan); err != nil {
		return nil, err
	}
	store := &Store{DB: db, Secret: secret, Config: c}
	if err = store.CheckCandidateIndexes(ctx); err != nil {
		return nil, err
	}
	ok = true
	return store, nil
}

type scanner interface{ Scan(...any) error }

func scanTicket(s scanner) (*pb.MatchAuthorityTicketRecord, error) {
	t := &pb.MatchAuthorityTicketRecord{}
	err := s.Scan(&t.PlayerId, &t.Ticket, &t.RequestId, &t.Mode, &t.ConfigId, &t.TeamSize, &t.State, &t.EnqueuedMs, &t.NotBeforeMs, &t.ZoneId, &t.RatingCenti, &t.FormationId, &t.TeamId, &t.NetworkProfile, &t.ExpiresMs, &t.SelectedRegion, &t.PreferredRegion, &t.OwnerAccount, &t.OwnerRevision)
	return t, err
}
func (s *Store) Ticket(ctx context.Context, pid uint64) (*pb.MatchAuthorityTicketRecord, error) {
	if _, present := roleidentity.Original(ctx); present {
		return s.ticketForActor(ctx, pid)
	}
	if _, e := s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND state=1 AND expires_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)", pid); e != nil {
		return nil, e
	}
	t, e := scanTicket(s.DB.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=?", pid))
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	return t, e
}
func now(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (uint64, error) {
	var n uint64
	err := q.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)").Scan(&n)
	return n, err
}
func (s *Store) Now(ctx context.Context) (uint64, error) { return now(ctx, s.DB) }

func (s *Store) SavePlan(ctx context.Context, pid uint64, p *pb.MatchNetworkProfile, expires uint64) error {
	if p == nil {
		return ErrOriginalPlayer
	}
	actor := originalPlayer(ctx, pid)
	p = proto.Clone(p).(*pb.MatchNetworkProfile)
	p.OwnerAccount, p.OwnerRevision = actor.OwnerAccount, actor.OwnerRevision
	b, e := proto.Marshal(p)
	if e != nil {
		return e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.lockActor(ctx, tx, pid); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO match_network_profile(player_id,payload,expires_ms) VALUES(?,?,?) ON DUPLICATE KEY UPDATE payload=VALUES(payload),expires_ms=VALUES(expires_ms)", pid, b, expires); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Profile(ctx context.Context, pid uint64) (*pb.MatchNetworkProfile, uint64, error) {
	var b []byte
	var expires uint64
	e := s.DB.QueryRowContext(ctx, "SELECT payload,expires_ms FROM match_network_profile WHERE player_id=?", pid).Scan(&b, &expires)
	if e != nil {
		return nil, 0, e
	}
	p, e := DecodeProfile(b)
	return p, expires, e
}
func (s *Store) CommitProfile(ctx context.Context, pid uint64, plan, binding string, receipts []*pb.NetworkProbeReceipt, expires uint64) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.lockActor(ctx, tx, pid); e != nil {
		return e
	}
	var b []byte
	var oldExpiry uint64
	if e = tx.QueryRowContext(ctx, "SELECT payload,expires_ms FROM match_network_profile WHERE player_id=? FOR UPDATE", pid).Scan(&b, &oldExpiry); e != nil {
		return e
	}
	p, e := DecodeProfile(b)
	if e != nil {
		return e
	}
	actor := originalPlayer(ctx, pid)
	if p.OwnerAccount != actor.OwnerAccount || p.OwnerRevision != actor.OwnerRevision {
		return ErrOriginalPlayer
	}
	n, e := now(ctx, tx)
	if e != nil {
		return e
	}
	if p.PlanId != plan || p.SessionBinding != binding || oldExpiry <= n || expires <= n {
		return errors.New("地域计划已经失效")
	}
	p.Receipts = receipts
	b, e = proto.Marshal(p)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE match_network_profile SET payload=?,expires_ms=? WHERE player_id=?", b, expires, pid); e != nil {
		return e
	}
	// 等待过程中刷新同一会话的实测数据，不修改票号或最初等待时刻。
	active, e := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", pid))
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	// 整队unit固定入队时全员探测原件；单人刷新不得绕过P全员/Team/launch锁顺序改单票。
	// 入口已关闭的票不再续命：不刷新它的有效期，最长一个探测有效期后自然过期出队。
	if e == nil && active.State == 1 && active.TeamId == 0 && s.catalog.Open(pb.MatchMode(active.Mode), active.ConfigId) {
		oldProfile, decodeErr := DecodeProfile(active.NetworkProfile)
		if decodeErr != nil {
			return decodeErr
		}
		if oldProfile.SessionBinding == binding {
			if _, e = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET network_profile=?,expires_ms=? WHERE player_id=? AND state=1", b, expires, pid); e != nil {
				return e
			}
			active.NetworkProfile, active.ExpiresMs = b, expires
			if e = s.replaceTicketRegions(ctx, tx, active, n); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}

// Join 的锁和提交只在 SQL：同请求回放返回原票，不重置最初等待时间。
func (s *Store) Join(ctx context.Context, t *pb.MatchAuthorityTicketRecord, plan, binding string) (*pb.MatchAuthorityTicketRecord, bool, error) {
	if t == nil {
		return nil, false, ErrOriginalPlayer
	}
	actor := originalPlayer(ctx, t.PlayerId)
	t = proto.Clone(t).(*pb.MatchAuthorityTicketRecord)
	t.OwnerAccount, t.OwnerRevision = actor.OwnerAccount, actor.OwnerRevision
	high, low, e := joinRequestParts(t.RequestId)
	if e != nil {
		return nil, false, e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, false, e
	}
	defer tx.Rollback()
	if e = s.lockActor(ctx, tx, t.PlayerId); e != nil {
		return nil, false, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT IGNORE INTO match_authority_ticket("+ticketColumns+") VALUES(?,'','',0,0,0,0,0,0,0,0,0,0,?,0,0,0,'',0)", t.PlayerId, []byte{}); e != nil {
		return nil, false, e
	}
	old, e := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", t.PlayerId))
	if e != nil {
		return nil, false, e
	}
	// Join 与 Cancel 永远先争同一玩家行锁。历史不能只放当前票据，否则换票后重放旧请求会复活。
	var original []byte
	var cancelled bool
	var intentOwner string
	var intentRevision uint64
	var exitPrepared bool
	var exitRaw []byte
	pending := false
	e = tx.QueryRowContext(ctx, "SELECT ticket_record,cancelled,owner_account,owner_revision,exit_prepared,spectator_exit FROM match_join_intent WHERE player_id=? AND request_high=? AND request_low=?", t.PlayerId, high, low).Scan(&original, &cancelled, &intentOwner, &intentRevision, &exitPrepared, &exitRaw)
	if e == nil {
		if intentOwner != t.OwnerAccount || intentRevision != t.OwnerRevision {
			return nil, false, ErrOriginalPlayer
		}
		if cancelled {
			return nil, false, ErrJoinCancelled
		}
		replayed := &pb.MatchAuthorityTicketRecord{}
		if e = proto.Unmarshal(original, replayed); e != nil || replayed.Ticket == "" {
			return nil, false, errors.New("匹配意图原票损坏")
		}
		if replayed.OwnerAccount != t.OwnerAccount || replayed.OwnerRevision != t.OwnerRevision {
			return nil, false, ErrOriginalPlayer
		}
		if replayed.Mode != t.Mode || replayed.ConfigId != t.ConfigId || replayed.TeamSize != t.TeamSize {
			return nil, false, errors.New("重复请求的匹配参数不同")
		}
		if replayed.State != 0 {
			if old.Ticket == replayed.Ticket {
				replayed = old
			} else {
				replayed.State = 4
			}
			return replayed, false, tx.Commit()
		}
		if !exitPrepared || old.State != 0 || old.Ticket != replayed.Ticket || old.RequestId != replayed.RequestId {
			return nil, false, ErrOriginalPlayer
		}
		if len(exitRaw) != 0 {
			exit := &pb.MatchSpectatorRecord{}
			if proto.Unmarshal(exitRaw, exit) != nil || exit.PlayerId != t.PlayerId {
				return nil, false, ErrOriginalPlayer
			}
			if e = s.RequireSpectatorExitsClosedTx(ctx, tx, []*pb.MatchSpectatorRecord{exit}); e != nil {
				return nil, false, e
			}
		}
		t, pending = replayed, true
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, false, e
	}
	if e = s.requireMatchEntryTx(ctx, tx, []*pb.MatchAuthorityTicketRecord{t}, nil); e != nil {
		return nil, false, e
	}
	if e = s.RequireNoSpectators(ctx, tx, []uint64{t.PlayerId}); e != nil {
		return nil, false, e
	}
	currentTime, e := now(ctx, tx)
	if e != nil {
		return nil, false, e
	}
	if old.State == 1 && old.ExpiresMs <= currentTime {
		old.State = 4
		if _, e = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=?", t.PlayerId); e != nil {
			return nil, false, e
		}
	}
	if !pending && old.RequestId == t.RequestId && old.Ticket != "" {
		if old.OwnerAccount != t.OwnerAccount || old.OwnerRevision != t.OwnerRevision {
			return nil, false, ErrOriginalPlayer
		}
		if old.Mode != t.Mode || old.ConfigId != t.ConfigId || old.TeamSize != t.TeamSize {
			return nil, false, errors.New("重复请求的匹配参数不同")
		}
		if e = saveJoinIntent(ctx, tx, old); e != nil {
			return nil, false, e
		}
		return old, false, tx.Commit()
	}
	if !pending && old.State == 0 && TicketOccupied(old) {
		return nil, false, errors.New("玩家已有待提交匹配原意图")
	}
	if old.State > 0 && old.State < 4 {
		if old.OwnerAccount != t.OwnerAccount || old.OwnerRevision != t.OwnerRevision {
			return nil, false, ErrOriginalPlayer
		}
		return old, false, tx.Commit()
	}
	var b []byte
	var exp uint64
	if pending {
		b, exp = t.NetworkProfile, t.ExpiresMs
	} else if b, exp, e = s.joinProfileTx(ctx, tx, t, plan, binding); e != nil {
		return nil, false, e
	}
	n, e := now(ctx, tx)
	if e != nil {
		return nil, false, e
	}
	if pending {
		p, decodeErr := DecodeProfile(b)
		if decodeErr != nil {
			return nil, false, decodeErr
		}
		if p.PlanId != plan || p.SessionBinding != binding || exp <= n || len(Latencies(p, n, s.Config.HardLatencyMs)) == 0 || p.OwnerAccount != t.OwnerAccount || p.OwnerRevision != t.OwnerRevision {
			return nil, false, errors.New("原匹配探测已失效，需取消原请求")
		}
	}
	t.State = 1
	t.EnqueuedMs = n
	t.NetworkProfile = b
	t.ExpiresMs = exp
	_, e = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET ticket=?,request_id=?,mode=?,config_id=?,team_size=?,state=1,enqueued_ms=?,not_before_ms=0,zone_id=?,rating_centi=?,formation_id=0,team_id=?,network_profile=?,expires_ms=?,selected_region=0,owner_account=?,owner_revision=? WHERE player_id=?", t.Ticket, t.RequestId, t.Mode, t.ConfigId, t.TeamSize, t.EnqueuedMs, t.ZoneId, t.RatingCenti, t.TeamId, t.NetworkProfile, t.ExpiresMs, t.OwnerAccount, t.OwnerRevision, t.PlayerId)
	if e != nil {
		return nil, false, e
	}
	if e = s.replaceTicketRegions(ctx, tx, t, n); e != nil {
		return nil, false, e
	}
	if e = saveJoinIntent(ctx, tx, t); e != nil {
		return nil, false, e
	}
	return t, true, tx.Commit()
}

func (s *Store) Cancel(ctx context.Context, pid uint64, ticket string) error {
	return s.CancelIntent(ctx, pid, ticket, "")
}

func saveJoinIntent(ctx context.Context, tx *sql.Tx, t *pb.MatchAuthorityTicketRecord) error {
	high, low, e := joinRequestParts(t.RequestId)
	if e != nil {
		return e
	}
	b, e := proto.MarshalOptions{Deterministic: true}.Marshal(t)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO match_join_intent(player_id,request_id,ticket_record,cancelled,request_high,request_low,owner_account,owner_revision,state) VALUES(?,?,?,0,?,?,?,?,2) ON DUPLICATE KEY UPDATE ticket_record=VALUES(ticket_record),state=2", t.PlayerId, t.RequestId, b, high, low, t.OwnerAccount, t.OwnerRevision)
	return e
}

func joinRequestParts(request string) (uint64, uint64, error) {
	parsed, e := uuid.Parse(request)
	if e != nil || parsed == uuid.Nil || parsed.String() != request {
		return 0, 0, errors.New("匹配请求编号无效")
	}
	return binary.BigEndian.Uint64(parsed[:8]), binary.BigEndian.Uint64(parsed[8:]), nil
}

// CancelIntent 的提交是 UNKNOWN Join 可以安全放弃的唯一依据。墓碑不影响玩家的新 request_id。
func (s *Store) CancelIntent(ctx context.Context, pid uint64, ticket, request string) error {
	if request != "" {
		if _, _, e := joinRequestParts(request); e != nil {
			return e
		}
	} else if ticket == "" {
		return errors.New("撤销需要原请求编号或票据")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.lockActor(ctx, tx, pid); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT IGNORE INTO match_authority_ticket("+ticketColumns+") VALUES(?,'','',0,0,0,0,0,0,0,0,0,0,?,0,0,0,'',0)", pid, []byte{}); e != nil {
		return e
	}
	old, e := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", pid))
	if e != nil {
		return e
	}
	if request == "" && old.Ticket == ticket {
		request = old.RequestId
	}
	actor := originalPlayer(ctx, pid)
	if old.Ticket != "" && ((request != "" && old.RequestId == request) || (ticket != "" && old.Ticket == ticket)) && (old.OwnerAccount != actor.OwnerAccount || old.OwnerRevision != actor.OwnerRevision) {
		return ErrOriginalPlayer
	}
	if request != "" {
		high, low, parseErr := joinRequestParts(request)
		if parseErr != nil {
			return parseErr
		}
		var owner string
		var revision uint64
		read := tx.QueryRowContext(ctx, "SELECT owner_account,owner_revision FROM match_join_intent WHERE player_id=? AND request_high=? AND request_low=? FOR UPDATE", pid, high, low).Scan(&owner, &revision)
		if read == nil && (owner != actor.OwnerAccount || revision != actor.OwnerRevision) {
			return ErrOriginalPlayer
		}
		if read != nil && !errors.Is(read, sql.ErrNoRows) {
			return read
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO match_join_intent(player_id,request_id,ticket_record,cancelled,request_high,request_low,owner_account,owner_revision,state) VALUES(?,?,?,1,?,?,?,?,3) ON DUPLICATE KEY UPDATE cancelled=1,state=3", pid, request, []byte{}, high, low, actor.OwnerAccount, actor.OwnerRevision); e != nil {
			return e
		}
	}
	if (old.State == 1 || (old.State == 0 && old.Ticket != "")) && ((request != "" && old.RequestId == request && (ticket == "" || old.Ticket == ticket)) || (request == "" && old.Ticket == ticket)) {
		if _, e = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND state IN(0,1)", pid); e != nil {
			return e
		}
		if e = deleteTicketRegions(ctx, tx, pid); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func (s *Store) RequestCancelled(ctx context.Context, pid uint64, request string) (bool, error) {
	if request == "" {
		return false, nil
	}
	high, low, e := joinRequestParts(request)
	if e != nil {
		return false, e
	}
	var cancelled bool
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	if e = s.lockActor(ctx, tx, pid); e != nil {
		return false, e
	}
	var owner string
	var revision uint64
	e = tx.QueryRowContext(ctx, "SELECT cancelled,owner_account,owner_revision FROM match_join_intent WHERE player_id=? AND request_high=? AND request_low=?", pid, high, low).Scan(&cancelled, &owner, &revision)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	actor := originalPlayer(ctx, pid)
	if owner != actor.OwnerAccount || revision != actor.OwnerRevision {
		return false, ErrOriginalPlayer
	}
	return cancelled, tx.Commit()
}
func (s *Store) Candidates(ctx context.Context) ([]*pb.MatchAuthorityTicketRecord, error) {
	return s.CandidatesAfter(ctx, 0, 0)
}
func (s *Store) CandidatesAfter(ctx context.Context, afterMs, afterPlayer uint64) ([]*pb.MatchAuthorityTicketRecord, error) {
	r, e := s.DB.QueryContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE state=1 AND expires_ms>CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) AND (enqueued_ms>? OR (enqueued_ms=? AND player_id>?)) ORDER BY enqueued_ms,player_id LIMIT 32", afterMs, afterMs, afterPlayer)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	var out []*pb.MatchAuthorityTicketRecord
	for r.Next() {
		t, e := scanTicket(r)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, r.Err()
}

func (s *Store) Claim(ctx context.Context, f *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.claimTx(ctx, tx, f, group, nil); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) claimTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord, source *matchEntryOriginal) error {
	if err := validatePvpTeamFormation(f, group); err != nil {
		return err
	}
	if e := s.lockOriginalPlayers(ctx, tx, group); e != nil {
		return e
	}
	if e := s.requireMatchEntryTx(ctx, tx, group, source); e != nil {
		return e
	}
	players := make([]uint64, 0, len(group))
	for _, member := range group {
		players = append(players, member.PlayerId)
	}
	if e := s.RequireNoSpectators(ctx, tx, players); e != nil {
		return e
	}
	locked := append([]*pb.MatchAuthorityTicketRecord(nil), group...)
	sort.Slice(locked, func(i, j int) bool { return locked[i].PlayerId < locked[j].PlayerId })
	actual := make(map[uint64]*pb.MatchAuthorityTicketRecord, len(locked))
	for _, want := range locked {
		t, e := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", want.PlayerId))
		if e != nil {
			return e
		}
		actual[want.PlayerId] = t
	}
	n, e := now(ctx, tx)
	if e != nil {
		return e
	}
	allPreferred := true
	if f.Mode == uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM) {
		current := make([]*pb.MatchAuthorityTicketRecord, 0, len(group))
		for _, want := range group {
			current = append(current, actual[want.PlayerId])
		}
		if err := validatePvpTeamFormation(f, current); err != nil {
			return err
		}
	}
	for _, want := range locked {
		t := actual[want.PlayerId]
		p, e := DecodeProfile(t.NetworkProfile)
		if e != nil {
			return e
		}
		rtt, ok := Latencies(p, n, s.Config.HardLatencyMs)[f.PhysicalRegion]
		// 候选扫描之后可能等待行锁；整份探测的有效期也必须在提交点重新检查。
		if t.Ticket != want.Ticket || t.State != 1 || t.NotBeforeMs > n || t.ExpiresMs <= n || !ok || rtt == 0 || !bytes.Equal(t.NetworkProfile, want.NetworkProfile) || t.OwnerAccount != want.OwnerAccount || t.OwnerRevision != want.OwnerRevision || p.OwnerAccount != t.OwnerAccount || p.OwnerRevision != t.OwnerRevision {
			return errors.New("成局成员代次或地域条件已经变化")
		}
		if PreferredRegion(p, n) != f.PhysicalRegion {
			allPreferred = false
		}
	}
	// RequeueOnFailure 只在排队成局上为真；点名成局（切磋、活动、整队直接开战）名单固定，不受下面三条约束。
	if f.RequeueOnFailure {
		current := make([]*pb.MatchAuthorityTicketRecord, 0, len(locked))
		for _, member := range locked {
			current = append(current, actual[member.PlayerId])
		}
		if !s.catalog.Open(pb.MatchMode(f.Mode), f.ConfigId) {
			return ErrTemplateClosed
		}
		// 组里没有陌生人时不必等跨机房阈值：首选机房没有节点就直接放到硬延迟内的机房（D3）。
		if !allPreferred && !singleUnit(current) {
			wait := uint64(s.Config.CrossRegionWaitSeconds) * 1000
			for _, ticket := range current {
				if n < wait || ticket.EnqueuedMs > n-wait {
					return errors.New("跨区须全员原等待达到阈值")
				}
			}
		}
		if _, admitted := s.admitScope(current, n); !admitted {
			return ErrScopeWait
		}
	}
	f.CreatedMs = n
	f.DeadlineMs = s.battleDeadlineMs(n, f.ConfigId)
	f.State = 1
	f.RetryAtMs = n
	sides := map[uint64]uint32{}
	for _, member := range f.Members {
		sides[member.PlayerId] = member.TeamIndex
	}
	f.Members = nil
	for _, t := range group {
		f.Members = append(f.Members, &pb.MatchFormationMember{PlayerId: t.PlayerId, Ticket: t.Ticket, TeamIndex: sides[t.PlayerId]})
	}
	// repeated 消息按 proto2mysql 的同一序列化格式写入，不制造平行成员模型。
	memberValue := &pb.MatchFormationRecord{Members: f.Members}
	mb, e := proto.Marshal(memberValue)
	if e != nil {
		return e
	}
	if f.ActivityContext == nil {
		f.ActivityContext = []byte{}
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO match_formation("+formationColumns+") VALUES("+strings.TrimSuffix(strings.Repeat("?,", 22), ",")+")", f.BattleId, f.OperationId, 1, f.PhysicalRegion, f.TargetNodeId, f.TargetInstanceId, f.TargetEndpoint, f.Mode, f.ConfigId, mb, []byte{}, f.CreatedMs, f.DeadlineMs, f.RetryAtMs, f.ActivityContext, f.RequeueOnFailure, []byte{}, uint32(0), uint32(0), "", uint32(0), false)
	if e != nil {
		return e
	}
	for _, t := range locked {
		if _, e = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=2,formation_id=?,selected_region=? WHERE player_id=? AND ticket=? AND state=1", f.BattleId, f.PhysicalRegion, t.PlayerId, t.Ticket); e != nil {
			return e
		}
		if e = deleteTicketRegions(ctx, tx, t.PlayerId); e != nil {
			return e
		}
	}
	return nil
}

// ReserveGroup 与成局在同一事务提交，预组队不会短暂暴露为可拆散的单人候选。
func (s *Store) ReserveGroup(ctx context.Context, f *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord) error {
	if len(group) == 0 || len(group) > 10 {
		return errors.New("整组人数无效")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = s.reserveGroupTx(ctx, tx, f, group, nil); e != nil {
		return e
	}
	return tx.Commit()
}

func (s *Store) reserveGroupTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord, source *matchEntryOriginal) error {
	if e := s.lockOriginalPlayers(ctx, tx, group); e != nil {
		return e
	}
	if e := s.requireMatchEntryTx(ctx, tx, group, source); e != nil {
		return e
	}
	players := make([]uint64, 0, len(group))
	for _, member := range group {
		players = append(players, member.PlayerId)
	}
	if e := s.RequireNoSpectators(ctx, tx, players); e != nil {
		return e
	}
	n, e := now(ctx, tx)
	if e != nil {
		return e
	}
	ordered := append([]*pb.MatchAuthorityTicketRecord(nil), group...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PlayerId < ordered[j].PlayerId })
	for i, t := range ordered {
		if t.PlayerId == 0 || (i > 0 && ordered[i-1].PlayerId == t.PlayerId) {
			return errors.New("整组身份无效")
		}
		if _, e = tx.ExecContext(ctx, "INSERT IGNORE INTO match_authority_ticket("+ticketColumns+") VALUES(?,'','',0,0,0,0,0,0,0,0,0,0,?,0,0,0,'',0)", t.PlayerId, []byte{}); e != nil {
			return e
		}
		old, e := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", t.PlayerId))
		if e != nil {
			return e
		}
		if TicketOccupied(old) && !(old.State == 0 && old.Ticket == t.Ticket && old.RequestId == t.RequestId && old.OwnerAccount == t.OwnerAccount && old.OwnerRevision == t.OwnerRevision) {
			return errors.New("整组成员已有在途票据")
		}
		var data []byte
		var exp uint64
		if e = tx.QueryRowContext(ctx, "SELECT payload,expires_ms FROM match_network_profile WHERE player_id=?", t.PlayerId).Scan(&data, &exp); e != nil {
			return e
		}
		p, e := DecodeProfile(data)
		if e != nil {
			return e
		}
		if p.OwnerAccount != t.OwnerAccount || p.OwnerRevision != t.OwnerRevision {
			return ErrOriginalPlayer
		}
		if exp <= n {
			return errors.New("整组成员的地域探测已过期")
		}
		if _, ok := Latencies(p, n, s.Config.HardLatencyMs)[f.PhysicalRegion]; !ok {
			return errors.New("整组没有符合硬延迟的共同机房")
		}
		t.NetworkProfile = data
		t.State = 1
		t.EnqueuedMs = n
		t.ExpiresMs = exp
		if _, e = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET ticket=?,request_id=?,mode=?,config_id=?,team_size=?,state=1,enqueued_ms=?,not_before_ms=0,zone_id=?,rating_centi=?,formation_id=0,team_id=?,network_profile=?,expires_ms=?,selected_region=0,owner_account=?,owner_revision=? WHERE player_id=?", t.Ticket, t.RequestId, t.Mode, t.ConfigId, t.TeamSize, n, t.ZoneId, t.RatingCenti, t.TeamId, data, t.ExpiresMs, t.OwnerAccount, t.OwnerRevision, t.PlayerId); e != nil {
			return e
		}
		if e = s.replaceTicketRegions(ctx, tx, t, n); e != nil {
			return e
		}
	}
	if e = s.claimTx(ctx, tx, f, group, source); e != nil {
		return e
	}
	return nil
}

// FindDirect 是同一业务意图的恢复入口。查询缺失只表示当前没有已提交成局，不能授权换请求号重试。
func (s *Store) FindDirect(ctx context.Context, key *pb.MatchDirectIntentRecord) (*pb.MatchFormationRecord, []byte, error) {
	if key == nil {
		return nil, nil, ErrDirectConflict
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, nil, e
	}
	defer tx.Rollback()
	actor := originalPlayer(ctx, key.OwnerId)
	if key.Kind != 2 {
		if e = s.lockActor(ctx, tx, key.OwnerId); e != nil {
			return nil, nil, e
		}
	}
	var id uint64
	var payload []byte
	var cancelled bool
	var owner string
	var revision uint64
	e = tx.QueryRowContext(ctx, "SELECT battle_id,payload,cancelled,owner_account,owner_revision FROM match_direct_intent WHERE kind=? AND owner_id=? AND request_high=? AND request_low=?", key.Kind, key.OwnerId, key.RequestHigh, key.RequestLow).Scan(&id, &payload, &cancelled, &owner, &revision)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if e != nil {
		return nil, nil, e
	}
	if key.Kind != 2 && (owner != actor.OwnerAccount || revision != actor.OwnerRevision) {
		return nil, nil, ErrOriginalPlayer
	}
	if e = tx.Commit(); e != nil {
		return nil, nil, e
	}
	if cancelled {
		return nil, payload, ErrDirectCancelled
	}
	if id == 0 {
		return nil, payload, nil
	}
	f, e := s.Formation(ctx, id)
	return f, payload, e
}

// 拒战和应战竞争相同意图主键；拒战墓碑没有成局，迟到接受不能越过它。
func (s *Store) DeclineDirect(ctx context.Context, key *pb.MatchDirectIntentRecord, payload []byte) (bool, error) {
	if key == nil || key.Kind != 3 || key.OwnerId == 0 || (key.RequestHigh == 0 && key.RequestLow == 0) || len(payload) == 0 {
		return false, errors.New("拒战原意图无效")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	if e = s.lockActor(ctx, tx, key.OwnerId); e != nil {
		return false, e
	}
	actor := originalPlayer(ctx, key.OwnerId)
	if _, e = tx.ExecContext(ctx, "INSERT IGNORE INTO match_direct_intent(kind,owner_id,request_high,request_low,payload,battle_id,cancelled,owner_account,owner_revision) VALUES(?,?,?,?,?,0,0,?,?)", key.Kind, key.OwnerId, key.RequestHigh, key.RequestLow, payload, actor.OwnerAccount, actor.OwnerRevision); e != nil {
		return false, e
	}
	var original []byte
	var battle uint64
	var owner string
	var revision uint64
	if e = tx.QueryRowContext(ctx, "SELECT payload,battle_id,owner_account,owner_revision FROM match_direct_intent WHERE kind=? AND owner_id=? AND request_high=? AND request_low=? FOR UPDATE", key.Kind, key.OwnerId, key.RequestHigh, key.RequestLow).Scan(&original, &battle, &owner, &revision); e != nil {
		return false, e
	}
	if owner != actor.OwnerAccount || revision != actor.OwnerRevision {
		return false, ErrOriginalPlayer
	}
	return battle == 0 && bytes.Equal(original, payload), tx.Commit()
}

func DirectUUIDKey(kind uint32, owner uint64, request string) (*pb.MatchDirectIntentRecord, error) {
	high, low, e := joinRequestParts(request)
	if e != nil {
		return nil, e
	}
	return &pb.MatchDirectIntentRecord{Kind: kind, OwnerId: owner, RequestHigh: high, RequestLow: low}, nil
}

// ReserveDirect 原业务意图、全成员占用和固定机房成局一次提交；超时重试返回原成局，不再发新 operation。
func (s *Store) ReserveDirect(ctx context.Context, key *pb.MatchDirectIntentRecord, payload []byte, f *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord) (*pb.MatchFormationRecord, error) {
	original, err := s.PrepareDirectAdmission(ctx, key, payload, f, group)
	if err != nil {
		return nil, err
	}
	if original.BattleId != 0 {
		return s.Formation(ctx, original.BattleId)
	}
	return s.CompletePreparedDirect(ctx, original)
}
func scanFormation(s scanner) (*pb.MatchFormationRecord, error) {
	f := &pb.MatchFormationRecord{}
	var members, prepared []byte
	e := s.Scan(&f.BattleId, &f.OperationId, &f.State, &f.PhysicalRegion, &f.TargetNodeId, &f.TargetInstanceId, &f.TargetEndpoint, &f.Mode, &f.ConfigId, &members, &f.CreateRequest, &f.CreatedMs, &f.DeadlineMs, &f.RetryAtMs, &f.ActivityContext, &f.RequeueOnFailure, &prepared, &f.OutcomeProtocolVersion, &f.PreflightSupportedVersion, &f.PreflightInstanceId, &f.AdoptedOutcomeProtocolVersion, &f.AdoptedOutcomeObserved)
	if e != nil {
		return nil, e
	}
	m := &pb.MatchFormationRecord{}
	if e = proto.Unmarshal(members, m); e != nil {
		return nil, e
	}
	f.Members = m.Members
	if e = proto.Unmarshal(prepared, m); e != nil {
		return nil, e
	}
	f.Prepared = m.Prepared
	return f, nil
}

// retry_at_ms 同时是工作租约的代次和截止时间，只有当前代次才能提交状态。
// 租约不续期：所有备战/建房准入期限不超过本代次；失联后须到期才能恢复。
var ErrWorkLease = errors.New("成局工作租约已失效或被其他副本持有")

const WorkLeaseMs uint64 = 120000

// ErrAwaitingExpiredOutcome 表示成局只剩到期终局一个出口，不再发工作租约（2026-10-22 修复，
// social-match-battle#0）。判定规则见 awaitsExpiredOutcome。
var ErrAwaitingExpiredOutcome = errors.New("成局已过权威截止，只能由到期终局恢复裁决")

// awaitsExpiredOutcome：已持久 v1 Create 原件且已过权威截止的成局，唯一出口是 RecoverExpiredOutcomes
// 在原 P→formation 锁内争唯一 SQL 终局，而它要求工作租约已经到期。worker 此时能做的只剩查询目标
// Battle 后 Unknown，每次取租约都把 retry_at 再推后 120 秒；多副本巡检或重放的开局请求一直抢在前面时，
// 到期出口迟迟走不到，票据停在 state 2/3。所以过截止后不再发租约，也不进 Pending 候选。
// expiredOutcomeSQL 是同一规则的 SQL 写法，供候选扫描在索引侧过滤，两处必须一起改。
func awaitsExpiredOutcome(f *pb.MatchFormationRecord, nowMs uint64) bool {
	return f.OutcomeProtocolVersion == 1 && len(f.CreateRequest) != 0 && f.DeadlineMs != 0 && f.DeadlineMs <= nowMs
}

const expiredOutcomeSQL = "(outcome_protocol_version=1 AND LENGTH(create_request)>0 AND deadline_ms>0 AND deadline_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED))"

// CountStrandedExpiredFormations 数被上面的门禁挡住、却进不了到期终局恢复的成局（2026-10-22 评审补，
// social-match-battle#0）：state 2/3、满足 expiredOutcomeSQL，但不满足 RecoverExpiredOutcomes 的候选形状
// recoverableOutcomeShapeSQL（能力证据缺失、preflight_instance 与 target 不一致、adopted 观察值不合法等）。
// 它们领不到租约、不进 Pending、也不会被到期恢复选中，票据静默停在 state 2/3。修复前同样没有出口，
// 但每 120 秒的租约轮回至少会出 recover/unknown 计数；门禁之后连这个信号也没了，所以由巡检单独报数。
// 只读，只给报数用，不参与任何释放判定；处置见 cross-zone-matchmaking.md §13.3。
// 形状合格但 Create 正文与成局字段对不上（supportedOutcomeOriginal 失败）的一类不在 SQL 里判，不计入。
func (s *Store) CountStrandedExpiredFormations(ctx context.Context) (uint64, error) {
	var stranded uint64
	err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_formation f WHERE f.state IN(2,3) AND "+expiredOutcomeSQL+" AND NOT ("+recoverableOutcomeShapeSQL+")").Scan(&stranded)
	return stranded, err
}

func (s *Store) AcquireWork(ctx context.Context, id uint64) (*pb.MatchFormationRecord, error) {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	f, e := scanFormation(tx.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=? FOR UPDATE", id))
	if e != nil {
		return nil, e
	}
	n, e := now(ctx, tx)
	if e != nil {
		return nil, e
	}
	if f.State >= 4 || f.RetryAtMs > n {
		return nil, ErrWorkLease
	}
	if awaitsExpiredOutcome(f, n) {
		return nil, ErrAwaitingExpiredOutcome
	}
	if f.State == 1 && f.RetryAtMs != f.CreatedMs {
		f.State = 3
	}
	f.RetryAtMs = n + WorkLeaseMs
	if _, e = tx.ExecContext(ctx, "UPDATE match_formation SET retry_at_ms=?,state=? WHERE battle_id=?", f.RetryAtMs, f.State, id); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return f, nil
}
func checkWork(ctx context.Context, tx *sql.Tx, f, work *pb.MatchFormationRecord) error {
	n, e := now(ctx, tx)
	if e != nil {
		return e
	}
	if work == nil || work.BattleId != f.BattleId || work.RetryAtMs == 0 || f.RetryAtMs != work.RetryAtMs || f.RetryAtMs <= n || f.State >= 4 {
		return ErrWorkLease
	}
	return nil
}
func (s *Store) CheckWork(ctx context.Context, work *pb.MatchFormationRecord) error {
	if work == nil {
		return ErrWorkLease
	}
	var active bool
	e := s.DB.QueryRowContext(ctx, "SELECT state IN(1,2,3) AND retry_at_ms=? AND retry_at_ms>CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) FROM match_formation WHERE battle_id=?", work.RetryAtMs, work.BattleId).Scan(&active)
	if e != nil {
		return e
	}
	if !active {
		return ErrWorkLease
	}
	return nil
}

// CreateIntentPersisted 在本代工作租约仍有效时回答原 operation 是否已持久 Create 原件；租约失效回 ErrWorkLease。
// 返回 false 是"原 operation 从未、也不会再向任何 Battle 发出 Create"的正面证据：gather 只在
// SaveSupportedIntent 于同一租约提交成功、且发送前 CheckWork 仍有效之后才发 Create，而租约被接管后
// 旧 worker 的 SaveIntent / CheckWork 都因 retry_at 不符被拒。持租约者本身不会与这次读取并发写原件。
// 调用方据此不必等目标 Battle 应答即可按发房前失败收尾（2026-10-22 修复，social-match-battle#0）。
func (s *Store) CreateIntentPersisted(ctx context.Context, work *pb.MatchFormationRecord) (bool, error) {
	if work == nil {
		return false, ErrWorkLease
	}
	var persisted bool
	e := s.DB.QueryRowContext(ctx, "SELECT LENGTH(create_request)>0 FROM match_formation WHERE battle_id=? AND state IN(1,2,3) AND retry_at_ms=? AND retry_at_ms>CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)", work.BattleId, work.RetryAtMs).Scan(&persisted)
	if errors.Is(e, sql.ErrNoRows) {
		return false, ErrWorkLease
	}
	return persisted, e
}

// 先持久备战时精确端点，恢复取消不再重新读取可能已经迁移的玩家位置。
func (s *Store) Prepared(ctx context.Context, work *pb.MatchFormationRecord, pid uint64, endpoint string) error {
	id := work.GetBattleId()
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	f, e := scanFormation(tx.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=? FOR UPDATE", id))
	if e != nil {
		return e
	}
	if e = checkWork(ctx, tx, f, work); e != nil {
		return e
	}
	if f.State != 1 {
		return errors.New("备战所属成局已经进入恢复或终态")
	}
	for _, p := range f.Prepared {
		if p.PlayerId == pid {
			return nil
		}
	}
	f.Prepared = append(f.Prepared, &pb.MatchPreparedEndpoint{PlayerId: pid, Endpoint: endpoint})
	b, e := proto.Marshal(&pb.MatchFormationRecord{Prepared: f.Prepared})
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE match_formation SET prepared=? WHERE battle_id=? AND state=1", b, id); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Formation(ctx context.Context, id uint64) (*pb.MatchFormationRecord, error) {
	return scanFormation(s.DB.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=?", id))
}
func (s *Store) Pending(ctx context.Context) ([]*pb.MatchFormationRecord, error) {
	// 过截止的 v1 原件只归到期终局恢复；留在这里既领不到租约，还会占住前 32 个名额挡住后面的成局。
	r, e := s.DB.QueryContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE state IN(1,2,3) AND retry_at_ms<=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) AND NOT "+expiredOutcomeSQL+" ORDER BY retry_at_ms LIMIT 32")
	if e != nil {
		return nil, e
	}
	defer r.Close()
	var out []*pb.MatchFormationRecord
	for r.Next() {
		f, e := scanFormation(r)
		if e != nil {
			return nil, e
		}
		out = append(out, f)
	}
	return out, r.Err()
}
func (s *Store) SaveIntent(ctx context.Context, work *pb.MatchFormationRecord, request []byte) ([]byte, error) {
	return s.saveIntent(ctx, work, request, nil)
}

// 能力证明和完整v1正文先于任何Create提交，adopted必须另取原创建回执。
func (s *Store) SaveSupportedIntent(ctx context.Context, work *pb.MatchFormationRecord, request []byte, capability *battlepb.QueryBattleCreationResponse) ([]byte, error) {
	if work == nil || capability == nil || capability.TargetInstanceId != work.TargetInstanceId || capability.SupportedOutcomeProtocolVersion != 1 || capability.State != battlepb.BattleCreationState_BATTLE_CREATION_ABSENT {
		return nil, errors.New("原Battle实例未证明支持终局协议1或已有未知创建")
	}
	r := &battlepb.CreateBattleRequest{}
	if proto.Unmarshal(request, r) != nil || r.BattleId != work.BattleId || r.PlacementOperationId != work.OperationId || r.TargetInstanceId != work.TargetInstanceId || r.OutcomeProtocolVersion != 1 || r.DeadlineMs == 0 {
		return nil, errors.New("终局协议创建原件不完整")
	}
	return s.saveIntent(ctx, work, request, capability)
}
func (s *Store) saveIntent(ctx context.Context, work *pb.MatchFormationRecord, request []byte, capability *battlepb.QueryBattleCreationResponse) ([]byte, error) {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	f, e := scanFormation(tx.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=? FOR UPDATE", work.GetBattleId()))
	if e != nil {
		return nil, e
	}
	if e = checkWork(ctx, tx, f, work); e != nil {
		return nil, e
	}
	if f.State == 1 {
		version, supported, instance := uint32(0), uint32(0), ""
		if capability != nil {
			version, supported, instance = 1, 1, capability.TargetInstanceId
		}
		if _, e = tx.ExecContext(ctx, "UPDATE match_formation SET state=2,create_request=?,outcome_protocol_version=?,preflight_supported_version=?,preflight_instance_id=? WHERE battle_id=?", request, version, supported, instance, f.BattleId); e != nil {
			return nil, e
		}
		f.CreateRequest = request
	} else if !bytes.Equal(f.CreateRequest, request) || (capability != nil && (f.OutcomeProtocolVersion != 1 || f.PreflightSupportedVersion != 1 || f.PreflightInstanceId != capability.TargetInstanceId)) {
		return nil, errors.New("持久创建原件或能力证据冲突")
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return f.CreateRequest, nil
}

func (s *Store) RecordBattleAdopted(ctx context.Context, work *pb.MatchFormationRecord, instance string, version uint32) error {
	if work == nil || instance != work.TargetInstanceId {
		return errors.New("Battle实际采用版本原件不符")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	f, err := scanFormation(tx.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=? FOR UPDATE", work.BattleId))
	if err != nil {
		return err
	}
	// 回执可能迟于工作租约；事实属于固定原op/UUID，不能因换worker丢掉明确adopted0。
	if f.OperationId != work.OperationId || f.TargetInstanceId != work.TargetInstanceId {
		return errors.New("采用版本回执原操作不符")
	}
	if f.OutcomeProtocolVersion != 1 || f.PreflightSupportedVersion != 1 || f.PreflightInstanceId != instance || f.TargetInstanceId != instance || (f.AdoptedOutcomeObserved && f.AdoptedOutcomeProtocolVersion != version) {
		return errors.New("缺原发起前能力证据或实际采用回执冲突")
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_formation SET adopted_outcome_protocol_version=?,adopted_outcome_observed=1 WHERE battle_id=?", version, work.BattleId); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) Unknown(ctx context.Context, work *pb.MatchFormationRecord) error {
	if work == nil {
		return ErrWorkLease
	}
	r, e := s.DB.ExecContext(ctx, "UPDATE match_formation SET state=3 WHERE battle_id=? AND retry_at_ms=? AND retry_at_ms>CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) AND state IN(1,2,3)", work.BattleId, work.RetryAtMs)
	if e != nil {
		return e
	}
	n, e := r.RowsAffected()
	if e == nil && n == 0 {
		return s.CheckWork(ctx, work) // 已是 UNKNOWN 的同代重放仍然幂等。
	}
	return e
}
func (s *Store) Finish(ctx context.Context, work *pb.MatchFormationRecord, success bool) error {
	return s.finish(ctx, work, success, nil)
}

// FinishFailed 是发房前失败的收尾，culprits 是确定由自己导致失败的成员（掉线、不在场景、配表不一致）。
// 排队成局失败时，单排肇事者的票直接终止、不回队，其余成员保留原入队时刻回队；
// 肇事者属于整队时整队照常回队，由队伍复核（CheckParty）决定是否撤单，绝不拆队。
func (s *Store) FinishFailed(ctx context.Context, work *pb.MatchFormationRecord, culprits []uint64) error {
	return s.finish(ctx, work, false, culprits)
}

func (s *Store) finish(ctx context.Context, work *pb.MatchFormationRecord, success bool, culprits []uint64) error {
	id := work.GetBattleId()
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	f, e := scanFormation(tx.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=? FOR UPDATE", id))
	if e != nil {
		return e
	}
	if e = checkWork(ctx, tx, f, work); e != nil {
		return e
	}
	// 发房前失败可沿原合同释放；一旦持久Create，运行时ABORTED/连接失败都不等于
	// 本权威已提交ABORT。保留原票给唯一SQL终局恢复，绝不能先state5或换新轮票。
	if !success && len(f.CreateRequest) != 0 {
		return errors.New("已有持久创建原件，失败收尾必须等待唯一SQL终局")
	}
	n, e := now(ctx, tx)
	if e != nil {
		return e
	}
	state, ticketState := uint32(5), uint32(4)
	if success {
		state = 4
		ticketState = 3
	}
	if !success && f.RequeueOnFailure {
		ticketState = 1
	}
	if _, e = tx.ExecContext(ctx, "UPDATE match_formation SET state=? WHERE battle_id=?", state, id); e != nil {
		return e
	}
	culprit := make(map[uint64]bool, len(culprits))
	for _, player := range culprits {
		culprit[player] = true
	}
	for _, m := range f.Members {
		memberState := ticketState
		formationID, region := id, f.PhysicalRegion
		var notBefore uint64
		if memberState == 1 && culprit[m.PlayerId] {
			// 只终止单排票；整队成员的票带 team_id，下面的条件更新不会命中它，整队照常回队。
			r, err := tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4,formation_id=?,selected_region=?,not_before_ms=0 WHERE player_id=? AND ticket=? AND formation_id=? AND team_id=0", id, region, m.PlayerId, m.Ticket, id)
			if err != nil {
				return err
			}
			if terminated, err := r.RowsAffected(); err != nil {
				return err
			} else if terminated == 1 {
				continue
			}
		}
		if memberState == 1 {
			formationID = 0
			region = 0
			notBefore = n + 1000
		}
		if _, e = tx.ExecContext(ctx, "UPDATE match_authority_ticket SET state=?,formation_id=?,selected_region=?,not_before_ms=? WHERE player_id=? AND ticket=? AND formation_id=?", memberState, formationID, region, notBefore, m.PlayerId, m.Ticket, id); e != nil {
			return e
		}
		current, readErr := scanTicket(tx.QueryRowContext(ctx, "SELECT "+ticketColumns+" FROM match_authority_ticket WHERE player_id=? FOR UPDATE", m.PlayerId))
		if readErr != nil {
			return readErr
		}
		if current.Ticket != m.Ticket {
			continue
		}
		if e = s.replaceTicketRegions(ctx, tx, current, n); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func (s *Store) ReleaseReady(ctx context.Context, pid uint64, ticket string, id uint64) error {
	_, e := s.DB.ExecContext(ctx, "UPDATE match_authority_ticket SET state=4 WHERE player_id=? AND ticket=? AND formation_id=? AND state=3", pid, ticket, id)
	return e
}
