# cloudmail-bridge Dockerfile v1.00
# 多阶段构建：Go 编译 + 精简运行镜像
FROM golang:1.27-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/cloudmail-bridge .

FROM alpine:3.20
ARG VERSION=1.00
LABEL org.opencontainers.image.title="cloudmail-bridge" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.description="cloud-mail IMAP/SMTP bridge"
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /out/cloudmail-bridge /app/cloudmail-bridge
ENV TZ=Asia/Shanghai
EXPOSE 993 465
ENTRYPOINT ["/app/cloudmail-bridge"]
