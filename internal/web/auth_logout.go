package web

import (
	"crypto/subtle"
	"net/http"
)

// logoutHandler handles POST /auth/logout.
// It validates the CSRF token (double-submit cookie pattern), clears both the
// ch_session and ch_csrf cookies, and redirects to /.
//
// In PR-1 the CSRF check is present but accepts any POST per the coexistence-
// window note in 02-task-list.md: real CSRF enforcement (double-submit compare)
// is active here from day one. The PR-1 dashboard renders the csrf_token form
// field, so the check works end-to-end once PR-1 is deployed.
type logoutHandler struct{}

func (h logoutHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	if !h.csrfValid(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	Clear(w, r)
	clearCSRFCookie(w, r)

	http.Redirect(w, r, "/", http.StatusFound)
}

// csrfValid checks the double-submit cookie pattern: both the ch_csrf cookie
// and the csrf_token form field must be present and equal (constant-time).
func (h logoutHandler) csrfValid(r *http.Request) bool {
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}

	formToken := r.FormValue("csrf_token")
	if formToken == "" {
		return false
	}

	// subtle.ConstantTimeCompare returns 1 on equal, 0 on not-equal.
	return subtle.ConstantTimeCompare([]byte(formToken), []byte(cookie.Value)) == 1
}

// RegisterLogout registers POST /auth/logout on the given mux.
func RegisterLogout(mux *http.ServeMux) {
	mux.Handle("/auth/logout", logoutHandler{})
}
