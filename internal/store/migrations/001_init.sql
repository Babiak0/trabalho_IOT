CREATE TABLE devices (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ,
    online        BOOLEAN NOT NULL DEFAULT false,
    actuator_on   BOOLEAN NOT NULL DEFAULT false,
    firmware      TEXT NOT NULL DEFAULT '',
    temp_min      DOUBLE PRECISION NOT NULL DEFAULT 15,
    temp_max      DOUBLE PRECISION NOT NULL DEFAULT 30,
    hum_min       DOUBLE PRECISION NOT NULL DEFAULT 30,
    hum_max       DOUBLE PRECISION NOT NULL DEFAULT 70
);

-- Telemetria (série temporal). received_at = chegada no back-end; device_at = medição no ESP32 (NTP).
CREATE TABLE readings (
    id               BIGSERIAL PRIMARY KEY,
    device_id        TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    received_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    device_at        TIMESTAMPTZ,
    boot_id          TEXT NOT NULL,
    seq              BIGINT NOT NULL,
    temperature      DOUBLE PRECISION NOT NULL,
    humidity         DOUBLE PRECISION NOT NULL,
    raw_temperature  DOUBLE PRECISION NOT NULL,
    raw_humidity     DOUBLE PRECISION NOT NULL,
    actuator_on      BOOLEAN NOT NULL,
    edge_alarm       BOOLEAN NOT NULL DEFAULT false,
    edge_failsafe    BOOLEAN NOT NULL DEFAULT false,
    edge_discarded   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (device_id, boot_id, seq)
);
CREATE INDEX readings_device_time ON readings (device_id, received_at DESC);

CREATE TABLE alerts (
    id               BIGSERIAL PRIMARY KEY,
    device_id        TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind             TEXT NOT NULL,
    severity         TEXT NOT NULL,
    message          TEXT NOT NULL,
    notify_status    TEXT NOT NULL,
    attempts         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at          TIMESTAMPTZ,
    last_error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX alerts_created ON alerts (created_at DESC);
CREATE INDEX alerts_outbox ON alerts (next_attempt_at) WHERE notify_status = 'pending';

CREATE TABLE commands (
    id          TEXT PRIMARY KEY,
    device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    action      TEXT NOT NULL,
    source      TEXT NOT NULL,
    status      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    acked_at    TIMESTAMPTZ
);
CREATE INDEX commands_device_created ON commands (device_id, created_at DESC);
