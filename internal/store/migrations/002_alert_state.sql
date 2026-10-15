-- Condições de alerta ativas, para o motor de alertas sobreviver a reinícios do servidor.
CREATE TABLE alert_state (
    device_id         TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind              TEXT NOT NULL,
    last_notified_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (device_id, kind)
);
