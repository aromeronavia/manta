# manta-server: enter a Dota 2 match id, get the interactive replay viewer.
#
#   docker build -t manta-server .
#   docker run -p 8080:8080 -v manta-data:/data manta-server
#
# Build args:
#   MINIMAPS=1  fetch the community minimap images used by ReDota (default);
#               set to 0 to skip and let the viewer draw a schematic map.

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/manta-server ./cmd/manta-server

FROM alpine:3.20 AS assets
ARG MINIMAPS=1
RUN apk add --no-cache curl && mkdir -p /assets/minimap && \
    if [ "$MINIMAPS" = "1" ]; then \
      for v in 7.23 7.29 7.33 7.38 7.40; do \
        curl -fsSL -o /assets/minimap/$v.webp "https://raw.githubusercontent.com/timkurvers/redota/master/public/images/minimap/$v.webp"; \
      done; \
    fi

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/manta-server /manta-server
COPY --from=assets /assets /assets
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/manta-server", "-addr", "0.0.0.0:8080", "-data", "/data", "-assets", "/assets"]
