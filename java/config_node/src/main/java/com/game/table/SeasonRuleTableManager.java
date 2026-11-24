
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
 * Auto-generated config manager for SeasonRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class SeasonRuleTableManager {

    private static final SeasonRuleTableManager INSTANCE = new SeasonRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final SeasonRuleTableData data;
        final Map<Integer, SeasonRuleTable> kvData;





        Snapshot(SeasonRuleTableData data,
                 Map<Integer, SeasonRuleTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            SeasonRuleTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static SeasonRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        SeasonRuleTableData.Builder builder = SeasonRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "seasonrule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "seasonrule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        SeasonRuleTableData data = builder.build();

        Map<Integer, SeasonRuleTable> kvData = new HashMap<>(data.getDataCount());

        for (SeasonRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public SeasonRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public SeasonRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, SeasonRuleTable> getKvData() {
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

    public List<SeasonRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<SeasonRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            SeasonRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public SeasonRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<SeasonRuleTable> where(Predicate<SeasonRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<SeasonRuleTable> result = new ArrayList<>();
        for (SeasonRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public SeasonRuleTable first(Predicate<SeasonRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (SeasonRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}