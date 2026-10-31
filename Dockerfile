FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sms-receiver .

FROM alpine:3.22

RUN addgroup -S app && adduser -S -G app -H app \
    && mkdir -p /data && chown app:app /data

COPY --from=build /out/sms-receiver /usr/local/bin/sms-receiver

ENV LISTEN_ADDR=:8044 \
    DB_PATH=/data/sms.db

USER app
WORKDIR /app
VOLUME ["/data"]
EXPOSE 8044

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8044/healthz >/dev/null || exit 1

ENTRYPOINT ["sms-receiver"]
