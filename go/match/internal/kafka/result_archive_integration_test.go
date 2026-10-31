package kafka

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"match/internal/authority"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	battlepb "proto/battle"
	kafkapb "proto/contracts/kafka"
	matchpb "proto/match"
	"schemamigrate"
)

// 连接独立本地三副本Kafka和真实TiDB，核对关闭reader后的broker位点，而非替身计数。
// 本切片只保全来源，测试不投递个人资产，也不驱动旧R07销账。
func TestCompletionKafkaTiDBArchiveBeforeOffsetRecovery(t *testing.T) {
	if os.Getenv("MATCH_IT_RESULT_KAFKA") == "" || os.Getenv("MATCH_IT_TIDB") == "" {
		t.Skip("设置MATCH_IT_RESULT_KAFKA与MATCH_IT_TIDB启用隔离三broker来源恢复测试")
	}
	brokers := strings.Split(os.Getenv("MATCH_IT_RESULT_KAFKA"), ",")
	require.Equal(t, []string{"127.0.0.1:19492", "127.0.0.1:19493", "127.0.0.1:19494"}, brokers, "只允许专用本地测试集群")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, dbName := completionIntegrationStore(t, ctx)
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	topic, group := "codex-completion-"+id, "codex-completion-group-"+id
	t.Logf("隔离资源 database=%s topic=%s group=%s brokers=%s", dbName, topic, group, strings.Join(brokers, ","))
	transport := &kafkago.Transport{DialTimeout: 3 * time.Second}
	t.Cleanup(transport.CloseIdleConnections)
	client := &kafkago.Client{Addr: kafkago.TCP(brokers...), Transport: transport, Timeout: 8 * time.Second}
	require.NoError(t, EnsureResultTopic(ResultConsumerConfig{Brokers: brokers, Topic: topic, GroupID: group, Partitions: 1, RequireDurability: true}))
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		deleted, err := client.DeleteGroups(cleanup, &kafkago.DeleteGroupsRequest{GroupIDs: []string{group}})
		if err != nil {
			t.Errorf("清理本测试消费组失败: %v", err)
		} else if err := deleted.Errors[group]; err != nil {
			t.Errorf("清理本测试消费组失败: %v", err)
		}
		topics, err := client.DeleteTopics(cleanup, &kafkago.DeleteTopicsRequest{Topics: []string{topic}})
		if err != nil {
			t.Errorf("清理本测试主题失败: %v", err)
		} else if err := topics.Errors[topic]; err != nil {
			t.Errorf("清理本测试主题失败: %v", err)
		}
	})
	event := completionIntegrationEvent()
	raw := mustResultBytes(t, event)
	// 合法的重复标量使原字节与重序列化不同，防止测试误把重编码当原件。
	raw = protowire.AppendTag(raw, 1, protowire.VarintType)
	raw = protowire.AppendVarint(raw, event.BattleId)
	require.False(t, bytes.Equal(raw, mustResultBytes(t, event)))
	writer := &kafkago.Writer{Addr: kafkago.TCP(brokers...), Topic: topic, RequiredAcks: kafkago.RequireAll, Balancer: &kafkago.Hash{}, MaxAttempts: 1, WriteTimeout: 8 * time.Second}
	t.Cleanup(func() { require.NoError(t, writer.Close()) })
	publish := func() {
		require.NoError(t, writer.WriteMessages(ctx, kafkago.Message{Key: []byte(strconv.FormatUint(event.BattleId, 10)), Value: raw}))
	}
	publish()
	metadata, err := client.Metadata(ctx, &kafkago.MetadataRequest{Topics: []string{topic}})
	require.NoError(t, err)
	require.Len(t, metadata.Brokers, 3)
	require.Len(t, metadata.Topics, 1)
	require.NoError(t, metadata.Topics[0].Error)
	require.Len(t, metadata.Topics[0].Partitions, 1)
	partition := metadata.Topics[0].Partitions[0]
	require.NoError(t, partition.Error)
	require.Len(t, partition.Replicas, 3)
	require.Len(t, partition.Isr, 3, "恢复验收在三副本均同步后进行")
	configs, err := client.DescribeConfigs(ctx, &kafkago.DescribeConfigsRequest{Resources: []kafkago.DescribeConfigRequestResource{{ResourceType: kafkago.ResourceTypeTopic, ResourceName: topic, ConfigNames: []string{"min.insync.replicas"}}}})
	require.NoError(t, err)
	require.Len(t, configs.Resources, 1)
	require.NoError(t, configs.Resources[0].Error)
	require.Len(t, configs.Resources[0].ConfigEntries, 1)
	require.Equal(t, "min.insync.replicas", configs.Resources[0].ConfigEntries[0].ConfigName)
	require.Equal(t, "2", configs.Resources[0].ConfigEntries[0].ConfigValue)
	t.Logf("专用主题确认：cluster=%s brokers=3 replicas=3 ISR=3 min.insync.replicas=2 producer_acks=all", metadata.ClusterID)
	newReader := func() *kafkago.Reader {
		return kafkago.NewReader(kafkago.ReaderConfig{Brokers: brokers, Topic: topic, GroupID: group, StartOffset: kafkago.FirstOffset, MinBytes: 1, MaxBytes: 1 << 20, MaxWait: 500 * time.Millisecond, CommitInterval: 0, SessionTimeout: 6 * time.Second, HeartbeatInterval: time.Second, RebalanceTimeout: 10 * time.Second})
	}
	offset := func() int64 {
		response, err := client.OffsetFetch(ctx, &kafkago.OffsetFetchRequest{GroupID: group, Topics: map[string][]int{topic: {0}}})
		require.NoError(t, err)
		require.NoError(t, response.Error)
		require.Len(t, response.Topics[topic], 1)
		part := response.Topics[topic][0]
		require.NoError(t, part.Error)
		require.Equal(t, 0, part.Partition)
		return part.CommittedOffset
	}

	// 实际已关闭的SQL连接池使三次来源归档失败；评分不得运行，原位点不能提交。
	unavailableConfig := mysql.NewConfig()
	unavailableConfig.Net, unavailableConfig.Addr, unavailableConfig.User, unavailableConfig.DBName = "tcp", os.Getenv("MATCH_IT_TIDB"), "root", dbName
	unavailable, err := sql.Open("mysql", unavailableConfig.FormatDSN())
	require.NoError(t, err)
	require.NoError(t, unavailable.Close())
	failedStore := &authority.Store{DB: unavailable}
	failedReader := newReader()
	t.Cleanup(func() { require.NoError(t, failedReader.Close()) })
	failed := &completionIntegrationReader{Reader: failedReader}
	attempts := 0
	runResultConsumer(ctx, failed, func(context.Context, *kafkapb.BattleResultEvent) error {
		t.Fatal("来源SQL失败时评分不能先运行")
		return nil
	}, sleepCtx, func(call context.Context, got *kafkapb.BattleResultEvent, body []byte, source string, part int, off int64) error {
		attempts++
		require.Equal(t, raw, body)
		return failedStore.ArchiveCompletion(call, got, body, source, part, off)
	})
	require.NoError(t, failedReader.Close())
	require.NoError(t, ctx.Err())
	require.Equal(t, 3, attempts)
	require.Zero(t, failed.commits)
	require.Len(t, failed.fetched, 1)
	require.Equal(t, int64(0), failed.fetched[0].Offset)
	require.Equal(t, int64(-1), offset(), "三次失败并关闭reader后broker仍无已提交位点")
	completionIntegrationAssertRows(t, ctx, store.DB, 0, 0)
	t.Log("来源失败3次后reader关闭：broker committed_offset=-1，SQL两张来源表均0行")

	// 同组新reader必须恢复同一原件；归档事务已可见，才允许真实提交位点。
	runRecovered := func(expectedOffset int64) {
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		r := newReader()
		defer func() { require.NoError(t, r.Close()) }()
		observed := &completionIntegrationReader{Reader: r}
		observed.beforeCommit = func(message kafkago.Message) {
			previous := expectedOffset
			if expectedOffset == 0 {
				previous = -1
			}
			require.Equal(t, previous, offset(), "SQL归档前不能提前提交位点")
			completionIntegrationAssertArchived(t, ctx, store.DB, event, raw, topic)
			require.Equal(t, expectedOffset, message.Offset)
		}
		observed.afterCommit = stop
		rated, archived := 0, 0
		runResultConsumer(runCtx, observed, func(context.Context, *kafkapb.BattleResultEvent) error {
			rated++
			require.Equal(t, 1, archived)
			return nil // 等价于评分关闭或不计分，完整来源仍必须归档。
		}, sleepCtx, func(call context.Context, got *kafkapb.BattleResultEvent, body []byte, source string, part int, off int64) error {
			archived++
			require.Equal(t, raw, body)
			require.Equal(t, expectedOffset, off)
			return store.ArchiveCompletion(call, got, body, source, part, off)
		})
		require.NoError(t, ctx.Err())
		require.Equal(t, 1, rated)
		require.Equal(t, 1, archived)
		require.Equal(t, 1, observed.commits)
		require.Len(t, observed.fetched, 1)
		require.Equal(t, raw, observed.fetched[0].Value)
		require.Equal(t, expectedOffset+1, offset())
	}
	runRecovered(0)
	t.Log("同组新reader恢复offset=0原件：原bytes/SHA及全员来源先同事务可见，再提交broker offset=1")
	publish()
	runRecovered(1)
	completionIntegrationAssertRows(t, ctx, store.DB, 1, len(event.Settlements))
	t.Log("同原件再次投递offset=1：仅保留首份来源位点0、主记录1行、个人来源2行，broker offset=2")
}

type completionIntegrationReader struct {
	*kafkago.Reader
	fetched      []kafkago.Message
	commits      int
	beforeCommit func(kafkago.Message)
	afterCommit  context.CancelFunc
}

func (r *completionIntegrationReader) FetchMessage(ctx context.Context) (kafkago.Message, error) {
	m, err := r.Reader.FetchMessage(ctx)
	if err == nil {
		r.fetched = append(r.fetched, m)
	}
	return m, err
}

func (r *completionIntegrationReader) CommitMessages(ctx context.Context, messages ...kafkago.Message) error {
	if r.beforeCommit != nil {
		for _, message := range messages {
			r.beforeCommit(message)
		}
	}
	if err := r.Reader.CommitMessages(ctx, messages...); err != nil {
		return err
	}
	r.commits++
	if r.afterCommit != nil {
		r.afterCommit()
	}
	return nil
}

func completionIntegrationStore(t *testing.T, ctx context.Context) (*authority.Store, string) {
	t.Helper()
	address := os.Getenv("MATCH_IT_TIDB")
	host, portText, err := net.SplitHostPort(address)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip)
	require.True(t, ip.IsLoopback())
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	require.NotZero(t, port)
	c := mysql.NewConfig()
	c.Net, c.Addr, c.User = "tcp", address, "root"
	c.Timeout, c.ReadTimeout, c.WriteTimeout = 3*time.Second, 8*time.Second, 8*time.Second
	c.Params = map[string]string{"tidb_txn_mode": "'pessimistic'", "transaction_isolation": "'READ-COMMITTED'", "tidb_read_staleness": "0", "tidb_low_resolution_tso": "OFF", "tidb_read_consistency": "'strict'", "tidb_replica_read": "'leader'", "tidb_snapshot": "''"}
	admin, err := sql.Open("mysql", c.FormatDSN())
	require.NoError(t, err)
	name := "matchit_result_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`")
	require.NoError(t, err)
	c.DBName = name
	db, err := sql.Open("mysql", c.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_, err := admin.ExecContext(cleanup, "DROP DATABASE `"+name+"`")
		if err != nil {
			t.Errorf("清理本测试隔离库失败: %v", err)
		}
		require.NoError(t, admin.Close())
	})
	var version string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version))
	require.Contains(t, strings.ToLower(version), "tidb")
	_, err = schemamigrate.Up(ctx, db, schemamigrate.Options{Database: name, Tables: []proto.Message{&matchpb.MatchBattleCompletionRecord{}, &matchpb.MatchBattleSettlementSourceRecord{}}})
	require.NoError(t, err)
	return &authority.Store{DB: db}, name
}

func completionIntegrationAssertRows(t *testing.T, ctx context.Context, db *sql.DB, complete, personal int) {
	t.Helper()
	var got int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_completion").Scan(&got))
	require.Equal(t, complete, got)
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_settlement_source").Scan(&got))
	require.Equal(t, personal, got)
}

func completionIntegrationAssertArchived(t *testing.T, ctx context.Context, db *sql.DB, event *kafkapb.BattleResultEvent, raw []byte, topic string) {
	t.Helper()
	completionIntegrationAssertRows(t, ctx, db, 1, len(event.Settlements))
	var body, digest []byte
	var source string
	var part int
	var off int64
	require.NoError(t, db.QueryRowContext(ctx, "SELECT raw_payload,payload_digest,source_topic,source_partition,source_offset FROM match_battle_completion WHERE battle_id=?", event.BattleId).Scan(&body, &digest, &source, &part, &off))
	require.Equal(t, raw, body)
	sum := sha256.Sum256(raw)
	require.Equal(t, sum[:], digest)
	require.Equal(t, topic, source)
	require.Zero(t, part)
	require.Zero(t, off, "幂等重放不能覆盖首次归档来源位点")
	for _, expected := range event.Settlements {
		require.NoError(t, db.QueryRowContext(ctx, "SELECT settlement_proto,payload_digest FROM match_battle_settlement_source WHERE battle_id=? AND player_id=?", event.BattleId, expected.PlayerId).Scan(&body, &digest))
		var got battlepb.BattleSettlementData
		require.NoError(t, proto.Unmarshal(body, &got))
		require.True(t, proto.Equal(expected, &got), "全部个人资产字段必须完整保留")
		personal := sha256.Sum256(body)
		require.Equal(t, personal[:], digest)
	}
}

func completionIntegrationEvent() *kafkapb.BattleResultEvent {
	return &kafkapb.BattleResultEvent{
		BattleId: 901, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW, TotalRounds: 8, FinishedAtMs: 123456,
		SettlementFormatVersion: 1,
		Teams:                   []*kafkapb.BattleResultTeam{{TeamIndex: 0, PlayerIds: []uint64{11, 22}}},
		Settlements: []*battlepb.BattleSettlementData{
			{BattleId: 901, PlayerId: 11, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW, PlayerTeamIndex: 0, TotalRounds: 8, GoldGain: 5, ExpGain: 8, Health: 100, Mana: 75,
				ItemsConsumed: []*battlepb.BattleItemEntry{{ItemTableId: 31, Count: 1}}, ItemsGained: []*battlepb.BattleItemEntry{{ItemTableId: 32, Count: 2}},
				Pets: []*battlepb.BattlePetSettlementData{{PetId: 41, Health: 50, Mana: 20}}, DefeatedMonsters: []*battlepb.BattleMonsterDefeat{{MonsterConfigId: 51, Count: 1}}},
			{BattleId: 901, PlayerId: 22, Outcome: battlepb.EBattleOutcome_BATTLE_OUTCOME_DRAW, PlayerTeamIndex: 0, TotalRounds: 8, Health: 25, Mana: 3},
		},
	}
}

var _ resultReader = (*completionIntegrationReader)(nil)
