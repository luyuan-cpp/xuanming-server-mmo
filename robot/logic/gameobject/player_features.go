package gameobject

import "google.golang.org/protobuf/proto"

// FeatureSnapshot separates each RPC's response and error from every other RPC.
// Sequence advances on failures as well as successes, so a rejected request
// cannot consume an older successful empty list.
type FeatureSnapshot struct {
	Sequence  uint64
	MessageID uint32
	Response  proto.Message
	TipID     uint32
	Failure   string
}

func (p *Player) FeatureSequence() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.featureSequence
}

func (p *Player) SetFeatureSnapshot(messageID uint32, response proto.Message, tipID uint32, failure string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.featureSnapshots == nil {
		p.featureSnapshots = make(map[uint32]FeatureSnapshot)
	}
	p.featureSequence++
	entry := FeatureSnapshot{Sequence: p.featureSequence, MessageID: messageID, TipID: tipID, Failure: failure}
	if response != nil && tipID == 0 && failure == "" {
		entry.Response = proto.Clone(response)
	}
	p.featureSnapshots[messageID] = entry
}

// FeatureResponse returns an owned copy only for the requested source and a
// sequence newer than the cursor captured immediately before sending the RPC.
func (p *Player) FeatureResponse(messageID uint32, after uint64) (FeatureSnapshot, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	entry, ok := p.featureSnapshots[messageID]
	if !ok || entry.Sequence <= after || entry.MessageID != messageID {
		return FeatureSnapshot{}, false
	}
	if entry.Response != nil {
		entry.Response = proto.Clone(entry.Response)
	}
	return entry, true
}
