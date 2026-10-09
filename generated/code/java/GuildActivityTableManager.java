
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
 * Auto-generated config manager for GuildActivity.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class GuildActivityTableManager {

    private static final GuildActivityTableManager INSTANCE = new GuildActivityTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final GuildActivityTableData data;
        final Map<Integer, GuildActivityTable> kvData;




        final Map<Integer, List<GuildActivityTable>> idxRewardId;

        final Map<Integer, List<GuildActivityTable>> idxDungeonId;


        Snapshot(GuildActivityTableData data,
                 Map<Integer, GuildActivityTable> kvData,
                 Map<Integer, List<GuildActivityTable>> idxRewardId,
                 Map<Integer, List<GuildActivityTable>> idxDungeonId) {
            this.data = data;
            this.kvData = kvData;
            this.idxRewardId = idxRewardId;
            this.idxDungeonId = idxDungeonId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            GuildActivityTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static GuildActivityTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        GuildActivityTableData.Builder builder = GuildActivityTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "guildactivity.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "guildactivity.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        GuildActivityTableData data = builder.build();

        Map<Integer, GuildActivityTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<GuildActivityTable>> idxRewardId = new HashMap<>();
        Map<Integer, List<GuildActivityTable>> idxDungeonId = new HashMap<>();

        for (GuildActivityTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxRewardId.computeIfAbsent(row.getRewardId(), k -> new ArrayList<>()).add(row);
            idxDungeonId.computeIfAbsent(row.getDungeonId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxRewardId, idxDungeonId);
    }

    public GuildActivityTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public GuildActivityTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, GuildActivityTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<GuildActivityTable> getByRewardId(int key) {
        return snapshot.idxRewardId.getOrDefault(key, Collections.emptyList());
    }

    public List<GuildActivityTable> getByDungeonId(int key) {
        return snapshot.idxDungeonId.getOrDefault(key, Collections.emptyList());
    }



    // FK: reward_id → Reward.id

    // FK: dungeon_id → Dungeon.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByRewardIdIndex(int key) {
        return snapshot.idxRewardId.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByDungeonIdIndex(int key) {
        return snapshot.idxDungeonId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<GuildActivityTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<GuildActivityTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            GuildActivityTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public GuildActivityTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<GuildActivityTable> where(Predicate<GuildActivityTable> pred) {
        Snapshot snap = this.snapshot;
        List<GuildActivityTable> result = new ArrayList<>();
        for (GuildActivityTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public GuildActivityTable first(Predicate<GuildActivityTable> pred) {
        Snapshot snap = this.snapshot;
        for (GuildActivityTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}