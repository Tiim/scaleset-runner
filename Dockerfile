# Controller image (NFR-011: at most 50 MB compressed).
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/scaleset-runner ./cmd/scaleset-runner

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/scaleset-runner /scaleset-runner
# Runs as root so it can use the host Docker socket (C-005).
USER 0
EXPOSE 9090
HEALTHCHECK --interval=30s --timeout=10s --start-period=60s CMD ["/scaleset-runner", "healthcheck"]
ENTRYPOINT ["/scaleset-runner"]
