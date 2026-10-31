package authority

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strconv"
	"strings"

	"match/internal/config"
)

// 不保存或打印密钥；同一事务域的撮合口径只准入一次，不让新副本覆盖旧策略。
// 全局运行配置已由go-zero补齐默认值；独立旧状态机的零值配置不能冒充生产策略。
func (s *Store) EnsureGlobalPolicy(ctx context.Context, c config.Config) error {
	if len(s.Secret) < 32 || c.GlobalMatch.CrossRegionWaitSeconds == 0 || c.GlobalMatch.HardLatencyMs == 0 || c.GlobalMatch.ProbeTTLSeconds == 0 || c.RatingToleranceBase <= 0 || c.RatingToleranceStepSeconds <= 0 || c.RatingToleranceStepDelta <= 0 || c.RatingToleranceMax <= 0 || c.RatingToleranceMaxWaitSeconds <= 0 || c.ChallengeTTLSeconds <= 0 {
		return errors.New("全局撮合策略未规范化")
	}
	parts := []string{"match-global-policy/v1", strconv.FormatUint(uint64(c.GlobalMatch.CrossRegionWaitSeconds), 10), strconv.FormatUint(uint64(c.GlobalMatch.HardLatencyMs), 10), strconv.FormatUint(uint64(c.GlobalMatch.ProbeTTLSeconds), 10), strconv.FormatBool(c.RatingEnabled), strconv.FormatInt(c.RatingToleranceBase, 10), strconv.FormatInt(c.RatingToleranceStepSeconds, 10), strconv.FormatInt(c.RatingToleranceStepDelta, 10), strconv.FormatInt(c.RatingToleranceMax, 10), strconv.FormatInt(c.RatingToleranceMaxWaitSeconds, 10), strconv.FormatInt(c.ChallengeTTLSeconds, 10)}
	parts = append(parts, "team-atomic-unit-home/v2", strconv.FormatBool(c.Team.AllowCrossZone), strconv.FormatBool(c.Team.FillQueueDisabled),
		strconv.Itoa(config.TeamCapacity), strconv.FormatUint(config.TeamApplicationTTLMs, 10), strconv.Itoa(config.TeamMaxApplications),
		strconv.FormatUint(config.TeamInviteTTLMs, 10), strconv.Itoa(config.TeamMaxInvitesPerTeam), strconv.Itoa(config.TeamMaxPendingInvitesPerInvitee), strconv.Itoa(config.TeamIdleTTLSeconds))
	parts = append(parts, "entry-authority/v1", c.GlobalMatch.AccountRoleSchema, c.GlobalMatch.GuildTrialSchema)
	// 范围树逐级放宽阈值是提交点规则（claimTx 用它复核），各副本必须一致。
	// 模板目录（PlayMode / Dungeon / ZoneGroup 表）不进指纹：改表滚动生效，不需要停全部副本。
	parts = append(parts, "scope-tree/v1", strconv.FormatUint(uint64(c.GlobalMatch.ScopeGroupAfterSeconds), 10), strconv.FormatUint(uint64(c.GlobalMatch.ScopeGlobalAfterSeconds), 10))
	mac := hmac.New(sha256.New, s.Secret)
	_, _ = mac.Write([]byte(strings.Join(parts, "\n")))
	digest := mac.Sum(nil)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT IGNORE INTO match_global_policy(policy_id,contract_digest) VALUES(1,?)", digest); err != nil {
		return err
	}
	var current []byte
	if err = tx.QueryRowContext(ctx, "SELECT contract_digest FROM match_global_policy WHERE policy_id=1 FOR UPDATE").Scan(&current); err != nil {
		return err
	}
	if !bytes.Equal(current, digest) {
		return errors.New("同一全局匹配事务域的策略不一致；须停全部副本后受控调整，禁止进程自动覆盖")
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.TeamConfig = c.Team
	return nil
}
