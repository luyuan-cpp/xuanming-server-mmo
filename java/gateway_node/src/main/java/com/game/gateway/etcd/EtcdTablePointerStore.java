package com.game.gateway.etcd;

import com.game.gateway.service.TablePointerStore;
import io.etcd.jetcd.ByteSequence;
import io.etcd.jetcd.Client;
import io.etcd.jetcd.KeyValue;
import io.etcd.jetcd.kv.GetResponse;
import io.etcd.jetcd.kv.TxnResponse;
import io.etcd.jetcd.op.Cmp;
import io.etcd.jetcd.op.CmpTarget;
import io.etcd.jetcd.op.Op;
import io.etcd.jetcd.options.DeleteOption;
import io.etcd.jetcd.options.GetOption;
import io.etcd.jetcd.options.PutOption;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Component;

import java.nio.charset.StandardCharsets;
import java.util.Collections;
import java.util.Map;
import java.util.Optional;
import java.util.TreeMap;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;

/**
 * {@link TablePointerStore} 的 etcd 实现(与 {@link EtcdKillSwitchRuleStore} 同口径):
 * 使用 etcd Txn 对 {@code mod_revision} 做原子比较并写/比较并删,带超时截止防止挂住管理接口线程。
 */
@Component
public class EtcdTablePointerStore implements TablePointerStore {

    private final Client etcd;
    private final long timeoutMs;

    public EtcdTablePointerStore(Client etcd, @Value("${tables.etcd-timeout-ms:3000}") long timeoutMs) {
        this.etcd = etcd;
        this.timeoutMs = timeoutMs;
    }

    @Override
    public Optional<Entry> get(String key) {
        GetResponse resp = await(etcd.getKVClient().get(bytes(key)), "get " + key);
        if (resp.getKvs().isEmpty()) {
            return Optional.empty();
        }
        KeyValue kv = resp.getKvs().get(0);
        return Optional.of(new Entry(kv.getValue().toString(StandardCharsets.UTF_8), kv.getModRevision()));
    }

    @Override
    public Map<String, Entry> list(String prefix) {
        GetResponse resp = await(etcd.getKVClient().get(bytes(prefix), GetOption.builder().isPrefix(true).build()),
                "get prefix " + prefix);
        Map<String, Entry> result = new TreeMap<>();
        for (KeyValue kv : resp.getKvs()) {
            result.put(kv.getKey().toString(StandardCharsets.UTF_8),
                    new Entry(kv.getValue().toString(StandardCharsets.UTF_8), kv.getModRevision()));
        }
        return Collections.unmodifiableMap(result);
    }

    @Override
    public boolean compareAndPut(String key, long expectedModRevision, String value) {
        ByteSequence k = bytes(key);
        Cmp cmp = new Cmp(k, Cmp.Op.EQUAL, CmpTarget.modRevision(expectedModRevision));
        Op put = Op.put(k, bytes(value), PutOption.builder().build());
        TxnResponse resp = await(etcd.getKVClient().txn().If(cmp).Then(put).commit(), "cas put " + key);
        return resp.isSucceeded();
    }

    @Override
    public boolean compareAndDelete(String key, long expectedModRevision) {
        ByteSequence k = bytes(key);
        Cmp cmp = new Cmp(k, Cmp.Op.EQUAL, CmpTarget.modRevision(expectedModRevision));
        Op del = Op.delete(k, DeleteOption.builder().build());
        TxnResponse resp = await(etcd.getKVClient().txn().If(cmp).Then(del).commit(), "cas delete " + key);
        return resp.isSucceeded();
    }

    private static ByteSequence bytes(String text) {
        return ByteSequence.from(text, StandardCharsets.UTF_8);
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
