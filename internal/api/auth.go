package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "iot_session"
	sessionTTL    = 12 * time.Hour
	maxFailures   = 5               // tentativas erradas permitidas por IP...
	lockout       = 5 * time.Minute // ...dentro desta janela; depois, bloqueio temporário
)

// auth implementa login por formulário com sessão em cookie assinado (HMAC-SHA256).
// A sessão não fica guardada no servidor: o cookie carrega usuário e validade, e a
// assinatura impede que seja forjado ou alterado.
type auth struct {
	user, pass string
	secret     []byte
	now        func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time
}

func newAuth(user, pass, secret string) *auth {
	key := []byte(secret)
	if len(key) == 0 {
		// Sem segredo configurado, as sessões valem só até o servidor reiniciar.
		key = make([]byte, 32)
		_, _ = rand.Read(key)
	}
	return &auth{user: user, pass: pass, secret: key, now: time.Now, failures: map[string][]time.Time{}}
}

func (a *auth) checkCredentials(user, pass string) bool {
	u := subtle.ConstantTimeCompare([]byte(user), []byte(a.user))
	p := subtle.ConstantTimeCompare([]byte(pass), []byte(a.pass))
	return u&p == 1
}

func (a *auth) sign(payload string) string {
	m := hmac.New(sha256.New, a.secret)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// token gera o valor do cookie: base64(usuario|expiracao).assinatura
func (a *auth) token(user string) (string, time.Time) {
	exp := a.now().Add(sessionTTL)
	payload := user + "|" + strconv.FormatInt(exp.Unix(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + a.sign(payload), exp
}

func (a *auth) valid(token string) bool {
	enc, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return false
	}
	payload := string(raw)
	if !hmac.Equal([]byte(sig), []byte(a.sign(payload))) {
		return false
	}
	user, expStr, ok := strings.Cut(payload, "|")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	return ok && err == nil && user == a.user && a.now().Unix() < exp
}

func (a *auth) authenticated(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && a.valid(c.Value)
}

// allowed informa se o IP ainda pode tentar logar (limite de tentativas erradas).
func (a *auth) allowed(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := a.now().Add(-lockout)
	recent := a.failures[ip][:0]
	for _, t := range a.failures[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(a.failures, ip)
	} else {
		a.failures[ip] = recent
	}
	return len(recent) < maxFailures
}

func (a *auth) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures[ip] = append(a.failures[ip], a.now())
}

func (a *auth) resetFailures(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.failures, ip)
}

func (a *auth) setCookie(w http.ResponseWriter, r *http.Request, value string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(time.Until(exp).Seconds()),
		HttpOnly: true, // inacessível ao JavaScript
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *auth) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
}

// isHTTPS considera o TLS terminado no proxy reverso (Nginx/Cloudflare).
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// clientIP usa os cabeçalhos do Cloudflare/Nginx. O app só escuta em 127.0.0.1,
// então essas requisições sempre passam pelo proxy.
func clientIP(r *http.Request) string {
	for _, h := range []string{"CF-Connecting-IP", "X-Real-IP"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	host, _, _ := strings.Cut(r.RemoteAddr, ":")
	return host
}

// publicPath lista o que pode ser acessado sem login.
func publicPath(p string) bool {
	switch p {
	case "/login", "/login.html", "/api/login", "/healthz", "/favicon.ico":
		return true
	}
	return false
}
