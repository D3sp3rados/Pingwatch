# ---- Build: Server + Agenten (Windows, Linux x64, Raspberry Pi) ----
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0
RUN (apk add --no-cache ca-certificates >/dev/null 2>&1 || true) \
 && go build -trimpath -ldflags "-s -w" -o /out/pingwatch . \
 && mkdir -p /out/downloads \
 && GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H windowsgui -s -w" -o /out/downloads/pingwatch-agent.exe . \
 && GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /out/downloads/pingwatch-agent-linux-amd64 . \
 && GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o /out/downloads/pingwatch-agent-linux-arm64 . \
 && GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o /out/downloads/pingwatch-agent-darwin-arm64 . \
 && GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /out/downloads/pingwatch-agent-darwin-amd64 . \
 && ( GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags "-s -w" -o /tmp/pw-armv7 . \
 && go run ./tools/mkimage -bin /out/downloads/pingwatch-agent-linux-arm64 -arch arm64 -out /out/downloads/pingwacht-agent-container-arm64.tar \
 && go run ./tools/mkimage -bin /tmp/pw-armv7 -arch arm -variant v7 -out /out/downloads/pingwacht-agent-container-arm.tar \
 && go run ./tools/mkimage -bin /out/downloads/pingwatch-agent-linux-amd64 -arch amd64 -out /out/downloads/pingwacht-agent-container-amd64.tar \
 || echo "Container-Images übersprungen" )

# ---- Laufzeit ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && (apk add --no-cache iputils-tracepath || apk add --no-cache iputils || true)
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
