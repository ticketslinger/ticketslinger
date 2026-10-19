# Two images from one file: --target api (the Go binary) and --target web
# (storefront and dashboard). Build context is the repository root.

FROM golang:1.27 AS build
WORKDIR /src/api
COPY api/go.mod api/go.sum ./
RUN go mod download
COPY api ./
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/ticketslinger ./cmd/ticketslinger

FROM gcr.io/distroless/static-debian12:nonroot AS api
COPY --from=build /out/ticketslinger /ticketslinger
ENTRYPOINT ["/ticketslinger"]
CMD ["serve"]

FROM oven/bun:1.3.14 AS webbuild
WORKDIR /src
COPY . .
RUN bun install --frozen-lockfile
RUN cd web/storefront && bun build.ts /out/storefront/dist \
 && bun build server/index.ts --target=bun --minify --outfile /out/storefront/server.js
RUN cd web/dashboard && bunx vite build --outDir /out/dashboard/dist --emptyOutDir \
 && bun build server/index.ts --target=bun --minify --outfile /out/dashboard/server.js

FROM oven/bun:1.3.14 AS web
COPY --from=webbuild /out /app
# Compose sets the working directory and command per service; the servers read ./dist.
USER bun
