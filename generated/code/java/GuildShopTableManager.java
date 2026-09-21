
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
 * Auto-generated config manager for GuildShop.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class GuildShopTableManager {

    private static final GuildShopTableManager INSTANCE = new GuildShopTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final GuildShopTableData data;
        final Map<Integer, GuildShopTable> kvData;




        final Map<Integer, List<GuildShopTable>> idxItemId;


        Snapshot(GuildShopTableData data,
                 Map<Integer, GuildShopTable> kvData,
                 Map<Integer, List<GuildShopTable>> idxItemId) {
            this.data = data;
            this.kvData = kvData;
            this.idxItemId = idxItemId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            GuildShopTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static GuildShopTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        GuildShopTableData.Builder builder = GuildShopTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "guildshop.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "guildshop.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        GuildShopTableData data = builder.build();

        Map<Integer, GuildShopTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<GuildShopTable>> idxItemId = new HashMap<>();

        for (GuildShopTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxItemId.computeIfAbsent(row.getItemId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxItemId);
    }

    public GuildShopTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public GuildShopTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, GuildShopTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<GuildShopTable> getByItemId(int key) {
        return snapshot.idxItemId.getOrDefault(key, Collections.emptyList());
    }



    // FK: item_id → Item.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByItemIdIndex(int key) {
        return snapshot.idxItemId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<GuildShopTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<GuildShopTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            GuildShopTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public GuildShopTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<GuildShopTable> where(Predicate<GuildShopTable> pred) {
        Snapshot snap = this.snapshot;
        List<GuildShopTable> result = new ArrayList<>();
        for (GuildShopTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public GuildShopTable first(Predicate<GuildShopTable> pred) {
        Snapshot snap = this.snapshot;
        for (GuildShopTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}