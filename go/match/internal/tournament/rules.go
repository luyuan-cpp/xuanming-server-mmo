package tournament

import (
	"encoding/binary"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MinSquadNameRunes = 2
	MaxSquadNameRunes = 12
)

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
	SquadID            uint64
	SeedRank           uint32
	SurvivingUnits     uint32
	RemainingHpBpsSum  uint64 // 每名存活单位剩余血量万分比之和(0..10000*N)
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
