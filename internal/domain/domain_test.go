package domain

import (
	"encoding/json"
	"testing"
)

func TestTelemetryValidate(t *testing.T) {
	valid := `{"boot_id":"abc","seq":1,"temperature":24.5,"humidity":50,"raw_temperature":24.4,"raw_humidity":51,"actuator":false}`
	cases := map[string]struct {
		body string
		ok   bool
	}{
		"válida":             {valid, true},
		"sem atuador":        {`{"boot_id":"abc","seq":1,"temperature":24.5,"humidity":50,"raw_temperature":24.4,"raw_humidity":51}`, false},
		"sem temperatura":    {`{"boot_id":"abc","seq":1,"humidity":50,"raw_temperature":24.4,"raw_humidity":51,"actuator":false}`, false},
		"temperatura física": {`{"boot_id":"abc","seq":1,"temperature":120,"humidity":50,"raw_temperature":24.4,"raw_humidity":51,"actuator":false}`, false},
		"umidade negativa":   {`{"boot_id":"abc","seq":1,"temperature":24,"humidity":-1,"raw_temperature":24.4,"raw_humidity":51,"actuator":false}`, false},
		"sem boot":           {`{"seq":1,"temperature":24.5,"humidity":50,"raw_temperature":24.4,"raw_humidity":51,"actuator":false}`, false},
	}
	for name, c := range cases {
		var tm Telemetry
		if err := json.Unmarshal([]byte(c.body), &tm); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := tm.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: esperado ok=%v, erro=%v", name, c.ok, err)
		}
	}
}

func TestLimitsValidate(t *testing.T) {
	if err := (Limits{2, 8, 30, 60}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, l := range []Limits{{10, 5, 30, 60}, {2, 8, 70, 60}, {-50, 8, 30, 60}, {2, 8, 30, 120}} {
		if l.Validate() == nil {
			t.Errorf("limite inválido aceito: %+v", l)
		}
	}
}

func TestDeviceID(t *testing.T) {
	for id, ok := range map[string]bool{"esp32-sala-01": true, "a": false, "ESP": false, "x/y": false, "+": false, "#": false} {
		if ValidDeviceID(id) != ok {
			t.Errorf("%q: esperado %v", id, ok)
		}
	}
}
