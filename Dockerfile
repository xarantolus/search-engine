FROM node:22 AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package*.json ./
RUN --mount=type=cache,target=/root/.npm npm install
COPY frontend/ ./
RUN npm run build -- --outDir dist

FROM golang:alpine AS builder
RUN apk add --no-cache git gcc musl-dev
WORKDIR /app
COPY backend/go.mod backend/go.sum ./
COPY shared/go.mod shared/go.sum ../shared/
RUN --mount=type=cache,target=/go/pkg/mod go mod download -x
COPY backend/ .
COPY shared/ ../shared/
RUN --mount=type=cache,target=/root/.cache/go-build \
	--mount=type=cache,target=/go/pkg/mod \
	CGO_ENABLED=1 go build -v -o /app/search-engine

FROM alpine:latest
RUN apk add --no-cache sqlite-libs
WORKDIR /app/
COPY --from=builder /app/search-engine .
COPY --from=frontend-builder /app/frontend/dist /frontend/dist
CMD ["./search-engine"]
