package maintenance

import (
	"context"
	"errors"
	"github.com/zeromicro/go-zero/core/logx"
	"mail/internal/config"
	"mail/internal/data"
	"mail/internal/metrics"
	"math/rand/v2"
	"shared/drainproof"
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

// Run由主进程的同一个取消和等待链管理，不另起lifecycle/session主干。
func Run(ctx context.Context, repo *data.Repo, c Config, admissions ...*drainproof.Session) {
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
		logx.Error("[mail] sweep配置无效，未启动清理")
		return
	}
	metrics.Register()
	metrics.MarkSweepRun(time.Now())
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(c.Interval))))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		if root == nil {
			round(ctx, repo, c)
		} else {
			finish, err := root.Enter()
			if err != nil && !errors.Is(err, drainproof.ErrClosed) {
				return
			}
			if err == nil {
				func() {
					defer finish()
					defer func() {
						if recovered := recover(); recovered != nil {
							root.Hold()
							panic(recovered)
						}
					}()
					if err := sweepRound(context.WithoutCancel(ctx), repo, c); err != nil && c.Mode == config.SweepModeDelete {
						root.Hold()
					}
				}()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func round(ctx context.Context, repo *data.Repo, c Config) { _ = sweepRound(ctx, repo, c) }

func sweepRound(ctx context.Context, repo *data.Repo, c Config) error {
	defer metrics.MarkSweepRun(time.Now())
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
	report, err := repo.Sweep(call, uint64(cutoff), c.BatchLimit, c.Mode == config.SweepModeDelete)
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
