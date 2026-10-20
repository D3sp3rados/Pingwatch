FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata curl jq iputils-tracepath || apk add --no-cache ca-certificates tzdata curl jq
COPY run.sh /run.sh
RUN chmod +x /run.sh
ENTRYPOINT ["/run.sh"]
