package main

import (
	"bytes"
	"compress/gzip"
	"testing"

	"google.golang.org/protobuf/proto"

	"proto/battle"
)

func sampleReplayBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := proto.Marshal(&battle.BattleReplay{
		BattleId:    77,
		Outcome:     battle.EBattleOutcome_BATTLE_OUTCOME_SIDE_A_WIN,
		EndReason:   battle.EBattleReplayEndReason_BATTLE_REPLAY_END_FINISHED,
		TotalRounds: 1,
		StartState:  &battle.BattleStateS2C{Actors: []*battle.BattleActorState{{ActorId: 1}, {ActorId: 2}}},
		Rounds:      []*battle.TurnResultS2C{{RoundIndex: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDecodeBattleReplayGzipRoundTrip(t *testing.T) {
	raw := sampleReplayBytes(t)
	replay, err := decodeBattleReplay(&battle.BattleReplayEnvelope{
		FormatVersion: 1, Codec: battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_GZIP, RawBytes: uint32(len(raw)), Payload: gzipBytes(t, raw),
	})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if replay.GetBattleId() != 77 || len(replay.GetRounds()) != 1 || len(replay.GetStartState().GetActors()) != 2 {
		t.Fatalf("decoded replay mismatch: %v", replay)
	}
}

func TestDecodeBattleReplayUncompressed(t *testing.T) {
	raw := sampleReplayBytes(t)
	replay, err := decodeBattleReplay(&battle.BattleReplayEnvelope{
		FormatVersion: 1, Codec: battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_NONE, RawBytes: uint32(len(raw)), Payload: raw,
	})
	if err != nil || replay.GetBattleId() != 77 {
		t.Fatalf("decode NONE codec: replay=%v err=%v", replay, err)
	}
}

func TestDecodeBattleReplayRejectsBadEnvelopes(t *testing.T) {
	raw := sampleReplayBytes(t)
	compressed := gzipBytes(t, raw)
	cases := map[string]*battle.BattleReplayEnvelope{
		"nil":             nil,
		"unknown format":  {FormatVersion: 2, Codec: battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_GZIP, RawBytes: uint32(len(raw)), Payload: compressed},
		"unknown codec":   {FormatVersion: 1, Codec: battle.EBattleReplayCodec(9), RawBytes: uint32(len(raw)), Payload: compressed},
		"raw_bytes short": {FormatVersion: 1, Codec: battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_GZIP, RawBytes: uint32(len(raw) - 1), Payload: compressed},
		"raw_bytes long":  {FormatVersion: 1, Codec: battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_GZIP, RawBytes: uint32(len(raw) + 1), Payload: compressed},
		"truncated gzip":  {FormatVersion: 1, Codec: battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_GZIP, RawBytes: uint32(len(raw)), Payload: compressed[:len(compressed)/2]},
		"not gzip":        {FormatVersion: 1, Codec: battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_GZIP, RawBytes: uint32(len(raw)), Payload: raw},
		"over limit":      {FormatVersion: 1, Codec: battle.EBattleReplayCodec_BATTLE_REPLAY_CODEC_NONE, RawBytes: battleReplayMaxRawBytes + 1, Payload: make([]byte, battleReplayMaxRawBytes+1)},
	}
	for name, envelope := range cases {
		if _, err := decodeBattleReplay(envelope); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
