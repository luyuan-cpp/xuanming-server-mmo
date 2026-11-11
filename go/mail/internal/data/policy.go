package data

import "errors"

// Limits 只控制容量与有效期，不改变签名、幂等、冻结名单或资产领取语义。
type Limits struct {
	MaxPersonalMailsPerPlayer uint32 `json:",default=200"`
	MaxActiveSystemMails      uint32 `json:",default=100"`
	MaxPersonalMailTtlDays    uint32 `json:",default=30"`
	MaxSystemMailTtlDays      uint32 `json:",default=30"`
	// MaxActiveGuildMailsPerGuild 是一个帮会同时有效的帮会邮件(文本与活动)上限,按帮计数(缺口清单第 27 条)。
	// 帮会邮件只对本帮成员可见,不进别人的收件箱查询,所以不计入全服共用的 MaxActiveSystemMails:
	// 帮会多了不会把全服系统邮件挤满,一个帮发满也只挡住它自己。
	MaxActiveGuildMailsPerGuild uint32 `json:",default=20"`
}

func (l Limits) Defaults() Limits {
	if l.MaxPersonalMailsPerPlayer == 0 {
		l.MaxPersonalMailsPerPlayer = 200
	}
	if l.MaxActiveSystemMails == 0 {
		l.MaxActiveSystemMails = 100
	}
	if l.MaxPersonalMailTtlDays == 0 {
		l.MaxPersonalMailTtlDays = 30
	}
	if l.MaxSystemMailTtlDays == 0 {
		l.MaxSystemMailTtlDays = 30
	}
	if l.MaxActiveGuildMailsPerGuild == 0 {
		l.MaxActiveGuildMailsPerGuild = 20
	}
	return l
}
func (l Limits) Validate() error {
	if l.MaxPersonalMailsPerPlayer < 1 || l.MaxPersonalMailsPerPlayer > 1000 || l.MaxActiveSystemMails < 1 || l.MaxActiveSystemMails > 500 || l.MaxPersonalMailTtlDays < 1 || l.MaxPersonalMailTtlDays > 365 || l.MaxSystemMailTtlDays < 1 || l.MaxSystemMailTtlDays > 365 {
		return errors.New("Mail容量或TTL超出允许范围")
	}
	if l.MaxActiveGuildMailsPerGuild < 1 || l.MaxActiveGuildMailsPerGuild > 100 {
		return errors.New("Mail.MaxActiveGuildMailsPerGuild must be within [1,100]")
	}
	return nil
}

// ConfigureLimits 只在暴露服务之前调用；运行期不可并发换限额。
func (r *Repo) ConfigureLimits(l Limits) error {
	l = l.Defaults()
	if err := l.Validate(); err != nil {
		return err
	}
	r.limits = l
	return nil
}
