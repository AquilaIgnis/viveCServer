# Vive Notes Community server

The self-hosted sync server for [viveNotes](https://github.com/AquilaIgnis/viveNotes). It is a
small Go service backed by PostgreSQL and distributed as a prebuilt container image.

The server stores, orders, and authorises. It never interprets a note: page documents are opaque
payloads, and everything that needs the document model runs on the client.

# Deploy with docker

Use docs/docker-compose.yml and env sample

`docker compose up -d`

# Dev

Local image build

```bash
  docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml up --build -d
```

Web interface over: ` http://localhost:8080`

Android emulator: `http://10.0.2.2:5444`
