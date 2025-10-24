# Stage 1 - Build frontend assets
FROM node:22-alpine AS frontend-builder
ARG ENABLE_SOURCEMAPS=false
ENV ENABLE_SOURCEMAPS=${ENABLE_SOURCEMAPS}
WORKDIR /frontend

# Install dependencies and build the frontend (expects a package.json in ./frontend)
COPY frontend/package*.json ./
RUN npm install
COPY frontend/ ./
RUN npm run build

# Stage 2 - Build the Go backend
FROM golang:1.24 AS backend-builder
WORKDIR /app

COPY backend/go.mod ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -o server

# Final stage - minimal runtime image
FROM gcr.io/distroless/base-debian12
WORKDIR /app

COPY --from=backend-builder /app/server ./server
COPY --from=frontend-builder /frontend/dist ./static

ENV PORT=8080
ENV STATIC_DIR=/app/static
EXPOSE 8080

ENTRYPOINT ["/app/server"]
