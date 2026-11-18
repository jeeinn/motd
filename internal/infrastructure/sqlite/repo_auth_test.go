package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

// P0-14 回归集：ssh_keys 查库三状态 + Resolver 端到端 + 参数化查询。
// 用真实 SQLite 与真实迁移，不依赖 mock 与种子数据以外的假设。

// 指纹常量用 strings.Repeat 生成，不手写 43 个字符：
// 手数长度一旦偏差就会被 ValidFingerprint 拦下（首轮全量回归实测踩到过）。
var (
	authFPBound   = "SHA256:" + strings.Repeat("A", 43)
	authFPRevoked = "SHA256:" + strings.Repeat("B", 43)
	authFPFree    = "SHA256:" + strings.Repeat("C", 43)
)

func newAuthEnv(t *testing.T) (*Pools, int64) {
	t.Helper()
	ctx := context.Background()
	pools := openTestPools(t)
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	now := testBaseTime.Unix()
	res, err := pools.writer.ExecContext(ctx,
		`INSERT INTO users (username, status, created_at, updated_at) VALUES (?, 'active', ?, ?)`,
		"keyowner", now, now)
	if err != nil {
		t.Fatalf("插入测试用户: %v", err)
	}
	uid, _ := res.LastInsertId()
	return pools, uid
}

func insertSSHKey(t *testing.T, pools *Pools, uid int64, fp string, revoked bool) {
	t.Helper()
	var revokedAt any
	if revoked {
		revokedAt = time.Now().Unix()
	}
	_, err := pools.writer.ExecContext(context.Background(),
		`INSERT INTO ssh_keys (user_id, fingerprint, public_key, created_at, revoked_at)
		 VALUES (?, ?, ?, ?, ?)`,
		uid, fp, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA placeholder", testBaseTime.Unix(), revokedAt)
	if err != nil {
		t.Fatalf("插入 ssh_keys(%s): %v", fp, err)
	}
}

func TestSSHKeyLookupThreeStates(t *testing.T) {
	ctx := context.Background()
	pools, uid := newAuthEnv(t)
	insertSSHKey(t, pools, uid, authFPBound, false)
	insertSSHKey(t, pools, uid, authFPRevoked, true)

	lookup := NewSSHKeyLookup(pools)
	for _, tc := range []struct {
		name        string
		fp          string
		wantFound   bool
		wantRevoked bool
	}{
		{"已绑定未吊销", authFPBound, true, false},
		{"已绑定已吊销", authFPRevoked, true, true},
		{"未绑定", authFPFree, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := lookup.Lookup(ctx, tc.fp)
			if err != nil {
				t.Fatal(err)
			}
			if st.Found != tc.wantFound || st.Revoked != tc.wantRevoked {
				t.Fatalf("KeyStatus = %+v，期望 found=%v revoked=%v", st, tc.wantFound, tc.wantRevoked)
			}
			if tc.wantFound && st.UserID != uid {
				t.Fatalf("UserID = %d，期望 %d", st.UserID, uid)
			}
		})
	}
}

func TestSSHKeyLookupIsParameterized(t *testing.T) {
	ctx := context.Background()
	pools, uid := newAuthEnv(t)
	insertSSHKey(t, pools, uid, authFPBound, false)

	lookup := NewSSHKeyLookup(pools)
	for _, injection := range []string{
		"' OR 1=1 --",
		authFPBound + "' OR '1'='1",
		"'; DROP TABLE ssh_keys; --",
	} {
		st, err := lookup.Lookup(ctx, injection)
		if err != nil {
			t.Fatalf("%q 不应导致查询报错: %v", injection, err)
		}
		if st.Found {
			t.Fatalf("%q 不应命中任何绑定", injection)
		}
	}
	// 注入串没有破坏表结构，原绑定仍可正常解析。
	st, err := lookup.Lookup(ctx, authFPBound)
	if err != nil || !st.Found {
		t.Fatalf("注入尝试后原绑定应仍可查询，status=%+v err=%v", st, err)
	}
}

func TestResolverWithRealSQLite(t *testing.T) {
	ctx := context.Background()
	pools, uid := newAuthEnv(t)
	insertSSHKey(t, pools, uid, authFPBound, false)
	insertSSHKey(t, pools, uid, authFPRevoked, true)

	resolver := auth.NewResolver(NewSSHKeyLookup(pools))
	for _, tc := range []struct {
		name     string
		fp       string
		wantKind auth.Kind
		wantErr  error
	}{
		{"未绑定→访客", authFPFree, auth.KindGuest, nil},
		{"已绑定→用户", authFPBound, auth.KindUser, nil},
		{"已吊销→拒绝", authFPRevoked, auth.KindGuest, auth.ErrKeyRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := resolver.Resolve(ctx, tc.fp)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v，期望 %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if p != (auth.Principal{}) {
					t.Fatalf("拒绝时必须返回零值，实际 %+v", p)
				}
				return
			}
			if p.Kind != tc.wantKind {
				t.Fatalf("Kind = %v，期望 %v", p.Kind, tc.wantKind)
			}
			if tc.wantKind == auth.KindUser && p.UserID != uid {
				t.Fatalf("UserID = %d，期望 %d", p.UserID, uid)
			}
			if p.KeyFingerprint != tc.fp {
				t.Fatalf("指纹未原样带回: %q", p.KeyFingerprint)
			}
		})
	}
}
