# Dockerfile for the production markup-server binary. See ADR-0013.
# Build context is the repo root: `docker build -f cmd/markup-server/Dockerfile .`
#
# Two stages: a golang:1.18 build stage compiles the binary
# (CGO_ENABLED=0 for static linking, -trimpath + -ldflags="-s -w"
# for reproducibility and image size), and a distroless static
# runtime stage. The runtime image inherits a non-root user (65532)
# pre-configured by distroless's :nonroot variant so the Kubernetes
# manifests do not need to set securityContext.runAsUser explicitly
# to get non-root execution.

FROM golang:1.18 AS build
WORKDIR /src

# Copy module manifest first to make the module-download layer
# cacheable across source-only changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the repo and compile.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
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
