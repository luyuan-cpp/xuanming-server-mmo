package idsegment

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Source 是 Minter 眼里的号段客户端。*Client 满足;单测用假实现。
type Source interface {
	Next(ctx context.Context) (uint64, error)
}

// ErrNoIDSource 表示 Minter 既没有号段也没有回退 —— 接线错误,启动时就该发现。
var ErrNoIDSource = errors.New("idsegment: minter has neither a segment source nor a fallback")

// Minter 把「号段优先、可选回退 snowflake」这条策略收成一处,login(player_id)与
// guild(guild_id)共用同一份规则,而不是各写一遍 if/else。
//
// 三种接法(设计稿 §6.4):
//   - Segment 非 nil、Fallback nil:号段失败 → 本次操作整体失败(**默认**)。
//   - Segment 非 nil、Fallback 非 nil:号段失败 → 记 ERROR 后走 Fallback。
//     回退是安全的(两套值域不相交),代价是 snowflake 机器永远留着,所以默认不开。
//   - Segment nil、Fallback 非 nil:号段关闭,纯 snowflake —— 保留一个版本的回滚开关。
//
// 无论哪条路,失败时返回错误,**绝不**返回 0 或自造 id。
type Minter struct {
	// Name 只用于日志,如 "player_id" / "guild_id"。
	Name string
	// Segment 为 nil 表示号段关闭。
	Segment Source
	// Fallback 为 nil 表示不回退。
	Fallback func() (uint64, error)
	// Logger 为 nil 用 logx。
	Logger Logger

	// lastFallbackLogSec 把「号段失败、已回退」的 ERROR 限到每墙钟秒一条:
	// 库倒下时每次建角都会走到这里,不限流会把日志打成瀑布。
	lastFallbackLogSec atomic.Int64
	fallbacks          atomic.Uint64
}

// Mint 按上面的策略发一个 id。
func (m *Minter) Mint(ctx context.Context) (uint64, error) {
	if m.Segment == nil {
		if m.Fallback == nil {
			return 0, ErrNoIDSource
		}
		return m.Fallback()
	}
	id, err := m.Segment.Next(ctx)
	if err == nil {
		return id, nil
	}
	if m.Fallback == nil {
		return 0, err
	}
	m.fallbacks.Add(1)
	if sec := time.Now().Unix(); m.lastFallbackLogSec.Swap(sec) != sec {
		m.logger().Errorf("[idsegment] %s: segment source failed, falling back to snowflake "+
			"(fallbacks_total=%d): %v", m.Name, m.fallbacks.Load(), err)
	}
	return m.Fallback()
}

// Fallbacks 返回累计回退次数(观测用)。
func (m *Minter) Fallbacks() uint64 { return m.fallbacks.Load() }

func (m *Minter) logger() Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return logxLogger{}
}
