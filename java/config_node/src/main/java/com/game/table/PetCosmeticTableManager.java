
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
 * Auto-generated config manager for PetCosmetic.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class PetCosmeticTableManager {

    private static final PetCosmeticTableManager INSTANCE = new PetCosmeticTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final PetCosmeticTableData data;
        final Map<Integer, PetCosmeticTable> kvData;





        Snapshot(PetCosmeticTableData data,
                 Map<Integer, PetCosmeticTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            PetCosmeticTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static PetCosmeticTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        PetCosmeticTableData.Builder builder = PetCosmeticTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "petcosmetic.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "petcosmetic.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        PetCosmeticTableData data = builder.build();

        Map<Integer, PetCosmeticTable> kvData = new HashMap<>(data.getDataCount());

        for (PetCosmeticTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public PetCosmeticTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public PetCosmeticTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, PetCosmeticTable> getKvData() {
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

    public List<PetCosmeticTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<PetCosmeticTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            PetCosmeticTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public PetCosmeticTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<PetCosmeticTable> where(Predicate<PetCosmeticTable> pred) {
        Snapshot snap = this.snapshot;
        List<PetCosmeticTable> result = new ArrayList<>();
        for (PetCosmeticTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public PetCosmeticTable first(Predicate<PetCosmeticTable> pred) {
        Snapshot snap = this.snapshot;
        for (PetCosmeticTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}