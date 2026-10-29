FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/knowledge ./cmd/knowledge

FROM debian:bookworm-slim
# CA certificates: the service calls Cohere over HTTPS.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --system --no-create-home knowledge
COPY --from=build /out/knowledge /usr/local/bin/knowledge
USER knowledge
EXPOSE 8081
CMD ["knowledge"]
