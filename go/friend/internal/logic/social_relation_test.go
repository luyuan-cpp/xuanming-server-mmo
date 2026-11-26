package logic

import (
	"context"
	"errors"
	"testing"

	"friend/internal/constants"
	pb "proto/friend"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchCheckSocialRelations(t *testing.T) {
	t.Run("all_friends_and_not_blocked", func(t *testing.T) {
		f := newFixture(t, nil)
		f.store.socialBlocked = false
		f.store.socialAllFriends = true

		resp, err := f.logic().BatchCheckSocialRelations(context.Background(), &pb.BatchCheckSocialRelationsRequest{
			SideAPlayerIds: []uint64{101, 102},
			SideBPlayerIds: []uint64{201, 202},
		})
		require.NoError(t, err)
		assert.Nil(t, resp.ErrorMessage)
		assert.False(t, resp.GetHasAnyBlock())
		assert.True(t, resp.GetAllAreFriends())
	})

	t.Run("blocked_pair_reported", func(t *testing.T) {
		f := newFixture(t, nil)
		f.store.socialBlocked = true
		f.store.socialAllFriends = false

		resp, err := f.logic().BatchCheckSocialRelations(context.Background(), &pb.BatchCheckSocialRelationsRequest{
			SideAPlayerIds: []uint64{101},
			SideBPlayerIds: []uint64{201},
		})
		require.NoError(t, err)
		assert.Nil(t, resp.ErrorMessage)
		assert.True(t, resp.GetHasAnyBlock())
		assert.False(t, resp.GetAllAreFriends())
	})

	t.Run("invalid_params_rejected_in_band", func(t *testing.T) {
		f := newFixture(t, nil)
		cases := []*pb.BatchCheckSocialRelationsRequest{
			nil,
			{SideAPlayerIds: nil, SideBPlayerIds: []uint64{1}},
			{SideAPlayerIds: []uint64{0}, SideBPlayerIds: []uint64{1}},
			{SideAPlayerIds: []uint64{101}, SideBPlayerIds: []uint64{101}},
			{SideAPlayerIds: []uint64{1, 2, 3, 4, 5, 6}, SideBPlayerIds: []uint64{7}},
		}
		for _, tc := range cases {
			resp, err := f.logic().BatchCheckSocialRelations(context.Background(), tc)
			require.NoError(t, err)
			require.NotNil(t, resp.ErrorMessage)
			assert.Equal(t, constants.ErrInvalidParameter, resp.ErrorMessage.GetId())
		}
		assert.Zero(t, f.store.calls["BatchCheckSocialRelations"])
	})

	t.Run("storage_error_mapped_in_band", func(t *testing.T) {
		f := newFixture(t, nil)
		f.store.fail("BatchCheckSocialRelations", errors.New("db down"))

		resp, err := f.logic().BatchCheckSocialRelations(context.Background(), &pb.BatchCheckSocialRelationsRequest{
			SideAPlayerIds: []uint64{101},
			SideBPlayerIds: []uint64{201},
		})
		require.NoError(t, err)
		require.NotNil(t, resp.ErrorMessage)
		assert.Equal(t, constants.ErrStorage, resp.ErrorMessage.GetId())
	})
}
