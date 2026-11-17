
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
 * Auto-generated config manager for EquipV1Affix.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class EquipV1AffixTableManager {

    private static final EquipV1AffixTableManager INSTANCE = new EquipV1AffixTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final EquipV1AffixTableData data;
        final Map<Integer, EquipV1AffixTable> kvData;




        final Map<Integer, List<EquipV1AffixTable>> idxPool;

        final Map<Integer, List<EquipV1AffixTable>> idxMaterialId;


        Snapshot(EquipV1AffixTableData data,
                 Map<Integer, EquipV1AffixTable> kvData,
                 Map<Integer, List<EquipV1AffixTable>> idxPool,
                 Map<Integer, List<EquipV1AffixTable>> idxMaterialId) {
            this.data = data;
            this.kvData = kvData;
            this.idxPool = idxPool;
            this.idxMaterialId = idxMaterialId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            EquipV1AffixTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static EquipV1AffixTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        EquipV1AffixTableData.Builder builder = EquipV1AffixTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "equipv1affix.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "equipv1affix.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        EquipV1AffixTableData data = builder.build();

        Map<Integer, EquipV1AffixTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<EquipV1AffixTable>> idxPool = new HashMap<>();
        Map<Integer, List<EquipV1AffixTable>> idxMaterialId = new HashMap<>();

        for (EquipV1AffixTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxPool.computeIfAbsent(row.getPool(), k -> new ArrayList<>()).add(row);
            idxMaterialId.computeIfAbsent(row.getMaterialId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxPool, idxMaterialId);
    }

    public EquipV1AffixTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public EquipV1AffixTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, EquipV1AffixTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<EquipV1AffixTable> getByPool(int key) {
        return snapshot.idxPool.getOrDefault(key, Collections.emptyList());
    }

    public List<EquipV1AffixTable> getByMaterialId(int key) {
        return snapshot.idxMaterialId.getOrDefault(key, Collections.emptyList());
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




    public int countByPoolIndex(int key) {
        return snapshot.idxPool.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByMaterialIdIndex(int key) {
        return snapshot.idxMaterialId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<EquipV1AffixTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<EquipV1AffixTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            EquipV1AffixTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public EquipV1AffixTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<EquipV1AffixTable> where(Predicate<EquipV1AffixTable> pred) {
        Snapshot snap = this.snapshot;
        List<EquipV1AffixTable> result = new ArrayList<>();
        for (EquipV1AffixTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public EquipV1AffixTable first(Predicate<EquipV1AffixTable> pred) {
        Snapshot snap = this.snapshot;
        for (EquipV1AffixTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}