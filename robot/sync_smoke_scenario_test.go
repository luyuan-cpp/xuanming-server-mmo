package main

// sync-smoke 里纯函数的单测:不连服务器,只喂构造出来的摘要与留底。
// 冒烟本身要等整套服务起来才跑得到,而它的判定函数若写反了(该红不红),联机时是看不出来的 ——
// 所以每个判定函数都要有「合法形状放行 / 每种坏形状各自被拦」两面。

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"proto/common/base"
	"proto/common/component"
	friendpb "proto/friend"
	"proto/scene"
	"robot/generated/pb/game"
	"robot/pkg"

	tiptable "shared/generated/pb/table"
)

const (
	syncTestFriendList    = scene.SyncDomain_SYNC_DOMAIN_FRIEND_LIST
	syncTestFriendRequest = scene.SyncDomain_SYNC_DOMAIN_FRIEND_REQUEST
	syncTestFriendBlock   = scene.SyncDomain_SYNC_DOMAIN_FRIEND_BLOCK
	syncTestTeam          = scene.SyncDomain_SYNC_DOMAIN_TEAM
)

// syncTestDigest 构造一份 status=OK 的摘要;versions 为 nil 时等于"没有任何域"。
func syncTestDigest(generation uint64, versions map[scene.SyncDomain]uint64) syncSmokeDigest {
	if versions == nil {
		versions = map[scene.SyncDomain]uint64{}
	}
	return syncSmokeDigest{
		status:     scene.SyncDigestStatus_SYNC_DIGEST_STATUS_OK,
		generation: generation,
		versions:   versions,
	}
}

func syncTestUnavailable() syncSmokeDigest {
	return syncSmokeDigest{
		status:   scene.SyncDigestStatus_SYNC_DIGEST_STATUS_UNAVAILABLE,
		versions: map[scene.SyncDomain]uint64{},
	}
}

func syncTestRecord(t *testing.T, messageId uint32, envelopeTip uint32, body proto.Message) travelSmokeRecord {
	t.Helper()
	record := travelSmokeRecord{messageId: messageId, envelopeTip: envelopeTip}
	if body != nil {
		raw, err := proto.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		record.body = raw
	}
	return record
}

func TestSyncSmokeRegisteredDomainsAreSortedAndExcludeUnspecified(t *testing.T) {
	domains := syncSmokeRegisteredDomains()
	if len(domains) == 0 {
		t.Fatal("no registered domains")
	}
	if !slices.IsSorted(domains) {
		t.Fatalf("domains not sorted: %v", domains)
	}
	if slices.Contains(domains, scene.SyncDomain_SYNC_DOMAIN_UNSPECIFIED) {
		t.Fatalf("UNSPECIFIED must not be registered: %v", domains)
	}
	for _, want := range []scene.SyncDomain{syncTestFriendList, syncTestFriendRequest, syncTestFriendBlock, syncTestTeam} {
		if !slices.Contains(domains, want) {
			t.Fatalf("missing domain %s in %v", want, domains)
		}
	}
	if len(domains) != len(scene.SyncDomain_name)-1 {
		t.Fatalf("registered %d domains, enum has %d values (minus UNSPECIFIED)", len(domains), len(scene.SyncDomain_name)-1)
	}
}

func TestSyncSmokeParseDigestAcceptsLegalShapes(t *testing.T) {
	cases := []struct {
		name string
		push *scene.SyncDigestS2C
		want syncSmokeDigest
	}{
		{"digest absent", &scene.SyncDigestS2C{}, syncTestDigest(0, nil)},
		{"generation without entries", &scene.SyncDigestS2C{Generation: 7}, syncTestDigest(7, nil)},
		{"entries", &scene.SyncDigestS2C{Generation: 7, Entries: []*scene.SyncDigestEntry{
			{Domain: syncTestFriendRequest, Version: 3},
			{Domain: syncTestFriendBlock, Version: 1},
		}}, syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1})},
		// 新服务端先上、生成枚举还没跟上时会出现不认识的正域号:保留,不当成坏形状。
		{"unknown positive domain kept", &scene.SyncDigestS2C{Generation: 7, Entries: []*scene.SyncDigestEntry{
			{Domain: scene.SyncDomain(9999), Version: 2},
		}}, syncTestDigest(7, map[scene.SyncDomain]uint64{scene.SyncDomain(9999): 2})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := syncSmokeParseDigest(c.push)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.status != c.want.status || got.generation != c.want.generation || !maps.Equal(got.versions, c.want.versions) {
				t.Fatalf("got %s, want %s", syncSmokeDescribe(got), syncSmokeDescribe(c.want))
			}
		})
	}
}

func TestSyncSmokeParseDigestRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		push *scene.SyncDigestS2C
	}{
		{"unspecified domain", &scene.SyncDigestS2C{Generation: 7, Entries: []*scene.SyncDigestEntry{
			{Domain: scene.SyncDomain_SYNC_DOMAIN_UNSPECIFIED, Version: 1},
		}}},
		{"negative domain", &scene.SyncDigestS2C{Generation: 7, Entries: []*scene.SyncDigestEntry{
			{Domain: scene.SyncDomain(-1), Version: 1},
		}}},
		{"duplicated domain", &scene.SyncDigestS2C{Generation: 7, Entries: []*scene.SyncDigestEntry{
			{Domain: syncTestFriendRequest, Version: 1},
			{Domain: syncTestFriendRequest, Version: 2},
		}}},
		{"entries without generation", &scene.SyncDigestS2C{Entries: []*scene.SyncDigestEntry{
			{Domain: syncTestFriendRequest, Version: 1},
		}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := syncSmokeParseDigest(c.push); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// UNAVAILABLE 时 generation / entries 没有意义,不校验形状,只把 status 交给调用方判。
func TestSyncSmokeParseDigestSkipsShapeChecksWhenUnavailable(t *testing.T) {
	got, err := syncSmokeParseDigest(&scene.SyncDigestS2C{
		Status:  scene.SyncDigestStatus_SYNC_DIGEST_STATUS_UNAVAILABLE,
		Entries: []*scene.SyncDigestEntry{{Domain: scene.SyncDomain_SYNC_DOMAIN_UNSPECIFIED}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.status != scene.SyncDigestStatus_SYNC_DIGEST_STATUS_UNAVAILABLE {
		t.Fatalf("status=%s, want UNAVAILABLE", got.status)
	}
}

func TestSyncSmokeDiffTreatsAbsentAsZeroAndSortsByDomain(t *testing.T) {
	before := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestTeam: 1, syncTestFriendList: 0, syncTestFriendBlock: 2})
	after := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestTeam: 3, syncTestFriendRequest: 1, syncTestFriendBlock: 2})
	got := syncSmokeDiff(before, after)
	want := []syncSmokeDomainChange{
		{domain: syncTestFriendRequest, before: 0, after: 1},
		{domain: syncTestTeam, before: 1, after: 3},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %s, want %s", syncSmokeDescribeChanges(got), syncSmokeDescribeChanges(want))
	}
	if changes := syncSmokeDiff(before, before); len(changes) != 0 {
		t.Fatalf("identical digests must not differ: %s", syncSmokeDescribeChanges(changes))
	}
}

func TestSyncSmokeCheckGenerationStable(t *testing.T) {
	cases := []struct {
		name          string
		before, after uint64
		wantErr       bool
	}{
		{"absent stays absent", 0, 0, false},
		{"hash created", 0, 9, false},
		{"unchanged", 9, 9, false},
		{"hash disappeared", 9, 0, true},
		{"hash rebuilt", 9, 10, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := syncSmokeCheckGenerationStable(syncTestDigest(c.before, nil), syncTestDigest(c.after, nil))
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%t", err, c.wantErr)
			}
		})
	}
}

func TestSyncSmokeCheckUnchanged(t *testing.T) {
	base := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1})
	if err := syncSmokeCheckUnchanged(base, syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1})); err != nil {
		t.Fatalf("identical digests rejected: %v", err)
	}
	// 显式的 0 与缺席等价。
	if err := syncSmokeCheckUnchanged(base, syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1, syncTestTeam: 0})); err != nil {
		t.Fatalf("explicit zero must equal absent: %v", err)
	}
	if err := syncSmokeCheckUnchanged(syncTestDigest(0, nil), syncTestDigest(0, nil)); err != nil {
		t.Fatalf("absent -> absent rejected: %v", err)
	}
	bad := []struct {
		name          string
		before, after syncSmokeDigest
	}{
		{"count changed", base, syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 4, syncTestFriendBlock: 1})},
		{"domain appeared", base, syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1, syncTestTeam: 1})},
		{"generation changed", base, syncTestDigest(8, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1})},
		{"digest disappeared", base, syncTestDigest(0, nil)},
		{"digest created", syncTestDigest(0, nil), syncTestDigest(5, map[scene.SyncDomain]uint64{syncTestTeam: 1})},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := syncSmokeCheckUnchanged(c.before, c.after); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestSyncSmokeCheckSingleBumpAcceptsExactlyOneIncrement(t *testing.T) {
	before := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1})
	after := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 4, syncTestFriendBlock: 1})
	if err := syncSmokeCheckSingleBump(before, after, syncTestFriendRequest); err != nil {
		t.Fatalf("legal single bump rejected: %v", err)
	}
	// 摘要此前不存在:第一次 bump 建 hash、写入新的 g,该域从 0 到 1。
	created := syncTestDigest(5, map[scene.SyncDomain]uint64{syncTestFriendRequest: 1})
	if err := syncSmokeCheckSingleBump(syncTestDigest(0, nil), created, syncTestFriendRequest); err != nil {
		t.Fatalf("hash creation by the first bump rejected: %v", err)
	}
}

func TestSyncSmokeCheckSingleBumpReportsNotYet(t *testing.T) {
	before := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3})
	if err := syncSmokeCheckSingleBump(before, before, syncTestFriendRequest); !errors.Is(err, errSyncSmokeNotBumpedYet) {
		t.Fatalf("err=%v, want errSyncSmokeNotBumpedYet", err)
	}
	if err := syncSmokeCheckSingleBump(syncTestDigest(0, nil), syncTestDigest(0, nil), syncTestFriendRequest); !errors.Is(err, errSyncSmokeNotBumpedYet) {
		t.Fatalf("absent -> absent: err=%v, want errSyncSmokeNotBumpedYet", err)
	}
}

func TestSyncSmokeCheckSingleBumpRejectsWrongShapes(t *testing.T) {
	before := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1})
	cases := []struct {
		name  string
		after syncSmokeDigest
	}{
		{"other domain also changed", syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 4, syncTestFriendBlock: 2})},
		{"only another domain changed", syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 2})},
		{"new domain appeared", syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 4, syncTestFriendBlock: 1, syncTestTeam: 1})},
		{"incremented by two", syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 5, syncTestFriendBlock: 1})},
		{"went down", syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 2, syncTestFriendBlock: 1})},
		{"generation changed", syncTestDigest(8, map[scene.SyncDomain]uint64{syncTestFriendRequest: 4, syncTestFriendBlock: 1})},
		{"digest disappeared", syncTestDigest(0, nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := syncSmokeCheckSingleBump(before, c.after, syncTestFriendRequest)
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, errSyncSmokeNotBumpedYet) {
				t.Fatalf("wrong shape must not be reported as not-yet: %v", err)
			}
		})
	}
}

func TestSyncSmokeDirtyDomains(t *testing.T) {
	registered := []scene.SyncDomain{syncTestFriendList, syncTestFriendRequest, syncTestFriendBlock, syncTestTeam}
	cached := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1})
	cases := []struct {
		name   string
		cached *syncSmokeDigest
		fresh  syncSmokeDigest
		want   []scene.SyncDomain
	}{
		{"no cache pulls everything", nil, cached, registered},
		{"unavailable pulls everything", &cached, syncTestUnavailable(), registered},
		{"generation changed pulls everything",
			&cached, syncTestDigest(8, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1}), registered},
		{"only changed domain",
			&cached, syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 4, syncTestFriendBlock: 1}),
			[]scene.SyncDomain{syncTestFriendRequest}},
		// 比较用「不相等」:计数变小(回档)同样要拉。
		{"count went down is still dirty",
			&cached, syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 2, syncTestFriendBlock: 1}),
			[]scene.SyncDomain{syncTestFriendRequest}},
		{"newly present domain",
			&cached, syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1, syncTestTeam: 1}),
			[]scene.SyncDomain{syncTestTeam}},
		{"nothing changed", &cached, cached, nil},
		// 不在注册集合里的域(机器人枚举还没跟上的新域)不参与待拉取计算。
		{"unregistered domain ignored",
			&cached, syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1, scene.SyncDomain(9999): 1}),
			nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := syncSmokeDirtyDomains(c.cached, c.fresh, registered)
			if !slices.Equal(got, c.want) {
				t.Fatalf("got %s, want %s", syncSmokeDomainList(got), syncSmokeDomainList(c.want))
			}
		})
	}
	// 全量拉取返回的是副本:调用方改它不能改到注册集合。
	all := syncSmokeDirtyDomains(nil, cached, registered)
	all[0] = syncTestTeam
	if registered[0] != syncTestFriendList {
		t.Fatal("dirty set aliases the registered slice")
	}
}

// syncTestSameProbe 逐字段比较两个探针结局。digest 是指针,== 只比地址没有意义:只比是否存在、status 与 g。
func syncTestSameProbe(got, want syncSmokeProbe) bool {
	return got.outcome == want.outcome &&
		got.envelopeTip == want.envelopeTip &&
		got.replyBeforeEnterScene == want.replyBeforeEnterScene &&
		got.kicked == want.kicked &&
		(got.digest == nil) == (want.digest == nil) &&
		got.digest.GetStatus() == want.digest.GetStatus() &&
		got.digest.GetGeneration() == want.digest.GetGeneration()
}

func syncTestDescribeProbe(probe syncSmokeProbe) string {
	digest := "nil"
	if probe.digest != nil {
		digest = fmt.Sprintf("{status=%s g=%d}", probe.digest.GetStatus(), probe.digest.GetGeneration())
	}
	return fmt.Sprintf("{outcome=%s envelope_tip=%d reply_before_enter_scene=%t kicked=%t digest=%s}",
		probe.outcome, probe.envelopeTip, probe.replyBeforeEnterScene, probe.kicked, digest)
}

func TestSyncSmokeClassifyProbe(t *testing.T) {
	okPush := &scene.SyncDigestS2C{Generation: 7}
	unavailablePush := &scene.SyncDigestS2C{Status: scene.SyncDigestStatus_SYNC_DIGEST_STATUS_UNAVAILABLE}
	enter := syncTestRecord(t, game.SceneSceneClientPlayerNotifyEnterSceneMessageId, 0, &scene.EnterSceneS2C{})
	okReply := syncTestRecord(t, game.SceneSceneClientPlayerNotifySyncDigestMessageId, 0, okPush)
	unavailableReply := syncTestRecord(t, game.SceneSceneClientPlayerNotifySyncDigestMessageId, 0, unavailablePush)
	envelope := syncTestRecord(t, game.SceneSceneClientPlayerGetSyncDigestMessageId, tipSyncRateLimited, nil)
	kick := syncTestRecord(t, game.SceneClientPlayerCommonKickPlayerMessageId, 0, nil)
	undecodable := travelSmokeRecord{messageId: game.SceneSceneClientPlayerNotifySyncDigestMessageId, body: []byte{0xff, 0xff, 0xff}}

	cases := []struct {
		name    string
		records []travelSmokeRecord
		want    syncSmokeProbe
	}{
		{"nothing arrived", nil, syncSmokeProbe{outcome: syncSmokeProbeDropped}},
		{"only enter scene", []travelSmokeRecord{enter}, syncSmokeProbe{outcome: syncSmokeProbeDropped}},
		{"reply after enter scene", []travelSmokeRecord{enter, okReply},
			syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, digest: okPush}},
		{"reply before enter scene", []travelSmokeRecord{okReply, enter},
			syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, replyBeforeEnterScene: true, digest: okPush}},
		{"unavailable reply", []travelSmokeRecord{enter, unavailableReply},
			syncSmokeProbe{outcome: syncSmokeProbeRepliedUnavailable, digest: unavailablePush}},
		{"envelope rejected", []travelSmokeRecord{envelope, enter},
			syncSmokeProbe{outcome: syncSmokeProbeEnvelopeRejected, envelopeTip: tipSyncRateLimited}},
		// 第一个决定性的结局为准:信封拒绝之后迟到的 254(不可能来自同一请求)不改写结论,也不带出摘要。
		{"first outcome wins", []travelSmokeRecord{envelope, okReply},
			syncSmokeProbe{outcome: syncSmokeProbeEnvelopeRejected, envelopeTip: tipSyncRateLimited}},
		{"undecodable reply", []travelSmokeRecord{undecodable},
			syncSmokeProbe{outcome: syncSmokeProbeUndecodable, replyBeforeEnterScene: true}},
		// kicked 扫完全部留底,不因结局已定而停。
		{"kicked after reply", []travelSmokeRecord{enter, okReply, kick},
			syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, kicked: true, digest: okPush}},
		// 253 不带信封拒绝的回包(scene 对 Empty 方法本不回包)不算结局。
		{"plain 253 ignored", []travelSmokeRecord{syncTestRecord(t, game.SceneSceneClientPlayerGetSyncDigestMessageId, 0, nil)},
			syncSmokeProbe{outcome: syncSmokeProbeDropped}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := syncSmokeClassifyProbe(c.records); !syncTestSameProbe(got, c.want) {
				t.Fatalf("got %s, want %s", syncTestDescribeProbe(got), syncTestDescribeProbe(c.want))
			}
		})
	}
}

// S6 的硬判定只拦与时序无关的几种情形;取决于时序的结局必须放行,否则冒烟会随机红。
func TestSyncSmokeJudgeProbe(t *testing.T) {
	last := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestFriendRequest: 3, syncTestFriendBlock: 1})
	samePush := &scene.SyncDigestS2C{Generation: 7, Entries: []*scene.SyncDigestEntry{
		{Domain: syncTestFriendBlock, Version: 1},
		{Domain: syncTestFriendRequest, Version: 3},
	}}
	pass := []struct {
		name  string
		probe syncSmokeProbe
	}{
		{"dropped", syncSmokeProbe{outcome: syncSmokeProbeDropped}},
		// 1003:gate 会话尚未绑定 scene(Scene not ready),取决于时序。
		{"envelope 1003", syncSmokeProbe{outcome: syncSmokeProbeEnvelopeRejected, envelopeTip: tipSyncServiceUnavailable}},
		{"replied ok and unchanged", syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, digest: samePush}},
		{"replied ok before enter scene and unchanged",
			syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, replyBeforeEnterScene: true, digest: samePush}},
		{"replied unavailable", syncSmokeProbe{outcome: syncSmokeProbeRepliedUnavailable,
			digest: &scene.SyncDigestS2C{Status: scene.SyncDigestStatus_SYNC_DIGEST_STATUS_UNAVAILABLE}}},
	}
	for _, c := range pass {
		t.Run("accept "+c.name, func(t *testing.T) {
			if err := syncSmokeJudgeProbe(c.probe, last); err != nil {
				t.Fatalf("timing-dependent outcome must pass, got: %v", err)
			}
		})
	}

	// wantErr 是失败原因里必须出现的子串:只断言"报错"的话,拦下它的可能是另一条规则,原因会指错方向。
	fail := []struct {
		name    string
		probe   syncSmokeProbe
		wantErr string
	}{
		{"undecodable", syncSmokeProbe{outcome: syncSmokeProbeUndecodable}, "could not be decoded"},
		// 1008:冒烟自己的 253 节流失效。
		{"envelope 1008", syncSmokeProbe{outcome: syncSmokeProbeEnvelopeRejected, envelopeTip: tipSyncRateLimited}, "throttle is broken"},
		{"replied ok with a count changed", syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, digest: &scene.SyncDigestS2C{Generation: 7,
			Entries: []*scene.SyncDigestEntry{{Domain: syncTestFriendBlock, Version: 1}, {Domain: syncTestFriendRequest, Version: 4}}}},
			"domain counts changed"},
		{"replied ok with generation changed", syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, digest: &scene.SyncDigestS2C{Generation: 8,
			Entries: []*scene.SyncDigestEntry{{Domain: syncTestFriendBlock, Version: 1}, {Domain: syncTestFriendRequest, Version: 3}}}},
			"g went 7 -> 8"},
		{"replied ok with digest gone", syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, digest: &scene.SyncDigestS2C{}},
			"the digest disappeared"},
		{"replied ok violating the contract", syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, digest: &scene.SyncDigestS2C{Generation: 7,
			Entries: []*scene.SyncDigestEntry{{Domain: syncTestFriendRequest, Version: 3}, {Domain: syncTestFriendRequest, Version: 3}}}},
			"violates the contract"},
		{"replied ok without digest", syncSmokeProbe{outcome: syncSmokeProbeRepliedOK}, "carries no decoded NotifySyncDigest"},
		// 被踢与结局无关:即便探针被正常丢弃,会话也不能断。
		{"kicked", syncSmokeProbe{outcome: syncSmokeProbeDropped, kicked: true}, "received KickPlayer"},
		// 被踢先于结局判定:回了与之前一致的摘要(单看结局会放行)也要报踢线,而不是放行或报别的原因。
		{"kicked after an unchanged ok reply", syncSmokeProbe{outcome: syncSmokeProbeRepliedOK, kicked: true, digest: samePush},
			"received KickPlayer"},
	}
	for _, c := range fail {
		t.Run("reject "+c.name, func(t *testing.T) {
			err := syncSmokeJudgeProbe(c.probe, last)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not mention %q", err, c.wantErr)
			}
		})
	}
}

func TestSyncSmokePrecleanVerdict(t *testing.T) {
	readFailure := errors.New("GetPendingRequests was rejected with tip=1003")
	cases := []struct {
		name           string
		seen, residual []uint64
		err            error
		wantErr        string // 空 = 应放行
	}{
		{"inbox already clean", nil, nil, nil, ""},
		{"seen requests all rejected", []uint64{31, 34}, nil, nil, ""},
		// 读不到收件箱就无从确认有没有残留:与传输层失败一样判失败。
		{"pending read failed", nil, nil, readFailure, "tip=1003"},
		{"reject failed after the first read", []uint64{31}, nil, errors.New("RejectFriend request_id=31: timeout"), "request_id=31"},
		{"residual after rejecting", []uint64{31, 34}, []uint64{34}, nil, "[34] are still PENDING"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := syncSmokePrecleanVerdict(c.seen, c.residual, c.err)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not mention %q", err, c.wantErr)
			}
			if c.err != nil && !errors.Is(err, c.err) {
				t.Fatalf("error %v does not wrap the rejectPendingFrom error %v", err, c.err)
			}
		})
	}
}

func TestSyncSmokeIsReplayedRequest(t *testing.T) {
	cases := []struct {
		name string
		seen []uint64
		id   uint64
		want bool
	}{
		{"preclean saw nothing", nil, 31, false},
		{"id the preclean saw pending", []uint64{31, 34}, 34, true},
		{"fresh id", []uint64{31, 34}, 35, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := syncSmokeIsReplayedRequest(c.seen, c.id); got != c.want {
				t.Fatalf("got %t, want %t", got, c.want)
			}
		})
	}
}

func TestSyncSmokePendingIDsFrom(t *testing.T) {
	const peer, other uint64 = 1001, 2002
	pending := friendpb.FriendRequestStatus_FRIEND_REQUEST_PENDING
	requests := []*friendpb.FriendRequest{
		{RequestId: 31, FromPlayerId: peer, Status: pending},
		// 别人发的
		{RequestId: 32, FromPlayerId: other, Status: pending},
		// 已不是 PENDING
		{RequestId: 33, FromPlayerId: peer, Status: friendpb.FriendRequestStatus_FRIEND_REQUEST_REJECTED},
		// 号源回填前的旧行:按号拒不掉,AddFriend 遇到它会补发新号并 bump,不计入
		{RequestId: 0, FromPlayerId: peer, Status: pending},
		{RequestId: 34, FromPlayerId: peer, Status: pending},
	}
	if got, want := syncSmokePendingIDsFrom(requests, peer), []uint64{31, 34}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := syncSmokePendingIDsFrom(nil, peer); len(got) != 0 {
		t.Fatalf("empty inbox: got %v", got)
	}
}

// 同一个码在 253(gate 直转 scene)与好友消息(经路由服)上原因不同,提示不能互相错指。
func TestSyncSmokeTipHintDependsOnMessage(t *testing.T) {
	digest := uint32(game.SceneSceneClientPlayerGetSyncDigestMessageId)
	friend := uint32(game.ClientPlayerFriendAddFriendMessageId)
	cases := []struct {
		name           string
		messageId, tip uint32
		want, wantNot  string
	}{
		{"253 unavailable points at scene binding", digest, tipSyncServiceUnavailable, "Scene not ready", "friend"},
		{"friend unavailable points at router", friend, tipSyncServiceUnavailable, "friend", "Scene not ready"},
		{"253 rate limited is the gate limit only", digest, tipSyncRateLimited, "syncSmokeDigestSpacing", "lane"},
		{"friend rate limited mentions the router lane", friend, tipSyncRateLimited, "lane", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hint := syncSmokeTipHint(c.messageId, c.tip)
			if !strings.Contains(hint, c.want) {
				t.Fatalf("hint %q does not mention %q", hint, c.want)
			}
			if c.wantNot != "" && strings.Contains(hint, c.wantNot) {
				t.Fatalf("hint %q must not mention %q", hint, c.wantNot)
			}
		})
	}
	if hint := syncSmokeTipHint(digest, 0); hint != "" {
		t.Fatalf("unknown tip must have no hint, got %q", hint)
	}
}

func TestSyncSmokeDescribeIsDeterministic(t *testing.T) {
	digest := syncTestDigest(7, map[scene.SyncDomain]uint64{syncTestTeam: 1, syncTestFriendRequest: 3})
	if got, want := syncSmokeDescribe(digest), "{g=7 FRIEND_REQUEST=3 TEAM=1}"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := syncSmokeDescribe(syncTestUnavailable()), "{status=SYNC_DIGEST_STATUS_UNAVAILABLE}"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := syncSmokeDomainList(nil), "[]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	changes := []syncSmokeDomainChange{{domain: syncTestFriendRequest, before: 3, after: 4}}
	if got, want := syncSmokeDescribeChanges(changes), "[FRIEND_REQUEST 3->4]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// travel-smoke 与 sync-smoke 共用的传送判据,以及 travel-smoke 的金币回包判定。
// travel-smoke 没有自己的单测文件,这些纯函数的单测放在这里。
// ---------------------------------------------------------------------------

// travelTestReply 构造一条成功形状的回包(body 序列化进 serialized_message)。
func travelTestReply(t *testing.T, body proto.Message) *base.MessageContent {
	t.Helper()
	raw, err := proto.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return &base.MessageContent{SerializedMessage: raw}
}

// travelTestSettled 构造一份已有结局的 TravelToZone 调用快照。
func travelTestSettled(reply *base.MessageContent, err error) travelSmokeReplyOutcome {
	return travelSmokeReplyOutcome{settled: true, reply: reply, err: err}
}

// 传送等待里回包是可选的:没结局、旧连接被换掉、认领窗口超时都不能判失败;回包带拒绝码才立即失败。
func TestTravelSmokeJudgeTravelReply(t *testing.T) {
	accepted := travelTestReply(t, &scene.TravelToZoneResponse{})
	rejected := travelTestReply(t, &scene.TravelToZoneResponse{ErrorMessage: &base.TipInfoMessage{Id: 7}})
	envelope := &base.MessageContent{ErrorMessage: &base.TipInfoMessage{Id: tipSyncRateLimited}}
	undecodable := &base.MessageContent{SerializedMessage: []byte{0xff, 0xff, 0xff}}

	cases := []struct {
		name     string
		reply    travelSmokeReplyOutcome
		wantSeen bool
		wantErr  string // 空 = 应放行
	}{
		{"not settled yet", travelSmokeReplyOutcome{}, false, ""},
		{"accepted", travelTestSettled(accepted, nil), true, ""},
		// 124 一到 SwapConn 就让旧连接上的在途调用以 ErrRPCDisconnected 收场:正常路径。
		{"old connection swapped", travelTestSettled(nil, pkg.ErrRPCDisconnected), false, ""},
		{"no reply within the claim window", travelTestSettled(nil, pkg.ErrRPCTimeout), false, ""},
		{"rejected in the reply body", travelTestSettled(rejected, nil), true, "scene 同步拒绝 tip=7"},
		{"envelope rejection", travelTestSettled(envelope, &pkg.ServerTipError{Tip: tipSyncRateLimited}), true,
			fmt.Sprintf("gate 信封拒绝 tip=%d", tipSyncRateLimited)},
		{"undecodable reply body", travelTestSettled(undecodable, nil), true, "decode TravelToZoneResponse"},
		{"not sent", travelTestSettled(nil, fmt.Errorf("%w: game client has no connection", pkg.ErrRPCNotSent)), false,
			"TravelToZone call failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			seen, err := travelSmokeJudgeTravelReply(c.reply)
			if seen != c.wantSeen {
				t.Fatalf("seen=%t, want %t", seen, c.wantSeen)
			}
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not mention %q", err, c.wantErr)
			}
		})
	}
}

// travelSmokeScanHop 先判回包结局再扫留底:replySeen 取自结局;回包被拒时即便留底里已有 124 与进场也判失败。
func TestTravelSmokeScanHopUsesCallOutcome(t *testing.T) {
	redirect := syncTestRecord(t, game.SceneClientPlayerCommonRedirectToGateMessageId, 0,
		&scene.RedirectToGateNotify{TargetIp: "127.0.0.1", TargetPort: 7001})
	enter := syncTestRecord(t, game.SceneSceneClientPlayerNotifyEnterSceneMessageId, 0,
		&scene.EnterSceneS2C{SceneInfo: &scene.SceneInfoComp{SceneConfigId: 3}})
	accepted := travelTestReply(t, &scene.TravelToZoneResponse{})
	rejected := travelTestReply(t, &scene.TravelToZoneResponse{ErrorMessage: &base.TipInfoMessage{Id: 7}})

	scan, err := travelSmokeScanHop(travelTestSettled(nil, pkg.ErrRPCDisconnected), []travelSmokeRecord{redirect, enter})
	if err != nil {
		t.Fatalf("swapped-out call with redirect and arrival must pass, got: %v", err)
	}
	if scan.replySeen || scan.hop == nil || !scan.arrived || scan.hop.sceneConfigId != 3 {
		t.Fatalf("got replySeen=%t hop=%v arrived=%t, want replySeen=false, a hop with scene_config_id=3 and arrived=true",
			scan.replySeen, scan.hop, scan.arrived)
	}

	scan, err = travelSmokeScanHop(travelTestSettled(accepted, nil), nil)
	if err != nil || !scan.replySeen || scan.hop != nil {
		t.Fatalf("accepted reply without redirect yet: got replySeen=%t hop=%v err=%v, want replySeen=true, no hop, no error",
			scan.replySeen, scan.hop, err)
	}

	scan, err = travelSmokeScanHop(travelTestSettled(rejected, nil), []travelSmokeRecord{redirect, enter})
	if err == nil || !strings.Contains(err.Error(), "scene 同步拒绝 tip=7") {
		t.Fatalf("rejected reply must fail the hop, got err=%v", err)
	}
	if !scan.replySeen {
		t.Fatal("a rejected reply still counts as seen")
	}

	// 受理之后、124 之前的 SendTipToClient 是异步失败通知,与回包结局无关,照旧立即失败。
	tip := syncTestRecord(t, game.SceneClientPlayerCommonSendTipToClientMessageId, 0, &base.TipInfoMessage{Id: 9})
	if _, err := travelSmokeScanHop(travelTestSettled(accepted, nil), []travelSmokeRecord{tip}); err == nil ||
		!strings.Contains(err.Error(), "受理后失败") {
		t.Fatalf("a tip before msg 124 must fail the hop, got err=%v", err)
	}
}

// expectTravelRejected 要求同步拒绝:只有回包体拒绝码非 0 才通过;受理、信封拒绝、没回包、断线都失败。
func TestTravelSmokeJudgeRejection(t *testing.T) {
	const zone uint32 = 0
	rejected := travelTestReply(t, &scene.TravelToZoneResponse{ErrorMessage: &base.TipInfoMessage{Id: 7}})
	accepted := travelTestReply(t, &scene.TravelToZoneResponse{})
	envelope := &base.MessageContent{ErrorMessage: &base.TipInfoMessage{Id: tipSyncRateLimited}}

	tip, err := travelSmokeJudgeRejection(zone, travelTestSettled(rejected, nil), travelSmokeUncorrelatedReplies{})
	if err != nil || tip != 7 {
		t.Fatalf("rejected reply: got tip=%d err=%v, want tip=7 and no error", tip, err)
	}

	// 窗口内收到过一条没回显请求号、回包体拒绝码为 7 的回包:超时原因要说出它,而不是"没有回包"。
	var uncorrelated travelSmokeUncorrelatedReplies
	uncorrelated.note(rejected)

	fail := []struct {
		name         string
		reply        travelSmokeReplyOutcome
		uncorrelated travelSmokeUncorrelatedReplies
		wantErr      string
		wantNot      string // 非空 = 失败原因里不得出现
	}{
		{"accepted", travelTestSettled(accepted, nil), travelSmokeUncorrelatedReplies{}, "tip=0", ""},
		{"envelope rejection is not a business rejection", travelTestSettled(envelope, &pkg.ServerTipError{Tip: tipSyncRateLimited}),
			travelSmokeUncorrelatedReplies{}, fmt.Sprintf("被 gate 信封拒绝 tip=%d", tipSyncRateLimited), ""},
		{"no reply", travelTestSettled(nil, pkg.ErrRPCTimeout), travelSmokeUncorrelatedReplies{}, "内没有回包", "without a request id"},
		// 判定不变(仍是失败),只是原因改写:回包到了、带拒绝码,但服务端没回显请求号。
		{"reply arrived without request id", travelTestSettled(nil, pkg.ErrRPCTimeout), uncorrelated,
			"received 1 TravelToZone reply(ies) without a request id", "内没有回包"},
		{"reply without request id carries the body tip", travelTestSettled(nil, pkg.ErrRPCTimeout), uncorrelated, "body tip=7", ""},
		{"disconnected", travelTestSettled(nil, pkg.ErrRPCDisconnected), uncorrelated, pkg.ErrRPCDisconnected.Error(), "without a request id"},
		{"not settled", travelSmokeReplyOutcome{}, travelSmokeUncorrelatedReplies{}, "caller bug", ""},
	}
	for _, c := range fail {
		t.Run(c.name, func(t *testing.T) {
			tip, err := travelSmokeJudgeRejection(zone, c.reply, c.uncorrelated)
			if err == nil {
				t.Fatalf("expected an error, got tip=%d", tip)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not mention %q", err, c.wantErr)
			}
			if c.wantNot != "" && strings.Contains(err.Error(), c.wantNot) {
				t.Fatalf("error %q must not mention %q", err, c.wantNot)
			}
		})
	}
}

// 没回显请求号的 TravelToZone 回包:计数、最近一条的信封 tip 与回包体 tip、按等待窗口截取、文案。
func TestTravelSmokeUncorrelatedReplies(t *testing.T) {
	var u travelSmokeUncorrelatedReplies
	if got := u.describe(); got != "" {
		t.Fatalf("zero value must describe nothing, got %q", got)
	}

	u.note(travelTestReply(t, &scene.TravelToZoneResponse{ErrorMessage: &base.TipInfoMessage{Id: 7}}))
	if u.count != 1 || u.lastEnvelopeTip != 0 || u.lastBodyTip != 7 || u.lastBodyUndecodable {
		t.Fatalf("after a body rejection: got %+v, want count=1 envelope=0 body=7", u)
	}
	before := u

	// 信封拒绝:回包体为空,回包体 tip 归 0,不能沿用上一条的 7。
	u.note(&base.MessageContent{ErrorMessage: &base.TipInfoMessage{Id: tipSyncRateLimited}})
	if u.count != 2 || u.lastEnvelopeTip != tipSyncRateLimited || u.lastBodyTip != 0 || u.lastBodyUndecodable {
		t.Fatalf("after an envelope rejection: got %+v, want count=2 envelope=%d body=0", u, tipSyncRateLimited)
	}
	described := u.describe()
	wants := []string{
		"received 2 TravelToZone reply(ies) without a request id",
		fmt.Sprintf("envelope tip=%d", tipSyncRateLimited),
		"kRateLimitExceeded",
		"body empty (envelope rejection)",
		"ClientRequest.id",
	}
	for _, want := range wants {
		if !strings.Contains(described, want) {
			t.Fatalf("describe %q does not mention %q", described, want)
		}
	}
	// 信封拒绝时回包体为空:不得写出 body tip 与「0 = accepted」图例,免得被读成 scene 已受理。
	for _, wantNot := range []string{"body tip=", "accepted"} {
		if strings.Contains(described, wantNot) {
			t.Fatalf("describe %q of an envelope rejection must not mention %q", described, wantNot)
		}
	}

	// 按窗口截取:只算 before 之后到的;窗口内一条都没有时返回零值,不能拿更早那条来解释这一次。
	if got := u.since(before); got.count != 1 || got.lastEnvelopeTip != tipSyncRateLimited {
		t.Fatalf("since: got %+v, want count=1 with the latest tips", got)
	}
	if got := u.since(u); got != (travelSmokeUncorrelatedReplies{}) {
		t.Fatalf("empty window: got %+v, want zero value", got)
	}

	u.note(&base.MessageContent{SerializedMessage: []byte{0xff, 0xff, 0xff}})
	if u.count != 3 || !u.lastBodyUndecodable || u.lastBodyTip != 0 {
		t.Fatalf("after an undecodable body: got %+v, want count=3 undecodable", u)
	}
	if got := u.describe(); !strings.Contains(got, "body undecodable") {
		t.Fatalf("describe %q does not mention the undecodable body", got)
	}
}

// 「回包没回显请求号」只在调用既没见到回包、也没见到 124 时才是首要原因。
func TestTravelSmokeUncorrelatedCause(t *testing.T) {
	var uncorrelated travelSmokeUncorrelatedReplies
	uncorrelated.note(travelTestReply(t, &scene.TravelToZoneResponse{ErrorMessage: &base.TipInfoMessage{Id: 7}}))
	cases := []struct {
		name         string
		scan         travelSmokeScan
		uncorrelated travelSmokeUncorrelatedReplies
		want         bool
	}{
		{"nothing seen, uncorrelated reply arrived", travelSmokeScan{}, uncorrelated, true},
		{"nothing seen, no uncorrelated reply", travelSmokeScan{}, travelSmokeUncorrelatedReplies{}, false},
		{"call saw its reply", travelSmokeScan{replySeen: true}, uncorrelated, false},
		{"redirect seen", travelSmokeScan{hop: &travelSmokeHop{targetAddr: "127.0.0.1:7001"}}, uncorrelated, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := travelSmokeUncorrelatedCause(c.scan, c.uncorrelated) != ""; got != c.want {
				t.Fatalf("got cause=%t, want %t", got, c.want)
			}
		})
	}
}

// 一跳超时的文案:回包与 124 都没到、窗口内却收到过没回显请求号的回包时,优先报它;
// 那条回包在窗口之前到的不算;其余分支照旧,只在现场字段里带上计数。
func TestTravelSmokeDescribeTimeout(t *testing.T) {
	const gate = "127.0.0.1:7001"
	const nothingArrived = "回包与 msg 124 都没到"
	var noted travelSmokeProgress
	noted.uncorrelatedTravel.note(travelTestReply(t, &scene.TravelToZoneResponse{ErrorMessage: &base.TipInfoMessage{Id: 7}}))
	hop := &travelSmokeHop{targetAddr: "127.0.0.2:7002"}

	cases := []struct {
		name             string
		scan             travelSmokeScan
		progress, before travelSmokeProgress
		want             []string
		wantNot          string
	}{
		{"uncorrelated reply is the cause", travelSmokeScan{}, noted, travelSmokeProgress{},
			[]string{"uncorrelated_replies=1", "without a request id", "body tip=7", gate}, nothingArrived},
		{"uncorrelated reply before the window does not count", travelSmokeScan{}, noted, noted,
			[]string{"uncorrelated_replies=0", nothingArrived}, "without a request id"},
		{"nothing arrived at all", travelSmokeScan{}, travelSmokeProgress{}, travelSmokeProgress{},
			[]string{"uncorrelated_replies=0", nothingArrived}, "without a request id"},
		{"redirect seen keeps its own hint", travelSmokeScan{hop: hop}, noted, travelSmokeProgress{},
			[]string{"uncorrelated_replies=1", "还没开始重登"}, "without a request id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := travelSmokeDescribeTimeout(c.scan, c.progress, c.before, gate, travelSmokeRedirectTimeout)
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Fatalf("description %q does not mention %q", got, want)
				}
			}
			if c.wantNot != "" && strings.Contains(got, c.wantNot) {
				t.Fatalf("description %q must not mention %q", got, c.wantNot)
			}
		})
	}
}

// 金币读写的回包:gate GM 闸、其余信封拒绝、回包体业务拒绝各自报出可辨认的原因;受理放行。
func TestTravelSmokeReplyRejection(t *testing.T) {
	gmGuard := uint32(tiptable.CommonError_kFeatureUnavailable)
	cases := []struct {
		name        string
		envelopeTip uint32
		reply       travelSmokeTipReply
		wantErr     string // 空 = 应放行
	}{
		{"accepted", 0, &scene.GmAddCurrencyResponse{BalanceAfter: 18}, ""},
		{"gate GM guard", gmGuard, &scene.GmAddCurrencyResponse{}, "gate GM guard rejected it"},
		{"other envelope rejection", tipSyncRateLimited, &scene.GetCurrencyListResponse{}, "travelSmokeRequestSpacing"},
		{"business rejection", 0, &scene.GetCurrencyListResponse{ErrorMessage: &base.TipInfoMessage{Id: 9}}, "scene 业务拒绝 tip=9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := travelSmokeReplyRejection(c.envelopeTip, c.reply)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q does not mention %q", err, c.wantErr)
			}
		})
	}
}

// 金币槽越界按 0(新号没有任何货币记录时空列表是合法回包),与改造前经 Player.GetCurrencyValue 读出的值一致。
// goldSlotValue 是 currency_crash_window_scenario.go 的共享规则,travel 的 readGold 与 readGoldBalance 共用它。
func TestGoldSlotValue(t *testing.T) {
	cases := []struct {
		name     string
		response *scene.GetCurrencyListResponse
		want     uint64
	}{
		{"no currency comp", &scene.GetCurrencyListResponse{}, 0},
		{"empty values", &scene.GetCurrencyListResponse{Currency: &component.CurrencyComp{}}, 0},
		{"gold slot present", &scene.GetCurrencyListResponse{Currency: &component.CurrencyComp{Values: []uint64{42, 5}}}, 42},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := goldSlotValue(c.response); got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}
