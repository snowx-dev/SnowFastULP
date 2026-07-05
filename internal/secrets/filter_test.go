package secrets

import "testing"

func TestRuleFilterEmptyKeepsAll(t *testing.T) {
	f := RuleFilter{}
	if !f.Empty() {
		t.Fatal("RuleFilter{} should be empty")
	}
	for _, id := range []string{"np.aws.1", "np.slack.2", "anything"} {
		if !f.Keep(id) {
			t.Fatalf("empty filter dropped %q", id)
		}
	}
}

func TestRuleFilterAllowRestricts(t *testing.T) {
	f := RuleFilter{Allow: []string{"np.aws.*"}}
	if f.Empty() {
		t.Fatal("allow filter should not be empty")
	}
	if !f.Keep("np.aws.1") {
		t.Fatal("np.aws.* should keep np.aws.1")
	}
	if f.Keep("np.slack.2") {
		t.Fatal("np.aws.* should drop np.slack.2")
	}
}

func TestRuleFilterDenyRemoves(t *testing.T) {
	f := RuleFilter{Deny: []string{"np.aws.3"}}
	if !f.Keep("np.aws.1") {
		t.Fatal("deny np.aws.3 should keep np.aws.1")
	}
	if f.Keep("np.aws.3") {
		t.Fatal("deny np.aws.3 should drop np.aws.3")
	}
}

func TestRuleFilterDenyWinsOverAllow(t *testing.T) {
	f := RuleFilter{Allow: []string{"np.aws.*"}, Deny: []string{"np.aws.3"}}
	if !f.Keep("np.aws.1") {
		t.Fatal("aws.1 should survive allow+deny")
	}
	if f.Keep("np.aws.3") {
		t.Fatal("deny must win over allow for np.aws.3")
	}
	if f.Keep("np.slack.2") {
		t.Fatal("non-aws should be dropped by allow")
	}
}

func TestRuleFilterGlobQuestionAndClass(t *testing.T) {
	f := RuleFilter{Allow: []string{"np.?ws.1"}}
	if !f.Keep("np.aws.1") {
		t.Fatal("np.?ws.1 should match np.aws.1")
	}
	if f.Keep("np.aws.12") {
		t.Fatal("np.?ws.1 should not match np.aws.12")
	}
}

func TestRuleFilterValidateRejectsBadGlob(t *testing.T) {
	f := RuleFilter{Allow: []string{"np.aws.["}}
	if err := f.Validate(); err == nil {
		t.Fatal("Validate should reject unterminated character class")
	}
}

func TestRuleFilterValidateAcceptsGoodGlobs(t *testing.T) {
	f := RuleFilter{Allow: []string{"np.aws.*", "np.slack.?"}, Deny: []string{"np.aws.3"}}
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate rejected valid globs: %v", err)
	}
}
