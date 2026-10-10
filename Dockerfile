# Build a static, CGO-free binary and ship it alone in a distroless image:
# no shell, no package manager, runs as a non-root user. Templates and
# front-end files are embedded in the binary.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /kanban .
# Distroless has no shell to create it with, so /data is made here.
RUN mkdir /data

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /kanban /kanban
# With STORAGE=sqlite the database file lives here; mount a volume on it.
COPY --from=build --chown=nonroot:nonroot /data /data
ENV SQLITE_PATH=/data/kanban.db
EXPOSE 17808
ENTRYPOINT ["/kanban"]
