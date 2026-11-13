package store

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// 目录「失败只记日志、不改结局」那几步的计数与名字缓存失效(account_directory_events.go)。目录各步用 flowSteps,
// 名字登记表用 fakeRoleNames;收尾里删小表那一行要真库,对应用例在 account_role_name_tidb_test.go。

// recordedDirectoryEvents 记下每一次计数,按调用顺序。
type recordedDirectoryEvents struct {
	mu             sync.Mutex
	bindFailures   []string
	settleFailures []string
}

func (r *recordedDirectoryEvents) NameBindFailed(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindFailures = append(r.bindFailures, reason)
}

func (r *recordedDirectoryEvents) RemovalSettleFailed(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.settleFailures = append(r.settleFailures, step)
}

// recordedNameCache 记下每一次删缓存;fail 按调用顺序给错误,nil 表示这一次成功。
type recordedNameCache struct {
	deleted []uint64
	fail    []error
}

func (c *recordedNameCache) DelPlayerName(_ context.Context, player uint64) error {
	c.deleted = append(c.deleted, player)
	if len(c.fail) > 0 {
		err := c.fail[0]
		c.fail = c.fail[1:]
		return err
	}
	return nil
}

// 用例「名字绑定失败要计数」:建角提交之后绑定没做完,建角照常成功,按原因计一次;绑上了不计。
func TestBindFailureIsCountedWithItsReason(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(*flowSteps, *fakeRoleNames)
		want  string
	}{
		{"写名字行出错", func(steps *flowSteps, _ *fakeRoleNames) { steps.fail["bindName"] = []error{errFlow} }, NameBindFailureError},
		{"名字在别人名下", func(_ *flowSteps, names *fakeRoleNames) { names.rows["flow"], names.bound["flow"] = 900, true }, NameBindFailureTaken},
		{"复核角色行出错", func(steps *flowSteps, _ *fakeRoleNames) { steps.fail["readOwner"] = []error{nil, errFlow} }, NameBindFailureError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, steps, names := namedFlowStore(t)
			events := &recordedDirectoryEvents{}
			s.ConfigureEvents(events)
			tc.setup(steps, names)
			out, err := s.Create(ctx, "owner", steps.routed(11))
			require.NoError(t, err, "提交之后的绑定失败不改建角结局")
			require.Len(t, out.Players, 1)
			require.Equal(t, []string{tc.want}, events.bindFailures)
		})
	}

	s, steps, _ := namedFlowStore(t)
	events := &recordedDirectoryEvents{}
	s.ConfigureEvents(events)
	_, err := s.Create(ctx, "owner", steps.routed(11))
	require.NoError(t, err)
	require.Empty(t, events.bindFailures, "绑上了不计")
}

// 复核对不上、规则算不出判重键:不绑,原因各自分开计(bindRoleNameBestEffort 取 nameBindError 里的原因)。
func TestBindFailureReasonsOfRefusedBinds(t *testing.T) {
	ctx := context.Background()
	s, steps, _ := namedFlowStore(t)
	events := &recordedDirectoryEvents{}
	s.ConfigureEvents(events)

	steps.setOwner(11, "", 2, 0) // 已是墓碑:复核对不上
	s.bindRoleNameBestEffort(ctx, steps.routed(11))
	role := steps.routed(12)
	role.Name = "\x00bad" // 现行字符集规则算不出判重键
	s.bindRoleNameBestEffort(ctx, role)
	require.Equal(t, []string{NameBindFailureRoleMismatch, NameBindFailureRulesChanged}, events.bindFailures)
	require.Zero(t, steps.count("bindName"), "两种都不碰名字登记表")
}

// 补登记撞上并发删角:本次补写的那一行撤掉了,计 role_removed;补登记前后名字缓存都要删(补回期间读到的名字不能再展示)。
func TestBindRestoreRacingARemovalIsCountedAndInvalidatesTheCache(t *testing.T) {
	ctx := context.Background()
	s, steps, names := namedFlowStore(t)
	events, cache := &recordedDirectoryEvents{}, &recordedNameCache{}
	s.ConfigureEvents(events)
	s.ConfigureNameCache(cache)
	delete(names.rows, "flow")
	steps.setOwner(11, "owner", 1, 0)
	steps.before["bindName"] = func() { steps.setOwner(11, "", 2, 0) }
	s.bindRoleNameBestEffort(ctx, steps.routed(11))
	require.Equal(t, []string{NameBindFailureRoleRemoved}, events.bindFailures)
	require.Equal(t, []uint64{11}, cache.deleted)
}

// 用例「名字修复之后删名字缓存」(在役角色):绑定时名字行不在、按原名补回之后删这名玩家的名字缓存,
// 名字缺着那段时间记下的负缓存不用等它过期;名字行本来就在(普通绑定)不删。
func TestBindRestoredInvalidatesTheNameCache(t *testing.T) {
	ctx := context.Background()
	s, steps, names := namedFlowStore(t)
	cache := &recordedNameCache{}
	s.ConfigureNameCache(cache)
	delete(names.rows, "flow")
	steps.setOwner(11, "owner", 1, 0)
	require.NoError(t, s.bindRoleName(ctx, steps.routed(11)))
	require.Equal(t, []uint64{11}, cache.deleted, "补回之后马上删缓存")

	s, steps, _ = namedFlowStore(t)
	cache = &recordedNameCache{}
	s.ConfigureNameCache(cache)
	steps.setOwner(11, "owner", 1, 0)
	require.NoError(t, s.bindRoleName(ctx, steps.routed(11)))
	require.Empty(t, cache.deleted, "名字行本来就在:缓存里本来就是这个名字")
}

// 删角释放名字之后删名字缓存(已删角色的名字不再展示);删缓存失败按上限重来,还是失败只记日志,释放照算成功。
func TestReleaseRemovedRoleNameInvalidatesTheNameCache(t *testing.T) {
	ctx := context.Background()
	s, steps, _ := namedFlowStore(t)
	cache := &recordedNameCache{}
	s.ConfigureNameCache(cache)
	steps.setOwner(11, "", 2, 0)
	require.NoError(t, s.releaseRemovedRoleName(ctx, 11))
	require.Equal(t, []uint64{11}, cache.deleted)

	s, steps, _ = namedFlowStore(t)
	broken := errors.New("redis down")
	cache = &recordedNameCache{fail: []error{broken, broken, broken, broken}}
	s.ConfigureNameCache(cache)
	steps.setOwner(11, "", 2, 0)
	require.NoError(t, s.releaseRemovedRoleName(ctx, 11), "缓存只是展示用的读缓存:删不掉不改释放的结局")
	require.Len(t, cache.deleted, nameCacheInvalidateAttempts, "重来次数有上限")
}

// 用例「删角释放名字失败要计数」:释放失败,收尾回错误、计 release_name 一次;释放之前就失败的不碰缓存。
func TestRemovalSettleFailureIsCounted(t *testing.T) {
	ctx := context.Background()
	s, steps, _ := namedFlowStore(t)
	events, cache := &recordedDirectoryEvents{}, &recordedNameCache{}
	s.ConfigureEvents(events)
	s.ConfigureNameCache(cache)
	steps.setOwner(11, "", 2, 0)
	steps.fail["releaseRemovedName"] = []error{errFlow}
	err := s.settleRemovedRole(ctx, 11)
	require.ErrorIs(t, err, errFlow)
	require.Equal(t, []string{RemovalSettleReleaseName}, events.settleFailures)
	require.Empty(t, cache.deleted)
}

// 删缓存的重试:成功就停;ctx 结束就停,不睡满。
func TestDelNameCacheWithRetry(t *testing.T) {
	cache := &recordedNameCache{fail: []error{errFlow}}
	require.NoError(t, delNameCacheWithRetry(context.Background(), cache, 7))
	require.Equal(t, []uint64{7, 7}, cache.deleted, "第二次成功就停")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cache = &recordedNameCache{fail: []error{errFlow, errFlow, errFlow}}
	err := delNameCacheWithRetry(ctx, cache, 7)
	require.ErrorIs(t, err, errFlow)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, cache.deleted, 1, "ctx 结束之后不再重来")
}
