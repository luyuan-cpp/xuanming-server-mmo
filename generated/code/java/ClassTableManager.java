
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
 * Auto-generated config manager for Class.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class ClassTableManager {

    private static final ClassTableManager INSTANCE = new ClassTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final ClassTableData data;
        final Map<Integer, ClassTable> kvData;



        final Map<Integer, List<ClassTable>> idxSkill;



        Snapshot(ClassTableData data,
                 Map<Integer, ClassTable> kvData,
                 Map<Integer, List<ClassTable>> idxSkill) {
            this.data = data;
            this.kvData = kvData;
            this.idxSkill = idxSkill;
        }
    }

    private Snapshot snapshot = new Snapshot(
            ClassTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static ClassTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        ClassTableData.Builder builder = ClassTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "class.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "class.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        ClassTableData data = builder.build();

        Map<Integer, ClassTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<ClassTable>> idxSkill = new HashMap<>();

        for (ClassTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getSkillList()) {
                idxSkill.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
        }

        this.snapshot = new Snapshot(data, kvData, idxSkill);
    }

    public ClassTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public ClassTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, ClassTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<ClassTable> findBySkillIndex(int key) {
        return snapshot.idxSkill.getOrDefault(key, Collections.emptyList());
    }






    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countBySkillIndex(int key) {
        return snapshot.idxSkill.getOrDefault(key, Collections.emptyList()).size();
    }



    // ---- FindByIds (IN) ----

    public List<ClassTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<ClassTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            ClassTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public ClassTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<ClassTable> where(Predicate<ClassTable> pred) {
        Snapshot snap = this.snapshot;
        List<ClassTable> result = new ArrayList<>();
        for (ClassTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public ClassTable first(Predicate<ClassTable> pred) {
        Snapshot snap = this.snapshot;
        for (ClassTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}