
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
 * Auto-generated config manager for GuildDonate.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class GuildDonateTableManager {

    private static final GuildDonateTableManager INSTANCE = new GuildDonateTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final GuildDonateTableData data;
        final Map<Integer, GuildDonateTable> kvData;





        Snapshot(GuildDonateTableData data,
                 Map<Integer, GuildDonateTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            GuildDonateTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static GuildDonateTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        GuildDonateTableData.Builder builder = GuildDonateTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "guilddonate.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "guilddonate.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        GuildDonateTableData data = builder.build();

        Map<Integer, GuildDonateTable> kvData = new HashMap<>(data.getDataCount());

        for (GuildDonateTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public GuildDonateTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public GuildDonateTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, GuildDonateTable> getKvData() {
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

    public List<GuildDonateTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<GuildDonateTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            GuildDonateTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public GuildDonateTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<GuildDonateTable> where(Predicate<GuildDonateTable> pred) {
        Snapshot snap = this.snapshot;
        List<GuildDonateTable> result = new ArrayList<>();
        for (GuildDonateTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public GuildDonateTable first(Predicate<GuildDonateTable> pred) {
        Snapshot snap = this.snapshot;
        for (GuildDonateTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}