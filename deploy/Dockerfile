# Build with the repository root as the context:
#   docker build -f deploy/Dockerfile .
# docker-compose.yml in this directory already does that.

FROM golang:1.26 AS build

WORKDIR /src

# Dependencies are copied and downloaded on their own so that editing source does not invalidate
# the module cache layer — the difference between a rebuild that takes seconds and one that
# re-downloads the world.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off makes the binary static, which is what lets the final stage be a distroless image with no
# libc in it. -trimpath keeps build machine paths out of the binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/vivecserver ./cmd/vivecserver

# distroless/static has no shell, no package manager and no setuid binaries, so the attack surface
# of the running container is the server and nothing else. It does carry ca-certificates, which a
# TLS connection to a managed PostgreSQL needs.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/vivecserver /usr/local/bin/vivecserver

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/vivecserver"]
