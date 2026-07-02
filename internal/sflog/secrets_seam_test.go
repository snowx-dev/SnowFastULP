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

func (c *capSink) ScanSecrets(_ context.Context, content []byte, prov string) {
	c.mu.Lock()
	c.got = append(c.got, prov+"|"+string(content))
	c.mu.Unlock()
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
	ec := extractCtx{} // no sink
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
