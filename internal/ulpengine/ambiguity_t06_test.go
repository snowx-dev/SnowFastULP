package ulpengine

import (
	"fmt"
	"strings"
	"testing"
)

func FuzzAmbiguityWitnessNeverChangesSelectedTuple(f *testing.F) {
	for _, seed := range []string{
		"https://example.com/login:alice:pw:tail",
		"https://example.com:12345:alice:pw",
		"https://example.com/path:to:alice:pw",
		"example.com:alice:pw",
		"https://example.com/login:alice:pw|annotation",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		if len(line) > maxParsedLineLen {
			t.Skip()
		}
		for _, loose := range []bool{false, true} {
			baseHost, baseURL, baseLogin, basePassword, baseOK := ParseLine(line, loose)
			host, url, login, password, ok, kind := ParseLineWithDiagnostics(line, loose)
			if host != baseHost || url != baseURL || login != baseLogin || password != basePassword || ok != baseOK {
				t.Fatalf("diagnostics changed selected tuple: base=(%q,%q,%q,%q,%v) diagnostic=(%q,%q,%q,%q,%v,%v)", baseHost, baseURL, baseLogin, basePassword, baseOK, host, url, login, password, ok, kind)
			}
			if kind != AmbiguityNone && !ok {
				t.Fatalf("rejected input reported ambiguity %v", kind)
			}
		}
	})
}

func BenchmarkAmbiguityColonScaling(b *testing.B) {
	for _, colons := range []int{64, 256, 1024} {
		b.Run(fmt.Sprintf("colons_%d", colons), func(b *testing.B) {
			line := "https://example.com/" + strings.Repeat("|:", colons) + "alice:pw"
			wantHost, wantURL, wantLogin, wantPassword, wantOK := ParseLine(line, false)
			if !wantOK {
				b.Fatalf("adversarial fixture not accepted at %d colons", colons)
			}
			host, url, login, password, ok, _ := ParseLineWithDiagnostics(line, false)
			if host != wantHost || url != wantURL || login != wantLogin || password != wantPassword || ok != wantOK {
				b.Fatalf("diagnostics changed selected tuple at %d colons", colons)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			b.ResetTimer()
			for range b.N {
				benchAmbiguityHost, benchAmbiguityURL, benchAmbiguityLogin, benchAmbiguityPassword, benchAmbiguityOK, benchAmbiguityKind = ParseLineWithDiagnostics(line, false)
			}
		})
	}
}

var (
	benchAmbiguityHost     string
	benchAmbiguityURL      string
	benchAmbiguityLogin    string
	benchAmbiguityPassword string
	benchAmbiguityOK       bool
	benchAmbiguityKind     AmbiguityKind
)
