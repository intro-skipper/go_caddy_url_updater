FROM golang:1.26.6 AS builder

WORKDIR /go/src/github.com/intro-skipper/go_caddy_url_updater

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o app .

# deployment image
FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

COPY --from=builder /go/src/github.com/intro-skipper/go_caddy_url_updater/app .
CMD [ "./app" ]

EXPOSE 8080
