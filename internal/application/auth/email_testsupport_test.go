package auth

import (
	"context"
	"sync"
	"time"
)

// 以下 fake 全部在进程内，绝不触网、不碰 SQLite；凭据用占位串。

type fakeEmailToken struct {
	id        int64
	email     string
	codeHash  string
	purpose   string
	expiresAt time.Time
	attempts  int
	consumed  bool
}

type fakeEmailUser struct {
	id           int64
	username     string
	status       string
	primaryEmail string
}

type fakeEmailStore struct {
	mu                 sync.Mutex
	tokens             []*fakeEmailToken
	users              []*fakeEmailUser
	identities         map[string]int64 // email(lower) -> userID
	creds              map[int64]string
	nextID             int64
	writes             int
	reads              int // 只读端口调用计数，供「预检不写库」断言
	lastWriteCommitted bool
}

func newFakeEmailStore() *fakeEmailStore {
	return &fakeEmailStore{
		identities: make(map[string]int64),
		creds:      make(map[int64]string),
		nextID:     100,
	}
}

func (s *fakeEmailStore) Write(ctx context.Context, fn func(EmailWriteTx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if err := fn(&fakeEmailTx{s: s}); err != nil {
		s.lastWriteCommitted = false
		return err
	}
	s.lastWriteCommitted = true
	return nil
}

func (s *fakeEmailStore) FindCredentialByEmail(ctx context.Context, email string) (CredentialView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ok := s.identities[email]
	if !ok {
		return CredentialView{}, ErrLoginNotFound
	}
	for _, u := range s.users {
		if u.id == uid {
			return CredentialView{
				UserID: u.id, Username: u.username, Status: u.status,
				PasswordHash: s.creds[u.id],
			}, nil
		}
	}
	return CredentialView{}, ErrLoginNotFound
}

func (s *fakeEmailStore) seedIdentity(email string, userID int64, passwordHash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.identities[email] = userID
	s.creds[userID] = passwordHash
	s.users = append(s.users, &fakeEmailUser{id: userID, username: "seeduser", status: "active", primaryEmail: email})
	if userID >= s.nextID {
		s.nextID = userID + 1
	}
}

type fakeEmailTx struct{ s *fakeEmailStore }

func (t *fakeEmailTx) UsernameExists(_ context.Context, name string) (bool, error) {
	for _, u := range t.s.users {
		if u.username == name {
			return true, nil
		}
	}
	return false, nil
}

func (t *fakeEmailTx) InsertUser(_ context.Context, a NewAccount) (int64, error) {
	t.s.nextID++
	u := &fakeEmailUser{id: t.s.nextID, username: a.Username, status: string(a.Status), primaryEmail: a.PrimaryEmail}
	t.s.users = append(t.s.users, u)
	return u.id, nil
}

func (t *fakeEmailTx) InsertIdentity(_ context.Context, i NewIdentity) error {
	t.s.identities[i.ProviderUID] = i.UserID
	return nil
}

func (t *fakeEmailTx) FindAccountByEmail(_ context.Context, email string) (AccountView, error) {
	for _, u := range t.s.users {
		if u.primaryEmail == email {
			return AccountView{UserID: u.id, Username: u.username}, nil
		}
	}
	return AccountView{}, ErrLoginNotFound
}

func (t *fakeEmailTx) InsertEmailToken(_ context.Context, n NewEmailToken) (int64, error) {
	t.s.nextID++
	t.s.tokens = append(t.s.tokens, &fakeEmailToken{
		id: t.s.nextID, email: n.Email, codeHash: n.CodeHash,
		purpose: n.Purpose, expiresAt: n.ExpiresAt,
	})
	return t.s.nextID, nil
}

// fakeLatestActiveToken 与写事务里的 FindLatestActiveToken 共用同一投影逻辑，
// 保证预检读池与正式 Verify 看到的是同一口径。
func fakeLatestActiveToken(tokens []*fakeEmailToken, email string) (EmailTokenView, error) {
	for i := len(tokens) - 1; i >= 0; i-- {
		tok := tokens[i]
		if tok.email == email && !tok.consumed && (tok.purpose == EmailPurposeRegister || tok.purpose == EmailPurposeRecover) {
			return EmailTokenView{
				ID: tok.id, Email: tok.email, CodeHash: tok.codeHash,
				Purpose: tok.purpose, ExpiresAt: tok.expiresAt, Attempts: tok.attempts,
			}, nil
		}
	}
	return EmailTokenView{}, ErrNoPendingToken
}

// FindLatestActiveTokenReadOnly 走「读池」：只记 reads，绝不递增 writes。
func (s *fakeEmailStore) FindLatestActiveTokenReadOnly(_ context.Context, email string) (EmailTokenView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return fakeLatestActiveToken(s.tokens, email)
}

func (t *fakeEmailTx) FindLatestActiveToken(_ context.Context, email string) (EmailTokenView, error) {
	return fakeLatestActiveToken(t.s.tokens, email)
}

func (t *fakeEmailTx) ConsumeEmailToken(_ context.Context, id int64, _ time.Time) error {
	for _, tok := range t.s.tokens {
		if tok.id == id {
			if tok.consumed {
				return ErrTokenConsumed
			}
			tok.consumed = true
			return nil
		}
	}
	return ErrTokenConsumed
}

func (t *fakeEmailTx) NoteEmailTokenAttempt(_ context.Context, id int64) error {
	for _, tok := range t.s.tokens {
		if tok.id == id {
			tok.attempts++
			return nil
		}
	}
	return ErrTokenConsumed
}

func (t *fakeEmailTx) FindUserByEmailIdentity(_ context.Context, email string) (AccountView, error) {
	uid, ok := t.s.identities[email]
	if !ok {
		return AccountView{}, ErrLoginNotFound
	}
	for _, u := range t.s.users {
		if u.id == uid {
			return AccountView{UserID: u.id, Username: u.username}, nil
		}
	}
	return AccountView{}, ErrLoginNotFound
}

func (t *fakeEmailTx) UpsertLocalCredential(_ context.Context, userID int64, hash string, _ time.Time) error {
	t.s.creds[userID] = hash
	return nil
}

// fakeEmailSender 记录调用次数与最近参数；可注入失败，且能断言「调用发生在写提交后」。
type fakeEmailSender struct {
	mu       sync.Mutex
	calls    int
	lastTo   string
	lastBody string
	fail     bool
	store    *fakeEmailStore
}

func (f *fakeEmailSender) Send(_ context.Context, to, _, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastTo, f.lastBody = to, body
	if f.store != nil && !f.store.lastWriteCommitted {
		return errSendInWriteTx
	}
	if f.fail {
		return errFakeSend
	}
	return nil
}

var (
	errFakeSend      = &fakeError{"fake send failure with AUTH_SECRET"}
	errSendInWriteTx = &fakeError{"SEND_INSIDE_WRITE_TX"}
)

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }

type fakeEmailRevoker struct {
	mu     sync.Mutex
	calls  int
	userID int64
	closed int
}

func (r *fakeEmailRevoker) RevokeUserSessions(_ context.Context, userID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.userID = userID
	return r.closed
}

func (r *fakeEmailRevoker) stats() (int, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.userID
}
