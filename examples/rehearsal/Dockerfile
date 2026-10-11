# A stand-in for production: Debian 13 with sshd, the libraries the Erlang releases need, an app user, a
# deploy user that may restart exactly one service, and a tiny systemctl that does that without systemd.
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server sudo curl ca-certificates \
      libssl3t64 libncurses6 libstdc++6 libsqlite3-0 openssl procps util-linux \
 && rm -rf /var/lib/apt/lists/* && mkdir -p /run/sshd
RUN useradd -m -s /bin/bash deploy-shop && chmod 755 /home/deploy-shop \
 && useradd -r -m -d /var/lib/shop -s /usr/sbin/nologin shop \
 && mkdir -p /home/deploy-shop/.ssh && chmod 700 /home/deploy-shop/.ssh
COPY authorized_keys /home/deploy-shop/.ssh/authorized_keys
RUN chown -R deploy-shop:deploy-shop /home/deploy-shop/.ssh && chmod 600 /home/deploy-shop/.ssh/authorized_keys
RUN echo 'deploy-shop ALL=(root) NOPASSWD: /usr/bin/systemctl restart shop.service' > /etc/sudoers.d/deploy-shop \
 && chmod 440 /etc/sudoers.d/deploy-shop
COPY systemctl /usr/bin/systemctl
RUN chmod 755 /usr/bin/systemctl
RUN printf 'PasswordAuthentication no\nPermitRootLogin no\nAllowUsers deploy-shop\n' > /etc/ssh/sshd_config.d/10-rehearsal.conf
CMD ["/bin/sh", "-c", "ssh-keygen -A && exec /usr/sbin/sshd -D -e"]
