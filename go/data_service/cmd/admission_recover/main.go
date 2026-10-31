// admission_recover 把已确认死掉的 scene 实例从一名玩家的写权行上摘掉。
//
// 持有节点判死时 scene_manager 会自动释放写权(ReleaseDeadPlayerStoreHolder)。这个工具只处理它够不着的情况:
// 节点在写 location 之前就死了,或者玩家释放之后、关登记之前死了,又有一笔角色交易在等这个实例排空。
// 摘掉之后,交易的 Begin / Confirm / Cancel 重试时会把它判为排空(store.fenceDeadTradeHolders)。
//
// 运维必须先确认实例已经死了:活着的实例被摘掉之后,它内存里没落盘的进度就丢了。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"data_service/internal/store"

	"github.com/go-sql-driver/mysql"

	"shared/accountdirectory"
	"shared/authoritysql"
)

type recoveryOptions struct {
	dsnEnv, schema, stores, mode, instance string
	playerID                               uint64
	confirmedDead                          bool
}

func main() {
	var o recoveryOptions
	flag.StringVar(&o.dsnEnv, "dsn-env", "ADMISSION_RECOVER_DSN", "全局库 DSN 所在的环境变量名")
	flag.StringVar(&o.schema, "schema", "", "全局库名")
	flag.StringVar(&o.stores, "storage-ids", "", "允许访问的 storage_id,逗号分隔")
	flag.Uint64Var(&o.playerID, "player-id", 0, "玩家")
	flag.StringVar(&o.instance, "holder-instance", "", "要摘掉的 scene 实例号,必须等于写权行的现任")
	flag.BoolVar(&o.confirmedDead, "confirmed-dead", false, "确认这个实例已经死了;apply 必填")
	flag.StringVar(&o.mode, "mode", "plan", "plan 只核对;apply 才写")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runRecovery(ctx, o, os.Stdout, os.Stderr, nil)
	stop()
	os.Exit(code)
}

type releaseHolder func(ctx context.Context, player uint64, instance string, apply bool) (uint64, error)

func runRecovery(ctx context.Context, o recoveryOptions, out, errOut io.Writer, execute releaseHolder) int {
	if o.mode != "plan" && o.mode != "apply" {
		fmt.Fprintln(errOut, "mode must be plan or apply")
		return 2
	}
	if o.playerID == 0 || o.instance == "" {
		fmt.Fprintln(errOut, "player-id and holder-instance are required")
		return 2
	}
	if o.mode == "apply" && !o.confirmedDead {
		fmt.Fprintln(errOut, "apply requires -confirmed-dead")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if execute == nil {
		ids, err := parseStorageIDs(o.stores)
		if err != nil {
			fmt.Fprintln(errOut, "invalid storage-ids")
			return 2
		}
		backend, err := authoritysql.LoadBackend(os.Getenv)
		if err != nil {
			fmt.Fprintln(errOut, "invalid "+authoritysql.BackendEnv)
			return 2
		}
		permission, err := authoritysql.LoadOperatorPermission(os.Getenv)
		if err != nil {
			fmt.Fprintln(errOut, "missing SQL operator identity")
			return 2
		}
		// 从环境读 DSN 后只取连接配置,错误里不回显原 DSN 或密码。
		cfg, err := mysql.ParseDSN(os.Getenv(o.dsnEnv))
		if err != nil || cfg.DBName != o.schema || cfg.Net != "tcp" {
			fmt.Fprintln(errOut, "invalid SQL connection parameters")
			return 2
		}
		directory, err := store.NewAccountDirectoryStore(store.MySQLConfig{Host: cfg.Addr, User: cfg.User, Password: cfg.Passwd, DBName: o.schema, Permission: permission}, backend, 100, 1)
		if err != nil {
			fmt.Fprintln(errOut, "cannot open the account directory")
			return 2
		}
		defer directory.Close()
		if err := directory.ConfigureInitialPlayerCommit(ids); err != nil {
			fmt.Fprintln(errOut, "invalid storage-ids")
			return 2
		}
		execute = directory.ReleaseHolderInstance
	}
	epoch, err := execute(ctx, o.playerID, o.instance, o.mode == "apply")
	if err != nil {
		// 底层错误可能含连接细节,只输出固定文案。
		switch {
		case errors.Is(err, accountdirectory.ErrConflict):
			fmt.Fprintln(errOut, "refused: the instance is not the current holder of this player")
		case errors.Is(err, accountdirectory.ErrMissing):
			fmt.Fprintln(errOut, "refused: this player has no owner row")
		default:
			fmt.Fprintln(errOut, "not confirmed: rejected or outcome unknown; rerun plan to check")
		}
		return 1
	}
	fmt.Fprintf(out, "mode=%s player=%d owner_epoch_before=%d\n", o.mode, o.playerID, epoch)
	return 0
}

func parseStorageIDs(raw string) ([]uint32, error) {
	var ids []uint32
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseUint(strings.TrimSpace(part), 10, 32)
		if err != nil || id == 0 {
			return nil, errors.New("invalid storage id")
		}
		ids = append(ids, uint32(id))
	}
	return ids, nil
}
