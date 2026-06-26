# syntax=docker/dockerfile:1.7
# 用法：docker build -f deploy/Dockerfile --build-arg SERVICE=wallet -t gamepub/wallet:dev .
#
# 安全基线：
#   - 生产构建请把两个基础镜像固定到 digest（@sha256:...），避免 tag 被覆盖引入供应链风险；
#   - 运行镜像为 distroless/static:nonroot：无 shell、无包管理器，以 UID 65532 运行；
#   - CGO_ENABLED=0 静态编译，-trimpath 去除构建机路径，镜像内只有一个二进制。

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify

COPY api ./api
COPY cmd ./cmd
COPY internal ./internal
COPY pkg ./pkg

ARG SERVICE
RUN test -n "${SERVICE}" || (echo "build-arg SERVICE is required" && exit 1)
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w -buildid=" -o /out/app ./cmd/${SERVICE}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
ENTRYPOINT ["/app"]
