package logic

import (
	"testing"

	"guild/internal/constants"

	"github.com/stretchr/testify/require"
)

func TestGuildEmblemPricingMatrix(t *testing.T) {
	_, ok := constants.GuildEmblemPriceDiamond(0)
	require.False(t, ok)
	_, ok = constants.GuildEmblemPriceDiamond(7)
	require.False(t, ok)
	for id := uint32(1); id <= 3; id++ {
		p, ok := constants.GuildEmblemPriceDiamond(id)
		require.True(t, ok)
		require.Equal(t, uint64(180), p)
	}
	for id := uint32(4); id <= 6; id++ {
		p, ok := constants.GuildEmblemPriceDiamond(id)
		require.True(t, ok)
		require.Equal(t, uint64(300), p)
	}
}
