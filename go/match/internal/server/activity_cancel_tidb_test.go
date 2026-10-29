package server

import (
	"context"
	"net"
	"os"
	"shared/authoritysql/sqltest"
	"strconv"
	"strings"
	"testing"
	"time"

	"match/internal/authority"
	"match/internal/svc"
	battlepb "proto/battle"
	pb "proto/match"
	"schemamigrate"
	"shared/serviceauth"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestTiDBActivityCancellationRPCSignsOriginalReceiptAndBlocksLateStart(t *testing.T) {
	addr := os.Getenv("MATCH_IT_TIDB")
	if addr == "" {
		if os.Getenv("MATCH_REQUIRE_TIDB") == "1" {
			t.Fatal("缺少MATCH_IT_TIDB")
		}
		t.Skip("未配置独占match取消TiDB")
	}
	host, portText, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip)
	require.True(t, ip.IsLoopback())
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	require.NotZero(t, port)
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = addr
	cfg.User = "root"
	schema := "match_cancel_it_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	opts, err := sqltest.OptionsFromEnv(cfg.FormatDSN(), schema+"_writer", schema)
	require.NoError(t, err)
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	install, err := fixture.OpenSchema(ctx, schema)
	require.NoError(t, err)
	var engine string
	require.NoError(t, install.QueryRowContext(ctx, "SELECT VERSION()").Scan(&engine))
	require.Contains(t, strings.ToLower(engine), "tidb")
	tables := []proto.Message{&pb.MatchDirectIntentRecord{}, &pb.MatchFormationRecord{}}
	report, err := schemamigrate.Up(ctx, install, schemamigrate.Options{Database: schema, Tables: tables})
	require.NoError(t, err)
	require.Empty(t, report.Manual)
	require.NoError(t, fixture.GrantProtoTables(ctx, schema, tables))
	db, err := fixture.Open(ctx, schema, 4)
	require.NoError(t, err)
	secret := strings.Repeat("z", 32)
	verifier, err := serviceauth.New(map[string]string{"guild": secret})
	require.NoError(t, err)
	server := NewMatchInternalServer(&svc.ServiceContext{Authority: &authority.Store{DB: db}, ActivityCancelAuth: verifier, ActivityCancelSecret: secret})
	original := &pb.StartActivityBattleRequest{RequestId: 51, BattleConfigId: 1, MemberPlayerIds: []uint64{11, 22}, ActivityContext: &battlepb.BattleActivityContext{Kind: battlepb.EBattleActivityKind_BATTLE_ACTIVITY_KIND_GUILD_TRIAL, GuildId: 44, ActivityId: 3, PeriodKey: 20261021, GuildPeriodKey: 20261021, InitiatorPlayerId: 11, LaunchRequestId: 51}}
	in := &pb.CancelActivityBattleRequest{Original: original}
	method := pb.MatchInternal_CancelActivityBattle_FullMethodName
	require.NoError(t, serviceauth.Sign(method, in, "guild", secret, uint64(time.Now().UnixMilli())))
	out, err := server.CancelActivityBattle(ctx, in)
	require.NoError(t, err)
	require.True(t, out.RequestCancelled)
	require.Zero(t, out.BattleId)
	digest, err := serviceauth.Digest(in)
	require.NoError(t, err)
	require.Equal(t, digest, out.RequestDigest)
	replyVerifier, err := serviceauth.New(map[string]string{"match": secret})
	require.NoError(t, err)
	caller, err := replyVerifier.Verify(ctx, method+"/reply", out)
	require.NoError(t, err)
	require.Equal(t, "match", caller)
	late, err := server.StartActivityBattle(ctx, original)
	require.NoError(t, err)
	require.NotEqual(t, pb.ActivityBattleReject_ACTIVITY_BATTLE_REJECT_NONE, late.Reject)
	require.Zero(t, late.BattleId)
	var formed int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_formation").Scan(&formed))
	require.Zero(t, formed)
}
