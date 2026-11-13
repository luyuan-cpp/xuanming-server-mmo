package logic

// 托管归位(hosted_resume.go,docs/design/player-connection-lost-hold.md §4.5)。SceneManager 假实现复用
// scene_manager_client_fake_test.go 的全包唯一一份。辅助标识符加 hostedResume 前缀。

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/svc"
	pb "proto/player_locator"
	smpb "proto/scene_manager"
	"shared/redisslot"
)

const (
	hostedResumeZone       = uint32(3)
	hostedResumeSession    = uint32(12345)
	hostedResumeGeneration = uint64(1700000000123)
	hostedResumeRevision   = uint64(77)
	// hostedResumeGoldenRecord 与 cpp/tests 的 HostedPresence 金样逐字相同(FormatReturnRecord(3, 77, 12345, 1700000000123))。
	hostedResumeGoldenRecord = "v1:3:77:12345:1700000000123"
)

func hostedResumeSessionOf(playerID uint64) *pb.PlayerSession {
	return &pb.PlayerSession{PlayerId: playerID, SessionId: hostedResumeSession, LoginGeneration: hostedResumeGeneration,
		State: pb.PlayerSessionState_SESSION_STATE_DISCONNECTING}
}

// hostedResumeSeed 摆好「托管角色掉出世界」:月卡还有一小时、归位信息是这次登录的、location 在本 zone,没有在场心跳。
func hostedResumeSeed(t *testing.T, mr *miniredis.Miniredis, playerID uint64, now time.Time) {
	t.Helper()
	require.NoError(t, mr.Set(redisslot.AfkPassKey(playerID), strconv.FormatInt(now.Add(time.Hour).Unix(), 10)))
	require.NoError(t, mr.Set(redisslot.HostedReturnKey(playerID), hostedResumeGoldenRecord))
	hostedResumeSetLocation(t, mr, playerID, hostedResumeZone)
}

func hostedResumeSetLocation(t *testing.T, mr *miniredis.Miniredis, playerID uint64, zone uint32) {
	t.Helper()
	raw, err := proto.Marshal(&smpb.PlayerLocation{SceneId: 5, NodeId: "10", ZoneId: zone, OwnerEpoch: 4,
		SessionId: hostedResumeSession, LoginGeneration: hostedResumeGeneration})
	require.NoError(t, err)
	require.NoError(t, mr.Set(redisslot.LocationKey(playerID), string(raw)))
}

func hostedResumeSetup(t *testing.T) (*svc.ServiceContext, *miniredis.Miniredis, *fakeSceneManagerClient) {
	t.Helper()
	sc, mr := newTestSvcCtx(t)
	sc.Config.Node.ZoneId = hostedResumeZone
	fake := &fakeSceneManagerClient{}
	sc.SceneManagerClient = fake
	return sc, mr, fake
}

// 写入方是 C++ 的 hosted_presence::FormatReturnRecord;两侧钉同一条金样。
func TestParseHostedReturnRecord(t *testing.T) {
	got, err := parseHostedReturnRecord(hostedResumeGoldenRecord)
	require.NoError(t, err)
	assert.Equal(t, hostedReturnRecord{zoneID: hostedResumeZone, ownerRevision: hostedResumeRevision,
		sessionID: hostedResumeSession, loginGeneration: hostedResumeGeneration}, got)

	for _, bad := range []string{
		"",
		"v2:3:77:12345:1700000000123",
		"v1:3:77:12345",
		"v1:3:77:12345:1700000000123:9",
		"v1:0:77:12345:1700000000123",
		"v1:3:0:12345:1700000000123",
		"v1:3:77:0:1700000000123",
		"v1:3:77:12345:0",
		"v1:3:077:12345:1700000000123",
		"v1:3:77:4294967296:1700000000123",
		"v1:4294967296:77:12345:1700000000123",
		"v1:3:77:-1:1700000000123",
		"v1:3:77::1700000000123",
	} {
		_, err := parseHostedReturnRecord(bad)
		assert.Error(t, err, "record %q", bad)
	}
}

func TestDecideHostedResume(t *testing.T) {
	record := hostedReturnRecord{zoneID: 7, ownerRevision: hostedResumeRevision, sessionID: hostedResumeSession,
		loginGeneration: hostedResumeGeneration}
	send := hostedResumeFacts{passRemaining: time.Hour, recordFound: true, record: record, sessionID: hostedResumeSession,
		loginGeneration: hostedResumeGeneration, locationZone: hostedResumeZone}
	cases := []struct {
		name     string
		mutate   func(*hostedResumeFacts)
		want     hostedResumeVerdict
		wantZone uint32
	}{
		{"lost: send to the location's zone", func(*hostedResumeFacts) {}, hostedResumeSend, hostedResumeZone},
		{"no location zone: send to the zone the record names", func(f *hostedResumeFacts) { f.locationZone = 0 }, hostedResumeSend, 7},
		{"presence wins over everything", func(f *hostedResumeFacts) { f.presencePresent, f.recordFound = true, false }, hostedResumePresent, 0},
		{"pass about to end", func(f *hostedResumeFacts) { f.passRemaining = hostedResumeMinPassRemaining - time.Second }, hostedResumePassEnding, 0},
		{"pass unreadable", func(f *hostedResumeFacts) { f.passRemaining = 0 }, hostedResumePassEnding, 0},
		{"no record", func(f *hostedResumeFacts) { f.recordFound = false }, hostedResumeNoRecord, 0},
		{"malformed record", func(f *hostedResumeFacts) { f.recordErr = assert.AnError }, hostedResumeRecordMalformed, 0},
		{"record of an older login", func(f *hostedResumeFacts) { f.loginGeneration++ }, hostedResumeOtherLogin, 0},
		{"record of another session", func(f *hostedResumeFacts) { f.sessionID++ }, hostedResumeOtherLogin, 0},
		{"session without generation", func(f *hostedResumeFacts) { f.loginGeneration, f.record.loginGeneration = 0, 0 }, hostedResumeOtherLogin, 0},
		{"no zone anywhere", func(f *hostedResumeFacts) { f.locationZone, f.record.zoneID = 0, 0 }, hostedResumeNoZone, 0},
		{"zero value never sends", func(f *hostedResumeFacts) { *f = hostedResumeFacts{} }, hostedResumePassEnding, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts := send
			tc.mutate(&facts)
			verdict, zone := decideHostedResume(facts)
			assert.Equal(t, tc.want, verdict)
			assert.Equal(t, tc.wantZone, zone)
		})
	}
}

// 掉出世界:替他向 location 所在 zone 的 scene_manager 发一次托管归位,形状是 hosted_placement、不带 gate、不带去向、
// 关联号 0,身份是断开的那次登录,归属修订取归位信息里的。
func TestResumeHostedIfLost_RequestsHostedResume(t *testing.T) {
	sc, mr, fake := hostedResumeSetup(t)
	const playerID = uint64(31001)
	now := time.Now()
	hostedResumeSeed(t, mr, playerID, now)

	resumeHostedIfLost(context.Background(), sc, hostedResumeSessionOf(playerID), now)

	calls := fake.enterCalls()
	require.Len(t, calls, 1)
	assert.True(t, proto.Equal(&smpb.EnterSceneRequest{PlayerId: playerID, SessionId: hostedResumeSession,
		LoginGeneration: hostedResumeGeneration, OwnerRevision: hostedResumeRevision, GateZoneId: hostedResumeZone,
		HostedPlacement: true}, calls[0]), "got %v", calls[0])
	assert.True(t, mr.Exists(redisslot.HostedReturnKey(playerID)), "成功不删归位信息:托管节点会接着续写")
}

// 在场心跳还在 = 有节点对他负责(托管着、改派途中、看护中),什么都不发。
func TestResumeHostedIfLost_PresenceMeansSomeoneIsResponsible(t *testing.T) {
	sc, mr, fake := hostedResumeSetup(t)
	const playerID = uint64(31002)
	now := time.Now()
	hostedResumeSeed(t, mr, playerID, now)
	require.NoError(t, mr.Set(redisslot.HostedPresenceKey(playerID), "scene-10-uuid"))

	resumeHostedIfLost(context.Background(), sc, hostedResumeSessionOf(playerID), now)

	assert.Empty(t, fake.enterCalls())
}

// 归位信息是更早那次登录的(这次登录没开始过托管):不替它归位。
func TestResumeHostedIfLost_RecordOfAnotherLoginIsIgnored(t *testing.T) {
	sc, mr, fake := hostedResumeSetup(t)
	const playerID = uint64(31003)
	now := time.Now()
	hostedResumeSeed(t, mr, playerID, now)
	session := hostedResumeSessionOf(playerID)
	session.LoginGeneration++

	resumeHostedIfLost(context.Background(), sc, session, now)

	assert.Empty(t, fake.enterCalls())
}

// 月卡快到期:落地就会退出,不归位。
func TestResumeHostedIfLost_PassEndingSoonDoesNotResume(t *testing.T) {
	sc, mr, fake := hostedResumeSetup(t)
	const playerID = uint64(31004)
	now := time.Now()
	hostedResumeSeed(t, mr, playerID, now)
	require.NoError(t, mr.Set(redisslot.AfkPassKey(playerID), strconv.FormatInt(now.Add(30*time.Second).Unix(), 10)))

	resumeHostedIfLost(context.Background(), sc, hostedResumeSessionOf(playerID), now)

	assert.Empty(t, fake.enterCalls())
}

// location 已被删(§17 撤销之后),按归位信息里托管节点的 zone 发;那是别的 zone,走按 zone 建的客户端,不发给本 zone。
func TestResumeHostedIfLost_WithoutLocationUsesTheRecordZone(t *testing.T) {
	sc, mr, own := hostedResumeSetup(t)
	const playerID = uint64(31005)
	now := time.Now()
	hostedResumeSeed(t, mr, playerID, now)
	mr.Del(redisslot.LocationKey(playerID))
	sc.Config.Node.ZoneId = 9 // 本副本在 zone 9,角色托管在 zone 3
	remote := &fakeSceneManagerClient{}
	sc.SetZoneSceneManagerForTest(hostedResumeZone, remote)

	resumeHostedIfLost(context.Background(), sc, hostedResumeSessionOf(playerID), now)

	assert.Empty(t, own.enterCalls(), "本 zone 的 scene_manager 放不了别的 zone 的落点")
	calls := remote.enterCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, hostedResumeZone, calls[0].GetGateZoneId())
}

// 归属修订被判失效(角色被交易 / 封存):删掉这份归位信息,不再替他归位。
func TestResumeHostedIfLost_RevisionRefusedDropsRecord(t *testing.T) {
	sc, mr, fake := hostedResumeSetup(t)
	const playerID = uint64(31006)
	now := time.Now()
	hostedResumeSeed(t, mr, playerID, now)
	fake.setEnterReply(nil, status.Error(codes.FailedPrecondition, "原角色归属修订已失效"))

	resumeHostedIfLost(context.Background(), sc, hostedResumeSessionOf(playerID), now)

	require.Len(t, fake.enterCalls(), 1)
	assert.False(t, mr.Exists(redisslot.HostedReturnKey(playerID)))
}

// scene_manager 的进场链结论(18 / 17 / 1 …)与传输失败:都留着归位信息,下一次续租再来。
func TestResumeHostedIfLost_RetryableOutcomesKeepRecord(t *testing.T) {
	for name, reply := range map[string]struct {
		resp *smpb.EnterSceneResponse
		err  error
	}{
		"handoff pending":   {resp: &smpb.EnterSceneResponse{ErrorCode: 18}},
		"transport failure": {err: status.Error(codes.Unavailable, "scene_manager down")},
	} {
		t.Run(name, func(t *testing.T) {
			sc, mr, fake := hostedResumeSetup(t)
			const playerID = uint64(31007)
			now := time.Now()
			hostedResumeSeed(t, mr, playerID, now)
			fake.setEnterReply(reply.resp, reply.err)

			resumeHostedIfLost(context.Background(), sc, hostedResumeSessionOf(playerID), now)

			require.Len(t, fake.enterCalls(), 1)
			assert.True(t, mr.Exists(redisslot.HostedReturnKey(playerID)))
		})
	}
}

// 月卡续费后归位信息按当下的月卡活着:续租时对齐存活(有节点负责时也对齐),别的登录留下的那一份不碰。
func TestResumeHostedIfLost_AlignsTheReturnRecordToTheCurrentPass(t *testing.T) {
	sc, mr, fake := hostedResumeSetup(t)
	const playerID = uint64(31009)
	now := time.Now()
	hostedResumeSeed(t, mr, playerID, now) // 月卡还有一小时
	require.NoError(t, mr.Set(redisslot.HostedPresenceKey(playerID), "scene-10-uuid"))
	mr.SetTTL(redisslot.HostedReturnKey(playerID), 90*time.Second) // 托管节点按续费前的月卡写的

	resumeHostedIfLost(context.Background(), sc, hostedResumeSessionOf(playerID), now)

	assert.Empty(t, fake.enterCalls(), "有节点负责:不归位")
	ttl := mr.TTL(redisslot.HostedReturnKey(playerID))
	assert.Greater(t, ttl, 59*time.Minute, "对齐到月卡剩余")
	assert.LessOrEqual(t, ttl, time.Hour)

	other := hostedResumeSessionOf(playerID)
	other.LoginGeneration++
	mr.SetTTL(redisslot.HostedReturnKey(playerID), 90*time.Second)
	resumeHostedIfLost(context.Background(), sc, other, now)
	assert.Equal(t, 90*time.Second, mr.TTL(redisslot.HostedReturnKey(playerID)), "不是这次登录的那一份:不动")
}

// 端到端:断线租约到期、月卡有效 → 续租,紧接着对账发现没人负责 → 托管归位;不调 LeaveScene,claim 还在(续上了)。
func TestLeaseMonitor_AfkRenewalResumesLostHostedCharacter(t *testing.T) {
	sc, mr, fake := hostedResumeSetup(t)
	const playerID = uint64(31008)
	ctx := context.Background()
	session := makeTestSession(playerID)
	session.GateId, session.GateInstanceId = "", ""
	session.SessionId, session.LoginGeneration = hostedResumeSession, hostedResumeGeneration
	_, err := NewSetSessionLogic(ctx, sc).SetSession(&pb.SetSessionRequest{Session: session})
	require.NoError(t, err)
	_, err = NewSetDisconnectingLogic(ctx, sc).SetDisconnecting(&pb.SetDisconnectingRequest{
		PlayerId: playerID, SessionId: session.SessionId, LeaseTtlSeconds: 1,
	})
	require.NoError(t, err)
	hostedResumeSeed(t, mr, playerID, time.Now())
	m := scanTestMonitor(sc, fixedSlots{redisslot.PlayerSlot(playerID)}, LeaseMonitorParams{BatchSize: 10},
		time.Now().Add(5*time.Second))

	m.tick(ctx)

	assert.Empty(t, fake.leaveCalls(), "月卡有效:不清理")
	assert.True(t, mr.Exists(sessionKey(playerID)))
	calls := fake.enterCalls()
	require.Len(t, calls, 1)
	assert.True(t, calls[0].GetHostedPlacement())
	assert.Equal(t, hostedResumeGeneration, calls[0].GetLoginGeneration())
}
