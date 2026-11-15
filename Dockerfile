# One image: the React build embedded in the Go binary, plus poppler for
# PDF previews. Render builds this on every push to main.

FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
# Writes to /src/internal/webui/dist (see web/vite.config.ts).
RUN npx vite build

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/webui/dist ./internal/webui/dist
RUN CGO_ENABLED=0 go build -tags embedui -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/seed ./cmd/seed

FROM alpine:3.22
RUN apk add --no-cache poppler-utils ca-certificates tzdata \
 && adduser -D -H -u 10001 app
COPY --from=build /out/server /out/seed /usr/local/bin/
USER app
ENV APP_ENV=production
EXPOSE 10000
CMD ["server"]
