package playerredis

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"shared/clusterredis"
)

// 别名层只转发,不得改变玩家域的口径:错误是同一个值,New 挂的是玩家域守卫,CheckKeys / CheckCommand 按玩家域规则判。
// 实现本身的用例在 shared/clusterredis。
func TestAliasesKeepPlayerDomainSemantics(t *testing.T) {
	wrapped := fmt.Errorf("wrap: %w", clusterredis.ErrUntaggedKey)
	if !errors.Is(wrapped, ErrUntaggedKey) || !errors.Is(fmt.Errorf("wrap: %w", ErrCrossSlot), clusterredis.ErrCrossSlot) {
		t.Fatal("错误值必须与 clusterredis 是同一个,调用方的 errors.Is 才认")
	}
	if (GuardHook{}).Policy != clusterredis.PlayerDomainKeys {
		t.Fatalf("GuardHook 零值的策略 = %v,应是玩家域", (GuardHook{}).Policy)
	}

	if err := CheckKeys([]string{"player:{42}:location", "player:{42}:owner_epoch"}); err != nil {
		t.Fatalf("同一玩家的两把键应放行: %v", err)
	}
	if err := CheckKeys([]string{"world_channel_directory:zone:1:2"}); !errors.Is(err, ErrUntaggedKey) {
		t.Fatalf("不带 tag 的键 err = %v,want ErrUntaggedKey", err)
	}
	if err := CheckKeys([]string{"scene:{ctl:7}:1:node"}); !errors.Is(err, clusterredis.ErrWrongDomainTag) {
		t.Fatalf("控制集群的族 tag err = %v,want ErrWrongDomainTag", err)
	}
	if err := CheckCommand([]interface{}{"get", "scene:{ctl:7}:1:node"}); !errors.Is(err, clusterredis.ErrWrongDomainTag) {
		t.Fatalf("CheckCommand 也按玩家域判: err = %v", err)
	}

	ctx := context.Background()
	mr := miniredis.RunT(t)
	c, err := New(Config{Addrs: []string{mr.Addr()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Set(ctx, "player:{42}:location", "x", 0).Err(); err != nil {
		t.Fatalf("玩家域键应能写入: %v", err)
	}
	if err := c.Set(ctx, "scene:{ctl:7}:1:node", "x", 0).Err(); !errors.Is(err, clusterredis.ErrWrongDomainTag) {
		t.Fatalf("New 构造的客户端发控制集群键 err = %v,want ErrWrongDomainTag", err)
	}
	if mr.Exists("scene:{ctl:7}:1:node") {
		t.Fatal("被守卫拒绝的命令不得发到服务端")
	}
}
