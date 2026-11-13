FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY gen ./gen
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ledger ./cmd/ledger

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ledger /ledger
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/ledger"]
