package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/conf"
)

// 仓内 etc/match_service.yaml 必须能加载并通过 Validate。
func TestMatchServiceYamlLoads(t *testing.T) {
	var c Config
	require.NoError(t, conf.Load(filepath.Join("..", "..", "etc", "match_service.yaml"), &c))
}

// 2026-10-30(评审 B-6):终局发件箱的节拍、批量与并发进配置。仓内 yaml 写的就是之前写死的值
// (2 秒 / 结果 8 行 / 首投 4 行 / 等待 4 行 / 串行);整段漏写时节拍按 2 秒,批量与并发的零值由 authority 换成同样的默认值。
func TestOutcomeOutboxConfigMatchesPreviousBehavior(t *testing.T) {
	var c Config
	require.NoError(t, conf.Load(filepath.Join("..", "..", "etc", "match_service.yaml"), &c))
	require.Equal(t, OutcomeOutboxConf{IntervalMs: 2000, ResultBatch: 8, FirstBatch: 4, WaitingBatch: 4, Concurrency: 1}, c.GlobalMatch.OutcomeOutbox)
	require.Equal(t, 2*time.Second, c.GlobalMatch.OutcomeOutboxInterval())

	require.Equal(t, 2*time.Second, GlobalMatchConf{}.OutcomeOutboxInterval(), "直接构造 Config 的零值按默认 2 秒")
	require.Equal(t, 2*time.Second, GlobalMatchConf{OutcomeOutbox: OutcomeOutboxConf{IntervalMs: -5}}.OutcomeOutboxInterval())
	require.Equal(t, 500*time.Millisecond, GlobalMatchConf{OutcomeOutbox: OutcomeOutboxConf{IntervalMs: 500}}.OutcomeOutboxInterval())
}
