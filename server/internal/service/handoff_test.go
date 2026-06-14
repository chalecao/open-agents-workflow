package service

import (
	"encoding/json"
	"testing"
)

// Regression test for the /api/autopilots 500 caused by
// EncodeHandoffRuleSet/DecodeHandoffRuleSet serialising the full
// HandoffRuleSet (an object) into autopilot.handoff_rules — a JSONB column
// that has CHECK (jsonb_typeof = 'array'). The DB rejected the object
// with `autopilot_handoff_rules_is_array` and the handler turned that
// into a 500 ("failed to create autopilot"). The fix narrows the
// persistence boundary so the column only ever stores the rule array;
// operator and comment template ride their own dedicated columns.

func TestEncodeHandoffRuleSet_PersistsArrayOnly(t *testing.T) {
	set := HandoffRuleSet{
		Operator:        HandoffRulesAll,
		Rules:           []HandoffRule{{Kind: HandoffRuleIssueHasHandoffData, Value: "auto"}},
		CommentTemplate: "继续",
	}
	got, err := EncodeHandoffRuleSet(set)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Must round-trip as a JSON array so it satisfies the
	// autopilot_handoff_rules_is_array CHECK at the SQL layer.
	var probe any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Fatalf("encoded bytes are not valid JSON: %v (got %q)", err, got)
	}
	arr, ok := probe.([]any)
	if !ok {
		t.Fatalf("encoded handoff_rules is %T, expected JSON array; got %s", probe, got)
	}
	if len(arr) != 1 {
		t.Fatalf("encoded handoff_rules has %d entries, want 1", len(arr))
	}
	// Operator and comment template MUST NOT leak into the array-encoded
	// column — they have their own columns and a re-introduction here
	// would silently re-trip the array CHECK once decode-side code starts
	// reading them.
	rule, ok := arr[0].(map[string]any)
	if !ok {
		t.Fatalf("rule[0] is %T, expected object", arr[0])
	}
	if _, present := rule["operator"]; present {
		t.Errorf("rule[0] leaked operator into array; encoded = %s", got)
	}
	if _, present := rule["comment_template"]; present {
		t.Errorf("rule[0] leaked comment_template into array; encoded = %s", got)
	}
}

func TestEncodeHandoffRuleSet_EmptyRulesEncodeAsArray(t *testing.T) {
	// A non-handoff autopilot INSERT still names the handoff_rules
	// column explicitly, so it must receive a concrete value (not nil
	// bytes — pgx would hand those to PG as NULL and trip NOT NULL).
	// The fix encodes a nil rule slice as the empty array `[]`.
	got, err := EncodeHandoffRuleSet(HandoffRuleSet{Rules: nil})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(got) != "[]" {
		t.Fatalf("expected \"[]\", got %q", got)
	}
}

func TestDecodeHandoffRuleSet_ReadsArrayFromColumn(t *testing.T) {
	// Mirrors the on-disk shape: a JSONB array of rule objects, with
	// operator / comment_template never present in this column.
	raw := []byte(`[{"kind":"issue_has_handoff_data","value":"auto"}]`)
	got, err := DecodeHandoffRuleSet(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(got.Rules))
	}
	if got.Rules[0].Kind != HandoffRuleIssueHasHandoffData {
		t.Errorf("rule kind = %q, want %q", got.Rules[0].Kind, HandoffRuleIssueHasHandoffData)
	}
	if got.Rules[0].Value != "auto" {
		t.Errorf("rule value = %q, want %q", got.Rules[0].Value, "auto")
	}
	// Operator and comment template come from dedicated columns, not
	// from the rules blob — Decode returns the zero/empty form for
	// those so the caller is forced to read the row's other columns
	// rather than silently picking up stale JSON values.
	if got.Operator != HandoffRulesAll {
		t.Errorf("operator = %q, want default %q", got.Operator, HandoffRulesAll)
	}
	if got.CommentTemplate != "" {
		t.Errorf("comment_template = %q, want empty (caller must read column)", got.CommentTemplate)
	}
}

func TestDecodeHandoffRuleSet_EmptyBlob(t *testing.T) {
	got, err := DecodeHandoffRuleSet(nil)
	if err != nil {
		t.Fatalf("decode nil: %v", err)
	}
	if got.Rules == nil {
		t.Errorf("rules should be non-nil empty slice so callers can range safely")
	}
	if len(got.Rules) != 0 {
		t.Errorf("len(rules) = %d, want 0", len(got.Rules))
	}
}

func TestDecodeHandoffRuleSet_RejectsObjectShape(t *testing.T) {
	// A legacy row authored by the pre-fix build persisted the full
	// HandoffRuleSet as an object. Decode must surface this as a parse
	// error so the dispatcher / handler can fall back to "skip" / 500
	// instead of silently accepting the wrong shape.
	raw := []byte(`{"operator":"all","rules":[],"comment_template":"继续"}`)
	if _, err := DecodeHandoffRuleSet(raw); err == nil {
		t.Fatalf("decode accepted object shape; want error")
	}
}

func TestEncodeDecodeHandoffRuleSet_RoundTrip(t *testing.T) {
	in := HandoffRuleSet{
		Rules: []HandoffRule{
			{Kind: HandoffRuleCommentContainsKeyword, Value: "foo"},
			{Kind: HandoffRuleIssueHasHandoffData, Value: "bar"},
		},
	}
	encoded, err := EncodeHandoffRuleSet(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := DecodeHandoffRuleSet(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Rules) != len(in.Rules) {
		t.Fatalf("round-trip rules length = %d, want %d", len(out.Rules), len(in.Rules))
	}
	for i, r := range in.Rules {
		if out.Rules[i] != r {
			t.Errorf("rule[%d] = %+v, want %+v", i, out.Rules[i], r)
		}
	}
}
