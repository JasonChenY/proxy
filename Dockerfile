#docker buildx build --platform=linux/amd64,linux/arm64   -t ccr.ccs.tencentyun.com/chenyong/proxy:v1   --push .

FROM --platform=$BUILDPLATFORM golang:1.22 as builder
ARG TARGETOS
ARG TARGETARCH 
WORKDIR /workspace
COPY proxy.go proxy.go 
COPY ds.go ds.go
COPY go.mod go.mod
COPY go.sum go.sum

ENV GOPROXY="https://goproxy.cn,direct"
RUN go mod download

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o proxy proxy.go ds.go


FROM --platform=$TARGETPLATFORM alpine:3.12
WORKDIR /
COPY --from=builder /workspace/proxy .
ENTRYPOINT ["/proxy"]
