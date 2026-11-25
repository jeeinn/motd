package forum

// R-15 / ADR-079 管理员白名单热重载的用例层回归：
// SetAdminWhitelist 原子换入后治理权立即变更；非法指纹被拒且旧名单不变。

import "testing"

const (
	testAdminFP = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	testOtherFP = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
)

func TestSetAdminWhitelistSwitchesAuthority(t *testing.T) {
	s := &Service{}
	s.adminWhitelist.Store(&adminWhitelistSnapshot{fingerprints: map[string]struct{}{testAdminFP: {}}})
	actor := Actor{Fingerprint: testAdminFP}

	// 初始在名单内：requireAdmin 放行。
	if err := s.requireAdmin(actor); err != nil {
		t.Fatalf("初始白名单应放行: %v", err)
	}
	// 换入不含该指纹的新名单：立即拒绝。
	if err := s.SetAdminWhitelist(map[string]struct{}{testOtherFP: {}}); err != nil {
		t.Fatalf("换入合法名单: %v", err)
	}
	if err := s.requireAdmin(actor); err == nil {
		t.Fatal("旧管理员在新名单生效后应被拒")
	}
	if err := s.requireAdmin(Actor{Fingerprint: testOtherFP}); err != nil {
		t.Fatalf("新名单内指纹应放行: %v", err)
	}
}

func TestSetAdminWhitelistRejectsInvalidAndKeepsOld(t *testing.T) {
	s := &Service{}
	s.adminWhitelist.Store(&adminWhitelistSnapshot{fingerprints: map[string]struct{}{testAdminFP: {}}})
	actor := Actor{Fingerprint: testAdminFP}

	bad := map[string]struct{}{"not-a-fingerprint": {}}
	if err := s.SetAdminWhitelist(bad); err == nil {
		t.Fatal("非法指纹应被拒绝")
	}
	// 拒绝后旧名单仍在，管理员没被锁在门外。
	if err := s.requireAdmin(actor); err != nil {
		t.Fatalf("非法重载后旧白名单必须保留: %v", err)
	}
}

func TestSetAdminWhitelistEmptyClearsAll(t *testing.T) {
	s := &Service{}
	s.adminWhitelist.Store(&adminWhitelistSnapshot{fingerprints: map[string]struct{}{testAdminFP: {}}})
	if err := s.SetAdminWhitelist(map[string]struct{}{}); err != nil {
		t.Fatalf("空名单是合法的（撤销全部管理员）: %v", err)
	}
	if err := s.requireAdmin(Actor{Fingerprint: testAdminFP}); err == nil {
		t.Fatal("空名单下所有治理请求都应拒绝")
	}
}

// 并发读安全：多个 goroutine 在换名单的同时判定，不应触发 race 或 panic。
func TestAdminWhitelistConcurrentReload(t *testing.T) {
	s := &Service{}
	s.adminWhitelist.Store(&adminWhitelistSnapshot{fingerprints: map[string]struct{}{testAdminFP: {}}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			_ = s.SetAdminWhitelist(map[string]struct{}{testAdminFP: {}, testOtherFP: {}})
			_ = s.SetAdminWhitelist(map[string]struct{}{testAdminFP: {}})
		}
	}()
	for i := 0; i < 1000; i++ {
		_ = s.requireAdmin(Actor{Fingerprint: testAdminFP})
	}
	<-done
}
