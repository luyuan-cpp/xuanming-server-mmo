package puzzle

import (
	"errors"
	"math/bits"
	"strings"
	"unicode"
	"unicode/utf8"

	battlepb "proto/battle"
)

const (
	WorkshopDungeonID uint32 = 314
	MaxPuzzlesPerPlayer      = 10
	DefaultListLimit  uint32 = 20
	MaxListLimit      uint32 = 30

	MinTitleRunes = 4
	MaxTitleRunes = 20
	MaxHintRunes  = 40

	MaxTagBitCount = 6
	MaxTagMask     = uint32((1 << MaxTagBitCount) - 1)
	MaxTagsPerItem = 2

	MinRoundLimit uint32 = 1
	MaxRoundLimit uint32 = 6

	MinCharPresetID uint32 = 101
	MaxCharPresetID uint32 = 103
	MinPetPresetID  uint32 = 201
	MaxPetPresetID  uint32 = 203
	MaxFormationID  uint32 = 5

	MinPlayerHpPct    uint32 = 15
	MaxPlayerHpPct    uint32 = 100
	MinPlayerMpPct    uint32 = 20
	MaxPlayerMpPct    uint32 = 100
	MaxPlayerShield   uint32 = 50
	MaxPlayerDebuff   uint32 = 3

	MinEnemies       = 1
	MaxEnemies       = 3
	MinMonsterID     uint32 = 311
	MaxMonsterID     uint32 = 316
	MinEnemyHpPct    uint32 = 10
	MaxEnemyHpPct    uint32 = 150
	MaxEnemyShield   uint32 = 60
	MinSpeedDeltaPct int32  = -30
	MaxSpeedDeltaPct int32  = 30
	MaxEnemyBuff     uint32 = 4

	StateDraft     uint32 = 1
	StatePublished uint32 = 2
)

var (
	ErrSpecInvalid = errors.New("tactical puzzle spec invalid")
	ErrTextInvalid = errors.New("tactical puzzle text invalid")
)

// ValidateSpec 强校验残局题面全部白名单参数，与 C++ tactical_puzzle_rules.h 逐项同源。
func ValidateSpec(spec *battlepb.TacticalPuzzleSpec) error {
	if spec == nil {
		return ErrSpecInvalid
	}
	if spec.GetRoundLimit() < MinRoundLimit || spec.GetRoundLimit() > MaxRoundLimit {
		return ErrSpecInvalid
	}
	if spec.GetCharacterPresetId() < MinCharPresetID || spec.GetCharacterPresetId() > MaxCharPresetID {
		return ErrSpecInvalid
	}
	pet := spec.GetPetPresetId()
	if pet != 0 && (pet < MinPetPresetID || pet > MaxPetPresetID) {
		return ErrSpecInvalid
	}
	if spec.GetFormationId() > MaxFormationID {
		return ErrSpecInvalid
	}
	if spec.GetPlayerInitialHpPct() < MinPlayerHpPct || spec.GetPlayerInitialHpPct() > MaxPlayerHpPct {
		return ErrSpecInvalid
	}
	if spec.GetPlayerInitialMpPct() < MinPlayerMpPct || spec.GetPlayerInitialMpPct() > MaxPlayerMpPct {
		return ErrSpecInvalid
	}
	if spec.GetPlayerInitialShieldPct() > MaxPlayerShield {
		return ErrSpecInvalid
	}
	if spec.GetPlayerInitialDebuffKind() > MaxPlayerDebuff {
		return ErrSpecInvalid
	}
	enemies := spec.GetEnemies()
	if len(enemies) < MinEnemies || len(enemies) > MaxEnemies {
		return ErrSpecInvalid
	}
	for _, enemy := range enemies {
		if enemy == nil {
			return ErrSpecInvalid
		}
		if enemy.GetMonsterTableId() < MinMonsterID || enemy.GetMonsterTableId() > MaxMonsterID {
			return ErrSpecInvalid
		}
		if enemy.GetInitialHpPct() < MinEnemyHpPct || enemy.GetInitialHpPct() > MaxEnemyHpPct {
			return ErrSpecInvalid
		}
		if enemy.GetInitialShieldPct() > MaxEnemyShield {
			return ErrSpecInvalid
		}
		if enemy.GetSpeedDeltaPct() < MinSpeedDeltaPct || enemy.GetSpeedDeltaPct() > MaxSpeedDeltaPct {
			return ErrSpecInvalid
		}
		if enemy.GetInitialBuffKind() > MaxEnemyBuff {
			return ErrSpecInvalid
		}
	}
	return nil
}

// ValidateTagMask 校验标签掩码仅含合法 6 位且至多选 2 项。
func ValidateTagMask(mask uint32) error {
	if mask&^MaxTagMask != 0 {
		return ErrSpecInvalid
	}
	if bits.OnesCount32(mask) > MaxTagsPerItem {
		return ErrSpecInvalid
	}
	return nil
}

// NormalizeAndValidateText 清理首尾空白并拒绝控制字符与超长/过短文本。
func NormalizeAndValidateText(title, hint string) (string, string, error) {
	t := strings.TrimSpace(title)
	h := strings.TrimSpace(hint)
	if !utf8.ValidString(t) || !utf8.ValidString(h) {
		return "", "", ErrTextInvalid
	}
	if hasControlRune(t) || hasControlRune(h) {
		return "", "", ErrTextInvalid
	}
	titleLen := utf8.RuneCountInString(t)
	if titleLen < MinTitleRunes || titleLen > MaxTitleRunes {
		return "", "", ErrTextInvalid
	}
	if utf8.RuneCountInString(h) > MaxHintRunes {
		return "", "", ErrTextInvalid
	}
	return t, h, nil
}

func hasControlRune(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// DeterministicSeed 按 (puzzleID, specRevision) 确定性派生非零战斗 RNG 种子（SplitMix64）。
func DeterministicSeed(puzzleID uint64, specRevision uint32) uint64 {
	x := puzzleID ^ (uint64(specRevision) * 0x9E3779B97F4A7C15) ^ 0xA0761D6478BD642F
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	x = x ^ (x >> 31)
	if x == 0 {
		return 1
	}
	return x
}

// DeriveHonor 派生纯展示零属性荣誉档位与称谓。
func DeriveHonor(published, cleared, likes uint32) (uint32, string) {
	switch {
	case cleared >= 30 || (published >= 3 && likes >= 20):
		return 4, "奇局宗师"
	case cleared >= 15 || (published >= 1 && likes >= 5):
		return 3, "破局国手"
	case cleared >= 5 || published >= 1:
		return 2, "妙手破局"
	case cleared >= 1:
		return 1, "初试棋局"
	default:
		return 0, ""
	}
}
