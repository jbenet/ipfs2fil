package store

import "time"

// SetStageAhead changes stageAhead for a test and returns the undo.
func SetStageAhead(d time.Duration) func() {
	old := stageAhead
	stageAhead = d
	return func() { stageAhead = old }
}

// SetPullTimeout changes pullTimeout for a test and returns the undo.
func SetPullTimeout(d time.Duration) func() {
	old := pullTimeout
	pullTimeout = func(int64) time.Duration { return d }
	return func() { pullTimeout = old }
}

// SetHelpAfter changes helpAfter for a test and returns the undo.
func SetHelpAfter(d time.Duration) func() {
	old := helpAfter
	helpAfter = d
	return func() { helpAfter = old }
}

// SetSlowPullWait changes slowPullWait for a test and returns the undo.
func SetSlowPullWait(d time.Duration) func() {
	old := slowPullWait
	slowPullWait = func(int64) time.Duration { return d }
	return func() { slowPullWait = old }
}
