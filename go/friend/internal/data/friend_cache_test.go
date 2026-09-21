package data

// loadVersionedFriendCache 的纯缓存侧回归(handoff §2 第 8 步末段)。
//
// 本文件**不需要 MySQL**:repo 用 newCacheOnlyRepo(miniredis + nil DB,夹具在
// friend_repo_mysql_test.go),loader 是测试自己给的假函数,所以这里不挂 FRIEND_TEST_MYSQL_DSN、
// 也不 Skip —— DSN 忘了设的那天,这几条仍然在跑。
// miniredis 自带 gopher-lua,fillFriendCacheScript / invalidateFriendCacheScript 都是真执行,
// 不是被 mock 掉的:本文件钉的正是 Go 侧与 Lua 侧对 generation 的**字符串**约定是否对得上。
//
// 同文件族里已有的 TestVersionedCache_RejectsStaleFillAfterWriteInvalidation 直接 Eval 脚本,
// 验的是脚本本身;这里验的是**调用脚本的那段 Go**,两者互不替代。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cacheTestPlayerID 只用来拼键,与任何库里的数据无关。
const cacheTestPlayerID uint64 = 42

// countingFriendLoader 返回一个假 loader 与它的调用计数。
// 计数是"这次读有没有穿透到 MySQL"的唯一可观察量(真 loader 是 loadFriendListFromMySQL)。
func countingFriendLoader(value []FriendEntry) (func(context.Context) ([]FriendEntry, error), *int) {
	calls := 0
	return func(context.Context) ([]FriendEntry, error) {
		calls++
		return value, nil
	}, &calls
}

// TestVersionedCache_FillsWhenGenerationKeyNeverWritten 钉"generation 空串补成 \"0\""那处修复。
//
// generation 键只有写路径(invalidateCaches 的 INCR)才会建出来,所以**从未被写过好友数据的玩家**
// —— 也就是绝大多数第一次拉列表的玩家 —— 读到的 generation 是"键不存在"。go-zero 的 GetCtx 把
// redis.Nil 吞成 ("", nil),而 fill 脚本里 GET 不到时按 "0" 比较:Go 侧若把空串原样传下去,
// "" ~= "0" → 脚本恒 return 0 → **缓存永远写不进去**。全程零报错:EvalCtx 成功、返回值本来就丢弃、
// 调用方照样拿到正确的列表,唯一的症状是每次 GetFriendList 都打一次 MySQL(而它每次登录都发生)。
// 这种故障只有断言"数据键真的存在"才抓得住,所以下面看的是 mr.Exists,不是返回值。
func TestVersionedCache_FillsWhenGenerationKeyNeverWritten(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	ctx := context.Background()
	key := friendListKey(cacheTestPlayerID)
	require.False(t, mr.Exists(friendCacheGenerationKey(key)),
		"前置条件:generation 键从未写过 —— 本用例要走的就是这条分支")

	want := []FriendEntry{
		{FriendPlayerID: 99, SinceMs: 1700000000000},
		{FriendPlayerID: 100, SinceMs: 1700000000001},
	}
	loader, calls := countingFriendLoader(want)

	got, err := loadVersionedFriendCache(ctx, repo, key, loader)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 1, *calls)

	require.True(t, mr.Exists(key),
		"generation 键不存在时回填必须成功:数据键缺席说明空串没被补成 \"0\",fill 脚本恒 return 0,"+
			"缓存永远写不进去且零报错")
	payload, err := mr.Get(key)
	require.NoError(t, err)
	var cached []FriendEntry
	require.NoError(t, json.Unmarshal([]byte(payload), &cached), "缓存里的内容必须能解回 loader 给的值")
	assert.Equal(t, want, cached)
	assert.Greater(t, mr.TTL(key), time.Duration(0),
		"回填必须带 TTL(repo 的 defaultTTL):不带 TTL 的键只能靠写路径失效,漏一次失效就永久陈旧")
	assert.False(t, mr.Exists(friendCacheGenerationKey(key)),
		"读路径不得建 generation 键:它只归写路径的 INCR 所有")
}

// TestVersionedCache_FillsWhenGenerationAlreadyBumped 是上一条的对照组:generation 已经被写路径
// INCR 过(非 "0")时同样要能回填。两条合起来才说明"读回来的 generation 原样传给脚本"这件事成立,
// 而不是某一边碰巧写死了 "0"。
func TestVersionedCache_FillsWhenGenerationAlreadyBumped(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	ctx := context.Background()
	key := friendListKey(cacheTestPlayerID)
	// 用真实的失效入口把 generation 推到 2,而不是手工 SET:键名与取值形状都跟着生产代码走。
	require.NoError(t, repo.invalidateCaches(ctx, key))
	require.NoError(t, repo.invalidateCaches(ctx, key))

	want := []FriendEntry{{FriendPlayerID: 7, SinceMs: 5}}
	loader, _ := countingFriendLoader(want)
	got, err := loadVersionedFriendCache(ctx, repo, key, loader)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.True(t, mr.Exists(key), "generation 未在 loader 期间变化,就必须回填")
}

// TestVersionedCache_SecondReadIsServedFromCache:回填之后的第二次读不得再调 loader。
// 这是 cache-aside 的全部意义;它同时反向钉住上一条 —— 回填若静默失败,这里的计数会是 2。
func TestVersionedCache_SecondReadIsServedFromCache(t *testing.T) {
	repo, _ := newCacheOnlyRepo(t)
	ctx := context.Background()
	key := friendListKey(cacheTestPlayerID)

	want := []FriendEntry{{FriendPlayerID: 99, SinceMs: 1}}
	loader, calls := countingFriendLoader(want)

	first, err := loadVersionedFriendCache(ctx, repo, key, loader)
	require.NoError(t, err)
	second, err := loadVersionedFriendCache(ctx, repo, key, loader)
	require.NoError(t, err)

	assert.Equal(t, want, first)
	assert.Equal(t, want, second, "缓存命中解出来的值必须与 loader 的原值一致")
	assert.Equal(t, 1, *calls, "第二次读必须走缓存;loader 被调了两次说明第一次的回填没写进去")
}

// TestVersionedCache_EmptyListIsCachedToo:零好友的玩家也要能命中缓存。
// loadFriendListFromMySQL 刻意把 nil 归一成 []FriendEntry{}(序列化为 "[]" 而不是空串),
// readCache 才分得清"未命中"与"命中了一个空列表";分不清的话,零好友玩家 —— 新号的绝大多数 ——
// 每次登录都穿透到 MySQL。
func TestVersionedCache_EmptyListIsCachedToo(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	ctx := context.Background()
	key := friendListKey(cacheTestPlayerID)

	loader, calls := countingFriendLoader([]FriendEntry{})
	_, err := loadVersionedFriendCache(ctx, repo, key, loader)
	require.NoError(t, err)
	got, err := loadVersionedFriendCache(ctx, repo, key, loader)
	require.NoError(t, err)

	assert.Empty(t, got)
	assert.True(t, mr.Exists(key))
	assert.Equal(t, 1, *calls, "空列表同样是一次合法命中,不得被当成未命中反复穿透")
}

// TestVersionedCache_DoesNotFillWhenGenerationBumpedDuringLoad 钉经典 cache-aside 竞态:
// reader 未命中 → 读到旧 MySQL 快照 → writer 提交并失效缓存 → reader 最后才回填。
// 不挡住的话,旧快照会在写路径失效**之后**落进缓存,并合法地活满一个 CacheTTL
// (表现为"刚删掉的好友还在列表里")。
//
// 用 loader 内部调 invalidateCaches 来模拟"loader 执行期间有写事务提交":时序是确定的,
// 不需要 goroutine 与 sleep,所以不会 flaky。
func TestVersionedCache_DoesNotFillWhenGenerationBumpedDuringLoad(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	ctx := context.Background()
	key := friendListKey(cacheTestPlayerID)

	stale := []FriendEntry{{FriendPlayerID: 99, SinceMs: 1}}
	staleCalls := 0
	staleLoader := func(ctx context.Context) ([]FriendEntry, error) {
		staleCalls++
		// 此刻 reader 已经读过 generation(= "0"),快照也已经"读出来"了;
		// 写路径在它回填之前提交,走的是与生产完全相同的 INCR + DEL。
		require.NoError(t, repo.invalidateCaches(ctx, key))
		return stale, nil
	}

	got, err := loadVersionedFriendCache(ctx, repo, key, staleLoader)
	require.NoError(t, err, "放弃回填不是错误:本次读到的快照照样返回给调用方")
	assert.Equal(t, stale, got)
	assert.Equal(t, 1, staleCalls)
	assert.False(t, mr.Exists(key),
		"loader 期间 generation 变了就不得回填:否则写入前的旧快照会在失效之后落进缓存")

	// 被放弃的那次回填不得留下任何后遗症:下一次读必须重新穿透,并且这次能正常回填。
	fresh := []FriendEntry{}
	freshLoader, freshCalls := countingFriendLoader(fresh)
	got, err = loadVersionedFriendCache(ctx, repo, key, freshLoader)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 1, *freshCalls, "上一次没回填,这一次必须重新读权威数据")
	assert.True(t, mr.Exists(key), "generation 稳定之后回填要恢复正常")
}
