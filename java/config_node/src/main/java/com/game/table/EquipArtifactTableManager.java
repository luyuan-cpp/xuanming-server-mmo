
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
 * Auto-generated config manager for EquipArtifact.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class EquipArtifactTableManager {

    private static final EquipArtifactTableManager INSTANCE = new EquipArtifactTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final EquipArtifactTableData data;
        final Map<Integer, EquipArtifactTable> kvData;





        Snapshot(EquipArtifactTableData data,
                 Map<Integer, EquipArtifactTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            EquipArtifactTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static EquipArtifactTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        EquipArtifactTableData.Builder builder = EquipArtifactTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "equipartifact.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "equipartifact.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        EquipArtifactTableData data = builder.build();

        Map<Integer, EquipArtifactTable> kvData = new HashMap<>(data.getDataCount());

        for (EquipArtifactTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public EquipArtifactTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public EquipArtifactTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, EquipArtifactTable> getKvData() {
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

    public List<EquipArtifactTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<EquipArtifactTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            EquipArtifactTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public EquipArtifactTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<EquipArtifactTable> where(Predicate<EquipArtifactTable> pred) {
        Snapshot snap = this.snapshot;
        List<EquipArtifactTable> result = new ArrayList<>();
        for (EquipArtifactTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public EquipArtifactTable first(Predicate<EquipArtifactTable> pred) {
        Snapshot snap = this.snapshot;
        for (EquipArtifactTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}