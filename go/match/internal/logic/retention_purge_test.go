package logic

import (
	"errors"
	"fmt"
	"testing"

	"match/internal/metrics"

	"shared/authoritysql"

	"github.com/stretchr/testify/require"
)

// 一直是满页时一轮清到页数上限为止,不占着循环;扫完或出错立刻结束。
func TestDrainRetentionPagesStopsAtPageCapEndOrError(t *testing.T) {
	pages := 0
	require.NoError(t, drainRetentionPages(func() (bool, error) { pages++; return true, nil }))
	require.Equal(t, retentionPurgePagesPerRound, pages)

	pages = 0
	require.NoError(t, drainRetentionPages(func() (bool, error) { pages++; return pages < 3, nil }))
	require.Equal(t, 3, pages, "扫完(more=false)就结束")

	pages = 0
	failed := errors.New("page failed")
	require.ErrorIs(t, drainRetentionPages(func() (bool, error) { pages++; return true, failed }), failed)
	require.Equal(t, 1, pages, "出错不再清下一页")
}

// 一轮结局的口径:成功不计;搬库冻结(写者闸门拒绝,这一轮确定没写)不计失败,包了一层、几步都冻结也一样;
// 其余失败计一次;冻结里混着别的错误照常计。
func TestObserveRetentionRoundDoesNotCountFrozenWriter(t *testing.T) {
	const purge = metrics.RetentionPurgeSpectatorIntent
	base := metrics.RetentionPurgeFailureValue(purge)

	observeRetentionRound(purge, nil)
	observeRetentionRound(purge, authoritysql.ErrWriterFrozen)
	observeRetentionRound(purge, fmt.Errorf("read expired page: %w", authoritysql.ErrWriterFrozen))
	observeRetentionRound(purge, errors.Join(authoritysql.ErrWriterFrozen, authoritysql.ErrWriterFrozen))
	require.Equal(t, base, metrics.RetentionPurgeFailureValue(purge), "闸门冻结不是清理失败")

	observeRetentionRound(purge, errors.New("TiDB unavailable"))
	observeRetentionRound(purge, errors.Join(authoritysql.ErrWriterFrozen, errors.New("TiDB unavailable")))
	require.Equal(t, base+2, metrics.RetentionPurgeFailureValue(purge), "真失败计数;冻结里混着别的错误也计")
}
