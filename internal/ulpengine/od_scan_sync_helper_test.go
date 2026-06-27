package ulpengine

import "context"

// runODScan is synchronous now (no deferred routing); this wrapper is kept so
// existing tests keep compiling.
func runODScanSync(ctx context.Context, cfg odConfig, m *ODMetrics) (*ODResult, error) {
	return runODScan(ctx, cfg, m)
}

// destBucketHasKey reports whether key k would be found in bucket bucketIdx of
// the library, by gathering that bucket's keys from the result's sidecars.
// replaces the old "is k in dest_keys/bucket_NNNN.bin" check.
func destBucketHasKey(res *ODResult, k uint64, bucketIdx, numBuckets int) (bool, error) {
	for _, sc := range res.DestSidecarPaths {
		keys, err := sidecarBucketKeys(sc, bucketIdx, numBuckets)
		if err != nil {
			return false, err
		}
		for _, key := range keys {
			if key == k {
				return true, nil
			}
		}
	}
	return false, nil
}
