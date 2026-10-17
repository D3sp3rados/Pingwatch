# ---- Build: Server + Agenten (Windows, Linux x64, Raspberry Pi) ----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w" -o /out/pingwatch . \
 && mkdir -p /out/downloads \
 && GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H windowsgui -s -w" -o /out/downloads/pingwatch-agent.exe . \
 && GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /out/downloads/pingwatch-agent-linux-amd64 . \
 && GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o /out/downloads/pingwatch-agent-linux-arm64 .

# ---- Laufzeit ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Europe/Berlin \
    DATA_DIR=/data \
    PORT=8080 \
    AGENT_DIR=/app/downloads
COPY --from=build /out/pingwatch /app/pingwatch
COPY --from=build /out/downloads /app/downloads
RUN adduser -D -u 1000 pingwatch && mkdir -p /data && chown -R pingwatch:pingwatch /data
USER pingwatch
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/app/pingwatch", "server"]
