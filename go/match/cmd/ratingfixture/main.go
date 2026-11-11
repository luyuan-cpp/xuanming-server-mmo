// Command ratingfixture 把冒烟要用的评分写进 match_rating,只用于 dev / test 环境
// (docs/design/global-match-hierarchical-pools.md §12.5 第 5 步):robot 的 pvp-team-smoke 先用 phase=prepare 登录账号、
// 写出「player_id → 评分」的夹具文件,本工具把它写进库,robot 再用 phase=run 跑看评分的情况。
//
//	ratingfixture -f etc/match_service.yaml -in ../../robot/logs/pvp_team_smoke_fixture.json
//
// 数据库地址取 match 配置的 GlobalMatch(Host / User / Database,口令从 PasswordEnv 指的环境变量读)。
// 一个事务里按 (player_id, mode) 写:已有的行只改分数和时刻,局数不动(榜按局数做版本,局数退回去榜就不认新结算了);
// 没有的行局数记 0。全部行用同一个写库时刻(数据库时钟,与 match 结算记 achieved_ms 同一口径)。
// 提交之后把这个时刻写回夹具文件的 applied_ms:robot run 阶段只认 achieved_ms 比它新的榜条目,并在开跑时把它清零,
// 一次写库只够跑一轮。写回失败时评分已经写进库,照原命令重跑即可(写库幂等)。
// 配置的 Mode 不是 dev / test、或者按映射拨号(DbPlacement)时拒绝运行:这是测试夹具,不是运营工具,
// 也不经过服务的写者闸门。
//
// 退出码:0 = 写入并写回成功;1 = 连接、写入或写回失败;2 = 参数、配置或夹具文件不合格。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"match/internal/config"
	"match/internal/ratingrule"

	pb "proto/match"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/service"
)

// fixture 是夹具文件的格式,与 robot/pvp_team_smoke_scenario.go 的 pvpSmokeFixture 一致。
type fixture struct {
	Mode    uint32  `json:"mode"`
	Ratings []entry `json:"ratings"`
	// AppliedMs 由本工具在写库成功后写回;读进来的旧值不用。
	AppliedMs uint64 `json:"applied_ms"`
}

type entry struct {
	Account     string `json:"account"` // 只用于报错时定位
	PlayerID    uint64 `json:"player_id"`
	RatingCenti int64  `json:"rating_centi"`
}

// nowSQL 读数据库时钟,与 match 的 authority.now 同一表达式。
const nowSQL = "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)"

// upsertSQL 按 match_rating 的主键 (player_id, mode) 写一行,两个时刻都是本次写库时刻。
const upsertSQL = "INSERT INTO match_rating(player_id,mode,rating_centi,games,updated_ms,score_changed_ms) VALUES(?,?,?,0,?,?) " +
	"ON DUPLICATE KEY UPDATE rating_centi=VALUES(rating_centi),updated_ms=VALUES(updated_ms),score_changed_ms=VALUES(score_changed_ms)"

func main() {
	configPath := flag.String("f", "etc/match_service.yaml", "match config file")
	input := flag.String("in", "", "rating fixture written by the robot pvp-team-smoke prepare phase")
	timeout := flag.Duration("timeout", 10*time.Second, "database timeout for the whole write")
	flag.Parse()
	if *input == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		os.Exit(2)
	}
	var c config.Config
	if err := conf.Load(*configPath, &c); err != nil {
		fmt.Fprintf(os.Stderr, "load match config %s: %v\n", *configPath, err)
		os.Exit(2)
	}
	if err := checkTarget(c); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	file, err := os.Open(*input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open fixture: %v\n", err)
		os.Exit(2)
	}
	parsed, err := parseFixture(file)
	_ = file.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid fixture %s: %v\n", *input, err)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	db, err := sql.Open("mysql", dsn(c.GlobalMatch))
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	appliedMs, err := apply(ctx, db, parsed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "write ratings to %s/%s: %v\n", c.GlobalMatch.Host, c.GlobalMatch.Database, err)
		os.Exit(1)
	}
	parsed.AppliedMs = appliedMs
	if err := writeFixture(*input, parsed); err != nil {
		fmt.Fprintf(os.Stderr, "ratings were written, but recording applied_ms in %s failed (rerun this command): %v\n", *input, err)
		os.Exit(1)
	}
	fmt.Printf("applied %d ratings for mode %d to %s/%s at applied_ms=%d\n", len(parsed.Ratings), parsed.Mode, c.GlobalMatch.Host, c.GlobalMatch.Database, appliedMs)
}

// checkTarget 只允许写开发与测试环境,并且只认直连的数据库地址。
func checkTarget(c config.Config) error {
	if c.Mode != service.DevMode && c.Mode != service.TestMode {
		return fmt.Errorf("refusing to write ratings: match Mode=%q, only %q and %q are allowed", c.Mode, service.DevMode, service.TestMode)
	}
	if c.GlobalMatch.DbPlacement.Enabled() {
		return errors.New("refusing to write ratings: GlobalMatch.DbPlacement is enabled; use a dev config that dials the database directly")
	}
	if c.GlobalMatch.Host == "" || c.GlobalMatch.User == "" || c.GlobalMatch.Database == "" {
		return errors.New("GlobalMatch.Host, GlobalMatch.User and GlobalMatch.Database must be set")
	}
	return nil
}

// parseFixture 读夹具并校验:模式必须计分;至少一行;player_id 非 0 且不重复;评分不为负。不认识的字段报错。
func parseFixture(r io.Reader) (fixture, error) {
	var parsed fixture
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return parsed, err
	}
	if !ratingrule.Rated(pb.MatchMode(parsed.Mode)) {
		return parsed, fmt.Errorf("mode %d is not a rated mode", parsed.Mode)
	}
	if len(parsed.Ratings) == 0 {
		return parsed, errors.New("fixture has no ratings")
	}
	seen := map[uint64]bool{}
	for _, row := range parsed.Ratings {
		switch {
		case row.PlayerID == 0:
			return parsed, fmt.Errorf("entry %q has player_id 0", row.Account)
		case row.RatingCenti < 0:
			return parsed, fmt.Errorf("player %d has a negative rating %d", row.PlayerID, row.RatingCenti)
		case seen[row.PlayerID]:
			return parsed, fmt.Errorf("player %d appears more than once", row.PlayerID)
		}
		seen[row.PlayerID] = true
	}
	return parsed, nil
}

func dsn(g config.GlobalMatchConf) string {
	mc := mysql.NewConfig()
	mc.User, mc.Passwd = g.User, os.Getenv(g.PasswordEnv)
	mc.Net, mc.Addr, mc.DBName = "tcp", g.Host, g.Database
	mc.Timeout, mc.ReadTimeout, mc.WriteTimeout = 3*time.Second, 5*time.Second, 5*time.Second
	return mc.FormatDSN()
}

// apply 在一个事务里写全部评分,返回写库时刻:要么全写上,要么一行都不写。
func apply(ctx context.Context, db *sql.DB, parsed fixture) (uint64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var stamp uint64
	if err := tx.QueryRowContext(ctx, nowSQL).Scan(&stamp); err != nil {
		return 0, fmt.Errorf("read database clock: %w", err)
	}
	for _, row := range parsed.Ratings {
		if _, err := tx.ExecContext(ctx, upsertSQL, row.PlayerID, parsed.Mode, row.RatingCenti, stamp, stamp); err != nil {
			return 0, fmt.Errorf("player %d (%s): %w", row.PlayerID, row.Account, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return stamp, nil
}

// writeFixture 把夹具(带 applied_ms)写回原文件:先写临时文件再改名,中途失败不会留下半个夹具。格式与 robot 写的相同。
func writeFixture(path string, parsed fixture) error {
	body, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
