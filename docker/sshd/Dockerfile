FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

# we unlock the root user for sshd
# https://github.com/alpinelinux/docker-alpine/issues/28#issuecomment-510510532
# https://github.com/alpinelinux/docker-alpine/issues/28#issuecomment-659551571
# tar, zstd and pigz carry the tar data mover: this side runs the other half
# of the pipe, reading or writing the volume that the ssh session reaches.
# See docker/rclone/Dockerfile for why gzip is pointed at pigz.
RUN apk add --no-cache rsync openssh openssh-server-pam tini tar zstd pigz && \
    ln -sf "$(command -v pigz)" /bin/gzip && \
    sed -i -e 's/^root:!:/root:*:/' /etc/shadow && \
    addgroup -g 10000 pvmigrate && \
    adduser -D -u 10000 -G pvmigrate -h /home/pvmigrate pvmigrate && \
    mkdir -p /home/pvmigrate/.ssh && \
    chown -R pvmigrate:pvmigrate /home/pvmigrate && \
    chmod 700 /home/pvmigrate/.ssh

COPY sshd_config /etc/ssh/sshd_config

EXPOSE 22

ENTRYPOINT ["tini", "--"]
CMD ["/usr/sbin/sshd", "-D", "-e", "-f", "/etc/ssh/sshd_config"]
