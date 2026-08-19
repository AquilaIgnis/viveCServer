# Build with the repository root as the context:
#   docker build -f deploy/Dockerfile .
# docker-compose.yml in this directory already does that.

FROM golang:1.26 AS build

WORKDIR /src

# Dependencies
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off makes the binary static
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/vivecserver ./cmd/vivecserver

# An empty directory to carry into the final image. It exists only so that the attachment volume
# mounted over it inherits nonroot's ownership: Docker copies the image's ownership at a mount point
# into a fresh named volume, and creates the volume root as root:root when there is nothing there to
# copy — which a container running as nonroot cannot then write to.
RUN mkdir -p /out/blobs

# distroless/static has no shell, no package manager and no setuid binaries, so the attack surface
# of the running container is the server and nothing else. It does carry ca-certificates, which a
# TLS connection to a managed PostgreSQL needs.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/vivecserver /usr/local/bin/vivecserver

# Attachment bytes (S5). The rootfs is read-only in the supplied Compose file, so this is the one
# path the server needs a writable volume at; VIVE_BLOB_DIR moves it.
COPY --from=build --chown=nonroot:nonroot /out/blobs /var/lib/vivecserver/blobs
VOLUME ["/var/lib/vivecserver/blobs"]

USER nonroot:nonroot

WORKDIR /

EXPOSE 8080 8281

ENTRYPOINT ["/usr/local/bin/vivecserver"]
