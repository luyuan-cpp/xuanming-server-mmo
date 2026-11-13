package authority

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dspb "proto/data_service"
	pb "proto/match"
	"shared/rolefence"
	"shared/rolefence/fencetest"
	"shared/roleidentity"
)

// 复用独占TiDB建表/清理，但显式用真实护栏副本首锁覆盖旧状态机夹具的fake guard。
// 用例直接改副本行摆状态,自愈读回副本现状,不改变任何东西。真正从目录自愈的路径见 occupancy_native_test.go。
func nativeRoleStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("MATCH_IT_REQUIRE_TIDB") == "1" && os.Getenv("MATCH_IT_TIDB") == "" {
		t.Fatal("缺少MATCH_IT_TIDB，原生角色回归不得跳过")
	}
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, s.ConfigureRoleFence(ctx, fencetest.ReplicaReader{DB: s.DB}, fencetest.Admissions{Valid: true}))
	return s
}

func nativeRoleActor(t *testing.T, ctx context.Context, player uint64, account string, revision uint64) context.Context {
	t.Helper()
	actor, err := roleidentity.Capture(ctx, &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: player}})
	require.NoError(t, err)
	return actor
}

func nativeRoleJoin(t *testing.T, s *Store, ctx context.Context, player uint64) (context.Context, *pb.MatchAuthorityTicketRecord, *pb.MatchNetworkProfile) {
	t.Helper()
	account := fmt.Sprintf("original-%d", player)
	fencetest.Seed(t, ctx, s.DB, player, account, 7)
	actor := nativeRoleActor(t, ctx, player, account, 7)
	n, err := s.Now(ctx)
	require.NoError(t, err)
	plan := fmt.Sprintf("native-plan-%d", player)
	profile := &pb.MatchNetworkProfile{PlanId: plan, SessionBinding: "native-bound", Receipts: []*pb.NetworkProbeReceipt{{Grant: &pb.NetworkProbeGrant{PlayerId: player, PlanId: plan, SessionBinding: "native-bound", PhysicalRegion: 1, ExpiresAtMs: n + 120000}, RttMs: 50, SampleCount: 3, MeasuredAtMs: n, ExpiresAtMs: n + 120000}}}
	require.NoError(t, s.SavePlan(actor, player, profile, n+120000))
	ticket, created, err := s.Join(actor, &pb.MatchAuthorityTicketRecord{PlayerId: player, Ticket: uuid.NewString(), RequestId: uuid.NewString(), Mode: 3, ConfigId: 1, TeamSize: 2, RatingCenti: 150000}, plan, "native-bound")
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, account, ticket.OwnerAccount)
	require.EqualValues(t, 7, ticket.OwnerRevision)
	return actor, ticket, profile
}

// 原始SQL字节快照只含本例匹配表；P归属在测试中有意变化，不能混进零副作用比较。
func nativeMatchSnapshot(t *testing.T, s *Store, ctx context.Context) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for table, order := range map[string]string{
		"match_network_profile": "player_id", "match_authority_ticket": "player_id",
		"match_join_intent":   "player_id,request_high,request_low",
		"match_direct_intent": "kind,owner_id,request_high,request_low", "match_formation": "battle_id",
		"match_challenge_invitation": "challenge_id",
	} {
		rows, err := s.DB.QueryContext(ctx, "SELECT * FROM `"+table+"` ORDER BY "+order)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		grid := [][]string{columns}
		for rows.Next() {
			values := make([]sql.RawBytes, len(columns))
			args := make([]any, len(columns))
			for index := range values {
				args[index] = &values[index]
			}
			require.NoError(t, rows.Scan(args...))
			line := make([]string, len(values))
			for index, value := range values {
				if value == nil {
					line[index] = "NULL"
				} else {
					line[index] = "hex:" + hex.EncodeToString(value)
				}
			}
			grid = append(grid, line)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		raw, err := json.Marshal(grid)
		require.NoError(t, err)
		out[table] = string(raw)
	}
	return out
}

func TestAuthorityTiDBOriginalRoleRejectsTransferABAAndFreezeBeforeAnyMatchWrite(t *testing.T) {
	for _, mode := range []string{"transfer", "same-account-ABA", "frozen"} {
		t.Run(mode, func(t *testing.T) {
			s := nativeRoleStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			actor, ticket, profile := nativeRoleJoin(t, s, ctx, 11)
			before := nativeMatchSnapshot(t, s, ctx)
			if mode == "transfer" || mode == "same-account-ABA" {
				fencetest.Update(t, ctx, s.DB, 11, "new-account", 8, 0)
				if mode == "same-account-ABA" {
					fencetest.Update(t, ctx, s.DB, 11, "original-11", 9, 0)
				}
			} else {
				fencetest.Update(t, ctx, s.DB, 11, "original-11", 7, 901)
			}
			require.ErrorIs(t, s.SavePlan(actor, 11, profile, ticket.ExpiresMs), ErrOriginalPlayer)
			require.Equal(t, before, nativeMatchSnapshot(t, s, ctx))
			require.ErrorIs(t, s.CommitProfile(actor, 11, profile.PlanId, profile.SessionBinding, profile.Receipts, ticket.ExpiresMs), ErrOriginalPlayer)
			require.Equal(t, before, nativeMatchSnapshot(t, s, ctx))
			for _, request := range []string{ticket.RequestId, uuid.NewString()} {
				in := proto.Clone(ticket).(*pb.MatchAuthorityTicketRecord)
				in.RequestId = request
				replayed, created, err := s.Join(actor, in, profile.PlanId, profile.SessionBinding)
				require.ErrorIs(t, err, ErrOriginalPlayer)
				require.Nil(t, replayed)
				require.False(t, created)
				require.Equal(t, before, nativeMatchSnapshot(t, s, ctx))
			}
			require.ErrorIs(t, s.CancelIntent(actor, 11, ticket.Ticket, ticket.RequestId), ErrOriginalPlayer)
			require.Equal(t, before, nativeMatchSnapshot(t, s, ctx))
			_, err := s.Ticket(actor, 11)
			require.ErrorIs(t, err, ErrOriginalPlayer)
			require.Equal(t, before, nativeMatchSnapshot(t, s, ctx))
		})
	}
}

func TestAuthorityTiDBNewOwnerCannotReplayOldJoinUUIDOrOldProfile(t *testing.T) {
	s := nativeRoleStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, ticket, profile := nativeRoleJoin(t, s, ctx, 11)
	fencetest.Update(t, ctx, s.DB, 11, "new-owner", 8, 0)
	actor := nativeRoleActor(t, ctx, 11, "new-owner", 8)
	before := nativeMatchSnapshot(t, s, ctx)
	replayed, created, err := s.Join(actor, ticket, profile.PlanId, profile.SessionBinding)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	require.Nil(t, replayed)
	require.False(t, created)
	require.ErrorIs(t, s.CommitProfile(actor, 11, profile.PlanId, profile.SessionBinding, profile.Receipts, ticket.ExpiresMs), ErrOriginalPlayer)
	require.ErrorIs(t, s.CancelIntent(actor, 11, ticket.Ticket, ticket.RequestId), ErrOriginalPlayer)
	require.Equal(t, before, nativeMatchSnapshot(t, s, ctx))
}

// 入口归属预检(docs/design/gate-online-identity.md §9.3):不加锁读副本,只判归属。战报、作战脚本列表这些只读方法靠它核归属。
func TestAuthorityTiDBVerifyOriginalOwnerFollowsOwnershipNotFreeze(t *testing.T) {
	s := nativeRoleStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fencetest.Seed(t, ctx, s.DB, 11, "seller", 7)
	seller := nativeRoleActor(t, ctx, 11, "seller", 7)
	before := nativeMatchSnapshot(t, s, ctx)
	require.NoError(t, s.VerifyOriginalOwner(seller))

	// 冻结(公示、投票、删角待确认)期间角色仍归原身份,读不拦;写入照旧由提交点拒绝。
	fencetest.Update(t, ctx, s.DB, 11, "seller", 7, 901)
	require.NoError(t, s.VerifyOriginalOwner(seller))

	// 过户之后旧主的连接:确定不归。新主通过;修订号相同而账号不同同样拒绝。
	fencetest.Update(t, ctx, s.DB, 11, "buyer", 8, 0)
	err := s.VerifyOriginalOwner(seller)
	require.ErrorIs(t, err, ErrOriginalPlayer)
	require.True(t, OwnerRejected(err))
	require.NoError(t, s.VerifyOriginalOwner(nativeRoleActor(t, ctx, 11, "buyer", 8)))
	require.True(t, OwnerRejected(s.VerifyOriginalOwner(nativeRoleActor(t, ctx, 11, "other", 8))))

	// 删角墓碑、目录里没有这名角色、ctx 里没有原身份:都是确定不归。
	fencetest.Update(t, ctx, s.DB, 11, "", 9, 0)
	require.True(t, OwnerRejected(s.VerifyOriginalOwner(nativeRoleActor(t, ctx, 11, "buyer", 8))))
	require.True(t, OwnerRejected(s.VerifyOriginalOwner(nativeRoleActor(t, ctx, 404, "ghost", 1))))
	require.True(t, OwnerRejected(s.VerifyOriginalOwner(ctx)))

	require.Equal(t, before, nativeMatchSnapshot(t, s, ctx), "预检只读,不碰任何匹配表")
}

// 过户提交、推送丢了:本库副本还停在「旧主 + 冻结」。预检不看冻结,但冻结的副本要先读目录再判,否则旧主的连接还能读战报。
// 目录读不到时这一次判不了,不能当成角色变了。
func TestAuthorityTiDBVerifyOriginalOwnerHealsFrozenReplicaBeforeJudging(t *testing.T) {
	s, directory := healStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seller, buyer := nativeRoleActor(t, ctx, 11, "seller", 7), nativeRoleActor(t, ctx, 11, "buyer", 8)

	// 副本缺行:读一次目录后通过;之后副本相符,只读本库。
	directory.Set(11, "seller", 7, 0)
	require.NoError(t, s.VerifyOriginalOwner(seller))
	require.NoError(t, s.VerifyOriginalOwner(seller))
	require.Equal(t, []uint64{11}, directory.Reads, "副本相符不读目录")

	// 冻结写进了副本,过户提交,推送丢了。
	fencetest.Update(t, ctx, s.DB, 11, "seller", 7, 901)
	directory.Set(11, "seller", 7, 901)
	directory.Set(11, "buyer", 8, 0)
	require.ErrorIs(t, s.VerifyOriginalOwner(seller), ErrOriginalPlayer)
	require.NoError(t, s.VerifyOriginalOwner(buyer))

	// 冻结而目录读不到:判不了,不是拒绝。
	fencetest.Update(t, ctx, s.DB, 11, "buyer", 8, 902)
	directory.Err = errors.Join(rolefence.ErrUnavailable, errors.New("data_service down"))
	err := s.VerifyOriginalOwner(buyer)
	require.ErrorIs(t, err, rolefence.ErrUnavailable)
	require.False(t, OwnerRejected(err))
}

func nativeRoleFormation(battle uint64) *pb.MatchFormationRecord {
	return &pb.MatchFormationRecord{BattleId: battle, OperationId: uuid.NewString(), PhysicalRegion: 1, TargetNodeId: 3, TargetInstanceId: "fixture-instance", TargetEndpoint: "127.0.0.1:1", Mode: 3, ConfigId: 1}
}

func TestAuthorityTiDBClaimRejectsOwnerAndProfileChangesAfterCandidateRead(t *testing.T) {
	for _, mode := range []string{"owner", "profile-bytes"} {
		t.Run(mode, func(t *testing.T) {
			s := nativeRoleStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, a, _ := nativeRoleJoin(t, s, ctx, 11)
			actor, b, profile := nativeRoleJoin(t, s, ctx, 22)
			if mode == "owner" {
				fencetest.Update(t, ctx, s.DB, 22, "original-22", 8, 0)
			} else {
				changed := proto.Clone(profile.Receipts[0]).(*pb.NetworkProbeReceipt)
				changed.RttMs++
				require.NoError(t, s.CommitProfile(actor, 22, profile.PlanId, profile.SessionBinding, []*pb.NetworkProbeReceipt{changed}, b.ExpiresMs))
			}
			before := nativeMatchSnapshot(t, s, ctx)
			err := s.Claim(ctx, nativeRoleFormation(901), []*pb.MatchAuthorityTicketRecord{a, b})
			require.Error(t, err)
			if mode == "owner" {
				require.ErrorIs(t, err, ErrOriginalPlayer)
			}
			require.Equal(t, before, nativeMatchSnapshot(t, s, ctx))
			_, err = s.Formation(ctx, 901)
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}

func TestAuthorityTiDBOriginalPlayerLocksRemainUnsignedOrderedUnderOppositeClaims(t *testing.T) {
	s := nativeRoleStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for round := uint64(0); round < 4; round++ {
		_, a, _ := nativeRoleJoin(t, s, ctx, 11+round)
		_, b, _ := nativeRoleJoin(t, s, ctx, ^uint64(0)-round)
		start := make(chan struct{})
		out := make(chan error, 2)
		go func() {
			<-start
			out <- s.Claim(ctx, nativeRoleFormation(1000+round*2), []*pb.MatchAuthorityTicketRecord{a, b})
		}()
		go func() {
			<-start
			out <- s.Claim(ctx, nativeRoleFormation(1001+round*2), []*pb.MatchAuthorityTicketRecord{b, a})
		}()
		close(start)
		wins := 0
		for range 2 {
			select {
			case err := <-out:
				if err == nil {
					wins++
				} else {
					var sqlErr *mysql.MySQLError
					if errors.As(err, &sqlErr) {
						require.NotContains(t, []uint16{1205, 1213, 9007}, sqlErr.Number, "反向名单不能形成P锁死锁或写冲突")
					}
					require.ErrorContains(t, err, "成局成员代次或地域条件已经变化")
				}
			case <-ctx.Done():
				t.Fatal("全员P锁并发超出统一预算")
			}
		}
		require.Equal(t, 1, wins)
		var count int
		require.NoError(t, s.DB.QueryRowContext(ctx, "SELECT COUNT(DISTINCT formation_id) FROM match_authority_ticket WHERE player_id IN (?,?) AND state=2", a.PlayerId, b.PlayerId).Scan(&count))
		require.Equal(t, 1, count)
	}
}
