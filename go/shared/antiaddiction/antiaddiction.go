// Package antiaddiction 是未成年人防沉迷的纯规则:从国家系统的 pi 解出生日期、算周岁、可玩时段、充值档位、
// 身份证号与姓名的本地预检(docs/design/real-name-anti-addiction.md §1、§2、§5)。没有 I/O,login 与 payment 共用。
package antiaddiction

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Beijing 是所有规则用的时区。中国不实行夏令时,用固定偏移,不依赖容器里的时区库。
var Beijing = time.FixedZone("CST", 8*3600)

// AdultAge 是成年的周岁。
const AdultAge = 18

// PILength 是国家系统用户唯一标识 pi 的长度:前 6 位出生日期,后 32 位系统编码(技术规范 V1.9 第六节)。
const PILength = 38

// piDigits 是出生日期段的 26 进制字符:十个数字加字母表前 16 个字母。
const piDigits = "0123456789abcdefghijklmnop"

// BirthDate 从 pi 前 6 位解出出生日期(北京日期的零点)。长度不对、含 0-9a-z 以外的字符、日期不合法时 ok=false。
func BirthDate(pi string) (time.Time, bool) {
	if len(pi) != PILength {
		return time.Time{}, false
	}
	for i := 0; i < len(pi); i++ {
		c := pi[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') {
			return time.Time{}, false
		}
	}
	value := 0
	for i := 0; i < 6; i++ {
		digit := strings.IndexByte(piDigits, pi[i])
		if digit < 0 {
			return time.Time{}, false
		}
		value = value*26 + digit
	}
	return dateOf(value/10000, value/100%100, value%100)
}

// ValidPI 报告 pi 的格式和出生日期段是否合法。
func ValidPI(pi string) bool {
	_, ok := BirthDate(pi)
	return ok
}

// AgeAt 返回 now 时的周岁,按北京日期算:生日当天满一岁。
func AgeAt(birth, now time.Time) int {
	n := now.In(Beijing)
	age := n.Year() - birth.Year()
	if n.Month() < birth.Month() || (n.Month() == birth.Month() && n.Day() < birth.Day()) {
		age--
	}
	return age
}

// SpendLimit 是一个年龄段的充值上限,单位分(国新出发〔2019〕34 号)。Unlimited 为真时两个上限都不适用;
// 否则 SingleFen 为 0 表示不得充值。
type SpendLimit struct {
	Unlimited  bool
	SingleFen  uint64
	MonthlyFen uint64
}

// SpendLimitAt 返回该周岁的充值上限:未满 8 周岁不得充值;8–16 单次 50 元、每月 200 元;16–18 单次 100 元、每月 400 元。
func SpendLimitAt(age int) SpendLimit {
	switch {
	case age >= AdultAge:
		return SpendLimit{Unlimited: true}
	case age >= 16:
		return SpendLimit{SingleFen: 100_00, MonthlyFen: 400_00}
	case age >= 8:
		return SpendLimit{SingleFen: 50_00, MonthlyFen: 200_00}
	default:
		return SpendLimit{}
	}
}

// 未成年人只能在可玩日的 20:00–21:00 玩(国新出发〔2021〕14 号)。规则由法规定,写成常量。
const (
	playStartHour = 20
	playEndHour   = 21
)

// Calendar 是未成年人的可玩日:周五、周六、周日,加上额外的北京日期(法定节假日中不落在周五到周日的那些天,
// 运维按每年的国务院放假通知填)。零值只有周五到周日:名单漏填只会更严,不会违规。
type Calendar struct {
	extra map[string]struct{}
}

// NewCalendar 解析额外可玩日,格式 2006-01-02;有一个不合法就报错,调用方拒绝启动。
func NewCalendar(extraDays []string) (Calendar, error) {
	extra := make(map[string]struct{}, len(extraDays))
	for _, day := range extraDays {
		parsed, err := time.ParseInLocation(time.DateOnly, day, Beijing)
		if err != nil || parsed.Format(time.DateOnly) != day {
			return Calendar{}, fmt.Errorf("invalid extra play day %q: want YYYY-MM-DD", day)
		}
		extra[day] = struct{}{}
	}
	return Calendar{extra: extra}, nil
}

// PlayDeadline 返回未成年人此刻能玩到的时刻;此刻不在可玩时段时 ok=false。
func (c Calendar) PlayDeadline(now time.Time) (time.Time, bool) {
	n := now.In(Beijing)
	if !c.playDay(n) {
		return time.Time{}, false
	}
	start := time.Date(n.Year(), n.Month(), n.Day(), playStartHour, 0, 0, 0, Beijing)
	end := time.Date(n.Year(), n.Month(), n.Day(), playEndHour, 0, 0, 0, Beijing)
	if n.Before(start) || !n.Before(end) {
		return time.Time{}, false
	}
	return end, true
}

func (c Calendar) playDay(n time.Time) bool {
	switch n.Weekday() {
	case time.Friday, time.Saturday, time.Sunday:
		return true
	}
	_, ok := c.extra[n.Format(time.DateOnly)]
	return ok
}

var idWeights = [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}

// idCheckCodes 按加权和除以 11 的余数取校验码(GB 11643)。
const idCheckCodes = "10X98765432"

// NormalizeIDNumber 去掉首尾空白,校验码的 x 写成大写 X。
func NormalizeIDNumber(id string) string {
	return strings.ToUpper(strings.TrimSpace(id))
}

// ValidIDNumber 核 18 位居民身份证号的字符、校验码与出生日期(合法且不晚于 now)。只挡明显填错的,真伪以国家系统为准。
// 传入前先 NormalizeIDNumber。
func ValidIDNumber(id string, now time.Time) bool {
	if len(id) != 18 {
		return false
	}
	sum := 0
	for i := 0; i < 17; i++ {
		c := id[i]
		if c < '0' || c > '9' {
			return false
		}
		sum += int(c-'0') * idWeights[i]
	}
	if idCheckCodes[sum%11] != id[17] {
		return false
	}
	year, _ := strconv.Atoi(id[6:10])
	month, _ := strconv.Atoi(id[10:12])
	day, _ := strconv.Atoi(id[12:14])
	birth, ok := dateOf(year, month, day)
	return ok && !birth.After(now.In(Beijing))
}

// ValidName 只挡明显不对的姓名:去掉首尾空白后 1–32 个字符,不含控制字符。真伪以国家系统为准。
func ValidName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 32 {
		return false
	}
	return strings.IndexFunc(name, unicode.IsControl) < 0
}

func dateOf(year, month, day int) (time.Time, bool) {
	t := time.Date(year, time.Month(month), day, 0, 0, 0, 0, Beijing)
	if year < 1900 || t.Year() != year || int(t.Month()) != month || t.Day() != day {
		return time.Time{}, false
	}
	return t, true
}
