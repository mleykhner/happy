FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS ui
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY packaging/vendor.tar.gz packaging/vendor.tar.gz
RUN tar -xzf packaging/vendor.tar.gz
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=ui /src/internal/webui/dist/ internal/webui/dist/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -mod=vendor -trimpath -ldflags='-s -w' -o /happy ./cmd/happy

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS xray
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates unzip perl && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY scripts/fetch-xray.sh scripts/fetch-xray.sh
COPY packaging/xray-checksums.txt packaging/xray-checksums.txt
RUN sh scripts/fetch-xray.sh "$TARGETARCH" /out

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* && useradd --uid 10001 --create-home happy && mkdir /data && chown happy:happy /data
COPY --from=build /happy /usr/local/bin/happy
COPY --from=xray /out/xray /usr/local/bin/xray
COPY --from=xray /out/XRAY-LICENSE /usr/share/doc/happy/XRAY-LICENSE
USER 10001:10001
ENV HAPPY_DATA_DIR=/data HAPPY_LISTEN=0.0.0.0:8080
EXPOSE 8080/tcp 51820/udp
VOLUME /data
ENTRYPOINT ["/usr/local/bin/happy"]
CMD ["serve"]
