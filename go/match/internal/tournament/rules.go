package tournament

import (
	"encoding/binary"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	battlepb "proto/battle"
)

const (
	MinSquadNameRunes        = 2
	MaxSquadNameRunes        = 12
	MinSpectateCheerPhraseID = 1
	MaxSpectateCheerPhraseID = 6
	DefaultIntermissionSec   = 60
	MinSquadEmblemID         = 1
	MaxSquadEmblemID         = 8
	DefaultEmblemID          = 1

	// V4 模块 J：战队赛季荣耀铭牌位图（纯荣耀无属性加成，单调累加）。
	HonorBitDebut    uint32 = 1 << 0 // 问道初成（完赛达标）
	HonorBitStreak   uint32 = 1 << 1 // 连胜破竹（海选 >=3 胜）
	HonorBitUnbeaten uint32 = 1 << 2 // 不败金身（海选 >=3 场且 0 负）
	HonorBitComeback uint32 = 1 << 3 // 逆转乾坤（赢下打满 3 局的 BO3 生死战）
	HonorBitSweep    uint32 = 1 << 4 // 零封强敌（BO3 以 2:0 完胜）
	HonorBitChampion uint32 = 1 << 5 // 问鼎群仙（夺得本季全服总冠军）
)

var utc8Location = time.FixedZone("UTC+8", 8*3600)

// NormalizeSquadName 校验并归一化战队名称:
//   - 去首尾空白,长度须在 [2, 12] Unicode 字符内;
//   - 禁止任何控制字符、换行或连续不可见符;
//   - 返回展示名 display 与同季唯一归一化键 norm(全小写去所有空白)。
func NormalizeSquadName(raw string) (display string, norm string, ok bool) {
	trimmed := strings.TrimSpace(raw)
	runeCount := utf8.RuneCountInString(trimmed)
	if runeCount < MinSquadNameRunes || runeCount > MaxSquadNameRunes {
		return "", "", false
	}
	var normBuilder strings.Builder
	normBuilder.Grow(len(trimmed))
	for _, r := range trimmed {
		if unicode.IsControl(r) || r == '\uFFFD' {
			return "", "", false
		}
		if unicode.IsSpace(r) {
			continue
		}
		normBuilder.WriteRune(unicode.ToLower(r))
	}
	norm = normBuilder.String()
	if utf8.RuneCountInString(norm) < MinSquadNameRunes {
		return "", "", false
	}
	return trimmed, norm, true
}

// KnockoutBracketSize 按海选有效战队数动态计算单败淘汰赛规模(低人口冷启动绝不流产):
//   - < 2 队: 返回 0(跳过淘汰赛直接按参与结算发保底);
//   - 2..3 队: 2 强直接决赛;
//   - 4..7 队: 4 强;
//   - 8..15 队: 8 强;
//   - >=16 队: 取不超过 maxTeams 的最大 2 的幂(上限 16)。
func KnockoutBracketSize(qualifiedCount int, maxTeams uint32) int {
	if qualifiedCount < 2 {
		return 0
	}
	capLimit := int(maxTeams)
	if capLimit < 2 {
		capLimit = 2
	}
	if capLimit > 16 {
		capLimit = 16
	}
	effective := qualifiedCount
	if effective > capLimit {
		effective = capLimit
	}
	size := 2
	for size*2 <= effective {
		size *= 2
	}
	// 例如 3 支队 -> 取 4 强并由 1 号种子首轮轮空,让第 2/3 名都有淘汰赛可打;不超过 capLimit。
	if effective > size && size*2 <= capLimit {
		size *= 2
	}
	return size
}

// TotalKnockoutRounds 返回 bracketSize(2/4/8/16) 对应的淘汰赛总轮数(1/2/3/4)。
func TotalKnockoutRounds(bracketSize int) int {
	rounds := 0
	for s := bracketSize; s >= 2; s /= 2 {
		rounds++
	}
	return rounds
}

// SeedPairs 返回标准单败淘汰赛首轮对阵种子对(1-based 种子号),确保 1 号与 2 号种子分守上下半区。
func SeedPairs(bracketSize int) [][2]int {
	switch bracketSize {
	case 2:
		return [][2]int{{1, 2}}
	case 4:
		return [][2]int{{1, 4}, {2, 3}}
	case 8:
		return [][2]int{{1, 8}, {4, 5}, {2, 7}, {3, 6}}
	case 16:
		return [][2]int{
			{1, 16}, {8, 9}, {4, 13}, {5, 12},
			{2, 15}, {7, 10}, {3, 14}, {6, 11},
		}
	default:
		return nil
	}
}

// StandingEntry 海选积分榜确定性排序元素。
type StandingEntry struct {
	SquadID         uint64
	QualifierPoints uint32
	QualifierWins   uint32
	QualifierDraws  uint32
	RegisteredMs    uint64
}

// SortStandings 按确定性规则排海选名次:
// 积分降序 -> 胜场降序 -> 平局降序 -> 锁定报名时刻升序 -> SquadID 升序(仅作最终兜底平手)。
func SortStandings(entries []StandingEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.QualifierPoints != b.QualifierPoints {
			return a.QualifierPoints > b.QualifierPoints
		}
		if a.QualifierWins != b.QualifierWins {
			return a.QualifierWins > b.QualifierWins
		}
		if a.QualifierDraws != b.QualifierDraws {
			return a.QualifierDraws > b.QualifierDraws
		}
		if a.RegisteredMs != b.RegisteredMs {
			return a.RegisteredMs < b.RegisteredMs
		}
		return a.SquadID < b.SquadID
	})
}

// KnockoutTieMetrics 淘汰赛打满回合判平时的四级确定性决胜事实。
type KnockoutTieMetrics struct {
	SquadID           uint64
	SeedRank          uint32
	SurvivingUnits    uint32
	RemainingHpBpsSum uint64 // 每名存活单位剩余血量万分比之和(0..10000*N)
}

// ResolveKnockoutDrawWinner 淘汰赛平局确定性决胜,返回 0 表示 A 方晋级,1 表示 B 方晋级。
// 优先级: 存活单位多 -> 剩余血量比例和更高 -> 海选种子顺位高(数字小) -> SquadID 小兜底。
func ResolveKnockoutDrawWinner(sideA, sideB KnockoutTieMetrics) int {
	if sideA.SurvivingUnits != sideB.SurvivingUnits {
		if sideA.SurvivingUnits > sideB.SurvivingUnits {
			return 0
		}
		return 1
	}
	if sideA.RemainingHpBpsSum != sideB.RemainingHpBpsSum {
		if sideA.RemainingHpBpsSum > sideB.RemainingHpBpsSum {
			return 0
		}
		return 1
	}
	if sideA.SeedRank != sideB.SeedRank && sideA.SeedRank > 0 && sideB.SeedRank > 0 {
		if sideA.SeedRank < sideB.SeedRank {
			return 0
		}
		return 1
	}
	if sideA.SquadID <= sideB.SquadID {
		return 0
	}
	return 1
}

// RankForEliminatedRound 计算在淘汰赛第 roundIndex(1-based) 轮落败的战队所获最终名次下界:
// 例: bracketSize=16 时,R1(16进8)败=第9名(十六强),R2(8进4)败=第5名(八强),R3(半决赛)败=第3名(四强),R4(决赛)败=第2名(亚军)。
func RankForEliminatedRound(bracketSize, roundIndex int) uint32 {
	total := TotalKnockoutRounds(bracketSize)
	if total == 0 || roundIndex < 1 || roundIndex > total {
		return 0
	}
	remaining := bracketSize
	for r := 1; r < roundIndex; r++ {
		remaining /= 2
	}
	// 本轮结束后剩余 remaining/2 支胜者,本轮败者并列第 (remaining/2 + 1) 名。
	return uint32(remaining/2 + 1)
}

// EncodeLineup 将出战阵容 player_id 列表确定性编码为紧凑小端字节序列。
func EncodeLineup(ids []uint64) []byte {
	if len(ids) == 0 {
		return nil
	}
	buf := make([]byte, len(ids)*8)
	for i, id := range ids {
		binary.LittleEndian.PutUint64(buf[i*8:], id)
	}
	return buf
}

// DecodeLineup 解码 EncodeLineup 产出的成员 ID 列表;非法长度安全返回 nil。
func DecodeLineup(raw []byte) []uint64 {
	if len(raw) == 0 || len(raw)%8 != 0 {
		return nil
	}
	out := make([]uint64, len(raw)/8)
	for i := range out {
		out[i] = binary.LittleEndian.Uint64(raw[i*8:])
	}
	return out
}

// BestOfForKnockout 判定淘汰赛某一轮是否为 BO3(半决赛与总决赛,即该轮参赛队数 remaining <= 4)。
func BestOfForKnockout(bracketSize, roundIndex int, bo3Top4Enabled bool) uint32 {
	if !bo3Top4Enabled || bracketSize < 2 || roundIndex < 1 {
		return 1
	}
	remaining := bracketSize >> (roundIndex - 1)
	if remaining >= 2 && remaining <= 4 {
		return 3
	}
	return 1
}

func splitMix64(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// DrawLeylines 为一场 BO3 淘汰赛确定性无放回抽取五行灵脉顺序(与 Java 侧逐字节同源)。
func DrawLeylines(seasonID uint32, matchID uint64, bestOf uint32) []battlepb.TournamentLeylineElement {
	return DrawLeylinesWithBans(seasonID, matchID, bestOf, 0, 0)
}

// IsValidLeylineElement 校验五行灵脉枚举是否为 1..5(金/木/水/火/土)。
func IsValidLeylineElement(e battlepb.TournamentLeylineElement) bool {
	return e >= battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_METAL &&
		e <= battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_EARTH
}

// DrawLeylinesWithBans V4 模块 H：从五行灵脉池中剔除双方队长禁选(0 表示未禁)，再以 SplitMix64 确定性无放回抽取 bestOf 条灵脉(与 Java 逐字节同源)。
func DrawLeylinesWithBans(seasonID uint32, matchID uint64, bestOf uint32, bannedA, bannedB battlepb.TournamentLeylineElement) []battlepb.TournamentLeylineElement {
	if bestOf <= 1 {
		return nil
	}
	all := []battlepb.TournamentLeylineElement{
		battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_METAL,
		battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_WOOD,
		battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_WATER,
		battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_FIRE,
		battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_EARTH,
	}
	pool := make([]battlepb.TournamentLeylineElement, 0, len(all))
	for _, e := range all {
		if (IsValidLeylineElement(bannedA) && e == bannedA) || (IsValidLeylineElement(bannedB) && e == bannedB) {
			continue
		}
		pool = append(pool, e)
	}
	if len(pool) == 0 {
		pool = all
	}
	count := int(bestOf)
	if count > len(pool) {
		count = len(pool)
	}
	state := (uint64(seasonID) << 32) ^ matchID
	if IsValidLeylineElement(bannedA) || IsValidLeylineElement(bannedB) {
		state ^= (uint64(bannedA) << 56) ^ (uint64(bannedB) << 48)
	}
	out := make([]battlepb.TournamentLeylineElement, 0, count)
	for i := 0; i < count; i++ {
		state = splitMix64(state + uint64(i+1))
		rem := len(pool) - i
		pick := i + int(state%uint64(rem))
		pool[i], pool[pick] = pool[pick], pool[i]
		out = append(out, pool[i])
	}
	return out
}

// ComputeQualifierHonorBits V4 模块 J：海选每场结束后单调累加战队荣耀铭牌(问道初成/连胜破竹/不败金身)。
func ComputeQualifierHonorBits(existing, played, wins, losses, minParticipation uint32) uint32 {
	bits := existing
	if minParticipation == 0 {
		minParticipation = 2
	}
	if played >= minParticipation {
		bits |= HonorBitDebut
	}
	if wins >= 3 {
		bits |= HonorBitStreak
	}
	if played >= 3 && losses == 0 {
		bits |= HonorBitUnbeaten
	}
	return bits
}

// ComputeKnockoutSeriesHonorBits V4 模块 J：淘汰赛大场决胜后单调累加胜方战队荣耀铭牌(逆转乾坤/零封强敌/问鼎群仙)。
func ComputeKnockoutSeriesHonorBits(existing, bestOf, winnerWins, loserWins uint32, isChampion bool) uint32 {
	bits := existing
	if bestOf >= 3 {
		if winnerWins+loserWins >= 3 {
			bits |= HonorBitComeback
		}
		if winnerWins >= 2 && loserWins == 0 {
			bits |= HonorBitSweep
		}
	}
	if isChampion {
		bits |= HonorBitChampion
	}
	return bits
}

// ClassifyHighlightTag V4 模块 I：为已完赛的公开淘汰赛判定名局录标签与确定性排序优先级(0 最高)。
func ClassifyHighlightTag(isThirdPlace bool, roundIndex, maxRound, bestOf, winsA, winsB uint32) (tag uint32, label string, priority int) {
	if !isThirdPlace && maxRound > 0 && roundIndex >= maxRound {
		return 1, "总决赛巅峰局", 0
	}
	if bestOf >= 3 && winsA+winsB >= 3 {
		return 2, "决胜三局激战", 1
	}
	if isThirdPlace {
		return 3, "季军争夺战", 2
	}
	return 4, "高光问鼎局", 3
}

// EncodeLeylineSequence 将灵脉枚举序列编码为逗号分隔整数串(如 "1,4,2")存入 SQL。
func EncodeLeylineSequence(seq []battlepb.TournamentLeylineElement) string {
	if len(seq) == 0 {
		return ""
	}
	parts := make([]string, 0, len(seq))
	for _, e := range seq {
		if e != battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_UNSPECIFIED {
			parts = append(parts, strconv.Itoa(int(e)))
		}
	}
	return strings.Join(parts, ",")
}

// DecodeLeylineSequence 解析 SQL 持久化的灵脉序列。
func DecodeLeylineSequence(raw string) []battlepb.TournamentLeylineElement {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, ",")
	out := make([]battlepb.TournamentLeylineElement, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 || n > 5 {
			continue
		}
		out = append(out, battlepb.TournamentLeylineElement(n))
	}
	return out
}

// LeylineForGame 返回 1-based 小局对应的灵脉;不存在返回 UNSPECIFIED。
func LeylineForGame(seq []battlepb.TournamentLeylineElement, gameIndex uint32) battlepb.TournamentLeylineElement {
	if gameIndex == 0 || int(gameIndex) > len(seq) {
		return battlepb.TournamentLeylineElement_TOURNAMENT_LEYLINE_UNSPECIFIED
	}
	return seq[gameIndex-1]
}

// Utc8DayKey 将 Unix 毫秒转换为 UTC+8 自然日键 YYYYMMDD(每日免费观道致敬重置口径)。
func Utc8DayKey(unixMs uint64) uint32 {
	t := time.UnixMilli(int64(unixMs)).In(utc8Location)
	return uint32(t.Year()*10000 + int(t.Month())*100 + t.Day())
}

// IsValidSpectateCheerPhrase 校验观战预设喝彩白名单 ID(严格 1..6,零自由文本)。
func IsValidSpectateCheerPhrase(phraseID uint32) bool {
	return phraseID >= MinSpectateCheerPhraseID && phraseID <= MaxSpectateCheerPhraseID
}

// IsValidSpectateCheerSide 校验喝彩支持阵营(0=A队, 1=B队)。
func IsValidSpectateCheerSide(side uint32) bool {
	return side == 0 || side == 1
}

// IsValidEmblemID 校验官方预设道家队徽白名单(1..8)。
func IsValidEmblemID(id uint32) bool {
	return id >= MinSquadEmblemID && id <= MaxSquadEmblemID
}

// NormalizeEmblemID 建队时将 0 归一化为默认 1 青剑;非 0 须落在 1..8。
func NormalizeEmblemID(id uint32) (uint32, bool) {
	if id == 0 {
		return DefaultEmblemID, true
	}
	if IsValidEmblemID(id) {
		return id, true
	}
	return 0, false
}

// PredictionWeightForRound 返回一场淘汰赛命中神机预测对应的慧眼分权重:
// 十六强=1,八强=2,半决赛=3,季军战=3,总决赛=5。
func PredictionWeightForRound(bracketSize, roundIndex int, isThirdPlace bool) uint32 {
	if isThirdPlace {
		return 3
	}
	total := TotalKnockoutRounds(bracketSize)
	if total == 0 || roundIndex < 1 || roundIndex > total {
		return 1
	}
	remaining := bracketSize >> (roundIndex - 1)
	switch remaining {
	case 2:
		return 5
	case 4:
		return 3
	case 8:
		return 2
	default:
		return 1
	}
}

// MatchMvpResult 单局权威问鼎之星(MVP)评定结果。
type MatchMvpResult struct {
	PlayerID  uint64
	TeamIndex uint32
	Score     uint32
	Damage    uint64
	Healing   uint64
	Controls  uint32
}

// ComputeMatchMvp 基于 BattleSettlementData 权威累计统计确定性评定本局 MVP(与 Java 侧逐条对齐)。
func ComputeMatchMvp(settlements []*battlepb.BattleSettlementData, winnerTeamIndex int) (MatchMvpResult, bool) {
	var best MatchMvpResult
	var bestFound bool
	var bestWin bool
	var bestAlive bool
	var bestContrib uint64
	for _, s := range settlements {
		if s == nil || s.GetPlayerId() == 0 {
			continue
		}
		dmg := s.GetTotalDamageDealt()
		heal := s.GetTotalHealingDone()
		ctrl := s.GetTotalControlsLanded()
		alive := !s.GetIsDead() && !s.GetFled()
		onWin := winnerTeamIndex >= 0 && int(s.GetPlayerTeamIndex()) == winnerTeamIndex
		raw := dmg/100 + heal/80 + uint64(ctrl)*60
		if alive {
			raw += 120
		}
		if onWin {
			raw += 180
		}
		if raw > 100000 {
			raw = 100000
		}
		score := uint32(raw)
		contrib := dmg + heal
		cand := MatchMvpResult{
			PlayerID:  s.GetPlayerId(),
			TeamIndex: s.GetPlayerTeamIndex(),
			Score:     score,
			Damage:    dmg,
			Healing:   heal,
			Controls:  ctrl,
		}
		if !bestFound ||
			cand.Score > best.Score ||
			(cand.Score == best.Score && contrib > bestContrib) ||
			(cand.Score == best.Score && contrib == bestContrib && onWin && !bestWin) ||
			(cand.Score == best.Score && contrib == bestContrib && onWin == bestWin && alive && !bestAlive) ||
			(cand.Score == best.Score && contrib == bestContrib && onWin == bestWin && alive == bestAlive && cand.PlayerID < best.PlayerID) {
			best = cand
			bestFound = true
			bestWin = onWin
			bestAlive = alive
			bestContrib = contrib
		}
	}
	return best, bestFound
}
