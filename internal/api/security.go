package api

import (
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"

	"llm-gateway/internal/config"
)

// authCookieName stores the browser's auth token, exchanged once via
// /app/<name>?token=<auth_token>. The value is base64 so any token survives
// cookie-value sanitization.
const authCookieName = "llm_gateway_auth"

// SecurityMiddleware validates the request's Host header against the
// configured allowlist (defeats DNS rebinding from web pages) and enforces
// the auth token before any routing. Clients authenticate with
// "Authorization: Bearer <auth_token>"; browsers exchange the token for a
// cookie once via /app/<name>?token=<auth_token>.
//
// /health is exempt from both checks: it answers with static JSON only, and
// it is what uptime monitors and tunnel health probes call.
func SecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		if !hostAllowed(r.Host) {
			slog.Warn("request rejected — Host not allowed", "host", r.Host, "remote_addr", r.RemoteAddr)
			http.Error(w, "Host not allowed", http.StatusForbidden)
			return
		}

		if config.AuthEnabled() && !requestAuthorized(r) {
			// A valid ?token= on an /app/ launch passes through once so
			// AppLaunchHandler can exchange it for the auth cookie.
			if strings.HasPrefix(r.URL.Path, "/app/") && queryTokenValid(r) {
				next.ServeHTTP(w, r)
				return
			}
			slog.Warn("request rejected — bad auth token", "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="llm-gateway"`)
			writeOpenAIError(w, "Invalid or missing auth token", "unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// hostAllowed matches the request's Host header (port stripped, lowercased)
// against the configured allowlist. An empty allowlist disables the check.
func hostAllowed(hostHeader string) bool {
	allowed := config.AllowedHosts()
	if len(allowed) == 0 {
		return true
	}
	host := strings.ToLower(config.NormalizeHost(hostHeader))
	for _, a := range allowed {
		if host == a {
			return true
		}
	}
	return false
}

// requestAuthorized checks the Authorization header first; a present but
// wrong header never falls through to the cookie. Token comparisons are
// constant-time.
func requestAuthorized(r *http.Request) bool {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "bearer "
		if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(auth[len(prefix):])), []byte(config.AuthToken())) == 1
	}
	if token := cookieAuthToken(r); token != "" {
		return subtle.ConstantTimeCompare([]byte(token), []byte(config.AuthToken())) == 1
	}
	return false
}

// queryTokenValid reports whether the ?token= query parameter matches the
// configured auth token.
func queryTokenValid(r *http.Request) bool {
	token := r.URL.Query().Get("token")
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(config.AuthToken())) == 1
}

// setAuthCookie stores the auth token as a long-lived HttpOnly cookie so
// browsers can use the web apps without per-request headers.
func setAuthCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    base64.StdEncoding.EncodeToString([]byte(config.AuthToken())),
		Path:     "/",
		MaxAge:   31536000, // one year; /app/ clears it earlier
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
	})
}

func cookieAuthToken(r *http.Request) string {
	c, err := r.Cookie(authCookieName)
	if err != nil || c.Value == "" {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(c.Value)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// requestIsHTTPS reports whether the request arrived over TLS — directly or
// via a proxy/tunnel that sets X-Forwarded-Proto. Used only for the cookie
// Secure flag; never for auth decisions.
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
