// Package jobs defines the background jobs the API enqueues.
package jobs

import "github.com/riverqueue/river"

// CheckVersion runs every background check on one completed version. It is
// inserted in the same transaction that moves the version to "checking",
// and unique by version, so a version gets exactly one check job even if
// "complete" is called twice.
type CheckVersion struct {
	VersionID string `json:"version_id" river:"unique"`
}

// Kind names the job in River's table.
func (CheckVersion) Kind() string { return "check_version" }

// InsertOpts makes the job unique per version across every live state.
func (CheckVersion) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueChecks, MaxAttempts: 8, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

// QueueChecks is the queue the check workers read.
const QueueChecks = "checks"
