// Teste da lógica de borda no computador:  c++ -std=c++17 -I../src edge_test.cpp && ./a.out
#include <cassert>
#include <cstdio>
#include "edge.h"

int main() {
  EdgeFilter f;
  assert(!f.add(NAN, 50) && !f.add(100, 50) && !f.add(20, 120));  // leituras impossíveis
  assert(f.takeDiscarded() == 3 && !f.ready());
  for (int i = 0; i < 5; i++) assert(f.add(20 + i, 50));
  assert(fabsf(f.avgT() - 22) < 0.01);                    // média de 20..24
  assert(f.add(26, 50) && fabsf(f.avgT() - 23.2f) < 0.01); // janela desliza: 21..24 e 26
  assert(!f.add(60, 50) && f.takeDiscarded() == 1);       // pico isolado rejeitado
  assert(f.add(25, 50));                                  // valor normal zera a contagem de picos
  assert(!f.add(40, 50) && !f.add(40, 50) && f.add(40, 50));  // 3 saltos seguidos = mudança real
  assert(fabsf(f.avgT() - 40) < 0.01);                    // média recomeça no novo patamar

  ReportPolicy p;
  assert(p.shouldSend(20, 50, false, false, 0));  // primeiro envio
  p.sent(20, 50, false, false, 0);
  assert(!p.shouldSend(20.1, 50.5, false, false, 3000));  // mudança pequena: não envia
  assert(p.shouldSend(20.4, 50, false, false, 3000));     // passou do delta
  assert(p.shouldSend(20, 50, true, false, 3000));        // atuador mudou
  assert(p.shouldSend(20, 50, false, false, 10000));      // batimento
  p.sent(31, 50, false, true, 20000);
  assert(p.shouldSend(31, 50, false, true, 22000));       // em alarme envia a cada 2 s
  Limits l; assert(l.valid()); l.tempMin = 40; assert(!l.valid());
  puts("edge: todos os testes passaram");
}
