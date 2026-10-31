package svc

import (
	"context"
	"errors"
	"testing"

	"shared/playerredis"

	"github.com/alicebob/miniredis/v2"
)

// 玩家域、登录域句柄走生产的构造器:不带 hash tag 的键在发送前就被守卫拒绝,单实例开发时同样生效。
func TestDomainRedisClientCarriesTheGuard(t *testing.T) {
	mr := miniredis.RunT(t)
	client := mustNewDomainRedis(context.Background(), "LoginRedis", playerredis.Config{Addrs: []string{mr.Addr()}})
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Get(context.Background(), "login_session:untagged").Err(); !errors.Is(err, playerredis.ErrUntaggedKey) {
		t.Fatalf("不带 tag 的键 err = %v, want ErrUntaggedKey", err)
	}
}

func TestDomainRedisClientFailsStartupWhenUnreachable(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close()
	defer func() {
		if recover() == nil {
			t.Fatal("连不上必须起服即失败")
		}
	}()
	mustNewDomainRedis(context.Background(), "PlayerRedis", playerredis.Config{Addrs: []string{addr}})
}

func TestDomainRedisClientFailsStartupOnBadConfig(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("配置非法必须起服即失败")
		}
	}()
	mustNewDomainRedis(context.Background(), "LoginRedis", playerredis.Config{Addrs: []string{"a:1", "b:2"}})
}
