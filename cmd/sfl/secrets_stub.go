//go:build !secrets

package main

import (
	"errors"

	"github.com/snowx-dev/SnowFastULP/internal/secrets"
	"github.com/snowx-dev/SnowFastULP/internal/sflog"
)

const secretsEnabled = false

// buildSecretSink is the no-scanner stub for the default build. Titus (and its
// ~14MB of embedded rules + regex engine) is only linked in with `-tags
// secrets`, so -secrets here fails fast with an actionable message instead of
// silently doing nothing. The filter arg is accepted but ignored.
func buildSecretSink(string, int, secrets.RuleFilter) (sflog.SecretSink, func() (secrets.Stats, error), error) {
	return nil, nil, errors.New("this sfl build has no secrets scanning support; rebuild with `-tags secrets`")
}
