#!/bin/sh
# Junta secrets.h + edge.h + main.cpp em um único sketch.ino para colar no Wokwi (navegador).
# Saída em wokwi-bundle/ (fora do git, pois contém senhas).
set -eu
cd "$(dirname "$0")/.."
[ -f include/secrets.h ] || { echo "Crie include/secrets.h a partir de include/secrets.example.h"; exit 1; }
mkdir -p wokwi-bundle
{
  echo "// Gerado por tools/bundle-wokwi.sh - NÃO commitar (contém senhas)."
  echo "// ===== secrets.h ====="
  grep -v '#pragma once' include/secrets.h
  echo
  echo "// ===== edge.h ====="
  grep -v '#pragma once' src/edge.h
  echo
  echo "// ===== main.cpp ====="
  grep -vE '#include "(edge|secrets)\.h"' src/main.cpp
} > wokwi-bundle/sketch.ino
cp diagram.json libraries.txt wokwi-bundle/
echo "Pronto: $(pwd)/wokwi-bundle (sketch.ino, diagram.json, libraries.txt)"
