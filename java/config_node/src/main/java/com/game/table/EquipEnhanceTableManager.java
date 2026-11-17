
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
 * Auto-generated config manager for EquipEnhance.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class EquipEnhanceTableManager {

    private static final EquipEnhanceTableManager INSTANCE = new EquipEnhanceTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final EquipEnhanceTableData data;
        final Map<Integer, EquipEnhanceTable> kvData;




        final Map<Integer, List<EquipEnhanceTable>> idxMaterialId;

        final Map<Integer, List<EquipEnhanceTable>> idxProfileId;


        Snapshot(EquipEnhanceTableData data,
                 Map<Integer, EquipEnhanceTable> kvData,
                 Map<Integer, List<EquipEnhanceTable>> idxMaterialId,
                 Map<Integer, List<EquipEnhanceTable>> idxProfileId) {
            this.data = data;
            this.kvData = kvData;
            this.idxMaterialId = idxMaterialId;
            this.idxProfileId = idxProfileId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            EquipEnhanceTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static EquipEnhanceTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        EquipEnhanceTableData.Builder builder = EquipEnhanceTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "equipenhance.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "equipenhance.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        EquipEnhanceTableData data = builder.build();

        Map<Integer, EquipEnhanceTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<EquipEnhanceTable>> idxMaterialId = new HashMap<>();
        Map<Integer, List<EquipEnhanceTable>> idxProfileId = new HashMap<>();

        for (EquipEnhanceTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxMaterialId.computeIfAbsent(row.getMaterialId(), k -> new ArrayList<>()).add(row);
            idxProfileId.computeIfAbsent(row.getProfileId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxMaterialId, idxProfileId);
    }

    public EquipEnhanceTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public EquipEnhanceTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, EquipEnhanceTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<EquipEnhanceTable> getByMaterialId(int key) {
        return snapshot.idxMaterialId.getOrDefault(key, Collections.emptyList());
    }

    public List<EquipEnhanceTable> getByProfileId(int key) {
        return snapshot.idxProfileId.getOrDefault(key, Collections.emptyList());
    }



    // FK: material_id → Item.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByMaterialIdIndex(int key) {
        return snapshot.idxMaterialId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByProfileIdIndex(int key) {
        return snapshot.idxProfileId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<EquipEnhanceTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<EquipEnhanceTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            EquipEnhanceTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public EquipEnhanceTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<EquipEnhanceTable> where(Predicate<EquipEnhanceTable> pred) {
        Snapshot snap = this.snapshot;
        List<EquipEnhanceTable> result = new ArrayList<>();
        for (EquipEnhanceTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public EquipEnhanceTable first(Predicate<EquipEnhanceTable> pred) {
        Snapshot snap = this.snapshot;
        for (EquipEnhanceTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}