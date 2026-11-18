
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
 * Auto-generated config manager for PetEntry.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class PetEntryTableManager {

    private static final PetEntryTableManager INSTANCE = new PetEntryTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final PetEntryTableData data;
        final Map<Integer, PetEntryTable> kvData;





        Snapshot(PetEntryTableData data,
                 Map<Integer, PetEntryTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            PetEntryTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static PetEntryTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        PetEntryTableData.Builder builder = PetEntryTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "petentry.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "petentry.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        PetEntryTableData data = builder.build();

        Map<Integer, PetEntryTable> kvData = new HashMap<>(data.getDataCount());

        for (PetEntryTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public PetEntryTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public PetEntryTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, PetEntryTable> getKvData() {
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

    public List<PetEntryTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<PetEntryTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            PetEntryTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public PetEntryTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<PetEntryTable> where(Predicate<PetEntryTable> pred) {
        Snapshot snap = this.snapshot;
        List<PetEntryTable> result = new ArrayList<>();
        for (PetEntryTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public PetEntryTable first(Predicate<PetEntryTable> pred) {
        Snapshot snap = this.snapshot;
        for (PetEntryTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}