
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
 * Auto-generated config manager for PlayerLevelExp.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class PlayerLevelExpTableManager {

    private static final PlayerLevelExpTableManager INSTANCE = new PlayerLevelExpTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final PlayerLevelExpTableData data;
        final Map<Integer, PlayerLevelExpTable> kvData;





        Snapshot(PlayerLevelExpTableData data,
                 Map<Integer, PlayerLevelExpTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            PlayerLevelExpTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static PlayerLevelExpTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        PlayerLevelExpTableData.Builder builder = PlayerLevelExpTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "playerlevelexp.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "playerlevelexp.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        PlayerLevelExpTableData data = builder.build();

        Map<Integer, PlayerLevelExpTable> kvData = new HashMap<>(data.getDataCount());

        for (PlayerLevelExpTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public PlayerLevelExpTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public PlayerLevelExpTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, PlayerLevelExpTable> getKvData() {
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

    public List<PlayerLevelExpTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<PlayerLevelExpTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            PlayerLevelExpTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public PlayerLevelExpTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<PlayerLevelExpTable> where(Predicate<PlayerLevelExpTable> pred) {
        Snapshot snap = this.snapshot;
        List<PlayerLevelExpTable> result = new ArrayList<>();
        for (PlayerLevelExpTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public PlayerLevelExpTable first(Predicate<PlayerLevelExpTable> pred) {
        Snapshot snap = this.snapshot;
        for (PlayerLevelExpTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}