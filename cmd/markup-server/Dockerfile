# Dockerfile for the production markup-server binary. See ADR-0013
# (initial deployment artifacts) + ADR-0018 (multi-arch build).
# Build context is the repo root: `docker build -f cmd/markup-server/Dockerfile .`
#
# Two stages: a golang:1.18 build stage compiles the binary
# (CGO_ENABLED=0 for static linking, -trimpath + -ldflags="-s -w"
# for reproducibility and image size), and a distroless static
# runtime stage. The runtime image inherits a non-root user (65532)
# pre-configured by distroless's :nonroot variant so the Kubernetes
# manifests do not need to set securityContext.runAsUser explicitly
# to get non-root execution.
#
# Multi-arch (ADR-0018): the build stage runs on the BUILD platform
# (e.g. linux/amd64 on a GitHub-hosted runner) but cross-compiles the
# binary for the TARGET platform (linux/amd64 OR linux/arm64). The
# resulting image manifest is a list so `docker pull` on Apple Silicon
# picks the arm64 variant and skips the Rosetta emulation tax. buildx
# is required (the CI workflow installs it via docker/setup-buildx-action).

# --platform=$BUILDPLATFORM keeps the build stage on the runner's
# native architecture (no QEMU emulation during go build) while
# cross-compiling via GOARCH below.
FROM --platform=$BUILDPLATFORM golang:1.18 AS build
WORKDIR /src

# BUILDPLATFORM, TARGETOS, TARGETARCH are provided automatically by
# buildx when the build is launched with --platform. Without buildx
# the args fall through with empty values; the go build command below
# defaults GOARCH to the host arch in that case so a plain
# `docker build` still produces a working amd64-only image.
ARG BUILDPLATFORM
ARG TARGETOS
ARG TARGETARCH

# Copy module manifest first to make the module-download layer
# cacheable across source-only changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the repo and compile.
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" \
    -o /out/markup-server ./cmd/markup-server

FROM gcr.io/distroless/static-debian11:nonroot
COPY --from=build /out/markup-server /usr/local/bin/markup-server

# Distroless :nonroot ships USER 65532:65532. The explicit USER line
# documents the inherited posture and survives a base-image change
# that might drop the implicit default.
USER 65532:65532

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/markup-server"]
