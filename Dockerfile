FROM golang:1.13.5 as builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY es-health-keeper.go .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a --ldflags '-w -extldflags "-static"' -tags netgo -installsuffix netgo -o main .


FROM alpine:3.10 as certs

RUN apk add --no-cache ca-certificates


FROM scratch

COPY --from=builder /app/main /
COPY --from=certs /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

USER 65534

CMD ["/main"]
