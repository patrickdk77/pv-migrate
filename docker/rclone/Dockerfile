FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# tar, zstd and pigz are the archive workflow's data mover. BusyBox's own tar
# cannot take --xattrs, --sparse or --numeric-owner, so GNU tar is installed
# over it rather than relied upon.
#
# pigz takes the gzip name rather than being named in the command, because tar
# picks the decompressor itself when it extracts from a file, invoking
# "gzip -d" by name through PATH, which no argument of ours can redirect. The
# format is unchanged, each reads what the other writes, and on 937 MiB pigz
# compresses 13x faster and decompresses 1.4x faster. gzip is not installed
# alongside: the symlink would overwrite its binary anyway, so the package
# would be dead weight.
# Keep this UID/GID in sync with nonRootUID in internal/bucketstorage.
RUN apk add --no-cache rclone tar zstd pigz && \
    ln -sf "$(command -v pigz)" /bin/gzip && \
    addgroup -g 10000 pvmigrate && \
    adduser -D -u 10000 -G pvmigrate -h /home/pvmigrate pvmigrate && \
    chown -R pvmigrate:pvmigrate /home/pvmigrate
