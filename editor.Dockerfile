FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/editor-server ./cmd/editor-server

FROM debian:trixie-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends adbd \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m --create-home --shell /bin/bash --user-group arduino

COPY --from=build /out/editor-server /usr/local/bin/editor-server

WORKDIR /home/arduino
EXPOSE 5555 8998

CMD ["/bin/sh", "-c", "su arduino -c adbd & /usr/local/bin/editor-server -addr :8998"]
