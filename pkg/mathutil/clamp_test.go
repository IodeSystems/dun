package mathutil

import "testing"

func TestClamp(t *testing.T) {
	tests := []struct {
		name string
		v, lo, hi, want int
	}{
		{"within range", 5, 1, 10, 5},
		{"below lo", 0, 1, 10, 1},
		{"above hi", 20, 1, 10, 10},
		{"equal to lo", 1, 1, 10, 1},
		{"equal to hi", 10, 1, 10, 10},
		{"lo equals hi", 5, 3, 3, 3},
		{"negative values", -5, -10, -1, -5},
		{"below negative range", -20, -10, -1, -10},
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
