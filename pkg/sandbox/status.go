package sandbox

// Status is the outcome of one benchmark against one provider.
//
// The four values exist to keep a leaderboard honest. A provider with no
// suspend operation must not be scored zero on a resume benchmark; it must be
// reported as unsupported, which is a statement about capability, not speed.
type Status string

const (
	// StatusCompleted means the benchmark ran and its numbers are reportable.
	StatusCompleted Status = "completed"
	// StatusFailed means the provider supports the operation but the run broke.
	StatusFailed Status = "failed"
	// StatusUnsupported means the provider offers no such capability.
	StatusUnsupported Status = "unsupported"
	// StatusSkipped means the configuration did not ask for this benchmark.
	StatusSkipped Status = "skipped"
)
