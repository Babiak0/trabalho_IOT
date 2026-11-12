#!/bin/sh
# Monta uma pasta de sketch para gravar a placa física pela Arduino IDE (DHT11, LEDs nos GPIO 26/25).
# Saída em arduino-bundle/ (fora do git, pois contém senhas).
set -eu
cd "$(dirname "$0")/.."
[ -f include/secrets.h ] || { echo "Crie include/secrets.h a partir de include/secrets.example.h"; exit 1; }
out=arduino-bundle/esp32_ambiente
mkdir -p "$out"
cp src/main.cpp "$out/esp32_ambiente.ino"
cp src/edge.h include/secrets.h "$out/"
echo "Pronto: $(pwd)/$out (abra esp32_ambiente.ino na Arduino IDE)"
