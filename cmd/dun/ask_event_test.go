package main

import "testing"

func TestNewAskEvent_NoCustomIsSet(t *testing.T) {
	a := newAskEvent("call-1", "Q", []string{"a", "b"}, false, true, nil)
	if !a.NoCustom {
		t.Error("NoCustom must be true when the parameter is true")
	}
	if a.Question != "Q" || a.CallID != "call-1" {
		t.Errorf("fields not carried: %+v", a)
	}
}

func TestNewAskEvent_NoCustomFalse(t *testing.T) {
	a := newAskEvent("call-2", "Q", []string{"a"}, false, false, nil)
	if a.NoCustom {
		t.Error("NoCustom must be false when the parameter is false")
	}
}

func TestNewAskEvent_Recommended(t *testing.T) {
	a := newAskEvent("call-3", "Q", []string{"a", "b"}, false, false, []string{"b"})
	if a.Recommended != "b" {
		t.Errorf("Recommended = %q, want %q", a.Recommended, "b")
	}
}
