FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static binary so the runtime image needs no libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
# No migrations are copied: the schema belongs to the backoffice
# (github.com/playxdev/dormplace), which owns and applies them. This service
# only reads the database.
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/api"]
