FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && mkdir -p /data
COPY --from=build /out/server /server
ENV PORT=8080 DB_PATH=/data/calendar.db TZ=Asia/Tokyo
EXPOSE 8080
CMD ["/server"]
