package v1

import "testing"

func TestAllocationIDForSessionGoldenVectors(t *testing.T) {
	tests := []struct {
		name       string
		sessionID  string
		generation uint64
		want       string
	}{
		{
			name:       "generation one",
			sessionID:  "11111111-1111-4111-8111-111111111111",
			generation: 1,
			want:       "aa57149f-4f09-58e1-881e-c0771e42cc24",
		},
		{
			name:       "generation seven",
			sessionID:  "11111111-1111-4111-8111-111111111111",
			generation: 7,
			want:       "bbccedbc-f431-50fe-856e-4d3090d825f0",
		},
		{
			name:       "maximum durable generation",
			sessionID:  "ffffffff-ffff-4fff-bfff-ffffffffffff",
			generation: MaxDurableValue,
			want:       "3ae7520c-7821-5955-89b0-d2f55021dc3e",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := AllocationIDForSession(test.sessionID, test.generation); got != test.want {
				t.Fatalf("AllocationIDForSession(%q, %d) = %q, want %q", test.sessionID, test.generation, got, test.want)
			}
		})
	}
}
