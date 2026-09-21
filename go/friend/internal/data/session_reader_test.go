package data

// SessionReader 的四条行为回归(handoff §3 第 6a 条):分批、MGET 空串 = 离线、proto 解码失败降级、
// 只认 SESSION_STATE_ONLINE。
//
// 本文件**不需要 MySQL**:SessionReader 只读共享 Redis 的 player:session:<id>,miniredis 足够。
// 所以这里不挂 FRIEND_TEST_MYSQL_DSN、也不 Skip。
//
// 会话值由测试用 proto.Marshal(plpb.PlayerSession) 直接写进 miniredis —— 与真正的写者
// player_locator 同一种编码。键名刻意用 playerSessionKeyPrefix 拼而不是再抄一份字面量:
// 这里要验的是读法,不是键的拼写(拼写是跨运行时契约,由 session_reader.go 文件头钉着)。

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"

	plpb "proto/player_locator"
)

// newSessionReaderOnMiniredis 给一个挂在 miniredis 上的 SessionReader。
// batchSize 原样透传给 NewSessionReader:传 0 就是在验它的兜底。
func newSessionReaderOnMiniredis(t *testing.T, batchSize uint32) (*SessionReader, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	return NewSessionReader(rdb, batchSize), mr
}

// putPlayerSession 按 player_locator 的写法放一条会话。
func putPlayerSession(t *testing.T, mr *miniredis.Miniredis, playerID uint64, state plpb.PlayerSessionState, lastActiveMs int64) {
	t.Helper()
	payload, err := proto.Marshal(&plpb.PlayerSession{
		PlayerId:     playerID,
		State:        state,
		LastActiveTs: lastActiveMs,
	})
	require.NoError(t, err)
	require.NoError(t, mr.Set(sessionKeyForTest(playerID), string(payload)))
}

func sessionKeyForTest(playerID uint64) string {
	return fmt.Sprintf("%s%d", playerSessionKeyPrefix, playerID)
}

// sequentialPlayerIDs 返回 [first, first+n) 的连续 id。
func sequentialPlayerIDs(first uint64, n int) []uint64 {
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = first + uint64(i)
	}
	return ids
}

// mgetBatchRecorder 记下 miniredis 收到的每一次 MGET 各带了几个 key。
//
// 为什么不用 mr.CommandCount() 的前后差:go-zero 的连接池带 MinIdleConns=8,后台会异步预建连接,
// 而 go-redis 在每条**新连接首次使用**时会自己发握手命令(HELLO / CLIENT SETINFO)——
// 这些握手落在哪一段测量里取决于调度,按总命令数断言批数必然 flaky。
// 服务端 pre-hook 只数 MGET,与连接池怎么建连无关。
type mgetBatchRecorder struct {
	mu       sync.Mutex
	keyCount []int // 每次 MGET 的 key 个数,按到达顺序
}

// recordMGetBatches 给 miniredis 装上记录钩子。钩子返回 false = "命令没处理完,照常执行"。
// 钩子跑在 miniredis 的连接 goroutine 上,所以要锁。
func recordMGetBatches(mr *miniredis.Miniredis) *mgetBatchRecorder {
	rec := &mgetBatchRecorder{}
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, args ...string) bool {
		if cmd == "MGET" { // miniredis 交给钩子的命令名已经转成大写
			rec.mu.Lock()
			rec.keyCount = append(rec.keyCount, len(args))
			rec.mu.Unlock()
		}
		return false
	})
	return rec
}

func (rec *mgetBatchRecorder) batches() []int {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]int(nil), rec.keyCount...)
}

// TestSessionReader_ZeroBatchSizeFallsBackToDefaultAndSplits:batchSize 传 0 必须回落到
// defaultSessionBatchSize,而且 >256 个 id 真的被切成多批、各批结果合并完整。
//
// 为什么 0 值得专门钉:用 0 去切片是"永远切不完"的死循环(start += 0),比任何降级都糟;
// 生产上 config.Validate 拒收 ListReadHardLimit=0,但那道闸在另一个包里,这里不能指望它。
//
// 只给**分属不同批**的 id 写会话(每批的首尾各一个),再断言全都读到:
// "只发了第一批"、"最后一批不足额被丢掉"、"批边界差一"三种写错都会让这条变红。
func TestSessionReader_ZeroBatchSizeFallsBackToDefaultAndSplits(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 0)
	require.Equal(t, defaultSessionBatchSize, reader.batchSize, "batchSize=0 必须回落到兜底值,不能带着 0 去切片")

	const first uint64 = 1000
	const total = 2*defaultSessionBatchSize + 88 // 600:两个满批 + 一个不足额的尾批
	ids := sequentialPlayerIDs(first, total)

	// 下标 → 所在批:0/255 在第 1 批,256/511 在第 2 批,512/599 在第 3 批。
	onlineIndexes := []int{
		0, defaultSessionBatchSize - 1,
		defaultSessionBatchSize, 2*defaultSessionBatchSize - 1,
		2 * defaultSessionBatchSize, total - 1,
	}
	for _, idx := range onlineIndexes {
		putPlayerSession(t, mr, ids[idx], plpb.PlayerSessionState_SESSION_STATE_ONLINE, int64(idx)+1)
	}

	rec := recordMGetBatches(mr)
	states, err := reader.BatchOnlineStatus(context.Background(), ids)
	require.NoError(t, err)

	assert.Equal(t, []int{defaultSessionBatchSize, defaultSessionBatchSize, total - 2*defaultSessionBatchSize}, rec.batches(),
		"600 个 id、每批 256 → 恰好 3 次 MGET(256+256+88);只有 1 次说明根本没分批"+
			"(一个巨大的请求包 + Redis 单线程上的一次长阻塞)")
	require.Len(t, states, len(onlineIndexes), "结果只包含读到 ONLINE 会话的玩家,其余一律缺席")
	for _, idx := range onlineIndexes {
		got, ok := states[ids[idx]]
		require.True(t, ok, "下标 %d(id=%d)的会话没读到:它所在的那一批被漏掉了", idx, ids[idx])
		assert.True(t, got.Online)
		assert.Equal(t, int64(idx)+1, got.LastActiveMs,
			"LastActiveMs 必须来自**这名玩家自己**的会话:对不上说明批内下标错位,把 A 的状态贴到了 B 身上")
	}
}

// TestSessionReader_SmallBatchSizeIssuesExactBatches:用小 batchSize 精确钉批数与尾批。
// 5 个 id、每批 2 → 3 批(2+2+1)。最后那个落单的 id 是唯一在线的:尾批被丢就读不到它。
func TestSessionReader_SmallBatchSizeIssuesExactBatches(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 2)
	ids := sequentialPlayerIDs(2000, 5)
	putPlayerSession(t, mr, ids[4], plpb.PlayerSessionState_SESSION_STATE_ONLINE, 77)

	rec := recordMGetBatches(mr)
	states, err := reader.BatchOnlineStatus(context.Background(), ids)
	require.NoError(t, err)

	assert.Equal(t, []int{2, 2, 1}, rec.batches(), "5 个 id、每批 2 → 3 次 MGET,尾批只有 1 个 key")
	assert.Equal(t, map[uint64]OnlineStatus{ids[4]: {Online: true, LastActiveMs: 77}}, states)
}

// TestSessionReader_DeduplicatesAndSkipsZeroID:重复 id 与 0 不进 MGET。
// 重复 id 让同一个 key 在一次 MGET 里出现两次(浪费带宽、指标偏高);0 不是合法 player_id。
func TestSessionReader_DeduplicatesAndSkipsZeroID(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 2)
	putPlayerSession(t, mr, 7, plpb.PlayerSessionState_SESSION_STATE_ONLINE, 5)

	rec := recordMGetBatches(mr)
	states, err := reader.BatchOnlineStatus(context.Background(), []uint64{0, 7, 7, 8, 0, 8})
	require.NoError(t, err)

	assert.Equal(t, []int{2}, rec.batches(), "去重并剔掉 0 之后只剩 {7, 8},每批 2 → 1 次 MGET、2 个 key")
	assert.Equal(t, map[uint64]OnlineStatus{7: {Online: true, LastActiveMs: 5}}, states)

	// 全是非法 id 时一次 MGET 都不该发。
	got, err := reader.BatchOnlineStatus(context.Background(), []uint64{0, 0})
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Len(t, rec.batches(), 1, "入参里没有合法 id 时不得再发 MGET")
}

// TestSessionReader_MissingOrEmptyValueMeansOffline:key 缺席与值为空串都是"没有会话 = 离线",
// **不是错误**。这是最常见的分支(好友列表里多数人不在线),把它当错误会让每次拉列表都报 fault。
//
// 结果 map 的契约是"只包含读到会话的玩家":离线玩家**缺席**,调用方读 map 拿到零值
// (Online=false)。下面两种断言方式都写上,钉的是同一条契约的两个侧面。
func TestSessionReader_MissingOrEmptyValueMeansOffline(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 0)
	const (
		neverLoggedIn uint64 = 3001 // key 根本不存在
		emptyValue    uint64 = 3002 // key 在,但值是空串
		online        uint64 = 3003 // 对照组:证明这一批确实被读了,而不是整批被跳过
	)
	require.NoError(t, mr.Set(sessionKeyForTest(emptyValue), ""))
	putPlayerSession(t, mr, online, plpb.PlayerSessionState_SESSION_STATE_ONLINE, 123)

	states, err := reader.BatchOnlineStatus(context.Background(), []uint64{neverLoggedIn, emptyValue, online})
	require.NoError(t, err, "没有会话是正常分支,不得报错")

	assert.NotContains(t, states, neverLoggedIn)
	assert.NotContains(t, states, emptyValue)
	assert.False(t, states[neverLoggedIn].Online, "缺席 = 调用方读到零值 = 离线")
	assert.False(t, states[emptyValue].Online)
	assert.Equal(t, OnlineStatus{Online: true, LastActiveMs: 123}, states[online])

	// 一个在线的都没有时同样不是错误;返回的 map 可以是空的,但读它必须安全。
	states, err = reader.BatchOnlineStatus(context.Background(), []uint64{neverLoggedIn, emptyValue})
	require.NoError(t, err)
	assert.Empty(t, states)
	assert.False(t, states[neverLoggedIn].Online)
}

// TestSessionReader_UndecodablePayloadOnlyDegradesThatPlayer:一条解不开的值只能把**那一名玩家**
// 降级成离线,不得把同批其它玩家拖成离线,也不得让整次调用报错。
//
// 在线态是展示字段:一条坏值(写者换了格式 / key 被别人覆盖)让 GetFriendList 整体失败,
// 比"这个人显示成灰的"糟得多。坏值夹在两个好值**中间**,是为了同时钉住
// "解码失败后 continue 而不是 break / return"。
func TestSessionReader_UndecodablePayloadOnlyDegradesThatPlayer(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 0)
	const (
		before  uint64 = 4001
		corrupt uint64 = 4002
		after   uint64 = 4003
	)
	putPlayerSession(t, mr, before, plpb.PlayerSessionState_SESSION_STATE_ONLINE, 11)
	// 0xFF 开头是一个续位永远不结束的 varint tag,proto.Unmarshal 必然报错;
	// 不用"随便一段 ASCII":那有可能碰巧被解成一条合法但无意义的消息,用例就假绿了。
	require.NoError(t, mr.Set(sessionKeyForTest(corrupt), "\xff\xff\xff"))
	putPlayerSession(t, mr, after, plpb.PlayerSessionState_SESSION_STATE_ONLINE, 33)

	states, err := reader.BatchOnlineStatus(context.Background(), []uint64{before, corrupt, after})
	require.NoError(t, err, "单条解码失败在内部降级(计 error 指标 + 限流日志),不上抛")

	assert.NotContains(t, states, corrupt, "解不开的会话按离线处理")
	assert.Equal(t, OnlineStatus{Online: true, LastActiveMs: 11}, states[before])
	assert.Equal(t, OnlineStatus{Online: true, LastActiveMs: 33}, states[after],
		"坏值之后的玩家必须照常读到:解码失败不得中断同批的后续下标")
}

// TestSessionReader_OnlyOnlineStateCountsAsOnline:**只认 SESSION_STATE_ONLINE**。
//
// ⚠ 这是业务语义,不是实现细节,**不要顺手放宽**成 "state != OFFLINE" 或 "有会话就算在线":
//   - DISCONNECTING 是"断线等重连"的租约期(30s),此时客户端收不到任何东西;把它显示成在线,
//     玩家发了消息对方不回,会以为对方在装作没看见。
//   - UNKNOWN 是 proto3 的零值:一条 state 字段缺失的会话解出来就是它,当在线等于"默认在线"。
//   - 以后 player_locator 再加状态,新状态在这里默认判为不在线才是安全方向(fail-closed)。
//
// 每个非 ONLINE 状态都带一个非零的 LastActiveTs:钉住"不在线的玩家不出现在结果里",
// 而不是"出现了但 Online=false" —— 后者会把 last_active 这种展示字段漏给调用方。
func TestSessionReader_OnlyOnlineStateCountsAsOnline(t *testing.T) {
	notOnline := map[string]plpb.PlayerSessionState{
		"UNKNOWN(零值)":        plpb.PlayerSessionState_SESSION_STATE_UNKNOWN,
		"DISCONNECTING(租约期)": plpb.PlayerSessionState_SESSION_STATE_DISCONNECTING,
		"OFFLINE":            plpb.PlayerSessionState_SESSION_STATE_OFFLINE,
		// 枚举里还不存在的取值:模拟 player_locator 先于 friend 升级、加了新状态。
		"未来新增的未知取值": plpb.PlayerSessionState(99),
	}
	for name, state := range notOnline {
		t.Run(name, func(t *testing.T) {
			reader, mr := newSessionReaderOnMiniredis(t, 0)
			const (
				subject uint64 = 5001
				control uint64 = 5002 // 同批的 ONLINE 对照:证明判离线的原因是 state,不是这一批没读到
			)
			putPlayerSession(t, mr, subject, state, 999)
			putPlayerSession(t, mr, control, plpb.PlayerSessionState_SESSION_STATE_ONLINE, 1000)

			states, err := reader.BatchOnlineStatus(context.Background(), []uint64{subject, control})
			require.NoError(t, err)

			assert.NotContains(t, states, subject, "state=%v 必须判为不在线", state)
			assert.False(t, states[subject].Online)
			assert.Equal(t, OnlineStatus{Online: true, LastActiveMs: 1000}, states[control])
		})
	}
}

// TestSessionReader_RedisFailureDegradesToOffline:共享 Redis 不可用时,
// BatchOnlineStatus 把 err 交给调用方,而 logic 实际走的 FillOnlineStatus 把它吃掉 ——
// 两个入口都不得 panic,结果都是"全部离线"。好友列表不能因为一个展示字段整体失败。
func TestSessionReader_RedisFailureDegradesToOffline(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 0)
	putPlayerSession(t, mr, 6001, plpb.PlayerSessionState_SESSION_STATE_ONLINE, 1)
	mr.Close()

	states, err := reader.BatchOnlineStatus(context.Background(), []uint64{6001, 6002})
	require.Error(t, err, "MGET 失败必须从 err 出来,不能与「全员没登录」混成同一个返回")
	assert.False(t, states[6001].Online, "读失败的玩家一律按离线")

	filled := reader.FillOnlineStatus(context.Background(), []uint64{6001, 6002})
	assert.False(t, filled[6001].Online)
	assert.False(t, filled[6002].Online)
}

// TestSessionReader_NilReaderAndEmptyInputAreSafe:nil 接收者 / nil 句柄 / 空入参都返回空结果。
// 调用方(logic 的 onlineStates)不做判空,靠的就是这条。
func TestSessionReader_NilReaderAndEmptyInputAreSafe(t *testing.T) {
	var nilReader *SessionReader
	states, err := nilReader.BatchOnlineStatus(context.Background(), []uint64{1})
	require.NoError(t, err)
	assert.False(t, states[1].Online)

	noHandle := NewSessionReader(nil, 10)
	states, err = noHandle.BatchOnlineStatus(context.Background(), []uint64{1})
	require.NoError(t, err)
	assert.Empty(t, states)

	reader, _ := newSessionReaderOnMiniredis(t, 10)
	states, err = reader.BatchOnlineStatus(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, states)
}
