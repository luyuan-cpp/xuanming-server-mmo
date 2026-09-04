
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
 * Auto-generated config manager for SkillPermission.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class SkillPermissionTableManager {

    private static final SkillPermissionTableManager INSTANCE = new SkillPermissionTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final SkillPermissionTableData data;
        final Map<Integer, SkillPermissionTable> kvData;



        final Map<Integer, List<SkillPermissionTable>> idxSkillType;



        Snapshot(SkillPermissionTableData data,
                 Map<Integer, SkillPermissionTable> kvData,
                 Map<Integer, List<SkillPermissionTable>> idxSkillType) {
            this.data = data;
            this.kvData = kvData;
            this.idxSkillType = idxSkillType;
        }
    }

    private Snapshot snapshot = new Snapshot(
            SkillPermissionTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static SkillPermissionTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        SkillPermissionTableData.Builder builder = SkillPermissionTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "skillpermission.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "skillpermission.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        SkillPermissionTableData data = builder.build();

        Map<Integer, SkillPermissionTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<SkillPermissionTable>> idxSkillType = new HashMap<>();

        for (SkillPermissionTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getSkillTypeList()) {
                idxSkillType.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
        }

        this.snapshot = new Snapshot(data, kvData, idxSkillType);
    }

    public SkillPermissionTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public SkillPermissionTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, SkillPermissionTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<SkillPermissionTable> findBySkillTypeIndex(int key) {
        return snapshot.idxSkillType.getOrDefault(key, Collections.emptyList());
    }






    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }



    public int countBySkillTypeIndex(int key) {
        return snapshot.idxSkillType.getOrDefault(key, Collections.emptyList()).size();
    }



    // ---- FindByIds (IN) ----

    public List<SkillPermissionTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<SkillPermissionTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            SkillPermissionTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public SkillPermissionTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<SkillPermissionTable> where(Predicate<SkillPermissionTable> pred) {
        Snapshot snap = this.snapshot;
        List<SkillPermissionTable> result = new ArrayList<>();
        for (SkillPermissionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public SkillPermissionTable first(Predicate<SkillPermissionTable> pred) {
        Snapshot snap = this.snapshot;
        for (SkillPermissionTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}