// Package domain define os tipos trocados entre ESP32, back-end, banco e painel.
package domain

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

// Faixas físicas do DHT22. Valores fora delas são erro de leitura, não alerta.
const (
	TempMinValid = -40.0
	TempMaxValid = 80.0
	HumMinValid  = 0.0
	HumMaxValid  = 100.0
)

var deviceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,47}$`)

func ValidDeviceID(id string) bool { return deviceIDPattern.MatchString(id) }

// Telemetry é o JSON publicado pelo ESP32 em <prefixo>/<device>/telemetry.
type Telemetry struct {
	BootID         string   `json:"boot_id"`
	Seq            int64    `json:"seq"`
	TimestampMs    int64    `json:"ts"` // epoch em ms via NTP; 0 se o relógio não sincronizou
	Temperature    *float64 `json:"temperature"`
	Humidity       *float64 `json:"humidity"`
	RawTemperature *float64 `json:"raw_temperature"`
	RawHumidity    *float64 `json:"raw_humidity"`
	Actuator       *bool    `json:"actuator"`
	Edge           EdgeInfo `json:"edge"`
}

// EdgeInfo resume o que o processamento na borda decidiu.
type EdgeInfo struct {
	Alarm     bool `json:"alarm"`     // limite local violado (LED vermelho)
	Failsafe  bool `json:"failsafe"`  // atuador ligado pelo próprio ESP32 sem nuvem
	Discarded int  `json:"discarded"` // leituras rejeitadas desde o último envio
}

func (t Telemetry) Validate() error {
	if t.BootID == "" || len(t.BootID) > 64 {
		return errors.New("boot_id inválido")
	}
	if t.Seq < 0 {
		return errors.New("seq inválido")
	}
	checks := []struct {
		name   string
		v      *float64
		lo, hi float64
	}{
		{"temperature", t.Temperature, TempMinValid, TempMaxValid},
		{"humidity", t.Humidity, HumMinValid, HumMaxValid},
		{"raw_temperature", t.RawTemperature, TempMinValid, TempMaxValid},
		{"raw_humidity", t.RawHumidity, HumMinValid, HumMaxValid},
	}
	for _, c := range checks {
		if c.v == nil || math.IsNaN(*c.v) || *c.v < c.lo || *c.v > c.hi {
			return fmt.Errorf("%s ausente ou fora da faixa", c.name)
		}
	}
	if t.Actuator == nil {
		return errors.New("actuator ausente")
	}
	return nil
}

// DeviceTime devolve o horário da medição informado pelo dispositivo, se plausível.
func (t Telemetry) DeviceTime() *time.Time {
	if t.TimestampMs < 1_600_000_000_000 {
		return nil
	}
	ts := time.UnixMilli(t.TimestampMs)
	return &ts
}

// Status é publicado (retido) em <prefixo>/<device>/status; o Last Will envia online=false.
type Status struct {
	Online bool   `json:"online"`
	IP     string `json:"ip,omitempty"`
	FW     string `json:"fw,omitempty"`
}

// Ack confirma a execução de um comando.
type Ack struct {
	ID       string `json:"id"`
	Actuator *bool  `json:"actuator"`
}

// CommandMessage é enviado pela nuvem em <prefixo>/<device>/cmd.
type CommandMessage struct {
	ID     string `json:"id"`
	Action string `json:"action"`
}

const (
	ActionOn  = "on"
	ActionOff = "off"
)

func ValidAction(a string) bool { return a == ActionOn || a == ActionOff }

// Limits são os limites de alerta de um dispositivo. Também são enviados ao ESP32
// (tópico config, retido) para que a borda decida o alarme local e o modo de segurança.
type Limits struct {
	TempMin float64 `json:"temp_min"`
	TempMax float64 `json:"temp_max"`
	HumMin  float64 `json:"hum_min"`
	HumMax  float64 `json:"hum_max"`
}

func (l Limits) Validate() error {
	for _, v := range []float64{l.TempMin, l.TempMax, l.HumMin, l.HumMax} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errors.New("limite inválido")
		}
	}
	if l.TempMin < TempMinValid || l.TempMax > TempMaxValid || l.TempMin >= l.TempMax {
		return fmt.Errorf("temperatura: mínimo deve ser menor que máximo, entre %.0f e %.0f °C", TempMinValid, TempMaxValid)
	}
	if l.HumMin < HumMinValid || l.HumMax > HumMaxValid || l.HumMin >= l.HumMax {
		return errors.New("umidade: mínimo deve ser menor que máximo, entre 0 e 100 %")
	}
	return nil
}

type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Online     bool       `json:"online"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	Actuator   bool       `json:"actuator"`
	Limits     Limits     `json:"limits"`
	Latest     *Reading   `json:"latest"`
}

type Reading struct {
	ID             int64      `json:"id"`
	DeviceID       string     `json:"device_id"`
	ReceivedAt     time.Time  `json:"received_at"`
	DeviceAt       *time.Time `json:"device_at"`
	BootID         string     `json:"-"`
	Seq            int64      `json:"seq"`
	Temperature    float64    `json:"temperature"`
	Humidity       float64    `json:"humidity"`
	RawTemperature float64    `json:"raw_temperature"`
	RawHumidity    float64    `json:"raw_humidity"`
	Actuator       bool       `json:"actuator"`
	EdgeAlarm      bool       `json:"edge_alarm"`
	EdgeFailsafe   bool       `json:"edge_failsafe"`
}

// Tipos de alerta.
const (
	AlertTempHigh      = "temperature_high"
	AlertTempLow       = "temperature_low"
	AlertHumHigh       = "humidity_high"
	AlertHumLow        = "humidity_low"
	AlertNormalized    = "normalized"
	AlertDeviceOffline = "device_offline"
	AlertDeviceOnline  = "device_online"
	AlertFailsafe      = "edge_failsafe"
)

// Situação do envio da notificação.
const (
	NotifyPending  = "pending"
	NotifySent     = "sent"
	NotifyFailed   = "failed"
	NotifyDisabled = "disabled"
)

type Alert struct {
	ID           int64      `json:"id"`
	DeviceID     string     `json:"device_id"`
	CreatedAt    time.Time  `json:"created_at"`
	Kind         string     `json:"kind"`
	Severity     string     `json:"severity"`
	Message      string     `json:"message"`
	NotifyStatus string     `json:"notify_status"`
	SentAt       *time.Time `json:"sent_at"`
	Attempts     int        `json:"attempts"`
}

// Situação de um comando.
const (
	CmdPending = "pending"
	CmdAcked   = "acked"
	CmdLate    = "acked_late"
	CmdExpired = "expired"
	CmdFailed  = "failed"
)

type Command struct {
	ID        string     `json:"id"`
	DeviceID  string     `json:"device_id"`
	Action    string     `json:"action"`
	Source    string     `json:"source"`
	Status    string     `json:"status"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	AckedAt   *time.Time `json:"acked_at"`
}
