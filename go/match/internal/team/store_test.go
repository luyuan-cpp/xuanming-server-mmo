package team

// SharedRedis 存储层的 miniredis 测试(设计文档 docs/design/team-system.md §I.3 #6-#11、#19-#22)。
//
// 时钟:miniredis 固定在 storeBase(2033 年,远离真实墙钟),Lua 里的 TIME 取它;
// 推进时间用 mr.SetTime,让 key 过期用 mr.FastForward。本包不读 Go 墙钟,测试结果与运行时刻无关。

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	component "proto/common/component"
	teampb "proto/team"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"
)

var storeBase = time.UnixMilli(2_000_000_000_000).UTC()

func baseMs() uint64 { return uint64(storeBase.UnixMilli()) }

func setRedisMs(mr *miniredis.Miniredis, ms uint64) { mr.SetTime(time.UnixMilli(int64(ms)).UTC()) }

func newTestStore(t *testing.T, sessions Sessions, cfg RuleConfig) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(storeBase)
	rds := redis.MustNewRedis(redis.RedisConf{Host: mr.Addr(), Type: "node"})
	loader := func(_ context.Context, ids []uint64) Sessions {
		out := Sessions{}
		for _, id := range ids {
			if state, ok := sessions[id]; ok {
				out[id] = state
			}
		}
		return out
	}
	return NewStore(rds, loader, cfg), mr
}

func mustMutate(t *testing.T, s *Store, bind Binding, op Op) *MutateResult {
	t.Helper()
	res, err := s.Mutate(context.Background(), bind, op)
	require.NoError(t, err)
	return res
}

func mustCommit(t *testing.T, s *Store, bind Binding, op Op) *CommitResult {
	t.Helper()
	res := mustMutate(t, s, bind, op)
	require.Equal(t, OutcomeCommitted, res.Outcome, "code=%d param=%d", res.Code, res.CodeParam)
	return res.Commit
}

func mustCreate(t *testing.T, s *Store, leader, tid uint64) *CommitResult {
	t.Helper()
	return mustCommit(t, s, BindCreate(leader, tid), CreateOp(leader, tid, 1))
}

// mustJoin 走真实路径入队:申请 → 队长同意。
func mustJoin(t *testing.T, s *Store, leader, tid, pid uint64) *CommitResult {
	t.Helper()
	mustCommit(t, s, BindTarget(pid, tid), ApplyOp(pid, 1))
	return mustCommit(t, s, BindCaller(leader, tid), HandleApplicationOp(leader, pid, true))
}

func loadRecord(t *testing.T, mr *miniredis.Miniredis, tid uint64) *teampb.TeamRecord {
	t.Helper()
	raw := mr.HGet(recordKey(tid), recFieldPb)
	require.NotEmpty(t, raw, "记录 %d 不存在", tid)
	rec := &teampb.TeamRecord{}
	require.NoError(t, proto.Unmarshal([]byte(raw), rec))
	return rec
}

func u64s(v uint64) string { return strconv.FormatUint(v, 10) }

// ---- #6:S_COMMIT 拒绝分支不产生任何写入 ----

func TestCommitRejectBranchesWriteNothing(t *testing.T) {
	ctx := context.Background()
	s, mr := newTestStore(t, nil, RuleConfig{})
	const tidA, tidB = uint64(1001), uint64(1002)
	mustCreate(t, s, 1, tidA)
	mustJoin(t, s, 1, tidA, 3)
	mustCreate(t, s, 2, tidB)
	now := baseMs()
	recA := loadRecord(t, mr, tidA)
	verA := mr.HGet(recordKey(tidA), recFieldVer)

	t.Run("版本冲突", func(t *testing.T) {
		d := Apply(ApplyOp(6, 1), recA, now, nil, RuleConfig{})
		before := mr.Dump()
		out, err := s.commit(ctx, tidA, "99", d, now)
		require.NoError(t, err)
		require.Equal(t, commitConflict, out.status)
		require.Equal(t, before, mr.Dump())
	})
	t.Run("非建队操作遇到记录不存在一律冲突", func(t *testing.T) {
		d := Apply(ApplyOp(6, ruleZone), newRecord(1), now, nil, RuleConfig{})
		before := mr.Dump()
		out, err := s.commit(ctx, ruleTid, "1", d, now)
		require.NoError(t, err)
		require.Equal(t, commitConflict, out.status)
		require.Equal(t, before, mr.Dump())
		require.False(t, mr.Exists(recordKey(ruleTid)), "绝不复活已删除的记录")
	})
	t.Run("建队哨兵遇到已存在记录", func(t *testing.T) {
		d := Apply(CreateOp(9, tidA, 1), nil, now, nil, RuleConfig{})
		before := mr.Dump()
		out, err := s.commit(ctx, tidA, "new", d, now)
		require.NoError(t, err)
		require.Equal(t, commitConflict, out.status)
		require.Equal(t, before, mr.Dump())
	})
	t.Run("{-1} 新成员已在别队", func(t *testing.T) {
		recWithApp := withApplication(proto.Clone(recA).(*teampb.TeamRecord), 2, 1, now, now+ApplicationTTLMs)
		d := Apply(HandleApplicationOp(1, 2, true), recWithApp, now, nil, RuleConfig{})
		require.Equal(t, []uint64{2}, d.Joined)
		before := mr.Dump()
		out, err := s.commit(ctx, tidA, verA, d, now)
		require.NoError(t, err)
		require.Equal(t, commitMemberInTeam, out.status)
		require.Equal(t, 1, out.index)
		require.Equal(t, before, mr.Dump())
	})
	t.Run("{-1} 经 Mutate 回 MemberInTeam", func(t *testing.T) {
		mustCommit(t, s, BindTarget(5, tidA), ApplyOp(5, 1))
		mustCreate(t, s, 5, 1005) // 申请挂着的同时自己建了队
		before := mr.Dump()
		res := mustMutate(t, s, BindCaller(1, tidA), HandleApplicationOp(1, 5, true))
		require.Equal(t, OutcomeRejected, res.Outcome)
		require.Equal(t, ErrMemberInTeam, res.Code)
		require.Equal(t, uint64(5), res.CodeParam)
		require.Equal(t, before, mr.Dump())
	})
	t.Run("{-2} 保留成员索引指向别队", func(t *testing.T) {
		rec := loadRecord(t, mr, tidA)
		ver := mr.HGet(recordKey(tidA), recFieldVer)
		mr.HSet(playerIndexKey(3), indexFieldTid, "555")
		t.Cleanup(func() { mr.HSet(playerIndexKey(3), indexFieldTid, u64s(tidA)) })
		d := Apply(ApplyOp(6, 1), rec, now, nil, RuleConfig{})
		require.Equal(t, []uint64{1, 3}, d.Kept)
		before := mr.Dump()
		out, err := s.commit(ctx, tidA, ver, d, now)
		require.NoError(t, err)
		require.Equal(t, commitIndexMismatch, out.status)
		require.Equal(t, 2, out.index)
		require.Equal(t, before, mr.Dump())
	})
}

// ---- #7:epoch 语义与精度 ----

func TestEpochSemantics(t *testing.T) {
	s, mr := newTestStore(t, nil, RuleConfig{})
	b := baseMs()
	const tidA, tidB = uint64(2001), uint64(2002)

	created := mustCreate(t, s, 1, tidA)
	require.Equal(t, IndexEntry{TeamId: tidA, Epoch: b + 1}, created.Indexes[1], "索引缺失时按 Redis TIME 毫秒数 +1 起种")
	require.Equal(t, u64s(b+1), mr.HGet(playerIndexKey(1), indexFieldEpoch), "13 位毫秒不丢精度")
	require.Equal(t, uint64(1), created.Version)

	setRedisMs(mr, b+1000)
	applied := mustCommit(t, s, BindTarget(3, tidA), ApplyOp(3, 1))
	require.Equal(t, IndexEntry{TeamId: tidA, Epoch: b + 1}, applied.Indexes[1], "tid 不变 epoch 不变")
	require.Equal(t, uint64(2), applied.Version)
	joined := mustCommit(t, s, BindCaller(1, tidA), HandleApplicationOp(1, 3, true))
	require.Equal(t, IndexEntry{TeamId: tidA, Epoch: b + 1001}, joined.Indexes[3])
	require.Equal(t, uint64(3), joined.Version, "每次提交严格 +1")

	setRedisMs(mr, b+2000)
	kicked := mustCommit(t, s, BindCaller(1, tidA), KickOp(1, 3))
	require.Equal(t, IndexEntry{TeamId: 0, Epoch: b + 1002}, kicked.Indexes[3], "已存在时 +1")
	require.True(t, mr.Exists(playerIndexKey(3)), "离队不删索引")
	require.Equal(t, "0", mr.HGet(playerIndexKey(3), indexFieldTid))

	recreated := mustCreate(t, s, 3, tidB)
	require.Equal(t, IndexEntry{TeamId: tidB, Epoch: b + 1003}, recreated.Indexes[3])

	// 14 位 epoch:string.format("%.0f") 精确写回。
	mr.HSet(playerIndexKey(3), indexFieldEpoch, "12345678901234")
	left := mustCommit(t, s, BindCaller(3, tidB), LeaveOp(3))
	require.True(t, left.Decision.Disbanded)
	require.Equal(t, IndexEntry{TeamId: 0, Epoch: 12345678901235}, left.Indexes[3])
	require.Equal(t, "12345678901235", mr.HGet(playerIndexKey(3), indexFieldEpoch))

	// 保留成员索引被删:下一次提交用新时钟重新起种并重建。
	mr.Del(playerIndexKey(1))
	setRedisMs(mr, b+5000)
	rebuilt := mustCommit(t, s, BindTarget(7, tidA), ApplyOp(7, 1))
	require.Equal(t, IndexEntry{TeamId: tidA, Epoch: b + 5001}, rebuilt.Indexes[1])
	require.Equal(t, u64s(tidA), mr.HGet(playerIndexKey(1), indexFieldTid))
}

// ---- #7 补充:索引缺失时读路径回报的 epoch 不回退(§C.4 / §H.3) ----

// clientAccepts §H.3 客户端快照接受条件(team_id 冲突判定另算)。
func clientAccepts(curEpoch, curVersion, inEpoch, inVersion uint64) bool {
	return inEpoch > curEpoch || (inEpoch == curEpoch && inVersion >= curVersion)
}

func TestMissingIndexEpochIsMonotonic(t *testing.T) {
	ctx := context.Background()
	s, mr := newTestStore(t, nil, RuleConfig{})
	b := baseMs()
	const tid = uint64(2101)

	// 同一毫秒:缺失索引的读回报 nowMs,随后起种严格更大。若相等,两份视图 epoch 相同、team_id 不同,
	// 客户端按冲突判定丢弃并重拉,拿回的仍是同一对 → 死循环。
	fresh, _, err := s.ReadFree(ctx, 1)
	require.NoError(t, err)
	require.Zero(t, fresh.PlayerTeamId)
	require.Equal(t, b, fresh.PlayerEpoch, "索引缺失回报 Redis nowMs")
	require.False(t, mr.Exists(playerIndexKey(1)), "读路径不写索引")
	created := mustCreate(t, s, 1, tid)
	require.Greater(t, created.Indexes[1].Epoch, fresh.PlayerEpoch)

	// 整队 24h 空闲过期:记录、投影、全员索引同批消失(Redis 时钟同步前进)。之后的空视图必须能被客户端接受。
	joined := mustJoin(t, s, 1, tid, 3)
	expiredAt := b + 25*3600*1000
	setRedisMs(mr, expiredAt)
	mr.FastForward(25 * time.Hour)
	require.False(t, mr.Exists(recordKey(tid)))
	for _, pid := range []uint64{1, 3} {
		last := joined.Indexes[pid]
		require.False(t, mr.Exists(playerIndexKey(pid)))
		gone, _, err := s.ReadFree(ctx, pid)
		require.NoError(t, err)
		require.Zero(t, gone.PlayerTeamId)
		require.True(t, clientAccepts(last.Epoch, joined.Version, gone.PlayerEpoch, 0),
			"player %d 过期前 epoch=%d,过期后空视图 epoch=%d 必须被接受", pid, last.Epoch, gone.PlayerEpoch)
		// 离队 / 解散等未绑定分支的回包视图同样取这份 S_READ。
		res := mustMutate(t, s, BindCaller(pid, tid), LeaveOp(pid))
		require.Equal(t, OutcomeNotBound, res.Outcome)
		require.Equal(t, gone.PlayerEpoch, res.Snapshot.PlayerEpoch)
	}

	// 过期后同一毫秒重新建队:起种仍严格大于刚回报的空视图 epoch。
	again, _, err := s.ReadFree(ctx, 3)
	require.NoError(t, err)
	recreated := mustCreate(t, s, 3, tid+1)
	require.Greater(t, recreated.Indexes[3].Epoch, again.PlayerEpoch)

	// 孤儿自愈在 epoch 缺失时的起种与 S_COMMIT 同口径(nowMs+1)。
	mr.HSet(playerIndexKey(9), indexFieldTid, "424242")
	orphan, healed, err := s.ReadFree(ctx, 9)
	require.NoError(t, err)
	require.True(t, healed)
	require.Zero(t, orphan.PlayerTeamId)
	require.Equal(t, expiredAt+1, orphan.PlayerEpoch)
}

// ---- #8:投影钉住 scene 契约;解散删记录与投影 ----

func TestProjectionAndDisband(t *testing.T) {
	s, mr := newTestStore(t, nil, RuleConfig{})
	const tid = uint64(3001)
	mustCreate(t, s, 1, tid)
	joined := mustJoin(t, s, 1, tid, 3)

	raw, err := mr.Get(projectionKey(tid))
	require.NoError(t, err)
	info := &component.TeamInfo{}
	require.NoError(t, proto.Unmarshal([]byte(raw), info), "team:<tid> 必须能按 TeamInfo 反序列化")
	require.Equal(t, tid, info.GetTeamId())
	require.Equal(t, uint64(1), info.GetLeaderId())
	require.ElementsMatch(t, []uint64{1, 3}, info.GetMembers())
	for _, key := range []string{recordKey(tid), projectionKey(tid), playerIndexKey(1), playerIndexKey(3)} {
		require.Equal(t, TeamIdleTTLSeconds*time.Second, mr.TTL(key), key)
	}

	disbanded := mustCommit(t, s, BindCaller(1, tid), DisbandOp(1))
	require.Equal(t, joined.Version+1, disbanded.Version)
	require.False(t, mr.Exists(recordKey(tid)))
	require.False(t, mr.Exists(projectionKey(tid)))
	for _, pid := range []uint64{1, 3} {
		require.Equal(t, uint64(0), disbanded.Indexes[pid].TeamId)
		require.Equal(t, "0", mr.HGet(playerIndexKey(pid), indexFieldTid))
		require.NotZero(t, disbanded.Indexes[pid].Epoch)
	}

	// 解散后重放:绑定仍是旧队但记录已无 → 不绑定(索引已是 0)。
	require.Equal(t, OutcomeNotBound, mustMutate(t, s, BindCaller(1, tid), DisbandOp(1)).Outcome)
}

// ---- #9:自愈 ----

func TestSelfHealing(t *testing.T) {
	ctx := context.Background()

	t.Run("成员索引被删,下一次提交重建", func(t *testing.T) {
		s, mr := newTestStore(t, nil, RuleConfig{})
		mustCreate(t, s, 1, 4001)
		mustJoin(t, s, 1, 4001, 3)
		mr.Del(playerIndexKey(3))
		mustCommit(t, s, BindTarget(7, 4001), ApplyOp(7, 1))
		require.Equal(t, "4001", mr.HGet(playerIndexKey(3), indexFieldTid))
	})

	t.Run("索引指向别队:{-2} 由 Go 修复后重算原操作", func(t *testing.T) {
		s, mr := newTestStore(t, nil, RuleConfig{})
		mustCreate(t, s, 1, 4002)
		mustJoin(t, s, 1, 4002, 3)
		mr.HSet(playerIndexKey(3), indexFieldTid, "555")
		res := mustMutate(t, s, BindTarget(7, 4002), ApplyOp(7, 1))
		require.Equal(t, OutcomeCommitted, res.Outcome)
		require.Len(t, res.Repairs, 1)
		fix := res.Repairs[0]
		require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_HEALED, fix.Decision.Reason)
		require.Equal(t, []uint64{3}, fix.Decision.Left)
		require.Equal(t, uint64(555), fix.Indexes[3].TeamId, "不改别队的索引,如实回报")
		require.Equal(t, fix.Version+1, res.Commit.Version)
		rec := loadRecord(t, mr, 4002)
		require.Equal(t, []uint64{1}, MemberIds(rec))
		require.NotNil(t, findApplication(rec, 7))
		require.Equal(t, "555", mr.HGet(playerIndexKey(3), indexFieldTid))
	})

	// Lua 只回报第一个错位下标;修复提交本身再 {-2} 时必须级联移出,否则每轮都卡在同一个人身上(§C.6 自愈走不通)。
	t.Run("两名保留成员索引都指向别队:修复一次移出两人,原操作照常提交", func(t *testing.T) {
		s, mr := newTestStore(t, nil, RuleConfig{})
		const tid = uint64(4006)
		mustCreate(t, s, 1, tid)
		mustJoin(t, s, 1, tid, 3)
		mustJoin(t, s, 1, tid, 4)
		mr.HSet(playerIndexKey(3), indexFieldTid, "555")
		mr.HSet(playerIndexKey(4), indexFieldTid, "556")

		res := mustMutate(t, s, BindTarget(7, tid), ApplyOp(7, 1))

		require.Equal(t, OutcomeCommitted, res.Outcome, "code=%d", res.Code)
		require.Len(t, res.Repairs, 1)
		fix := res.Repairs[0]
		require.Equal(t, teampb.TeamChangeReason_TEAM_CHANGE_REASON_HEALED, fix.Decision.Reason)
		require.Equal(t, []uint64{3, 4}, fix.Decision.Left, "Left 相对原记录计算,两名错位者都在")
		require.Equal(t, []uint64{1}, fix.Decision.Kept)
		require.Equal(t, uint64(555), fix.Indexes[3].TeamId, "不改别队的索引,如实回报")
		require.Equal(t, uint64(556), fix.Indexes[4].TeamId)
		require.Equal(t, fix.Version+1, res.Commit.Version)
		rec := loadRecord(t, mr, tid)
		require.Equal(t, []uint64{1}, MemberIds(rec))
		require.NotNil(t, findApplication(rec, 7))
	})

	t.Run("两名成员错位:开战锁钉版本提交与 EndMatch 都能推进", func(t *testing.T) {
		prevSleep := endMatchSleepFn
		endMatchSleepFn = func(time.Duration) { t.Fatalf("修复后应立即重读,不应退避") }
		t.Cleanup(func() { endMatchSleepFn = prevSleep })
		s, mr := newTestStore(t, nil, RuleConfig{})

		const lockTid = uint64(4007)
		mustCreate(t, s, 1, lockTid)
		mustJoin(t, s, 1, lockTid, 3)
		mustJoin(t, s, 1, lockTid, 4)
		snap, _, err := s.ReadFree(ctx, 1)
		require.NoError(t, err)
		mr.HSet(playerIndexKey(3), indexFieldTid, "555")
		mr.HSet(playerIndexKey(4), indexFieldTid, "556")
		lock, err := s.CommitMatchLock(ctx, snap, 1, "tok", MatchRoster(snap.Record), snap.NowMs+60_000)
		require.NoError(t, err)
		require.Nil(t, lock.Commit)
		require.True(t, lock.Retry)
		require.Len(t, lock.Repairs, 1)
		require.Equal(t, []uint64{3, 4}, lock.Repairs[0].Decision.Left)
		snap, _, err = s.ReadFree(ctx, 1)
		require.NoError(t, err)
		lock, err = s.CommitMatchLock(ctx, snap, 1, "tok", MatchRoster(snap.Record), snap.NowMs+60_000)
		require.NoError(t, err)
		require.NotNil(t, lock.Commit, "整轮重来后在修复过的记录上加锁成功")

		const endTid = uint64(4008)
		mustCreate(t, s, 11, endTid)
		mustJoin(t, s, 11, endTid, 13)
		mustJoin(t, s, 11, endTid, 14)
		snap, _, err = s.ReadFree(ctx, 11)
		require.NoError(t, err)
		lock, err = s.CommitMatchLock(ctx, snap, 11, "tok-end", MatchRoster(snap.Record), snap.NowMs+60_000)
		require.NoError(t, err)
		require.NotNil(t, lock.Commit)
		mr.HSet(playerIndexKey(13), indexFieldTid, "557")
		mr.HSet(playerIndexKey(14), indexFieldTid, "558")
		end := s.EndMatch(endTid, "tok-end", false)
		require.Equal(t, EndMatchReleased, end.Stop)
		require.Len(t, end.Repairs, 1)
		require.Equal(t, []uint64{13, 14}, end.Repairs[0].Decision.Left)
		rec := loadRecord(t, mr, endTid)
		require.Empty(t, rec.GetMatchLockToken())
		require.Equal(t, []uint64{11}, MemberIds(rec))
	})

	t.Run("孤儿索引:ReadFree 与 Mutate 都会 S_HEAL_ORPHAN", func(t *testing.T) {
		s, mr := newTestStore(t, nil, RuleConfig{})
		created := mustCreate(t, s, 1, 4003)
		mr.Del(recordKey(4003))
		snap, healed, err := s.ReadFree(ctx, 1)
		require.NoError(t, err)
		require.True(t, healed)
		require.Zero(t, snap.PlayerTeamId)
		require.Nil(t, snap.Record)
		require.Equal(t, created.Indexes[1].Epoch+1, snap.PlayerEpoch)

		mustCreate(t, s, 2, 4004)
		mr.Del(recordKey(4004))
		res := mustMutate(t, s, BindCaller(2, 4004), LeaveOp(2))
		require.Equal(t, OutcomeRecordMissing, res.Outcome)
		require.True(t, res.HealedOrphan)
		require.Equal(t, "0", mr.HGet(playerIndexKey(2), indexFieldTid))

		ok, err := s.HealOrphan(ctx, 1, 4003)
		require.NoError(t, err)
		require.False(t, ok, "索引已不指向该队时不写")
		mustCreate(t, s, 5, 4005)
		ok, err = s.HealOrphan(ctx, 5, 4005)
		require.NoError(t, err)
		require.False(t, ok, "记录还在时不写")
	})
}

// ---- #10:S_TOUCH ----

func TestTouchRenewsWithoutBumpingVersion(t *testing.T) {
	ctx := context.Background()
	s, mr := newTestStore(t, nil, RuleConfig{})
	mustCreate(t, s, 1, 5001)

	mr.FastForward(13 * time.Hour)
	snap, _, err := s.ReadFree(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, int64(11*3600), snap.RecordTTLSeconds)
	require.True(t, NeedsTouch(snap))
	mr.Del(projectionKey(5001))

	ok, err := s.Touch(ctx, snap)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "1", mr.HGet(recordKey(5001), recFieldVer), "触碰不改 ver")
	require.Equal(t, TeamIdleTTLSeconds*time.Second, mr.TTL(recordKey(5001)))
	require.Equal(t, TeamIdleTTLSeconds*time.Second, mr.TTL(playerIndexKey(1)))
	require.True(t, mr.Exists(projectionKey(5001)), "投影缺失时按该版记录重写")
	require.Equal(t, TeamIdleTTLSeconds*time.Second, mr.TTL(projectionKey(5001)))

	fresh, _, err := s.ReadFree(ctx, 1)
	require.NoError(t, err)
	require.False(t, NeedsTouch(fresh))

	mustCommit(t, s, BindTarget(7, 5001), ApplyOp(7, 1))
	ok, err = s.Touch(ctx, snap)
	require.NoError(t, err)
	require.False(t, ok, "ver 已变的旧快照不能续期")

	mr.FastForward(25 * time.Hour)
	gone, _, err := s.ReadFree(ctx, 1)
	require.NoError(t, err)
	require.Zero(t, gone.PlayerTeamId, "整体空闲过期后视为无队")
	require.Nil(t, gone.Record)
}

// ---- #19:建队哨兵;解散与接受邀请交错 ----

func TestCreateSentinelAndDisbandAcceptRace(t *testing.T) {
	ctx := context.Background()
	s, mr := newTestStore(t, nil, RuleConfig{})
	const tid = uint64(6001)
	mustCreate(t, s, 1, tid)
	mustCommit(t, s, BindCaller(1, tid), InviteOp(1, 4, 1))

	var once sync.Once
	afterReadHook = func(b Binding) {
		if b.PlayerId() != 4 {
			return
		}
		once.Do(func() {
			res, err := s.Mutate(ctx, BindCaller(1, tid), DisbandOp(1))
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, res.Outcome)
		})
	}
	t.Cleanup(func() { afterReadHook = nil })

	res := mustMutate(t, s, BindTarget(4, tid), RespondInviteOp(4, true))
	require.Equal(t, OutcomeRecordMissing, res.Outcome, "首次提交 {0},重读发现记录已删")
	require.False(t, mr.Exists(recordKey(tid)), "不重建 team:rec")
	require.False(t, mr.Exists(projectionKey(tid)), "不重建 team:<tid>")
	require.NotEqual(t, u64s(tid), mr.HGet(playerIndexKey(4), indexFieldTid))
	afterReadHook = nil

	mustCreate(t, s, 5, 6002)
	dup := mustMutate(t, s, BindCreate(6, 6002), CreateOp(6, 6002, 1))
	require.Equal(t, OutcomeRejected, dup.Outcome)
	require.Equal(t, ErrInternal, dup.Code, "新发的 team_id 已有记录是发号器故障")
	require.False(t, mr.Exists(playerIndexKey(6)))
}

// ---- #20:过期后重邀同一人 ----

func TestReinviteAfterExpiryKeepsIndex(t *testing.T) {
	ctx := context.Background()
	s, mr := newTestStore(t, nil, RuleConfig{})
	const tid = uint64(7001)
	b := baseMs()
	mustCreate(t, s, 1, tid)
	mustCommit(t, s, BindCaller(1, tid), InviteOp(1, 9, 1))
	score, err := mr.ZScore(inviteIndexKey(9), u64s(tid))
	require.NoError(t, err)
	require.Equal(t, float64(b+InviteTTLMs), score)

	setRedisMs(mr, b+InviteTTLMs) // 恰好过期
	again := mustCommit(t, s, BindCaller(1, tid), InviteOp(1, 9, 1))
	require.NotContains(t, again.Decision.InvitesRemoved, uint64(9), "Go 侧 ID := ID \\ IA")
	score, err = mr.ZScore(inviteIndexKey(9), u64s(tid))
	require.NoError(t, err)
	require.Equal(t, float64(b+2*InviteTTLMs), score)

	// 故意喂重叠集合:校验层拒绝;绕过校验直接执行 Lua,先 ZREM 后 ZADD 仍不丢索引。
	rec := loadRecord(t, mr, tid)
	overlap := Decision{
		Changed:        true,
		Record:         rec,
		Kept:           MemberIds(rec),
		InvitesAdded:   []InviteIndexAdd{{InviteeId: 9, ExpireAtMs: b + 5*InviteTTLMs}},
		InvitesRemoved: []uint64{9},
	}
	require.Error(t, validateCommitSets(tid, overlap))
	keys, args, err := buildCommitArgs(tid, mr.HGet(recordKey(tid), recFieldVer), overlap)
	require.NoError(t, err)
	raw, err := s.rds.EvalCtx(ctx, scriptCommit, keys, args...)
	require.NoError(t, err)
	require.Equal(t, int64(1), raw.([]any)[0])
	score, err = mr.ZScore(inviteIndexKey(9), u64s(tid))
	require.NoError(t, err)
	require.Equal(t, float64(b+5*InviteTTLMs), score)
}

// ---- #21:被邀请人待处理邀请上限 ----

func TestInviteeLimitIsAtomic(t *testing.T) {
	s, mr := newTestStore(t, nil, RuleConfig{})
	const invitee = uint64(50)
	const teams = MaxPendingInvitesPerInvitee + 1
	for i := 0; i < teams; i++ {
		mustCreate(t, s, uint64(101+i), uint64(8101+i))
	}

	results := make([]*MutateResult, teams)
	errs := make([]error, teams)
	var wg sync.WaitGroup
	for i := 0; i < teams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leader, tid := uint64(101+i), uint64(8101+i)
			results[i], errs[i] = s.Mutate(context.Background(), BindCaller(leader, tid), InviteOp(leader, invitee, 1))
		}(i)
	}
	wg.Wait()
	committed, limited := 0, 0
	for i, res := range results {
		require.NoError(t, errs[i])
		switch res.Outcome {
		case OutcomeCommitted:
			committed++
		case OutcomeRejected:
			require.Equal(t, ErrInviteLimit, res.Code)
			require.Equal(t, invitee, res.CodeParam)
			require.Nil(t, findInvite(loadRecord(t, mr, uint64(8101+i)), invitee), "被拒的队伍记录里不能有这条邀请")
			limited++
		default:
			t.Fatalf("意外结局 %d", res.Outcome)
		}
	}
	require.Equal(t, MaxPendingInvitesPerInvitee, committed)
	require.Equal(t, 1, limited)

	// 确定性复验:拒绝分支前后 Dump 完全一致。
	mustCreate(t, s, 112, 8112)
	before := mr.Dump()
	res := mustMutate(t, s, BindCaller(112, 8112), InviteOp(112, invitee, 1))
	require.Equal(t, ErrInviteLimit, res.Code)
	require.Equal(t, before, mr.Dump())

	// 已有本队条目 = 刷新,不占新名额。
	for i, r := range results {
		if r.Outcome == OutcomeCommitted {
			mustCommit(t, s, BindCaller(uint64(101+i), uint64(8101+i)), InviteOp(uint64(101+i), invitee, 1))
			break
		}
	}

	// 全部过期后不再计数,且写入前清掉过期反查项。
	setRedisMs(mr, baseMs()+InviteTTLMs)
	mustCommit(t, s, BindCaller(112, 8112), InviteOp(112, invitee, 1))
	members, err := mr.ZMembers(inviteIndexKey(invitee))
	require.NoError(t, err)
	require.Equal(t, []string{"8112"}, members)
}

// ---- #22:时钟只随 Redis TIME 变化 ----

func TestRedisClockDrivesExpiry(t *testing.T) {
	s, mr := newTestStore(t, nil, RuleConfig{})
	b := baseMs()

	mustCreate(t, s, 1, 9001)
	mustCommit(t, s, BindTarget(3, 9001), ApplyOp(3, 1))
	setRedisMs(mr, b+ApplicationTTLMs-1)
	mustCommit(t, s, BindCaller(1, 9001), HandleApplicationOp(1, 3, true))

	setRedisMs(mr, b)
	mustCreate(t, s, 2, 9002)
	mustCommit(t, s, BindTarget(5, 9002), ApplyOp(5, 1))
	setRedisMs(mr, b+ApplicationTTLMs)
	res := mustMutate(t, s, BindCaller(2, 9002), HandleApplicationOp(2, 5, true))
	require.Equal(t, ErrApplicationNotFound, res.Code, "expire <= Redis now 即过期")

	// 开战锁有效性只看 Redis 时钟。
	setRedisMs(mr, b)
	mustJoin(t, s, 1, 9001, 6)
	rec := loadRecord(t, mr, 9001)
	rec.MatchLockToken = "tok"
	rec.MatchLockExpireAtMs = b + 10_000
	rec.MatchLockRoster = MemberIds(rec)
	lock := Decision{Changed: true, Record: rec, Kept: MemberIds(rec)}
	out, err := s.commit(context.Background(), 9001, mr.HGet(recordKey(9001), recFieldVer), lock, b)
	require.NoError(t, err)
	require.Equal(t, commitOK, out.status)
	setRedisMs(mr, b+9_999)
	require.Equal(t, ErrInMatch, mustMutate(t, s, BindCaller(1, 9001), KickOp(1, 6)).Code)
	setRedisMs(mr, b+10_000)
	mustCommit(t, s, BindCaller(1, 9001), KickOp(1, 6))

	// 把 Redis 时钟拨到 2001 年:记录里的时间戳跟着 Redis 走,与 Go 墙钟无关。
	old := uint64(1_000_000_000_000)
	setRedisMs(mr, old)
	mustCreate(t, s, 20, 9003)
	mustCommit(t, s, BindCaller(20, 9003), InviteOp(20, 21, 1))
	inv := findInvite(loadRecord(t, mr, 9003), 21)
	require.Equal(t, old, inv.GetInvitedAtMs())
	require.Equal(t, old+InviteTTLMs, inv.GetExpireAtMs())
}

// ---- S_READ_MEMBERS / S_INVITE_LIST / S_INVITE_PRUNE ----

func TestReadMembersPairsTidAndEpoch(t *testing.T) {
	ctx := context.Background()
	s, mr := newTestStore(t, nil, RuleConfig{})
	mustCreate(t, s, 1, 9101)
	joined := mustJoin(t, s, 1, 9101, 3)
	mr.HSet(playerIndexKey(3), indexFieldTid, "555")

	snap, err := s.ReadMembers(ctx, 9101, []uint64{1}) // 传入过期的成员表 → 自动用新表重读
	require.NoError(t, err)
	require.Equal(t, joined.Version, snap.Version)
	require.Equal(t, IndexEntry{TeamId: 9101, Epoch: joined.Indexes[1].Epoch}, snap.Indexes[1])
	require.Equal(t, uint64(555), snap.Indexes[3].TeamId, "tid 不等的成员如实回报,由服务层跳过推送")

	missing, err := s.ReadMembers(ctx, 424242, nil)
	require.NoError(t, err)
	require.Nil(t, missing.Record)
	require.Equal(t, baseMs(), missing.NowMs)
}

func TestInviteListAndPruneCas(t *testing.T) {
	ctx := context.Background()
	s, mr := newTestStore(t, nil, RuleConfig{})
	b := baseMs()
	mustCreate(t, s, 1, 9201)
	mustCommit(t, s, BindCaller(1, 9201), InviteOp(1, 4, 1))
	_, err := mr.ZAdd(inviteIndexKey(4), float64(b-1), "777") // 已过期的残留项
	require.NoError(t, err)

	now, entries, err := s.ListInvites(ctx, 4)
	require.NoError(t, err)
	require.Equal(t, b, now)
	require.Len(t, entries, 1, "过期项被 Redis 时钟剔除")
	require.Equal(t, uint64(9201), entries[0].TeamId)
	require.Equal(t, b+InviteTTLMs, entries[0].ExpireAtMs)

	// LIST 之后、PRUNE 之前队长重邀(score 变化)→ 不删新项。
	setRedisMs(mr, b+1000)
	mustCommit(t, s, BindCaller(1, 9201), InviteOp(1, 4, 1))
	pruned, err := s.PruneInvite(ctx, 4, 9201, entries[0].Score)
	require.NoError(t, err)
	require.False(t, pruned)
	_, err = mr.ZScore(inviteIndexKey(4), "9201")
	require.NoError(t, err)

	_, fresh, err := s.ListInvites(ctx, 4)
	require.NoError(t, err)
	pruned, err = s.PruneInvite(ctx, 4, 9201, fresh[0].Score)
	require.NoError(t, err)
	require.True(t, pruned)
}

// ---- #11:并发不变量模糊测试 ----

type fuzzRecorder struct {
	mu       sync.Mutex
	failures []string
	epochTid map[uint64]map[uint64]uint64 // player → epoch → tid
	maxEpoch map[uint64]uint64
	versions map[[2]uint64]int // (tid, ver) → 提交次数
}

func (r *fuzzRecorder) fail(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.failures) < 20 {
		r.failures = append(r.failures, fmt.Sprintf(format, args...))
	}
}

func (r *fuzzRecorder) observe(pid, epoch, tid uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.epochTid[pid]
	if m == nil {
		m = map[uint64]uint64{}
		r.epochTid[pid] = m
	}
	if prev, ok := m[epoch]; ok && prev != tid {
		if len(r.failures) < 20 {
			r.failures = append(r.failures, fmt.Sprintf("player %d epoch %d 同时配对 tid %d 与 %d", pid, epoch, prev, tid))
		}
	}
	m[epoch] = tid
	if epoch > r.maxEpoch[pid] {
		r.maxEpoch[pid] = epoch
	}
}

func (r *fuzzRecorder) observeCommit(c *CommitResult) {
	for pid, idx := range c.Indexes {
		r.observe(pid, idx.Epoch, idx.TeamId)
	}
	r.mu.Lock()
	r.versions[[2]uint64{c.TeamId, c.Version}]++
	r.mu.Unlock()
}

func (r *fuzzRecorder) observeMutate(res *MutateResult, err error) {
	if err != nil {
		r.fail("Mutate 返回错误: %v", err)
		return
	}
	snap := res.Snapshot
	r.observe(snap.PlayerId, snap.PlayerEpoch, snap.PlayerTeamId)
	for i := range res.Repairs {
		r.observeCommit(&res.Repairs[i])
	}
	if res.Commit != nil {
		r.observeCommit(res.Commit)
	}
}

func TestConcurrentInvariantsFuzz(t *testing.T) {
	const players, workers = 20, 20
	iterations := 2000
	if testing.Short() {
		iterations = 100
	}
	sessions := Sessions{}
	for pid := uint64(1); pid <= players; pid++ {
		sessions[pid] = SessionOnline
	}
	sessions[1], sessions[2] = SessionAbsent, SessionAbsent // 覆盖惰性转让
	s, mr := newTestStore(t, sessions, RuleConfig{})
	rec := &fuzzRecorder{
		epochTid: map[uint64]map[uint64]uint64{},
		maxEpoch: map[uint64]uint64{},
		versions: map[[2]uint64]int{},
	}
	var nextTid atomic.Uint64
	nextTid.Store(100_000)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			ctx := context.Background()
			rng := rand.New(rand.NewSource(seed))
			randPid := func() uint64 { return uint64(rng.Intn(players) + 1) }
			for i := 0; i < iterations; i++ {
				pid := randPid()
				snap, _, err := s.ReadFree(ctx, pid)
				if err == ErrUnstableRead {
					continue
				}
				if err != nil {
					rec.fail("ReadFree: %v", err)
					return
				}
				rec.observe(pid, snap.PlayerEpoch, snap.PlayerTeamId)
				tid := snap.PlayerTeamId
				randMember := func() uint64 {
					ids := MemberIds(snap.Record)
					if len(ids) == 0 {
						return randPid()
					}
					return ids[rng.Intn(len(ids))]
				}
				switch rng.Intn(10) {
				case 0:
					id := nextTid.Add(1)
					rec.observeMutate(s.Mutate(ctx, BindCreate(pid, id), CreateOp(pid, id, 1)))
				case 1:
					target, _, err := s.ReadFree(ctx, randPid())
					if err == nil && target.PlayerTeamId != 0 {
						rec.observeMutate(s.Mutate(ctx, BindTarget(pid, target.PlayerTeamId), ApplyOp(pid, 1)))
					}
				case 2:
					if apps := snap.Record.GetApplications(); len(apps) > 0 {
						applicant := apps[rng.Intn(len(apps))].GetPlayerId()
						rec.observeMutate(s.Mutate(ctx, BindCaller(pid, tid), HandleApplicationOp(pid, applicant, rng.Intn(3) != 0)))
					}
				case 3:
					rec.observeMutate(s.Mutate(ctx, BindCaller(pid, tid), InviteOp(pid, randPid(), 1)))
				case 4:
					_, entries, err := s.ListInvites(ctx, pid)
					if err != nil {
						rec.fail("ListInvites: %v", err)
					} else if len(entries) > 0 {
						e := entries[rng.Intn(len(entries))]
						rec.observeMutate(s.Mutate(ctx, BindTarget(pid, e.TeamId), RespondInviteOp(pid, rng.Intn(3) != 0)))
					}
				case 5:
					rec.observeMutate(s.Mutate(ctx, BindCaller(pid, tid), LeaveOp(pid)))
				case 6:
					rec.observeMutate(s.Mutate(ctx, BindCaller(pid, tid), KickOp(pid, randMember())))
				case 7:
					rec.observeMutate(s.Mutate(ctx, BindCaller(pid, tid), TransferLeaderOp(pid, randMember())))
				case 8:
					rec.observeMutate(s.Mutate(ctx, BindCaller(pid, tid), DisbandOp(pid)))
				default:
					if tid != 0 {
						rec.observeMutate(s.Mutate(ctx, BindCaller(pid, tid), RefreshOp(pid)))
						if ms, err := s.ReadMembers(ctx, tid, MemberIds(snap.Record)); err == nil {
							for m, idx := range ms.Indexes {
								rec.observe(m, idx.Epoch, idx.TeamId)
							}
						} else if err != ErrMembersChanged {
							rec.fail("ReadMembers: %v", err)
						}
					}
				}
			}
		}(int64(w + 1))
	}
	wg.Wait()
	require.Empty(t, rec.failures)

	for key, n := range rec.versions {
		require.Equal(t, 1, n, "tid=%d ver=%d 被提交了 %d 次(ver CAS 失效)", key[0], key[1], n)
	}

	// 终态不变量。
	now := baseMs()
	teamOf := map[uint64]uint64{}
	records := map[uint64]*teampb.TeamRecord{}
	for _, key := range mr.Keys() {
		if !strings.HasPrefix(key, "team:rec:") {
			continue
		}
		tid, err := strconv.ParseUint(strings.TrimPrefix(key, "team:rec:"), 10, 64)
		require.NoError(t, err)
		r := loadRecord(t, mr, tid)
		records[tid] = r
		require.LessOrEqual(t, len(r.GetMembers()), Capacity)
		require.NotNil(t, FindMember(r, r.GetLeaderId()), "队长必须是成员 tid=%d", tid)
		for _, m := range MemberIds(r) {
			prev, dup := teamOf[m]
			require.False(t, dup, "player %d 同时在 %d 与 %d", m, prev, tid)
			teamOf[m] = tid
			require.Equal(t, u64s(tid), mr.HGet(playerIndexKey(m), indexFieldTid), "成员索引必须指向记录")
		}
		for _, inv := range r.GetInvites() {
			if inv.GetExpireAtMs() > now {
				_, err := mr.ZScore(inviteIndexKey(inv.GetInviteeId()), u64s(tid))
				require.NoError(t, err, "邀请反查索引 ⊇ 记录中未过期邀请 tid=%d invitee=%d", tid, inv.GetInviteeId())
			}
		}
		info := &component.TeamInfo{}
		raw, err := mr.Get(projectionKey(tid))
		require.NoError(t, err)
		require.NoError(t, proto.Unmarshal([]byte(raw), info))
		require.Equal(t, r.GetLeaderId(), info.GetLeaderId(), "投影与记录同步")
		require.ElementsMatch(t, MemberIds(r), info.GetMembers())
	}
	for pid := uint64(1); pid <= players; pid++ {
		tidStr := mr.HGet(playerIndexKey(pid), indexFieldTid)
		if tidStr != "" && tidStr != "0" {
			tid, _ := strconv.ParseUint(tidStr, 10, 64)
			require.Equal(t, tid, teamOf[pid], "索引指向的队伍必须包含该玩家 player=%d", pid)
		}
		if !mr.Exists(playerIndexKey(pid)) {
			// 从未组过队:只观察到读路径对缺失索引回报的 Redis nowMs(测试时钟固定),不得超过它。
			require.LessOrEqual(t, rec.maxEpoch[pid], now, "缺失索引回报的 epoch player=%d", pid)
			continue
		}
		epoch, _ := strconv.ParseUint(mr.HGet(playerIndexKey(pid), indexFieldEpoch), 10, 64)
		require.GreaterOrEqual(t, epoch, rec.maxEpoch[pid], "epoch 单调 player=%d", pid)
	}
}
