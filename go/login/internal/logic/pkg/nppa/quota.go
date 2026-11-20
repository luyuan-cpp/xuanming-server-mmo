package nppa

import (
	"context"
	"time"

	"shared/redisslot"

	"github.com/redis/go-redis/v9"
)

// QuotaRule 是一个接口的调用额度。规范限流是实名认证 100 QPS、结果查询 300 QPS、上下线上报 10 QPS,超出被封 1 分钟,
// 而且按运营单位算,与 login 有几个副本无关。
type QuotaRule struct {
	Name      string
	PerSecond int // 平均速率
	Burst     int // 允许一次连着多放的个数
}

// 只用到额定值的八成左右:任何 1 秒内的调用至多 PerSecond + Burst + 1 个,都留在额定值以下。
var (
	CheckQuota    = QuotaRule{Name: "check", PerSecond: 80, Burst: 10}
	QueryQuota    = QuotaRule{Name: "query", PerSecond: 240, Burst: 20}
	LoginoutQuota = QuotaRule{Name: "loginout", PerSecond: 8}
)

// gcraScript 是 GCRA:键里存理论到达时刻(微秒),按 Redis 服务器的时钟算,所以各副本的时钟差不影响结论。
// 放行条件是理论到达时刻不超过「现在 + 突发容量」;放行后理论到达时刻推后一个间隔。时刻用 %.0f 写回:
// 微秒数大于 Lua 默认的 14 位有效数字,直接传数字会被写成科学计数法、丢精度。
var gcraScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
local interval = tonumber(ARGV[1])
local tat = tonumber(redis.call('GET', KEYS[1]) or '0')
if tat < now then
  tat = now
end
if tat - now > tonumber(ARGV[2]) then
  return 0
end
local later = tat + interval
redis.call('SET', KEYS[1], string.format('%.0f', later), 'PX', math.ceil((later - now) / 1000) + 1000)
return 1
`)

// Quota 是所有 login 副本共用的国家系统调用配额,存在登录域 Redis 里。可并发使用。
type Quota struct {
	rdb redis.UniversalClient
}

func NewQuota(rdb redis.UniversalClient) *Quota {
	return &Quota{rdb: rdb}
}

// Allow 取一个调用名额。false = 这一刻的额度用完了;出错时调用方按没有名额处理(不调国家系统,宁可让玩家稍后重试,
// 也不冒被封 1 分钟的险)。
func (q *Quota) Allow(ctx context.Context, rule QuotaRule) (bool, error) {
	interval := int64(time.Second/time.Microsecond) / int64(rule.PerSecond)
	allowed, err := gcraScript.Run(ctx, q.rdb, []string{redisslot.NppaQuotaKey(rule.Name)}, interval, interval*int64(rule.Burst)).Int64()
	if err != nil {
		return false, err
	}
	return allowed == 1, nil
}
