FROM alpine:latest

RUN apk --no-cache add ca-certificates fuse3 mailcap tzdata coreutils && \
    echo "user_allow_other" >> /etc/fuse.conf

ARG TARGETARCH

ARG TARGETVARIANT

ARG VERSION

COPY build/bclone-${TARGETARCH}${TARGETVARIANT:+_$TARGETVARIANT}/rclone /usr/local/bin/rclone

RUN chmod +x /usr/local/bin/rclone

ARG RCLONE_UID=1009
ARG RCLONE_GID=1009

RUN addgroup -g "${RCLONE_GID}" rclone && adduser -u "${RCLONE_UID}" -Ds /bin/sh -G rclone rclone

ENTRYPOINT [ "rclone" ]

WORKDIR /data

ENV XDG_CONFIG_HOME=/config