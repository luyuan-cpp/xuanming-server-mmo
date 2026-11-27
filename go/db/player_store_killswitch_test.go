package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	storepb "proto/player_store"
	"shared/killswitch"
)

// 热关停只挡新工作、不挡收尾(docs/design/rpc-kill-switch.md §8;数据覆写审计 r2 第 4 轮 r2-ops#2):
// 运维写 `*`、类型通配或服务通配止血时,存盘照常进来,进场认领被挡;精确到方法的规则仍能关存盘。
func TestPlayerStoreKillSwitchKeepsSaveOpenUnderWildcards(t *testing.T) {
	typeScope := "type/" + playerStoreNodeTypeName() + "/"
	wildcards := []map[string]killswitch.Rule{
		{"*": {Deny: true}},
		{typeScope + "*": {Deny: true}},
		{"playerstore.PlayerStore/*": {Deny: true}},
		{"PlayerStore/*": {Deny: true}},
	}
	for _, rules := range wildcards {
		ks := newPlayerStoreKillSwitch()
		ks.SetRules(rules)
		_, blocked := ks.Lookup(storepb.PlayerStore_SavePlayer_FullMethodName)
		require.False(t, blocked, "wildcard %v must not block SavePlayer", rules)
		_, blocked = ks.Lookup(storepb.PlayerStore_LoadAndClaim_FullMethodName)
		require.True(t, blocked, "wildcard %v still blocks LoadAndClaim (new work)", rules)
	}

	for _, exact := range []string{"playerstore.PlayerStore/SavePlayer", typeScope + "PlayerStore/SavePlayer"} {
		ks := newPlayerStoreKillSwitch()
		ks.SetRules(map[string]killswitch.Rule{exact: {Deny: true}})
		_, blocked := ks.Lookup(storepb.PlayerStore_SavePlayer_FullMethodName)
		require.True(t, blocked, "exact rule %s can still stop SavePlayer", exact)
	}
}
