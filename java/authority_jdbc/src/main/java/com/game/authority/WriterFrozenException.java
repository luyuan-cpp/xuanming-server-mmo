package com.game.authority;

import java.sql.SQLTransientException;

/**
 * 写者闸门被冻结：route_meta.writable=0（搬库切换窗口、维护排空）。这笔事务一条业务语句都没发，调用方可以原样重试。
 * 资格不符（主体、世代、写者已退休）不用这个类型，那种错误重试没用。
 *
 * <p>SQLSTATE 40001 和事务进入期间 epoch 变化时的错误相同，调用方按同一口径重试；Spring 把它翻译成
 * TransientDataAccessException 一族。
 */
public final class WriterFrozenException extends SQLTransientException {
    private static final long serialVersionUID = 1L;

    public WriterFrozenException() {
        super("写者闸门已冻结，可重试", "40001");
    }
}
