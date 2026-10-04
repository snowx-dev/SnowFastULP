package history

import "time"

// SetFingerprintReadPause injects latency into every sampled read of the
// fingerprint pass, so tests can keep the "validation takes longer than the
// heartbeat interval" premise alive now that the sampled fingerprint reads
// only the head and tail of each file. Zero (the default) disables it.
func SetFingerprintReadPause(d time.Duration) (restore func()) {
	old := fingerprintReadPause
	fingerprintReadPause = d
	return func() { fingerprintReadPause = old }
}

// SetSweepOwnerRecheckHook installs a hook that runs between the sweeper's
// first owner-metadata read and its immediate pre-action re-read, so tests can
// race the owner token deterministically. Pass nil to clear.
func SetSweepOwnerRecheckHook(f func()) { sweepOwnerRecheckHook = f }

// SetHeartbeatInterval overrides the validation heartbeat cadence and returns
// a restore func that reinstalls the previous value.
func SetHeartbeatInterval(d time.Duration) (restore func()) {
	old := heartbeatInterval
	heartbeatInterval = d
	return func() { heartbeatInterval = old }
}

// SetHeartbeatRefreshHook installs a hook that runs after each successful
// periodic lease refresh during validation, so tests can observe refreshes
// mid-pass deterministically. Pass nil to clear.
func SetHeartbeatRefreshHook(f func()) { heartbeatRefreshHook = f }

// SQLiteFileURI exposes the SQLite URI construction for table tests.
func SQLiteFileURI(path string) (string, error) {
	return sqliteFileURI(path)
}

// OpenDatabaseProbe runs openDatabase and discards the handle, so tests can
// assert URI/validation errors without depending on a real database.
func OpenDatabaseProbe(path string, readOnly bool) error {
	db, err := openDatabase(path, readOnly)
	if db != nil {
		db.Close()
	}
	return err
}
