# Dockerfile for the scientific/v0.1.0 performance harness.
# Pins Linux/amd64 + Go 1.18 + the rule-set fixture so any machine
# that can run Docker reproduces the same numbers. CPU frequency,
# SMT state, and thermal posture remain host-residual (per ADR-0012);
# REPORT.md captures the reference host's lscpu summary alongside
# the bars so reproductions on different hardware see the posture.

FROM --platform=linux/amd64 golang:1.18 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM build AS bench
WORKDIR /src
# -count=50 produces 50 trial means per benchmark; -benchtime=1s
# makes each trial loop b.N iterations until it accumulates ~1 second
# of timed work. trial-mean methodology per ADR-0012.
CMD ["go", "test", "-bench=.", "-benchmem", "-count=50", "-benchtime=1s", "./scientific/v0.1.0/..."]
