package com.game.gateway.repository;

import com.game.gateway.entity.Announcement;
import com.game.gateway.entity.ZoneConfig;
import com.game.gateway.entity.ZoneWhitelist;
import org.hibernate.SessionFactory;
import org.hibernate.boot.MetadataSources;
import org.hibernate.boot.registry.StandardServiceRegistry;
import org.hibernate.boot.registry.StandardServiceRegistryBuilder;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.condition.EnabledIfEnvironmentVariable;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;
import org.springframework.core.io.ClassPathResource;
import org.springframework.jdbc.datasource.DriverManagerDataSource;
import org.springframework.jdbc.datasource.init.ScriptUtils;

import javax.sql.DataSource;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.Statement;
import java.net.InetAddress;
import java.net.URI;
import java.util.Locale;
import java.util.UUID;

import static org.junit.jupiter.api.Assertions.*;

/**
 * 正式 schema.sql 必须通过三张 JPA 实体的 validate，而非由 create-drop 反向生成表后自证。
 * 默认 H2 只核契约；显式 TiDB 用例另建随机专用库，验证后删除，绝不接管已有目录库。
 */
class GatewaySchemaContractTest {

    private static final long MAX_UINT32 = 0xffff_ffffL;

    @Test
    void formalSchemaMatchesEntitiesAndKeepsUnsignedZoneRange() throws Exception {
        DriverManagerDataSource dataSource = new DriverManagerDataSource(
                "jdbc:h2:mem:gateway_schema_" + UUID.randomUUID().toString().replace("-", "")
                        + ";MODE=MySQL;DB_CLOSE_DELAY=-1", "sa", "");
        assertFormalSchema(dataSource);
    }

    // 压测种子改已有区服(zone 1 由 schema.sql 插入)时必须让 version 加一:整行 PUT 靠 If-Match 比版本,不加一的话
    // 按改之前 GET 的 ETag 发的 PUT 照样通过,把种子刚改的容量写回去(2026-11-19 评审补)。新插的行取库缺省 1。
    @Test
    void bundledStressSeedBumpsVersionOfExistingZone() throws Exception {
        DriverManagerDataSource dataSource = new DriverManagerDataSource(
                "jdbc:h2:mem:gateway_seed_" + UUID.randomUUID().toString().replace("-", "")
                        + ";MODE=MySQL;DB_CLOSE_DELAY=-1", "sa", "");
        try (Connection connection = dataSource.getConnection()) {
            ScriptUtils.executeSqlScript(connection, new ClassPathResource("schema.sql"));
            assertEquals(1L, zoneVersion(connection, 1L), "schema.sql 的种子行取缺省 1");
            ScriptUtils.executeSqlScript(connection, new ClassPathResource("seed_stress_3zones.sql"));
            assertEquals(2L, zoneVersion(connection, 1L), "已有的 zone 1 被压测种子改过,版本必须加一");
            assertEquals(1L, zoneVersion(connection, 2L), "压测种子新插的 zone 2 取缺省 1");
        }
    }

    private static long zoneVersion(Connection connection, long zoneId) throws Exception {
        try (PreparedStatement statement = connection.prepareStatement("SELECT version FROM zone_config WHERE zone_id=?")) {
            statement.setLong(1, zoneId);
            try (ResultSet rows = statement.executeQuery()) {
                assertTrue(rows.next(), "zone " + zoneId + " 不在");
                return rows.getLong(1);
            }
        }
    }

    @Test
    @EnabledIfEnvironmentVariable(named = "GATEWAY_SCHEMA_TIDB_URL", matches = ".+")
    void formalSchemaValidatesOnRealTiDbAndRoundTripsUnsignedZoneRange() throws Exception {
        String endpoint = System.getenv("GATEWAY_SCHEMA_TIDB_URL");
        requireOwnedTiDbEndpoint(endpoint);
        String user = System.getenv("GATEWAY_SCHEMA_TIDB_USER");
        String password = System.getenv("GATEWAY_SCHEMA_TIDB_PASSWORD");
        assertNotNull(user, "测试账号必须从环境注入");
        assertNotNull(password, "测试口令必须显式从环境注入，可为空但不猜测");
        String database = "gateway_schema_" + UUID.randomUUID().toString().replace("-", "") + "_test";
        boolean created = false;
        try (Connection server = strongTiDbConnection(DriverManager.getConnection(
                endpoint + "?connectTimeout=3000&socketTimeout=10000", user, password))) {
            try {
                try (Statement statement = server.createStatement()) {
                    statement.execute("CREATE DATABASE `" + database + "` CHARACTER SET utf8mb4");
                    created = true;
                }
                DriverManagerDataSource dataSource = new DriverManagerDataSource(endpoint + database
                        + "?useSSL=false&connectTimeout=3000&socketTimeout=10000", user, password) {
                    @Override public Connection getConnection() throws java.sql.SQLException {
                        return strongTiDbConnection(super.getConnection());
                    }
                    @Override public Connection getConnection(String username, String credential)
                            throws java.sql.SQLException {
                        return strongTiDbConnection(super.getConnection(username, credential));
                    }
                };
                assertFormalSchema(dataSource);
            } finally {
                if (created) {
                    try (Statement statement = server.createStatement()) {
                        statement.execute("DROP DATABASE `" + database + "`");
                    }
                    try (PreparedStatement statement = server.prepareStatement(
                            "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?")) {
                        statement.setString(1, database);
                        try (ResultSet rows = statement.executeQuery()) {
                            assertTrue(rows.next());
                            assertEquals(0, rows.getInt(1), "随机专用测试库必须清理干净");
                        }
                    }
                }
            }
        }
    }

    /**
     * 白名单 add 在同一事务里「upsert 之后回读」必须用加锁读(2026-11-26 补的真库证据;之前只有 mock / H2 钉住用的是哪个方法)。
     * TiDB 的 REPEATABLE READ 快照建在事务开始时:事务 A 开始之后别人提交了同一个键、同一个 note 的行,A 的 upsert 走
     * 「已存在、note 没变」,自己没有任何修改,普通读看不见那一行(改之前的回读就在这里报 row invisible),加锁读看得见。
     * 两条语句用的是生产的同一份文本(命名参数换成 ?)。
     */
    @Test
    @EnabledIfEnvironmentVariable(named = "GATEWAY_SCHEMA_TIDB_URL", matches = ".+")
    void lockedReadBackSeesARowCommittedAfterTheTransactionBeganOnRealTiDb() throws Exception {
        String endpoint = System.getenv("GATEWAY_SCHEMA_TIDB_URL");
        requireOwnedTiDbEndpoint(endpoint);
        String user = System.getenv("GATEWAY_SCHEMA_TIDB_USER");
        String password = System.getenv("GATEWAY_SCHEMA_TIDB_PASSWORD");
        assertNotNull(user, "测试账号必须从环境注入");
        assertNotNull(password, "测试口令必须显式从环境注入，可为空但不猜测");
        String database = "gateway_schema_" + UUID.randomUUID().toString().replace("-", "") + "_test";
        String url = endpoint + database + "?useSSL=false&connectTimeout=3000&socketTimeout=10000";
        String upsert = ZoneWhitelistRepository.UPSERT_SQL.replaceAll(":[A-Za-z]+", "?");
        String lockedRead = ZoneWhitelistRepository.FIND_BY_KEY_FOR_UPDATE_SQL.replaceAll(":[A-Za-z]+", "?");
        boolean created = false;
        try (Connection server = strongTiDbConnection(DriverManager.getConnection(
                endpoint + "?connectTimeout=3000&socketTimeout=10000", user, password))) {
            try {
                try (Statement statement = server.createStatement()) {
                    statement.execute("CREATE DATABASE `" + database + "` CHARACTER SET utf8mb4");
                    created = true;
                }
                try (Connection writer = strongTiDbConnection(DriverManager.getConnection(url, user, password));
                     Connection reader = strongTiDbConnection(DriverManager.getConnection(url, user, password))) {
                    ScriptUtils.executeSqlScript(writer, new ClassPathResource("schema.sql"));
                    // 夹具把会话设成 READ COMMITTED;这条用例要的是生产缺省的 REPEATABLE READ(快照建在事务开始时)。
                    reader.setTransactionIsolation(Connection.TRANSACTION_REPEATABLE_READ);
                    reader.setAutoCommit(false);
                    assertFalse(whitelistNote(reader, "SELECT note FROM zone_whitelist WHERE zone_id = ? AND account_id = ?")
                            .isPresent(), "事务 A 先读一次,钉住快照;这时行还不在");

                    try (PreparedStatement insert = writer.prepareStatement(upsert)) { // 别人在 A 开始之后提交同一个键
                        bindWhitelist(insert, 9001L);
                        insert.executeUpdate();
                    }

                    try (PreparedStatement again = reader.prepareStatement(upsert)) { // A 的 upsert:已存在、note 没变
                        bindWhitelist(again, 9002L);
                        again.executeUpdate();
                    }
                    assertFalse(whitelistNote(reader, "SELECT note FROM zone_whitelist WHERE zone_id = ? AND account_id = ?")
                            .isPresent(), "普通读按 A 开始时的快照,看不见之后提交的那一行:回读不能用它");
                    assertEquals("same-note", whitelistNote(reader, lockedRead).orElse(null),
                            "加锁读读最新的已提交版本,回读必然命中");
                    reader.rollback();
                }
            } finally {
                if (created) {
                    try (Statement statement = server.createStatement()) {
                        statement.execute("DROP DATABASE `" + database + "`");
                    }
                }
            }
        }
    }

    private static void bindWhitelist(PreparedStatement statement, long rowId) throws java.sql.SQLException {
        statement.setLong(1, rowId);
        statement.setLong(2, 3L);
        statement.setLong(3, 1234L);
        statement.setString(4, "same-note");
    }

    /** 按 (zone 3, account 1234) 读 note;sql 的两个占位符依次是 zone_id、account_id。没有行返回 empty。 */
    private static java.util.Optional<String> whitelistNote(Connection connection, String sql) throws java.sql.SQLException {
        try (PreparedStatement statement = connection.prepareStatement(sql)) {
            statement.setLong(1, 3L);
            statement.setLong(2, 1234L);
            try (ResultSet rows = statement.executeQuery()) {
                return rows.next() ? java.util.Optional.of(rows.getString("note")) : java.util.Optional.empty();
            }
        }
    }

    @ParameterizedTest
    @ValueSource(strings = {"jdbc:mysql://127.0.0.1:24000/", "jdbc:mysql://127.0.0.2:1/",
            "jdbc:mysql://[::1]:65535/"})
    void ownedEndpointAcceptsOnlyExplicitNumericLoopback(String endpoint) {
        requireOwnedTiDbEndpoint(endpoint);
    }

    @ParameterizedTest
    @ValueSource(strings = {"jdbc:mysql://localhost:24000/", "jdbc:mysql://192.0.2.1:24000/",
            "jdbc:mysql://127.0.0.1/", "jdbc:mysql://127.0.0.1:0/", "jdbc:mysql://127.0.0.1:65536/",
            "jdbc:mysql://127.0.0.1:24000/game", "jdbc:mysql://user:password@127.0.0.1:24000/",
            "jdbc:mysql://127.0.0.1:24000/?tidb_snapshot=old", "jdbc:mysql://127.0.0.1:24000/#fragment",
            "jdbc:h2:mem:foreign", "jdbc:mysql://[::2]:24000/"})
    void ownedEndpointRejectsForeignDatabaseOrImplicitConfiguration(String endpoint) {
        assertThrows(AssertionError.class, () -> requireOwnedTiDbEndpoint(endpoint));
    }

    private static void requireOwnedTiDbEndpoint(String endpoint) {
        try {
            assertNotNull(endpoint, "必须显式配置本任务已核 TiDB 端点");
            assertTrue(endpoint.startsWith("jdbc:mysql://"));
            URI uri = URI.create(endpoint.substring("jdbc:".length()));
            String host = uri.getHost();
            assertNotNull(host);
            host = host.replace("[", "").replace("]", "");
            assertTrue(host.matches("[0-9a-fA-F:.]+"), "不解析 DNS 或接纳主机别名");
            assertTrue(InetAddress.getByName(host).isLoopbackAddress(), "只允许数字 loopback");
            assertTrue(uri.getPort() > 0 && uri.getPort() <= 65535, "必须显式合法非零端口");
            assertEquals("/", uri.getRawPath(), "禁止指向已有业务库");
            assertNull(uri.getRawUserInfo(), "凭据只从原环境变量读取");
            assertNull(uri.getRawQuery(), "禁止用 URL 参数替换强读契约");
            assertNull(uri.getRawFragment());
        } catch (Exception invalid) {
            throw new AssertionError("必须使用已核数字 loopback、合法显式端口与无库名 TiDB 根 URL", invalid);
        }
    }

    // 仅测试夹具：DDL 前确认真实 TiDB，并让本例 JPA 的每条连接使用同一强读会话。
    private static Connection strongTiDbConnection(Connection connection) throws java.sql.SQLException {
        try (Statement statement = connection.createStatement()) {
            statement.setQueryTimeout(5);
            try (ResultSet rows = statement.executeQuery("SELECT VERSION(), TIDB_VERSION()")) {
                assertTrue(rows.next());
                assertTrue(rows.getString(1).toLowerCase(Locale.ROOT).contains("tidb"), "必须验证真实 TiDB");
                String buildDetails = rows.getString(2);
                assertNotNull(buildDetails, "必须取得 TiDB 构建原件");
                String releaseVersion = buildDetails.lines().findFirst().orElse("");
                assertTrue(releaseVersion.startsWith("Release Version: "), "必须取得 TiDB 发布版本行");
                assertFalse(releaseVersion.substring("Release Version: ".length()).isBlank(),
                        "TiDB 发布版本不能为空");
            }
            connection.setTransactionIsolation(Connection.TRANSACTION_READ_COMMITTED);
            statement.execute("SET SESSION tidb_txn_mode='pessimistic',tidb_read_staleness=0,"
                    + "tidb_low_resolution_tso=OFF,tidb_replica_read='leader',"
                    + "tidb_read_consistency='strict',tidb_snapshot=''");
            try (ResultSet rows = statement.executeQuery("SELECT @@transaction_isolation,@@tidb_txn_mode,"
                    + "@@tidb_read_staleness,@@tidb_low_resolution_tso,@@tidb_replica_read,"
                    + "@@tidb_read_consistency,@@tidb_snapshot")) {
                assertTrue(rows.next());
                assertEquals("READ-COMMITTED", rows.getString(1));
                assertEquals("pessimistic", rows.getString(2));
                assertEquals(0, rows.getLong(3));
                assertEquals(0, rows.getLong(4));
                assertEquals("leader", rows.getString(5));
                assertEquals("strict", rows.getString(6));
                assertEquals("", rows.getString(7));
            }
            return connection;
        } catch (java.sql.SQLException | RuntimeException | Error failure) {
            try { connection.close(); } catch (java.sql.SQLException close) { failure.addSuppressed(close); }
            throw failure;
        }
    }

    private static void assertFormalSchema(DataSource dataSource) throws Exception {
        try (Connection connection = dataSource.getConnection()) {
            ScriptUtils.executeSqlScript(connection, new ClassPathResource("schema.sql"));
        }
        StandardServiceRegistry registry = new StandardServiceRegistryBuilder()
                .applySetting("hibernate.connection.datasource", dataSource)
                .applySetting("hibernate.dialect", "org.hibernate.dialect.MySQLDialect")
                .applySetting("hibernate.hbm2ddl.auto", "validate")
                .applySetting("hibernate.show_sql", false)
                .build();
        try {
            try (SessionFactory factory = new MetadataSources(registry)
                    .addAnnotatedClass(ZoneConfig.class)
                    .addAnnotatedClass(ZoneWhitelist.class)
                    .addAnnotatedClass(Announcement.class)
                    .buildMetadata().buildSessionFactory()) {
                ZoneConfig zone = new ZoneConfig();
                zone.setZoneId(MAX_UINT32);
                zone.setName("uint32-zone");
                ZoneWhitelist whitelist = new ZoneWhitelist();
                whitelist.setZoneId(MAX_UINT32);
                whitelist.setAccountId(7L);
                whitelist.setNote("uint32-whitelist");
                Announcement announcement = new Announcement();
                announcement.setTitle("schema-contract");
                announcement.setContent("正式建表后通过实体写入");
                factory.inTransaction(session -> {
                    session.persist(zone);
                    session.persist(whitelist);
                    session.persist(announcement);
                });
                factory.inSession(session -> {
                    assertEquals(MAX_UINT32, session.find(ZoneConfig.class, MAX_UINT32).getZoneId());
                    // 行版本(2026-11-12):schema.sql 的种子行不写 version,取库缺省 1;缺省丢了种子会插不进或落成 0。
                    assertEquals(1L, session.find(ZoneConfig.class, 1L).getVersion(), "种子区服取 version 的库缺省 1");
                    ZoneWhitelist restored = session.find(ZoneWhitelist.class, whitelist.getId());
                    assertNotNull(restored);
                    assertEquals(MAX_UINT32, restored.getZoneId());
                    assertEquals("uint32-whitelist", restored.getNote());
                    assertNotNull(session.find(Announcement.class, announcement.getId()));
                });
            }
        } finally {
            StandardServiceRegistryBuilder.destroy(registry);
        }
    }
}
