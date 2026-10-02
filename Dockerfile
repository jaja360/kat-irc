FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kat-irc .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && addgroup -g 568 apps && adduser -D -u 568 -G apps apps
COPY --from=build /out/kat-irc /usr/local/bin/kat-irc
USER 568:568
WORKDIR /app
ENTRYPOINT ["kat-irc"]
CMD ["serve", "-config", "/data/config.json"]
