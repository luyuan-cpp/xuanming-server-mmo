package data

// 生产大厅唯一权威：完整角色P升序→G→真实大厅→占位。所有参与者原owner一次捕获，
// 私有同意与状态同事务；Redis仅用于巡检租约。终局原行和slot墓碑永久保留。
import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"time"

	"guild/internal/constants"
	pb "proto/guild"
	"shared/accountdirectory"
	"shared/roleidentity"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

const trialLobbyColumns = "lobby_id,guild_id,activity_id,initiator_player_id,state,view_proto,original_proto,consent_proto,expires_ms,launch_deadline_ms,battle_id,created_ms,updated_ms"

func NewSQLTrialLobbyRepo(rdb *redis.Client, guilds *GuildRepo, mint func(context.Context) (uint64, error)) (*TrialLobbyRepo, error) {
	if rdb == nil || guilds == nil || guilds.roleAuthority == "" || guilds.matchAuthority == "" || mint == nil {
		return nil, errors.New("历练SQL大厅权威依赖缺失")
	}
	return &TrialLobbyRepo{rdb: rdb, guilds: guilds, mint: mint}, nil
}
func (r *TrialLobbyRepo) SQLAuthorityEnabled() bool { return r != nil && r.guilds != nil }

func scanTrialLobby(row rowScanner) (*pb.GuildTrialLobbyRecord, error) {
	v := &pb.GuildTrialLobbyRecord{}
	err := row.Scan(&v.LobbyId, &v.GuildId, &v.ActivityId, &v.InitiatorPlayerId, &v.State, &v.ViewProto, &v.OriginalProto, &v.ConsentProto, &v.ExpiresMs, &v.LaunchDeadlineMs, &v.BattleId, &v.CreatedMs, &v.UpdatedMs)
	return v, err
}
func lobbyPlayers(original *pb.GuildTrialLobbyOriginal) []uint64 {
	ids := make([]uint64, 0, len(original.Roster))
	for _, p := range original.Roster {
		ids = append(ids, p.PlayerId)
	}
	return ids
}
func lobbyView(row *pb.GuildTrialLobbyRecord) (*TrialLobby, *pb.GuildTrialLobbyOriginal, *pb.GuildTrialLobbyConsents, error) {
	o, v, c := &pb.GuildTrialLobbyOriginal{}, &pb.GuildTrialLobbyView{}, &pb.GuildTrialLobbyConsents{}
	if row == nil || len(row.OriginalProto) == 0 || len(row.OriginalProto) > 65536 || len(row.ViewProto) > 65536 || len(row.ConsentProto) > 65536 ||
		proto.Unmarshal(row.OriginalProto, o) != nil || proto.Unmarshal(row.ViewProto, v) != nil || proto.Unmarshal(row.ConsentProto, c) != nil ||
		len(o.ProtoReflect().GetUnknown()) != 0 || len(v.ProtoReflect().GetUnknown()) != 0 || len(c.ProtoReflect().GetUnknown()) != 0 ||
		row.LobbyId == 0 || row.GuildId == 0 || row.ActivityId == 0 || row.CreatedMs == 0 || row.ExpiresMs <= row.CreatedMs || row.State < 1 || row.State > 4 ||
		o.LobbyId != row.LobbyId || o.GuildId != row.GuildId || o.ActivityId != row.ActivityId || o.InitiatorPlayerId != row.InitiatorPlayerId || o.CreatedMs != row.CreatedMs || o.ExpiresMs != row.ExpiresMs ||
		v.LobbyId != row.LobbyId || v.InitiatorPlayerId != row.InitiatorPlayerId || uint32(v.State) != row.State || v.BattleId != row.BattleId || v.ExpireAtMs != row.ExpiresMs || len(o.Roster) < 2 || len(o.Roster) > maxTrialLobbyRoster || len(v.MemberPlayerIds) != len(o.Roster) {
		return nil, nil, nil, ErrWriteConflict
	}
	roles := map[uint64]*pb.GuildTrialRoleIdentity{}
	var previous uint64
	for _, p := range o.Roster {
		if p.GetPlayerId() <= previous || !accountdirectory.ValidAccount(p.Account) || p.OwnerRevision == 0 || len(p.ProtoReflect().GetUnknown()) != 0 {
			return nil, nil, nil, ErrWriteConflict
		}
		previous = p.PlayerId
		roles[p.PlayerId] = p
	}
	if roles[o.InitiatorPlayerId] == nil {
		return nil, nil, nil, ErrWriteConflict
	}
	seen := map[uint64]bool{}
	for _, p := range v.MemberPlayerIds {
		if roles[p] == nil || seen[p] {
			return nil, nil, nil, ErrWriteConflict
		}
		seen[p] = true
	}
	consented := map[uint64]bool{}
	previous = 0
	for _, consent := range c.Consents {
		p := consent.GetIdentity()
		if consent.GetLobbyId() != row.LobbyId || p.GetPlayerId() <= previous || len(consent.ProtoReflect().GetUnknown()) != 0 || !proto.Equal(p, roles[p.GetPlayerId()]) {
			return nil, nil, nil, ErrWriteConflict
		}
		previous = p.PlayerId
		consented[p.PlayerId] = true
	}
	if !consented[o.InitiatorPlayerId] || len(v.AcceptedPlayerIds) != len(consented) {
		return nil, nil, nil, ErrWriteConflict
	}
	seen = map[uint64]bool{}
	var mask uint32
	for _, p := range v.AcceptedPlayerIds {
		if !consented[p] || seen[p] {
			return nil, nil, nil, ErrWriteConflict
		}
		seen[p] = true
	}
	for i, p := range v.MemberPlayerIds {
		if consented[p] {
			mask |= 1 << uint(i)
		}
	}
	if (row.State == 2 || row.State == 3) && (len(consented) != len(roles) || row.LaunchDeadlineMs == 0) || row.State == 3 && row.BattleId == 0 {
		return nil, nil, nil, ErrWriteConflict
	}
	return &TrialLobby{durable: true, LobbyID: row.LobbyId, GuildID: row.GuildId, ActivityID: row.ActivityId, InitiatorID: row.InitiatorPlayerId, Roster: v.MemberPlayerIds, AcceptedMask: mask, State: v.State, ExpireAtMs: row.ExpiresMs, LaunchDeadlineMs: row.LaunchDeadlineMs, BattleID: row.BattleId, CreatedMs: row.CreatedMs, EndTipID: v.EndTipId, EndParams: v.EndParameters}, o, c, nil
}
func trialSQLNow(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var now uint64
	err := tx.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)").Scan(&now)
	return now, err
}
func writeLobby(ctx context.Context, tx *sql.Tx, row *pb.GuildTrialLobbyRecord, v *pb.GuildTrialLobbyView, c *pb.GuildTrialLobbyConsents, now uint64) error {
	var err error
	row.ViewProto, err = proto.MarshalOptions{Deterministic: true}.Marshal(v)
	if err != nil {
		return err
	}
	row.ConsentProto, err = proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE guild_trial_lobby SET state=?,view_proto=?,consent_proto=?,launch_deadline_ms=?,battle_id=?,updated_ms=? WHERE lobby_id=?", uint32(v.State), row.ViewProto, row.ConsentProto, row.LaunchDeadlineMs, v.BattleId, now, row.LobbyId)
	return err
}
func currentLobbyView(row *pb.GuildTrialLobbyRecord) *pb.GuildTrialLobbyView {
	v := &pb.GuildTrialLobbyView{}
	_ = proto.Unmarshal(row.ViewProto, v)
	return v
}

func (r *TrialLobbyRepo) createSQL(ctx context.Context, in TrialLobbyCreate) (out TrialLobbyCreateResult, err error) {
	if err = in.validate(); err != nil {
		return
	}
	actor, ok := roleidentity.Original(ctx)
	if !ok || actor.Role.PlayerId != in.Roster[0] {
		return out, ErrWriteConflict
	}
	id, err := r.mint(ctx)
	if err != nil {
		return out, err
	}
	if id == 0 {
		return out, ErrWriteConflict
	}
	ids := append([]uint64(nil), in.Roster...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	err = r.guilds.inTx(withRoleTargets(ctx, ids...), opTrialRegister, func(ctx context.Context, tx *sql.Tx) error {
		if _, e := lockTrialGuild(ctx, tx, in.GuildID); e != nil {
			return e
		}
		now, e := trialSQLNow(ctx, tx)
		if e != nil {
			return e
		}
		o := &pb.GuildTrialLobbyOriginal{LobbyId: id, GuildId: in.GuildID, ActivityId: in.ActivityID, InitiatorPlayerId: actor.Role.PlayerId, CreatedMs: now, ExpiresMs: now + uint64(in.TTL.Milliseconds())}
		for _, p := range ids {
			var account string
			var revision, listing, member uint64
			if e = tx.QueryRowContext(ctx, "SELECT account,owner_revision,trade_listing_id FROM `"+r.guilds.roleAuthority+"`.account_role WHERE player_id=? FOR UPDATE", p).Scan(&account, &revision, &listing); e != nil {
				return e
			}
			if !accountdirectory.ValidAccount(account) || revision == 0 || listing != 0 {
				return ErrWriteConflict
			}
			if e = tx.QueryRowContext(ctx, "SELECT player_id FROM guild_member WHERE guild_id=? AND player_id=? FOR UPDATE", in.GuildID, p).Scan(&member); e != nil {
				return e
			}
			o.Roster = append(o.Roster, &pb.GuildTrialRoleIdentity{PlayerId: p, Account: account, OwnerRevision: revision})
			var old, cd uint64
			e = tx.QueryRowContext(ctx, "SELECT lobby_id,cooldown_until_ms FROM guild_trial_lobby_slot WHERE player_id=? FOR UPDATE", p).Scan(&old, &cd)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return e
			}
			if p == o.InitiatorPlayerId && cd > now {
				out = TrialLobbyCreateResult{Status: TrialLobbyCooldown, CooldownRemaining: time.Duration(cd-now) * time.Millisecond}
				return nil
			}
			if old != 0 {
				var state uint32
				e = tx.QueryRowContext(ctx, "SELECT state FROM guild_trial_lobby WHERE lobby_id=?", old).Scan(&state)
				if e != nil {
					return e
				}
				if state != 4 {
					out = TrialLobbyCreateResult{Status: TrialLobbyMemberBusy, BusyPlayerID: p}
					return nil
				}
			}
		}
		if e = r.guilds.requireTrialEntryFree(ctx, tx, ids, 0); e != nil {
			return e
		}
		v := &pb.GuildTrialLobbyView{LobbyId: id, InitiatorPlayerId: o.InitiatorPlayerId, MemberPlayerIds: []uint64{o.InitiatorPlayerId}, AcceptedPlayerIds: []uint64{o.InitiatorPlayerId}, State: pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_PENDING, ExpireAtMs: o.ExpiresMs}
		for _, p := range ids {
			if p != o.InitiatorPlayerId {
				v.MemberPlayerIds = append(v.MemberPlayerIds, p)
			}
		}
		var originalRole *pb.GuildTrialRoleIdentity
		for _, p := range o.Roster {
			if p.PlayerId == o.InitiatorPlayerId {
				originalRole = p
			}
		}
		if originalRole.Account != actor.Account || originalRole.OwnerRevision != actor.OwnerRevision {
			return ErrWriteConflict
		}
		c := &pb.GuildTrialLobbyConsents{Consents: []*pb.GuildTrialLobbyConsent{{LobbyId: id, Identity: originalRole}}}
		raw, e := proto.MarshalOptions{Deterministic: true}.Marshal(o)
		if e != nil {
			return e
		}
		view, e := proto.MarshalOptions{Deterministic: true}.Marshal(v)
		if e != nil {
			return e
		}
		consent, e := proto.MarshalOptions{Deterministic: true}.Marshal(c)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO guild_trial_lobby("+trialLobbyColumns+") VALUES(?,?,?,?,1,?,?,?,?,0,0,?,?)", id, o.GuildId, o.ActivityId, o.InitiatorPlayerId, view, raw, consent, o.ExpiresMs, now, now)
		if e != nil {
			return e
		}
		for _, p := range ids {
			cd := uint64(0)
			if p == o.InitiatorPlayerId {
				cd = now + uint64(in.Cooldown.Milliseconds())
			}
			_, e = tx.ExecContext(ctx, "INSERT INTO guild_trial_lobby_slot(player_id,lobby_id,cooldown_until_ms) VALUES(?,?,?) ON DUPLICATE KEY UPDATE lobby_id=VALUES(lobby_id),cooldown_until_ms=GREATEST(cooldown_until_ms,VALUES(cooldown_until_ms))", p, id, cd)
			if e != nil {
				return e
			}
		}
		row := &pb.GuildTrialLobbyRecord{LobbyId: id, GuildId: o.GuildId, ActivityId: o.ActivityId, InitiatorPlayerId: o.InitiatorPlayerId, State: 1, ViewProto: view, OriginalProto: raw, ConsentProto: consent, ExpiresMs: o.ExpiresMs, CreatedMs: now}
		lb, _, _, e := lobbyView(row)
		out = TrialLobbyCreateResult{Status: TrialLobbyCreated, Lobby: lb}
		return e
	})
	return
}

func (r *TrialLobbyRepo) loadSQL(ctx context.Context, id uint64) (*TrialLobby, error) {
	row, e := scanTrialLobby(r.guilds.db.QueryRowContext(ctx, "SELECT "+trialLobbyColumns+" FROM guild_trial_lobby WHERE lobby_id=?", id))
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	lb, _, _, e := lobbyView(row)
	return lb, e
}
func (r *TrialLobbyRepo) loadSQLConsents(ctx context.Context, lb *TrialLobby) ([]*pb.GuildTrialRoleIdentity, error) {
	if lb == nil {
		return nil, ErrWriteConflict
	}
	row, e := scanTrialLobby(r.guilds.db.QueryRowContext(ctx, "SELECT "+trialLobbyColumns+" FROM guild_trial_lobby WHERE lobby_id=?", lb.LobbyID))
	if e != nil {
		return nil, e
	}
	_, o, c, e := lobbyView(row)
	if e != nil {
		return nil, e
	}
	if len(c.Consents) != len(o.Roster) || row.State != 2 {
		return nil, ErrWriteConflict
	}
	return o.Roster, nil
}

// 预读仅确定锁全集，进入事务后必须逐字重核邀请原件；不得把新owner填进旧consent。
func (r *TrialLobbyRepo) withSQLLobby(ctx context.Context, id uint64, fn func(context.Context, *sql.Tx, *pb.GuildTrialLobbyRecord, *pb.GuildTrialLobbyOriginal, *pb.GuildTrialLobbyConsents) error) error {
	pre, e := scanTrialLobby(r.guilds.db.QueryRowContext(ctx, "SELECT "+trialLobbyColumns+" FROM guild_trial_lobby WHERE lobby_id=?", id))
	if e != nil {
		return e
	}
	_, original, _, e := lobbyView(pre)
	if e != nil {
		return e
	}
	return r.guilds.inTx(withRoleTargets(ctx, lobbyPlayers(original)...), opTrialMark, func(ctx context.Context, tx *sql.Tx) error {
		if _, e = lockTrialGuild(ctx, tx, original.GuildId); e != nil && !errors.Is(e, ErrGuildGone) {
			return e
		}
		row, e := scanTrialLobby(tx.QueryRowContext(ctx, "SELECT "+trialLobbyColumns+" FROM guild_trial_lobby WHERE lobby_id=? FOR UPDATE", id))
		if e != nil {
			return e
		}
		_, o, c, e := lobbyView(row)
		if e != nil {
			return e
		}
		if !bytes.Equal(row.OriginalProto, pre.OriginalProto) {
			return ErrWriteConflict
		}
		return fn(ctx, tx, row, o, c)
	})
}
func (r *GuildRepo) verifyLobbyOwners(ctx context.Context, tx *sql.Tx, o *pb.GuildTrialLobbyOriginal) error {
	for _, p := range o.Roster {
		var account string
		var revision, listing uint64
		if e := tx.QueryRowContext(ctx, "SELECT account,owner_revision,trade_listing_id FROM `"+r.roleAuthority+"`.account_role WHERE player_id=? FOR UPDATE", p.PlayerId).Scan(&account, &revision, &listing); e != nil {
			return e
		}
		if account != p.Account || revision != p.OwnerRevision || listing != 0 {
			return ErrWriteConflict
		}
	}
	return nil
}
func (r *TrialLobbyRepo) respondSQL(ctx context.Context, id, player uint64, accept bool, window time.Duration) (out TrialLobbyRespondResult, err error) {
	actor, ok := roleidentity.Original(ctx)
	if !ok || actor.Role.PlayerId != player || id == 0 || window <= 0 {
		return out, ErrWriteConflict
	}
	err = r.withSQLLobby(ctx, id, func(ctx context.Context, tx *sql.Tx, row *pb.GuildTrialLobbyRecord, o *pb.GuildTrialLobbyOriginal, c *pb.GuildTrialLobbyConsents) error {
		if err := r.guilds.verifyLobbyOwners(ctx, tx, o); err != nil {
			return err
		}
		if err := lockActiveLobbySlots(ctx, tx, row, o); err != nil {
			return err
		}
		var role *pb.GuildTrialRoleIdentity
		for _, p := range o.Roster {
			if p.PlayerId == player {
				role = p
			}
		}
		if role == nil || role.Account != actor.Account || role.OwnerRevision != actor.OwnerRevision {
			return ErrWriteConflict
		}
		mine := false
		for _, v := range c.Consents {
			if v.Identity.PlayerId == player {
				mine = true
			}
		}
		if row.State != 1 {
			out = TrialLobbyRespondResult{Status: TrialLobbyRespondNotPending, AlreadyAccepted: mine}
			return nil
		}
		now, e := trialSQLNow(ctx, tx)
		if e != nil {
			return e
		}
		if now >= row.ExpiresMs {
			if e = endPendingLobby(ctx, tx, row, c, constants.ErrTrialInviteExpired, nil, now); e != nil {
				return e
			}
			out.Status = TrialLobbyRespondGone
			return nil
		}
		if !accept {
			if e = endPendingLobby(ctx, tx, row, c, constants.ErrTrialInviteDeclined, []string{strconv.FormatUint(player, 10)}, now); e != nil {
				return e
			}
			out.Status = TrialLobbyRespondDeclined
			return nil
		}
		if mine {
			out = TrialLobbyRespondResult{Status: TrialLobbyRespondNotPending, AlreadyAccepted: true}
			return nil
		}
		// 清理旧邀请允许G已消失；追加同意仍须真实帮会存在，不能在解散后产生新授权。
		if _, e = lockTrialGuild(ctx, tx, o.GuildId); e != nil {
			return e
		}
		if e = r.guilds.requireTrialEntryFree(ctx, tx, lobbyPlayers(o), id); e != nil {
			return e
		}
		c.Consents = append(c.Consents, &pb.GuildTrialLobbyConsent{LobbyId: id, Identity: role})
		sort.Slice(c.Consents, func(i, j int) bool { return c.Consents[i].Identity.PlayerId < c.Consents[j].Identity.PlayerId })
		v := currentLobbyView(row)
		v.AcceptedPlayerIds = nil
		for _, p := range c.Consents {
			v.AcceptedPlayerIds = append(v.AcceptedPlayerIds, p.Identity.PlayerId)
		}
		out.Status = TrialLobbyRespondAccepted
		if len(c.Consents) == len(o.Roster) {
			v.State = pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING
			row.LaunchDeadlineMs = now + uint64(window.Milliseconds())
			out.Status = TrialLobbyRespondLaunch
		}
		return writeLobby(ctx, tx, row, v, c, now)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return TrialLobbyRespondResult{Status: TrialLobbyRespondGone}, nil
	}
	return
}
func clearLobbySlots(ctx context.Context, tx *sql.Tx, row *pb.GuildTrialLobbyRecord) error {
	_, o, _, e := lobbyView(row)
	if e != nil {
		return e
	}
	for _, p := range o.Roster {
		var id uint64
		if e = tx.QueryRowContext(ctx, "SELECT lobby_id FROM guild_trial_lobby_slot WHERE player_id=? FOR UPDATE", p.PlayerId).Scan(&id); e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			return e
		}
		if id != row.LobbyId {
			continue // 仅取消旧原件；永远不清当前新大厅的slot，也不向旧请求补当前owner。
		}
		if _, e = tx.ExecContext(ctx, "UPDATE guild_trial_lobby_slot SET lobby_id=0 WHERE player_id=? AND lobby_id=?", p.PlayerId, row.LobbyId); e != nil {
			return e
		}
	}
	return nil
}

func lockActiveLobbySlots(ctx context.Context, tx *sql.Tx, row *pb.GuildTrialLobbyRecord, o *pb.GuildTrialLobbyOriginal) error {
	if row.State == 4 {
		return nil
	}
	for _, p := range o.Roster {
		var id uint64
		if err := tx.QueryRowContext(ctx, "SELECT lobby_id FROM guild_trial_lobby_slot WHERE player_id=? FOR UPDATE", p.PlayerId).Scan(&id); err != nil {
			return err
		}
		if id != row.LobbyId {
			return ErrWriteConflict
		}
	}
	return nil
}
func endPendingLobby(ctx context.Context, tx *sql.Tx, row *pb.GuildTrialLobbyRecord, c *pb.GuildTrialLobbyConsents, tip uint32, params []string, now uint64) error {
	var request uint64
	e := tx.QueryRowContext(ctx, "SELECT request_id FROM guild_trial_launch WHERE request_id=?", row.LobbyId).Scan(&request)
	if !errors.Is(e, sql.ErrNoRows) {
		if e != nil {
			return e
		}
		return ErrWriteConflict
	}
	if row.State != 1 && row.State != 2 {
		return ErrWriteConflict
	}
	if e = clearLobbySlots(ctx, tx, row); e != nil {
		return e
	}
	v := currentLobbyView(row)
	v.State = pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED
	v.EndTipId = tip
	v.EndParameters = params
	return writeLobby(ctx, tx, row, v, c, now)
}
func (r *TrialLobbyRepo) finishSQL(ctx context.Context, id uint64, from, to pb.GuildTrialLobbyState, battle uint64, tip uint32, params []string) (moved bool, err error) {
	err = r.withSQLLobby(ctx, id, func(ctx context.Context, tx *sql.Tx, row *pb.GuildTrialLobbyRecord, o *pb.GuildTrialLobbyOriginal, c *pb.GuildTrialLobbyConsents) error {
		if row.State != uint32(from) {
			return nil
		}
		now, e := trialSQLNow(ctx, tx)
		if e != nil {
			return e
		}
		if to == pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED {
			if e = endPendingLobby(ctx, tx, row, c, tip, params, now); e != nil {
				return e
			}
			moved = true
			return nil
		}
		if from != pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHING || to != pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_LAUNCHED || battle == 0 {
			return ErrWriteConflict
		}
		if _, e = lockTrialGuild(ctx, tx, o.GuildId); e != nil {
			return e
		}
		if e = r.guilds.verifyLobbyOwners(ctx, tx, o); e != nil {
			return e
		}
		if e = lockActiveLobbySlots(ctx, tx, row, o); e != nil {
			return e
		}
		launch, e := scanTrialLaunch(tx.QueryRowContext(ctx, "SELECT "+trialLaunchColumns+" FROM guild_trial_launch WHERE request_id=? FOR UPDATE", id))
		if e != nil {
			return e
		}
		original, e := decodeTrialLaunch(launch)
		if e != nil {
			return e
		}
		if launch.State != 2 || launch.BattleId != battle || !sameLobbyLaunch(o, original) {
			return ErrWriteConflict
		}
		v := currentLobbyView(row)
		v.State = to
		v.BattleId = battle
		moved = true
		return writeLobby(ctx, tx, row, v, c, now)
	})
	return
}
func sameLobbyLaunch(o *pb.GuildTrialLobbyOriginal, l *pb.GuildTrialLaunchOriginal) bool {
	if o.LobbyId != l.RequestId || o.GuildId != l.GuildId || o.ActivityId != l.ActivityId || o.InitiatorPlayerId != l.InitiatorPlayerId || len(o.Roster) != len(l.Roster) {
		return false
	}
	for i, p := range o.Roster {
		if !proto.Equal(p, l.Roster[i]) {
			return false
		}
	}
	return true
}
func (r *TrialLobbyRepo) loadPlayerSQL(ctx context.Context, p uint64) (*TrialLobby, error) {
	var id uint64
	e := r.guilds.db.QueryRowContext(ctx, "SELECT lobby_id FROM guild_trial_lobby_slot WHERE player_id=?", p).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) || e == nil && id == 0 {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if e = r.expireSQLLobby(ctx, id); e != nil {
		return nil, e
	}
	lb, e := r.loadSQL(ctx, id)
	if e == nil && (lb == nil || !lb.Has(p)) {
		return nil, ErrWriteConflict
	}
	return lb, e
}
func (r *TrialLobbyRepo) expireSQLLobby(ctx context.Context, id uint64) error {
	return r.withSQLLobby(ctx, id, func(ctx context.Context, tx *sql.Tx, row *pb.GuildTrialLobbyRecord, o *pb.GuildTrialLobbyOriginal, c *pb.GuildTrialLobbyConsents) error {
		if row.State != 1 && row.State != 2 {
			return nil
		}
		now, e := trialSQLNow(ctx, tx)
		if e != nil {
			return e
		}
		if row.State == 1 && now < row.ExpiresMs || row.State == 2 && now < row.LaunchDeadlineMs {
			return nil
		}
		if row.State == 2 {
			// Prepare是所有Start RPC的许可点；同P/G锁内无永久launch，再写4墓碑，迟到Prepare只能拒绝。
			// 已有launch（包括未知状态）只能沿原件恢复，绝不因截止或缺Match票解除。
			var request uint64
			err := tx.QueryRowContext(ctx, "SELECT request_id FROM guild_trial_launch WHERE request_id=?", id).Scan(&request)
			if err == nil {
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		tip := constants.ErrTrialInviteExpired
		if row.State == 2 {
			tip = constants.ErrTrialServiceBusy
		}
		return endPendingLobby(ctx, tx, row, c, tip, nil, now)
	})
}

// 每状态每轮固定四项keyset，失败也前进；到尾回零。永久launch一旦存在只能沿正证明结束。
func (r *TrialLobbyRepo) ExpirePendingSQL(ctx context.Context, after uint64) (uint64, error) {
	return r.expireUnpreparedPage(ctx, 1, after)
}
func (r *TrialLobbyRepo) ExpireUnpreparedSQL(ctx context.Context, after uint64) (uint64, error) {
	return r.expireUnpreparedPage(ctx, 2, after)
}
func (r *TrialLobbyRepo) expireUnpreparedPage(ctx context.Context, state uint32, after uint64) (uint64, error) {
	if !r.SQLAuthorityEnabled() {
		return 0, nil
	}
	rows, e := r.guilds.db.QueryContext(ctx, "SELECT lobby_id FROM guild_trial_lobby WHERE state=? AND lobby_id>? ORDER BY lobby_id LIMIT 4", state, after)
	if e != nil {
		return after, e
	}
	var ids []uint64
	for rows.Next() {
		var id uint64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return after, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return after, e
	}
	if len(ids) == 0 {
		return 0, nil
	}
	var first error
	for _, id := range ids {
		after = id
		if e = r.expireSQLLobby(ctx, id); e != nil && first == nil {
			first = e
		}
	}
	return after, first
}

// caller已完整P/G；在取永久launch之前核真实大厅，终局证明消费也沿同一锁序。
func lockTrialLobbyForLaunch(ctx context.Context, tx *sql.Tx, original *pb.GuildTrialLaunchOriginal) (*pb.GuildTrialLobbyRecord, error) {
	row, e := scanTrialLobby(tx.QueryRowContext(ctx, "SELECT "+trialLobbyColumns+" FROM guild_trial_lobby WHERE lobby_id=? FOR UPDATE", original.RequestId))
	if e != nil {
		return nil, e
	}
	_, o, c, e := lobbyView(row)
	if e != nil {
		return nil, e
	}
	if !sameLobbyLaunch(o, original) || len(c.Consents) != len(o.Roster) || row.State != 2 && row.State != 3 && row.State != 4 {
		return nil, ErrWriteConflict
	}
	if e = lockActiveLobbySlots(ctx, tx, row, o); e != nil {
		return nil, e
	}
	return row, nil
}
func closeProvenTrialLobby(ctx context.Context, tx *sql.Tx, row *pb.GuildTrialLobbyRecord, now uint64) error {
	if row.State == 4 {
		return nil
	}
	_, _, c, e := lobbyView(row)
	if e != nil {
		return e
	}
	if e = clearLobbySlots(ctx, tx, row); e != nil {
		return e
	}
	v := currentLobbyView(row)
	v.State = pb.GuildTrialLobbyState_GUILD_TRIAL_LOBBY_STATE_ENDED
	return writeLobby(ctx, tx, row, v, c, now)
}
