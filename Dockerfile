FROM golang:1.25-alpine AS build
# Networks that cannot reach proxy.golang.org can override:
#   docker build --build-arg GOPROXY=https://goproxy.cn,direct .
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/cmdcode2api ./cmd/cmdcode2api

FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/cmdcode2api /usr/local/bin/cmdcode2api

# Stateless by design: there is no config file and no data volume. Accounts and
# client keys added in the WebUI live in memory only and are lost on restart.
# Only the listen port needs to be exposed.
EXPOSE 11434

# --host 0.0.0.0 makes the gateway reachable from outside the container.
ENTRYPOINT ["cmdcode2api"]
CMD ["--host", "0.0.0.0"]
