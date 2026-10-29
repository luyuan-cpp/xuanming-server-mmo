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
