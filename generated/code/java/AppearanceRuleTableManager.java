
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
 * Auto-generated config manager for AppearanceRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class AppearanceRuleTableManager {

    private static final AppearanceRuleTableManager INSTANCE = new AppearanceRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final AppearanceRuleTableData data;
        final Map<Integer, AppearanceRuleTable> kvData;




        final Map<Integer, List<AppearanceRuleTable>> idxCardItemId;


        Snapshot(AppearanceRuleTableData data,
                 Map<Integer, AppearanceRuleTable> kvData,
                 Map<Integer, List<AppearanceRuleTable>> idxCardItemId) {
            this.data = data;
            this.kvData = kvData;
            this.idxCardItemId = idxCardItemId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            AppearanceRuleTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static AppearanceRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        AppearanceRuleTableData.Builder builder = AppearanceRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "appearancerule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "appearancerule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        AppearanceRuleTableData data = builder.build();

        Map<Integer, AppearanceRuleTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<AppearanceRuleTable>> idxCardItemId = new HashMap<>();

        for (AppearanceRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxCardItemId.computeIfAbsent(row.getCardItemId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxCardItemId);
    }

    public AppearanceRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public AppearanceRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, AppearanceRuleTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<AppearanceRuleTable> getByCardItemId(int key) {
        return snapshot.idxCardItemId.getOrDefault(key, Collections.emptyList());
    }



    // FK: card_item_id → Item.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByCardItemIdIndex(int key) {
        return snapshot.idxCardItemId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<AppearanceRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<AppearanceRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            AppearanceRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public AppearanceRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<AppearanceRuleTable> where(Predicate<AppearanceRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<AppearanceRuleTable> result = new ArrayList<>();
        for (AppearanceRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public AppearanceRuleTable first(Predicate<AppearanceRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (AppearanceRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}