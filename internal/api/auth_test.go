package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionToken(t *testing.T) {
	a := newAuth("grupo", "segredo", "chave-de-teste")
	now := time.Unix(1_000_000, 0)
	a.now = func() time.Time { return now }

	tok, _ := a.token("grupo")
	if !a.valid(tok) {
		t.Fatal("token recém-criado deveria ser válido")
	}
	enc, sig, _ := strings.Cut(tok, ".")
	if a.valid(enc + "." + strings.Repeat("0", len(sig))) {
		t.Fatal("assinatura adulterada aceita")
	}
	forged, _ := newAuth("grupo", "segredo", "outra-chave").token("grupo")
	if a.valid(forged) {
		t.Fatal("token assinado com outra chave aceito")
	}
	if a.valid("lixo") || a.valid("") {
		t.Fatal("token malformado aceito")
	}
	now = now.Add(sessionTTL + time.Second)
	if a.valid(tok) {
		t.Fatal("token expirado aceito")
	}
}

func TestCredentialsAndLockout(t *testing.T) {
	a := newAuth("grupo", "segredo", "k")
	if !a.checkCredentials("grupo", "segredo") || a.checkCredentials("grupo", "errada") || a.checkCredentials("outro", "segredo") {
		t.Fatal("verificação de credenciais incorreta")
	}
	now := time.Unix(0, 0)
	a.now = func() time.Time { return now }
	for i := 0; i < maxFailures; i++ {
		if !a.allowed("1.2.3.4") {
			t.Fatalf("bloqueou cedo demais na tentativa %d", i+1)
		}
		a.recordFailure("1.2.3.4")
	}
	if a.allowed("1.2.3.4") {
		t.Fatal("deveria bloquear após muitas tentativas")
	}
	if !a.allowed("5.6.7.8") {
		t.Fatal("bloqueio não pode afetar outros IPs")
	}
	now = now.Add(lockout + time.Second)
	if !a.allowed("1.2.3.4") {
		t.Fatal("bloqueio deveria expirar")
	}
}

func TestMiddlewareRequiresSession(t *testing.T) {
	a := &API{auth: newAuth("grupo", "segredo", "k")}
	h := a.middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	do := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("/", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("página sem sessão deveria ir para /login, veio %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do("/api/overview", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("API sem sessão deveria dar 401, veio %d", rec.Code)
	}
	for _, p := range []string{"/login", "/healthz"} {
		if rec := do(p, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s deveria ser público, veio %d", p, rec.Code)
		}
	}
	tok, _ := a.auth.token("grupo")
	if rec := do("/api/overview", &http.Cookie{Name: sessionCookie, Value: tok}); rec.Code != http.StatusOK {
		t.Fatalf("com sessão válida deveria passar, veio %d", rec.Code)
	}
}
