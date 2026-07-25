package models

import "testing"

// PROB-13: image > base_image > default.
func TestEffectiveImage(t *testing.T) {
	cases := []struct {
		name string
		p    Problem
		want string
	}{
		{"image wins", Problem{Image: "custom:v2", BaseImage: "base:v1"}, "custom:v2"},
		{"base_image fallback", Problem{BaseImage: "base:v1"}, "base:v1"},
		{"default fallback", Problem{}, DefaultBaseImage},
		{"default is k3s-base", Problem{}, "k3s-base:latest"},
	}
	for _, tc := range cases {
		if got := tc.p.EffectiveImage(); got != tc.want {
			t.Errorf("%s: EffectiveImage() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
