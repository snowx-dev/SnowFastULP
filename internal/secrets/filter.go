package secrets

import (
	"fmt"
	"path"
	"strings"
)

// RuleFilter is a glob-based whitelist/blacklist over titus rule IDs (e.g.
// "np.aws.1", "np.slack.2"). Allow restricts the set; Deny removes from it and
// wins on conflict. Empty Allow means "allow all"; an entirely empty filter
// preserves the full rule set. Patterns use shell glob syntax (path.Match):
// "*", "?", "[...]". Canonical globs target the rule ID prefix, e.g.
// "np.aws.*" for all AWS rules.
type RuleFilter struct {
	Allow []string
	Deny  []string
}

// Empty reports whether the filter is a no-op (keeps every rule).
func (f RuleFilter) Empty() bool { return len(f.Allow) == 0 && len(f.Deny) == 0 }

// Validate returns an error if any pattern is not a valid glob. Called once at
// pool build so a typo surfaces as a hard error before scanning starts.
func (f RuleFilter) Validate() error {
	for _, p := range f.Allow {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("secrets: invalid allow glob %q: %w", p, err)
		}
	}
	for _, p := range f.Deny {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("secrets: invalid deny glob %q: %w", p, err)
		}
	}
	return nil
}

// Keep reports whether a rule with the given ID survives the filter. Allow is
// applied first (empty = pass), then Deny; a deny match always drops the rule
// even if it also matched an allow pattern.
func (f RuleFilter) Keep(id string) bool {
	if len(f.Allow) > 0 && !matchAnyGlob(id, f.Allow) {
		return false
	}
	if matchAnyGlob(id, f.Deny) {
		return false
	}
	return true
}

func matchAnyGlob(id string, patterns []string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, id); err == nil && ok {
			return true
		}
	}
	return false
}

// String is a compact debugging aid used in debug logs.
func (f RuleFilter) String() string {
	if f.Empty() {
		return "{}"
	}
	return fmt.Sprintf("{allow:[%s] deny:[%s]}", strings.Join(f.Allow, ","), strings.Join(f.Deny, ","))
}
