package com.game.gateway.etcd;

import com.game.gateway.service.ZonePolicyStore;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.kv.GetResponse;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

import java.nio.charset.StandardCharsets;
import java.util.Optional;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;

/**
 * {@link ZonePolicyStore} 的 etcd 实现:单键点读、单键写,不挂租约。每次请求有 {@code zone-policy.etcd-timeout-ms} 的截止
 * (缺省 3 秒,小于发布周期 5 秒,读写挂住时不至于让调度线程堆积)。
 */
@Component
public class EtcdZonePolicyStore implements ZonePolicyStore {

    private final Client etcd;
    private final long timeoutMs;

    public EtcdZonePolicyStore(Client etcd, @Value("${zone-policy.etcd-timeout-ms:3000}") long timeoutMs) {
        this.etcd = etcd;
        this.timeoutMs = timeoutMs;
    }

    @Override
    public Optional<byte[]> get(String key) {
        GetResponse resp = await(etcd.getKVClient().get(ByteSequence.from(key, StandardCharsets.UTF_8)), "get " + key);
        if (resp.getKvs().isEmpty()) {
            return Optional.empty();
        }
        return Optional.of(resp.getKvs().get(0).getValue().getBytes());
    }

    @Override
    public void put(String key, byte[] value) {
        await(etcd.getKVClient().put(ByteSequence.from(key, StandardCharsets.UTF_8), ByteSequence.from(value)), "put " + key);
    }

    private <R> R await(CompletableFuture<R> future, String what) {
        try {
            return future.get(timeoutMs, TimeUnit.MILLISECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new IllegalStateException("interrupted while waiting for etcd " + what, e);
        } catch (ExecutionException e) {
            throw new IllegalStateException("etcd " + what + " failed: " + e.getCause(), e.getCause());
        } catch (TimeoutException e) {
            future.cancel(true);
            throw new IllegalStateException("etcd " + what + " timed out after " + timeoutMs + " ms", e);
        }
    }
}
