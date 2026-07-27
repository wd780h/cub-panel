package panel

import (
	"cubpanel/internal/store"
	"testing"
)

func TestNodePublicHost(t *testing.T) {
	n := &store.Node{Endpoint: "https://10.0.0.5:8788", Domain: ""}
	if got := nodePublicHost(n); got != "10.0.0.5" {
		t.Fatalf("endpoint only: got %q", got)
	}
	n.Domain = "node1.example.com"
	if got := nodePublicHost(n); got != "node1.example.com" {
		t.Fatalf("domain preferred: got %q", got)
	}
	n.Domain = "node1.example.com."
	if got := nodePublicHost(n); got != "node1.example.com" {
		t.Fatalf("trailing dot: got %q", got)
	}
	if got := nodePublicHost(nil); got != "" {
		t.Fatalf("nil: got %q", got)
	}
}

func TestValidNodeDomain(t *testing.T) {
	ok := []string{"a.com", "node1.ddns.net", "203.0.113.10", "localhost"}
	bad := []string{"", "http://x.com", "a/b", "a@b.com", "a:8788", "a b"}
	for _, s := range ok {
		if !validNodeDomain(s) {
			t.Errorf("want ok %q", s)
		}
	}
	for _, s := range bad {
		if validNodeDomain(s) {
			t.Errorf("want bad %q", s)
		}
	}
}
