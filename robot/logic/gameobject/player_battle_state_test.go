package gameobject

import "testing"

// InBattle 只比对局 id:同一会话连打两局也判得对;不是当前这局的结束(登录补推的上一局结算)不算结束。
func TestInBattleFollowsBattleIdsAcrossBattles(t *testing.T) {
	p := NewPlayer(1)
	if p.InBattle() {
		t.Fatalf("a fresh player must not be in battle")
	}
	p.SignalBattleStart(10)
	if !p.InBattle() {
		t.Fatalf("after BattleStart(10) the player must be in battle")
	}
	p.SignalBattleEnd(9, 1) // 上一局的陈旧结算:被 battle_id 过滤
	if !p.InBattle() {
		t.Fatalf("a stale BattleEnd(9) must not end battle 10")
	}
	p.SignalBattleEnd(10, 1)
	if p.InBattle() {
		t.Fatalf("after BattleEnd(10) the player must be out of battle")
	}
	// 第二局:一次性广播通道已经关过,InBattle 仍按 id 判。
	p.SignalBattleStart(11)
	if !p.InBattle() {
		t.Fatalf("after BattleStart(11) the player must be in battle again")
	}
	p.SignalBattleEnd(11, 2)
	if p.InBattle() {
		t.Fatalf("after BattleEnd(11) the player must be out of battle")
	}
}
