package sflog

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// capSink is a concurrency-safe SecretSink recorder for tests: extraction may
// call ScanSecrets from multiple worker goroutines.
type capSink struct {
	mu  sync.Mutex
	got []string
}

func (c *capSink) ScanSecrets(_ context.Context, content []byte, prov string) int {
	c.mu.Lock()
	c.got = append(c.got, prov+"|"+string(content))
	c.mu.Unlock()
	return 0
}

// sawSecret reports whether any recorded call contains both fragments.
func (c *capSink) sawSecret(nameFrag, secretFrag string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range c.got {
		if strings.Contains(g, nameFrag) && strings.Contains(g, secretFrag) {
			return true
		}
	}
	return false
}

func TestScanSecretsNoopWhenNil(t *testing.T) {
	ec := extractCtx{}                                                   // no sink
	ec.scanSecrets(context.Background(), strings.NewReader("data"), "p") // must not panic
}

func TestScanSecretsReadsCappedAndForwards(t *testing.T) {
	c := &capSink{}
	ec := extractCtx{secrets: c, secretMaxLen: 4}
	ec.scanSecrets(context.Background(), strings.NewReader("abcdefgh"), "log!f")
	if len(c.got) != 1 || c.got[0] != "log!f|abcd" {
		t.Fatalf("got %v, want [log!f|abcd]", c.got)
	}
}

func TestScanSecretsSkipsEmpty(t *testing.T) {
	c := &capSink{}
	ec := extractCtx{secrets: c, secretMaxLen: defaultSecretMaxLen}
	ec.scanSecrets(context.Background(), strings.NewReader(""), "log!f")
	if len(c.got) != 0 {
		t.Fatalf("empty reader should not reach the sink, got %v", c.got)
	}
}

// TestScanSecretsRestoresExtractingStage guards the sequential-archive stuck
// label: scanSecrets flags the slot StageScanning for the Titus call, then must
// flip it back to StageExtracting so a RAR/7z reader parsing the next credential
// member isn't reported as still scanning secrets.
func TestScanSecretsRestoresExtractingStage(t *testing.T) {
	c := &capSink{}
	var mu sync.Mutex
	var stages []WorkerStage
	ec := extractCtx{
		secrets:      c,
		secretMaxLen: defaultSecretMaxLen,
		setStage: func(s WorkerStage) {
			mu.Lock()
			stages = append(stages, s)
			mu.Unlock()
		},
	}
	ec.scanSecrets(context.Background(), strings.NewReader("AKIAEXAMPLE"), "log!f")

	mu.Lock()
	defer mu.Unlock()
	if len(stages) != 2 || stages[0] != StageScanning || stages[1] != StageExtracting {
		t.Fatalf("stage sequence = %v, want [StageScanning, StageExtracting]", stages)
	}
}
