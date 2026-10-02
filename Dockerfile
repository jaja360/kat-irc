FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/kat-irc .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && addgroup -g 10001 kat && adduser -D -u 10001 -G kat kat
COPY --from=build /out/kat-irc /usr/local/bin/kat-irc
USER 10001:10001
WORKDIR /app
ENTRYPOINT ["kat-irc"]
CMD ["serve", "-config", "/data/config.json"]
