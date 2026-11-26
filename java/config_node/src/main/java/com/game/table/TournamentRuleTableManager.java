
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
 * Auto-generated config manager for TournamentRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TournamentRuleTableManager {

    private static final TournamentRuleTableManager INSTANCE = new TournamentRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final TournamentRuleTableData data;
        final Map<Integer, TournamentRuleTable> kvData;




        final Map<Integer, List<TournamentRuleTable>> idxBattleConfigId;


        Snapshot(TournamentRuleTableData data,
                 Map<Integer, TournamentRuleTable> kvData,
                 Map<Integer, List<TournamentRuleTable>> idxBattleConfigId) {
            this.data = data;
            this.kvData = kvData;
            this.idxBattleConfigId = idxBattleConfigId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            TournamentRuleTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static TournamentRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        TournamentRuleTableData.Builder builder = TournamentRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "tournamentrule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "tournamentrule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        TournamentRuleTableData data = builder.build();

        Map<Integer, TournamentRuleTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<TournamentRuleTable>> idxBattleConfigId = new HashMap<>();

        for (TournamentRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxBattleConfigId.computeIfAbsent(row.getBattleConfigId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxBattleConfigId);
    }

    public TournamentRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public TournamentRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, TournamentRuleTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<TournamentRuleTable> getByBattleConfigId(int key) {
        return snapshot.idxBattleConfigId.getOrDefault(key, Collections.emptyList());
    }



    // FK: battle_config_id → Dungeon.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByBattleConfigIdIndex(int key) {
        return snapshot.idxBattleConfigId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<TournamentRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<TournamentRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            TournamentRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public TournamentRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<TournamentRuleTable> where(Predicate<TournamentRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<TournamentRuleTable> result = new ArrayList<>();
        for (TournamentRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public TournamentRuleTable first(Predicate<TournamentRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (TournamentRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}