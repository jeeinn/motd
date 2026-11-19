package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeeinn/motd/internal/application/auth"
)

func newEmailReposTest(t *testing.T) (*Pools, *Writer, *EmailRepos) {
	t.Helper()
	ctx := context.Background()
	pools, err := Open(ctx, filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	if _, err := Migrate(ctx, pools); err != nil {
		t.Fatal(err)
	}
	w := pools.NewWriter()
	w.Start()
	t.Cleanup(w.Stop)
	return pools, w, NewEmailRepos(pools, w)
}

func TestEmailReposTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	pools, _, repos := newEmailReposTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	hash := "deadbeef-hash-not-plaintext"

	var tokenID int64
	err := repos.Write(ctx, func(tx auth.EmailWriteTx) error {
		var err error
		tokenID, err = tx.InsertEmailToken(ctx, auth.NewEmailToken{
			Email: "user@example.com", CodeHash: hash, Purpose: auth.EmailPurposeRegister,
			ExpiresAt: now.Add(10 * time.Minute), CreatedAt: now,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = repos.Write(ctx, func(tx auth.EmailWriteTx) error {
		got, err := tx.FindLatestActiveToken(ctx, "USER@example.com")
		if err != nil {
			return err
		}
		if got.ID != tokenID || got.CodeHash != hash || got.Attempts != 0 || got.Consumed {
			return fmt.Errorf("读到的 token 不对: %+v", got)
		}
		if err := tx.NoteEmailTokenAttempt(ctx, tokenID); err != nil {
			return err
		}
		return tx.ConsumeEmailToken(ctx, tokenID, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	// 二次消费在独立事务里：条件更新 0 行 → ErrTokenConsumed，且整事务回滚不影响首次消费。
	err = repos.Write(ctx, func(tx auth.EmailWriteTx) error {
		return tx.ConsumeEmailToken(ctx, tokenID, now)
	})
	if !errors.Is(err, auth.ErrTokenConsumed) {
		t.Fatalf("重复消费必须报 ErrTokenConsumed，实际 %v", err)
	}

	// 库里只有散列，没有明文码列，也没有明文值。
	varcode := "123456"
	var stored string
	if err := pools.QueryRowContext(ctx, `SELECT code_hash FROM email_tokens WHERE id = ?`, tokenID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != hash || strings.Contains(stored, varcode) {
		t.Fatalf("code_hash 只能是散列: %q", stored)
	}
	var consumed int
	if err := pools.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_tokens WHERE id = ? AND consumed_at IS NOT NULL`, tokenID).Scan(&consumed); err != nil || consumed != 1 {
		t.Fatalf("消费应置 consumed_at 而非删行: consumed=%d err=%v", consumed, err)
	}
}

// 并发抢消费：N 个写事务同时 UPDATE 同一行，只有 1 个 RowsAffected==1。
func TestEmailReposConcurrentConsumeHasSingleWinner(t *testing.T) {
	ctx := context.Background()
	_, _, repos := newEmailReposTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	var tokenID int64
	if err := repos.Write(ctx, func(tx auth.EmailWriteTx) error {
		var err error
		tokenID, err = tx.InsertEmailToken(ctx, auth.NewEmailToken{
			Email: "race@example.com", CodeHash: "h", Purpose: auth.EmailPurposeRegister,
			ExpiresAt: now.Add(time.Minute), CreatedAt: now,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = repos.Write(ctx, func(tx auth.EmailWriteTx) error {
				return tx.ConsumeEmailToken(ctx, tokenID, now)
			})
		}(i)
	}
	wg.Wait()
	win := 0
	for _, err := range errs {
		switch {
		case err == nil:
			win++
		case errors.Is(err, auth.ErrTokenConsumed):
		default:
			t.Fatalf("意外错误: %v", err)
		}
	}
	if win != 1 {
		t.Fatalf("并发抢消费应恰好 1 个成功，实际 %d", win)
	}
}

func TestEmailReposAccountAndCredential(t *testing.T) {
	ctx := context.Background()
	_, _, repos := newEmailReposTest(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	var userID int64
	err := repos.Write(ctx, func(tx auth.EmailWriteTx) error {
		ID, err := tx.InsertUser(ctx, auth.NewAccount{
			Username: "alice", DisplayName: "alice", PrimaryEmail: "alice@example.com", Status: "active", CreatedAt: now,
		})
		if err != nil {
			return err
		}
		userID = ID
		if err := tx.InsertIdentity(ctx, auth.NewIdentity{
			UserID: ID, Provider: auth.ProviderEmail, ProviderUID: "alice@example.com",
			Email: "alice@example.com", EmailVerified: true, CreatedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.UpsertLocalCredential(ctx, ID, "first-hash", now); err != nil {
			return err
		}
		return tx.UpsertLocalCredential(ctx, ID, "second-hash", now.Add(time.Second))
	})
	if err != nil {
		t.Fatal(err)
	}

	cred, err := repos.FindCredentialByEmail(ctx, "ALICE@example.com")
	if err != nil || cred.UserID != userID || cred.PasswordHash != "second-hash" {
		t.Fatalf("凭证读取/覆盖失败: %+v err=%v", cred, err)
	}
	err = repos.Write(ctx, func(tx auth.EmailWriteTx) error {
		acct, err := tx.FindUserByEmailIdentity(ctx, "alice@example.com")
		if err != nil || acct.UserID != userID {
			return fmt.Errorf("按邮箱身份查号失败: %+v %v", acct, err)
		}
		_, err = tx.FindUserByEmailIdentity(ctx, "nobody@example.com")
		if !errors.Is(err, auth.ErrLoginNotFound) {
			return fmt.Errorf("未命中应为 ErrLoginNotFound，实际 %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
