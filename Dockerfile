# OPL-API — Open Perf Lab control plane
FROM golang:1.25-alpine AS builder
RUN apk --no-cache add git ca-certificates
WORKDIR /app
ENV GOPROXY=https://proxy.golang.org,direct
ENV GOSUMDB=sum.golang.org
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o opl-api .

FROM docker:27-cli AS dockercli

FROM debian:bookworm-slim AS opl-api
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl wget bash \
 && rm -rf /var/lib/apt/lists/*
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
WORKDIR /root/
COPY --from=builder /app/opl-api .
COPY scripts/ /opt/opa/scripts/
RUN mkdir -p /opa-jmeter
ENV HTTP_ADDR=:8092
EXPOSE 8092
CMD ["./opl-api"]


# Orchestrator = same image, different command (compose: opl-api orchestrator).
FROM opl-api AS opl-orchestrator
ENV ORCHESTRATOR_LISTEN_ADDR=:8097
CMD ["./opl-api", "orchestrator"]

# Ephemeral JMeter runner — one container per load run.
FROM eclipse-temurin:17-jre-jammy AS opl-runner-jmeter
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /opt/jmeter /home/opa \
 && chown -R 65532:65532 /home/opa
USER 65532:65532
WORKDIR /home/opa
CMD ["sleep", "infinity"]
