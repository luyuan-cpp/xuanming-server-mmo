package svc

import (
	"context"
	"fmt"
	"time"

	"login/internal/config"
	"login/internal/logic/pkg/nppa"
	"login/internal/logic/pkg/realname"
	dspb "proto/data_service"
	login_proto "proto/login"
	"shared/antiaddiction"
	"shared/generated/pb/table"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// RealNameDirectory 是账号目录上的实名读写,*accountdirectory.Client 实现它(docs/design/real-name-anti-addiction.md §5.1)。
type RealNameDirectory interface {
	// ReadWithLifecycle 与 Directory.Read 是同一次目录读,应答另带账号的 real_name_pi(空串 = 没实名)与注销状态。
	ReadWithLifecycle(ctx context.Context, account string, create bool) (*dspb.AccountDirectoryResponse, error)
	SetRealName(ctx context.Context, account, pi string) (string, error)
}

// RealNames 返回账号目录的实名读写;目录实现不支持时(测试替身)ok=false。
func (s *ServiceContext) RealNames() (RealNameDirectory, bool) {
	realNames, ok := s.AccountDirectory.(RealNameDirectory)
	return realNames, ok
}

// AntiAddiction 是 login 里实名与防沉迷用到的依赖(docs/design/real-name-anti-addiction.md §5)。部署没开防沉迷时
// ServiceContext.AntiAddiction 为 nil,Evaluate 在 nil 上回放行。
type AntiAddiction struct {
	Calendar antiaddiction.Calendar
	Reporter *nppa.Reporter
	RealName *realname.Service
	// PlatformManaged 是由渠道平台实名并执行防沉迷的 auth_type。
	PlatformManaged map[string]bool
}

// SelfManaged 报告这个渠道(签发令牌时的 auth_type)是否要游戏方自己实名、执行限制并上报。
func (a *AntiAddiction) SelfManaged(authType string) bool {
	return !a.PlatformManaged[authType]
}

// Verdict 是一次登录按防沉迷规则的判定(§4、§5.3)。
type Verdict struct {
	Status    *login_proto.AntiAddictionStatus // nil = 部署没开防沉迷
	RejectTip uint32                           // 非 0 = gate 上的 Login 拒绝绑定
	Deadline  time.Time                        // 未成年人本次能玩到的时刻;零值 = 不限
	ReportPI  string                           // 非空 = 这次会话由我们上报上下线
}

// Evaluate 按本次登录的渠道与账号的 pi 判定。账号已有 pi 时,任何渠道都按我们的规则限(§4 最后一条)。
// pi 解不出出生日期是库里的数据坏了:按故障拒绝,不按成年人放行。
func (a *AntiAddiction) Evaluate(channel, pi string, now time.Time) Verdict {
	if a == nil {
		return Verdict{}
	}
	v := Verdict{Status: &login_proto.AntiAddictionStatus{}}
	selfManaged := a.SelfManaged(channel)
	if pi == "" {
		if selfManaged {
			v.Status.RealNameRequired = true
			v.RejectTip = uint32(table.LoginError_kLoginRealNameRequired)
		}
		return v
	}
	birth, ok := antiaddiction.BirthDate(pi)
	if !ok {
		v.RejectTip = uint32(table.LoginError_kLoginRealNameServiceUnavailable)
		return v
	}
	if selfManaged {
		v.ReportPI = pi
	}
	if antiaddiction.AgeAt(birth, now) >= antiaddiction.AdultAge {
		return v
	}
	v.Status.Minor = true
	deadline, open := a.Calendar.PlayDeadline(now)
	if !open {
		v.RejectTip = uint32(table.LoginError_kLoginMinorNotInPlayTime)
		return v
	}
	v.Deadline = deadline
	v.Status.PlayDeadlineMs = uint64(deadline.UnixMilli())
	return v
}

// NewAntiAddiction 按已经核过的配置建好依赖(配置门禁在 config.ResolveSecrets 里,早于任何外部连接)。
func NewAntiAddiction(c config.AntiAddictionConf, secretKey string, s *ServiceContext) (*AntiAddiction, error) {
	calendar, err := antiaddiction.NewCalendar(c.ExtraPlayDays)
	if err != nil {
		return nil, err
	}
	client, err := nppa.NewClient(nppa.Config{
		AppID: c.Nppa.AppId, BizID: c.Nppa.BizId, SecretKey: secretKey,
		CheckURL: c.Nppa.CheckUrl, QueryURL: c.Nppa.QueryUrl, LoginoutURL: c.Nppa.LoginoutUrl,
	}, nil)
	if err != nil {
		return nil, err
	}
	realNames, ok := s.RealNames()
	if !ok {
		return nil, fmt.Errorf("the account directory client does not support real names")
	}
	managed := make(map[string]bool, len(c.PlatformManagedAuthTypes))
	for _, authType := range c.PlatformManagedAuthTypes {
		managed[authType] = true
	}
	return &AntiAddiction{
		Calendar: calendar,
		Reporter: nppa.NewReporter(client),
		RealName: &realname.Service{
			Redis: s.LoginRedis, Verifier: client, Directory: realNames, DailyLimit: c.DailySubmitLimit, Now: time.Now,
		},
		PlatformManaged: managed,
	}, nil
}

func (s *ServiceContext) initAntiAddiction() {
	c := config.AppConfig.AntiAddiction
	if !c.Enabled {
		return
	}
	secretKey := ""
	if config.ActiveSecrets != nil {
		secretKey = config.ActiveSecrets.NppaSecretKey
	}
	aa, err := NewAntiAddiction(c, secretKey, s)
	if err != nil {
		panic(fmt.Errorf("anti-addiction configuration rejected; refusing to start: %w", err))
	}
	s.AntiAddiction = aa
	logx.Infof("[anti-addiction] enabled: platform_managed_auth_types=%v extra_play_days=%d daily_submit_limit=%d",
		c.PlatformManagedAuthTypes, len(c.ExtraPlayDays), c.DailySubmitLimit)
}

// startAntiAddictionReporter 起上下线上报循环(每个 login 副本一份,见 nppa.Reporter)。
func (s *ServiceContext) startAntiAddictionReporter() {
	if s.AntiAddiction == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.antiAddictionStop = cancel
	reporter := s.AntiAddiction.Reporter
	s.background.Add(1)
	safego.Go("login.nppa_reporter", func() {
		defer s.background.Done()
		reporter.Run(ctx)
	})
}
