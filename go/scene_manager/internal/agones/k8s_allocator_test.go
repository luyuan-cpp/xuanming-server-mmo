package agones

// K8sAllocator 按条目增删 rooms 列表的单元测试(docs/design/agones-scene-node-high-density.md §13)。
//
// 用 client-go 自带的 dynamic fake:钉住读写的是 status.lists.rooms 这一个字段、增删按条目幂等、容量检查、
// 分配之后核对条目确实落进了列表。真集群上 Agones 是否接受直接改 status.lists、GSA 返回对象的字段形状,
// 仍要按设计稿 §13.8 上集群核对,这里覆盖不到,也不假装覆盖到了。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

const testNamespace = "mmorpg-zone-test"

func gameServerObject(name, podIP string, capacity int64, entries ...string) *unstructured.Unstructured {
	values := make([]any, 0, len(entries))
	for _, entry := range entries {
		values = append(values, entry)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "agones.dev/v1",
		"kind":       "GameServer",
		"metadata": map[string]any{
			"name":      name,
			"namespace": testNamespace,
			"labels":    map[string]any{"mmorpg.io/zone": "1"},
		},
		"status": map[string]any{
			"state":     "Allocated",
			"addresses": []any{map[string]any{"type": "PodIP", "address": podIP}},
			"lists":     map[string]any{RoomListName: map[string]any{"capacity": capacity, "values": values}},
		},
	}}
}

func newTestAllocator(highDensity bool, objects ...runtime.Object) (*K8sAllocator, *dynamicfake.FakeDynamicClient) {
	client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
	return NewK8sAllocator(client, K8sAllocatorOptions{HighDensity: highDensity, ConflictBackoff: time.Millisecond}), client
}

func listedEntries(t *testing.T, a *K8sAllocator, name string) []string {
	t.Helper()
	servers, err := a.ListGameServerRooms(context.Background(), testNamespace, "1")
	require.NoError(t, err)
	for _, gs := range servers {
		if gs.Name == name {
			require.EqualValues(t, len(gs.Entries), gs.Count())
			return gs.Entries
		}
	}
	t.Fatalf("gameserver %s not listed", name)
	return nil
}

// 加与删都按条目幂等:同一条加两次只占一份,删两次只还一份;满了拒绝;GameServer 不存在时加回没容量、删算成功。
func TestK8sAllocator_RoomEntriesAreIdempotent(t *testing.T) {
	a, _ := newTestAllocator(true, gameServerObject("gs-1", "10.0.0.8", 2))
	ctx := context.Background()

	require.NoError(t, a.AddRoomEntry(ctx, testNamespace, "gs-1", "1.a"))
	require.NoError(t, a.AddRoomEntry(ctx, testNamespace, "gs-1", "1.a"), "已在就是成功")
	require.Equal(t, []string{"1.a"}, listedEntries(t, a, "gs-1"))
	require.NoError(t, a.AddRoomEntry(ctx, testNamespace, "gs-1", "2.b"))
	err := a.AddRoomEntry(ctx, testNamespace, "gs-1", "3.c")
	require.True(t, errors.Is(err, ErrNoCapacity), "满了必须拒绝: %v", err)
	require.Equal(t, []string{"1.a", "2.b"}, listedEntries(t, a, "gs-1"))

	require.NoError(t, a.RemoveRoomEntry(ctx, testNamespace, "gs-1", "1.a"))
	require.NoError(t, a.RemoveRoomEntry(ctx, testNamespace, "gs-1", "1.a"), "不在就是成功")
	require.Equal(t, []string{"2.b"}, listedEntries(t, a, "gs-1"))

	require.NoError(t, a.RemoveRoomEntry(ctx, testNamespace, "gs-gone", "2.b"), "GameServer 已不存在,名额随它消失")
	require.True(t, errors.Is(a.AddRoomEntry(ctx, testNamespace, "gs-gone", "4.d"), ErrNoCapacity))
}

// 非高密度模式不记名额:增删直接成功,不碰 GameServer。
func TestK8sAllocator_NonHighDensityTracksNoRooms(t *testing.T) {
	a, _ := newTestAllocator(false, gameServerObject("gs-1", "10.0.0.8", 1, "1.a"))
	ctx := context.Background()
	require.NoError(t, a.AddRoomEntry(ctx, testNamespace, "gs-1", "2.b"))
	require.NoError(t, a.RemoveRoomEntry(ctx, testNamespace, "gs-1", "1.a"))
	require.Equal(t, []string{"1.a"}, listedEntries(t, a, "gs-1"))
}

// allocationReactor 让 GSA 的 Create 回一个已分配的结果;lists 为 nil 时返回对象里不带 status.lists。
// 收到的请求对象写进 *seen。
func allocationReactor(seen **unstructured.Unstructured, gsName string, lists map[string]any) clienttesting.ReactionFunc {
	return func(action clienttesting.Action) (bool, runtime.Object, error) {
		req := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured)
		*seen = req
		status := map[string]any{
			"state":          "Allocated",
			"gameServerName": gsName,
			"addresses":      []any{map[string]any{"type": "PodIP", "address": "10.0.0.8"}},
		}
		if lists != nil {
			status["lists"] = lists
		}
		resp := req.DeepCopy()
		resp.Object["status"] = status
		return true, resp, nil
	}
}

// 分配请求带列表选择器与加值动作;返回对象里有这一条才算成功,没有(列表已满时 Agones 不加值)按没容量处理。
func TestK8sAllocator_AllocateAddsTheEntryAndVerifiesIt(t *testing.T) {
	ctx := context.Background()
	req := AllocationRequest{Namespace: testNamespace, ZoneLabel: "1", RoleLabel: "instance", Entry: "9.x"}

	a, client := newTestAllocator(true)
	var seen *unstructured.Unstructured
	client.PrependReactor("create", "gameserverallocations", allocationReactor(&seen, "gs-1",
		map[string]any{RoomListName: map[string]any{"capacity": int64(4), "values": []any{"1.a", "9.x"}}}))
	result, err := a.Allocate(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "gs-1", result.GameServerName)
	require.Equal(t, "10.0.0.8", result.PodIP)
	require.EqualValues(t, 2, result.RoomsCount)
	require.EqualValues(t, 4, result.RoomsCapacity)

	addValues, found, err := unstructured.NestedSlice(seen.Object, "spec", "lists", RoomListName, "addValues")
	require.NoError(t, err)
	require.True(t, found, "分配请求要带 lists.rooms.addValues")
	require.Equal(t, []any{"9.x"}, addValues)
	selectors, _, err := unstructured.NestedSlice(seen.Object, "spec", "selectors")
	require.NoError(t, err)
	require.Len(t, selectors, 2, "先 Allocated 后 Ready")
	minAvailable, found, err := unstructured.NestedInt64(selectors[0].(map[string]any), "lists", RoomListName, "minAvailable")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 1, minAvailable, "选择器要求列表至少还有一个空位")

	b, client2 := newTestAllocator(true)
	client2.PrependReactor("create", "gameserverallocations", allocationReactor(&seen, "gs-1",
		map[string]any{RoomListName: map[string]any{"capacity": int64(1), "values": []any{"1.a"}}}))
	_, err = b.Allocate(ctx, req)
	require.True(t, errors.Is(err, ErrNoCapacity), "回了 Allocated 却没加上这一条,不能当成占到了: %v", err)
}

// 返回对象里没有 status.lists(Agones 版本不回这一段)时 GET 这台 GameServer 核对。
func TestK8sAllocator_AllocateChecksTheGameServerWhenTheResponseHasNoLists(t *testing.T) {
	ctx := context.Background()
	req := AllocationRequest{Namespace: testNamespace, ZoneLabel: "1", RoleLabel: "instance", Entry: "9.x"}
	var seen *unstructured.Unstructured

	a, client := newTestAllocator(true, gameServerObject("gs-1", "10.0.0.8", 4, "9.x"))
	client.PrependReactor("create", "gameserverallocations", allocationReactor(&seen, "gs-1", nil))
	result, err := a.Allocate(ctx, req)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.RoomsCount)

	b, client2 := newTestAllocator(true, gameServerObject("gs-1", "10.0.0.8", 4))
	client2.PrependReactor("create", "gameserverallocations", allocationReactor(&seen, "gs-1", nil))
	_, err = b.Allocate(ctx, req)
	require.True(t, errors.Is(err, ErrNoCapacity), "GameServer 上没有这一条: %v", err)
}

// 高密度模式下分配必须带条目:没有条目就无从按身份归还。
func TestK8sAllocator_HighDensityAllocationNeedsAnEntry(t *testing.T) {
	a, _ := newTestAllocator(true)
	_, err := a.Allocate(context.Background(), AllocationRequest{Namespace: testNamespace, ZoneLabel: "1", RoleLabel: "instance"})
	require.Error(t, err)
}
