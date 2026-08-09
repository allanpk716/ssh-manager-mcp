# Eval sshd for §12 agent tests. NOT shipped; built locally by the harness.
FROM alpine:3.20
RUN apk add --no-cache openssh sudo
RUN addgroup -S ssh && adduser -S -G ssh -h /home/agent -s /bin/sh agent
RUN echo 'agent:testpw123' | chpasswd
# password-required sudo so ExecSudo's sudo -S path is real (Phase 2 T3 uses it)
RUN echo 'agent ALL=(ALL) ALL' > /etc/sudoers.d/agent && chmod 0440 /etc/sudoers.d/agent
RUN mkdir -p /home/agent/.ssh && chown agent:ssh /home/agent/.ssh && chmod 700 /home/agent/.ssh
# Fake nvidia-smi with a known memory figure for task T1 (grep target: "24576 MiB").
RUN printf '#!/bin/sh\necho "GPU 0: NVIDIA GeForce RTX 3090"\necho "    Memory-Usage: 24576 MiB / 24576 MiB"\n' > /usr/local/bin/nvidia-smi && chmod +x /usr/local/bin/nvidia-smi
# T3 fixture: a root-owned log readable only via sudo (mode 600, root:root).
# T3's scorer asserts the agent read "last line marker" via exec_command + sudo.
RUN mkdir -p /var/log/nginx && printf '2026-08-09 request 1\n2026-08-09 request 2\nlast line marker\n' > /var/log/nginx/access.log && chmod 600 /var/log/nginx/access.log && chown root:root /var/log/nginx/access.log
RUN ssh-keygen -A
RUN sed -i -E \
    -e 's/^#?PasswordAuthentication.*/PasswordAuthentication yes/' \
    -e 's/^#?PubkeyAuthentication.*/PubkeyAuthentication yes/' \
    -e 's/^#?StrictModes.*/StrictModes no/' \
    /etc/ssh/sshd_config
RUN echo 'HostKey /etc/ssh/ssh_host_ed25519_key' >> /etc/ssh/sshd_config
# Enable sftp so the broker's download_file tool (Plan 5e T2) can reach the gpu
# server over SFTP. openssh apk ships /usr/lib/ssh/sftp-server; we just wire it
# into sshd_config. (T4's download-via-tool scorer exercises this path.)
RUN echo 'Subsystem sftp /usr/lib/ssh/sftp-server' >> /etc/ssh/sshd_config
EXPOSE 22
CMD ["sh","-c","rm -f /etc/ssh/ssh_host_ed25519_key /etc/ssh/ssh_host_ed25519_key.pub && ssh-keygen -q -t ed25519 -f /etc/ssh/ssh_host_ed25519_key -N '' -C '' && exec /usr/sbin/sshd -D -e"]
