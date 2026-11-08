FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /parkingbot ./cmd/parkingbot

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 app && mkdir /data && chown app /data
USER app
COPY --from=build /parkingbot /usr/local/bin/parkingbot
ENV DB_PATH=/data/parkingapp.db
VOLUME /data
ENTRYPOINT ["parkingbot"]
