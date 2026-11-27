package internalrpcauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultRedisReplayPrefix = "internalrpc:nonce:"

// RedisReplayStore is a cross-replica nonce replay store backed by Redis SETNX with TTL.
type RedisReplayStore struct {
	client redis.Cmdable
	prefix string
	tagged bool
}

// NewRedisReplayStore constructs a player-domain Redis Cluster safe replay store.
// Nonce keys use a non-control hash tag "{s:<sha256>}" so every SETNX satisfies
// clusterredis.PlayerDomainKeys while hiding raw caller/nonce metadata from keys and logs.
func NewRedisReplayStore(client redis.Cmdable, prefix string) (*RedisReplayStore, error) {
	return newRedisReplayStore(client, prefix, true)
}

// NewControlClusterReplayStore constructs a control-cluster replay store using untagged
// single-key SETNX ("<prefix><sha256>"), satisfying clusterredis.ControlClusterKeys.
func NewControlClusterReplayStore(client redis.Cmdable, prefix string) (*RedisReplayStore, error) {
	return newRedisReplayStore(client, prefix, false)
}

func newRedisReplayStore(client redis.Cmdable, prefix string, tagged bool) (*RedisReplayStore, error) {
	if client == nil {
		return nil, errors.New("internal RPC Redis replay client is required")
	}
	if prefix == "" {
		prefix = defaultRedisReplayPrefix
	}
	if strings.ContainsAny(prefix, "{}") {
		return nil, errors.New("internal RPC Redis replay prefix must not contain braces")
	}
	return &RedisReplayStore{client: client, prefix: prefix, tagged: tagged}, nil
}

// Consume atomically claims nonceKey in Redis for ttl. Returns (true, nil) on first use,
// (false, nil) on replay, and (false, err) on Redis failure (fail-closed).
func (s *RedisReplayStore) Consume(ctx context.Context, nonceKey string, ttl time.Duration) (bool, error) {
	if s == nil || s.client == nil || nonceKey == "" || ttl <= 0 {
		return false, errors.New("internal RPC Redis replay store unavailable")
	}
	// Hash caller:nonce so attacker-controlled metadata never becomes a raw Redis key fragment.
	digest := sha256.Sum256([]byte(nonceKey))
	hexDigest := hex.EncodeToString(digest[:])
	key := s.prefix + hexDigest
	if s.tagged {
		key = s.prefix + "{s:" + hexDigest + "}"
	}
	return s.client.SetNX(ctx, key, "1", ttl).Result()
}
