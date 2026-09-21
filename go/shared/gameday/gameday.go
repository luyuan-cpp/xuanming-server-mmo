// Package gameday 定义全服统一的"游戏日"与"游戏周":UTC+8、每日 05:00 切日;
// 周按 ISO 周编号、周一 05:00 切周。它只回答"这个时刻属于哪一天 / 哪一周、下一次切点是什么时候"。
//
// 为什么单独成包:每日次数、每周限购、活动个人次数都按同一个切点重置(用户决策 U1)。
// 切点各写一份,迟早有一处写成 00:00 或写成本地时区,玩家会在某个系统里"多领一次"而没有任何报错。
// 首个使用者是帮会经济(捐献次数、商店限购),帮会活动(B6)复用。
//
// 边界(故意做得很窄):
//   - 不读墙钟:所有函数以 time.Time 入参。调用方一次请求只取一次 now,周期键与各时间戳用同一个值,
//     否则跨切点的请求会把"占用次数"与"展示的重置时刻"算进两个不同的周期。
//   - 固定时区,不依赖 tzdata(容器镜像里可能没有),也没有夏令时。
//   - 不连库、不读配置、不打日志。05:00 与 UTC+8 是全服契约而不是策划数值:
//     改它会让已落库的周期键整体错位,所以不进配表。
//
// 时钟口径:取服务进程时钟。各副本 NTP 偏差须 < 1s;跨切点 1s 内的请求可能记到相邻周期,可接受。
//
// 设计:docs/design/guild-phase2/05-economy.md §5.14。
package gameday

import "time"

// ResetHour 是切日的小时数(Zone 时区)。
const ResetHour = 5

// Zone 是游戏日所在的固定时区。只读,不得重新赋值(Go 没有 *time.Location 常量,与 time.UTC 同一种惯例)。
var Zone = time.FixedZone("UTC+8", 8*3600)

// shifted 把时刻平移到"05:00 即零点"的坐标系:平移后的自然日 / ISO 周就是游戏日 / 游戏周。
func shifted(t time.Time) time.Time {
	return t.In(Zone).Add(-ResetHour * time.Hour)
}

// DayKey 返回 t 所属游戏日的 8 位键 YYYYMMDD(如 20260916)。05:00 之前算前一天。
func DayKey(t time.Time) uint32 {
	y, m, d := shifted(t).Date()
	return uint32(y*10000 + int(m)*100 + d)
}

// WeekKey 返回 t 所属游戏周的 6 位键 YYYYWW(如 202638)。
//
// 年份必须取 ISOWeek 返回的那个年,不能取自然年:2027-01-01 属于 2026 年第 53 周,键是 202653。
// 写成自然年会得到 202753,比下一周的 202701 还大 —— 键不再随时间单调,
// 而按"键 <= 截止键"做范围删除的清理任务会误判。
func WeekKey(t time.Time) uint32 {
	y, w := shifted(t).ISOWeek()
	return uint32(y*100 + w)
}

// NextDailyReset 返回严格晚于 t 的下一个切日时刻(Zone 时区的 05:00)。t 恰在 05:00:00 时返回次日 05:00。
func NextDailyReset(t time.Time) time.Time {
	y, m, d := shifted(t).Date()
	// 平移坐标系里"明天零点"就是真实时间的下一个 05:00;日期溢出由 time.Date 归一化。
	return time.Date(y, m, d+1, ResetHour, 0, 0, 0, Zone)
}

// NextWeeklyReset 返回严格晚于 t 的下一个切周时刻(Zone 时区的周一 05:00)。t 恰在周一 05:00:00 时返回下周一。
func NextWeeklyReset(t time.Time) time.Time {
	s := shifted(t)
	y, m, d := s.Date()
	daysSinceMonday := (int(s.Weekday()) + 6) % 7 // time.Sunday = 0 → 6;time.Monday = 1 → 0
	return time.Date(y, m, d+7-daysSinceMonday, ResetHour, 0, 0, 0, Zone)
}

// 限购周期的取值,与配表 GuildShop.limit_period 的数值一致。
const (
	PeriodNone   uint32 = 0 // 不限:不占计数行
	PeriodDaily  uint32 = 1 // 每游戏日
	PeriodWeekly uint32 = 2 // 每游戏周
)

// PeriodKey 按限购周期取周期键:PeriodNone → (0, true),0 表示"不占计数行";
// PeriodDaily → DayKey;PeriodWeekly → WeekKey;其它值 → (0, false),调用方应当作配置错误拒绝。
//
// DayKey(8 位)与 WeekKey(6 位)的数值域不相交,清理任务靠这一点分别删日键行与周键行。
func PeriodKey(limitPeriod uint32, t time.Time) (key uint32, ok bool) {
	switch limitPeriod {
	case PeriodNone:
		return 0, true
	case PeriodDaily:
		return DayKey(t), true
	case PeriodWeekly:
		return WeekKey(t), true
	default:
		return 0, false
	}
}
