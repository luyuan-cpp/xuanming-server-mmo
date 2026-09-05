
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
 * Auto-generated config manager for AttributeAutoPlan.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class AttributeAutoPlanTableManager {

    private static final AttributeAutoPlanTableManager INSTANCE = new AttributeAutoPlanTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final AttributeAutoPlanTableData data;
        final Map<Integer, AttributeAutoPlanTable> kvData;



        final Map<Integer, List<AttributeAutoPlanTable>> idxDimension;

        final Map<Integer, List<AttributeAutoPlanTable>> idxWeight;


        final Map<Integer, List<AttributeAutoPlanTable>> idxClassId;

        final Map<Integer, List<AttributeAutoPlanTable>> idxPoolId;


        Snapshot(AttributeAutoPlanTableData data,
                 Map<Integer, AttributeAutoPlanTable> kvData,
                 Map<Integer, List<AttributeAutoPlanTable>> idxDimension,
                 Map<Integer, List<AttributeAutoPlanTable>> idxWeight,
                 Map<Integer, List<AttributeAutoPlanTable>> idxClassId,
                 Map<Integer, List<AttributeAutoPlanTable>> idxPoolId) {
            this.data = data;
            this.kvData = kvData;
            this.idxDimension = idxDimension;
            this.idxWeight = idxWeight;
            this.idxClassId = idxClassId;
            this.idxPoolId = idxPoolId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            AttributeAutoPlanTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static AttributeAutoPlanTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        AttributeAutoPlanTableData.Builder builder = AttributeAutoPlanTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "attributeautoplan.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "attributeautoplan.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        AttributeAutoPlanTableData data = builder.build();

        Map<Integer, AttributeAutoPlanTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<AttributeAutoPlanTable>> idxDimension = new HashMap<>();
        Map<Integer, List<AttributeAutoPlanTable>> idxWeight = new HashMap<>();
        Map<Integer, List<AttributeAutoPlanTable>> idxClassId = new HashMap<>();
        Map<Integer, List<AttributeAutoPlanTable>> idxPoolId = new HashMap<>();

        for (AttributeAutoPlanTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getDimensionList()) {
                idxDimension.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getWeightList()) {
                idxWeight.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            idxClassId.computeIfAbsent(row.getClassId(), k -> new ArrayList<>()).add(row);
            idxPoolId.computeIfAbsent(row.getPoolId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxDimension, idxWeight, idxClassId, idxPoolId);
    }

    public AttributeAutoPlanTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public AttributeAutoPlanTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, AttributeAutoPlanTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<AttributeAutoPlanTable> findByDimensionIndex(int key) {
        return snapshot.idxDimension.getOrDefault(key, Collections.emptyList());
    }

    public List<AttributeAutoPlanTable> findByWeightIndex(int key) {
        return snapshot.idxWeight.getOrDefault(key, Collections.emptyList());
    }



    public List<AttributeAutoPlanTable> getByClassId(int key) {
        return snapshot.idxClassId.getOrDefault(key, Collections.emptyList());
    }

    public List<AttributeAutoPlanTable> getByPoolId(int key) {
        return snapshot.idxPoolId.getOrDefault(key, Collections.emptyList());
    }



    // FK: pool_id → AttributePool.id

    // FK: dimension → AttributeDimension.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countByDimensionIndex(int key) {
        return snapshot.idxDimension.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByWeightIndex(int key) {
        return snapshot.idxWeight.getOrDefault(key, Collections.emptyList()).size();
    }


    public int countByClassIdIndex(int key) {
        return snapshot.idxClassId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByPoolIdIndex(int key) {
        return snapshot.idxPoolId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<AttributeAutoPlanTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<AttributeAutoPlanTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            AttributeAutoPlanTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public AttributeAutoPlanTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<AttributeAutoPlanTable> where(Predicate<AttributeAutoPlanTable> pred) {
        Snapshot snap = this.snapshot;
        List<AttributeAutoPlanTable> result = new ArrayList<>();
        for (AttributeAutoPlanTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public AttributeAutoPlanTable first(Predicate<AttributeAutoPlanTable> pred) {
        Snapshot snap = this.snapshot;
        for (AttributeAutoPlanTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}