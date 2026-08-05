package mathutil

import "testing"

func TestClamp(t *testing.T) {
	tests := []struct {
		name string
		v, lo, hi int
		want   int
	}{
		{"within range", 5, 0, 10, 5},
		{"below range", -1, 0, 10, 0},
		{"above range", 11, 0, 10, 10},
		{"equal to lo", 0, 0, 10, 0},
		{"equal to hi", 10, 0, 10, 10},
		{"lo equals hi", 5, 3, 3, 3},
		{"negative range", -5, -10, -1, -5},
		{"below negative range", -11, -10, -1, -10},
		{"above negative range", 0, -10, -1, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clamp(tt.v, tt.lo, tt.hi); got != tt.want {
				t.Errorf("Clamp(%d, %d, %d) = %d, want %d", tt.v, tt.lo, tt.hi, got, tt.want)
			}
		})
	}
}
