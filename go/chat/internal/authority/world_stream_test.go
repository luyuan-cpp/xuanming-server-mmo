package authority

import (
	"database/sql"
	"fmt"
	"shared/authoritysql"
	"testing"

	"chat/internal/constants"

	"github.com/stretchr/testify/require"
)

// 世界频道按发送者分段:段数固定,一名玩家只落在一段,私聊的流键不变。
func TestWorldStreamKeyShardsBySender(t *testing.T) {
	distinct := map[string]bool{}
	for _, key := range worldStreamKeys {
		distinct[key] = true
	}
	require.Len(t, distinct, constants.WorldStreamShards, "每一段一个不同的流键")
	require.Equal(t, "world-v3:3", worldStreamKey(11), "11 % 8 = 3")
	require.Equal(t, worldStreamKey(11), worldStreamKey(19), "同余的玩家同一段")
	require.NotEqual(t, worldStreamKey(11), worldStreamKey(12))
	require.Equal(t, worldStreamKey(11), streamKey(11, 0), "世界频道的流键只看发送者")
	require.Equal(t, "private-v2:11:22", streamKey(11, 22))
	require.Equal(t, streamKey(11, 22), streamKey(22, 11), "私聊双方同一个流键")
	for sender := uint64(0); sender < 64; sender++ {
		require.Equal(t, fmt.Sprintf("world-v3:%d", sender%constants.WorldStreamShards), worldStreamKey(sender))
	}
}

// 多段合并必须和单段查询同一个顺序(接受时刻、发送者、请求号都从大到小),并且只留最新的 limit 行:
// 可见窗口的截断规则(visibleHistory)按这个顺序逐行走。
func TestNewestRowsMergesShardsInSingleStreamOrder(t *testing.T) {
	row := func(ms, sender uint64, id string) historyRow {
		return historyRow{acceptedMs: ms, sender: sender, requestID: []byte(id)}
	}
	// 三段各自已按新到旧排好,像三次单段查询的结果拼在一起。
	rows := []historyRow{
		row(300, 8, "a"), row(100, 8, "a"),
		row(300, 9, "b"), row(300, 9, "a"), row(200, 9, "z"),
		row(400, 10, "a"),
	}
	got := newestRows(rows, 4)
	want := []historyRow{row(400, 10, "a"), row(300, 9, "b"), row(300, 9, "a"), row(300, 8, "a")}
	require.Equal(t, want, got)
	require.Len(t, newestRows([]historyRow{row(1, 1, "a")}, 4), 1, "不足 limit 行时全留")
	require.Empty(t, newestRows(nil, 4))
}

// 保留期列固定写 UTC,带毫秒。DATETIME 不带时区,不能跟着进程或会话的时区走。
func TestAcceptedAtIsUTCWithMilliseconds(t *testing.T) {
	require.Equal(t, "1970-01-01 00:01:40.000", acceptedAt(100000))
	require.Equal(t, "2026-11-12 00:00:00.123", acceptedAt(1794441600123))
}

// 建表语句自带开着的 TTL 属性;识别函数认得 TiDB 的 SHOW CREATE TABLE 输出,关掉、改了天数或根本没有时都判不在。
func TestRetentionAttributesAreRecognised(t *testing.T) {
	require.True(t, hasRetentionAttributes(sendDDL), "建表语句必须带保留期属性")
	tidbOutput := "CREATE TABLE `chat_send_authority` (\n  `accepted_at` datetime(3) NOT NULL,\n  PRIMARY KEY (`player_id`,`request_id`) /*T![clustered_index] CLUSTERED */\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin /*T![ttl] TTL=`accepted_at` + INTERVAL 30 DAY */ /*T![ttl] TTL_ENABLE='ON' */ /*T![ttl] TTL_JOB_INTERVAL='24h' */"
	require.True(t, hasRetentionAttributes(tidbOutput))
	for name, ddl := range map[string]string{
		"关掉":   "/*T![ttl] TTL=`accepted_at` + INTERVAL 30 DAY */ /*T![ttl] TTL_ENABLE='OFF' */",
		"改了天数": "/*T![ttl] TTL=`accepted_at` + INTERVAL 7 DAY */ /*T![ttl] TTL_ENABLE='ON' */",
		"别的列":  "/*T![ttl] TTL=`created_at` + INTERVAL 30 DAY */ /*T![ttl] TTL_ENABLE='ON' */",
		"没有":   "CREATE TABLE `chat_send_authority` (`accepted_at` datetime(3) NOT NULL) ENGINE=InnoDB",
	} {
		require.False(t, hasRetentionAttributes(ddl), name)
	}
}

// 发言间隔与可见窗口越界在装配时就拒绝,不等第一次请求。数据库句柄只为过前面的非空判断,这些规则在连库之前就被拒掉。
func TestNewRejectsCooldownAndHistoryWindowOutOfRange(t *testing.T) {
	db, err := sql.Open("mysql", "chat:unused@tcp(127.0.0.1:1)/chat_unused")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for name, change := range map[string]func(*Rules){
		"发言间隔为负":      func(r *Rules) { r.WorldChannelCooldownMs = -1 },
		"发言间隔超过上限":    func(r *Rules) { r.WorldChannelCooldownMs = constants.MaxWorldChannelCooldownMs + 1 },
		"可见窗口长过原件保留期": func(r *Rules) { r.HistoryTTLSeconds = constants.MaxHistoryTTLSeconds + 1 },
	} {
		rules := fixtureRules()
		change(&rules)
		_, err = New(t.Context(), db, newFixtureRoleStates(), authoritysql.BackendTiDB, rules)
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}
