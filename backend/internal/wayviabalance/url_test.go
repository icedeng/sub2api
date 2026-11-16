package wayviabalance

import "testing"

func TestNormalizeBalanceRoot(t *testing.T) {
	for _, tt := range []struct{ input, root string }{
		{"https://EXAMPLE.com:443/", "https://example.com"},
		{"https://example.com/v1/", "https://example.com"},
		{"https://example.com/tenant/v1/", "https://example.com/tenant"},
		{"https://example.com/tenant", "https://example.com/tenant"},
		{"https://example.com:8443/proxy/v1", "https://example.com:8443/proxy"},
		{"https://[2001:db8::1]:443/tenant/v1", "https://[2001:db8::1]/tenant"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			root, err := normalizeRoot(tt.input)
			if err != nil || root != tt.root {
				t.Fatalf("root=%s err=%v want=%s", root, err, tt.root)
			}
			if got := endpointURL(root, "/api/usage/token/"); got != tt.root+"/api/usage/token/" {
				t.Fatal(got)
			}
			if endpointURL(root, "/v1/usage") == endpointURL(root, "/usage") {
				t.Fatal("fallback path must differ")
			}
		})
	}
}

func TestNormalizeBalanceRootRejectsUnsafeStructure(t *testing.T) {
	for _, input := range []string{
		"", "http://example.com", "https://user:pass@example.com", "https://example.com?key=secret", "https://example.com?", "https://example.com#", "https://example.com/#fragment", "https://example.com/%2e%2e/v1", "https://example.com/tenant%2fv1", "https://example.com/../v1", "https://example.com/./v1", "https://example.com//v1", "https://example.com/tenant\\evil", "https://example.com/v1\n/more", "https:///v1", "https://example.com:0/v1", "https://example.com:99999/v1", "https://example.com:/v1", "https://[fe80::1%25eth0]/v1", "https://foo..example.com", "https://example.com/%252e%252e/v1",
	} {
		t.Run(input, func(t *testing.T) {
			if got, err := normalizeRoot(input); err == nil {
				t.Fatalf("accepted %q as %q", input, got)
			}
		})
	}
}
