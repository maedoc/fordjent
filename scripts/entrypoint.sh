#!/bin/sh
set -e

git config --global user.email "${FORDJENT_GIT_EMAIL:-fordjent@forgejo.local}"
git config --global user.name "${FORDJENT_GIT_NAME:-Fordjent Agent}"
git config --global push.default current

# Copy gitconfig to fordjent home for non-root user
cp /root/.gitconfig /var/lib/fordjent/.gitconfig 2>/dev/null || true
chown fordjent:fordjent /var/lib/fordjent/.gitconfig 2>/dev/null || true

exec fordjent -config /etc/fordjent/fordjent.yaml "$@"
