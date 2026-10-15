// Firmware do ESP32: DHT22 -> processamento na borda -> MQTT (TLS) -> nuvem, e comandos de volta.
//
// Hardware:
//   DHT22 dados .......... GPIO 4
//   Ventilação (relé/LED)  GPIO 18   atuador controlado pela nuvem
//   Alarme local (LED)     GPIO 19   aceso quando a borda detecta valor fora dos limites
//
// Tópicos: ambiente/<DEVICE_ID>/{telemetry,status,ack} (publica) e {cmd,config} (assina).
#include <Arduino.h>
#include <ArduinoJson.h>
#include <DHT.h>
#include <Preferences.h>
#include <PubSubClient.h>
#include <WiFi.h>
#include <WiFiClientSecure.h>
#include <sys/time.h>

#include "edge.h"
#include "secrets.h"

#define FW_VERSION "1.0.0"

const int PIN_DHT = 4;
const int PIN_ACTUATOR = 18;
const int PIN_ALARM = 19;
const unsigned long SAMPLE_MS = 2000;      // o DHT22 aceita no máximo 1 leitura a cada 2 s
const unsigned long FAILSAFE_MS = 30000;   // sem nuvem por esse tempo -> borda assume o controle

#if MQTT_USE_TLS
WiFiClientSecure net;
#else
WiFiClient net;
#endif
PubSubClient mqtt(net);
DHT dht(PIN_DHT, DHT22);
Preferences prefs;
EdgeFilter filter;
ReportPolicy policy;
Limits limits;

String topicBase, bootId;
unsigned long seq = 0;
bool actuator = false;
bool failsafe = false;  // atuador ligado pela própria borda, sem a nuvem
unsigned long lastSample = 0, lastMqttTry = 0, mqttDownSince = 0, mqttRetryMs = 2000;
float lastRawT = NAN, lastRawH = NAN;
int sensorErrors = 0;
String executed[10];  // últimos comandos executados (evita repetir em reentregas QoS 1)
int executedHead = 0;

// ---------- utilidades ----------

String topic(const char* kind) { return topicBase + "/" + kind; }

double round2(float v) { return round(v * 100.0) / 100.0; }

int64_t epochMs() {
  struct timeval tv;
  gettimeofday(&tv, nullptr);
  if (tv.tv_sec < 1600000000) return 0;  // NTP ainda não sincronizou
  return (int64_t)tv.tv_sec * 1000 + tv.tv_usec / 1000;
}

void setActuator(bool on) {
  actuator = on;
  digitalWrite(PIN_ACTUATOR, on ? HIGH : LOW);
}

void loadLimits() {
  prefs.begin("edge", true);
  Limits l;
  l.tempMin = prefs.getFloat("tmin", l.tempMin);
  l.tempMax = prefs.getFloat("tmax", l.tempMax);
  l.humMin = prefs.getFloat("hmin", l.humMin);
  l.humMax = prefs.getFloat("hmax", l.humMax);
  prefs.end();
  if (l.valid()) limits = l;
}

void saveLimits() {
  prefs.begin("edge", false);
  prefs.putFloat("tmin", limits.tempMin);
  prefs.putFloat("tmax", limits.tempMax);
  prefs.putFloat("hmin", limits.humMin);
  prefs.putFloat("hmax", limits.humMax);
  prefs.end();
}

// ---------- mensagens recebidas da nuvem ----------

void handleCommand(JsonDocument& doc) {
  const char* id = doc["id"] | "";
  const char* action = doc["action"] | "";
  bool on = strcmp(action, "on") == 0;
  if (strlen(id) != 32 || (!on && strcmp(action, "off") != 0)) {
    Serial.println("Comando inválido descartado");
    return;
  }
  bool seen = false;
  for (auto& e : executed) seen = seen || e == id;
  if (!seen) {
    setActuator(on);
    failsafe = false;  // a nuvem voltou a comandar
    executed[executedHead] = id;
    executedHead = (executedHead + 1) % 10;
    Serial.printf("Comando executado: ventilação %s\n", on ? "LIGADA" : "DESLIGADA");
  }
  JsonDocument ack;
  ack["id"] = id;
  ack["actuator"] = actuator;
  char buf[96];
  serializeJson(ack, buf);
  mqtt.publish(topic("ack").c_str(), buf);
}

void handleConfig(JsonDocument& doc) {
  Limits l;
  l.tempMin = doc["temp_min"] | NAN;
  l.tempMax = doc["temp_max"] | NAN;
  l.humMin = doc["hum_min"] | NAN;
  l.humMax = doc["hum_max"] | NAN;
  if (!l.valid()) {
    Serial.println("Limites inválidos ignorados");
    return;
  }
  limits = l;
  saveLimits();  // persiste para o modo de segurança funcionar mesmo após reiniciar sem rede
  Serial.printf("Limites: %.1f a %.1f °C, %.1f a %.1f %%\n", l.tempMin, l.tempMax, l.humMin, l.humMax);
}

void onMessage(char* t, byte* payload, unsigned int len) {
  JsonDocument doc;
  if (deserializeJson(doc, payload, len)) return;
  String tp(t);
  if (tp == topic("cmd")) handleCommand(doc);
  else if (tp == topic("config")) handleConfig(doc);
}

// ---------- conectividade (não bloqueante) ----------

void ensureMqtt(unsigned long now) {
  if (mqtt.connected()) {
    mqttDownSince = 0;
    return;
  }
  if (mqttDownSince == 0) mqttDownSince = now;
  if (WiFi.status() != WL_CONNECTED || now - lastMqttTry < mqttRetryMs) return;
  lastMqttTry = now;
  String clientId = String(DEVICE_ID) + "-" + bootId.substring(0, 6);
  Serial.printf("Conectando ao broker %s:%d...\n", MQTT_HOST, MQTT_PORT);
  // Last Will: se o ESP32 cair, o broker publica offline (retido) no lugar dele.
  if (mqtt.connect(clientId.c_str(), MQTT_USER, MQTT_PASSWORD, topic("status").c_str(), 1, true, "{\"online\":false}")) {
    mqttRetryMs = 2000;
    String status = String("{\"online\":true,\"fw\":\"" FW_VERSION "\",\"ip\":\"") + WiFi.localIP().toString() + "\"}";
    mqtt.publish(topic("status").c_str(), status.c_str(), true);
    mqtt.subscribe(topic("cmd").c_str(), 1);
    mqtt.subscribe(topic("config").c_str(), 1);
    Serial.println("Broker conectado");
  } else {
    mqttRetryMs = min(mqttRetryMs * 2, 30000UL);  // backoff exponencial
    Serial.printf("Falha MQTT (estado %d), nova tentativa em %lus\n", mqtt.state(), mqttRetryMs / 1000);
  }
}

// ---------- ciclo de medição ----------

void sampleAndPublish(unsigned long now) {
  float rawT = dht.readTemperature();
  float rawH = dht.readHumidity();
  if (!isnan(rawT) && !isnan(rawH)) {
    sensorErrors = 0;
    lastRawT = rawT;
    lastRawH = rawH;
  } else if (++sensorErrors % 5 == 1) {
    Serial.printf("Falha ao ler o DHT22 (%d seguidas). Verifique a ligação no GPIO %d.\n", sensorErrors, PIN_DHT);
  }
  filter.add(rawT, rawH);  // leituras inválidas são contadas como descarte
  if (!filter.ready()) return;
  float t = filter.avgT(), h = filter.avgH();

  // Decisões locais, mesmo sem nuvem.
  bool alarm = t > limits.tempMax || t < limits.tempMin || h > limits.humMax || h < limits.humMin;
  digitalWrite(PIN_ALARM, alarm ? HIGH : LOW);
  bool cloudLost = mqttDownSince != 0 && now - mqttDownSince > FAILSAFE_MS;
  if (cloudLost && t > limits.tempMax && !actuator) {
    setActuator(true);
    failsafe = true;
    Serial.println("Sem nuvem e temperatura alta: ventilação ligada pela borda");
  }
  if (failsafe && t <= limits.tempMax - 1.0) {
    setActuator(false);
    failsafe = false;
    Serial.println("Temperatura normalizada: modo de segurança encerrado");
  }

  if (!mqtt.connected() || !policy.shouldSend(t, h, actuator, alarm, now)) return;
  JsonDocument doc;
  doc["boot_id"] = bootId;
  doc["seq"] = ++seq;
  doc["ts"] = epochMs();
  doc["temperature"] = round2(t);
  doc["humidity"] = round2(h);
  doc["raw_temperature"] = round2(lastRawT);
  doc["raw_humidity"] = round2(lastRawH);
  doc["actuator"] = actuator;
  JsonObject edge = doc["edge"].to<JsonObject>();
  edge["alarm"] = alarm;
  edge["failsafe"] = failsafe;
  edge["discarded"] = filter.takeDiscarded();
  char buf[320];
  size_t n = serializeJson(doc, buf);
  if (mqtt.publish(topic("telemetry").c_str(), (const uint8_t*)buf, n)) {
    policy.sent(t, h, actuator, alarm, now);
    Serial.printf("Telemetria #%lu: %.2f °C  %.2f %%  ventilação=%d  alarme=%d\n", seq, t, h, actuator, alarm);
  }
}

void setup() {
  Serial.begin(115200);
  pinMode(PIN_ACTUATOR, OUTPUT);
  pinMode(PIN_ALARM, OUTPUT);
  setActuator(false);
  digitalWrite(PIN_ALARM, LOW);
  dht.begin();
  loadLimits();

  char id[17];
  snprintf(id, sizeof(id), "%08lx%08lx", (unsigned long)esp_random(), (unsigned long)esp_random());
  bootId = id;
  topicBase = String(TOPIC_PREFIX) + "/" + DEVICE_ID;

  WiFi.mode(WIFI_STA);
  WiFi.setAutoReconnect(true);
  WiFi.begin(WIFI_SSID, WIFI_PASSWORD);
  configTime(0, 0, "pool.ntp.org", "time.google.com");  // horário para medir latência

#if MQTT_USE_TLS
  net.setCACert(ROOT_CA);
#endif
  mqtt.setServer(MQTT_HOST, MQTT_PORT);
  mqtt.setCallback(onMessage);
  mqtt.setBufferSize(512);
  mqtt.setKeepAlive(30);  // tolera lentidão (ex.: simulação no Wokwi); quedas reais o back-end detecta pela falta de telemetria
  mqtt.setSocketTimeout(5);
  Serial.printf("Dispositivo %s (fw %s) iniciado\n", DEVICE_ID, FW_VERSION);
}

void loop() {
  unsigned long now = millis();
  ensureMqtt(now);
  mqtt.loop();
  if (now - lastSample >= SAMPLE_MS) {
    lastSample = now;
    sampleAndPublish(now);
  }
  delay(10);
}
