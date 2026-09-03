
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
 * Auto-generated config manager for AttributeDimension.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class AttributeDimensionTableManager {

    private static final AttributeDimensionTableManager INSTANCE = new AttributeDimensionTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final AttributeDimensionTableData data;
        final Map<Integer, AttributeDimensionTable> kvData;




        final Map<Integer, List<AttributeDimensionTable>> idxPoolId;


        Snapshot(AttributeDimensionTableData data,
                 Map<Integer, AttributeDimensionTable> kvData,
                 Map<Integer, List<AttributeDimensionTable>> idxPoolId) {
            this.data = data;
            this.kvData = kvData;
            this.idxPoolId = idxPoolId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            AttributeDimensionTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static AttributeDimensionTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        AttributeDimensionTableData.Builder builder = AttributeDimensionTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "attributedimension.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "attributedimension.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        AttributeDimensionTableData data = builder.build();

        Map<Integer, AttributeDimensionTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<AttributeDimensionTable>> idxPoolId = new HashMap<>();

        for (AttributeDimensionTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxPoolId.computeIfAbsent(row.getPoolId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxPoolId);
    }

    public AttributeDimensionTableData findAll() {
        return snapshot.data;
    }

    public AttributeDimensionTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, AttributeDimensionTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<AttributeDimensionTable> getByPoolId(int key) {
        return snapshot.idxPoolId.getOrDefault(key, Collections.emptyList());
    }



    // FK: pool_id → AttributePool.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByPoolIdIndex(int key) {
        return snapshot.idxPoolId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<AttributeDimensionTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<AttributeDimensionTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            AttributeDimensionTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public AttributeDimensionTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<AttributeDimensionTable> where(Predicate<AttributeDimensionTable> pred) {
        Snapshot snap = this.snapshot;
        List<AttributeDimensionTable> result = new ArrayList<>();
        for (AttributeDimensionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public AttributeDimensionTable first(Predicate<AttributeDimensionTable> pred) {
        Snapshot snap = this.snapshot;
        for (AttributeDimensionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}