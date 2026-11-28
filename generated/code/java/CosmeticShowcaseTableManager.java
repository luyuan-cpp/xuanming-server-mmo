
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
 * Auto-generated config manager for CosmeticShowcase.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class CosmeticShowcaseTableManager {

    private static final CosmeticShowcaseTableManager INSTANCE = new CosmeticShowcaseTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final CosmeticShowcaseTableData data;
        final Map<Integer, CosmeticShowcaseTable> kvData;





        Snapshot(CosmeticShowcaseTableData data,
                 Map<Integer, CosmeticShowcaseTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            CosmeticShowcaseTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static CosmeticShowcaseTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        CosmeticShowcaseTableData.Builder builder = CosmeticShowcaseTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "cosmeticshowcase.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "cosmeticshowcase.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        CosmeticShowcaseTableData data = builder.build();

        Map<Integer, CosmeticShowcaseTable> kvData = new HashMap<>(data.getDataCount());

        for (CosmeticShowcaseTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public CosmeticShowcaseTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public CosmeticShowcaseTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, CosmeticShowcaseTable> getKvData() {
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

    public List<CosmeticShowcaseTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<CosmeticShowcaseTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            CosmeticShowcaseTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public CosmeticShowcaseTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<CosmeticShowcaseTable> where(Predicate<CosmeticShowcaseTable> pred) {
        Snapshot snap = this.snapshot;
        List<CosmeticShowcaseTable> result = new ArrayList<>();
        for (CosmeticShowcaseTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public CosmeticShowcaseTable first(Predicate<CosmeticShowcaseTable> pred) {
        Snapshot snap = this.snapshot;
        for (CosmeticShowcaseTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}