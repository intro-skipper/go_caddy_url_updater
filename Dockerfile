FROM golang:1.25.4 AS builder

WORKDIR /go/src/github.com/jumoog/go_caddy_url_updater

COPY . .

RUN go get .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o app .

# deployment image
FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /root/

COPY --from=builder /go/src/github.com/jumoog/go_caddy_url_updater/app .
CMD [ "./app" ]

EXPOSE 8080