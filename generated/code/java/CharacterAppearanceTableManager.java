
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
 * Auto-generated config manager for CharacterAppearance.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class CharacterAppearanceTableManager {

    private static final CharacterAppearanceTableManager INSTANCE = new CharacterAppearanceTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final CharacterAppearanceTableData data;
        final Map<Integer, CharacterAppearanceTable> kvData;





        Snapshot(CharacterAppearanceTableData data,
                 Map<Integer, CharacterAppearanceTable> kvData) {
            this.data = data;
            this.kvData = kvData;
        }
    }

    private Snapshot snapshot = new Snapshot(
            CharacterAppearanceTableData.getDefaultInstance(),
            Collections.emptyMap()
    );

    public static CharacterAppearanceTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        CharacterAppearanceTableData.Builder builder = CharacterAppearanceTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "characterappearance.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "characterappearance.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        CharacterAppearanceTableData data = builder.build();

        Map<Integer, CharacterAppearanceTable> kvData = new HashMap<>(data.getDataCount());

        for (CharacterAppearanceTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
        }

        this.snapshot = new Snapshot(data, kvData);
    }

    public CharacterAppearanceTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public CharacterAppearanceTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, CharacterAppearanceTable> getKvData() {
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

    public List<CharacterAppearanceTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<CharacterAppearanceTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            CharacterAppearanceTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public CharacterAppearanceTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<CharacterAppearanceTable> where(Predicate<CharacterAppearanceTable> pred) {
        Snapshot snap = this.snapshot;
        List<CharacterAppearanceTable> result = new ArrayList<>();
        for (CharacterAppearanceTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public CharacterAppearanceTable first(Predicate<CharacterAppearanceTable> pred) {
        Snapshot snap = this.snapshot;
        for (CharacterAppearanceTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}