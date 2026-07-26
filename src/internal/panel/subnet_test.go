package panel

import (
	"strings"
	"testing"
)

func TestSubnetMismatch(t *testing.T) {
	// Match (different textual forms of the same network) → no warning.
	if w := subnetMismatch("lxdbr0", "10.180.0.0/24", "10.180.0.1/24"); w != "" {
		t.Fatalf("want no warning, got %q", w)
	}
	// Mismatch → warning naming both networks and the fix command.
	w := subnetMismatch("lxdbr0", "10.59.94.0/24", "10.201.55.1/24")
	if w == "" || !strings.Contains(w, "10.201.55.0/24") ||
		!strings.Contains(w, "ipv4.address=10.59.94.1/24") {
		t.Fatalf("unexpected warning: %q", w)
	}
	// Unparsable input → silent (other validation owns that).
	if w := subnetMismatch("lxdbr0", "garbage", "10.0.0.1/24"); w != "" {
		t.Fatalf("want empty on bad input, got %q", w)
	}
}
