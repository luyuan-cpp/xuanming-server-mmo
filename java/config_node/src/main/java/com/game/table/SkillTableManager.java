
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
 * Auto-generated config manager for Skill.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class SkillTableManager {

    private static final SkillTableManager INSTANCE = new SkillTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final SkillTableData data;
        final Map<Integer, SkillTable> kvData;



        final Map<Integer, List<SkillTable>> idxSkillType;

        final Map<Integer, List<SkillTable>> idxTargetingMode;

        final Map<Integer, List<SkillTable>> idxEffect;



        Snapshot(SkillTableData data,
                 Map<Integer, SkillTable> kvData,
                 Map<Integer, List<SkillTable>> idxSkillType,
                 Map<Integer, List<SkillTable>> idxTargetingMode,
                 Map<Integer, List<SkillTable>> idxEffect) {
            this.data = data;
            this.kvData = kvData;
            this.idxSkillType = idxSkillType;
            this.idxTargetingMode = idxTargetingMode;
            this.idxEffect = idxEffect;
        }
    }

    private Snapshot snapshot = new Snapshot(
            SkillTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static SkillTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        SkillTableData.Builder builder = SkillTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "skill.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "skill.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        SkillTableData data = builder.build();

        Map<Integer, SkillTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<SkillTable>> idxSkillType = new HashMap<>();
        Map<Integer, List<SkillTable>> idxTargetingMode = new HashMap<>();
        Map<Integer, List<SkillTable>> idxEffect = new HashMap<>();

        for (SkillTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            for (Integer elem : row.getSkillTypeList()) {
                idxSkillType.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getTargetingModeList()) {
                idxTargetingMode.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
            for (Integer elem : row.getEffectList()) {
                idxEffect.computeIfAbsent(elem, k -> new ArrayList<>()).add(row);
            }
        }

        this.snapshot = new Snapshot(data, kvData, idxSkillType, idxTargetingMode, idxEffect);
    }

    public SkillTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public SkillTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, SkillTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }





    public List<SkillTable> findBySkillTypeIndex(int key) {
        return snapshot.idxSkillType.getOrDefault(key, Collections.emptyList());
    }

    public List<SkillTable> findByTargetingModeIndex(int key) {
        return snapshot.idxTargetingMode.getOrDefault(key, Collections.emptyList());
    }

    public List<SkillTable> findByEffectIndex(int key) {
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



    public int countBySkillTypeIndex(int key) {
        return snapshot.idxSkillType.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByTargetingModeIndex(int key) {
        return snapshot.idxTargetingMode.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByEffectIndex(int key) {
        return snapshot.idxEffect.getOrDefault(key, Collections.emptyList()).size();
    }



    // ---- FindByIds (IN) ----

    public List<SkillTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<SkillTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            SkillTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public SkillTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<SkillTable> where(Predicate<SkillTable> pred) {
        Snapshot snap = this.snapshot;
        List<SkillTable> result = new ArrayList<>();
        for (SkillTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public SkillTable first(Predicate<SkillTable> pred) {
        Snapshot snap = this.snapshot;
        for (SkillTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}