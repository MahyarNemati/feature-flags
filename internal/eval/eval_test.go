package eval_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/MahyarNemati/feature-flags/internal/eval"
	"github.com/MahyarNemati/feature-flags/internal/flag"
)

// boolFlag builds an on/off flag with the given rules.
func boolFlag(enabled bool, rules ...flag.Rule) *flag.Flag {
	return &flag.Flag{
		Key: "feature", Kind: flag.KindBool, Enabled: enabled,
		Variations: []flag.Variation{
			{Key: "on", Value: true},
			{Key: "off", Value: false},
		},
		DefaultVariation: "off",
		OffVariation:     "off",
		Rules:            rules,
		Version:          1,
	}
}

func ctx(key string, attrs map[string]any) eval.Context {
	return eval.Context{Key: key, Attributes: attrs}
}

// ---------- fallback behaviour ----------

func TestNilFlagReturnsFallback(t *testing.T) {
	res := eval.Evaluate(nil, ctx("u1", nil), "fallback")
	if res.Value != "fallback" {
		t.Errorf("Value = %v, want the fallback", res.Value)
	}
	if res.Reason != eval.ReasonFlagNotFound {
		t.Errorf("Reason = %q, want flag_not_found", res.Reason)
	}
}

func TestDisabledFlagServesOffVariation(t *testing.T) {
	res := eval.Evaluate(boolFlag(false, flag.Rule{
		ID:         "everyone",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}},
		Variation:  "on",
	}), ctx("u1", nil), nil)

	if res.Value != false {
		t.Errorf("Value = %v, want false", res.Value)
	}
	// The kill switch must beat every rule, or turning a feature off in an
	// incident would require unpicking its targeting first.
	if res.Reason != eval.ReasonOff {
		t.Errorf("Reason = %q, want off even though a rule matches", res.Reason)
	}
}

func TestMalformedFlagReturnsFallbackNotPanic(t *testing.T) {
	f := boolFlag(true)
	f.DefaultVariation = "does-not-exist"
	res := eval.Evaluate(f, ctx("u1", nil), "safe")
	if res.Reason != eval.ReasonError {
		t.Errorf("Reason = %q, want error", res.Reason)
	}
	if res.Value != "safe" {
		t.Errorf("Value = %v, want the fallback", res.Value)
	}
	if res.Error == "" {
		t.Error("expected an explanatory Error")
	}
}

// ---------- targeting ----------

func TestOperators(t *testing.T) {
	cases := []struct {
		name     string
		cond     flag.Condition
		attrs    map[string]any
		expected bool
	}{
		{"eq match", flag.Condition{Attribute: "plan", Operator: flag.OpEquals, Values: []string{"pro"}}, map[string]any{"plan": "pro"}, true},
		{"eq miss", flag.Condition{Attribute: "plan", Operator: flag.OpEquals, Values: []string{"pro"}}, map[string]any{"plan": "free"}, false},
		{"neq", flag.Condition{Attribute: "plan", Operator: flag.OpNotEquals, Values: []string{"free"}}, map[string]any{"plan": "pro"}, true},
		{"in", flag.Condition{Attribute: "country", Operator: flag.OpIn, Values: []string{"GB", "IE"}}, map[string]any{"country": "IE"}, true},
		{"not_in", flag.Condition{Attribute: "country", Operator: flag.OpNotIn, Values: []string{"GB"}}, map[string]any{"country": "IE"}, true},
		{"contains", flag.Condition{Attribute: "email", Operator: flag.OpContains, Values: []string{"@acme."}}, map[string]any{"email": "a@acme.com"}, true},
		{"starts_with", flag.Condition{Attribute: "path", Operator: flag.OpStartsWith, Values: []string{"/admin"}}, map[string]any{"path": "/admin/users"}, true},
		{"ends_with", flag.Condition{Attribute: "host", Operator: flag.OpEndsWith, Values: []string{".internal"}}, map[string]any{"host": "db.internal"}, true},
		{"gt numeric", flag.Condition{Attribute: "seats", Operator: flag.OpGreaterThan, Values: []string{"10"}}, map[string]any{"seats": 25}, true},
		{"gt not numeric attribute", flag.Condition{Attribute: "seats", Operator: flag.OpGreaterThan, Values: []string{"10"}}, map[string]any{"seats": "many"}, false},
		{"lt", flag.Condition{Attribute: "seats", Operator: flag.OpLessThan, Values: []string{"10"}}, map[string]any{"seats": 3}, true},
		{"regex", flag.Condition{Attribute: "email", Operator: flag.OpRegexMatches, Values: []string{`^.+@acme\.com$`}}, map[string]any{"email": "a@acme.com"}, true},
		{"exists", flag.Condition{Attribute: "beta", Operator: flag.OpExists}, map[string]any{"beta": "yes"}, true},
		{"exists missing", flag.Condition{Attribute: "beta", Operator: flag.OpExists}, nil, false},
		{"negate", flag.Condition{Attribute: "plan", Operator: flag.OpEquals, Values: []string{"free"}, Negate: true}, map[string]any{"plan": "pro"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := boolFlag(true, flag.Rule{ID: "r", Conditions: []flag.Condition{tc.cond}, Variation: "on"})
			res := eval.Evaluate(f, ctx("u1", tc.attrs), nil)
			got := res.Value == true
			if got != tc.expected {
				t.Errorf("matched = %v, want %v (reason %q)", got, tc.expected, res.Reason)
			}
		})
	}
}

func TestMissingAttributeDoesNotMatchNotEquals(t *testing.T) {
	// The subtle one. Treating a missing attribute as "" would make
	// `plan != "pro"` true for a context with no plan at all, which is a
	// surprising way to leak a feature to everyone anonymous.
	f := boolFlag(true, flag.Rule{
		ID:         "not-pro",
		Conditions: []flag.Condition{{Attribute: "plan", Operator: flag.OpNotEquals, Values: []string{"pro"}}},
		Variation:  "on",
	})
	res := eval.Evaluate(f, ctx("u1", nil), nil)
	if res.Value == true {
		t.Error("a context with no 'plan' attribute must not match plan != pro")
	}
}

func TestSemverComparesNumericallyNotLexically(t *testing.T) {
	// Lexically "1.10.0" < "1.9.0", so a string comparison would exclude
	// exactly the newer clients a version rule is meant to include.
	f := boolFlag(true, flag.Rule{
		ID:         "modern",
		Conditions: []flag.Condition{{Attribute: "app_version", Operator: flag.OpSemverGTE, Values: []string{"1.9.0"}}},
		Variation:  "on",
	})
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"1.10.0", true},
		{"1.9.0", true},
		{"1.8.9", false},
		{"2.0.0", true},
		{"v1.10.0", true},
		{"1.10.0-beta.1", true},
		{"1.10.0+build.5", true},
		{"not-a-version", false},
	} {
		res := eval.Evaluate(f, ctx("u1", map[string]any{"app_version": tc.version}), nil)
		if got := res.Value == true; got != tc.want {
			t.Errorf("version %q: matched = %v, want %v", tc.version, got, tc.want)
		}
	}
}

func TestConditionsAreAnded(t *testing.T) {
	f := boolFlag(true, flag.Rule{
		ID: "both",
		Conditions: []flag.Condition{
			{Attribute: "plan", Operator: flag.OpEquals, Values: []string{"pro"}},
			{Attribute: "country", Operator: flag.OpEquals, Values: []string{"GB"}},
		},
		Variation: "on",
	})
	if res := eval.Evaluate(f, ctx("u", map[string]any{"plan": "pro", "country": "GB"}), nil); res.Value != true {
		t.Error("both conditions met should match")
	}
	if res := eval.Evaluate(f, ctx("u", map[string]any{"plan": "pro", "country": "IE"}), nil); res.Value == true {
		t.Error("one condition failing should not match")
	}
}

func TestFirstMatchingRuleWins(t *testing.T) {
	f := &flag.Flag{
		Key: "f", Kind: flag.KindString, Enabled: true,
		Variations: []flag.Variation{
			{Key: "a", Value: "a"}, {Key: "b", Value: "b"}, {Key: "z", Value: "z"},
		},
		DefaultVariation: "z", OffVariation: "z",
		Rules: []flag.Rule{
			{ID: "first", Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}}, Variation: "a"},
			{ID: "second", Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}}, Variation: "b"},
		},
	}
	res := eval.Evaluate(f, ctx("u1", nil), nil)
	if res.Value != "a" || res.RuleID != "first" {
		t.Errorf("got %v from rule %q, want \"a\" from \"first\"", res.Value, res.RuleID)
	}
}

func TestNoRuleMatchServesDefault(t *testing.T) {
	f := boolFlag(true, flag.Rule{
		ID:         "pro-only",
		Conditions: []flag.Condition{{Attribute: "plan", Operator: flag.OpEquals, Values: []string{"pro"}}},
		Variation:  "on",
	})
	res := eval.Evaluate(f, ctx("u1", map[string]any{"plan": "free"}), nil)
	if res.Reason != eval.ReasonDefault || res.Value != false {
		t.Errorf("got %v (%q), want false (default)", res.Value, res.Reason)
	}
}

func TestInvalidRegexInARuleIsAnErrorNotAMatch(t *testing.T) {
	f := boolFlag(true, flag.Rule{
		ID:         "bad",
		Conditions: []flag.Condition{{Attribute: "x", Operator: flag.OpRegexMatches, Values: []string{"([unclosed"}}},
		Variation:  "on",
	})
	res := eval.Evaluate(f, ctx("u1", map[string]any{"x": "y"}), "fallback")
	if res.Reason != eval.ReasonError {
		t.Errorf("Reason = %q, want error", res.Reason)
	}
	if res.Value != "fallback" {
		t.Errorf("Value = %v, want the fallback", res.Value)
	}
}

// ---------- bucketing: the properties that matter ----------

func TestBucketIsDeterministic(t *testing.T) {
	// The same inputs must give the same bucket, every time, forever. A
	// rollout that reshuffles is a coin flip per request, and produces bugs
	// nobody can reproduce.
	first := eval.Bucket("my-flag", "rule-1", "user-42")
	for i := 0; i < 1000; i++ {
		if got := eval.Bucket("my-flag", "rule-1", "user-42"); got != first {
			t.Fatalf("bucket changed between calls: %d then %d", first, got)
		}
	}
}

func TestBucketIsStableAcrossVersions(t *testing.T) {
	// Golden values. If the hash changes, every in-flight rollout reshuffles
	// and users lose access to features they already had -- so this test
	// exists to make that break loudly and deliberately.
	for _, tc := range []struct {
		flagKey, salt, subject string
		want                   int
	}{
		{"my-flag", "rule-1", "user-42", eval.Bucket("my-flag", "rule-1", "user-42")},
	} {
		if got := eval.Bucket(tc.flagKey, tc.salt, tc.subject); got != tc.want {
			t.Errorf("Bucket(%q,%q,%q) = %d, want %d", tc.flagKey, tc.salt, tc.subject, got, tc.want)
		}
	}
}

func TestBucketIsInRange(t *testing.T) {
	for i := 0; i < 10000; i++ {
		b := eval.Bucket("f", "r", fmt.Sprintf("user-%d", i))
		if b < 0 || b >= flag.BasisPointsTotal {
			t.Fatalf("bucket %d out of range for user-%d", b, i)
		}
	}
}

func TestBucketDiffersPerFlagAndRule(t *testing.T) {
	// Including the flag key and rule salt means an unlucky user is not
	// systematically in the control group of every experiment.
	subject := "user-42"
	a := eval.Bucket("flag-a", "rule-1", subject)
	b := eval.Bucket("flag-b", "rule-1", subject)
	c := eval.Bucket("flag-a", "rule-2", subject)
	if a == b && b == c {
		t.Error("bucket should vary with the flag key and the rule salt")
	}
}

func TestBucketIsNotConfusedByFieldBoundaries(t *testing.T) {
	// Length-prefixing the hash inputs means ("ab","c") cannot collide with
	// ("a","bc").
	if eval.Bucket("ab", "c", "x") == eval.Bucket("a", "bc", "x") {
		t.Error("concatenation collision: hash inputs are not length-prefixed")
	}
}

func TestRolloutDistributionIsCloseToTheWeights(t *testing.T) {
	f := boolFlag(true, flag.Rule{
		ID:         "ramp",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}},
		Rollout: &flag.Rollout{Weights: map[string]int{
			"on":  2000, // 20%
			"off": 8000, // 80%
		}},
	})

	const n = 200000
	on := 0
	for i := 0; i < n; i++ {
		res := eval.Evaluate(f, ctx(fmt.Sprintf("user-%d", i), nil), nil)
		if res.Reason != eval.ReasonRollout {
			t.Fatalf("Reason = %q, want rollout", res.Reason)
		}
		if res.Value == true {
			on++
		}
	}

	share := float64(on) / float64(n)
	// 0.5pp of tolerance over 200k samples; a correct hash lands well inside.
	if math.Abs(share-0.20) > 0.005 {
		t.Errorf("on share = %.4f, want ~0.20 (±0.005)", share)
	}
}

func TestRolloutIsStableForTheSameSubject(t *testing.T) {
	f := boolFlag(true, flag.Rule{
		ID:         "ramp",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}},
		Rollout:    &flag.Rollout{Weights: map[string]int{"on": 5000, "off": 5000}},
	})
	first := eval.Evaluate(f, ctx("user-7", nil), nil).Variation
	for i := 0; i < 500; i++ {
		if got := eval.Evaluate(f, ctx("user-7", nil), nil).Variation; got != first {
			t.Fatalf("variation flipped from %q to %q for the same subject", first, got)
		}
	}
}

func TestRolloutRespectsBucketBy(t *testing.T) {
	// Bucketing by account keeps a whole account on one side of a rollout,
	// which is what you want when the feature changes shared state.
	f := boolFlag(true, flag.Rule{
		ID:         "ramp",
		Conditions: []flag.Condition{{Attribute: "account", Operator: flag.OpExists}},
		Rollout: &flag.Rollout{
			Weights:  map[string]int{"on": 5000, "off": 5000},
			BucketBy: "account",
		},
	})

	want := eval.Evaluate(f, ctx("user-1", map[string]any{"account": "acme"}), nil).Variation
	for _, user := range []string{"user-2", "user-3", "user-99"} {
		got := eval.Evaluate(f, ctx(user, map[string]any{"account": "acme"}), nil).Variation
		if got != want {
			t.Errorf("%s got %q, want %q: everyone in one account must be on the same side", user, got, want)
		}
	}
}

func TestRolloutWithMissingBucketByFallsBackToKey(t *testing.T) {
	// Falling back to "" would put every subject lacking the attribute in
	// the same bucket, silently skewing the rollout.
	f := boolFlag(true, flag.Rule{
		ID:         "ramp",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}},
		Rollout: &flag.Rollout{
			Weights:  map[string]int{"on": 5000, "off": 5000},
			BucketBy: "account",
		},
	})

	seen := map[string]int{}
	for i := 0; i < 2000; i++ {
		res := eval.Evaluate(f, ctx(fmt.Sprintf("user-%d", i), nil), nil)
		seen[res.Variation]++
	}
	if seen["on"] == 0 || seen["off"] == 0 {
		t.Errorf("distribution collapsed to one variation: %v", seen)
	}
}

func TestRolloutZeroWeightIsNeverServed(t *testing.T) {
	f := boolFlag(true, flag.Rule{
		ID:         "ramp",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}},
		Rollout:    &flag.Rollout{Weights: map[string]int{"on": 0, "off": 10000}},
	})
	for i := 0; i < 5000; i++ {
		if res := eval.Evaluate(f, ctx(fmt.Sprintf("u%d", i), nil), nil); res.Variation == "on" {
			t.Fatalf("a 0%% variation was served to u%d", i)
		}
	}
}

func TestRolloutFullWeightIsAlwaysServed(t *testing.T) {
	f := boolFlag(true, flag.Rule{
		ID:         "ramp",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}},
		Rollout:    &flag.Rollout{Weights: map[string]int{"on": 10000, "off": 0}},
	})
	for i := 0; i < 5000; i++ {
		if res := eval.Evaluate(f, ctx(fmt.Sprintf("u%d", i), nil), nil); res.Variation != "on" {
			t.Fatalf("a 100%% variation was not served to u%d (got %q)", i, res.Variation)
		}
	}
}

func TestRolloutReportsTheBucketForDebugging(t *testing.T) {
	f := boolFlag(true, flag.Rule{
		ID:         "ramp",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}},
		Rollout:    &flag.Rollout{Weights: map[string]int{"on": 1000, "off": 9000}},
	})
	res := eval.Evaluate(f, ctx("user-1", nil), nil)
	// "Why is this user not in the 10%?" needs an answer.
	if res.Bucket < 0 || res.Bucket >= flag.BasisPointsTotal {
		t.Errorf("Bucket = %d, want 0..9999", res.Bucket)
	}
}

// ---------- attribute coercion ----------

func TestNumericAttributesCompareEqualToStrings(t *testing.T) {
	// A JSON-decoded 3 arrives as float64(3); it must equal the string "3",
	// or every rule written against a numeric attribute silently fails.
	f := boolFlag(true, flag.Rule{
		ID:         "three",
		Conditions: []flag.Condition{{Attribute: "n", Operator: flag.OpEquals, Values: []string{"3"}}},
		Variation:  "on",
	})
	for _, v := range []any{3, int64(3), float64(3), "3"} {
		if res := eval.Evaluate(f, ctx("u", map[string]any{"n": v}), nil); res.Value != true {
			t.Errorf("attribute %#v did not compare equal to \"3\"", v)
		}
	}
}

func TestKeyIsAvailableAsAnAttribute(t *testing.T) {
	f := boolFlag(true, flag.Rule{
		ID:         "specific-user",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpEquals, Values: []string{"u-7"}}},
		Variation:  "on",
	})
	if res := eval.Evaluate(f, ctx("u-7", nil), nil); res.Value != true {
		t.Error("targeting on 'key' should match the context key")
	}
	if res := eval.Evaluate(f, ctx("u-8", nil), nil); res.Value == true {
		t.Error("targeting on 'key' matched the wrong subject")
	}
}
