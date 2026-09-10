
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
 * Auto-generated config manager for PetRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class PetRuleTableManager {

    private static final PetRuleTableManager INSTANCE = new PetRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final PetRuleTableData data;
        final Map<Integer, PetRuleTable> kvData;





        Snapshot(PetRuleTableData data,
                 Map<Integer, PetRuleTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            PetRuleTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static PetRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        PetRuleTableData.Builder builder = PetRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "petrule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "petrule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        PetRuleTableData data = builder.build();

        Map<Integer, PetRuleTable> kvData = new HashMap<>(data.getDataCount());

        for (PetRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public PetRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public PetRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, PetRuleTable> getKvData() {
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

    public List<PetRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<PetRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            PetRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public PetRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<PetRuleTable> where(Predicate<PetRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<PetRuleTable> result = new ArrayList<>();
        for (PetRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public PetRuleTable first(Predicate<PetRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (PetRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}