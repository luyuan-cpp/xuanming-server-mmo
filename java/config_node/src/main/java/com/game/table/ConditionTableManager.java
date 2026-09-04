
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
 * Auto-generated config manager for Condition.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class ConditionTableManager {

    private static final ConditionTableManager INSTANCE = new ConditionTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final ConditionTableData data;
        final Map<Integer, ConditionTable> kvData;



        final Map<Integer, List<ConditionTable>> idxCondition1;

        final Map<Integer, List<ConditionTable>> idxCondition2;

        final Map<Integer, List<ConditionTable>> idxCondition3;

        final Map<Integer, List<ConditionTable>> idxCondition4;



        Snapshot(ConditionTableData data,
                 Map<Integer, ConditionTable> kvData,
                 Map<Integer, List<ConditionTable>> idxCondition1,
                 Map<Integer, List<ConditionTable>> idxCondition2,
                 Map<Integer, List<ConditionTable>> idxCondition3,
                 Map<Integer, List<ConditionTable>> idxCondition4) {
            this.data = data;
            this.kvData = kvData;
            this.idxCondition1 = idxCondition1;
            this.idxCondition2 = idxCondition2;
            this.idxCondition3 = idxCondition3;
            this.idxCondition4 = idxCondition4;
        }
    }

    private Snapshot snapshot = new Snapshot(
            ConditionTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static ConditionTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        ConditionTableData.Builder builder = ConditionTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "condition.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "condition.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        ConditionTableData data = builder.build();

        Map<Integer, ConditionTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<ConditionTable>> idxCondition1 = new HashMap<>();
        Map<Integer, List<ConditionTable>> idxCondition2 = new HashMap<>();
        Map<Integer, List<ConditionTable>> idxCondition3 = new HashMap<>();
        Map<Integer, List<ConditionTable>> idxCondition4 = new HashMap<>();

        for (ConditionTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getCondition1List()) {
                idxCondition1.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getCondition2List()) {
                idxCondition2.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getCondition3List()) {
                idxCondition3.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getCondition4List()) {
                idxCondition4.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
        }

        this.snapshot = new Snapshot(data, kvData, idxCondition1, idxCondition2, idxCondition3, idxCondition4);
    }

    public ConditionTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public ConditionTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, ConditionTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<ConditionTable> findByCondition1Index(int key) {
        return snapshot.idxCondition1.getOrDefault(key, Collections.emptyList());
    }

    public List<ConditionTable> findByCondition2Index(int key) {
        return snapshot.idxCondition2.getOrDefault(key, Collections.emptyList());
    }

    public List<ConditionTable> findByCondition3Index(int key) {
        return snapshot.idxCondition3.getOrDefault(key, Collections.emptyList());
    }

    public List<ConditionTable> findByCondition4Index(int key) {
        return snapshot.idxCondition4.getOrDefault(key, Collections.emptyList());
    }






    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countByCondition1Index(int key) {
        return snapshot.idxCondition1.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByCondition2Index(int key) {
        return snapshot.idxCondition2.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByCondition3Index(int key) {
        return snapshot.idxCondition3.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByCondition4Index(int key) {
        return snapshot.idxCondition4.getOrDefault(key, Collections.emptyList()).size();
    }



    // ---- FindByIds (IN) ----

    public List<ConditionTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<ConditionTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            ConditionTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public ConditionTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<ConditionTable> where(Predicate<ConditionTable> pred) {
        Snapshot snap = this.snapshot;
        List<ConditionTable> result = new ArrayList<>();
        for (ConditionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public ConditionTable first(Predicate<ConditionTable> pred) {
        Snapshot snap = this.snapshot;
        for (ConditionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}