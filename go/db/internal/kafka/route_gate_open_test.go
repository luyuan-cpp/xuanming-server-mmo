package kafka

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RouteGateOpen 是就绪闸给存盘补投器(saveoutbox.RouteGate)的只读视图(r2-rr#5 数据侧):跟着每一次刷新走,标记不可信或读脚本
// 出错都关(fail-closed);不记等待次数(waits 只给停在闸前的消费循环 / 重试循环用);开关关着(闸为 nil)恒放行。
// 闸的刷新由用例直接调 probe / refreshGate,不依赖墙钟。
func TestRouteGateOpen_FollowsEachRefreshWithoutCountingWaits(t *testing.T) {
	reader := newFakeRouteReadyReader(t, nil)
	c := &KeyOrderedKafkaConsumer{routeGate: newRouteReadyGate(reader, time.Second)}
	assert.False(t, c.RouteGateOpen(), "the gate starts closed until the startup probe")

	require.NoError(t, c.routeGate.probe(context.Background()))
	assert.True(t, c.RouteGateOpen(), "a trusted marker opens the gate for the replay too")

	reader.setAbsent()
	refreshGate(c.routeGate)
	assert.False(t, c.RouteGateOpen(), "the reconcile leader withdrew the marker: the replay must pause")

	reader.setTrusted(t)
	refreshGate(c.routeGate)
	require.True(t, c.RouteGateOpen())
	reader.set(routeReadyTestMarker(t, routeReadyTestIdentity.RunID, routeReadyTestIdentity.MasterReplID),
		routeReadyTestIdentity, errors.New("dial tcp: connection refused"))
	refreshGate(c.routeGate)
	assert.False(t, c.RouteGateOpen(), "a failed read keeps the gate closed for the replay too")

	assert.Zero(t, c.routeGate.waits.Load(), "the replay's read-only view does not count as a consumer waiting at the gate")
	assert.True(t, (&KeyOrderedKafkaConsumer{}).RouteGateOpen(), "Placement.RequireRouteReady=false: no gate, always open")
}
