
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
 * Auto-generated config manager for RewardedAdSlot.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class RewardedAdSlotTableManager {

    private static final RewardedAdSlotTableManager INSTANCE = new RewardedAdSlotTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final RewardedAdSlotTableData data;
        final Map<Integer, RewardedAdSlotTable> kvData;





        Snapshot(RewardedAdSlotTableData data,
                 Map<Integer, RewardedAdSlotTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            RewardedAdSlotTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static RewardedAdSlotTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        RewardedAdSlotTableData.Builder builder = RewardedAdSlotTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "rewardedadslot.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "rewardedadslot.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        RewardedAdSlotTableData data = builder.build();

        Map<Integer, RewardedAdSlotTable> kvData = new HashMap<>(data.getDataCount());

        for (RewardedAdSlotTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public RewardedAdSlotTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public RewardedAdSlotTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, RewardedAdSlotTable> getKvData() {
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

    public List<RewardedAdSlotTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<RewardedAdSlotTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            RewardedAdSlotTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public RewardedAdSlotTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<RewardedAdSlotTable> where(Predicate<RewardedAdSlotTable> pred) {
        Snapshot snap = this.snapshot;
        List<RewardedAdSlotTable> result = new ArrayList<>();
        for (RewardedAdSlotTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public RewardedAdSlotTable first(Predicate<RewardedAdSlotTable> pred) {
        Snapshot snap = this.snapshot;
        for (RewardedAdSlotTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}