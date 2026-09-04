
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
 * Auto-generated config manager for Test.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TestTableManager {

    private static final TestTableManager INSTANCE = new TestTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final TestTableData data;
        final Map<Integer, TestTable> kvData;



        final Map<Integer, List<TestTable>> idxEffect;



        Snapshot(TestTableData data,
                 Map<Integer, TestTable> kvData,
                 Map<Integer, List<TestTable>> idxEffect) {
            this.data = data;
            this.kvData = kvData;
            this.idxEffect = idxEffect;
        }
    }

    private Snapshot snapshot = new Snapshot(
            TestTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static TestTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        TestTableData.Builder builder = TestTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "test.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "test.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        TestTableData data = builder.build();

        Map<Integer, TestTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<TestTable>> idxEffect = new HashMap<>();

        for (TestTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getEffectList()) {
                idxEffect.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
        }

        this.snapshot = new Snapshot(data, kvData, idxEffect);
    }

    public TestTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public TestTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, TestTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<TestTable> findByEffectIndex(int key) {
        return snapshot.idxEffect.getOrDefault(key, Collections.emptyList());
    }






    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countByEffectIndex(int key) {
        return snapshot.idxEffect.getOrDefault(key, Collections.emptyList()).size();
    }



    // ---- FindByIds (IN) ----

    public List<TestTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<TestTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            TestTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public TestTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<TestTable> where(Predicate<TestTable> pred) {
        Snapshot snap = this.snapshot;
        List<TestTable> result = new ArrayList<>();
        for (TestTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public TestTable first(Predicate<TestTable> pred) {
        Snapshot snap = this.snapshot;
        for (TestTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}