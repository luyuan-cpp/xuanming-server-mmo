
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
 * Auto-generated config manager for SkillVfxCosmetic.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class SkillVfxCosmeticTableManager {

    private static final SkillVfxCosmeticTableManager INSTANCE = new SkillVfxCosmeticTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final SkillVfxCosmeticTableData data;
        final Map<Integer, SkillVfxCosmeticTable> kvData;




        final Map<Integer, List<SkillVfxCosmeticTable>> idxSkillTableId;


        Snapshot(SkillVfxCosmeticTableData data,
                 Map<Integer, SkillVfxCosmeticTable> kvData,
                 Map<Integer, List<SkillVfxCosmeticTable>> idxSkillTableId) {
            this.data = data;
            this.kvData = kvData;
            this.idxSkillTableId = idxSkillTableId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            SkillVfxCosmeticTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static SkillVfxCosmeticTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        SkillVfxCosmeticTableData.Builder builder = SkillVfxCosmeticTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "skillvfxcosmetic.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "skillvfxcosmetic.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        SkillVfxCosmeticTableData data = builder.build();

        Map<Integer, SkillVfxCosmeticTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<SkillVfxCosmeticTable>> idxSkillTableId = new HashMap<>();

        for (SkillVfxCosmeticTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxSkillTableId.computeIfAbsent(row.getSkillTableId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxSkillTableId);
    }

    public SkillVfxCosmeticTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public SkillVfxCosmeticTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, SkillVfxCosmeticTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<SkillVfxCosmeticTable> getBySkillTableId(int key) {
        return snapshot.idxSkillTableId.getOrDefault(key, Collections.emptyList());
    }



    // FK: skill_table_id → Skill.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countBySkillTableIdIndex(int key) {
        return snapshot.idxSkillTableId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<SkillVfxCosmeticTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<SkillVfxCosmeticTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            SkillVfxCosmeticTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public SkillVfxCosmeticTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<SkillVfxCosmeticTable> where(Predicate<SkillVfxCosmeticTable> pred) {
        Snapshot snap = this.snapshot;
        List<SkillVfxCosmeticTable> result = new ArrayList<>();
        for (SkillVfxCosmeticTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public SkillVfxCosmeticTable first(Predicate<SkillVfxCosmeticTable> pred) {
        Snapshot snap = this.snapshot;
        for (SkillVfxCosmeticTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}