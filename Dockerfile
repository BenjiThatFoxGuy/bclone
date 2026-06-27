FROM alpine:latest

RUN apk --no-cache add ca-certificates fuse3 tzdata coreutils && \
    echo "user_allow_other" >> /etc/fuse.conf

ARG TARGETARCH

ARG TARGETVARIANT

ARG VERSION

COPY build/bclone-${TARGETARCH}${TARGETVARIANT:+_$TARGETVARIANT}/rclone /usr/local/bin/rclone

RUN chmod +x /usr/local/bin/rclone

RUN addgroup -g 1009 rclone && adduser -u 1009 -Ds /bin/sh -G rclone rclone

ENTRYPOINT [ "rclone" ]

WORKDIR /data

ENV XDG_CONFIG_HOME=/config