// Package eval evaluates a flag against a context.
//
// The property that matters most is **determinism**: the same flag and the
// same subject must produce the same variation everywhere, forever, in every
// SDK. A rollout that reshuffles on each evaluation is not a rollout, it is a
// coin flip per request — and a user who sees a feature appear and disappear
// will file a bug you cannot reproduce.
//
// That is achieved by hashing (flagKey, salt, subject) with a fixed algorithm
// and mapping the result onto 10,000 basis points. No randomness, no state,
// no clock.
package eval

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/MahyarNemati/feature-flags/internal/flag"
)

// Context is the subject being evaluated.
type Context struct {
	// Key identifies the subject and is what bucketing hashes by default.
	// Usually a user id. An empty key means an anonymous subject, which can
	// still match rules but is bucketed on the empty string, so every
	// anonymous subject lands in the same bucket.
	Key string `json:"key"`
	// Attributes are matched by targeting conditions.
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Attr returns an attribute as a string, plus whether it was present.
// "key" is exposed as an attribute too, so a rule can target it directly.
func (c Context) Attr(name string) (string, bool) {
	if name == "key" {
		return c.Key, c.Key != ""
	}
	v, ok := c.Attributes[name]
	if !ok {
		return "", false
	}
	return stringify(v), true
}

func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// Render a whole float without a trailing ".0", so a JSON-decoded 3
		// compares equal to the string "3".
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return fmt.Sprint(t)
	}
}

// Reason explains why a variation was served. Returning this is what makes a
// flag system debuggable: "why am I not seeing the feature?" has an answer.
type Reason string

// Evaluation reasons.
const (
	// ReasonOff means the flag is disabled.
	ReasonOff Reason = "off"
	// ReasonRuleMatch means a targeting rule matched.
	ReasonRuleMatch Reason = "rule_match"
	// ReasonRollout means a rule matched and a percentage rollout chose the
	// variation.
	ReasonRollout Reason = "rollout"
	// ReasonDefault means the flag is on and no rule matched.
	ReasonDefault Reason = "default"
	// ReasonFlagNotFound means no such flag; the caller's fallback is used.
	ReasonFlagNotFound Reason = "flag_not_found"
	// ReasonError means the flag is malformed; the caller's fallback is used.
	ReasonError Reason = "error"
)

// Result is one evaluation outcome.
type Result struct {
	FlagKey string `json:"flag_key"`
	// Variation is the chosen variation key, empty on error.
	Variation string `json:"variation,omitempty"`
	Value     any    `json:"value"`
	Reason    Reason `json:"reason"`
	// RuleID names the rule that matched, when one did.
	RuleID string `json:"rule_id,omitempty"`
	// Version is the flag version evaluated, for cache diagnosis.
	Version int `json:"version,omitempty"`
	// Bucket is the subject's position in 0..9999, when a rollout ran. Useful
	// for explaining "why is this user not in the 10%".
	Bucket int `json:"bucket,omitempty"`
	// Error carries the problem when Reason is error.
	Error string `json:"error,omitempty"`
}

// Evaluate resolves a flag for a context. fallback is returned when the flag
// cannot be evaluated, so a caller always gets a usable value.
//
// It never returns an error: a flag system that can fail a request is worse
// than one that serves the default. Problems surface in Reason and Error.
func Evaluate(f *flag.Flag, ctx Context, fallback any) Result {
	if f == nil {
		return Result{Variation: "", Value: fallback, Reason: ReasonFlagNotFound}
	}

	res := Result{FlagKey: f.Key, Version: f.Version}

	if !f.Enabled {
		v, ok := f.Variation(f.OffVariation)
		if !ok {
			res.Reason, res.Error, res.Value = ReasonError, "off_variation not found", fallback
			return res
		}
		res.Variation, res.Value, res.Reason = v.Key, v.Value, ReasonOff
		return res
	}

	for _, rule := range f.Rules {
		matched, err := matchRule(rule, ctx)
		if err != nil {
			res.Reason, res.Error, res.Value = ReasonError, err.Error(), fallback
			return res
		}
		if !matched {
			continue
		}

		res.RuleID = rule.ID

		if rule.Rollout != nil {
			key := bucketKey(rule.Rollout, ctx)
			bucket := Bucket(f.Key, rule.ID, key)
			variation, ok := pick(rule.Rollout, bucket)
			if !ok {
				res.Reason, res.Error, res.Value = ReasonError, "rollout selected no variation", fallback
				return res
			}
			v, ok := f.Variation(variation)
			if !ok {
				res.Reason, res.Error, res.Value = ReasonError, "rollout variation "+variation+" not found", fallback
				return res
			}
			res.Variation, res.Value, res.Reason, res.Bucket = v.Key, v.Value, ReasonRollout, bucket
			return res
		}

		v, ok := f.Variation(rule.Variation)
		if !ok {
			res.Reason, res.Error, res.Value = ReasonError, "rule variation "+rule.Variation+" not found", fallback
			return res
		}
		res.Variation, res.Value, res.Reason = v.Key, v.Value, ReasonRuleMatch
		return res
	}

	v, ok := f.Variation(f.DefaultVariation)
	if !ok {
		res.Reason, res.Error, res.Value = ReasonError, "default_variation not found", fallback
		return res
	}
	res.Variation, res.Value, res.Reason = v.Key, v.Value, ReasonDefault
	return res
}

// bucketKey returns the value a rollout buckets by.
func bucketKey(r *flag.Rollout, ctx Context) string {
	if r.BucketBy == "" || r.BucketBy == "key" {
		return ctx.Key
	}
	// Falling back to the targeting key when the attribute is missing keeps
	// the subject in *some* stable bucket. Falling back to "" would put every
	// subject lacking the attribute in the same bucket, which silently skews
	// a rollout.
	if v, ok := ctx.Attr(r.BucketBy); ok {
		return v
	}
	return ctx.Key
}

// Bucket maps a subject onto 0..9999 deterministically.
//
// The inputs are (flagKey, salt, subject): including the flag key and a
// per-rule salt means a user who is unlucky in one rollout is not
// systematically unlucky in every rollout. Without the salt, the same 10% of
// users would be the guinea pigs for every experiment forever.
//
// SHA-256 is chosen over a faster hash because this value must match across
// SDKs in different languages for years. A non-cryptographic hash would be
// faster and is a worse choice: implementations differ in seeds and
// endianness, and "the Go SDK and the Python SDK disagree about who is in the
// rollout" is a miserable bug.
func Bucket(flagKey, salt, subject string) int {
	h := sha256.New()
	// Length-prefix each field so ("ab","c") and ("a","bc") cannot collide.
	for _, s := range []string{flagKey, salt, subject} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	sum := h.Sum(nil)
	// Take 8 bytes and reduce modulo the basis-point total. The modulo bias
	// here is about 1 part in 2^50, which is irrelevant for traffic splitting.
	v := binary.BigEndian.Uint64(sum[:8])
	return int(v % uint64(flag.BasisPointsTotal))
}

// pick walks the weights in a deterministic order and returns the variation
// whose cumulative range contains bucket.
//
// Sorting by variation key, not map order, is essential: Go randomises map
// iteration, so an unsorted walk would assign a subject to a different
// variation on each evaluation.
func pick(r *flag.Rollout, bucket int) (string, bool) {
	keys := make([]string, 0, len(r.Weights))
	for k := range r.Weights {
		keys = append(keys, k)
	}
	sortStrings(keys)

	cumulative := 0
	for _, k := range keys {
		cumulative += r.Weights[k]
		if bucket < cumulative {
			return k, true
		}
	}
	// Only reachable when the weights do not sum to 10000, which Validate
	// rejects. Returning the last key is a safer failure than returning
	// nothing.
	if len(keys) > 0 {
		return keys[len(keys)-1], true
	}
	return "", false
}

// sortStrings is a tiny insertion sort to avoid importing sort for one call
// in a hot path.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// matchRule reports whether every condition matches (AND semantics).
func matchRule(r flag.Rule, ctx Context) (bool, error) {
	for _, c := range r.Conditions {
		ok, err := matchCondition(c, ctx)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func matchCondition(c flag.Condition, ctx Context) (bool, error) {
	got, present := ctx.Attr(c.Attribute)

	result, err := func() (bool, error) {
		if c.Operator == flag.OpExists {
			return present, nil
		}
		// A missing attribute does not match any comparison. Treating it as
		// an empty string would make `plan != "pro"` true for a context with
		// no plan at all, which is a surprising way to leak a feature.
		if !present {
			return false, nil
		}
		if len(c.Values) == 0 {
			return false, fmt.Errorf("operator %q has no values", c.Operator)
		}

		switch c.Operator {
		case flag.OpEquals:
			return got == c.Values[0], nil
		case flag.OpNotEquals:
			return got != c.Values[0], nil
		case flag.OpIn:
			return containsString(c.Values, got), nil
		case flag.OpNotIn:
			return !containsString(c.Values, got), nil
		case flag.OpContains:
			return strings.Contains(got, c.Values[0]), nil
		case flag.OpStartsWith:
			return strings.HasPrefix(got, c.Values[0]), nil
		case flag.OpEndsWith:
			return strings.HasSuffix(got, c.Values[0]), nil
		case flag.OpGreaterThan, flag.OpLessThan:
			a, err := strconv.ParseFloat(got, 64)
			if err != nil {
				return false, nil // non-numeric attribute never matches
			}
			b, err := strconv.ParseFloat(c.Values[0], 64)
			if err != nil {
				return false, fmt.Errorf("operator %q needs a numeric value, got %q", c.Operator, c.Values[0])
			}
			if c.Operator == flag.OpGreaterThan {
				return a > b, nil
			}
			return a < b, nil
		case flag.OpSemverGTE, flag.OpSemverLT:
			cmp, ok := compareSemver(got, c.Values[0])
			if !ok {
				return false, nil
			}
			if c.Operator == flag.OpSemverGTE {
				return cmp >= 0, nil
			}
			return cmp < 0, nil
		case flag.OpRegexMatches:
			re, err := regexp.Compile(c.Values[0])
			if err != nil {
				return false, fmt.Errorf("invalid regex %q: %w", c.Values[0], err)
			}
			return re.MatchString(got), nil
		default:
			return false, fmt.Errorf("unknown operator %q", c.Operator)
		}
	}()
	if err != nil {
		return false, err
	}
	if c.Negate {
		return !result, nil
	}
	return result, nil
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// compareSemver compares two dotted versions numerically, ignoring any
// pre-release suffix. Returns false when either side is unparseable.
//
// Numeric comparison matters: lexically, "1.10.0" < "1.9.0", so a string
// comparison would exclude exactly the newer clients a version rule is meant
// to include.
func compareSemver(a, b string) (int, bool) {
	pa, ok := parseSemver(a)
	if !ok {
		return 0, false
	}
	pb, ok := parseSemver(b)
	if !ok {
		return 0, false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// Drop build metadata and pre-release: "1.2.3-beta.1+sha" -> "1.2.3".
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
