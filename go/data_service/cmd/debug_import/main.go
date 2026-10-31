// debug_import 把 debug_fetch 导出的存档写进本地已有的一个角色,用来在本地复现线上问题
// (docs/ops/online-debug-data-fetch.md)。
//
// 只改 player_store 里目标角色的行,不碰路由和账号目录:先用本地客户端或机器人建好角色,再把存档盖上去。
// 不换全局记录,所以线上 ID 不进本地,也不会和本地发号器撞号。
//
// 写之前的闸门:目标地址必须是本机(或显式放行的本地服务名),目标库必须带本地标记。port-forward 到线上时地址也是
// 127.0.0.1,只看地址拦不住,所以标记才是判据;标记不要放进任何部署脚本。默认只预览,-dry-run=false 才写。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"data_service/internal/playerbundle"
	"shared/generated/persistschema"
)

const localTargetMarker = "local-debug-import-target"

type options struct {
	file      string
	dsnEnv    string
	globalDB  string
	player    uint64
	allowHost string
	dryRun    bool
	timeout   time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.file, "file", "", "bundle written by debug_fetch (required)")
	flag.StringVar(&o.dsnEnv, "dsn-env", "", "name of the environment variable holding the local MySQL/TiDB DSN (required)")
	flag.StringVar(&o.globalDB, "global-db", "mmorpg_global", "global schema holding player_route")
	flag.Uint64Var(&o.player, "local-player", 0, "existing local player to overwrite (required)")
	flag.StringVar(&o.allowHost, "allow-target-host", "", "local service name allowed besides loopback, for example a docker service")
	flag.BoolVar(&o.dryRun, "dry-run", true, "only print the plan")
	flag.DurationVar(&o.timeout, "timeout", time.Minute, "overall timeout")
	flag.Parse()
	if err := run(o); err != nil {
		log.Fatalf("debug_import: %v", err)
	}
}

func run(o options) error {
	if o.file == "" || o.dsnEnv == "" || o.player == 0 {
		return errors.New("-file, -dsn-env and -local-player are required")
	}
	raw, err := os.ReadFile(o.file)
	if err != nil {
		return err
	}
	bundle, err := playerbundle.Parse(raw)
	if err != nil {
		return err
	}
	dsn := os.Getenv(o.dsnEnv)
	if dsn == "" {
		return fmt.Errorf("environment variable %s is empty", o.dsnEnv)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return errors.New("cannot parse the DSN") // 解析错误会带上 DSN 原文,不往外打
	}
	if err := requireLocalAddr(cfg.Addr, o.allowHost); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	global, err := openSchema(cfg, o.globalDB)
	if err != nil {
		return err
	}
	defer global.Close()
	route, found, err := playerbundle.ReadRoute(ctx, global, o.player)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("local player %d has no route: create the role with a local client or robot first", o.player)
	}
	if route.FrozenRunID != "" {
		return fmt.Errorf("local route of player %d is frozen by run %s", o.player, route.FrozenRunID)
	}
	store, err := openSchema(cfg, route.StoreSchema)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := requireLocalMarker(ctx, store, route.StoreSchema); err != nil {
		return err
	}
	return overwrite(ctx, store, bundle, o, route.StoreSchema)
}

// overwrite 在一个事务里替换全部数据行:锁住写权行后,本地 scene 的认领和存盘都进不来。
func overwrite(ctx context.Context, store *sql.DB, bundle *playerbundle.Bundle, o options, schema string) error {
	tx, err := store.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var local playerbundle.LocalOwner
	err = tx.QueryRowContext(ctx, "SELECT save_seq,schema_gen,holder_instance,released FROM player_store WHERE player_id=? AND kind=0 FOR UPDATE", o.player).
		Scan(&local.SaveSeq, &local.SchemaGen, &local.Holder, &local.Released)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("local player %d has no owner row in %s", o.player, schema)
	}
	if err != nil {
		return fmt.Errorf("read local owner row: %w", err)
	}
	plan, err := playerbundle.PlanImport(bundle, local, persistschema.Gen)
	if err != nil {
		return err
	}
	fmt.Printf("source player %d (revision %s) -> local player %d in %s: %d data rows, save_seq %d -> %d, schema_gen %d -> %d\n",
		bundle.PlayerID, bundle.ServerRevision, o.player, schema, len(plan.Rows), local.SaveSeq, plan.SaveSeq, local.SchemaGen, plan.SchemaGen)
	if o.dryRun {
		fmt.Println("dry run: nothing written; pass -dry-run=false to write")
		return nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM player_store WHERE player_id=? AND kind>0", o.player); err != nil {
		return fmt.Errorf("delete local data rows: %w", err)
	}
	for _, row := range plan.Rows {
		// 数据行的代次只记录最后写入时的值,判定只看写权行;holder_instance、payload 是 NOT NULL,绑定空切片。
		if _, err := tx.ExecContext(ctx, "INSERT INTO player_store (player_id,kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload) VALUES (?,?,0,?,?,?,0,?)",
			o.player, row.Kind, plan.SaveSeq, plan.SchemaGen, []byte{}, nonNil(row.Payload)); err != nil {
			return fmt.Errorf("insert local row of kind %d: %w", row.Kind, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE player_store SET save_seq=?,schema_gen=? WHERE player_id=? AND kind=0", plan.SaveSeq, plan.SchemaGen, o.player); err != nil {
		return fmt.Errorf("update local owner row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Println("imported; log the local role in to load it")
	return nil
}

func nonNil(payload []byte) []byte {
	if payload == nil {
		return []byte{}
	}
	return payload
}

// openSchema 用普通连接:写权框架管的是线上写者,这里只写本地库,由上面的闸门把关。
func openSchema(cfg *mysql.Config, schema string) (*sql.DB, error) {
	scoped := cfg.Clone()
	scoped.DBName = schema
	db, err := sql.Open("mysql", scoped.FormatDSN())
	if err != nil {
		return nil, errors.New("cannot open the database")
	}
	return db, nil
}

// requireLocalAddr 拒绝本机以外的地址。allowHost 只放行一个显式给出的名字。
func requireLocalAddr(addr, allowHost string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if strings.EqualFold(host, "localhost") || (allowHost != "" && strings.EqualFold(host, allowHost)) {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("target host %q is not local; pass -allow-target-host for a local service name", host)
}

func requireLocalMarker(ctx context.Context, store *sql.DB, schema string) error {
	var marker string
	err := store.QueryRowContext(ctx, "SELECT marker FROM debug_import_local_target WHERE marker=?", localTargetMarker).Scan(&marker)
	if err != nil {
		return fmt.Errorf("schema %s has no local target marker (see docs/ops/online-debug-data-fetch.md): %w", schema, err)
	}
	return nil
}
