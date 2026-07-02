package sflog

import (
	"context"
	"strings"
	"testing"
)

type capSink struct{ got []string }

func (c *capSink) ScanSecrets(_ context.Context, content []byte, prov string) {
	c.got = append(c.got, prov+"|"+string(content))
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
