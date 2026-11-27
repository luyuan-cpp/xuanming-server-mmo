package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"shared/clusterredis"
	"shared/redisslot"
)

func TestKeyFormatsSatisfyPlayerDomainClusterPolicy(t *testing.T) {
	k1 := Key("trade", "order", 42)
	if !redisslot.HasHashTag(k1) {
		t.Fatalf("Key(%q) missing hash tag", k1)
	}
	if err := clusterredis.PlayerDomainKeys.CheckKeys([]string{k1}); err != nil {
		t.Fatalf("Key(%q) rejected by PlayerDomainKeys: %v", k1, err)
	}
	k2 := KeyString("login", "fail", "10.0.0.1{bad}")
	if err := clusterredis.PlayerDomainKeys.CheckKeys([]string{k2}); err != nil {
		t.Fatalf("KeyString(%q) rejected by PlayerDomainKeys: %v", k2, err)
	}
}

func TestCooldownQuotaAndPenaltyLifecycle(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	// 1. Cooldown claim, block, clear, re-claim.
	ck := Key("chat", "world", 7)
	if ok, err := Cooldown(ctx, rdb, ck, 5*time.Second); err != nil || !ok {
		t.Fatalf("first Cooldown=(%v,%v), want (true,nil)", ok, err)
	}
	if ok, err := Cooldown(ctx, rdb, ck, 5*time.Second); err != nil || ok {
		t.Fatalf("second Cooldown=(%v,%v), want (false,nil)", ok, err)
	}
	if err := ClearCooldown(ctx, rdb, ck); err != nil {
		t.Fatal(err)
	}
	if ok, err := Cooldown(ctx, rdb, ck, 5*time.Second); err != nil || !ok {
		t.Fatalf("after ClearCooldown=(%v,%v), want (true,nil)", ok, err)
	}

	// 2. ActionQuota fixed window with TTL.
	q := &ActionQuota{RDB: rdb, Domain: "friend", Limit: 2, Window: time.Minute}
	for i := 0; i < 2; i++ {
		if ok, err := q.Allow(ctx, "apply", 99); err != nil || !ok {
			t.Fatalf("attempt %d Allow=(%v,%v), want (true,nil)", i+1, ok, err)
		}
	}
	if ok, err := q.Allow(ctx, "apply", 99); err != nil || ok {
		t.Fatalf("over-limit Allow=(%v,%v), want (false,nil)", ok, err)
	}
	if ttl := mr.TTL(Key("friend", "apply", 99)); ttl <= 0 {
		t.Fatalf("quota key missing TTL: %v", ttl)
	}

	// 3. Penalty arm and remaining query.
	pk := Key("match", "noshow", 11)
	if rem, err := PenaltyRemaining(ctx, rdb, pk); err != nil || rem != 0 {
		t.Fatalf("absent penalty rem=%v err=%v", rem, err)
	}
	if err := ArmPenalty(ctx, rdb, pk, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if rem, err := PenaltyRemaining(ctx, rdb, pk); err != nil || rem <= 0 {
		t.Fatalf("armed penalty rem=%v err=%v", rem, err)
	}
}

func TestFailOpenOnRedisError(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: 0, DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = rdb.Close() })
	mr.Close()

	ctx := context.Background()
	if ok, err := Cooldown(ctx, rdb, Key("t", "a", 1), time.Second); !ok || err == nil {
		t.Fatalf("Cooldown on dead Redis must fail-open with err, got (%v,%v)", ok, err)
	}
	if ok, err := Quota(ctx, rdb, Key("t", "b", 1), 1, time.Second); !ok || err == nil {
		t.Fatalf("Quota on dead Redis must fail-open with err, got (%v,%v)", ok, err)
	}
	if rem, err := PenaltyRemaining(ctx, rdb, Key("t", "c", 1)); rem != 0 || errors.Is(err, nil) {
		t.Fatalf("PenaltyRemaining on dead Redis=(%v,%v), want (0, non-nil)", rem, err)
	}
}
