package authority

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"match/internal/config"
	pb "proto/match"
	teampb "proto/team"
	"schemamigrate"
	"shared/authoritysql"
	"shared/dbplacement"
	"shared/rolefence"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
)

// Tables 是匹配、组队权威的唯一迁移清单；DDL 继续由正式 proto 描述生成。
func Tables() []proto.Message {
	return []proto.Message{
		&pb.MatchSpectatorRecord{}, &pb.MatchSpectatorIntentRecord{}, &pb.MatchSpectatorHeadRecord{},
		&pb.MatchNetworkProfileRecord{}, &pb.MatchAuthorityTicketRecord{},
		&pb.MatchChallengeInvitationRecord{}, &pb.MatchGlobalPolicyRecord{}, &pb.MatchJoinIntentRecord{},
		&pb.MatchDirectIntentRecord{}, &pb.MatchFormationRecord{}, &pb.MatchRatingRecord{},
		&pb.MatchRatingAppliedRecord{}, &pb.MatchRankSourceRecord{}, &pb.MatchRankOutboxRecord{},
		&pb.MatchBattleCompletionRecord{}, &pb.MatchBattleSettlementSourceRecord{}, &pb.MatchRoleOccupancyRecord{},
		&pb.MatchManualResolutionRecord{}, // 人工处置审计(2026-11-05,manual_abort.go)
		&pb.MatchBattleReportRecord{}, &pb.MatchPlayerBattleReportRecord{}, // 战报回放(battle_report.go)
		&pb.MatchBattleScriptRecord{}, &pb.MatchBattleScriptReviewRecord{}, &pb.MatchBattleScriptLoadoutRecord{}, // 战斗剧本(battle_script.go)
		&teampb.TeamAuthorityRecord{}, &teampb.TeamMembershipRecord{}, &teampb.TeamInviteIndexRecord{},
		&teampb.TeamActionReceiptRecord{}, &teampb.TeamLaunchRecord{},
		&teampb.TeamApplicationIndexRecord{}, &teampb.TeamLaunchRoleRecord{},
	}
}

// CheckCandidateIndexes 核对撮合与维护扫描依赖的普通索引。既有表缺索引时迁移器只给计划 / 警告，
// 扫描不能带着全表排序接流，所以启动与迁移后都核一遍。
// 机房投影表 match_ticket_region / team_launch_region 已随按池撮合退役（2026-11-05）；旧库里若还在是孤表，不再核对。
// match_spectator_intent 的索引给已关闭意图的 7 天保留期清理用(PurgeClosedSpectatorIntents 按 (created_ms, 主键) 翻页):
// 缺了它每轮清理都是全表扫描加排序,3 秒预算内跑不完,清理永远不前进(观战第二轮评审 R2-GO 一般-4)。
func (s *Store) CheckCandidateIndexes(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT table_name,index_name,column_name FROM information_schema.statistics
		WHERE table_schema=DATABASE() AND table_name IN ('match_authority_ticket','match_challenge_invitation','match_direct_intent','match_join_intent','team_authority','team_invite_index','team_application_index','team_launch','team_launch_role','match_spectator','match_spectator_intent')
		ORDER BY table_name,index_name,seq_in_index`)
	if err != nil {
		return err
	}
	indexes := map[string][]string{}
	for rows.Next() {
		var table, index, column string
		if err = rows.Scan(&table, &index, &column); err != nil {
			rows.Close()
			return err
		}
		key := table + "/" + index
		indexes[key] = append(indexes[key], column)
	}
	readErr := rows.Err()
	closeErr := rows.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	for table, required := range map[string][]string{
		"match_authority_ticket":     {"state,enqueued_ms,player_id", "state,mode,team_id,enqueued_ms,player_id", "state,mode,config_id,team_size,team_id,enqueued_ms,player_id"},
		"match_challenge_invitation": {"target_player_id,state,expires_ms,challenge_id", "challenger_player_id,state,expires_ms,challenge_id"},
		"match_direct_intent":        {"exit_prepared,cancelled,battle_id,kind,owner_id,request_high,request_low"},
		"match_join_intent":          {"state,player_id,request_high,request_low"},
		"match_spectator":            {"state,player_id", "updated_ms,player_id"},
		"match_spectator_intent":     {"state,created_ms,player_id,request_high,request_low"},
		"team_authority":             {"state,idle_expires_ms,team_id"},
		"team_invite_index":          {"invitee_id,expires_ms,team_id"},
		"team_application_index":     {"applicant_id,expires_ms,team_id"},
		"team_launch":                {"state,enqueued_ms,actor_id,request_high,request_low", "team_id,state,enqueued_ms,actor_id,request_high,request_low", "state,mode,config_id,target_size,enqueued_ms,actor_id,request_high,request_low"},
		"team_launch_role":           {"player_id,state,actor_id,request_high,request_low"},
	} {
		for _, columns := range required {
			found := false
			for key, actual := range indexes {
				if strings.HasPrefix(key, table+"/") && strings.Join(actual, ",") == columns {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%s缺候选索引(%s)，先执行迁移计划再接流", table, columns)
			}
		}
	}
	return nil
}

type schemaRunner func(context.Context, *sql.DB, schemamigrate.Options) (schemamigrate.Report, error)

// requireCurrentSchema 只读核对已迁移 schema，缺表、漂移或人工项都不能由常驻账号补建。
func requireCurrentSchema(ctx context.Context, db *sql.DB, database string, plan schemaRunner) error {
	report, err := plan(ctx, db, schemamigrate.Options{Database: database, Tables: Tables()})
	if err != nil {
		return fmt.Errorf("全球匹配权威 schema 核对失败: %w", err)
	}
	if !report.Clean() {
		return errors.New("全球匹配权威 schema 尚未就绪，使用迁移专用账号执行 match -f <配置> -migrate；人工项须先审阅，拒绝接流")
	}
	return nil
}

// Migrate 只执行显式 schema 迁移，不读取探测密钥或装配节点、消费者、匹配循环。
// 返回报告及错误原样交给 schemamigrate.ExitCode；改列只接受本次 CLI 的显式授权。
func Migrate(ctx context.Context, c config.GlobalMatchConf, allowModify bool) (schemamigrate.Report, error) {
	permission, err := authoritysql.LoadSchemaPermission(os.Getenv)
	if err != nil {
		return schemamigrate.Report{}, err
	}
	// 搬库期间(state=MOVING)拒绝 DDL;库搬走之后连当前集群。
	options, err := dbplacement.ForMigration(ctx, c.DbPlacement, c.Database)
	if err != nil {
		return schemamigrate.Report{}, err
	}
	db, err := openAuthorityDatabaseWithPermission(ctx, c, permission, options...)
	if err != nil {
		return schemamigrate.Report{}, err
	}
	defer db.Close()
	report, err := schemamigrate.Up(ctx, db, schemamigrate.Options{
		Database: c.Database, Tables: Tables(), AllowModifyColumn: allowModify,
	})
	// 闸门跟着逻辑库走:匹配库自带一份。角色护栏副本同样每个参与者的库一张。
	if err == nil {
		err = authoritysql.EnsureCatalogSchema(ctx, db, c.Database)
	}
	if err == nil {
		err = rolefence.EnsureSchema(ctx, db)
	}
	// 普通索引的历史漂移不由迁移器擅自改写；Job也不能将其误报为已可接流。
	if err == nil && len(report.Manual) == 0 {
		if indexErr := (&Store{DB: db}).CheckCandidateIndexes(ctx); indexErr != nil {
			report.Manual = append(report.Manual, fmt.Sprintf("candidate indexes still need manual review after migration: %v", indexErr))
		}
	}
	return report, err
}

// openAuthorityDatabase 共用原强读池，迁移与常驻形态只在 schema 写权限上分离。
func openAuthorityDatabase(ctx context.Context, c config.GlobalMatchConf) (*sql.DB, error) {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		return nil, err
	}
	return openAuthorityDatabaseWithPermission(ctx, c, authoritysql.Writer(writer))
}

func openAuthorityDatabaseWithPermission(ctx context.Context, c config.GlobalMatchConf, permission authoritysql.Permission, options ...authoritysql.OpenOption) (*sql.DB, error) {
	if c.Host == "" || c.User == "" || c.Database != "mmorpg_match" {
		return nil, errors.New("全球匹配数据库配置无效")
	}
	backend, err := authoritysql.ParseBackend(c.Backend)
	if err != nil {
		return nil, err
	}
	mc := mysql.NewConfig()
	mc.User, mc.Passwd = c.User, os.Getenv(c.PasswordEnv)
	mc.Net, mc.Addr, mc.DBName = "tcp", c.Host, c.Database
	mc.ParseTime = true
	// 避免临时 PREPARE/CLOSE 在本机 Docker 转发链路逐次停顿约 40ms，耗尽队列扫描预算。
	// 使用驱动的 utf8mb4 参数转义，仍保持单语句和原有写者闸门。
	mc.InterpolateParams = true
	mc.Timeout, mc.ReadTimeout, mc.WriteTimeout = 3*time.Second, 3*time.Second, 3*time.Second
	mc.Params = map[string]string{"charset": "utf8mb4", "sql_mode": "'STRICT_TRANS_TABLES'", "transaction_isolation": "'READ-COMMITTED'"}
	if backend == authoritysql.BackendTiDB {
		for k, v := range map[string]string{"tidb_txn_mode": "'pessimistic'", "tidb_read_staleness": "0", "tidb_low_resolution_tso": "OFF", "tidb_replica_read": "'leader'", "tidb_read_consistency": "'strict'", "tidb_snapshot": "''"} {
			mc.Params[k] = v
		}
	}
	db, err := authoritysql.Open(ctx, mc.FormatDSN(), c.Database, backend, 16, permission, options...)
	if err != nil {
		return nil, err
	}
	db.SetMaxIdleConns(4)
	return db, nil
}
