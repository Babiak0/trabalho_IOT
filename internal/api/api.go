// Package api expõe o painel web e a API REST.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/brugge-matheus/iot-aps/internal/domain"
	"github.com/brugge-matheus/iot-aps/internal/service"
	"github.com/brugge-matheus/iot-aps/internal/store"
	"github.com/brugge-matheus/iot-aps/web"
)

type API struct {
	svc        *service.Service
	user, pass string
}

func New(svc *service.Service, user, pass string) http.Handler {
	a := &API{svc: svc, user: user, pass: pass}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /api/overview", a.overview)
	mux.HandleFunc("GET /api/devices/{id}/readings", a.readings)
	mux.HandleFunc("GET /api/devices/{id}/readings.csv", a.exportCSV)
	mux.HandleFunc("GET /api/devices/{id}/commands", a.commands)
	mux.HandleFunc("POST /api/devices/{id}/commands", a.sendCommand)
	mux.HandleFunc("PUT /api/devices/{id}/limits", a.setLimits)
	mux.HandleFunc("PUT /api/devices/{id}/name", a.rename)
	mux.HandleFunc("GET /api/alerts", a.alerts)
	mux.HandleFunc("GET /api/metrics", a.metrics)
	mux.Handle("GET /", http.FileServerFS(web.Files))
	return a.middleware(mux)
}

func (a *API) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.URL.Path != "/healthz" && a.user != "" {
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(a.user)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(a.pass)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="painel", charset="UTF-8"`)
				writeErr(w, http.StatusUnauthorized, "autenticação necessária")
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Bloqueia requisições de outros sites (CSRF) e limita o corpo.
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					writeErr(w, http.StatusForbidden, "origem não permitida")
					return
				}
			}
			if r.Header.Get("Content-Type") != "application/json" {
				writeErr(w, http.StatusUnsupportedMediaType, "envie JSON")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 4096)
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail converte erros do serviço em respostas HTTP sem vazar detalhes internos.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "dispositivo não encontrado")
	case errors.Is(err, service.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrUnavailable):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		slog.Error("erro interno", "erro", err)
		writeErr(w, http.StatusInternalServerError, "erro interno")
	}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: JSON inválido", service.ErrInvalid)
	}
	return nil
}

func intParam(r *http.Request, name string, def, min, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return clamp(n, min, max)
}

func clamp(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	dbOK := a.svc.Store().Ping(ctx) == nil
	o := map[string]any{"database": dbOK}
	if ov, err := a.svc.Overview(ctx); err == nil {
		o["broker"] = ov.BrokerConnected
		o["uptime_seconds"] = ov.UptimeSeconds
	}
	status := http.StatusOK
	if !dbOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, o)
}

func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	o, err := a.svc.Overview(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (a *API) readings(w http.ResponseWriter, r *http.Request) {
	minutes := intParam(r, "minutes", 30, 1, 7*24*60)
	limit := intParam(r, "limit", 720, 1, 5000)
	out, err := a.svc.Store().Readings(r.Context(), r.PathValue("id"), time.Now().Add(-time.Duration(minutes)*time.Minute), limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) exportCSV(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := a.svc.Store().Device(r.Context(), id); err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="leituras-%s.csv"`, id))
	_, _ = w.Write([]byte("\xef\xbb\xbf")) // BOM para o Excel reconhecer UTF-8
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	_ = cw.Write([]string{"recebido_em", "medido_em", "seq", "temperatura_c", "umidade_pct", "temperatura_bruta", "umidade_bruta", "atuador", "alarme_borda", "modo_seguranca"})
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	err := a.svc.Store().EachReading(r.Context(), id, func(x domain.Reading) error {
		dev := ""
		if x.DeviceAt != nil {
			dev = x.DeviceAt.Format(time.RFC3339)
		}
		return cw.Write([]string{x.ReceivedAt.Format(time.RFC3339), dev, strconv.FormatInt(x.Seq, 10),
			f(x.Temperature), f(x.Humidity), f(x.RawTemperature), f(x.RawHumidity),
			strconv.FormatBool(x.Actuator), strconv.FormatBool(x.EdgeAlarm), strconv.FormatBool(x.EdgeFailsafe)})
	})
	cw.Flush()
	if err != nil {
		slog.Error("exportação CSV interrompida", "erro", err)
	}
}

func (a *API) commands(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.Store().Commands(r.Context(), r.PathValue("id"), intParam(r, "limit", 10, 1, 100))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) sendCommand(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, err)
		return
	}
	c, err := a.svc.SendCommand(r.Context(), r.PathValue("id"), body.Action, "dashboard")
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, c)
}

func (a *API) setLimits(w http.ResponseWriter, r *http.Request) {
	var l domain.Limits
	if err := decode(r, &l); err != nil {
		fail(w, err)
		return
	}
	if err := a.svc.SetLimits(r.Context(), r.PathValue("id"), l); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (a *API) rename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decode(r, &body); err != nil {
		fail(w, err)
		return
	}
	if err := a.svc.Rename(r.Context(), r.PathValue("id"), body.Name); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *API) alerts(w http.ResponseWriter, r *http.Request) {
	out, err := a.svc.Store().Alerts(r.Context(), r.URL.Query().Get("device"), intParam(r, "limit", 20, 1, 200))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request) {
	window, err := time.ParseDuration(r.URL.Query().Get("window"))
	if err != nil || window <= 0 || window > 30*24*time.Hour {
		window = 24 * time.Hour
	}
	m, err := a.svc.Metrics(r.Context(), window)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
