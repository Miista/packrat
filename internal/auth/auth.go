// Package auth implements the first-launch admin setup and session-cookie
// authentication used to gate every route once an admin account exists,
// following the same pattern as Sonarr/Radarr/etc.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/miista/packrat/internal/store"
)

const (
	sessionCookieName = "packrat_session"
	sessionTTL        = 30 * 24 * time.Hour
)

var (
	ErrNoAdmin      = errors.New("no admin account configured")
	ErrAdminExists  = errors.New("admin account already exists")
	ErrInvalidCreds = errors.New("invalid username or password")
	ErrWeakPassword = errors.New("password must be at least 8 characters")
)

// Manager owns session state and the HMAC key used to sign session tokens.
// Both are persisted via store (see store.Auth), so a process restart
// (container rebuild, image update, etc.) does not sign the user out — the
// cookie only ever carries an opaque signed session ID, so nothing about
// the admin account itself is exposed to the client.
type Manager struct {
	store *store.Store

	mu       sync.RWMutex
	hmacKey  []byte
	sessions map[string]time.Time // sessionID -> expiry
}

// NewManager creates an auth manager backed by st, loading a previously
// persisted HMAC key and session set if one exists, or generating and
// persisting a fresh key otherwise.
func NewManager(st *store.Store) (*Manager, error) {
	m := &Manager{store: st, sessions: map[string]time.Time{}}

	var persistedKey []byte
	var persistedSessions map[string]int64
	st.View(func(s store.State) {
		persistedKey = append([]byte(nil), s.Auth.HMACKey...)
		persistedSessions = s.Auth.Sessions
	})

	if len(persistedKey) == 0 {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		persistedKey = key
		if err := st.Update(func(s *store.State) {
			s.Auth.HMACKey = key
		}); err != nil {
			return nil, err
		}
	}
	m.hmacKey = persistedKey

	now := time.Now()
	for id, expiry := range persistedSessions {
		t := time.Unix(expiry, 0)
		if t.After(now) {
			m.sessions[id] = t
		}
	}

	return m, nil
}

// persistSessionsLocked writes the current in-memory session set to store.
// Caller must hold m.mu.
func (m *Manager) persistSessionsLocked() {
	snapshot := make(map[string]int64, len(m.sessions))
	for id, expiry := range m.sessions {
		snapshot[id] = expiry.Unix()
	}
	_ = m.store.Update(func(s *store.State) {
		s.Auth.Sessions = snapshot
	})
}

// Disabled reports whether auth is disabled via PACKRAT_AUTH_DISABLED.
// Callers should check this before enforcing RequireSession.
func (m *Manager) HasAdmin() bool {
	has := false
	m.store.View(func(s store.State) {
		has = s.Admin != nil
	})
	return has
}

// CreateAdmin sets the first (and only) admin account. Fails if one already
// exists — use ChangePassword to update credentials afterward.
//
// The existence check and the write happen inside one store.Update call, so
// two concurrent first-launch setup requests can't both pass the check and
// race to overwrite each other's admin account.
func (m *Manager) CreateAdmin(username, password string) error {
	if len(password) < 8 {
		return ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	alreadyExists := false
	updateErr := m.store.Update(func(s *store.State) {
		if s.Admin != nil {
			alreadyExists = true
			return
		}
		s.Admin = &store.Admin{Username: username, PasswordHash: string(hash)}
	})
	if alreadyExists {
		return ErrAdminExists
	}
	return updateErr
}

// Authenticate verifies a username/password pair against the stored admin
// account and, on success, creates a new session and returns its token.
func (m *Manager) Authenticate(username, password string) (token string, err error) {
	var admin *store.Admin
	m.store.View(func(s store.State) {
		if s.Admin != nil {
			cp := *s.Admin
			admin = &cp
		}
	})
	if admin == nil {
		return "", ErrNoAdmin
	}
	// Constant-time username compare to avoid leaking which part failed;
	// bcrypt.CompareHashAndPassword is already constant-time for the hash.
	if subtle.ConstantTimeCompare([]byte(username), []byte(admin.Username)) != 1 {
		// Still run bcrypt to keep timing consistent whether or not the
		// username matched.
		_ = bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(password))
		return "", ErrInvalidCreds
	}
	if err := bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(password)); err != nil {
		return "", ErrInvalidCreds
	}
	return m.newSession(), nil
}

func (m *Manager) newSession() string {
	id := randomID(32)
	signed := m.sign(id)
	m.mu.Lock()
	m.sessions[id] = time.Now().Add(sessionTTL)
	m.persistSessionsLocked()
	m.mu.Unlock()
	return signed
}

func (m *Manager) sign(id string) string {
	mac := hmac.New(sha256.New, m.hmacKey)
	mac.Write([]byte(id))
	sig := hex.EncodeToString(mac.Sum(nil))
	return id + "." + sig
}

func (m *Manager) verify(token string) (id string, ok bool) {
	dot := -1
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		return "", false
	}
	id, gotSig := token[:dot], token[dot+1:]
	mac := hmac.New(sha256.New, m.hmacKey)
	mac.Write([]byte(id))
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(gotSig), []byte(wantSig)) {
		return "", false
	}
	m.mu.RLock()
	expiry, exists := m.sessions[id]
	m.mu.RUnlock()
	if !exists || time.Now().After(expiry) {
		return "", false
	}
	return id, true
}

func (m *Manager) invalidate(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.persistSessionsLocked()
	m.mu.Unlock()
}

// SetSessionCookie writes a signed, httpOnly session cookie for token.
func (m *Manager) SetSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
}

// ClearSessionCookie logs the current request's session out.
func (m *Manager) ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if id, ok := m.verify(c.Value); ok {
			m.invalidate(id)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// Authenticated reports whether the request carries a valid session cookie.
func (m *Manager) Authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	_, ok := m.verify(c.Value)
	return ok
}

func randomID(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}
