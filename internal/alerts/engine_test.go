package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/brugge-matheus/iot-aps/internal/domain"
)

var lim = domain.Limits{TempMin: 2, TempMax: 30, HumMin: 20, HumMax: 70}

func newEngine() *Engine {
	return NewEngine(Config{Samples: 3, Reminder: time.Minute, StaleAfter: 30 * time.Second, TempHyst: 1, HumHyst: 3})
}

func reading(t, h float64) domain.Reading { return domain.Reading{Temperature: t, Humidity: h} }

func TestAlertOnlyAfterConsecutiveSamples(t *testing.T) {
	e := newEngine()
	now := time.Unix(0, 0)
	for i := 0; i < 2; i++ {
		if ev := e.Evaluate("d1", "Sala", reading(35, 50), lim, now); len(ev) != 0 {
			t.Fatalf("alerta antecipado na leitura %d: %v", i+1, ev)
		}
		now = now.Add(5 * time.Second)
	}
	ev := e.Evaluate("d1", "Sala", reading(35, 50), lim, now)
	if len(ev) != 1 || ev[0].Kind != domain.AlertTempHigh {
		t.Fatalf("esperava alerta de temperatura alta, veio %v", ev)
	}
}

func TestSpikeResetsStreak(t *testing.T) {
	e := newEngine()
	now := time.Unix(0, 0)
	for _, temp := range []float64{35, 35, 25, 35, 35} {
		if ev := e.Evaluate("d1", "Sala", reading(temp, 50), lim, now); len(ev) != 0 {
			t.Fatalf("pico isolado não deveria alertar: %v", ev)
		}
		now = now.Add(5 * time.Second)
	}
}

func TestReminderAndNormalizationWithHysteresis(t *testing.T) {
	e := newEngine()
	now := time.Unix(0, 0)
	step := func(temp float64) []Event {
		ev := e.Evaluate("d1", "Sala", reading(temp, 50), lim, now)
		now = now.Add(5 * time.Second)
		return ev
	}
	step(35)
	step(35)
	if ev := step(35); len(ev) != 1 {
		t.Fatalf("esperava alerta: %v", ev)
	}
	for i := 0; i < 11; i++ { // até 55 s depois do alerta, antes do lembrete de 1 min
		if ev := step(35); len(ev) != 0 {
			t.Fatalf("lembrete antes do intervalo: %v", ev)
		}
	}
	if ev := step(35); len(ev) != 1 || !strings.HasPrefix(ev[0].Message, "Lembrete") {
		t.Fatalf("esperava lembrete: %v", ev)
	}
	if ev := step(29.5); len(ev) != 0 { // dentro do limite, mas sem passar a histerese
		t.Fatalf("normalizou sem histerese: %v", ev)
	}
	if !e.Active("d1")[domain.AlertTempHigh] {
		t.Fatal("alerta deveria continuar ativo")
	}
	ev := step(28.5)
	if len(ev) != 1 || ev[0].Kind != domain.AlertNormalized || !strings.Contains(ev[0].Message, "temperatura") {
		t.Fatalf("esperava normalização: %v", ev)
	}
}

func TestLowLimitsAndHumidity(t *testing.T) {
	e := newEngine()
	now := time.Unix(0, 0)
	var ev []Event
	for i := 0; i < 3; i++ {
		ev = e.Evaluate("d1", "Câmara", reading(0, 80), lim, now)
		now = now.Add(5 * time.Second)
	}
	kinds := map[string]bool{}
	for _, x := range ev {
		kinds[x.Kind] = true
	}
	if !kinds[domain.AlertTempLow] || !kinds[domain.AlertHumHigh] || len(ev) != 2 {
		t.Fatalf("esperava temperatura baixa e umidade alta: %v", ev)
	}
}

func TestGapWithoutReadingsResetsStreak(t *testing.T) {
	e := newEngine()
	now := time.Unix(0, 0)
	e.Evaluate("d1", "Sala", reading(35, 50), lim, now)
	e.Evaluate("d1", "Sala", reading(35, 50), lim, now.Add(5*time.Second))
	if ev := e.Evaluate("d1", "Sala", reading(35, 50), lim, now.Add(2*time.Minute)); len(ev) != 0 {
		t.Fatalf("sequência deveria ter sido zerada após lacuna: %v", ev)
	}
}

func TestDevicesAreIndependent(t *testing.T) {
	e := newEngine()
	now := time.Unix(0, 0)
	for i := 0; i < 3; i++ {
		e.Evaluate("d1", "A", reading(35, 50), lim, now)
		if ev := e.Evaluate("d2", "B", reading(20, 50), lim, now); len(ev) != 0 {
			t.Fatalf("d2 não deveria alertar: %v", ev)
		}
		now = now.Add(5 * time.Second)
	}
	if !e.Active("d1")[domain.AlertTempHigh] || e.Active("d2")[domain.AlertTempHigh] {
		t.Fatal("estado de alerta misturado entre dispositivos")
	}
}

func TestRestoreAvoidsDuplicateAfterRestart(t *testing.T) {
	e := newEngine()
	now := time.Unix(1000, 0)
	e.Restore("d1", domain.AlertTempHigh, now.Add(-10*time.Second)) // alerta enviado antes do reinício
	if !e.Active("d1")[domain.AlertTempHigh] {
		t.Fatal("estado restaurado deveria estar ativo")
	}
	for i := 0; i < 5; i++ {
		if ev := e.Evaluate("d1", "Sala", reading(35, 50), lim, now); len(ev) != 0 {
			t.Fatalf("alerta repetido após reinício: %v", ev)
		}
		now = now.Add(5 * time.Second)
	}
	ev := e.Evaluate("d1", "Sala", reading(25, 50), lim, now)
	if len(ev) != 1 || ev[0].Kind != domain.AlertNormalized || ev[0].Condition != domain.AlertTempHigh {
		t.Fatalf("esperava normalização da condição restaurada: %v", ev)
	}
	e.Restore("d1", "desconhecido", now) // tipo inválido é ignorado
}
