package logic

// DataServicePlayerNames 的单测(设计 docs/design/guild-phase2/03-names.md §3.21「guild 测试」)。
//
// 这里钉的是 PlayerNameResolver 的契约本身:fail-open(任何错误 → 空 map、不 panic)、
// 去重去 0、每次查询都带有界超时。指标 guild_player_name_lookup_failed_total 不在这里断言数值:
// go-zero core/metric 的计数要过 prometheus.Enabled() 全局开关,单测进程里它是关的。

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	dspb "proto/data_service"
)

// fakePlayerNameService 只实现 BatchGetPlayerName;其余方法经内嵌的 nil 接口调用会 panic,
// 正好证明取名没有碰别的 RPC(与 home_zone_test.go 的 fakeDataService 同套路,
// 分开两个类型是因为两边记录的东西不同,合并只会让每个用例都背着用不到的字段)。
type fakePlayerNameService struct {
	dspb.DataServiceClient

	names map[uint64]string
	err   error
	// blockUntilCtxDone:不回包,一直等到调用方的 ctx 结束 —— 模拟 data_service 慢响应。
	blockUntilCtxDone bool

	mu          sync.Mutex
	calls       int
	lastIDs     []uint64
	hadDeadline bool
}

// slowResponseSafetyNet 只在被测代码**忘了设超时**时才会到期:那种情况下让用例失败而不是永远挂住。
// 正常路径上先到的一定是 resolver 自己的短超时,这个值不参与用例的时序。
const slowResponseSafetyNet = 5 * time.Second

func (f *fakePlayerNameService) BatchGetPlayerName(ctx context.Context, req *dspb.BatchGetPlayerNameRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerNameResponse, error) {
	f.mu.Lock()
	f.calls++
	f.lastIDs = append([]uint64(nil), req.GetPlayerIds()...)
	_, f.hadDeadline = ctx.Deadline()
	f.mu.Unlock()

	if f.blockUntilCtxDone {
		select {
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		case <-time.After(slowResponseSafetyNet):
			// 走到这里 = 调用方没给超时。回一份"成功"结果,让"慢响应必须得到空 map"的断言当场红掉。
			return &dspb.BatchGetPlayerNameResponse{Names: f.names}, nil
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return &dspb.BatchGetPlayerNameResponse{Names: f.names}, nil
}

func (f *fakePlayerNameService) snapshot() (calls int, lastIDs []uint64, hadDeadline bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.lastIDs, f.hadDeadline
}

func TestDataServicePlayerNames_MapsNames(t *testing.T) {
	// 3 号没登记名字(早于名字功能建的角色):不出现在结果里,不是错误。
	// 4 号回了空串:与"没登记"同义,同样不放进结果。
	client := &fakePlayerNameService{names: map[uint64]string{1: "云中君", 2: "青莲客", 4: ""}}

	names := NewDataServicePlayerNames(client, 0).BatchResolve(context.Background(), []uint64{1, 2, 3, 4})

	assert.Equal(t, map[uint64]string{1: "云中君", 2: "青莲客"}, names)
	calls, lastIDs, hadDeadline := client.snapshot()
	assert.Equal(t, 1, calls)
	assert.Equal(t, []uint64{1, 2, 3, 4}, lastIDs)
	assert.True(t, hadDeadline, "每次查询都必须带有界超时")
}

func TestDataServicePlayerNames_RPCErrorYieldsEmptyMap(t *testing.T) {
	cases := map[string]error{
		"data_service 不可用": status.Error(codes.Unavailable, "error_code=25: dial tcp"),
		"超限被拒":             status.Error(codes.InvalidArgument, "batch too large"),
		"非 gRPC 错误":        errors.New("boom"),
	}
	for name, rpcErr := range cases {
		t.Run(name, func(t *testing.T) {
			client := &fakePlayerNameService{err: rpcErr, names: map[uint64]string{1: "不该被读到"}}

			names := NewDataServicePlayerNames(client, 0).BatchResolve(context.Background(), []uint64{1, 2})

			// fail-open:调用方拿到的是一张可以直接按键取值的空 map,而不是 nil 或 error。
			require.NotNil(t, names)
			assert.Empty(t, names)
		})
	}
}

func TestDataServicePlayerNames_NilClientOrReceiverYieldsEmptyMap(t *testing.T) {
	t.Run("nil client", func(t *testing.T) {
		var names map[uint64]string
		require.NotPanics(t, func() {
			names = NewDataServicePlayerNames(nil, 0).BatchResolve(context.Background(), []uint64{1, 2})
		})
		require.NotNil(t, names)
		assert.Empty(t, names)
	})

	// nil 指针装进接口之后 != nil,WithPlayerNames 的判空拦不住它,只能靠 nil 接收者分支兜住。
	t.Run("nil receiver behind a non-nil interface", func(t *testing.T) {
		var resolver PlayerNameResolver = (*DataServicePlayerNames)(nil)
		var names map[uint64]string
		require.NotPanics(t, func() {
			names = resolver.BatchResolve(context.Background(), []uint64{1, 2})
		})
		require.NotNil(t, names)
		assert.Empty(t, names)
	})
}

func TestDataServicePlayerNames_DedupesAndDropsZero(t *testing.T) {
	t.Run("重复 id 与 0 只发一次、0 被丢弃", func(t *testing.T) {
		client := &fakePlayerNameService{names: map[uint64]string{7: "云中君"}}

		// 帮主 7 同时出现在成员列表与 leader_id 里,是 toProtoGuild 的常态输入。
		names := NewDataServicePlayerNames(client, 0).BatchResolve(context.Background(), []uint64{7, 0, 9, 7, 0, 9})

		assert.Equal(t, map[uint64]string{7: "云中君"}, names)
		calls, lastIDs, _ := client.snapshot()
		assert.Equal(t, 1, calls)
		assert.Equal(t, []uint64{7, 9}, lastIDs, "保持首次出现的顺序,排障时能和 data_service 的日志对上")
	})

	t.Run("清洗后为空就不发 RPC", func(t *testing.T) {
		for name, ids := range map[string][]uint64{"nil": nil, "空切片": {}, "全是 0": {0, 0}} {
			t.Run(name, func(t *testing.T) {
				client := &fakePlayerNameService{}

				names := NewDataServicePlayerNames(client, 0).BatchResolve(context.Background(), ids)

				require.NotNil(t, names)
				assert.Empty(t, names)
				calls, _, _ := client.snapshot()
				assert.Zero(t, calls)
			})
		}
	})
}

// TestDataServicePlayerNames_SlowResponseYieldsEmptyMap:慢响应不能拖住 GetGuild。
//
// 时序完全由被测代码的超时驱动:fake 一直阻塞到 resolver 的 ctx 到期才返回,
// 用例里没有任何 sleep,也不依赖"等多久算慢"这种墙钟判断。
func TestDataServicePlayerNames_SlowResponseYieldsEmptyMap(t *testing.T) {
	client := &fakePlayerNameService{blockUntilCtxDone: true, names: map[uint64]string{1: "来得太晚"}}
	resolver := NewDataServicePlayerNames(client, 20*time.Millisecond)

	names := resolver.BatchResolve(context.Background(), []uint64{1})

	require.NotNil(t, names)
	assert.Empty(t, names, "超时之后必须按取名失败处理,不能把迟到的结果当成功")
	calls, _, hadDeadline := client.snapshot()
	assert.Equal(t, 1, calls, "超时不重试:名字是展示数据,下一次拉取自然会再取")
	assert.True(t, hadDeadline)
}

// TestDataServicePlayerNames_ParentContextAlreadyDone:请求 ctx 已经结束(玩家断线 / 整请求预算耗尽)
// 时同样回空 map,不 panic、不阻塞,并且**不发 RPC**。
//
// 不发 RPC 是归因上的要求:toProtoGuild / applicantViews 都是先在线 MGET、后取名,而在线 MGET 没有
// 独立超时 —— locator Redis 卡住时它会先把整请求预算吃光。此时再发取名 RPC 只会立刻失败,
// 并把 guild_player_name_lookup_failed_total 与 ERROR 日志记到无辜的 data_service 头上。
// 计数器数值在单测里断言不了(见文件头),所以用"RPC 根本没发出去"钉住这条分支。
func TestDataServicePlayerNames_ParentContextAlreadyDone(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// 截止时间取一个确定已过去的时刻,不依赖"等多久才到期"的墙钟判断。
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()

	for name, ctx := range map[string]context.Context{"已取消": cancelled, "预算已耗尽": expired} {
		t.Run(name, func(t *testing.T) {
			client := &fakePlayerNameService{names: map[uint64]string{1: "不该被读到"}}

			names := NewDataServicePlayerNames(client, time.Minute).BatchResolve(ctx, []uint64{1})

			require.NotNil(t, names)
			assert.Empty(t, names)
			calls, _, _ := client.snapshot()
			assert.Zero(t, calls, "请求 ctx 已结束就不该再发取名 RPC")
		})
	}
}

// TestDataServicePlayerNames_TruncatesOversizeBatch:data_service 对超过 500 个 id 的请求回
// InvalidArgument 而不是截断(proto 注释)。整批被拒 = 所有人都没名字,所以这里自己截到上限:
// 前 500 个照常有名字,其余留空并记 ERROR。
func TestDataServicePlayerNames_TruncatesOversizeBatch(t *testing.T) {
	ids := make([]uint64, 0, playerNameBatchLimit+100)
	for id := uint64(1); id <= playerNameBatchLimit+100; id++ {
		ids = append(ids, id)
	}
	client := &fakePlayerNameService{names: map[uint64]string{1: "云中君"}}

	names := NewDataServicePlayerNames(client, 0).BatchResolve(context.Background(), ids)

	assert.Equal(t, map[uint64]string{1: "云中君"}, names)
	calls, lastIDs, _ := client.snapshot()
	assert.Equal(t, 1, calls, "超限也只发一次,不拆成多批(预算只够一次往返)")
	require.Len(t, lastIDs, playerNameBatchLimit)
	assert.Equal(t, ids[:playerNameBatchLimit], lastIDs)
}

func TestNewDataServicePlayerNames_DefaultTimeout(t *testing.T) {
	assert.Equal(t, DefaultPlayerNameLookupTimeout, NewDataServicePlayerNames(nil, 0).timeout)
	assert.Equal(t, DefaultPlayerNameLookupTimeout, NewDataServicePlayerNames(nil, -time.Second).timeout)
	assert.Equal(t, 50*time.Millisecond, NewDataServicePlayerNames(nil, 50*time.Millisecond).timeout)
}
