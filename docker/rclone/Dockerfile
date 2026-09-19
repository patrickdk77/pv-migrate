FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# tar, zstd and gzip are the archive workflow's data mover. BusyBox's own tar
# cannot take --xattrs, --sparse or --numeric-owner, so GNU tar is installed
# over it rather than relied upon.
# Keep this UID/GID in sync with nonRootUID in internal/bucketstorage.
RUN apk add --no-cache rclone tar zstd gzip && \
    addgroup -g 10000 pvmigrate && \
    adduser -D -u 10000 -G pvmigrate -h /home/pvmigrate pvmigrate && \
    chown -R pvmigrate:pvmigrate /home/pvmigrate
