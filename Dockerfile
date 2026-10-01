FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates build-base

COPY . /build
WORKDIR /build
RUN ./build.sh -o /usr/bin/mautrix-discord

FROM alpine:3.22

ENV UID=1337 \
    GID=1337

RUN apk add --no-cache su-exec ca-certificates ffmpeg lottieconverter

COPY --from=builder /usr/bin/mautrix-discord /usr/bin/mautrix-discord
COPY --from=builder /build/example-config.yaml /opt/mautrix-discord/example-config.yaml
COPY --from=builder /build/docker-run.sh /docker-run.sh
VOLUME /data
WORKDIR /data

CMD ["/docker-run.sh"]
