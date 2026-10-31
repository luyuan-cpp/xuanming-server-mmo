package svc

import (
	"context"
	"slices"
	"sync"
	"testing"

	"data_service/internal/config"
	"data_service/internal/store"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rowIDIssuer struct {
	IdSegmentStore
	mu   sync.Mutex
	tags []string
	next uint64
}

func (f *rowIDIssuer) Allocate(_ context.Context, bizTag string, step uint32) (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags = append(f.tags, bizTag)
	lo := f.next + 1
	f.next += uint64(step)
	return lo, lo + uint64(step), nil
}

// 发号器不可用时不接行号来源:插入失败(消费者停在当前 offset),不自造 id。
func TestUseSnapshotRowIDs_NoIssuerFailsClosed(t *testing.T) {
	ss := &store.SnapshotStore{}
	assert.Nil(t, useSnapshotRowIDs(ss, nil))

	_, err := ss.InsertSnapshot(context.Background(), &store.SnapshotRow{})
	require.ErrorIs(t, err, store.ErrNoRowIDSource)
}

// 两张表各用各的 biz_tag,而且都在默认清单里。不在清单里的 tag 会被发号器拒绝,生产上插入就全失败了。
func TestUseSnapshotRowIDs_UsesRegisteredTags(t *testing.T) {
	issuer := &rowIDIssuer{}
	segments := useSnapshotRowIDs(&store.SnapshotStore{}, issuer)
	require.Len(t, segments, 2)
	for _, segment := range segments {
		t.Cleanup(segment.Close)
		_, err := segment.Next(context.Background())
		require.NoError(t, err)
	}

	issuer.mu.Lock()
	defer issuer.mu.Unlock()
	for _, tag := range []string{snapshotRowBizTag, rollbackAuditBizTag} {
		assert.Contains(t, issuer.tags, tag)
		assert.Truef(t, slices.Contains(config.DefaultIdSegmentTags, tag), "%s 没登记在 DefaultIdSegmentTags", tag)
	}
}
