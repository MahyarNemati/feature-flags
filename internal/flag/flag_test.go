package flag_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/MahyarNemati/feature-flags/internal/flag"
)

func valid() *flag.Flag {
	return &flag.Flag{
		Key: "new-checkout", Kind: flag.KindBool, Enabled: true,
		Variations: []flag.Variation{
			{Key: "on", Value: true},
			{Key: "off", Value: false},
		},
		DefaultVariation: "off",
		OffVariation:     "off",
	}
}

func TestValidFlagPasses(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestKeyRules(t *testing.T) {
	for _, tc := range []struct {
		key string
		ok  bool
	}{
		{"new-checkout", true},
		{"a1", true},
		{"my.flag_name-2", true},
		{"", false},
		{"a", false},         // too short
		{"-leading", false},  // must start alphanumeric
		{"trailing-", false}, // must end alphanumeric
		{"Upper", false},     // lowercase only
		{"has space", false},
		{"has/slash", false},
		{strings.Repeat("a", 200), false},
	} {
		f := valid()
		f.Key = tc.key
		err := f.Validate()
		if tc.ok && err != nil {
			t.Errorf("key %q should be valid: %v", tc.key, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("key %q should be rejected", tc.key)
		}
	}
}

func TestVariationValueMustMatchKind(t *testing.T) {
	f := valid()
	f.Kind = flag.KindString
	// Values are still bools.
	if err := f.Validate(); err == nil {
		t.Error("a bool value under a string flag should be rejected")
	}
}

func TestIntKindAcceptsWholeFloats(t *testing.T) {
	// JSON decodes numbers as float64, so a whole float must be accepted or
	// every int flag loaded from JSON would fail validation.
	f := &flag.Flag{
		Key: "limit", Kind: flag.KindInt, Enabled: true,
		Variations:       []flag.Variation{{Key: "low", Value: float64(10)}},
		DefaultVariation: "low", OffVariation: "low",
	}
	if err := f.Validate(); err != nil {
		t.Errorf("whole float should be a valid int variation: %v", err)
	}

	f.Variations[0].Value = 10.5
	if err := f.Validate(); err == nil {
		t.Error("a fractional value should be rejected for an int flag")
	}
}

func TestFallbackVariationsMustExist(t *testing.T) {
	for _, field := range []string{"default", "off"} {
		f := valid()
		if field == "default" {
			f.DefaultVariation = "nope"
		} else {
			f.OffVariation = "nope"
		}
		err := f.Validate()
		if err == nil {
			t.Fatalf("%s_variation pointing at a missing variation should be rejected", field)
		}
		if !errors.Is(err, flag.ErrInvalid) {
			t.Errorf("expected ErrInvalid, got %v", err)
		}
	}
}

func TestDuplicateVariationKeyRejected(t *testing.T) {
	f := valid()
	f.Variations = append(f.Variations, flag.Variation{Key: "on", Value: true})
	if err := f.Validate(); err == nil {
		t.Error("duplicate variation keys should be rejected")
	}
}

func TestRuleMustHaveConditions(t *testing.T) {
	f := valid()
	f.Rules = []flag.Rule{{ID: "empty", Variation: "on"}}
	err := f.Validate()
	if err == nil {
		t.Fatal("a rule with no conditions should be rejected")
	}
	// The message should point at the right alternative.
	if !strings.Contains(err.Error(), "default_variation") {
		t.Errorf("error should suggest default_variation, got %v", err)
	}
}

func TestRuleVariationMustBeDeclared(t *testing.T) {
	f := valid()
	f.Rules = []flag.Rule{{
		ID:         "r",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}},
		Variation:  "undeclared",
	}}
	if err := f.Validate(); err == nil {
		t.Error("a rule serving an undeclared variation should be rejected")
	}
}

func TestDuplicateRuleIDRejected(t *testing.T) {
	cond := []flag.Condition{{Attribute: "key", Operator: flag.OpExists}}
	f := valid()
	f.Rules = []flag.Rule{
		{ID: "same", Conditions: cond, Variation: "on"},
		{ID: "same", Conditions: cond, Variation: "off"},
	}
	if err := f.Validate(); err == nil {
		t.Error("duplicate rule IDs should be rejected: the id is the rollout salt")
	}
}

func TestRolloutWeightsMustSumToExactlyOneHundredPercent(t *testing.T) {
	cond := []flag.Condition{{Attribute: "key", Operator: flag.OpExists}}
	for _, tc := range []struct {
		name    string
		weights map[string]int
		ok      bool
	}{
		{"exact", map[string]int{"on": 2500, "off": 7500}, true},
		{"under", map[string]int{"on": 2500, "off": 7000}, false},
		{"over", map[string]int{"on": 5000, "off": 6000}, false},
		{"empty", map[string]int{}, false},
		{"negative", map[string]int{"on": -1000, "off": 11000}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := valid()
			f.Rules = []flag.Rule{{ID: "r", Conditions: cond, Rollout: &flag.Rollout{Weights: tc.weights}}}
			err := f.Validate()
			if tc.ok && err != nil {
				t.Errorf("should be valid: %v", err)
			}
			// Anything but an exact total silently drops or duplicates
			// traffic, and the arithmetic error is invisible in production.
			if !tc.ok && err == nil {
				t.Error("should be rejected")
			}
		})
	}
}

func TestRolloutVariationsMustBeDeclared(t *testing.T) {
	f := valid()
	f.Rules = []flag.Rule{{
		ID:         "r",
		Conditions: []flag.Condition{{Attribute: "key", Operator: flag.OpExists}},
		Rollout:    &flag.Rollout{Weights: map[string]int{"on": 5000, "ghost": 5000}},
	}}
	if err := f.Validate(); err == nil {
		t.Error("a rollout referencing an undeclared variation should be rejected")
	}
}

func TestInvalidRegexIsRejectedAtDefinitionTime(t *testing.T) {
	// Catching this when the flag is saved is far better than discovering it
	// during evaluation, on a request.
	f := valid()
	f.Rules = []flag.Rule{{
		ID:         "r",
		Conditions: []flag.Condition{{Attribute: "e", Operator: flag.OpRegexMatches, Values: []string{"([unclosed"}}},
		Variation:  "on",
	}}
	if err := f.Validate(); err == nil {
		t.Error("an invalid regex should be rejected at definition time")
	}
}

func TestUnknownOperatorRejected(t *testing.T) {
	f := valid()
	f.Rules = []flag.Rule{{
		ID:         "r",
		Conditions: []flag.Condition{{Attribute: "x", Operator: "wiggles", Values: []string{"y"}}},
		Variation:  "on",
	}}
	if err := f.Validate(); err == nil {
		t.Error("an unknown operator should be rejected")
	}
}

func TestOperatorNeedingValuesRejectsEmpty(t *testing.T) {
	f := valid()
	f.Rules = []flag.Rule{{
		ID:         "r",
		Conditions: []flag.Condition{{Attribute: "x", Operator: flag.OpEquals}},
		Variation:  "on",
	}}
	if err := f.Validate(); err == nil {
		t.Error("eq with no values should be rejected")
	}
}

func TestExistsNeedsNoValues(t *testing.T) {
	f := valid()
	f.Rules = []flag.Rule{{
		ID:         "r",
		Conditions: []flag.Condition{{Attribute: "x", Operator: flag.OpExists}},
		Variation:  "on",
	}}
	if err := f.Validate(); err != nil {
		t.Errorf("exists should need no values: %v", err)
	}
}
