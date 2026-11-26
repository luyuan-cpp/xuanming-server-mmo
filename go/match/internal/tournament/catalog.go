package tournament

import (
	"errors"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"
	mailpb "proto/mail"
	matchpb "proto/match"
	tablepb "shared/generated/pb/table"
	"shared/generated/table"
)

const (
	// GoldCurrencyType 对应 scene 银两货币枚举(kCurrencyGold = 1)。
	GoldCurrencyType uint32 = 1
	// DefaultRuleID 固定读取 TournamentRule 表 id=1。
	DefaultRuleID uint32 = 1
)

// Catalog 是全服赛季战队锦标赛《群仙问道赛》的不可变配置目录(支持启动校验与表包热更原子切换)。
type Catalog struct {
	rule           *tablepb.TournamentRuleTable
	rewardsByID    map[uint32]*tablepb.TournamentRewardTable
	rewardsByTier  map[uint32][]*tablepb.TournamentRewardTable
	orderedRewards []*tablepb.TournamentRewardTable
}

// BuildFromTableSet 从表包候选 TableSet 编译校验锦标赛目录(fail-closed)。
func BuildFromTableSet(ts *table.TableSet) (*Catalog, error) {
	if ts == nil || ts.TournamentRule == nil || ts.TournamentReward == nil {
		return nil, errors.New("tournament: TableSet missing TournamentRule or TournamentReward")
	}
	rule, ok := ts.TournamentRule.FindById(DefaultRuleID)
	if !ok || rule == nil {
		return nil, errors.New("tournament: TournamentRule missing id=1")
	}
	return Build(rule, ts.TournamentReward.FindAll())
}

// Build 校验并构造不可变锦标赛目录;任一非法参数直接拒启。
func Build(rule *tablepb.TournamentRuleTable, rewards []*tablepb.TournamentRewardTable) (*Catalog, error) {
	if rule == nil {
		return nil, errors.New("tournament: rule is nil")
	}
	if rule.GetBattleConfigId() == 0 {
		return nil, errors.New("tournament: battle_config_id must be > 0")
	}
	if rule.GetBattleTeamSize() == 0 || rule.GetBattleTeamSize() > 5 {
		return nil, fmt.Errorf("tournament: battle_team_size=%d out of [1,5]", rule.GetBattleTeamSize())
	}
	if rule.GetRosterMaxSize() < rule.GetBattleTeamSize() || rule.GetRosterMaxSize() > 7 {
		return nil, fmt.Errorf("tournament: roster_max_size=%d out of [%d,7]", rule.GetRosterMaxSize(), rule.GetBattleTeamSize())
	}
	if rule.GetRegistrationDays() == 0 || rule.GetQualifierDays() == 0 || rule.GetKnockoutDays() == 0 || rule.GetSettleDisplayDays() == 0 {
		return nil, errors.New("tournament: all four stage durations must be > 0 days")
	}
	if rule.GetQualifierMaxMatches() == 0 {
		return nil, errors.New("tournament: qualifier_max_matches must be > 0")
	}
	switch rule.GetKnockoutMaxTeams() {
	case 2, 4, 8, 16:
	default:
		return nil, fmt.Errorf("tournament: knockout_max_teams=%d must be one of 2/4/8/16", rule.GetKnockoutMaxTeams())
	}
	if rule.GetKnockoutRoundTimeoutSeconds() == 0 {
		return nil, errors.New("tournament: knockout_round_timeout_seconds must be > 0")
	}
	if rule.GetSkillBanSlotsPerTeam() > 3 {
		return nil, fmt.Errorf("tournament: skill_ban_slots_per_team=%d out of [0,3]", rule.GetSkillBanSlotsPerTeam())
	}

	c := &Catalog{
		rule:           proto.Clone(rule).(*tablepb.TournamentRuleTable),
		rewardsByID:    make(map[uint32]*tablepb.TournamentRewardTable, len(rewards)),
		rewardsByTier:  make(map[uint32][]*tablepb.TournamentRewardTable),
		orderedRewards: make([]*tablepb.TournamentRewardTable, 0, len(rewards)),
	}
	for _, row := range rewards {
		if row == nil || row.GetId() == 0 {
			continue
		}
		if _, dup := c.rewardsByID[row.GetId()]; dup {
			return nil, fmt.Errorf("tournament: duplicate TournamentReward id=%d", row.GetId())
		}
		cloned := proto.Clone(row).(*tablepb.TournamentRewardTable)
		c.rewardsByID[cloned.GetId()] = cloned
		c.rewardsByTier[cloned.GetTierKind()] = append(c.rewardsByTier[cloned.GetTierKind()], cloned)
		c.orderedRewards = append(c.orderedRewards, cloned)
	}
	sort.Slice(c.orderedRewards, func(i, j int) bool { return c.orderedRewards[i].GetId() < c.orderedRewards[j].GetId() })
	return c, nil
}

// DefaultFallback 供单测在未加载配置表目录时回落到与首测表一致的默认目录。
func DefaultFallback() *Catalog {
	rule := &tablepb.TournamentRuleTable{
		Id:                          1,
		RuleVersion:                 4,
		Enabled:                     true,
		BattleConfigId:              5,
		BattleTeamSize:              3,
		RosterMaxSize:               4,
		MinLevel:                    15,
		RegistrationDays:            42,
		QualifierDays:               7,
		KnockoutDays:                2,
		SettleDisplayDays:           5,
		QualifierMaxMatches:         6,
		ParticipationMinMatches:     2,
		KnockoutMaxTeams:            16,
		KnockoutRoundTimeoutSeconds: 1800,
		QualifierWinPoints:          3,
		QualifierDrawPoints:         1,
		QualifierLossPoints:         0,
		InviteTtlSeconds:            259200,
		Bo3Top4Enabled:              true,
		IntermissionSeconds:         60,
		PublicKnockoutReplay:        true,
		SaluteGoldAmount:            5000,
		SaluteItemId:                20,
		SaluteItemCount:             3,
		WarmupSparEnabled:           true,
		PredictionEnabled:           true,
		PredictionRewardMinHits:     2,
		ThirdPlaceMatchEnabled:      true,
		LeylineBanEnabled:           true,
		SkillBanSlotsPerTeam:        3,
	}
	rewards := []*tablepb.TournamentRewardTable{
		{Id: 1, TierKind: uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_CHAMPION), MinRank: 1, MaxRank: 1, GoldAmount: 120000, ItemIds: []uint32{3403, 3406, 20, 3407}, ItemCounts: []uint32{30, 30, 60, 3}, TitleCode: "tournament_champion", BadgeCode: "badge_tournament_champion", MailTitle: "群仙问道赛·全服总冠军奖励", MailBody: "恭喜你的战队夺得本赛季群仙问道赛全服总冠军！", Desc: "全服总冠军（第1名）"},
		{Id: 2, TierKind: uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_RUNNER_UP), MinRank: 2, MaxRank: 2, GoldAmount: 80000, ItemIds: []uint32{3403, 3406, 20, 3407}, ItemCounts: []uint32{20, 20, 40, 2}, TitleCode: "tournament_runner_up", BadgeCode: "badge_tournament_runner_up", MailTitle: "群仙问道赛·全服亚军奖励", MailBody: "恭喜你的战队斩获本赛季群仙问道赛全服亚军！", Desc: "全服亚军（第2名）"},
		{Id: 3, TierKind: uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_TOP4), MinRank: 3, MaxRank: 4, GoldAmount: 50000, ItemIds: []uint32{3403, 3406, 20, 3407}, ItemCounts: []uint32{14, 14, 30, 2}, TitleCode: "tournament_top4", BadgeCode: "badge_tournament_top4", MailTitle: "群仙问道赛·全服四强奖励", MailBody: "恭喜你的战队晋级本赛季群仙问道赛全服四强！", Desc: "全服四强（第3-4名）"},
		{Id: 4, TierKind: uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_TOP8), MinRank: 5, MaxRank: 8, GoldAmount: 35000, ItemIds: []uint32{3403, 3406, 20, 3407}, ItemCounts: []uint32{10, 10, 20, 1}, BadgeCode: "badge_tournament_top8", MailTitle: "群仙问道赛·全服八强奖励", MailBody: "恭喜你的战队闯入本赛季群仙问道赛全服八强！", Desc: "全服八强（第5-8名）"},
		{Id: 5, TierKind: uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_TOP16), MinRank: 9, MaxRank: 16, GoldAmount: 20000, ItemIds: []uint32{3403, 3406, 20, 3407}, ItemCounts: []uint32{6, 6, 15, 1}, BadgeCode: "badge_tournament_top16", MailTitle: "群仙问道赛·全服十六强奖励", MailBody: "恭喜你的战队入围本赛季群仙问道赛十六强！", Desc: "全服十六强（第9-16名）"},
		{Id: 6, TierKind: uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_PARTICIPANT), MinRank: 0, MaxRank: 0, GoldAmount: 12000, ItemIds: []uint32{3403, 3406, 20}, ItemCounts: []uint32{4, 4, 10}, BadgeCode: "badge_tournament_participant", MailTitle: "群仙问道赛·赛季参赛保底奖", MailBody: "感谢参与本赛季群仙问道赛并完成达标场次！", Desc: "参赛保底（完赛≥2场）"},
		{Id: 7, TierKind: uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_CHEER), MinRank: 0, MaxRank: 0, GoldAmount: 5000, ItemIds: []uint32{3403, 20}, ItemCounts: []uint32{1, 5}, BadgeCode: "badge_tournament_cheer", MailTitle: "群仙问道赛·观赛助威纪念奖", MailBody: "你免费助威的战队闯入四强或夺冠，奉上纪念礼！", Desc: "免费助威命中（进四强/夺冠）"},
		{Id: 8, TierKind: uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_THIRD_PLACE), MinRank: 3, MaxRank: 3, GoldAmount: 65000, ItemIds: []uint32{3403, 3406, 20, 3407}, ItemCounts: []uint32{18, 18, 35, 2}, TitleCode: "tournament_third_place", BadgeCode: "badge_tournament_third_place", MailTitle: "群仙问道赛·季军嘉奖", MailBody: "恭喜阁下在群仙问道赛季军争夺战中力克群雄斩获季军！特赐修炼物资与季军荣耀。", Desc: "季军争夺战第3名专属奖励"},
		{Id: 9, TierKind: uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_PREDICTION), MinRank: 0, MaxRank: 0, GoldAmount: 12000, ItemIds: []uint32{3406, 20}, ItemCounts: []uint32{6, 10}, TitleCode: "tournament_oracle", BadgeCode: "badge_tournament_oracle", MailTitle: "群仙问道赛·神机慧眼礼", MailBody: "恭喜道友在群仙问道赛神机竞猜中料事如神达到领奖门槛！特赐绑定修炼物资与慧眼识仙称号。", Desc: "免费神机竞猜命中达标奖励(绑定自用)"},
	}
	cat, _ := Build(rule, rewards)
	return cat
}

func (c *Catalog) Rule() *tablepb.TournamentRuleTable {
	if c == nil || c.rule == nil {
		return DefaultFallback().rule
	}
	return c.rule
}

func (c *Catalog) RewardByID(id uint32) (*tablepb.TournamentRewardTable, bool) {
	if c == nil {
		return nil, false
	}
	r, ok := c.rewardsByID[id]
	return r, ok
}

// RewardForPlacement 返回名次对应的最高一档奖励;不在前 16 且完赛达标则返回参赛保底档。
func (c *Catalog) RewardForPlacement(finalRank uint32, qualifierPlayed uint32) *tablepb.TournamentRewardTable {
	if c == nil {
		return nil
	}
	if finalRank == 3 && c.Rule().GetThirdPlaceMatchEnabled() {
		if rows := c.rewardsByTier[uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_THIRD_PLACE)]; len(rows) > 0 {
			return rows[0]
		}
	}
	if finalRank > 0 {
		for _, r := range c.orderedRewards {
			if r.GetTierKind() == uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_THIRD_PLACE) && !c.Rule().GetThirdPlaceMatchEnabled() {
				continue
			}
			if r.GetMinRank() > 0 && finalRank >= r.GetMinRank() && finalRank <= r.GetMaxRank() {
				return r
			}
		}
	}
	if qualifierPlayed >= c.Rule().GetParticipationMinMatches() {
		if rows := c.rewardsByTier[uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_PARTICIPANT)]; len(rows) > 0 {
			return rows[0]
		}
	}
	return nil
}

// CheerReward 返回免费助威命中的纪念奖励档。
func (c *Catalog) CheerReward() *tablepb.TournamentRewardTable {
	if c == nil {
		return nil
	}
	if rows := c.rewardsByTier[uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_CHEER)]; len(rows) > 0 {
		return rows[0]
	}
	return nil
}

// PredictionReward 返回神机竞猜命中达标的绑定纪念礼档。
func (c *Catalog) PredictionReward() *tablepb.TournamentRewardTable {
	if c == nil {
		return nil
	}
	if rows := c.rewardsByTier[uint32(matchpb.TournamentRewardTierKind_TOURNAMENT_REWARD_TIER_PREDICTION)]; len(rows) > 0 {
		return rows[0]
	}
	return nil
}

// RewardTierViews 输出面向客户端展示的全档位奖励只读视图。
func (c *Catalog) RewardTierViews() []*matchpb.TournamentRewardTierView {
	if c == nil {
		return nil
	}
	out := make([]*matchpb.TournamentRewardTierView, 0, len(c.orderedRewards))
	for _, r := range c.orderedRewards {
		v := &matchpb.TournamentRewardTierView{
			TierId:     r.GetId(),
			TierKind:   matchpb.TournamentRewardTierKind(r.GetTierKind()),
			MinRank:    r.GetMinRank(),
			MaxRank:    r.GetMaxRank(),
			GoldAmount: r.GetGoldAmount(),
			TitleCode:  r.GetTitleCode(),
			BadgeCode:  r.GetBadgeCode(),
			Desc:       r.GetDesc(),
		}
		ids := r.GetItemIds()
		cnts := r.GetItemCounts()
		for i := 0; i < len(ids) && i < len(cnts); i++ {
			if ids[i] != 0 && cnts[i] != 0 {
				v.Items = append(v.Items, &matchpb.TournamentRewardItem{ItemId: ids[i], Count: cnts[i]})
			}
		}
		out = append(out, v)
	}
	return out
}

// BuildMailAttachments 构造确定性邮件附件(银两 + 仅自用绑定物品),供 MailAdmin.SendPersonalMail 幂等投递。
func BuildMailAttachments(r *tablepb.TournamentRewardTable) (*mailpb.MailAttachments, []byte, error) {
	att := &mailpb.MailAttachments{}
	if r != nil {
		if r.GetGoldAmount() > 0 {
			att.Currencies = append(att.Currencies, &mailpb.MailCurrency{
				CurrencyType: GoldCurrencyType,
				Amount:       r.GetGoldAmount(),
			})
		}
		ids := r.GetItemIds()
		cnts := r.GetItemCounts()
		for i := 0; i < len(ids) && i < len(cnts); i++ {
			if ids[i] != 0 && cnts[i] != 0 {
				att.Items = append(att.Items, &mailpb.MailItem{
					ConfigId: ids[i],
					Count:    cnts[i],
				})
			}
		}
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(att)
	return att, raw, err
}

// BuildSaluteMailAttachments 构造每日免费观道致敬的绑定自用邮件附件(仅绑定银两 + 绑定道具)。
func BuildSaluteMailAttachments(rule *tablepb.TournamentRuleTable) (*mailpb.MailAttachments, uint64, []*matchpb.TournamentRewardItem) {
	att := &mailpb.MailAttachments{}
	var gold uint64
	var items []*matchpb.TournamentRewardItem
	if rule != nil {
		gold = uint64(rule.GetSaluteGoldAmount())
		if gold > 0 {
			att.Currencies = append(att.Currencies, &mailpb.MailCurrency{
				CurrencyType: GoldCurrencyType,
				Amount:       gold,
			})
		}
		if rule.GetSaluteItemId() != 0 && rule.GetSaluteItemCount() != 0 {
			att.Items = append(att.Items, &mailpb.MailItem{
				ConfigId: rule.GetSaluteItemId(),
				Count:    rule.GetSaluteItemCount(),
			})
			items = append(items, &matchpb.TournamentRewardItem{
				ItemId: rule.GetSaluteItemId(),
				Count:  rule.GetSaluteItemCount(),
			})
		}
	}
	return att, gold, items
}
