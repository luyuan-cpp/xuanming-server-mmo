package svc

import (
	"testing"

	"data_service/internal/config"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/service"
)

// 地域码 0 只给 dev、test:Region 是可选字段,漏写就是 0,其他模式不能把漏写当成一个地域。
func TestNewIdIssuer_RegionZeroReservedForDevAndTest(t *testing.T) {
	for _, mode := range []string{service.ProMode, service.PreMode, service.RtMode, ""} {
		var c config.Config
		c.Mode = mode
		_, err := NewIdIssuer(c)
		require.ErrorContains(t, err, "reserved for dev and test", "mode=%q", mode)
	}
}

// staging、prod 的配置来自运维预置的 Secret:抄错的地域码不报错,只会和别的地域撞号。部署参数和配置要对得上。
func TestCheckExpectedRegion(t *testing.T) {
	require.NoError(t, checkExpectedRegion("", 5), "本机路径没有这个环境变量")
	require.NoError(t, checkExpectedRegion("5", 5))
	require.ErrorContains(t, checkExpectedRegion("6", 5), "fix the runtime config")
}

// 这些配置过了地域码这一关。它们没给 etcd 地址,会停在地址校验,不连 etcd。
func TestNewIdIssuer_RegionAccepted(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		region uint32
	}{
		{service.DevMode, 0},
		{service.TestMode, 0},
		{service.DevMode, 1},
		{service.ProMode, 1},
		{service.ProMode, 63},
	} {
		var c config.Config
		c.Mode = tc.mode
		c.IdSegment.Issuer.Region = tc.region
		_, err := NewIdIssuer(c)
		require.ErrorContains(t, err, "PrimaryEtcd, SecondaryEtcd and RegistryEtcd are all required", "mode=%q region=%d", tc.mode, tc.region)
	}
}
