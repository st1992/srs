package main

import "testing"

func TestLocatorKey(t *testing.T) {
	if got, want := locatorKey("abc-123"), "loc:abc-123"; got != want {
		t.Fatalf("locatorKey() = %q, want %q", got, want)
	}
}
