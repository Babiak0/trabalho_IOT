// Processamento na borda (Edge Computing): validação, rejeição de picos e média móvel.
// C++ puro, sem dependência do Arduino, para facilitar entendimento e testes.
#pragma once
#include <math.h>

struct Limits {
  float tempMin = 15, tempMax = 30, humMin = 30, humMax = 70;

  bool valid() const {
    return tempMin >= -40 && tempMax <= 80 && tempMin < tempMax &&
           humMin >= 0 && humMax <= 100 && humMin < humMax;
  }
};

class EdgeFilter {
 public:
  static const int WINDOW = 5;          // tamanho da média móvel
  static constexpr float SPIKE_T = 5;   // salto máximo aceito entre leituras (°C)
  static constexpr float SPIKE_H = 15;  // salto máximo aceito entre leituras (%)
  static const int SPIKE_CONFIRM = 3;   // saltos seguidos = mudança real, não ruído

  // Devolve true se a leitura foi aceita. Leituras rejeitadas incrementam discarded.
  bool add(float t, float h) {
    if (isnan(t) || isnan(h) || t < -40 || t > 80 || h < 0 || h > 100) {
      discarded++;
      return false;
    }
    if (count >= 3 && (fabsf(t - avgT()) > SPIKE_T || fabsf(h - avgH()) > SPIKE_H)) {
      if (++spikes < SPIKE_CONFIRM) {
        discarded++;
        return false;
      }
      count = 0;  // o salto se confirmou: recomeça a média a partir do novo patamar
    }
    spikes = 0;
    temps[head] = t;
    hums[head] = h;
    head = (head + 1) % WINDOW;
    if (count < WINDOW) count++;
    return true;
  }

  bool ready() const { return count > 0; }
  float avgT() const { return avg(temps); }
  float avgH() const { return avg(hums); }

  // Número de leituras descartadas desde a última chamada (enviado na telemetria).
  int takeDiscarded() {
    int d = discarded;
    discarded = 0;
    return d;
  }

 private:
  float temps[WINDOW] = {0};
  float hums[WINDOW] = {0};
  int head = 0, count = 0, spikes = 0, discarded = 0;

  float avg(const float* v) const {
    if (count == 0) return NAN;
    float s = 0;
    // As `count` posições mais recentes terminam em head-1.
    for (int i = 0; i < count; i++) s += v[(head - 1 - i + WINDOW) % WINDOW];
    return s / count;
  }
};

// Decide se vale a pena publicar agora (report by exception):
// envia quando o valor muda o suficiente, quando há alarme, ou como "batimento" periódico.
struct ReportPolicy {
  static constexpr float DELTA_T = 0.3;            // °C
  static constexpr float DELTA_H = 1.0;            // %
  static const unsigned long HEARTBEAT_MS = 10000;  // envio mínimo mesmo sem mudança
  static const unsigned long ALARM_MS = 2000;       // em alarme, envia a cada leitura

  float lastT = NAN, lastH = NAN;
  bool lastActuator = false, lastAlarm = false;
  unsigned long lastSent = 0;
  bool sentOnce = false;

  bool shouldSend(float t, float h, bool actuator, bool alarm, unsigned long now) const {
    if (!sentOnce || actuator != lastActuator || alarm != lastAlarm) return true;
    unsigned long elapsed = now - lastSent;
    if (alarm && elapsed >= ALARM_MS) return true;
    if (elapsed >= HEARTBEAT_MS) return true;
    return fabsf(t - lastT) >= DELTA_T || fabsf(h - lastH) >= DELTA_H;
  }

  void sent(float t, float h, bool actuator, bool alarm, unsigned long now) {
    lastT = t;
    lastH = h;
    lastActuator = actuator;
    lastAlarm = alarm;
    lastSent = now;
    sentOnce = true;
  }
};
