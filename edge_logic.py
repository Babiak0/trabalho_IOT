"""Mesma filtragem no ESP32 virtual e na demonstração Python."""
class EdgeFilter:
    def __init__(self, size=3):
        self.size = size
        self.values = []

    def add(self, temperature, humidity):
        # Comparações rejeitam também NaN e infinitos.
        if (isinstance(temperature, bool) or isinstance(humidity, bool)
                or not isinstance(temperature, (int, float))
                or not isinstance(humidity, (int, float))
                or not -40 <= temperature <= 80 or not 0 <= humidity <= 100):
            raise ValueError('Leitura do sensor inválida')
        self.values.append((temperature, humidity))
        self.values = self.values[-self.size:]
        n = len(self.values)
        return (round(sum(x[0] for x in self.values) / n, 2),
                round(sum(x[1] for x in self.values) / n, 2))
