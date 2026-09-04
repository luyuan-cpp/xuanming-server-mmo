
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
 * Auto-generated config manager for Reward.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class RewardTableManager {

    private static final RewardTableManager INSTANCE = new RewardTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final RewardTableData data;
        final Map<Integer, RewardTable> kvData;





        Snapshot(RewardTableData data,
                 Map<Integer, RewardTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            RewardTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static RewardTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        RewardTableData.Builder builder = RewardTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "reward.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "reward.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        RewardTableData data = builder.build();

        Map<Integer, RewardTable> kvData = new HashMap<>(data.getDataCount());

        for (RewardTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public RewardTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public RewardTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, RewardTable> getKvData() {
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

    public List<RewardTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<RewardTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            RewardTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public RewardTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<RewardTable> where(Predicate<RewardTable> pred) {
        Snapshot snap = this.snapshot;
        List<RewardTable> result = new ArrayList<>();
        for (RewardTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public RewardTable first(Predicate<RewardTable> pred) {
        Snapshot snap = this.snapshot;
        for (RewardTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}