// Package jobs defines the background jobs the server runs.
package jobs

import "github.com/riverqueue/river"

// Queue names.
const (
	QueueChecks      = "checks"
	QueueMaintenance = "maintenance"
)

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

// ExpireStaleUploads expires versions still uploading after the grace
// window closed, so their storage is released even if the browser never
// comes back. It runs every minute.
type ExpireStaleUploads struct{}

// Kind names the job in River's table.
func (ExpireStaleUploads) Kind() string { return "expire_stale_uploads" }

// InsertOpts puts the sweep on the maintenance queue.
func (ExpireStaleUploads) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 3}
}

// MeasureStorage sums the real bucket so the storage limiter can never
// trust a count that drifted below reality. It runs every 10 minutes.
type MeasureStorage struct{}

// Kind names the job in River's table.
func (MeasureStorage) Kind() string { return "measure_storage" }

// InsertOpts puts the measurement on the maintenance queue.
func (MeasureStorage) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 3}
}
