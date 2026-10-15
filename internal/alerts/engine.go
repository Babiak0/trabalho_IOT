// Package alerts decide quando uma sequência de leituras vira alerta.
//
// Regras:
//   - um alerta só dispara após N leituras consecutivas fora do limite (evita falso positivo);
//   - enquanto a condição persistir, lembretes respeitam um intervalo mínimo;
//   - a condição só é considerada normalizada com histerese (volta X unidades para dentro);
//   - um intervalo grande sem leituras zera as sequências.
package alerts

import (
	"fmt"
	"sync"
	"time"

	"github.com/brugge-matheus/iot-aps/internal/domain"
)

type Config struct {
	Samples    int
	Reminder   time.Duration
	StaleAfter time.Duration
	TempHyst   float64
	HumHyst    float64
}

type Event struct {
	Kind      string
	Condition string // condição afetada (ex.: temperature_high); em Normalized, a que voltou ao normal
	Severity  string // "critical", "warning" ou "info"
	Message   string
}

type condition struct {
	kind      string
	label     string
	metric    string
	unit      string
	high      bool
	value     func(domain.Reading) float64
	threshold func(domain.Limits) float64
	hyst      func(Config) float64
}

var conditions = []condition{
	{domain.AlertTempHigh, "Temperatura acima do limite", "temperatura", "°C", true,
		func(r domain.Reading) float64 { return r.Temperature },
		func(l domain.Limits) float64 { return l.TempMax }, func(c Config) float64 { return c.TempHyst }},
	{domain.AlertTempLow, "Temperatura abaixo do limite", "temperatura", "°C", false,
		func(r domain.Reading) float64 { return r.Temperature },
		func(l domain.Limits) float64 { return l.TempMin }, func(c Config) float64 { return c.TempHyst }},
	{domain.AlertHumHigh, "Umidade acima do limite", "umidade", "%", true,
		func(r domain.Reading) float64 { return r.Humidity },
		func(l domain.Limits) float64 { return l.HumMax }, func(c Config) float64 { return c.HumHyst }},
	{domain.AlertHumLow, "Umidade abaixo do limite", "umidade", "%", false,
		func(r domain.Reading) float64 { return r.Humidity },
		func(l domain.Limits) float64 { return l.HumMin }, func(c Config) float64 { return c.HumHyst }},
}

type condState struct {
	streak       int
	active       bool
	lastNotified time.Time
}

type deviceState struct {
	lastReading time.Time
	conds       map[string]*condState
}

type Engine struct {
	cfg     Config
	mu      sync.Mutex
	devices map[string]*deviceState
}

func NewEngine(cfg Config) *Engine {
	return &Engine{cfg: cfg, devices: map[string]*deviceState{}}
}

func (e *Engine) state(deviceID string) *deviceState {
	s, ok := e.devices[deviceID]
	if !ok {
		s = &deviceState{conds: map[string]*condState{}}
		for _, c := range conditions {
			s.conds[c.kind] = &condState{}
		}
		e.devices[deviceID] = s
	}
	return s
}

// Evaluate processa uma leitura e devolve os eventos que devem virar alerta.
func (e *Engine) Evaluate(deviceID, deviceName string, r domain.Reading, lim domain.Limits, now time.Time) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.state(deviceID)
	if !s.lastReading.IsZero() && now.Sub(s.lastReading) > e.cfg.StaleAfter {
		for _, cs := range s.conds {
			cs.streak = 0
		}
	}
	s.lastReading = now

	var events []Event
	for _, c := range conditions {
		cs := s.conds[c.kind]
		v, t := c.value(r), c.threshold(lim)
		violated := (c.high && v > t) || (!c.high && v < t)
		if violated {
			cs.streak++
			if cs.streak < e.cfg.Samples {
				continue
			}
			if cs.active && now.Sub(cs.lastNotified) < e.cfg.Reminder {
				continue
			}
			prefix := ""
			if cs.active {
				prefix = "Lembrete: "
			}
			cs.active = true
			cs.lastNotified = now
			events = append(events, Event{
				Kind:      c.kind,
				Condition: c.kind,
				Severity:  "critical",
				Message:   fmt.Sprintf("%s%s em %s: %.1f %s (limite %.1f %s).", prefix, c.label, deviceName, v, c.unit, t, c.unit),
			})
			continue
		}
		cs.streak = 0
		if !cs.active {
			continue
		}
		h := c.hyst(e.cfg)
		back := (c.high && v <= t-h) || (!c.high && v >= t+h)
		if back {
			cs.active = false
			events = append(events, Event{
				Kind:      domain.AlertNormalized,
				Condition: c.kind,
				Severity:  "info",
				Message:   fmt.Sprintf("Normalizado em %s: %s voltou para %.1f %s.", deviceName, c.metric, v, c.unit),
			})
		}
	}
	return events
}

// Active informa as condições em alerta de um dispositivo (para o painel).
func (e *Engine) Active(deviceID string) map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]bool{}
	if s, ok := e.devices[deviceID]; ok {
		for k, cs := range s.conds {
			if cs.active {
				out[k] = true
			}
		}
	}
	return out
}

// Restore recarrega um alerta ativo salvo no banco (após reiniciar o servidor),
// evitando repetir a notificação antes do intervalo de lembrete.
func (e *Engine) Restore(deviceID, kind string, lastNotified time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cs, ok := e.state(deviceID).conds[kind]; ok {
		cs.active = true
		cs.lastNotified = lastNotified
	}
}

// Reset limpa o estado do dispositivo (usado quando os limites mudam).
func (e *Engine) Reset(deviceID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.devices, deviceID)
}
