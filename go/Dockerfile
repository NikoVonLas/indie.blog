FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /blog ./cmd/blog
FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 blog
COPY --from=build /blog /usr/local/bin/blog
USER blog
ENV LISTEN_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["blog"]
