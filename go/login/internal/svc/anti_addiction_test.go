package svc

import (
	"testing"
	"time"

	"shared/antiaddiction"
	"shared/generated/pb/table"
)

const testPISuffix = "09b57f3f8185f8cb5094ea3f26278efb"

var (
	testAdultPI = antiaddiction.PIBirthPrefix(1990, 1, 1) + testPISuffix
	testMinorPI = antiaddiction.PIBirthPrefix(2012, 6, 1) + testPISuffix
	// 2026-11-20 是周五。
	fridayEvening  = time.Date(2026, time.November, 20, 20, 30, 0, 0, antiaddiction.Beijing)
	fridayDeadline = time.Date(2026, time.November, 20, 21, 0, 0, 0, antiaddiction.Beijing)
	fridayNoon     = time.Date(2026, time.November, 20, 12, 0, 0, 0, antiaddiction.Beijing)
)

func testAntiAddiction() *AntiAddiction {
	return &AntiAddiction{PlatformManaged: map[string]bool{"wechat_minigame": true}}
}

// 部署没开防沉迷:什么都不判,应答里也不带状态。
func TestEvaluateOnADisabledDeploymentAdmitsEveryone(t *testing.T) {
	var disabled *AntiAddiction
	v := disabled.Evaluate("password", "", fridayNoon)
	if v.Status != nil || v.RejectTip != 0 || !v.Deadline.IsZero() || v.ReportPI != "" {
		t.Fatalf("verdict = %+v, want the zero verdict", v)
	}
}

func TestEvaluateByChannelAndAge(t *testing.T) {
	cases := []struct {
		name       string
		channel    string
		pi         string
		now        time.Time
		wantTip    table.LoginError
		wantNeeded bool      // 要先实名
		wantMinor  bool      //
		wantUntil  time.Time // 截止时刻
		wantReport string    // 由我们上报的 pi
	}{
		{name: "self-managed channel without a real name", channel: "password", now: fridayEvening,
			wantTip: table.LoginError_kLoginRealNameRequired, wantNeeded: true},
		{name: "platform-managed channel without a real name", channel: "wechat_minigame", now: fridayNoon},
		{name: "an unknown new channel is self-managed", channel: "some_new_channel", now: fridayNoon,
			wantTip: table.LoginError_kLoginRealNameRequired, wantNeeded: true},
		{name: "adult on a self-managed channel is reported", channel: "wechat", pi: testAdultPI, now: fridayNoon,
			wantReport: testAdultPI},
		{name: "adult with a real name on a platform channel is not reported", channel: "wechat_minigame", pi: testAdultPI, now: fridayNoon},
		{name: "minor inside the window", channel: "wechat", pi: testMinorPI, now: fridayEvening,
			wantMinor: true, wantUntil: fridayDeadline, wantReport: testMinorPI},
		{name: "minor outside the window", channel: "wechat", pi: testMinorPI, now: fridayNoon,
			wantTip: table.LoginError_kLoginMinorNotInPlayTime, wantMinor: true, wantReport: testMinorPI},
		// 账号上有实名时,走平台渠道也按我们的规则限,但上下线由平台报。
		{name: "known minor on a platform channel is still limited", channel: "wechat_minigame", pi: testMinorPI, now: fridayNoon,
			wantTip: table.LoginError_kLoginMinorNotInPlayTime, wantMinor: true},
		{name: "known minor on a platform channel inside the window", channel: "wechat_minigame", pi: testMinorPI, now: fridayEvening,
			wantMinor: true, wantUntil: fridayDeadline},
		// 库里的 pi 解不出出生日期:按故障拒绝,不当成成年人放行。
		{name: "corrupt pi fails closed", channel: "wechat", pi: "not-a-pi", now: fridayNoon,
			wantTip: table.LoginError_kLoginRealNameServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := testAntiAddiction().Evaluate(c.channel, c.pi, c.now)
			if v.Status == nil {
				t.Fatal("an enabled deployment always returns a status")
			}
			if v.RejectTip != uint32(c.wantTip) {
				t.Errorf("RejectTip = %d, want %d", v.RejectTip, uint32(c.wantTip))
			}
			if v.Status.RealNameRequired != c.wantNeeded || v.Status.Minor != c.wantMinor {
				t.Errorf("status = %+v, want real_name_required=%v minor=%v", v.Status, c.wantNeeded, c.wantMinor)
			}
			if !v.Deadline.Equal(c.wantUntil) {
				t.Errorf("Deadline = %v, want %v", v.Deadline, c.wantUntil)
			}
			wantMs := uint64(0)
			if !c.wantUntil.IsZero() {
				wantMs = uint64(c.wantUntil.UnixMilli())
			}
			if v.Status.PlayDeadlineMs != wantMs {
				t.Errorf("PlayDeadlineMs = %d, want %d", v.Status.PlayDeadlineMs, wantMs)
			}
			if v.ReportPI != c.wantReport {
				t.Errorf("ReportPI = %q, want %q", v.ReportPI, c.wantReport)
			}
		})
	}
}

// 满 18 周岁当天起不再受限。
func TestEvaluateTreatsThe18thBirthdayAsAdult(t *testing.T) {
	pi := antiaddiction.PIBirthPrefix(2008, 11, 20) + testPISuffix
	if v := testAntiAddiction().Evaluate("wechat", pi, fridayNoon); v.RejectTip != 0 || v.Status.Minor {
		t.Fatalf("on the 18th birthday: %+v, want an adult verdict", v)
	}
	dayBefore := time.Date(2026, time.November, 19, 12, 0, 0, 0, antiaddiction.Beijing)
	if v := testAntiAddiction().Evaluate("wechat", pi, dayBefore); !v.Status.Minor {
		t.Fatalf("the day before the 18th birthday: %+v, want a minor verdict", v)
	}
}

func TestNowUsesTheInjectedClock(t *testing.T) {
	a := &AntiAddiction{Clock: func() time.Time { return fridayNoon }}
	if !a.Now().Equal(fridayNoon) {
		t.Fatal("Now must use Clock when it is set")
	}
	var disabled *AntiAddiction
	if disabled.Now().IsZero() {
		t.Fatal("Now on a disabled deployment falls back to the wall clock")
	}
}
