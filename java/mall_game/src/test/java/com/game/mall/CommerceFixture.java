package com.game.mall;

import java.time.Clock;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.UUID;
import java.sql.*;
import javax.sql.DataSource;
import org.springframework.boot.autoconfigure.jdbc.DataSourceProperties;
import org.h2.jdbcx.JdbcDataSource;
import org.springframework.core.io.ClassPathResource;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.jdbc.datasource.DataSourceTransactionManager;
import org.springframework.jdbc.datasource.init.ResourceDatabasePopulator;

final class CommerceFixture implements AutoCloseable {
    final DataSource source;
    final String jdbcUrl;
    private final OwnedTiDbFixture.Owner owner;
    final JdbcTemplate jdbc;
    final CommerceProperties props = new CommerceProperties();
    final CommerceStore store;
    final DataSourceTransactionManager manager;
    CommerceFixture() {
        props.setGatewaySecret("test-only-gateway-key-00000000000001"); props.setPaymentSecret("test-only-payment-key-00000000000002");
        // 旧回执夹具显式允许test；运行模式本身不能授权任意支付渠道，mock仍默认关闭。
        props.setAllowedPaymentChannels(java.util.Set.of("test")); props.validate();
        String endpoint = System.getProperty("commerce.it.tidb");
        if (endpoint == null) {
            owner = null; jdbcUrl = "jdbc:h2:mem:commerce_" + UUID.randomUUID() + ";MODE=MySQL;DATABASE_TO_LOWER=TRUE;DB_CLOSE_DELAY=-1;LOCK_TIMEOUT=4000";
            JdbcDataSource h2 = new JdbcDataSource(); h2.setURL(jdbcUrl); source = h2;
            props.setRunMode("test"); props.setRequireTidb(false);
        } else {
            props.setRunMode("prod"); props.setRequireTidb(true);
            try { owner = OwnedTiDbFixture.open(); jdbcUrl = owner.jdbcUrl(); source = owner.openPool(props); }
            catch (java.io.IOException | SQLException | RuntimeException failure) {
                throw new IllegalStateException("商城真实夹具数据源初始化失败", failure);
            }
        }
        jdbc = new JdbcTemplate(source);
        manager = new DataSourceTransactionManager(source);
        try {
            // TiDB DDL 走独立安装连接预算，生产请求池仍保留原6秒超时。
            // 多个随机库并行建表时，不能让连接的请求预算冒充迁移预算。
            if (owner == null) { migrate("db/schema.sql"); }
            props.validate(); new DatabaseConfiguration().commerceSchemaReady(jdbc, props, source).afterPropertiesSet();
            store = new CommerceStore(jdbc, manager, Clock.fixed(Instant.ofEpochMilli(1_760_000_000_000L), ZoneOffset.UTC), props);
        } catch (Exception failure) {
            try { close(); } catch (RuntimeException cleanup) { failure.addSuppressed(cleanup); }
            throw new IllegalStateException("商城隔离测试库初始化失败", failure);
        }
    }
    void migrate(String resource) {
        if (owner != null) {
            try { owner.migrate(resource); return; }
            catch (java.io.IOException failed) { throw new IllegalStateException("商城自有真库迁移未确认", failed); }
        }
        new ResourceDatabasePopulator(new ClassPathResource(resource)).execute(source);
    }
    OwnedTiDbFixture.OwnedPool openAdditionalPool(DataSourceProperties properties) throws java.io.IOException, SQLException {
        if (owner == null) { throw new IllegalStateException("额外NORMAL boot仅用于显式真TiDB夹具"); }
        return owner.openPool(properties, props);
    }
    void sku(long id, int stock, long price) {
        jdbc.update("INSERT INTO pms_sku_stock(id,product_id,sku_code,price_minor,currency,stock,lock_stock,published) VALUES (?,?,?,?,'CNY',?,0,1)", id, id, "sku-" + id, price, stock);
        jdbc.update("INSERT INTO commerce_sku_grant(sku_id,item_config_id,count_per_unit,title) VALUES(?,10,1,'玄铁')",id);
    }
    long value(String table, String column) { return jdbc.queryForObject("SELECT COALESCE(SUM(" + column + "),0) FROM " + table, Long.class); }
    int count(String table) { return jdbc.queryForObject("SELECT COUNT(*) FROM " + table, Integer.class); }
    @Override public void close() {
        if (owner == null) { jdbc.execute("SHUTDOWN"); return; }
        owner.close();
    }
}
