// Simulador de ESP32: publica telemetria e responde a comandos usando o mesmo protocolo do firmware.
// Útil para desenvolver sem hardware e como plano B na apresentação.
//
// Durante a execução, digite no terminal:
//
//	t 35    define a temperatura alvo (°C)
//	h 80    define a umidade alvo (%)
//	q       encerra
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	mrand "math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

type limits struct {
	TempMin float64 `json:"temp_min"`
	TempMax float64 `json:"temp_max"`
	HumMin  float64 `json:"hum_min"`
	HumMax  float64 `json:"hum_max"`
}

type sim struct {
	mu               sync.Mutex
	targetT, targetH float64
	rawT, rawH       float64
	window           [][2]float64
	actuator         bool
	lim              limits
	executed         []string
}

func main() {
	url := flag.String("url", envOr("MQTT_URL", "tcp://localhost:1883"), "URL do broker (tcp:// ou tls://)")
	user := flag.String("user", envOr("SIM_USERNAME", "esp32-sim-01"), "usuário MQTT (igual ao id do dispositivo)")
	pass := flag.String("pass", envOr("SIM_PASSWORD", ""), "senha MQTT")
	device := flag.String("device", envOr("SIM_DEVICE", "esp32-sim-01"), "id do dispositivo")
	prefix := flag.String("prefix", envOr("MQTT_TOPIC_PREFIX", "ambiente"), "prefixo dos tópicos")
	interval := flag.Duration("interval", 5*time.Second, "intervalo entre leituras")
	temp := flag.Float64("t", 24, "temperatura inicial")
	hum := flag.Float64("h", 50, "umidade inicial")
	flag.Parse()

	s := &sim{targetT: *temp, targetH: *hum, rawT: *temp, rawH: *hum,
		lim: limits{TempMin: -40, TempMax: 80, HumMin: 0, HumMax: 100}}
	base := *prefix + "/" + *device
	boot := randomHex(8)
	var client paho.Client

	opts := paho.NewClientOptions().AddBroker(*url).SetClientID(*device+"-"+boot[:6]).
		SetUsername(*user).SetPassword(*pass).SetKeepAlive(15*time.Second).
		SetAutoReconnect(true).SetConnectRetry(true).SetCleanSession(true).
		SetWill(base+"/status", `{"online":false}`, 1, true)
	opts.SetOnConnectHandler(func(c paho.Client) {
		c.Subscribe(base+"/cmd", 1, func(c paho.Client, m paho.Message) { s.command(c, base, m.Payload()) })
		c.Subscribe(base+"/config", 1, func(_ paho.Client, m paho.Message) { s.config(m.Payload()) })
		c.Publish(base+"/status", 1, true, `{"online":true,"fw":"simulador-go"}`)
		log.Printf("conectado a %s como %s", *url, *device)
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) { log.Printf("conexão perdida: %v", err) })
	client = paho.NewClient(opts)
	if tok := client.Connect(); tok.Wait() && tok.Error() != nil {
		log.Fatal(tok.Error())
	}

	go s.readStdin(func() {
		client.Publish(base+"/status", 1, true, `{"online":false}`).Wait()
		client.Disconnect(250)
		os.Exit(0)
	})

	var seq int64
	for range time.Tick(*interval) {
		t, h, rawT, rawH, act, alarm := s.step()
		seq++
		msg := map[string]any{
			"boot_id": boot, "seq": seq, "ts": time.Now().UnixMilli(),
			"temperature": t, "humidity": h, "raw_temperature": rawT, "raw_humidity": rawH,
			"actuator": act, "edge": map[string]any{"alarm": alarm, "failsafe": false, "discarded": 0},
		}
		body, _ := json.Marshal(msg)
		if client.IsConnectionOpen() {
			client.Publish(base+"/telemetry", 0, false, body)
			log.Printf("telemetria: %.1f °C  %.1f %%  atuador=%v  alarme=%v", t, h, act, alarm)
		}
	}
}

// step simula o sensor (passeio aleatório em direção ao alvo) e o filtro de média móvel da borda.
func (s *sim) step() (t, h, rawT, rawH float64, act, alarm bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cooling := 0.0
	if s.actuator {
		cooling = 0.6 // ventilação ligada puxa a temperatura para baixo
	}
	s.rawT += (s.targetT-s.rawT)*0.35 - cooling + (mrand.Float64()-0.5)*0.4
	s.rawH += (s.targetH-s.rawH)*0.35 + (mrand.Float64()-0.5)*1.0
	s.rawT = round(clamp(s.rawT, -40, 80))
	s.rawH = round(clamp(s.rawH, 0, 100))
	s.window = append(s.window, [2]float64{s.rawT, s.rawH})
	if len(s.window) > 5 {
		s.window = s.window[1:]
	}
	for _, v := range s.window {
		t += v[0]
		h += v[1]
	}
	n := float64(len(s.window))
	t, h = round(t/n), round(h/n)
	alarm = t > s.lim.TempMax || t < s.lim.TempMin || h > s.lim.HumMax || h < s.lim.HumMin
	return t, h, s.rawT, s.rawH, s.actuator, alarm
}

func (s *sim) command(c paho.Client, base string, payload []byte) {
	var cmd struct{ ID, Action string }
	if json.Unmarshal(payload, &cmd) != nil || len(cmd.ID) != 32 || (cmd.Action != "on" && cmd.Action != "off") {
		log.Printf("comando inválido descartado")
		return
	}
	s.mu.Lock()
	seen := false
	for _, id := range s.executed {
		seen = seen || id == cmd.ID
	}
	if !seen {
		s.actuator = cmd.Action == "on"
		s.executed = append(s.executed, cmd.ID)
		if len(s.executed) > 10 {
			s.executed = s.executed[1:]
		}
	}
	act := s.actuator
	s.mu.Unlock()
	ack, _ := json.Marshal(map[string]any{"id": cmd.ID, "actuator": act})
	c.Publish(base+"/ack", 0, false, ack)
	log.Printf("comando %s executado; atuador=%v", cmd.Action, act)
}

func (s *sim) config(payload []byte) {
	var l limits
	if err := json.Unmarshal(payload, &l); err != nil {
		return
	}
	s.mu.Lock()
	s.lim = l
	s.mu.Unlock()
	log.Printf("limites recebidos: temp %.1f–%.1f °C, umidade %.1f–%.1f %%", l.TempMin, l.TempMax, l.HumMin, l.HumMax)
}

func (s *sim) readStdin(quit func()) {
	fmt.Println("Digite 't 35' para temperatura, 'h 80' para umidade, 'q' para sair.")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 1 && f[0] == "q" {
			quit()
		}
		if len(f) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(f[1], ",", "."), 64)
		if err != nil {
			continue
		}
		s.mu.Lock()
		switch f[0] {
		case "t":
			s.targetT = clamp(v, -40, 80)
		case "h":
			s.targetH = clamp(v, 0, 100)
		}
		log.Printf("alvo: %.1f °C  %.1f %%", s.targetT, s.targetH)
		s.mu.Unlock()
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func clamp(v, lo, hi float64) float64 { return max(lo, min(hi, v)) }
func round(v float64) float64         { return math.Round(v*100) / 100 }
