FROM golang:1.25.12-bookworm@sha256:6359592445455f2dbe2412bed411336035bc019a50017720d77454ffdd6d0f82 AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOMAXPROCS=2 GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN --network=none go build -mod=readonly -trimpath -ldflags="-s -w" -o /messagehub ./cmd/messagehub

FROM scratch
COPY --from=build /messagehub /messagehub
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY LICENSE NOTICE.md /usr/share/licenses/hermes-messagehub/
COPY LICENSES/ /usr/share/licenses/hermes-messagehub/LICENSES/
USER 10001:10001
ENTRYPOINT ["/messagehub"]
CMD ["serve", "--config", "/etc/messagehub/config.json"]
