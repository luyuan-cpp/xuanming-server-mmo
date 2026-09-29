package data

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2/server"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	pb "proto/friend"
	plpb "proto/player_locator"
)

type directoryDataService struct {
	dspb.DataServiceClient
	zones map[uint64]uint32
	names map[uint64]string
	err   error
}

func (d directoryDataService) BatchGetPlayerHomeZone(ctx context.Context, in *dspb.BatchGetPlayerHomeZoneRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerHomeZoneResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.err != nil {
		return nil, d.err
	}
	result := &dspb.BatchGetPlayerHomeZoneResponse{PlayerZoneMap: map[uint64]uint32{}}
	for _, id := range in.GetPlayerIds() {
		result.PlayerZoneMap[id] = d.zones[id]
	}
	return result, nil
}

func (d directoryDataService) BatchGetPlayerName(ctx context.Context, in *dspb.BatchGetPlayerNameRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerNameResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.err != nil {
		return nil, d.err
	}
	return &dspb.BatchGetPlayerNameResponse{Names: d.names}, nil
}

func directoryProfile(t *testing.T, id uint64, name string) string {
	t.Helper()
	profile := &dbpb.PlayerAllData{}
	require.NoError(t, protojson.Unmarshal([]byte(fmt.Sprintf(`{"playerDatabaseData":{"playerId":"%d","levelComponent":{"level":12},"uint32PbComponent":{"class":1},"profileComponent":{"name":%q,"appearanceId":"daoshi","gender":1}}}`, id, name)), profile))
	raw, err := proto.Marshal(profile)
	require.NoError(t, err)
	return string(raw)
}

func TestOnlineDirectoryFiltersAndPreservesLastBatch(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 64)
	zones := map[uint64]uint32{1: 1}
	prefix := string((&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName())
	for id := uint64(1); id <= 15; id++ {
		zones[id] = 1
		putPlayerSession(t, mr, id, plpb.PlayerSessionState_SESSION_STATE_ONLINE, 123)
		require.NoError(t, mr.Set(fmt.Sprintf("%s:%d", prefix, id), directoryProfile(t, id, fmt.Sprintf("道人%d", id))))
	}
	// 不同 home zone、断线重连、缺资料、错误身份、自身和排除项都不得出现。
	zones[3] = 2
	putPlayerSession(t, mr, 4, plpb.PlayerSessionState_SESSION_STATE_DISCONNECTING, 123)
	mr.Del(fmt.Sprintf("%s:5", prefix))
	require.NoError(t, mr.Set(fmt.Sprintf("%s:6", prefix), directoryProfile(t, 99, "错误身份")))
	directory := OnlineDirectory{Redis: reader.rdb, DataService: directoryDataService{zones: zones}}
	seen := map[uint64]bool{}
	cursor := ""
	for page := 0; ; page++ {
		require.Less(t, page, 30, "分页必须结束")
		resp, err := directory.List(context.Background(), 1, cursor, 2, []uint64{2}, "")
		require.NoError(t, err)
		require.True(t, resp.GetOnlineDirectory())
		require.LessOrEqual(t, len(resp.GetCandidates()), 2)
		for _, entry := range resp.GetCandidates() {
			require.GreaterOrEqual(t, entry.GetCandidatePlayerId(), uint64(7))
			require.False(t, seen[entry.GetCandidatePlayerId()])
			seen[entry.GetCandidatePlayerId()] = true
			require.Equal(t, uint32(12), entry.GetLevel())
			require.Equal(t, uint32(1), entry.GetZoneId())
			require.True(t, entry.GetIsOnline())
		}
		cursor = resp.GetNextCursor()
		if cursor == "" {
			break
		}
	}
	require.Len(t, seen, 9, "末批游标为零时，页内剩余玩家仍需完整保留")
	resp, err := directory.List(context.Background(), 1, "", 10, nil, "道人12")
	require.NoError(t, err)
	require.Len(t, resp.GetCandidates(), 1)
	require.Equal(t, uint64(12), resp.GetCandidates()[0].GetCandidatePlayerId())
}

func TestOnlineDirectoryInputAndDependencyFailures(t *testing.T) {
	for _, raw := range []string{"bad", "v1:-1:0", "v1:18446744073709551616:0", "v1:1:65535", strings.Repeat("0", 49)} {
		require.ErrorIs(t, ValidateDirectoryInput(raw, ""), ErrInvalidDirectoryInput)
	}
	require.NoError(t, ValidateDirectoryInput("v1:18446744073709551615:0", ""))
	require.ErrorIs(t, ValidateDirectoryInput("", strings.Repeat("字", 65)), ErrInvalidDirectoryInput)
	reader, _ := newSessionReaderOnMiniredis(t, 64)
	directory := OnlineDirectory{Redis: reader.rdb, DataService: directoryDataService{zones: map[uint64]uint32{}}}
	_, err := directory.List(context.Background(), 1, "", 10, nil, "")
	require.Error(t, err, "归属区缺失不能返回跨区玩家")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = directory.List(ctx, 1, "", 10, nil, "")
	require.ErrorIs(t, err, context.Canceled)
	directory.DataService = directoryDataService{err: errors.New("data service unavailable")}
	_, err = directory.List(context.Background(), 1, "", 10, nil, "")
	require.Error(t, err)
}

func TestOnlineDirectoryEmptyPageContinuesWithinScanBudget(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 64)
	// miniredis 的内建 SCAN 忽略 COUNT，一次返回全部匹配键；在公开 RESP 接缝
	// 提供真实 Redis 允许的空批次，验证预算与完整 uint64 游标，而非断言实现循环。
	mr.Server().SetPreHook(func(peer *server.Peer, cmd string, args ...string) bool {
		if cmd != "SCAN" {
			return false
		}
		next := "0"
		switch args[0] {
		case "0":
			next = "18446744073709551615"
		case "18446744073709551615":
			next = "42"
		case "42":
			next = "43"
		case "43":
			next = "44"
		case "44":
		default:
			peer.WriteError("ERR unexpected cursor")
			return true
		}
		peer.WriteLen(2)
		peer.WriteBulk(next)
		if next == "0" {
			peer.WriteLen(1)
			peer.WriteBulk("player:session:1999")
		} else {
			peer.WriteLen(0)
		}
		return true
	})
	putPlayerSession(t, mr, 1999, plpb.PlayerSessionState_SESSION_STATE_ONLINE, 123)
	prefix := string((&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName())
	require.NoError(t, mr.Set(prefix+":1999", directoryProfile(t, 1999, "最后的在线玩家")))
	directory := OnlineDirectory{Redis: reader.rdb, DataService: directoryDataService{zones: map[uint64]uint32{1: 1, 1999: 1}}}
	cursor := ""
	seenEmptyPage := false
	var found []uint64
	for page := 0; ; page++ {
		require.Less(t, page, 30)
		resp, err := directory.List(context.Background(), 1, cursor, 10, nil, "最后")
		require.NoError(t, err)
		if len(resp.GetCandidates()) == 0 && resp.GetNextCursor() != "" {
			seenEmptyPage = true
		}
		for _, entry := range resp.GetCandidates() {
			found = append(found, entry.GetCandidatePlayerId())
		}
		cursor = resp.GetNextCursor()
		if cursor == "" {
			break
		}
	}
	require.True(t, seenEmptyPage, "扫描预算到期返回空页也必须允许继续")
	require.Equal(t, []uint64{1999}, found)
}

func TestFriendProfilesFillCacheAndResolveOfflineNames(t *testing.T) {
	reader, mr := newSessionReaderOnMiniredis(t, 64)
	prefix := string((&dbpb.PlayerAllData{}).ProtoReflect().Descriptor().FullName())
	require.NoError(t, mr.Set(prefix+":2", directoryProfile(t, 2, "青云")))
	friends := []*pb.FriendEntry{{FriendPlayerId: 2, IsOnline: true}, {FriendPlayerId: 3}}
	directory := OnlineDirectory{Redis: reader.rdb, DataService: directoryDataService{zones: map[uint64]uint32{2: 1, 3: 2}, names: map[uint64]string{3: "离线好友"}}}
	require.NoError(t, directory.FillFriendProfiles(context.Background(), friends))
	require.Equal(t, "青云", friends[0].GetName())
	require.Equal(t, uint32(12), friends[0].GetLevel())
	require.Equal(t, "daoshi", friends[0].GetAppearanceId())
	require.Equal(t, "离线好友", friends[1].GetName())
	require.Equal(t, uint32(2), friends[1].GetZoneId())
	require.Zero(t, friends[1].GetLevel())
	require.False(t, friends[1].GetIsOnline())
}
