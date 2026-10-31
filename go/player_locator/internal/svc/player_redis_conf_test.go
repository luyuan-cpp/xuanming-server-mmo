package svc

import (
	"testing"

	"player_locator/internal/config"
	"shared/playerredis"
)

// 没写 PlayerRedis 段:指向 RedisClient 这个单实例,沿用它的口令、连接池与 go-redis 缺省超时。
func TestPlayerRedisConf_DefaultsToRedisClient(t *testing.T) {
	var c config.Config
	c.RedisClient = config.RedisConf{Host: "127.0.0.1:6379", Password: "secret", PoolSize: 20}

	got, err := playerRedisConf(c)
	if err != nil {
		t.Fatalf("playerRedisConf: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("缺省配置必须能过校验: %v", err)
	}
	if got.Cluster || len(got.Addrs) != 1 || got.Addrs[0] != "127.0.0.1:6379" {
		t.Errorf("缺省应是这个单实例,得到 addrs=%v cluster=%v", got.Addrs, got.Cluster)
	}
	if got.Password != "secret" || got.PoolSizePerNode != 20 {
		t.Errorf("缺省应沿用口令与连接池,得到 pool=%d", got.PoolSizePerNode)
	}
}

// 连接池上限超过 playerredis 的上界时截住,不让缺省配置过不了校验。
func TestPlayerRedisConf_ClampsPoolSize(t *testing.T) {
	var c config.Config
	c.RedisClient = config.RedisConf{Host: "127.0.0.1:6379", PoolSize: playerredis.MaxPoolSizePerNode + 100}

	got, err := playerRedisConf(c)
	if err != nil {
		t.Fatalf("playerRedisConf: %v", err)
	}
	if got.PoolSizePerNode != playerredis.MaxPoolSizePerNode {
		t.Errorf("PoolSizePerNode = %d", got.PoolSizePerNode)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// 写了 PlayerRedis 段就原样用。
func TestPlayerRedisConf_ExplicitSectionWins(t *testing.T) {
	var c config.Config
	c.RedisClient = config.RedisConf{Host: "127.0.0.1:6379", DB: 3}
	c.PlayerRedis = playerredis.Config{Addrs: []string{"10.0.0.1:7000", "10.0.0.2:7000"}, Cluster: true, PoolSizePerNode: 4}

	got, err := playerRedisConf(c)
	if err != nil {
		t.Fatalf("playerRedisConf: %v", err)
	}
	if !got.Cluster || len(got.Addrs) != 2 || got.PoolSizePerNode != 4 {
		t.Errorf("写了就原样用,得到 %+v", got)
	}
}

// 玩家域只用 DB 0。RedisClient 不在 DB 0 时不能悄悄缺省过去:会话换了 DB,读会话的别的服务看不到。
func TestPlayerRedisConf_RejectsDefaultWhenRedisClientIsNotDB0(t *testing.T) {
	var c config.Config
	c.RedisClient = config.RedisConf{Host: "127.0.0.1:6379", DB: 3}

	if _, err := playerRedisConf(c); err == nil {
		t.Fatal("RedisClient.DB != 0 且没写 PlayerRedis 时必须报错")
	}
}
