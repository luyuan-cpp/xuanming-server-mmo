package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"guild/internal/constants"
	"guild/internal/data"
	"guild/internal/session"
	base "proto/common/base"
	pb "proto/guild"
	"shared/placement"
)

// 客户端来源请求的身份与 zone 隔离。需要 MySQL 的入会 zone 校验在
// data/guild_repo_zone_test.go(ApplyToGuild / ReviewApplication,GUILD_TEST_MYSQL_DSN 门控)。

type fakeHomeZones struct {
	zone       uint32
	err        error
	calls      int
	lastPlayer uint64
}

func (f *fakeHomeZones) HomeZone(_ context.Context, playerID uint64) (uint32, error) {
	f.calls++
	f.lastPlayer = playerID
	return f.zone, f.err
}

func clientCtx(playerID uint64) context.Context {
	return session.WithDetails(context.Background(), &base.SessionDetails{SessionId: 1, PlayerId: playerID})
}

// 归属查不到让流程停在铸号之前:repo 与发号器都传 nil,误触即 panic。请求体里的 ZoneId=7 如果被采用,这里就不会被拒。
func TestCreateGuild_ClientIdentityAndZoneComeFromServer(t *testing.T) {
	homeZones := &fakeHomeZones{zone: 0}
	l := NewGuildLogic(nil, nil, nil, homeZones)

	resp, err := l.CreateGuild(clientCtx(42), &pb.CreateGuildRequest{PlayerId: 999, Name: "青云门", ZoneId: 7})

	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnknown, resp.GetErrorMessage().GetId())
	assert.Equal(t, uint64(42), homeZones.lastPlayer, "身份必须取会话,不能取请求体里伪造的 999")
}

// 内部调用不查归属 zone。repo 为 nil:越过这一步走到仓储就 panic,这里只钉「不查」。
func TestInternalCreateGuildSkipsHomeZoneLookup(t *testing.T) {
	homeZones := &fakeHomeZones{zone: 2}
	l := NewGuildLogic(nil, nil, nil, homeZones)

	func() {
		defer func() { _ = recover() }()
		_, _ = l.CreateGuild(context.Background(), &pb.CreateGuildRequest{PlayerId: 1001, Name: "内部建帮", ZoneId: 7})
	}()

	assert.Zero(t, homeZones.calls, "内部调用不查归属 zone")
}

func TestClientRequestWithoutHomeZoneMappingIsRefused(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, &fakeHomeZones{zone: 0})
	ctx := clientCtx(42)

	create, err := l.CreateGuild(ctx, &pb.CreateGuildRequest{Name: "青云门"})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnknown, create.GetErrorMessage().GetId())

	// 入帮改申请制:原先那个直接入帮的 RPC 已删。提交申请同样要先知道玩家属于哪个区 ——
	// 帮会按 zone 隔离,归属未知就没法判断他能不能申请这个帮。
	apply, err := l.ApplyJoinGuild(ctx, &pb.ApplyJoinGuildRequest{GuildId: 1})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnknown, apply.GetErrorMessage().GetId())

	get, err := l.GetGuild(ctx, &pb.GetGuildRequest{GuildId: 1})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnknown, get.GetErrorMessage().GetId())

	rank, err := l.GetGuildRank(ctx, &pb.GetGuildRankRequest{})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnknown, rank.GetErrorMessage().GetId())

	byGuild, err := l.GetGuildRankByGuild(ctx, &pb.GetGuildRankByGuildRequest{GuildId: 1})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrHomeZoneUnknown, byGuild.GetErrorMessage().GetId())
}

func TestClientRequestHomeZoneLookupFailures(t *testing.T) {
	t.Run("data_service error is a fault, not a tip", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, &fakeHomeZones{err: errors.New("mapping redis down")})

		resp, err := l.GetGuildRank(clientCtx(42), &pb.GetGuildRankRequest{})

		require.Error(t, err)
		assert.Nil(t, resp)
	})
	t.Run("lookup not wired", func(t *testing.T) {
		l := NewGuildLogic(nil, nil, nil, nil)

		_, err := l.CreateGuild(clientCtx(42), &pb.CreateGuildRequest{Name: "青云门"})

		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
}

func TestCreateGuild_RejectsInvalidNames(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"blank":        "   ",
		"too long":     strings.Repeat("帮", constants.MaxGuildNameRunes+1),
		"control char": "青云\n门",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			homeZones := &fakeHomeZones{zone: 2}
			l := NewGuildLogic(nil, nil, nil, homeZones)

			resp, err := l.CreateGuild(clientCtx(42), &pb.CreateGuildRequest{Name: raw})

			require.NoError(t, err)
			assert.Equal(t, constants.ErrGuildNameInvalid, resp.GetErrorMessage().GetId())
			assert.Zero(t, homeZones.calls, "非法帮名在任何外部查询之前拒绝")
		})
	}
}

func TestNormalizeGuildName(t *testing.T) {
	name, ok := normalizeGuildName("  青云门 ")
	assert.True(t, ok)
	assert.Equal(t, "青云门", name)

	_, ok = normalizeGuildName(strings.Repeat("帮", constants.MaxGuildNameRunes))
	assert.True(t, ok, "恰好上限个汉字必须允许:按字符数计,不按字节")
}

func TestSetAnnouncement_RejectsOversizeBeforeTouchingStorage(t *testing.T) {
	l := NewGuildLogic(nil, nil, nil, nil)
	oversize := strings.Repeat("汉", constants.MaxAnnouncementBytes/3+1) // 每个汉字 3 字节

	resp, err := l.SetAnnouncement(clientCtx(42), &pb.SetAnnouncementRequest{GuildId: 1, Announcement: oversize})

	require.NoError(t, err)
	assert.Equal(t, constants.ErrAnnouncementTooLong, resp.GetErrorMessage().GetId())
}

// newCacheOnlyRepo 返回只接 Redis 的仓储:榜单读 ZSET、公会读 guild:v2:{id} 缓存。
// 用例把数据预先放进缓存,读路径不碰 MySQL(db 为 nil,误触即 panic)。
func newCacheOnlyRepo(t *testing.T) (*data.GuildRepo, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return data.NewGuildRepo(rdb, nil, time.Minute), mr
}

// seedGuild 按 data 包的键契约(guild_repo.go 的 guildKey / guildRankKey / zoneRankKey)写缓存与两张榜。
func seedGuild(t *testing.T, mr *miniredis.Miniredis, guild data.GuildData, score float64) {
	t.Helper()
	payload, err := json.Marshal(guild)
	require.NoError(t, err)
	require.NoError(t, mr.Set(fmt.Sprintf("guild:v2:%d", guild.GuildID), string(payload)))
	member := strconv.FormatUint(guild.GuildID, 10)
	_, err = mr.ZAdd("guild_rank", score, member)
	require.NoError(t, err)
	_, err = mr.ZAdd(fmt.Sprintf("guild_rank:zone:%d", guild.ZoneID), score, member)
	require.NoError(t, err)
}

func TestGetGuildRank_ClientSeesOnlyHomeZone(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, data.GuildData{GuildID: 501, Name: "二区帮", ZoneID: 2}, 20)
	seedGuild(t, mr, data.GuildData{GuildID: 701, Name: "七区帮", ZoneID: 7}, 30)
	l := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: 2})

	// 客户端要全服榜(zone 0)、一页 500 条:zone 被归属 zone 覆盖,页长被夹到上限。
	resp, err := l.GetGuildRank(clientCtx(42), &pb.GetGuildRankRequest{ZoneId: 0, PageSize: 500})
	require.NoError(t, err)
	require.Len(t, resp.GetEntries(), 1)
	assert.Equal(t, uint64(501), resp.GetEntries()[0].GetGuildId())
	assert.Equal(t, "二区帮", resp.GetEntries()[0].GetName())
	assert.Equal(t, uint32(1), resp.GetTotalCount())
	assert.Equal(t, constants.MaxRankPageSize, resp.GetPageSize())

	// 内部调用保留全服榜与原页长。
	internal, err := l.GetGuildRank(context.Background(), &pb.GetGuildRankRequest{ZoneId: 0, PageSize: 500})
	require.NoError(t, err)
	assert.Len(t, internal.GetEntries(), 2)
	assert.Equal(t, uint32(500), internal.GetPageSize())
}

func TestGetGuild_OtherZoneLooksNonexistentToClient(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, data.GuildData{GuildID: 701, Name: "七区帮", ZoneID: 7}, 30)

	other, err := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: 2}).
		GetGuild(clientCtx(42), &pb.GetGuildRequest{GuildId: 701})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrGuildNotFound, other.GetErrorMessage().GetId())
	assert.Nil(t, other.GetGuild())

	same, err := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: 7}).
		GetGuild(clientCtx(42), &pb.GetGuildRequest{GuildId: 701})
	require.NoError(t, err)
	assert.Equal(t, uint64(701), same.GetGuild().GetGuildId())
}

// bindMergedZones 给 repo 接一份固定的合服映射。用真缓存,不写替身:不 Start 就不起 goroutine。
func bindMergedZones(t *testing.T, repo *data.GuildRepo, merges map[uint32]uint32) {
	t.Helper()
	cache, err := placement.NewMergedZoneCache(context.Background(), func(context.Context) (map[uint32]uint32, error) {
		return merges, nil
	}, placement.MergedZoneCacheOptions{})
	require.NoError(t, err)
	repo.BindMergedZones(cache)
}

// 合服不改写公会行:zone_id 还是 7,7 区合进 2 区之后,2 区的玩家看得见它,客户端拿到的是最终区。
func TestGetGuild_MergedZoneIsVisibleAndShowsTheFinalZone(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, data.GuildData{GuildID: 701, Name: "七区帮", ZoneID: 7}, 30)
	bindMergedZones(t, repo, map[uint32]uint32{7: 2})

	for _, home := range []uint32{2, 7} { // 归属读可能滞后一拍,还回答 7:两种答案都要看得见
		resp, err := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: home}).
			GetGuild(clientCtx(42), &pb.GetGuildRequest{GuildId: 701})
		require.NoError(t, err)
		require.NotNil(t, resp.GetGuild(), "home=%d", home)
		assert.Equal(t, uint32(2), resp.GetGuild().GetZoneId(), "home=%d", home)
	}

	other, err := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: 3}).
		GetGuild(clientCtx(42), &pb.GetGuildRequest{GuildId: 701})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrGuildNotFound, other.GetErrorMessage().GetId(), "没合服的区照旧看不见")
}

// 客户端读本区榜:归属读滞后、还回答源区时,读到的也是最终区那张榜。
func TestGetGuildRank_ClientReadsTheFinalZoneRank(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, data.GuildData{GuildID: 501, Name: "二区帮", ZoneID: 2}, 20)
	bindMergedZones(t, repo, map[uint32]uint32{7: 2})

	for _, home := range []uint32{2, 7} {
		l := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: home})
		page, err := l.GetGuildRank(clientCtx(42), &pb.GetGuildRankRequest{})
		require.NoError(t, err)
		require.Len(t, page.GetEntries(), 1, "home=%d", home)
		assert.Equal(t, uint64(501), page.GetEntries()[0].GetGuildId())

		byGuild, err := l.GetGuildRankByGuild(clientCtx(42), &pb.GetGuildRankByGuildRequest{GuildId: 501})
		require.NoError(t, err)
		assert.Equal(t, uint32(1), byGuild.GetEntry().GetRank(), "home=%d", home)
	}
}

// 申请列表的过滤口径与 GetGuild 相同:合进来的区不算别区。
func TestMyApplicationViews_MergedZoneIsNotAnotherZone(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, data.GuildData{GuildID: 701, Name: "七区帮", ZoneID: 7}, 30)
	seedGuild(t, mr, data.GuildData{GuildID: 801, Name: "八区帮", ZoneID: 8}, 30)
	bindMergedZones(t, repo, map[uint32]uint32{7: 2})
	l := NewGuildLogic(repo, nil, nil, nil)

	views, err := l.myApplicationViews(context.Background(), []data.ApplicationRow{{GuildID: 701}, {GuildID: 801}}, 2)

	require.NoError(t, err)
	require.Len(t, views, 1)
	assert.Equal(t, uint64(701), views[0].GetGuildId())
}

func TestGetGuildRankByGuild_ClientOnlyRanksInHomeZone(t *testing.T) {
	repo, mr := newCacheOnlyRepo(t)
	seedGuild(t, mr, data.GuildData{GuildID: 701, Name: "七区帮", ZoneID: 7}, 30)

	other, err := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: 2}).
		GetGuildRankByGuild(clientCtx(42), &pb.GetGuildRankByGuildRequest{GuildId: 701, ZoneId: 7})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrNotRanked, other.GetErrorMessage().GetId())

	same, err := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: 7}).
		GetGuildRankByGuild(clientCtx(42), &pb.GetGuildRankByGuildRequest{GuildId: 701})
	require.NoError(t, err)
	assert.Equal(t, uint32(1), same.GetEntry().GetRank())
}
