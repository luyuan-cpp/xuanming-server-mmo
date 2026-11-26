
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
 * Auto-generated config manager for SpiritShopLimitGroup.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class SpiritShopLimitGroupTableManager {

    private static final SpiritShopLimitGroupTableManager INSTANCE = new SpiritShopLimitGroupTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final SpiritShopLimitGroupTableData data;
        final Map<Integer, SpiritShopLimitGroupTable> kvData;





        Snapshot(SpiritShopLimitGroupTableData data,
                 Map<Integer, SpiritShopLimitGroupTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            SpiritShopLimitGroupTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static SpiritShopLimitGroupTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        SpiritShopLimitGroupTableData.Builder builder = SpiritShopLimitGroupTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "spiritshoplimitgroup.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "spiritshoplimitgroup.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        SpiritShopLimitGroupTableData data = builder.build();

        Map<Integer, SpiritShopLimitGroupTable> kvData = new HashMap<>(data.getDataCount());

        for (SpiritShopLimitGroupTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public SpiritShopLimitGroupTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public SpiritShopLimitGroupTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, SpiritShopLimitGroupTable> getKvData() {
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

    public List<SpiritShopLimitGroupTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<SpiritShopLimitGroupTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            SpiritShopLimitGroupTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public SpiritShopLimitGroupTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<SpiritShopLimitGroupTable> where(Predicate<SpiritShopLimitGroupTable> pred) {
        Snapshot snap = this.snapshot;
        List<SpiritShopLimitGroupTable> result = new ArrayList<>();
        for (SpiritShopLimitGroupTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public SpiritShopLimitGroupTable first(Predicate<SpiritShopLimitGroupTable> pred) {
        Snapshot snap = this.snapshot;
        for (SpiritShopLimitGroupTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}