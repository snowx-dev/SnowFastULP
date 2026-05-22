package main

import (
	"crypto/rand"
	"fmt"
	"time"
)

// crockford b32, no i/l/o/u
const crockfordAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

const runIDLen = 6

// 6-char id from crypto/rand, no time-of-day for opsec
func newRunID() (string, error) {
	var raw [runIDLen]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("runid: read entropy: %w", err)
	}
	out := make([]byte, runIDLen)
	for i, b := range raw {
		out[i] = crockfordAlphabet[b&0x1f] // 5 bits, uniform
	}
	return string(out), nil
}

// yyyymmdd_<id>
func runStamp(t time.Time, id string) string {
	return t.Format("20060102") + "_" + id
}
