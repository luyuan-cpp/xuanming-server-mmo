package com.game.gateway.repository;

import org.junit.jupiter.api.Assumptions;
import org.springframework.core.io.ClassPathResource;
import org.springframework.jdbc.datasource.init.ScriptUtils;

import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

import static org.junit.jupiter.api.Assertions.fail;

/**
 * gateway 真 MySQL 锁序回归用例的公共夹具(zone_whitelist / zone_config 两份用例共用,只此一份)。
 *
 * <h2>门控(不设即跳过,跳过不代表通过)</h2>
 * {@value #URL_ENV}(JDBC URL)、{@value #USER_ENV}、{@value #PASSWORD_ENV};账号要能读
 * performance_schema.data_lock_waits / data_locks。设了 {@value #REQUIRE_ENV} 时,缺库、库不是专用测试库、
 * 读不了 performance_schema 一律判红,验收必须看到 PASS 而不是 SKIP。
 *
 * <h2>只许连专用测试库</h2>
 * 用例会对连上的库执行整份 schema.sql(建表 + zone_config 默认区种子)并增删测试行。库名必须以
 * {@code _it} 或 {@code _test} 结尾(例如 {@code mmorpg_it}),否则一条 DDL/DML 都不执行、直接跳过并说明原因
 * —— 绝不能污染开发库 {@code mmorpg}:schema.sql 与 deploy/mysql-init 的列类型并不完全一致,抢先建表会把分叉固化。
 *
 * <h2>编排:未提交的删除压着一条记录 + 两个写者排在它后面</h2>
 * 这是 MySQL 手册「Locks Set by Different SQL Statements」里 DELETE + 双 INSERT 的例子。删除提交的那一刻,两个
 * 写者挂在同一条删除标记记录上的锁请求被<b>同时</b>处理:普通 INSERT 要的是 S,两把 S 一起授予,随后都要升级成 X
 * → 互等 1213;ODKU 要的是 X,只能一个一个授予 → 只排队。「先到的写者回滚、排队的两个一起被授予」是同一个锁状态,
 * 这里用未提交的删除来摆,是因为它能确定性地让两个写者先都排上队(看得见两个等待者之后才提交)。
 *
 * <h2>purge 挡板</h2>
 * 删除提交之后、写者真正写入之前,purge 可能恰好把那条删除标记记录清掉:两个写者挂在它上面的锁会被继承成后继记录上
 * 的间隙锁,双方的插入意向互相挡住 → 1213。这是 InnoDB 固有情形(生产由控制器的有上限重试兜住),与被测语句的
 * 锁序无关;不关掉这个窗口,「零 1213」的断言就不是确定性的。所以编排期间用另一条连接持有一个 RR 一致性读视图
 * (早于删除提交创建):purge 只清理对最老读视图也已不可见的删除标记记录,于是那条记录在整个窗口内都保持
 * 「删除标记、未清理」。挡板只做一致性读,不加任何行锁,不影响 data_lock_waits 的计数。
 */
final class MySqlLockOrderFixture {

    static final String URL_ENV = "GATEWAY_TEST_MYSQL_URL";
    static final String USER_ENV = "GATEWAY_TEST_MYSQL_USER";
    static final String PASSWORD_ENV = "GATEWAY_TEST_MYSQL_PASSWORD";
    static final String REQUIRE_ENV = "GATEWAY_REQUIRE_MYSQL_TESTS";

    static final int MYSQL_ER_LOCK_DEADLOCK = 1213;

    private static final List<String> DEDICATED_DB_SUFFIXES = List.of("_it", "_test");
    private static final Duration WAIT_BUDGET = Duration.ofSeconds(10);
    /** 写者会话的锁等待上限:编排走样时让语句自己超时退出,而不是挂满默认的 50s。 */
    private static final int WRITER_LOCK_WAIT_TIMEOUT_S = 20;

    /** 数本库某张表上正在排队等行锁的事务数;别的库、别的表的等待不算。 */
    private static final String LOCK_WAITERS_SQL = """
            SELECT COUNT(DISTINCT w.REQUESTING_ENGINE_TRANSACTION_ID)
            FROM performance_schema.data_lock_waits w
            JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID
            WHERE l.OBJECT_SCHEMA = DATABASE() AND l.OBJECT_NAME = ?""";

    private MySqlLockOrderFixture() {
    }

    /** 在一条连接上执行一段 SQL。 */
    @FunctionalInterface
    interface SqlWork {
        void run(Connection c) throws SQLException;
    }

    /** 一个写者在自己事务里要执行的语句;marker 区分两个写者(w1 / w2),写进被测行,事后看是谁留下的。 */
    @FunctionalInterface
    interface WriterWork {
        void run(Connection c, String marker) throws SQLException;
    }

    /**
     * {@code @BeforeAll} 调:确认连的是专用测试库,是则执行 schema.sql(全是 CREATE TABLE IF NOT EXISTS,种子是
     * INSERT IGNORE,已有则不动)并返回 null;不能用则返回原因,且不执行任何 DDL/DML。
     */
    static String prepareDedicatedSchema() throws SQLException {
        if (!isConfigured()) {
            return URL_ENV + " 未设置";
        }
        try (Connection c = open()) {
            String db = currentDatabase(c);
            if (!isDedicatedTestDatabase(db)) {
                return "拒绝在库 `" + db + "` 上运行:用例会执行 schema.sql(建表 + zone_config 种子)并增删测试行,"
                        + "只允许库名以 " + DEDICATED_DB_SUFFIXES + " 结尾的专用测试库(例如 mmorpg_it),不许连开发库";
            }
            ScriptUtils.executeSqlScript(c, new ClassPathResource("schema.sql"));
            return null;
        }
    }

    /**
     * {@code @BeforeEach} 调(门控放在每个用例前而不是 {@code @BeforeAll}:类级 abort 会让 surefire 报
     * 「Tests run: 0」,连 SKIPPED 都不显示,看上去和「没有这些用例」一样)。不能用时:验收模式判红,否则跳过。
     */
    static void assumeUsable(String unusableReason) {
        if (unusableReason == null) {
            return;
        }
        if (isRequired()) {
            fail(REQUIRE_ENV + " 已设置,真库用例不许跳过:" + unusableReason);
        }
        Assumptions.abort(unusableReason + ";跳过真库锁序用例(不代表通过)");
    }

    static boolean isConfigured() {
        String url = System.getenv(URL_ENV);
        return url != null && !url.isBlank();
    }

    static Connection open() throws SQLException {
        return DriverManager.getConnection(System.getenv(URL_ENV), System.getenv(USER_ENV), System.getenv(PASSWORD_ENV));
    }

    static boolean isDeadlock(SQLException e) {
        return e.getErrorCode() == MYSQL_ER_LOCK_DEADLOCK;
    }

    static String describe(List<Optional<SQLException>> outcomes) {
        return outcomes.stream()
                .map(o -> o.map(e -> e.getErrorCode() + ":" + e.getMessage()).orElse("OK"))
                .toList()
                .toString();
    }

    /**
     * 编排:seed 预置活行(自动提交)→ 开 purge 挡板 → 独立事务执行 pendingDelete 且不提交 → 两个写者各开事务
     * 执行 writer 并提交(与控制器「写 + 同事务回读」的事务形状一致),都会卡在那条记录上 → 在 {@code table} 上
     * 看见 2 个等待者 → 提交删除 → 收集两个写者的结局(empty = 成功)→ 关挡板。
     *
     * @param pendingDelete 在删除者连接上执行,须自行断言恰好删掉 1 行
     */
    static List<Optional<SQLException>> runTwoWritersBehindPendingDelete(
            String table, SqlWork seed, SqlWork pendingDelete, WriterWork writer) throws Exception {
        try (Connection c = open()) {
            seed.run(c);
        }

        ExecutorService pool = Executors.newFixedThreadPool(2);
        try (PurgeBaffle baffle = PurgeBaffle.open(); Connection deleter = open()) {
            deleter.setAutoCommit(false);
            pendingDelete.run(deleter);

            List<Future<Optional<SQLException>>> writers = new ArrayList<>();
            for (String marker : List.of("w1", "w2")) {
                writers.add(pool.submit(() -> runWriter(writer, marker)));
            }

            try {
                awaitLockWaiters(deleter, table, 2);
            } catch (LockWaitsUnobservable e) {
                deleter.rollback();
                drain(writers);
                assumeUsable("读不了 performance_schema,编排不出本场景(测试账号需要它的 SELECT 权限): " + e.getCause());
            } catch (AssertionError e) {
                deleter.rollback();
                drain(writers);
                throw e;
            }

            // 放掉删除者的 X:普通 INSERT 成环的时刻就在这里。挡板此时仍开着,删除标记记录不会被 purge。
            deleter.commit();
            return drain(writers);
        } finally {
            pool.shutdownNow();
        }
    }

    /** 把 Spring Data 的命名参数语句换成 JDBC 占位符,按名字绑定(同名参数可以出现多次)。 */
    record NamedSql(String jdbcSql, List<String> names) {

        private static final Pattern NAMED_PARAM = Pattern.compile("(?<![:\\w]):(\\w+)");

        static NamedSql parse(String sql) {
            List<String> names = new ArrayList<>();
            Matcher m = NAMED_PARAM.matcher(sql);
            StringBuilder out = new StringBuilder();
            while (m.find()) {
                names.add(m.group(1));
                m.appendReplacement(out, "?");
            }
            m.appendTail(out);
            return new NamedSql(out.toString(), List.copyOf(names));
        }

        /** 按名字绑定并执行一次;{@code values} 缺任何一个名字都直接判红,不绑 null 了事。 */
        int executeUpdate(Connection c, Map<String, ?> values) throws SQLException {
            try (PreparedStatement ps = c.prepareStatement(jdbcSql)) {
                for (int i = 0; i < names.size(); i++) {
                    String name = names.get(i);
                    if (!values.containsKey(name)) {
                        fail("语句的命名参数 :" + name + " 没有给值(生产语句改了参数,用例没跟上): " + jdbcSql);
                    }
                    ps.setObject(i + 1, values.get(name));
                }
                return ps.executeUpdate();
            }
        }
    }

    private static Optional<SQLException> runWriter(WriterWork writer, String marker) {
        try (Connection c = open()) {
            try (Statement s = c.createStatement()) {
                s.execute("SET SESSION innodb_lock_wait_timeout = " + WRITER_LOCK_WAIT_TIMEOUT_S);
            }
            c.setAutoCommit(false);
            try {
                writer.run(c, marker);
                c.commit();
                return Optional.empty();
            } catch (SQLException e) {
                c.rollback();
                return Optional.of(e);
            }
        } catch (SQLException e) {
            return Optional.of(e);
        }
    }

    /** 轮询观察等待者数量,不靠 sleep 估时间;预算用尽即判红(编排失败本身就说明锁行为与推演不符)。 */
    private static void awaitLockWaiters(Connection observer, String table, int want)
            throws LockWaitsUnobservable, InterruptedException {
        long deadlineNanos = System.nanoTime() + WAIT_BUDGET.toNanos();
        int waiters = 0;
        while (System.nanoTime() < deadlineNanos) {
            try (PreparedStatement ps = observer.prepareStatement(LOCK_WAITERS_SQL)) {
                ps.setString(1, table);
                try (ResultSet rs = ps.executeQuery()) {
                    rs.next();
                    waiters = rs.getInt(1);
                }
            } catch (SQLException e) {
                throw new LockWaitsUnobservable(e);
            }
            if (waiters >= want) {
                return;
            }
            Thread.sleep(10); // 只是不空转打爆 MySQL,不是估时间
        }
        fail(WAIT_BUDGET + " 内只看到 " + waiters + "/" + want + " 个事务排进 " + table
                + " 的锁等待队列:未提交的 DELETE 应当让每个写者都卡在那条被删记录的重复键检查上");
    }

    private static List<Optional<SQLException>> drain(List<Future<Optional<SQLException>>> writers) throws Exception {
        List<Optional<SQLException>> out = new ArrayList<>();
        for (Future<Optional<SQLException>> f : writers) {
            out.add(f.get(WRITER_LOCK_WAIT_TIMEOUT_S * 2L, TimeUnit.SECONDS));
        }
        return out;
    }

    private static boolean isRequired() {
        String v = System.getenv(REQUIRE_ENV);
        return v != null && !v.isBlank();
    }

    private static String currentDatabase(Connection c) throws SQLException {
        try (Statement s = c.createStatement(); ResultSet rs = s.executeQuery("SELECT DATABASE()")) {
            rs.next();
            return rs.getString(1);
        }
    }

    private static boolean isDedicatedTestDatabase(String db) {
        if (db == null) {
            return false;
        }
        String lower = db.toLowerCase(Locale.ROOT);
        return DEDICATED_DB_SUFFIXES.stream().anyMatch(lower::endsWith);
    }

    /** purge 挡板:见类注释。必须在删除提交之前打开,在写者全部结束之后关闭。 */
    private static final class PurgeBaffle implements AutoCloseable {
        private final Connection c;

        private PurgeBaffle(Connection c) {
            this.c = c;
        }

        static PurgeBaffle open() throws SQLException {
            Connection c = MySqlLockOrderFixture.open();
            try {
                c.setAutoCommit(false);
                // 只有 RR 下 WITH CONSISTENT SNAPSHOT 才会立刻建读视图;只设挡板这一条会话,被测写者仍走服务器默认。
                c.setTransactionIsolation(Connection.TRANSACTION_REPEATABLE_READ);
                try (Statement s = c.createStatement()) {
                    s.execute("START TRANSACTION WITH CONSISTENT SNAPSHOT");
                }
                return new PurgeBaffle(c);
            } catch (SQLException e) {
                c.close();
                throw e;
            }
        }

        @Override
        public void close() throws SQLException {
            try {
                c.rollback();
            } finally {
                c.close();
            }
        }
    }

    /** 读不了 performance_schema(账号无权限或库不是 MySQL 8):编排做不出来,不是产品缺陷。 */
    private static final class LockWaitsUnobservable extends Exception {
        LockWaitsUnobservable(SQLException cause) {
            super(cause);
        }
    }
}
