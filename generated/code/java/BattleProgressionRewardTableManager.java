
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
 * Auto-generated config manager for BattleProgressionReward.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class BattleProgressionRewardTableManager {

    private static final BattleProgressionRewardTableManager INSTANCE = new BattleProgressionRewardTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final BattleProgressionRewardTableData data;
        final Map<Integer, BattleProgressionRewardTable> kvData;





        Snapshot(BattleProgressionRewardTableData data,
                 Map<Integer, BattleProgressionRewardTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            BattleProgressionRewardTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static BattleProgressionRewardTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        BattleProgressionRewardTableData.Builder builder = BattleProgressionRewardTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "battleprogressionreward.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "battleprogressionreward.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        BattleProgressionRewardTableData data = builder.build();

        Map<Integer, BattleProgressionRewardTable> kvData = new HashMap<>(data.getDataCount());

        for (BattleProgressionRewardTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public BattleProgressionRewardTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public BattleProgressionRewardTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, BattleProgressionRewardTable> getKvData() {
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

    public List<BattleProgressionRewardTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<BattleProgressionRewardTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            BattleProgressionRewardTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public BattleProgressionRewardTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<BattleProgressionRewardTable> where(Predicate<BattleProgressionRewardTable> pred) {
        Snapshot snap = this.snapshot;
        List<BattleProgressionRewardTable> result = new ArrayList<>();
        for (BattleProgressionRewardTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public BattleProgressionRewardTable first(Predicate<BattleProgressionRewardTable> pred) {
        Snapshot snap = this.snapshot;
        for (BattleProgressionRewardTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}