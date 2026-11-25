
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
 * Auto-generated config manager for EquipAffixQuality.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class EquipAffixQualityTableManager {

    private static final EquipAffixQualityTableManager INSTANCE = new EquipAffixQualityTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final EquipAffixQualityTableData data;
        final Map<Integer, EquipAffixQualityTable> kvData;





        Snapshot(EquipAffixQualityTableData data,
                 Map<Integer, EquipAffixQualityTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            EquipAffixQualityTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static EquipAffixQualityTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        EquipAffixQualityTableData.Builder builder = EquipAffixQualityTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "equipaffixquality.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "equipaffixquality.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        EquipAffixQualityTableData data = builder.build();

        Map<Integer, EquipAffixQualityTable> kvData = new HashMap<>(data.getDataCount());

        for (EquipAffixQualityTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public EquipAffixQualityTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public EquipAffixQualityTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, EquipAffixQualityTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }










    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }





    // ---- FindByIds (IN) ----

    public List<EquipAffixQualityTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<EquipAffixQualityTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            EquipAffixQualityTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public EquipAffixQualityTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<EquipAffixQualityTable> where(Predicate<EquipAffixQualityTable> pred) {
        Snapshot snap = this.snapshot;
        List<EquipAffixQualityTable> result = new ArrayList<>();
        for (EquipAffixQualityTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public EquipAffixQualityTable first(Predicate<EquipAffixQualityTable> pred) {
        Snapshot snap = this.snapshot;
        for (EquipAffixQualityTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}