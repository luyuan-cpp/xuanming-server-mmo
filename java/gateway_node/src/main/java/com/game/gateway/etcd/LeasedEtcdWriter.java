package com.game.gateway.etcd;

import com.game.gateway.config.GateProperties;
import com.game.gateway.config.ZoneProbeProperties;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.common.exception.ErrorCode;
import io.etcd.jetcd.common.exception.EtcdException;
import io.etcd.jetcd.options.PutOption;
import jakarta.annotation.PreDestroy;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

import java.nio.charset.StandardCharsets;
import java.util.UUID;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;
import java.util.function.LongSupplier;

/**
 * 本进程往 etcd 写「我还活着才算数」的键的入口(2026-11-11,服务器缺口清单第 29 条):一份进程级租约,写的键都挂在它上面,
 * 进程没了,键随租约过期消失。现在的用户是区服健康汇总的发布者(docs/design/gateway-zone-health-publish-20261111.md §4.2)。
 *
 * <p>调用方每拍:
 * <ol>
 *   <li>{@link #renewLease()} 续租;租约丢了就申请一份新的,返回的代次加一。代次变了,说明挂在旧租约上的键已经随它消失,
 *       调用方要把自己的键全部重写一遍;</li>
 *   <li>{@link #put} 写(挂在当前租约上),{@link #delete} 删。</li>
 * </ol>
 *
 * <p>续租由调用方的循环驱动(每拍一次 keepAliveOnce),不交给 jetcd 的后台续租:发布循环卡死时租约跟着过期、键消失,读方看到的是
 * 「不知道」,而不是一条一直挂着、内容却不再更新的记录。所以调用方的周期必须明显短于租约 TTL(发布者要求至少两倍余量)。
 *
 * <p>租约 TTL 取 {@code zone-probe.status-ttl-ms}(向上取整到秒):发布者死后,它最后写的记录最多再被看到这么久,
 * 与读方「最近一次成功快照只在 TTL 内有效」同一个口径。每次 etcd 请求的期限取 {@code gate.discovery-timeout-ms}。
 *
 * <p>线程安全:租约状态在 {@link #lock} 下读写;{@link #renewLease()} 持锁做 I/O(与关闭互斥),{@link #put} / {@link #delete} 不持锁做 I/O。
 */
@Component
public class LeasedEtcdWriter {

    private static final Logger log = LoggerFactory.getLogger(LeasedEtcdWriter.class);
    private static final long NO_LEASE = 0L;

    private final Client etcd;
    private final long ttlSeconds;
    private final long ttlMs;
    private final long requestTimeoutMs;
    private final LongSupplier monotonicMs;
    private final String instanceId;

    private final Object lock = new Object();
    // 以下字段都在 lock 下读写。
    private long leaseId = NO_LEASE;
    /** 每申请到一份新租约加一;0 = 还没申请过。 */
    private long generation;
    /** 最近一次确认租约有效(续租成功或刚申请到)时的单调时刻,取在应答之后。 */
    private long renewedAtMs;
    private boolean closed;

    @Autowired
    public LeasedEtcdWriter(Client etcdClient, ZoneProbeProperties probeProps, GateProperties gateProps) {
        this(etcdClient, probeProps.getStatusTtlMs(), gateProps.getDiscoveryTimeoutMs(),
                () -> System.nanoTime() / 1_000_000L, UUID.randomUUID().toString());
    }

    /** 包内可见:单测注入时钟与实例号。 */
    LeasedEtcdWriter(Client etcdClient, long leaseTtlMs, long requestTimeoutMs, LongSupplier monotonicMs, String instanceId) {
        if (leaseTtlMs <= 0) {
            throw new IllegalArgumentException("lease TTL must be positive, got " + leaseTtlMs + " ms");
        }
        this.etcd = etcdClient;
        this.ttlSeconds = Math.max(1L, (leaseTtlMs + 999L) / 1000L);
        this.ttlMs = ttlSeconds * 1000L;
        this.requestTimeoutMs = requestTimeoutMs;
        this.monotonicMs = monotonicMs;
        this.instanceId = instanceId;
    }

    /** 本进程的实例号(启动时随机生成),发布者把它放进自己的键里,同一 zone 的两个副本各写各的键。 */
    public String instanceId() {
        return instanceId;
    }

    /**
     * 续租;没有租约或租约已经失效时申请一份新的。
     *
     * @return 租约代次:每申请到一份新租约加一。与上次写入时的代次不同,就说明之前写的键已经随旧租约消失
     * @throws EtcdWriteException 需要申请新租约而 etcd 不可用;本进程这一拍写不了任何键
     */
    public long renewLease() {
        synchronized (lock) {
            requireOpenLocked();
            if (leaseId != NO_LEASE) {
                try {
                    long ttl = await(etcd.getLeaseClient().keepAliveOnce(leaseId), "keep-alive of lease " + hex(leaseId)).getTTL();
                    if (ttl > 0) {
                        renewedAtMs = monotonicMs.getAsLong();
                        return generation;
                    }
                    log.warn("etcd lease {} has expired (ttl={}), granting a new one", hex(leaseId), ttl);
                } catch (EtcdWriteException e) {
                    if (!isLeaseNotFound(e) && monotonicMs.getAsLong() - renewedAtMs < ttlMs) {
                        // 可能只是这一次请求失败,租约在服务端还活着:沿用。真丢了的话,本拍的 put 会以 NOT_FOUND 失败并作废它。
                        log.warn("etcd lease {} keep-alive failed, keeping the lease for now: {}", hex(leaseId), e.getMessage());
                        return generation;
                    }
                    log.warn("etcd lease {} is gone, granting a new one: {}", hex(leaseId), e.getMessage());
                }
                leaseId = NO_LEASE;
            }
            long granted = await(etcd.getLeaseClient().grant(ttlSeconds), "lease grant").getID();
            leaseId = granted;
            generation++;
            renewedAtMs = monotonicMs.getAsLong();
            log.info("etcd lease granted: lease={}, ttl={}s, generation={}, instance={}",
                    hex(granted), ttlSeconds, generation, instanceId);
            return generation;
        }
    }

    /**
     * 把 {@code key} 写成 {@code value},挂在当前租约上。
     *
     * @throws EtcdWriteException 还没有租约、写失败或超时。租约在服务端已经不存在时(NOT_FOUND)顺带作废它,
     *                            下一次 {@link #renewLease()} 申请新租约、代次加一
     */
    public void put(String key, String value) {
        long lease;
        synchronized (lock) {
            requireOpenLocked();
            lease = leaseId;
        }
        if (lease == NO_LEASE) {
            throw new EtcdWriteException("no etcd lease to attach " + key + " to; renew the lease first", null);
        }
        try {
            await(etcd.getKVClient().put(bytes(key), bytes(value), PutOption.builder().withLeaseId(lease).build()),
                    "put of " + key);
        } catch (EtcdWriteException e) {
            if (isLeaseNotFound(e)) {
                synchronized (lock) {
                    if (leaseId == lease) {
                        leaseId = NO_LEASE;
                    }
                }
            }
            throw e;
        }
    }

    /**
     * 删除 {@code key}(不在也算成功)。
     *
     * @throws EtcdWriteException 删失败或超时
     */
    public void delete(String key) {
        synchronized (lock) {
            requireOpenLocked();
        }
        await(etcd.getKVClient().delete(bytes(key)), "delete of " + key);
    }

    /** 撤销租约:本进程挂在它上面的键立即消失(正常停机时读方马上改看同 zone 另一个副本的记录)。之后不能再写。 */
    @PreDestroy
    public void close() {
        long lease;
        synchronized (lock) {
            if (closed) {
                return;
            }
            closed = true;
            lease = leaseId;
            leaseId = NO_LEASE;
        }
        if (lease == NO_LEASE) {
            return;
        }
        try {
            await(etcd.getLeaseClient().revoke(lease), "revoke of lease " + hex(lease));
            log.info("etcd lease {} revoked, keys of instance {} removed", hex(lease), instanceId);
        } catch (EtcdWriteException e) {
            log.warn("etcd lease {} could not be revoked, its keys expire within {}s: {}", hex(lease), ttlSeconds, e.getMessage());
        }
    }

    /** 失败原因链上有 etcd 的 NOT_FOUND:对租约续期和带租约的写来说,就是租约在服务端已经不存在。 */
    static boolean isLeaseNotFound(Throwable error) {
        for (Throwable cause = error; cause != null; cause = cause.getCause()) {
            if (cause instanceof EtcdException etcdError && etcdError.getErrorCode() == ErrorCode.NOT_FOUND) {
                return true;
            }
        }
        return false;
    }

    private void requireOpenLocked() {
        if (closed) {
            throw new EtcdWriteException("etcd writer of instance " + instanceId + " is closed", null);
        }
    }

    private <R> R await(CompletableFuture<R> future, String what) {
        try {
            return future.get(requestTimeoutMs, TimeUnit.MILLISECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new EtcdWriteException("interrupted while waiting for etcd " + what, e);
        } catch (ExecutionException e) {
            throw new EtcdWriteException("etcd " + what + " failed: " + e.getCause(), e.getCause());
        } catch (TimeoutException e) {
            future.cancel(true);
            throw new EtcdWriteException("etcd " + what + " timed out after " + requestTimeoutMs + " ms", e);
        }
    }

    private static ByteSequence bytes(String text) {
        return ByteSequence.from(text, StandardCharsets.UTF_8);
    }

    private static String hex(long id) {
        return Long.toHexString(id);
    }

    /** 写 etcd 失败(含还没有租约、已关闭)。 */
    public static class EtcdWriteException extends RuntimeException {
        public EtcdWriteException(String message, Throwable cause) {
            super(message, cause);
        }
    }
}
