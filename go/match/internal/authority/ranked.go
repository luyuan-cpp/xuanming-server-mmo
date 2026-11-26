package authority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"match/internal/metrics"
	"match/internal/ranked"
	"match/internal/ratingrule"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	pb "proto/match"
	rankpb "proto/rank"
)

const millisPerDay uint64 = 86_400_000

const (
	RolloverStageNone              uint32 = 0
	RolloverStageAdmissionStopped  uint32 = 1
	RolloverStageFormationsDrained uint32 = 2
	RolloverStageRatingsDrained    uint32 = 3
	RolloverStageRankStopped       uint32 = 4
	RolloverStageRankDrained       uint32 = 5
	RolloverStageBoardSealed       uint32 = 6
	RolloverStageHonorsSnapshot    uint32 = 7
	RolloverStageDone              uint32 = 8
)

const (
	seasonColumns        = "season_id,season_key,season_name,rank_generation,source_epoch,rule_version,state,rollover_stage,start_ms,end_ms,rank_board_id,expected_rank_cursor,champion_player_id,champion_name,champion_rating_centi,updated_ms"
	playerSeasonColumns  = "season_id,player_id,qualified,qualified_at_ms,rating_centi,peak_rating_centi,peak_tier,valid_matches,wins,losses,draws,milestone_bits,honor_bits,final_rank,updated_ms,current_win_streak,best_win_streak"
	rivalryColumns       = "season_id,player_id,opponent_id,opponent_name,wins,losses,draws,last_outcome,last_battle_id,updated_ms"
	featuredBuildColumns = "season_id,build_key,season_seal_id,starter_pet_table_id,core_skill_ids,appearances,wins,high_tier_wins,featured_battle_id,featured_player_id,featured_player_name,featured_rating_centi,featured_team_size,featured_total_rounds,updated_ms"
)

var (
	ErrRankedSeasonSettling      = errors.New("ranked: current season is settling")
	ErrRankedPresetInvalid       = errors.New("ranked: invalid preset or cosmetic loadout")
	ErrRankedRuleVersionMismatch = errors.New("ranked: rule version mismatch")
)

// SwapRankedCatalog 在启动及表包两阶段热更提交时原子切换当前生效的论剑排位目录。
func (s *Store) SwapRankedCatalog(cat *ranked.Catalog) {
	if s != nil {
		s.rankedCatalog.Store(cat)
	}
}

// RankedCatalog 返回当前生效的论剑排位目录;单测未装配时回落默认 V1 目录。
func (s *Store) RankedCatalog() *ranked.Catalog {
	if s == nil {
		return ranked.DefaultFallback()
	}
	if c := s.rankedCatalog.Load(); c != nil {
		return c
	}
	return ranked.DefaultFallback()
}

func scanSeason(row scanner) (*pb.MatchRankedSeasonRecord, error) {
	r := &pb.MatchRankedSeasonRecord{}
	err := row.Scan(&r.SeasonId, &r.SeasonKey, &r.SeasonName, &r.RankGeneration, &r.SourceEpoch, &r.RuleVersion, &r.State, &r.RolloverStage, &r.StartMs, &r.EndMs, &r.RankBoardId, &r.ExpectedRankCursor, &r.ChampionPlayerId, &r.ChampionName, &r.ChampionRatingCenti, &r.UpdatedMs)
	return r, err
}

func scanPlayerSeason(row scanner) (*pb.MatchRankedPlayerSeasonRecord, error) {
	r := &pb.MatchRankedPlayerSeasonRecord{}
	err := row.Scan(&r.SeasonId, &r.PlayerId, &r.Qualified, &r.QualifiedAtMs, &r.RatingCenti, &r.PeakRatingCenti, &r.PeakTier, &r.ValidMatches, &r.Wins, &r.Losses, &r.Draws, &r.MilestoneBits, &r.HonorBits, &r.FinalRank, &r.UpdatedMs, &r.CurrentWinStreak, &r.BestWinStreak)
	return r, err
}

func seasonNameFor(id uint32) string {
	return fmt.Sprintf("第%d赛季", id)
}

func (s *Store) ensureActiveSeasonTx(ctx context.Context, tx *sql.Tx, stamp uint64) (*pb.MatchRankedSeasonRecord, error) {
	cur, err := scanSeason(tx.QueryRowContext(ctx, "SELECT "+seasonColumns+" FROM match_ranked_season ORDER BY season_id DESC LIMIT 1"))
	if err == nil {
		return cur, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	cat := s.RankedCatalog()
	rule := cat.Rule()
	endMs := stamp + uint64(rule.GetSeasonDurationDays())*millisPerDay
	s1 := &pb.MatchRankedSeasonRecord{
		SeasonId:       1,
		SeasonKey:      "s1",
		SeasonName:     "第一赛季",
		RankGeneration: 1,
		SourceEpoch:    1,
		RuleVersion:    cat.RuleVersion(),
		State:          uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_ACTIVE),
		RolloverStage:  RolloverStageNone,
		StartMs:        stamp,
		EndMs:          endMs,
		UpdatedMs:      stamp,
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO match_ranked_season("+seasonColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		s1.SeasonId, s1.SeasonKey, s1.SeasonName, s1.RankGeneration, s1.SourceEpoch, s1.RuleVersion, s1.State, s1.RolloverStage,
		s1.StartMs, s1.EndMs, s1.RankBoardId, s1.ExpectedRankCursor, s1.ChampionPlayerId, s1.ChampionName, s1.ChampionRatingCenti, s1.UpdatedMs)
	if err != nil {
		var dup *mysql.MySQLError
		if errors.As(err, &dup) && dup.Number == 1062 {
			return scanSeason(tx.QueryRowContext(ctx, "SELECT "+seasonColumns+" FROM match_ranked_season WHERE season_id=1"))
		}
		return nil, err
	}
	return s1, nil
}

// EnsureActiveSeason 读取当前最新赛季;空表时惰性创建第一赛季(S1)。
func (s *Store) EnsureActiveSeason(ctx context.Context) (*pb.MatchRankedSeasonRecord, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	cur, err := s.ensureActiveSeasonTx(ctx, tx, stamp)
	if err != nil {
		return nil, err
	}
	return cur, tx.Commit()
}

// requireRankedAdmissionOpenTx 校验当前赛季是否处于可接纳新排位报名/成局的 ACTIVE 窗口([start_ms, end_ms))。
func (s *Store) requireRankedAdmissionOpenTx(ctx context.Context, tx *sql.Tx, stamp uint64) error {
	cur, err := s.ensureActiveSeasonTx(ctx, tx, stamp)
	if err != nil {
		return err
	}
	if cur.State != uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_ACTIVE) || (cur.EndMs > 0 && stamp >= cur.EndMs) {
		return ErrRankedSeasonSettling
	}
	return nil
}

// freezeRankedFormationContextTx 在 PVP_TEAM 成局提交点校验并冻结完整的 RankedMatchContext。
// 若所有成员票均未携带 ranked_context(旧非排位单测夹具),且当前库中没有处于 SETTLING 的赛季,则保持 RankedSeasonId=0。
func (s *Store) freezeRankedFormationContextTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, current []*pb.MatchAuthorityTicketRecord, stamp uint64) error {
	hasAnyRanked := false
	for _, t := range current {
		if len(t.RankedContext) > 0 {
			hasAnyRanked = true
			break
		}
	}
	if !hasAnyRanked {
		// 检查是否有已创建的赛季表记录且正处于 SETTLING/到期停招;单测未迁排位表或空表时放行旧夹具。
		var state uint32
		var endMs uint64
		err := tx.QueryRowContext(ctx, "SELECT state,end_ms FROM match_ranked_season ORDER BY season_id DESC LIMIT 1").Scan(&state, &endMs)
		if err == nil && (state != uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_ACTIVE) || (endMs > 0 && stamp >= endMs)) {
			return ErrRankedSeasonSettling
		}
		return nil
	}
	cur, err := s.ensureActiveSeasonTx(ctx, tx, stamp)
	if err != nil {
		return err
	}
	if cur.State != uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_ACTIVE) || (cur.EndMs > 0 && stamp >= cur.EndMs) {
		return ErrRankedSeasonSettling
	}
	cat := s.RankedCatalog()
	if f.ConfigId != cat.BattleConfigId() {
		return ErrRankedPresetInvalid
	}
	// 正式人宠会武(PVP_TEAM)已拍板使用本人真实养成(UseFreePreset=false)；
	// 校验每个成员票的规则版本与当前目录一致，但不再强制要求携带免费 3×3 预设。
	for _, t := range current {
		if len(t.RankedContext) == 0 {
			return ErrRankedPresetInvalid
		}
		var tkCtx battlepb.RankedMatchContext
		if err := proto.Unmarshal(t.RankedContext, &tkCtx); err != nil {
			return ErrRankedPresetInvalid
		}
		if tkCtx.GetRuleVersion() != 0 && tkCtx.GetRuleVersion() != cat.RuleVersion() {
			return ErrRankedRuleVersionMismatch
		}
	}
	var memberStreaks map[uint64]uint32
	minRatingCenti := int64(0)
	hasMemberRating := false
	for _, t := range current {
		var streak uint32
		ratingCenti := int64(ratingrule.DefaultCenti)
		sErr := tx.QueryRowContext(ctx, "SELECT current_win_streak,rating_centi FROM match_ranked_player_season WHERE season_id=? AND player_id=?", cur.SeasonId, t.PlayerId).Scan(&streak, &ratingCenti)
		if sErr == nil {
			if streak >= 3 {
				if memberStreaks == nil {
					memberStreaks = make(map[uint64]uint32)
				}
				memberStreaks[t.PlayerId] = streak
			}
		} else if !errors.Is(sErr, sql.ErrNoRows) {
			return sErr
		}
		if !hasMemberRating || ratingCenti < minRatingCenti {
			minRatingCenti = ratingCenti
			hasMemberRating = true
		}
	}
	skillBanSlots := uint32(0)
	if ruleBanSlots := cat.Rule().GetSkillBanSlotsPerTeam(); ruleBanSlots > 0 && hasMemberRating && minRatingCenti >= int64(cat.Rule().GetSkillBanMinRatingCenti()) {
		skillBanSlots = ruleBanSlots
	}
	rivalryEncounters, err := loadRivalryEncountersForFormationTx(ctx, tx, cur.SeasonId, f.GetMembers())
	if err != nil {
		return err
	}
	frozen := &battlepb.RankedMatchContext{
		SeasonId:             cur.SeasonId,
		SeasonKey:            cur.SeasonKey,
		RankGeneration:       cur.RankGeneration,
		RuleVersion:          cat.RuleVersion(),
		TeamSize:             uint32(len(current) / 2),
		ConfigFingerprint:    cat.Fingerprint(),
		UseFreePreset:        false,
		MemberWinStreaks:     memberStreaks,
		SkillBanSlotsPerTeam: skillBanSlots,
		RivalryEncounters:    rivalryEncounters,
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(frozen)
	if err != nil {
		return err
	}
	f.RankedContext = raw
	f.RankedSeasonId = cur.SeasonId
	return nil
}

// freezeFreePresetFormationContextTx 为轮换娱乐场（物法易势 5V5 / 护卫救援 PVE）冻结免费 3×3 预设上下文：
// SeasonId 恒为 0（不计正式排位分），UseFreePreset 恒为 true；成员未在票中显式传预设时读已保存偏好或默认(101/203)。
func (s *Store) freezeFreePresetFormationContextTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, current []*pb.MatchAuthorityTicketRecord) error {
	cat := s.RankedCatalog()
	defChar, defPet := cat.DefaultPresets()
	members := make([]*battlepb.RankedMemberLoadout, 0, len(current))
	for _, t := range current {
		var chID, petID uint32
		if len(t.RankedContext) > 0 {
			var tkCtx battlepb.RankedMatchContext
			if err := proto.Unmarshal(t.RankedContext, &tkCtx); err != nil {
				return ErrRankedPresetInvalid
			}
			if tkCtx.GetRuleVersion() != 0 && tkCtx.GetRuleVersion() != cat.RuleVersion() {
				return ErrRankedRuleVersionMismatch
			}
			for _, m := range tkCtx.GetMembers() {
				if m.GetPlayerId() == t.PlayerId {
					chID = m.GetPresets().GetCharacterPresetId()
					petID = m.GetPresets().GetPetPresetId()
					break
				}
			}
		}
		if chID == 0 || petID == 0 {
			prefs, err := s.readPlayerPrefsTx(ctx, tx, t.PlayerId)
			if err == nil {
				if chID == 0 {
					chID = prefs.CharacterPresetId
				}
				if petID == 0 {
					petID = prefs.PetPresetId
				}
			}
		}
		if chID == 0 {
			chID = defChar
		}
		if petID == 0 {
			petID = defPet
		}
		if !cat.IsValidPair(chID, petID) {
			return ErrRankedPresetInvalid
		}
		members = append(members, &battlepb.RankedMemberLoadout{
			PlayerId: t.PlayerId,
			Presets: &battlepb.RankedPresetSelection{
				CharacterPresetId: chID,
				PetPresetId:       petID,
			},
		})
	}
	sort.Slice(members, func(i, j int) bool { return members[i].PlayerId < members[j].PlayerId })
	teamSize := uint32(len(current))
	if f.Mode == uint32(pb.MatchMode_MATCH_MODE_POLARITY_5V5) {
		teamSize = uint32(len(current) / 2)
	}
	frozen := &battlepb.RankedMatchContext{
		SeasonId:          0,
		RuleVersion:       cat.RuleVersion(),
		TeamSize:          teamSize,
		ConfigFingerprint: cat.Fingerprint(),
		Members:           members,
		UseFreePreset:     true,
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(frozen)
	if err != nil {
		return err
	}
	f.RankedContext = raw
	f.RankedSeasonId = 0
	return nil
}

// ResolveSoloRankedContext 校验单排正式自养 PVP_TEAM 入队的赛季状态与规则版本并构造单人 RankedMatchContext(UseFreePreset=false)。
func (s *Store) ResolveSoloRankedContext(ctx context.Context, playerID uint64, reqChar, reqPet, reqRuleVer, teamSize uint32) ([]byte, error) {
	var raw []byte
	err := s.Fenced(ctx, func() error {
		cat := s.RankedCatalog()
		if reqRuleVer != 0 && reqRuleVer != cat.RuleVersion() {
			return ErrRankedRuleVersionMismatch
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = s.lockActor(ctx, tx, playerID); err != nil {
			return err
		}
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		cur, err := s.ensureActiveSeasonTx(ctx, tx, stamp)
		if err != nil {
			return err
		}
		if cur.State != uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_ACTIVE) || (cur.EndMs > 0 && stamp >= cur.EndMs) {
			return ErrRankedSeasonSettling
		}
		single := &battlepb.RankedMatchContext{
			SeasonId:          cur.SeasonId,
			SeasonKey:         cur.SeasonKey,
			RankGeneration:    cur.RankGeneration,
			RuleVersion:       cat.RuleVersion(),
			TeamSize:          teamSize,
			ConfigFingerprint: cat.Fingerprint(),
			UseFreePreset:     false,
		}
		raw, err = (proto.MarshalOptions{Deterministic: true}).Marshal(single)
		if err != nil {
			return err
		}
		return tx.Commit()
	})
	return raw, err
}

// ResolveFreePresetContext 校验轮换娱乐场（物法易势 5V5 / 护卫救援）的免费 3×3 预设偏好，写回偏好并构造非计分预设上下文(UseFreePreset=true)。
func (s *Store) ResolveFreePresetContext(ctx context.Context, playerID uint64, reqChar, reqPet, reqRuleVer, teamSize uint32) ([]byte, error) {
	var raw []byte
	err := s.Fenced(ctx, func() error {
		cat := s.RankedCatalog()
		if reqRuleVer != 0 && reqRuleVer != cat.RuleVersion() {
			return ErrRankedRuleVersionMismatch
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = s.lockActor(ctx, tx, playerID); err != nil {
			return err
		}
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		prefs, err := s.readPlayerPrefsTx(ctx, tx, playerID)
		if err != nil {
			return err
		}
		defChar, defPet := cat.DefaultPresets()
		ch := reqChar
		if ch == 0 {
			ch = prefs.CharacterPresetId
		}
		if ch == 0 {
			ch = defChar
		}
		pet := reqPet
		if pet == 0 {
			pet = prefs.PetPresetId
		}
		if pet == 0 {
			pet = defPet
		}
		if !cat.IsValidPair(ch, pet) {
			return ErrRankedPresetInvalid
		}
		if prefs.CharacterPresetId != ch || prefs.PetPresetId != pet || prefs.RuleVersion != cat.RuleVersion() {
			prefs.PlayerId = playerID
			prefs.CharacterPresetId = ch
			prefs.PetPresetId = pet
			prefs.RuleVersion = cat.RuleVersion()
			prefs.UpdatedMs = stamp
			if err = upsertPlayerPrefsTx(ctx, tx, prefs); err != nil {
				return err
			}
		}
		single := &battlepb.RankedMatchContext{
			SeasonId:          0,
			RuleVersion:       cat.RuleVersion(),
			TeamSize:          teamSize,
			ConfigFingerprint: cat.Fingerprint(),
			Members: []*battlepb.RankedMemberLoadout{{
				PlayerId: playerID,
				Presets: &battlepb.RankedPresetSelection{
					CharacterPresetId: ch,
					PetPresetId:       pet,
				},
			}},
			UseFreePreset: true,
		}
		raw, err = (proto.MarshalOptions{Deterministic: true}).Marshal(single)
		if err != nil {
			return err
		}
		return tx.Commit()
	})
	return raw, err
}

func (s *Store) readPlayerPrefsTx(ctx context.Context, q rowQuerier, playerID uint64) (*pb.MatchRankedPlayerPrefsRecord, error) {
	r := &pb.MatchRankedPlayerPrefsRecord{PlayerId: playerID}
	err := q.QueryRowContext(ctx, "SELECT player_id,character_preset_id,pet_preset_id,rule_version,cosmetic_loadout,updated_ms FROM match_ranked_player_prefs WHERE player_id=?", playerID).
		Scan(&r.PlayerId, &r.CharacterPresetId, &r.PetPresetId, &r.RuleVersion, &r.CosmeticLoadout, &r.UpdatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		defC, defP := s.RankedCatalog().DefaultPresets()
		r.CharacterPresetId = defC
		r.PetPresetId = defP
		r.RuleVersion = s.RankedCatalog().RuleVersion()
		r.CosmeticLoadout = []byte{}
		return r, nil
	}
	return r, err
}

func upsertPlayerPrefsTx(ctx context.Context, tx *sql.Tx, r *pb.MatchRankedPlayerPrefsRecord) error {
	if r.CosmeticLoadout == nil {
		r.CosmeticLoadout = []byte{}
	}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO match_ranked_player_prefs(player_id,character_preset_id,pet_preset_id,rule_version,cosmetic_loadout,updated_ms) VALUES(?,?,?,?,?,?) "+
			"ON DUPLICATE KEY UPDATE character_preset_id=VALUES(character_preset_id),pet_preset_id=VALUES(pet_preset_id),rule_version=VALUES(rule_version),cosmetic_loadout=VALUES(cosmetic_loadout),updated_ms=VALUES(updated_ms)",
		r.PlayerId, r.CharacterPresetId, r.PetPresetId, r.RuleVersion, r.CosmeticLoadout, r.UpdatedMs)
	return err
}

func readPlayerRewards(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, playerID uint64) ([]*pb.MatchRankedRewardRecord, error) {
	rows, err := q.QueryContext(ctx, "SELECT player_id,season_id,reward_kind,tier_value,item_code,granted_at_ms FROM match_ranked_reward WHERE player_id=? ORDER BY season_id,reward_kind,tier_value", playerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchRankedRewardRecord
	for rows.Next() {
		r := &pb.MatchRankedRewardRecord{}
		if err := rows.Scan(&r.PlayerId, &r.SeasonId, &r.RewardKind, &r.TierValue, &r.ItemCode, &r.GrantedAtMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func decodeCosmeticLoadout(raw []byte) *pb.RankedCosmeticLoadout {
	out := &pb.RankedCosmeticLoadout{}
	if len(raw) > 0 {
		_ = proto.Unmarshal(raw, out)
	}
	return out
}

func effectiveCosmeticLoadout(prefs *pb.MatchRankedPlayerPrefsRecord, owned []*pb.MatchRankedRewardRecord) *pb.RankedCosmeticLoadout {
	c := decodeCosmeticLoadout(prefs.GetCosmeticLoadout())
	talisman := c.GetEquippedTalismanCode()
	if talisman == "" && !c.GetHideTalisman() {
		for _, r := range owned {
			if r.GetRewardKind() == uint32(pb.RankedRewardKind_RANKED_REWARD_TALISMAN) {
				talisman = r.GetItemCode()
			}
		}
	}
	if c.GetHideTalisman() {
		talisman = ""
	}
	return &pb.RankedCosmeticLoadout{
		EquippedTalismanCode:      talisman,
		EquippedAvatarPatternCode: c.GetEquippedAvatarPatternCode(),
		EquippedCardBgCode:        c.GetEquippedCardBgCode(),
		EquippedTitleCode:         c.GetEquippedTitleCode(),
		HideTalisman:              c.GetHideTalisman(),
	}
}

func ownsRewardCode(owned []*pb.MatchRankedRewardRecord, kind pb.RankedRewardKind, code string) bool {
	if code == "" {
		return true
	}
	for _, r := range owned {
		if r.GetRewardKind() == uint32(kind) && r.GetItemCode() == code {
			return true
		}
	}
	return false
}

func ownsTitleRewardCode(owned []*pb.MatchRankedRewardRecord, code string) bool {
	return ownsRewardCode(owned, pb.RankedRewardKind_RANKED_REWARD_TIER_TITLE, code) ||
		ownsRewardCode(owned, pb.RankedRewardKind_RANKED_REWARD_WIN_MILESTONE, code)
}

func (s *Store) toSeasonInfo(r *pb.MatchRankedSeasonRecord) *pb.RankedSeasonInfo {
	rule := s.RankedCatalog().Rule()
	allowedSizes := s.Catalog().AllowedTeamSizes(pb.MatchMode_MATCH_MODE_PVP_TEAM, rule.GetBattleConfigId())
	if len(allowedSizes) == 0 {
		allowedSizes = []uint32{1, 3}
	}
	return &pb.RankedSeasonInfo{
		SeasonId:            r.SeasonId,
		SeasonKey:           r.SeasonKey,
		SeasonName:          r.SeasonName,
		RankGeneration:      r.RankGeneration,
		RuleVersion:         r.RuleVersion,
		State:               pb.RankedSeasonState(r.State),
		StartMs:             r.StartMs,
		EndMs:               r.EndMs,
		BattleConfigId:      rule.GetBattleConfigId(),
		WaitPromptSeconds:   rule.GetWaitPromptSeconds(),
		SpreadTier1Seconds:  rule.GetSpreadTier1Seconds(),
		SpreadTier1Points:   rule.GetSpreadTier1Points(),
		SpreadTier2Seconds:  rule.GetSpreadTier2Seconds(),
		SpreadTier2Points:   rule.GetSpreadTier2Points(),
		SpreadTier3Points:   rule.GetSpreadTier3Points(),
		ChampionPlayerId:    r.ChampionPlayerId,
		ChampionName:        r.ChampionName,
		ChampionRatingCenti: r.ChampionRatingCenti,
		RankBoardSealed:     r.RolloverStage >= RolloverStageBoardSealed || r.State == uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_CLOSED),
		AllowedTeamSizes:    allowedSizes,
		UsesOwnProgression:  true,
		PrimaryTeamSizes:    []uint32{1, 3, 5},
		SpecialTeamSizes:    []uint32{2, 4},
		TierQingfengCenti:   rule.GetTierQingfengCenti(),
		TierYunzhangCenti:   rule.GetTierYunzhangCenti(),
		TierTianshuCenti:    rule.GetTierTianshuCenti(),
		TierWushuangCenti:   rule.GetTierWushuangCenti(),
		MilestoneWinMatches: s.RankedCatalog().WinMilestoneMatches(),
	}
}

func (s *Store) toPlayerSeasonSummary(r *pb.MatchRankedPlayerSeasonRecord, seasonKey string) *pb.RankedPlayerSeasonSummary {
	cat := s.RankedCatalog()
	peakTier := pb.RankedTier(r.PeakTier)
	if peakTier == pb.RankedTier_RANKED_TIER_UNSPECIFIED && r.Qualified {
		peakTier = cat.TierFor(r.PeakRatingCenti, true)
	}
	return &pb.RankedPlayerSeasonSummary{
		SeasonId:        r.SeasonId,
		SeasonKey:       seasonKey,
		Qualified:       r.Qualified,
		RatingCenti:     r.RatingCenti,
		CurrentTier:     cat.TierFor(r.RatingCenti, r.Qualified),
		PeakRatingCenti: r.PeakRatingCenti,
		PeakTier:        peakTier,
		ValidMatches:    r.ValidMatches,
		Wins:            r.Wins,
		Losses:          r.Losses,
		Draws:           r.Draws,
		QualifiedAtMs:   r.QualifiedAtMs,
		MilestoneBits:    r.MilestoneBits,
		FinalRank:        r.FinalRank,
		ChampionAwarded:  (r.HonorBits & 4) != 0,
		CurrentWinStreak: r.CurrentWinStreak,
		BestWinStreak:    r.BestWinStreak,
	}
}

func toRewardEntitlement(r *pb.MatchRankedRewardRecord) *pb.RankedRewardEntitlement {
	return &pb.RankedRewardEntitlement{
		SeasonId:    r.SeasonId,
		Kind:        pb.RankedRewardKind(r.RewardKind),
		TierValue:   r.TierValue,
		ItemCode:    r.ItemCode,
		GrantedAtMs: r.GrantedAtMs,
	}
}

// LoadRankedProfile 读取论剑排位大厅所需的全部权威状态(赛季、本季进度、预设偏好、权益、可选往季历史)。
func (s *Store) LoadRankedProfile(ctx context.Context, playerID uint64, includeHistory bool) (*pb.GetRankedProfileResponse, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stamp, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	cur, err := s.ensureActiveSeasonTx(ctx, tx, stamp)
	if err != nil {
		return nil, err
	}
	carriedRating, err := ratingCenti(ctx, tx, playerID, uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM))
	if err != nil {
		return nil, err
	}
	progress, err := scanPlayerSeason(tx.QueryRowContext(ctx, "SELECT "+playerSeasonColumns+" FROM match_ranked_player_season WHERE season_id=? AND player_id=?", cur.SeasonId, playerID))
	if errors.Is(err, sql.ErrNoRows) {
		progress = &pb.MatchRankedPlayerSeasonRecord{
			SeasonId:    cur.SeasonId,
			PlayerId:    playerID,
			RatingCenti: carriedRating,
		}
		err = nil
	}
	if err != nil {
		return nil, err
	}
	prefs, err := s.readPlayerPrefsTx(ctx, tx, playerID)
	if err != nil {
		return nil, err
	}
	owned, err := readPlayerRewards(ctx, tx, playerID)
	if err != nil {
		return nil, err
	}
	var pastSeasons []*pb.MatchRankedSeasonRecord
	var history []*pb.MatchRankedPlayerSeasonRecord
	if includeHistory {
		sRows, err := tx.QueryContext(ctx, "SELECT "+seasonColumns+" FROM match_ranked_season WHERE season_id<? ORDER BY season_id DESC", cur.SeasonId)
		if err != nil {
			return nil, err
		}
		for sRows.Next() {
			row, scanErr := scanSeason(sRows)
			if scanErr != nil {
				sRows.Close()
				return nil, scanErr
			}
			pastSeasons = append(pastSeasons, row)
		}
		if err = errors.Join(sRows.Err(), sRows.Close()); err != nil {
			return nil, err
		}
		hRows, err := tx.QueryContext(ctx, "SELECT "+playerSeasonColumns+" FROM match_ranked_player_season WHERE player_id=? AND season_id<? ORDER BY season_id DESC", playerID, cur.SeasonId)
		if err != nil {
			return nil, err
		}
		for hRows.Next() {
			row, scanErr := scanPlayerSeason(hRows)
			if scanErr != nil {
				hRows.Close()
				return nil, scanErr
			}
			history = append(history, row)
		}
		if err = errors.Join(hRows.Err(), hRows.Close()); err != nil {
			return nil, err
		}
	}
	featuredBuilds, err := loadTopFeaturedBuildsTx(ctx, tx, cur.SeasonId, 5)
	if err != nil {
		return nil, err
	}
	topRivals, err := loadTopRivalsTx(ctx, tx, cur.SeasonId, playerID, 8)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	cat := s.RankedCatalog()
	defC, defP := cat.DefaultPresets()
	ch, pet := prefs.CharacterPresetId, prefs.PetPresetId
	if ch == 0 {
		ch = defC
	}
	if pet == 0 {
		pet = defP
	}
	resp := &pb.GetRankedProfileResponse{
		CurrentSeason:   s.toSeasonInfo(cur),
		CurrentProgress: s.toPlayerSeasonSummary(progress, cur.SeasonKey),
		SelectedPresets: &battlepb.RankedPresetSelection{CharacterPresetId: ch, PetPresetId: pet},
		CosmeticLoadout: effectiveCosmeticLoadout(prefs, owned),
		Presets:         cat.PresetViews(),
		ServerTimeMs:    stamp,
		FeaturedBuilds:  featuredBuilds,
		TopRivals:       topRivals,
	}
	for _, r := range owned {
		resp.Entitlements = append(resp.Entitlements, toRewardEntitlement(r))
	}
	keysByID := make(map[uint32]string, len(pastSeasons))
	for _, ps := range pastSeasons {
		keysByID[ps.SeasonId] = ps.SeasonKey
		resp.PastSeasons = append(resp.PastSeasons, s.toSeasonInfo(ps))
	}
	for _, h := range history {
		key := keysByID[h.SeasonId]
		if key == "" {
			key = fmt.Sprintf("s%d", h.SeasonId)
		}
		resp.History = append(resp.History, s.toPlayerSeasonSummary(h, key))
	}
	return resp, nil
}

// SaveRankedPreset 保存单排默认人宠预设偏好。
func (s *Store) SaveRankedPreset(ctx context.Context, playerID uint64, charPresetID, petPresetID, ruleVersion uint32) (*battlepb.RankedPresetSelection, uint32, error) {
	cat := s.RankedCatalog()
	curVer := cat.RuleVersion()
	if ruleVersion != 0 && ruleVersion != curVer {
		return nil, curVer, ErrRankedRuleVersionMismatch
	}
	if !cat.IsValidPair(charPresetID, petPresetID) {
		return nil, curVer, ErrRankedPresetInvalid
	}
	var sel *battlepb.RankedPresetSelection
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = s.lockActor(ctx, tx, playerID); err != nil {
			return err
		}
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		if err = s.requireRankedAdmissionOpenTx(ctx, tx, stamp); err != nil {
			return err
		}
		prefs, err := s.readPlayerPrefsTx(ctx, tx, playerID)
		if err != nil {
			return err
		}
		prefs.PlayerId = playerID
		prefs.CharacterPresetId = charPresetID
		prefs.PetPresetId = petPresetID
		prefs.RuleVersion = curVer
		prefs.UpdatedMs = stamp
		if err = upsertPlayerPrefsTx(ctx, tx, prefs); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		sel = &battlepb.RankedPresetSelection{CharacterPresetId: charPresetID, PetPresetId: petPresetID}
		return nil
	})
	return sel, curVer, err
}

// UpdateRankedCosmetic 校验展示权益归属并更新佩戴配置。
func (s *Store) UpdateRankedCosmetic(ctx context.Context, playerID uint64, loadout *pb.RankedCosmeticLoadout) (*pb.RankedCosmeticLoadout, error) {
	if loadout == nil {
		return nil, ErrRankedPresetInvalid
	}
	var out *pb.RankedCosmeticLoadout
	err := s.Fenced(ctx, func() error {
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = s.lockActor(ctx, tx, playerID); err != nil {
			return err
		}
		owned, err := readPlayerRewards(ctx, tx, playerID)
		if err != nil {
			return err
		}
		if !ownsRewardCode(owned, pb.RankedRewardKind_RANKED_REWARD_TALISMAN, loadout.GetEquippedTalismanCode()) ||
			!ownsRewardCode(owned, pb.RankedRewardKind_RANKED_REWARD_AVATAR_PATTERN, loadout.GetEquippedAvatarPatternCode()) ||
			!ownsRewardCode(owned, pb.RankedRewardKind_RANKED_REWARD_CARD_BACKGROUND, loadout.GetEquippedCardBgCode()) ||
			!ownsTitleRewardCode(owned, loadout.GetEquippedTitleCode()) {
			return ErrRankedPresetInvalid
		}
		stamp, err := now(ctx, tx)
		if err != nil {
			return err
		}
		prefs, err := s.readPlayerPrefsTx(ctx, tx, playerID)
		if err != nil {
			return err
		}
		blob, err := (proto.MarshalOptions{Deterministic: true}).Marshal(loadout)
		if err != nil {
			return err
		}
		prefs.PlayerId = playerID
		prefs.CosmeticLoadout = blob
		prefs.UpdatedMs = stamp
		if err = upsertPlayerPrefsTx(ctx, tx, prefs); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		out = effectiveCosmeticLoadout(prefs, owned)
		return nil
	})
	return out, err
}

// GetRankedBattleResult 查询本人指定或最近一局排位权威结算摘要;指定 battle_id 尚未落账时返回 PENDING 占位。
func (s *Store) GetRankedBattleResult(ctx context.Context, playerID, battleID uint64) (*pb.RankedBattleSettlementView, error) {
	var raw []byte
	var err error
	if battleID != 0 {
		err = s.DB.QueryRowContext(ctx, "SELECT summary FROM match_ranked_battle WHERE battle_id=? AND player_id=?", battleID, playerID).Scan(&raw)
	} else {
		err = s.DB.QueryRowContext(ctx, "SELECT summary FROM match_ranked_battle WHERE player_id=? ORDER BY settled_at_ms DESC,battle_id DESC LIMIT 1", playerID).Scan(&raw)
	}
	if errors.Is(err, sql.ErrNoRows) {
		if battleID != 0 {
			return &pb.RankedBattleSettlementView{
				BattleId: battleID,
				Status:   pb.RankedMatchRatingStatus_RANKED_MATCH_RATING_PENDING,
			}, nil
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	view := &pb.RankedBattleSettlementView{}
	if err := proto.Unmarshal(raw, view); err != nil {
		return nil, err
	}
	return view, nil
}

const (
	rankedMinRoundsForLoss       uint32 = 3
	rankedMinManualRoundsForLoss uint32 = 2
)

func evaluateRankedParticipation(fact *battlepb.BattleParticipationFact, settlement *battlepb.BattleSettlementData, totalRounds uint32, playerWon bool) bool {
	if totalRounds == 0 {
		return false
	}
	if settlement != nil && settlement.GetFled() {
		return false
	}
	baseParticipated := false
	noActionOpportunity := false
	if fact != nil {
		noActionOpportunity = fact.GetEnteredRoom() && fact.GetNoActionOpportunity()
		if fact.GetManualActionAccepted() || noActionOpportunity || (fact.GetEnteredRoom() && fact.GetExplicitAutoEnabled()) {
			baseParticipated = true
		}
	} else {
		baseParticipated = settlement != nil && settlement.GetParticipated()
	}
	if !baseParticipated {
		return false
	}
	// 胜局与首回合即被秒/全程受控无操作机会者直接放行；负/平局与材料账统一要求有效对抗门槛，防 1 回合开自动送死蹭 12 场皮肤与 3V3 坑队友。
	if playerWon || noActionOpportunity {
		return true
	}
	if settlement == nil {
		return false
	}
	return totalRounds >= rankedMinRoundsForLoss &&
		settlement.GetManualActionRounds() >= rankedMinManualRoundsForLoss &&
		settlement.GetEffectiveCombatContribution()
}

// ApplyRankedRating 在单一 SQL 事务内完成论剑排位的 Elo 落账、有效参赛与首次亮段判定、4/8/12 展示权益解锁、
// 本季定向投榜(仅已亮段玩家)与个人排位战果明细(match_ranked_battle)。重复结果严格幂等。
func (s *Store) ApplyRankedRating(ctx context.Context, event *kafkapb.BattleResultEvent, teamA, teamB []uint64, scoreA float64) (bool, error) {
	if event == nil || event.BattleId == 0 || len(teamA) == 0 || len(teamB) == 0 || event.GetRankedContext().GetSeasonId() == 0 {
		return false, errors.New("ranked: missing battle, teams or ranked_context")
	}
	mode := uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM)
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(event)
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(encoded)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stamp, err := now(ctx, tx)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO match_rating_applied(battle_id,payload_digest,applied_ms) VALUES(?,?,?)", event.BattleId, digest[:], stamp)
	if err != nil {
		var duplicate *mysql.MySQLError
		if !errors.As(err, &duplicate) || duplicate.Number != 1062 {
			return false, err
		}
		var prior []byte
		if err = tx.QueryRowContext(ctx, "SELECT payload_digest FROM match_rating_applied WHERE battle_id=?", event.BattleId).Scan(&prior); err != nil {
			return false, err
		}
		if !bytes.Equal(prior, digest[:]) {
			return false, errors.New("ranked: duplicate battle result digest mismatch")
		}
		return false, nil
	}

	rctx := event.GetRankedContext()
	season, err := scanSeason(tx.QueryRowContext(ctx, "SELECT "+seasonColumns+" FROM match_ranked_season WHERE season_id=?", rctx.GetSeasonId()))
	if errors.Is(err, sql.ErrNoRows) {
		// 仅允许在显式测试里缺行时回退构造对应赛季元数据;生产环境赛季行必在成局前已存在。
		season = &pb.MatchRankedSeasonRecord{
			SeasonId:       rctx.GetSeasonId(),
			SeasonKey:      rctx.GetSeasonKey(),
			SeasonName:     seasonNameFor(rctx.GetSeasonId()),
			RankGeneration: rctx.GetRankGeneration(),
			SourceEpoch:    uint64(rctx.GetSeasonId()),
			RuleVersion:    rctx.GetRuleVersion(),
			State:          uint32(pb.RankedSeasonState_RANKED_SEASON_STATE_ACTIVE),
			StartMs:        stamp,
			EndMs:          stamp + uint64(s.RankedCatalog().Rule().GetSeasonDurationDays())*millisPerDay,
			UpdatedMs:      stamp,
		}
		if season.SeasonKey == "" {
			season.SeasonKey = fmt.Sprintf("s%d", season.SeasonId)
		}
		if season.RankGeneration == 0 {
			season.RankGeneration = uint64(season.SeasonId)
		}
	} else if err != nil {
		return false, err
	}
	epoch := season.SourceEpoch
	if epoch == 0 {
		epoch = uint64(season.SeasonId)
	}
	generation := season.RankGeneration
	if generation == 0 {
		generation = uint64(season.SeasonId)
	}

	shard := rankSourceShard(event.BattleId)
	if _, err = tx.ExecContext(ctx, "INSERT INTO match_rank_source(source_epoch,mode,shard,produced_cursor,committed_cursor,board_id,stopped) VALUES(?,?,?,0,0,0,0) ON DUPLICATE KEY UPDATE source_epoch=source_epoch", epoch, mode, shard); err != nil {
		return false, err
	}
	var cursor uint64
	var stopped bool
	if err = tx.QueryRowContext(ctx, "SELECT produced_cursor,stopped FROM match_rank_source WHERE source_epoch=? AND mode=? AND shard=? FOR UPDATE", epoch, mode, shard).Scan(&cursor, &stopped); err != nil {
		return false, err
	}

	members := append(append([]uint64(nil), teamA...), teamB...)
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	ratings := make(map[uint64]int64, len(members))
	games := make(map[uint64]uint64, len(members))
	changedAt := make(map[uint64]uint64, len(members))
	playerSeasons := make(map[uint64]*pb.MatchRankedPlayerSeasonRecord, len(members))

	for i, pid := range members {
		if pid == 0 || (i > 0 && members[i-1] == pid) {
			return false, errors.New("ranked: empty or duplicate member in result")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO match_rating(player_id,mode,rating_centi,games,updated_ms,score_changed_ms) VALUES(?,?,?,0,?,?) ON DUPLICATE KEY UPDATE player_id=player_id", pid, mode, ratingrule.DefaultCenti, stamp, stamp); err != nil {
			return false, err
		}
		var rating int64
		var played, changed uint64
		if err = tx.QueryRowContext(ctx, "SELECT rating_centi,games,score_changed_ms FROM match_rating WHERE player_id=? AND mode=? FOR UPDATE", pid, mode).Scan(&rating, &played, &changed); err != nil {
			return false, err
		}
		ratings[pid] = rating
		games[pid] = played
		changedAt[pid] = changed

		ps, psErr := scanPlayerSeason(tx.QueryRowContext(ctx, "SELECT "+playerSeasonColumns+" FROM match_ranked_player_season WHERE season_id=? AND player_id=? FOR UPDATE", season.SeasonId, pid))
		if errors.Is(psErr, sql.ErrNoRows) {
			ps = &pb.MatchRankedPlayerSeasonRecord{
				SeasonId:    season.SeasonId,
				PlayerId:    pid,
				RatingCenti: rating,
			}
			psErr = nil
		}
		if psErr != nil {
			return false, psErr
		}
		playerSeasons[pid] = ps
	}

	settlementByPlayer := make(map[uint64]*battlepb.BattleSettlementData, len(event.GetSettlements()))
	for _, st := range event.GetSettlements() {
		if st != nil {
			settlementByPlayer[st.GetPlayerId()] = st
		}
	}
	factByPlayer := make(map[uint64]*battlepb.BattleParticipationFact, len(event.GetParticipationFacts()))
	for _, pf := range event.GetParticipationFacts() {
		if pf != nil {
			factByPlayer[pf.GetPlayerId()] = pf
		}
	}
	loadoutByPlayer := make(map[uint64]*battlepb.RankedMemberLoadout, len(rctx.GetMembers()))
	for _, m := range rctx.GetMembers() {
		if m != nil {
			loadoutByPlayer[m.GetPlayerId()] = m
		}
	}

	sides := make(map[uint64]uint32, len(members))
	for _, pid := range teamA {
		sides[pid] = 0
	}
	for _, pid := range teamB {
		sides[pid] = 1
	}

	cat := s.RankedCatalog()
	rule := cat.Rule()
	zeroRound := event.GetTotalRounds() == 0
	var delta int64
	if !zeroRound {
		average := func(team []uint64) float64 {
			sum := 0.0
			for _, pid := range team {
				sum += float64(ratings[pid]) / 100.0
			}
			return sum / float64(len(team))
		}
		delta = int64(math.Round(3200 * (scoreA - 1/(1+math.Pow(10, (average(teamB)-average(teamA))/400)))))
	}

	batch := &rankpb.SubmitRankScoresRequest{
		Generation:   generation,
		SourceEpoch:  epoch,
		SourceShard:  shard,
		SourceCursor: cursor + 1,
		RequestId:    event.BattleId,
		Mode:         rankpb.RankSubmitMode_RANK_SUBMIT_SET,
	}

	teamSize := rctx.GetTeamSize()
	if teamSize == 0 {
		teamSize = uint32(len(teamA))
	}

	// 论剑 V6：按确定性 (player_id, opponent_id) 顺序预读历史宿敌行，供本局雪耻复仇判定与同事务原子更新。
	type rivalryKey struct{ playerID, opponentID uint64 }
	priorRivalries := make(map[rivalryKey]*pb.MatchRankedRivalryRecord)
	if !zeroRound {
		pairs := make([]rivalryKey, 0, len(teamA)*len(teamB)*2)
		for _, a := range teamA {
			for _, b := range teamB {
				pairs = append(pairs, rivalryKey{a, b}, rivalryKey{b, a})
			}
		}
		sort.Slice(pairs, func(i, j int) bool {
			if pairs[i].playerID != pairs[j].playerID {
				return pairs[i].playerID < pairs[j].playerID
			}
			return pairs[i].opponentID < pairs[j].opponentID
		})
		for _, pair := range pairs {
			rec, rErr := scanRivalryRecord(tx.QueryRowContext(ctx,
				"SELECT "+rivalryColumns+" FROM match_ranked_rivalry WHERE season_id=? AND player_id=? AND opponent_id=? FOR UPDATE",
				season.SeasonId, pair.playerID, pair.opponentID))
			if rErr == nil {
				priorRivalries[pair] = rec
			} else if !errors.Is(rErr, sql.ErrNoRows) {
				return false, rErr
			}
		}
	}

	for _, pid := range members {
		ps := playerSeasons[pid]
		st := settlementByPlayer[pid]
		fact := factByPlayer[pid]
		loadout := loadoutByPlayer[pid]
		beforeRating := ratings[pid]
		tierBefore := cat.TierFor(beforeRating, ps.Qualified)
		fled := st != nil && st.GetFled()
		playerWon := !zeroRound && ((sides[pid] == 0 && scoreA == 1.0) || (sides[pid] == 1 && scoreA == 0.0))
		participated := evaluateRankedParticipation(fact, st, event.GetTotalRounds(), playerWon)

		status := pb.RankedMatchRatingStatus_RANKED_MATCH_RATING_CONFIRMED
		afterRating := beforeRating
		actualDelta := int64(0)
		achieved := changedAt[pid]
		nextGames := games[pid]
		qualifiedAfter := ps.Qualified
		newlyQualified := false
		qualifiedAt := ps.QualifiedAtMs
		validAfter := ps.ValidMatches
		wins, losses, draws := ps.Wins, ps.Losses, ps.Draws
		curStreak, bestStreak := ps.CurrentWinStreak, ps.BestWinStreak
		endedOpponentStreak := uint32(0)
		revengedRival := false
		revengedOpponentName := ""
		revengedWinsAfter := uint32(0)
		revengedLossesAfter := uint32(0)
		peakRating := ps.PeakRatingCenti
		peakTier := pb.RankedTier(ps.PeakTier)
		bits := ps.MilestoneBits
		var unlocked []*pb.RankedRewardEntitlement

		if zeroRound {
			status = pb.RankedMatchRatingStatus_RANKED_MATCH_RATING_ZERO_ROUND
		} else {
			change := -delta
			if sides[pid] == 0 {
				change = delta
			}
			afterRating = max(int64(0), beforeRating+change)
			actualDelta = afterRating - beforeRating
			if afterRating != beforeRating {
				achieved = stamp
			}
			nextGames = games[pid] + 1
			if _, err = tx.ExecContext(ctx, "UPDATE match_rating SET rating_centi=?,games=?,updated_ms=?,score_changed_ms=? WHERE player_id=? AND mode=?", afterRating, nextGames, stamp, achieved, pid, mode); err != nil {
				return false, err
			}

			qualifiedAfter = ps.Qualified || participated
			newlyQualified = !ps.Qualified && qualifiedAfter
			if newlyQualified {
				qualifiedAt = stamp
			}
			if participated {
				validAfter++
			}
			switch {
			case scoreA == 0.5:
				draws++
			case (sides[pid] == 0 && scoreA == 1.0) || (sides[pid] == 1 && scoreA == 0.0):
				wins++
				curStreak++
				if curStreak > bestStreak {
					bestStreak = curStreak
				}
				if st != nil && st.GetEndedOpponentStreak() >= 3 {
					endedOpponentStreak = st.GetEndedOpponentStreak()
				} else {
					for oppID, oppSide := range sides {
						if oppSide == sides[pid] {
							continue
						}
						oppStreak := rctx.GetMemberWinStreaks()[oppID]
						if oppStreak < 3 && playerSeasons[oppID] != nil {
							oppStreak = playerSeasons[oppID].CurrentWinStreak
						}
						if oppStreak >= 3 && oppStreak > endedOpponentStreak {
							endedOpponentStreak = oppStreak
						}
					}
				}
			default:
				losses++
				curStreak = 0
			}
			tierAfter := cat.TierFor(afterRating, qualifiedAfter)
			if qualifiedAfter {
				if !ps.Qualified || afterRating > peakRating {
					peakRating = afterRating
				}
				if uint32(tierAfter) > uint32(peakTier) {
					peakTier = tierAfter
				}
			}
			if participated {
				if (bits&1) == 0 && validAfter >= rule.GetMilestoneAvatarMatches() {
					bits |= 1
					r, gErr := grantRankedRewardTx(ctx, tx, pid, season.SeasonId, pb.RankedRewardKind_RANKED_REWARD_AVATAR_PATTERN, rule.GetMilestoneAvatarMatches(), season.SeasonKey+"_avatar_pattern", stamp)
					if gErr != nil {
						return false, gErr
					}
					if r != nil {
						unlocked = append(unlocked, r)
					}
				}
				if (bits&2) == 0 && validAfter >= rule.GetMilestoneCardMatches() {
					bits |= 2
					r, gErr := grantRankedRewardTx(ctx, tx, pid, season.SeasonId, pb.RankedRewardKind_RANKED_REWARD_CARD_BACKGROUND, rule.GetMilestoneCardMatches(), season.SeasonKey+"_card_bg", stamp)
					if gErr != nil {
						return false, gErr
					}
					if r != nil {
						unlocked = append(unlocked, r)
					}
				}
				if (bits&4) == 0 && validAfter >= rule.GetMilestoneTalismanMatches() {
					bits |= 4
					r, gErr := grantRankedRewardTx(ctx, tx, pid, season.SeasonId, pb.RankedRewardKind_RANKED_REWARD_TALISMAN, rule.GetMilestoneTalismanMatches(), season.SeasonKey+"_qingyun_talisman", stamp)
					if gErr != nil {
						return false, gErr
					}
					if r != nil {
						unlocked = append(unlocked, r)
					}
				}
				if winTarget := cat.WinMilestoneMatches(); (bits&8) == 0 && winTarget > 0 && wins >= winTarget {
					bits |= 8
					r, gErr := grantRankedRewardTx(ctx, tx, pid, season.SeasonId, pb.RankedRewardKind_RANKED_REWARD_WIN_MILESTONE, winTarget, fmt.Sprintf("%s_win_milestone_%d", season.SeasonKey, winTarget), stamp)
					if gErr != nil {
						return false, gErr
					}
					if r != nil {
						unlocked = append(unlocked, r)
					}
				}
			}
			if _, err = tx.ExecContext(ctx,
				"INSERT INTO match_ranked_player_season("+playerSeasonColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) "+
					"ON DUPLICATE KEY UPDATE qualified=VALUES(qualified),qualified_at_ms=VALUES(qualified_at_ms),rating_centi=VALUES(rating_centi),peak_rating_centi=VALUES(peak_rating_centi),peak_tier=VALUES(peak_tier),valid_matches=VALUES(valid_matches),wins=VALUES(wins),losses=VALUES(losses),draws=VALUES(draws),milestone_bits=VALUES(milestone_bits),updated_ms=VALUES(updated_ms),current_win_streak=VALUES(current_win_streak),best_win_streak=VALUES(best_win_streak)",
				season.SeasonId, pid, qualifiedAfter, qualifiedAt, afterRating, peakRating, uint32(peakTier), validAfter, wins, losses, draws, bits, ps.HonorBits, ps.FinalRank, stamp, curStreak, bestStreak); err != nil {
				return false, err
			}
			if qualifiedAfter {
				rankAchieved := achieved
				if newlyQualified {
					rankAchieved = qualifiedAt
				}
					batch.Entries = append(batch.Entries, &rankpb.RankScoreInput{
					EntityId:     pid,
					Score:        afterRating,
					AchievedMs:   rankAchieved,
					ScoreVersion: nextGames,
				})
			}

			// 论剑 V6：更新宿敌交手账本并判定本局雪耻复仇，同时汇总正式自养局的高段热门流派。
			opponents := teamB
			if sides[pid] == 1 {
				opponents = teamA
			}
			var bestRevengeOpp uint64
			var bestRevengePrior *pb.MatchRankedRivalryRecord
			for _, oppID := range opponents {
				prior := priorRivalries[rivalryKey{pid, oppID}]
				oppName := ""
				if oppSt := settlementByPlayer[oppID]; oppSt != nil && oppSt.GetPlayerDisplayName() != "" {
					oppName = oppSt.GetPlayerDisplayName()
				} else if prior != nil && prior.OpponentName != "" {
					oppName = prior.OpponentName
				}
				if playerWon && isRivalryRevengeTarget(prior) {
					if bestRevengePrior == nil ||
						prior.Losses > bestRevengePrior.Losses ||
						(prior.Losses == bestRevengePrior.Losses && prior.Wins > bestRevengePrior.Wins) ||
						(prior.Losses == bestRevengePrior.Losses && prior.Wins == bestRevengePrior.Wins && oppID < bestRevengeOpp) {
						bestRevengeOpp = oppID
						bestRevengePrior = prior
						revengedOpponentName = oppName
					}
				}
				nextRec := nextRivalryRecord(season.SeasonId, pid, oppID, oppName, prior, scoreA == 0.5, playerWon, event.BattleId, stamp)
				if err = upsertRivalryRecordTx(ctx, tx, nextRec); err != nil {
					return false, err
				}
			}
			if bestRevengePrior != nil {
				revengedRival = true
				revengedWinsAfter = bestRevengePrior.Wins + 1
				revengedLossesAfter = bestRevengePrior.Losses
			} else if playerWon && st != nil && st.GetRevengedRival() {
				revengedRival = true
				revengedOpponentName = st.GetRevengedOpponentName()
				revengedWinsAfter = st.GetRevengedWinsAfter()
				revengedLossesAfter = st.GetRevengedLossesAfter()
			}

			if !rctx.GetUseFreePreset() && participated && st != nil {
				if err = upsertFeaturedBuildTx(ctx, tx, season.SeasonId, cat.Rule().GetTierQingfengCenti(),
					event.BattleId, pid, st, afterRating, teamSize, event.GetTotalRounds(), playerWon, stamp); err != nil {
					return false, err
				}
			}
		}

		view := &pb.RankedBattleSettlementView{
			BattleId:                event.BattleId,
			SeasonId:                season.SeasonId,
			TeamSize:                teamSize,
			Status:                  status,
			Outcome:                 event.GetOutcome(),
			PlayerTeamIndex:         sides[pid],
			TotalRounds:             event.GetTotalRounds(),
			Participated:            participated,
			Fled:                    fled,
			RatingBeforeCenti:       beforeRating,
			RatingAfterCenti:        afterRating,
			RatingDeltaCenti:        actualDelta,
			TierBefore:              tierBefore,
			TierAfter:               cat.TierFor(afterRating, qualifiedAfter),
			NewlyQualified:          newlyQualified,
			SeasonValidMatchesAfter: validAfter,
			NewlyUnlockedRewards:    unlocked,
			SettledAtMs:             stamp,
			CharacterPresetId:       loadout.GetPresets().GetCharacterPresetId(),
			PetPresetId:             loadout.GetPresets().GetPetPresetId(),
			WinStreakAfter:          curStreak,
			EndedOpponentStreak:     endedOpponentStreak,
			RevengedRival:           revengedRival,
			RevengedOpponentName:    revengedOpponentName,
			RevengedWinsAfter:       revengedWinsAfter,
			RevengedLossesAfter:     revengedLossesAfter,
		}
		if err = writeRankedBattleRowTx(ctx, tx, view, pid); err != nil {
			return false, err
		}
	}

	unpublishedAfterStop := false
	if len(batch.Entries) > 0 {
		if !stopped {
			body, err := (proto.MarshalOptions{Deterministic: true}).Marshal(batch)
			if err != nil {
				return false, err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO match_rank_outbox(source_epoch,mode,shard,source_cursor,request_id,entries) VALUES(?,?,?,?,?,?)", epoch, mode, shard, cursor+1, event.BattleId, body); err != nil {
				return false, err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE match_rank_source SET produced_cursor=? WHERE source_epoch=? AND mode=? AND shard=?", cursor+1, epoch, mode, shard); err != nil {
				return false, err
			}
		} else {
			unpublishedAfterStop = true
		}
	}
	if err = tx.Commit(); err != nil {
		return false, fmt.Errorf("ranked rating commit outcome unknown; replay original battle: %w", err)
	}
	if unpublishedAfterStop {
		logx.Errorf("[ranked] Late ranked battle arrived after season rank source stopped; rating saved without leaking into next season battle=%d season=%d epoch=%d", event.BattleId, season.SeasonId, epoch)
		metrics.ObserveRatingUnpublished(pb.MatchMode_MATCH_MODE_PVP_TEAM.String())
	}
	return true, nil
}

func grantRankedRewardTx(ctx context.Context, tx *sql.Tx, playerID uint64, seasonID uint32, kind pb.RankedRewardKind, tierValue uint32, itemCode string, stamp uint64) (*pb.RankedRewardEntitlement, error) {
	res, err := tx.ExecContext(ctx,
		"INSERT IGNORE INTO match_ranked_reward(player_id,season_id,reward_kind,tier_value,item_code,granted_at_ms) VALUES(?,?,?,?,?,?)",
		playerID, seasonID, uint32(kind), tierValue, itemCode, stamp)
	if err != nil {
		return nil, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, nil
	}
	return &pb.RankedRewardEntitlement{
		SeasonId:    seasonID,
		Kind:        kind,
		TierValue:   tierValue,
		ItemCode:    itemCode,
		GrantedAtMs: stamp,
	}, nil
}

func writeRankedBattleRowTx(ctx context.Context, tx *sql.Tx, view *pb.RankedBattleSettlementView, playerID uint64) error {
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(view)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		"INSERT IGNORE INTO match_ranked_battle(battle_id,player_id,season_id,settled_at_ms,summary) VALUES(?,?,?,?,?)",
		view.BattleId, playerID, view.SeasonId, view.SettledAtMs, raw)
	return err
}

// writeVoidRankedBattlesTx 在 commitAbortTx 同一事务内为作废的论剑排位成局写入 VOID 摘要,
// 不改评分、不计季次数、不解锁权益,确保结算屏与重登查询立刻看到明确 VOID 状态。
func writeVoidRankedBattlesTx(ctx context.Context, tx *sql.Tx, f *pb.MatchFormationRecord, stamp uint64) error {
	if f == nil || f.RankedSeasonId == 0 || len(f.RankedContext) == 0 {
		return nil
	}
	var rctx battlepb.RankedMatchContext
	if err := proto.Unmarshal(f.RankedContext, &rctx); err != nil {
		return nil
	}
	loadouts := make(map[uint64]*battlepb.RankedMemberLoadout, len(rctx.GetMembers()))
	for _, m := range rctx.GetMembers() {
		if m != nil {
			loadouts[m.GetPlayerId()] = m
		}
	}
	cat := ranked.DefaultFallback()
	teamSize := rctx.GetTeamSize()
	if teamSize == 0 && len(f.Members) >= 2 {
		teamSize = uint32(len(f.Members) / 2)
	}
	for _, m := range f.Members {
		rating, err := ratingCenti(ctx, tx, m.PlayerId, uint32(pb.MatchMode_MATCH_MODE_PVP_TEAM))
		if err != nil {
			return err
		}
		var qualified bool
		var validMatches uint32
		err = tx.QueryRowContext(ctx, "SELECT qualified,valid_matches FROM match_ranked_player_season WHERE season_id=? AND player_id=?", f.RankedSeasonId, m.PlayerId).Scan(&qualified, &validMatches)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		tier := cat.TierFor(rating, qualified)
		lo := loadouts[m.PlayerId]
		view := &pb.RankedBattleSettlementView{
			BattleId:                f.BattleId,
			SeasonId:                f.RankedSeasonId,
			TeamSize:                teamSize,
			Status:                  pb.RankedMatchRatingStatus_RANKED_MATCH_RATING_VOID,
			Outcome:                 battlepb.EBattleOutcome_BATTLE_OUTCOME_ONGOING,
			PlayerTeamIndex:         m.TeamIndex,
			RatingBeforeCenti:       rating,
			RatingAfterCenti:        rating,
			RatingDeltaCenti:        0,
			TierBefore:              tier,
			TierAfter:               tier,
			SeasonValidMatchesAfter: validMatches,
			SettledAtMs:             stamp,
			CharacterPresetId:       lo.GetPresets().GetCharacterPresetId(),
			PetPresetId:             lo.GetPresets().GetPetPresetId(),
		}
		if err := writeRankedBattleRowTx(ctx, tx, view, m.PlayerId); err != nil {
			return err
		}
	}
	return nil
}

// ---- 论剑 V6：宿敌恩怨账本与问道名局热门流派辅助函数 ----

func scanRivalryRecord(row scanner) (*pb.MatchRankedRivalryRecord, error) {
	r := &pb.MatchRankedRivalryRecord{}
	err := row.Scan(&r.SeasonId, &r.PlayerId, &r.OpponentId, &r.OpponentName, &r.Wins, &r.Losses, &r.Draws, &r.LastOutcome, &r.LastBattleId, &r.UpdatedMs)
	return r, err
}

func scanFeaturedBuildRecord(row scanner) (*pb.MatchRankedFeaturedBuildRecord, error) {
	r := &pb.MatchRankedFeaturedBuildRecord{}
	err := row.Scan(&r.SeasonId, &r.BuildKey, &r.SeasonSealId, &r.StarterPetTableId, &r.CoreSkillIds, &r.Appearances, &r.Wins, &r.HighTierWins, &r.FeaturedBattleId, &r.FeaturedPlayerId, &r.FeaturedPlayerName, &r.FeaturedRatingCenti, &r.FeaturedTeamSize, &r.FeaturedTotalRounds, &r.UpdatedMs)
	return r, err
}

func isRivalryRevengeTarget(rec *pb.MatchRankedRivalryRecord) bool {
	if rec == nil || rec.Wins+rec.Losses == 0 {
		return false
	}
	return rec.LastOutcome == 2 || rec.Losses > rec.Wins
}

func nextRivalryRecord(seasonID uint32, playerID, opponentID uint64, opponentName string, prior *pb.MatchRankedRivalryRecord, isDraw, playerWon bool, battleID, stamp uint64) *pb.MatchRankedRivalryRecord {
	rec := &pb.MatchRankedRivalryRecord{
		SeasonId:     seasonID,
		PlayerId:     playerID,
		OpponentId:   opponentID,
		OpponentName: opponentName,
		LastBattleId: battleID,
		UpdatedMs:    stamp,
	}
	if prior != nil {
		rec.Wins = prior.Wins
		rec.Losses = prior.Losses
		rec.Draws = prior.Draws
		if rec.OpponentName == "" {
			rec.OpponentName = prior.OpponentName
		}
	}
	switch {
	case isDraw:
		rec.Draws++
		rec.LastOutcome = 3
	case playerWon:
		rec.Wins++
		rec.LastOutcome = 1
	default:
		rec.Losses++
		rec.LastOutcome = 2
	}
	return rec
}

func upsertRivalryRecordTx(ctx context.Context, tx *sql.Tx, rec *pb.MatchRankedRivalryRecord) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO match_ranked_rivalry("+rivalryColumns+") VALUES(?,?,?,?,?,?,?,?,?,?) "+
			"ON DUPLICATE KEY UPDATE opponent_name=IF(VALUES(opponent_name)<>'',VALUES(opponent_name),opponent_name),wins=VALUES(wins),losses=VALUES(losses),draws=VALUES(draws),last_outcome=VALUES(last_outcome),last_battle_id=VALUES(last_battle_id),updated_ms=VALUES(updated_ms)",
		rec.SeasonId, rec.PlayerId, rec.OpponentId, rec.OpponentName, rec.Wins, rec.Losses, rec.Draws, rec.LastOutcome, rec.LastBattleId, rec.UpdatedMs)
	return err
}

func loadRivalryEncountersForFormationTx(ctx context.Context, tx *sql.Tx, seasonID uint32, members []*pb.MatchFormationMember) ([]*battlepb.RankedRivalryEncounter, error) {
	if seasonID == 0 || len(members) < 2 {
		return nil, nil
	}
	var out []*battlepb.RankedRivalryEncounter
	for _, a := range members {
		for _, b := range members {
			if a.GetPlayerId() == 0 || b.GetPlayerId() == 0 || a.GetTeamIndex() == b.GetTeamIndex() {
				continue
			}
			rec, err := scanRivalryRecord(tx.QueryRowContext(ctx,
				"SELECT "+rivalryColumns+" FROM match_ranked_rivalry WHERE season_id=? AND player_id=? AND opponent_id=?",
				seasonID, a.GetPlayerId(), b.GetPlayerId()))
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if rec.Wins+rec.Losses == 0 {
				continue
			}
			out = append(out, &battlepb.RankedRivalryEncounter{
				PlayerId:      rec.PlayerId,
				OpponentId:    rec.OpponentId,
				OpponentName:  rec.OpponentName,
				Wins:          rec.Wins,
				Losses:        rec.Losses,
				RevengeTarget: isRivalryRevengeTarget(rec),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PlayerId != out[j].PlayerId {
			return out[i].PlayerId < out[j].PlayerId
		}
		return out[i].OpponentId < out[j].OpponentId
	})
	return out, nil
}

func loadTopRivalsTx(ctx context.Context, tx *sql.Tx, seasonID uint32, playerID uint64, limit int) ([]*pb.RankedRivalrySummary, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT "+rivalryColumns+" FROM match_ranked_rivalry WHERE season_id=? AND player_id=? ORDER BY (wins+losses) DESC,updated_ms DESC LIMIT ?",
		seasonID, playerID, limit)
	if err != nil {
		return nil, err
	}
	var out []*pb.RankedRivalrySummary
	for rows.Next() {
		rec, scanErr := scanRivalryRecord(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		if rec.Wins+rec.Losses+rec.Draws == 0 {
			continue
		}
		out = append(out, &pb.RankedRivalrySummary{
			OpponentId:   rec.OpponentId,
			OpponentName: rec.OpponentName,
			Wins:         rec.Wins,
			Losses:       rec.Losses,
			Draws:        rec.Draws,
			LastOutcome:  rec.LastOutcome,
			LastBattleId: rec.LastBattleId,
			UpdatedMs:    rec.UpdatedMs,
		})
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return out, nil
}

func canonicalBuildKey(sealID, petTableID uint32, skillIDs []uint32) (string, string, []uint32) {
	seen := make(map[uint32]struct{}, len(skillIDs))
	norm := make([]uint32, 0, 4)
	for _, sid := range skillIDs {
		if sid == 0 {
			continue
		}
		if _, dup := seen[sid]; dup {
			continue
		}
		seen[sid] = struct{}{}
		norm = append(norm, sid)
	}
	sort.Slice(norm, func(i, j int) bool { return norm[i] < norm[j] })
	if len(norm) > 4 {
		norm = norm[:4]
	}
	if sealID == 0 && petTableID == 0 && len(norm) == 0 {
		return "", "", nil
	}
	parts := make([]string, len(norm))
	for i, sid := range norm {
		parts[i] = strconv.FormatUint(uint64(sid), 10)
	}
	csv := strings.Join(parts, ",")
	return fmt.Sprintf("%d:%d:%s", sealID, petTableID, csv), csv, norm
}

func parseCoreSkillIDs(csv string) []uint32 {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	raw := strings.Split(csv, ",")
	out := make([]uint32, 0, len(raw))
	for _, token := range raw {
		v, err := strconv.ParseUint(strings.TrimSpace(token), 10, 32)
		if err == nil && v > 0 {
			out = append(out, uint32(v))
		}
	}
	return out
}

func upsertFeaturedBuildTx(ctx context.Context, tx *sql.Tx, seasonID, tierQingfengCenti uint32, battleID, playerID uint64, st *battlepb.BattleSettlementData, afterRating int64, teamSize, totalRounds uint32, playerWon bool, stamp uint64) error {
	buildKey, csv, _ := canonicalBuildKey(st.GetFinalSeasonSealId(), st.GetStarterPetTableId(), st.GetCoreSkillIds())
	if buildKey == "" {
		return nil
	}
	highTierMin := int64(tierQingfengCenti)
	if highTierMin <= 0 {
		highTierMin = 152000
	}
	highTierWin := playerWon && afterRating >= highTierMin
	rec, err := scanFeaturedBuildRecord(tx.QueryRowContext(ctx,
		"SELECT "+featuredBuildColumns+" FROM match_ranked_featured_build WHERE season_id=? AND build_key=? FOR UPDATE",
		seasonID, buildKey))
	if errors.Is(err, sql.ErrNoRows) {
		rec = &pb.MatchRankedFeaturedBuildRecord{
			SeasonId:          seasonID,
			BuildKey:          buildKey,
			SeasonSealId:      st.GetFinalSeasonSealId(),
			StarterPetTableId: st.GetStarterPetTableId(),
			CoreSkillIds:      csv,
		}
	} else if err != nil {
		return err
	}
	rec.Appearances++
	if playerWon {
		rec.Wins++
		if highTierWin {
			rec.HighTierWins++
		}
		if rec.FeaturedBattleId == 0 || afterRating >= rec.FeaturedRatingCenti {
			rec.FeaturedBattleId = battleID
			rec.FeaturedPlayerId = playerID
			rec.FeaturedPlayerName = st.GetPlayerDisplayName()
			rec.FeaturedRatingCenti = afterRating
			rec.FeaturedTeamSize = teamSize
			rec.FeaturedTotalRounds = totalRounds
		}
	}
	rec.UpdatedMs = stamp
	_, err = tx.ExecContext(ctx,
		"INSERT INTO match_ranked_featured_build("+featuredBuildColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) "+
			"ON DUPLICATE KEY UPDATE appearances=VALUES(appearances),wins=VALUES(wins),high_tier_wins=VALUES(high_tier_wins),featured_battle_id=VALUES(featured_battle_id),featured_player_id=VALUES(featured_player_id),featured_player_name=VALUES(featured_player_name),featured_rating_centi=VALUES(featured_rating_centi),featured_team_size=VALUES(featured_team_size),featured_total_rounds=VALUES(featured_total_rounds),updated_ms=VALUES(updated_ms)",
		rec.SeasonId, rec.BuildKey, rec.SeasonSealId, rec.StarterPetTableId, rec.CoreSkillIds,
		rec.Appearances, rec.Wins, rec.HighTierWins, rec.FeaturedBattleId, rec.FeaturedPlayerId,
		rec.FeaturedPlayerName, rec.FeaturedRatingCenti, rec.FeaturedTeamSize, rec.FeaturedTotalRounds, rec.UpdatedMs)
	return err
}

func loadTopFeaturedBuildsTx(ctx context.Context, tx *sql.Tx, seasonID uint32, limit int) ([]*pb.RankedFeaturedBuildView, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT "+featuredBuildColumns+" FROM match_ranked_featured_build WHERE season_id=? AND wins>0 ORDER BY high_tier_wins DESC,wins DESC,featured_rating_centi DESC,updated_ms DESC LIMIT ?",
		seasonID, limit)
	if err != nil {
		return nil, err
	}
	var out []*pb.RankedFeaturedBuildView
	for rows.Next() {
		rec, scanErr := scanFeaturedBuildRecord(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		out = append(out, &pb.RankedFeaturedBuildView{
			BuildKey:            rec.BuildKey,
			SeasonSealId:        rec.SeasonSealId,
			StarterPetTableId:   rec.StarterPetTableId,
			CoreSkillIds:        parseCoreSkillIDs(rec.CoreSkillIds),
			Appearances:         rec.Appearances,
			Wins:                rec.Wins,
			HighTierWins:        rec.HighTierWins,
			FeaturedBattleId:    rec.FeaturedBattleId,
			FeaturedPlayerId:    rec.FeaturedPlayerId,
			FeaturedPlayerName:  rec.FeaturedPlayerName,
			FeaturedRatingCenti: rec.FeaturedRatingCenti,
			FeaturedTeamSize:    rec.FeaturedTeamSize,
			FeaturedTotalRounds: rec.FeaturedTotalRounds,
			UpdatedMs:           rec.UpdatedMs,
		})
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return out, nil
}

// isFeaturedRankedReplayAllowed 允许全服玩家观摩「问道名局」高段热门流派代表战报（fail-closed：仅查 featured_battle_id 索引）。
func (s *Store) isFeaturedRankedReplayAllowed(ctx context.Context, battleID uint64) bool {
	if s == nil || s.DB == nil || battleID == 0 {
		return false
	}
	var one int
	return s.DB.QueryRowContext(ctx, "SELECT 1 FROM match_ranked_featured_build WHERE featured_battle_id=? LIMIT 1", battleID).Scan(&one) == nil
}
