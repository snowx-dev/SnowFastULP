package ulpengine

import "testing"

func TestParseWithAmbiguityLooseModeUsesSelectedGrammar(t *testing.T) {
	line := "https://example.com/login:alice:pw:tail"
	strictHost, strictURL, strictLogin, strictPassword, strictOK, strictKind := parseWithAmbiguity(line, false)
	looseHost, looseURL, looseLogin, loosePassword, looseOK, looseKind := parseWithAmbiguity(line, true)
	if strictHost != looseHost || strictURL != looseURL || strictLogin != looseLogin || strictPassword != loosePassword || strictOK != looseOK || strictKind != looseKind {
		t.Fatalf("loose mode changed strict-selected result: strict=(%q,%q,%q,%q,%v,%v) loose=(%q,%q,%q,%q,%v,%v)", strictHost, strictURL, strictLogin, strictPassword, strictOK, strictKind, looseHost, looseURL, looseLogin, loosePassword, looseOK, looseKind)
	}
	if strictKind != AmbiguityPathOrPassword {
		t.Fatalf("loose mode did not report the strict-grammar witness: %v", strictKind)
	}

	looseOnly := "example.com:$user:opaque-password"
	if _, _, _, _, ok := parseFor(looseOnly, false); ok {
		t.Fatal("loose-only control unexpectedly passed strict parsing")
	}
	_, _, _, _, ok, kind := parseWithAmbiguity(looseOnly, true)
	if !ok || kind != AmbiguityNone {
		t.Fatalf("loose-only unique record = accepted %v ambiguity %v, want accepted unique", ok, kind)
	}

	invalidAlternate := "https://example.com/path:$bad:pw:tail"
	for _, mode := range []bool{false, true} {
		_, _, _, _, ok, kind := parseWithAmbiguity(invalidAlternate, mode)
		if !ok || kind != AmbiguityNone {
			t.Errorf("mode loose=%v invalid alternate result: accepted %v ambiguity %v, want accepted unique", mode, ok, kind)
		}
	}
}
