package logic

// 落点会话身份的两张纯判定表(docs/design/scene-enter-chain-residuals.md §3.2、§5.2 ①)。
// 逐行覆盖设计稿的判定表;与 C++ enter_fence::Decide(EnterFence.* 用例)同一口径,只差设计稿写明的那一行
// (请求无代次、当前有代次、会话不同:scene 判 kRejectUnversioned,这里 legacy 放行且不降级身份)。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"scene_manager/internal/constants"
)

func TestValidSessionMatchesCppIsBoundSession(t *testing.T) {
	assert.False(t, validSession(0), "0 = 未绑定")
	assert.False(t, validSession(0xFFFFFFFF), "0xFFFFFFFF = C++ 未绑定哨兵")
	for _, sid := range []uint32{1, 11, 0x7FFFFFFF, 0xFFFFFFFE} {
		assert.True(t, validSession(sid), "sid=%d", sid)
	}
}

func TestDecidePlacementIdentity_Table(t *testing.T) {
	// id 是本表的 (会话号, 代次) 简写:这些行都不带 gate 实例(未知);带实例的行见 TestDecidePlacementIdentity_GateInstance。
	type id struct {
		sid uint32
		gen uint64
	}
	identity := func(v id) placementIdentity { return placementIdentity{sessionID: v.sid, loginGeneration: v.gen} }
	const (
		gA = uint64(1757000000001)
		gB = uint64(1757000000002)
	)
	cases := []struct {
		name       string
		cur        id
		curExists  bool
		req        id
		wantVerd   placementVerdict
		wantRecord id
	}{
		// L 不存在
		{"no_location", id{}, false, id{11, gA}, placementAdmitFresh, id{11, gA}},
		{"location_without_identity", id{}, true, id{11, gA}, placementAdmitFresh, id{11, gA}},
		{"location_without_identity_legacy_request", id{}, true, id{11, 0}, placementAdmitFresh, id{11, 0}},
		// 同一会话:代次取 max,不因落后而拒
		{"same_session_newer", id{11, gA}, true, id{11, gB}, placementAdmitSameSession, id{11, gB}},
		{"same_session_older_keeps_max", id{11, gB}, true, id{11, gA}, placementAdmitSameSession, id{11, gB}},
		{"same_session_without_generation", id{22, gB}, true, id{22, 0}, placementAdmitSameSession, id{22, gB}},
		{"same_session_location_without_generation", id{22, 0}, true, id{22, gA}, placementAdmitSameSession, id{22, gA}},
		// 请求会话号无效时不算同一会话
		{"invalid_request_session_equal_sid_zero_gen_equal", id{0, gA}, true, id{0, gA}, placementAdmitSameSession, id{0, gA}},
		{"unbound_request_session_does_not_match", id{0xFFFFFFFF, gB}, true, id{0xFFFFFFFF, gA}, placementRejectStale, id{}},
		// legacy:请求无代次、会话不同
		{"legacy_keeps_versioned_identity", id{22, gB}, true, id{11, 0}, placementAdmitLegacy, id{22, gB}},
		{"legacy_on_unversioned_location_writes_request_session", id{11, 0}, true, id{22, 0}, placementAdmitLegacy, id{22, 0}},
		{"legacy_invalid_request_session_keeps_identity", id{22, gB}, true, id{0, 0}, placementAdmitLegacy, id{22, gB}},
		// newer
		{"location_without_generation_request_versioned", id{11, 0}, true, id{22, gA}, placementAdmitNewer, id{22, gA}},
		{"request_generation_newer", id{11, gA}, true, id{22, gB}, placementAdmitNewer, id{22, gB}},
		// stale
		{"request_generation_older", id{22, gB}, true, id{11, gA}, placementRejectStale, id{}},
		// 代次相等、会话不同
		{"equal_generation_valid_location_session_conflict", id{22, gA}, true, id{11, gA}, placementRejectConflict, id{}},
		{"equal_generation_invalid_request_session_conflict", id{22, gA}, true, id{0, gA}, placementRejectConflict, id{}},
		{"equal_generation_unbound_location_session_admitted", id{0xFFFFFFFF, gA}, true, id{11, gA}, placementAdmitSameSession, id{11, gA}},
		{"equal_generation_zero_location_session_admitted", id{0, gA}, true, id{11, gA}, placementAdmitSameSession, id{11, gA}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verdict, record := decidePlacementIdentity(identity(c.cur), c.curExists, identity(c.req))
			assert.Equal(t, c.wantVerd, verdict, "verdict=%s", verdict)
			assert.Equal(t, identity(c.wantRecord), record)
			assert.Equal(t, c.wantVerd != placementRejectStale && c.wantVerd != placementRejectConflict, verdict.admits())
		})
	}
}

// 会话号相同而 gate 实例不同 = 另一个 gate 进程上的另一条会话(scene-enter-chain-residuals.md §14.8):不走同一会话那一行,
// 按代次裁决;任一边实例未知时仍按会话号认。与 C++ GateInstanceFence.ReusedSessionIdOnAnotherGateProcessIsNotTheSameSession
// 同一组情形(只差 legacy 那一行,见文件头)。
func TestDecidePlacementIdentity_GateInstance(t *testing.T) {
	const (
		gA      = uint64(1757000000001)
		gB      = uint64(1757000000002)
		oldGate = "gate-node3-old-process"
		newGate = "gate-node3-new-process"
	)
	onNew := placementIdentity{sessionID: 11, loginGeneration: gB, gateInstanceID: newGate}
	cases := []struct {
		name       string
		cur        placementIdentity
		req        placementIdentity
		wantVerd   placementVerdict
		wantRecord placementIdentity
	}{
		{"old_process_late_request_is_stale", onNew, placementIdentity{11, gA, oldGate}, placementRejectStale, placementIdentity{}},
		{"old_process_equal_generation_conflicts", onNew, placementIdentity{11, gB, oldGate}, placementRejectConflict, placementIdentity{}},
		{"old_process_without_generation_is_legacy_and_keeps_identity", onNew, placementIdentity{11, 0, oldGate},
			placementAdmitLegacy, onNew},
		{"newer_login_on_another_process_takes_over", placementIdentity{11, gA, oldGate}, placementIdentity{11, gB, newGate},
			placementAdmitNewer, onNew},
		{"same_process_is_the_same_session", onNew, placementIdentity{11, gA, newGate}, placementAdmitSameSession, onNew},
		{"request_without_instance_keeps_the_recorded_one", onNew, placementIdentity{11, gB, ""}, placementAdmitSameSession, onNew},
		{"location_without_instance_learns_it", placementIdentity{11, gB, ""}, placementIdentity{11, gA, newGate},
			placementAdmitSameSession, onNew},
		{"fresh_records_the_request_instance", placementIdentity{}, placementIdentity{11, gA, newGate}, placementAdmitFresh,
			placementIdentity{11, gA, newGate}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			verdict, record := decidePlacementIdentity(c.cur, true, c.req)
			assert.Equal(t, c.wantVerd, verdict, "verdict=%s", verdict)
			assert.Equal(t, c.wantRecord, record)
		})
	}
	assert.True(t, sameGateInstance("", oldGate), "location 实例未知:按会话号认")
	assert.True(t, sameGateInstance(newGate, ""), "请求实例未知(托管落点):按会话号认")
	assert.False(t, sameGateInstance(newGate, oldGate), "两边都已知且不同")
}

func TestPlacementVerdictNamesCoverEveryValue(t *testing.T) {
	seen := map[string]bool{}
	for v := placementVerdict(0); v < placementVerdictCount; v++ {
		name := v.String()
		require.NotEmpty(t, name, "verdict %d 没有名字:名表比枚举短", v)
		require.NotEqual(t, "unknown", name)
		require.False(t, seen[name], "名字 %q 重复", name)
		seen[name] = true
	}
	assert.Equal(t, int(placementVerdictCount), len(placementVerdictNames))
	assert.Equal(t, "unknown", placementVerdictCount.String())
	// 名表就是指标 label,与 metrics.go 的 Help 一一对应。
	assert.Equal(t, []string{"fresh", "same_session", "legacy", "newer", "stale", "conflict"}, placementVerdictNames[:])
}

func TestDecideLeaveIdentity_Table(t *testing.T) {
	// id 是本表的 (会话号, 代次) 简写:离场表以代次为主,不看 gate 实例。
	type id struct {
		sid uint32
		gen uint64
	}
	identity := func(v id) placementIdentity { return placementIdentity{sessionID: v.sid, loginGeneration: v.gen} }
	const (
		g1 = uint64(100)
		g2 = uint64(200)
	)
	cases := []struct {
		name string
		loc  id
		req  id
		want leaveVerdict
	}{
		{"legacy_request_without_generation", id{22, g2}, id{11, 0}, leaveVerdictLegacy},
		{"legacy_location_without_generation", id{22, 0}, id{11, g1}, leaveVerdictLegacy},
		{"legacy_both_without_identity", id{}, id{}, leaveVerdictLegacy},
		{"location_newer_keeps", id{22, g2}, id{11, g1}, leaveVerdictKeepNewer},
		{"same_session_newer_relog_keeps", id{11, g2}, id{11, g1}, leaveVerdictKeepNewer},
		{"equal_generation_different_valid_sessions_conflict", id{22, g1}, id{11, g1}, leaveVerdictKeepConflict},
		{"location_older_deleted", id{11, g1}, id{22, g2}, leaveVerdictDeleted},
		{"same_identity_deleted", id{11, g1}, id{11, g1}, leaveVerdictDeleted},
		{"equal_generation_unbound_location_session_deleted", id{0xFFFFFFFF, g1}, id{11, g1}, leaveVerdictDeleted},
		{"equal_generation_zero_request_session_deleted", id{22, g1}, id{0, g1}, leaveVerdictDeleted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideLeaveIdentity(identity(c.loc), identity(c.req))
			assert.Equal(t, c.want, got, "verdict=%s", got)
			assert.Equal(t, c.want == leaveVerdictLegacy || c.want == leaveVerdictDeleted, got.deletes())
		})
	}
}

func TestLeaveVerdictNamesCoverEveryValue(t *testing.T) {
	seen := map[string]bool{}
	for v := leaveVerdict(0); v < leaveVerdictCount; v++ {
		name := v.String()
		require.NotEmpty(t, name, "verdict %d 没有名字:名表比枚举短", v)
		require.NotEqual(t, "unknown", name)
		require.False(t, seen[name], "名字 %q 重复", name)
		seen[name] = true
	}
	assert.Equal(t, int(leaveVerdictCount), len(leaveVerdictNames))
	assert.Equal(t, "unknown", leaveVerdictCount.String())
	assert.Equal(t, []string{"legacy", "keep_newer", "keep_conflict", "deleted"}, leaveVerdictNames[:])
}

// 码 25 是跨语言契约:C++ player_lifecycle.h 手抄为 kSmErrPlacementSessionSuperseded,并列入
// SmRejectedBeforeAnyPlacementWrite(C++ cross_zone_test.cpp 的 pinnedCodes 钉住那一侧)。这里钉住 Go 这一侧的值,
// 改号必须两边同批改。
func TestErrPlacementSessionSupersededIsPinnedTo25(t *testing.T) {
	assert.Equal(t, uint32(25), constants.ErrPlacementSessionSuperseded)
}
