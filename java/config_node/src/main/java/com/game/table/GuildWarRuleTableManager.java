
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
 * Auto-generated config manager for GuildWarRule.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class GuildWarRuleTableManager {

    private static final GuildWarRuleTableManager INSTANCE = new GuildWarRuleTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final GuildWarRuleTableData data;
        final Map<Integer, GuildWarRuleTable> kvData;




        final Map<Integer, List<GuildWarRuleTable>> idxBattleConfigId;


        Snapshot(GuildWarRuleTableData data,
                 Map<Integer, GuildWarRuleTable> kvData,
                 Map<Integer, List<GuildWarRuleTable>> idxBattleConfigId) {
            this.data = data;
            this.kvData = kvData;
            this.idxBattleConfigId = idxBattleConfigId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            GuildWarRuleTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static GuildWarRuleTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        GuildWarRuleTableData.Builder builder = GuildWarRuleTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "guildwarrule.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "guildwarrule.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        GuildWarRuleTableData data = builder.build();

        Map<Integer, GuildWarRuleTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<GuildWarRuleTable>> idxBattleConfigId = new HashMap<>();

        for (GuildWarRuleTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxBattleConfigId.computeIfAbsent(row.getBattleConfigId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxBattleConfigId);
    }

    public GuildWarRuleTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public GuildWarRuleTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, GuildWarRuleTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<GuildWarRuleTable> getByBattleConfigId(int key) {
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

    public List<GuildWarRuleTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<GuildWarRuleTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            GuildWarRuleTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public GuildWarRuleTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<GuildWarRuleTable> where(Predicate<GuildWarRuleTable> pred) {
        Snapshot snap = this.snapshot;
        List<GuildWarRuleTable> result = new ArrayList<>();
        for (GuildWarRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public GuildWarRuleTable first(Predicate<GuildWarRuleTable> pred) {
        Snapshot snap = this.snapshot;
        for (GuildWarRuleTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}