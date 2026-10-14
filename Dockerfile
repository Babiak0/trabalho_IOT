# Etapa 1: compila os binários estáticos
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/simulator ./cmd/simulator

# Etapa 2: imagem mínima (sem shell), com certificados de CA para TLS e usuário sem privilégios
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /out/simulator /
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=4s --retries=3 CMD ["/server", "-healthcheck"]
ENTRYPOINT ["/server"]
