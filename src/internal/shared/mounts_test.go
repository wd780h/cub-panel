package shared

import "testing"

func TestParseMounts(t *testing.T) {
	ok, err := ParseMounts("/data/share:/mnt/share:ro\n/data/pub:/srv/pub, /a:/b:rw")
	if err != nil || len(ok) != 3 {
		t.Fatalf("valid specs: %v %v", ok, err)
	}
	if !ok[0].ReadOnly || ok[1].ReadOnly || ok[2].ReadOnly {
		t.Fatalf("ro flags wrong: %+v", ok)
	}
	if m, err := ParseMounts(""); err != nil || len(m) != 0 {
		t.Fatalf("empty spec should be fine: %v %v", m, err)
	}
	for _, bad := range []string{
		"relative:/mnt",  // relative host path
		"/a:relative",    // relative guest path
		"/a/../etc:/mnt", // dot-dot
		"/a:/",           // guest root
		"/a:/b:xx",       // bad option
		"/only-one-part", // missing guest path
		"/a:/1:ro,/b:/2,/c:/3,/d:/4,/e:/5,/f:/6,/g:/7,/h:/8,/i:/9", // 9 mounts
	} {
		if _, err := ParseMounts(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}
