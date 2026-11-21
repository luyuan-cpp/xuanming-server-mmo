package logic

import (
	"context"
	"testing"

	"match/internal/constants"
	"match/internal/svc"

	matchpb "proto/match"

	"github.com/stretchr/testify/require"
)

func TestListBattleReportsRejectsMissingOrUnboundSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "no_session", ctx: context.Background()},
		{name: "unbound_session", ctx: unboundSessionCtx()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp *matchpb.ListBattleReportsResponse
			var err error
			callRejected(t, func() {
				resp, err = NewBattleReportLogic(tc.ctx, &svc.ServiceContext{}).ListBattleReports(&matchpb.ListBattleReportsRequest{})
			})
			require.NoError(t, err)
			require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
			require.Equal(t, missingIdentityParameters, resp.GetErrorMessage().GetParameters())
			require.Empty(t, resp.GetReports())
		})
	}
}

func TestGetBattleReportRejectsMissingOrUnboundSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "no_session", ctx: context.Background()},
		{name: "unbound_session", ctx: unboundSessionCtx()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var resp *matchpb.GetBattleReportResponse
			var err error
			callRejected(t, func() {
				resp, err = NewBattleReportLogic(tc.ctx, &svc.ServiceContext{}).GetBattleReport(&matchpb.GetBattleReportRequest{BattleId: 1})
			})
			require.NoError(t, err)
			require.Equal(t, constants.ErrInternal, resp.GetErrorMessage().GetId())
			require.Equal(t, missingIdentityParameters, resp.GetErrorMessage().GetParameters())
			require.Nil(t, resp.GetReplay())
		})
	}
}

func TestGetBattleReportZeroBattleIDReturnsNotFoundWithoutReadingStore(t *testing.T) {
	resp, err := NewBattleReportLogic(boundSessionCtx(ownPlayerID), &svc.ServiceContext{}).GetBattleReport(
		&matchpb.GetBattleReportRequest{BattleId: 0})
	require.NoError(t, err)
	require.Equal(t, constants.ErrBattleReportNotFound, resp.GetErrorMessage().GetId())
	require.Nil(t, resp.GetReplay())
}

func TestBattleReportStoreErrorReturnsInternal(t *testing.T) {
	listResp, err := NewBattleReportLogic(boundSessionCtx(ownPlayerID), &svc.ServiceContext{}).ListBattleReports(
		&matchpb.ListBattleReportsRequest{})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, listResp.GetErrorMessage().GetId())
	require.Empty(t, listResp.GetReports())

	getResp, err := NewBattleReportLogic(boundSessionCtx(ownPlayerID), &svc.ServiceContext{}).GetBattleReport(
		&matchpb.GetBattleReportRequest{BattleId: 42})
	require.NoError(t, err)
	require.Equal(t, constants.ErrInternal, getResp.GetErrorMessage().GetId())
	require.Nil(t, getResp.GetReplay())
}
