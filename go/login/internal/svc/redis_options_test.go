package svc

import (
	"testing"
	"time"

	"login/internal/config"
)

// 配置里写了的连接参数都要原样进入 go-redis 选项:曾经三项超时没传,DialTimeout 3s 被静默忽略。
func TestRedisOptions_CarriesEveryConfiguredField(t *testing.T) {
	c := config.RedisConf{
		Host:         "redis.example:6379",
		Password:     "pw",
		DB:           2,
		DialTimeout:  3 * time.Second,
		ReadTimeout:  4 * time.Second,
		WriteTimeout: 5 * time.Second,
		PoolSize:     17,
	}

	o := redisOptions(c)

	if o.Addr != c.Host || o.Password != c.Password || o.DB != 2 {
		t.Fatalf("地址 / 口令 / DB 没有原样带入: addr=%q db=%d", o.Addr, o.DB)
	}
	if o.DialTimeout != 3*time.Second || o.ReadTimeout != 4*time.Second || o.WriteTimeout != 5*time.Second {
		t.Fatalf("超时没有带入: dial=%v read=%v write=%v", o.DialTimeout, o.ReadTimeout, o.WriteTimeout)
	}
	if o.PoolSize != 17 {
		t.Fatalf("PoolSize 没有带入: %d", o.PoolSize)
	}
	if !o.DisableIndentity {
		t.Fatal("DisableIndentity 必须保持 true(Redis < 7.2 不认 CLIENT SETINFO)")
	}
}

// 不写超时(零值)时交给 go-redis 自己的缺省,不能被当成「不超时」。
func TestRedisOptions_ZeroTimeoutsKeepGoRedisDefaults(t *testing.T) {
	o := redisOptions(config.RedisConf{Host: "redis.example:6379"})

	if o.DialTimeout != 0 || o.ReadTimeout != 0 || o.WriteTimeout != 0 {
		t.Fatalf("零值必须原样传给 go-redis 由它补缺省: dial=%v read=%v write=%v",
			o.DialTimeout, o.ReadTimeout, o.WriteTimeout)
	}
}
