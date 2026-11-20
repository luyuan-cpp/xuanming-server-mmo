package antiaddiction

import (
	"strings"
	"testing"
	"time"
)

func beijing(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, Beijing)
}

// 技术规范 V1.9 第六节的样例:前缀 1hpfml 是出生日期。
const specPI = "1hpfml09b57f3f8185f8cb5094ea3f26278efb"

func TestBirthDateDecodesSpecExample(t *testing.T) {
	got, ok := BirthDate(specPI)
	if !ok || !got.Equal(beijing(2010, time.January, 1, 0, 0)) {
		t.Fatalf("BirthDate(spec) = %v, %v; want 2010-01-01", got, ok)
	}
}

func TestBirthDateRejectsMalformed(t *testing.T) {
	for _, pi := range []string{
		"",
		specPI[:37],                            // 短一位
		specPI + "0",                           // 长一位
		"1HPFML09b57f3f8185f8cb5094ea3f26278efb", // 大写
		"zzzzzz09b57f3f8185f8cb5094ea3f26278efb", // 日期段超出 26 进制字符
		"00000009b57f3f8185f8cb5094ea3f26278efb", // 解出 0 年
		"1hpfml09b57f3f8185f8cb5094ea3f26278ef!", // 编码段非法字符
	} {
		if _, ok := BirthDate(pi); ok {
			t.Errorf("BirthDate(%q) accepted a malformed pi", pi)
		}
	}
}

func TestAgeAtTurnsOnBirthdayInBeijing(t *testing.T) {
	birth := beijing(2010, time.January, 1, 0, 0)
	if got := AgeAt(birth, beijing(2027, time.December, 31, 23, 59)); got != 17 {
		t.Fatalf("age the day before the 18th birthday = %d, want 17", got)
	}
	if got := AgeAt(birth, beijing(2028, time.January, 1, 0, 0)); got != 18 {
		t.Fatalf("age on the 18th birthday = %d, want 18", got)
	}
	// UTC 还是 12-31,北京已经是生日当天。
	if got := AgeAt(birth, time.Date(2027, time.December, 31, 16, 0, 0, 0, time.UTC)); got != 18 {
		t.Fatalf("age must follow the Beijing date, got %d", got)
	}
}

func TestSpendLimitAtFollowsThe2019Notice(t *testing.T) {
	cases := []struct {
		age  int
		want SpendLimit
	}{
		{7, SpendLimit{}},
		{8, SpendLimit{SingleFen: 5000, MonthlyFen: 20000}},
		{15, SpendLimit{SingleFen: 5000, MonthlyFen: 20000}},
		{16, SpendLimit{SingleFen: 10000, MonthlyFen: 40000}},
		{17, SpendLimit{SingleFen: 10000, MonthlyFen: 40000}},
		{18, SpendLimit{Unlimited: true}},
	}
	for _, c := range cases {
		if got := SpendLimitAt(c.age); got != c.want {
			t.Errorf("SpendLimitAt(%d) = %+v, want %+v", c.age, got, c.want)
		}
	}
}

func TestPlayDeadline(t *testing.T) {
	calendar, err := NewCalendar([]string{"2026-11-23"}) // 周一,当作节假日
	if err != nil {
		t.Fatal(err)
	}
	friday := beijing(2026, time.November, 20, 20, 30)
	deadline, ok := calendar.PlayDeadline(friday)
	if !ok || !deadline.Equal(beijing(2026, time.November, 20, 21, 0)) {
		t.Fatalf("Friday 20:30 = %v, %v; want 21:00 the same day", deadline, ok)
	}
	for _, closed := range []time.Time{
		beijing(2026, time.November, 20, 19, 59), // 还没到 20:00
		beijing(2026, time.November, 20, 21, 0),  // 21:00 整点已结束
		beijing(2026, time.November, 19, 20, 30), // 周四
		beijing(2026, time.November, 24, 20, 30), // 周二,不在名单
	} {
		if _, ok := calendar.PlayDeadline(closed); ok {
			t.Errorf("PlayDeadline(%v) must be closed", closed)
		}
	}
	if _, ok := calendar.PlayDeadline(beijing(2026, time.November, 23, 20, 0)); !ok {
		t.Fatal("an extra play day must open at 20:00")
	}
	if _, ok := (Calendar{}).PlayDeadline(beijing(2026, time.November, 22, 20, 0)); !ok {
		t.Fatal("the zero Calendar must still allow Sunday")
	}
}

func TestNewCalendarRejectsMalformedDays(t *testing.T) {
	for _, day := range []string{"2026-1-1", "2026/01/01", "2026-02-30", ""} {
		if _, err := NewCalendar([]string{day}); err == nil {
			t.Errorf("NewCalendar(%q) accepted a malformed day", day)
		}
	}
}

func TestValidIDNumber(t *testing.T) {
	now := beijing(2026, time.November, 19, 12, 0)
	for _, id := range []string{"11010519491231002X", "371321199012310916"} {
		if !ValidIDNumber(id, now) {
			t.Errorf("ValidIDNumber(%q) rejected a valid number", id)
		}
	}
	if !ValidIDNumber(NormalizeIDNumber(" 11010519491231002x "), now) {
		t.Error("NormalizeIDNumber must accept a lowercase check code and surrounding spaces")
	}
	for _, id := range []string{
		"371321199012310912", // 技术规范的加密样例,校验码不对
		"110105194912310021", // 校验码不对
		"110105209912310010", // 校验码对,出生日期在未来
		"110105201902300015", // 校验码对,2 月 30 日
		"11010519491231002",  // 17 位
		"1101051949123100A2", // 非数字
	} {
		if ValidIDNumber(id, now) {
			t.Errorf("ValidIDNumber(%q) accepted an invalid number", id)
		}
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"张三", "阿依古丽·买买提", " 李四 "} {
		if !ValidName(name) {
			t.Errorf("ValidName(%q) rejected a valid name", name)
		}
	}
	for _, name := range []string{"", "   ", "张\n三", strings.Repeat("张", 33)} {
		if ValidName(name) {
			t.Errorf("ValidName(%q) accepted an invalid name", name)
		}
	}
}
