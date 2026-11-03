package com.game.mall;

import static org.junit.jupiter.api.Assertions.*;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.ResultSet;
import java.sql.Statement;
import java.util.ArrayList;
import java.util.List;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;
import org.springframework.boot.autoconfigure.jdbc.DataSourceProperties;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.datasource.SingleConnectionDataSource;
import org.springframework.transaction.support.TransactionTemplate;

/** 仅显式 -Dtest=TiDbEnvironmentIT -Dcommerce.it.tidb=jdbc:mysql://127.0.0.1:24000 执行，不伪造数据库。 */
class TiDbEnvironmentIT {
    @Test void realTidbHasVerifiedSchemaAndEveryPhysicalConnectionUsesStrongSessions() throws Exception {
        assertNotNull(System.getProperty("commerce.it.tidb"), "此集成用例必须显式连接真实 TiDB");
        try (var f = new CommerceFixture()) {
            SchemaVerifier.verify(f.jdbc, f.source);
            DataSourceProperties properties = new DataSourceProperties();
            String snapshot = currentSnapshot(f);
            properties.setUrl(f.jdbcUrl + "?sessionVariables=" + URLEncoder.encode(
                    "tidb_read_consistency='weak',tidb_snapshot='" + snapshot + "'", StandardCharsets.UTF_8));
            // 同一owner的新池领取独立NORMAL boot，不能再次消费第一个池的单次grant。
            try (var pool = f.openAdditionalPool(properties)) {
                List<Connection> held = new ArrayList<>();
                try {
                    // 同时持有四个连接，不能靠同一条物理连接反复借还冒充初始化验证。
                    for (int i = 0; i < 4; i++) { held.add(pool.getConnection()); }
                    for (Connection connection : held) {
                        assertEquals(Connection.TRANSACTION_READ_COMMITTED, connection.getTransactionIsolation());
                        connection.setAutoCommit(false);
                        try (Statement statement = connection.createStatement()) {
                            statement.setQueryTimeout(5);
                            try (ResultSet ignored = statement.executeQuery("SELECT nonce FROM commerce_request_nonce WHERE caller_role='probe' AND nonce=X'00'")) { while (ignored.next()) { } }
                            try (ResultSet rows = statement.executeQuery("SELECT TIDB_VERSION(),@@session.tidb_txn_mode,@@session.tidb_replica_read,@@session.tidb_read_staleness,@@session.tidb_low_resolution_tso,@@session.sql_mode,(TIDB_CURRENT_TSO() >> 18),@@session.tidb_read_consistency,@@session.tidb_snapshot")) {
                                assertTrue(rows.next()); assertFalse(rows.getString(1).isBlank());
                                assertEquals("pessimistic", rows.getString(2)); assertEquals("leader", rows.getString(3));
                                assertEquals(0, rows.getLong(4)); assertEquals(0, rows.getInt(5)); assertTrue(rows.getString(6).contains("STRICT_TRANS_TABLES"));
                                assertEquals("strict", rows.getString(8)); assertEquals("", rows.getString(9));
                                assertTrue(rows.getLong(7) > 1_700_000_000_000L);
                            }
                        } finally { connection.rollback(); }
                    }
                } finally { for (Connection connection : held) { connection.close(); } }
            }
        }
    }
    @ParameterizedTest @ValueSource(strings = {"weak", "snapshot"})
    void normalSessionsRejectWeakOrHistoricalSessionChanges(String setting) throws Exception {
        assertNotNull(System.getProperty("commerce.it.tidb"));
        try (var f = new CommerceFixture()) {
            String snapshot = currentSnapshot(f);
            try (Connection connection = f.source.getConnection(); Statement statement = connection.createStatement()) {
                statement.setQueryTimeout(5);
                var single = new SingleConnectionDataSource(connection, true);
                // NORMAL在执行前拒绝改弱会话；不能先绕过StrongPolicy制造弱态，再用SET清理。
                var denied = assertThrows(java.sql.SQLSyntaxErrorException.class, () ->
                        statement.execute("weak".equals(setting) ? "SET SESSION tidb_read_consistency='weak'"
                                : "SET SESSION tidb_snapshot='" + snapshot + "'"));
                assertEquals("42000", denied.getSQLState());
                try (ResultSet rows = statement.executeQuery(
                        "SELECT @@session.tidb_read_consistency,@@session.tidb_snapshot")) {
                    assertTrue(rows.next());
                    assertEquals("strict", rows.getString(1));
                    assertEquals("", rows.getString(2));
                    assertFalse(rows.next());
                }
                assertDoesNotThrow(() -> new DatabaseConfiguration()
                        .commerceSchemaReady(new JdbcTemplate(single), f.props, single).afterPropertiesSet());
            }
        }
    }
    private static String currentSnapshot(CommerceFixture f) {
        return new TransactionTemplate(f.manager).execute(status -> {
            f.jdbc.queryForList("SELECT nonce FROM commerce_request_nonce WHERE caller_role='probe' AND nonce=X'00'");
            return f.jdbc.queryForObject("SELECT CAST(TIDB_CURRENT_TSO() AS CHAR)", String.class);
        });
    }
    /** 接口负例使用JDBC替身，单列为单元证据；真实NORMAL连接的改弱拒绝由上面的真库用例验证。 */
    @ParameterizedTest @ValueSource(strings = {"weak", "snapshot"})
    void startupRejectsAlreadyWeakOrHistoricalSessionFromDataSource(String setting) throws Exception {
        var values = new java.util.HashMap<String, Object>();
        values.put("SELECT tidb_version()", "test-tidb-capability");
        values.put("SELECT @@session.tidb_txn_mode", "pessimistic");
        values.put("SELECT @@session.tidb_replica_read", "leader");
        values.put("SELECT @@session.tidb_read_staleness", 0L);
        values.put("SELECT @@session.tidb_low_resolution_tso", 0);
        values.put("SELECT @@session.tidb_read_consistency", "weak".equals(setting) ? "weak" : "strict");
        values.put("SELECT @@session.tidb_snapshot", "snapshot".equals(setting) ? "2026-01-01 00:00:00" : "");
        values.put("SELECT @@session.sql_mode", "STRICT_TRANS_TABLES");
        var observedQueries = new ArrayList<String>();
        var source = org.mockito.Mockito.mock(javax.sql.DataSource.class);
        var connection = org.mockito.Mockito.mock(Connection.class);
        org.mockito.Mockito.when(source.getConnection()).thenReturn(connection);
        org.mockito.Mockito.when(connection.createStatement()).thenAnswer(call -> {
            var statement = org.mockito.Mockito.mock(Statement.class);
            org.mockito.Mockito.when(statement.executeQuery(org.mockito.ArgumentMatchers.anyString())).thenAnswer(query -> {
                String sql = query.getArgument(0);
                if (!values.containsKey(sql)) { throw new java.sql.SQLException("非预期会话查询"); }
                observedQueries.add(sql);
                Object value = values.get(sql);
                var rows = org.mockito.Mockito.mock(ResultSet.class);
                var metadata = org.mockito.Mockito.mock(java.sql.ResultSetMetaData.class);
                org.mockito.Mockito.when(metadata.getColumnCount()).thenReturn(1);
                org.mockito.Mockito.when(rows.getMetaData()).thenReturn(metadata);
                org.mockito.Mockito.when(rows.next()).thenReturn(true, false);
                org.mockito.Mockito.when(rows.getString(1)).thenReturn(value.toString());
                if (value instanceof Long number) { org.mockito.Mockito.when(rows.getLong(1)).thenReturn(number); }
                if (value instanceof Integer number) { org.mockito.Mockito.when(rows.getInt(1)).thenReturn(number); }
                return rows;
            });
            return statement;
        });
        var props = new CommerceProperties(); props.setRequireTidb(true);
        var schema = org.mockito.Mockito.mock(JdbcTemplate.class);
        var refused = assertThrows(IllegalStateException.class, () -> new DatabaseConfiguration()
                .commerceSchemaReady(schema, props, source).afterPropertiesSet());
        assertEquals("商城生产数据库必须为 TiDB 悲观事务逻辑权威库", refused.getMessage());
        assertTrue(observedQueries.contains("weak".equals(setting)
                ? "SELECT @@session.tidb_read_consistency" : "SELECT @@session.tidb_snapshot"));
        assertEquals(values.keySet(), new java.util.HashSet<>(observedQueries));
        assertEquals(values.size(), observedQueries.size());
        org.mockito.Mockito.verifyNoInteractions(schema);
        org.mockito.Mockito.verify(source).getConnection();
        org.mockito.Mockito.verify(connection).close();
    }
    @Test void sqlTsoNonceIsSharedCaseSensitiveAndRejectsExpiredSignatures() {
        assertNotNull(System.getProperty("commerce.it.tidb"));
        try (var f = new CommerceFixture()) {
            NonceStore first = new NonceStore(f.jdbc, f.props, f.manager); NonceStore second = new NonceStore(f.jdbc, f.props, f.manager);
            long now = System.currentTimeMillis();
            first.claim("gateway", "NonceCase00000001", now);
            assertEquals(409, assertThrows(CommerceException.class, () -> second.claim("gateway", "NonceCase00000001", now)).status());
            second.claim("gateway", "noncecase00000001", now);
            assertEquals(401, assertThrows(CommerceException.class, () -> second.claim("gateway", "ExpiredNonce00001", now - 500_000)).status());
            assertEquals(401, assertThrows(CommerceException.class, () -> second.claim("gateway", "FutureNonce000001", now + 500_000)).status());
            assertEquals(2, f.count("commerce_request_nonce"));
        }
    }
}
