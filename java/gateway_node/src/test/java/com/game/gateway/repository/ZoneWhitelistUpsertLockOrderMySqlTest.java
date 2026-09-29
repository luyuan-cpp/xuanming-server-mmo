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
import java.sql.Statement;
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
 *   <li>{@link #legacyPlainInsertBehindPendingDeleteLosesOneWriter}:对照组。旧 add = save(entry) 发出的普通 INSERT
 *       在同一时序下必须<b>恰好有一个写者失败</b>,失败码是 1213 或 1062。它证明夹具真的把两个写者摆到了同一条
 *       删除标记记录后面;哪天它不再成立(夹具走样),上一个用例的绿就什么也证明不了 —— 要查原因,不要删它。</li>
 * </ul>
 *
 * <h2>为什么对照组<b>不</b>强断言「必有 1213」(2026-09-29 真库实测后改)</h2>
 * 原先它断言 {@code deadlocks == 1}。真库(MySQL 26.7.0)上这条<b>失败</b>了:两个并发 add 里一个直接拿到
 * {@code 1062 Duplicate entry '990001-880000002' for key 'zone_whitelist.uk_zone_account'},没有 1213。
 * 同日的独立探针给出了原因,也给出了概率:{@code zone_whitelist} 的主键是<b>自增</b>,新行的唯一键项必然排在
 * 删除标记项<b>之后</b>,落在探针表里「竞争者主键都更大」那一路 —— 那一路普通 INSERT 只有 <b>2/5</b> 成环、
 * ODKU <b>0/5</b>,而且同一格在不同轮次跑出过 2/5 与 5/5,<b>随时序波动</b>。
 *
 * <p>所以「必有 1213」在这张表上天然不确定,再怎么加轮数也只是把失败概率压低到一个说不清的数,而 AGENTS.md
 * §11.4 要求测试确定、可重复。这里选的是另一条:<b>把强判据换成确定性的那一半,把 1213 降级成记录</b>。
 * <ul>
 *   <li>确定性的部分(硬断言):夹具本身要求在 {@code performance_schema} 里看见<b>两个</b>事务排进
 *       {@code zone_whitelist} 的锁等待队列,否则直接判红 —— 「夹具摆到位了没有」是这条在管,与机器快慢无关;
 *       再加上本用例的「恰好一个写者失败、失败码 ∈ {1213, 1062}、最后恰好一行」。两个写者插同一个唯一键,
 *       能提交的只可能是一个,这与 InnoDB 挑不挑牺牲者无关。</li>
 *   <li>记录的部分(不判红):跑 {@value #LEGACY_ROUNDS} 轮(与探针每格的重复次数同量级),把 N/轮数 的成环
 *       次数连同 {@code SELECT VERSION()} 打进 surefire 的 stdout。下次 MySQL 大版本变动时,拿这行与探针表对照
 *       就知道锁行为动没动 —— 这正是原断言想要的信息,只是不再拿它当红绿判据。</li>
 * </ul>
 * 代价写明白:<b>本用例不再能证明「1213 在本机复现得出来」。</b>真要那份证据,跑探针
 * ({@code <scratchpad>/lockprobe-keep/}),或在「新行主键更小」的形状上编排 —— 那一路是 5/5,才适合做强断言。
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

    /**
     * 对照组重复的轮数。取 5 是为了与 2026-09-29 探针每格的重复次数一致,打出来的 N/5 能直接与探针表对照;
     * 它<b>不是</b>用来把「至少一轮成环」的失败概率压下去的 —— 本用例不拿成环次数判红(见类注释)。
     * 每轮要开 4 条连接、等两个写者排队,单轮通常几百毫秒,5 轮不会顶到 surefire 的超时。
     */
    private static final int LEGACY_ROUNDS = 5;

    /** MySQL ER_DUP_ENTRY:唯一键重复。幸存者先提交之后,后到的普通 INSERT 拿到的就是它。 */
    private static final int MYSQL_ER_DUP_ENTRY = 1062;

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

    /**
     * 对照组:旧 add 的普通 INSERT 在同一时序下必须恰好淘汰一个写者。1213 与 1062 都算「被唯一键挡下」,
     * 哪一种出现取决于 InnoDB 有没有成环,而这张表上成环与否随时序波动(见类注释),所以只记录、不判红。
     */
    @Test
    void legacyPlainInsertBehindPendingDeleteLosesOneWriter() throws Exception {
        int deadlockRounds = 0;
        for (int round = 1; round <= LEGACY_ROUNDS; round++) {
            cleanTestKeys(); // 每轮都从「表里没有这个键」开始:seed 才会拿到一个全新的自增 id
            List<Optional<SQLException>> outcomes = runScenario(ACCOUNT_LEGACY, (c, note) -> {
                try (PreparedStatement ps = c.prepareStatement(LEGACY_SAVE_INSERT_SQL)) {
                    ps.setLong(1, ZONE);
                    ps.setLong(2, ACCOUNT_LEGACY);
                    ps.setString(3, note);
                    ps.executeUpdate();
                }
                readBack(c, ACCOUNT_LEGACY);
            });

            String where = "第 " + round + "/" + LEGACY_ROUNDS + " 轮,实际结局: " + describe(outcomes);
            long successes = outcomes.stream().filter(Optional::isEmpty).count();
            assertEquals(1, successes, "两个写者插同一个唯一键,能提交的恰好是一个。" + where
                    + "。两个都成功 = 夹具连唯一键都没摆对;两个都失败 = 编排走样(看错误码,多半是 1205 超时)。"
                    + "两种都会让 upsert 用例的绿失去证明力 —— 查原因,不要删本用例");
            SQLException loser = outcomes.stream().flatMap(Optional::stream).findFirst().orElseThrow();
            assertTrue(isDeadlock(loser) || loser.getErrorCode() == MYSQL_ER_DUP_ENTRY,
                    () -> "失败的那个写者应当是被唯一键挡下的(1213 成环,或 1062 后到者撞已提交的活行);"
                            + "别的错误码说明编排走样(例如 1205 锁等待超时)。" + where);
            assertEquals(1, countRows(ACCOUNT_LEGACY), where);
            if (isDeadlock(loser)) {
                deadlockRounds++;
            }
        }

        // 记录而非判据(理由见类注释):下次 MySQL 版本变动时拿这行与 2026-09-29 的探针表对照。
        System.out.println("[锁序记录] zone_whitelist 旧 add(普通 INSERT)压在未提交删除后面:成环 "
                + deadlockRounds + "/" + LEGACY_ROUNDS + " 轮;MySQL 版本 " + serverVersion()
                + "(2026-09-29 探针在同形状上测得普通 INSERT 2/5、ODKU 0/5,随时序波动)");
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

    /** 只进记录行,不参与任何断言:下次 MySQL 版本变动时靠它认出「锁行为是不是跟着版本动了」。 */
    private static String serverVersion() throws SQLException {
        try (Connection c = open();
             Statement s = c.createStatement();
             ResultSet rs = s.executeQuery("SELECT VERSION()")) {
            rs.next();
            return rs.getString(1);
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
