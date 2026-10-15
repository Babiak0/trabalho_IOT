package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brugge-matheus/iot-aps/internal/domain"
	"github.com/brugge-matheus/iot-aps/internal/store"
	"github.com/brugge-matheus/iot-aps/internal/telegram"
)

const telegramHelp = `Comandos disponíveis:
/status – leituras atuais de todos os dispositivos
/ligar [dispositivo] – liga o atuador (ventilação)
/desligar [dispositivo] – desliga o atuador
/limites [dispositivo] – mostra os limites de alerta
Com um único dispositivo cadastrado, o nome é opcional.`

var telegramMenu = []telegram.Command{
	{Command: "status", Description: "Leituras atuais dos dispositivos"},
	{Command: "ligar", Description: "Liga a ventilação"},
	{Command: "desligar", Description: "Desliga a ventilação"},
	{Command: "limites", Description: "Mostra os limites de alerta"},
	{Command: "ajuda", Description: "Lista os comandos"},
}

// telegramCommand responde aos comandos do chat autorizado (nuvem -> dispositivo pelo Telegram).
func (s *Service) telegramCommand(ctx context.Context, cmd string, args []string) string {
	switch cmd {
	case "/start", "/ajuda", "/help":
		return telegramHelp
	case "/status":
		return s.telegramStatus(ctx)
	case "/ligar", "/desligar":
		dev, err := s.pickDevice(ctx, args)
		if err != nil {
			return err.Error()
		}
		action := domain.ActionOn
		if cmd == "/desligar" {
			action = domain.ActionOff
		}
		if dev.Actuator == (action == domain.ActionOn) {
			estado := map[bool]string{true: "ligada", false: "desligada"}[dev.Actuator]
			return fmt.Sprintf("A ventilação de %s já está %s.", dev.Name, estado)
		}
		c, err := s.SendCommand(ctx, dev.ID, action, "telegram")
		if err != nil {
			return "Não foi possível enviar: " + friendly(err)
		}
		return s.waitAck(ctx, c, dev.Name)
	case "/limites":
		dev, err := s.pickDevice(ctx, args)
		if err != nil {
			return err.Error()
		}
		l := dev.Limits
		return fmt.Sprintf("Limites de %s:\nTemperatura: %.1f a %.1f °C\nUmidade: %.1f a %.1f %%",
			dev.Name, l.TempMin, l.TempMax, l.HumMin, l.HumMax)
	}
	return "Comando desconhecido.\n\n" + telegramHelp
}

func (s *Service) telegramStatus(ctx context.Context) string {
	o, err := s.Overview(ctx)
	if err != nil {
		return "Erro ao consultar o banco."
	}
	if len(o.Devices) == 0 {
		return "Nenhum dispositivo enviou dados ainda."
	}
	var b strings.Builder
	for i, d := range o.Devices {
		if i > 0 {
			b.WriteString("\n\n")
		}
		state := "🔴 offline"
		if d.Online {
			state = "🟢 online"
		}
		fmt.Fprintf(&b, "%s (%s) – %s", d.Name, d.ID, state)
		if d.Latest != nil {
			act := "desligado"
			if d.Actuator {
				act = "ligado"
			}
			fmt.Fprintf(&b, "\n🌡 %.1f °C   💧 %.1f %%\nAtuador: %s\nÚltima leitura: %s",
				d.Latest.Temperature, d.Latest.Humidity, act, d.Latest.ReceivedAt.Local().Format("02/01 15:04:05"))
		}
		if len(d.ActiveAlerts) > 0 {
			fmt.Fprintf(&b, "\n⚠️ Alertas ativos: %d", len(d.ActiveAlerts))
		}
	}
	return b.String()
}

func (s *Service) pickDevice(ctx context.Context, args []string) (domain.Device, error) {
	if len(args) > 0 {
		d, err := s.store.Device(ctx, strings.ToLower(args[0]))
		if errors.Is(err, store.ErrNotFound) {
			return d, fmt.Errorf("Dispositivo %q não encontrado. Use /status para ver os ids.", args[0])
		}
		return d, err
	}
	devs, err := s.store.Devices(ctx)
	if err != nil {
		return domain.Device{}, err
	}
	switch len(devs) {
	case 0:
		return domain.Device{}, errors.New("Nenhum dispositivo cadastrado.")
	case 1:
		return devs[0], nil
	}
	ids := make([]string, len(devs))
	for i, d := range devs {
		ids[i] = d.ID
	}
	return domain.Device{}, fmt.Errorf("Informe o dispositivo: %s", strings.Join(ids, ", "))
}

// waitAck aguarda a confirmação do ESP32 para responder no chat com o resultado real.
func (s *Service) waitAck(ctx context.Context, c domain.Command, name string) string {
	verb := map[string]string{domain.ActionOn: "ligado", domain.ActionOff: "desligado"}[c.Action]
	deadline := time.Now().Add(s.cfg.CommandTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(300 * time.Millisecond):
		}
		cmds, err := s.store.Commands(ctx, c.DeviceID, 5)
		if err != nil {
			continue
		}
		for _, x := range cmds {
			if x.ID == c.ID && x.AckedAt != nil {
				return fmt.Sprintf("✅ Atuador de %s %s (confirmado em %d ms).", name, verb, x.AckedAt.Sub(x.CreatedAt).Milliseconds())
			}
		}
	}
	return fmt.Sprintf("⏳ Comando enviado para %s, mas sem confirmação em %s.", name, s.cfg.CommandTimeout)
}

func friendly(err error) string {
	msg := err.Error()
	for _, p := range []string{ErrUnavailable.Error() + ": ", ErrInvalid.Error() + ": "} {
		msg = strings.TrimPrefix(msg, p)
	}
	return msg
}
