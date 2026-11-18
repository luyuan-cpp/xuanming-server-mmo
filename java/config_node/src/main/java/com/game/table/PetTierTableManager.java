
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
 * Auto-generated config manager for PetTier.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class PetTierTableManager {

    private static final PetTierTableManager INSTANCE = new PetTierTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final PetTierTableData data;
        final Map<Integer, PetTierTable> kvData;





        Snapshot(PetTierTableData data,
                 Map<Integer, PetTierTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            PetTierTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static PetTierTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        PetTierTableData.Builder builder = PetTierTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "pettier.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "pettier.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        PetTierTableData data = builder.build();

        Map<Integer, PetTierTable> kvData = new HashMap<>(data.getDataCount());

        for (PetTierTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public PetTierTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public PetTierTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, PetTierTable> getKvData() {
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

    public List<PetTierTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<PetTierTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            PetTierTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public PetTierTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<PetTierTable> where(Predicate<PetTierTable> pred) {
        Snapshot snap = this.snapshot;
        List<PetTierTable> result = new ArrayList<>();
        for (PetTierTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public PetTierTable first(Predicate<PetTierTable> pred) {
        Snapshot snap = this.snapshot;
        for (PetTierTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}