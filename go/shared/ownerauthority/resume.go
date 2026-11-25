package ownerauthority

// EntryState is the minimal client-facing resume state machine output.
type EntryState uint8

const (
	EntryWait EntryState = iota
	EntryTarget
	EntryRoleRequired
)

// WaitReason explains why the client must wait and retry the same entry endpoint.
type WaitReason uint8

const (
	WaitNone WaitReason = iota
	WaitAuthorityUnavailable
	WaitBarrierPending
	WaitTransitionInProgress
	WaitNoOwnerAssigned
)

// DefaultResumeRetryAfterMs is the default backoff hint when authority is unavailable
// or ownership is still converging.
const DefaultResumeRetryAfterMs uint32 = 1000

// ResumeInput carries all authoritative facts needed to decide reconnect routing.
type ResumeInput struct {
	QueryErr            error
	HasRole             bool
	Found               bool
	Record              OwnerRecord
	NowMs               int64
	DefaultRetryAfterMs uint32
}

// ResumeDecision is the deterministic output of ResolveResume.
type ResumeDecision struct {
	State        EntryState
	WaitReason   WaitReason
	RetryAfterMs uint32
	Record       OwnerRecord
}

// ResolveResume deterministically converges reconnect state without any local route guessing.
func ResolveResume(in ResumeInput) ResumeDecision {
	defaultRetry := in.DefaultRetryAfterMs
	if defaultRetry == 0 {
		defaultRetry = DefaultResumeRetryAfterMs
	}
	if in.QueryErr != nil {
		return ResumeDecision{
			State:        EntryWait,
			WaitReason:   WaitAuthorityUnavailable,
			RetryAfterMs: defaultRetry,
		}
	}
	if !in.HasRole {
		return ResumeDecision{
			State:      EntryRoleRequired,
			WaitReason: WaitNone,
		}
	}
	if !in.Found || in.Record.Phase == PhaseNone || in.Record.OwnerType == OwnerNone {
		return ResumeDecision{
			State:        EntryWait,
			WaitReason:   WaitNoOwnerAssigned,
			RetryAfterMs: defaultRetry,
		}
	}
	switch in.Record.Phase {
	case PhasePending:
		if in.NowMs < in.Record.AdmitNotBeforeMs {
			return ResumeDecision{
				State:        EntryWait,
				WaitReason:   WaitBarrierPending,
				RetryAfterMs: clampRetryMs(in.Record.AdmitNotBeforeMs - in.NowMs),
				Record:       in.Record,
			}
		}
		return ResumeDecision{
			State:        EntryWait,
			WaitReason:   WaitTransitionInProgress,
			RetryAfterMs: defaultRetry,
			Record:       in.Record,
		}
	case PhaseAdmitted:
		return ResumeDecision{
			State:      EntryTarget,
			WaitReason: WaitNone,
			Record:     in.Record,
		}
	default:
		return ResumeDecision{
			State:        EntryWait,
			WaitReason:   WaitAuthorityUnavailable,
			RetryAfterMs: defaultRetry,
		}
	}
}
