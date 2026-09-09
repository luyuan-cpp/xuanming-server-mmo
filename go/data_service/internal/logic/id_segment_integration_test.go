//go:build integration

package logic

// 集成测试:AllocateIdSegment 的错误码定性接真实 id_segment 表。
// 跑法:  go test -tags=integration ./internal/logic/...
// 需要本地 MySQL,见 internal/store/storetest 包注释(建库权限不够时用 root)。

import (
	"context"
	"testing"

	"data_service/internal/constants"
	"data_service/internal/store"
	"data_service/internal/store/storetest"
	"data_service/internal/svc"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllocateIdSegment_RealStore_ErrorCodeMapping(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	// dev 形态(AllowAutoSeed=true):首次使用自动种行。生产形态见下一个用例。
	st, err := store.NewIdSegmentStore(db.Cfg, store.IdSegmentOptions{AllowAutoSeed: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	svcCtx := &svc.ServiceContext{IdSegmentStore: st}
	ctx := context.Background()

	// 成功:首次使用从 1 起;step=0 继承。
	resp, err := AllocateIdSegment(ctx, svcCtx, &AllocateIdSegmentReq{BizTag: "player", Step: 100})
	require.NoError(t, err)
	assert.Zero(t, resp.ErrorCode)
	assert.Equal(t, uint64(1), resp.Lo)
	assert.Equal(t, uint64(101), resp.Hi)
	resp, err = AllocateIdSegment(ctx, svcCtx, &AllocateIdSegmentReq{BizTag: "player"})
	require.NoError(t, err)
	assert.Zero(t, resp.ErrorCode)
	assert.Equal(t, uint64(101), resp.Lo)
	assert.Equal(t, uint64(201), resp.Hi)

	// 调用方参数问题 → InvalidRequest,err=nil(不算故障),表不动。
	for _, req := range []*AllocateIdSegmentReq{
		{BizTag: "Player!", Step: 1},
		{BizTag: "guild", Step: 0}, // 首次使用没有可继承的 step
		{BizTag: "player", Step: store.IdSegmentMaxStep + 1},
	} {
		resp, err = AllocateIdSegment(ctx, svcCtx, req)
		require.NoError(t, err, "req=%+v", req)
		assert.Equal(t, constants.ErrCodeInvalidRequest, resp.ErrorCode, "req=%+v", req)
		assert.Zero(t, resp.Lo)
		assert.Zero(t, resp.Hi)
	}
	assert.Equal(t, int64(1), db.Count(t, "id_segment", ""), "only the player row may exist")

	// 值域见底 → IdSegmentExhausted,err=nil(handler 走 in-band 码),行零变更。
	_, err = db.Raw.Exec(`INSERT INTO id_segment (biz_tag, max_id, step, version) VALUES ('item', ?, 100, 1)`, store.IdSegmentCap-50)
	require.NoError(t, err)
	resp, err = AllocateIdSegment(ctx, svcCtx, &AllocateIdSegmentReq{BizTag: "item"})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeIdSegmentExhausted, resp.ErrorCode)
	var maxID, version uint64
	require.NoError(t, db.Raw.QueryRow(`SELECT max_id, version FROM id_segment WHERE biz_tag = 'item'`).Scan(&maxID, &version))
	assert.Equal(t, store.IdSegmentCap-50, maxID)
	assert.Equal(t, uint64(1), version)

	// store 没配起来(MySQL 不可达 / 迁移失败)→ IdSegmentDBError 且带 err(拦截器记故障)。
	resp, err = AllocateIdSegment(ctx, &svc.ServiceContext{}, &AllocateIdSegmentReq{BizTag: "player", Step: 1})
	require.Error(t, err)
	assert.Equal(t, constants.ErrCodeIdSegmentDBError, resp.ErrorCode)
}

// TestAllocateIdSegment_RealStore_UnknownTagWithoutAutoSeed 锁死生产形态(设计 §7.5 第 7 条):
// IdSegment.AllowAutoSeed=false 时,表里没有的 biz_tag 得到 ErrCodeIdSegmentUnknownTag、
// err=nil(in-band 码,拦截器按 FaultCodeSet 记故障),且**一行都不写** —— 这是"库被重置后
// 从 1 重发、覆盖别人角色行"的唯一防线。行由迁移 BootstrapTags 创建之后才能发号。
func TestAllocateIdSegment_RealStore_UnknownTagWithoutAutoSeed(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st, err := store.NewIdSegmentStore(db.Cfg, store.IdSegmentOptions{AllowAutoSeed: false})
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	svcCtx := &svc.ServiceContext{IdSegmentStore: st}
	ctx := context.Background()

	for _, req := range []*AllocateIdSegmentReq{
		{BizTag: "player", Step: 100},
		{BizTag: "player"}, // step=0 也先按缺行定性,而不是 InvalidRequest
	} {
		resp, err := AllocateIdSegment(ctx, svcCtx, req)
		require.NoError(t, err, "req=%+v", req)
		assert.Equal(t, constants.ErrCodeIdSegmentUnknownTag, resp.ErrorCode, "req=%+v", req)
		assert.Zero(t, resp.Lo)
		assert.Zero(t, resp.Hi)
	}
	assert.Equal(t, int64(0), db.Count(t, "id_segment", ""), "unknown tag must not create a row")

	// 非法 tag 仍是调用方参数问题,不是缺行。
	resp, err := AllocateIdSegment(ctx, svcCtx, &AllocateIdSegmentReq{BizTag: "Player!", Step: 1})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCodeInvalidRequest, resp.ErrorCode)

	// 迁移按 BootstrapTags 建行之后,同一个 store 立刻可以发号,从 1 起。
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{BootstrapTags: []string{"player"}}))
	resp, err = AllocateIdSegment(ctx, svcCtx, &AllocateIdSegmentReq{BizTag: "player", Step: 100})
	require.NoError(t, err)
	assert.Zero(t, resp.ErrorCode)
	assert.Equal(t, uint64(1), resp.Lo)
	assert.Equal(t, uint64(101), resp.Hi)
}
