#!/bin/sh
# Starts a stand-in for the server an app is deployed to (Debian 13, sshd, a deploy user that may restart one
# service, a tiny systemctl) plus a PostgreSQL, and writes a dymploy configuration whose target "production" points
# at it. Everything lives in a throw-away directory and two containers; `down.sh` removes them.
#
#   examples/rehearsal/up.sh [directory]      then:   dymploy try --deploy --config <directory>/etc/config.yaml
#
# Needs Docker. With a VM-based engine (Colima, Docker Desktop) use a directory under your home. The
# names (shop, owner acme, port 4022) are the ones of the first application; change them for another.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dir=${1:-$HOME/.cache/dymploy-rehearsal}
mkdir -p "$dir/production" "$dir/etc"
cd "$dir"
[ -f id ] || { ssh-keygen -q -t ed25519 -N "" -C dymploy-rehearsal -f id; chmod 600 id; }
cp id.pub production/authorized_keys
cp "$here/Dockerfile" "$here/systemctl" production/

docker network create dy-net >/dev/null 2>&1 || true
docker rm -f dy-db dy-production >/dev/null 2>&1 || true
docker run -d --name dy-db --network dy-net -e POSTGRES_USER=shop -e POSTGRES_PASSWORD=rehearsal-pw \
  -e POSTGRES_DB=shop_prod postgres:17 >/dev/null
docker build -q -t dy-production production >/dev/null
docker run -d --name dy-production --network dy-net -p 127.0.0.1:2222:22 dy-production >/dev/null
docker exec dy-production sh -c "printf 'DATABASE_URL=ecto://shop:rehearsal-pw@dy-db/shop_prod\nSECRET_KEY_BASE=%s\nDOMAIN=shop.test\nPORT=4022\nMAILGUN_API_KEY=x\nMAILGUN_DOMAIN=x\n' \$(openssl rand -hex 48) > /etc/shop.env; chmod 640 /etc/shop.env; chgrp shop /etc/shop.env"

# the host key is taken from the machine itself, as an operator would when adding a target
for i in 1 2 3 4 5 6 7 8 9 10; do
  ssh-keyscan -p 2222 127.0.0.1 2>/dev/null > known_hosts
  [ -s known_hosts ] && break
  sleep 1
done
[ -s known_hosts ] || { echo "the stand-in does not answer on 127.0.0.1:2222" >&2; exit 1; }

cat > etc/config.yaml <<CFG
# Rehearsal configuration: the same shape as production, pointing at the stand-in.
webhooks:
  github:
    secret_file: $dir/etc/webhook-secret
policy:
  - {provider: github, owner: acme, repositories: [shop], branches: [main]}
targets:
  production:
    host: 127.0.0.1
    port: 2222
    user: deploy-shop
    key_file: $dir/id
    known_hosts: $dir/known_hosts
    allow:
      - {provider: github, owner: acme, repositories: [shop], branches: [main]}
CFG
[ -f etc/webhook-secret ] || { openssl rand -hex 24 > etc/webhook-secret; chmod 600 etc/webhook-secret; }
echo "ready. Try it from the repository to deploy:"
echo "  dymploy try --deploy --config $dir/etc/config.yaml"
echo "and look at the result:  docker exec dy-production curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:4022/"
