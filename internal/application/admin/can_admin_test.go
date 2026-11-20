package admin

import (
	"strings"
	"testing"
)

const testAdminFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestCanAdmin(t *testing.T) {
	other := "SHA256:" + strings.Repeat("B", 43)
	wl := map[string]struct{}{testAdminFP: {}, other: {}}
	cases := []struct {
		name string
		fp   string
		list map[string]struct{}
		want bool
	}{
		{"白名单内", testAdminFP, wl, true},
		{"第二把钥匙", other, wl, true},
		{"名单外", "SHA256:" + strings.Repeat("C", 43), wl, false},
		{"空指纹", "", wl, false},
		{"空白名单", testAdminFP, nil, false},
		{"空名单", testAdminFP, map[string]struct{}{}, false},
		{"非法格式", "ssh-rsa AAAAexample", map[string]struct{}{"ssh-rsa AAAAexample": {}}, false},
		{"指纹正文大小写敏感", testAdminFP, map[string]struct{}{"SHA256:" + strings.Repeat("a", 43): {}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanAdmin(tc.fp, tc.list); got != tc.want {
				t.Fatalf("CanAdmin(%q) = %v，期望 %v", tc.fp, got, tc.want)
			}
		})
	}
}

func TestCanAdminDoesNotMutateWhitelist(t *testing.T) {
	wl := map[string]struct{}{testAdminFP: {}}
	_ = CanAdmin(testAdminFP, wl)
	_ = CanAdmin("SHA256:" + strings.Repeat("C", 43), wl)
	if len(wl) != 1 {
		t.Fatalf("谓词不得改写白名单，实际 %v", wl)
	}
}
