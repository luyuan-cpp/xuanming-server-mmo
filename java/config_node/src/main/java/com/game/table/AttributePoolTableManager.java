
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
 * Auto-generated config manager for AttributePool.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class AttributePoolTableManager {

    private static final AttributePoolTableManager INSTANCE = new AttributePoolTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final AttributePoolTableData data;
        final Map<Integer, AttributePoolTable> kvData;





        Snapshot(AttributePoolTableData data,
                 Map<Integer, AttributePoolTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            AttributePoolTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static AttributePoolTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        AttributePoolTableData.Builder builder = AttributePoolTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "attributepool.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "attributepool.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        AttributePoolTableData data = builder.build();

        Map<Integer, AttributePoolTable> kvData = new HashMap<>(data.getDataCount());

        for (AttributePoolTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public AttributePoolTableData findAll() {
        return snapshot.data;
    }

    public AttributePoolTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, AttributePoolTable> getKvData() {
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

    public List<AttributePoolTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<AttributePoolTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            AttributePoolTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public AttributePoolTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<AttributePoolTable> where(Predicate<AttributePoolTable> pred) {
        Snapshot snap = this.snapshot;
        List<AttributePoolTable> result = new ArrayList<>();
        for (AttributePoolTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public AttributePoolTable first(Predicate<AttributePoolTable> pred) {
        Snapshot snap = this.snapshot;
        for (AttributePoolTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}