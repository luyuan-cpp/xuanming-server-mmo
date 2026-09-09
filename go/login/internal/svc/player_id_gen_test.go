package svc

import (
	"errors"
	"testing"
	"time"

	"github.com/bwmarrin/snowflake"

	sfshared "shared/snowflake"
)

func newTestPlayerIDGen(t *testing.T) *PlayerIDGen {
	t.Helper()
	node, err := snowflake.NewNode(1)
	if err != nil {
		t.Fatal(err)
	}
	return NewPlayerIDGen(node)
}

// 没挂闸时行为不变(单测 / 未接分配器)。
func TestPlayerIDGen_GeneratesWithoutFenceClock(t *testing.T) {
	g := newTestPlayerIDGen(t)
	if _, err := g.Generate(); err != nil {
		t.Fatal(err)
	}
}

// bwmarrin 包装与 shared/snowflake.Node 执行同一条规则:距上次水位写成功超过 F 就拒发,
// 两个时钟任一触发;重新 Ack 即恢复;永久 Fence 优先。
func TestPlayerIDGen_RefusesWhenWatermarkStale(t *testing.T) {
	g := newTestPlayerIDGen(t)
	mono := time.Hour
	wall := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	fc := sfshared.NewFenceClockWithClocks(time.Minute,
		func() time.Duration { return mono },
		func() time.Time { return wall })
	g.SetFenceClock(fc)

	// 从未 Ack:fail-closed。
	if id, err := g.Generate(); !errors.Is(err, ErrPlayerIDGenWatermarkStale) || !errors.Is(err, sfshared.ErrWatermarkStale) || id != 0 {
		t.Fatalf("从未 Ack 必须拒发, got id=%d err=%v", id, err)
	}
	fc.Ack()
	if _, err := g.Generate(); err != nil {
		t.Fatalf("Ack 后必须能发号: %v", err)
	}

	// 只推墙钟(VM 挂起:单调钟停走)。
	wall = wall.Add(61 * time.Second)
	if _, err := g.Generate(); !errors.Is(err, ErrPlayerIDGenWatermarkStale) {
		t.Fatalf("墙钟越过 F 必须拒发, got %v", err)
	}
	fc.Ack()
	if _, err := g.Generate(); err != nil {
		t.Fatalf("重新 Ack 后必须恢复: %v", err)
	}

	// 只推单调钟(墙钟被回拨)。
	mono += 61 * time.Second
	wall = wall.Add(-time.Hour)
	if _, err := g.Generate(); !errors.Is(err, ErrPlayerIDGenWatermarkStale) {
		t.Fatalf("单调钟越过 F 必须拒发, got %v", err)
	}

	// 永久 fence 优先级更高,且不受 Ack 影响。
	g.Fence()
	fc.Ack()
	if _, err := g.Generate(); !errors.Is(err, ErrPlayerIDGenFenced) {
		t.Fatalf("Fence 后必须是 ErrPlayerIDGenFenced, got %v", err)
	}
}
