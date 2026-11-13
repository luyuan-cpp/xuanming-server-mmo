package com.game.authority;

import java.sql.SQLTransientConnectionException;

/**
 * 事务进入前的对端复核没通过：这条物理连接的对端已不合格（mysql 档：主从切换后它连着的已不是可写主库），
 * 或者复核语句本身没跑通。这笔事务一条业务语句都没发，这条物理连接已经丢弃，调用方可以原样重试；
 * 连接池会新建连接，新连接在建连时重新核对。
 *
 * <p>它不是“结局未知”（SQLSTATE 08007）：结局确定是什么都没发生。SQLSTATE 用 08S01，
 * Hikari 按 08 开头把这条连接摘出连接池；Spring 把它翻译成 TransientDataAccessResourceException。
 */
public final class ServerRecheckFailedException extends SQLTransientConnectionException {
    private static final long serialVersionUID = 1L;

    public ServerRecheckFailedException(Throwable cause) {
        super("server recheck before the transaction failed; the physical connection was discarded and no business statement was sent, retry is safe", "08S01", cause);
    }
}
