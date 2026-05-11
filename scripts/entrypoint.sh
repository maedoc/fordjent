#!/bin/sh
set -e

# Configure git identity — write to the fordjent user's home directly
# since the entrypoint runs as the fordjent user (not root)
GIT_EMAIL="${FORDJENT_GIT_EMAIL:-fordjent@forgejo.local}"
GIT_NAME="${FORDJENT_GIT_NAME:-Fordjent Agent}"

git config --global user.email "$GIT_EMAIL"
git config --global user.name "$GIT_NAME"
git config --global push.default current

exec fordjent -config /etc/fordjent/fordjent.yaml "$@"