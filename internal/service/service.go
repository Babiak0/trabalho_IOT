// Package service contém as regras de negócio: ingestão, alertas, comandos e notificações.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/brugge-matheus/iot-aps/internal/alerts"
	"github.com/brugge-matheus/iot-aps/internal/broker"
	"github.com/brugge-matheus/iot-aps/internal/config"
	"github.com/brugge-matheus/iot-aps/internal/domain"
	"github.com/brugge-matheus/iot-aps/internal/store"
	"github.com/brugge-matheus/iot-aps/internal/telegram"
)

// ErrInvalid indica erro de entrada do usuário; ErrUnavailable, que a ação não pode ser feita agora.
var (
	ErrInvalid     = errors.New("requisição inválida")
	ErrUnavailable = errors.New("indisponível")
)

func invalid(msg string) error     { return fmt.Errorf("%w: %s", ErrInvalid, msg) }
func unavailable(msg string) error { return fmt.Errorf("%w: %s", ErrUnavailable, msg) }

// Publisher é a parte do cliente MQTT usada pelo serviço.
type Publisher interface {
	Publish(deviceID, kind string, payload any, retained bool) error
	Connected() bool
	LastError() string
}

type Service struct {
	cfg     config.Config
	store   *store.Store
	engine  *alerts.Engine
	bot     *telegram.Bot
	mqtt    Publisher
	started time.Time

	mu          sync.Mutex
	failsafe    map[string]bool // último estado do modo de segurança informado pela borda
	configDirty map[string]bool // dispositivos cujos limites ainda não foram publicados
}

func New(ctx context.Context, cfg config.Config, st *store.Store, bot *telegram.Bot) (*Service, error) {
	s := &Service{
		cfg:   cfg,
		store: st,
		bot:   bot,
		engine: alerts.NewEngine(alerts.Config{
			Samples: cfg.AlertSamples, Reminder: cfg.AlertReminder, StaleAfter: cfg.StaleAfter,
			TempHyst: 1.0, HumHyst: 3.0,
		}),
		started:     time.Now(),
		failsafe:    map[string]bool{},
		configDirty: map[string]bool{},
	}
	// Restaura os alertas ativos antes de processar qualquer mensagem MQTT.
	states, err := st.ActiveAlerts(ctx)
	if err != nil {
		return nil, fmt.Errorf("carregar estado dos alertas: %w", err)
	}
	for _, a := range states {
		s.engine.Restore(a.DeviceID, a.Kind, a.LastNotified)
	}
	if len(states) > 0 {
		slog.Info("alertas ativos restaurados", "quantidade", len(states))
	}
	return s, nil
}

// SetPublisher é chamado depois de criar o cliente MQTT (que precisa do HandleMQTT).
func (s *Service) SetPublisher(p Publisher) { s.mqtt = p }

// ---------- entrada MQTT ----------

func (s *Service) HandleMQTT(deviceID, kind string, payload []byte, retained bool) {
	if !domain.ValidDeviceID(deviceID) {
		slog.Warn("id de dispositivo inválido", "device", deviceID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	switch kind {
	case broker.KindTelemetry:
		if !retained {
			err = s.ingest(ctx, deviceID, payload)
		}
	case broker.KindStatus:
		err = s.status(ctx, deviceID, payload)
	case broker.KindAck:
		if !retained {
			err = s.ack(ctx, deviceID, payload)
		}
	}
	if err != nil {
		slog.Warn("mensagem descartada", "device", deviceID, "tipo", kind, "erro", err)
	}
}

func (s *Service) ingest(ctx context.Context, deviceID string, payload []byte) error {
	var t domain.Telemetry
	if err := json.Unmarshal(payload, &t); err != nil {
		return fmt.Errorf("json: %w", err)
	}
	if err := t.Validate(); err != nil {
		return err
	}
	if err := s.ensureDevice(ctx, deviceID); err != nil {
		return err
	}
	now := time.Now()
	r := domain.Reading{
		DeviceID: deviceID, ReceivedAt: now, DeviceAt: t.DeviceTime(), BootID: t.BootID, Seq: t.Seq,
		Temperature: *t.Temperature, Humidity: *t.Humidity,
		RawTemperature: *t.RawTemperature, RawHumidity: *t.RawHumidity,
		Actuator: *t.Actuator, EdgeAlarm: t.Edge.Alarm, EdgeFailsafe: t.Edge.Failsafe,
	}
	inserted, wasOnline, err := s.store.InsertReading(ctx, &r, t.Edge.Discarded)
	if err != nil || !inserted {
		return err
	}
	dev, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return err
	}
	if !wasOnline {
		s.raise(ctx, dev.ID, domain.AlertDeviceOnline, "info", fmt.Sprintf("%s está online e enviando dados.", dev.Name))
	}
	s.mu.Lock()
	prevFailsafe := s.failsafe[deviceID]
	s.failsafe[deviceID] = r.EdgeFailsafe
	s.mu.Unlock()
	if r.EdgeFailsafe && !prevFailsafe {
		s.raise(ctx, dev.ID, domain.AlertFailsafe, "warning",
			fmt.Sprintf("%s ligou o atuador sozinho (modo de segurança na borda) enquanto estava sem conexão com a nuvem.", dev.Name))
	}
	for _, ev := range s.engine.Evaluate(dev.ID, dev.Name, r, dev.Limits, now) {
		s.raise(ctx, dev.ID, ev.Kind, ev.Severity, ev.Message)
		var err error
		if ev.Kind == domain.AlertNormalized {
			err = s.store.ClearAlert(ctx, dev.ID, ev.Condition)
		} else {
			err = s.store.SetAlertActive(ctx, dev.ID, ev.Condition, now)
		}
		if err != nil {
			slog.Error("falha ao salvar estado do alerta", "erro", err)
		}
	}
	return nil
}

func (s *Service) status(ctx context.Context, deviceID string, payload []byte) error {
	var st domain.Status
	if err := json.Unmarshal(payload, &st); err != nil {
		return fmt.Errorf("json: %w", err)
	}
	if err := s.ensureDevice(ctx, deviceID); err != nil {
		return err
	}
	if st.Online {
		// "Online" passa a valer quando a telemetria chegar; aqui só registramos o firmware
		// e reenviamos os limites (o ESP32 pode ter reiniciado).
		s.markConfigDirty(deviceID)
		return s.store.SetFirmware(ctx, deviceID, st.FW)
	}
	changed, err := s.store.SetOffline(ctx, deviceID)
	if err != nil {
		return err
	}
	if changed {
		dev, err := s.store.Device(ctx, deviceID)
		if err != nil {
			return err
		}
		s.raise(ctx, deviceID, domain.AlertDeviceOffline, "critical",
			fmt.Sprintf("%s desconectou do broker (Last Will). Verifique energia e Wi-Fi.", dev.Name))
	}
	return nil
}

func (s *Service) ack(ctx context.Context, deviceID string, payload []byte) error {
	var a domain.Ack
	if err := json.Unmarshal(payload, &a); err != nil {
		return fmt.Errorf("json: %w", err)
	}
	if a.Actuator == nil || a.ID == "" {
		return errors.New("ack incompleto")
	}
	c, err := s.store.AckCommand(ctx, deviceID, a.ID, *a.Actuator, time.Now())
	if errors.Is(err, store.ErrNotFound) {
		return nil // duplicado ou desconhecido
	}
	if err == nil {
		slog.Info("comando confirmado", "device", deviceID, "acao", c.Action, "status", c.Status)
	}
	return err
}

func (s *Service) ensureDevice(ctx context.Context, id string) error {
	if _, err := s.store.Device(ctx, id); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if err := s.store.EnsureDevice(ctx, id); err != nil {
		return err
	}
	slog.Info("novo dispositivo cadastrado", "device", id)
	s.markConfigDirty(id)
	return nil
}

// raise registra o alerta; a entrega ao Telegram é feita pelo worker (outbox), com novas tentativas.
func (s *Service) raise(ctx context.Context, deviceID, kind, severity, message string) {
	status := domain.NotifyDisabled
	if s.bot.Enabled() {
		status = domain.NotifyPending
	}
	if _, err := s.store.InsertAlert(ctx, deviceID, kind, severity, message, status); err != nil {
		slog.Error("falha ao gravar alerta", "erro", err)
		return
	}
	slog.Info("alerta", "device", deviceID, "tipo", kind, "mensagem", message)
}

func (s *Service) markConfigDirty(id string) {
	s.mu.Lock()
	s.configDirty[id] = true
	s.mu.Unlock()
}

// ---------- ações do usuário ----------

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Service) SendCommand(ctx context.Context, deviceID, action, source string) (domain.Command, error) {
	if !domain.ValidAction(action) {
		return domain.Command{}, invalid("ação deve ser on ou off")
	}
	dev, err := s.store.Device(ctx, deviceID)
	if err != nil {
		return domain.Command{}, err
	}
	if !s.mqtt.Connected() {
		return domain.Command{}, unavailable("broker MQTT desconectado")
	}
	if !dev.Online {
		return domain.Command{}, unavailable(dev.Name + " está offline")
	}
	now := time.Now()
	c := domain.Command{ID: newID(), DeviceID: deviceID, Action: action, Source: source,
		Status: domain.CmdPending, CreatedAt: now, ExpiresAt: now.Add(s.cfg.CommandTimeout)}
	if err := s.store.InsertCommand(ctx, c); err != nil {
		return c, err
	}
	if err := s.mqtt.Publish(deviceID, broker.KindCommand, domain.CommandMessage{ID: c.ID, Action: action}, false); err != nil {
		_ = s.store.SetCommandStatus(ctx, c.ID, domain.CmdFailed)
		return c, unavailable("falha ao publicar comando: " + err.Error())
	}
	slog.Info("comando enviado", "device", deviceID, "acao", action, "origem", source)
	return c, nil
}

func (s *Service) SetLimits(ctx context.Context, deviceID string, l domain.Limits) error {
	if err := l.Validate(); err != nil {
		return invalid(err.Error())
	}
	if err := s.store.SetLimits(ctx, deviceID, l); err != nil {
		return err
	}
	s.engine.Reset(deviceID)
	if err := s.store.ClearAlerts(ctx, deviceID); err != nil {
		slog.Error("falha ao limpar estado dos alertas", "erro", err)
	}
	s.markConfigDirty(deviceID)
	s.publishConfigs(ctx)
	return nil
}

func (s *Service) Rename(ctx context.Context, deviceID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 60 {
		return invalid("nome deve ter entre 1 e 60 caracteres")
	}
	return s.store.Rename(ctx, deviceID, name)
}

// ---------- consultas ----------

type DeviceView struct {
	domain.Device
	ActiveAlerts []string `json:"active_alerts"`
}

type Overview struct {
	Devices         []DeviceView `json:"devices"`
	BrokerConnected bool         `json:"broker_connected"`
	BrokerError     string       `json:"broker_error"`
	TelegramEnabled bool         `json:"telegram_enabled"`
	UptimeSeconds   int64        `json:"uptime_seconds"`
}

func (s *Service) Overview(ctx context.Context) (Overview, error) {
	devs, err := s.store.Devices(ctx)
	if err != nil {
		return Overview{}, err
	}
	o := Overview{
		Devices: make([]DeviceView, 0, len(devs)), BrokerConnected: s.mqtt.Connected(), BrokerError: s.mqtt.LastError(),
		TelegramEnabled: s.bot.Enabled(), UptimeSeconds: int64(time.Since(s.started).Seconds()),
	}
	for _, d := range devs {
		v := DeviceView{Device: d, ActiveAlerts: []string{}}
		for k := range s.engine.Active(d.ID) {
			v.ActiveAlerts = append(v.ActiveAlerts, k)
		}
		o.Devices = append(o.Devices, v)
	}
	return o, nil
}

func (s *Service) Store() *store.Store { return s.store }

type MetricsView struct {
	store.Metrics
	UptimeSeconds   int64 `json:"uptime_seconds"`
	BrokerConnected bool  `json:"broker_connected"`
}

func (s *Service) Metrics(ctx context.Context, window time.Duration) (MetricsView, error) {
	m, err := s.store.Metrics(ctx, time.Now().Add(-window))
	m.Window = window.String()
	return MetricsView{Metrics: m, UptimeSeconds: int64(time.Since(s.started).Seconds()), BrokerConnected: s.mqtt.Connected()}, err
}

// ---------- tarefas em segundo plano ----------

func (s *Service) Run(ctx context.Context) {
	if s.bot.Enabled() {
		if err := s.bot.SetCommands(ctx, telegramMenu); err != nil {
			slog.Warn("falha ao registrar menu do telegram", "erro", err)
		}
		go s.bot.Listen(ctx, s.telegramCommand)
	}
	devs, err := s.store.Devices(ctx)
	if err == nil {
		for _, d := range devs {
			s.markConfigDirty(d.ID)
		}
	}
	fast := time.NewTicker(1 * time.Second)
	slow := time.NewTicker(5 * time.Second)
	prune := time.NewTicker(time.Hour)
	defer fast.Stop()
	defer slow.Stop()
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-fast.C:
			s.deliverAlerts(ctx)
		case <-slow.C:
			s.watchdog(ctx)
			s.publishConfigs(ctx)
		case <-prune.C:
			if n, err := s.store.PruneReadings(ctx, time.Now().Add(-s.cfg.HistoryRetention)); err == nil && n > 0 {
				slog.Info("leituras antigas removidas", "quantidade", n)
			}
		}
	}
}

func (s *Service) watchdog(ctx context.Context) {
	now := time.Now()
	ids, err := s.store.MarkStale(ctx, now.Add(-s.cfg.StaleAfter))
	if err != nil {
		slog.Error("watchdog", "erro", err)
		return
	}
	for _, id := range ids {
		dev, err := s.store.Device(ctx, id)
		if err != nil {
			continue
		}
		s.raise(ctx, id, domain.AlertDeviceOffline, "critical",
			fmt.Sprintf("%s sem enviar dados há mais de %s.", dev.Name, s.cfg.StaleAfter))
	}
	if err := s.store.ExpireCommands(ctx, now); err != nil {
		slog.Error("expirar comandos", "erro", err)
	}
}

func (s *Service) publishConfigs(ctx context.Context) {
	if s.mqtt == nil || !s.mqtt.Connected() {
		return
	}
	s.mu.Lock()
	ids := make([]string, 0, len(s.configDirty))
	for id := range s.configDirty {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		dev, err := s.store.Device(ctx, id)
		if err != nil {
			continue
		}
		if err := s.mqtt.Publish(id, broker.KindConfig, dev.Limits, true); err != nil {
			slog.Warn("falha ao publicar limites", "device", id, "erro", err)
			continue
		}
		s.mu.Lock()
		delete(s.configDirty, id)
		s.mu.Unlock()
	}
}

const maxNotifyAttempts = 5

func (s *Service) deliverAlerts(ctx context.Context) {
	for i := 0; i < 10; i++ { // até 10 por segundo
		a, err := s.store.NextPendingAlert(ctx, time.Now())
		if err != nil || a == nil {
			return
		}
		icon := map[string]string{"critical": "🚨", "warning": "⚠️", "info": "✅"}[a.Severity]
		err = s.bot.Send(ctx, icon+" "+a.Message)
		if err == nil {
			_ = s.store.AlertSent(ctx, a.ID, time.Now())
			continue
		}
		status := domain.NotifyPending
		if a.Attempts+1 >= maxNotifyAttempts {
			status = domain.NotifyFailed
		}
		backoff := time.Duration(5<<a.Attempts) * time.Second // 5s, 10s, 20s, 40s
		_ = s.store.AlertFailed(ctx, a.ID, status, time.Now().Add(backoff), err.Error())
		slog.Warn("falha ao notificar no telegram", "alerta", a.ID, "tentativa", a.Attempts+1, "erro", err)
		return
	}
}
