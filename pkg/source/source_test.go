package source

import "testing"

func TestParseS3URI(t *testing.T) {
	b, k, err := parseS3URI("s3://acme-backups/payments/latest.dump")
	if err != nil || b != "acme-backups" || k != "payments/latest.dump" {
		t.Fatalf("got %q %q %v", b, k, err)
	}
	for _, bad := range []string{"http://x/y", "s3://", "s3://bucket", "s3://bucket/"} {
		if _, _, err := parseS3URI(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}
