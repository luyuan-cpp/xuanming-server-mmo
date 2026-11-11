package clusterredis

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

// 控制集群的键策略(docs/design/redis-cluster-all-domains-20261110.md §3.1):单键可以不带 tag;带 tag 只能是登记过的族;
// 多键命令每个键都带 tag,且是同一个 tag。玩家域的 tag 形状发到控制集群一律拒绝。
func TestControlClusterKeysRules(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want error
	}{
		{"单键不带 tag(选主锁、去重键)", []string{"trade:assetop:maintenance:leader"}, nil},
		{"单键带控制族 tag", []string{"queue:{q:3}"}, nil},
		{"今天已带 tag 的 friend 键", []string{"friend:{f:7}:list:v3"}, nil},
		{"单键带玩家 tag", []string{"player:{42}:location"}, ErrWrongDomainTag},
		{"单键带 slot-tag", []string{"player:save_pending:{afps}"}, ErrWrongDomainTag},
		{"单键带没登记的族", []string{"x:{unknown:1}"}, ErrWrongDomainTag},
		{"多键同一个 tag", []string{"queue:{q:3}", "admitted:zset:{q:3}", "queue:meta:{q:3}:abc"}, nil},
		{"多键都不带 tag", []string{"a", "b"}, ErrUntaggedKey},
		{"多键有一个不带 tag", []string{"queue:{q:3}", "b"}, ErrUntaggedKey},
		{"多键同族不同身份", []string{"queue:{q:3}", "queue:{q:4}"}, ErrMixedTags},
		{"多键同身份不同族", []string{"x:{q:3}", "y:{gm:3}"}, ErrMixedTags},
		{"多键混进玩家 tag", []string{"x:{ctl:7}", "player:{42}:location"}, ErrWrongDomainTag},
		{"没有键(EVAL numkeys=0)", nil, nil},
	}
	for _, tc := range cases {
		err := ControlClusterKeys.CheckKeys(tc.keys)
		if (tc.want == nil) != (err == nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: %v → err = %v, want %v", tc.name, tc.keys, err, tc.want)
		}
	}
}

func TestPlayerDomainKeysRejectControlFamilyTags(t *testing.T) {
	if err := PlayerDomainKeys.CheckKeys([]string{"scene:{ctl:7}:1:node"}); !errors.Is(err, ErrWrongDomainTag) {
		t.Fatalf("控制族 tag 发到玩家域 err = %v,want ErrWrongDomainTag", err)
	}
	// {42} 在 slot 8000,{afps} 在 slot 0:两把都是玩家域 tag,不同槽照旧按 CROSSSLOT 拒绝。
	if err := PlayerDomainKeys.CheckKeys([]string{"player:{42}:location", "player:save_pending:{afps}"}); !errors.Is(err, ErrCrossSlot) {
		t.Fatalf("玩家域跨槽 err = %v,want ErrCrossSlot", err)
	}
	if err := PlayerDomainKeys.CheckKeys([]string{"player:{42}:location", "player:{42}:owner_epoch"}); err != nil {
		t.Fatalf("同一玩家的键应放行: %v", err)
	}
}

func TestNewRejectsUnknownKeyPolicy(t *testing.T) {
	mr := miniredis.RunT(t)
	if _, err := New(Config{Addrs: []string{mr.Addr()}}, keyPolicyCount); err == nil {
		t.Fatal("越界的键策略必须拒绝构造")
	}
}

// 守卫挂在线上:被拒的命令不到服务端,管道里的非幂等命令两个域都拒绝。
func TestControlClusterClientGuardsOnTheWire(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	c, err := New(Config{Addrs: []string{mr.Addr()}}, ControlClusterKeys)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if err := c.Set(ctx, "trade:assetop:maintenance:leader", "me", 0).Err(); err != nil {
		t.Fatalf("不带 tag 的单键应放行: %v", err)
	}
	if err := c.Set(ctx, "player:{42}:location", "x", 0).Err(); !errors.Is(err, ErrWrongDomainTag) {
		t.Fatalf("玩家域键发到控制集群 err = %v,want ErrWrongDomainTag", err)
	}
	if mr.Exists("player:{42}:location") {
		t.Fatal("被守卫拒绝的命令不得发到服务端")
	}
	if err := c.Eval(ctx, "return 1", []string{"queue:{q:1}", "queue:{q:2}"}).Err(); !errors.Is(err, ErrMixedTags) {
		t.Fatalf("多键脚本带不同 tag err = %v,want ErrMixedTags", err)
	}
	if err := c.Eval(ctx, "return 1", []string{"queue:{q:1}", "admitted:zset:{q:1}"}).Err(); err != nil {
		t.Fatalf("同一个 tag 的多键脚本应放行: %v", err)
	}

	pipe := c.Pipeline()
	pipe.Eval(ctx, "return 1", []string{"queue:{q:1}"})
	if _, err := pipe.Exec(ctx); !errors.Is(err, ErrNonIdempotentInPipeline) {
		t.Fatalf("管道里的 EVAL err = %v,want ErrNonIdempotentInPipeline", err)
	}
}
