package main

import (
	"context"
	"fmt"
)

// sync wrapper for the deferred-load runODScan, hides channel dance.
// test-only, prod calls runODScan to get phase 0/1 overlap
func runODScanSync(ctx context.Context, cfg odConfig, m *odMetrics) (*odResult, error) {
	res, loadCh, err := runODScan(ctx, cfg, m)
	if err != nil {
		return nil, err
	}
	if loadCh != nil {
		lr := <-loadCh
		if lr.Err != nil {
			return nil, fmt.Errorf("od-scan: %w", lr.Err)
		}
		res.DestKeyBucketPaths = lr.DestKeyBucketPaths
		res.TotalKeysLoaded = lr.TotalKeysLoaded
		res.Elapsed = lr.Elapsed
	}
	return res, nil
}
