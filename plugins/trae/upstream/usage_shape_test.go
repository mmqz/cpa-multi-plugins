package upstream

import "testing"

func TestReadUsageReasoningScansPastZeroAliases(t *testing.T) {
	tests := []struct {
		name string
		raw  map[string]any
		want int64
		ok   bool
	}{
		{
			name: "completion details zero does not mask output details",
			raw: map[string]any{
				"completion_tokens_details": map[string]any{"reasoning_tokens": int64(0)},
				"output_tokens_details":     map[string]any{"reasoning_tokens": int64(20)},
			},
			want: 20,
			ok:   true,
		},
		{
			name: "completion details zero does not mask top-level reasoning",
			raw: map[string]any{
				"completion_tokens_details": map[string]any{"reasoning_tokens": int64(0)},
				"output_tokens_details":     map[string]any{"reasoning_tokens": int64(0)},
				"reasoning_tokens":          int64(12),
			},
			want: 12,
			ok:   true,
		},
		{
			name: "details zero does not mask completion thinking",
			raw: map[string]any{
				"completion_tokens_details": map[string]any{
					"reasoning_tokens": int64(0),
					"reasoning":        int64(0),
				},
				"completion_thinking_tokens": int64(7),
			},
			want: 7,
			ok:   true,
		},
		{
			name: "thinking zero does not mask reasoning count",
			raw: map[string]any{
				"completion_thinking_tokens": int64(0),
				"reasoning_token_count":      int64(9),
			},
			want: 9,
			ok:   true,
		},
		{
			name: "explicit zero is preserved when every alias is zero",
			raw: map[string]any{
				"completion_tokens_details":  map[string]any{"reasoning_tokens": int64(0)},
				"output_tokens_details":      map[string]any{"reasoning_tokens": int64(0)},
				"completion_thinking_tokens": int64(0),
			},
			want: 0,
			ok:   true,
		},
		{
			name: "absent stays absent",
			raw:  map[string]any{"prompt_tokens": int64(10)},
			want: 0,
			ok:   false,
		},
		{
			name: "nil map stays absent",
			raw:  nil,
			want: 0,
			ok:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := readUsageReasoning(tt.raw)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("readUsageReasoning() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestNormalizeUsageForHostKeepsPositiveLaterAlias(t *testing.T) {
	raw := map[string]any{
		"prompt_tokens":     int64(10),
		"completion_tokens": int64(30),
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": int64(0),
		},
		"completion_thinking_tokens": int64(7),
	}

	got := NormalizeUsageForHost(raw)
	for _, key := range []string{"completion_tokens_details", "output_tokens_details"} {
		details, ok := got[key].(map[string]any)
		if !ok {
			t.Fatalf("normalized %s = %#v, want object", key, got[key])
		}
		if n, ok := usageField(details, "reasoning_tokens"); !ok || n != 7 {
			t.Fatalf("normalized %s.reasoning_tokens = (%d, %v), want (7, true)", key, n, ok)
		}
	}

	original, ok := raw["completion_tokens_details"].(map[string]any)
	if !ok {
		t.Fatal("original completion_tokens_details was replaced")
	}
	if n, ok := usageField(original, "reasoning_tokens"); !ok || n != 0 {
		t.Fatalf("NormalizeUsageForHost mutated input: reasoning_tokens = (%d, %v), want (0, true)", n, ok)
	}
}
