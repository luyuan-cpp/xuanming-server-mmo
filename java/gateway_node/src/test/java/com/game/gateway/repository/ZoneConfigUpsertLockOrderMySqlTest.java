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
import java.time.LocalDateTime;
import java.time.temporal.ChronoUnit;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.Set;

import static com.game.gateway.repository.MySqlLockOrderFixture.describe;
import static com.game.gateway.repository.MySqlLockOrderFixture.isDeadlock;
import static com.game.gateway.repository.MySqlLockOrderFixture.open;
import static org.junit.jupiter.api.Assertions.*;

/**
 * zone_config 主键版的真库确定性回归(死锁审计 #18 同类项):「未提交的删除压着主键记录 + 两个同 zone_id 的
 * create 排队」。
 *
 * <p>zone_config 的 zone_id 是业务赋值主键、表上没有二级索引,所以这里的环整个在聚簇索引的<b>记录锁</b>上:
 * 普通 INSERT 做主键重复检查时对删除标记记录取 S(记录锁),要「插入到删除标记记录上」时再要 X;删除一提交,
 * 两个写者的 S 同时被授予,随后互等对方放 S → 1213(手册里 DELETE + 双 INSERT 的主键例)。ODKU 做同一个检查时
 * 直接取 X,只排队。编排、purge 挡板与门控见 {@link MySqlLockOrderFixture}。
 *
 * <ul>
 *   <li>{@link #upsertBehindPendingDeleteOnlyQueues}:生产的 {@link ZoneConfigRepository#UPSERT_SQL}(同一个常量)
 *       两个都成功、零 1213、只剩一行。</li>
 *   <li>{@link #legacyMergeInsertBehindPendingDeleteDeadlocks}:对照组。旧 create = save(zone) 在 merge 的 SELECT
 *       查不到行时发出的普通 INSERT,同一时序下必须撞出恰好一个 1213;不复现就说明夹具走样,查原因,不要删它。</li>
 * </ul>
 *
 * <p>运行条件同 {@link ZoneWhitelistUpsertLockOrderMySqlTest}:专用测试库(库名以 _it / _test 结尾)+ 能读
 * performance_schema;隔离级别不设,与生产同口径走服务器默认(RR)。
 */
class ZoneConfigUpsertLockOrderMySqlTest {

    /** 专用测试区服号:真实部署不会用到,两个用例各用一个,互不干扰。 */
    private static final long ZONE_UPSERT = 990_001L;
    private static final long ZONE_LEGACY = 990_002L;

    private static final NamedSql UPSERT = NamedSql.parse(ZoneConfigRepository.UPSERT_SQL);
    /** 与 {@link ZoneConfigRepository#DELETE_BY_ID_JPQL} 等价的 SQL:按主键的单条删除。 */
    private static final String DELETE_BY_ID_SQL = "DELETE FROM zone_config WHERE zone_id = ?";
    /** 旧 create = save(zone) 走 merge、SELECT 查不到行时 Hibernate 发出的普通 INSERT 形状,只给对照组用。 */
    private static final String LEGACY_MERGE_INSERT_SQL = "INSERT INTO zone_config (zone_id, name, manual_status,"
            + " capacity, maintenance_msg, open_time, recommended, sort_order, created_at, updated_at)"
            + " VALUES (?, ?, 0, 5000, '', NULL, 0, 0, ?, ?)";
    /** create 在同一事务里的回读,与 findById 同形。 */
    private static final String READ_BACK_SQL = "SELECT name FROM zone_config WHERE zone_id = ?";

    /** null = 真库可用;否则是跳过 / 判红的原因。 */
    private static String unusableReason;

    @BeforeAll
    static void prepareDedicatedSchema() throws SQLException {
        unusableReason = MySqlLockOrderFixture.prepareDedicatedSchema();
    }

    @BeforeEach
    void requireMySqlAndCleanBefore() throws SQLException {
        MySqlLockOrderFixture.assumeUsable(unusableReason);
        cleanTestZones();
    }

    @AfterEach
    void cleanAfter() throws SQLException {
        if (unusableReason == null) {
            cleanTestZones();
        }
    }

    @Test
    void upsertBehindPendingDeleteOnlyQueues() throws Exception {
        List<Optional<SQLException>> outcomes = runScenario(ZONE_UPSERT, (c, name) -> {
            UPSERT.executeUpdate(c, upsertParams(ZONE_UPSERT, name));
            readBack(c, ZONE_UPSERT);
        });

        for (int i = 0; i < outcomes.size(); i++) {
            Optional<SQLException> err = outcomes.get(i);
            if (err.isPresent() && isDeadlock(err.get())) {
                fail("第 " + (i + 1) + " 个 upsert 撞上 InnoDB 死锁(1213):主键重复检查又拿回了 S 锁"
                        + "(UPSERT_SQL 被改回普通 INSERT / INSERT IGNORE?)—— 见 ZoneConfigRepository#upsert。"
                        + "purge 挡板已关掉「purge 恰在窗口内」的固有情形;仍不明时看 SHOW ENGINE INNODB STATUS 的"
                        + " LATEST DETECTED DEADLOCK 段", err.get());
            }
            if (err.isPresent()) {
                fail("第 " + (i + 1) + " 个 upsert 出现非预期错误", err.get());
            }
        }
        assertEquals(1, countRows(ZONE_UPSERT), "两个 upsert 都成功之后该 zone_id 必须恰好一行");
        assertTrue(Set.of("w1", "w2").contains(readName(ZONE_UPSERT)),
                "留下的行必须是两个 upsert 之一写的,而不是删除前的旧行(name=seed)");
    }

    @Test
    void legacyMergeInsertBehindPendingDeleteDeadlocks() throws Exception {
        List<Optional<SQLException>> outcomes = runScenario(ZONE_LEGACY, (c, name) -> {
            LocalDateTime now = nowSeconds();
            try (PreparedStatement ps = c.prepareStatement(LEGACY_MERGE_INSERT_SQL)) {
                ps.setLong(1, ZONE_LEGACY);
                ps.setString(2, name);
                ps.setObject(3, now);
                ps.setObject(4, now);
                ps.executeUpdate();
            }
            readBack(c, ZONE_LEGACY);
        });

        long deadlocks = outcomes.stream().filter(o -> o.isPresent() && isDeadlock(o.get())).count();
        long successes = outcomes.stream().filter(Optional::isEmpty).count();
        assertEquals(1, deadlocks, "对照组必须复现主键上的 S→X 环(恰好一个 1213)。不复现说明夹具没摆出那个时序,"
                + "upsert 用例的绿因此不再有证明力 —— 查原因,不要删本用例。实际结局: " + describe(outcomes));
        assertEquals(1, successes, "幸存者应当插入成功。实际结局: " + describe(outcomes));
        assertEquals(1, countRows(ZONE_LEGACY));
    }

    private static List<Optional<SQLException>> runScenario(long zoneId, WriterWork writer) throws Exception {
        return MySqlLockOrderFixture.runTwoWritersBehindPendingDelete(
                "zone_config",
                c -> UPSERT.executeUpdate(c, upsertParams(zoneId, "seed")),
                deleter -> {
                    try (PreparedStatement del = deleter.prepareStatement(DELETE_BY_ID_SQL)) {
                        del.setLong(1, zoneId);
                        assertEquals(1, del.executeUpdate(), "夹具:DELETE 应当恰好删 1 行(此时未提交、持有该行的 X)");
                    }
                },
                writer);
    }

    /** 与 {@link ZoneConfigRepository#upsert} 绑定的列一一对应;HashMap 是因为 open_time 要绑 null。 */
    private static Map<String, Object> upsertParams(long zoneId, String name) {
        Map<String, Object> p = new HashMap<>();
        p.put("zoneId", zoneId);
        p.put("name", name);
        p.put("manualStatus", 0);
        p.put("capacity", 5000);
        p.put("maintenanceMsg", "");
        p.put("openTime", null);
        p.put("recommended", false);
        p.put("sortOrder", 0);
        p.put("now", nowSeconds());
        return p;
    }

    /** 截到秒:DATETIME 列不带小数秒,免得驱动 / 服务器的舍入方式影响断言以外的东西。 */
    private static LocalDateTime nowSeconds() {
        return LocalDateTime.now().truncatedTo(ChronoUnit.SECONDS);
    }

    private static void readBack(Connection c, long zoneId) throws SQLException {
        try (PreparedStatement ps = c.prepareStatement(READ_BACK_SQL)) {
            ps.setLong(1, zoneId);
            ps.executeQuery().close();
        }
    }

    /** 只删本用例的专用区服号,不碰表里别的数据(包括 schema.sql 的默认区种子)。 */
    private static void cleanTestZones() throws SQLException {
        try (Connection c = open();
             PreparedStatement ps = c.prepareStatement("DELETE FROM zone_config WHERE zone_id IN (?, ?)")) {
            ps.setLong(1, ZONE_UPSERT);
            ps.setLong(2, ZONE_LEGACY);
            ps.executeUpdate();
        }
    }

    private static long countRows(long zoneId) throws SQLException {
        try (Connection c = open();
             PreparedStatement ps = c.prepareStatement("SELECT COUNT(*) FROM zone_config WHERE zone_id = ?")) {
            ps.setLong(1, zoneId);
            try (ResultSet rs = ps.executeQuery()) {
                rs.next();
                return rs.getLong(1);
            }
        }
    }

    private static String readName(long zoneId) throws SQLException {
        try (Connection c = open();
             PreparedStatement ps = c.prepareStatement("SELECT name FROM zone_config WHERE zone_id = ?")) {
            ps.setLong(1, zoneId);
            try (ResultSet rs = ps.executeQuery()) {
                assertTrue(rs.next(), "行不在");
                return rs.getString(1);
            }
        }
    }
}
