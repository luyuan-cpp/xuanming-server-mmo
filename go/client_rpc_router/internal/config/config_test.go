package config

import (
	"testing"

	base "proto/common/base"

	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/conf"
)

// 留空按默认:只有 login 是 zone-scoped。
func TestResolveZoneScopedNodeTypesDefault(t *testing.T) {
	c := Config{}
	resolved, err := c.ResolveZoneScopedNodeTypes()
	require.NoError(t, err)
	require.Len(t, resolved, 1)
	_, ok := resolved[base.ENodeType_LoginNodeService]
	require.True(t, ok, "默认必须包含 LoginNodeService")
}

// 显式配置按名字解析成枚举值。
func TestResolveZoneScopedNodeTypesExplicit(t *testing.T) {
	c := Config{ZoneScopedNodeTypes: []string{"LoginNodeService", "MatchNodeService"}}
	resolved, err := c.ResolveZoneScopedNodeTypes()
	require.NoError(t, err)
	require.Len(t, resolved, 2)
	_, ok := resolved[base.ENodeType_MatchNodeService]
	require.True(t, ok)
}

// 未知名字必须报错:配错会让 login 走全局随机,跨 zone 打到别的 login。
func TestResolveZoneScopedNodeTypesRejectsUnknown(t *testing.T) {
	c := Config{ZoneScopedNodeTypes: []string{"NoSuchNodeService"}}
	_, err := c.ResolveZoneScopedNodeTypes()
	require.Error(t, err)
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "默认可用", cfg: Config{ForwardTimeoutMs: 5000}, wantErr: false},
		{name: "ForwardTimeoutMs 为 0", cfg: Config{ForwardTimeoutMs: 0}, wantErr: true},
		{name: "zrpc Timeout 小于转发超时", cfg: func() Config {
			c := Config{ForwardTimeoutMs: 5000}
			c.Timeout = 2000
			return c
		}(), wantErr: true},
		{name: "zrpc Timeout 等于转发超时", cfg: func() Config {
			c := Config{ForwardTimeoutMs: 5000}
			c.Timeout = 5000
			return c
		}(), wantErr: true},
		{name: "zrpc Timeout 大于转发超时", cfg: func() Config {
			c := Config{ForwardTimeoutMs: 5000}
			c.Timeout = 6000
			return c
		}(), wantErr: false},
		{name: "zone 类型名非法", cfg: Config{ForwardTimeoutMs: 5000, ZoneScopedNodeTypes: []string{"x"}}, wantErr: true},
		// Stat 开着但没屏蔽 Forward 的内容 = 明文密码进日志,必须拒绝启动。
		{name: "Stat 开启但未屏蔽 Forward 内容", cfg: func() Config {
			c := Config{ForwardTimeoutMs: 5000}
			c.Middlewares.Stat = true
			return c
		}(), wantErr: true},
		{name: "Stat 开启且已屏蔽 Forward 内容", cfg: func() Config {
			c := Config{ForwardTimeoutMs: 5000}
			c.Middlewares.Stat = true
			c.Middlewares.StatConf.IgnoreContentMethods = []string{ForwardFullMethod}
			return c
		}(), wantErr: false},
		{name: "Stat 整体关闭", cfg: func() Config {
			c := Config{ForwardTimeoutMs: 5000}
			c.Middlewares.Stat = false
			return c
		}(), wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// 仓库里的 etc/client_rpc_router.yaml 必须能被 go-zero 加载且通过 Validate:
// 这是本地一键脚本真正读的文件,配置结构改了必须同步。
func TestRepoYamlLoadsAndValidates(t *testing.T) {
	var c Config
	require.NoError(t, conf.Load("../../etc/client_rpc_router.yaml", &c))
	require.Equal(t, "client_rpc_router.rpc", c.Name)
	require.Equal(t, "127.0.0.1:50600", c.ListenOn)
	require.Equal(t, int64(5000), c.ForwardTimeoutMs)
	require.Equal(t, uint32(1), c.ZoneId)
	require.Equal(t, int64(60), c.LeaseTTL)
	require.Equal(t, []string{"LoginNodeService"}, c.ZoneScopedNodeTypes)
	require.Equal(t, "", c.MetricsListenAddr, "dev 默认关闭 /metrics")
	// go-zero 的 Stat 默认 true;yaml 必须把 Forward 列进 IgnoreContentMethods,
	// 否则每次转发都会把客户端原包(含登录密码)整包 JSON 打进 INFO 日志。
	require.True(t, c.Middlewares.Stat, "Stat 默认应为 true(说明这条屏蔽是必需的)")
	require.Contains(t, c.Middlewares.StatConf.IgnoreContentMethods, ForwardFullMethod)
	require.NoError(t, c.Validate())
}
