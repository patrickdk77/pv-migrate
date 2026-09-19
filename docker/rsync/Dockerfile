FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# tar, zstd and pigz carry the tar data mover, which moves a volume over the
# same ssh transport rsync uses. BusyBox's own tar cannot take --xattrs,
# --sparse or --numeric-owner, so GNU tar is installed over it. gzip takes
# pigz's place for the reason given in docker/rclone/Dockerfile: tar picks the
# decompressor itself and invokes it by name through PATH.
RUN apk add --no-cache rsync openssh tar zstd pigz && \
    ln -sf "$(command -v pigz)" /bin/gzip && \
    addgroup -g 10000 pvmigrate && \
    adduser -D -u 10000 -G pvmigrate -h /home/pvmigrate pvmigrate && \
    mkdir -p /home/pvmigrate/.ssh && \
    chown -R pvmigrate:pvmigrate /home/pvmigrate && \
    chmod 700 /home/pvmigrate/.ssh
