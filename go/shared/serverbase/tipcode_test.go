package serverbase

import (
	"testing"

	"shared/generated/pb/table"
	"shared/generated/tip"
)

func TestTipVerdict(t *testing.T) {
	tests := []struct {
		name string
		code uint32
		want Verdict
	}{
		{"码 0 是成功", 0, VerdictOK},
		{"kSuccess 也是成功", uint32(table.CommonError_kSuccess), VerdictOK},

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
		// 这条以前不在 serverbase 的手写表里,靠 guild 的本地 map 补。
		// 分类进 Tip.xlsx 的 fault 列之后,全局判定就能直接认出它。
		{"公会发号器被 fence", uint32(table.GuildError_kGuildIdGenUnavailable), VerdictFault},
		// kMatchInternal 刻意**不标**:match 把它同时用作「缺少玩家身份」的参数出口
		// (一码两用),标了会把参数拒绝刷成故障告警。拆码之前维持业务拒绝,
		// 见 docs/design/tip-code-axis.md「故障分类」一节。
		{"匹配内部错误一码两用,拿不准不标", uint32(table.MatchError_kMatchInternal), VerdictBizReject},

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
		{"公会已满不是故障", uint32(table.GuildError_kGuildFull), VerdictBizReject},
		{"匹配中已在队列不是故障", uint32(table.MatchError_kMatchAlreadyQueued), VerdictBizReject},

		// —— 码表漂移 ——
		{"高于本段已分配上界(对端码表更新)", uint32(table.CrossServerError_kSceneTransferInProgress) + 1, VerdictUnknown},
		{"落在所有段之外", 100000, VerdictUnknown},
		{"段与段之间的空隙", 999, VerdictUnknown},
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
// 如果故障表里有一个越界的码,它在 TipVerdict 里会先被判成
// VerdictUnknown,永远走不到故障分支 —— 这个测试让那种失配立刻暴露。
//
// 故障表与段表都是导表器从同一份 Tip.xlsx 生成的,理论上不可能失配;
// 这里是编译进二进制之后的第二道网(比如有人手改了其中一个产物)。
func TestTipFaultCodesAllInKnownRange(t *testing.T) {
	if len(tip.Faults) == 0 {
		t.Fatal("tip.Faults 为空:导表器没跑过,或 Tip.xlsx 的 fault 列被清空了")
	}
	for _, f := range tip.Faults {
		code := f.Code
		if code == 0 || !tip.InAllocatedRange(code) {
			t.Errorf("故障码 %d(%s.%s) 不在任何段的已分配区间内", code, f.Group, f.Name)
			continue
		}
		if TipDomain(code) == "unknown" {
			t.Errorf("故障码 %d(%s.%s) 落在所有已声明分段之外", code, f.Group, f.Name)
		}
		if !tip.IsFault(code) {
			t.Errorf("故障码 %d(%s.%s) 在 Faults 里却 IsFault=false:产物自相矛盾", code, f.Group, f.Name)
		}
		if got := TipVerdict(code); got != VerdictFault {
			t.Errorf("故障码 %d(%s.%s) 的 TipVerdict = %v, 期望 VerdictFault", code, f.Group, f.Name, got)
		}
	}
}

// TestTipFaultTableIsSorted 守住产物形状:Faults 按码升序且不重复,
// 这样人读产物、diff 产物时才有稳定顺序。
func TestTipFaultTableIsSorted(t *testing.T) {
	for i := 1; i < len(tip.Faults); i++ {
		if tip.Faults[i].Code <= tip.Faults[i-1].Code {
			t.Errorf("tip.Faults 未按码严格升序: [%d]=%d 排在 [%d]=%d 之后",
				i, tip.Faults[i].Code, i-1, tip.Faults[i-1].Code)
		}
	}
}

// TestTipSegmentsDisjointAndSorted 守住段表本身。
//
// 注意与改造前的差别:段**不再要求连续**。改造前 13 个段紧挨着排满 1..129、
// 零余量,于是往任何一组加码都只能拿全局队尾的号、落在自己段外。
// 现在每段各留余量(默认 1000 号),段之间有空隙是正常的、也是刻意的;
// 要守的只有「不重叠、按序、已用区间不越段」。
//
// 段表是生成产物(shared/generated/tip),源头是 data/tip/Tip.xlsx 的组头行;
// 导表器在生成期已经自检过一遍,这里是编译进二进制之后的第二道网。
func TestTipSegmentsDisjointAndSorted(t *testing.T) {
	if len(tip.Segments) == 0 {
		t.Fatal("tip.Segments 为空:导表器没跑过,或段表没被生成出来")
	}
	for i, s := range tip.Segments {
		if s.Width == 0 {
			t.Fatalf("段 %q 的 Width 为 0", s.Domain)
		}
		if s.Count > 0 && (s.Lo < s.Base || s.Hi >= s.Base+s.Width) {
			t.Fatalf("段 %q 的已用区间 [%d,%d] 越出段 [%d,%d)",
				s.Domain, s.Lo, s.Hi, s.Base, s.Base+s.Width)
		}
		if i > 0 {
			prev := tip.Segments[i-1]
			if s.Base < prev.Base {
				t.Fatalf("段表未按 Base 升序: %q(%d) 排在 %q(%d) 之后",
					s.Domain, s.Base, prev.Domain, prev.Base)
			}
			if s.Base < prev.Base+prev.Width {
				t.Fatalf("段重叠: %q [%d,%d) 与 %q [%d,%d)",
					prev.Domain, prev.Base, prev.Base+prev.Width,
					s.Domain, s.Base, s.Base+s.Width)
			}
		}
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
		{100000, "unknown"},
		{999, "unknown"},
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
