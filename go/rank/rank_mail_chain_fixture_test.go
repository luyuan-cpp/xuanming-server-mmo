package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	mailpb "proto/mail"
	rankpb "proto/rank"
	"rank/internal/data"
	"rank/internal/reward"
	"rank/internal/server"
	"schemamigrate"
	"shared/authoritysql"
	"shared/domainhost"
	"shared/serviceauth"
)

func rankMainFixtureEndpoint(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("排行榜RPC夹具必须显式数值loopback端口")
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || number < 1 || number > 65535 {
		return errors.New("排行榜RPC夹具仅接受数值loopback与合法非零端口")
	}
	return nil
}
func TestRankMainFixtureEndpointRejectsForeignOrInvalidPort(t *testing.T) {
	for _, address := range []string{"127.0.0.1:24000", "[::1]:24000"} {
		if err := rankMainFixtureEndpoint(address); err != nil {
			t.Fatal(err)
		}
	}
	for _, address := range []string{"localhost:24000", "10.0.0.1:24000", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:mysql", "127.0.0.1"} {
		if rankMainFixtureEndpoint(address) == nil {
			t.Fatal("非法夹具端点未拒绝")
		}
	}
}

// 只作为跨 module 真 RPC 验收的独立进程；普通测试不会启动数据库或监听。
func TestRankMailChainFixture(t *testing.T) {
	if os.Getenv("RANK_MAIL_CHAIN_FIXTURE") != "rank" {
		t.Skip("仅由显式启用的跨域验收进程启动")
	}
	address := os.Getenv("RANK_MAIL_CHAIN_IT_TIDB")
	if err := rankMainFixtureEndpoint(address); err != nil {
		t.Fatal(err)
	}
	database := os.Getenv("RANK_MAIL_CHAIN_DATABASE")
	if !regexp.MustCompile(`^rank_mail_chain_[a-f0-9]{16}_rank$`).MatchString(database) {
		t.Fatal("跨域夹具数据库必须是本次独有的随机库")
	}
	secret := os.Getenv("RANK_MAIL_CHAIN_SECRET")
	if len(secret) < 32 {
		t.Fatal("跨域夹具缺少临时认证密钥")
	}
	endpoint := os.Getenv("RANK_MAIL_CHAIN_MAIL_ENDPOINT")
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host != "127.0.0.1" || port == "" || port == "0" {
		t.Fatal("邮件 RPC 必须是本次本机随机监听端口")
	}
	controlDir := os.Getenv("RANK_MAIL_CHAIN_CONTROL_DIR")
	if !filepath.IsAbs(controlDir) {
		t.Fatal("跨域夹具控制目录必须是绝对路径")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	adminDSN := os.Getenv("RANK_MAIL_CHAIN_SCHEMA_DSN")
	normalDSN := os.Getenv("RANK_MAIL_CHAIN_NORMAL_DSN")
	adminConfig, err := mysql.ParseDSN(adminDSN)
	if err != nil || adminConfig.Net != "tcp" || adminConfig.Addr != address || adminConfig.DBName != database {
		t.Fatal("整链安装DSN与本例目标不符")
	}
	normalConfig, err := mysql.ParseDSN(normalDSN)
	if err != nil || normalConfig.Net != "tcp" || normalConfig.Addr != address || normalConfig.DBName != database || normalConfig.User == adminConfig.User || !regexp.MustCompile(`^chain_writer_[0-9a-f]{16}$`).MatchString(normalConfig.User) {
		t.Fatal("整链normal主体未隔离")
	}
	schemaPermission, err := authoritysql.LoadSchemaPermission(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	install, err := authoritysql.OpenForMigration(ctx, adminDSN, database, true, schemaPermission)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := install.Close(); err != nil {
			t.Errorf("关闭安装池: %v", err)
		}
	})
	tables := data.Tables()
	if report, err := schemamigrate.Up(ctx, install, schemamigrate.Options{Database: database, Tables: tables}); err != nil || len(report.Manual) != 0 {
		t.Fatalf("整链schema: %v %+v", err, report)
	}
	for _, model := range tables {
		table, ok := proto2mysql.TableNameFromDescriptor(model.ProtoReflect().Descriptor())
		if !ok || !regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`).MatchString(table) {
			t.Fatal("正式表名无效")
		}
		if _, err = install.ExecContext(ctx, "GRANT SELECT,INSERT,UPDATE,DELETE ON `"+database+"`.`"+table+"` TO '"+normalConfig.User+"'@'%'"); err != nil {
			t.Fatal("整链具名业务表授权失败")
		}
	}
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	db, err := authoritysql.Open(ctx, normalDSN, database, true, 2, authoritysql.Writer(writer))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭normal池: %v", err)
		}
	})
	var boardIDs, settlementIDs, rewardIDs atomic.Uint64
	// 仅替代外部号段服务；评分、封榜和奖励状态均由原生产实现写入。
	repo := data.New(db,
		func(context.Context) (uint64, error) { return boardIDs.Add(1), nil },
		func(context.Context) (uint64, error) { return settlementIDs.Add(1), nil },
		func(context.Context) (uint64, error) { return rewardIDs.Add(1), nil },
		func(uint32) bool { return false })
	auth, err := serviceauth.New(map[string]string{"match": secret})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	rpc := grpc.NewServer(grpc.UnaryInterceptor(domainhost.Guard(server.ClientMethods(), 3*time.Second)))
	rankpb.RegisterRankAdminServer(rpc, server.New(repo, auth, nil))
	serveDone := make(chan struct{})
	var serveErr error
	go func() {
		defer close(serveDone)
		serveErr = rpc.Serve(listener)
	}()
	defer func() {
		rpc.Stop()
		select {
		case <-serveDone:
			if serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) && !errors.Is(serveErr, net.ErrClosed) {
				t.Errorf("排行榜 RPC 退出: %v", serveErr)
			}
		case <-time.After(3 * time.Second):
			t.Error("排行榜 RPC 未在预算内退出")
		}
	}()
	mailConnection, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer mailConnection.Close()
	worker := &reward.Worker{Repo: repo, Mail: mailpb.NewMailAdminClient(mailConnection), Secret: secret}
	readyCtx, stopReady := context.WithTimeout(ctx, 3*time.Second)
	connection, err := grpc.DialContext(readyCtx, listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	stopReady()
	if err != nil {
		t.Fatalf("排行榜 RPC 未就绪: %v", err)
	}
	if err = connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(controlDir, "ready"), []byte(listener.Addr().String()), 0600); err != nil {
		t.Fatal(err)
	}
	commands := make(chan string)
	inputDone := make(chan struct{})
	var inputErr error
	go func() {
		defer close(inputDone)
		defer close(commands)
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			select {
			case commands <- strings.TrimSpace(scanner.Text()):
			case <-ctx.Done():
				return
			}
		}
		inputErr = scanner.Err()
	}()
	defer func() {
		cancel()
		_ = os.Stdin.Close()
		select {
		case <-inputDone:
		case <-time.After(3 * time.Second):
			t.Error("排行榜输入监听未在预算内退出")
		}
	}()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("排行榜夹具超过整体截止时间")
		case <-serveDone:
			t.Fatalf("排行榜 RPC 提前停止: %v", serveErr)
		case command, ok := <-commands:
			if !ok {
				if inputErr != nil {
					t.Fatalf("排行榜输入监听失败: %v", inputErr)
				}
				return
			}
			if command == "stop" {
				return
			}
			if command != "tick" {
				t.Fatal("不支持的夹具控制命令")
			}
			step, stop := context.WithTimeout(ctx, 8*time.Second)
			err = worker.Tick(step)
			stop()
			result := struct{ Error string }{}
			if err != nil {
				result.Error = err.Error()
			}
			body, e := json.Marshal(result)
			if e != nil {
				t.Fatal(e)
			}
			// 完整写完后才发布回执，父进程不会读到半个 JSON。
			pending := filepath.Join(controlDir, "tick.pending")
			if e = os.WriteFile(pending, body, 0600); e != nil {
				t.Fatal(e)
			}
			if e = os.Rename(pending, filepath.Join(controlDir, "tick.done")); e != nil {
				t.Fatal(e)
			}
		}
	}
}
