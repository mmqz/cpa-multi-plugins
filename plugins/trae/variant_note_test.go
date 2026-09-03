package main

import "testing"

// v0.12.15: trae credentials previously carried no region/variant note at
// all, so the host credential manager could not tell cn/solo/intl accounts
// apart. authNote supplies the tag (workbuddy/qoder parity); the intl chain
// pins "INTL" directly.

func TestAuthNote(t *testing.T) {
	cases := []struct {
		variant string
		want    string
	}{
		{variantCN, "CN"},
		{variantSolo, "SOLO"},
		{variantIntl, "INTL"},
		{"", "CN"},        // legacy/unknown falls to CN (cn is the default variant)
		{" SOLO ", "SOLO"}, // tolerant of stray whitespace/case
		{"INTL", "INTL"},
		{"garbage", "CN"},
	}
	for _, tc := range cases {
		if got := authNote(tc.variant); got != tc.want {
			t.Errorf("authNote(%q) = %q, want %q", tc.variant, got, tc.want)
		}
	}
}
