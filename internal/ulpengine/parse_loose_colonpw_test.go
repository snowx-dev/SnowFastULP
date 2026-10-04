package ulpengine

import (
	"testing"
)

// TestLooseKeepsStrictParseableColonPasswords pins the A3 fix: everything the
// strict parser stores, -loose must store too. Several isLikelyJunk patterns
// (`":"`, `:target=`, ...) match passwords inside perfectly parseable
// credentials; the junk filter must only bound the loose-only extras, never
// veto a strict parse. A real-world shape: two path-variant records whose
// password contains a colon-quote — strict stores 1 deduped entry, so loose
// must too.
func TestLooseKeepsStrictParseableColonPasswords(t *testing.T) {
	lines := []string{
		`facebook.com/profile.php?id=100:alice:pa":"ss`,
		`facebook.com:alice:pa":"ss`,
		`site.com:bob:LegacyGeneric:target=cred`,
		`site.com:carol:x:PasswordText y`,
	}
	for _, line := range lines {
		sh, _, sl, sp, sok := parse(line)
		if !sok {
			t.Fatalf("strict rejected %q — fixture premise broken", line)
		}
		lh, _, ll, lp, lok := parseLoose(line)
		if !lok {
			t.Fatalf("loose dropped a strict-parseable credential: %q", line)
		}
		if lh != sh || ll != sl || lp != sp {
			t.Fatalf("loose fields diverge from strict for %q:\nstrict = (%q,%q,%q)\nloose  = (%q,%q,%q)",
				line, sh, sl, sp, lh, ll, lp)
		}
	}
}

// TestLooseStillRejectsJunk pins the loose garbage rejection that must not
// regress now that the junk filter runs after the strict attempt: junk-shaped
// lines that no parser accepts stay rejected.
func TestLooseStillRejectsJunk(t *testing.T) {
	junk := []string{
		`{"url":"https://x.com","login":"a","password":"b"}`,
		`"x":"y"`,
		`LegacyGeneric:target=site.com`,
		`{"session":"abc","exp":"y"}`,
	}
	for _, line := range junk {
		if _, _, _, _, ok := parse(line); ok {
			t.Fatalf("strict unexpectedly parsed junk %q — pick a line strict rejects", line)
		}
		if _, _, _, _, ok := parseLoose(line); ok {
			t.Fatalf("loose admitted junk %q", line)
		}
	}
}
