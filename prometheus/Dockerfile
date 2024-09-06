ARG BUILD_DATE=0
ARG COMMIT=0
ARG VERSION=unknown
ARG BINARY=wattpilot_exporter

FROM golang:stretch as builder
ARG BINARY
RUN mkdir -p $GOPATH/pkg/mod $GOPATH/bin $GOPATH/src /${BINARY}
COPY . /${BINARY}
WORKDIR /${BINARY}

RUN CGO_ENABLED=0 make ${BINARY}

FROM debian:buster-slim

ARG BUILD_DATE
ARG VERSION
ARG BINARY

ENV WATTPILOT_HOST=""
ENV WATTPILOT_PWD=""
ENV WATTPILOT_LOG=Info

# install tools for healthcheck
RUN apt-get -y update &&  \ 
    apt-get install --no-install-recommends -y \
    curl \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /${BINARY}/prometheus/${BINARY} /image

ENTRYPOINT [ "/image" ]%

HEALTHCHECK --interval=1m --timeout=3s \
  CMD curl -fs http://localhost:9101/metrics | tail -n 1 | grep "^wattpilot_.*" || exit 1
