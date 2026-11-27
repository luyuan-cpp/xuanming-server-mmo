package clusterredis

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"shared/redisslot"
)

// match 私有集群的键策略(docs/design/redis-cluster-all-domains-20261110.md §4.9):每个键都以 spectate: 开头、都带 hash tag,
// 多键命令同 slot,不出现控制集群的族 tag。键形取自 go/match/internal/logic 的 spectate_*.go。
func TestMatchPrivateKeysRules(t *testing.T) {
	if redisslot.HashSlot("spectate:tier:{1001}") == redisslot.HashSlot("spectate:tier:{1002}") {
		t.Fatal("夹具失效:两场战斗的键应落在不同 slot,换一对 battle_id")
	}
	cases := []struct {
		name string
		keys []string
		want error
	}{
		{"本场的单键", []string{"spectate:tier:{1001}"}, nil},
		{"本场的三把键同一个 tag(一条 DEL)", []string{"spectate:tier:{1001}", "spectate:relay_gates:{1001}", "spectate:featured:{1001}"}, nil},
		{"全服索引的桶", []string{"spectate:battles:active:{sba:3}"}, nil},
		{"玩家域的键走错句柄", []string{"player:{1001}:location"}, ErrNotMatchPrivateKey},
		{"多键里混进玩家域的键", []string{"spectate:tier:{1001}", "player:{1001}:location"}, ErrNotMatchPrivateKey},
		{"不带 tag", []string{"spectate:tier:1001"}, ErrUntaggedKey},
		{"两场战斗的键进同一条命令", []string{"spectate:tier:{1001}", "spectate:tier:{1002}"}, ErrCrossSlot},
		{"控制集群的族 tag", []string{"spectate:tier:{ctl:7}"}, ErrWrongDomainTag},
		{"没有键(EVAL numkeys=0)", nil, nil},
	}
	for _, tc := range cases {
		err := MatchPrivateKeys.CheckKeys(tc.keys)
		if (tc.want == nil) != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: %v → err = %v, want %v", tc.name, tc.keys, err, tc.want)
		}
	}
}

// 守卫挂在线上:被拒的命令不到服务端;管道里的非幂等命令同样拒绝。
func TestMatchPrivateClientGuardsOnTheWire(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	c, err := New(Config{Addrs: []string{mr.Addr()}}, MatchPrivateKeys)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if err := c.Set(ctx, "spectate:tier:{1001}", "1", 0).Err(); err != nil {
		t.Fatalf("match 私有键应放行: %v", err)
	}
	if err := c.Set(ctx, "player:{1001}:location", "x", 0).Err(); !errors.Is(err, ErrNotMatchPrivateKey) {
		t.Fatalf("玩家域键发到 match 私有集群 err = %v,want ErrNotMatchPrivateKey", err)
	}
	if mr.Exists("player:{1001}:location") {
		t.Fatal("被守卫拒绝的命令不得发到服务端")
	}

	pipe := c.Pipeline()
	pipe.Eval(ctx, "return 1", []string{"spectate:tier:{1001}"})
	if _, err := pipe.Exec(ctx); !errors.Is(err, ErrNonIdempotentInPipeline) {
		t.Fatalf("管道里的 EVAL err = %v,want ErrNonIdempotentInPipeline", err)
	}
}
