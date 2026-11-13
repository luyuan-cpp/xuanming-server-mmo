package com.game.gateway.ratelimit;

import io.lettuce.core.AbstractRedisClient;
import io.lettuce.core.RedisClient;
import io.lettuce.core.api.StatefulConnection;
import io.lettuce.core.api.StatefulRedisConnection;
import io.lettuce.core.api.sync.RedisScriptingCommands;
import io.lettuce.core.cluster.RedisClusterClient;
import io.lettuce.core.cluster.api.StatefulRedisClusterConnection;
import io.lettuce.core.codec.ByteArrayCodec;

/**
 * 限流专用的那一条 Lettuce 连接(2026-11-12,docs/design/redis-cluster-all-domains-20261110.md §4.5、§5.3)。
 *
 * <p>两种形态,对 {@link RedisRateLimitStore} 一样:单实例({@link RedisClient},本机开发与还没上控制集群的部署)、
 * Redis Cluster({@link RedisClusterClient},按键路由、跟随 MOVED / ASK)。存储只用到脚本命令,两种连接的同步接口都有。
 *
 * <p>连接在 {@link #connect} 时建立:连不上直接抛异常,随 Spring 启动失败(与此前单实例的启动期依赖相同)。
 * {@link #close} 只关连接;客户端由它自己的 bean 关({@link RateLimitConfig#rateLimitRedisClient},Spring 先销毁依赖它的这一个)。
 */
public final class RateLimitRedis implements AutoCloseable {

    private final StatefulConnection<byte[], byte[]> connection;
    private final RedisScriptingCommands<byte[], byte[]> commands;

    private RateLimitRedis(StatefulConnection<byte[], byte[]> connection, RedisScriptingCommands<byte[], byte[]> commands) {
        this.connection = connection;
        this.commands = commands;
    }

    /** 在 {@code client} 上建一条字节码连接。只认单实例与集群两种客户端。 */
    static RateLimitRedis connect(AbstractRedisClient client) {
        if (client instanceof RedisClusterClient cluster) {
            StatefulRedisClusterConnection<byte[], byte[]> connection = cluster.connect(ByteArrayCodec.INSTANCE);
            return new RateLimitRedis(connection, connection.sync());
        }
        if (client instanceof RedisClient single) {
            StatefulRedisConnection<byte[], byte[]> connection = single.connect(ByteArrayCodec.INSTANCE);
            return new RateLimitRedis(connection, connection.sync());
        }
        throw new IllegalArgumentException("unsupported Redis client type " + client.getClass().getName());
    }

    /** 同步接口(多线程共用这一条连接);阻塞。 */
    RedisScriptingCommands<byte[], byte[]> commands() {
        return commands;
    }

    @Override
    public void close() {
        connection.close();
    }
}
