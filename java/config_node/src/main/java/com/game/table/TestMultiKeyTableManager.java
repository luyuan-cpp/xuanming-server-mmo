
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
 * Auto-generated config manager for TestMultiKey.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TestMultiKeyTableManager {

    private static final TestMultiKeyTableManager INSTANCE = new TestMultiKeyTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final TestMultiKeyTableData data;
        final Map<Integer, List<TestMultiKeyTable>> kvData;

        final Map<String, TestMultiKeyTable> kvStringKeyData;

        final Map<Integer, TestMultiKeyTable> kvUint32KeyData;

        final Map<Integer, TestMultiKeyTable> kvInt32KeyData;


        final Map<String, List<TestMultiKeyTable>> kvMStringKeyData;

        final Map<Integer, List<TestMultiKeyTable>> kvMUint32KeyData;

        final Map<Integer, List<TestMultiKeyTable>> kvMInt32KeyData;


        final Map<Integer, List<TestMultiKeyTable>> idxEffect;

        final Map<Integer, List<TestMultiKeyTable>> idxTestRefs;


        final Map<Integer, List<TestMultiKeyTable>> idxLevel;

        final Map<Integer, List<TestMultiKeyTable>> idxTestRef;


        Snapshot(TestMultiKeyTableData data,
                 Map<Integer, List<TestMultiKeyTable>> kvData,
                 Map<String, TestMultiKeyTable> kvStringKeyData,
                 Map<Integer, TestMultiKeyTable> kvUint32KeyData,
                 Map<Integer, TestMultiKeyTable> kvInt32KeyData,
                 Map<String, List<TestMultiKeyTable>> kvMStringKeyData,
                 Map<Integer, List<TestMultiKeyTable>> kvMUint32KeyData,
                 Map<Integer, List<TestMultiKeyTable>> kvMInt32KeyData,
                 Map<Integer, List<TestMultiKeyTable>> idxEffect,
                 Map<Integer, List<TestMultiKeyTable>> idxTestRefs,
                 Map<Integer, List<TestMultiKeyTable>> idxLevel,
                 Map<Integer, List<TestMultiKeyTable>> idxTestRef) {
            this.data = data;
            this.kvData = kvData;
            this.kvStringKeyData = kvStringKeyData;
            this.kvUint32KeyData = kvUint32KeyData;
            this.kvInt32KeyData = kvInt32KeyData;
            this.kvMStringKeyData = kvMStringKeyData;
            this.kvMUint32KeyData = kvMUint32KeyData;
            this.kvMInt32KeyData = kvMInt32KeyData;
            this.idxEffect = idxEffect;
            this.idxTestRefs = idxTestRefs;
            this.idxLevel = idxLevel;
            this.idxTestRef = idxTestRef;
        }
    }

    private Snapshot snapshot = new Snapshot(
            TestMultiKeyTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static TestMultiKeyTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        TestMultiKeyTableData.Builder builder = TestMultiKeyTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "testmultikey.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "testmultikey.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        TestMultiKeyTableData data = builder.build();

        Map<Integer, List<TestMultiKeyTable>> kvData = new HashMap<>(data.getDataCount());
        Map<String, TestMultiKeyTable> kvStringKeyData = new HashMap<>(data.getDataCount());
        Map<Integer, TestMultiKeyTable> kvUint32KeyData = new HashMap<>(data.getDataCount());
        Map<Integer, TestMultiKeyTable> kvInt32KeyData = new HashMap<>(data.getDataCount());
        Map<String, List<TestMultiKeyTable>> kvMStringKeyData = new HashMap<>();
        Map<Integer, List<TestMultiKeyTable>> kvMUint32KeyData = new HashMap<>();
        Map<Integer, List<TestMultiKeyTable>> kvMInt32KeyData = new HashMap<>();
        Map<Integer, List<TestMultiKeyTable>> idxEffect = new HashMap<>();
        Map<Integer, List<TestMultiKeyTable>> idxTestRefs = new HashMap<>();
        Map<Integer, List<TestMultiKeyTable>> idxLevel = new HashMap<>();
        Map<Integer, List<TestMultiKeyTable>> idxTestRef = new HashMap<>();

        for (TestMultiKeyTable row : data.getDataList()) {
            kvData.computeIfAbsent(row.getId(), k -> new ArrayList<>()).add(row);
            kvStringKeyData.put(row.getStringKey(), row);
            kvUint32KeyData.put(row.getUint32Key(), row);
            kvInt32KeyData.put(row.getInt32Key(), row);
            kvMStringKeyData.computeIfAbsent(row.getMStringKey(), k -> new ArrayList<>()).add(row);
            kvMUint32KeyData.computeIfAbsent(row.getMUint32Key(), k -> new ArrayList<>()).add(row);
            kvMInt32KeyData.computeIfAbsent(row.getMInt32Key(), k -> new ArrayList<>()).add(row);
            for (Integer elem : row.getEffectList()) {
                idxEffect.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getTestRefsList()) {
                idxTestRefs.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            idxLevel.computeIfAbsent(row.getLevel(), k -> new ArrayList<>()).add(row);
            idxTestRef.computeIfAbsent(row.getTestRef(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, kvStringKeyData, kvUint32KeyData, kvInt32KeyData, kvMStringKeyData, kvMUint32KeyData, kvMInt32KeyData, idxEffect, idxTestRefs, idxLevel, idxTestRef);
    }

    public TestMultiKeyTableData findAll() {
        return snapshot.data;
    }

    /**
     * 返回该 id 命中的全部行。这张表的主键声明了 (cfg_multi)，单值语义不成立，
     * 因此不提供 findById —— 把可重复主键当单值用会在编译期断，而不是悄悄只看第一行。
     *
     * <p>热更契约：返回的对象属于当前快照，只许当场用，调用方**只存 id**。
     */
    public List<TestMultiKeyTable> findAllById(int id) {
        return snapshot.kvData.getOrDefault(id, Collections.emptyList());
    }

    public Map<Integer, List<TestMultiKeyTable>> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }


    public TestMultiKeyTable findByStringKey(String key) {
        return snapshot.kvStringKeyData.get(key);
    }

    public TestMultiKeyTable findByUint32Key(int key) {
        return snapshot.kvUint32KeyData.get(key);
    }

    public TestMultiKeyTable findByInt32Key(int key) {
        return snapshot.kvInt32KeyData.get(key);
    }


    public List<TestMultiKeyTable> findByMStringKey(String key) {
        return snapshot.kvMStringKeyData.getOrDefault(key, Collections.emptyList());
    }

    public List<TestMultiKeyTable> findByMUint32Key(int key) {
        return snapshot.kvMUint32KeyData.getOrDefault(key, Collections.emptyList());
    }

    public List<TestMultiKeyTable> findByMInt32Key(int key) {
        return snapshot.kvMInt32KeyData.getOrDefault(key, Collections.emptyList());
    }



    public List<TestMultiKeyTable> findByEffectIndex(int key) {
        return snapshot.idxEffect.getOrDefault(key, Collections.emptyList());
    }

    public List<TestMultiKeyTable> findByTestRefsIndex(int key) {
        return snapshot.idxTestRefs.getOrDefault(key, Collections.emptyList());
    }



    public List<TestMultiKeyTable> getByLevel(int key) {
        return snapshot.idxLevel.getOrDefault(key, Collections.emptyList());
    }

    public List<TestMultiKeyTable> getByTestRef(int key) {
        return snapshot.idxTestRef.getOrDefault(key, Collections.emptyList());
    }



    // FK: test_ref → Test.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }


    public boolean existsByStringKey(String key) {
        return snapshot.kvStringKeyData.containsKey(key);
    }

    public boolean existsByUint32Key(int key) {
        return snapshot.kvUint32KeyData.containsKey(key);
    }

    public boolean existsByInt32Key(int key) {
        return snapshot.kvInt32KeyData.containsKey(key);
    }


    // ---- Count ----

    public int count() {
        return snapshot.data.getDataCount();
    }


    public int countByMStringKey(String key) {
        return snapshot.kvMStringKeyData.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByMUint32Key(int key) {
        return snapshot.kvMUint32KeyData.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByMInt32Key(int key) {
        return snapshot.kvMInt32KeyData.getOrDefault(key, Collections.emptyList()).size();
    }


    public int countByEffectIndex(int key) {
        return snapshot.idxEffect.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByTestRefsIndex(int key) {
        return snapshot.idxTestRefs.getOrDefault(key, Collections.emptyList()).size();
    }


    public int countByLevelIndex(int key) {
        return snapshot.idxLevel.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByTestRefIndex(int key) {
        return snapshot.idxTestRef.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<TestMultiKeyTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<TestMultiKeyTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            result.addAll(snap.kvData.getOrDefault(id, Collections.emptyList()));
        }
        return result;
    }

    // ---- RandOne ----

    public TestMultiKeyTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<TestMultiKeyTable> where(Predicate<TestMultiKeyTable> pred) {
        Snapshot snap = this.snapshot;
        List<TestMultiKeyTable> result = new ArrayList<>();
        for (TestMultiKeyTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public TestMultiKeyTable first(Predicate<TestMultiKeyTable> pred) {
        Snapshot snap = this.snapshot;
        for (TestMultiKeyTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}