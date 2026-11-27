package puzzle

import (
	"testing"

	battlepb "proto/battle"
)

func validSampleSpec() *battlepb.TacticalPuzzleSpec {
	return &battlepb.TacticalPuzzleSpec{
		RoundLimit:              3,
		CharacterPresetId:       101,
		PetPresetId:             0,
		FormationId:             2,
		PlayerInitialHpPct:      60,
		PlayerInitialMpPct:      80,
		PlayerInitialShieldPct:  20,
		PlayerInitialDebuffKind: 2,
		Enemies: []*battlepb.TacticalPuzzleEnemySpec{
			{
				MonsterTableId:   311,
				InitialHpPct:     80,
				InitialShieldPct: 30,
				SpeedDeltaPct:    -15,
				InitialBuffKind:  1,
			},
			{
				MonsterTableId:   316,
				InitialHpPct:     120,
				InitialShieldPct: 0,
				SpeedDeltaPct:    25,
				InitialBuffKind:  4,
			},
		},
	}
}

func TestValidateSpecAndText(t *testing.T) {
	if err := ValidateSpec(validSampleSpec()); err != nil {
		t.Fatalf("expected valid spec, got %v", err)
	}
	bad := validSampleSpec()
	bad.RoundLimit = 7
	if err := ValidateSpec(bad); err == nil {
		t.Fatalf("expected round limit > 6 rejected")
	}
	bad = validSampleSpec()
	bad.PetPresetId = 204
	if err := ValidateSpec(bad); err == nil {
		t.Fatalf("expected pet preset 204 rejected")
	}
	bad = validSampleSpec()
	bad.Enemies[0].MonsterTableId = 101
	if err := ValidateSpec(bad); err == nil {
		t.Fatalf("expected non-puzzle monster 101 rejected")
	}
	bad = validSampleSpec()
	bad.Enemies[0].SpeedDeltaPct = 35
	if err := ValidateSpec(bad); err == nil {
		t.Fatalf("expected speed delta 35 rejected")
	}

	if err := ValidateTagMask((1 << 0) | (1 << 5)); err != nil {
		t.Fatalf("expected 2 tags valid, got %v", err)
	}
	if err := ValidateTagMask((1 << 0) | (1 << 1) | (1 << 2)); err == nil {
		t.Fatalf("expected 3 tags rejected")
	}
	if err := ValidateTagMask(1 << 6); err == nil {
		t.Fatalf("expected out-of-range tag bit rejected")
	}

	title, hint, err := NormalizeAndValidateText("  两回合破盾反杀  ", "  先驱散护盾再点杀  ")
	if err != nil || title != "两回合破盾反杀" || hint != "先驱散护盾再点杀" {
		t.Fatalf("unexpected normalize result: %q %q %v", title, hint, err)
	}
	if _, _, err = NormalizeAndValidateText("短题", ""); err == nil {
		t.Fatalf("expected 2-rune title rejected")
	}
	if _, _, err = NormalizeAndValidateText("正常题名\n换行", ""); err == nil {
		t.Fatalf("expected control char rejected")
	}
}

func TestDeterministicSeedAndHonor(t *testing.T) {
	s1 := DeterministicSeed(10001, 1)
	s2 := DeterministicSeed(10001, 1)
	s3 := DeterministicSeed(10001, 2)
	if s1 == 0 || s1 != s2 || s1 == s3 {
		t.Fatalf("unexpected seeds: %d %d %d", s1, s2, s3)
	}
	tier, name := DeriveHonor(3, 10, 20)
	if tier != 4 || name != "奇局宗师" {
		t.Fatalf("unexpected honor: %d %q", tier, name)
	}
	tier, name = DeriveHonor(0, 1, 0)
	if tier != 1 || name != "初试棋局" {
		t.Fatalf("unexpected honor: %d %q", tier, name)
	}
}
