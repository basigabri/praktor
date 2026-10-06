# Build context: the repository root (stdlib only, so no module downloads).
FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY test/e2e/mockllm/ ./test/e2e/mockllm/
RUN CGO_ENABLED=0 go build -o /mockllm ./test/e2e/mockllm

FROM alpine:3.23
COPY --from=build /mockllm /usr/local/bin/mockllm
USER nobody
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/mockllm"]
