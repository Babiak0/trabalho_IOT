// Package store persiste dispositivos, telemetria, alertas e comandos no PostgreSQL.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brugge-matheus/iot-aps/internal/domain"
)

//go:embed migrations/*.sql
var migrations embed.FS

var ErrNotFound = errors.New("não encontrado")

type Store struct {
	db *pgxpool.Pool
}

// Open conecta ao banco, aguardando-o subir (útil no docker compose), e aplica as migrações.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("configuração do banco: %w", err)
	}
	for attempt := 1; ; attempt++ {
		err = pool.Ping(ctx)
		if err == nil {
			break
		}
		if attempt == 30 {
			pool.Close()
			return nil, fmt.Errorf("banco indisponível: %w", err)
		}
		slog.Info("aguardando banco de dados", "tentativa", attempt)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	s := &Store{db: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close()                         { s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return fmt.Errorf("tabela de migrações: %w", err)
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES($1)`, name)
			return err
		})
		if err != nil {
			return fmt.Errorf("migração %s: %w", name, err)
		}
		slog.Info("migração aplicada", "arquivo", name)
	}
	return nil
}

// ---------- dispositivos ----------

// EnsureDevice cadastra o dispositivo na primeira mensagem (o broker já autenticou o ESP32).
func (s *Store) EnsureDevice(ctx context.Context, id string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO devices(id, name) VALUES($1, $1) ON CONFLICT (id) DO NOTHING`, id)
	return err
}

const deviceCols = `d.id, d.name, d.online, d.last_seen_at, d.actuator_on, d.temp_min, d.temp_max, d.hum_min, d.hum_max`

func scanDevice(row pgx.Row, d *domain.Device) error {
	return row.Scan(&d.ID, &d.Name, &d.Online, &d.LastSeenAt, &d.Actuator,
		&d.Limits.TempMin, &d.Limits.TempMax, &d.Limits.HumMin, &d.Limits.HumMax)
}

func (s *Store) Device(ctx context.Context, id string) (domain.Device, error) {
	var d domain.Device
	err := scanDevice(s.db.QueryRow(ctx, `SELECT `+deviceCols+` FROM devices d WHERE d.id=$1`, id), &d)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

func (s *Store) Devices(ctx context.Context) ([]domain.Device, error) {
	rows, err := s.db.Query(ctx, `SELECT `+deviceCols+`,
		r.id, r.received_at, r.device_at, r.seq, r.temperature, r.humidity, r.raw_temperature, r.raw_humidity,
		r.actuator_on, r.edge_alarm, r.edge_failsafe
		FROM devices d
		LEFT JOIN LATERAL (SELECT * FROM readings WHERE device_id = d.id ORDER BY received_at DESC LIMIT 1) r ON true
		ORDER BY d.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Device{}
	for rows.Next() {
		var d domain.Device
		var r struct {
			id                   *int64
			received, deviceAt   *time.Time
			seq                  *int64
			t, h, rt, rh         *float64
			act, alarm, failsafe *bool
		}
		if err := rows.Scan(&d.ID, &d.Name, &d.Online, &d.LastSeenAt, &d.Actuator,
			&d.Limits.TempMin, &d.Limits.TempMax, &d.Limits.HumMin, &d.Limits.HumMax,
			&r.id, &r.received, &r.deviceAt, &r.seq, &r.t, &r.h, &r.rt, &r.rh, &r.act, &r.alarm, &r.failsafe); err != nil {
			return nil, err
		}
		if r.id != nil {
			d.Latest = &domain.Reading{ID: *r.id, DeviceID: d.ID, ReceivedAt: *r.received, DeviceAt: r.deviceAt, Seq: *r.seq,
				Temperature: *r.t, Humidity: *r.h, RawTemperature: *r.rt, RawHumidity: *r.rh,
				Actuator: *r.act, EdgeAlarm: *r.alarm, EdgeFailsafe: *r.failsafe}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SetLimits(ctx context.Context, id string, l domain.Limits) error {
	tag, err := s.db.Exec(ctx, `UPDATE devices SET temp_min=$2, temp_max=$3, hum_min=$4, hum_max=$5 WHERE id=$1`,
		id, l.TempMin, l.TempMax, l.HumMin, l.HumMax)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (s *Store) Rename(ctx context.Context, id, name string) error {
	tag, err := s.db.Exec(ctx, `UPDATE devices SET name=$2 WHERE id=$1`, id, name)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

// SetOffline marca o dispositivo como offline e informa se ele estava online.
func (s *Store) SetOffline(ctx context.Context, id string) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE devices SET online=false WHERE id=$1 AND online`, id)
	return err == nil && tag.RowsAffected() > 0, err
}

func (s *Store) SetFirmware(ctx context.Context, id, firmware string) error {
	if firmware == "" {
		return nil
	}
	_, err := s.db.Exec(ctx, `UPDATE devices SET firmware=$2 WHERE id=$1`, id, firmware)
	return err
}

// MarkStale marca como offline os dispositivos sem telemetria desde `before` e devolve quais mudaram.
func (s *Store) MarkStale(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.db.Query(ctx, `UPDATE devices SET online=false
		WHERE online AND (last_seen_at IS NULL OR last_seen_at < $1) RETURNING id`, before)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// ---------- telemetria ----------

// InsertReading grava a leitura (ignorando duplicatas) e atualiza o dispositivo.
// Devolve false quando a mensagem já havia sido recebida. wasOnline indica o estado anterior.
func (s *Store) InsertReading(ctx context.Context, r *domain.Reading, discarded int) (inserted, wasOnline bool, err error) {
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `INSERT INTO readings(device_id, received_at, device_at, boot_id, seq, temperature, humidity,
			raw_temperature, raw_humidity, actuator_on, edge_alarm, edge_failsafe, edge_discarded)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (device_id, boot_id, seq) DO NOTHING RETURNING id`,
			r.DeviceID, r.ReceivedAt, r.DeviceAt, r.BootID, r.Seq, r.Temperature, r.Humidity,
			r.RawTemperature, r.RawHumidity, r.Actuator, r.EdgeAlarm, r.EdgeFailsafe, discarded).Scan(&r.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		inserted = true
		return tx.QueryRow(ctx, `UPDATE devices d SET last_seen_at=$2, online=true, actuator_on=$3
			FROM (SELECT online FROM devices WHERE id=$1 FOR UPDATE) old
			WHERE d.id=$1 RETURNING old.online`, r.DeviceID, r.ReceivedAt, r.Actuator).Scan(&wasOnline)
	})
	return inserted, wasOnline, err
}

const readingCols = `id, device_id, received_at, device_at, seq, temperature, humidity, raw_temperature, raw_humidity, actuator_on, edge_alarm, edge_failsafe`

func scanReading(row pgx.Row, r *domain.Reading) error {
	return row.Scan(&r.ID, &r.DeviceID, &r.ReceivedAt, &r.DeviceAt, &r.Seq, &r.Temperature, &r.Humidity,
		&r.RawTemperature, &r.RawHumidity, &r.Actuator, &r.EdgeAlarm, &r.EdgeFailsafe)
}

// Readings devolve as leituras desde `since`, em ordem cronológica, limitadas às mais recentes.
func (s *Store) Readings(ctx context.Context, deviceID string, since time.Time, limit int) ([]domain.Reading, error) {
	rows, err := s.db.Query(ctx, `SELECT * FROM (SELECT `+readingCols+` FROM readings
		WHERE device_id=$1 AND received_at >= $2 ORDER BY received_at DESC LIMIT $3) x ORDER BY received_at`,
		deviceID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Reading{}
	for rows.Next() {
		var r domain.Reading
		if err := scanReading(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EachReading percorre todo o histórico de um dispositivo (exportação CSV).
func (s *Store) EachReading(ctx context.Context, deviceID string, fn func(domain.Reading) error) error {
	rows, err := s.db.Query(ctx, `SELECT `+readingCols+` FROM readings WHERE device_id=$1 ORDER BY received_at`, deviceID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r domain.Reading
		if err := scanReading(rows, &r); err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (s *Store) PruneReadings(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM readings WHERE received_at < $1`, before)
	return tag.RowsAffected(), err
}

// ---------- alertas ----------

func (s *Store) InsertAlert(ctx context.Context, deviceID, kind, severity, message, notifyStatus string) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO alerts(device_id, kind, severity, message, notify_status)
		VALUES($1,$2,$3,$4,$5) RETURNING id`, deviceID, kind, severity, message, notifyStatus).Scan(&id)
	return id, err
}

const alertCols = `id, device_id, created_at, kind, severity, message, notify_status, sent_at, attempts`

func (s *Store) Alerts(ctx context.Context, deviceID string, limit int) ([]domain.Alert, error) {
	rows, err := s.db.Query(ctx, `SELECT `+alertCols+` FROM alerts
		WHERE ($1 = '' OR device_id = $1) ORDER BY created_at DESC LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Alert{}
	for rows.Next() {
		var a domain.Alert
		if err := rows.Scan(&a.ID, &a.DeviceID, &a.CreatedAt, &a.Kind, &a.Severity, &a.Message, &a.NotifyStatus, &a.SentAt, &a.Attempts); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// NextPendingAlert devolve o próximo alerta da fila de notificação (padrão outbox).
func (s *Store) NextPendingAlert(ctx context.Context, now time.Time) (*domain.Alert, error) {
	var a domain.Alert
	err := s.db.QueryRow(ctx, `SELECT `+alertCols+` FROM alerts
		WHERE notify_status='pending' AND next_attempt_at <= $1 ORDER BY id LIMIT 1`, now).
		Scan(&a.ID, &a.DeviceID, &a.CreatedAt, &a.Kind, &a.Severity, &a.Message, &a.NotifyStatus, &a.SentAt, &a.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

func (s *Store) AlertSent(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE alerts SET notify_status='sent', sent_at=$2, attempts=attempts+1, last_error='' WHERE id=$1`, id, at)
	return err
}

func (s *Store) AlertFailed(ctx context.Context, id int64, status string, next time.Time, errMsg string) error {
	_, err := s.db.Exec(ctx, `UPDATE alerts SET notify_status=$2, next_attempt_at=$3, attempts=attempts+1, last_error=$4 WHERE id=$1`,
		id, status, next, errMsg)
	return err
}

// ---------- estado do motor de alertas ----------

type AlertState struct {
	DeviceID     string
	Kind         string
	LastNotified time.Time
}

func (s *Store) SetAlertActive(ctx context.Context, deviceID, kind string, at time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO alert_state(device_id, kind, last_notified_at) VALUES($1,$2,$3)
		ON CONFLICT (device_id, kind) DO UPDATE SET last_notified_at = EXCLUDED.last_notified_at`, deviceID, kind, at)
	return err
}

func (s *Store) ClearAlert(ctx context.Context, deviceID, kind string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM alert_state WHERE device_id=$1 AND kind=$2`, deviceID, kind)
	return err
}

func (s *Store) ClearAlerts(ctx context.Context, deviceID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM alert_state WHERE device_id=$1`, deviceID)
	return err
}

func (s *Store) ActiveAlerts(ctx context.Context) ([]AlertState, error) {
	rows, err := s.db.Query(ctx, `SELECT device_id, kind, last_notified_at FROM alert_state`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlertState, error) {
		var a AlertState
		err := r.Scan(&a.DeviceID, &a.Kind, &a.LastNotified)
		return a, err
	})
}

// ---------- comandos ----------

func (s *Store) InsertCommand(ctx context.Context, c domain.Command) error {
	_, err := s.db.Exec(ctx, `INSERT INTO commands(id, device_id, action, source, status, created_at, expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)`, c.ID, c.DeviceID, c.Action, c.Source, c.Status, c.CreatedAt, c.ExpiresAt)
	return err
}

func (s *Store) SetCommandStatus(ctx context.Context, id, status string) error {
	_, err := s.db.Exec(ctx, `UPDATE commands SET status=$2 WHERE id=$1`, id, status)
	return err
}

// AckCommand confirma um comando pendente/expirado se o estado informado pelo ESP32 bate com a ação.
func (s *Store) AckCommand(ctx context.Context, deviceID, id string, actuator bool, now time.Time) (*domain.Command, error) {
	var c domain.Command
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id, device_id, action, source, status, created_at, expires_at FROM commands
			WHERE id=$1 AND device_id=$2 AND status IN ('pending','expired') FOR UPDATE`, id, deviceID).
			Scan(&c.ID, &c.DeviceID, &c.Action, &c.Source, &c.Status, &c.CreatedAt, &c.ExpiresAt)
		if err != nil {
			return err
		}
		if (c.Action == domain.ActionOn) != actuator {
			return pgx.ErrNoRows
		}
		c.Status = domain.CmdAcked
		if now.After(c.ExpiresAt) {
			c.Status = domain.CmdLate
		}
		c.AckedAt = &now
		if _, err := tx.Exec(ctx, `UPDATE commands SET status=$2, acked_at=$3 WHERE id=$1`, c.ID, c.Status, now); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE devices SET actuator_on=$2 WHERE id=$1`, deviceID, actuator)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

func (s *Store) ExpireCommands(ctx context.Context, now time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE commands SET status='expired' WHERE status='pending' AND expires_at < $1`, now)
	return err
}

func (s *Store) Commands(ctx context.Context, deviceID string, limit int) ([]domain.Command, error) {
	rows, err := s.db.Query(ctx, `SELECT id, device_id, action, source, status, created_at, expires_at, acked_at
		FROM commands WHERE device_id=$1 ORDER BY created_at DESC LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Command{}
	for rows.Next() {
		var c domain.Command
		if err := rows.Scan(&c.ID, &c.DeviceID, &c.Action, &c.Source, &c.Status, &c.CreatedAt, &c.ExpiresAt, &c.AckedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- métricas para o SLA ----------

type Percentiles struct {
	Count int64    `json:"count"`
	P50   *float64 `json:"p50_ms"`
	P95   *float64 `json:"p95_ms"`
}

type Metrics struct {
	Window             string      `json:"window"`
	Readings           int64       `json:"readings"`
	TelemetryLatency   Percentiles `json:"telemetry_latency"`    // medição no ESP32 -> gravação no banco
	AlertDelivery      Percentiles `json:"alert_delivery"`       // alerta criado -> entregue ao Telegram
	CommandRoundTrip   Percentiles `json:"command_round_trip"`   // comando enviado -> confirmação do ESP32
	CommandSuccessRate *float64    `json:"command_success_rate"` // confirmados no prazo / finalizados
	DataCompleteness   *float64    `json:"data_completeness"`    // minutos com dados / minutos observados
	AlertsCreated      int64       `json:"alerts_created"`
	AlertsFailed       int64       `json:"alerts_failed"`
}

func (s *Store) Metrics(ctx context.Context, since time.Time) (Metrics, error) {
	var m Metrics
	q := func(sql string, dst *Percentiles) error {
		return s.db.QueryRow(ctx, sql, since).Scan(&dst.Count, &dst.P50, &dst.P95)
	}
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM readings WHERE received_at >= $1`, since).Scan(&m.Readings); err != nil {
		return m, err
	}
	if err := q(`SELECT count(*),
		percentile_cont(0.5) WITHIN GROUP (ORDER BY ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY ms)
		FROM (SELECT EXTRACT(EPOCH FROM received_at - device_at)*1000 AS ms FROM readings
		      WHERE received_at >= $1 AND device_at IS NOT NULL) x WHERE ms >= 0`, &m.TelemetryLatency); err != nil {
		return m, err
	}
	if err := q(`SELECT count(*),
		percentile_cont(0.5) WITHIN GROUP (ORDER BY ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY ms)
		FROM (SELECT EXTRACT(EPOCH FROM sent_at - created_at)*1000 AS ms FROM alerts
		      WHERE created_at >= $1 AND sent_at IS NOT NULL) x`, &m.AlertDelivery); err != nil {
		return m, err
	}
	if err := q(`SELECT count(*),
		percentile_cont(0.5) WITHIN GROUP (ORDER BY ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY ms)
		FROM (SELECT EXTRACT(EPOCH FROM acked_at - created_at)*1000 AS ms FROM commands
		      WHERE created_at >= $1 AND acked_at IS NOT NULL) x`, &m.CommandRoundTrip); err != nil {
		return m, err
	}
	if err := s.db.QueryRow(ctx, `SELECT CASE WHEN count(*) = 0 THEN NULL
		ELSE count(*) FILTER (WHERE status='acked')::float8 / count(*) END
		FROM commands WHERE created_at >= $1 AND status <> 'pending'`, since).Scan(&m.CommandSuccessRate); err != nil {
		return m, err
	}
	if err := s.db.QueryRow(ctx, `SELECT CASE WHEN min(received_at) IS NULL THEN NULL
		ELSE LEAST(1.0, count(DISTINCT date_trunc('minute', received_at))::float8 /
		     GREATEST(1, CEIL(EXTRACT(EPOCH FROM now() - min(received_at)) / 60))) END
		FROM readings WHERE received_at >= $1`, since).Scan(&m.DataCompleteness); err != nil {
		return m, err
	}
	err := s.db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE notify_status='failed')
		FROM alerts WHERE created_at >= $1`, since).Scan(&m.AlertsCreated, &m.AlertsFailed)
	return m, err
}
