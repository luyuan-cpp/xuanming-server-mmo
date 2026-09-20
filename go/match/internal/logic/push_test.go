package logic

import (
	"testing"

	"match/internal/metrics"

	plpb "proto/player_locator"

	"github.com/stretchr/testify/require"
)

// match_kafka_push_total 只由 logic 的 pushToPlayer 记,口径与抽出 playercontract 之前一致:
// 不在线、会话解析失败都记 error。组队推送不经这里,不会进这个指标。
func TestPushToPlayerRecordsKafkaPushMetric(t *testing.T) {
	svcCtx, mr := newTestSvcCtx(t)
	msg := &plpb.PlayerSession{} // 任意 proto 消息
	errBefore := metrics.KafkaPushValue("error")
	okBefore := metrics.KafkaPushValue("ok")

	err := pushToPlayer(svcCtx, 31, 1, msg)
	require.ErrorIs(t, err, errPlayerOffline)
	require.Equal(t, errBefore+1, metrics.KafkaPushValue("error"))

	require.NoError(t, mr.Set(playerSessionKey(32), "\x0a\x05ab")) // 截断的 length-delimited 字段
	err = pushToPlayer(svcCtx, 32, 1, msg)
	require.Error(t, err)
	require.NotErrorIs(t, err, errPlayerOffline)
	require.Equal(t, errBefore+2, metrics.KafkaPushValue("error"))

	require.Equal(t, okBefore, metrics.KafkaPushValue("ok"))
}
