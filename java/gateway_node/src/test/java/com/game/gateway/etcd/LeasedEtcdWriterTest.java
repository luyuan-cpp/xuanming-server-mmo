package com.game.gateway.etcd;

import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.KV;
import io.etcd.jetcd.Lease;
import io.etcd.jetcd.common.exception.ErrorCode;
import io.etcd.jetcd.common.exception.EtcdExceptionFactory;
import io.etcd.jetcd.kv.DeleteResponse;
import io.etcd.jetcd.kv.PutResponse;
import io.etcd.jetcd.lease.LeaseGrantResponse;
import io.etcd.jetcd.lease.LeaseKeepAliveResponse;
import io.etcd.jetcd.lease.LeaseRevokeResponse;
import io.etcd.jetcd.options.PutOption;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.mockito.ArgumentCaptor;

import java.nio.charset.StandardCharsets;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.atomic.AtomicLong;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyLong;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * {@link LeasedEtcdWriter}:第一次续租申请租约;租约活着时只续不申请;租约不存在(NOT_FOUND 或 TTL ≤ 0)就申请新的、代次加一;
 * 一次续租请求失败但还在 TTL 内时沿用旧租约,超过 TTL 才重新申请;写挂在当前租约上,写回 NOT_FOUND 作废租约;
 * 还没有租约时不写;关闭时撤销租约、之后拒绝写。
 */
class LeasedEtcdWriterTest {

    private static final long TTL_MS = 15_000L;

    private Lease lease;
    private KV kv;
    private final AtomicLong clock = new AtomicLong(1_000L);
    private LeasedEtcdWriter writer;

    @BeforeEach
    void setUp() {
        Client client = mock(Client.class);
        lease = mock(Lease.class);
        kv = mock(KV.class);
        when(client.getLeaseClient()).thenReturn(lease);
        when(client.getKVClient()).thenReturn(kv);
        when(kv.put(any(ByteSequence.class), any(ByteSequence.class), any(PutOption.class)))
                .thenReturn(CompletableFuture.completedFuture(mock(PutResponse.class)));
        when(kv.delete(any(ByteSequence.class))).thenReturn(CompletableFuture.completedFuture(mock(DeleteResponse.class)));
        when(lease.revoke(anyLong())).thenReturn(CompletableFuture.completedFuture(mock(LeaseRevokeResponse.class)));
        grantReturns(0x10L);
        writer = new LeasedEtcdWriter(client, TTL_MS, 100, clock::get, "instance-1");
    }

    private void grantReturns(long... ids) {
        CompletableFuture<LeaseGrantResponse>[] futures = new CompletableFuture[ids.length];
        for (int i = 0; i < ids.length; i++) {
            LeaseGrantResponse response = mock(LeaseGrantResponse.class);
            when(response.getID()).thenReturn(ids[i]);
            futures[i] = CompletableFuture.completedFuture(response);
        }
        if (futures.length == 1) {
            when(lease.grant(anyLong())).thenReturn(futures[0]);
        } else {
            when(lease.grant(anyLong())).thenReturn(futures[0], java.util.Arrays.copyOfRange(futures, 1, futures.length));
        }
    }

    private void keepAliveReturnsTtl(long ttl) {
        LeaseKeepAliveResponse response = mock(LeaseKeepAliveResponse.class);
        when(response.getTTL()).thenReturn(ttl);
        when(lease.keepAliveOnce(anyLong())).thenReturn(CompletableFuture.completedFuture(response));
    }

    private void keepAliveFails(Throwable error) {
        when(lease.keepAliveOnce(anyLong())).thenReturn(CompletableFuture.failedFuture(error));
    }

    private long putLeaseId() {
        ArgumentCaptor<PutOption> option = ArgumentCaptor.forClass(PutOption.class);
        verify(kv, org.mockito.Mockito.atLeastOnce()).put(any(ByteSequence.class), any(ByteSequence.class), option.capture());
        return option.getValue().getLeaseId();
    }

    @Test
    void firstRenewGrantsLease_andPutsAttachToIt() {
        assertEquals(1L, writer.renewLease());
        verify(lease).grant(15L);

        writer.put("/k", "v");

        ArgumentCaptor<ByteSequence> key = ArgumentCaptor.forClass(ByteSequence.class);
        verify(kv).put(key.capture(), any(ByteSequence.class), any(PutOption.class));
        assertEquals("/k", key.getValue().toString(StandardCharsets.UTF_8));
        assertEquals(0x10L, putLeaseId(), "键挂在本进程的租约上,进程没了键随租约消失");
        assertEquals("instance-1", writer.instanceId());
    }

    @Test
    void ttlIsRoundedUpToWholeSeconds() {
        LeasedEtcdWriter shortLease = new LeasedEtcdWriter(clientWith(lease, kv), 1_500L, 100, clock::get, "i");
        shortLease.renewLease();
        verify(lease).grant(2L);
    }

    @Test
    void liveLeaseIsOnlyRenewed() {
        writer.renewLease();
        keepAliveReturnsTtl(15);

        assertEquals(1L, writer.renewLease());
        assertEquals(1L, writer.renewLease());

        verify(lease, times(1)).grant(anyLong());
        verify(lease, times(2)).keepAliveOnce(0x10L);
    }

    @Test
    void leaseNotFound_grantsNewLeaseWithNextGeneration() {
        grantReturns(0x10L, 0x11L);
        writer.renewLease();
        keepAliveFails(EtcdExceptionFactory.newEtcdException(ErrorCode.NOT_FOUND, "etcdserver: requested lease not found"));

        assertEquals(2L, writer.renewLease(), "租约换了,代次加一,调用方据此重写自己的键");
        writer.put("/k", "v");
        assertEquals(0x11L, putLeaseId());
    }

    @Test
    void expiredTtl_grantsNewLease() {
        grantReturns(0x10L, 0x11L);
        writer.renewLease();
        keepAliveReturnsTtl(0);

        assertEquals(2L, writer.renewLease());
    }

    @Test
    void transientKeepAliveFailure_keepsLeaseUntilTtlHasSurelyPassed() {
        grantReturns(0x10L, 0x11L);
        writer.renewLease();
        keepAliveFails(new RuntimeException("unavailable"));

        clock.addAndGet(TTL_MS - 1);
        assertEquals(1L, writer.renewLease(), "还在 TTL 内:租约在服务端可能还活着,沿用");
        verify(lease, times(1)).grant(anyLong());

        clock.addAndGet(1);
        assertEquals(2L, writer.renewLease(), "距上次确认已满 TTL:旧租约必然过期,申请新的");
    }

    @Test
    void grantFailure_propagates_andNextRenewRetries() {
        when(lease.grant(anyLong())).thenReturn(CompletableFuture.failedFuture(new RuntimeException("etcd down")));
        assertThrows(LeasedEtcdWriter.EtcdWriteException.class, writer::renewLease);
        assertThrows(LeasedEtcdWriter.EtcdWriteException.class, () -> writer.put("/k", "v"), "没有租约时不写");

        grantReturns(0x12L);
        assertEquals(1L, writer.renewLease());
    }

    @Test
    void putWithoutLease_isRejectedWithoutTouchingEtcd() {
        assertThrows(LeasedEtcdWriter.EtcdWriteException.class, () -> writer.put("/k", "v"));
        verify(kv, never()).put(any(ByteSequence.class), any(ByteSequence.class), any(PutOption.class));
    }

    @Test
    void putNotFound_invalidatesLease_soNextRenewGrantsAnother() {
        grantReturns(0x10L, 0x11L);
        writer.renewLease();
        when(kv.put(any(ByteSequence.class), any(ByteSequence.class), any(PutOption.class))).thenReturn(CompletableFuture.failedFuture(
                EtcdExceptionFactory.newEtcdException(ErrorCode.NOT_FOUND, "etcdserver: requested lease not found")));

        LeasedEtcdWriter.EtcdWriteException error = assertThrows(LeasedEtcdWriter.EtcdWriteException.class, () -> writer.put("/k", "v"));
        assertTrue(LeasedEtcdWriter.isLeaseNotFound(error));

        assertEquals(2L, writer.renewLease(), "租约已作废,直接申请新的");
        verify(lease, never()).keepAliveOnce(anyLong());
    }

    @Test
    void deleteRemovesKey() {
        writer.delete("/k");

        ArgumentCaptor<ByteSequence> key = ArgumentCaptor.forClass(ByteSequence.class);
        verify(kv).delete(key.capture());
        assertEquals("/k", key.getValue().toString(StandardCharsets.UTF_8));
    }

    @Test
    void closeRevokesLease_andRejectsLaterWrites() {
        writer.renewLease();

        writer.close();

        verify(lease).revoke(eq(0x10L));
        assertThrows(LeasedEtcdWriter.EtcdWriteException.class, () -> writer.put("/k", "v"));
        assertThrows(LeasedEtcdWriter.EtcdWriteException.class, writer::renewLease);
        writer.close();
        verify(lease, times(1)).revoke(anyLong());
    }

    private static Client clientWith(Lease lease, KV kv) {
        Client client = mock(Client.class);
        when(client.getLeaseClient()).thenReturn(lease);
        when(client.getKVClient()).thenReturn(kv);
        return client;
    }
}
