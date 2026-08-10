package serverbase

import (
	"testing"

	"shared/generated/pb/table"
)

func TestTipVerdict(t *testing.T) {
	tests := []struct {
		name string
		code uint32
		want Verdict
	}{
		{"码 0 是成功", 0, VerdictOK},
		{"kSuccess=1 也是成功", uint32(table.CommonError_kSuccess), VerdictOK},

		// —— 服务端内部故障 ——
		{"依赖不可用", uint32(table.CommonError_kServiceUnavailable), VerdictFault},
		{"服务端实体为空", uint32(table.CommonError_kEntityIsNull), VerdictFault},
		{"会话查不到", uint32(table.CommonError_kSessionNotFound), VerdictFault},
		{"Redis 出错", uint32(table.LoginError_kLoginRedisError), VerdictFault},
		{"登录状态机走死", uint32(table.LoginError_kLoginFsmFailed), VerdictFault},
		{"登录超时", uint32(table.LoginError_kLoginTimeout), VerdictFault},
		{"无可用场景节点", uint32(table.SceneError_kEnterNodeUnavailable), VerdictFault},
		{"服务端场景状态缺失", uint32(table.SceneError_kEnterSceneYourSceneIsNull), VerdictFault},
		{"任务组件缺失", uint32(table.MissionError_kPlayerMissionComponentNotFound), VerdictFault},
		{"背包基础组件缺失", uint32(table.BagError_kBagAddItemHasNotBaseComponent), VerdictFault},

		// —— 正常业务拒绝:这些绝不能被判成故障 ——
		{"背包满不是故障", uint32(table.BagError_kBagAddItemBagFull), VerdictBizReject},
		{"背包空间不足不是故障", uint32(table.BagError_kBagInsufficientBagSpace), VerdictBizReject},
		{"队伍满不是故障", uint32(table.TeamError_kTeamMembersFull), VerdictBizReject},
		{"场景满不是故障", uint32(table.SceneError_kEnterSceneSceneFull), VerdictBizReject},
		{"GS 满不是故障", uint32(table.SceneError_kEnterSceneGsFull), VerdictBizReject},
		{"技能冷却未到不是故障", uint32(table.SkillError_kSkillCooldownNotReady), VerdictBizReject},
		{"限流不是故障", uint32(table.CommonError_kRateLimitExceeded), VerdictBizReject},
		{"客户端参数非法不是故障", uint32(table.CommonError_kInvalidParameter), VerdictBizReject},
		{"请求解析失败是客户端侧", uint32(table.CommonError_kRequestMessageParseError), VerdictBizReject},
		{"账号不存在不是故障", uint32(table.LoginError_kLoginAccountNotFound), VerdictBizReject},
		{"被顶号不是故障", uint32(table.LoginError_kLoginBeKickByAnOtherAccount), VerdictBizReject},
		{"奖励已领取不是故障", uint32(table.RewardError_kRewardAlreadyClaimed), VerdictBizReject},
		{"换场进行中不是故障", uint32(table.CrossServerError_kSceneTransferInProgress), VerdictBizReject},

		// —— 码表漂移 ——
		{"超出已知码表上界", TipMaxKnownCode + 1, VerdictUnknown},
		{"远超上界", 100000, VerdictUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TipVerdict(tt.code); got != tt.want {
				t.Fatalf("TipVerdict(%d) = %v, 期望 %v", tt.code, got, tt.want)
			}
		})
	}
}

// TestTipFaultCodesAllInKnownRange 守住"故障码集合"与码表分段的一致性:
// 如果有人往集合里塞了一个越界的码,它在 TipVerdict 里会先被判成
// VerdictUnknown,永远走不到故障分支 —— 这个测试让那种失配立刻暴露。
func TestTipFaultCodesAllInKnownRange(t *testing.T) {
	for code := range tipFaultCodes {
		if code == 0 || code > TipMaxKnownCode {
			t.Errorf("故障码 %d 超出已知码表范围 (1..%d)", code, TipMaxKnownCode)
			continue
		}
		if TipDomain(code) == "unknown" {
			t.Errorf("故障码 %d 落在 tipDomains 的任何分段之外", code)
		}
		if got := TipVerdict(code); got != VerdictFault {
			t.Errorf("故障码 %d 的 TipVerdict = %v, 期望 VerdictFault", code, got)
		}
	}
}

// TestTipDomainsContiguous 守住分段表本身:必须按序、无空洞、无重叠,
// 且恰好覆盖 1..TipMaxKnownCode。tip 码表是扁平单命名空间,一旦分段
// 出现空洞,落在洞里的真实码会被 TipDomain 报成 unknown。
func TestTipDomainsContiguous(t *testing.T) {
	if len(tipDomains) == 0 {
		t.Fatal("tipDomains 为空")
	}
	if tipDomains[0].Lo != 1 {
		t.Fatalf("tipDomains 起点 = %d, 期望 1", tipDomains[0].Lo)
	}
	for i, r := range tipDomains {
		if r.Lo > r.Hi {
			t.Fatalf("分段 %q 区间倒置: [%d,%d]", r.Domain, r.Lo, r.Hi)
		}
		if i > 0 && r.Lo != tipDomains[i-1].Hi+1 {
			t.Fatalf("分段 %q 与上一段不连续: 上段止于 %d, 本段起于 %d",
				r.Domain, tipDomains[i-1].Hi, r.Lo)
		}
	}
	if last := tipDomains[len(tipDomains)-1].Hi; last != TipMaxKnownCode {
		t.Fatalf("tipDomains 终点 = %d, 期望 TipMaxKnownCode=%d", last, TipMaxKnownCode)
	}
}

func TestTipDomain(t *testing.T) {
	tests := []struct {
		code uint32
		want string
	}{
		{0, "ok"},
		{uint32(table.CommonError_kServiceUnavailable), "common"},
		{uint32(table.LoginError_kLoginRedisError), "login"},
		{uint32(table.SceneError_kEnterSceneFailed), "scene"},
		{uint32(table.TeamError_kTeamMembersFull), "team"},
		{uint32(table.MissionError_kMissionNotInProgress), "mission"},
		{uint32(table.BagError_kBagAddItemBagFull), "bag"},
		{uint32(table.SkillError_kSkillInvalidTarget), "skill"},
		{uint32(table.BuffError_kBuffMaxBuffStack), "buff"},
		{uint32(table.EntityError_kEntityTransformNotFound), "entity"},
		{uint32(table.MountError_kMountNotMounted), "mount"},
		{uint32(table.RewardError_kRewardAlreadyClaimed), "reward"},
		{uint32(table.CrossServerError_kSceneTransferInProgress), "cross_server"},
		{TipMaxKnownCode + 1, "unknown"},
	}
	for _, tt := range tests {
		if got := TipDomain(tt.code); got != tt.want {
			t.Fatalf("TipDomain(%d) = %q, 期望 %q", tt.code, got, tt.want)
		}
	}
}

func TestFaultCodeSet(t *testing.T) {
	// 取 scene_manager 私有码表的真实取值:
	// 1=ErrNoAvailableNode(故障) 8=ErrRedis(故障) 9=ErrDuplicateScene(业务拒绝)
	classify := FaultCodeSet(1, 8)

	tests := []struct {
		name string
		code uint32
		want Verdict
	}{
		{"0 是成功", 0, VerdictOK},
		{"集合内 → 故障", 1, VerdictFault},
		{"集合内 → 故障", 8, VerdictFault},
		{"集合外的非 0 码 → 业务拒绝", 9, VerdictBizReject},
		{"未知的非 0 码 → 业务拒绝(不误报)", 12345, VerdictBizReject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.code); got != tt.want {
				t.Fatalf("classify(%d) = %v, 期望 %v", tt.code, got, tt.want)
			}
		})
	}

	// 空集合:任何非 0 码都只是业务拒绝。
	empty := FaultCodeSet()
	if got := empty(1); got != VerdictBizReject {
		t.Fatalf("空集合 classify(1) = %v, 期望 VerdictBizReject", got)
	}
}

func TestVerdictString(t *testing.T) {
	tests := []struct {
		v    Verdict
		want string
	}{
		{VerdictOK, "ok"},
		{VerdictBizReject, "biz_reject"},
		{VerdictFault, "fault"},
		{VerdictUnknown, "unknown_code"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Fatalf("Verdict(%d).String() = %q, 期望 %q", tt.v, got, tt.want)
		}
	}
}
