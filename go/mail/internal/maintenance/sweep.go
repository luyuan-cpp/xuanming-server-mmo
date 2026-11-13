package maintenance

import (
	"context"
	"errors"
	"github.com/zeromicro/go-zero/core/logx"
	"mail/internal/config"
	"mail/internal/data"
	"mail/internal/metrics"
	"math/rand/v2"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/workshard"
	"time"
)

type Config struct {
	Mode          string        `json:",default=report_only"`
	Interval      time.Duration `json:",default=5m"`
	RetentionDays int           `json:",default=7"`
	BatchLimit    int           `json:",default=1000"`
}

func (c Config) Defaults() Config {
	if c.Mode == "" {
		c.Mode = config.SweepModeReportOnly
	}
	if c.Interval == 0 {
		c.Interval = 5 * time.Minute
	}
	if c.RetentionDays == 0 {
		c.RetentionDays = 7
	}
	if c.BatchLimit == 0 {
		c.BatchLimit = 1000
	}
	return c
}
func (c Config) Validate() error {
	if (c.Mode != config.SweepModeReportOnly && c.Mode != config.SweepModeDelete) || c.Interval < time.Second || c.Interval > 24*time.Hour || c.RetentionDays < 1 || c.RetentionDays > 36500 || c.BatchLimit < 1 || c.BatchLimit > 10000 {
		return errors.New("Sweep模式/周期/保留期/批次无效")
	}
	return nil
}

// Sweeper 是过期清理用到的存储能力。*data.Repo 满足它;单测注入假实现,不连库。
type Sweeper interface {
	Sweep(ctx context.Context, cutoff uint64, limit int, deleteRows bool, owned workshard.Filter) (data.SweepReport, error)
}

var _ Sweeper = (*data.Repo)(nil)

// sweepPoint 是清理每一轮的 safego 点位名(safego_panic_total 的 label,必须是常量)。
const sweepPoint = "mail.expired_sweep"

// Run由主进程的同一个取消和等待链管理，不另起lifecycle/session主干。
//
// owns 是按片分工的归属判定(缺口清单第 25 条第四批,docs/design/assetop-redelivery-sharding-20261110.md §14.2):
// 生产传附件重投那份成员表的 Owns,片号 = mail_id % 64;nil 表示不分工、清全部片。归属每轮开头算一次,一轮里不变。
//
// 活性(mail_sweep_last_run_unixtime,同一设计 §14.3;口径在 shared/looplive 的包注释):起来时写一次;每一轮正常返回
// (成败都算)就写这一轮结束的时刻,panic 的一轮不写;ctx 结束、有意停止时删掉序列。值停住 = 循环卡在某一轮里,
// 或每一轮都 panic;分片之后它名下的片没人清,有告警 MailSweepStalled(按副本判)。
//
// 每一轮各自 recover(sweepTick):一轮 panic 只丢这一轮,循环接着跑。原来 panic 会一路抛出去把进程打死 —— 按片分工之后,
// 一封坏邮件会先打死负责它那一片的副本,片换主之后再打死下一个,整个邮件服务跟着倒。
func Run(ctx context.Context, repo Sweeper, c Config, owns func(key string) bool, admissions ...*drainproof.Session) {
	var root *drainproof.Session
	if len(admissions) > 1 {
		return
	}
	if len(admissions) == 1 {
		root = admissions[0]
		if root == nil {
			return
		}
	}
	c = c.Defaults()
	if err := c.Validate(); err != nil {
		logx.Error("[mail] invalid sweep configuration; cleanup not started")
		return
	}
	metrics.Register()
	metrics.SweepLive.Mark(time.Now())
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(c.Interval))))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		metrics.SweepLive.Stop()
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		sweepTick(ctx, repo, c, owns, root)
		select {
		case <-ctx.Done():
			metrics.SweepLive.Stop()
			return
		case <-ticker.C:
		}
	}
}

// sweepTick 是清理循环的一拍,返回这一轮是否正常跑完。sweepRound panic 时(它先打 HOLD 再重新抛出)由 Round 里的 safego 兜住:
// 不写活性时刻,计 safego_panic_total{point="mail.expired_sweep"},循环接着跑下一拍。
func sweepTick(ctx context.Context, repo Sweeper, c Config, owns func(key string) bool, root *drainproof.Session) bool {
	return metrics.SweepLive.Round(sweepPoint, func() { sweepRound(ctx, repo, c, owns, root) })
}

// sweepRound 跑一轮清理。开头按 owns 算出本轮负责的片。
//
// root 是排空 SOURCE(生产必带;nil 只给不带排空的单测):入口已关(停机排空)就跳过这一轮;删除模式下清理出错按结局未知
// 打 HOLD,闸门冻结除外(确定没写,下一轮接着清);panic 先打 HOLD 再抛给调用方。已经进了入口的一轮不随 ctx 取消:
// 它用自己的 30 秒预算跑完,停机不会把一笔删除截在半路、留下结局未知。
func sweepRound(ctx context.Context, repo Sweeper, c Config, owns func(key string) bool, root *drainproof.Session) {
	owned := workshard.SQLFilter(owns, data.SweepWorkKind, data.SweepShardColumn)
	if root == nil {
		_ = sweepOwned(ctx, repo, c, owned)
		return
	}
	finish, err := root.Enter()
	if err != nil {
		return
	}
	defer finish()
	defer func() {
		if recovered := recover(); recovered != nil {
			root.Hold()
			panic(recovered)
		}
	}()
	if err := sweepOwned(context.WithoutCancel(ctx), repo, c, owned); err != nil && c.Mode == config.SweepModeDelete && !authoritysql.IsWriterFrozen(err) {
		root.Hold()
	}
}

// sweepOwned 清 owned 那些片里保留期外的邮件,刷新 sweep 指标。单轮预算 30 秒(周期更短时取周期),跑不完下一轮接着清。
func sweepOwned(ctx context.Context, repo Sweeper, c Config, owned workshard.Filter) error {
	cutoff := time.Now().Add(-time.Duration(c.RetentionDays) * 24 * time.Hour).UnixMilli()
	if cutoff <= 0 {
		return nil
	}
	budget := 30 * time.Second
	if c.Interval < budget {
		budget = c.Interval
	}
	call, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	report, err := repo.Sweep(call, uint64(cutoff), c.BatchLimit, c.Mode == config.SweepModeDelete, owned)
	for _, entry := range []struct {
		name  string
		stats data.SweepStats
	}{{metrics.TablePersonal, report.Personal}, {metrics.TableSystem, report.System}, {metrics.TableSystemState, report.SystemState}} {
		metrics.SetSweepCandidateRows(entry.name, c.Mode, float64(entry.stats.Candidates))
		metrics.AddSweepDeleted(entry.name, float64(entry.stats.Deleted))
		metrics.AddSweepInvalidRows(entry.name, float64(entry.stats.Invalid))
	}
	if err != nil {
		logx.Errorf("[mail] bounded sweep failed mode=%s reason=%T", c.Mode, err)
	}
	return err
}
