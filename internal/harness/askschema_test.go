package harness

import "testing"

// `recommended` is a plain array of strings in the schema (no anyOf), and a
// bare string on input still arrives as a one-element list.
func TestAskToolDef_RecommendedIsStringArray(t *testing.T) {
	props := askToolDef().Function.Parameters.(map[string]any)["properties"].(map[string]any)
	rec := props["recommended"].(map[string]any)
	if _, has := rec["anyOf"]; has || rec["type"] != "array" {
		t.Fatalf("recommended schema: %v", rec)
	}
	if items := rec["items"].(map[string]any); items["type"] != "string" {
		t.Fatalf("items: %v", items)
	}
	got := askRecommended([]byte(`"a"`), []string{"a", "b"}, false)
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("bare string: %v", got)
	}
}
