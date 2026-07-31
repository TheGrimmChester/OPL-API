# OPA Perf Lab — Wave 29/31 load scenarios + Docker JMeter
FROM golang:1.25-alpine AS builder
RUN apk --no-cache add git ca-certificates
WORKDIR /app
ENV GOPROXY=https://proxy.golang.org,direct
ENV GOSUMDB=sum.golang.org
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o opa-perf-lab .

FROM docker:27-cli AS dockercli

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl wget bash \
 && rm -rf /var/lib/apt/lists/*
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
WORKDIR /root/
COPY --from=builder /app/opa-perf-lab .
COPY scripts/ /opt/opa/scripts/
RUN mkdir -p /opa-jmeter
ENV HTTP_ADDR=:8092
EXPOSE 8092
CMD ["./opa-perf-lab"]
