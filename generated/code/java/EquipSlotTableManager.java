
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
 * Auto-generated config manager for EquipSlot.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class EquipSlotTableManager {

    private static final EquipSlotTableManager INSTANCE = new EquipSlotTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final EquipSlotTableData data;
        final Map<Integer, EquipSlotTable> kvData;





        Snapshot(EquipSlotTableData data,
                 Map<Integer, EquipSlotTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            EquipSlotTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static EquipSlotTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        EquipSlotTableData.Builder builder = EquipSlotTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "equipslot.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "equipslot.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        EquipSlotTableData data = builder.build();

        Map<Integer, EquipSlotTable> kvData = new HashMap<>(data.getDataCount());

        for (EquipSlotTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public EquipSlotTableData findAll() {
        return snapshot.data;
    }

    public EquipSlotTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, EquipSlotTable> getKvData() {
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

    public List<EquipSlotTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<EquipSlotTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            EquipSlotTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public EquipSlotTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<EquipSlotTable> where(Predicate<EquipSlotTable> pred) {
        Snapshot snap = this.snapshot;
        List<EquipSlotTable> result = new ArrayList<>();
        for (EquipSlotTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public EquipSlotTable first(Predicate<EquipSlotTable> pred) {
        Snapshot snap = this.snapshot;
        for (EquipSlotTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}