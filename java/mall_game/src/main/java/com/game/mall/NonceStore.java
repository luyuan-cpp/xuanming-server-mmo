package com.game.mall;

import java.nio.charset.StandardCharsets;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.dao.DuplicateKeyException;
import org.springframework.context.annotation.DependsOn;
import org.springframework.jdbc.core.JdbcTemplate;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Repository;
import org.springframework.transaction.PlatformTransactionManager;
import org.springframework.transaction.TransactionDefinition;
import org.springframework.transaction.support.TransactionTemplate;

@Repository
@DependsOn("commerceSchemaReady")
public class NonceStore {
    private static final Logger LOG = LoggerFactory.getLogger(NonceStore.class);
    private final JdbcTemplate jdbc;
    private final String nowSql;
    private final String pruneExpiredSql;
    private final TransactionTemplate tx;
    public NonceStore(JdbcTemplate jdbc, CommerceProperties props, PlatformTransactionManager manager) {
        this.jdbc = jdbc;
        tx = new TransactionTemplate(manager); tx.setIsolationLevel(TransactionDefinition.ISOLATION_READ_COMMITTED);
        tx.setPropagationBehavior(TransactionDefinition.PROPAGATION_REQUIRES_NEW); tx.setTimeout(5);
        boolean h2 = Boolean.TRUE.equals(jdbc.execute((org.springframework.jdbc.core.ConnectionCallback<Boolean>) connection -> connection.getMetaData().getDatabaseProductName().equals("H2")));
        nowSql = h2 ? "CAST(EXTRACT(EPOCH FROM CURRENT_TIMESTAMP())*1000 AS BIGINT)" : props.isRequireTidb() ? "(TIDB_CURRENT_TSO() >> 18)" : "CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS SIGNED)";
        pruneExpiredSql = h2
                ? "DELETE FROM commerce_request_nonce WHERE expires_ms<? LIMIT 500"
                : "DELETE FROM commerce_request_nonce WHERE expires_ms<? ORDER BY expires_ms LIMIT 500";
        jdbc.setQueryTimeout(5);
    }
    public void claim(String role, String nonce, long sentMs) {
        tx.executeWithoutResult(status -> claimInTransaction(role, nonce, sentMs));
    }
    private void claimInTransaction(String role, String nonce, long sentMs) {
        // TiDB 的 setAutoCommit(false) 是 lazy BEGIN；无真实表访问时 CURRENT_TSO() 仍为 0。
        // 对本请求 nonce 做不加锁主键点读，初始化当前事务，不制造全服共享热锁行。
        jdbc.queryForList("SELECT nonce FROM commerce_request_nonce WHERE caller_role=? AND nonce=?", role, nonce.getBytes(StandardCharsets.US_ASCII));
        long nowMs = jdbc.queryForObject("SELECT " + nowSql, Long.class);
        if (nowMs < HmacAuthentication.WINDOW_MS) { throw new CommerceException(503, "权威认证时间不可用"); }
        // 时窗由同一权威 SQL 时钟在插入语句内裁决，不能用各应用副本的墙钟各自验过期。
        // 去重再保留 10 分钟，覆盖至多 5 秒的 SQL/行锁等待；禁止暂停旧验签结果后重新插入。
        try {
            int changed = jdbc.update("INSERT INTO commerce_request_nonce(caller_role,nonce,expires_ms) SELECT ?,?,? WHERE ?>=" + nowSql + "-120000 AND ?<=" + nowSql + "+120000",
                    role, nonce.getBytes(StandardCharsets.US_ASCII), sentMs + 720_000, sentMs, sentMs);
            if (changed != 1) { throw new CommerceException(401, "签名请求已过期"); }
        }
        catch (DuplicateKeyException replay) { throw new CommerceException(409, "签名请求已使用，请以新 nonce 重试原业务请求号"); }
    }

    /** 定时分批清理过期 nonce，避免热路径鉴权承担跨副本锁等待。 */
    @Scheduled(fixedDelay = 10_000, initialDelay = 10_000)
    void runPruneExpired() {
        try {
            pruneExpired();
        } catch (RuntimeException failed) {
            LOG.warn("scheduled nonce cleanup failed: type={}", failed.getClass().getSimpleName());
        }
    }

    public int pruneExpired() {
        Integer deleted = tx.execute(status -> {
            jdbc.queryForList("SELECT expires_ms FROM commerce_request_nonce LIMIT 1");
            Long nowMs = jdbc.queryForObject("SELECT " + nowSql, Long.class);
            if (nowMs == null || nowMs < HmacAuthentication.WINDOW_MS) { return 0; }
            return jdbc.update(pruneExpiredSql, nowMs);
        });
        return deleted == null ? 0 : deleted;
    }
}
