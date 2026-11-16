# Один образ на все сервисы: общий модуль собирается один раз, а нужный
# бинарник выбирается командой в docker-compose. Образ без shell и пакетного
# менеджера (distroless) — меньше поверхность атаки.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
ENV ZONEINFO=/zoneinfo.zip
COPY --from=build /out/ /app/
USER nonroot:nonroot
