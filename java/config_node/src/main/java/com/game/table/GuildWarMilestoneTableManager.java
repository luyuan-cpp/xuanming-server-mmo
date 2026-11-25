
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
 * Auto-generated config manager for GuildWarMilestone.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class GuildWarMilestoneTableManager {

    private static final GuildWarMilestoneTableManager INSTANCE = new GuildWarMilestoneTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final GuildWarMilestoneTableData data;
        final Map<Integer, GuildWarMilestoneTable> kvData;




        final Map<Integer, List<GuildWarMilestoneTable>> idxRewardItem1Id;

        final Map<Integer, List<GuildWarMilestoneTable>> idxRewardItem2Id;


        Snapshot(GuildWarMilestoneTableData data,
                 Map<Integer, GuildWarMilestoneTable> kvData,
                 Map<Integer, List<GuildWarMilestoneTable>> idxRewardItem1Id,
                 Map<Integer, List<GuildWarMilestoneTable>> idxRewardItem2Id) {
            this.data = data;
            this.kvData = kvData;
            this.idxRewardItem1Id = idxRewardItem1Id;
            this.idxRewardItem2Id = idxRewardItem2Id;
        }
    }

    private Snapshot snapshot = new Snapshot(
            GuildWarMilestoneTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static GuildWarMilestoneTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        GuildWarMilestoneTableData.Builder builder = GuildWarMilestoneTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "guildwarmilestone.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "guildwarmilestone.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        GuildWarMilestoneTableData data = builder.build();

        Map<Integer, GuildWarMilestoneTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<GuildWarMilestoneTable>> idxRewardItem1Id = new HashMap<>();
        Map<Integer, List<GuildWarMilestoneTable>> idxRewardItem2Id = new HashMap<>();

        for (GuildWarMilestoneTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxRewardItem1Id.computeIfAbsent(row.getRewardItem1Id(), k -> new ArrayList<>()).add(row);
            idxRewardItem2Id.computeIfAbsent(row.getRewardItem2Id(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxRewardItem1Id, idxRewardItem2Id);
    }

    public GuildWarMilestoneTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public GuildWarMilestoneTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, GuildWarMilestoneTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<GuildWarMilestoneTable> getByRewardItem1Id(int key) {
        return snapshot.idxRewardItem1Id.getOrDefault(key, Collections.emptyList());
    }

    public List<GuildWarMilestoneTable> getByRewardItem2Id(int key) {
        return snapshot.idxRewardItem2Id.getOrDefault(key, Collections.emptyList());
    }



    // FK: reward_item1_id → Item.id

    // FK: reward_item2_id → Item.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByRewardItem1IdIndex(int key) {
        return snapshot.idxRewardItem1Id.getOrDefault(key, Collections.emptyList()).size();
    }

    public int countByRewardItem2IdIndex(int key) {
        return snapshot.idxRewardItem2Id.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<GuildWarMilestoneTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<GuildWarMilestoneTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            GuildWarMilestoneTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public GuildWarMilestoneTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<GuildWarMilestoneTable> where(Predicate<GuildWarMilestoneTable> pred) {
        Snapshot snap = this.snapshot;
        List<GuildWarMilestoneTable> result = new ArrayList<>();
        for (GuildWarMilestoneTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public GuildWarMilestoneTable first(Predicate<GuildWarMilestoneTable> pred) {
        Snapshot snap = this.snapshot;
        for (GuildWarMilestoneTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}