// Package flag defines the flag model: what a flag is, what rules it can
// carry, and how a rollout is expressed.
package flag

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Kind is a flag's value type. Keeping this explicit means the evaluation API
// can return a typed value rather than making every caller parse a string.
type Kind string

// Flag kinds.
const (
	KindBool   Kind = "bool"
	KindString Kind = "string"
	KindInt    Kind = "int"
	KindJSON   Kind = "json"
)

// Kinds lists every supported kind.
var Kinds = []Kind{KindBool, KindString, KindInt, KindJSON}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	for _, c := range Kinds {
		if c == k {
			return true
		}
	}
	return false
}

// Operator is a targeting comparison.
type Operator string

// Targeting operators.
const (
	OpEquals       Operator = "eq"
	OpNotEquals    Operator = "neq"
	OpIn           Operator = "in"
	OpNotIn        Operator = "not_in"
	OpContains     Operator = "contains"
	OpStartsWith   Operator = "starts_with"
	OpEndsWith     Operator = "ends_with"
	OpGreaterThan  Operator = "gt"
	OpLessThan     Operator = "lt"
	OpSemverGTE    Operator = "semver_gte"
	OpSemverLT     Operator = "semver_lt"
	OpRegexMatches Operator = "regex"
	OpExists       Operator = "exists"
)

// Operators lists every supported operator.
var Operators = []Operator{
	OpEquals, OpNotEquals, OpIn, OpNotIn, OpContains, OpStartsWith, OpEndsWith,
	OpGreaterThan, OpLessThan, OpSemverGTE, OpSemverLT, OpRegexMatches, OpExists,
}

// Valid reports whether o is a known operator.
func (o Operator) Valid() bool {
	for _, c := range Operators {
		if c == o {
			return true
		}
	}
	return false
}

// Condition is one attribute comparison.
type Condition struct {
	// Attribute is a key from the evaluation context, e.g. "email" or "plan".
	Attribute string   `json:"attribute"`
	Operator  Operator `json:"operator"`
	// Values holds the comparison operands. Single-operand operators use the
	// first element.
	Values []string `json:"values,omitempty"`
	// Negate inverts the result, so one operator covers both directions
	// without doubling the operator list.
	Negate bool `json:"negate,omitempty"`
}

// Rule is a targeting rule: when every condition matches, the rule applies.
type Rule struct {
	ID          string      `json:"id"`
	Description string      `json:"description,omitempty"`
	Conditions  []Condition `json:"conditions"`
	// Variation is the variation key served when the rule matches and
	// Rollout is nil.
	Variation string `json:"variation,omitempty"`
	// Rollout splits matching traffic across variations by weight. When set,
	// Variation is ignored.
	Rollout *Rollout `json:"rollout,omitempty"`
}

// Rollout distributes traffic across variations.
type Rollout struct {
	// Weights maps a variation key to a weight in basis points (1/100th of a
	// percent), so 10% is 1000 and the total must be 10000.
	//
	// Basis points rather than percentages because "5.5% of users" is a real
	// request and an integer percentage cannot express it. Integers rather
	// than floats because a rollout must be reproducible across languages,
	// and float arithmetic is not.
	Weights map[string]int `json:"weights"`
	// BucketBy names the context attribute used to bucket a subject.
	// Defaults to the targeting key. Setting it to e.g. "accountId" keeps a
	// whole account on the same side of a rollout.
	BucketBy string `json:"bucket_by,omitempty"`
}

// Total returns the sum of the weights.
func (r *Rollout) Total() int {
	total := 0
	for _, w := range r.Weights {
		total += w
	}
	return total
}

// BasisPointsTotal is the required weight total: 100% in basis points.
const BasisPointsTotal = 10000

// Variation is one possible value of a flag.
type Variation struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
	// Description is shown in the dashboard.
	Description string `json:"description,omitempty"`
}

// Flag is a complete flag definition.
type Flag struct {
	Key         string `json:"key"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Kind        Kind   `json:"kind"`
	// Enabled false short-circuits to OffVariation, regardless of rules. This
	// is the kill switch: one field to turn a feature off without unpicking
	// its targeting.
	Enabled    bool        `json:"enabled"`
	Variations []Variation `json:"variations"`
	// Rules are evaluated in order; the first match wins.
	Rules []Rule `json:"rules,omitempty"`
	// DefaultVariation is served when the flag is enabled and no rule matches.
	DefaultVariation string `json:"default_variation"`
	// OffVariation is served when the flag is disabled.
	OffVariation string `json:"off_variation"`
	// Version increments on every change, so a client can tell whether its
	// cached copy is stale.
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Tags are free-form labels for organising a large flag set.
	Tags []string `json:"tags,omitempty"`
	// Archived flags are hidden from the dashboard but still evaluate, so
	// archiving cannot break production.
	Archived bool `json:"archived,omitempty"`
}

var keyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,126}[a-z0-9]$`)

// ErrInvalid is returned when a flag definition is not usable.
var ErrInvalid = errors.New("flag: invalid definition")

// Validate checks a flag definition thoroughly. Every error it can catch here
// is an error that would otherwise be a wrong value served to a user.
func (f *Flag) Validate() error {
	if !keyPattern.MatchString(f.Key) {
		return fmt.Errorf("%w: key %q must be 2-128 chars of [a-z0-9._-], starting and ending alphanumeric", ErrInvalid, f.Key)
	}
	if !f.Kind.Valid() {
		return fmt.Errorf("%w: unknown kind %q", ErrInvalid, f.Kind)
	}
	if len(f.Variations) < 1 {
		return fmt.Errorf("%w: flag %q needs at least one variation", ErrInvalid, f.Key)
	}

	seen := map[string]bool{}
	for i, v := range f.Variations {
		if v.Key == "" {
			return fmt.Errorf("%w: flag %q variation %d has no key", ErrInvalid, f.Key, i)
		}
		if seen[v.Key] {
			return fmt.Errorf("%w: flag %q has duplicate variation %q", ErrInvalid, f.Key, v.Key)
		}
		seen[v.Key] = true
		if err := checkKind(f.Kind, v.Value); err != nil {
			return fmt.Errorf("%w: flag %q variation %q: %v", ErrInvalid, f.Key, v.Key, err)
		}
	}

	// Both fallbacks must exist, or a disabled flag has nothing to serve.
	for name, key := range map[string]string{
		"default_variation": f.DefaultVariation,
		"off_variation":     f.OffVariation,
	} {
		if key == "" {
			return fmt.Errorf("%w: flag %q needs %s", ErrInvalid, f.Key, name)
		}
		if !seen[key] {
			return fmt.Errorf("%w: flag %q %s %q is not a declared variation", ErrInvalid, f.Key, name, key)
		}
	}

	ruleIDs := map[string]bool{}
	for i, r := range f.Rules {
		if r.ID == "" {
			return fmt.Errorf("%w: flag %q rule %d has no id", ErrInvalid, f.Key, i)
		}
		if ruleIDs[r.ID] {
			return fmt.Errorf("%w: flag %q has duplicate rule id %q", ErrInvalid, f.Key, r.ID)
		}
		ruleIDs[r.ID] = true

		if len(r.Conditions) == 0 {
			return fmt.Errorf("%w: flag %q rule %q has no conditions; use default_variation instead", ErrInvalid, f.Key, r.ID)
		}
		for j, c := range r.Conditions {
			if err := c.validate(); err != nil {
				return fmt.Errorf("%w: flag %q rule %q condition %d: %v", ErrInvalid, f.Key, r.ID, j, err)
			}
		}

		switch {
		case r.Rollout != nil:
			if err := r.Rollout.validate(seen); err != nil {
				return fmt.Errorf("%w: flag %q rule %q: %v", ErrInvalid, f.Key, r.ID, err)
			}
		case r.Variation == "":
			return fmt.Errorf("%w: flag %q rule %q needs a variation or a rollout", ErrInvalid, f.Key, r.ID)
		case !seen[r.Variation]:
			return fmt.Errorf("%w: flag %q rule %q serves undeclared variation %q", ErrInvalid, f.Key, r.ID, r.Variation)
		}
	}
	return nil
}

func (c Condition) validate() error {
	if c.Attribute == "" {
		return errors.New("attribute is required")
	}
	if !c.Operator.Valid() {
		return fmt.Errorf("unknown operator %q", c.Operator)
	}
	// exists takes no operand; everything else takes at least one.
	if c.Operator == OpExists {
		return nil
	}
	if len(c.Values) == 0 {
		return fmt.Errorf("operator %q needs at least one value", c.Operator)
	}
	if c.Operator == OpRegexMatches {
		if _, err := regexp.Compile(c.Values[0]); err != nil {
			return fmt.Errorf("invalid regex %q: %v", c.Values[0], err)
		}
	}
	return nil
}

func (r *Rollout) validate(declared map[string]bool) error {
	if len(r.Weights) == 0 {
		return errors.New("rollout has no weights")
	}
	for key, w := range r.Weights {
		if !declared[key] {
			return fmt.Errorf("rollout references undeclared variation %q", key)
		}
		if w < 0 {
			return fmt.Errorf("rollout weight for %q is negative", key)
		}
	}
	// Requiring an exact total is deliberate. Accepting anything else means
	// silently dropping or duplicating traffic, and the arithmetic error is
	// invisible in production.
	if total := r.Total(); total != BasisPointsTotal {
		return fmt.Errorf("rollout weights sum to %d basis points, want exactly %d (100%%)", total, BasisPointsTotal)
	}
	return nil
}

// checkKind verifies a variation's value matches the flag's declared kind.
func checkKind(k Kind, v any) error {
	switch k {
	case KindBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("value %v is not a bool", v)
		}
	case KindString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("value %v is not a string", v)
		}
	case KindInt:
		// JSON numbers decode as float64, so accept a whole float too.
		switch n := v.(type) {
		case int, int32, int64:
		case float64:
			if n != float64(int64(n)) {
				return fmt.Errorf("value %v is not a whole number", v)
			}
		default:
			return fmt.Errorf("value %v is not an int", v)
		}
	case KindJSON:
		if v == nil {
			return errors.New("json variation value must not be null")
		}
	}
	return nil
}

// Variation returns the variation with the given key.
func (f *Flag) Variation(key string) (Variation, bool) {
	for _, v := range f.Variations {
		if v.Key == key {
			return v, true
		}
	}
	return Variation{}, false
}
