
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
 * Auto-generated config manager for GuildLevel.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class GuildLevelTableManager {

    private static final GuildLevelTableManager INSTANCE = new GuildLevelTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final GuildLevelTableData data;
        final Map<Integer, GuildLevelTable> kvData;





        Snapshot(GuildLevelTableData data,
                 Map<Integer, GuildLevelTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            GuildLevelTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static GuildLevelTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        GuildLevelTableData.Builder builder = GuildLevelTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "guildlevel.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "guildlevel.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        GuildLevelTableData data = builder.build();

        Map<Integer, GuildLevelTable> kvData = new HashMap<>(data.getDataCount());

        for (GuildLevelTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public GuildLevelTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public GuildLevelTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, GuildLevelTable> getKvData() {
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

    public List<GuildLevelTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<GuildLevelTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            GuildLevelTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public GuildLevelTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<GuildLevelTable> where(Predicate<GuildLevelTable> pred) {
        Snapshot snap = this.snapshot;
        List<GuildLevelTable> result = new ArrayList<>();
        for (GuildLevelTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public GuildLevelTable first(Predicate<GuildLevelTable> pred) {
        Snapshot snap = this.snapshot;
        for (GuildLevelTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}