// Package realname 是游戏方自己做的实名:本地预检、每日次数、调国家系统、把 pi 写进账号目录
// (docs/design/real-name-anti-addiction.md §5.1、§5.2)。姓名和身份证号只在 Submit 里经手,不存、不进日志。
package realname

import (
	"context"
	"errors"
	"strings"
	"time"

	"login/internal/logic/pkg/nppa"
	"shared/antiaddiction"
	"shared/generated/pb/table"
	"shared/redisslot"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// Verifier 是国家系统的认证与查询,*nppa.Client 实现它。
type Verifier interface {
	Check(ctx context.Context, ai, name, idNumber string) (nppa.AuthResult, error)
	Query(ctx context.Context, ai string) (nppa.AuthResult, error)
}

// Directory 是账号目录上的实名写:只写一次,返回写之后库里的 pi。
type Directory interface {
	SetRealName(ctx context.Context, account, pi string) (string, error)
}

// Quota 是所有 login 副本共用的国家系统调用额度,*nppa.Quota 实现它。超了额会被系统封 1 分钟,所以取不到名额就不调。
type Quota interface {
	Allow(ctx context.Context, rule nppa.QuotaRule) (bool, error)
}

// pendingTTL 是认证中的结果可查的时长(技术规范:48 小时内可以查询)。
const pendingTTL = 48 * time.Hour

// attemptsTTL 让按日期计的次数键在当天过去之后自己消失。
const attemptsTTL = 25 * time.Hour

// countAttemptScript 计一次提交并在第一次时设过期;INCR 不能进管道(clusterredis 守卫),所以用脚本。
var countAttemptScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then
  redis.call('EXPIRE', KEYS[1], ARGV[1])
end
return n
`)

// Service 可并发使用。
type Service struct {
	Redis      redis.UniversalClient // 登录域
	Verifier   Verifier
	Directory  Directory
	Quota      Quota
	DailyLimit int
	Now        func() time.Time
}

// allow 取一个国家系统的调用名额;取不到或额度读不了都按没有名额处理。
func (s *Service) allow(ctx context.Context, rule nppa.QuotaRule) bool {
	ok, err := s.Quota.Allow(ctx, rule)
	if err != nil {
		logx.Errorf("[real-name] NPPA quota unavailable: api=%s err=%v", rule.Name, err)
	}
	return ok
}

// Submit 提交一次实名。成功返回库里的 pi 与 0;否则返回 tip(导表码):信息有误、认证中、次数用完、已绑定别的实名、服务故障。
// 账号已经绑定了别的实名时,返回库里的那个 pi 与 kLoginRealNameAlreadyBound。
func (s *Service) Submit(ctx context.Context, account, name, idNumber string) (string, uint32) {
	name = strings.TrimSpace(name)
	idNumber = antiaddiction.NormalizeIDNumber(idNumber)
	if !antiaddiction.ValidName(name) || !antiaddiction.ValidIDNumber(idNumber, s.Now()) {
		return "", uint32(table.LoginError_kLoginRealNameInvalid)
	}
	// 先取全服的调用名额,再计账号当天的次数:额度紧时回「繁忙」,不占玩家的提交次数。
	if !s.allow(ctx, nppa.CheckQuota) {
		return "", uint32(table.LoginError_kLoginRealNameServiceUnavailable)
	}
	attempts, err := countAttemptScript.Run(ctx, s.Redis,
		[]string{redisslot.RealNameAttemptsKey(account, s.Now().In(antiaddiction.Beijing).Format(time.DateOnly))},
		int(attemptsTTL/time.Second)).Int64()
	if err != nil {
		logx.Errorf("[real-name] count submit attempt failed: account=%s err=%v", account, err)
		return "", uint32(table.LoginError_kLoginRealNameServiceUnavailable)
	}
	if attempts > int64(s.DailyLimit) {
		return "", uint32(table.LoginError_kLoginRealNameTooManyAttempts)
	}
	ai, err := nppa.NewID() // 没被删除的 ai 不能再提交,所以每次提交都新生成
	if err != nil {
		logx.Errorf("[real-name] generate ai failed: %v", err)
		return "", uint32(table.LoginError_kLoginRealNameServiceUnavailable)
	}
	result, err := s.Verifier.Check(ctx, ai, name, idNumber)
	switch {
	case errors.Is(err, nppa.ErrInvalidIdentity):
		return "", uint32(table.LoginError_kLoginRealNameInvalid)
	case err != nil:
		logx.Errorf("[real-name] NPPA check failed: account=%s err=%v", account, err)
		return "", uint32(table.LoginError_kLoginRealNameServiceUnavailable)
	}
	switch result.Status {
	case nppa.AuthOK:
		return s.bind(ctx, account, result.PI)
	case nppa.AuthPending:
		if err := s.Redis.Set(ctx, redisslot.RealNamePendingKey(account), ai, pendingTTL).Err(); err != nil {
			// 记不下 ai 就查不到结果,玩家只能重新提交;照样回认证中。
			logx.Errorf("[real-name] remember pending ai failed: account=%s err=%v", account, err)
		}
		return "", uint32(table.LoginError_kLoginRealNamePending)
	default:
		return "", uint32(table.LoginError_kLoginRealNameInvalid)
	}
}

// ResolvePending 收尾认证中的提交:查到成功就绑定并返回库里的 pi;还在认证中、没有待查的、查询或绑定失败都返回空,
// 下一次登录再查。查询成功的结果 300 秒后会被系统删掉,所以绑定失败时留着 ai 重试,过了时限会查到「没有记录」再清掉。
func (s *Service) ResolvePending(ctx context.Context, account string) string {
	key := redisslot.RealNamePendingKey(account)
	ai, err := s.Redis.Get(ctx, key).Result()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			logx.Errorf("[real-name] read pending ai failed: account=%s err=%v", account, err)
		}
		return ""
	}
	if !s.allow(ctx, nppa.QueryQuota) {
		return "" // 下一次登录再查
	}
	result, err := s.Verifier.Query(ctx, ai)
	switch {
	case errors.Is(err, nppa.ErrNoRecord):
		s.forgetPending(ctx, key)
		return ""
	case err != nil:
		logx.Errorf("[real-name] NPPA query failed: account=%s err=%v", account, err)
		return ""
	}
	switch result.Status {
	case nppa.AuthPending:
		return ""
	case nppa.AuthOK:
		pi, tip := s.bind(ctx, account, result.PI)
		if tip == uint32(table.LoginError_kLoginRealNameServiceUnavailable) {
			return ""
		}
		s.forgetPending(ctx, key)
		return pi
	default:
		s.forgetPending(ctx, key)
		return ""
	}
}

func (s *Service) bind(ctx context.Context, account, pi string) (string, uint32) {
	stored, err := s.Directory.SetRealName(ctx, account, pi)
	if err != nil {
		logx.Errorf("[real-name] bind real name to account directory failed: account=%s err=%v", account, err)
		return "", uint32(table.LoginError_kLoginRealNameServiceUnavailable)
	}
	if stored != pi {
		return stored, uint32(table.LoginError_kLoginRealNameAlreadyBound)
	}
	return stored, 0
}

func (s *Service) forgetPending(ctx context.Context, key string) {
	if err := s.Redis.Del(ctx, key).Err(); err != nil {
		logx.Errorf("[real-name] clear pending ai failed: err=%v", err)
	}
}
