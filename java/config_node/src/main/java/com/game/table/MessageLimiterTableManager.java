
package com.game.table;

import com.google.protobuf.util.JsonFormat;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ThreadLocalRandom;
import java.util.function.Predicate;

/**
 * Auto-generated config manager for MessageLimiter.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class MessageLimiterTableManager {

    private static final MessageLimiterTableManager INSTANCE = new MessageLimiterTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final MessageLimiterTableData data;
        final Map<Integer, MessageLimiterTable> kvData;





        Snapshot(MessageLimiterTableData data,
                 Map<Integer, MessageLimiterTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            MessageLimiterTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static MessageLimiterTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        MessageLimiterTableData.Builder builder = MessageLimiterTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "messagelimiter.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "messagelimiter.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        MessageLimiterTableData data = builder.build();

        Map<Integer, MessageLimiterTable> kvData = new HashMap<>(data.getDataCount());

        for (MessageLimiterTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public MessageLimiterTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public MessageLimiterTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, MessageLimiterTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }










    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }





    // ---- FindByIds (IN) ----

    public List<MessageLimiterTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<MessageLimiterTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            MessageLimiterTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public MessageLimiterTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<MessageLimiterTable> where(Predicate<MessageLimiterTable> pred) {
        Snapshot snap = this.snapshot;
        List<MessageLimiterTable> result = new ArrayList<>();
        for (MessageLimiterTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public MessageLimiterTable first(Predicate<MessageLimiterTable> pred) {
        Snapshot snap = this.snapshot;
        for (MessageLimiterTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}