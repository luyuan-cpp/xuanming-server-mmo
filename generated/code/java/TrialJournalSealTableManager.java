
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
 * Auto-generated config manager for TrialJournalSeal.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TrialJournalSealTableManager {

    private static final TrialJournalSealTableManager INSTANCE = new TrialJournalSealTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final TrialJournalSealTableData data;
        final Map<Integer, TrialJournalSealTable> kvData;




        final Map<Integer, List<TrialJournalSealTable>> idxEntryId;


        Snapshot(TrialJournalSealTableData data,
                 Map<Integer, TrialJournalSealTable> kvData,
                 Map<Integer, List<TrialJournalSealTable>> idxEntryId) {
            this.data = data;
            this.kvData = kvData;
            this.idxEntryId = idxEntryId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            TrialJournalSealTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static TrialJournalSealTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        TrialJournalSealTableData.Builder builder = TrialJournalSealTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "trialjournalseal.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "trialjournalseal.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        TrialJournalSealTableData data = builder.build();

        Map<Integer, TrialJournalSealTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<TrialJournalSealTable>> idxEntryId = new HashMap<>();

        for (TrialJournalSealTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxEntryId.computeIfAbsent(row.getEntryId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxEntryId);
    }

    public TrialJournalSealTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public TrialJournalSealTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, TrialJournalSealTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<TrialJournalSealTable> getByEntryId(int key) {
        return snapshot.idxEntryId.getOrDefault(key, Collections.emptyList());
    }



    // FK: entry_id → TrialJournalEntry.id


    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByEntryIdIndex(int key) {
        return snapshot.idxEntryId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<TrialJournalSealTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<TrialJournalSealTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            TrialJournalSealTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public TrialJournalSealTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<TrialJournalSealTable> where(Predicate<TrialJournalSealTable> pred) {
        Snapshot snap = this.snapshot;
        List<TrialJournalSealTable> result = new ArrayList<>();
        for (TrialJournalSealTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public TrialJournalSealTable first(Predicate<TrialJournalSealTable> pred) {
        Snapshot snap = this.snapshot;
        for (TrialJournalSealTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}