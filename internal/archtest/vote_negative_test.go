package archtest

// 投票守卫的负向注入：证明每条规则拿掉后都会报警。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardDetectsTouchThreadInVotePath(t *testing.T) {
	tmp := t.TempDir()
	bad := `package forum

func bad(tx WriteTx) {
	tx.TouchThread(ctx, 1, now, 0)
}
`
	if err := os.WriteFile(filepath.Join(tmp, "vote.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "other.go"), []byte(`package forum
func applyVote(tx WriteTx) error {
	tx.Write(ctx, func(tx WriteTx) error {
		tx.GetVote(ctx, 1, 1, 1)
		tx.UpsertVote(ctx, 1, 1, 1, 1, now)
		tx.DeleteVote(ctx, 1, 1, 1)
		tx.AdjustScore(ctx, 1, 1)
		tx.AppendOutbox(ctx, ev)
		return nil
	})
	return nil
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectVoteAppIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "不得调用 TouchThread") {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入的 TouchThread 调用未被检出: %v", issues)
	}
}

func TestGuardDetectsMissingVoteWriteSteps(t *testing.T) {
	tmp := t.TempDir()
	bad := `package forum
func applyVote(tx WriteTx) error {
	return tx.AdjustScore(ctx, 1, 1)
}
`
	if err := os.WriteFile(filepath.Join(tmp, "vote.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectVoteAppIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(issues, "\n")
	for _, want := range []string{"GetVote", "UpsertVote", "DeleteVote", "AppendOutbox"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少写步骤 %q 未被检出: %s", want, joined)
		}
	}
}

func TestGuardDetectsLastReplyAtInAdjustScore(t *testing.T) {
	tmp := t.TempDir()
	bad := `package sqlite
func (w *sqlWriteTx) AdjustScore(ctx context.Context, id, delta int64) error {
	_, err := w.tx.ExecContext(ctx, "UPDATE threads SET last_reply_at = ?, score = score + ? WHERE id = ?", now, delta, id)
	return err
}
func (w *sqlWriteTx) UpsertVote() error {
	_, err := w.tx.Exec("INSERT INTO votes VALUES (?,?,?,?,?) ON CONFLICT(user_id,target_type,target_id) DO UPDATE SET value=excluded.value")
	return err
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectVoteInfraIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "不得出现 last_reply_at") {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入的 last_reply_at 未被检出: %v", issues)
	}
}

func TestGuardDetectsUpsertWithoutConflictClause(t *testing.T) {
	tmp := t.TempDir()
	bad := `package sqlite
func (w *sqlWriteTx) AdjustScore() error {
	_, err := w.tx.Exec("UPDATE threads SET score = score + ? WHERE id = ?", 1, 1)
	return err
}
func (w *sqlWriteTx) UpsertVote() error {
	_, err := w.tx.Exec("INSERT INTO votes VALUES (?,?,?,?,?)")
	return err
}
`
	if err := os.WriteFile(filepath.Join(tmp, "bad.go"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := collectVoteInfraIssues(tmp)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue, "缺少 ON CONFLICT") {
			found = true
		}
	}
	if !found {
		t.Fatalf("注入的无 ON CONFLICT 写入未被检出: %v", issues)
	}
}
