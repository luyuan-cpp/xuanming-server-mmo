package logic

// 名字填充点的单测(设计 docs/design/guild-phase2/03-names.md §3.21;申请视图两条来自
// 90-consistency.md Y-07)。
//
// 夹具沿用 client_zone_test.go 的 newCacheOnlyRepo / seedGuild:帮会预先放进 miniredis 缓存,
// 读路径不碰 MySQL(db 为 nil,误触即 panic)。resolver 用 WithPlayerNames 注入(Y-02)。
//
// 每个填充点都钉两件事:名字填对了;**一次请求只发一次批量查询**。后者是性能契约 ——
// 在循环里逐个查,功能测试照样全绿,只有这里的 calls 断言能抓住。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"guild/internal/constants"
	"guild/internal/data"
	pb "proto/guild"
)

// recordingPlayerNames 是 PlayerNameResolver 的记录型替身:按固定表作答,并记下每一批收到的 id。
// 不加锁:所有填充点都在请求协程里同步调用它。
type recordingPlayerNames struct {
	names   map[uint64]string
	batches [][]uint64
}

func (f *recordingPlayerNames) BatchResolve(_ context.Context, playerIDs []uint64) map[uint64]string {
	f.batches = append(f.batches, append([]uint64(nil), playerIDs...))
	return f.names
}

const (
	namesTestLeaderA  = uint64(1001) // 有名字
	namesTestMemberB  = uint64(1002) // 早于名字功能建的角色:没有名字
	namesTestLeaderC  = uint64(1003) // 另一个帮的帮主,有名字
	namesTestLeaderD  = uint64(1004) // 别区帮会的帮主
	namesTestGuildOne = uint64(501)
	namesTestGuildTwo = uint64(502)
	namesTestGuildFar = uint64(701) // 在 zone 7
)

func namesTestGuild(guildID, leaderID uint64, zoneID uint32, name string, extraMembers ...uint64) data.GuildData {
	guild := data.GuildData{
		GuildID:    guildID,
		Name:       name,
		LeaderID:   leaderID,
		ZoneID:     zoneID,
		MaxMembers: 30,
		Members:    []data.MemberData{{PlayerID: leaderID, Role: constants.RoleLeader}},
	}
	for _, id := range extraMembers {
		guild.Members = append(guild.Members, data.MemberData{PlayerID: id, Role: constants.RoleMember})
	}
	return guild
}

func memberNameOf(t *testing.T, info *pb.GuildInfo, playerID uint64) string {
	t.Helper()
	for _, m := range info.GetMembers() {
		if m.GetPlayerId() == playerID {
			return m.GetName()
		}
	}
	require.Failf(t, "member missing", "player %d is not in the guild snapshot", playerID)
	return ""
}

func TestGetGuild_FillsMemberAndLeaderNames(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, namesTestGuild(namesTestGuildOne, namesTestLeaderA, 2, "青云门", namesTestMemberB), 10)
	names := &recordingPlayerNames{names: map[uint64]string{namesTestLeaderA: "云中君"}}
	l := NewGuildLogic(repo, nil, nil, nil, nil, WithPlayerNames(names))

	// 内部调用(无会话):不查归属 zone,也不算待审数,读路径只剩缓存 + 取名。
	resp, err := l.GetGuild(context.Background(), &pb.GetGuildRequest{GuildId: namesTestGuildOne})

	require.NoError(t, err)
	guild := resp.GetGuild()
	require.NotNil(t, guild)
	assert.Equal(t, "云中君", guild.GetLeaderName())
	assert.Equal(t, "云中君", memberNameOf(t, guild, namesTestLeaderA))
	assert.Empty(t, memberNameOf(t, guild, namesTestMemberB), "没登记名字的成员留空,由客户端回落成编号")

	// 成员 id 与帮主 id 合并成**一次**批量查询。
	require.Len(t, names.batches, 1)
	assert.Subset(t, names.batches[0], []uint64{namesTestLeaderA, namesTestMemberB})
}

// TestGetGuild_LeaderMissingFromMembersStillNamed:leader_id 与成员表矛盾的坏数据下,
// 帮主名仍然取得到 —— 这正是 toProtoGuild 把 leader_id 单独追加进批量查询的理由。
func TestGetGuild_LeaderMissingFromMembersStillNamed(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	guild := namesTestGuild(namesTestGuildOne, namesTestLeaderA, 2, "青云门")
	guild.Members = []data.MemberData{{PlayerID: namesTestMemberB, Role: constants.RoleMember}}
	seedGuild(t, mr, guild, 10)
	names := &recordingPlayerNames{names: map[uint64]string{namesTestLeaderA: "云中君"}}
	l := NewGuildLogic(repo, nil, nil, nil, nil, WithPlayerNames(names))

	resp, err := l.GetGuild(context.Background(), &pb.GetGuildRequest{GuildId: namesTestGuildOne})

	require.NoError(t, err)
	assert.Equal(t, "云中君", resp.GetGuild().GetLeaderName())
	require.Len(t, names.batches, 1)
}

func TestGetGuildRank_ResolvesLeaderNamesInOneBatch(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, namesTestGuild(namesTestGuildOne, namesTestLeaderA, 2, "青云门", namesTestMemberB), 20)
	seedGuild(t, mr, namesTestGuild(namesTestGuildTwo, namesTestLeaderC, 2, "凌霄阁"), 30)
	names := &recordingPlayerNames{names: map[uint64]string{
		namesTestLeaderA: "云中君",
		namesTestLeaderC: "青莲客",
	}}
	l := NewGuildLogic(repo, nil, nil, nil, nil, WithPlayerNames(names))

	resp, err := l.GetGuildRank(context.Background(), &pb.GetGuildRankRequest{ZoneId: 2})

	require.NoError(t, err)
	require.Len(t, resp.GetEntries(), 2)
	leaderNames := make(map[uint64]string, 2)
	for _, entry := range resp.GetEntries() {
		leaderNames[entry.GetGuildId()] = entry.GetLeaderName()
	}
	assert.Equal(t, map[uint64]string{namesTestGuildOne: "云中君", namesTestGuildTwo: "青莲客"}, leaderNames)

	// 两个帮会整页只查一次,且只查帮主(榜单不展示成员)。
	require.Len(t, names.batches, 1)
	assert.ElementsMatch(t, []uint64{namesTestLeaderA, namesTestLeaderC}, names.batches[0])
}

func TestGetGuildRankByGuild_FillsLeaderName(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, namesTestGuild(namesTestGuildOne, namesTestLeaderA, 2, "青云门", namesTestMemberB), 20)
	names := &recordingPlayerNames{names: map[uint64]string{namesTestLeaderA: "云中君"}}
	l := NewGuildLogic(repo, nil, nil, nil, nil, WithPlayerNames(names))

	resp, err := l.GetGuildRankByGuild(context.Background(), &pb.GetGuildRankByGuildRequest{GuildId: namesTestGuildOne, ZoneId: 2})

	require.NoError(t, err)
	assert.Equal(t, "云中君", resp.GetEntry().GetLeaderName())
	require.Len(t, names.batches, 1)
	assert.Equal(t, []uint64{namesTestLeaderA}, names.batches[0])
}

// TestApplicantViews_FillsNamesInOneBatch:审批人视角的待审名单(Y-07,GuildApplicantView.name)。
// repo / onlineResolver 都传 nil:这一段装配不碰库,在线状态的 resolver 自己 nil 安全。
func TestApplicantViews_FillsNamesInOneBatch(t *testing.T) {
	names := &recordingPlayerNames{names: map[uint64]string{namesTestLeaderA: "云中君"}}
	l := NewGuildLogic(nil, nil, nil, nil, nil, WithPlayerNames(names))
	rows := []data.ApplicantRow{
		{PlayerID: namesTestLeaderA, ApplyMs: 100, ExpireMs: 200},
		{PlayerID: namesTestMemberB, ApplyMs: 300, ExpireMs: 400},
	}

	views := l.applicantViews(context.Background(), rows)

	require.Len(t, views, 2)
	assert.Equal(t, namesTestLeaderA, views[0].GetPlayerId())
	assert.Equal(t, "云中君", views[0].GetName())
	assert.Equal(t, uint64(100), views[0].GetApplyMs())
	assert.Equal(t, uint64(200), views[0].GetExpireMs())
	assert.Equal(t, namesTestMemberB, views[1].GetPlayerId())
	assert.Empty(t, views[1].GetName())
	require.Len(t, names.batches, 1)
	assert.Equal(t, []uint64{namesTestLeaderA, namesTestMemberB}, names.batches[0])
}

// TestMyApplicationViews_FillsLeaderNamesInOneBatch:申请人视角的列表(Y-07,GuildApplicationView.leader_name)。
func TestMyApplicationViews_FillsLeaderNamesInOneBatch(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, namesTestGuild(namesTestGuildOne, namesTestLeaderA, 2, "青云门"), 10)
	seedGuild(t, mr, namesTestGuild(namesTestGuildTwo, namesTestLeaderC, 2, "凌霄阁"), 20)
	// 回滚合服后留下的跨区残留行:不展示,也不该为它的帮主付一次取名。
	seedGuild(t, mr, namesTestGuild(namesTestGuildFar, namesTestLeaderD, 7, "七区帮"), 30)
	names := &recordingPlayerNames{names: map[uint64]string{
		namesTestLeaderA: "云中君",
		namesTestLeaderD: "不该出现",
	}}
	l := NewGuildLogic(repo, nil, nil, nil, nil, WithPlayerNames(names))
	rows := []data.ApplicationRow{
		{GuildID: namesTestGuildOne, ApplyMs: 100, ExpireMs: 200},
		{GuildID: namesTestGuildFar, ApplyMs: 300, ExpireMs: 400},
		{GuildID: namesTestGuildTwo, ApplyMs: 500, ExpireMs: 600},
	}

	views, err := l.myApplicationViews(context.Background(), rows, 2)

	require.NoError(t, err)
	require.Len(t, views, 2)
	assert.Equal(t, namesTestGuildOne, views[0].GetGuildId())
	assert.Equal(t, "青云门", views[0].GetGuildName())
	assert.Equal(t, namesTestLeaderA, views[0].GetLeaderId())
	assert.Equal(t, "云中君", views[0].GetLeaderName())
	assert.Equal(t, uint64(100), views[0].GetApplyMs())
	assert.Equal(t, namesTestGuildTwo, views[1].GetGuildId())
	assert.Empty(t, views[1].GetLeaderName(), "帮主没登记名字时留空")
	assert.Equal(t, uint64(600), views[1].GetExpireMs())

	require.Len(t, names.batches, 1)
	assert.Equal(t, []uint64{namesTestLeaderA, namesTestLeaderC}, names.batches[0])
}

// TestNamesStayEmptyWithoutResolver:不注入 resolver(没配 DataServiceRpc)时名字全空且不 panic。
// WithPlayerNames(nil) 与"根本不传"必须等价 —— guild.go 在没配 DataServiceRpc 时传的就是 nil 接口。
func TestNamesStayEmptyWithoutResolver(t *testing.T) {
	for name, opts := range map[string][]Option{
		"不传 Option":            nil,
		"WithPlayerNames(nil)": {WithPlayerNames(nil)},
	} {
		t.Run(name, func(t *testing.T) {
			repo, mr := newCacheOnlyRepo(t)
			seedGuild(t, mr, namesTestGuild(namesTestGuildOne, namesTestLeaderA, 2, "青云门", namesTestMemberB), 10)
			l := NewGuildLogic(repo, nil, nil, nil, nil, opts...)
			ctx := context.Background()

			require.NotPanics(t, func() {
				got, err := l.GetGuild(ctx, &pb.GetGuildRequest{GuildId: namesTestGuildOne})
				require.NoError(t, err)
				assert.Empty(t, got.GetGuild().GetLeaderName())
				require.Len(t, got.GetGuild().GetMembers(), 2)
				for _, m := range got.GetGuild().GetMembers() {
					assert.Empty(t, m.GetName())
				}

				rank, err := l.GetGuildRank(ctx, &pb.GetGuildRankRequest{ZoneId: 2})
				require.NoError(t, err)
				require.Len(t, rank.GetEntries(), 1)
				assert.Empty(t, rank.GetEntries()[0].GetLeaderName())

				byGuild, err := l.GetGuildRankByGuild(ctx, &pb.GetGuildRankByGuildRequest{GuildId: namesTestGuildOne, ZoneId: 2})
				require.NoError(t, err)
				assert.Empty(t, byGuild.GetEntry().GetLeaderName())

				applicants := l.applicantViews(ctx, []data.ApplicantRow{{PlayerID: namesTestMemberB}})
				require.Len(t, applicants, 1)
				assert.Empty(t, applicants[0].GetName())

				mine, err := l.myApplicationViews(ctx, []data.ApplicationRow{{GuildID: namesTestGuildOne}}, 2)
				require.NoError(t, err)
				require.Len(t, mine, 1)
				assert.Empty(t, mine[0].GetLeaderName())
			})
		})
	}
}

// TestGetGuild_NameLookupFailureDoesNotFailTheRead:名字是展示数据(fail-open)。
// 走**真实**的 DataServicePlayerNames + 报错 / 慢响应的 data_service 替身,钉住整条链:
// data_service 挂了,GetGuild 照常返回成员列表,只是名字留空。
func TestGetGuild_NameLookupFailureDoesNotFailTheRead(t *testing.T) {
	cases := map[string]*fakePlayerNameService{
		"data_service 报错":  {err: status.Error(codes.Unavailable, "dial tcp: connection refused")},
		"data_service 慢响应": {blockUntilCtxDone: true},
	}
	for name, client := range cases {
		t.Run(name, func(t *testing.T) {
			repo, mr := newCacheOnlyRepo(t)
			seedGuild(t, mr, namesTestGuild(namesTestGuildOne, namesTestLeaderA, 2, "青云门", namesTestMemberB), 10)
			// 短超时只为让慢响应用例尽快结束;时序由 resolver 自己的超时驱动,用例里没有 sleep。
			l := NewGuildLogic(repo, nil, nil, nil, nil,
				WithPlayerNames(NewDataServicePlayerNames(client, 20*time.Millisecond)))

			resp, err := l.GetGuild(context.Background(), &pb.GetGuildRequest{GuildId: namesTestGuildOne})

			require.NoError(t, err)
			require.Zero(t, resp.GetErrorMessage().GetId(), "取名失败不得变成业务拒绝")
			guild := resp.GetGuild()
			require.NotNil(t, guild)
			assert.Equal(t, "青云门", guild.GetName())
			assert.Len(t, guild.GetMembers(), 2)
			assert.Empty(t, guild.GetLeaderName())
			calls, _, _ := client.snapshot()
			assert.Equal(t, 1, calls)
		})
	}
}
