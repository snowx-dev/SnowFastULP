//go:build secrets

package main

// secretsEnabled gates secret-scanning help entries (and any secrets-only UI
// affordances) so a default build's -h never advertises a feature that isn't
// compiled in. The flags themselves stay registered (see main.go) so -secrets
// still errors with an actionable "rebuild with -tags secrets" message.
const secretsEnabled = true
