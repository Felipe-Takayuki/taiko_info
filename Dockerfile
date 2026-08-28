# syntax=docker/dockerfile:1

# -----------------------------------------------------------------------------
# Estágio 1: Build (Compilação do binário Go com CGO habilitado para SQLite)
# -----------------------------------------------------------------------------
FROM golang:alpine AS builder

WORKDIR /src

# Habilita download automático de toolchain se necessário e instala GCC
ENV GOTOOLCHAIN=auto
RUN apk add --no-cache gcc musl-dev git

# Cache de dependências do Go
COPY go.mod go.sum ./
RUN go mod download

# Copia código fonte
COPY . .

# Compilação otimizada para produção (strip debug info com -s -w)
RUN CGO_ENABLED=1 GOOS=linux go build -ldflags="-s -w" -o /bin/taiko .

# -----------------------------------------------------------------------------
# Estágio 2: Imagem Final Leve (~25MB)
# -----------------------------------------------------------------------------
FROM alpine:latest

WORKDIR /app

# Pacotes essenciais de runtime:
# - ca-certificates: Para chamadas HTTPS seguras (API do Gemini e WhatsApp)
# - tzdata: Para timezone correto das datas dos eventos (America/Sao_Paulo)
# - curl: Para healthchecks
RUN apk add --no-cache ca-certificates tzdata curl

# Copia o binário compilado
COPY --from=builder /bin/taiko /app/taiko

# Cria diretório de persistência de dados
RUN mkdir -p /app/data

# Define variáveis de ambiente padrão
ENV DB_PATH=/app/data/app.db \
    HTTP_PORT=8080 \
    TIMEZONE=America/Sao_Paulo \
    TZ=America/Sao_Paulo

EXPOSE 8080

ENTRYPOINT ["/app/taiko"]
CMD ["all"]
