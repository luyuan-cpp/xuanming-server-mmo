package idsegment

import (
	"context"
	"errors"
)

// Source 是 Minter 眼里的号段客户端。*Client 满足;单测用假实现。
type Source interface {
	Next(ctx context.Context) (uint64, error)
}

// ErrNoIDSource 表示 Minter 没有号段来源 —— 接线错误或 IdSegment.Enabled=false。
var ErrNoIDSource = errors.New("idsegment: minter has no segment source")

// Minter 是永久 id 的发号入口。只有号段一条路:号段失败,本次操作整体失败,
// **绝不**返回 0 或自造 id。
//
// 这里曾经可以回退到 snowflake,安全的前提是两套号的值域不相交。发号打散之后号段值铺满 63 位,
// 这个前提不再成立,回退可能撞号,所以删掉了(docs/design/player-data-tidb-scaling-plan.md §6.4)。
type Minter struct {
	// Name 只用于识别用途,如 "player_id" / "guild_id"。
	Name string
	// Segment 为 nil 时 Mint 返回 ErrNoIDSource。
	Segment Source
}

// Mint 发一个 id。
func (m *Minter) Mint(ctx context.Context) (uint64, error) {
	if m.Segment == nil {
		return 0, ErrNoIDSource
	}
	return m.Segment.Next(ctx)
}
