# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/literouter \
    ./cmd/literouter
RUN mkdir /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/literouter /literouter
COPY --chown=nonroot:nonroot --from=build /out/data /data

EXPOSE 8317 1455 1456 1457
USER nonroot:nonroot
ENTRYPOINT ["/literouter"]
