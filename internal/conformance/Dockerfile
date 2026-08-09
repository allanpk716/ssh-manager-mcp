# Conformance sshd for §13 tests. NOT shipped; built locally by the test harness.
FROM alpine:3.20
RUN apk add --no-cache openssh sudo
RUN addgroup -S ssh && adduser -S -G ssh -h /home/sshuser -s /bin/sh sshuser
# Login password (used by broker password-auth and as the sudo password).
RUN echo 'sshuser:testpw123' | chpasswd
# Password-required sudo so ExecSudo's `sudo -S` path is exercised against REAL sudo.
RUN echo 'sshuser ALL=(ALL) ALL' > /etc/sudoers.d/sshuser && chmod 0440 /etc/sudoers.d/sshuser
RUN mkdir -p /home/sshuser/.ssh && chown sshuser:ssh /home/sshuser/.ssh && chmod 700 /home/sshuser/.ssh
RUN ssh-keygen -A
# StrictModes no lets us bind-mount authorized_keys (root-owned, read-only) without perm churn.
# HostKey pinned to ed25519 only: by default sshd also presents ecdsa/rsa and the Go client
# negotiates whichever the server offers first — so a FixedHostKey on ed25519 would mismatch.
# Pinning the server to a single HostKey makes the negotiated key deterministic for the harness.
RUN sed -i -E \
    -e 's/^#?PasswordAuthentication.*/PasswordAuthentication yes/' \
    -e 's/^#?PubkeyAuthentication.*/PubkeyAuthentication yes/' \
    -e 's/^#?PermitRootLogin.*/PermitRootLogin no/' \
    -e 's/^#?StrictModes.*/StrictModes no/' \
    /etc/ssh/sshd_config
RUN echo 'HostKey /etc/ssh/ssh_host_ed25519_key' >> /etc/ssh/sshd_config
EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
