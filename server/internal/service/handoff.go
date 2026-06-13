package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ── Handoff rule shape ────────────────────────────────────────────────────
//
// A handoff rule is a single condition the autopilot evaluates against an
// event. Two rule kinds are supported:
//
//   - comment_contains_keyword:  fires when the source agent's comment text
//     contains the configured keyword (case-insensitive substring match).
//   - issue_has_handoff_data:    fires when the issue's handoff_data JSON
//     contains the configured key (top-level key presence; supports the
//     common "did the source agent write a handoff payload?" check without
//     forcing the user to learn JSONPath).
//
// Rules are AND'd together (operator="all") or OR'd (operator="any"). An
// empty rule list is intentionally non-matching — a handoff autopilot with
// zero rules never fires, regardless of operator, so the user is forced to
// configure at least one condition.
//
// HandoffRuleSet is the on-the-wire shape (JSON in handler requests, JSONB
// in autopilot.handoff_rules). It is intentionally separate from
// autopilot.handoff_rules (raw []byte from sqlc) so handlers can decode +
// validate before persisting, and so the engine can pass a typed value
// around instead of re-parsing bytes at every call site.

type HandoffRuleKind string

const (
	HandoffRuleCommentContainsKeyword HandoffRuleKind = "comment_contains_keyword"
	HandoffRuleIssueHasHandoffData    HandoffRuleKind = "issue_has_handoff_data"
)

// HandoffRulesOperator chooses the boolean combinator across a rule list.
// The DB CHECK constraint autopilot_handoff_rules_operator_check also
// enforces this set at the SQL layer.
type HandoffRulesOperator string

const (
	HandoffRulesAll HandoffRulesOperator = "all"
	HandoffRulesAny HandoffRulesOperator = "any"
)

// HandoffRule is one rule entry inside an autopilot's handoff_rules array.
//
// The struct is decoded from / encoded to JSON as a flat object with
// discriminated "kind" — no nested envelope, so a hand-typed user
// authoring a rule in the editor sees the same shape the API expects.
//
// "value" is intentionally the only payload field. comment_contains_keyword
// treats it as a keyword; issue_has_handoff_data treats it as a key name.
// Splitting into per-kind fields would just trade clarity for stricter
// typing the wire format doesn't need.
type HandoffRule struct {
	Kind  HandoffRuleKind `json:"kind"`
	Value string          `json:"value"`
}

// HandoffRuleSet is the full handoff configuration stored on an autopilot
// row: the rule array + the boolean operator + the comment template.
//
// CommentTemplate is the body of the @-mention comment the autopilot posts
// when a rule set matches. It supports {{handoff_data}} interpolation
// (whole object → JSON string) plus the same {{date}} variable used by
// issue-title templates — see InterpolateHandoffCommentTemplate.
type HandoffRuleSet struct {
	Operator        HandoffRulesOperator `json:"operator"`
	Rules           []HandoffRule        `json:"rules"`
	CommentTemplate string               `json:"comment_template"`
}

// ── Validation ────────────────────────────────────────────────────────────

// MaxHandoffRules caps the rule array length at the handler layer. The DB
// CHECK constraint autopilot_handoff_rules_size_limit guards the encoded
// byte size (≤16 KiB); the count cap is a stricter "don't let users author
// an unbounded pile of ORs" knob that is much cheaper to enforce here.
const MaxHandoffRules = 32

// MaxHandoffKeywordLen caps an individual comment_contains_keyword value.
// 200 chars is well over what a real agent-name token or status phrase
// needs and keeps regex compilation cheap during evaluation.
const MaxHandoffKeywordLen = 200

// MaxHandoffKeyLen caps an issue_has_handoff_data key. Matches the
// issue.metadata key regex (a-zA-Z0-9_.- up to 64 chars), so a handoff
// rule can mirror the exact same key the source agent wrote into
// issue.metadata / issue.handoff_data.
const MaxHandoffKeyLen = 64

// MaxHandoffCommentTemplateLen caps the comment template body. The body is
// stored on autopilot.handoff_comment_template (TEXT, no DB-side length
// cap), so this is purely a wire-format sanity bound.
const MaxHandoffCommentTemplateLen = 4000

var handoffKeyRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)

// ValidateHandoffRuleSet checks that a HandoffRuleSet from a client (or
// loaded from JSONB) is structurally valid. Returns the first error
// encountered — the handler maps this to a 400, the dispatcher treats it
// as "rule set misconfigured, skip this autopilot".
func ValidateHandoffRuleSet(set HandoffRuleSet) error {
	switch set.Operator {
	case HandoffRulesAll, HandoffRulesAny:
		// ok
	case "":
		// Default to "all" so an omitted operator on a client payload
		// still validates. The DB also defaults to 'all' at column
		// level — this matches.
	default:
		return fmt.Errorf("invalid operator %q; supported: all, any", set.Operator)
	}
	if len(set.Rules) > MaxHandoffRules {
		return fmt.Errorf("too many rules: %d > max %d", len(set.Rules), MaxHandoffRules)
	}
	for i, r := range set.Rules {
		if err := validateHandoffRule(r); err != nil {
			return fmt.Errorf("rule[%d]: %w", i, err)
		}
	}
	if len(set.CommentTemplate) > MaxHandoffCommentTemplateLen {
		return fmt.Errorf("comment_template exceeds %d characters", MaxHandoffCommentTemplateLen)
	}
	return nil
}

func validateHandoffRule(r HandoffRule) error {
	switch r.Kind {
	case HandoffRuleCommentContainsKeyword:
		if strings.TrimSpace(r.Value) == "" {
			return errors.New("comment_contains_keyword requires non-empty value")
		}
		if len(r.Value) > MaxHandoffKeywordLen {
			return fmt.Errorf("comment_contains_keyword value exceeds %d characters", MaxHandoffKeywordLen)
		}
	case HandoffRuleIssueHasHandoffData:
		if !handoffKeyRE.MatchString(r.Value) {
			return fmt.Errorf("issue_has_handoff_data value must match ^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$")
		}
	default:
		return fmt.Errorf("unknown kind %q; supported: comment_contains_keyword, issue_has_handoff_data", r.Kind)
	}
	return nil
}

// ── JSONB round-trip ──────────────────────────────────────────────────────

// EncodeHandoffRuleSet marshals a typed HandoffRuleSet to the JSON bytes
// stored in autopilot.handoff_rules. The DB column default is '[]'::jsonb,
// so a brand-new autopilot with no rules writes an empty array — never
// nil bytes, which sqlc would otherwise hand to PG as NULL and trip the
// autopilot_handoff_rules_is_array CHECK.
func EncodeHandoffRuleSet(set HandoffRuleSet) ([]byte, error) {
	if set.Rules == nil {
		set.Rules = []HandoffRule{}
	}
	if set.Operator == "" {
		set.Operator = HandoffRulesAll
	}
	return json.Marshal(set)
}

// DecodeHandoffRuleSet unmarshals the JSONB blob stored on autopilot rows.
// A nil/empty blob (legacy rows that pre-date the column) decodes to the
// zero value (operator="" → defaults to 'all' at evaluation time, no rules
// → never matches). Unparseable bytes are also returned as the zero
// value + error, so the caller can decide whether to skip the autopilot
// or treat it as a misconfigured row.
func DecodeHandoffRuleSet(raw []byte) (HandoffRuleSet, error) {
	if len(raw) == 0 {
		return HandoffRuleSet{Operator: HandoffRulesAll, Rules: []HandoffRule{}}, nil
	}
	var set HandoffRuleSet
	if err := json.Unmarshal(raw, &set); err != nil {
		return HandoffRuleSet{}, fmt.Errorf("decode handoff_rules: %w", err)
	}
	if set.Operator == "" {
		set.Operator = HandoffRulesAll
	}
	return set, nil
}

// ── Evaluation ────────────────────────────────────────────────────────────

// HandoffEvalContext bundles everything a handoff rule needs to make a
// decision. The dispatcher builds one of these from a freshly-created
// comment and the issue the comment lives on, then loops the autopilot's
// rules against it. Keeping this in a struct (instead of a long argument
// list) makes the call site readable and future rule kinds easier to
// extend — they just read from the struct.
type HandoffEvalContext struct {
	// CommentContent is the body of the source agent's comment, lower-cased
	// lazily by the keyword rule so the alloc only happens when there is
	// a keyword rule to evaluate. The struct field is the original case
	// for callers that need it.
	CommentContent string
	// IssueHandoffData is the issue's handoff_data JSONB. The dispatcher
	// only loads it when at least one active autopilot in the workspace
	// has a handoff rule (saves a hot-path row read when no handoff
	// autopilots are configured).
	IssueHandoffData map[string]any
}

// MatchesHandoffRuleSet returns true when the configured rule set fires
// against the event. An empty rule set never matches — this is the only
// way to author a handoff autopilot that the dispatcher can "see is
// configured but should not fire", and it keeps the misconfiguration
// surface small.
func MatchesHandoffRuleSet(set HandoffRuleSet, ev HandoffEvalContext) bool {
	if len(set.Rules) == 0 {
		return false
	}
	matches := 0
	for _, r := range set.Rules {
		if matchHandoffRule(r, ev) {
			matches++
			if set.Operator == HandoffRulesAny {
				return true
			}
		} else if set.Operator == HandoffRulesAll {
			// Short-circuit: under "all", a single non-match kills the
			// whole set. Saves work and makes evaluation order
			// irrelevant for the result.
			return false
		}
	}
	// After the loop: under "all" we never short-circuited away, so every
	// rule matched. Under "any" we never returned true, so zero rules
	// matched. Both reduce to "matches == len(rules)".
	return matches == len(set.Rules)
}

func matchHandoffRule(r HandoffRule, ev HandoffEvalContext) bool {
	switch r.Kind {
	case HandoffRuleCommentContainsKeyword:
		// case-insensitive substring match. Trim the keyword of
		// surrounding whitespace so a copy-paste with a trailing space
		// does not silently disable the rule.
		kw := strings.ToLower(strings.TrimSpace(r.Value))
		if kw == "" {
			return false
		}
		return strings.Contains(strings.ToLower(ev.CommentContent), kw)
	case HandoffRuleIssueHasHandoffData:
		if ev.IssueHandoffData == nil {
			return false
		}
		_, ok := ev.IssueHandoffData[r.Value]
		return ok
	default:
		// Unknown kinds are treated as non-matches. The validator rejects
		// them at write time, so this branch is defense-in-depth for
		// legacy rows that may pre-date a rule-kind rename.
		return false
	}
}

// ── Comment template interpolation ────────────────────────────────────────

// handoffCommentTemplateTokenRE mirrors issueTitleTemplateTokenRE — it
// matches any {{...}} token and tolerates whitespace inside the braces
// ({{ handoff_data }}) so the validator and the renderer agree on what
// is a token.
var handoffCommentTemplateTokenRE = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// SupportedHandoffCommentTemplateVariables enumerates the placeholders
// InterpolateHandoffCommentTemplate will substitute. Keep this in sync
// with the substitution switch below — ValidateHandoffCommentTemplate
// rejects templates that reference any variable not in this list.
var SupportedHandoffCommentTemplateVariables = []string{"handoff_data", "date"}

// InterpolateHandoffCommentTemplate renders the configured comment
// template body for posting against the matched issue. It is a pure
// string transform — no side effects, no error if the template references
// an unknown variable (we leave the original token in place, matching the
// behaviour of interpolateTemplate for issue titles).
//
// {{handoff_data}} → JSON-encoded handoff_data (string, suitable for
// dropping into a comment body — agents downstream read it back as
// JSON).
//
// {{date}} → the trigger time formatted in UTC YYYY-MM-DD, the same
// format the issue-title template uses. We deliberately do not let the
// handoff template pick a timezone: this comment is posted by the
// system, not a user, so picking a workspace timezone is fine for the
// title (which a human reads) but the comment body is consumed by an
// agent and ISO dates sort lexically. The dispatcher passes a precomputed
// value to keep this function pure.
func InterpolateHandoffCommentTemplate(tmpl string, handoffData map[string]any, triggerDate string) string {
	if tmpl == "" {
		return tmpl
	}
	// JSON-marshal once so a {{handoff_data}} reference that appears
	// multiple times in the template is encoded identically each time
	// (avoids pretty-print drift between the first and second occurrence
	// when the value is an object).
	handoffJSON, err := json.Marshal(handoffData)
	if err != nil {
		// handoffData is decoded from PG, so a marshal failure means the
		// stored blob is not a JSON object. Fall back to "{}" — the
		// column CHECK guarantees object shape, so this branch is
		// defense-in-depth for rows somehow predating the constraint.
		handoffJSON = []byte("{}")
	}
	return handoffCommentTemplateTokenRE.ReplaceAllStringFunc(tmpl, func(match string) string {
		name := strings.TrimSpace(match[2 : len(match)-2])
		switch name {
		case "handoff_data":
			return string(handoffJSON)
		case "date":
			return triggerDate
		default:
			return match
		}
	})
}

// ValidateHandoffCommentTemplate rejects templates that contain any
// {{...}} token not in SupportedHandoffCommentTemplateVariables. Empty
// template is valid (the dispatcher uses the autopilot title as a
// fallback so a handoff autopilot can still be created without
// configuring a comment body).
func ValidateHandoffCommentTemplate(tmpl string) error {
	if tmpl == "" {
		return nil
	}
	for _, m := range handoffCommentTemplateTokenRE.FindAllStringSubmatch(tmpl, -1) {
		name := m[1]
		if !isSupportedHandoffCommentVariable(name) {
			return fmt.Errorf(
				"unknown comment-template variable %q; supported: {{%s}}",
				name,
				strings.Join(SupportedHandoffCommentTemplateVariables, "}}, {{"),
			)
		}
	}
	return nil
}

func isSupportedHandoffCommentVariable(name string) bool {
	for _, v := range SupportedHandoffCommentTemplateVariables {
		if name == v {
			return true
		}
	}
	return false
}
