
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
 * Auto-generated config manager for TrialJournalEntry.
 * DO NOT EDIT — regenerate from Excel via Data Table Exporter.
 */
public class TrialJournalEntryTableManager {

    private static final TrialJournalEntryTableManager INSTANCE = new TrialJournalEntryTableManager();

    /**
     * Internal snapshot holding all parsed data and indices.
     * load() builds a new snapshot and swaps it in, replacing the old one.
     */
    private static class Snapshot {
        final TrialJournalEntryTableData data;
        final Map<Integer, TrialJournalEntryTable> kvData;




        final Map<Integer, List<TrialJournalEntryTable>> idxChapterId;


        Snapshot(TrialJournalEntryTableData data,
                 Map<Integer, TrialJournalEntryTable> kvData,
                 Map<Integer, List<TrialJournalEntryTable>> idxChapterId) {
            this.data = data;
            this.kvData = kvData;
            this.idxChapterId = idxChapterId;
        }
    }

    private Snapshot snapshot = new Snapshot(
            TrialJournalEntryTableData.getDefaultInstance(),
            Collections.emptyMap(),
            Collections.emptyMap()
    );

    public static TrialJournalEntryTableManager getInstance() {
        return INSTANCE;
    }

    public void load(String configDir, boolean useBinary) throws Exception {
        TrialJournalEntryTableData.Builder builder = TrialJournalEntryTableData.newBuilder();
        if (useBinary) {
            byte[] raw = Files.readAllBytes(Path.of(configDir, "trialjournalentry.pb"));
            builder.mergeFrom(raw);
        } else {
            String json = Files.readString(Path.of(configDir, "trialjournalentry.json"));
            JsonFormat.parser().ignoringUnknownFields().merge(json, builder);
        }
        TrialJournalEntryTableData data = builder.build();

        Map<Integer, TrialJournalEntryTable> kvData = new HashMap<>(data.getDataCount());
        Map<Integer, List<TrialJournalEntryTable>> idxChapterId = new HashMap<>();

        for (TrialJournalEntryTable row : data.getDataList()) {
            kvData.put(row.getId(), row);
            idxChapterId.computeIfAbsent(row.getChapterId(), k -> new ArrayList<>()).add(row);
        }

        this.snapshot = new Snapshot(data, kvData, idxChapterId);
    }

    public TrialJournalEntryTableData findAll() {
        return snapshot.data;
    }

    /** 热更契约：返回的对象属于当前快照，调用方**只存 id**，不要长期持有引用。 */
    public TrialJournalEntryTable findById(int id) {
        return snapshot.kvData.get(id);
    }

    public Map<Integer, TrialJournalEntryTable> getKvData() {
        return Collections.unmodifiableMap(snapshot.kvData);
    }







    public List<TrialJournalEntryTable> getByChapterId(int key) {
        return snapshot.idxChapterId.getOrDefault(key, Collections.emptyList());
    }




    // ---- Exists ----

    public boolean exists(int id) {
        return snapshot.kvData.containsKey(id);
    }



    // ---- Count ----

    public int count() {
        return snapshot.kvData.size();
    }




    public int countByChapterIdIndex(int key) {
        return snapshot.idxChapterId.getOrDefault(key, Collections.emptyList()).size();
    }


    // ---- FindByIds (IN) ----

    public List<TrialJournalEntryTable> findByIds(List<Integer> ids) {
        Snapshot snap = this.snapshot;
        List<TrialJournalEntryTable> result = new ArrayList<>(ids.size());
        for (int id : ids) {
            TrialJournalEntryTable row = snap.kvData.get(id);
            if (row != null) { result.add(row); }
        }
        return result;
    }

    // ---- RandOne ----

    public TrialJournalEntryTable randOne() {
        Snapshot snap = this.snapshot;
        if (snap.data == null || snap.data.getDataCount() == 0) return null;
        int idx = ThreadLocalRandom.current().nextInt(snap.data.getDataCount());
        return snap.data.getData(idx);
    }

    // ---- Where / First ----

    public List<TrialJournalEntryTable> where(Predicate<TrialJournalEntryTable> pred) {
        Snapshot snap = this.snapshot;
        List<TrialJournalEntryTable> result = new ArrayList<>();
        for (TrialJournalEntryTable row : snap.data.getDataList()) {
            if (pred.test(row)) { result.add(row); }
        }
        return result;
    }

    public TrialJournalEntryTable first(Predicate<TrialJournalEntryTable> pred) {
        Snapshot snap = this.snapshot;
        for (TrialJournalEntryTable row : snap.data.getDataList()) {
            if (pred.test(row)) { return row; }
        }
        return null;
    }
}