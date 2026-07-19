package main

import "testing"

func TestSplitHostPortTCP(t *testing.T) {
	host, port, err := splitHostPortTCP("0.0.0.0:8080")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "0.0.0.0" {
		t.Fatalf("host = %q, want %q", host, "0.0.0.0")
	}
	if port != 8080 {
		t.Fatalf("port = %d, want %d", port, 8080)
	}
}

func TestSplitHostPortTCPInvalid(t *testing.T) {
	if _, _, err := splitHostPortTCP("not-an-addr"); err == nil {
		t.Fatal("expected error for malformed address, got nil")
	}
}
