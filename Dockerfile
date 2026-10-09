# Two stages, no cgo: the result is a static binary in a scratch image, a
# few megabytes all in. Templates and static assets are embedded in the
# binary, so nothing is copied beside it.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags="-s -w" -o /gotime .

FROM scratch
# For the TLS handshake with api.themoviedb.org.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /gotime /gotime
VOLUME /data
ENV GOTIME_DB=/data/gotime.db
EXPOSE 3000
ENTRYPOINT ["/gotime"]
