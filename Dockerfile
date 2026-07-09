# Build the firedrill binary (CLI + operator in one).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X github.com/kirilurbonas/FireDrill/pkg/version.Version=${VERSION}" \
      -o /firedrill ./cmd/firedrill

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /firedrill /firedrill
ENTRYPOINT ["/firedrill"]
