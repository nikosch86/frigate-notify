FROM golang:1.25-alpine AS build

WORKDIR /app

COPY go.mod /app/
COPY go.sum /app/

# gcc & musl-dev required for matrix support
RUN apk --no-cache add tzdata ca-certificates gcc musl-dev
RUN go mod download

COPY . /app/

# Tag goolm & ldflags required for matrix support
RUN go build -tags "goolm" -a -ldflags '-linkmode external -extldflags "-static"' -o /frigate-notify .

FROM alpine:3.24

WORKDIR /app

# ffmpeg transcodes Telegram clips (clip_transcode). The Intel VA driver backs
# clip_transcode_vaapi_device & is only packaged for x86_64
RUN apk --no-cache add ffmpeg tzdata ca-certificates \
    && if [ "$(apk --print-arch)" = "x86_64" ]; then apk --no-cache add intel-media-driver; fi

COPY --from=build /frigate-notify /app/frigate-notify
COPY /templates /app/templates

EXPOSE 8000

ENTRYPOINT [ "/app/frigate-notify" ]
