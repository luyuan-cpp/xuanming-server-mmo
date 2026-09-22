package com.game.gateway.repository;

import com.game.gateway.repository.MySqlLockOrderFixture.NamedSql;
import com.game.gateway.repository.MySqlLockOrderFixture.WriterWork;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeAll;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.Set;

import static com.game.gateway.repository.MySqlLockOrderFixture.describe;
import static com.game.gateway.repository.MySqlLockOrderFixture.isDeadlock;
import static com.game.gateway.repository.MySqlLockOrderFixture.open;
import static org.junit.jupiter.api.Assertions.*;

/**
 * 死锁审计 #18 的真库确定性回归:zone_whitelist 上「未提交的删除压着唯一键 + 两个同键 add 排队」。
 *
 * <h2>为什么是真 MySQL、为什么手工编排</h2>
 * 环在 InnoDB 的唯一二级索引锁上(uk_zone_account),H2 没有这套锁语义,测不出来。环要求「两个 add 同时排在
 * 同一条被删记录后面」,靠并发度去撞的概率取决于机器快慢;所以照手册一步一步摆,编排、purge 挡板与门控都在
 * {@link MySqlLockOrderFixture}(取锁细节按手册与 InnoDB 已知行为推演,以真库上跑出来的结果为准)。
 *
 * <h2>两个用例缺一不可</h2>
 * <ul>
 *   <li>{@link #upsertBehindPendingDeleteOnlyQueues}:生产的 {@link ZoneWhitelistRepository#UPSERT_SQL}(同一个常量,
 *       不另抄)在这个时序下两个都成功、零 1213、只剩一行。purge 挡板关掉了 InnoDB 固有的「purge 恰在窗口内」情形,
 *       所以这条断言是确定性的。</li>
 *   <li>{@link #legacySaveInsertBehindPendingDeleteDeadlocks}:对照组。旧 add = save(entry) 发出的普通 INSERT 在
 *       同一时序下必须撞出 1213。它证明夹具真的摆出了审计里的环;哪天它不再复现(MySQL 行为变了或夹具走样),
 *       上一个用例的绿就什么也证明不了 —— 要查原因,不要删它。</li>
 * </ul>
 *
 * <h2>运行条件</h2>
 * 见 {@link MySqlLockOrderFixture}:配 {@code GATEWAY_TEST_MYSQL_URL} 等环境变量,且库名必须以 _it / _test 结尾
 * (专用测试库),否则跳过;设了 {@code GATEWAY_REQUIRE_MYSQL_TESTS} 时跳过一律判红。
 *
 * <p>隔离级别刻意不设:与生产 JDBC URL 同口径,走服务器默认(默认 RR)。
 */
class ZoneWhitelistUpsertLockOrderMySqlTest {

    /** 专用测试键:zone 取一个真实部署不会用到的号,两个用例各用一个 account,互不干扰。 */
    private static final long ZONE = 990_001L;
    private static final long ACCOUNT_UPSERT = 880_000_001L;
    private static final long ACCOUNT_LEGACY = 880_000_002L;

    /** 生产 upsert 常量,按名字绑定。 */
    private static final NamedSql UPSERT = NamedSql.parse(ZoneWhitelistRepository.UPSERT_SQL);
    /** 与 {@link ZoneWhitelistRepository#DELETE_BY_KEY_JPQL} 等价的 SQL:按 uk_zone_account 定位的单条删除。 */
    private static final String DELETE_BY_KEY_SQL = "DELETE FROM zone_whitelist WHERE zone_id = ? AND account_id = ?";
    /** 旧 add = save(entry)(id 为空)时 Hibernate 发出的普通 INSERT 形状,只给对照组用。 */
    private static final String LEGACY_SAVE_INSERT_SQL =
            "INSERT INTO zone_whitelist (zone_id, account_id, note) VALUES (?, ?, ?)";
    /** add 在同一事务里的回读,与 findByZoneIdAndAccountId 同形。 */
    private static final String READ_BACK_SQL = "SELECT id FROM zone_whitelist WHERE zone_id = ? AND account_id = ?";

    /** null = 真库可用;否则是跳过 / 判红的原因。 */
    private static String unusableReason;

    @BeforeAll
    static void prepareDedicatedSchema() throws SQLException {
        unusableReason = MySqlLockOrderFixture.prepareDedicatedSchema();
    }

    @BeforeEach
    void requireMySqlAndCleanBefore() throws SQLException {
        MySqlLockOrderFixture.assumeUsable(unusableReason);
        cleanTestKeys();
    }

    @AfterEach
    void cleanAfter() throws SQLException {
        if (unusableReason == null) {
            cleanTestKeys();
        }
    }

    @Test
    void upsertBehindPendingDeleteOnlyQueues() throws Exception {
        List<Optional<SQLException>> outcomes = runScenario(ACCOUNT_UPSERT, (c, note) -> {
            UPSERT.executeUpdate(c, upsertParams(ACCOUNT_UPSERT, note));
            readBack(c, ACCOUNT_UPSERT);
        });

        for (int i = 0; i < outcomes.size(); i++) {
            Optional<SQLException> err = outcomes.get(i);
            if (err.isPresent() && isDeadlock(err.get())) {
                fail("第 " + (i + 1) + " 个 upsert 撞上 InnoDB 死锁(1213):重复键检查又拿回了 S 锁"
                        + "(UPSERT_SQL 被改回普通 INSERT / INSERT IGNORE?)—— 见 ZoneWhitelistRepository#upsert。"
                        + "purge 挡板已关掉「purge 恰在窗口内」的固有情形;仍不明时看 SHOW ENGINE INNODB STATUS 的"
                        + " LATEST DETECTED DEADLOCK 段", err.get());
            }
            if (err.isPresent()) {
                fail("第 " + (i + 1) + " 个 upsert 出现非预期错误", err.get());
            }
        }
        assertEquals(1, countRows(ACCOUNT_UPSERT), "两个 upsert 都成功之后 (zone, account) 必须恰好一行");
        assertTrue(Set.of("w1", "w2").contains(readNote(ACCOUNT_UPSERT)),
                "留下的行必须是两个 upsert 之一写的,而不是删除前的旧行(note=seed)");
    }

    @Test
    void legacySaveInsertBehindPendingDeleteDeadlocks() throws Exception {
        List<Optional<SQLException>> outcomes = runScenario(ACCOUNT_LEGACY, (c, note) -> {
            try (PreparedStatement ps = c.prepareStatement(LEGACY_SAVE_INSERT_SQL)) {
                ps.setLong(1, ZONE);
                ps.setLong(2, ACCOUNT_LEGACY);
                ps.setString(3, note);
                ps.executeUpdate();
            }
            readBack(c, ACCOUNT_LEGACY);
        });

        long deadlocks = outcomes.stream().filter(o -> o.isPresent() && isDeadlock(o.get())).count();
        long successes = outcomes.stream().filter(Optional::isEmpty).count();
        assertEquals(1, deadlocks, "对照组必须复现审计 #18 的环(恰好一个 1213)。不复现说明夹具没摆出那个时序,"
                + "upsert 用例的绿因此不再有证明力 —— 查原因,不要删本用例。实际结局: " + describe(outcomes));
        assertEquals(1, successes, "幸存者应当插入成功。实际结局: " + describe(outcomes));
        assertEquals(1, countRows(ACCOUNT_LEGACY));
    }

    private static List<Optional<SQLException>> runScenario(long account, WriterWork writer) throws Exception {
        return MySqlLockOrderFixture.runTwoWritersBehindPendingDelete(
                "zone_whitelist",
                c -> UPSERT.executeUpdate(c, upsertParams(account, "seed")),
                deleter -> {
                    try (PreparedStatement del = deleter.prepareStatement(DELETE_BY_KEY_SQL)) {
                        del.setLong(1, ZONE);
                        del.setLong(2, account);
                        assertEquals(1, del.executeUpdate(), "夹具:DELETE 应当恰好删 1 行(此时未提交、持有该行的 X)");
                    }
                },
                writer);
    }

    private static Map<String, Object> upsertParams(long account, String note) {
        return Map.of("zoneId", ZONE, "accountId", account, "note", note);
    }

    private static void readBack(Connection c, long account) throws SQLException {
        try (PreparedStatement ps = c.prepareStatement(READ_BACK_SQL)) {
            ps.setLong(1, ZONE);
            ps.setLong(2, account);
            ps.executeQuery().close();
        }
    }

    /** 只删本用例的专用键,不碰表里别的数据。 */
    private static void cleanTestKeys() throws SQLException {
        try (Connection c = open();
             PreparedStatement ps = c.prepareStatement(
                     "DELETE FROM zone_whitelist WHERE zone_id = ? AND account_id IN (?, ?)")) {
            ps.setLong(1, ZONE);
            ps.setLong(2, ACCOUNT_UPSERT);
            ps.setLong(3, ACCOUNT_LEGACY);
            ps.executeUpdate();
        }
    }

    private static long countRows(long account) throws SQLException {
        try (Connection c = open();
             PreparedStatement ps = c.prepareStatement(
                     "SELECT COUNT(*) FROM zone_whitelist WHERE zone_id = ? AND account_id = ?")) {
            ps.setLong(1, ZONE);
            ps.setLong(2, account);
            try (ResultSet rs = ps.executeQuery()) {
                rs.next();
                return rs.getLong(1);
            }
        }
    }

    private static String readNote(long account) throws SQLException {
        try (Connection c = open();
             PreparedStatement ps = c.prepareStatement(
                     "SELECT note FROM zone_whitelist WHERE zone_id = ? AND account_id = ?")) {
            ps.setLong(1, ZONE);
            ps.setLong(2, account);
            try (ResultSet rs = ps.executeQuery()) {
                assertTrue(rs.next(), "行不在");
                return rs.getString(1);
            }
        }
    }
}
