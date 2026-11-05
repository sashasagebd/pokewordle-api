# Built by the ops-dashboard platform with Docker's legacy builder:
# multi-stage builds work, BuildKit-only features (RUN --mount) don't.
FROM golang:1.26-alpine AS build
WORKDIR /src
# Dependencies first, so a code change doesn't download them again.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /pokewordle-api .

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /pokewordle-api /pokewordle-api
EXPOSE 8080
ENTRYPOINT ["/pokewordle-api"]
