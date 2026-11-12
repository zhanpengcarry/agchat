# syntax=docker/dockerfile:1

# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src

# 依赖层单独缓存：只有 go.mod/go.sum 变化时才重新下载
COPY server/go.mod server/go.sum ./
RUN go mod download

COPY server/ ./
# CGO_ENABLED=0 是必须的：modernc.org/sqlite 是纯 Go 实现，关掉 cgo 才能静态链接
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/agchatd ./cmd/agchatd

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build /out/agchatd /app/agchatd

# skill 与 install 说明是运行时可读的普通文件（服务端不 embed 它们，保持单一来源）
COPY --chown=nonroot:nonroot skills/agchat/SKILL.md /app/skill/SKILL.md
COPY --chown=nonroot:nonroot mvp/join.md /app/skill/join.md

# 数据与审计日志挂在可写卷上
VOLUME ["/data"]
USER nonroot:nonroot
EXPOSE 8788

ENTRYPOINT ["/app/agchatd"]
CMD ["-addr", ":8788", "-db", "/data/agchat.db", \
     "-skill", "/app/skill/SKILL.md", "-join", "/app/skill/join.md", \
     "-audit-log", "/data/audit.jsonl"]
