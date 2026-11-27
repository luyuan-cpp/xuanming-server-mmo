package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"table_sync/internal/pointer"
)

// 各 zone namespace 里的 StatefulSet 同名(guild-0、gate-0…)却共用 infra 的 etcd:状态键必须带 namespace,
// 否则两个 zone 的同名 Pod 在同一轨道上互相覆盖。不在 K8s 里跑(没有 namespace)时退回只用 Pod 名。
// gateway 的 /admin/tables 把轨道之后的整段当实例名,所以键里多一段 namespace 不需要改它的解析。
func TestStatusIdentityJoinsNamespaceSoSameNamedPodsInDifferentZonesStayApart(t *testing.T) {
	zone1 := syncConfig{PodName: "guild-0", PodNamespace: "mmorpg-zone-1"}
	zone2 := syncConfig{PodName: "guild-0", PodNamespace: "mmorpg-zone-2"}
	require.Equal(t, "mmorpg-zone-1/guild-0", zone1.statusIdentity())
	require.NotEqual(t,
		pointer.StatusKey("stable", zone1.statusIdentity()),
		pointer.StatusKey("stable", zone2.statusIdentity()))
	require.Equal(t, "/mmorpg/tables/status/stable/mmorpg-zone-1/guild-0", pointer.StatusKey("stable", zone1.statusIdentity()))

	local := syncConfig{PodName: "dev-host"}
	require.Equal(t, "dev-host", local.statusIdentity())
}
