package store

import "time"

// SetStageAhead changes stageAhead for a test and returns the undo.
func SetStageAhead(d time.Duration) func() {
	old := stageAhead
	stageAhead = d
	return func() { stageAhead = old }
}
