package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/middleware"
)

// Cookie names/paths are a frozen contract with the frontend (FRONT-3):
//   - access_token:  Path=/,          Max-Age=900    (15 min, matches JWT exp)
//   - refresh_token: Path=/api/auth,  Max-Age=604800 (7 days)
//
// Both are HttpOnly + SameSite=Lax; Secure iff FRONTEND_URL is https.
const (
	AccessTokenCookie  = "access_token"
	RefreshTokenCookie = "refresh_token"

	accessTokenMaxAge  = 900
	refreshTokenMaxAge = 7 * 24 * 60 * 60
)

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	auth := rg.Group("/auth")
	auth.GET("/github", h.GithubLogin)
	auth.GET("/github/callback", h.GithubCallback)
	auth.POST("/refresh", h.Refresh)
	auth.POST("/logout", h.Logout)
	auth.DELETE("/logout", h.Logout)
	auth.POST("/dev-login", h.DevLogin)
	auth.GET("/me", middleware.Auth(h.service), h.Me)
}

// cookieSecure is true when the frontend is served over https, so auth
// cookies get the Secure flag in production.
func (h *Handler) cookieSecure() bool {
	return h.service.cfg.CookieSecure()
}

func (h *Handler) setStateCookie(c *gin.Context, value string, maxAge int) {
	// SameSite=Lax blocks the state cookie from being sent on cross-site
	// POSTs (CSRF); Secure is set when the frontend is https.
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("oauth_state", value, maxAge, "/", "", h.cookieSecure(), true)
}

// setTokenCookies writes the access + refresh httpOnly cookies per the
// frozen FRONT-3 contract.
func (h *Handler) setTokenCookies(c *gin.Context, accessToken, refreshToken string) {
	secure := h.cookieSecure()
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(AccessTokenCookie, accessToken, accessTokenMaxAge, "/", "", secure, true)
	c.SetCookie(RefreshTokenCookie, refreshToken, refreshTokenMaxAge, "/api/auth", "", secure, true)
}

// clearTokenCookies expires both auth cookies (logout / refresh failure).
func (h *Handler) clearTokenCookies(c *gin.Context) {
	secure := h.cookieSecure()
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(AccessTokenCookie, "", -1, "/", "", secure, true)
	c.SetCookie(RefreshTokenCookie, "", -1, "/api/auth", "", secure, true)
}

func (h *Handler) GithubLogin(c *gin.Context) {
	state := generateState()
	h.setStateCookie(c, state, 300)
	c.Redirect(http.StatusTemporaryRedirect, h.service.GetAuthURL(state))
}

func (h *Handler) GithubCallback(c *gin.Context) {
	frontend := h.service.cfg.FrontendURL

	state := c.Query("state")
	cookieState, err := c.Cookie("oauth_state")
	if err != nil || state == "" || state != cookieState {
		c.Redirect(http.StatusTemporaryRedirect, frontend+"/login?error=invalid_state")
		return
	}

	code := c.Query("code")
	if code == "" {
		c.Redirect(http.StatusTemporaryRedirect, frontend+"/login?error=missing_code")
		return
	}

	accessToken, refreshToken, _, err := h.service.HandleCallback(c.Request.Context(), code)
	if err != nil {
		c.Redirect(http.StatusTemporaryRedirect, frontend+"/login?error=auth_failed")
		return
	}

	// Tokens travel ONLY in httpOnly cookies (FRONT-3) — never in the URL.
	// The SPA detects success via the #callback=1 fragment and calls /me.
	h.setStateCookie(c, "", -1)
	h.setTokenCookies(c, accessToken, refreshToken)
	c.Redirect(http.StatusFound, frontend+"/login#callback=1")
}

// Refresh reads the refresh_token cookie, rotates it (AUTH-5: old token
// marked used, new token in the same family) and responds 200 {user} with
// fresh cookies. Any failure → 401 + cleared cookies so the SPA resets.
func (h *Handler) Refresh(c *gin.Context) {
	refreshToken, err := c.Cookie(RefreshTokenCookie)
	if err != nil || refreshToken == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}

	accessToken, newRefresh, u, err := h.service.RefreshAccessToken(c.Request.Context(), refreshToken)
	if err != nil {
		h.clearTokenCookies(c)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid refresh token"})
		return
	}

	h.setTokenCookies(c, accessToken, newRefresh)
	c.JSON(http.StatusOK, u)
}

// Logout revokes the caller's refresh-token family server-side and clears
// both cookies → 204.
func (h *Handler) Logout(c *gin.Context) {
	if refreshToken, err := c.Cookie(RefreshTokenCookie); err == nil {
		h.service.RevokeRefresh(c.Request.Context(), refreshToken)
	}
	h.clearTokenCookies(c)
	c.Status(http.StatusNoContent)
}

// DevLogin is a LOCAL-DEV-ONLY endpoint (enabled iff FRONTEND_URL is local)
// that converts a validly-signed access JWT (e.g. minted by an admin script)
// into the cookie session the browser flow uses. It is not an auth bypass:
// the token must pass normal signature/expiry validation.
func (h *Handler) DevLogin(c *gin.Context) {
	if !h.service.cfg.IsLocal() {
		// Do not reveal the endpoint exists outside local dev.
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}

	var req struct {
		Token string `json:"token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token required"})
		return
	}

	u, err := h.service.ValidateAccessToken(req.Token)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return
	}

	accessToken, refreshToken, err := h.service.IssueTokens(c.Request.Context(), u)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue tokens"})
		return
	}

	h.setTokenCookies(c, accessToken, refreshToken)
	c.JSON(http.StatusOK, u)
}

func (h *Handler) Me(c *gin.Context) {
	u := middleware.GetUser(c)
	c.JSON(http.StatusOK, u)
}

func generateState() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
