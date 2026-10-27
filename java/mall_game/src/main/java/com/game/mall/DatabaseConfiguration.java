package com.game.mall;

import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;
import java.time.Clock;
import javax.sql.DataSource;
import org.springframework.beans.factory.InitializingBean;
import org.springframework.boot.autoconfigure.jdbc.DataSourceProperties;
import org.springframework.boot.sql.init.dependency.DependsOnDatabaseInitialization;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.datasource.SingleConnectionDataSource;

@Configuration
public class DatabaseConfiguration {
    @Bean Clock commerceClock() { return Clock.systemUTC(); }
    @Bean DataSource dataSource(DataSourceProperties source, CommerceProperties props) throws java.io.IOException, java.sql.SQLException {
        HikariConfig config = new HikariConfig();
        config.setJdbcUrl(source.getUrl()); config.setUsername(source.getUsername()); config.setPassword(source.getPassword());
        config.setMaximumPoolSize(16); config.setConnectionTimeout(3000); config.setValidationTimeout(2000);
        if (source.getUrl() != null && source.getUrl().startsWith("jdbc:mysql:")) {
            config.addDataSourceProperty("connectTimeout", "3000"); config.addDataSourceProperty("socketTimeout", "6000");
        }
        config.setTransactionIsolation("TRANSACTION_READ_COMMITTED");
        // 每个新物理连接都设置事务模式，不能只在启动探测连接上设置。
        if (props.isRequireTidb()) {
            // 先清除 JDBC URL 注入的历史快照，再固定强读；不能依赖外部会话缺省值。
            config.setConnectionInitSql("SET SESSION tidb_snapshot='',tidb_txn_mode='pessimistic',tidb_replica_read='leader',tidb_read_consistency='strict',tidb_read_staleness=0,tidb_low_resolution_tso=OFF,sql_mode='STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION',innodb_lock_wait_timeout=5");
        } else if (source.getUrl() != null && source.getUrl().startsWith("jdbc:mysql:")) {
            config.setConnectionInitSql("SET SESSION sql_mode='STRICT_TRANS_TABLES,NO_ENGINE_SUBSTITUTION',innodb_lock_wait_timeout=5");
        }
        var credentials=new java.util.Properties();credentials.putAll(config.getDataSourceProperties());
        credentials.setProperty("user",source.getUsername());credentials.setProperty("password",source.getPassword());
        String init=config.getConnectionInitSql();
        var boot=com.game.authority.WriterAuthority.consumeEnvironment();
        config.setDataSource(new com.game.authority.AuthorityDataSource(source.getUrl(),credentials,
                boot,props.isRequireTidb(),init==null?java.util.List.of():java.util.List.of(init)));
        config.setConnectionInitSql(null);
        return new HikariDataSource(config){@Override public void close(){boot.seal();super.close();}};
    }
    @Bean @DependsOnDatabaseInitialization
    InitializingBean commerceSchemaReady(JdbcTemplate jdbc, CommerceProperties props, DataSource source) {
        return () -> {
            if (props.isRequireTidb()) {
                // 所有会话证据必须来自同一条实际持有的连接。
                try (var connection = source.getConnection()) {
                    JdbcTemplate session = new JdbcTemplate(new SingleConnectionDataSource(connection, true));
                    session.setQueryTimeout(5);
                    String version = session.queryForObject("SELECT tidb_version()", String.class);
                    String mode = session.queryForObject("SELECT @@session.tidb_txn_mode", String.class);
                    String read = session.queryForObject("SELECT @@session.tidb_replica_read", String.class);
                    Long staleness = session.queryForObject("SELECT @@session.tidb_read_staleness", Long.class);
                    Integer lowResolution = session.queryForObject("SELECT @@session.tidb_low_resolution_tso", Integer.class);
                    String readConsistency = session.queryForObject("SELECT @@session.tidb_read_consistency", String.class);
                    String snapshot = session.queryForObject("SELECT @@session.tidb_snapshot", String.class);
                    String sqlMode = session.queryForObject("SELECT @@session.sql_mode", String.class);
                    // TIDB_VERSION() 的正文不保证含 TiDB 字样；成功调用专属函数及变量、内容非空才是能力证据。
                    if (version == null || version.isBlank() || !"pessimistic".equalsIgnoreCase(mode) || !"leader".equalsIgnoreCase(read) || staleness == null || staleness != 0 || lowResolution == null || lowResolution != 0 || !"strict".equalsIgnoreCase(readConsistency) || snapshot == null || !snapshot.isEmpty() || sqlMode == null || !(sqlMode.contains("STRICT_TRANS_TABLES") || sqlMode.contains("STRICT_ALL_TABLES"))) {
                        throw new IllegalStateException("商城生产数据库必须为 TiDB 悲观事务逻辑权威库");
                    }
                }
            }
            // 版本、列类型、唯一约束缺失即拒启，不能把 CREATE IF NOT EXISTS 当兼容迁移。
            SchemaVerifier.verify(jdbc, source);
        };
    }
}
