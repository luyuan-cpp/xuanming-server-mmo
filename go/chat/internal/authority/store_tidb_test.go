package authority

import (
	"context"
	"database/sql"
	"fmt"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
	"shared/placement"
	"strings"
	"sync"
	"testing"
	"time"

	"chat/internal/constants"
	chatpb "proto/chat"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func sendRequest(id string, peer uint64) *chatpb.SendChatRequest {
	channel := chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD
	if peer != 0 {
		channel = chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE
	}
	return &chatpb.SendChatRequest{RequestId: id, Message: &chatpb.ChatMessage{Channel: channel, TargetPlayerId: peer, SenderPlayerId: 999, SendTimeMs: 1, Content: "广告 原消息"}}
}
func original(t *testing.T, id uint64) context.Context {
	return actor(t, id, fmt.Sprintf("original-%d", id), 7)
}
func pull(t *testing.T, s *Store, ctx context.Context, peer uint64) []*chatpb.ChatMessage {
	t.Helper()
	channel := chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD
	if peer != 0 {
		channel = chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE
	}
	messages, err := s.Pull(ctx, &chatpb.PullChatHistoryRequest{Channel: channel, PeerPlayerId: peer}, 50)
	require.NoError(t, err)
	return messages
}
func count(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM chat_send_authority").Scan(&n))
	return n
}

// 副本重新走生产连接入口；共享事务域，不共享进程内 Store 或 SQL 池。
func independentStore(t *testing.T, s *Store, fixture *sqltest.Fixture) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var database string
	require.NoError(t, s.db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database))
	require.Regexp(t, `^chat_it_[0-9a-f]{32}$`, database)
	db, err := fixture.Open(ctx, database, 8)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	first, err := s.db.Conn(ctx)
	require.NoError(t, err)
	defer first.Close()
	second, err := db.Conn(ctx)
	require.NoError(t, err)
	defer second.Close()
	var firstID, secondID uint64
	require.NoError(t, first.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&firstID))
	require.NoError(t, second.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&secondID))
	require.NotEqual(t, firstID, secondID, "副本必须持有不同物理连接")
	peer, err := New(ctx, db, s.roleSchema, authoritysql.BackendTiDB, s.rules)
	require.NoError(t, err)
	return peer
}

func TestChatTiDBSQLSendPullPreserveOriginalAndIgnoreRedis(t *testing.T) {
	s, fixture := nativeStoreFixture(t, false)
	ctx := original(t, 11)
	in := sendRequest("original", 22)
	before := proto.Clone(in)
	r, err := s.Send(ctx, in)
	require.NoError(t, err)
	require.Zero(t, r.Tip)
	require.False(t, r.Duplicate)
	require.True(t, proto.Equal(before, in))
	for _, v := range []struct{ self, peer uint64 }{{11, 22}, {22, 11}} {
		messages := pull(t, s, original(t, v.self), v.peer)
		require.Len(t, messages, 1)
		require.EqualValues(t, 11, messages[0].SenderPlayerId)
		require.Equal(t, "** 原消息", messages[0].Content)
		require.Greater(t, messages[0].SendTimeMs, int64(1))
	}
	// 故意没有任何Redis依赖：重建Store实例仍从同一SQL原件读取完整窗口。
	restarted := independentStore(t, s, fixture)
	r, err = restarted.Send(ctx, in)
	require.NoError(t, err)
	require.True(t, r.Duplicate)
	require.Equal(t, 1, count(t, s))
	require.Len(t, pull(t, restarted, ctx, 22), 1)
	changed := proto.Clone(in).(*chatpb.SendChatRequest)
	changed.Message.Content = "原ID换正文"
	_, err = s.Send(ctx, changed)
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, 1, count(t, s))
}

func TestChatTiDBOldOwnerTransferABAAndFreezeRejectAllEntrances(t *testing.T) {
	for _, kind := range []string{"transfer", "same-account-ABA", "freeze"} {
		t.Run(kind, func(t *testing.T) {
			s, fixture := nativeStoreFixture(t, false)
			peer := independentStore(t, s, fixture)
			old := original(t, 11)
			_, err := s.Send(old, sendRequest("old", 22))
			require.NoError(t, err)
			account, revision, listing := "original-11", uint64(7), uint64(0)
			switch kind {
			case "transfer":
				account, revision = "new-owner", 8
			case "same-account-ABA":
				revision = 9
			case "freeze":
				listing = 99
			}
			_, err = peer.db.Exec("UPDATE account_role SET account=?,owner_revision=?,trade_listing_id=? WHERE player_id=11", []byte(account), revision, listing)
			require.NoError(t, err)
			for _, id := range []string{"old", "late-new"} {
				for _, replica := range []*Store{s, peer} {
					_, err = replica.Send(old, sendRequest(id, 22))
					require.ErrorIs(t, err, ErrOriginal)
				}
			}
			_, err = s.Pull(old, &chatpb.PullChatHistoryRequest{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, PeerPlayerId: 22}, 20)
			require.ErrorIs(t, err, ErrOriginal)
			require.Equal(t, 1, count(t, s))
			if kind != "freeze" {
				current := actor(t, 11, account, revision)
				require.Len(t, pull(t, peer, current, 22), 1, "合法新owner保留role-id历史")
				_, err = s.Send(current, sendRequest("old", 22))
				require.ErrorIs(t, err, ErrOriginal)
				_, err = s.Send(current, sendRequest("new-owner", 22))
				require.NoError(t, err)
				require.Len(t, pull(t, s, current, 22), 2)
			}
		})
	}
}

func TestChatTiDBConcurrentSameOriginalCommitsOnce(t *testing.T) {
	s, fixture := nativeStoreFixture(t, false)
	peer := independentStore(t, s, fixture)
	ctx := original(t, 11)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := range 8 {
		replica := []*Store{s, peer}[i%2]
		wg.Add(1)
		go func() { defer wg.Done(); _, err := replica.Send(ctx, sendRequest("concurrent", 22)); results <- err }()
	}
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.Equal(t, 1, count(t, s))
	require.Len(t, pull(t, peer, ctx, 22), 1)
	changed := sendRequest("concurrent", 22)
	changed.Message.Content = "同原件不同正文"
	_, err := peer.Send(ctx, changed)
	require.ErrorIs(t, err, ErrConflict)
	restored := independentStore(t, peer, fixture)
	replayed, err := restored.Send(ctx, sendRequest("concurrent", 22))
	require.NoError(t, err)
	require.True(t, replayed.Duplicate)
	require.Len(t, pull(t, restored, ctx, 22), 1)
	conflicts := make(chan error, 2)
	start := make(chan struct{})
	for i, replica := range []*Store{s, peer} {
		request := sendRequest("concurrent-conflict", 22)
		request.Message.Content = fmt.Sprintf("不同正文-%d", i)
		go func() { <-start; _, err := replica.Send(ctx, request); conflicts <- err }()
	}
	close(start)
	accepted, rejected := 0, 0
	for range 2 {
		err := <-conflicts
		if err == nil {
			accepted++
		} else {
			require.ErrorIs(t, err, ErrConflict)
			rejected++
		}
	}
	require.Equal(t, 1, accepted)
	require.Equal(t, 1, rejected)
	require.Equal(t, 2, count(t, restored))
}

func TestChatTiDBSQLRateRejectDoesNotConsumeUUID(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	ctx := original(t, 11)
	// 只控制时间接缝使固定秒边界确定；TiDB角色/route/唯一行与事务均为真实实现。
	now := uint64(100000)
	s.clock = func(context.Context, *sql.Tx) (uint64, error) { return now, nil }
	for i := range 5 {
		r, err := s.Send(ctx, sendRequest(fmt.Sprintf("rate-%d", i), 0))
		require.NoError(t, err)
		require.Zero(t, r.Tip)
	}
	r, err := s.Send(ctx, sendRequest("sixth", 0))
	require.NoError(t, err)
	require.Equal(t, constants.ErrRateLimited, r.Tip)
	require.Equal(t, 5, count(t, s))
	now += 1000
	r, err = s.Send(ctx, sendRequest("sixth", 0))
	require.NoError(t, err)
	require.Zero(t, r.Tip)
	require.Equal(t, 6, count(t, s))
}

func TestChatTiDBCallerCanReadHistoryAfterPeerDeletion(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	ctx := original(t, 11)
	_, err := s.Send(ctx, sendRequest("peer-gone", 22))
	require.NoError(t, err)
	_, err = s.db.Exec("DELETE FROM account_role WHERE player_id=22")
	require.NoError(t, err)
	_, err = s.db.Exec("DELETE FROM player_route WHERE player_id=22")
	require.NoError(t, err)
	require.Len(t, pull(t, s, ctx, 22), 1)
	_, err = s.Send(ctx, sendRequest("peer-still-selector", 22))
	require.NoError(t, err)
	require.Len(t, pull(t, s, ctx, 22), 2)
}

// 校验是不加锁的普通读:别的事务持着路由行和角色行的锁未提交时,聊天不排队,读到的是已提交的旧归属;
// 过户提交之后(TTL=0)立刻拒绝。
func TestChatTiDBUncommittedTransferDoesNotBlockChatThenRejectsAfterCommit(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	ctx := original(t, 11)
	tx, err := s.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer tx.Rollback()
	var rev uint64
	require.NoError(t, tx.QueryRow("SELECT rev FROM player_route WHERE route_shard=? AND player_id=11 FOR UPDATE", placement.RouteRowShard(11)).Scan(&rev))
	_, err = tx.Exec("UPDATE account_role SET account='new-owner',owner_revision=8 WHERE player_id=11")
	require.NoError(t, err)

	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = s.Send(bounded, sendRequest("before-commit", 22))
	require.NoError(t, err, "过户持锁未提交时聊天不排队")
	_, err = s.Pull(bounded, &chatpb.PullChatHistoryRequest{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, PeerPlayerId: 22}, 20)
	require.NoError(t, err, "拉取同样不排队")

	require.NoError(t, tx.Commit())
	_, err = s.Send(ctx, sendRequest("after-commit", 22))
	require.ErrorIs(t, err, ErrOriginal)
	_, err = s.Pull(ctx, &chatpb.PullChatHistoryRequest{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, PeerPlayerId: 22}, 20)
	require.ErrorIs(t, err, ErrOriginal)
	require.Equal(t, 1, count(t, s))
}

// 发送事务只碰聊天自己的表:发送卡在提交之前时过户照常完成,不等它。这条消息按校验时的旧归属落库,
// 这是校验不再和过户串行的代价(actor_verify.go),消息记在角色名下,新主能看到。
func TestChatTiDBInFlightSendDoesNotBlockTransferAndHistoryStaysWithRole(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	ctx, cancel := context.WithTimeout(original(t, 11), 10*time.Second)
	defer cancel()
	inTx := make(chan struct{})
	release := make(chan struct{})
	clock := s.clock
	s.clock = func(ctx context.Context, tx *sql.Tx) (uint64, error) {
		close(inTx)
		select {
		case <-release:
			return clock(ctx, tx)
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	sent := make(chan error, 1)
	go func() { _, err := s.Send(ctx, sendRequest("in-flight", 22)); sent <- err }()
	select {
	case <-inTx:
	case err := <-sent:
		t.Fatalf("发送尚未进入事务即返回：%v", err)
	case <-ctx.Done():
		t.Fatal("发送未进入事务")
	}
	_, err := s.db.ExecContext(ctx, "UPDATE account_role SET account='new-owner',owner_revision=8 WHERE player_id=11")
	require.NoError(t, err, "发送事务不持角色锁,过户不等它")
	close(release)
	select {
	case err := <-sent:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("发送未收敛")
	}
	s.clock = clock
	require.Len(t, pull(t, s, actor(t, 11, "new-owner", 8), 22), 1, "历史属于角色,新主能看到")
	_, err = s.Send(ctx, sendRequest("late-old", 22))
	require.ErrorIs(t, err, ErrOriginal)
	require.Equal(t, 1, count(t, s))
}

// TTL 内旧会话还能说话(写明的代价),过期后被拒;拒绝不进缓存,新主用新 revision 不受旧条目影响。
func TestChatTiDBActorVerifyTTLBoundsOldSessionWindow(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	var now time.Duration
	s.actors = newActorVerifyCache(30*time.Second, actorVerifyMaxEntries, func() time.Duration { return now })
	old := original(t, 11)
	_, err := s.Send(old, sendRequest("first", 22))
	require.NoError(t, err)
	_, err = s.db.Exec("UPDATE account_role SET account='new-owner',owner_revision=8 WHERE player_id=11")
	require.NoError(t, err)

	now += 29 * time.Second
	_, err = s.Send(old, sendRequest("inside-ttl", 22))
	require.NoError(t, err, "TTL 内不重读,旧会话还能发")
	require.Len(t, pull(t, s, old, 22), 2)

	now += 2 * time.Second
	_, err = s.Send(old, sendRequest("after-ttl", 22))
	require.ErrorIs(t, err, ErrOriginal, "过期后重读,旧会话被拒")
	_, err = s.Send(old, sendRequest("still-rejected", 22))
	require.ErrorIs(t, err, ErrOriginal, "拒绝不写缓存")

	current := actor(t, 11, "new-owner", 8)
	_, err = s.Send(current, sendRequest("new-owner-first", 22))
	require.NoError(t, err)
	require.Equal(t, 3, count(t, s))
}

// 过户之后,原 request_id 的重放按原件里的归属判定,TTL 窗口不能让旧原件换主。
func TestChatTiDBActorVerifyTTLDoesNotLetNewOwnerReplayOldReceipt(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	var now time.Duration
	s.actors = newActorVerifyCache(30*time.Second, actorVerifyMaxEntries, func() time.Duration { return now })
	_, err := s.Send(original(t, 11), sendRequest("receipt", 22))
	require.NoError(t, err)
	_, err = s.db.Exec("UPDATE account_role SET account='new-owner',owner_revision=8 WHERE player_id=11")
	require.NoError(t, err)
	_, err = s.Send(actor(t, 11, "new-owner", 8), sendRequest("receipt", 22))
	require.ErrorIs(t, err, ErrOriginal)
	require.Equal(t, 1, count(t, s))
}

func TestChatTiDBSQLWindowExpiryGapAndMaximumRemainBounded(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	s.rules.RateLimitPerSecond = 100
	s.rules.HistoryTTLSeconds = 10
	s.rules.HistoryMaxEntries = 3
	ctx := original(t, 11)
	now := uint64(100000)
	s.clock = func(context.Context, *sql.Tx) (uint64, error) { return now, nil }
	for i := range 4 {
		now += 1000
		_, err := s.Send(ctx, sendRequest(fmt.Sprintf("window-%d", i), 0))
		require.NoError(t, err)
	}
	got := pull(t, s, ctx, 0)
	require.Len(t, got, 3)
	require.Greater(t, got[0].SendTimeMs, got[1].SendTimeMs)
	now += 10000
	require.Empty(t, pull(t, s, ctx, 0), "闲置TTL到点窗口为空")
	_, err := s.Send(ctx, sendRequest("after-expiry", 0))
	require.NoError(t, err)
	got = pull(t, s, ctx, 0)
	require.Len(t, got, 1, "过期后新发送不复活旧窗口")
	// 旧UUID的永久收据不随显示TTL删除，也不会刷新历史窗口。
	r, err := s.Send(ctx, sendRequest("window-0", 0))
	require.NoError(t, err)
	require.True(t, r.Duplicate)
	require.Len(t, pull(t, s, ctx, 0), 1)
}

func TestChatTiDBFrozenRouteRejectsBeforeAnyChatRow(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	_, err := s.db.Exec("UPDATE player_route SET frozen_run_id='chat-frozen' WHERE player_id=11")
	require.NoError(t, err)
	_, err = s.Send(original(t, 11), sendRequest("denied", 22))
	require.Error(t, err)
	_, err = s.Pull(original(t, 11), &chatpb.PullChatHistoryRequest{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD}, 20)
	require.Error(t, err)
	require.Zero(t, count(t, s))
}

func TestChatTiDBPolicyMismatchAndMissingIndexFailClosed(t *testing.T) {
	boundary := fixtureRules()
	boundary.HistoryMaxLimit = 200
	boundary.HistoryMaxEntries = 1000
	s, fixture := nativeStoreFixture(t, false, boundary)
	tooLarge := boundary
	tooLarge.HistoryMaxLimit = 201
	_, err := New(context.Background(), s.db, s.roleSchema, authoritysql.BackendTiDB, tooLarge)
	require.ErrorIs(t, err, ErrInvalid)
	rules := s.rules
	rules.RateLimitPerSecond++
	_, err = New(context.Background(), s.db, s.roleSchema, authoritysql.BackendTiDB, rules)
	require.ErrorIs(t, err, ErrPolicy)
	install, err := fixture.OpenSchema(context.Background(), s.roleSchema)
	require.NoError(t, err)
	_, err = install.Exec("ALTER TABLE chat_send_authority DROP INDEX idx_chat_stream")
	require.NoError(t, err)
	_, err = New(context.Background(), s.db, s.roleSchema, authoritysql.BackendTiDB, s.rules)
	require.ErrorContains(t, err, "索引")
}

func TestChatTiDBUnsignedPlayerAndOriginalBodySurviveRestart(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	id := ^uint64(0)
	ctx := original(t, id)
	in := sendRequest("unsigned", 11)
	in.Message.Content = strings.Repeat("x", 512)
	r, err := s.Send(ctx, in)
	require.NoError(t, err)
	require.Zero(t, r.Tip)
	messages := pull(t, s, ctx, 11)
	require.Len(t, messages, 1)
	require.Equal(t, id, messages[0].SenderPlayerId)
	var raw []byte
	require.NoError(t, s.db.QueryRow("SELECT request_payload FROM chat_send_authority WHERE player_id=? AND request_id=?", id, []byte("unsigned")).Scan(&raw))
	saved := &chatpb.SendChatRequest{}
	require.NoError(t, proto.Unmarshal(raw, saved))
	require.True(t, proto.Equal(in, saved))
}

func TestChatTiDBMissingOriginalAndCorruptReceiptFailClosed(t *testing.T) {
	s, _ := nativeStoreFixture(t, false)
	in := sendRequest("permanent", 22)
	_, err := s.Send(context.Background(), in)
	require.ErrorIs(t, err, ErrOriginal)
	_, err = s.Pull(context.Background(), &chatpb.PullChatHistoryRequest{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, PeerPlayerId: 22}, 20)
	require.ErrorIs(t, err, ErrOriginal)
	require.Zero(t, count(t, s))
	ctx := original(t, 11)
	_, err = s.Send(ctx, in)
	require.NoError(t, err)
	_, err = s.db.Exec("UPDATE chat_send_authority SET message_payload=? WHERE player_id=11 AND request_id=?", []byte{0xff}, []byte(in.RequestId))
	require.NoError(t, err)
	_, err = s.Send(ctx, in)
	require.ErrorContains(t, err, "原件")
	messages, err := s.Pull(ctx, &chatpb.PullChatHistoryRequest{Channel: chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, PeerPlayerId: 22}, 20)
	require.Error(t, err)
	require.Empty(t, messages)
	require.Equal(t, 1, count(t, s))
}
