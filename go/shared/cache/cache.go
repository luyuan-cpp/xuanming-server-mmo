package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// singleflightGroup provides in-process dedup without requiring golang.org/x/sync.
var (
	sfMu    sync.Mutex
	sfCalls = map[string]*sfCall{}
)

type sfCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

func singleflightDo(key string, fn func() (any, error)) (any, error) {
	sfMu.Lock()
	if c, ok := sfCalls[key]; ok {
		sfMu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &sfCall{}
	c.wg.Add(1)
	sfCalls[key] = c
	sfMu.Unlock()

	// 收尾必须走 defer。
	//
	// 旧写法是 fn() 之后才 wg.Done() + delete:dbLoader 一旦 panic(nil 解引用、
	// 越界、类型断言失败……),这两步都不执行 —— 该 key 的 sfCall 永远留在 map 里
	// 且 WaitGroup 计数永不归零,此后**每一个**用同 key 的调用都会在 wg.Wait()
	// 上永久阻塞:该缓存键功能性永久失效 + goroutine 无上界泄漏,只能重启进程。
	// defer 保证即使 panic 穿过去(panic 仍会正常向上传播给调用方),
	// 等待者也会被唤醒、map 条目也会被清掉。
	defer func() {
		sfMu.Lock()
		delete(sfCalls, key)
		sfMu.Unlock()
		c.wg.Done()
	}()

	c.val, c.err = fn()
	return c.val, c.err
}

// LoadOrCache implements the cache-aside pattern with in-process dedup.
// It tries Redis first, falls back to dbLoader on cache miss, and writes
// the result back to Redis with the given TTL.
func LoadOrCache[T any](
	ctx context.Context,
	rdb *redis.Client,
	cacheKey string,
	sfKey string,
	ttl time.Duration,
	dbLoader func(ctx context.Context) (T, error),
) (T, error) {
	var zero T

	// 1. Try Redis
	data, err := rdb.Get(ctx, cacheKey).Bytes()
	if err == nil {
		var result T
		if err := json.Unmarshal(data, &result); err != nil {
			return zero, fmt.Errorf("cache unmarshal %s: %w", cacheKey, err)
		}
		return result, nil
	}
	if err != redis.Nil {
		return zero, fmt.Errorf("redis get %s: %w", cacheKey, err)
	}

	// 2. Cache miss -> singleflight -> DB
	raw, err := singleflightDo(sfKey, func() (any, error) {
		value, err := dbLoader(ctx)
		if err != nil {
			return nil, err
		}
		// Write back to cache
		if bs, jsonErr := json.Marshal(value); jsonErr == nil {
			rdb.Set(ctx, cacheKey, bs, ttl)
		}
		return value, nil
	})
	if err != nil {
		return zero, err
	}
	// 用 comma-ok 而非裸断言:sfKey 由调用方自选,两个不同 T 的调用点若撞了同一个
	// sfKey,后到者会拿到前者的值,裸 raw.(T) 就是运行时 panic。返回错误让调用方
	// 看见"键撞了",而不是把整个请求打崩。
	typed, ok := raw.(T)
	if !ok {
		return zero, fmt.Errorf("cache singleflight key %q returned %T, want %T (sfKey collision between different types)",
			sfKey, raw, zero)
	}
	return typed, nil
}
