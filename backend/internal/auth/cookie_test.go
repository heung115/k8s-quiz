package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/config"
	"github.com/k8s-quiz/backend/pkg/models"
)

// ---- in-memory RefreshTokenStore (AUTH-5 unit tests without a DB) ----

type memTokenRow struct {
	userID    string
	familyID  string
	used      bool
	expiresAt time.Time
}

type memRefreshStore struct {
	mu     sync.Mutex
	nextID int
	rows   map[string]*memTokenRow // key: tokenHash
}

func newMemRefreshStore() *memRefreshStore {
	return &memRefreshStore{rows: make(map[string]*memTokenRow)}
}

func (m *memRefreshStore) CreateRefreshToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time, familyID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if familyID == "" {
		m.nextID++
		familyID = "family-" + string(rune('a'+m.nextID-1))
	}
	m.rows[tokenHash] = &memTokenRow{userID: userID, familyID: familyID, expiresAt: expiresAt}
	return familyID, nil
}

func (m *memRefreshStore) FindRefreshToken(ctx context.Context, tokenHash string) (*RefreshTokenRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[tokenHash]
	if !ok {
		return nil, errors.New("not found")
	}
	return &RefreshTokenRecord{
		UserID: r.userID, FamilyID: r.familyID, Used: r.used, ExpiresAt: r.expiresAt,
	}, nil
}

func (m *memRefreshStore) MarkRefreshTokenUsed(ctx context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.rows[tokenHash]; ok {
		r.used = true
	}
	return nil
}

func (m *memRefreshStore) DeleteRefreshToken(ctx context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, tokenHash)
	return nil
}

func (m *memRefreshStore) DeleteRefreshTokenFamily(ctx context.Context, familyID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, r := range m.rows {
		if r.familyID == familyID {
			delete(m.rows, h)
		}
	}
	return nil
}

func (m *memRefreshStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

// ---- fake user repo ----

type fakeUserRepo struct{ user *models.User }

func (f *fakeUserRepo) FindByGithubID(ctx context.Context, githubID int64) (*models.User, error) {
	return nil, errors.New("not found")
}
func (f *fakeUserRepo) FindByID(ctx context.Context, id string) (*models.User, error) {
	if f.user != nil && f.user.ID == id {
		return f.user, nil
	}
	return nil, errors.New("not found")
}
func (f *fakeUserRepo) Create(ctx context.Context, u *models.User) error { return nil }
func (f *fakeUserRepo) Update(ctx context.Context, u *models.User) error { return nil }

var cookieTestUser = &models.User{ID: "u1", Username: "tester", Role: models.RoleUser}

// newCookieTestService builds a Service wired to the in-memory store.
func newCookieTestService(store *memRefreshStore) *Service {
	cfg := &config.Config{FrontendURL: "http://localhost:5173", JWTSecret: "test-secret"}
	svc := NewService(cfg, nil, &fakeUserRepo{user: cookieTestUser})
	svc.refreshStore = store
	return svc
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ---- cookie shape (FRONT-3 contract) ----

func TestSetTokenCookiesShape(t *testing.T) {
	h := newTestHandler() // http://localhost:5173 → Secure=false
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.setTokenCookies(c, "acc-tok", "ref-tok")

	cookies := w.Result().Cookies()
	acc := findCookie(cookies, AccessTokenCookie)
	if acc == nil {
		t.Fatal("access_token cookie missing")
	}
	if acc.Value != "acc-tok" || acc.Path != "/" || acc.MaxAge != 900 || !acc.HttpOnly || acc.Secure {
		t.Errorf("access_token cookie wrong: %+v", acc)
	}
	if acc.SameSite != http.SameSiteLaxMode {
		t.Errorf("access_token SameSite = %v, want Lax", acc.SameSite)
	}

	ref := findCookie(cookies, RefreshTokenCookie)
	if ref == nil {
		t.Fatal("refresh_token cookie missing")
	}
	if ref.Value != "ref-tok" || ref.Path != "/api/auth" || ref.MaxAge != 604800 || !ref.HttpOnly || ref.Secure {
		t.Errorf("refresh_token cookie wrong: %+v", ref)
	}
}

func TestSetTokenCookiesSecureForHTTPS(t *testing.T) {
	cfg := &config.Config{FrontendURL: "https://quiz.example.com"}
	h := NewHandler(NewService(cfg, nil, nil))
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.setTokenCookies(c, "a", "b")

	for _, name := range []string{AccessTokenCookie, RefreshTokenCookie} {
		ck := findCookie(w.Result().Cookies(), name)
		if ck == nil || !ck.Secure {
			t.Errorf("%s must be Secure when FRONTEND_URL is https: %+v", name, ck)
		}
	}
}

func TestClearTokenCookies(t *testing.T) {
	h := newTestHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	h.clearTokenCookies(c)

	acc := findCookie(w.Result().Cookies(), AccessTokenCookie)
	ref := findCookie(w.Result().Cookies(), RefreshTokenCookie)
	if acc == nil || ref == nil {
		t.Fatal("expected both clearing cookies")
	}
	if acc.MaxAge >= 0 || ref.MaxAge >= 0 {
		t.Errorf("clearing cookies must expire (Max-Age<0): acc=%d ref=%d", acc.MaxAge, ref.MaxAge)
	}
	if acc.Path != "/" || ref.Path != "/api/auth" {
		t.Errorf("clearing cookies must keep original paths: acc=%s ref=%s", acc.Path, ref.Path)
	}
}

// ---- refresh rotation + reuse detection (AUTH-5) ----

func TestRefreshRotatesInSameFamily(t *testing.T) {
	store := newMemRefreshStore()
	svc := newCookieTestService(store)

	old, err := svc.generateRefreshToken(context.Background(), cookieTestUser, "")
	if err != nil {
		t.Fatalf("seed token: %v", err)
	}
	oldHash := hashToken(old)
	family := store.rows[oldHash].familyID

	access, fresh, u, err := svc.RefreshAccessToken(context.Background(), old)
	if err != nil {
		t.Fatalf("RefreshAccessToken failed: %v", err)
	}
	if access == "" || fresh == "" || fresh == old {
		t.Error("expected fresh distinct tokens")
	}
	if u == nil || u.ID != "u1" {
		t.Errorf("expected user in response, got %+v", u)
	}
	if !store.rows[oldHash].used {
		t.Error("old token must be marked used, not deleted")
	}
	freshRow := store.rows[hashToken(fresh)]
	if freshRow == nil || freshRow.familyID != family {
		t.Error("new token must join the same family")
	}
}

func TestRefreshReuseRevokesFamily(t *testing.T) {
	store := newMemRefreshStore()
	svc := newCookieTestService(store)

	old, _ := svc.generateRefreshToken(context.Background(), cookieTestUser, "")
	_, fresh, _, err := svc.RefreshAccessToken(context.Background(), old)
	if err != nil {
		t.Fatalf("first refresh failed: %v", err)
	}
	if store.count() != 2 {
		t.Fatalf("expected 2 rows after rotation, got %d", store.count())
	}

	// Replay the already-used old token → whole family revoked.
	_, _, _, err = svc.RefreshAccessToken(context.Background(), old)
	if !errors.Is(err, ErrInvalidRefresh) {
		t.Fatalf("expected ErrInvalidRefresh on reuse, got %v", err)
	}
	if store.count() != 0 {
		t.Errorf("expected whole family revoked, %d rows remain", store.count())
	}

	// The legitimate fresh token is now also revoked.
	if _, _, _, err := svc.RefreshAccessToken(context.Background(), fresh); err == nil {
		t.Error("fresh token from revoked family must fail")
	}
}

func TestRefreshExpiredToken(t *testing.T) {
	store := newMemRefreshStore()
	svc := newCookieTestService(store)

	raw, _ := svc.generateRefreshToken(context.Background(), cookieTestUser, "")
	store.rows[hashToken(raw)].expiresAt = time.Now().Add(-time.Hour)

	_, _, _, err := svc.RefreshAccessToken(context.Background(), raw)
	if !errors.Is(err, ErrExpiredToken) {
		t.Fatalf("expected ErrExpiredToken, got %v", err)
	}
}

func TestRevokeRefreshRevokesFamily(t *testing.T) {
	store := newMemRefreshStore()
	svc := newCookieTestService(store)

	old, _ := svc.generateRefreshToken(context.Background(), cookieTestUser, "")
	svc.RefreshAccessToken(context.Background(), old) // rotate → 2 rows
	if store.count() != 2 {
		t.Fatalf("expected 2 rows, got %d", store.count())
	}
	svc.RevokeRefresh(context.Background(), old)
	if store.count() != 0 {
		t.Errorf("logout must revoke the whole family, %d rows remain", store.count())
	}
}

// ---- handler-level cookie flows ----

func TestRefreshHandlerSetsCookiesAndReturnsUser(t *testing.T) {
	store := newMemRefreshStore()
	svc := newCookieTestService(store)
	raw, _ := svc.generateRefreshToken(context.Background(), cookieTestUser, "")
	r := setupAuthRouter(NewHandler(svc))

	req := httptest.NewRequest("POST", "/api/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: RefreshTokenCookie, Value: raw})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var u models.User
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil || u.ID != "u1" {
		t.Errorf("expected user body, got %s", w.Body.String())
	}
	acc := findCookie(w.Result().Cookies(), AccessTokenCookie)
	ref := findCookie(w.Result().Cookies(), RefreshTokenCookie)
	if acc == nil || ref == nil || acc.Value == "" || ref.Value == "" {
		t.Error("expected fresh access + refresh cookies")
	}
}

func TestRefreshHandlerNoCookie401(t *testing.T) {
	r := setupAuthRouter(newCookieTestHandler(newMemRefreshStore()))
	req := httptest.NewRequest("POST", "/api/auth/refresh", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without cookie, got %d", w.Code)
	}
}

func TestRefreshHandlerReuseClearsCookies(t *testing.T) {
	store := newMemRefreshStore()
	svc := newCookieTestService(store)
	old, _ := svc.generateRefreshToken(context.Background(), cookieTestUser, "")
	svc.RefreshAccessToken(context.Background(), old) // consume old
	r := setupAuthRouter(NewHandler(svc))

	req := httptest.NewRequest("POST", "/api/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: RefreshTokenCookie, Value: old})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on reuse, got %d", w.Code)
	}
	acc := findCookie(w.Result().Cookies(), AccessTokenCookie)
	if acc == nil || acc.MaxAge >= 0 {
		t.Error("expected clearing access_token cookie on 401")
	}
}

func TestLogoutRevokesFamilyAndClearsCookies(t *testing.T) {
	store := newMemRefreshStore()
	svc := newCookieTestService(store)
	raw, _ := svc.generateRefreshToken(context.Background(), cookieTestUser, "")
	r := setupAuthRouter(NewHandler(svc))

	req := httptest.NewRequest("POST", "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: RefreshTokenCookie, Value: raw})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if store.count() != 0 {
		t.Errorf("expected family revoked, %d rows remain", store.count())
	}
	acc := findCookie(w.Result().Cookies(), AccessTokenCookie)
	ref := findCookie(w.Result().Cookies(), RefreshTokenCookie)
	if acc == nil || ref == nil || acc.MaxAge >= 0 || ref.MaxAge >= 0 {
		t.Error("expected both cookies cleared")
	}
}

// ---- dev-login (FRONT-3, local gate) ----

func newCookieTestHandler(store *memRefreshStore) *Handler {
	return NewHandler(newCookieTestService(store))
}

func TestDevLoginLocalSuccess(t *testing.T) {
	store := newMemRefreshStore()
	svc := newCookieTestService(store)
	token, err := svc.generateAccessToken(cookieTestUser)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	r := setupAuthRouter(NewHandler(svc))

	req := httptest.NewRequest("POST", "/api/auth/dev-login", strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var u models.User
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil || u.ID != "u1" {
		t.Errorf("expected user body, got %s", w.Body.String())
	}
	if acc := findCookie(w.Result().Cookies(), AccessTokenCookie); acc == nil || acc.Value == "" {
		t.Error("expected access_token cookie from dev-login")
	}
	if store.count() != 1 {
		t.Errorf("expected a refresh token family row, got %d", store.count())
	}
}

func TestDevLoginInvalidToken401(t *testing.T) {
	r := setupAuthRouter(newCookieTestHandler(newMemRefreshStore()))
	req := httptest.NewRequest("POST", "/api/auth/dev-login", strings.NewReader(`{"token":"garbage"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestDevLoginDisabledOutsideLocal(t *testing.T) {
	cfg := &config.Config{FrontendURL: "https://quiz.example.com", JWTSecret: "test-secret"}
	svc := NewService(cfg, nil, &fakeUserRepo{user: cookieTestUser})
	svc.refreshStore = newMemRefreshStore()
	r := setupAuthRouter(NewHandler(svc))

	req := httptest.NewRequest("POST", "/api/auth/dev-login", strings.NewReader(`{"token":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 outside local dev, got %d", w.Code)
	}
}
