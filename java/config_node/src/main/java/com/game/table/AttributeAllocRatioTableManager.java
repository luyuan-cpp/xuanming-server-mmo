
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
 * Auto-generated config manager for AttributeAllocRatio.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class AttributeAllocRatioTableManager {

    private static final AttributeAllocRatioTableManager INSTANCE = new AttributeAllocRatioTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final AttributeAllocRatioTableData data;
        final Map<Integer, AttributeAllocRatioTable> kvData;




        final Map<Integer, List<AttributeAllocRatioTable>> idxDimensionId;

        final Map<Integer, List<AttributeAllocRatioTable>> idxClassId;


        Snapshot(AttributeAllocRatioTableData data,
                 Map<Integer, AttributeAllocRatioTable> kvData,
                 Map<Integer, List<AttributeAllocRatioTable>> idxDimensionId,
                 Map<Integer, List<AttributeAllocRatioTable>> idxClassId) {
            this.data = data;
            this.kvData = kvData;
            this.idxDimensionId = idxDimensionId;
            this.idxClassId = idxClassId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            AttributeAllocRatioTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static AttributeAllocRatioTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        AttributeAllocRatioTableData.Builder builder = AttributeAllocRatioTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "attributeallocratio.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "attributeallocratio.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        AttributeAllocRatioTableData data = builder.build();

        Map<Integer, AttributeAllocRatioTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<AttributeAllocRatioTable>> idxDimensionId = new HashMap<>();
        Map<Integer, List<AttributeAllocRatioTable>> idxClassId = new HashMap<>();

        for (AttributeAllocRatioTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxDimensionId.computeIfAbsent(row.getDimensionId(), k -> new ArrayList<>()).add(row);
            idxClassId.computeIfAbsent(row.getClassId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxDimensionId, idxClassId);
    }

    public AttributeAllocRatioTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public AttributeAllocRatioTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, AttributeAllocRatioTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<AttributeAllocRatioTable> getByDimensionId(int key) {
        return snapshot.idxDimensionId.getOrDefault(key, Collections.emptyList());
    }

    public List<AttributeAllocRatioTable> getByClassId(int key) {
        return snapshot.idxClassId.getOrDefault(key, Collections.emptyList());
    }



    // FK: dimension_id → AttributeDimension.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByDimensionIdIndex(int key) {
        return snapshot.idxDimensionId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByClassIdIndex(int key) {
        return snapshot.idxClassId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<AttributeAllocRatioTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<AttributeAllocRatioTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            AttributeAllocRatioTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public AttributeAllocRatioTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<AttributeAllocRatioTable> where(Predicate<AttributeAllocRatioTable> pred) {
        Snapshot snap = this.snapshot;
        List<AttributeAllocRatioTable> result = new ArrayList<>();
        for (AttributeAllocRatioTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public AttributeAllocRatioTable first(Predicate<AttributeAllocRatioTable> pred) {
        Snapshot snap = this.snapshot;
        for (AttributeAllocRatioTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}