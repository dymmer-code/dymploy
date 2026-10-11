#!/bin/sh
# Removes what up.sh started (the containers and the network; the directory with the keys stays).
docker rm -f dy-db dy-production >/dev/null 2>&1 || true
docker network rm dy-net >/dev/null 2>&1 || true
echo "removed"
